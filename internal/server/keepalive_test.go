package server

import (
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 每轮批量要同时满足两件事：热满够快、且连接保活能在 TTL 内轮一遍。
func TestWarmBatchMeetsConnTTL(t *testing.T) {
	// 一个 TTL 内必须把 target 个号走完，否则先保活的已被回收，白跑。
	//
	// 🔴 用常量而不是写死 90：TTL 本身会跟着"号轮转周期"调整
	// （2026-11-12 实测 90s 顶不住 96s 的轮转，已改 300s）。
	// 写死的话，常量一改这条测试就从"守约束"退化成"守旧数字"。
	ttl := int(upstream.IdleConnTimeout / time.Second)
	for _, target := range []int{4, 8, 16, 32, 64, 128} {
		b := warmBatch(target, 512, 20)
		rounds := (target + b - 1) / b
		if sec := rounds * 20; sec > ttl {
			t.Fatalf("target=%d batch=%d 覆盖要 %ds > %ds，保活会失效", target, b, sec, ttl)
		}
	}
	// 上限必须生效（防止一轮把上游打太热）。
	if b := warmBatch(4096, 48, 20); b != 48 {
		t.Fatalf("应被 batch_max 夹住，得到 %d", b)
	}
	// 下限 4：低流量也要热得起来。
	if b := warmBatch(1, 48, 20); b != 4 {
		t.Fatalf("下限应为 4，得到 %d", b)
	}
	// tick 大到"一个 TTL 里只剩几轮"时，批量必须被覆盖约束推大，
	// 否则先保活的那批会先过期（TTL 拉长后，小 tick 不再需要额外补偿 ——
	// 那时批量的主项是 target/8，即"8 轮热满"）。
	if small, big := warmBatch(64, 512, 10), warmBatch(64, 512, 100); big <= small {
		t.Fatalf("tick 大到 TTL 只够 3 轮时应推大批量：10s→%d，100s→%d", small, big)
	}
}

// 会话/连接的覆盖范围按**并发**算，不是按 token 的 target。
//
// 这条是"动态优化"的核心：并发 5 时保活 512 条连接，等于每 20 秒
// 白打 500 次代理往返（每次 0.4~2 秒）。
func TestChatTargetScalesWithPeakNotTokenTarget(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)

	// 默认 ConcFactor=2 ⇒ 5×2+4 = 14，远小于 token target 512。
	if got := s.chatTarget(5, 512); got != 14 {
		t.Fatalf("peak=5 应只需 14 个号，得到 %d", got)
	}
	// 不低于下限 4（否则低流量下根本不预热）。
	if got := s.chatTarget(0, 512); got != 4 {
		t.Fatalf("peak=0 应回落到下限 4，得到 %d", got)
	}
	// 不超过 token target（不会覆盖到"根本不会被选中"的号）。
	if got := s.chatTarget(100, 8); got != 8 {
		t.Fatalf("应被 token target 封顶到 8，得到 %d", got)
	}
	// 并发涨上去时线性放大。
	if got := s.chatTarget(50, 512); got != 104 {
		t.Fatalf("peak=50 应为 104，得到 %d", got)
	}
}

// 并发峰值：取最大值、读后清零，且退出不改变峰值。
func TestInflightPeakTracksMaxAndResets(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)

	s.noteInflight(1)
	s.noteInflight(1)
	s.noteInflight(1)
	s.noteInflight(-1)
	s.noteInflight(-1)
	s.noteInflight(-1)

	if got := s.inflightNow.Load(); got != 0 {
		t.Fatalf("进出应抵消，当前在途应为 0，得到 %d", got)
	}
	if got := s.inflightPeak.Swap(0); got != 3 {
		t.Fatalf("峰值应为 3，得到 %d", got)
	}
	if got := s.inflightPeak.Swap(0); got != 0 {
		t.Fatalf("读后应清零，得到 %d", got)
	}
}

// 预热必须真的轮到每一个号，否则尾部账号的连接永远是冷的。
//
// 这条守的是 2026-11-12 修的那个 bug：HotNames 的排序键（alive/served/tokenExp）
// 不会被预热自己改写，所以"每轮取前 limit 个"等于永远只热同一批。
func TestRotateHotCoversAllAccounts(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)

	names := []string{"a", "b", "c", "d", "e"}
	const limit = 2
	seen := map[string]int{}
	// 5 个号、每轮 2 个 ⇒ 3 轮（6 个名额）必然覆盖全部。
	for round := 0; round < 3; round++ {
		batch := s.rotateHot(names, limit)
		if len(batch) != limit {
			t.Fatalf("第 %d 轮应取 %d 个，得到 %d", round, limit, len(batch))
		}
		for _, n := range batch {
			seen[n]++
		}
	}
	if len(seen) != len(names) {
		t.Fatalf("3 轮应覆盖全部 %d 个号，实际只覆盖了 %v", len(names), seen)
	}

	// limit 不小于总数时原样返回（整池都热，不需要轮转）。
	if got := s.rotateHot(names, 5); len(got) != 5 {
		t.Fatalf("limit=len 应原样返回，得到 %d 个", len(got))
	}
}
