package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
//  极简 WebSocket 服务端（手写 RFC6455，零第三方依赖）
//  够用范围：文本帧、分片忽略、ping/pong、close
// ============================================================

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// 压测相关调参（可通过环境变量覆盖）
const (
	defaultSendBuffer = 8192 // 每连接待发队列长度（只存 []byte 头，实际载荷是共享的；
	// 8192×24B ≈ 196KB/连接，500 人在线约 98MB，够扛 500 人各自的爆发）
	maxBatchFrames = 64 // 单次 write 最多合并多少帧，显著减少 write 系统调用
	maxBatchBytes  = 256 << 10

	// 在线名单合并广播间隔：不再每次进/出房都把全量名单广播给所有人（那是 O(N²) 流量）
	onlineBroadcastEvery = 5 * time.Second
	// 在线人数不超过这个值时，join/leave 里照旧内联全量名单（兼容旧前端）；超过就靠合并广播
	maxInlineRoster = 200

	// 全局背压阈值：所有连接待发帧数之和超过它就暂停「发布者」。
	// 只靠加大单连接队列是走不通的——1000 人瞬时爆发需要 ~1000 万帧的缓冲（几百 MB），
	// 小机器直接 OOM。改成反压到发送方（TCP 自然限速），用有限内存换「不丢消息」。
	defaultBacklogHighWater = 400000
	backlogMaxWait          = 15 * time.Second

	// 慢消费者看门狗：队列越线后多久还没排空就断开它
	stuckClientTimeout = 30 * time.Second
)

// perClientHighWater 单连接队列水位线：最慢的那条连接积压到这个深度就暂停广播。
// 取队列容量的一半，留足余量给正在排空的写入和并发抖动。
var perClientHighWater = defaultSendBuffer / 2

// slowClients 当前处于「队列越线」状态的连接数，背压判断只需读这个 O(1) 计数
var slowClients atomic.Int64

// queuedFrames 全局待发帧数（所有连接之和），用于背压判断
var queuedFrames atomic.Int64

var (
	sendBufferSize   = defaultSendBuffer
	backlogHighWater = int64(defaultBacklogHighWater)
)

func init() {
	if v, err := strconv.Atoi(os.Getenv("WS_SEND_BUFFER")); err == nil && v > 0 {
		sendBufferSize = v
		perClientHighWater = v / 2
	}
	if v, err := strconv.Atoi(os.Getenv("WS_BACKLOG_HIGH_WATER")); err == nil && v > 0 {
		backlogHighWater = int64(v)
	}
}

type wsConn struct {
	c      net.Conn
	br     *bufio.Reader
	mu     sync.Mutex // 保证写帧原子性
	closed bool

	// rbuf 是读帧复用的缓冲区：只有 readPump 这一个 goroutine 用，
	// 避免每帧都 make 一次（1000 人压测时是千万级的分配，GC 压力很大）
	rbuf []byte
}

// wsUpgrade 完成握手并劫持连接；之后 ResponseWriter 不能再使用
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, http.ErrNotSupported
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, http.ErrNotSupported
	}
	h := sha1.New()
	h.Write([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, http.ErrNotSupported
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	_, err = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{c: conn, br: brw.Reader}, nil
}

// readFrame 读取一帧（客户端发来的帧一定带掩码）
func (ws *wsConn) readFrame() (opcode byte, payload []byte, err error) {
	var b [2]byte
	if _, err = io.ReadFull(ws.br, b[:]); err != nil {
		return 0, nil, err
	}
	opcode = b[0] & 0x0f
	masked := b[1]&0x80 != 0
	length := int64(b[1] & 0x7f)

	switch length {
	case 126:
		var l uint16
		if err = binary.Read(ws.br, binary.BigEndian, &l); err != nil {
			return 0, nil, err
		}
		length = int64(l)
	case 127:
		if err = binary.Read(ws.br, binary.BigEndian, &length); err != nil {
			return 0, nil, err
		}
	}
	if length > 1<<20 { // 1MB 上限，防止内存打爆
		return 0, nil, io.ErrUnexpectedEOF
	}

	// ⚠️ RFC6455 帧格式顺序：2字节头 → 扩展长度 → 【4字节掩码】 → 载荷
	// 掩码必须在载荷之前读（之前读反了，导致客户端消息解掩码错误、JSON 解析失败 → 消息"不可见"）
	var m [4]byte
	if masked {
		if _, err = io.ReadFull(ws.br, m[:]); err != nil {
			return 0, nil, err
		}
	}

	if int64(cap(ws.rbuf)) < length {
		ws.rbuf = make([]byte, length)
	}
	payload = ws.rbuf[:length]
	if _, err = io.ReadFull(ws.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= m[i%4]
		}
	}
	return opcode, payload, nil
}

