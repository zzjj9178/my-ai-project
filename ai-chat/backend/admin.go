package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================
//  管理后台：幸运观众策略
//
//  关闭（默认）：每个人第一次进房都被 AI 欢迎一次
//  开启        ：进房的瞬间掷一次骰子 ——
//                中奖概率 = probability（0~1）
//                最多中奖人数 = maxWinners（抽满即止）
//                中奖的人立刻收到欢迎语 + 「幸运用户00x」编号
//                没中奖的人不触发任何 AI 调用
//
//  没有定时器、没有"手动抽一位"：判定就发生在进房那一刻。
// ============================================================

const (
	adminSessionTTL = 12 * time.Hour
	maxWinnersLimit = 1000
)

type luckyWinner struct {
	No     int    `json:"no"`
	Code   string `json:"code"`
	UserID string `json:"userId"`
	Name   string `json:"name"`
	School string `json:"school"`
	At     string `json:"at"`
}

type luckyState struct {
	mu         sync.Mutex
	enabled    bool
	prob       float64 // 中奖概率 0~1
	maxWinners int     // 最多中奖人数
	drawn      int     // 已中奖人数
	joined     int     // 本期参与抽奖的人数（按人去重）
	round      int     // 期号，每「开始新一期」+1
	winners    []luckyWinner
	codes      map[string]string // userID -> 中奖编号

	// 本期已经掷过骰子的人。进房只判定一次 —— 反复进出/刷新页面不会重新获得机会，
	// 否则学生一直刷就能把概率刷穿。
	participated map[string]bool
}

var lucky = &luckyState{
	enabled:      false,
	prob:         0.10, // 默认 10%
	maxWinners:   10,
	codes:        make(map[string]string),
	participated: make(map[string]bool),
}

func luckyCode(no int) string { return fmt.Sprintf("幸运用户%03d", no) }

func luckyEnabled() bool {
	lucky.mu.Lock()
	defer lucky.mu.Unlock()
	return lucky.enabled
}

// luckyOf 返回某个用户的中奖信息（没中奖返回 nil）；只发给本人
func luckyOf(u *User) map[string]any {
	if u == nil {
		return nil
	}
	lucky.mu.Lock()
	code, ok := lucky.codes[u.ID]
	lucky.mu.Unlock()
	if !ok {
		return nil
	}
	return map[string]any{"code": code}
}

type luckySnapshot struct {
	Enabled     bool
	Probability float64
	MaxWinners  int
	Drawn       int
	Joined      int
	Round       int
	Winners     []luckyWinner
}

func (s *luckyState) snapshot() luckySnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return luckySnapshot{
		Enabled: s.enabled, Probability: s.prob, MaxWinners: s.maxWinners,
		Drawn: s.drawn, Joined: s.joined, Round: s.round,
		Winners: append([]luckyWinner(nil), s.winners...),
	}
}

func (s *luckyState) set(enabled bool, prob float64, maxWinners int) {
	if prob < 0 {
		prob = 0
	}
	if prob > 1 {
		prob = 1
	}
	if maxWinners < 0 {
		maxWinners = 0
	}
	if maxWinners > maxWinnersLimit {
		maxWinners = maxWinnersLimit
	}
	s.mu.Lock()
	s.enabled, s.prob, s.maxWinners = enabled, prob, maxWinners
	s.mu.Unlock()
}

func (s *luckyState) reset() {
	s.mu.Lock()
	s.round++ // 新一期：期号 +1，抽奖资格与中奖名单一起重置
	s.drawn, s.joined = 0, 0
	s.winners = nil
	s.codes = make(map[string]string)
	s.participated = make(map[string]bool)
	s.mu.Unlock()
}

// roundNo 当前期号（幸运观众的欢迎语按「期」去重，新一期可以重新欢迎）
func (s *luckyState) roundNo() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.round
}

// roll 在「进房那一刻」判定是否中奖。命中就占一个名额并返回编号。
func (s *luckyState) roll(u *User) (string, int, bool) {
	s.mu.Lock()
	if !s.enabled {
		s.mu.Unlock()
		return "", 0, false
	}
	// 本期已经掷过骰子的人：反复进出/刷新不再给新机会（否则能把概率刷穿）
	if s.participated[u.ID] {
		s.mu.Unlock()
		return "", 0, false
	}
	s.participated[u.ID] = true
	s.joined++ // 本期参与人数（按人去重）

	prob, maxW, drawn := s.prob, s.maxWinners, s.drawn
	_, dup := s.codes[u.ID]
	s.mu.Unlock()

	if dup || drawn >= maxW || prob <= 0 {
		return "", 0, false
	}
	if prob < 1 && !randHit(prob) {
		return "", 0, false // 这次没中
	}

	// 中了 —— 抢名额（可能被并发进房的人先抢走）
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drawn >= s.maxWinners {
		return "", 0, false
	}
	if _, dup := s.codes[u.ID]; dup {
		return "", 0, false
	}
	s.drawn++
	no := s.drawn
	code := luckyCode(no)
	s.codes[u.ID] = code
	s.winners = append(s.winners, luckyWinner{
		No: no, Code: code, UserID: u.ID, Name: u.Name, School: u.School,
		At: time.Now().Format("15:04:05"),
	})
	return code, no, true
}

