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
	// keepAliveBatch 单轮最多换几个 —— 别把上游打热。
	keepAliveBatch = 8
	// keepAliveParallel 单个批次内的并发。
	keepAliveParallel = 4
)

// KeepAlive 后台保活循环（由 main 启动，测试里不跑）。
func (s *Server) KeepAlive(ctx context.Context) {
	t := time.NewTicker(keepAliveTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.warmOnce(ctx)
		}
	}
}

// warmOnce 跑一轮预热。
func (s *Server) warmOnce(ctx context.Context) {
	names := s.pool.WarmCandidates(s.warmTarget(), keepAliveWindow, keepAliveBatch)
	if len(names) == 0 {
		return
	}
	sem := make(chan struct{}, keepAliveParallel)
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.refreshTokenOnly(ctx, name)
		}(n)
	}
	wg.Wait()
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
	if target > 256 {
		target = 256
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