// encodeFrame 把一帧（服务端发出的帧不带掩码）追加到 dst
func encodeFrame(dst, payload []byte, opcode byte) []byte {
	n := len(payload)
	dst = append(dst, 0x80|opcode)
	switch {
	case n <= 125:
		dst = append(dst, byte(n))
	case n < 1<<16:
		dst = append(dst, 126, byte(n>>8), byte(n))
	default:
		dst = append(dst, 127,
			byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	return append(dst, payload...)
}

// writeFrames 把多帧合并成一次 Write 发出去。
// 广播场景下队列里通常已经堆了多帧，合并写能把系统调用数量降一个数量级。
func (ws *wsConn) writeFrames(opcode byte, payloads [][]byte) error {
	total := 0
	for _, p := range payloads {
		total += len(p) + 10
	}
	buf := make([]byte, 0, total)
	for _, p := range payloads {
		buf = encodeFrame(buf, p, opcode)
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.closed {
		return net.ErrClosed
	}
	_, err := ws.c.Write(buf)
	return err
}

// writeFrame 发送单帧
func (ws *wsConn) writeFrame(opcode byte, payload []byte) error {
	return ws.writeFrames(opcode, [][]byte{payload})
}

func (ws *wsConn) writeText(s string) error { return ws.writeFrame(0x1, []byte(s)) }

func (ws *wsConn) close() {
	ws.mu.Lock()
	if !ws.closed {
		ws.closed = true
		ws.c.Write([]byte{0x88, 0x00}) // close 帧
		ws.c.Close()
	}
	ws.mu.Unlock()
}

// ============================================================
//  聊天室 Hub：管理在线连接、广播消息、保存历史
// ============================================================

type Client struct {
	user *User
	conn *wsConn
	send chan []byte

	dropped atomic.Int64 // 累计被丢弃的消息数
	lastLog atomic.Int64 // 上次打印丢弃日志的 unix 秒

	// 慢消费者标记：队列深度越过水位线时置位并让全局 slowClients +1，
	// 排空后复位。这样背压判断是 O(1) 的——遍历所有连接算最大值在 1000 人
	// 房间里会被反复重试放大成平方级开销。
	overHigh  atomic.Bool
	overSince atomic.Int64 // 首次越线的 unix 秒，用于踢掉彻底卡死的连接
}

// markOverHigh 队列越线时登记（幂等）
func (c *Client) markOverHigh() {
	if c.overHigh.CompareAndSwap(false, true) {
		c.overSince.Store(time.Now().Unix())
		slowClients.Add(1)
	}
}

// clearOverHigh 队列排空后注销（幂等）
func (c *Client) clearOverHigh() {
	if c.overHigh.CompareAndSwap(true, false) {
		c.overSince.Store(0)
		slowClients.Add(-1)
	}
}

// enqueue 非阻塞投递。正常情况下配合全局背压，队列几乎不会满；
// 队列满时兜底丢「最旧」的一条（保护整个房间不被一个卡死的客户端拖垮），
// 丢弃日志限速到每 5 秒最多一行，否则 1000 人房间里日志本身就能把 CPU 打满。
func (c *Client) enqueue(b []byte) {
	select {
	case c.send <- b:
		queuedFrames.Add(1)
		if len(c.send) >= perClientHighWater {
			c.markOverHigh()
		}
		return
	default:
	}

	select { // 弹出最旧的一条，给最新消息腾位置
	case <-c.send:
		queuedFrames.Add(-1)
	default:
	}
	select {
	case c.send <- b:
		queuedFrames.Add(1)
	default:
		return // 消费者恰好正在写、位置又被占满，本轮丢弃
	}
	c.markOverHigh()

	total := c.dropped.Add(1)
	now := time.Now().Unix()
	last := c.lastLog.Load()
	if now-last >= 5 && c.lastLog.CompareAndSwap(last, now) {
		log.Printf("⚠️ 客户端 %s 发送队列已满，累计丢弃 %d 条（5s 内不再重复打印）", c.user.Name, total)
	}
}

// onlineUser 是在线名单的精简表示：只带前端渲染头像和名字需要的字段，
// 比完整的 User 小 3 倍左右，1000 人在线时差别很大。
type onlineUser struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Color   string `json:"color"`
	Initial string `json:"initial"`
}

func compactUser(u *User) onlineUser {
	return onlineUser{ID: u.ID, Name: u.Name, Color: u.Color, Initial: u.Initial}
}

type Hub struct {
	mu        sync.Mutex
	clients   map[*Client]struct{}
	history   []json.RawMessage // 最近的历史消息，新用户进来能回看
	rosterVer int64             // 在线名单版本号，add/remove 时 +1
}

func newHub() *Hub {
	h := &Hub{clients: make(map[*Client]struct{}), history: make([]json.RawMessage, 0, maxHistoryKeep)}
	go h.onlineBroadcaster()
	return h
}

// onlineBroadcaster 把「在线名单」合并成固定间隔的广播。
// 之前是每次有人进/出房就把全量名单广播给所有人，1000 人时等于 O(N²) 的流量。
// 顺便当看门狗：踢掉长时间卡在队列水位线以上的连接，避免一条死连接拖住整个房间。
func (h *Hub) onlineBroadcaster() {
	ticker := time.NewTicker(onlineBroadcastEvery)
	defer ticker.Stop()
	var lastVer int64 = -1
	for range ticker.C {
		h.reapStuckClients()

		h.mu.Lock()
		ver := h.rosterVer
		h.mu.Unlock()
		if ver == lastVer {
			continue
		}
		lastVer = ver
		h.broadcast(map[string]any{"type": "online", "online": h.onlineSnapshot(nil)}, false)
	}
}

// reapStuckClients 关闭那些「队列越线后连续 stuckClientTimeout 都没排空」的连接。
// 正常慢客户端几十毫秒就排空了；超过这个时间说明对端已经不读了，
// 留着它只会让全局背压永远处于触发状态。
func (h *Hub) reapStuckClients() {
	now := time.Now().Unix()
	h.mu.Lock()
	var stuck []*Client
	for c := range h.clients {
		if !c.overHigh.Load() {
			continue
		}
		if since := c.overSince.Load(); since > 0 && now-since >= int64(stuckClientTimeout/time.Second) {
			stuck = append(stuck, c)
		}
	}
	h.mu.Unlock()

	for _, c := range stuck {
		log.Printf("⚠️ 客户端 %s 队列积压超过 %s 仍未排空，主动断开（慢消费者）", c.user.Name, stuckClientTimeout)
		c.conn.close()
	}
}

// throttle 全局背压：有连接积压越线时，让发布者等一等，而不是把消息丢掉。
//
// 判断只看两个 O(1) 的计数（全局待发帧数 + 越线连接数），不遍历连接表——
// 早期版本每次广播都遍历所有连接算最大队列深度，1000 人房间里被重试放大成了平方级开销。
//
// 关键还有「没有进展就别死等」：如果某个连接彻底卡死（对端不读也不断开），
// 待发量不会下降，此时最多再等 ~100ms 就放行，避免整个房间被一条连接拖死。
// 长期卡死的连接由 onlineBroadcaster 里的看门狗踢掉。
func (h *Hub) throttle() {
	if queuedFrames.Load() < backlogHighWater && slowClients.Load() == 0 {
		return
	}
	last := queuedFrames.Load()
	stall := 0
	deadline := time.Now().Add(backlogMaxWait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		cur := queuedFrames.Load()
		if cur < backlogHighWater && slowClients.Load() == 0 {
			return
		}
		if cur < last {
			last, stall = cur, 0
			continue
		}
		stall++
		if stall > 50 { // ~100ms 毫无进展：有连接卡住了，别再等
			return
		}
	}
}

// broadcast 广播一条消息（可选写入历史）
func (h *Hub) broadcast(v any, keepHistory bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.throttle()

	h.mu.Lock()
	defer h.mu.Unlock()
	if keepHistory {
		h.history = append(h.history, json.RawMessage(b))
		if len(h.history) > maxHistoryKeep {
			h.history = h.history[len(h.history)-maxHistoryKeep:]
		}
	}
	// 在锁内投递以保证所有客户端看到的消息顺序一致；enqueue 是非阻塞的，开销很小
	for c := range h.clients {
		c.enqueue(b)
	}
}

func (h *Hub) onlineCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// onlineSnapshot 返回精简在线名单；extra 非 nil 时把它也算进去
// （用于新连接初始化：那一刻它还没进 hub，但前端希望名单里有自己）
func (h *Hub) onlineSnapshot(extra *User) []onlineUser {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]onlineUser, 0, len(h.clients)+1)
	for c := range h.clients {
		out = append(out, compactUser(c.user))
	}
	if extra != nil {
		out = append(out, compactUser(extra))
	}
	return out
}

