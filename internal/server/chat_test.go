package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 真实帧序列（2026-10-28 抓包）—— 集成测试用它当"上游"。
var realFrames = []string{
	`{"response.created":{"chat_id":"chat-1","parent_id":"","response_id":"resp-1","response_index":"0"}}`,
	`{"choices":[{"delta":{"role":"assistant","content":"","phase":"thinking_summary","extra":{"summary_title":{"content":["解析问题"]},"summary_thought":{"content":["先理解用户意图。"]}},"status":"typing"}}],"response_id":"resp-1","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"output_tokens_details":{"reasoning_tokens":15,"text_tokens":5}}}`,
	`{"choices":[{"delta":{"role":"assistant","content":"","phase":"thinking_summary","status":"finished"}}],"response_id":"resp-1"}`,
	`{"choices":[{"delta":{"role":"assistant","content":"你","phase":"answer","status":"typing"}}],"response_id":"resp-1","usage":{"input_tokens":100,"output_tokens":25,"total_tokens":125,"output_tokens_details":{"reasoning_tokens":15,"text_tokens":10}}}`,
	`{"choices":[{"delta":{"role":"assistant","content":"好","phase":"answer","status":"typing"}}],"response_id":"resp-1"}`,
	`{"choices":[{"delta":{"content":"","role":"assistant","status":"finished","phase":"answer"}}],"response_id":"resp-1"}`,
}

// fakeUpstream 假上游：按路径返回建会话 / SSE 流；failFirst 指定前 N 次调用返回 401。
type fakeUpstream struct {
	srv       *httptest.Server
	calls     int
	failFirst int
	gotSubmit map[string]any
	// submits 按顺序记录每一次 completions 请求体（n>1 会多次）。
	submits []map[string]any
	// gotSTS 记录 getstsToken 的请求体（图片/文件上传链的第一步）。
	gotSTS []map[string]any
}

func newFakeUpstream(t *testing.T, failFirst int) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{failFirst: failFirst}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if f.failFirst > 0 {
			f.failFirst--
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Unauthorized","details":"t"}}`))
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/api/v2/files/getstsToken"):
			// 上传链的第一步：这里返回 500，用来断言"图片确实进了上传链"。
			// （第二步 OSS PUT 的 host 由 STS 决定，httptest 接管不了，见 imageinput_test.go）
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.gotSTS = append(f.gotSTS, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"success":false,"data":{"code":"InternalError","details":"test"}}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/chats/new"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"request_id":"r","data":{"id":"chat-1"}}`))
		case strings.Contains(r.URL.Path, "/api/v2/chat/completions"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.gotSubmit = body
			f.submits = append(f.submits, body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			for _, fr := range realFrames {
				fmt.Fprintf(w, "data: %s\n\n", fr)
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestServer(t *testing.T, up *fakeUpstream, accounts int, apiKeys []string) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.APIKeys = apiKeys
	cfg.StateFile = ""
	cfg.Accounts = nil
	for i := 0; i < accounts; i++ {
		cfg.Accounts = append(cfg.Accounts, config.Account{
			Name:  fmt.Sprintf("a%d", i),
			Token: fmt.Sprintf("tok-%d", i),
		})
	}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	s := New(cfg, p, cli, reqlog.New(10), "test")
	return s.Handler()
}

func TestChatStreamEndToEnd(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.7-plus","stream":true,"thinking":"auto",
		"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 不对: %q", ct)
	}

	// 1) 首帧必须带 role（OpenAI 客户端依赖它初始化消息）。
	if !strings.Contains(out, `"delta":{"role":"assistant"}`) {
		t.Fatalf("缺角色首帧:\n%s", out)
	}
	// 2) 思考摘要走 reasoning_content（不是混进 content）。
	if !strings.Contains(out, `"reasoning_content":"【解析问题】\n先理解用户意图。"`) {
		t.Fatalf("思考内容未透出:\n%s", out)
	}
	// 3) 正文两段增量都在。
	if !strings.Contains(out, `"content":"你"`) || !strings.Contains(out, `"content":"好"`) {
		t.Fatalf("正文增量丢失:\n%s", out)
	}
	// 4) 收尾帧。
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("缺 finish_reason:\n%s", out)
	}
	// 5) usage 透出（字段名必须是 OpenAI 的 prompt/completion）。
	if !strings.Contains(out, `"prompt_tokens":100`) || !strings.Contains(out, `"completion_tokens":25`) {
		t.Fatalf("usage 未透出或字段名不对:\n%s", out)
	}
	if !strings.Contains(out, `"reasoning_tokens":15`) {
		t.Fatalf("思考 token 未透出:\n%s", out)
	}
	// 6) [DONE] 收流。
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("缺 [DONE]:\n%s", out)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices 数量: %d", len(resp.Choices))
	}
	if got := resp.Choices[0].Message.Content; got != "你好" {
		t.Fatalf("正文聚合错误: %q", got)
	}
	if got := resp.Choices[0].Message.ReasoningContent; !strings.Contains(got, "先理解用户意图") {
		t.Fatalf("思考聚合错误: %q", got)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 100 {
		t.Fatalf("usage 缺失: %+v", resp.Usage)
	}
	// 上游请求体必须带上双写 chatId 与空 parentId（回归：少一个就 400）。
	if up.gotSubmit["chatId"] != "chat-1" || up.gotSubmit["chat_id"] != "chat-1" {
		t.Fatalf("chatId 未双写: %+v", up.gotSubmit)
	}
	if up.gotSubmit["version"] != "2.1" {
		t.Fatalf("请求体 version 应为 2.1: %v", up.gotSubmit["version"])
	}
}

func TestChatRotatesAccountOn401(t *testing.T) {
	// 第一个号被打回 401 ⇒ 应换第二个号重试并成功。
	up := newFakeUpstream(t, 1) // 第一个号的第一次调用被打回 401
	h := newTestServer(t, up, 2, nil)

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("换号后应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("换号后正文缺失: %s", rec.Body.String())
	}
}

func TestChatNoAccountReturns503(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 0, nil)

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无账号应 503，得到 %d", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, []string{"sk-good"})

	// 无 key
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401，得到 %d", rec.Code)
	}
	// 错 key
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-bad")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 key 应 401，得到 %d", rec.Code)
	}
	// 对 key
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-good")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("对 key 应 200，得到 %d", rec.Code)
	}
}

func TestMessagesStreamEvents(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.7-plus","max_tokens":100,"stream":true,
		"messages":[{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start", "event: content_block_delta",
		"event: content_block_stop", "event: message_delta", "event: message_stop",
		`"type":"text_delta"`, `"text":"你"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Anthropic 事件序列缺 %q:\n%s", want, out)
		}
	}
}

// newTestServerWithServer 返回 *Server（需要直接摸 store 的测试用）。
func newTestServerWithServer(t *testing.T, up *fakeUpstream, accounts int) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = nil
	for i := 0; i < accounts; i++ {
		cfg.Accounts = append(cfg.Accounts, config.Account{
			Name:  fmt.Sprintf("a%d", i),
			Token: fmt.Sprintf("tok-%d", i),
		})
	}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	return New(cfg, p, cli, reqlog.New(10), "test")
}
