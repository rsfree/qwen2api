package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 对话粘性（Agent 专用）。
//
// 目的只有一个：**让多轮对话复用同一个上游会话**，从而命中上游的 prompt cache。
// 实测（docs/UPSTREAM.md §10.6）：Agent 每轮带约 14k 系统提示，同一 chat_id 内
// 只有第一次全价（13.3k 命中 cache）；换会话就重新付一次。
//
// 🔴 它**不承担上下文正确性**：历史由客户端每轮发全量 messages、我们拍平进 prompt
// 保证。所以粘性失效（并发降级 / 换号 / 重启）只会多花 token，不会答错。
// 这条性质是整套设计可以"尽力而为"的前提 —— 拿不到就换新号，绝不阻塞请求。
const (
	// dialogTTL 一段对话多久不活跃就回收。Agent 对话停 15 分钟基本等于结束。
	dialogTTL = 15 * time.Minute
	// dialogMax 最多记多少段对话。超了按 LRU 淘汰（池子有几千号，但活跃对话没那么多）。
	dialogMax = 200

	// dialogHeader 显式对话 id 的请求头。
	dialogHeader = "X-Dialog-Id"

	// dialogMinAnchor 首条用户消息至少这么长（rune），才拿它当对话锚点。
	//
	// 为什么要有下限：短首句（"你好" / "1" / "继续" / "在吗"）在真实流量里高度重复，
	// hash 撞键会把两段**无关**对话并成一段 —— 后果是答非所问 + 泄漏别人上下文，
	// 不是省钱的问题。宁可少省一次 cache，也不能串对话。
	// 想放宽/收紧就改这个常量（或用 X-Dialog-Id 显式指定，那条路不受限制）。
	dialogMinAnchor = 8
)

// dialogEntry 一段可复用的上游会话。
type dialogEntry struct {
	account  string
	chatID   string
	parentID string // 下一轮要用的 parentId（= 上一轮的 response_id）
	model    string
	lastUsed time.Time

	// mu 是**使用权**闸门：同一对话同时只容一个请求复用它。
	//
	// 拿不到时不是排队、也不是 409，而是**换新号开新会话**（用户 2026-11-08 定的策略：
	// "先共享 cache，并发不够用新号"）。理由：agent 一轮 10s~3min，排队会让调用方
	// 以为服务挂了；而并发共用同一个 parentId 会让上游的对话链分叉，历史错乱。
	mu sync.Mutex
	// created 标记"这条记录是本次请求建的"，失败时要回收（别留个空壳占位）。
	created bool
}

// dialogStore 对话表。纯内存、不落盘。
//
// 为什么不落盘/不用 Redis：会话是**账号维度**资源，跨号即 CHAT_NOT_FOUND；
// 重启丢表 = 一次 cache miss，可接受；引入外部依赖会把"零依赖单二进制"这条优势抹掉。
type dialogStore struct {
	mu      sync.Mutex
	entries map[string]*dialogEntry
	ttl     time.Duration
	max     int
}

func newDialogStore() *dialogStore {
	return &dialogStore{entries: map[string]*dialogEntry{}, ttl: dialogTTL, max: dialogMax}
}

// peek 取一段对话（不锁、不建）。过期条目顺手清掉。
func (d *dialogStore) peek(key string) *dialogEntry {
	if d == nil || key == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.entries[key]
	if e == nil {
		return nil
	}
	if time.Since(e.lastUsed) > d.ttl {
		delete(d.entries, key)
		return nil
	}
	return e
}

// borrow 尝试**独占**一段已存在的对话。拿不到使用权返回 nil（调用方应降级换新号）。
func (d *dialogStore) borrow(key, model string) *dialogEntry {
	e := d.peek(key)
	if e == nil {
		return nil
	}
	// 模型变了就不能复用：上游会话建的时候 models[] 就定死了。
	if e.model != model {
		return nil
	}
	if !e.mu.TryLock() {
		return nil
	}
	// 二次校验：等锁期间可能被 TTL 清掉（peek 后到这里之间）。
	if cur := d.peek(key); cur != e {
		e.mu.Unlock()
		return nil
	}
	return e
}

// bind 把一次**新建成功**的会话记进表并锁住，返回可写的条目。
//
// 若该 key 已被别的请求抢先绑定（并发新建），后来的不覆盖，直接返回 nil ——
// 本次请求仍能正常完成，只是不参与后续复用。
func (d *dialogStore) bind(key, account, chatID, model string) *dialogEntry {
	if d == nil || key == "" || chatID == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.entries[key]; e != nil && time.Since(e.lastUsed) <= d.ttl {
		return nil
	}
	d.evictLocked()
	e := &dialogEntry{account: account, chatID: chatID, model: model, lastUsed: time.Now(), created: true}
	if d.max > 0 && len(d.entries) >= d.max {
		delete(d.entries, d.oldestLocked())
	}
	d.entries[key] = e
	e.mu.Lock()
	return e
}