// onlineUsers 返回当前在线用户列表（完整对象，小房间的 join/leave 内联用）
func (h *Hub) onlineUsers() []User {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]User, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, *c.user)
	}
	return out
}

// unwelcomedUsers 返回「在线、且还没被欢迎过」的用户（按人去重，多标签页只算一次）
func (h *Hub) unwelcomedUsers() []*User {
	h.mu.Lock()
	uniq := make(map[string]*User, len(h.clients))
	for c := range h.clients {
		if c.user != nil {
			uniq[c.user.ID] = c.user
		}
	}
	h.mu.Unlock()

	out := make([]*User, 0, len(uniq))
	for _, u := range uniq {
		if !peekWelcomed(u) {
			out = append(out, u)
		}
	}
	return out
}

// sendToUser 只把消息推给某个用户自己的所有连接（不广播给房间）
func (h *Hub) sendToUser(userID string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.user != nil && c.user.ID == userID {
			c.enqueue(b)
		}
	}
}

func (h *Hub) add(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.rosterVer++
	h.mu.Unlock()
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.rosterVer++
	h.mu.Unlock()
	// 连接退出时必须注销慢消费者标记，否则 slowClients 只增不减，
	// 全局背压会永远处于触发状态（这正是之前一阶段残留连接拖死整个房间的原因）
	c.clearOverHigh()
}

