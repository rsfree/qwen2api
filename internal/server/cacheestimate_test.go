package server

// 上游不报 cached_tokens 时的保守估算（2026-11-12 用户要求）。
//
// 原则：只在"确定的链式复用 + 发的是增量"时估，且宁可少报 ——
// 调用方会拿这个数算省了多少钱，虚高比没有更糟。

import (
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

func TestEstimateCachedIsConservative(t *testing.T) {
	// 上界是 min(prev, cur)，再打 3/4 折。
	if got := estimateCached(10000, 8000); got != 6000 {
		t.Fatalf("prev=10000 cur=8000 应估 6000，得到 %d", got)
	}
	if got := estimateCached(5000, 9000); got != 3750 {
		t.Fatalf("cur 更大时应按 prev 折算，得到 %d", got)
	}
	// 任何情况下都不能超过本次输入 token。
	for _, c := range [][2]int{{100, 100}, {1, 1000}, {999999, 7}} {
		if got := estimateCached(c[0], c[1]); got > c[1] {
			t.Fatalf("估算 %d 超过了输入 token %d", got, c[1])
		}
	}
	// 没有上一轮读数就不估。
	if got := estimateCached(0, 8000); got != 0 {
		t.Fatalf("prev=0 不应估算，得到 %d", got)
	}
}

func TestUsageForEstimateRules(t *testing.T) {
	// ① 上游报了真读数 ⇒ 照用，且不标估算。
	real := &upstream.Usage{InputTokens: 7000, OutputTokens: 10}
	real.PromptTokensDetails.CachedTokens = 6144
	sd := &opened{dialogHit: true, promptMode: "delta", dialogPrevInput: 13000}
	out, est := sd.usageFor(real)
	if est || out.PromptTokensDetails == nil || out.PromptTokensDetails.CachedTokens != 6144 {
		t.Fatalf("上游读数必须原样保留，得到 est=%v out=%+v", est, out.PromptTokensDetails)
	}

	// ② 上游没报 + 复用 + 增量 ⇒ 估算并标记。
	sd = &opened{dialogHit: true, promptMode: "delta", dialogPrevInput: 13000}
	out, est = sd.usageFor(&upstream.Usage{InputTokens: 7000, OutputTokens: 10})
	if !est {
		t.Fatal("应标记为估算")
	}
	if out.PromptTokensDetails == nil || out.PromptTokensDetails.CachedTokens != 5250 {
		t.Fatalf("应估算 13000 与 7000 的较小者再打 3/4 折 = 5250，得到 %+v", out.PromptTokensDetails)
	}

	// ③ 发的是全量（非增量）⇒ 不估：那时 prompt 里已有完整历史，上游未必走缓存。
	sd = &opened{dialogHit: true, promptMode: "full", dialogPrevInput: 13000}
	if out, est = sd.usageFor(&upstream.Usage{InputTokens: 7000}); est || out.PromptTokensDetails != nil {
		t.Fatalf("全量模式不应估算，得到 est=%v", est)
	}

	// ④ 压根没复用 ⇒ 不估。
	sd = &opened{promptMode: "delta", dialogPrevInput: 13000}
	if _, est = sd.usageFor(&upstream.Usage{InputTokens: 7000}); est {
		t.Fatal("未复用会话不应估算")
	}
}
