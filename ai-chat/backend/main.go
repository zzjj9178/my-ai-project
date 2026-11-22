package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://api.deepseek.com"
	// ⚠️ 老模型名 deepseek-chat / deepseek-reasoner 已于 2026-07-24 退役，
	// 当前在售：deepseek-flash（V4.1-Flash，默认，便宜）/ deepseek-v4-pro（推理更强）
	defaultModel  = "deepseek-flash"
	defaultSystem = "你是一个友好、简洁的中文 AI 助手。请用中文回答，结构化、易读，必要时用 Markdown。" +
		"注意：你现在身处多人聊天室，回答请简明扼要，控制在 200 字以内，除非用户要求详细展开。"

	maxHistoryTurns = 10 // 每个用户保留的 AI 对话轮数
	maxQuestionLen  = 2000
	maxChatLen      = 500 // 聊天室单条消息长度
	maxHistoryKeep  = 50  // 聊天室保留的历史消息条数
	maxContextMsgs  = 30  // @AI 时携带的群聊上下文条数（别太大，token 是要花钱的）

	// AI 主动欢迎新成员的冷却时间（分钟）。0 = 每次进房都欢迎（费钱，不建议公网开）
	welcomeCooldownMin = 5
	maxNameLen         = 20
	maxSchoolLen       = 30
	maxMajorLen        = 30
	maxBioLen          = 100 // 自我介绍，太长会让欢迎语 prompt 失控

	authCookieName = "ai_token"

	defaultStaticDir  = "./web"
	defaultListenAddr = "0.0.0.0:8081"
)

var rateLimitPerMin = 20 // 单 IP/用户每分钟 AI 调用上限，0 不限

// ============================================================
//  用户体系（内存存储，重启即清空；够课程演示用）
// ============================================================

// User 是纯数据结构（不含锁），可以安全地被拷贝、序列化、广播
type User struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	School  string `json:"school"`
	Major   string `json:"major"`
	Color   string `json:"color"`   // 头像配色，由 ID 决定
	Initial string `json:"initial"` // 头像首字
	Bio     string `json:"bio"`     // 自我介绍，会喂给 AI 生成个性化欢迎语

	IP string `json:"-"` // 登录 IP，仅服务端记录用

	// ⚠️ 联系方式是隐私：json:"-" 保证它们永远不会出现在广播（join/leave/在线名单/
	// 历史消息）里，只通过 /api/login、/api/me、/api/profile 单独下发给本人。
	QQ    string `json:"-"`
	Phone string `json:"-"`

	// ClientID 是浏览器本地生成的稳定标识，用来识别「同一个人的再次登录」
	ClientID string `json:"-"`
	// Welcomed 记录这个人是否已经触发过 AI 欢迎语（只欢迎第一次）
	Welcomed bool `json:"-"`
}

// contact 只发给本人的私密字段
func contactOf(u *User) map[string]string {
	return map[string]string{"qq": u.QQ, "phone": u.Phone}
}

var (
	userMu       sync.Mutex
	usersByToken = make(map[string]*User) // token -> 用户

	// clientID -> 身份。用浏览器本地 ID 做锚点，同一台设备在 TTL 内再次登录
	// （哪怕改了昵称）都算同一个人，不会被当成新用户、也不会重复触发 AI 欢迎语。
	identityTTL   = time.Hour
	usersByClient = make(map[string]*clientIdentity)

	// 按用户 ID 索引。中奖名单存的是「中奖那一刻」的昵称快照，
	// 用户之后在个人中心改名，名单要显示最新昵称，就靠这张表回查。
	usersByID = make(map[string]*User)

	// AI 对话历史按用户 ID 单独存放（避免 User 里放锁导致拷贝问题）
	histMu        sync.Mutex
	userHistories = make(map[string][]chatMessage)
)

