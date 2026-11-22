package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================
//  AI 聊天室压测脚本
//
//  行为：登录 N 个用户（默认 压测1..压测1000）→ 每人开一条 WebSocket
//        → 全部连上后，每人以「无间隔」的方式连发 M 条普通聊天消息
//        → 等消息分发干净，输出延迟 / 吞吐 / 送达率报告
//
//  普通聊天消息不带 @AI 前缀，不会触发 DeepSeek 调用。
// ============================================================

type Options struct {
	Base            string
	Users           int
	Msgs            int
	NamePrefix      string
	School          string
	Major           string
	LoginParallel   int
	ConnectParallel int
	Samples         int
	Interval        time.Duration
	Settle          time.Duration
	DrainQuiet      time.Duration
	DrainTimeout    time.Duration
	DialTimeout     time.Duration
	WaitOnline      time.Duration
	Out             string
	Label           string
}

type Report struct {
	Label string `json:"label"`
	Base  string `json:"base"`
	Users int    `json:"users"`
	Msgs  int    `json:"msgs_each"`
	Start string `json:"start_time"`
	End   string `json:"end_time"`

	HealthBefore string `json:"health_before"`
	HealthAfter  string `json:"health_after"`

	LoginOK   int64   `json:"login_ok"`
	LoginFail int64   `json:"login_fail"`
	LoginP50  string  `json:"login_p50"`
	LoginP90  string  `json:"login_p90"`
	LoginP99  string  `json:"login_p99"`
	LoginMax  string  `json:"login_max"`
	LoginAvg  string  `json:"login_avg"`
	LoginSec  float64 `json:"login_seconds"`

	WSOK       int64   `json:"ws_ok"`
	WSFail     int64   `json:"ws_fail"`
	WSP50      string  `json:"ws_p50"`
	WSP90      string  `json:"ws_p90"`
	WSP99      string  `json:"ws_p99"`
	WSMax      string  `json:"ws_max"`
	WSAvg      string  `json:"ws_avg"`
	ConnectSec float64 `json:"connect_seconds"`

	Sent       int64   `json:"sent"`
	SendFail   int64   `json:"send_fail"`
	BurstSec   float64 `json:"burst_seconds"`
	SendRatePS float64 `json:"send_rate_per_sec"`

	TotalSec      float64 `json:"total_seconds"`
	ChatRecv      int64   `json:"chat_recv_total"`
	ChatExpected  int64   `json:"chat_expected_total"`
	ChatPerClient int64   `json:"chat_expected_per_client"`
	DeliveryPct   float64 `json:"delivery_pct"`
	MissingTotal  int64   `json:"missing_total"`
	ClientsFull   int     `json:"clients_100pct"`
	ClientsLost   int     `json:"clients_with_disconnect"`
	MinPerClient  int64   `json:"min_recv_per_client"`
	MaxPerClient  int64   `json:"max_recv_per_client"`
	AvgPerClient  float64 `json:"avg_recv_per_client"`

	RecvFrames  int64   `json:"recv_frames"`
	RecvBytes   int64   `json:"recv_bytes"`
	RecvRatePS  float64 `json:"recv_frames_per_sec"`
	RecvMBps    float64 `json:"recv_mbps"`
	Disconnects int64   `json:"disconnects"`
	ReadErr     int64   `json:"read_errors"`
	Pongs       int64   `json:"pongs"`

	AIStart    int64 `json:"ai_start"`
	AIDelta    int64 `json:"ai_delta"`
	AIEnd      int64 `json:"ai_end"`
	JoinRecv   int64 `json:"join_recv"`
	LeaveRecv  int64 `json:"leave_recv"`
	InitRecv   int64 `json:"init_recv"`
	HistRecv   int64 `json:"hist_recv"`
	OnlineRecv int64 `json:"online_recv"`
	ErrRecv    int64 `json:"error_frames"`

	AIEndTotal   int64   `json:"ai_end_total"`
	AIEndMin     int64   `json:"ai_end_min_per_client"`
	AIEndMax     int64   `json:"ai_end_max_per_client"`
	AIEndAvg     float64 `json:"ai_end_avg_per_client"`
	AIFullOK     int     `json:"clients_got_all_ai"`
	AIStartTotal int64   `json:"ai_start_total"`
	AIDeltaTotal int64   `json:"ai_delta_total"`
	FirstAIP50   string  `json:"first_ai_end_p50"`
	FirstAIP90   string  `json:"first_ai_end_p90"`
	FirstAIMax   string  `json:"first_ai_end_max"`
	FirstAIAvg   string  `json:"first_ai_end_avg"`
	AISpanP50    string  `json:"ai_span_p50"`
	AISpanP90    string  `json:"ai_span_p90"`
	AISpanP99    string  `json:"ai_span_p99"`
	AISpanMax    string  `json:"ai_span_max"`
	AISpanCount  int     `json:"ai_span_samples"`

	RTTCount int    `json:"rtt_samples"`
	RTTP50   string `json:"rtt_p50"`
	RTTP90   string `json:"rtt_p90"`
	RTTP99   string `json:"rtt_p99"`
	RTTMax   string `json:"rtt_max"`
	RTTAvg   string `json:"rtt_avg"`

	DrainTimedOut bool             `json:"drain_timed_out"`
	Timeline      []TimelineSample `json:"timeline"`
}

