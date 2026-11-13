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
	"hash/fnv"
	"log"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

const (
	// refreshMaintainWindow refresh_token 剩余不到这个时间就提前换新。
	// 它 TTL 是 30 天，留 3 天余量足够吸收停机/抖动。
	refreshMaintainWindow = 3 * 24 * time.Hour
	// refreshMaintainBatch 单轮最多保养几个（到期最急的先做）。
	refreshMaintainBatch = 2
	// refreshRetryBackoff 续期失败后的静默期。
	//
	// 预热带是按 tick 轮转的：不压住失败号，它就会被每轮重新选中，对同一张可能
	// 已经作废的 refresh_token 持续打 auth 域 —— 既无意义，还容易招风控。
	// 取 5 分钟，与失败日志的去重窗口一致：一个号一次故障只留一条日志、一次处置。
	// ⚠️ 只压「结果未知」的失败。401 不进这张表：票已被撤销是确定性结论，
	// 再退避只会推迟唯一有效的修复动作（见 afterRefreshFailure）。
	refreshRetryBackoff = 5 * time.Minute
	// refreshLockShards 换票互斥的分片数（见 lockRefresh）。
	refreshLockShards = 64
)

// keepaliveTick 预热轮询间隔（配置可热更，改完下一轮生效）。
func (s *Server) keepaliveTick() time.Duration {
	ka := s.conf().Pool.Keepalive.Effective()
	return time.Duration(ka.TickS) * time.Second
}

// KeepAlive 后台保活循环（由 main 启动，测试里不跑）。
func (s *Server) KeepAlive(ctx context.Context) {
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

	// runOne 跑一轮完整预热：补 refresh_token → 续 access → 预建会话/保活连接
	// → 保养 refresh_token。
	//
	// 🔴 peak 每轮只取一次：它是"上一轮到现在"的并发峰值，几个环节必须共用
	// 同一个数 —— 让每个函数各调一次 Swap(0) 的话，第一个调用者就把峰值吃掉，
	// 后面的环节全看到 0，等于白算。
	runOne := func() {
		peak := int(s.inflightPeak.Swap(0))
		// 顺带维护上游模型清单：请求路径要拿它拒绝"已下架的模型"（见
		// openUpstreamSpec），但校验本身绝不能去打上游 —— 所以刷新放在这里，
		// 每轮一次（缓存 10 分钟，绝大多数轮次是空转）。
		_ = s.models.List(ctx)
		changed := s.adoptOnce(ctx, peak)
		changed = s.warmOnce(ctx, peak) || changed
		s.prewarmChats(ctx, peak)
		changed = s.maintainRefresh(ctx) || changed
		saveIfDue(changed)
	}

	runOne()
	for {
		select {
		case <-ctx.Done():
			// 退出前把最后一批续期结果写掉（重启后不用重登）。
			if err := s.saveAccounts(); err != nil {
				log.Printf("[qwen2api] 退出前保存续期结果失败: %v", err)
			}
			return
		case <-time.After(s.keepaliveTick()):
			runOne()
		}
	}
}

