package panel

import (
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/pool"
)

func TestAcctStatusOf(t *testing.T) {
	cases := []struct {
		name string
		v    pool.View
		want string
	}{
		{"失效", pool.View{State: "auth_dead"}, "dead"},
		{"冷却", pool.View{State: "cooling"}, "cooling"},
		{"正常", pool.View{State: "ready"}, "ready"},
		// 出图额度用尽且没有余额 ⇒ 这个号暂时什么都做不了，单列一档
		{"额度用尽", pool.View{State: "ready", ImageLimit: 6, ImageToday: 6, CreditKnown: true, CreditBalance: 0}, "exhausted"},
		// 额度用尽但还有积分 ⇒ 还能做视频，不算用尽
		{"额度用尽但有积分", pool.View{State: "ready", ImageLimit: 6, ImageToday: 6, CreditKnown: true, CreditBalance: 40}, "ready"},
		// 余额未知不判用尽（宁可放行，让上游兜底）
		{"余额未知", pool.View{State: "ready", ImageLimit: 6, ImageToday: 6}, "ready"},
	}
	for _, c := range cases {
		if got := acctStatusOf(c.v, 30); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	views := []pool.View{
		{State: "ready", ImageLimit: 6, ImageToday: 2, CreditKnown: true, CreditBalance: 90, HasRefresh: true, TokenLeftS: 600},
		{State: "ready", ImageLimit: 6, ImageToday: 6, CreditKnown: true, CreditBalance: 0},
		{State: "cooling", ImageLimit: 6, ImageToday: 0},
		{State: "auth_dead"},
	}
	st := summarize(views, 30)

	if st.Total != 4 || st.Ready != 1 || st.Cooling != 1 || st.Dead != 1 || st.Exhausted != 1 {
		t.Fatalf("状态计数错: %+v", st)
	}
	// 剩余张数：第一个号剩 4，第二个 0，第三个 6（冷却也计入余量，它只是暂时不可用）
	if st.ImgLeft != 10 {
		t.Fatalf("剩余张数应 10，实际 %d", st.ImgLeft)
	}
	if st.Credits != 90 || st.CreditsKnown != 2 {
		t.Fatalf("余额汇总错: %+v", st)
	}
	// 90 分 ÷ 30 = 3 条 720P 5s
	if st.VidLeft != 3 {
		t.Fatalf("可做视频数应 3，实际 %d", st.VidLeft)
	}
	if st.RefreshKnown != 1 || st.TokenFresh != 1 {
		t.Fatalf("保活口径错: %+v", st)
	}
}

func TestAtoiOr(t *testing.T) {
	if atoiOr("", 7) != 7 || atoiOr("abc", 7) != 7 || atoiOr("12", 7) != 12 {
		t.Fatal("atoiOr 行为不对")
	}
}
