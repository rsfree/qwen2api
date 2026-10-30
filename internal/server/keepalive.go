package server

// 账号保活：在请求路径之外，提前把「接下来会被用到」的账号凭据换新。
//
// 为什么需要它（2026-10-30 实测）：
//   - 上游 access token TTL 只有 **900 秒**；
//   - signin 的 token cookie 一次性，但 refresh 下发的 refresh_token **可重复使用**；
//   - 3000 个账号均匀轮询时，每个号被用到时大概率已过期 —— 不预热的话
//     每个请求都要先花几百毫秒续期（甚至重新登录），延迟直接翻倍。
//
// 预热的顺序与选号一致（served 升序），所以"预热的就是下一步要用的"。
// 这样做的额外开销是每 20 秒最多 8 次 refresh，与请求量解耦、也不碰密码。

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

const (
	// keepAliveTick 预热轮询间隔。
	keepAliveTick = 20 * time.Second
	// keepAliveWindow 剩余寿命小于它就提前换新（要大于 tick 才不会"过期了才换"）。
	keepAliveWindow = 3 * time.Minute
	// keepAliveParallel 单个批次内的并发。
	keepAliveParallel = 4
	// keepAliveAdoptBatch 单轮最多补几个"还没 refresh_token"的号。
	// 12/轮 = 36/分钟 ⇒ 3000 个号约 80 分钟补完；补完之后每号 30 天只需保养一次。
	keepAliveAdoptBatch = 12
	// refreshMaintainWindow refresh_token 剩余不到这个时间就提前换新。
	// 它 TTL 是 30 天，留 3 天余量足够吸收停机/抖动。
	refreshMaintainWindow = 3 * 24 * time.Hour
	// refreshMaintainBatch 单轮最多保养几个（到期最急的先做）。
	refreshMaintainBatch = 2
)

// KeepAlive 后台保活循环（由 main 启动，测试里不跑）。
func (s *Server) KeepAlive(ctx context.Context) {
	t := time.NewTicker(keepAliveTick)
	defer t.Stop()
	// 启动就先跑一轮：重启后整池凭据都过期，等 20 秒才热身会让首批请求很慢。
	// 新凭据（access + refresh_token）必须落盘，否则重启后又要全量重登。
	// 用"最多两分钟写一次"折中：2000+ 账号的 accounts.json 有几百 KB，
	// 每个 tick 都写是浪费，而丢两分钟的续期结果无所谓（重新续一次就好）。
	lastSave := time.Now()
	saveIfDue := func(changed bool) {
		if !changed || time.Since(lastSave) < 2*time.Minute {
			return
		}
		if err := s.saveAccounts(); err != nil {
			log.Printf("[qwen2api] 保存续期结果失败: %v", err)
			return
		}
		lastSave = time.Now()
	}

	changed := s.adoptOnce(ctx)
	changed = s.warmOnce(ctx) || changed
	s.prewarmChats(ctx)
	changed = s.maintainRefresh(ctx) || changed
	saveIfDue(changed)
	for {
		select {
		case <-ctx.Done():
			// 退出前把最后一批续期结果写掉（重启后不用重登）。
			if err := s.saveAccounts(); err != nil {
				log.Printf("[qwen2api] 退出前保存续期结果失败: %v", err)
			}
			return
		case <-t.C:
			changed := s.adoptOnce(ctx)
			changed = s.warmOnce(ctx) || changed
			s.prewarmChats(ctx)
			changed = s.maintainRefresh(ctx) || changed
			saveIfDue(changed)
		}
	}
}

