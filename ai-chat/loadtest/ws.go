package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxFrameSize 单帧上限。1000 人在线时 init/history 会带上完整在线列表（可达数百 KB），
// 所以这里给到 16MB，避免误判成坏帧。
const maxFrameSize = 16 << 20

// ============================================================
//  极简 WebSocket 客户端（手写 RFC6455，零第三方依赖）
//  只用到：文本帧、ping/pong、close、扩展长度
// ============================================================

type client struct {
	idx   int
	name  string
	token string
	conn  net.Conn
	br    *bufio.Reader
	wmu   sync.Mutex // 保证写帧原子性（burst 与 pong 可能并发）

	shutdown atomic.Bool
	readErr  atomic.Bool

	chatRecv  atomic.Int64 // 本连接收到的 type=msg 条数
	frames    atomic.Int64
	recvBytes atomic.Int64

	// AI 专项：欢迎语/回答是否人人收到、要等多久
	aiStart   atomic.Int64
	aiDelta   atomic.Int64
	aiEnd     atomic.Int64
	firstEnd  atomic.Int64 // 建连后收到的第一条 ai_end 的时间（unix nano）
	connectAt atomic.Int64 // 建连完成的时刻

	// 抽样客户端才开：测量「自己发的消息绕一圈回到自己」的延迟
	sampled   bool
	ownPrefix []byte
	sendAt    []atomic.Int64
	rttMu     sync.Mutex
	rtts      []time.Duration

	// 抽样客户端才开：配对 ai_start / ai_end，算单条 AI 消息的生成时长
	aiMu     sync.Mutex
	aiStarts map[int64]time.Time
	aiSpans  []time.Duration
	firstSeq []time.Duration // 建连 → 第一条 ai_end
}

// newClient 建一个客户端对象（sampled 的才分配延迟统计用的结构）
func newClient(idx int, name, token string, sampled bool, msgs int) *client {
	c := &client{idx: idx, name: name, token: token, sampled: sampled}
	if sampled {
		c.ownPrefix = []byte(fmt.Sprintf("压测消息 %d-", idx+1))
		c.sendAt = make([]atomic.Int64, msgs)
		c.aiStarts = make(map[int64]time.Time)
	}
	return c
}

// extractID 从 {"..."} 里取出 "id" 字段（只给 AI 帧用，抽样客户端才走）
func extractID(b []byte) int64 {
	i := bytes.Index(b, []byte(`"id":`))
	if i < 0 {
		return 0
	}
	j := i + 5
	for j < len(b) && (b[j] == ' ') {
		j++
	}
	k := j
	for k < len(b) && b[k] >= '0' && b[k] <= '9' {
		k++
	}
	if k == j {
		return 0
	}
	n, err := strconv.ParseInt(string(b[j:k]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// noteAIStart / noteAIEnd 抽样客户端配对一条 AI 消息的起止，算出生成时长
func (c *client) noteAIStart(id int64) {
	c.aiMu.Lock()
	c.aiStarts[id] = time.Now()
	c.aiMu.Unlock()
}

func (c *client) noteAIEnd(id int64) {
	now := time.Now()
	c.aiMu.Lock()
	if t, ok := c.aiStarts[id]; ok {
		c.aiSpans = append(c.aiSpans, now.Sub(t))
		delete(c.aiStarts, id)
	}
	c.aiMu.Unlock()

	// 建连 → 第一条 AI 欢迎语到达
	if c.firstEnd.CompareAndSwap(0, now.UnixNano()) {
		if at := c.connectAt.Load(); at > 0 {
			c.aiMu.Lock()
			c.firstSeq = append(c.firstSeq, now.Sub(time.Unix(0, at)))
			c.aiMu.Unlock()
		}
	}
}

// dialWS 完成 TCP/TLS 连接 + WebSocket 握手
func dialWS(base, token string, timeout time.Duration) (net.Conn, *bufio.Reader, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" || u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	var conn net.Conn
	dialer := &net.Dialer{Timeout: timeout}
	switch u.Scheme {
	case "https", "wss":
		conn, err = tls.DialWithDialer(dialer, "tcp", host, &tls.Config{ServerName: u.Hostname()})
	default:
		conn, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, nil, err
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, nil, err
	}
	path := "/ws?token=" + url.QueryEscape(token)
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(keyBytes) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"User-Agent: ai-chat-loadtest/1.0\r\n\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, nil, err
	}

	// 16KB 足够：大帧走 io.ReadFull 会绕过 bufio 直接读进目标缓冲，
	// 1000 个客户端时这一项就能省下 ~50MB
	br := bufio.NewReaderSize(conn, 16*1024)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	status = strings.TrimSpace(status)
	if !strings.Contains(status, "101") {
		conn.Close()
		return nil, nil, fmt.Errorf("握手失败: %s", status)
	}
	for { // 丢弃响应头
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{}) // 之后不设读超时，压测期间连接要一直挂着
	return conn, br, nil
}

// readFrame 读一帧。服务端发出的帧不带掩码，这里仍然兼容带掩码的情况。
// buf 复用，避免每帧分配内存（1000 万帧的规模下这点很关键）。
func (c *client) readFrame(buf *[]byte) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return 0, nil, err
	}
	opcode := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	n := int64(h[1] & 0x7f)

	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int64(binary.BigEndian.Uint64(b[:]))
	}
	if n < 0 || n > maxFrameSize {
		return 0, nil, fmt.Errorf("帧过大或非法: %d", n)
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	if int64(cap(*buf)) < n {
		*buf = make([]byte, n)
	}
	p := (*buf)[:n]
	if _, err := io.ReadFull(c.br, p); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range p {
			p[i] ^= mask[i%4]
		}
	}
	return opcode, p, nil
}

