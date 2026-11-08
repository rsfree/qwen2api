package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// Agent 模式的真实帧与请求体（2026-11-08 一手抓包，见 docs/UPSTREAM.md §10）。
//
// 测试的价值全在"形态不是编的"：这些字符串是从上游 SSE 与请求体里逐字抄下来的，
// 上游一改形态，这里就会先红。
const (
	// think：思考**原文增量**（不是 thinking_summary 的累加数组）。
	agentFrameThink    = `{"choices":[{"delta":{"role":"assistant","content":"The user just sent \"1\"","phase":"think","status":"typing"}}],"response_id":"d128f90d","usage":{},"timestamp":1794143183}`
	agentFrameThinkFin = `{"choices":[{"delta":{"role":"assistant","content":"","phase":"think","status":"finished"}}],"response_id":"d128f90d","usage":{},"timestamp":1794143184}`

	// tool_call 中间帧：arguments 是**增量拼接**（image_gen_tool 是全量快照，方向相反）。
	agentFrameToolTyping = `{"choices":[{"delta":{"role":"assistant","content":"","phase":"tool_call","status":"typing","function_call":{"name":"bash","arguments":"{\"command\": \"cat"},"function_id":"round_1_call_37749abba5d54e7c","extra":{"display_position":"shell"}}}],"response_id":"f782fa6a","usage":{},"timestamp":1794143330}`

	// tool_call 结束帧：arguments 是**完整 JSON**，且 extra.tool_result 是服务端已执行的结果。
	agentFrameToolFin = `{"choices":[{"delta":{"role":"tool","content":"","phase":"tool_call","status":"finished","function_id":"round_1_call_37749abba5d54e7c","function_call":{"name":"bash","arguments":"{\"command\":\"cd /home/user && cat hello.py\"}"},"extra":{"function_id":"round_1_call_37749abba5d54e7c","tool_result":{"status":"completed","stderr":"","stdout":"hello\n","exit_code":0,"duration_ms":29},"display_position":"shell"}}}],"response_id":"f782fa6a","usage":{},"timestamp":1794143330}`

	// answer 中间帧：usage 恒为 {}（Agent 模式不在这里给 usage）。
	agentFrameAnswerTyping = `{"choices":[{"delta":{"role":"assistant","content":"4","phase":"answer","status":"typing","extra":{"display_position":"answer"}}}],"response_id":"f782fa6a","usage":{},"timestamp":1794143330}`

	// answer 结束帧：真 usage 在 extra.model_service_info（顶层还是空 {}）。
	agentFrameAnswerFin = `{"choices":[{"delta":{"role":"assistant","content":"","phase":"answer","status":"finished","extra":{"display_position":"answer","model_service_info":{"usage":{"prompt_tokens":14605,"output_tokens":142,"total_tokens":14747,"output_tokens_details":{"reasoning_tokens":101,"text_tokens":142}},"usage_scope":"task","task_usage":{"scope":"task","usage":{"prompt_tokens":14605,"output_tokens":142,"total_tokens":14747}},"run_id":"df825fa2"},"file_list":[],"arena_settlement":{"task_id":"f782fa6a","status":"settled","retryable":false}}}}],"response_id":"f782fa6a","usage":{},"timestamp":1794143330}`

	// ask_user：交互式工具，结束帧没有 tool_result（API 场景无解，见 §10.8）。
	agentFrameAskUser = `{"choices":[{"delta":{"role":"assistant","content":"","phase":"tool_call","status":"finished","function_call":{"name":"ask_user","arguments":"{\"questions\":[{\"id\":\"intent\",\"question\":\"What would you like me to help you with?\",\"allowCustomResponse\":true}]}"},"function_id":"round_0_call_7f0d81caf9c64aea","extra":{"display_position":"shell","tool_call_id":"call_7f0d81caf9c64aea"}}}],"response_id":"d128f90d","usage":{},"timestamp":1794143186}`
)

func mustJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	return m
}

// Agent 的提交体必须逐字对齐 §10.4：三个类型位点 + 5 键 feature_config。
func TestAgentSubmitBodyShape(t *testing.T) {
	body := BuildSubmitSpec("chat-1", "", "qwen3.8-flash", "1", "thinking", AgentSpec(), mustTime())
	m := mustJSON(t, body)

	if m["chat_mode"] != "normal" || m["version"] != SubmitVersion {
		t.Fatalf("顶层字段不对: %v", m)
	}
	msgs, ok := m["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages 形态不对: %v", m["messages"])
	}
	msg := msgs[0].(map[string]any)

	if msg["chat_type"] != AgentChatType {
		t.Fatalf("chat_type 应为 %q，得到 %v", AgentChatType, msg["chat_type"])
	}
	if msg["sub_chat_type"] != AgentSubChatType {
		t.Fatalf("sub_chat_type 应为 %q，得到 %v", AgentSubChatType, msg["sub_chat_type"])
	}
	meta, _ := msg["extra"].(map[string]any)["meta"].(map[string]any)
	if meta["subChatType"] != AgentSubChatType {
		t.Fatalf("extra.meta.subChatType 应为 %q，得到 %v", AgentSubChatType, meta["subChatType"])
	}

	// 🔴 5 个键：不带 auto_search、不带 thinking_format（§10.4）。
	fc, _ := msg["feature_config"].(map[string]any)
	if len(fc) != 5 {
		t.Fatalf("Agent 的 feature_config 必须只有 5 个键，得到 %d 个: %v", len(fc), fc)
	}
	if _, has := fc["auto_search"]; has {
		t.Fatal("Agent 的 feature_config 不该带 auto_search")
	}
	if _, has := fc["thinking_format"]; has {
		t.Fatal("Agent 的 feature_config 不该带 thinking_format")
	}
	if fc["thinking_mode"] != "Fast" || fc["thinking_enabled"] != false {
		t.Fatalf("Agent 固定 Fast 档，得到 %v", fc)
	}

	// gear 参数对 Agent 无效：传 thinking 也不该变成 Thinking 档。
	if fc["thinking_mode"] == "Thinking" {
		t.Fatal("Agent 模式没有思考档位，不该跟随 gear")
	}
}

