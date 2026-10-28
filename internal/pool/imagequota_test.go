package pool

import (
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func newQuotaPool(t *testing.T, limit int, names ...string) *Pool {
	t.Helper()
	cfg := config.Pool{MaxInflightPerAccount: 2, MaxImagePerDay: limit}
	var accs []config.Account
	for _, n := range names {
		accs = append(accs, config.Account{Name: n, Token: "tok"})
	}
	return New(cfg, accs, "")
}

func TestImageQuotaBlocksAfterLimit(t *testing.T) {
	p := newQuotaPool(t, 3, "a")
	a := p.Get("a")
	if a == nil {
		t.Fatal("账号不存在")
	}

	for i := 0; i < 3; i++ {
		if _, ok := p.PickForImage(); !ok {
			t.Fatalf("第 %d 张不该被拦", i+1)
		}
		p.Release(a)
		p.MarkImageDone(a)
	}

	if used, limit := p.ImageUsage(a); used != 3 || limit != 3 {
		t.Fatalf("用量统计不对: %d/%d", used, limit)
	}
	// 到达上限后，图片选号必须拒绝（普通 Pick 仍可用 —— 文本不受限）。
	if _, ok := p.PickForImage(); ok {
		t.Fatal("达到每日上限后仍能选出图片账号")
	}
	if _, ok := p.Pick(); !ok {
		t.Fatal("文本选号不该被图片配额影响")
	}
}

func TestImageQuotaSkipsExhaustedAccount(t *testing.T) {
	// 两个号，a 用尽 ⇒ 图片请求要落到 b。
	p := newQuotaPool(t, 2, "a", "b")
	a := p.Get("a")
	p.MarkImageDone(a)
	p.MarkImageDone(a)

	for i := 0; i < 5; i++ {
		got, ok := p.PickForImage()
		if !ok {
			t.Fatal("b 还有额度，不该选不出来")
		}
		if got.Name != "b" {
			t.Fatalf("应跳过已达上限的 a，实际选了 %s", got.Name)
		}
		p.Release(got)
	}
}

func TestImageQuotaResetsNextDay(t *testing.T) {
	p := newQuotaPool(t, 2, "a")
	a := p.Get("a")
	p.MarkImageDone(a)
	p.MarkImageDone(a)

	if _, ok := p.PickForImage(); ok {
		t.Fatal("当日已用尽")
	}

	// 模拟跨天：把计数的日期改成昨天（同包可直接改内部字段）。
	p.mu.Lock()
	a.imageDay = "2000-01-01"
	p.mu.Unlock()

	if _, ok := p.PickForImage(); !ok {
		t.Fatal("跨天后应重置为可用")
	}
	if used, _ := p.ImageUsage(a); used != 0 {
		t.Fatalf("跨天后计数应清零，得到 %d", used)
	}
}

func TestImageQuotaLeft(t *testing.T) {
	p := newQuotaPool(t, 2, "a", "b")
	if left := p.ImageQuotaLeft(); left != 4 {
		t.Fatalf("初始剩余应为 4，得到 %d", left)
	}
	p.MarkImageDone(p.Get("a"))
	p.MarkImageDone(p.Get("a"))
	if left := p.ImageQuotaLeft(); left != 2 {
		t.Fatalf("a 用尽后应剩 2，得到 %d", left)
	}

	// limit=0 ⇒ 不限，返回 -1
	p2 := newQuotaPool(t, 0, "a")
	if left := p2.ImageQuotaLeft(); left != -1 {
		t.Fatalf("不限额应返回 -1，得到 %d", left)
	}
	if _, ok := p2.PickForImage(); !ok {
		t.Fatal("不限额时不该拦")
	}
}

func TestImageQuotaPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	stateFile := dir + "/state.json"
	cfg := config.Pool{MaxInflightPerAccount: 2, MaxImagePerDay: 2}
	accs := []config.Account{{Name: "a", Token: "tok"}}

	p1 := New(cfg, accs, stateFile)
	a := p1.Get("a")
	p1.MarkImageDone(a)
	p1.MarkImageDone(a)
	if _, ok := p1.PickForImage(); ok {
		t.Fatal("p1 当日已用尽")
	}

	// 重启：配额应保留（否则重启就等于重置额度，上游会被打穿）。
	p2 := New(cfg, accs, stateFile)
	if _, ok := p2.PickForImage(); ok {
		t.Fatal("重启后配额丢失了")
	}
	if used, _ := p2.ImageUsage(p2.Get("a")); used != 2 {
		t.Fatalf("重启后用量应为 2，得到 %d", used)
	}
}

func TestSnapshotShowsImageUsage(t *testing.T) {
	p := newQuotaPool(t, 4, "a")
	p.MarkImageDone(p.Get("a"))
	snap := p.Snapshot()
	if len(snap) != 1 {
		t.Fatal("快照数量不对")
	}
	if snap[0].ImageToday != 1 || snap[0].ImageLimit != 4 {
		t.Fatalf("快照里的配额字段不对: %+v", snap[0])
	}
}