// adoptOnce 给"还没 refresh_token 的号"补一张（用密码登录一次）。
//
// 只补"接下来会被用到的那批"（served 升序），单轮数量走配置、并发固定 2 ——
// 目的是把一次性的 3000 次登录摊成后台小流量，而不是再打一次批量登录。
// 并发刻意不跟 Parallel 走：密码登录是最容易吃 WAF 的一步，快没有意义。
func (s *Server) adoptOnce(ctx context.Context, peak int) bool {
	ka := s.conf().Pool.Keepalive.Effective()
	names := s.pool.AdoptCandidates(s.warmTarget(peak), ka.AdoptBatch)
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
	// 与预热 refresh 互斥：signin 会换掉整张会话，和 refresh 交叉执行时
	// 会把对方刚拿到的票作废（见 lockRefresh）。
	unlock := s.lockRefresh(name)
	defer unlock()
	// 与 refresh 共用退避表：登录失败（尤其被 WAF 拦）不能每轮都撞一次 ——
	// 一批密码失效的号足以把 signin 打成主要流量，还会挤占真正可修的重登名额。
	if !s.refreshDue(name, time.Now()) {
		return false
	}
	a := s.pool.Get(name)
	if a == nil || a.Password == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	access, refresh, err := s.up.SignInSession(upstream.WithAccount(cctx, name), a.Email, a.Password)
	if err != nil {
		s.noteRefreshErr(name, err)
		// 401（密码错）/ 400（未激活、封禁）是**永久**失败：再试一万次也一样，
		// 只会白吃 WAF 并一直占着重登队列最前面。403 除外 —— 那多半是 WAF
		// 挑战页，退避一轮再来才是对的。
		if upstream.KindOf(err) == upstream.KindAuth && upstream.StatusOf(err) != 403 {
			s.pool.MarkAuth(a, "signin: "+err.Error())
			log.Printf("[qwen2api] 账号 %s 重登被上游永久拒绝，已停用: %v", name, err)
		}
		return false
	}
	s.pool.SetToken(name, access)
	if refresh != "" {
		s.pool.SetRefresh(name, refresh)
	}
	s.clearRefreshErr(name) // 修好了就别留着上一次的失败时间戳
	// 成功必须留痕：否则「自愈」是静默的，出问题时分不清「还没轮到它」和
	// 「早已修好」，只剩一条失败日志可看。
	log.Printf("[qwen2api] 账号 %s 后台重登成功，凭据已换新", name)
	return true
}

// warmBatch 每轮预热多少个 —— 跟着目标数一起缩放。
//
// 两个约束同时要满足：
//  1. **热满要快**：取 target/8 让热满耗时稳定在约 8 轮，与规模无关
//     （原来是常量 8，并发一上来热满 512 个要 21 分钟，期间请求全在撞冷号）；
//  2. **保活要跟上连接 TTL**：连接只活 90 秒，一轮必须走够 target×tick/90 个，
//     否则先保活的那批早就被回收了 —— 这一轮等于白跑。
//
// 下限 4 保证低流量时也能热起来，上限走配置（batch_max，默认 48）。
func warmBatch(target, max, tickS int) int {
	if max <= 0 {
		max = 48
	}
	if tickS <= 0 {
		tickS = 20
	}
	b := target / 8
	// 连接只活 TTL，一轮走不完就得在 TTL 内轮一遍：
	// 一个 TTL 里能跑 rounds 轮，每轮至少要覆盖 ceil(target/rounds) 个。
	ttlS := int(upstream.IdleConnTimeout / time.Second)
	rounds := ttlS / tickS
	if rounds < 1 {
		rounds = 1
	}
	if connNeed := (target + rounds - 1) / rounds; connNeed > b {
		b = connNeed
	}
	if b < 4 {
		b = 4
	}
	if b > max {
		b = max
	}
	return b
}

// warmTarget 估算"要随时保持可用的账号数"（token 口径）。
//
// 每个 access 活 15 分钟，所以速率为 r 次/分钟时需要 r*15 个可用凭据。
// 用最近 10 分钟的实际请求数推算，低流量时只热几个号（不做无用续期）。
//
// 并发兜底（2026-11-02）：只用速率会漏掉"短时间打满"的情况 —— 10 分钟内
// 只有 20 个请求，但全挤在 3 秒里，那 20 个号必须同时可用。所以再拿
// 上一轮的并发峰值算一遍，取较大者。
func (s *Server) warmTarget(peak int) int {
	ka := s.conf().Pool.Keepalive.Effective()
	const lookback = 10 * time.Minute
	cut := time.Now().Add(-lookback)
	n := 0
	// Recent(0) = 取满整个环形缓冲（容量 500）。写死 200 会在高流量时低估速率。
	for _, e := range s.log.Recent(0) {
		if e.Time.After(cut) {
			n++
		}
	}
	// 采样窗口可能被截断，按"至少 1 次"算，避免 0 导致不预热。
	rate := float64(n) / lookback.Minutes()
	target := int(rate*15*1.5) + 8
	if need := peak*ka.ConcFactor + 4; need > target {
		target = need
	}
	if target < 8 {
		target = 8
	}
	// 上限不是拍脑袋：热满时间必须 < access TTL，否则刚热完就开始过期。
	if target > ka.TargetMax {
		target = ka.TargetMax
	}
	return target
}

