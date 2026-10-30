package server

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ChatPool 按「账号 + 会话类型」缓存的**空会话** id。
//
// 为什么要它（2026-10-30 实测）：`POST /api/v2/chats/new` 走住宅代理要
// **2.0~2.8 秒**，而一次对话总共 5~8 秒 —— 建会话占掉约三分之一。
// 提前建好，请求路径上就只剩"提交 + 读流"。
//
// 🔴 三个硬约束（都实测过）：
//  1. **会话是账号维度资源**：A 号建的 chat 让 B 号提交会得到
//     `CHAT_NOT_FOUND`（"此对话已删除"）—— 池子必须按账号分组。
//  2. **会话类型在建的时候就定了**（t2t / t2i / t2v / image_edit），
//     提交时虽然也带 chat_type，但建会话时就带上了，所以按类型再分一层。
//  3. **只能用一个**：提交后会话里就有消息了，再拿它提交会变成多轮对话
//     （parentId 语义也乱）。所以是"取出即删"，用完由后台补。
type ChatPool struct {
	mu  sync.Mutex
	m   map[string]map[string][]chatEntry
	per int           // 每号每类型最多缓存几个（0 = 关闭预建）
	ttl time.Duration // 超过这个年龄的会话丢弃
	now func() time.Time
	// hit / miss 命中率观测。命中=省掉一次 chats/new；
	// 如果 miss 很高，说明预热覆盖的号与实际选号错位（要看 warmTarget/HotNames）。
	hit, miss int64
	// buildN / buildUS 现建会话的耗时样本（微秒累计）——
	// 用来回答"命中到底省了多少"：独立 curl 测出 2.4s，但那是含 TLS 握手的；
	// 网关用长连接，真实成本可能低一个量级。
	buildN  int64
	buildUS int64
}

type chatEntry struct {
	id string
	at time.Time
}

// NewChatPool per：每号每类型缓存上限（<=0 表示不做预建，池子始终空）。
// ttl：会话最长缓存多久。上游是否会让空会话过期没有实测依据，
// 保守取 15 分钟（与 access token 同量级，且远大于一次流量轮转周期）。
func NewChatPool(per int, ttl time.Duration) *ChatPool {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &ChatPool{
		m:   map[string]map[string][]chatEntry{},
		per: per,
		ttl: ttl,
		now: time.Now,
	}
}