func main() {
	var o Options
	flag.StringVar(&o.Base, "base", "http://127.0.0.1", "被压测地址，例如 http://127.0.0.1 或 http://1.2.3.4:8082")
	flag.IntVar(&o.Users, "users", 1000, "并发用户数（登录名 = 前缀+序号）")
	flag.IntVar(&o.Msgs, "msgs", 10, "每人发送的普通聊天消息条数；0 = 只进房不发消息（用来单独压进房风暴与 AI 欢迎语）")
	flag.StringVar(&o.NamePrefix, "name-prefix", "压测", "登录名前缀，最终为 压测1..压测N")
	flag.StringVar(&o.School, "school", "压测大学", "登录学校")
	flag.StringVar(&o.Major, "major", "压测专业", "登录专业")
	flag.IntVar(&o.LoginParallel, "login-parallel", 50, "登录并发度")
	flag.IntVar(&o.ConnectParallel, "connect-parallel", 200, "WebSocket 建连并发度")
	flag.IntVar(&o.Samples, "samples", 20, "抽样测回环延迟的客户端数量")
	flag.DurationVar(&o.Interval, "interval", 0, "同一用户两条消息之间的间隔；0 = 无间隔极限压满")
	flag.DurationVar(&o.Settle, "settle", 0, "全部连上后、开压前的等待（用来让 AI 欢迎语风暴先过去）")
	flag.DurationVar(&o.DrainQuiet, "drain-quiet", 5*time.Second, "连续这么久没有新聊天消息就认为发完了")
	flag.DurationVar(&o.DrainTimeout, "drain-timeout", 300*time.Second, "收尾最长等待")
	flag.DurationVar(&o.DialTimeout, "dial-timeout", 30*time.Second, "单次建连超时")
	flag.DurationVar(&o.WaitOnline, "wait-online", 30*time.Second, "开压前等待服务端确认所有连接已登记的最长时间")
	flag.StringVar(&o.Out, "out", "", "报告 JSON 输出路径")
	flag.StringVar(&o.Label, "label", "", "本次压测标签")
	flag.Parse()

	if o.Users <= 0 || o.Msgs < 0 {
		fmt.Fprintln(os.Stderr, "users 必须大于 0，msgs 不能为负（msgs=0 表示只进房、不发消息）")
		os.Exit(2)
	}
	if o.Samples > o.Users {
		o.Samples = o.Users
	}
	o.Base = strings.TrimRight(o.Base, "/")

	rep := run(o)
	printReport(rep)

	if o.Out != "" {
		if err := writeJSON(o.Out, rep); err != nil {
			fmt.Fprintf(os.Stderr, "写报告失败: %v\n", err)
		} else {
			fmt.Printf("\n报告已保存: %s\n", o.Out)
		}
	}
}

