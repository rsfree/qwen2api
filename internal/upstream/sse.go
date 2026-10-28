package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Usage 上游真实用量（帧顶层携带，随流递增，最后一份即终值）。
//
// 实测 2026-10-28 形态：
//
//	{"input_tokens":1239,"output_tokens":105,"characters":0,"total_tokens":1344,
//	 "input_tokens_details":{"text_tokens":1239},
//	 "output_tokens_details":{"reasoning_tokens":99,"text_tokens":105}}
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// Characters 上游恒 0（保留原样，不做解释）。
	Characters         int `json:"characters"`
	InputTokensDetails struct {
		TextTokens int `json:"text_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
		TextTokens      int `json:"text_tokens"`
	} `json:"output_tokens_details"`
}

// Created 首帧 `response.created` 的载荷（实测新增于旧契约之外）。
type Created struct {
	ChatID        string `json:"chat_id"`
	ParentID      string `json:"parent_id"`
	ResponseID    string `json:"response_id"`
	ResponseIndex string `json:"response_index"`
}

// Event 一条 SSE 帧解析结果。零值字段表示该帧不含此类信息。
type Event struct {
	Text      string // 正文增量（phase=answer）
	Reasoning string // 思考摘要增量（phase=thinking_summary，已做累加 diff）
	Usage     *Usage // 本帧携带的用量快照
	Created   *Created
	Done      bool // 收到 answer 阶段 finished

	// ImagePrompt 图片工具参数里的提示词（phase=image_gen_tool，取最后一次快照）。
	// 上游让模型先把中文需求改写成英文绘图提示词，这个字段是**上游产物**，不是我们想的。
	ImagePrompt string
	// Images 图片产物直链（phase=image_gen_tool 且 status=finished 时出现）。
	// 形如 https://cdn.qwenlm.ai/output/<uid>/t2i/<chat>/1req_x.png?key=<JWT>，可直接下载。
	Images []string
	// ToolName 工具名（目前只见过 image_gen）。
	ToolName string
	// QuotaExhausted 上游明确表示额度/权益不足。
	//
	// 🔴 这是**第三条错误通道**（前两条是 HTTP 4xx 与流内 error 帧）：
	// 上游把"额度用完"当成一句正常 answer 下发，只是额外带 `extra.update_member: true`。
	// 不识别它就会把"额度耗尽"误报成"模型没画" —— 2026-10-28 实测踩到。
	QuotaExhausted bool
	// QuotaMessage 上游原话（透给调用方，便于判断是"今日额度"还是"会员权益"）。
	QuotaMessage string
	// UnknownPhase 上游出现了本仓没处理过的 phase。
	//
	// 为什么单独记：qwen 的工具面在扩（code_interpreter / web_extractor / amap …），
	// 新 phase 的帧会被解析器当"无关帧"忽略 —— 表象是"模型说了句没头没尾的话"，
	// 而真正的原因是产物被静默丢掉。有了这个字段，上层能把它记进日志而不是瞎猜。
	UnknownPhase string
}

// wireEvent 上游帧的宽松映射 —— 只声明实现用到的字段。
type wireEvent struct {
	Created    *Created        `json:"response.created"`
	Error      json.RawMessage `json:"error"`
	Usage      *Usage          `json:"usage"`
	ResponseID string          `json:"response_id"`
	Choices    []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			Phase   string `json:"phase"`
			Status  string `json:"status"`
			Name    string `json:"name"`
			// FunctionCall 工具调用：arguments 是**全量快照**（逐帧累加，不是增量）。
			FunctionCall *struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function_call"`
			Extra struct {
				SummaryTitle struct {
					Content []string `json:"content"`
				} `json:"summary_title"`
				SummaryThought struct {
					Content []string `json:"content"`
				} `json:"summary_thought"`
				// 图片工具结果（phase=image_gen_tool + status=finished）。
				FunctionID string `json:"function_id"`
				ToolResult []struct {
					Image string `json:"image"`
				} `json:"tool_result"`
				ImageList []struct {
					Image string `json:"image"`
				} `json:"image_list"`
				DisplayPosition string `json:"display_position"`
				// UpdateMember 额度/权益耗尽的上游信号（2026-10-28 实测：
				// 与一句"今日生图额度已用完，升级会员…"的 answer 同时出现）。
				UpdateMember bool `json:"update_member"`
			} `json:"extra"`
		} `json:"delta"`
	} `json:"choices"`
}