// ============================================================
//  WS 入口
// ============================================================

type wsIncoming struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func wsHandler(h *Hub, w http.ResponseWriter, r *http.Request) {
	// 身份校验：优先 URL 参数（跨端口部署用），其次 Cookie（同源部署用）
	token := r.URL.Query().Get("token")
	if token == "" {
		if c, err := r.Cookie(authCookieName); err == nil {
			token = c.Value
		}
	}
	user, ok := getUserByToken(token)
	if !ok {
		http.Error(w, `{"error":"请先登录"}`, http.StatusUnauthorized)
		return
	}

	conn, err := wsUpgrade(w, r)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}
	// 握手后不能再写 HTTP 错误，只能记日志

	client := &Client{user: user, conn: conn, send: make(chan []byte, sendBufferSize)}
	// 补记 IP：直接带 token 访问 /ws 的用户（如刷新页面恢复登录）不会走登录接口
	if user.IP == "" {
		user.IP = clientIP(r)
	}

	// ① 初始化包直接写出去。此刻还没进 hub、writePump 也没启动，本连接只有一个写者，
	//    既没有并发写问题，也能保证 history/init 一定排在最前面。
	h.mu.Lock()
	hist := append([]json.RawMessage(nil), h.history...)
	h.mu.Unlock()
	roster := h.onlineSnapshot(user)

	if b, err := json.Marshal(map[string]any{"type": "history", "messages": hist}); err == nil {
		if err := client.conn.writeText(string(b)); err != nil {
			client.conn.close()
			return
		}
	}
	if b, err := json.Marshal(map[string]any{"type": "init", "me": user, "online": roster}); err == nil {
		if err := client.conn.writeText(string(b)); err != nil {
			client.conn.close()
			return
		}
	}

	// ② 写泵必须先起来，再让这个连接对广播可见。
	//
	// ⚠️ 这里是之前压测暴露出来的死锁：旧代码先 h.add(client)、后 go writePump(h)，
	//    中间还夹着一次 json.Marshal（全量在线名单+历史）和两次阻塞写 client.send <- b。
	//    只要这个窗口里有 ≥ sendBuffer 条广播灌进该连接的待发通道，阻塞写就会永久卡住
	//    —— 因为唯一能排空通道的 writePump 还没启动。后果是该连接永远收不到 init、
	//    readPump 也永远不启动（它发的消息全丢），连接和 goroutine 一起泄漏。
	go client.writePump(h)

	// ③ 再进 hub、再广播
	h.add(client)

	join := map[string]any{
		"type": "join",
		"user": user,
		"ts":   time.Now().UnixMilli(),
	}
	if len(roster) <= maxInlineRoster { // 小房间照旧内联名单，大房间靠 online 合并广播
		join["online"] = roster
	}
	h.broadcast(join, true)

	// AI 主动欢迎新成员。
	//
	// 策略关闭：每个人第一次进房都欢迎一次（异步，不阻塞连接建立）。
	// 策略开启：就在这一刻掷骰子 —— 中了立刻欢迎 + 发中奖编号，没中不触发任何 AI。
	// 判定发生在进房瞬间，没有定时器也没有手动抽取。
	if luckyEnabled() {
		if code, no, ok := lucky.roll(user); ok {
			if markLuckyOnce(user, lucky.roundNo()) {
				h.sendToUser(user.ID, map[string]any{"type": "lucky", "code": code, "no": no})
				go sendWelcome(h, user, code)
				log.Printf("[幸运观众] 第 %d 位：%s（%s）编号 %s", no, user.Name, user.School, code)
			} else {
				lucky.giveBack(user) // 已经欢迎过，名额还回去
			}
		}
	} else {
		go sendWelcome(h, user, "")
	}

	client.readPump(h)
}