type clientIdentity struct {
	user     *User
	lastSeen time.Time
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func getUserByToken(token string) (*User, bool) {
	if token == "" {
		return nil, false
	}
	userMu.Lock()
	defer userMu.Unlock()
	u, ok := usersByToken[token]
	return u, ok
}

// 头像配色：按 ID 取，保证同一用户每次进来颜色一致
var avatarColors = []string{"#6366f1", "#8b5cf6", "#ec4899", "#f43f5e", "#f97316", "#eab308", "#22c55e", "#14b8a6", "#0ea5e9", "#3b82f6"}

func colorOf(id string) string {
	sum := 0
	for _, r := range id {
		sum += int(r)
	}
	return avatarColors[sum%len(avatarColors)]
}

func initialOf(name string) string {
	runes := []rune(name)
	if len(runes) == 0 {
		return "?"
	}
	return string(runes[0])
}

// appendUserHistory 记录该用户的 AI 对话上下文（每人独立，互不干扰）
func appendUserHistory(u *User, question, answer string) {
	histMu.Lock()
	defer histMu.Unlock()
	h := append(userHistories[u.ID],
		chatMessage{Role: "user", Content: question},
		chatMessage{Role: "assistant", Content: answer},
	)
	if len(h) > maxHistoryTurns*2 {
		h = h[len(h)-maxHistoryTurns*2:]
	}
	userHistories[u.ID] = h
}

func loadUserHistory(u *User) []chatMessage {
	if u == nil {
		return nil
	}
	histMu.Lock()
	defer histMu.Unlock()
	return append([]chatMessage(nil), userHistories[u.ID]...)
}

// ============================================================
//  DeepSeek 协议结构体
// ============================================================

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type thinkingConfig struct {
	Type string `json:"type"`
}

type deepseekRequest struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	Temperature     float64         `json:"temperature,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Thinking        *thinkingConfig `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
}

type delta struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
}

type choice struct {
	Delta delta `json:"delta"`
}

type streamResponse struct {
	Choices []choice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ============================================================
//  俱乐部知识库：让 AI 知道社团内部信息（成员去向、竞赛战绩、纳新流程…）
//  来源是纳新宣讲 PPT，整理在 knowledge.md 里，启动时读入内存
// ============================================================

var clubKB = loadClubKB()

func loadClubKB() string {
	path := env("CLUB_KB_FILE", "knowledge.md")
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("⚠️  未加载俱乐部知识库(%s): %v —— AI 将不具备社团内部知识", path, err)
		return ""
	}
	kb := strings.TrimSpace(string(b))
	log.Printf("  俱乐部知识库: %s（%d 字）", path, len([]rune(kb)))
	return kb
}

// systemPrompt 系统提示词 = 基础人设 + 俱乐部资料。
// 欢迎语、@AI 问答、旧版单聊接口统一走这里，避免有的地方漏掉资料。
func systemPrompt() string {
	base := env("SYSTEM_PROMPT", defaultSystem)
	if clubKB == "" {
		return base
	}
	return base + "\n\n【西安工业大学云原生俱乐部 · 参考资料】\n" + clubKB +
		"\n【资料结束】凡涉及本俱乐部的问题，一律以上面资料为准；资料里没有的信息就说不太确定，不要编造。"
}

// ============================================================
//  AI 流式调用（聊天室版本：把 delta 回调出去，而非写 SSE）
// ============================================================

// buildAIMessages 系统提示 + 群聊上下文 + 该用户的历史 + 本轮问题。
// room 为空时（如旧版单聊接口）就只有个人历史。
func buildAIMessages(u *User, question string, room []string) []chatMessage {
	msgs := []chatMessage{{Role: "system", Content: systemPrompt()}}

	// 群聊上下文：让 AI 知道房间里刚刚发生了什么
	if len(room) > 0 {
		ctx := "【聊天室最近的消息记录，按时间顺序】\n" + strings.Join(room, "\n") +
			"\n【以上为背景】请结合这些上下文回答紧接着的问题。若上下文与问题无关，直接回答即可，不要复述聊天记录。"
		msgs = append(msgs, chatMessage{Role: "system", Content: ctx})
	}

	hist := loadUserHistory(u)
	if len(hist) > maxHistoryTurns*2 {
		hist = hist[len(hist)-maxHistoryTurns*2:]
	}
	msgs = append(msgs, hist...)
	msgs = append(msgs, chatMessage{Role: "user", Content: question})
	return msgs
}

