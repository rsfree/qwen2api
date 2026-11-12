package pool

// 选号偏好"手里有预建会话"的号。
//
// 背景（2026-11-12 线上实测）：预热只覆盖 chatTarget 个号（按并发算，默认 6），
// 而选号面是 token 活着的那批（116 个）。两者错开 20 倍 ⇒ 38% 的请求落到
// 没有预建会话的号上，每次现建 2.9~3.1 秒，直接吃掉首字。
// 这一档把"命中"从"两张表碰巧对齐"变成"选号时就知道"。

import (
	"sync/atomic"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func newChatReadyPool(t *testing.T) *Pool {
	t.Helper()
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, nil, "")
	for _, n := range []string{"cold_a", "warm", "cold_b"} {
		p.Add(config.Account{Name: n, Token: freshToken(t)})
	}
	return p
}

func TestPickPrefersChatReady(t *testing.T) {
	p := newChatReadyPool(t)
	p.MarkChatReady("warm", true)

	got, ok := p.Pick()
	if !ok {
		t.Fatal("应能选出账号")
	}
	defer p.Release(got)
	if got.Name != "warm" {
		t.Fatalf("应优先选有预建会话的号，实际选了 %s", got.Name)
	}
}

// 有货的号即使 served 已经很高，也要排在没货的号前面 ——
// served 只决定"同样有货/同样没货时谁上"，不能盖过 3 秒的差距。
func TestChatReadyBeatsServed(t *testing.T) {
	p := newChatReadyPool(t)
	p.mu.Lock()
	for _, a := range p.accounts {
		if a.Name == "warm" {
			a.served = 99
		}
	}
	p.mu.Unlock()
	p.MarkChatReady("warm", true)

	got, _ := p.Pick()
	defer p.Release(got)
	if got.Name != "warm" {
		t.Fatalf("有货的号应压过 served 更小的冷号，实际选了 %s", got.Name)
	}
}

// 会话被取走（MarkChatReady false）后，选号必须立刻回到其他号，
// 否则下一个请求会撞上"标志说有货、实际没货"的号，白付一次现建。
func TestChatReadyClearedFallsBack(t *testing.T) {
	p := newChatReadyPool(t)
	p.MarkChatReady("warm", true)
	if got, _ := p.Pick(); got != nil {
		p.Release(got)
		if got.Name != "warm" {
			t.Fatalf("第一轮应选 warm，实际 %s", got.Name)
		}
	}
	p.MarkChatReady("warm", false)

	got, _ := p.Pick()
	defer p.Release(got)
	if got.Name == "warm" {
		t.Fatal("已清空标志的号不应再被优先选中")
	}
}

// SetChatReady 是全量刷新：不在集合里的号必须被置假（会话 TTL 过期、
// 池被关掉都靠这条兜底）。
func TestSetChatReadyIsTotalRefresh(t *testing.T) {
	p := newChatReadyPool(t)
	p.SetChatReady(map[string]struct{}{"cold_a": {}, "warm": {}})

	p.mu.RLock()
	state := map[string]bool{}
	for _, a := range p.accounts {
		state[a.Name] = a.chatReady
	}
	p.mu.RUnlock()
	if !state["cold_a"] || !state["warm"] {
		t.Fatalf("集合内的号应置真，得到 %v", state)
	}
	if state["cold_b"] {
		t.Fatal("集合外的号应为假")
	}

	// 会话池被清空（nil）⇒ 所有标志归零，不能残留旧值。
	p.SetChatReady(nil)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		if a.chatReady {
			t.Fatalf("%s 的标志应被清空", a.Name)
		}
	}
}

// 都过期时仍然先比 refresh_token（几百毫秒）再比 chatReady：
// signin 是最贵、最容易被 WAF 拦的一步，不该被 3 秒的建会话收益盖过去。
func TestRefreshStillBeatsChatReady(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, nil, "")
	p.Add(config.Account{Name: "has_refresh", Token: "bearer:not-a-jwt",
		RefreshToken: "rt", Email: "a@b.c", Password: "x"})
	p.Add(config.Account{Name: "only_chat", Token: "bearer:not-a-jwt", Password: "x"})
	p.MarkChatReady("only_chat", true)

	got, _ := p.Pick()
	defer p.Release(got)
	if got.Name != "has_refresh" {
		t.Fatalf("能 refresh 续期的号应优先，实际 %s", got.Name)
	}
}

// Agent 选号必须避开"正在跑另一个 agent"的号：上游并发槽 = 1，
// 本地允许 2 等于主动撞 user.too_many_concurrent 再白等 1.2 秒。
func TestPickIdleSkipsBusy(t *testing.T) {
	p := newChatReadyPool(t) // cold_a / warm / cold_b，都是 fresh token

	p.mu.Lock()
	for _, a := range p.accounts {
		if a.Name != "cold_b" {
			atomic.AddInt32(&a.inflight, 1)
		}
	}
	p.mu.Unlock()

	got, ok := p.PickIdle()
	if !ok {
		t.Fatal("应能挑到唯一空闲的号")
	}
	defer p.Release(got)
	if got.Name != "cold_b" {
		t.Fatalf("应跳过忙号选 cold_b，实际 %s", got.Name)
	}

	// 全忙 ⇒ 返回 false（调用方退回 Pick，宁可等也不能没号）。
	p.mu.Lock()
	for _, a := range p.accounts {
		atomic.AddInt32(&a.inflight, 1)
	}
	p.mu.Unlock()
	if _, ok := p.PickIdle(); ok {
		t.Fatal("全部忙时应返回 false")
	}
}