// adoptOnce 给"还没 refresh_token 的号"补一张（用密码登录一次）。
//
// 只补"接下来会被用到的那批"（served 升序），单轮 6 个、并发 2 ——
// 目的是把一次性的 3000 次登录摊成后台小流量，而不是再打一次批量登录。
func (s *Server) adoptOnce(ctx context.Context) bool {
	names := s.pool.AdoptCandidates(s.warmTarget(), keepAliveAdoptBatch)
	if len(names) == 0 {
		return false
	}
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	changed := false
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s.adoptOne(ctx, name) {
				mu.Lock()
				changed = true
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return changed
}

// maintainRefresh 给"refresh_token 快到期"的号续一张。
//
// refresh_token 的 TTL 是 30 天；到期后 refresh 会失效，只能密码重登。
// 在到期前用密码换新的，就把"某天整批突然失效"摊成了平滑的后台任务：
// 3000 个号 ÷ 30 天 ≈ 每天 100 次，每轮 2 个绰绰有余。
func (s *Server) maintainRefresh(ctx context.Context) bool {
	names := s.pool.RefreshExpiring(refreshMaintainWindow, refreshMaintainBatch)
	if len(names) == 0 {
		return false
	}
	changed := false
	for _, n := range names {
		if s.adoptOne(ctx, n) { // 同一个动作：signin 换新 access + refresh_token
			changed = true
		}
	}
	if changed {
		log.Printf("[qwen2api] refresh_token 保养 %d 个（剩余不足 3 天）", len(names))
	}
	return changed
}

// adoptOne 登录一次并把 access + refresh_token 都存下来。
func (s *Server) adoptOne(ctx context.Context, name string) bool {
	a := s.pool.Get(name)
	if a == nil || a.Password == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	access, refresh, err := s.up.SignInSession(upstream.WithAccount(cctx, name), a.Email, a.Password)
	if err != nil {
		s.noteRefreshErr(name, err)
		return false
	}
	s.pool.SetToken(name, access)
	if refresh != "" {
		s.pool.SetRefresh(name, refresh)
	}
	return true
}

// warmBatch 每轮预热多少个 —— 跟着目标数一起缩放。
//
// 🔴 为什么要动态（2026-10-30）：目标数按流量估算（8~256），但批量原来是常量 8，
// 于是一旦并发突然上来（压测、批量导入后、上游恢复瞬间），热满 256 个要
// 256/8×20s ≈ 10 分钟 —— 这期间请求全在撞过期号，每个多几百毫秒续期。
//
// 取 1/8 让"热满耗时"稳定在约 8 轮（≈2.7 分钟），与目标规模无关；
// 下限 4 保证低流量时不至于太慢，上限 48 避免一轮把上游打太热。
func warmBatch(target int) int {
	b := target / 8
	if b < 4 {
		b = 4
	}
	if b > 48 {
		b = 48
	}
	return b
}

// warmOnce 跑一轮预热。
func (s *Server) warmOnce(ctx context.Context) bool {
	target := s.warmTarget()
	batch := warmBatch(target)
	names := s.pool.WarmCandidates(target, keepAliveWindow, batch)
	// 即使候选为空也要记：否则 /stats 的 warm.target 永远停在 0（观测失真）。
	s.noteWarm(target, batch, len(names))
	if len(names) == 0 {
		return false
	}
	sem := make(chan struct{}, keepAliveParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	changed := false
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s.refreshTokenOnly(ctx, name) {
				mu.Lock()
				changed = true
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return changed
}

// prewarmChats 给"马上要被用到的那批号"预建空会话。
//
// 覆盖范围与 access token 预热**同源**：都是 warmTarget（按最近 10 分钟流量估算），
// 单轮数量用 warmBatch 一起缩放。理由也一样 —— 目标数会随并发涨到几百，
// 固定批量会让"热满"耗时随规模线性增长。
//
// 只补 t2t：对话占请求的绝大多数；t2i / image_edit / t2v 一次就要 50~90 秒，
// 建会话那 2.4s 占比可以忽略，不值得为它们维护池子。
func (s *Server) prewarmChats(ctx context.Context) {
	if !s.chats.Enabled() {
		return
	}
	if n := s.chats.Trim(); n > 0 {
		log.Printf("[qwen2api] 会话池清理过期 %d 个", n)
	}
	target := s.warmTarget()
	names := s.pool.HotNames(target)
	if len(names) == 0 {
		return
	}
	limit := warmBatch(target)
	sem := make(chan struct{}, 4) // 建会话 2.4s/个，4 并发约 6 个/秒
	var wg sync.WaitGroup
	built := 0
	var mu sync.Mutex
	for _, n := range names {
		if built >= limit {
			break
		}
		if s.chats.Short(n, "t2t") == 0 {
			continue
		}
		// 只用新鲜凭据：过期的号建了也会被上游拒（且白占一次代理往返）。
		if !s.tokenFresh(n, time.Minute) {
			continue
		}
		wg.Add(1)
		built++
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cred := s.pool.Credential(name)
			if cred == "" {
				return
			}
			s.prewarmChat(ctx, name, cred, s.cfg.DefaultModel, "t2t")
			mu.Lock()
			defer mu.Unlock()
		}(n)
	}
	wg.Wait()
}

// noteWarm 记录最近一轮预热的参数（/stats 与面板观测用）。
func (s *Server) noteWarm(target, batch, actual int) {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	s.warmStat = WarmStat{
		Target: target, Batch: batch, Actual: actual, At: time.Now(),
	}
}

// WarmStat 最近一轮预热的参数快照。
type WarmStat struct {
	Target int       `json:"target"` // 估算需要保持可用的账号数
	Batch  int       `json:"batch"`  // 本轮取了几个候选
	Actual int       `json:"actual"` // 实际取到的（候选不足时会小于 batch）
	At     time.Time `json:"at"`
}

// WarmSnapshot 最近一轮预热参数。
func (s *Server) WarmSnapshot() WarmStat {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	return s.warmStat
}

// warmTarget 估算"要随时保持可用的账号数"。
//
// 每个 access 活 15 分钟，所以速率为 r 次/分钟时需要 r*15 个可用凭据。
// 用最近 10 分钟的实际请求数推算，低流量时只热几个号（不做无用续期），
// 高流量时自动扩到上限。
func (s *Server) warmTarget() int {
	const lookback = 10 * time.Minute
	cut := time.Now().Add(-lookback)
	n := 0
	for _, e := range s.log.Recent(200) {
		if e.Time.After(cut) {
			n++
		}
	}
	// 采样窗口可能被截断（Recent 有上限），按"至少 1 次"算，避免 0 导致不预热。
	rate := float64(n) / lookback.Minutes()
	target := int(rate*15*1.5) + 8
	if target < 8 {
		target = 8
	}
	// 上限不是拍脑袋：热满时间必须 < access TTL，否则刚热完就开始过期。
	// 每轮 warmBatch(512)=48 个、20s 一轮 ⇒ 144/分钟 ⇒ 512 个约 3.6 分钟热满，
	// 离 15 分钟的 TTL 还有 11 分钟余量；再往上（1024+）热满要 7~15 分钟，
	// 边际收益趋零且会把 auth 域打热。
	if target > 512 {
		target = 512
	}
	return target
}

// refreshTokenOnly 只用 refresh_token 换新（不碰密码）。
// 失败只记日志：下一轮会重试，真不行时请求路径上的 relogin 会兜底。
func (s *Server) refreshTokenOnly(ctx context.Context, name string) bool {
	rt := s.pool.RefreshToken(name)
	if rt == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	access, next, err := s.up.RefreshWithToken(upstream.WithAccount(cctx, name), rt)
	if err != nil {
		s.noteRefreshErr(name, err)
		return false
	}
	s.pool.SetToken(name, upstream.ModeBearer+":"+access)
	if next != "" {
		s.pool.SetRefresh(name, next)
	}
	return true
}

// noteRefreshErr 续期失败日志去重（同一个号 5 分钟内只记一次）。
//
// 3000 个号同时失败时，逐条打日志会把日志刷爆，反而看不到别的问题。
func (s *Server) noteRefreshErr(name string, err error) {
	s.creditErrMu.Lock()
	last, ok := s.refreshErrAt[name]
	now := time.Now()
	if ok && now.Sub(last) < 5*time.Minute {
		s.creditErrMu.Unlock()
		return
	}
	if s.refreshErrAt == nil {
		s.refreshErrAt = map[string]time.Time{}
	}
	s.refreshErrAt[name] = now
	s.creditErrMu.Unlock()
	log.Printf("[qwen2api] 账号 %s 预热续期失败: %v", name, err)
}