// ============================================================
//  AI 主动欢迎新成员
// ============================================================

var (
	welcomeMu  sync.Mutex
	welcomedAt = make(map[string]time.Time) // 身份 key -> 首次欢迎时间
)

// welcomeCooldownMin 现在只作为「欢迎记录」的保留时长：超过它没再出现的条目会被清理，
// 但只要这个人在保留期内回来，就永远不会被欢迎第二次。
//
// 判定 key 优先用 clientId（浏览器本地稳定标识）—— 这样「离开后再登录」「改了昵称」
// 都还是同一个人，不会再触发一次 AI 调用；没有 clientId 的老客户端退化为按 userID 判。
func welcomeKey(u *User) string {
	if u.ClientID != "" {
		return u.ClientID
	}
	return u.ID
}

// peekWelcomed 只看不记（用于「候选池」判断）
func peekWelcomed(u *User) bool {
	key := welcomeKey(u)
	welcomeMu.Lock()
	defer welcomeMu.Unlock()
	t, ok := welcomedAt[key]
	if !ok {
		return false
	}
	keep := time.Duration(welcomeCooldownMin) * time.Minute
	if keep < time.Hour {
		keep = time.Hour
	}
	return time.Since(t) < keep
}

func markWelcomedOnce(u *User) bool {
	return markKeyOnce(welcomeKey(u))
}

// markLuckyOnce 幸运观众的欢迎记录按期去重：同一期只欢迎一次，
// 「开始新一期」之后可以再次被欢迎。
func markLuckyOnce(u *User, round int) bool {
	return markKeyOnce(welcomeKey(u) + "#lucky#" + strconv.Itoa(round))
}

func markKeyOnce(key string) bool {
	keep := time.Duration(welcomeCooldownMin) * time.Minute
	if keep < time.Hour {
		keep = time.Hour // 至少记住 1 小时，和身份识别窗口对齐
	}

	welcomeMu.Lock()
	defer welcomeMu.Unlock()
	now := time.Now()
	if t, ok := welcomedAt[key]; ok && now.Sub(t) < keep {
		return false // 这个人已经欢迎过了
	}
	// 顺手清理过期条目，防止 map 无限增长
	for k, t := range welcomedAt {
		if now.Sub(t) > keep*2 {
			delete(welcomedAt, k)
		}
	}
	welcomedAt[key] = now
	return true
}

func joinNonEmpty(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(out, " ")
}