// giveBack 中奖但最终没能欢迎（比如已经被欢迎过）时把名额还回去
func (s *luckyState) giveBack(u *User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.codes[u.ID]; !ok {
		return
	}
	delete(s.codes, u.ID)
	s.drawn--
	for i := range s.winners {
		if s.winners[i].UserID == u.ID {
			s.winners = append(s.winners[:i], s.winners[i+1:]...)
			break
		}
	}
}

// randHit 以 p 的概率返回 true（用加密随机数，避免可预测）
func randHit(p float64) bool {
	const scale = int64(1) << 31
	n, err := rand.Int(rand.Reader, big.NewInt(scale))
	if err != nil {
		return false
	}
	return float64(n.Int64())/float64(scale) < p
}

// ============================================================
//  管理端登录态
// ============================================================

var (
	adminMu       sync.Mutex
	adminSessions = map[string]time.Time{} // token -> 过期时间
)

func adminUser() string { return env("ADMIN_USER", "admin") }
func adminPass() string { return env("ADMIN_PASS", "qwer1234") }

func newAdminSession() string {
	tok := newToken()
	adminMu.Lock()
	adminSessions[tok] = time.Now().Add(adminSessionTTL)
	for t, exp := range adminSessions {
		if time.Now().After(exp) {
			delete(adminSessions, t)
		}
	}
	adminMu.Unlock()
	return tok
}

func adminToken(r *http.Request) string {
	if t := r.Header.Get("X-Admin-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

func adminAuthed(r *http.Request) bool {
	tok := adminToken(r)
	if tok == "" {
		return false
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	exp, ok := adminSessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(adminSessions, tok)
		return false
	}
	return true
}

// ============================================================
//  管理端接口
// ============================================================

type adminLoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req adminLoginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	uOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(adminUser())) == 1
	pOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(adminPass())) == 1
	if !uOK || !pOK {
		log.Printf("[管理后台] 登录失败 IP=%s 用户名=%q", clientIP(r), req.Username)
		time.Sleep(400 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "账号或密码不正确")
		return
	}
	tok := newAdminSession()
	log.Printf("[管理后台] 登录成功 IP=%s", clientIP(r))
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": tok, "ttl": int(adminSessionTTL.Seconds())})
}

// adminStateHandler 管理端总览：策略 + 中奖名单
func adminStateHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !adminAuthed(r) {
		writeErr(w, http.StatusUnauthorized, "未登录或登录已过期")
		return
	}
	s := lucky.snapshot()
	// 名单里的昵称/学校用「当前值」而不是中奖那一刻的快照 ——
	// 用户后来在个人中心改了名字，这里要立刻跟着变
	for i := range s.Winners {
		if u := userByID(s.Winners[i].UserID); u != nil {
			s.Winners[i].Name = u.Name
			s.Winners[i].School = u.School
		}
	}
	sort.Slice(s.Winners, func(i, j int) bool { return s.Winners[i].No < s.Winners[j].No })
	if s.Winners == nil {
		s.Winners = []luckyWinner{}
	}
	rate := 0.0
	if s.Joined > 0 {
		rate = float64(s.Drawn) / float64(s.Joined) * 100
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"enabled":     s.Enabled,
		"probability": s.Probability, // 0~1
		"maxWinners":  s.MaxWinners,
		"drawn":       s.Drawn,
		"joined":      s.Joined,
		"round":       s.Round,
		"actualRate":  rate, // 实际中奖率（%）
		"winners":     s.Winners,
		"online":      hub.onlineCount(),
	})
}

type adminConfigReq struct {
	Enabled     *bool    `json:"enabled"`
	Probability *float64 `json:"probability"` // 0~1
	MaxWinners  *int     `json:"maxWinners"`
	Reset       bool     `json:"reset"`
}

func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !adminAuthed(r) {
		writeErr(w, http.StatusUnauthorized, "未登录或登录已过期")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req adminConfigReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	cur := lucky.snapshot()
	enabled, prob, maxW := cur.Enabled, cur.Probability, cur.MaxWinners
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if req.Probability != nil {
		prob = *req.Probability
	}
	if req.MaxWinners != nil {
		maxW = *req.MaxWinners
	}
	lucky.set(enabled, prob, maxW)
	if req.Reset {
		lucky.reset()
		log.Printf("[管理后台] 开始新一期：清空中奖名单")
	}
	log.Printf("[管理后台] 策略更新 enabled=%v 概率=%.1f%% 上限=%d", enabled, prob*100, maxW)
	adminStateHandler(w, r)
}

// adminPageHandler 把 /admin 直接映射到 web/admin.html
func adminPageHandler(staticDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, strings.TrimRight(staticDir, "/")+"/admin.html")
	}
}