// Parser 有状态帧解析器：思考摘要的 extra 是**累加数组**，必须按账号级 diff，
// 否则每帧都会把全部历史思考重发一遍。
type Parser struct {
	seenTitle   int
	seenThought int
}

// NewParser 新建解析器。
func NewParser() *Parser { return &Parser{} }

// Parse 解析一条 SSE data 载荷。
//
// 返回 (event, ok)：ok=false 表示这帧是心跳/无关帧（调用方忽略，不算失败）。
func (p *Parser) Parse(payload []byte) (Event, bool, error) {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed == "[DONE]" {
		return Event{}, false, nil
	}
	var w wireEvent
	if err := json.Unmarshal([]byte(trimmed), &w); err != nil {
		// 非 JSON 帧（心跳/注释）不是错误：上游确实会插空行与注释。
		return Event{}, false, nil
	}
	var ev Event

	if w.Error != nil && len(w.Error) > 0 && string(w.Error) != "null" {
		// 流内错误（HTTP 200 包着错误）必须响亮失败，不能当成"这帧没内容"。
		return Event{}, true, decodeStreamError(w.Error)
	}
	if w.Created != nil {
		ev.Created = w.Created
		return ev, true, nil
	}
	if w.Usage != nil {
		ev.Usage = w.Usage
	}

	if len(w.Choices) == 0 {
		// 只有 usage 没有 choices 的帧（收尾计数），仍算有效帧。
		return ev, ev.Usage != nil, nil
	}
	d := w.Choices[0].Delta

	switch d.Phase {
	case "thinking_summary":
		if d.Status == "finished" {
			return ev, true, nil
		}
		ev.Reasoning = p.diffReasoning(d.Extra.SummaryTitle.Content, d.Extra.SummaryThought.Content)
	case "answer":
		// 额度耗尽伪装成 answer：extra.update_member + 一句提示语。
		if d.Extra.UpdateMember || looksLikeQuota(d.Content) {
			ev.QuotaExhausted = true
			ev.QuotaMessage = strings.TrimSpace(d.Content)
			ev.Done = d.Status == "finished"
			return ev, true, nil
		}
		if d.Status == "finished" {
			ev.Done = true
			return ev, true, nil
		}
		ev.Text = d.Content
	case "image_gen":
		// 🔴 t2i 专用路径（2026-10-28 实测）：`delta.content` 直接就是**图片直链**
		// （不是 markdown、不是工具参数），并且带 `extra.output_image_hw` 实际像素。
		// 这与 t2t 的 image_gen_tool 是两条完全不同的路：
		//   · t2i  ：phase=image_gen，content=URL，一帧一张
		//   · t2t  ：phase=image_gen_tool，arguments 里是 prompt，产物在 extra.image_list
		// 结束帧同样是 status=finished —— 但它**不是**整条流的结束（后面可能还有 answer）。
		if d.Status == "finished" {
			return ev, true, nil
		}
		if u := strings.TrimSpace(d.Content); strings.HasPrefix(u, "http") {
			ev.Images = append(ev.Images, u)
		} else if d.Content != "" {
			ev.Text = d.Content
		}
	case "image_gen_tool":
		// 图片生成 = 模型在 t2t 对话里调 image_gen 工具。
		//   · 中间帧：function_call.arguments 逐帧**全量**给出（含 prompt），
		//   · 结束帧：status=finished + extra.image_list[].image（带 JWT 的 CDN 直链）。
		// 注意结束帧的 role 是 "function" 而不是 "assistant"。
		if d.FunctionCall != nil {
			ev.ToolName = d.FunctionCall.Name
			ev.ImagePrompt = extractToolPrompt(d.FunctionCall.Arguments)
		}
		if d.Name != "" {
			ev.ToolName = d.Name
		}
		if d.Status == "finished" {
			// image_list 是最终产物；tool_result 是工具原始返回，两者通常同时有，
			// 去重后一起给出（上游偶发只给其中一个）。
			seen := map[string]bool{}
			for _, item := range d.Extra.ImageList {
				if item.Image != "" && !seen[item.Image] {
					seen[item.Image] = true
					ev.Images = append(ev.Images, item.Image)
				}
			}
			for _, item := range d.Extra.ToolResult {
				if item.Image != "" && !seen[item.Image] {
					seen[item.Image] = true
					ev.Images = append(ev.Images, item.Image)
				}
			}
		}
	}
	return ev, true, nil
}

