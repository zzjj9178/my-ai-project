package main

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Stats 全局计数器，压测过程中所有 goroutine 共享
type Stats struct {
	LoginOK   atomic.Int64
	LoginFail atomic.Int64

	WSOK   atomic.Int64
	WSFail atomic.Int64

	Sent     atomic.Int64 // 成功发出的聊天消息
	SendFail atomic.Int64

	Frames atomic.Int64 // 收到的 WebSocket 文本帧总数
	Bytes  atomic.Int64 // 收到的字节总数

	ChatRecv   atomic.Int64 // 收到的普通聊天消息（type=msg）
	AIStart    atomic.Int64
	AIDelta    atomic.Int64
	AIEnd      atomic.Int64
	JoinRecv   atomic.Int64
	LeaveRecv  atomic.Int64
	InitRecv   atomic.Int64
	HistRecv   atomic.Int64
	OnlineRecv atomic.Int64
	ErrRecv    atomic.Int64
	OtherRecv  atomic.Int64

	ReadErr     atomic.Int64 // 读帧失败
	Disconnects atomic.Int64 // 非正常掉线（服务端断开/被重启）
	Pongs       atomic.Int64
}

// LatencySet 收集一组耗时，最后算分位数
type LatencySet struct {
	mu   sync.Mutex
	data []time.Duration
}

func (l *LatencySet) Add(d time.Duration) {
	l.mu.Lock()
	l.data = append(l.data, d)
	l.mu.Unlock()
}

func (l *LatencySet) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.data)
}

// Summary 返回 p50/p90/p99/max/avg
func (l *LatencySet) Summary() (p50, p90, p99, max, avg time.Duration) {
	l.mu.Lock()
	d := append([]time.Duration(nil), l.data...)
	l.mu.Unlock()
	if len(d) == 0 {
		return
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	var sum time.Duration
	for _, v := range d {
		sum += v
	}
	avg = sum / time.Duration(len(d))
	pick := func(q float64) time.Duration {
		i := int(float64(len(d)-1) * q)
		if i < 0 {
			i = 0
		}
		return d[i]
	}
	p50, p90, p99, max = pick(0.50), pick(0.90), pick(0.99), d[len(d)-1]
	return
}

// Timeline 每秒采样一次，得到吞吐曲线
type Timeline struct {
	mu      sync.Mutex
	samples []TimelineSample
}

type TimelineSample struct {
	Sec      int   `json:"sec"`
	ChatPS   int64 `json:"chat_per_sec"`   // 每秒新增聊天消息
	FramesPS int64 `json:"frames_per_sec"` // 每秒新增帧
	BytesPS  int64 `json:"bytes_per_sec"`  // 每秒新增字节
}

func (t *Timeline) add(s TimelineSample) {
	t.mu.Lock()
	t.samples = append(t.samples, s)
	t.mu.Unlock()
}

func (t *Timeline) Samples() []TimelineSample {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TimelineSample(nil), t.samples...)
}
