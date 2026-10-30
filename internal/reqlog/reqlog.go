// Package reqlog 是进程内的请求流水（环形缓冲 + 一行 stdout）。
//
// 只放"排查一次失败需要的东西"：谁、用哪个号、走没走通、多快、多少 token。
// 不落原始 prompt/响应（那是用户内容，落盘就变成数据风险）。
package reqlog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Entry 一条请求记录。
type Entry struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Path         string    `json:"path"`
	Model        string    `json:"model"`
	Account      string    `json:"account"`
	Stream       bool      `json:"stream"`
	Status       int       `json:"status"`
	PromptTokens int       `json:"prompt_tokens"`
	OutputTokens int       `json:"output_tokens"`
	Chars        int       `json:"chars"`
	FirstByteMS  int64     `json:"first_byte_ms"`
	DurationMS   int64     `json:"duration_ms"`
	Error        string    `json:"error,omitempty"`
	Degraded     string    `json:"degraded,omitempty"`
	ClientIP     string    `json:"client_ip,omitempty"`
	UserAgent    string    `json:"user_agent,omitempty"`
}

// Stats 汇总。
type Stats struct {
	Total      int            `json:"total"`
	OK         int            `json:"ok"`
	Failed     int            `json:"failed"`
	Streaming  int            `json:"streaming"`
	ByModel    map[string]int `json:"by_model"`
	ByAccount  map[string]int `json:"by_account"`
	StartedAt  time.Time      `json:"started_at"`
	TOKENS     int            `json:"tokens_total"`
	AvgLatency int64          `json:"avg_duration_ms"`
}

// Log 环形流水。
type Log struct {
	mu        sync.RWMutex
	entries   []Entry
	next      int
	filled    bool
	capacity  int
	started   time.Time
	total     int
	ok        int
	failed    int
	streaming int
	tokens    int
	latency   int64
	byModel   map[string]int
	byAccount map[string]int
	// Echo 同时打到 stdout（容器日志里能直接看流水）。
	Echo bool
	// Out Echo 的落点，nil = os.Stdout。
	//
	// 之所以可换：面板「日志」页要把流水行也镜一份进内存环形缓冲，
	// 而 os.Stdout 一旦被进程内替换，所有 fmt.Print* 都会跟着变 —— 那是副作用更大的做法。
	Out io.Writer
}

// New 构造流水，capacity<=0 回落 500。
func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = 500
	}
	return &Log{
		entries:   make([]Entry, capacity),
		capacity:  capacity,
		started:   time.Now(),
		byModel:   map[string]int{},
		byAccount: map[string]int{},
		Echo:      true,
	}
}

// Add 记一条。
func (l *Log) Add(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.mu.Lock()
	l.entries[l.next] = e
	l.next = (l.next + 1) % l.capacity
	if l.next == 0 {
		l.filled = true
	}
	l.total++
	if e.Status >= 200 && e.Status < 400 && e.Error == "" {
		l.ok++
	} else {
		l.failed++
	}
	if e.Stream {
		l.streaming++
	}
	l.tokens += e.PromptTokens + e.OutputTokens
	l.latency += e.DurationMS
	if e.Model != "" {
		l.byModel[e.Model]++
	}
	if e.Account != "" {
		l.byAccount[e.Account]++
	}
	l.mu.Unlock()

	if l.Echo {
		line := fmt.Sprintf("[req] %s %s model=%s acct=%s stream=%v status=%d tok=%d/%d %dms",
			e.ID, e.Path, e.Model, e.Account, e.Stream, e.Status,
			e.PromptTokens, e.OutputTokens, e.DurationMS)
		if e.Error != "" {
			line += " err=" + e.Error
		}
		out := l.Out
		if out == nil {
			out = os.Stdout
		}
		fmt.Fprintln(out, line)
	}
}

// Recent 最近 n 条（新→旧）。
func (l *Log) Recent(n int) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > l.capacity {
		n = l.capacity
	}
	out := make([]Entry, 0, n)
	total := l.capacity
	if !l.filled {
		total = l.next
	}
	for i := 0; i < n && i < total; i++ {
		idx := (l.next - 1 - i + l.capacity) % l.capacity
		out = append(out, l.entries[idx])
	}
	return out
}

// Stats 汇总快照。
func (l *Log) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s := Stats{
		Total:     l.total,
		OK:        l.ok,
		Failed:    l.failed,
		Streaming: l.streaming,
		StartedAt: l.started,
		TOKENS:    l.tokens,
	}
	s.ByModel = make(map[string]int, len(l.byModel))
	for k, v := range l.byModel {
		s.ByModel[k] = v
	}
	s.ByAccount = make(map[string]int, len(l.byAccount))
	for k, v := range l.byAccount {
		s.ByAccount[k] = v
	}
	if l.total > 0 {
		s.AvgLatency = l.latency / int64(l.total)
	}
	return s
}

// JSON 便于面板直接序列化。
func (s Stats) JSON() string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