// streamDeepSeek 调用 DeepSeek 并逐块回调 delta
func streamDeepSeek(messages []chatMessage, onDelta func(string) error) error {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		return errors.New("服务端未配置 DEEPSEEK_API_KEY，请联系管理员")
	}

	dsReq := deepseekRequest{
		Model:       env("DEEPSEEK_MODEL", defaultModel),
		Messages:    messages,
		Stream:      true,
		Temperature: 0.7,
	}
	if env("THINKING", "off") == "on" {
		dsReq.Thinking = &thinkingConfig{Type: "enabled"}
		dsReq.ReasoningEffort = env("REASONING_EFFORT", "medium")
	}
	payload, _ := json.Marshal(dsReq)

	req, err := http.NewRequest(http.MethodPost, env("DEEPSEEK_BASE_URL", defaultBaseURL)+"/chat/completions", strings.NewReader(string(payload)))
	if err != nil {
		return errors.New("构造上游请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		log.Printf("调用 DeepSeek 失败: %v", err)
		return errors.New("调用 DeepSeek 失败，请检查网络或 API Key")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("DeepSeek 返回 %d: %s", resp.StatusCode, body)
		return fmt.Errorf("DeepSeek 返回 %d（可能是 Key 无效或余额不足）", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	sent := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var sr streamResponse
		if err := json.Unmarshal([]byte(data), &sr); err != nil {
			continue
		}
		if sr.Error != nil {
			return errors.New(sr.Error.Message)
		}
		if len(sr.Choices) == 0 {
			continue
		}
		// 思维链不广播到聊天室（刷屏），只要正文
		if c := sr.Choices[0].Delta.Content; c != "" {
			sent = true
			if err := onDelta(c); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("上游连接中断")
	}
	if !sent {
		return errors.New("模型没有返回内容，请稍后重试")
	}
	return nil
}

// ============================================================
//  限流：保护你的余额
// ============================================================

type visitor struct {
	count    int
	windowAt time.Time
}

var (
	rateMu   sync.Mutex
	visitors = make(map[string]*visitor)
)

func allow(key string) bool {
	limit := rateLimitPerMin
	if limit <= 0 {
		return true
	}
	rateMu.Lock()
	defer rateMu.Unlock()

	now := time.Now()
	v, ok := visitors[key]
	if !ok || now.Sub(v.windowAt) > time.Minute {
		visitors[key] = &visitor{count: 1, windowAt: now}
		return true
	}
	v.count++
	return v.count <= limit
}

func clientIP(r *http.Request) string {
	if os.Getenv("TRUST_PROXY") == "on" {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if ip := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); ip != "" {
			return strings.Split(ip, ",")[0]
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// withCORS 全局跨域中间件。
// 前端(80) 与后端(8082) 不同源时必须加，否则浏览器会直接拒收响应，
// 表现为 fetch 报 "Failed to fetch"（后端其实已经处理成功了，但前端收不到）。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// 预检请求直接返回，不进业务逻辑
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================================================
//  HTTP 接口
// ============================================================

type loginRequest struct {
	Name     string `json:"name"`
	School   string `json:"school"`
	Major    string `json:"major"`
	Bio      string `json:"bio"`      // 自我介绍（选填）
	QQ       string `json:"qq"`       // 选填
	Phone    string `json:"phone"`    // 选填
	ClientID string `json:"clientId"` // 浏览器本地生成的稳定标识
}

// 校验 + 归一化联系方式
func normalizeContact(qq, phone string) (string, string, error) {
	qq = strings.TrimSpace(qq)
	phone = strings.TrimSpace(phone)
	if qq != "" {
		if len(qq) < 5 || len(qq) > 12 {
			return "", "", errors.New("QQ 号看起来不对（5–12 位数字）")
		}
		for _, c := range qq {
			if c < '0' || c > '9' {
				return "", "", errors.New("QQ 号只能是数字")
			}
		}
	}
	if phone != "" {
		if len(phone) != 11 {
			return "", "", errors.New("手机号应为 11 位数字")
		}
		for _, c := range phone {
			if c < '0' || c > '9' {
				return "", "", errors.New("手机号只能是数字")
			}
		}
	}
	return qq, phone, nil
}

// findIdentity 按浏览器本地 ID 找回「同一个人」；超过 TTL 就视为新身份
func findIdentity(clientID string) (*User, bool) {
	if clientID == "" {
		return nil, false
	}
	userMu.Lock()
	defer userMu.Unlock()
	id, ok := usersByClient[clientID]
	if !ok {
		return nil, false
	}
	if time.Since(id.lastSeen) > identityTTL {
		delete(usersByClient, clientID)
		return nil, false
	}
	id.lastSeen = time.Now()
	return id.user, true
}

// userByID 按 ID 找回用户（可能已经被清理，返回 nil）
func userByID(id string) *User {
	userMu.Lock()
	defer userMu.Unlock()
	return usersByID[id]
}

// dropTokensOf 让该用户之前的旧 token 失效（同一个人换标签页/重登时不重复在线）
func dropTokensOf(u *User) {
	userMu.Lock()
	for t, x := range usersByToken {
		if x == u {
			delete(usersByToken, t)
		}
	}
	userMu.Unlock()
}

// 登录：填名称+学校+专业即可，后端发 token。
// 带 clientId 时，TTL 内再次登录会被认成同一个人（改昵称也算），不会重复触发 AI 欢迎语。
func loginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.School = strings.TrimSpace(req.School)
	req.Major = strings.TrimSpace(req.Major)
	req.Bio = strings.TrimSpace(req.Bio)
	req.ClientID = strings.TrimSpace(req.ClientID)

	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "请填写名称")
		return
	}
	if len([]rune(req.Name)) > maxNameLen || len([]rune(req.School)) > maxSchoolLen || len([]rune(req.Major)) > maxMajorLen {
		writeErr(w, http.StatusBadRequest, "名称/学校/专业太长了")
		return
	}
	if len([]rune(req.Bio)) > maxBioLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("自我介绍请控制在 %d 字以内", maxBioLen))
		return
	}
	qq, phone, err := normalizeContact(req.QQ, req.Phone)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	token := newToken()
	u, isReturning := findIdentity(req.ClientID)

	if isReturning {
		// 老朋友回来了：沿用同一个 ID / 头像配色，只更新资料。
		// 这样在线人数不会被算成两个人，AI 也不会再欢迎一次。
		dropTokensOf(u)
		userMu.Lock()
		u.Name, u.School, u.Major, u.Bio = req.Name, req.School, req.Major, req.Bio
		u.QQ, u.Phone = qq, phone
		u.Initial = initialOf(u.Name)
		if u.ClientID == "" {
			u.ClientID = req.ClientID
		}
		usersByToken[token] = u
		usersByID[u.ID] = u
		userMu.Unlock()
		log.Printf("[登录] %s（%s-%s）IP=%s 识别为老用户（clientId 命中，ID=%s）",
			u.Name, u.School, u.Major, clientIP(r), u.ID)
	} else {
		u = &User{
			ID:       fmt.Sprintf("u%d", time.Now().UnixNano()),
			Name:     req.Name,
			School:   req.School,
			Major:    req.Major,
			Bio:      req.Bio,
			QQ:       qq,
			Phone:    phone,
			ClientID: req.ClientID,
		}
		u.Color = colorOf(u.ID)
		u.Initial = initialOf(u.Name)
		userMu.Lock()
		usersByToken[token] = u
		usersByID[u.ID] = u
		if req.ClientID != "" {
			usersByClient[req.ClientID] = &clientIdentity{user: u, lastSeen: time.Now()}
		}
		userMu.Unlock()
	}

	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   86400 * 7,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// 登录时把 IP 记进用户对象，后续聊天/AI 调用日志都能带上
	u.IP = clientIP(r)

	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "token": token, "user": u,
		"contact": contactOf(u), "lucky": luckyOf(u), "returning": isReturning,
	})
	if !isReturning {
		log.Printf("[登录] %s（%s-%s）IP=%s", u.Name, u.School, u.Major, u.IP)
	}
}

