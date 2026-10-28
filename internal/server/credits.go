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
)

const (
	// creditTTL 余额缓存有效期。视频请求才会触发刷新，所以这个值决定
	// "一个号花完钱后多久被发现" —— 60s 足够，且把第三方域名的调用摊薄。
	creditTTL = 60 * time.Second
	// creditTTLStats /stats（面板）用的更长 TTL：人看数据不需要 60s 级实时，
	// 但它会定时轮询，别让轮询把三方域名打热。
	creditTTLStats = 2 * time.Minute
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

	// 整体加个上限：这是辅助动作，不该拖慢主请求。
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			c, err := s.up.Credits(ctx, s.pool.Credential(name))
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
