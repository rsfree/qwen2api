package server

import (
	"errors"
	"fmt"
	"strings"
)

// ErrImageUnsupported 图片输入：上游要它自己 OSS 的 file 对象（STS 上传链），
// 外链 URL 会被拒 —— 未实现前必须明确 400，不能静默丢图让模型"看不见"。
var ErrImageUnsupported = errors.New(
	"图片输入暂不支持：上游要求先上传到 qwen 自有 OSS（STS 签名链），" +
		"当前版本只受理纯文本；请先把图片转成文字描述，或等上传链落地（见 docs/UPSTREAM.md §5）")

// errEmptyInput Responses 前门：input 为空。
var errEmptyInput = errors.New("input 不能为空（支持字符串或 {role, content} 数组）")

// errToolsUnsupported 工具调用：上游网页端没有函数调用通道。
var errToolsUnsupported = errors.New(
	"工具调用暂不支持：qwen 网页端没有函数调用通道，网关不伪造 tool_calls —— " +
		"请改用普通对话，或在客户端侧关闭工具")

// FlattenResult 拍平结果。
type FlattenResult struct {
	Prompt string
	// Degraded 有值表示这轮做了信息损失处理（多轮/系统提示被合并），
	// 只进服务端日志，不下发响应（对齐 rsfree 的"降级说明不下发"口径）。
	Degraded string
}

// Flatten 把 OpenAI messages 拍平成上游单条 prompt。
//
// 为什么要拍平：上游每次只发一条 user 消息，而上下文靠 chat_id 的 parent 链承载。
// 网关每次请求都新建会话（无状态），所以必须把历史显式写进文本。
//
// 形态选择的取舍：
//   - **单条 user、无 system、无历史** ⇒ 原文直出。这是绝大多数客户端的第一轮，
//     直出能让模型行为与网页端完全一致（不引入任何包装词）；
//   - 其余情况 ⇒ 结构化拼接（系统指令 / 对话历史 / 当前问题），标签清晰可读。
func Flatten(msgs []ChatMessage) (FlattenResult, error) {
	type item struct {
		role string
		text string
	}
	items := make([]item, 0, len(msgs))
	for _, m := range msgs {
		if m.HasImage() {
			return FlattenResult{}, ErrImageUnsupported
		}
		text, err := m.Text()
		if err != nil {
			return FlattenResult{}, err
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			role = "system"
		case "user", "assistant", "tool", "function":
			// tool/function 结果对上游没有对应概念，按 user 侧资料并入历史。
			if role == "tool" || role == "function" {
				role = "user"
			}
		default:
			role = "user"
		}
		items = append(items, item{role: role, text: text})
	}
	if len(items) == 0 {
		return FlattenResult{}, fmt.Errorf("messages 为空")
	}

	// 直出路径：单条 user 且是最后一条。
	if len(items) == 1 && items[0].role == "user" {
		return FlattenResult{Prompt: items[0].text}, nil
	}

	var systems []string
	var turns []item
	for _, it := range items {
		if it.role == "system" {
			if s := strings.TrimSpace(it.text); s != "" {
				systems = append(systems, s)
			}
			continue
		}
		turns = append(turns, it)
	}
	if len(turns) == 0 {
		return FlattenResult{Prompt: strings.Join(systems, "\n\n")}, nil
	}

	var sb strings.Builder
	degraded := ""
	if len(systems) > 0 {
		sb.WriteString("系统指令：\n")
		sb.WriteString(strings.Join(systems, "\n\n"))
		sb.WriteString("\n\n")
	}

	// 最后一条通常是当前问题；之前的是历史。
	last := turns[len(turns)-1]
	history := turns[:len(turns)-1]
	if len(history) > 0 {
		sb.WriteString("对话历史：\n")
		for _, h := range history {
			label := "用户"
			if h.role == "assistant" {
				label = "助手"
			}
			sb.WriteString(label)
			sb.WriteString("：")
			sb.WriteString(strings.TrimSpace(h.text))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		degraded = fmt.Sprintf("多轮已拍平：%d 条历史 + 当前问题", len(history))
	}
	if last.role == "assistant" {
		// 末尾是 assistant（预填续写场景）：标注清楚，别让模型以为在自问自答。
		sb.WriteString("请接着上一条助手回复继续：\n")
		degraded = joinDegraded(degraded, "末尾 assistant 作为续写前缀")
	} else {
		sb.WriteString("当前问题：\n")
	}
	sb.WriteString(strings.TrimSpace(last.text))

	if len(systems) > 0 {
		degraded = joinDegraded(degraded, fmt.Sprintf("system 已并入 prompt（%d 条）", len(systems)))
	}
	return FlattenResult{Prompt: sb.String(), Degraded: degraded}, nil
}

func joinDegraded(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}
