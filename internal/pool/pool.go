// Package pool 是多账号池：选号、在途限流、冷却与失败计数。
//
// 设计取向（与 workbuddy2api 同源，按 qwen 的实际情况简化）：
//   - **凭据是 Bearer token**，池内不刷新、不落盘（配置里有，重启重读）；
//   - 选号 = "最闲 + 最久未用"，不按权重（qwen 没有可读的剩余额度接口，
//     编一个权重只会让调度看起来有道理）；
//   - 冷却分级：401 ⇒ 长冷却（默认 1800s），额度/风控/429 ⇒ 软冷却（指数退避）。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// Account 单个账号的运行时状态。
type Account struct {
	Name  string
	Token string
	Email string
	// Password 非空 ⇒ 凭据失效时可自动重登（signin）。
	Password string
	Note     string

	// authMode 探测到的凭据形态（cookie / bearer）；空 = 尚未探测。
	authMode string

	inflight int32
	// imageDay / imageCount 是"本地每日出图配额"：跨天自动清零。
	// 存日期而不是时间戳，因为上游的口径是「今日」（自然日），不是滚动 24h。
	imageDay   string
	imageCount int
	videoDay   string
	videoCount int
	// 视频积分预算（与个数并存：积分是主口径，个数是硬上限）
	creditDay     string
	creditUsed    int
	cooldownUntil time.Time
	failures      int
	lastUsed      time.Time
	served        int64
	lastError     string
	authDead      bool
}