// chatTarget 会话/连接要覆盖多少个号。
//
// 🔴 与 warmTarget 分开（2026-11-02）：token 有 15 分钟 TTL，必须提前热一大片；
// 但空会话和出口连接只服务"同时在跑的请求" —— 并发 5 的场景下保活 512 条连接，
// 等于每 20 秒白打 500 次代理往返（每次 0.4~2 秒），把上游和代理都打热。
//
// 所以按并发峰值算，再用 token 目标封顶（不会超过"实际会用到的那批"）。
func (s *Server) chatTarget(peak, tokenTarget int) int {
	ka := s.conf().Pool.Keepalive.Effective()
	n := peak*ka.ConcFactor + 4
	if n < 4 {
		n = 4
	}
	if n > tokenTarget {
		n = tokenTarget
	}
	return n
}

// warmOnce 跑一轮 access token 续期。
func (s *Server) warmOnce(ctx context.Context, peak int) bool {
	ka := s.conf().Pool.Keepalive.Effective()
	target := s.warmTarget(peak)
	batch := warmBatch(target, ka.BatchMax, ka.TickS)
	window := time.Duration(ka.TokenWindowS) * time.Second
	names := s.pool.WarmCandidates(target, window, batch)
	// 即使候选为空也要记：否则 /stats 的 warm.target 永远停在 0（观测失真）。
	s.noteWarm(target, batch, len(names), peak)
	if len(names) == 0 {
		return false
	}
	sem := make(chan struct{}, ka.Parallel)
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

// prewarmChats 给"马上要被用到的那批号"预建空会话，并把它们的出口连接保活。
//
// 覆盖范围按**选号面**算（warmTarget），不是按并发（chatTarget）——
// 这条是 2026-11-12 用线上数据纠正的：低流量下号轮转周期远大于连接寿命，
// 按并发保活等于让每个请求都重新握一次 SOCKS5+TLS。
// 详见 prewarmChats 里的注释。
//
// 池子已经满的号不会重复建会话（那是白花一次上游调用），但会发一个 HEAD 保住
// TCP+TLS —— 实测走住宅代理重新握手要 1.7 秒，比建会话便宜得多，收益却一样直接。
//
// 只补 t2t：对话占请求的绝大多数；t2i / image_edit / t2v 一次就要 50~90 秒，
// 建会话那 2.4s 占比可以忽略，不值得为它们维护池子。
func (s *Server) prewarmChats(ctx context.Context, peak int) {
	if !s.chats.Enabled() {
		// 池被关掉（per=0）⇒ 没有任何号"手里有货"。不清标志的话，选号会一直
		// 优先撞那些残留标志为 true 的号，而它们每次都要现建 3 秒。
		s.pool.SetChatReady(nil)
		return
	}
	if n := s.chats.Trim(); n > 0 {
		log.Printf("[qwen2api] 会话池清理过期 %d 个", n)
	}
	ka := s.conf().Pool.Keepalive.Effective()
	// 🔴 need 用 warmTarget，不是 chatTarget（2026-11-12 线上实测修正）。
	//
	// chatTarget 的原始论证是"会话和连接只服务同时在跑的请求"，它隐含假设
	// **选号面 = 并发**。但这个假设不成立：选号面是"token 还活着的号"
	// （实测 139 个，而峰值并发只有 1~2）。低流量下同一个号要几十分钟才轮到
	// 一次，连接早被回收 —— 于是每次请求都在付一次 SOCKS5+TLS。日志里能直接
	// 看到：同一个号第一次 submit 6024ms、第二次 984ms。
	//
	// 保活面必须覆盖选号面，否则"省下来的保活"会以"每次多 2~5 秒首字"
	// 的形式十倍还回去。
	need := s.warmTarget(peak)
	s.chatTargetNow.Store(int32(need))
	names := s.pool.HotNames(need)
	if len(names) == 0 {
		return
	}
	// 把"谁手里有货 **且本轮会被保活**"同步给选号（见 pool.SetChatReady）。
	//
	// 🔴 必须取交集，不能直接拿 ReadyAccounts：会话池里会残留"上一轮预热建的、
	// 但已经不在热点集里"的号（实测 ready=116 而保活面只有 80）。它们的会话是
	// 现成的，chatReady 一置真，选号就优先挑它们 —— 但它们的**连接没人管**，
	// 于是照样要付 2~5 秒握手。表现就是"命中率 99% 但 submit 还是 2.8 秒"。
	//
	// 交集之后：有货 ⇒ 一定在热点集 ⇒ 连接被游标保活。"命中"才真正等于"热"。
	ready := capReadyToHot(s.chats.ReadyAccounts(), names)
	// 位置在 Trim 之后：刚清掉的那批必须立刻从标志里消失。
	// 本轮后续补货写入时会单点置真（见 prewarmChat），不必等下一轮。
	s.pool.SetChatReady(ready)
	limit := warmBatch(need, ka.BatchMax, ka.TickS)
	// 按游标取一段，而不是"每轮都从头取 limit 个"：
	// HotNames 的排序键（alive / served / tokenExp）不会被预热自己改写，
	// 不轮转的话第一轮的前 8 个就是永远的前 8 个，其余号的连接永远冷着。
	names = s.rotateHot(names, limit)
	sem := make(chan struct{}, ka.Parallel)
	var wg sync.WaitGroup
	for _, n := range names {
		// 建会话要凭据：过期的号建了也会被上游拒（且白占一次代理往返）。
		// 但**连接保活不需要凭据**，所以过期号照样往下走 —— 下次请求用到它时
		// 请求路径会先续期，那条已经握好的连接正好复用。
		needChat := s.chats.Short(n, "t2t") > 0 && s.tokenFresh(n, time.Minute)
		wg.Add(1)
		go func(name string, needChat bool) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if needChat {
				cred := s.pool.Credential(name)
				if cred == "" {
					return
				}
				s.prewarmChat(ctx, name, cred, s.conf().DefaultModel, "t2t")
				return
			}
			// 这条号马上会被用到，保住它的出口连接就够了（实测省 1.7 秒握手）。
			_ = s.up.WarmConn(upstream.WithAccount(ctx, name))
		}(n, needChat)
	}
	wg.Wait()
}

