package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ChatRequest OpenAI /v1/chat/completions 请求体（只声明用得到的字段；
// 其余字段解析后忽略 —— 网关不假装支持上游没有的采样参数）。
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	// StreamOptions 仅识别 include_usage：为 true 时流末补一个 usage 块。
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	// Thinking 本网关扩展：auto / thinking / fast（对齐网页端三档）。
	Thinking string `json:"thinking,omitempty"`
	// ReasoningEffort 兼容 OpenAI/Anthropic 习惯：low ⇒ fast，medium ⇒ auto，high ⇒ thinking。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// User 仅作日志/粘性键，不影响上游。
	User string `json:"user,omitempty"`
}

// ChatMessage 一条消息。content 允许 string 或 [{"type":"text","text":...}]。
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`
}

// ContentPart 一段内容。文本 / 图片 / 文件三种在实现里都用同一结构接，
// 按 Type 分派（未知类型忽略，不猜语义）。
type ContentPart struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	FileID string `json:"file_id"`
	File   *struct {
		FileID string `json:"file_id"`
	} `json:"file"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// parts 解析 content 为分段数组（content 是纯字符串时返回 nil）。
func (m ChatMessage) parts() []ContentPart {
	if len(m.Content) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(m.Content, &one); err == nil {
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil
	}
	return parts
}

// Text 提取消息文本。
//
// file/input_file 段在这里**被忽略**（它们由 collectFiles 单独走上传条目），
// 图片段仍然返回明确错误 —— 但注意：`/v1/images` 之外的入口用 image_url 时，
// 本仓会明确拒绝（见 ErrImageUnsupported），不做静默丢弃。
func (m ChatMessage) Text() (string, error) {
	if len(m.Content) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, nil
	}
	var probe []map[string]any
	if err := json.Unmarshal(m.Content, &probe); err != nil {
		return "", fmt.Errorf("content 形态不支持（既不是字符串也不是分段数组）")
	}
	var sb strings.Builder
	for _, p := range m.parts() {
		switch p.Type {
		case "text", "input_text":
			sb.WriteString(p.Text)
		case "file", "input_file":
			// 由 collectFiles 处理，不参与文本。
		case "image_url", "input_image", "image":
			return "", ErrImageUnsupported
		default:
			// 未知段类型忽略。
		}
	}
	return sb.String(), nil
}

// HasImage 消息里是否带图片段。
func (m ChatMessage) HasImage() bool {
	for _, p := range m.parts() {
		switch p.Type {
		case "image_url", "input_image", "image":
			return true
		}
	}
	return false
}

// ResolveThinking 把请求里的思考意图归一成 auto/thinking/fast。
func (r ChatRequest) ResolveThinking(fallback string) string {
	if v := strings.TrimSpace(r.Thinking); v != "" {
		switch strings.ToLower(v) {
		case "auto", "thinking", "fast":
			return strings.ToLower(v)
		case "off", "none", "disabled", "false":
			return "fast"
		case "on", "enabled", "true":
			return "thinking"
		}
	}
	switch strings.ToLower(strings.TrimSpace(r.ReasoningEffort)) {
	case "minimal", "low":
		return "fast"
	case "medium":
		return "auto"
	case "high":
		return "thinking"
	}
	if fallback == "" {
		return "auto"
	}
	return fallback
}

// ---------- 响应（OpenAI 形态） ----------

// ResponseMessage 非流式响应里的 assistant 消息。
type ResponseMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Choice 非流式选择项。
type Choice struct {
	Index        int              `json:"index"`
	Message      *ResponseMessage `json:"message,omitempty"`
	FinishReason *string          `json:"finish_reason"`
}

// ChatResponse 非流式响应。
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Delta 流式增量。
type Delta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// StreamChoice 流式选择项。
type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// ChatChunk 流式响应块。
type ChatChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

// Usage OpenAI 用量形态。
type Usage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// ErrorBody OpenAI 错误形态。
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 错误明细（OpenAI 形态；param/code 可空）。
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}
