package pool

import (
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func newCreditPool(t *testing.T, creditsPerDay, perDay int) *Pool {
	t.Helper()
	cfg := config.Pool{
		MaxInflightPerAccount: 2,
		MaxImagePerDay:        6,
		MaxVideoPerDay:        perDay,
		MaxVideoCreditsPerDay: creditsPerDay,
	}
	return New(cfg, []config.Account{{Name: "a", Token: "t"}}, "")
}

func TestVideoCreditsGateByPrice(t *testing.T) {
	// 普号每天 40 分：720P 5s（30）能过，1080P 15s（120）必须被拦。
	p := newCreditPool(t, 40, 0) // perDay=0 → 只看积分
	if _, ok := p.PickForVideo(30); !ok {
		t.Fatal("40 分预算下 720P 5s(30) 应放行")
	}
	p.Release(p.Get("a"))
	if _, ok := p.PickForVideo(120); ok {
		t.Fatal("40 分预算下 1080P 15s(120) 应被拦（否则会透支 3 倍）")
	}
}

func TestVideoCreditsAccumulate(t *testing.T) {
	p := newCreditPool(t, 40, 0)
	a := p.Get("a")
	p.SpendVideoCredits(a, 30) // 用掉一个 720P 5s

	if left := p.CreditsLeft(); left != 10 {
		t.Fatalf("用掉 30 后应剩 10，得到 %d", left)
	}
	if _, ok := p.PickForVideo(30); ok {
		t.Fatal("只剩 10 分时不该再放行 30 分")
	}
	// 边界：正好用完可以
	if _, ok := p.PickForVideo(10); !ok {
		t.Fatal("剩 10 分时应允许 10 分档")
	}
}

func TestVideoCreditsDisabledFallsBackToCount(t *testing.T) {
	// 预算 0 ⇒ 不启用积分模式，只看个数上限（默认 1）。
	p := newCreditPool(t, 0, 1)
	if _, ok := p.PickForVideo(120); !ok {
		t.Fatal("未启用积分模式时不该按积分拦")
	}
	a := p.Get("a")
	p.Release(a)
	p.MarkVideoDone(a)
	if _, ok := p.PickForVideo(0); ok {
		t.Fatal("个数上限 1 已用满，应拦")
	}
}

func TestVideoCreditsPersistAndReset(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Pool{MaxInflightPerAccount: 2, MaxVideoCreditsPerDay: 40}
	accs := []config.Account{{Name: "a", Token: "t"}}

	p1 := New(cfg, accs, dir+"/s.json")
	p1.SpendVideoCredits(p1.Get("a"), 30)

	// 重启：积分消耗应保留（否则重启就能白拿额度）
	p2 := New(cfg, accs, dir+"/s.json")
	if used, limit := p2.CreditsUsage(p2.Get("a")); used != 30 || limit != 40 {
		t.Fatalf("重启后应保留 30/40，得到 %d/%d", used, limit)
	}

	// 跨天：清零。注意别在持写锁时调 Get()（它内部 RLock，会自己死锁）。
	p2.mu.Lock()
	p2.accounts[0].creditDay = "2000-01-01"
	p2.mu.Unlock()
	if used, _ := p2.CreditsUsage(p2.Get("a")); used != 0 {
		t.Fatalf("跨天后应清零，得到 %d", used)
	}
}
