package server

// 账号真实积分余额的刷新与记账。
//
// 为什么需要这一层：视频按积分计费，而 40 分预算只是"猜"——同一个号可能是
// 免费号（每天 40）、会员号（订阅 270）、或昨天剩 10。只有上游账本知道真实值。
//
// 端点见 upstream/credits.go（sg-entitlement.qwen.com，第三个域名）。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

const (
	// creditTTLStats /stats（面板）用的更长 TTL：人看数据不需要 60s 级实时，
	// 但它会定时轮询，别让轮询把三方域名打热。
	creditTTLStats = 2 * time.Minute

	// maxCreditRefreshesPerRound 单轮最多刷多少个账号的余额。
	//
	// 🔴 大池子必须有这道闸：3000+ 账号时"把过期的全刷一遍"= 每 2 分钟打上游
	// 3000 次（每个号还要过一条代理）。一轮只刷 32 个、游标轮转，几小时内覆盖全池，
	// 单个号的余额最多"旧几轮"，而余额只在视频计费时才用来拦截 —— 由上游
	// insufficient_quota 兜底，不会误放行花钱动作。
	maxCreditRefreshesPerRound = 32
)

// creditRefreshMu 保证同一时刻只有一轮刷新在跑。
//
// 不加它的话，并发 10 个视频请求会各刷一遍 8 个账号 = 80 次外部调用。
var creditRefreshMu sync.Mutex

// refreshStaleCredits 把余额过期（或没读过）的账号刷一遍。
//
// 失败**不阻断**：读不到就标"未知"，选号退回不按余额拦，
// 由上游的 insufficient_quota 兜底（那条已经能被正确识别成 429）。
func (s *Server) refreshStaleCredits(ctx context.Context, ttl time.Duration) {
	names := s.pool.CreditStale(ttl)
	if len(names) == 0 {
		return
	}
	if !creditRefreshMu.TryLock() {
		return // 已有一轮在跑，本次用现有缓存值
	}
	defer creditRefreshMu.Unlock()

	// 大池子限流 + 轮转：不加这一步，3000 个账号会让面板轮询每 2 分钟
	// 触发 3000 次上游调用（实测首次导入后就是这样起风暴的）。
	if len(names) > maxCreditRefreshesPerRound {
		var next int
		names, next = rotateWindow(names, s.creditCursor, maxCreditRefreshesPerRound)
		s.creditCursor = next
	}

	// 整体加个上限：这是辅助动作，不该拖慢主请求。
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			c, err := s.up.Credits(upstream.WithAccount(ctx, name), s.pool.Credential(name))
			if err != nil {
				s.noteCreditErr(name, err)
				s.pool.SetCreditBalance(name, -1) // 标未知，不拦
				return
			}
			s.pool.SetCreditBalance(name, c.Total)
		}(n)
	}
	wg.Wait()
}

// rotateWindow 从 names 里取 [cursor, cursor+n) 的窗口（越界环绕），
// 并返回下一次的 cursor。
//
// 单独抽出来是因为"轮转"最容易写错且不好从外部观察：直接对 3000 个名字
// 取前 32 个，会让后面 2900 个**永远**轮不到，余额永远显示"未知"。
func rotateWindow(names []string, cursor, n int) ([]string, int) {
	if n <= 0 || len(names) == 0 {
		return nil, cursor
	}
	if n >= len(names) {
		out := make([]string, len(names))
		copy(out, names)
		return out, 0
	}
	start := cursor % len(names)
	if start < 0 {
		start += len(names)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, names[(start+i)%len(names)])
	}
	return out, (start + n) % len(names)
}

