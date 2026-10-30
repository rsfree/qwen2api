package pool

// 大池子的选号调度：优先用"凭据还新鲜"的号。

import (
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestPickPrefersFreshToken(t *testing.T) {
	cfg := config.Default().Pool
	p := New(cfg, []config.Account{
		{Name: "cold", Token: "bearer:a"},
		{Name: "hot", Token: "bearer:b"},
	}, "")
	p.mu.Lock()
	p.accounts[0].tokenAt = time.Now().Add(-2 * time.Hour) // 冷：早过期了
	p.accounts[1].tokenAt = time.Now()                     // 热
	p.mu.Unlock()

	// 连续取 3 次都该是热号 —— 冷号只在没有热号时才被动用。
	for i := 0; i < 3; i++ {
		a, ok := p.Pick()
		if !ok {
			t.Fatal("应该能取到账号")
		}
		if a.Name != "hot" {
			t.Fatalf("第 %d 次取到 %s，应优先热号", i+1, a.Name)
		}
		p.Release(a)
	}
}

func TestPickFallsBackWhenAllTokensCold(t *testing.T) {
	// 全是冷号时不能"选不出"—— 否则池子会假装没账号。
	// 这时选一个冷号，路由层会因 401 自动重登（这正是要付出的代价）。
	cfg := config.Default().Pool
	p := New(cfg, []config.Account{
		{Name: "a", Token: "bearer:a"},
		{Name: "b", Token: "bearer:b"},
	}, "")
	p.mu.Lock()
	for _, a := range p.accounts {
		a.tokenAt = time.Now().Add(-2 * time.Hour)
	}
	p.mu.Unlock()

	if _, ok := p.Pick(); !ok {
		t.Fatal("全是冷号时仍应能选出一个")
	}
}

func TestUnknownAgeIsNotFreshButUsable(t *testing.T) {
	// 重启后从文件加载的凭据"年龄未知"。不能当成热号（否则 LRU 会把
	// 3000 个冷号轮流碰一遍），但也不能选不出来。
	cfg := config.Default().Pool
	p := New(cfg, []config.Account{
		{Name: "unknown", Token: "bearer:a"},
		{Name: "hot", Token: "bearer:b"},
	}, "")
	p.mu.Lock()
	p.accounts[1].tokenAt = time.Now() // 只有它是热号
	p.mu.Unlock()

	a, ok := p.Pick()
	if !ok || a.Name != "hot" {
		t.Fatalf("应优先热号，实际 %v %v", a, ok)
	}
	p.Release(a)

	// 热号全部不可用时，未知年龄的号仍能被选中（随后靠 401 重登）。
	p.mu.Lock()
	p.accounts[1].cooldownUntil = time.Now().Add(time.Hour)
	p.mu.Unlock()
	a, ok = p.Pick()
	if !ok || a.Name != "unknown" {
		t.Fatalf("热号都不可用时应回落到未知号，实际 %v %v", a, ok)
	}
}

func TestMarkOKMakesItHot(t *testing.T) {
	// 冷号被用成功一次后应转热，之后优先于其它冷号 —— 这是热池随流量
	// 自然增长的关键：否则每次都会挑下一个冷号，永远在重登。
	cfg := config.Default().Pool
	p := New(cfg, []config.Account{
		{Name: "a", Token: "bearer:a"},
		{Name: "b", Token: "bearer:b"},
	}, "")
	p.mu.Lock()
	for _, x := range p.accounts {
		x.tokenAt = time.Now().Add(-2 * time.Hour)
	}
	p.mu.Unlock()

	first, ok := p.Pick()
	if !ok {
		t.Fatal("应能选出账号")
	}
	p.MarkOK(first)
	p.Release(first)

	for i := 0; i < 2; i++ {
		next, ok := p.Pick()
		if !ok || next.Name != first.Name {
			t.Fatalf("成功的号应转热并被优先复用，实际 %v", next)
		}
		p.Release(next)
	}
}
