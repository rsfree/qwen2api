// Package runlog 是进程运行日志的环形缓冲（stdout/stderr 镜像）。
//
// 为什么需要它：`docker logs` 只在宿主机上能看，面板在浏览器里 ——
// 一旦请求失败，运维第一句话是"日志呢"，而把用户踢回 SSH 是最差的一条路。
// 这里把标准 log（stderr）与请求流水行（stdout）同时镜一份到内存环形缓冲，
// 面板「日志」页直接读它。
//
// 边界很清楚：
//   - 只在内存，重启归零（与 reqlog 同口径）；
//   - 不落盘、不进第三方 —— 日志里可能带账号名/错误原文，落盘就是数据风险；
//   - 单行长度有上限，避免一次超长错误把缓冲撑爆。
package runlog

import (
	"bytes"
	"sync"
	"time"
)

// Line 一行日志。
type Line struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// 单行上限：正常日志几十字节，超长的一般是上游错误体/URL，截断比占内存划算。
const maxLineBytes = 4096

// Buffer 环形日志缓冲。实现 io.Writer，可直接塞进 io.MultiWriter。
type Buffer struct {
	mu       sync.Mutex
	lines    []Line
	next     int
	filled   bool
	capacity int
	// pending 是"还没等到换行"的半行（log.Printf 一次一行，但 Write 不保证）。
	pending []byte
}

// New 构造缓冲，capacity<=0 回落 800 行。
func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 800
	}
	return &Buffer{lines: make([]Line, capacity), capacity: capacity}
}

// Write 实现 io.Writer。返回 len(p) 表示"已接受"，无论是否被截断 ——
// 日志写失败不该反过来影响业务。
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.pending = append(b.pending, p...)
	for {
		i := bytes.IndexByte(b.pending, '\n')
		if i < 0 {
			break
		}
		b.pushLocked(string(b.pending[:i]))
		b.pending = b.pending[i+1:]
	}
	// 长时间没有换行（比如上游一次吐了 8MB 错误体）就截断，别把内存吃掉。
	if len(b.pending) > maxLineBytes {
		b.pushLocked(string(b.pending[:maxLineBytes]) + "…（单行超长已截断）")
		b.pending = nil
	}
	return len(p), nil
}

func (b *Buffer) pushLocked(s string) {
	// 去掉行尾 \r（Windows / 某些库会带），空行保留（日志里的分隔空行有意义）。
	s = trimCR(s)
	if len(s) > maxLineBytes {
		s = s[:maxLineBytes] + "…（单行超长已截断）"
	}
	b.lines[b.next] = Line{Time: time.Now(), Text: s}
	b.next = (b.next + 1) % b.capacity
	if b.next == 0 {
		b.filled = true
	}
}

func trimCR(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// Lines 取最近 n 行，**按时间正序**返回（旧 → 新，终端形态）。
// n<=0 或超过容量时返回全部。
func (b *Buffer) Lines(n int) []Line {
	b.mu.Lock()
	defer b.mu.Unlock()

	total := b.capacity
	if !b.filled {
		total = b.next
	}
	if n <= 0 || n > total {
		n = total
	}
	out := make([]Line, 0, n)
	start := b.next - n
	for i := 0; i < n; i++ {
		idx := (start + i + b.capacity*2) % b.capacity
		out = append(out, b.lines[idx])
	}
	return out
}

// Len 当前已缓存行数。
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.filled {
		return b.capacity
	}
	return b.next
}
