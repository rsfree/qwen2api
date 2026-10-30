package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- Anthropic Messages 前门 ----------
//
// 目的：Claude Code / Cline 这类只认 /v1/messages 的客户端能零改造接入。
// 语义对齐：请求翻成 chat 形态复用同一条选号/轮换/冷却链路，响应（含 SSE）
// 翻回 Anthropic 事件序列。
//
// 🔴 诚实边界：不做 thinking block（Anthropic 的 thinking 需要签名，编一个
// 假 signature 会让客户端校验失败）；推理内容只走 chat 门的 reasoning_content。

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    json.RawMessage    `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicResponse struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []anthropicTextBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	StopSequence string               `json:"stop_sequence,omitempty"`
	Usage        anthropicUsage       `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// handleMessages `POST /v1/messages`。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	msgs, err := anthropicToChat(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	flat, err := Flatten(msgs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.cfg.DefaultModel
	}

	sd, err := s.openUpstream(r.Context(), model, s.cfg.DefaultThinking, flat.Prompt)
	if err != nil {
		writeError(w, httpStatusFor(err), errTypeFor(err), err.Error())
		return
	}
	defer sd.stream.Close()
	defer s.pool.Release(sd.acct)
	defer s.pool.MarkOK(sd.acct)

	id := "msg_" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]

	if !req.Stream {
		var sb strings.Builder
		var usage *upstream.Usage
		for {
			ev, more, err := sd.stream.Next()
			if err != nil {
				writeError(w, httpStatusFor(err), errTypeFor(err), err.Error())
				return
			}
			if !more {
				break
			}
			if ev.Usage != nil {
				usage = ev.Usage
			}
			if ev.Text != "" {
				sb.WriteString(ev.Text)
			}
			if ev.Done {
				break
			}
		}
		resp := anthropicResponse{
			ID: id, Type: "message", Role: "assistant", Model: model,
			Content:    []anthropicTextBlock{{Type: "text", Text: sb.String()}},
			StopReason: "end_turn",
		}
		if usage != nil {
			resp.Usage = anthropicUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 流式：严格按 Anthropic 事件序列。
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "响应写入器不支持流式")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s.anthropicEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	s.anthropicEvent(w, flusher, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})

	var usage *upstream.Usage
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			s.anthropicEvent(w, flusher, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": err.Error()},
			})
			break
		}
		if !more {
			break
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.Text != "" {
			s.anthropicEvent(w, flusher, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": ev.Text},
			})
		}
		if ev.Done {
			break
		}
	}

	s.anthropicEvent(w, flusher, "content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	})
	usageOut := map[string]any{"output_tokens": 0}
	if usage != nil {
		usageOut["output_tokens"] = usage.OutputTokens
		usageOut["input_tokens"] = usage.InputTokens
	}
	s.anthropicEvent(w, flusher, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": usageOut,
	})
	s.anthropicEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
	flusher.Flush()
}

// handleCountTokens 粗略估算输入 token（Anthropic 客户端会调）。
//
// 口径：CJK 约 1 字 = 1 token，其余按 4 字符 = 1 token 估。**明确是估算**，
// 不做假精确 —— 真实用量由上游 usage 在正式响应里给出。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	msgs, err := anthropicToChat(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	flat, err := Flatten(msgs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": EstimateTokens(flat.Prompt)})
}

// EstimateTokens 估算 token 数（CJK 1:1，ASCII 4:1）。
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func (s *Server) anthropicEvent(w http.ResponseWriter, f http.Flusher, event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "event: "+event+"\ndata: ")
	_, _ = w.Write(raw)
	_, _ = io.WriteString(w, "\n\n")
	f.Flush()
}

// anthropicToChat Anthropic messages → 内部 chat messages。
func anthropicToChat(req anthropicRequest) ([]ChatMessage, error) {
	var out []ChatMessage
	if sys := anthropicText(req.System); sys != "" {
		out = append(out, ChatMessage{Role: "system", Content: mustJSON(sys)})
	}
	for _, m := range req.Messages {
		content, err := anthropicContentRaw(m.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, ChatMessage{Role: m.Role, Content: content})
	}
	return out, nil
}

// anthropicText 解析 system 字段（字符串或分段数组）。
func anthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []anthropicTextBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// anthropicContentText 解析消息 content（字符串或分段数组）。
func anthropicContentText(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errContentShape
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			sb.WriteString(p.Text)
		case "image", "image_url", "input_image":
			// 图片不参与文本（只用于 token 估算）；真正的多模态走 anthropicContentRaw。
		}
	}
	return sb.String(), nil
}

// errContentShape content 既不是字符串也不是分段数组。
var errContentShape = errors.New("content 形态不支持（既不是字符串也不是分段数组）")

// errDocumentUnsupported Anthropic document 块。
var errDocumentUnsupported = errors.New(
	"document 块暂不支持：上游要它自己 OSS 的附件条目 —— " +
		"请先用 /v1/files 上传，再在 content 里引用 file_id")

// anthropicContentRaw 把 Anthropic / Responses 的 content 归一成内部形态。
//
//   - 字符串 ⇒ 原样（转成 JSON 字符串）
//   - 分段数组 ⇒ 过滤：文本与图片保留（图片交给 collectImageRefs → 上游 files[]），
//     工具/文档块明确 400（静默丢块会让客户端以为工具跑过了）
func anthropicContentRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return mustJSON(""), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return mustJSON(s), nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errContentShape
	}
	keep := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p, &head)
		switch head.Type {
		case "text", "input_text", "image", "image_url", "input_image":
			keep = append(keep, p)
		case "tool_use", "tool_result":
			return nil, errToolsUnsupported
		case "document":
			return nil, errDocumentUnsupported
		default:
			// 未知块类型忽略（与 chat 门同口径）。
		}
	}
	return json.Marshal(keep)
}

func mustJSON(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}