// 个人中心：改昵称/学校/专业/自我介绍/QQ/手机号。
// ⚠️ 这里刻意不调用任何 AI —— 改资料不该重新触发欢迎语，也不该花钱。
func profileHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	u, ok := getUserByToken(tokenFromRequest(r))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "登录已失效，请重新进入")
		return
	}

	// 用指针接收：只更新「这次真的提交了」的字段，
	// 避免前端表单删掉某个输入框后，保存资料把它原来的值一起清空
	var req struct {
		Name   *string `json:"name"`
		School *string `json:"school"`
		Major  *string `json:"major"`
		Bio    *string `json:"bio"`
		QQ     *string `json:"qq"`
		Phone  *string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	trim := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	name, school, major := trim(req.Name), trim(req.School), trim(req.Major)
	if req.Name != nil && name == "" {
		writeErr(w, http.StatusBadRequest, "昵称不能为空")
		return
	}
	if len([]rune(name)) > maxNameLen || len([]rune(school)) > maxSchoolLen || len([]rune(major)) > maxMajorLen {
		writeErr(w, http.StatusBadRequest, "名称/学校/专业太长了")
		return
	}
	if req.Bio != nil && len([]rune(trim(req.Bio))) > maxBioLen {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("自我介绍请控制在 %d 字以内", maxBioLen))
		return
	}
	qq, phone, err := normalizeContact(trim(req.QQ), trim(req.Phone))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	userMu.Lock()
	if req.Name != nil {
		u.Name, u.Initial = name, initialOf(name)
	}
	if req.School != nil {
		u.School = school
	}
	if req.Major != nil {
		u.Major = major
	}
	if req.Bio != nil {
		u.Bio = trim(req.Bio)
	}
	if req.QQ != nil {
		u.QQ = qq
	}
	if req.Phone != nil {
		u.Phone = phone
	}
	userMu.Unlock()

	// 广播一份新的在线名单，让所有人的名单里立刻显示新昵称
	hub.broadcast(map[string]any{"type": "online", "online": hub.onlineSnapshot(nil)}, false)

	log.Printf("[改资料] %s（%s-%s）IP=%s", u.Name, u.School, u.Major, u.IP)
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "user": u, "contact": contactOf(u), "lucky": luckyOf(u),
	})
}