// 普通 t2t 的 7 键 feature_config 不能因为 Agent 的改动而丢掉 auto_search。
func TestTextSubmitKeepsAutoSearch(t *testing.T) {
	body := BuildSubmitSpec("chat-1", "", "qwen3.7-plus", "hi", "auto", TextSpec(), mustTime())
	m := mustJSON(t, body)
	msg := m["messages"].([]any)[0].(map[string]any)
	fc, _ := msg["feature_config"].(map[string]any)
	if fc["auto_search"] != true {
		t.Fatalf("t2t 的 auto_search 必须仍为 true（omitempty 不能误伤）: %v", fc)
	}
	if fc["thinking_mode"] != "Auto" {
		t.Fatalf("t2t auto 档应为 Auto，得到 %v", fc["thinking_mode"])
	}
	if msg["chat_type"] != "t2t" {
		t.Fatalf("t2t 的 chat_type 被改了: %v", msg["chat_type"])
	}
}

// Agent 的建会话体：无 title、带 chatId:""（§10.3）。
func TestAgentNewChatBodyShape(t *testing.T) {
	raw, err := json.Marshal(BuildNewChatTyped("qwen3.8-flash", AgentChatType, mustTime()))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	got := string(raw)

	if strings.Contains(got, `"title"`) {
		t.Fatalf("Agent 的建会话体不该带 title: %s", got)
	}
	if !strings.Contains(got, `"chatId":""`) {
		t.Fatalf("Agent 的建会话体应带空 chatId: %s", got)
	}
	if !strings.Contains(got, `"chat_type":"agent_mode"`) {
		t.Fatalf("chat_type 不对: %s", got)
	}

	// 反向：t2t 不该出现 chatId（那个键是 Agent 独有的）。
	raw2, _ := json.Marshal(BuildNewChatTyped("qwen3.7-plus", "t2t", mustTime()))
	if strings.Contains(string(raw2), `"chatId"`) {
		t.Fatalf("t2t 建会话体不该带 chatId: %s", raw2)
	}
	if !strings.Contains(string(raw2), `"title":"New Chat"`) {
		t.Fatalf("t2t 建会话体应保留 title: %s", raw2)
	}
}

// think 是原文增量，不是累加数组 —— 不能走 diffReasoning。
func TestParserAgentThink(t *testing.T) {
	p := NewParser()
	ev, ok, err := p.Parse([]byte(agentFrameThink))
	if err != nil || !ok {
		t.Fatalf("think 帧应被接受: ok=%v err=%v", ok, err)
	}
	if ev.Reasoning != `The user just sent "1"` {
		t.Fatalf("think 的思考原文未透出: %q", ev.Reasoning)
	}
	if ev.Text != "" {
		t.Fatalf("think 帧不该产出正文: %q", ev.Text)
	}
	// 两帧相同内容不得被当"累加数组"去重（那是 thinking_summary 的逻辑）。
	ev2, _, _ := p.Parse([]byte(agentFrameThink))
	if ev2.Reasoning != `The user just sent "1"` {
		t.Fatalf("think 是增量，重复帧也应原样透出: %q", ev2.Reasoning)
	}

	ev3, _, _ := p.Parse([]byte(agentFrameThinkFin))
	if ev3.Reasoning != "" {
		t.Fatalf("think finished 不该产出思考文本: %q", ev3.Reasoning)
	}
	if ev3.Done {
		t.Fatal("think finished 不是整条流的结束（结束判据是 answer finished）")
	}
}