// SetLimits 热更新每号每类型的目标数与 TTL（面板保存后调用）。
//
// per 变 0 就是关掉预建：已缓存的会话会留在池里自然过期（Trim 清），
// 不影响在途请求；新的请求不再命中（Take 里 Enabled() 为 false 直接返回 miss）。
func (p *ChatPool) SetLimits(per int, ttl time.Duration) {
	if p == nil {
		return
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	p.mu.Lock()
	p.per = per
	p.ttl = ttl
	p.mu.Unlock()
}

// Enabled 是否启用预建。
func (p *ChatPool) Enabled() bool { return p != nil && p.per > 0 }

// Per 每号每类型的目标数量。
func (p *ChatPool) Per() int {
	if p == nil {
		return 0
	}
	return p.per
}

// Take 取一个可用会话（取出即删）。过期的顺手丢掉。
func (p *ChatPool) Take(account, chatType string) (string, bool) {
	if !p.Enabled() {
		return "", false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()

	byType := p.m[account]
	if byType == nil {
		// 这个号压根没预热过 —— 也算 miss（否则命中率会虚高）。
		atomic.AddInt64(&p.miss, 1)
		return "", false
	}
	list := byType[chatType]
	// 从后往前取（后进的新，先被用掉能让缓存里的会话年龄更平均）。
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		list = append(list[:i], list[i+1:]...)
		if now.Sub(e.at) > p.ttl {
			continue // 过期：丢掉，继续找一个新的
		}
		if len(list) == 0 {
			delete(byType, chatType)
		} else {
			byType[chatType] = list
		}
		atomic.AddInt64(&p.hit, 1)
		return e.id, true
	}
	if len(list) == 0 {
		delete(byType, chatType)
	} else {
		byType[chatType] = list
	}
	atomic.AddInt64(&p.miss, 1)
	return "", false
}

// Put 存一个刚建好的空会话。超上限/已关闭则直接丢弃。
func (p *ChatPool) Put(account, chatType, id string) bool {
	if !p.Enabled() || id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	byType := p.m[account]
	if byType == nil {
		byType = map[string][]chatEntry{}
		p.m[account] = byType
	}
	list := byType[chatType]
	// 顺手清过期，避免长期运行的池里堆死数据。
	now := p.now()
	kept := list[:0]
	for _, e := range list {
		if now.Sub(e.at) <= p.ttl {
			kept = append(kept, e)
		}
	}
	if len(kept) >= p.per {
		byType[chatType] = kept
		return false
	}
	byType[chatType] = append(kept, chatEntry{id: id, at: now})
	return true
}

// RecordBuild 记录一次"现建会话"的真实耗时（走网关自己的长连接）。
func (p *ChatPool) RecordBuild(d time.Duration) {
	if p == nil || d <= 0 {
		return
	}
	atomic.AddInt64(&p.buildN, 1)
	atomic.AddInt64(&p.buildUS, int64(d/time.Microsecond))
}

// Short 该号该类型还差几个（0 = 已够，不需要补）。
func (p *ChatPool) Short(account, chatType string) int {
	if !p.Enabled() {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.m[account][chatType])
	if n >= p.per {
		return 0
	}
	return p.per - n
}

// Has 该号该类型池里有没有货（只看有没有，不取出）。
func (p *ChatPool) Has(account, chatType string) bool {
	if !p.Enabled() {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m[account][chatType]) > 0
}

// Trim 清理所有过期项（保活循环定期调）。
func (p *ChatPool) Trim() int {
	if !p.Enabled() {
		return 0
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	removed := 0
	for acct, byType := range p.m {
		for typ, list := range byType {
			kept := list[:0]
			for _, e := range list {
				if now.Sub(e.at) <= p.ttl {
					kept = append(kept, e)
				} else {
					removed++
				}
			}
			if len(kept) == 0 {
				delete(byType, typ)
			} else {
				byType[typ] = kept
			}
		}
		if len(byType) == 0 {
			delete(p.m, acct)
		}
	}
	return removed
}

// ChatPoolStat 观测口径（/stats 与面板用）。
type ChatPoolStat struct {
	Per    int            `json:"per"`     // 每号每类型目标数
	TTLS   int            `json:"ttl_s"`   // 缓存上限（秒）
	Ready  int            `json:"ready"`   // 当前可用的会话总数
	Hit    int64          `json:"hit"`     // 命中次数（省掉一次 chats/new）
	Miss   int64          `json:"miss"`    // 未命中次数（只能现建）
	HitPct float64        `json:"hit_pct"` // 命中率（%）
	BuildMS float64       `json:"build_ms_avg"` // 现建会话的平均耗时（ms）
	BuildN  int64         `json:"build_n"`      // 现建样本数
	ByName map[string]int `json:"by_type"` // 按会话类型分布
}

// Stat 汇总当前池子。
func (p *ChatPool) Stat() ChatPoolStat {
	out := ChatPoolStat{ByName: map[string]int{}}
	if p == nil {
		return out
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out.Per = p.per
	out.TTLS = int(p.ttl.Seconds())
	out.Hit = atomic.LoadInt64(&p.hit)
	out.Miss = atomic.LoadInt64(&p.miss)
	if total := out.Hit + out.Miss; total > 0 {
		out.HitPct = float64(out.Hit) * 100 / float64(total)
	}
	n := atomic.LoadInt64(&p.buildN)
	if n > 0 {
		out.BuildN = n
		out.BuildMS = float64(atomic.LoadInt64(&p.buildUS)) / 1000 / float64(n)
	}
	for _, byType := range p.m {
		for typ, list := range byType {
			out.Ready += len(list)
			out.ByName[typ] += len(list)
		}
	}
	return out
}

// Accounts 池里有哪些号（调试/观测用，按名字排序保证输出稳定）。
func (p *ChatPool) Accounts() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.m))
	for a := range p.m {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