// writeFrame 发一帧（客户端发出的帧必须带掩码）
func (c *client) writeFrame(opcode byte, payload []byte) error {
	n := len(payload)
	var head [14]byte
	head[0] = 0x80 | opcode
	off := 2
	switch {
	case n <= 125:
		head[1] = 0x80 | byte(n)
	case n < 1<<16:
		head[1] = 0x80 | 126
		binary.BigEndian.PutUint16(head[2:], uint16(n))
		off = 4
	default:
		head[1] = 0x80 | 127
		binary.BigEndian.PutUint64(head[2:], uint64(n))
		off = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(head[off:], mask[:])
	off += 4

	buf := make([]byte, off+n)
	copy(buf, head[:off])
	for i := 0; i < n; i++ {
		buf[off+i] = payload[i] ^ mask[i%4]
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.conn.Write(buf)
	return err
}

// ============================================================
//  帧类型识别（不解析 JSON，避免 1000 万次反序列化把压测机自己压死）
//  服务端 json.Marshal(map) 的 key 是排好序的，一定存在 "type":"xxx"
// ============================================================

var (
	kType    = []byte(`"type":"`)
	tMsg     = []byte("msg")
	tAIStart = []byte("ai_start")
	tAIDelta = []byte("ai_delta")
	tAIEnd   = []byte("ai_end")
	tJoin    = []byte("join")
	tLeave   = []byte("leave")
	tInit    = []byte("init")
	tHist    = []byte("history")
	tOnline  = []byte("online")
	tErr     = []byte("error")
)

const (
	tcOther = iota
	tcMsg
	tcAIStart
	tcAIDelta
	tcAIEnd
	tcJoin
	tcLeave
	tcInit
	tcHistory
	tcOnline
	tcError
)

func hasTypeAt(s, name []byte) bool {
	return bytes.HasPrefix(s, name) && len(s) > len(name) && s[len(name)] == '"'
}

func frameTypeCode(b []byte) int {
	// 用 LastIndex：服务端 map 序列化时 key 按字典序排，外层 "type" 一定排在
	// 可能嵌套 type 的字段（history 的 messages、msg 的 user 等）之后。
	// 用 Index 会把 history 里嵌套的历史消息类型当成外层类型，统计就错了。
	i := bytes.LastIndex(b, kType)
	if i < 0 {
		return tcOther
	}
	s := b[i+len(kType):]
	if len(s) == 0 {
		return tcOther
	}
	switch s[0] {
	case 'm':
		if hasTypeAt(s, tMsg) {
			return tcMsg
		}
	case 'a':
		if hasTypeAt(s, tAIStart) {
			return tcAIStart
		}
		if hasTypeAt(s, tAIDelta) {
			return tcAIDelta
		}
		if hasTypeAt(s, tAIEnd) {
			return tcAIEnd
		}
	case 'j':
		if hasTypeAt(s, tJoin) {
			return tcJoin
		}
	case 'l':
		if hasTypeAt(s, tLeave) {
			return tcLeave
		}
	case 'i':
		if hasTypeAt(s, tInit) {
			return tcInit
		}
	case 'h':
		if hasTypeAt(s, tHist) {
			return tcHistory
		}
	case 'o':
		if hasTypeAt(s, tOnline) {
			return tcOnline
		}
	case 'e':
		if hasTypeAt(s, tErr) {
			return tcError
		}
	}
	return tcOther
}

// ============================================================
//  读循环
// ============================================================

func (c *client) readLoop(st *Stats) {
	var buf []byte
	for {
		opcode, payload, err := c.readFrame(&buf)
		if err != nil {
			c.readErr.Store(true)
			if !c.shutdown.Load() && err != io.EOF {
				st.ReadErr.Add(1)
				st.Disconnects.Add(1)
			} else if !c.shutdown.Load() {
				st.Disconnects.Add(1)
			}
			return
		}
		switch opcode {
		case 0x1: // text
			st.Frames.Add(1)
			st.Bytes.Add(int64(len(payload)))
			c.frames.Add(1)
			c.recvBytes.Add(int64(len(payload)))
			switch frameTypeCode(payload) {
			case tcMsg:
				st.ChatRecv.Add(1)
				c.chatRecv.Add(1)
				if c.sampled {
					c.checkOwnMessage(payload)
				}
			case tcAIStart:
				st.AIStart.Add(1)
				c.aiStart.Add(1)
				if c.sampled {
					if id := extractID(payload); id != 0 {
						c.noteAIStart(id)
					}
				}
			case tcAIDelta:
				st.AIDelta.Add(1)
				c.aiDelta.Add(1)
			case tcAIEnd:
				st.AIEnd.Add(1)
				c.aiEnd.Add(1)
				if c.sampled {
					c.noteAIEnd(extractID(payload))
				}
			case tcJoin:
				st.JoinRecv.Add(1)
			case tcLeave:
				st.LeaveRecv.Add(1)
			case tcInit:
				st.InitRecv.Add(1)
			case tcHistory:
				st.HistRecv.Add(1)
			case tcOnline:
				st.OnlineRecv.Add(1)
			case tcError:
				st.ErrRecv.Add(1)
			default:
				st.OtherRecv.Add(1)
			}
		case 0x9: // ping -> pong
			if err := c.writeFrame(0xA, payload); err == nil {
				st.Pongs.Add(1)
			}
		case 0x8: // close
			if !c.shutdown.Load() {
				st.Disconnects.Add(1)
			}
			return
		}
	}
}

// checkOwnMessage 抽样客户端用：从消息文本里认出自己发的那条，算出回环延迟
func (c *client) checkOwnMessage(payload []byte) {
	i := bytes.Index(payload, c.ownPrefix)
	if i < 0 {
		return
	}
	j := i + len(c.ownPrefix)
	k := j
	for k < len(payload) && payload[k] >= '0' && payload[k] <= '9' {
		k++
	}
	if k == j {
		return
	}
	seq, err := strconv.Atoi(string(payload[j:k]))
	if err != nil || seq < 1 || seq > len(c.sendAt) {
		return
	}
	sent := c.sendAt[seq-1].Load()
	if sent == 0 {
		return
	}
	d := time.Since(time.Unix(0, sent))
	c.rttMu.Lock()
	c.rtts = append(c.rtts, d)
	c.rttMu.Unlock()
}

// burst 开始发消息：等待 start 信号后把 msgs 条发出去。
// interval=0 表示无间隔（极限压满）；>0 则模拟真人打字节奏。
func (c *client) burst(start <-chan struct{}, msgs int, interval time.Duration, st *Stats) {
	<-start
	for j := 0; j < msgs; j++ {
		text := fmt.Sprintf("压测消息 %d-%d", c.idx+1, j+1)
		body, _ := json.Marshal(text)
		payload := make([]byte, 0, len(body)+22)
		payload = append(payload, `{"type":"msg","text":`...)
		payload = append(payload, body...)
		payload = append(payload, '}')

		if c.sampled {
			c.sendAt[j].Store(time.Now().UnixNano())
		}
		if err := c.writeFrame(0x1, payload); err != nil {
			st.SendFail.Add(1)
			return
		}
		st.Sent.Add(1)
		if interval > 0 && j < msgs-1 {
			time.Sleep(interval)
		}
	}
}