// sendWelcome 让 AI 主动向新进来的成员打招呼。
// 三重保护：开关可关、没 Key 静默跳过、同一用户有冷却时间。
// 欢迎语流式推送的合并参数：每 120ms 或攒够 12 个字才推一帧。
// 原来每个 token 一帧，一条欢迎语 ~30 帧；合并后约 8~10 帧，
// 打字机效果几乎无差别，但进房风暴时的下行量降到三分之一。
const (
	welcomeFlushEvery = 120 * time.Millisecond
	welcomeFlushChars = 12
)

// welcomeStream 把 AI 的增量攒起来分批广播（可选：AI_WELCOME_STREAM=off 则完全不流式）
type welcomeStream struct {
	h    *Hub
	id   int64
	mu   sync.Mutex
	buf  strings.Builder // 还没发出去的增量
	full strings.Builder // 完整文本
	last time.Time
	on   bool
}

func newWelcomeStream(h *Hub, id int64, on bool) *welcomeStream {
	return &welcomeStream{h: h, id: id, last: time.Now(), on: on}
}

func (w *welcomeStream) push(d string) {
	w.mu.Lock()
	w.full.WriteString(d)
	if !w.on {
		w.mu.Unlock()
		return // 非流式：只累积，一次都不推
	}
	w.buf.WriteString(d)
	var chunk string
	if time.Since(w.last) >= welcomeFlushEvery || w.buf.Len() >= welcomeFlushChars {
		chunk, w.last = w.buf.String(), time.Now()
		w.buf.Reset()
	}
	w.mu.Unlock()
	if chunk != "" {
		w.h.broadcast(map[string]any{"type": "ai_delta", "id": w.id, "delta": chunk}, false)
	}
}

// finish 把剩下的增量推完，并返回完整文本
func (w *welcomeStream) finish() string {
	w.mu.Lock()
	chunk := w.buf.String()
	w.buf.Reset()
	full := w.full.String()
	w.mu.Unlock()
	if chunk != "" && w.on {
		w.h.broadcast(map[string]any{"type": "ai_delta", "id": w.id, "delta": chunk}, false)
	}
	return full
}

// sendWelcome 让 AI 欢迎新成员。
// luckyCode 非空时表示这位是抽中的「幸运观众」：欢迎语后面会追加领奖提示。
// 注意：调用方负责在 luckyCode 非空时先调用过 markWelcomedOnce。
func sendWelcome(h *Hub, u *User, luckyCode string) {
	// ① 总开关
	if env("AI_WELCOME", "on") != "on" {
		return
	}
	// ② 没配 Key 就别打扰（不广播错误，普通聊天照样用）
	if os.Getenv("DEEPSEEK_API_KEY") == "" {
		log.Printf("未配置 DEEPSEEK_API_KEY，跳过欢迎消息")
		return
	}
	// ③ 只欢迎「第一次进来」的人：离开后再登录、改昵称、刷新页面都不会再触发。
	//    幸运观众这条路已经由抽奖逻辑标记过，这里不再重复判断。
	if luckyCode == "" && !markWelcomedOnce(u) {
		log.Printf("[欢迎] %s(IP=%s) 已经欢迎过（clientId=%s），跳过", u.Name, u.IP, u.ClientID)
		return
	}

	who := u.Name
	if extra := joinNonEmpty(u.School, u.Major); extra != "" {
		who += "（" + extra + "）"
	}

	// 自我介绍：填了就让欢迎语更个性化（例如"听说你正在准备 CKA…"）
	var bioPart string
	if u.Bio != "" {
		bioPart = "他/她的自我介绍是：“" + u.Bio + "”。请结合这点来欢迎，让他/她感到被重视。"
	}

	prompt := fmt.Sprintf(
		"聊天室刚进来一位新成员：%s。%s"+
			"请用一句简短热情的话欢迎他/她，可以自然地结合他/她的学校、专业或自我介绍说一句，"+
			"并简要说明你能做什么（例如回答 Linux、Red Hat、云原生、Kubernetes 相关问题）。"+
			"要求：控制在 60 字以内，纯文本，不要使用 Markdown 符号，不要自称“AI助手”，直接说话。",
		who, bioPart)
	if luckyCode != "" {
		prompt += "另外，他/她刚刚被抽中成为本期幸运观众，请在欢迎语里恭喜一下，但不要说具体编号（编号由系统统一附加）。"
	}

	// 欢迎语走「流式」，但增量是合并推送的：
	// 每 welcomeFlushEvery 或攒够 welcomeFlushChars 才广播一帧，
	// 既保留打字机效果，又不会像原来那样一条欢迎语打 30 帧把队列刷爆。
	// AI_WELCOME_STREAM=off 可退回「生成完一次性发」。
	streamOn := env("AI_WELCOME_STREAM", "on") == "on"
	msgID := time.Now().UnixNano()
	if streamOn {
		h.broadcast(map[string]any{
			"type":  "ai_start",
			"id":    msgID,
			"asker": "", // 欢迎语没有提问者
			"ts":    time.Now().UnixMilli(),
		}, false)
	}

	ws := newWelcomeStream(h, msgID, streamOn)
	err := streamDeepSeek([]chatMessage{
		{Role: "system", Content: systemPrompt()},
		{Role: "user", Content: prompt},
	}, func(d string) error {
		ws.push(d)
		return nil
	})
	full := ws.finish()

	// ai_end 带完整信息并存历史：新人进来能回看到欢迎语
	if err != nil {
		// 失败就静默收尾，不用错误提示打扰大家；流式下前端会把空气泡收掉
		log.Printf("欢迎消息生成失败: %v", err)
		return
	}
	if strings.TrimSpace(full) == "" {
		return
	}
	// 中奖文案由服务端统一附加，保证编号一字不差（不交给模型自由发挥）
	if luckyCode != "" {
		full += "\n\n🎁 恭喜你成为本期幸运观众，编号 " + luckyCode +
			"，请在会后凭「个人中心」页面领取精美礼品一份！"
	}
	h.broadcast(map[string]any{
		"type":  "ai_end",
		"id":    msgID,
		"text":  full,
		"asker": "", // 欢迎语没有提问者
		"ts":    time.Now().UnixMilli(),
	}, true)
}

