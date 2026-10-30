package server

import "testing"

// 每轮预热批量随目标数缩放：目标是"要随时可用的号数"，批量太小会让热满耗时
// 随规模线性增长（256 个 × 20s/8 = 约 10 分钟），太小则低流量下热不起来。
func TestWarmBatchScalesWithTarget(t *testing.T) {
	cases := []struct{ target, want int }{
		{0, 4}, {8, 4}, {16, 4}, {32, 4}, {48, 6},
		{64, 8}, {128, 16}, {256, 32}, {512, 48}, {4096, 48},
	}
	for _, c := range cases {
		if got := warmBatch(c.target); got != c.want {
			t.Fatalf("warmBatch(%d) = %d，期望 %d", c.target, got, c.want)
		}
	}
	// 热满所需轮数应稳定（与规模无关）：256 个目标 ≈ 8 轮，512 个 ≈ 11 轮。
	if rounds := 256 / warmBatch(256); rounds > 10 {
		t.Fatalf("target=256 需要 %d 轮才热满，太慢", rounds)
	}
}