// capReadyToHot 把"有预建会话"限制在"本轮会被保活"的号内。
//
// 为什么必须相交（2026-11-12 线上实测）：会话池会残留上一轮预热建的、但已经
// 不在热点集里的号（实测 ready=116，保活面只有 80）。它们的会话是现成的，
// chatReady 一置真，选号就优先挑它们 —— 但它们的连接没人管，照样要付
// 2~5 秒握手。表现就是"命中率 99% 却 submit 2.8 秒"。
//
// 相交之后语义才闭合：**有货 ⇒ 一定在热点集 ⇒ 连接被游标保活**，
// 于是"命中"同时等于"会话热"和"连接热"。
func capReadyToHot(ready map[string]struct{}, hot []string) map[string]struct{} {
	keep := make(map[string]struct{}, len(hot))
	for _, n := range hot {
		keep[n] = struct{}{}
	}
	for name := range ready {
		if _, ok := keep[name]; !ok {
			delete(ready, name)
		}
	}
	return ready
}

// rotateHot 从 names 里按游标环形取 limit 个。
//
// 存在的理由：HotNames 的排序是 (alive, served, tokenExp)，而预热本身不改这三个值。
// 如果每轮都取前 limit 个，头部那批会被反复热身，尾部的号永远轮不到 ——
// 偏偏选号（lessPick）也用同一套排序，于是 served 一旦摊开，请求打到的就是
// 一批从没保活过的冷号。游标让"多轮下来每个号都被轮到"，与 need 一起保证
// 一个 TTL 内覆盖完整个选号面。
func (s *Server) rotateHot(names []string, limit int) []string {
	if limit <= 0 || limit >= len(names) {
		return names
	}
	off := int(s.chatCursor.Load()) % len(names)
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, names[(off+i)%len(names)])
	}
	s.chatCursor.Store(uint32((off + limit) % len(names)))
	return out
}

