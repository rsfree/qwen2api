package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- OpenAI Responses 前门 ----------
//
// 供 Codex 之类只认 /v1/responses 的客户端接入。请求被翻成 chat 形态，
// 响应按 Responses 的对象/事件形态回写。
//
// 🔴 只实现文本输出：tools / 内置检索 / 多模态输入一律 400 —— 上游网页端
// 没有对应通道，假装支持只会让客户端拿到永远不触发的工具调用。

type responsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`
	Stream       bool            `json:"stream"`
}

// handleResponses `POST /v1/responses`。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	msgs, err := responsesToChat(req)
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
		model = s.conf().DefaultModel
	}

	// 图片：input_image → 上游 files[]（vision）。坏引用一律 400。
	imgRefs, imgErr := collectImageRefs(msgs)
	if imgErr != nil {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", imgErr.Error(), "image")
		return
	}
	if len(imgRefs) > maxInputImages {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("图片最多 %d 张（本次 %d 张）", maxInputImages, len(imgRefs)), "image")
		return
	}
	spec := upstream.TextSpec()
	spec.ImageRefs = imgRefs
	sd, err := s.openUpstream(r.Context(), s.blobBase(r), model, s.conf().DefaultThinking, flat.Prompt, spec)
	if err != nil {
		writeError(w, httpStatusFor(err), errTypeFor(err), err.Error())
		return
	}
	defer sd.stream.Close()
	defer s.pool.Release(sd.acct)
	defer s.pool.MarkOK(sd.acct)

	id := "resp_" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
	msgID := "msg_" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
	created := time.Now().Unix()

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
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model,
			"status": "completed",
			"output": []any{map[string]any{
				"id": msgID, "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": sb.String(), "annotations": []any{}}},
			}},
			"usage": responsesUsage(usage),
		})
		return
	}

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

	seq := 0
	emit := func(event string, payload map[string]any) {
		payload["type"] = event
		payload["sequence_number"] = seq
		seq++
		s.anthropicEvent(w, flusher, event, payload)
	}

	emit("response.created", map[string]any{
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model, "status": "in_progress",
		},
	})
	emit("response.output_item.added", map[string]any{
		"output_index": 0, "item": map[string]any{"id": msgID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
	})
	emit("response.content_part.added", map[string]any{
		"item_id": msgID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})

	var (
		sb    strings.Builder
		usage *upstream.Usage
	)
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			emit("response.failed", map[string]any{
				"response": map[string]any{"id": id, "status": "failed",
					"error": map[string]any{"code": "upstream_error", "message": err.Error()}},
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
			sb.WriteString(ev.Text)
			emit("response.output_text.delta", map[string]any{
				"item_id": msgID, "output_index": 0, "content_index": 0, "delta": ev.Text,
			})
		}
		if ev.Done {
			break
		}
	}

	text := sb.String()
	emit("response.output_text.done", map[string]any{
		"item_id": msgID, "output_index": 0, "content_index": 0, "text": text,
	})
	emit("response.content_part.done", map[string]any{
		"item_id": msgID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	emit("response.output_item.done", map[string]any{
		"output_index": 0,
		"item": map[string]any{"id": msgID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}},
	})
	emit("response.completed", map[string]any{
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model, "status": "completed",
			"output": []any{map[string]any{
				"id": msgID, "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			}},
			"usage": responsesUsage(usage),
		},
	})
	flusher.Flush()
}

func responsesUsage(u *upstream.Usage) map[string]any {
	if u == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return map[string]any{
		"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "total_tokens": total,
	}
}

// responsesToChat 解析 input 字段（字符串 或 消息数组）。
func responsesToChat(req responsesRequest) ([]ChatMessage, error) {
	var out []ChatMessage
	if req.Instructions != "" {
		out = append(out, ChatMessage{Role: "system", Content: mustJSON(req.Instructions)})
	}
	if len(req.Input) == 0 {
		return nil, errEmptyInput
	}
	var s string
	if err := json.Unmarshal(req.Input, &s); err == nil {
		out = append(out, ChatMessage{Role: "user", Content: mustJSON(s)})
		return out, nil
	}
	var items []struct {
		Role    string          `json:"role"`
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, errEmptyInput
	}
	for _, it := range items {
		// 工具调用/工具输出项：上游没有对应通道 ⇒ 明确拒绝，不静默丢。
		if it.Type == "function_call" || it.Type == "function_call_output" {
			return nil, errToolsUnsupported
		}
		role := it.Role
		if role == "" {
			role = "user"
		}
		content, err := anthropicContentRaw(it.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, ChatMessage{Role: role, Content: content})
	}
	if len(out) == 0 {
		return nil, errEmptyInput
	}
	return out, nil
}
