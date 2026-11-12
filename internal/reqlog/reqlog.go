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
	"sort"
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
	// CachedTokens 命中 prompt cache 的输入 token（上游只对 Agent 报，见 upstream.Usage）。
	// 与 Dialog 一起才能回答"粘性命中到底省没省到钱"——粘性只是必要条件，
	// 真正命中的证据是这个数。
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	// CacheEst cached 是**估算**而非上游读数（粘性命中但上游没报 cached_tokens）。
	// 流水里会标 (est)：排障时必须能分清"上游报的"和"我们推的"。
	CacheEst    bool  `json:"cache_est,omitempty"`
	FirstByteMS int64 `json:"first_byte_ms"`
	// SubmitMS 提交（POST chat/completions）→ 收到上游响应头。
	// 这一段是**上游推理排队**，不含建会话、不含我们的解析。
	SubmitMS int64 `json:"submit_ms,omitempty"`
	// FirstTextMS 提交 → 第一个正文字符（不含 role 帧与心跳）。
	// 与 SubmitMS 的差 = "上游发完头之后多久才吐第一个字"。
	//
	// 🔴 为什么单独记：客户端看到的"首字"常被**首个 role 帧**（我们立刻下发）
	// 掩盖成建连时间，真正决定体感的是 FirstTextMS。
	FirstTextMS int64  `json:"first_text_ms,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
	Degraded    string `json:"degraded,omitempty"`
	ClientIP    string `json:"client_ip,omitempty"`
	UserAgent   string `json:"user_agent,omitempty"`
	// ChatHit 本次是否命中了预建会话（命中省掉一次 chats/new）。
	ChatHit bool `json:"chat_hit,omitempty"`
	// ChatMS 建会话耗时（毫秒）：命中为 0，未命中为真实值。
	// 与 ChatHit 一起就能算"命中到底省了多少"。
	ChatMS int64 `json:"chat_ms,omitempty"`
	// RefImages 本次带的参考图（垫图）张数 —— 排查"变慢/超时"时第一个要看的东西：
	// 多一张图上游确实更慢（实测每张约 +18s）。
	RefImages int `json:"ref_images,omitempty"`
	// EgressIP 本次用的出口公网 IP（账号对应 socks5 的实测出口，空 = 还没探到）。
	// 与 ClientIP 是两件事：ClientIP 是"谁调我们"，EgressIP 是"我们从哪个 IP 访问上游"。
	EgressIP string `json:"egress_ip,omitempty"`
	// ChatID 上游会话 id。**失败也记** —— acct 只说明"哪个号"，
	// chat_id 才说明"哪个会话"，是事后去上游会话历史里补捞产物的唯一线索。
	ChatID string `json:"chat_id,omitempty"`
	// Dialog 对话粘性命中情况：hit = 复用了上一轮的会话（吃到 prompt cache），
	// new = 新建（无粘性/首轮/并发降级）。这是"省没省到钱"的唯一直接指标。
	Dialog string `json:"dialog,omitempty"`
	// Prompt 本轮实际提交的 prompt 形态：delta = 只发本轮输入（复用会话，
	// 历史靠上游会话链）；full = 拼接了 system + 全部历史（首轮/降级/带附件）。
	// 与 Dialog 一起看才能证明"降级真的退回了全量拼接"，而不是想当然。
	Prompt string `json:"prompt,omitempty"`
	// Tools Agent 模式的工具执行轨迹（`bash(python3 -c ...) → completed exit=0`）。
	// 没有它，流水里只有"耗时 11 秒"，回答不了"这 11 秒到底干了什么"。
	Tools string `json:"tools,omitempty"`
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
	// TTFT 端到端首字分位（收到请求 → 第一个正文/思考增量）。
	// 别看 avg：首字是长尾分布，均值会被个别 10s 的请求拉飞。
	TTFT TTFTStat `json:"ttft"`
	// UpstreamTTFT 上游首字分位（提交 → 第一个正文）。
	// 与 TTFT 的差 = 选号 + 建会话 + 建连 + 提交前的那段时间。
	UpstreamTTFT TTFTStat `json:"upstream_ttft"`
}

// TTFTStat 一组首字样本的分位（样本来自环形流水，不是全量）。
type TTFTStat struct {
	N   int   `json:"n"`
	P50 int64 `json:"p50_ms"`
	P95 int64 `json:"p95_ms"`
	Max int64 `json:"max_ms"`
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
		// 首字拆两段：submit=提交到上游响应头，first=提交到第一个正文。
		// 两个都打出来，才能一眼看出慢在"上游排队"还是"上游吐字"。
		if e.SubmitMS > 0 {
			line += fmt.Sprintf(" submit=%dms", e.SubmitMS)
		}
		if e.FirstTextMS > 0 {
			line += fmt.Sprintf(" first=%dms", e.FirstTextMS)
		}
		if e.EgressIP != "" {
			line += " egress=" + e.EgressIP
		}
		if e.ChatHit {
			line += " chat=cached"
		} else if e.ChatMS > 0 {
			line += fmt.Sprintf(" chat=%dms", e.ChatMS)
		}
		if e.Dialog != "" {
			line += " dlg=" + e.Dialog
		}
		if e.Prompt != "" {
			line += " prompt=" + e.Prompt
		}
		// 缓存命中：粘性只说明"同一个会话"，命中要靠这个数证明。
		if e.CachedTokens > 0 {
			suffix := ""
			if e.CacheEst {
				suffix = "(est)"
			}
			line += fmt.Sprintf(" cache=%d/%d%s", e.CachedTokens, e.PromptTokens, suffix)
		}
		if e.ChatID != "" {
			line += " chat_id=" + e.ChatID
		}
		// 工具轨迹：Agent 的"11 秒里干了什么"全在这一行。
		// 截断到 200 字（完整值在面板的 JSON 里），否则多轮任务会把日志冲垮。
		if e.Tools != "" {
			line += " tools=" + clip(e.Tools, 200)
		}
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
	// 首字分位：样本来自环形流水（最多 capacity 条），够看趋势。
	// 只统计"真的出了字"的请求 —— 失败的 FirstByteMS 为 0，混进来会把分位拉低。
	n := l.capacity
	if !l.filled {
		n = l.next
	}
	var e2e, up []int64
	for i := 0; i < n; i++ {
		e := l.entries[i]
		if e.Time.IsZero() {
			continue
		}
		if e.FirstByteMS > 0 {
			e2e = append(e2e, e.FirstByteMS)
		}
		if e.FirstTextMS > 0 {
			up = append(up, e.FirstTextMS)
		}
	}
	s.TTFT = statOf(e2e)
	s.UpstreamTTFT = statOf(up)
	return s
}

// statOf 算 P50 / P95 / Max。样本少时就是原值，不做插值。
func statOf(v []int64) TTFTStat {
	if len(v) == 0 {
		return TTFTStat{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return TTFTStat{
		N:   len(v),
		P50: v[len(v)*50/100],
		P95: v[min(len(v)-1, len(v)*95/100)],
		Max: v[len(v)-1],
	}
}

// JSON 便于面板直接序列化。
func (s Stats) JSON() string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// clip 截断到 n 个字符（按 rune，不切坏中文）。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
