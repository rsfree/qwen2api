package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Agent 模型是**网关侧的别名**：上游只认 qwen3.8-max / qwen3.8-flash（§10.2）。
func TestResolveAgentModel(t *testing.T) {
	cases := []struct {
		in   string
		base string
		ok   bool
	}{
		{"qwen3.8-max-agent", "qwen3.8-max", true},
		{"qwen3.8-flash-agent", "qwen3.8-flash", true},
		{"Qwen3.8-Flash-Agent", "qwen3.8-flash", true},
		{"  qwen3.8-max-agent  ", "qwen3.8-max", true},
		{"qwen3.8-max", "", false},
		{"qwen3.8-omni-flash", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		base, ok := resolveAgentModel(c.in)
		if base != c.base || ok != c.ok {
			t.Fatalf("resolveAgentModel(%q) = (%q,%v)，期望 (%q,%v)", c.in, base, ok, c.base, c.ok)
		}
	}
}

// 端到端：model=qwen3.8-max-agent 必须落成 agent_mode 会话 + 底座模型。
func TestAgentModelRoutesToAgentMode(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-max-agent","stream":true,
		"messages":[{"role":"user","content":"1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, rec.Body.String())
	}

	// chats/new：Agent 形态（无 title、带空 chatId、chat_type=agent_mode）。
	if up.gotNewChat == nil {
		t.Fatal("没抓到 chats/new 请求体")
	}
	if up.gotNewChat["chat_type"] != "agent_mode" {
		t.Fatalf("建会话 chat_type 应为 agent_mode，得到 %v", up.gotNewChat["chat_type"])
	}
	if _, has := up.gotNewChat["title"]; has {
		t.Fatalf("Agent 建会话不该带 title: %v", up.gotNewChat)
	}
	if v, has := up.gotNewChat["chatId"]; !has || v != "" {
		t.Fatalf("Agent 建会话应带空 chatId，得到 %v（存在=%v）", v, has)
	}

	// completions：底座模型名 + agent 类型位点。
	if up.gotSubmit == nil {
		t.Fatal("没抓到 completions 请求体")
	}
	if up.gotSubmit["model"] != "qwen3.8-max" {
		t.Fatalf("提交给上游的模型应为底座名 qwen3.8-max，得到 %v", up.gotSubmit["model"])
	}
	msgs, _ := up.gotSubmit["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 长度异常: %v", up.gotSubmit["messages"])
	}
	msg := msgs[0].(map[string]any)
	if msg["chat_type"] != "agent_mode" || msg["sub_chat_type"] != "lite" {
		t.Fatalf("类型位点不对: chat_type=%v sub_chat_type=%v", msg["chat_type"], msg["sub_chat_type"])
	}
	meta, _ := msg["extra"].(map[string]any)["meta"].(map[string]any)
	if meta["subChatType"] != "lite" {
		t.Fatalf("extra.meta.subChatType 应为 lite: %v", meta)
	}
	fc, _ := msg["feature_config"].(map[string]any)
	if len(fc) != 5 {
		t.Fatalf("Agent 的 feature_config 应只有 5 个键: %v", fc)
	}

	// 响应体里的 model 必须是**客户端传的名字**，不能变脸上游底座名。
	if !strings.Contains(rec.Body.String(), `"model":"qwen3.8-max-agent"`) {
		t.Fatalf("响应体 model 被改成了上游名: %s", rec.Body.String())
	}
}

// 端到端：Agent 的 think 走 reasoning_content，usage 从 model_service_info 兜底。
func TestAgentStreamThinkAndUsage(t *testing.T) {
	up := newFakeUpstream(t, 0)
	frames := []string{
		`{"response.created":{"chat_id":"c1","parent_id":"p1","response_id":"r1","response_index":"0"}}`,
		`{"choices":[{"delta":{"role":"assistant","content":"weighing options","phase":"think","status":"typing"}}],"usage":{},"timestamp":1}`,
		`{"choices":[{"delta":{"role":"assistant","content":"","phase":"think","status":"finished"}}],"usage":{},"timestamp":1}`,
		`{"choices":[{"delta":{"role":"assistant","content":"4","phase":"answer","status":"typing","extra":{"display_position":"answer"}}}],"usage":{},"timestamp":2}`,
		`{"choices":[{"delta":{"role":"assistant","content":"","phase":"answer","status":"finished","extra":{"display_position":"answer","model_service_info":{"usage":{"prompt_tokens":14605,"completion_tokens":142,"total_tokens":14747,"input_tokens":14605,"output_tokens":142},"usage_scope":"task"},"file_list":[],"arena_settlement":{"status":"settled"}}}}],"usage":{},"timestamp":3}`,
	}
	up.frames = frames
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-flash-agent","stream":true,
		"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", rec.Code, got)
	}
	// think 必须进 reasoning_content，且**不能**混进正文。
	if !strings.Contains(got, `"reasoning_content":"weighing options"`) {
		t.Fatalf("Agent 的 think 未映射到 reasoning_content: %s", got)
	}
	if strings.Contains(got, `"content":"weighing options"`) {
		t.Fatalf("思考原文被当成正文吐出去了: %s", got)
	}
	if !strings.Contains(got, `"content":"4"`) {
		t.Fatalf("正文缺失: %s", got)
	}
	// usage：顶层是空的，真值在 model_service_info。
	if !strings.Contains(got, `"prompt_tokens":14605`) {
		t.Fatalf("usage 未从 model_service_info 兜底: %s", got)
	}
	if !strings.Contains(got, `"reasoning_tokens"`) && !strings.Contains(got, `"completion_tokens":142`) {
		t.Fatalf("usage 明细缺失: %s", got)
	}
}

// 非 Agent 的普通模型不能因为上面这层判断被改道。
func TestPlainModelStaysT2T(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.7-plus","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if up.gotSubmit["model"] != "qwen3.7-plus" {
		t.Fatalf("普通模型名被改了: %v", up.gotSubmit["model"])
	}
	msgs := up.gotSubmit["messages"].([]any)
	msg := msgs[0].(map[string]any)
	if msg["chat_type"] != "t2t" || msg["sub_chat_type"] != "t2t" {
		t.Fatalf("普通模型被改道成 %v/%v", msg["chat_type"], msg["sub_chat_type"])
	}
}