// tool_call：中间帧只给名字，结束帧给完整参数 + 服务端执行结果。
func TestParserAgentToolCall(t *testing.T) {
	p := NewParser()

	ev, ok, err := p.Parse([]byte(agentFrameToolTyping))
	if err != nil || !ok {
		t.Fatalf("tool_call 中间帧应被接受: ok=%v err=%v", ok, err)
	}
	if ev.ToolName != "bash" {
		t.Fatalf("工具名未提取: %q", ev.ToolName)
	}
	if ev.ToolArgs != "" {
		t.Fatalf("中间帧的 arguments 是增量，不该当完整参数: %q", ev.ToolArgs)
	}
	if ev.Done {
		t.Fatal("tool_call 中间帧不是流结束")
	}

	ev2, _, _ := p.Parse([]byte(agentFrameToolFin))
	if ev2.ToolArgs != `{"command":"cd /home/user && cat hello.py"}` {
		t.Fatalf("结束帧应给出完整参数: %q", ev2.ToolArgs)
	}
	if !strings.Contains(ev2.ToolResult, "completed") ||
		!strings.Contains(ev2.ToolResult, "exit=0") ||
		!strings.Contains(ev2.ToolResult, "stdout=hello") {
		t.Fatalf("tool_result 摘要不对: %q", ev2.ToolResult)
	}
	if ev2.Done {
		t.Fatal("tool_call finished 不是整条流的结束")
	}

	// ask_user 没有 tool_result：不能报错、也不能伪造结果。
	ev3, ok3, err3 := p.Parse([]byte(agentFrameAskUser))
	if err3 != nil || !ok3 {
		t.Fatalf("ask_user 帧应被接受而非丢弃: ok=%v err=%v", ok3, err3)
	}
	if ev3.ToolName != "ask_user" {
		t.Fatalf("ask_user 工具名未提取: %q", ev3.ToolName)
	}
	if ev3.ToolResult != "" {
		t.Fatalf("ask_user 没有执行结果，不该编一个: %q", ev3.ToolResult)
	}
}

// usage：顶层恒空，真值在 extra.model_service_info；且不能覆盖正常模式的 usage。
func TestParserAgentUsageFallback(t *testing.T) {
	p := NewParser()

	// 中间帧：usage 是 {} ⇒ 不得当成"有 usage 且为 0"。
	ev, _, _ := p.Parse([]byte(agentFrameAnswerTyping))
	if ev.Usage != nil {
		t.Fatalf("空 usage 不该被采纳（上游每帧都发 {}）: %+v", ev.Usage)
	}
	if ev.Text != "4" {
		t.Fatalf("answer 正文未透出: %q", ev.Text)
	}

	ev2, _, _ := p.Parse([]byte(agentFrameAnswerFin))
	if !ev2.Done {
		t.Fatal("answer finished 必须判定为流结束")
	}
	if ev2.Usage == nil {
		t.Fatal("最后一帧的 model_service_info.usage 未被兜底读取")
	}
	if ev2.Usage.InputTokens != 14605 || ev2.Usage.OutputTokens != 142 {
		t.Fatalf("usage 数值不对: %+v", ev2.Usage)
	}
	if ev2.Usage.OutputTokensDetails.ReasoningTokens != 101 {
		t.Fatalf("思考 token 未透出: %+v", ev2.Usage.OutputTokensDetails)
	}
}

// 普通模式的 usage（顶层）不能被兜底逻辑改走。
func TestParserTextUsageUnaffected(t *testing.T) {
	p := NewParser()
	ev, _, _ := p.Parse([]byte(frameAnswer))
	if ev.Usage == nil || ev.Usage.InputTokens != 1239 {
		t.Fatalf("t2t 顶层 usage 未采纳: %+v", ev.Usage)
	}
}

// Agent 并发槽满（2026-11-08 线上实测）：同一个号同时只能跑一个 agent 任务。
//
// 这条必须被认出来 —— 否则归 KindUnknown、retrySafe=false，客户端直接吃 502，
// 而上游其实只是说"稍等"。
func TestAgentBusyIsRetryable(t *testing.T) {
	raw := `{"error":{"code":"user.too_many_concurrent","details":"当前运行的任务数已达上限，请等待其他任务完成，或升级订阅以运行更多任务。"}}`
	_, _, err := NewParser().Parse([]byte(raw))
	if err == nil {
		t.Fatal("并发槽满应判为错误")
	}
	if !IsAgentBusy(err) {
		t.Fatalf("未识别为并发槽满: %v", err)
	}
	if !RetrySafe(err) {
		t.Fatalf("并发槽满是明确未受理，必须可安全重试: %v", err)
	}
	if KindOf(err) != KindRateLimit {
		t.Fatalf("应归 rate_limit（软冷却），得到 %v", KindOf(err))
	}

	// 字符串形态（上游另一种写法）
	raw2 := `{"error":"user.too_many_concurrent: 当前运行的任务数已达上限"}`
	_, _, err2 := NewParser().Parse([]byte(raw2))
	if !IsAgentBusy(err2) || !RetrySafe(err2) {
		t.Fatalf("字符串形态未识别: %v", err2)
	}

	// 普通错误不能被误判
	raw3 := `{"error":{"code":"invalid_input","details":"bad"}}`
	_, _, err3 := NewParser().Parse([]byte(raw3))
	if IsAgentBusy(err3) {
		t.Fatal("普通错误被误判成并发槽满")
	}
	if RetrySafe(err3) {
		t.Fatal("普通错误不该被判为可重试")
	}
}
