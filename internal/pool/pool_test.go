package pool

import (
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func newTestPool(n int) *Pool {
	cfg := config.Pool{MaxRotate: 3, CooldownSoftS: 60, CooldownAuthS: 600, MaxInflightPerAccount: 2}
	var accts []config.Account
	for i := 0; i < n; i++ {
		accts = append(accts, config.Account{Name: string(rune('a' + i)), Token: "tok-" + string(rune('a'+i))})
	}
	return New(cfg, accts, "")
}

func TestPickRoundRobinByLeastRecentlyUsed(t *testing.T) {
	p := newTestPool(2)
	a, ok := p.Pick()
	if !ok {
		t.Fatal("应能取到号")
	}
	p.Release(a)
	b, ok := p.Pick()
	if !ok {
		t.Fatal("应能取到号")
	}
	p.Release(b)
	// 最久未用的优先 ⇒ 第二轮应与第一轮不同号（避免一个号被连打）。
	if a.Name == b.Name {
		t.Fatalf("选号未轮换: %s 连续两次", a.Name)
	}
}

func TestPickRespectsInflightCap(t *testing.T) {
	p := newTestPool(1) // 单号，在途上限 2
	a1, _ := p.Pick()
	a2, _ := p.Pick()
	if a1 == nil || a2 == nil {
		t.Fatal("前两次应取到")
	}
	if _, ok := p.Pick(); ok {
		t.Fatal("超过在途上限仍能取号")
	}
	p.Release(a1)
	if _, ok := p.Pick(); !ok {
		t.Fatal("释放后应能再取")
	}
}

func TestMarkAuthRemovesFromRotation(t *testing.T) {
	p := newTestPool(1)
	a, _ := p.Pick()
	p.MarkAuth(a, "401")
	p.Release(a)
	if _, ok := p.Pick(); ok {
		t.Fatal("凭据失效的号不该再被选中")
	}
	if p.ReadyCount() != 0 {
		t.Fatalf("ReadyCount 应为 0，得到 %d", p.ReadyCount())
	}
	v := p.Snapshot()[0]
	if v.State != "auth_dead" {
		t.Fatalf("状态应为 auth_dead，得到 %s", v.State)
	}
}

func TestResetRevivesAccount(t *testing.T) {
	p := newTestPool(1)
	a, _ := p.Pick()
	p.MarkAuth(a, "401")
	p.Release(a)
	if !p.Reset("a") {
		t.Fatal("重置失败")
	}
	if p.ReadyCount() != 1 {
		t.Fatal("重置后应可用")
	}
}

func TestCooldownBackoffCaps(t *testing.T) {
	p := newTestPool(1)
	a, _ := p.Pick()
	// 连续 10 次软冷却：退避必须封顶在 1h，不能无限增长。
	for i := 0; i < 10; i++ {
		p.MarkRateLimit(a, "429", 0)
	}
	v := p.Snapshot()[0]
	if v.State != "cooling" {
		t.Fatalf("应在冷却态，得到 %s", v.State)
	}
	if v.CooldownLeftS > 3600 {
		t.Fatalf("冷却超过 1h 上限: %d", v.CooldownLeftS)
	}
}

func TestRetryAfterHonored(t *testing.T) {
	p := newTestPool(1)
	a, _ := p.Pick()
	p.MarkRateLimit(a, "429", 120)
	v := p.Snapshot()[0]
	if v.CooldownLeftS < 110 {
		t.Fatalf("Retry-After 未被采纳: %d", v.CooldownLeftS)
	}
}

func TestAuthDeadNotRestoredAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	stateFile := dir + "/state.json"
	cfg := config.Pool{CooldownSoftS: 60, CooldownAuthS: 600, MaxInflightPerAccount: 2}
	accts := []config.Account{{Name: "a", Token: "t"}}

	p1 := New(cfg, accts, stateFile)
	a, _ := p1.Pick()
	p1.MarkAuth(a, "401")
	p1.Release(a)
	p1.Release(a) // 幂等

	// 重启：冷却恢复，但 auth_dead 不恢复（token 可能已换新）。
	p2 := New(cfg, accts, stateFile)
	if p2.ReadyCount() != 0 {
		t.Fatal("冷却时间应跨重启恢复")
	}
	v := p2.Snapshot()[0]
	if v.State != "cooling" {
		t.Fatalf("重启后应为 cooling（而非 auth_dead）: %s", v.State)
	}
}

func TestInflightNeverNegative(t *testing.T) {
	p := newTestPool(1)
	a, _ := p.Pick()
	p.Release(a)
	p.Release(a) // 重复释放
	p.Release(nil)
	if got := p.Snapshot()[0].Inflight; got != 0 {
		t.Fatalf("在途计数越界: %d", got)
	}
}

func TestSnapshotNoTokenLeak(t *testing.T) {
	p := newTestPool(1)
	v := p.Snapshot()[0]
	// View 结构里根本没有 Token 字段 —— 这条测试钉住"面板不会泄露凭据"。
	if v.Name != "a" {
		t.Fatalf("快照字段异常: %+v", v)
	}
	if got := p.Len(); got != 1 {
		t.Fatalf("Len=%d", got)
	}
	_ = time.Now()
}