// noteInflight 记录一次 chat 请求进出，并维护"上一轮预热以来的并发峰值"。
//
// 用 CAS 而不是加锁：每个请求只进来一次，锁会成为高频请求下的争用点。
func (s *Server) noteInflight(delta int32) {
	if s == nil {
		return
	}
	n := s.inflightNow.Add(delta)
	if delta <= 0 || n <= 0 {
		return
	}
	for {
		p := s.inflightPeak.Load()
		if n <= p || s.inflightPeak.CompareAndSwap(p, n) {
			return
		}
	}
}

// noteWarm 记录最近一轮预热的参数（/stats 与面板观测用）。
func (s *Server) noteWarm(target, batch, actual, peak int) {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	s.warmStat = WarmStat{
		Target: target, Batch: batch, Actual: actual, Peak: peak, At: time.Now(),
	}
}

// WarmStat 最近一轮预热的参数快照。
type WarmStat struct {
	Target int       `json:"target"` // 估算需要保持可用的账号数（token 口径）
	Batch  int       `json:"batch"`  // 本轮取了几个候选
	Actual int       `json:"actual"` // 实际取到的（候选不足时会小于 batch）
	Peak   int       `json:"peak"`   // 本轮用到的并发峰值（会话/连接按它算）
	At     time.Time `json:"at"`
}

// WarmSnapshot 最近一轮预热参数。
func (s *Server) WarmSnapshot() WarmStat {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	return s.warmStat
}

