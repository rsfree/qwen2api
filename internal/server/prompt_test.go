package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// upstreamUsageStub 一次真实响应里的 usage（2026-10-28 抓包）。
var upstreamUsageStub = func() upstream.Usage {
	var u upstream.Usage
	u.InputTokens = 1239
	u.OutputTokens = 105
	u.TotalTokens = 1344
	u.OutputTokensDetails.ReasoningTokens = 99
	u.OutputTokensDetails.TextTokens = 105
	return u
}()

func msg(role, text string) ChatMessage {
	raw, _ := json.Marshal(text)
	return ChatMessage{Role: role, Content: raw}
}

func TestFlattenSingleUserIsVerbatim(t *testing.T) {
	// 单条 user 必须原文直出：这是绝大多数客户端的第一轮，
	// 任何包装词都会让模型行为偏离网页端。
	got, err := Flatten([]ChatMessage{msg("user", "你好")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "你好" {
		t.Fatalf("单条 user 被改写了: %q", got.Prompt)
	}
	if got.Degraded != "" {
		t.Fatalf("不该有降级说明: %q", got.Degraded)
	}
}

func TestFlattenMultiTurn(t *testing.T) {
	got, err := Flatten([]ChatMessage{
		msg("system", "你是助手"),
		msg("user", "第一问"),
		msg("assistant", "第一答"),
		msg("user", "第二问"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"系统指令：", "你是助手", "对话历史：", "用户：第一问", "助手：第一答", "当前问题：", "第二问"} {
		if !strings.Contains(got.Prompt, want) {
			t.Fatalf("拍平结果缺 %q:\n%s", want, got.Prompt)
		}
	}
	if got.Degraded == "" {
		t.Fatal("多轮必须记降级说明（只进日志）")
	}
	// 历史必须在当前问题之前。
	if strings.Index(got.Prompt, "第一问") > strings.Index(got.Prompt, "第二问") {
		t.Fatal("历史与当前问题顺序颠倒")
	}
}

func TestFlattenAssistantTail(t *testing.T) {
	// 末尾 assistant = 预填续写，必须标注，别让模型以为在自问自答。
	got, _ := Flatten([]ChatMessage{msg("user", "写个开头"), msg("assistant", "从前有座山")})
	if !strings.Contains(got.Prompt, "继续") {
		t.Fatalf("末尾 assistant 未标注续写:\n%s", got.Prompt)
	}
}

func TestFlattenIgnoresImageParts(t *testing.T) {
	// 图片走上游 files[]（file_class=vision），不参与文本拍平；
	// 文本部分必须原样保留（直出路径不加包装）。
	raw := json.RawMessage(`[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]`)
	res, err := Flatten([]ChatMessage{{Role: "user", Content: raw}})
	if err != nil {
		t.Fatalf("图片不该再被拒: %v", err)
	}
	if res.Prompt != "看图" {
		t.Fatalf("文本被图片污染: %q", res.Prompt)
	}
}

func TestFlattenContentParts(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"第一段"},{"type":"text","text":"第二段"}]`)
	got, err := Flatten([]ChatMessage{{Role: "user", Content: raw}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "第一段第二段" {
		t.Fatalf("分段文本拼接错误: %q", got.Prompt)
	}
}

func TestResolveThinking(t *testing.T) {
	cases := []struct {
		req      ChatRequest
		fallback string
		want     string
	}{
		{ChatRequest{Thinking: "fast"}, "auto", "fast"},
		{ChatRequest{Thinking: "off"}, "auto", "fast"},
		{ChatRequest{Thinking: "on"}, "auto", "thinking"},
		{ChatRequest{ReasoningEffort: "low"}, "auto", "fast"},
		{ChatRequest{ReasoningEffort: "high"}, "auto", "thinking"},
		{ChatRequest{ReasoningEffort: "medium"}, "auto", "auto"},
		{ChatRequest{}, "thinking", "thinking"},
		{ChatRequest{Thinking: "乱写"}, "auto", "auto"},
	}
	for _, c := range cases {
		if got := c.req.ResolveThinking(c.fallback); got != c.want {
			t.Fatalf("%+v fallback=%s → %s, want %s", c.req, c.fallback, got, c.want)
		}
	}
}

func TestToUsage(t *testing.T) {
	// 上游 input/output 必须映射成 prompt/completion，不能直接透传字段名。
	u := toUsage(&upstreamUsageStub)
	if u.PromptTokens != 1239 || u.CompletionTokens != 105 || u.TotalTokens != 1344 {
		t.Fatalf("字段映射错误: %+v", u)
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 99 {
		t.Fatalf("reasoning_tokens 未透出: %+v", u.CompletionTokensDetails)
	}
}

func TestEstimateTokens(t *testing.T) {
	if n := EstimateTokens("你好世界"); n != 4 {
		t.Fatalf("CJK 应 1:1，得到 %d", n)
	}
	if n := EstimateTokens("abcdefgh"); n != 2 {
		t.Fatalf("ASCII 应 4:1，得到 %d", n)
	}
}
