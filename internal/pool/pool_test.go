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

// 全池凭据同时过期时，HotNames 仍要给出候选 —— 否则会话预热和连接保活
// 会在最需要的时候集体停摆，而 Pick 依旧会选这些号。
func TestHotNamesFallsBackWhenAllExpired(t *testing.T) {
	cfg := config.Pool{}
	p := New(cfg, []config.Account{
		{Name: "a0", Token: "t0"},
		{Name: "a1", Token: "t1"},
	}, "")

	// 全部压成"已过期"（tokenAlive 要求剩余 > 2min）。
	past := time.Now().Add(-time.Minute)
	for _, a := range p.accounts {
		a.tokenExp = past
	}
	names := p.HotNames(2)
	if len(names) != 2 {
		t.Fatalf("全冷时也必须返回候选（否则预热停摆），得到 %d 个", len(names))
	}
}

// 活着的号必须排在过期号前面：它们才是 Pick 真正会选的。
func TestHotNamesPrefersAlive(t *testing.T) {
	cfg := config.Pool{}
	p := New(cfg, []config.Account{
		{Name: "dead", Token: "t0"},
		{Name: "live", Token: "t1"},
	}, "")
	now := time.Now()
	for _, a := range p.accounts {
		if a.Name == "live" {
			a.tokenExp = now.Add(10 * time.Minute)
		} else {
			a.tokenExp = now.Add(-time.Minute)
		}
	}
	names := p.HotNames(1)
	if len(names) != 1 || names[0] != "live" {
		t.Fatalf("应优先返回活着的号，得到 %v", names)
	}
}
