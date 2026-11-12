package server

// 粘性命中发增量、未命中回退全量拼接（2026-11-12 用户要求）。
//
// 背景：复用上游会话时，会话里已经有前几轮历史，再发全量拍平文本等于
// 把历史写两遍。所以两份文本各管一路：
//   - Delta 只在"确定的链式复用"（复用 + parentID 非空）下用；
//   - 其余一切情况（首轮 / 并发降级 / 换号 / 带附件）都退回 Full，
//     也就是旧版本的"拼接历史对话"。

import (
	"encoding/json"
	"testing"
)

func TestSubmitPromptsFor(t *testing.T) {
	ps := submitPrompts{Full: "全量拼接", Delta: "本轮问题"}
	cases := []struct {
		name   string
		reused bool
		ps     submitPrompts
		want   string
	}{
		{"复用了会话且增量可用", true, ps, "本轮问题"},
		{"没复用（首轮/降级）", false, ps, "全量拼接"},
		{"复用了但没有增量（末尾 assistant）", true, submitPrompts{Full: "全量拼接"}, "全量拼接"},
		{"增量是空白", true, submitPrompts{Full: "全量拼接", Delta: " \n\t"}, "全量拼接"},
	}
	for _, c := range cases {
		if got := c.ps.For(c.reused); got != c.want {
			t.Errorf("%s: 得到 %q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestFlattenLastUser(t *testing.T) {
	msg := func(role, text string) ChatMessage {
		raw, _ := json.Marshal(text)
		return ChatMessage{Role: role, Content: raw}
	}

	// 单条 user：直出路径，增量就是它本身。
	if got, err := Flatten([]ChatMessage{msg("user", "你好呀")}); err != nil || got.LastUser != "你好呀" {
		t.Fatalf("单条 user 的 LastUser 应为原文，得到 %q err=%v", got.LastUser, err)
	}

	// 多轮末尾是 user：增量只取最后那条，不能带历史。
	multi, err := Flatten([]ChatMessage{
		msg("system", "你是助手"),
		msg("user", "第一问"),
		msg("assistant", "第一答"),
		msg("user", "第二问"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if multi.LastUser != "第二问" {
		t.Fatalf("多轮 LastUser 应为最后一条 user，得到 %q", multi.LastUser)
	}
	if multi.Prompt == multi.LastUser {
		t.Fatal("全量 Prompt 必须包含历史，不能等于增量")
	}

	// 末尾是 assistant（续写场景）：没有增量，必须回退全量。
	cont, err := Flatten([]ChatMessage{
		msg("user", "写个函数"),
		msg("assistant", "func f() {"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cont.LastUser != "" {
		t.Fatalf("末尾 assistant 不应给增量，得到 %q", cont.LastUser)
	}
}

// 带文本附件时必须强制回退全量：附件内容只内联在 Full 里。
// 这条守的是"粘性命中后附件凭空消失"的静默错误。
func TestSubmitPromptsAttachmentForcesFull(t *testing.T) {
	ps := submitPrompts{Full: "全量（含附件正文）", Delta: "本轮问题"}
	ps.Delta = "" // chat.go 在 len(textFiles)>0 时就是这么做的
	if got := ps.For(true); got != "全量（含附件正文）" {
		t.Fatalf("带附件时应回退全量，得到 %q", got)
	}
}