// View 面板/日志用的只读快照（不含 token）。
type View struct {
	Name          string `json:"name"`
	Email         string `json:"email,omitempty"`
	Note          string `json:"note,omitempty"`
	State         string `json:"state"` // ready / cooling / auth_dead
	Inflight      int    `json:"inflight"`
	CooldownLeftS int    `json:"cooldown_left_s"`
	Failures      int    `json:"failures"`
	Served        int64  `json:"served"`
	LastUsed      string `json:"last_used,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// ImageToday 今日已成功出图张数；ImageLimit 本地每日上限（0 = 不限）。
	ImageToday int `json:"image_today"`
	ImageLimit int `json:"image_limit"`
	// VideoToday 今日已提交的视频任务数；VideoLimit 本地每日上限（0 = 不限）。
	VideoToday int `json:"video_today"`
	VideoLimit int `json:"video_limit"`
	// CreditsUsed 今日已用视频积分；CreditsLimit 每日积分预算（0 = 不启用）。
	CreditsUsed  int `json:"credits_used"`
	CreditsLimit int `json:"credits_limit"`
}

// Pool 账号池（并发安全）。
type Pool struct {
	mu       sync.RWMutex
	accounts []*Account
	cfg      config.Pool
	state    string
	rr       int
}

// New 构造池并尝试恢复上次的冷却状态。
func New(cfg config.Pool, accounts []config.Account, stateFile string) *Pool {
	p := &Pool{cfg: cfg, state: stateFile}
	for _, a := range accounts {
		p.accounts = append(p.accounts, &Account{
			Name:     a.Name,
			Token:    a.Token,
			Email:    a.Email,
			Password: a.Password,
			Note:     a.Note,
		})
	}
	p.restore()
	return p
}

// Len 账号数。
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// Pick 取一个可用账号并加在途租约。ok=false 表示全部冷却/失效。
//
// 选择顺序：在途数少的优先；同等在途取最久未用的（近似轮询，避免一个号被连打）。
func (p *Pool) Pick() (*Account, bool) {
	return p.pick(kindAny)
}

// PickForImage 取一个"今日出图额度还没用完"的账号。
//
// 与 Pick 的唯一区别是多一道本地配额过滤：某号今日已出够 `max_image_per_day` 张，
// 就先跳过它，别等打到上游硬限（那时上游只会挂起，没有明确报错）。
func (p *Pool) PickForImage() (*Account, bool) {
	return p.pick(kindImage)
}

// PickForVideo 取一个"今日视频额度还够这一次"的账号。
//
// needCredits 是本次视频要扣的积分（由 server 按价格表算好传入）。
// 积分模式启用时按"剩余预算 ≥ needCredits"过滤；未启用时只看个数上限。
//
// 🔴 不能只看个数：720P 5s 是 30 分、1080P 15s 是 120 分，
// 按"1 个/天"放行的话，1080P 请求会把预算打穿 3 倍。
func (p *Pool) PickForVideo(needCredits int) (*Account, bool) {
	return p.pickVideo(needCredits)
}

// pickVideo 视频选号（带积分过滤）。
func (p *Pool) pickVideo(needCredits int) (*Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var candidates []*Account
	for _, a := range p.accounts {
		if a.authDead || now.Before(a.cooldownUntil) {
			continue
		}
		if int(atomic.LoadInt32(&a.inflight)) >= p.cfg.MaxInflightPerAccount {
			continue
		}
		if p.cfg.MaxVideoPerDay > 0 {
			p.rollVideoDay(a, now)
			if a.videoCount >= p.cfg.MaxVideoPerDay {
				continue
			}
		}
		if p.cfg.MaxVideoCreditsPerDay > 0 {
			p.rollCreditDay(a, now)
			if a.creditUsed+needCredits > p.cfg.MaxVideoCreditsPerDay {
				continue
			}
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := atomic.LoadInt32(&candidates[i].inflight), atomic.LoadInt32(&candidates[j].inflight)
		if ci != cj {
			return ci < cj
		}
		return candidates[i].lastUsed.Before(candidates[j].lastUsed)
	})
	chosen := candidates[0]
	atomic.AddInt32(&chosen.inflight, 1)
	chosen.lastUsed = now
	return chosen, true
}

type pickKind int

const (
	kindAny pickKind = iota
	kindImage
	kindVideo
)

func (p *Pool) pick(need pickKind) (*Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var candidates []*Account
	for _, a := range p.accounts {
		if a.authDead {
			continue
		}
		if now.Before(a.cooldownUntil) {
			continue
		}
		if int(atomic.LoadInt32(&a.inflight)) >= p.cfg.MaxInflightPerAccount {
			continue
		}
		if need == kindImage && p.cfg.MaxImagePerDay > 0 {
			p.rollImageDay(a, now)
			if a.imageCount >= p.cfg.MaxImagePerDay {
				continue
			}
		}
		if need == kindVideo && p.cfg.MaxVideoPerDay > 0 {
			p.rollVideoDay(a, now)
			if a.videoCount >= p.cfg.MaxVideoPerDay {
				continue
			}
		}
		_ = need
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := atomic.LoadInt32(&candidates[i].inflight), atomic.LoadInt32(&candidates[j].inflight)
		if ci != cj {
			return ci < cj
		}
		return candidates[i].lastUsed.Before(candidates[j].lastUsed)
	})
	chosen := candidates[0]
	atomic.AddInt32(&chosen.inflight, 1)
	chosen.lastUsed = now
	return chosen, true
}

// rollImageDay 跨天清零（必须在持锁时调用）。
//
// 用本地日期（time.Now 的时区）而不是 UTC：调用方理解的"每天"是他自己的自然日。
func (p *Pool) rollImageDay(a *Account, now time.Time) {
	today := now.Format("2006-01-02")
	if a.imageDay != today {
		a.imageDay = today
		a.imageCount = 0
	}
}

// rollVideoDay 跨天清零（持锁调用）。
func (p *Pool) rollVideoDay(a *Account, now time.Time) {
	today := now.Format("2006-01-02")
	if a.videoDay != today {
		a.videoDay = today
		a.videoCount = 0
	}
}

// MarkVideoDone 记一次**已提交**的视频任务。
//
// 与图片的差别：视频提交成功就扣上游额度（异步任务，产物 15 分钟后才知道），
// 所以计数点在"拿到 task_id"，不在"下载成功"。反过来说，提交失败不该计数 ——
// 那种情况上游根本没建任务。
func (p *Pool) MarkVideoDone(a *Account) {
	if a == nil {
		return
	}
	p.mu.Lock()
	p.rollVideoDay(a, time.Now())
	a.videoCount++
	p.mu.Unlock()
	p.save()
}

// VideoUsage 返回该号今日已提交视频数与上限（limit=0 表示不限）。
func (p *Pool) VideoUsage(a *Account) (used, limit int) {
	if a == nil {
		return 0, p.cfg.MaxVideoPerDay
	}
	p.mu.Lock()
	p.rollVideoDay(a, time.Now())
	used = a.videoCount
	p.mu.Unlock()
	return used, p.cfg.MaxVideoPerDay
}

// VideoQuotaLeft 全池今日剩余视频额度（0 且 limit>0 ⇒ 用尽；-1 ⇒ 不限）。
func (p *Pool) VideoQuotaLeft() int {
	limit := p.cfg.MaxVideoPerDay
	if limit <= 0 {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	total := 0
	for _, a := range p.accounts {
		if a.authDead {
			continue
		}
		p.rollVideoDay(a, now)
		if left := limit - a.videoCount; left > 0 {
			total += left
		}
	}
	return total
}

// rollCreditDay 视频积分预算跨天清零（持锁调用）。
func (p *Pool) rollCreditDay(a *Account, now time.Time) {
	today := now.Format("2006-01-02")
	if a.creditDay != today {
		a.creditDay = today
		a.creditUsed = 0
	}
}

// SpendVideoCredits 扣一次视频积分（提交成功后调用）。
func (p *Pool) SpendVideoCredits(a *Account, credits int) {
	if a == nil || credits <= 0 {
		return
	}
	p.mu.Lock()
	p.rollCreditDay(a, time.Now())
	a.creditUsed += credits
	p.mu.Unlock()
	p.save()
}

// CreditsUsage 返回 (今日已用积分, 每日预算)。预算 0 表示不启用积分模式。
func (p *Pool) CreditsUsage(a *Account) (used, limit int) {
	limit = p.cfg.MaxVideoCreditsPerDay
	if a == nil {
		return 0, limit
	}
	p.mu.Lock()
	p.rollCreditDay(a, time.Now())
	used = a.creditUsed
	p.mu.Unlock()
	return used, limit
}

// CanSpendVideoCredits 是否存在**单个**账号的剩余预算够这一次。
//
// 注意不能拿 CreditsLeft() >= need 判断：两个号各剩 20 分时总和够 40，
// 但 30 分的请求谁都不够 —— 那种情况要报"积分不足"，不是"账号不可用"。
func (p *Pool) CanSpendVideoCredits(need int) bool {
	limit := p.cfg.MaxVideoCreditsPerDay
	if limit <= 0 {
		return true // 未启用积分模式
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accounts {
		if a.authDead {
			continue
		}
		p.rollCreditDay(a, now)
		if limit-a.creditUsed >= need {
			return true
		}
	}
	return false
}

// CreditsLeft 全池今日剩余视频积分（-1 表示不启用积分模式）。
func (p *Pool) CreditsLeft() int {
	limit := p.cfg.MaxVideoCreditsPerDay
	if limit <= 0 {
		return -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	total := 0
	for _, a := range p.accounts {
		if a.authDead {
			continue
		}
		p.rollCreditDay(a, now)
		if left := limit - a.creditUsed; left > 0 {
			total += left
		}
	}
	return total
}

// MarkImageDone 记一张**成功出图**（失败/挂起不计数）。
//
// 为什么只记成功：2026-10-28 实测，同一账号 9 张成功即耗尽，
// 中途两次"只发心跳"的挂起并没有扣额度 —— 按成功数计才与服务端口径一致。
func (p *Pool) MarkImageDone(a *Account) {
	if a == nil {
		return
	}
	p.mu.Lock()
	p.rollImageDay(a, time.Now())
	a.imageCount++
	p.mu.Unlock()
	p.save()
}

// ImageUsage 返回该号今日已出图数与本地上限（limit=0 表示不限）。
func (p *Pool) ImageUsage(a *Account) (used, limit int) {
	if a == nil {
		return 0, p.cfg.MaxImagePerDay
	}
	p.mu.Lock()
	p.rollImageDay(a, time.Now())
	used = a.imageCount
	p.mu.Unlock()
	return used, p.cfg.MaxImagePerDay
}

// ImageQuotaLeft 所有账号今日剩余出图额度之和（0 且 limit>0 ⇒ 全池配额用尽）。
func (p *Pool) ImageQuotaLeft() int {
	limit := p.cfg.MaxImagePerDay
	if limit <= 0 {
		return -1 // 不限
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	total := 0
	for _, a := range p.accounts {
		if a.authDead {
			continue
		}
		p.rollImageDay(a, now)
		if left := limit - a.imageCount; left > 0 {
			total += left
		}
	}
	return total
}

// Release 释放在途租约（必须 defer 调用）。
func (p *Pool) Release(a *Account) {
	if a == nil {
		return
	}
	if n := atomic.AddInt32(&a.inflight, -1); n < 0 {
		atomic.StoreInt32(&a.inflight, 0)
	}
}

// MarkOK 成功一次：清零失败计数并记账。
func (p *Pool) MarkOK(a *Account) {
	if a == nil {
		return
	}
	p.mu.Lock()
	a.failures = 0
	a.lastError = ""
	a.served++
	p.mu.Unlock()
}

// MarkAuth 凭据失效：长冷却 + 标记 authDead（同进程内不再选它）。
func (p *Pool) MarkAuth(a *Account, reason string) {
	if a == nil {
		return
	}
	p.mu.Lock()
	a.authDead = true
	a.failures++
	a.lastError = "auth: " + reason
	a.cooldownUntil = time.Now().Add(time.Duration(p.cfg.CooldownAuthS) * time.Second)
	p.mu.Unlock()
	p.save()
}

// MarkQuota 额度/积分耗尽：软冷却（该号今天没量了，退避到下一个窗口）。
func (p *Pool) MarkQuota(a *Account, reason string) {
	p.cooldown(a, reason, 0)
}

// MarkRateLimit 限流/风控：软冷却，支持 Retry-After。
func (p *Pool) MarkRateLimit(a *Account, reason string, retryAfter int) {
	p.cooldown(a, reason, retryAfter)
}

// cooldown 指数退避：base * 2^(n-1)，封顶 1h。
func (p *Pool) cooldown(a *Account, reason string, retryAfter int) {
	if a == nil {
		return
	}
	p.mu.Lock()
	a.failures++
	a.lastError = reason
	base := time.Duration(p.cfg.CooldownSoftS) * time.Second
	if retryAfter > 0 {
		base = time.Duration(retryAfter) * time.Second
	}
	shift := a.failures - 1
	if shift > 6 {
		shift = 6
	}
	d := base * time.Duration(1<<uint(shift))
	if d > time.Hour {
		d = time.Hour
	}
	a.cooldownUntil = time.Now().Add(d)
	p.mu.Unlock()
	p.save()
}

// SetToken 更新账号凭据（自动重登成功后调用），并解除失效标记。
//
// 入参应是**带形态前缀**的凭据（`cookie:<jwt>`），形态由登录链路确定。
func (p *Pool) SetToken(name, cred string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.Token = cred
			a.authMode = ""
			a.authDead = false
			a.failures = 0
			a.cooldownUntil = time.Time{}
			a.lastError = ""
			return true
		}
	}
	return false
}

// SetAuthMode 记录探测到的凭据形态，并把形态写回凭据串。
func (p *Pool) SetAuthMode(name, mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.authMode = mode
			if mode != "" && !strings.HasPrefix(a.Token, mode+":") {
				a.Token = mode + ":" + strings.TrimPrefix(a.Token, "bearer:")
			}
			return
		}
	}
}

// Get 按名取账号指针（nil 表示不存在）。仅给启动探测/重登这类"已知账号"的场景用。
func (p *Pool) Get(name string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// Credential 返回当前凭据（带形态前缀）。
func (p *Pool) Credential(name string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.Name == name {
			return a.Token
		}
	}
	return ""
}

// Reset 解除冷却/失效标记（面板"复活"按钮用）。
func (p *Pool) Reset(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.Name == name {
			a.authDead = false
			a.failures = 0
			a.cooldownUntil = time.Time{}
			a.lastError = ""
			return true
		}
	}
	return false
}

// Snapshot 只读快照（按配置顺序）。
func (p *Pool) Snapshot() []View {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	today := now.Format("2006-01-02")
	out := make([]View, 0, len(p.accounts))
	for _, a := range p.accounts {
		// 只读口径：存量的日期不是今天 ⇒ 显示 0（今天还没出图），
		// 真正的清零发生在下一次 PickForImage / MarkImageDone。
		imageToday := 0
		if a.imageDay == today {
			imageToday = a.imageCount
		}
		videoToday := 0
		if a.videoDay == today {
			videoToday = a.videoCount
		}
		creditsUsed := 0
		if a.creditDay == today {
			creditsUsed = a.creditUsed
		}
		v := View{
			Name:         a.Name,
			Email:        a.Email,
			Note:         a.Note,
			Inflight:     int(atomic.LoadInt32(&a.inflight)),
			Failures:     a.failures,
			Served:       a.served,
			LastError:    a.lastError,
			ImageToday:   imageToday,
			ImageLimit:   p.cfg.MaxImagePerDay,
			VideoToday:   videoToday,
			VideoLimit:   p.cfg.MaxVideoPerDay,
			CreditsUsed:  creditsUsed,
			CreditsLimit: p.cfg.MaxVideoCreditsPerDay,
		}
		switch {
		case a.authDead:
			v.State = "auth_dead"
		case now.Before(a.cooldownUntil):
			v.State = "cooling"
			v.CooldownLeftS = int(a.cooldownUntil.Sub(now).Seconds())
		default:
			v.State = "ready"
		}
		if !a.lastUsed.IsZero() {
			v.LastUsed = a.lastUsed.Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}

// ReadyCount 可用（未冷却未失效）账号数。
func (p *Pool) ReadyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, a := range p.accounts {
		if !a.authDead && !now.Before(a.cooldownUntil) {
			n++
		}
	}
	return n
}

// ---------- 状态持久化（只存冷却与计数，不存 token） ----------

type persisted struct {
	Accounts map[string]persistedAccount `json:"accounts"`
	SavedAt  string                      `json:"saved_at"`
}

type persistedAccount struct {
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	Failures      int    `json:"failures,omitempty"`
	Served        int64  `json:"served,omitempty"`
	AuthDead      bool   `json:"auth_dead,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// ImageDay/ImageCount 每日出图配额（跨天由 rollImageDay 清零，跨重启保留）。
	ImageDay   string `json:"image_day,omitempty"`
	ImageCount int    `json:"image_count,omitempty"`
	// VideoDay/VideoCount 每日视频配额（同上）。
	VideoDay   string `json:"video_day,omitempty"`
	VideoCount int    `json:"video_count,omitempty"`
	// CreditDay/CreditUsed 每日视频积分消耗（同上）。
	CreditDay  string `json:"credit_day,omitempty"`
	CreditUsed int    `json:"credit_used,omitempty"`
}