// RefreshCredits 给面板用的余额刷新。
//
// force=false 走 2 分钟 TTL（面板 5s 轮询不会把三方域名打热）；
// force=true 无视 TTL（用户点「刷新余额」时的明确意图）。
func (s *Server) RefreshCredits(ctx context.Context, force bool) {
	ttl := creditTTLStats
	if force {
		ttl = 0
		// 用户主动点「刷新余额」＝要立刻看到新数字，这里同步等结果。
		s.refreshStaleCredits(ctx, ttl)
		return
	}
	// 🔴 轮询路径**必须异步**：3000+ 账号时一轮刷新最坏等满 10s 超时，
	// 而面板 5s 就轮询一次 —— 同步会直接把请求叠起来（实测 overview 10.7s）。
	// refreshStaleCredits 内部 TryLock，重复触发不会堆叠。
	go s.refreshStaleCredits(context.Background(), ttl)
}

// refreshOneCredit 只刷一个账号的余额，返回是否拿到了值。
//
// 视频路径用：选号之后才知道是哪个号，没必要为了一个号把 32 个都刷一遍。
func (s *Server) refreshOneCredit(ctx context.Context, name string) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	c, err := s.up.Credits(upstream.WithAccount(ctx, name), s.pool.Credential(name))
	if err != nil {
		s.noteCreditErr(name, err)
		s.pool.SetCreditBalance(name, -1)
		return false
	}
	s.pool.SetCreditBalance(name, c.Total)
	return true
}

// noteCreditErr 余额读取失败只记一次日志（否则一次网络抖动会刷满屏幕）。
func (s *Server) noteCreditErr(name string, err error) {
	s.creditErrMu.Lock()
	last := s.creditErrAt
	shouldLog := time.Since(last) > 5*time.Minute
	if shouldLog {
		s.creditErrAt = time.Now()
	}
	s.creditErrMu.Unlock()
	if shouldLog {
		log.Printf("[qwen2api] 读取积分余额失败（%s）：%v —— 本次不做余额拦截，"+
			"由上游 insufficient_quota 兜底", name, err)
	}
}

// creditsShortfall 判断"选不出账号"是不是因为钱不够，并给出可读原因。
//
// 为什么单独做判断：选号失败有两种完全不同的原因 ——
//   - 全部冷却/失效  ⇒ 该等冷却或加号（errNoAccount）
//   - 钱不够        ⇒ 该充值/等每日额度（429 insufficient_quota）
//
// 混成一句"没有可用账号"会让调用方改错方向（甚至去加号，加了也没用）。
//
// 🔴 有"余额未知"的账号时**不下结论**：那种情况下我们根本不知道它够不够，
// 说"余额不足"可能是错的。
func (s *Server) creditsShortfall(need int) (error, string, bool) {
	if need <= 0 {
		return nil, "", false
	}
	views := s.pool.Snapshot()

	// ① 本地预算口径（只有显式配置了才管）
	if s.cfg.Pool.MaxVideoCreditsPerDay > 0 && !s.pool.CanSpendVideoCredits(need) {
		return errVideoQuotaLocal, fmt.Sprintf("全部 %d 个账号今日视频积分预算都不够这一次"+
			"（预算 %d 分/号/天，本次需要 %d 分）",
			len(views), s.cfg.Pool.MaxVideoCreditsPerDay, need), true
	}

	// ② 真实余额口径：所有活着的号都读到了余额，且都小于本次所需
	var parts []string
	affordable, unknown, alive := 0, 0, 0
	for _, v := range views {
		if v.State == "auth_dead" {
			continue
		}
		alive++
		if !v.CreditKnown {
			unknown++
			continue
		}
		if v.CreditBalance >= need {
			affordable++
		}
		parts = append(parts, fmt.Sprintf("%s=%d", v.Name, v.CreditBalance))
	}
	if alive == 0 || unknown > 0 || affordable > 0 || len(parts) == 0 {
		return nil, "", false
	}
	return errCreditsShort, fmt.Sprintf("本次需要 %d 积分，各账号余额 %s"+
		"（720P 5s=30、1080P 15s=120；普号每日赠送 40）", need, strings.Join(parts, "、")), true
}
