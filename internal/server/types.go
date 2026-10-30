package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxRequestBody 单个请求体上限（字节）。
//
// 为什么 32MB：多模态是 base64 内联的，一张 8MB 的图编码后约 10.7MB，
// 原来 chat 16MB / images 1MB 的上限会把"发三四张截图"这种正常用法直接挡死
// （images 那条 1MB 连一张手机照片都放不下）。部署侧 nginx 是
// client_max_body_size 64m，这里取一半留出 JSON 与网关缓冲的余量。
const maxRequestBody = 32 << 20

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
	// ImageURL 图片引用，两种写法都收（OpenAI 生态里都出现过）：
	//
	//	"image_url": "https://…"            部分 SDK 直发字符串
	//	"image_url": {"url": "https://…"}   OpenAI 官方形态
	//
	// 所以保留原始 JSON 由 imageRef 分流（写成 *struct{URL} 会吃掉前一种）。
	ImageURL json.RawMessage `json:"image_url"`
	// Source Anthropic 形态：{"type":"base64","media_type":"image/png","data":"…"}
	// 也支持 {"type":"url","url":"https://…"}。
	Source *ImageSource `json:"source"`
	// Data / MediaType 少数客户端把 base64 平铺在 part 上（不带 image_url / source）。
	Data      string `json:"data"`
	MediaType string `json:"media_type"`
}

// ImageSource Anthropic 图片源。
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
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
// file/input_file 段在这里**被忽略**（它们由 collectFiles 单独走上传条目）；
// 图片段也跳过 —— 图片走 collectImageRefs → 上游 files[]（file_class=vision），
// 不参与文本拼接。
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
			// 由 collectImageRefs 处理，不参与文本。
		default:
			// 未知段类型忽略。
		}
	}
	return sb.String(), nil
}

// HasImage 消息里是否带图片段。
func (m ChatMessage) HasImage() bool {
	for _, p := range m.parts() {
		if p.isImagePart() {
			return true
		}
	}
	return false
}

// ---------- 图片引用 ----------

// 图片段的三个类型名：OpenAI chat（image_url）、OpenAI Responses（input_image）、
// Anthropic / 部分 SDK（image）。
func (p ContentPart) isImagePart() bool {
	switch p.Type {
	case "image_url", "input_image", "image":
		return true
	}
	return false
}

// imageRef 取该段指向的图片引用（URL / data URI / 裸 base64 三种形态都放行）。
//
// 三条来源按优先级（同一段只应出现一条）：
//  1. `image_url`：字符串 或 {"url":…} —— OpenAI chat / Responses
//  2. `source`：Anthropic 形态（base64 + media_type，或 url）
//  3. 平铺的 `data`：少数 SDK 把 base64 直接放 part 上
//
// 🔴 这里**只做形态归一，不下载不解码** —— 三种形态的解码在 resolveImageRef，
// 与 /v1/images 的 `image` 字段共用同一条链（image-adapter 的既定契约）。
// 也正因为共用，裸 base64 不必在这里猜 mime：下游按魔数嗅探。
func (p ContentPart) imageRef() (string, error) {
	if raw := strings.TrimSpace(string(p.ImageURL)); raw != "" && raw != "null" {
		var s string
		if err := json.Unmarshal(p.ImageURL, &s); err == nil {
			if v := strings.TrimSpace(s); v != "" {
				return v, nil
			}
			return "", fmt.Errorf("%s.image_url 是空字符串", p.Type)
		}
		var obj struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(p.ImageURL, &obj); err != nil {
			return "", fmt.Errorf("%s.image_url 既不是字符串也不是 {\"url\":…}", p.Type)
		}
		if v := strings.TrimSpace(obj.URL); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s.image_url.url 为空", p.Type)
	}

	if p.Source != nil {
		switch strings.ToLower(strings.TrimSpace(p.Source.Type)) {
		case "base64":
			data := strings.TrimSpace(p.Source.Data)
			if data == "" {
				return "", fmt.Errorf("%s.source.data 为空", p.Type)
			}
			// 少数客户端把完整 data URI 塞进 data；原样放行。
			if strings.HasPrefix(data, "data:") {
				return data, nil
			}
			mime := strings.TrimSpace(p.Source.MediaType)
			if mime == "" {
				mime = "image/png"
			}
			return "data:" + mime + ";base64," + data, nil
		case "url":
			if v := strings.TrimSpace(p.Source.URL); v != "" {
				return v, nil
			}
			return "", fmt.Errorf("%s.source.url 为空", p.Type)
		default:
			return "", fmt.Errorf("%s.source.type=%q 不支持（只认 base64 / url）",
				p.Type, p.Source.Type)
		}
	}

	if d := strings.TrimSpace(p.Data); d != "" {
		// data URI / URL 直通；裸 base64 也直通（mime 由下游魔数嗅探）。
		return d, nil
	}
	return "", fmt.Errorf("%s 段缺少图片引用（需要 image_url / source / data 之一）", p.Type)
}

// imageRefs 取本条消息里的全部图片引用（按出现顺序）。
func (m ChatMessage) imageRefs() ([]string, error) {
	var out []string
	for i, p := range m.parts() {
		if !p.isImagePart() {
			continue
		}
		ref, err := p.imageRef()
		if err != nil {
			return nil, fmt.Errorf("第 %d 个 content part: %w", i+1, err)
		}
		out = append(out, ref)
	}
	return out, nil
}

// collectImageRefs 收集整轮 messages 的图片引用。
//
// 🔴 任何一段解析失败都返回错误 —— 绝不能"跳过这张图继续聊"：
// 那会让模型看不见图却照常作答，调用方以为多模态生效了。
func collectImageRefs(msgs []ChatMessage) ([]string, error) {
	var out []string
	for i, m := range msgs {
		refs, err := m.imageRefs()
		if err != nil {
			return nil, fmt.Errorf("messages[%d]（role=%s）: %w", i, m.Role, err)
		}
		out = append(out, refs...)
	}
	return out, nil
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