func (p *Pool) save() {
	if p.state == "" {
		return
	}
	p.mu.RLock()
	out := persisted{Accounts: map[string]persistedAccount{}, SavedAt: time.Now().Format(time.RFC3339)}
	for _, a := range p.accounts {
		item := persistedAccount{
			Failures:   a.failures,
			Served:     a.served,
			AuthDead:   a.authDead,
			LastError:  a.lastError,
			ImageDay:   a.imageDay,
			ImageCount: a.imageCount,
			VideoDay:   a.videoDay,
			VideoCount: a.videoCount,
			CreditDay:  a.creditDay,
			CreditUsed: a.creditUsed,
		}
		if !a.cooldownUntil.IsZero() {
			item.CooldownUntil = a.cooldownUntil.Unix()
		}
		out.Accounts[a.Name] = item
	}
	p.mu.RUnlock()

	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(p.state)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.state + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.state)
}

func (p *Pool) restore() {
	if p.state == "" {
		return
	}
	raw, err := os.ReadFile(p.state)
	if err != nil {
		return
	}
	var in persisted
	if err := json.Unmarshal(raw, &in); err != nil {
		return
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		st, ok := in.Accounts[a.Name]
		if !ok {
			continue
		}
		a.failures = st.Failures
		a.served = st.Served
		a.lastError = st.LastError
		// 配额跨重启保留；若存的是昨天的日期，下次读到时 rollImageDay 会清零。
		a.imageDay = st.ImageDay
		a.imageCount = st.ImageCount
		a.videoDay = st.VideoDay
		a.videoCount = st.VideoCount
		a.creditDay = st.CreditDay
		a.creditUsed = st.CreditUsed
		// 🔴 authDead 不跨重启恢复：token 可能已经换了（配置文件被改过），
		// 一启动就全死会让"我明明更新了 token"变成玄学。冷却时间恢复，
		// 失效标记只在本次进程内有效。
		if st.CooldownUntil > 0 {
			t := time.Unix(st.CooldownUntil, 0)
			if t.After(now) {
				a.cooldownUntil = t
			}
		}
	}
}