// diffReasoning 把累加数组转成本帧新增文本：标题包【】，要点按行。
func (p *Parser) diffReasoning(titles, thoughts []string) string {
	var parts []string
	if len(titles) > p.seenTitle {
		for _, t := range titles[p.seenTitle:] {
			if s := strings.TrimSpace(t); s != "" {
				parts = append(parts, "【"+s+"】")
			}
		}
		p.seenTitle = len(titles)
	}
	if len(thoughts) > p.seenThought {
		for _, t := range thoughts[p.seenThought:] {
			if s := strings.TrimSpace(t); s != "" {
				parts = append(parts, s)
			}
		}
		p.seenThought = len(thoughts)
	}
	return strings.Join(parts, "\n")
}

// looksLikeQuota 兜底识别额度文案（上游不一定每次都带 update_member）。
//
// 只匹配明确指向"额度/会员权益"的措辞，避免把模型正常的解释文本误判成错误。
func looksLikeQuota(content string) bool {
	c := content
	for _, kw := range []string{"生图额度已用完", "额度已用完", "今日额度已用完",
		"升级会员可获得更多", "quota exceeded", "out of credits"} {
		if strings.Contains(c, kw) {
			return true
		}
	}
	return false
}

// extractToolPrompt 从工具参数 JSON 里取 prompt（取不到返回空串，不猜）。
func extractToolPrompt(args string) string {
	if args == "" {
		return ""
	}
	var v struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		// 参数是流式累加的，中途可能是非法 JSON —— 静默忽略，等最后一帧。
		return ""
	}
	return strings.TrimSpace(v.Prompt)
}

// decodeStreamError 流内错误事件有两种形态：字符串 / 对象。
func decodeStreamError(raw json.RawMessage) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return NewErr(KindUnknown, "chat 流", 200, "", s, false)
	}
	var obj struct {
		Code    string `json:"code"`
		Details string `json:"details"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		detail := obj.Details
		if detail == "" {
			detail = obj.Message
		}
		return NewErr(KindUnknown, "chat 流", 200, obj.Code, detail, false)
	}
	return NewErr(KindUnknown, "chat 流", 200, "", string(raw), false)
}

// SSEFrames 从字节流里切 SSE 帧的增量切分器（按空行分隔，兼容 \r\n）。
//
// 独立于 net/http：便于单测直接喂字节。
type SSEFrames struct {
	buf []byte
}

// Push 追加一段字节，返回本次可完整解析的 data 载荷列表。
func (s *SSEFrames) Push(chunk []byte) [][]byte {
	s.buf = append(s.buf, chunk...)
	var out [][]byte
	for {
		idx, sepLen := findBlankLine(s.buf)
		if idx < 0 {
			break
		}
		block := s.buf[:idx]
		s.buf = s.buf[idx+sepLen:]
		if data := extractData(block); data != nil {
			out = append(out, data)
		}
	}
	// 防止对端不发空行导致 buf 无限增长（上游正常会发；这里只是兜底）。
	if len(s.buf) > 4<<20 {
		s.buf = s.buf[len(s.buf)-(1<<19):]
	}
	return out
}

// Flush 收流时把残留缓冲当最后一帧处理。
func (s *SSEFrames) Flush() [][]byte {
	if len(s.buf) == 0 {
		return nil
	}
	block := s.buf
	s.buf = nil
	if data := extractData(block); data != nil {
		return [][]byte{data}
	}
	return nil
}

func findBlankLine(b []byte) (int, int) {
	for i := 0; i+1 < len(b); i++ {
		// \r\n\r\n
		if b[i] == '\r' && b[i+1] == '\n' {
			if i+3 < len(b) && b[i+2] == '\r' && b[i+3] == '\n' {
				return i, 4
			}
			continue
		}
		// \n\n 与 \n\r\n
		if b[i] == '\n' {
			if b[i+1] == '\n' {
				return i, 2
			}
			if b[i+1] == '\r' && i+2 < len(b) && b[i+2] == '\n' {
				return i, 3
			}
		}
	}
	return -1, 0
}

func extractData(block []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n")
	var payload []string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "data:") {
			payload = append(payload, strings.TrimPrefix(strings.TrimPrefix(ln, "data:"), " "))
		}
	}
	if len(payload) == 0 {
		return nil
	}
	return []byte(strings.Join(payload, "\n"))
}

// ErrNoText 上游流跑完却一个字没提取到 —— 说明帧形态与实测不符，必须响亮失败，
// 不能静默返回空回复（那会让调用方以为模型答了空）。
var ErrNoText = fmt.Errorf("上游流式响应未提取到任何文本（SSE 形态可能与实测不符，见 docs/UPSTREAM.md §4）")