// done 结束一段使用权：刷新活跃时间并解锁。
//
// parentID 由调用方在持锁期间直接写（拿到本轮 response_id 的那一刻），
// 这里不再接受参数 —— 两个写入口容易漏掉一个。
func (e *dialogEntry) done() {
	if e == nil {
		return
	}
	e.lastUsed = time.Now()
	e.created = false
	e.mu.Unlock()
}

// drop 回收一段（失败时用，只删自己建的那条）。
func (d *dialogStore) drop(key string) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	delete(d.entries, key)
	d.mu.Unlock()
}

// dropAccount 某号不可用时，把钉在它上面的对话全清掉。
//
// 不清的话：下一轮还会先试这个号、失败、再降级 —— 白等一次 Acquire 的往返。
func (d *dialogStore) dropAccount(name string) {
	if d == nil || name == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, e := range d.entries {
		if e.account == name {
			delete(d.entries, k)
		}
	}
}

func (d *dialogStore) evictLocked() {
	if d.max > 0 && len(d.entries) > d.max*2 {
		for i := 0; i < d.max; i++ {
			delete(d.entries, d.oldestLocked())
		}
	}
}

// oldestLocked 找最久未用的一条（线性扫，200 条规模够用）。
func (d *dialogStore) oldestLocked() string {
	var (
		oldestKey string
		oldestAt  time.Time
	)
	for k, e := range d.entries {
		if oldestKey == "" || e.lastUsed.Before(oldestAt) {
			oldestKey, oldestAt = k, e.lastUsed
		}
	}
	return oldestKey
}

// snapshot 观测面（面板/调试用）。
func (d *dialogStore) snapshot() map[string]any {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	live := 0
	for _, e := range d.entries {
		if now.Sub(e.lastUsed) <= d.ttl {
			live++
		}
	}
	return map[string]any{"entries": live, "ttl_s": int(d.ttl.Seconds()), "max": d.max}
}

// ---------- 对话键 ----------

// dialogKeyOf 决定"这个请求属于哪段对话"。返回空串 = 不做粘性（退回无状态）。
//
// 三级优先，都是零客户端改造的兜底链：
//
//	1. `X-Dialog-Id` 头   —— 显式契约，客户端最清楚自己开的是哪个对话；
//	2. `user` 字段        —— OpenAI 标准位置，语义是"终端用户"；
//	3. 首条 user 消息的 hash —— 零改造：多数客户端的首句在多轮里不变，
//	   于是"同一个对话"自然算出同一个键。
//
// 🔴 为什么不用整串 messages 的 hash：它每轮都在变（历史在增长），
// 算出来的键只能活一轮，等于没有粘性。
//
// ⚠️ 第 3 级会撞键：两段不同对话首句相同（比如都是"你好"）时会被算成同一段。
// 所以 hash 里带上 model 缩小碰撞面，并且这是**最末一级** —— 客户端只要传
// X-Dialog-Id 或 user 就不会走到这里。
func dialogKeyOf(r *http.Request, user, model string, messages []ChatMessage) (key, source string) {
	if v := strings.TrimSpace(r.Header.Get(dialogHeader)); v != "" {
		return "h:" + v, "header"
	}
	if v := strings.TrimSpace(user); v != "" {
		return "u:" + v, "user"
	}
	if anchor := dialogAnchor(model, messages); anchor != "" {
		sum := sha256.Sum256([]byte(anchor))
		return "f:" + hex.EncodeToString(sum[:12]), "first_message"
	}
	return "", ""
}

// dialogAnchor 拼出"这段对话的身份指纹"：model + 所有 system 消息 + 首条 user 消息。
//
// 三个组成部分各有理由：
//   - model：上游会话建的时候 models[] 就定死了，换模型必须换会话；
//   - system：同一个应用内稳定，**跨应用能立即区分**（多数撞键是不同应用撞的）；
//   - 首条 user：同一应用内区分不同对话的锚点，多轮里它不动。
//
// 为什么不把后续消息也算进去：它们每轮都在增长，算出来的键只能活一轮。
//
// 返回空串 = 锚点太弱（首句短于 dialogMinAnchor），调用方应视为"不做粘性"。
func dialogAnchor(model string, messages []ChatMessage) string {
	var sys, first strings.Builder
	for _, m := range messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if t := plainText(m.Content); t != "" {
				sys.WriteString(t)
				sys.WriteByte('\n')
			}
		case "user":
			if first.Len() == 0 {
				first.WriteString(plainText(m.Content))
			}
		}
	}
	anchor := strings.TrimSpace(first.String())
	if len([]rune(anchor)) < dialogMinAnchor {
		return ""
	}
	return model + "\x00" + sys.String() + "\x00" + anchor
}

// plainText 从 content（string 或 content part 数组）里抽出文本。
func plainText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}