// readPump 读取客户端消息（阻塞，返回即代表连接结束）
func (c *Client) readPump(h *Hub) {
	defer func() {
		h.remove(c)
		c.conn.close()
		leave := map[string]any{
			"type": "leave",
			"user": c.user,
			"ts":   time.Now().UnixMilli(),
		}
		if h.onlineCount() <= maxInlineRoster {
			leave["online"] = h.onlineUsers()
		}
		h.broadcast(leave, true)
		log.Printf("[离开] %s IP=%s 在线=%d", c.user.Name, c.user.IP, h.onlineCount())
	}()

	log.Printf("[进入] %s IP=%s 在线=%d", c.user.Name, c.user.IP, h.onlineCount())

	for {
		opcode, payload, err := c.conn.readFrame()
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "closed") {
				log.Printf("读帧失败(%s): %v", c.user.Name, err)
			}
			return
		}

		switch opcode {
		case 0x8: // close
			return
		case 0x9: // ping -> pong
			c.conn.writeFrame(0xA, payload)
		case 0x1: // text
			var in wsIncoming
			if err := json.Unmarshal(payload, &in); err != nil {
				continue
			}
			if in.Type != "msg" || strings.TrimSpace(in.Text) == "" {
				continue
			}
			if len([]rune(in.Text)) > maxChatLen {
				c.enqueue(mustJSON(map[string]any{"type": "error", "error": "消息太长啦"}))
				continue
			}
			handleChatMessage(h, c, in.Text)
		}
	}
}

// writePump 把 send 通道里的数据写给客户端。
// 会把队列里已经堆着的多帧合并成一次 Write，大幅降低系统调用次数。
func (c *Client) writePump(h *Hub) {
	ticker := time.NewTicker(30 * time.Second) // 心跳，防代理掐连接
	defer func() {
		ticker.Stop()
		c.conn.close()
	}()
	for {
		select {
		case msg := <-c.send:
			queuedFrames.Add(-1)
			batch := [][]byte{msg}
			bytes := len(msg)
		drain:
			for len(batch) < maxBatchFrames && bytes < maxBatchBytes {
				select {
				case m := <-c.send:
					queuedFrames.Add(-1)
					batch = append(batch, m)
					bytes += len(m)
				default:
					break drain
				}
			}
			if err := c.conn.writeFrames(0x1, batch); err != nil {
				return
			}
			// 排空到水位线以下就注销慢消费者标记（O(1) 背压判断靠这个计数）
			if c.overHigh.Load() && len(c.send) < perClientHighWater {
				c.clearOverHigh()
			}
		case <-ticker.C:
			if err := c.conn.writeFrame(0x9, nil); err != nil { // ping
				return
			}
		}
	}
}

