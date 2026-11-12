package server

// 缓存用量按 OpenAI 协议透出（2026-11-12 用户要求"补相关字段"）。
//
// 三个前门的字段位置各不相同，写死 JSON 名字断言 —— 名字错了客户端就是读不到，
// 而这类错误在单测里只有把序列化结果翻出来看才发现。

import (
	"encoding/json"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

func cachedUsage() *upstream.Usage {
	u := &upstream.Usage{InputTokens: 14605, OutputTokens: 142, TotalTokens: 14747}
	u.PromptTokensDetails.CachedTokens = 13312
	u.PromptTokensDetails.TextTokens = 14605
	u.OutputTokensDetails.ReasoningTokens = 101
	return u
}

// Chat Completions：prompt_tokens_details.cached_tokens
func TestToUsageExposesCachedTokens(t *testing.T) {
	raw, err := json.Marshal(toUsage(cachedUsage()))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.PromptTokensDetails.CachedTokens != 13312 {
		t.Fatalf("cached_tokens 应为 13312，实际 JSON=%s", raw)
	}
}

// 没有缓存口径时不要凭空造一个 0 字段（普通 t2t 上游根本不报）。
func TestToUsageOmitsCachedWhenAbsent(t *testing.T) {
	u := &upstream.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
	raw, _ := json.Marshal(toUsage(u))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["prompt_tokens_details"]; ok {
		t.Fatalf("上游没报缓存时不应输出该字段：%s", raw)
	}
}

// Responses：input_tokens_details.cached_tokens / output_tokens_details.reasoning_tokens
func TestResponsesUsageDetails(t *testing.T) {
	m := responsesUsage(cachedUsage())
	in, _ := m["input_tokens_details"].(map[string]any)
	if in["cached_tokens"] != 13312 {
		t.Fatalf("Responses 的 cached_tokens 位置不对: %v", m)
	}
	out, _ := m["output_tokens_details"].(map[string]any)
	if out["reasoning_tokens"] != 101 {
		t.Fatalf("Responses 的 reasoning_tokens 位置不对: %v", m)
	}
	// nil 也要给出完整形状（客户端不会为 0 做特判）。
	if empty := responsesUsage(nil); empty["input_tokens_details"] == nil {
		t.Fatalf("nil usage 也应保留明细结构: %v", empty)
	}
}
