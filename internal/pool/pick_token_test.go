package pool

// 选号优先"token 还活着"的账号（2026-10-30 用户要求）。
//
// 背景：池里 3135 个号，access TTL 只有 15 分钟，保活只热最前面的一小批
// ⇒ 绝大多数号的 token 长期是过期的。按 served 轮询等于"每个请求都挑一个过期号"，
// 要么多一次 refresh、要么吃一次 401 再续期，白白多几百毫秒。

import (
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// freshToken 造一个"还活着"的 token（10 分钟后过期）。
func freshToken(t *testing.T) string {
	t.Helper()
	return "bearer:" + fakeJWT(t, time.Now().Add(10*time.Minute).Unix())
}

func TestPickPrefersAliveToken(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, nil, "")

	p.Add(config.Account{Name: "stale", Token: "bearer:not-a-jwt"})
	p.Add(config.Account{Name: "fresh", Token: freshToken(t)})
	// 故意让 fresh 的 served 更大：按老规则（只比 served）会选 stale，
	// 新规则必须先把 token 活着的排前面。
	p.mu.Lock()
	for _, a := range p.accounts {
		if a.Name == "fresh" {
			a.served = 9
		}
	}
	p.mu.Unlock()

	got, ok := p.Pick()
	if !ok {
		t.Fatal("应能选出账号")
	}
	defer p.Release(got)
	if got.Name != "fresh" {
		t.Fatalf("应优先选 token 活着的号，实际选了 %s", got.Name)
	}
}

// 全池都过期时也不能卡死：排序只是"优先"，不是"过滤"。
func TestPickFallsBackWhenAllStale(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, []config.Account{
		{Name: "a", Token: "bearer:a"},
		{Name: "b", Token: "bearer:b"},
	}, "")
	got, ok := p.Pick()
	if !ok || got == nil {
		t.Fatal("全部过期时仍应选出账号（否则请求会 503）")
	}
	p.Release(got)
}

// 活着的号里，越快过期的越先续 —— 它们正在服务，掉线就把续期甩到请求路径上。
func TestWarmCandidatesRefreshesHotBeforeItDies(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, []config.Account{
		{Name: "healthy", Token: "bearer:a", RefreshToken: "rt-a"},
		{Name: "dying", Token: "bearer:b", RefreshToken: "rt-b"},
	}, "")
	p.mu.Lock()
	// healthy：还活着且离过期还远（10 分钟）⇒ 不该被续
	p.accounts[0].tokenExp = time.Now().Add(10 * time.Minute)
	// dying：活着（>2 分钟余量）但 2.5 分钟后到期 ⇒ 落在 3 分钟窗口内，该续
	p.accounts[1].tokenExp = time.Now().Add(150 * time.Second)
	p.mu.Unlock()

	names := p.WarmCandidates(2, 3*time.Minute, 8)
	if len(names) != 1 || names[0] != "dying" {
		t.Fatalf("只该续快过期的那个，实际 %v", names)
	}
}

// 活着的够用时不必动过期号；不够时才按 served 补 —— 那正是接下来要用的。
func TestWarmCandidatesFillsWithNextToUse(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, []config.Account{
		{Name: "alive", Token: "bearer:a", RefreshToken: "rt-a"},
		{Name: "next", Token: "bearer:b", RefreshToken: "rt-b"},
	}, "")
	p.mu.Lock()
	p.accounts[0].tokenExp = time.Now().Add(10 * time.Minute)
	p.accounts[1].tokenExp = time.Now().Add(-time.Minute) // 已过期
	p.mu.Unlock()

	// target=1：只覆盖"活着的"那个，它离过期还远 ⇒ 这轮不该续任何人
	if names := p.WarmCandidates(1, 3*time.Minute, 8); len(names) != 0 {
		t.Fatalf("target=1 只覆盖活号时不该续期，实际 %v", names)
	}
	// target=2：把接下来要用的过期号补进来续掉
	if names := p.WarmCandidates(2, 3*time.Minute, 8); len(names) != 1 || names[0] != "next" {
		t.Fatalf("target=2 应续 next，实际 %v", names)
	}
}