func run(o Options) *Report {
	st := &Stats{}
	tl := &Timeline{}
	loginLat := &LatencySet{}
	wsLat := &LatencySet{}

	rep := &Report{
		Label: o.Label,
		Base:  o.Base,
		Users: o.Users,
		Msgs:  o.Msgs,
		Start: time.Now().Format(time.RFC3339),
	}
	rep.HealthBefore = health(o.Base)
	startAll := time.Now()

	// ---------- 阶段 1：登录 ----------
	fmt.Printf("▶ 阶段1/4 登录 %d 个用户（%s1 .. %s%d，并发 %d）\n",
		o.Users, o.NamePrefix, o.NamePrefix, o.Users, o.LoginParallel)

	tokens := make([]string, o.Users)
	httpCli := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        o.Users + 50,
			MaxIdleConnsPerHost: o.Users + 50,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	t0 := time.Now()
	{
		jobs := make(chan int, o.Users)
		var wg sync.WaitGroup
		for w := 0; w < o.LoginParallel; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					name := fmt.Sprintf("%s%d", o.NamePrefix, i+1)
					begin := time.Now()
					tok, err := loginOne(httpCli, o.Base, name, o.School, o.Major)
					if err != nil {
						st.LoginFail.Add(1)
						continue
					}
					loginLat.Add(time.Since(begin))
					tokens[i] = tok
					st.LoginOK.Add(1)
				}
			}()
		}
		for i := 0; i < o.Users; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	}
	rep.LoginSec = time.Since(t0).Seconds()
	rep.LoginOK = st.LoginOK.Load()
	rep.LoginFail = st.LoginFail.Load()
	rep.LoginP50, rep.LoginP90, rep.LoginP99, rep.LoginMax, rep.LoginAvg = durStrings(loginLat.Summary())
	fmt.Printf("  登录完成: 成功 %d / 失败 %d，耗时 %.2fs\n", rep.LoginOK, rep.LoginFail, rep.LoginSec)

	// ---------- 阶段 2：建 WebSocket ----------
	okUsers := 0
	for _, t := range tokens {
		if t != "" {
			okUsers++
		}
	}
	if okUsers == 0 {
		fmt.Println("❌ 没有任何用户登录成功，终止")
		rep.HealthAfter = health(o.Base)
		rep.End = time.Now().Format(time.RFC3339)
		return rep
	}

	clients := make([]*client, 0, okUsers)
	for i := 0; i < o.Users; i++ {
		if tokens[i] == "" {
			continue
		}
		clients = append(clients, newClient(
			i,
			fmt.Sprintf("%s%d", o.NamePrefix, i+1),
			tokens[i],
			len(clients) < o.Samples,
			o.Msgs,
		))
	}

	fmt.Printf("▶ 阶段2/4 建立 %d 条 WebSocket 连接（并发 %d）\n", len(clients), o.ConnectParallel)
	t1 := time.Now()
	{
		idx := make(chan int, len(clients))
		var wg sync.WaitGroup
		for w := 0; w < o.ConnectParallel; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range idx {
					c := clients[i]
					begin := time.Now()
					conn, br, err := dialWS(o.Base, c.token, o.DialTimeout)
					if err != nil {
						st.WSFail.Add(1)
						continue
					}
					c.conn, c.br = conn, br
					c.connectAt.Store(time.Now().UnixNano())
					wsLat.Add(time.Since(begin))
					st.WSOK.Add(1)
					go c.readLoop(st)
				}
			}()
		}
		for i := range clients {
			idx <- i
		}
		close(idx)
		wg.Wait()
	}
	rep.ConnectSec = time.Since(t1).Seconds()
	rep.WSOK = st.WSOK.Load()
	rep.WSFail = st.WSFail.Load()
	rep.WSP50, rep.WSP90, rep.WSP99, rep.WSMax, rep.WSAvg = durStrings(wsLat.Summary())
	fmt.Printf("  建连完成: 成功 %d / 失败 %d，耗时 %.2fs；服务端自报在线 %s\n",
		rep.WSOK, rep.WSFail, rep.ConnectSec, onlineOf(health(o.Base)))

	// ⚠️ 关键：收到 101 只代表握手完成，服务端把它登记进 hub 还有微小延迟。
	// 不等待就开压的话，后登记的客户端会漏掉前面广播的消息，
	// 压出来的「送达率」其实是在测自己的竞态，不是测服务端。
	if n, ok := waitForOnline(o.Base, int(rep.WSOK), o.WaitOnline); ok {
		fmt.Printf("  服务端已确认全部 %d 个连接登记完成（耗时 %.2fs）\n", n, time.Since(t1).Seconds()-rep.ConnectSec)
	} else {
		fmt.Printf("  ⚠️ 等待登记超时：服务端只报告 %d/%d 在线，后面的送达率会偏低\n", n, rep.WSOK)
	}

	if o.Settle > 0 {
		fmt.Printf("  等待 %s（settle）...\n", o.Settle)
		time.Sleep(o.Settle)
	}

	// ---------- 阶段 3：无间隔连发 ----------
	connected := make([]*client, 0, len(clients))
	for _, c := range clients {
		if c.conn != nil {
			connected = append(connected, c)
		}
	}
	fmt.Printf("▶ 阶段3/4 火力全开: %d 人 × %d 条（间隔 %v）\n", len(connected), o.Msgs, o.Interval)

	stopTL := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var lastChat, lastFrames, lastBytes int64
		sec := 0
		for {
			select {
			case <-stopTL:
				return
			case <-ticker.C:
				sec++
				chat, frames, by := st.ChatRecv.Load(), st.Frames.Load(), st.Bytes.Load()
				s := TimelineSample{Sec: sec, ChatPS: chat - lastChat, FramesPS: frames - lastFrames, BytesPS: by - lastBytes}
				lastChat, lastFrames, lastBytes = chat, frames, by
				tl.add(s)
				fmt.Printf("    t=%3ds  聊天 %8d/s  帧 %8d/s  下行 %6.1f MB/s\n",
					sec, s.ChatPS, s.FramesPS, float64(s.BytesPS)/1024/1024)
			}
		}
	}()

	start := make(chan struct{})
	var sendWG sync.WaitGroup
	t2 := time.Now()
	for _, c := range connected {
		sendWG.Add(1)
		go func(c *client) { defer sendWG.Done(); c.burst(start, o.Msgs, o.Interval, st) }(c)
	}
	close(start)
	sendWG.Wait()
	rep.BurstSec = time.Since(t2).Seconds()

	rep.Sent = st.Sent.Load()
	rep.SendFail = st.SendFail.Load()
	if rep.BurstSec > 0 {
		rep.SendRatePS = float64(rep.Sent) / rep.BurstSec
	}
	fmt.Printf("  发送结束: 成功 %d / 失败 %d，耗时 %.2fs（%.0f 条/秒）\n",
		rep.Sent, rep.SendFail, rep.BurstSec, rep.SendRatePS)

	// ---------- 阶段 4：收尾 ----------
	fmt.Printf("▶ 阶段4/4 等待消息分发完毕（安静 %s 或最长 %s）\n", o.DrainQuiet, o.DrainTimeout)
	// 每条消息都会广播给房间里的每个人：总下行 = 人数 × 人数 × 每人条数
	perClient := int64(len(connected)) * int64(o.Msgs)
	expected := perClient * int64(len(connected))
	// 收尾判定：聊天消息收齐、且「所有类型」的帧都安静下来才算真的结束。
	// 只看聊天消息会误判——服务端可能还堵在 AI 流/广播里没来得及处理上行。
	lastChat, lastFrames := st.ChatRecv.Load(), st.Frames.Load()
	quietSince := time.Now()
	deadline := time.Now().Add(o.DrainTimeout)
	for {
		time.Sleep(500 * time.Millisecond)
		chat, frames := st.ChatRecv.Load(), st.Frames.Load()
		if chat != lastChat || frames != lastFrames {
			quietSince = time.Now()
		}
		lastChat, lastFrames = chat, frames
		if expected > 0 && chat >= expected && time.Since(quietSince) >= time.Second {
			break
		}
		if time.Since(quietSince) >= o.DrainQuiet {
			break
		}
		if time.Now().After(deadline) {
			rep.DrainTimedOut = true
			fmt.Println("  ⚠️ 收尾超时，仍有消息未送达")
			break
		}
	}
	close(stopTL)
	time.Sleep(1100 * time.Millisecond) // 让最后一秒的采样落地

	// ---------- 汇总 ----------
	rep.TotalSec = time.Since(startAll).Seconds()
	rep.End = time.Now().Format(time.RFC3339)
	rep.ChatRecv = st.ChatRecv.Load()
	rep.ChatExpected = expected
	rep.ChatPerClient = perClient
	if expected > 0 {
		rep.DeliveryPct = float64(rep.ChatRecv) / float64(expected) * 100
		rep.MissingTotal = expected - rep.ChatRecv
	}
	var minC, maxC, sumC int64 = -1, 0, 0
	full := 0
	for _, c := range connected {
		v := c.chatRecv.Load()
		if minC < 0 || v < minC {
			minC = v
		}
		if v > maxC {
			maxC = v
		}
		sumC += v
		if perClient > 0 && v >= perClient {
			full++
		}
	}
	if len(connected) > 0 {
		rep.MinPerClient, rep.MaxPerClient = minC, maxC
		rep.AvgPerClient = float64(sumC) / float64(len(connected))
	}
	rep.ClientsFull = full

	// ---------- AI 专项：欢迎语/回答是不是人人都收到了、要等多久 ----------
	rep.AIStartTotal, rep.AIDeltaTotal, rep.AIEndTotal = st.AIStart.Load(), st.AIDelta.Load(), st.AIEnd.Load()
	var aiMin, aiMax, aiSum int64 = -1, 0, 0
	aiFull := 0
	for _, c := range connected {
		v := c.aiEnd.Load()
		if aiMin < 0 || v < aiMin {
			aiMin = v
		}
		if v > aiMax {
			aiMax = v
		}
		aiSum += v
		if aiMax > 0 && v >= aiMax { // 以收得最多的客户端为基准
			aiFull++
		}
	}
	if len(connected) > 0 {
		rep.AIEndMin, rep.AIEndMax = aiMin, aiMax
		rep.AIEndAvg = float64(aiSum) / float64(len(connected))
	}
	rep.AIFullOK = aiFull

	// 抽样回环延迟
	rtt := &LatencySet{}
	lost := 0
	firstAI := &LatencySet{}
	aiSpan := &LatencySet{}
	for _, c := range connected {
		if c.readErr.Load() || c.conn == nil {
			lost++
		}
		if !c.sampled {
			continue
		}
		c.rttMu.Lock()
		for _, d := range c.rtts {
			rtt.Add(d)
		}
		c.rttMu.Unlock()

		c.aiMu.Lock()
		for _, d := range c.firstSeq {
			firstAI.Add(d)
		}
		for _, d := range c.aiSpans {
			aiSpan.Add(d)
		}
		c.aiMu.Unlock()
	}
	rep.ClientsLost = lost
	rep.RTTCount = rtt.Len()
	rep.RTTP50, rep.RTTP90, rep.RTTP99, rep.RTTMax, rep.RTTAvg = durStrings(rtt.Summary())
	rep.FirstAIP50, rep.FirstAIP90, _, rep.FirstAIMax, rep.FirstAIAvg = durStrings(firstAI.Summary())
	rep.AISpanP50, rep.AISpanP90, rep.AISpanP99, rep.AISpanMax, _ = durStrings(aiSpan.Summary())
	rep.AISpanCount = aiSpan.Len()

	rep.RecvFrames = st.Frames.Load()
	rep.RecvBytes = st.Bytes.Load()
	if rep.TotalSec > 0 {
		rep.RecvRatePS = float64(rep.RecvFrames) / rep.TotalSec
		rep.RecvMBps = float64(rep.RecvBytes) / rep.TotalSec / 1024 / 1024
	}
	rep.Disconnects = st.Disconnects.Load()
	rep.ReadErr = st.ReadErr.Load()
	rep.Pongs = st.Pongs.Load()
	rep.AIStart, rep.AIDelta, rep.AIEnd = st.AIStart.Load(), st.AIDelta.Load(), st.AIEnd.Load()
	rep.JoinRecv, rep.LeaveRecv = st.JoinRecv.Load(), st.LeaveRecv.Load()
	rep.InitRecv, rep.HistRecv = st.InitRecv.Load(), st.HistRecv.Load()
	rep.OnlineRecv = st.OnlineRecv.Load()
	rep.ErrRecv = st.ErrRecv.Load()
	rep.Timeline = tl.Samples()

	// 收尾：优雅关闭所有连接
	for _, c := range connected {
		c.shutdown.Store(true)
		_ = c.writeFrame(0x8, nil)
		_ = c.conn.Close()
	}
	time.Sleep(500 * time.Millisecond)
	rep.HealthAfter = health(o.Base)

	return rep
}