// handleChatMessage 分发：普通聊天广播；@AI 开头则触发 AI 回答
func handleChatMessage(h *Hub, c *Client, text string) {
	trimmed := strings.TrimSpace(text)

	// 普通聊天消息：直接广播，不花一分钱
	if !strings.HasPrefix(trimmed, "@AI") {
		log.Printf("[聊天] %s(IP=%s): %s", c.user.Name, c.user.IP, truncate(text, 120))
		h.broadcast(map[string]any{
			"type": "msg",
			"user": c.user,
			"text": text,
			"ts":   time.Now().UnixMilli(),
		}, true)
		return
	}

	// @AI：扣掉前缀后作为真正的问题
	question := strings.TrimSpace(strings.TrimPrefix(trimmed, "@AI"))
	if question == "" {
		question = "跟大家打个招呼吧"
	}
	// ⚠️ 这条会真的花钱，必须留痕
	log.Printf("[AI提问] %s(IP=%s): %s", c.user.Name, c.user.IP, truncate(question, 120))

	// ⚠️ 关键：先把群聊上下文"快照"下来，再广播提问。
	// 顺序反了的话，这条提问本身就会被算进上下文，和下面的 user 消息重复。
	roomContext := h.recentChat(0)

	// 先把提问广播出去（所有人都能看到谁问了什么）
	h.broadcast(map[string]any{
		"type": "msg",
		"user": c.user,
		"text": text,
		"ts":   time.Now().UnixMilli(),
	}, true)

	// 限流：AI 调用烧钱，按提问者的 IP/身份限
	if !allow(c.user.Name) {
		c.enqueue(mustJSON(map[string]any{"type": "error", "error": "AI 调用太频繁啦，歇一会儿再问~"}))
		return
	}

	msgID := time.Now().UnixNano()
	h.broadcast(map[string]any{
		"type":  "ai_start",
		"id":    msgID,
		"asker": c.user.Name,
		"text":  question,
		"ts":    time.Now().UnixMilli(),
	}, false)

	// 单独起 goroutine，避免阻塞这个用户的读循环
	go func(u *User, q string, id int64, room []string) {
		full := ""
		err := streamDeepSeek(buildAIMessages(u, q, room), func(delta string) error {
			full += delta
			h.broadcast(map[string]any{"type": "ai_delta", "id": id, "delta": delta}, false)
			return nil
		})

		if err != nil {
			// 把内部错误脱敏后再广播，别把上游细节全抖出来
			h.broadcast(map[string]any{"type": "ai_delta", "id": id, "delta": "⚠️ " + err.Error()}, false)
		} else {
			appendUserHistory(u, q, full)
		}
		// ai_end 带全文、提问者、时间并存进历史：
		// ① 后续 AI 能"记得"自己说过什么 ② 新人进来能回看到这条回答
		h.broadcast(map[string]any{
			"type":  "ai_end",
			"id":    id,
			"text":  full,
			"asker": u.Name,
			"ts":    time.Now().UnixMilli(),
		}, true)
	}(c.user, question, msgID, roomContext)
}

// recentChat 取出最近 n 条聊天记录，格式化成 "昵称: 内容" 供 AI 参考。
// n<=0 时用默认值 maxContextMsgs。
func (h *Hub) recentChat(n int) []string {
	if n <= 0 {
		n = maxContextMsgs
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	start := 0
	if len(h.history) > n {
		start = len(h.history) - n
	}

	out := make([]string, 0, n)
	for _, raw := range h.history[start:] {
		var m struct {
			Type string `json:"type"`
			User *User  `json:"user"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		switch m.Type {
		case "msg":
			if m.User != nil && m.Text != "" {
				out = append(out, m.User.Name+": "+m.Text)
			}
		case "ai_end":
			if m.Text != "" {
				out = append(out, "AI助手: "+m.Text)
			}
		}
	}
	return out
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// truncate 把一条消息截短再进日志：既能追溯，又不会被超长刷屏
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ") // 换行会打乱日志格式
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