// refreshTokenOnly 只用 refresh_token 换新（不碰密码）。
//
// 失败分两种，处置必须不同 —— 这是 2026-11-02 那次"timeout 之后紧接着 401
// 令牌已撤销"暴露出来的：
//
//   - **明确失效**（HTTP 401）：这张票已经不能用了，清掉它。AdoptCandidates 只挑
//     "没有 refresh_token"的号，清空是唯一能让后台用密码补一张的入口。留着它只会
//     让预热每 20s 撞一次 401（日志被 5 分钟去重，看着像"偶发"，其实一直在打）。
//   - **结果未知**（超时 / 网络）：HTTP 超时 ≠ 没执行，上游很可能已经消费掉这张
//     票。所以既不拿它原地重试，也不立刻重登（可能白花一次最容易吃 WAF 的
//     signin），先静默 refreshRetryBackoff 再说。
//
// ⚠️ 清空不等于"马上修复"：这个号会退出预热队列，修复时机交回两处 ——
// 后台 adoptOnce（只补"接下来要用"的那批）或请求路径上的 relogin（真正要用时）。
// 这样不会为了一个冷号白花密码登录，也不会让它继续污染 auth 域。
func (s *Server) refreshTokenOnly(ctx context.Context, name string) bool {
	unlock := s.lockRefresh(name)
	defer unlock()
	// 拿到锁后复查：等锁这一小会儿里，请求路径可能已经把它续好了。
	// 再换一张纯属浪费 —— 每多换一次就多一次撞 401「令牌已撤销」的机会。
	ka := s.conf().Pool.Keepalive.Effective()
	if s.tokenFresh(name, time.Duration(ka.TokenWindowS)*time.Second) {
		return false
	}
	rt := s.pool.RefreshToken(name)
	if rt == "" {
		return false
	}
	if !s.refreshDue(name, time.Now()) {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	access, next, err := s.up.RefreshWithToken(upstream.WithAccount(cctx, name), rt)
	if err != nil {
		s.noteRefreshErr(name, err)
		s.afterRefreshFailure(name, err)
		return false
	}
	s.pool.SetToken(name, upstream.ModeBearer+":"+access)
	if next != "" {
		s.pool.SetRefresh(name, next)
	}
	s.clearRefreshErr(name)
	return true
}

// lockRefresh 按账号名取一把分片锁，返回解锁函数。
//
// 为什么需要它：同一个号可能被三条路径同时换票 —— 预热的 refresh、请求路径的
// relogin、后台的 signin。并发换票会让一方刚拿到的票被另一方作废（实测
// 「timeout 之后紧接着 401 令牌已撤销」的嫌疑来源），而每次多余失败都要用
// 密码 signin 收尾 —— 那正是最贵、最该避免的一步。
//
// 用固定分片而不是 per-account 的 map：3135 个号各一把锁要常驻一张表，
// 而真实并发换票只有个位数，64 片几乎不会碰撞。
func (s *Server) lockRefresh(name string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	mu := &s.refreshLocks[h.Sum32()%refreshLockShards]
	mu.Lock()
	return mu.Unlock
}

// afterRefreshFailure 按错误种类决定这个号下一步怎么办。
//
// 只对 401 清票：403 在 auth 域通常是 WAF/风控挑战（KindAuth 但票没坏），
// 清掉反而会把一个还能用的号推去密码重登 —— 那才是最容易被拦的一步。
func (s *Server) afterRefreshFailure(name string, err error) {
	if upstream.KindOf(err) != upstream.KindAuth || upstream.StatusOf(err) != 401 {
		return // 其余情况只退避（时间戳已由 noteRefreshErr 记下）
	}
	if s.pool.SetRefresh(name, "") {
		// 没票又没密码 = 已经没有任何续期手段。留在池里只会显示"可用"、
		// 却永远不被选中（lessPick 把它排最后），是最难排查的那种僵尸号。
		if a := s.pool.Get(name); a != nil && a.Password == "" {
			s.pool.MarkAuth(a, "refresh_token 已撤销，且未保存密码，无法重登")
			log.Printf("[qwen2api] 账号 %s 的 refresh_token 已撤销且没有密码，已停用", name)
			return
		}
		// 打上 needLogin：AdoptCandidates 会把它插到队列最前面（趁请求还没
		// 碰到就修好），lessPick 也会把它排到最后（请求路径不再撞上几秒的 signin）。
		s.pool.MarkNeedLogin(name)
		// 🔴 401 是**确定性**失败：票已经清空，不存在「再撞同一张死票」的风险，
		// 退避在这里没有防护价值 —— 留着它只会把唯一有效的修复动作（密码重登）
		// 推迟满 5 分钟。实测 2026-11-13 17:06 一批 15 个号同时被撤销，最早的重登
		// 尝试落在 5m21s 后、最晚 7m49s 后，这段停摆本可以只有一个 tick（20s）。
		s.clearRefreshErr(name)
		log.Printf("[qwen2api] 账号 %s 的 refresh_token 已失效（401），插队重登", name)
	}
}

// refreshDue 上次失败是否已过静默期（见 refreshRetryBackoff）。
//
// 复用 creditErrMu：这张表只在预热路径上读写，频率极低，不值得再加一把锁。
func (s *Server) refreshDue(name string, now time.Time) bool {
	s.creditErrMu.Lock()
	defer s.creditErrMu.Unlock()
	last, ok := s.refreshErrAt[name]
	return !ok || now.Sub(last) >= refreshRetryBackoff
}

// clearRefreshErr 续期成功后抹掉失败记录，避免静默期把这个号白挡一轮。
func (s *Server) clearRefreshErr(name string) {
	s.creditErrMu.Lock()
	delete(s.refreshErrAt, name)
	s.creditErrMu.Unlock()
}

// noteRefreshErr 记录续期失败：一份数据两个用途 —— 日志去重 + 重试退避。
//
// 3000 个号同时失败时，逐条打日志会把日志刷爆，反而看不到别的问题；
// 而"上次失败是什么时候"正好也是 refreshDue 需要的退避起点，不必另开状态。
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
	log.Printf("[qwen2api] 账号 %s 续期失败: %v", name, err)
}