func tokenFromRequest(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		return c.Value
	}
	return ""
}

// 用已有 token 恢复登录态（刷新页面不用重新填）
func meHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	token := tokenFromRequest(r)
	if u, ok := getUserByToken(token); ok {
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "user": u, "token": token,
			"contact": contactOf(u), "lucky": luckyOf(u),
		})
		return
	}
	writeErr(w, http.StatusUnauthorized, "未登录")
}

// 退出只结束这次会话，不动 clientId —— 所以再进来还是同一个人
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	token := tokenFromRequest(r)
	userMu.Lock()
	delete(usersByToken, token)
	userMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: authCookieName, Value: "", Path: "/", MaxAge: -1})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, `{"status":"ok","model":"%s","online":%d,"apikey_configured":%v,"time":"%s"}`,
		env("DEEPSEEK_MODEL", defaultModel),
		hub.onlineCount(),
		os.Getenv("DEEPSEEK_API_KEY") != "",
		time.Now().Format(time.RFC3339))
}

// 供旧版单聊页面使用（保留兼容）
func askHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if !allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("请求太频繁，每分钟最多 %d 次", rateLimitPerMin))
		return
	}

	var req struct {
		Question string        `json:"question"`
		History  []chatMessage `json:"history,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		writeErr(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	// 切 SSE
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "不支持流式输出")
		return
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	msgs := []chatMessage{{Role: "system", Content: systemPrompt()}}
	msgs = append(msgs, req.History...)
	msgs = append(msgs, chatMessage{Role: "user", Content: req.Question})

	err := streamDeepSeek(msgs, func(d string) error {
		b, _ := json.Marshal(map[string]string{"type": "content", "delta": d})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		return nil
	})
	if err != nil {
		b, _ := json.Marshal(map[string]string{"type": "error", "delta": err.Error()})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// ============================================================

var hub = newHub()

func main() {
	addr := env("LISTEN_ADDR", defaultListenAddr)
	if v, err := strconv.Atoi(env("RATE_LIMIT_PER_MIN", "20")); err == nil {
		rateLimitPerMin = v
	}

	log.Printf("AI 聊天室启动中: http://%s", addr)
	log.Printf("  身份识别: clientId 命中 %v 内视为同一个人（不再重复触发 AI 欢迎语）", identityTTL)
	log.Printf("  管理后台: http://<host>/admin  账号=%s（密码用 ADMIN_PASS 覆盖）", adminUser())
	log.Printf("  欢迎策略: 默认人人欢迎；开启幸运观众后改为「进房瞬间按概率判定 + 人数上限」")
	log.Printf("  model=%s | thinking=%s | AI限流=%d次/分钟 | apikey已配置=%v",
		env("DEEPSEEK_MODEL", defaultModel), env("THINKING", "off"),
		rateLimitPerMin, os.Getenv("DEEPSEEK_API_KEY") != "")
	if os.Getenv("DEEPSEEK_API_KEY") == "" {
		log.Printf("  ⚠️  未检测到 DEEPSEEK_API_KEY：@AI 功能会报错，普通聊天不受影响")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", loginHandler)
	mux.HandleFunc("/api/me", meHandler)
	mux.HandleFunc("/api/profile", profileHandler)
	mux.HandleFunc("/api/logout", logoutHandler)
	mux.HandleFunc("/api/ask", askHandler)
	mux.HandleFunc("/api/health", healthHandler)

	// 管理后台（/admin 是独立页面，不是首页）
	mux.HandleFunc("/api/admin/login", adminLoginHandler)
	mux.HandleFunc("/api/admin/state", adminStateHandler)
	mux.HandleFunc("/api/admin/config", adminConfigHandler)
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) { wsHandler(hub, w, r) })

	// 静态前端（登录页 + 个人主页 + 聊天室都在里面）
	staticDir := env("STATIC_DIR", defaultStaticDir)
	// STATIC_DIR=off：明确关闭静态文件服务。
	// 前后端分离部署（Nginx 提供网页、本进程只提供 API）时用这个值 ——
	// env() 把空字符串当成"没设置"，所以需要一个显式的哨兵值。
	if staticDir == "off" || staticDir == "none" {
		staticDir = ""
		log.Printf("  静态目录: 已关闭（STATIC_DIR=%s，网页由前端 Nginx 提供）", "off")
	}
	if staticDir != "" {
		if fi, err := os.Stat(staticDir); err == nil && fi.IsDir() {
			// /admin 直接映射到 web/admin.html（比 / 更具体，ServeMux 会优先匹配）
			mux.HandleFunc("/admin", adminPageHandler(staticDir))
			mux.Handle("/", noCache(http.FileServer(http.Dir(staticDir))))
			log.Printf("  静态目录: %s", staticDir)
		} else {
			log.Printf("⚠️  STATIC_DIR=%s 不可用，仅提供 API", staticDir)
		}
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      withCORS(mux), // 跨域中间件包在最外层，所有接口都生效
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // 流式响应不能设写超时
		IdleTimeout:  120 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