func loginOne(cli *http.Client, base, name, school, major string) (string, error) {
	body, _ := json.Marshal(map[string]string{"name": name, "school": school, "major": major})
	req, err := http.NewRequest(http.MethodPost, base+"/api/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var lr struct {
		OK    bool   `json:"ok"`
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &lr); err != nil {
		return "", fmt.Errorf("登录响应无法解析: %s", truncateStr(string(raw), 120))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !lr.OK || lr.Token == "" {
		return "", fmt.Errorf("登录失败(%d): %s", resp.StatusCode, lr.Error)
	}
	return lr.Token, nil
}

func health(base string) string {
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(base + "/api/health")
	if err != nil {
		return "unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

// waitForOnline 轮询 /api/health，直到服务端报告的在线的连接数达到 want。
// 注意：如果服务端上还有别的连接（比如你自己开的浏览器页面），这里会提前满足，
// 所以压测前最好确保房间里没有其他客户端。
func waitForOnline(base string, want int, timeout time.Duration) (int, bool) {
	cli := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	last := 0
	for {
		resp, err := cli.Get(base + "/api/health")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var m struct {
				Online int `json:"online"`
			}
			if json.Unmarshal(b, &m) == nil {
				last = m.Online
				if last >= want {
					return last, true
				}
			}
		}
		if time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func onlineOf(h string) string {
	var m struct {
		Online int `json:"online"`
	}
	if json.Unmarshal([]byte(h), &m) == nil {
		return fmt.Sprintf("%d 人", m.Online)
	}
	return "未知"
}

func durStrings(p50, p90, p99, max, avg time.Duration) (string, string, string, string, string) {
	f := func(d time.Duration) string {
		if d <= 0 {
			return "-"
		}
		return d.Round(time.Microsecond).String()
	}
	return f(p50), f(p90), f(p99), f(max), f(avg)
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func writeJSON(path string, v any) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func printReport(r *Report) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    压 测 结 果 报 告                          ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	if r.Label != "" {
		fmt.Printf("  标签          : %s\n", r.Label)
	}
	fmt.Printf("  目标          : %s\n", r.Base)
	fmt.Printf("  规模          : %d 人 × %d 条 = %d 条消息\n", r.Users, r.Msgs, r.Users*r.Msgs)
	fmt.Printf("  时间窗口      : %s → %s（共 %.1fs）\n", r.Start, r.End, r.TotalSec)
	fmt.Println("──────────────────────────────────────────────────────────────")
	fmt.Printf("  压测前健康    : %s\n", r.HealthBefore)
	fmt.Printf("  压测后健康    : %s\n", r.HealthAfter)
	fmt.Println("──────────────────────────────────────────────────────────────")
	fmt.Println("  [登录]")
	fmt.Printf("    成功/失败   : %d / %d（耗时 %.2fs）\n", r.LoginOK, r.LoginFail, r.LoginSec)
	fmt.Printf("    延迟 p50/p90/p99/max/avg : %s / %s / %s / %s / %s\n",
		r.LoginP50, r.LoginP90, r.LoginP99, r.LoginMax, r.LoginAvg)
	fmt.Println("  [WebSocket 建连]")
	fmt.Printf("    成功/失败   : %d / %d（耗时 %.2fs）\n", r.WSOK, r.WSFail, r.ConnectSec)
	fmt.Printf("    延迟 p50/p90/p99/max/avg : %s / %s / %s / %s / %s\n",
		r.WSP50, r.WSP90, r.WSP99, r.WSMax, r.WSAvg)
	fmt.Println("  [发送]")
	fmt.Printf("    成功/失败   : %d / %d（耗时 %.2fs，%.0f 条/秒）\n",
		r.Sent, r.SendFail, r.BurstSec, r.SendRatePS)
	if r.Msgs == 0 {
		fmt.Println("  [送达率 —— 本次为「只进房不发消息」模式，不统计聊天送达率]")
	} else {
		fmt.Println("  [送达率 —— 核心指标]")
		fmt.Printf("    总下行应有/实收 : %d / %d（每条消息广播给所有人）\n", r.ChatExpected, r.ChatRecv)
		fmt.Printf("    每人应有    : %d 条\n", r.ChatPerClient)
		fmt.Printf("    送达率      : %.2f%%    丢失: %d 条\n", r.DeliveryPct, r.MissingTotal)
		fmt.Printf("    每客户端实收 : 最少 %d / 最多 %d / 平均 %.1f\n",
			r.MinPerClient, r.MaxPerClient, r.AvgPerClient)
		fmt.Printf("    100%% 收到的客户端: %d / %d\n", r.ClientsFull, r.WSOK)
	}
	fmt.Println("  [下行压力]")
	fmt.Printf("    收到帧数    : %d（%.0f 帧/秒）\n", r.RecvFrames, r.RecvRatePS)
	fmt.Printf("    收到数据量  : %.1f MB（%.1f MB/s）\n",
		float64(r.RecvBytes)/1024/1024, r.RecvMBps)
	fmt.Printf("    掉线/读错误 : %d / %d，pong 应答 %d\n", r.Disconnects, r.ReadErr, r.Pongs)
	fmt.Println("  [消息类型拆解]")
	fmt.Printf("    聊天 msg    : %d\n", r.ChatRecv)
	fmt.Printf("    AI 流        : ai_start=%d ai_delta=%d ai_end=%d\n", r.AIStart, r.AIDelta, r.AIEnd)
	fmt.Printf("    进出房      : join=%d leave=%d history=%d init=%d online=%d\n",
		r.JoinRecv, r.LeaveRecv, r.HistRecv, r.InitRecv, r.OnlineRecv)
	fmt.Printf("    服务端错误帧: %d\n", r.ErrRecv)
	if r.AIEndTotal > 0 {
		fmt.Println("  [AI 欢迎语 / AI 回答 —— 是不是人人都收到了、要等多久]")
		fmt.Printf("    ai_end 总数 : %d（每有一条 AI 消息就会广播给所有人）\n", r.AIEndTotal)
		fmt.Printf("    每客户端收到: 最少 %d / 最多 %d / 平均 %.1f\n", r.AIEndMin, r.AIEndMax, r.AIEndAvg)
		fmt.Printf("    收到全部 AI 消息的客户端: %d / %d\n", r.AIFullOK, r.WSOK)
		if r.FirstAIP50 != "-" {
			fmt.Printf("    建连 → 首条 AI 到达（抽样）: p50 %s / p90 %s / max %s / avg %s\n",
				r.FirstAIP50, r.FirstAIP90, r.FirstAIMax, r.FirstAIAvg)
		}
		if r.AISpanCount > 0 {
			fmt.Printf("    单条 AI 生成时长 ai_start→ai_end（抽样 %d 条）: p50 %s / p90 %s / p99 %s / max %s\n",
				r.AISpanCount, r.AISpanP50, r.AISpanP90, r.AISpanP99, r.AISpanMax)
		}
	}
	if r.RTTCount > 0 {
		fmt.Println("  [消息回环延迟（抽样客户端，自己→服务端→自己）]")
		fmt.Printf("    样本 %d 条，p50/p90/p99/max/avg : %s / %s / %s / %s / %s\n",
			r.RTTCount, r.RTTP50, r.RTTP90, r.RTTP99, r.RTTMax, r.RTTAvg)
	}
	if len(r.Timeline) > 0 {
		fmt.Println("  [吞吐曲线（每秒）]")
		for _, s := range r.Timeline {
			fmt.Printf("    t=%3ds  聊天 %8d/s  帧 %8d/s  下行 %6.1f MB/s\n",
				s.Sec, s.ChatPS, s.FramesPS, float64(s.BytesPS)/1024/1024)
		}
	}
	fmt.Println("──────────────────────────────────────────────────────────────")
	fmt.Println("  判定：", verdict(r))
	fmt.Println("══════════════════════════════════════════════════════════════")
}

func verdict(r *Report) string {
	if r.WSFail > 0 || r.LoginFail > 0 {
		return fmt.Sprintf("❌ 未通过：登录失败 %d、建连失败 %d（%d 人没全连上）", r.LoginFail, r.WSFail, r.Users)
	}
	if r.Disconnects > 0 || r.ReadErr > 0 {
		return fmt.Sprintf("❌ 未通过：压测期间掉线 %d 次（服务端可能被压崩或重启）", r.Disconnects)
	}
	if r.Msgs == 0 {
		if r.WSOK > 0 && r.AIEndTotal > 0 && int64(r.AIFullOK) < r.WSOK {
			return fmt.Sprintf("⚠️ 有问题：有 %d/%d 个客户端没收到全部 AI 消息（欢迎语被丢过）",
				r.WSOK-int64(r.AIFullOK), r.WSOK)
		}
		return fmt.Sprintf("✅ 通过：%d 人只进房不发消息，进房过程与 AI 消息分发正常", r.WSOK)
	}
	if r.DeliveryPct < 99.9 {
		return fmt.Sprintf("⚠️ 有问题：消息送达率仅 %.2f%%，丢了 %d 条（服务端 send 队列满时会静默丢消息）",
			r.DeliveryPct, r.MissingTotal)
	}
	if r.SendFail > 0 {
		return fmt.Sprintf("⚠️ 有问题：有 %d 条消息没发出去", r.SendFail)
	}
	return fmt.Sprintf("✅ 通过：%d 人同时在线、每人 %d 条消息全部送达", r.Users, r.Msgs)
}
