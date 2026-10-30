package server

// 生成模型（图片/视频）的清单与路由测试。
//
// 背景：上游没有独立的图片/视频模型 id —— 图片靠 extra.meta.model、
// 视频靠 biz_data.rootModel 选内核。网关对外把它们包装成模型清单里的条目，
// 所以"清单里列的名字"与"请求真的发到上游的字段"之间必须钉死：
// 列了却翻译不过去，等于给客户端一个必然失败的模型名。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// routingUpstream 记录网关发给上游的建会话 / 提交请求体。
type routingUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	// newChats 按顺序记录 /api/v2/chats/new 的请求体。
	newChats []map[string]any
	// submits 按顺序记录 /api/v2/chat/completions 的请求体。
	submits []map[string]any
}

func newRoutingUpstream(t *testing.T) *routingUpstream {
	t.Helper()
	f := &routingUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/entitlement/credits/credits/history"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"httpCode":200,"success":true,"data":{"totalCredits":1000,"subscriptionCredits":0,"bonusCredits":0}}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/chats/new"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.newChats = append(f.newChats, body)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"request_id":"r","data":{"id":"chat-1"}}`))
		case strings.Contains(r.URL.Path, "/api/v2/chat/completions"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.submits = append(f.submits, body)
			f.mu.Unlock()
			if chatTypeOf(body) == "t2v" {
				// 视频必须同步拿 task_id（stream=false）。
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"success":true,"data":{"messages":[{"extra":{"wanx":{"task_id":"tsk_test123"}}}]}}`))
				return
			}
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

func (f *routingUpstream) lastNewChat(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.newChats) == 0 {
		t.Fatal("没有收到建会话请求")
	}
	return f.newChats[len(f.newChats)-1]
}

func (f *routingUpstream) lastSubmit(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.submits) == 0 {
		t.Fatal("没有收到提交请求")
	}
	return f.submits[len(f.submits)-1]
}

// chatTypeOf 从提交体里取 messages[0].chat_type。
func chatTypeOf(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	m, _ := msgs[0].(map[string]any)
	s, _ := m["chat_type"].(string)
	return s
}

// metaOf 从提交体里取 messages[0].extra.meta。
func metaOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("提交体里没有 messages")
	}
	m, _ := msgs[0].(map[string]any)
	extra, _ := m["extra"].(map[string]any)
	meta, _ := extra["meta"].(map[string]any)
	if meta == nil {
		t.Fatalf("提交体里没有 extra.meta: %v", body)
	}
	return meta
}

func newRoutingServer(t *testing.T, up *routingUpstream) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.Upstream.AuthBaseURL = up.srv.URL
	cfg.Upstream.EntitlementBaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	return New(cfg, p, cli, reqlog.New(10), "test").Handler()
}

func doJSON(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

// TestModelsListIncludesMediaModels 清单必须同时有文本、图片、视频三类，
// 且生成模型要带 endpoint —— 否则客户端不知道该往哪个端点发。
func TestModelsListIncludesMediaModels(t *testing.T) {
	up := newRoutingUpstream(t)
	h := newRoutingServer(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models 应 200，得到 %d", rec.Code)
	}
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析清单失败: %v", err)
	}

	byID := map[string]ModelInfo{}
	for _, m := range out.Data {
		byID[m.ID] = m
	}
	img, ok := byID["qwen-image-3.0"]
	if !ok {
		t.Fatalf("清单缺少图片模型 qwen-image-3.0: %v", byID)
	}
	if img.Media != "image" || img.Endpoint != "/v1/images/generations" {
		t.Fatalf("图片模型条目不对: media=%q endpoint=%q", img.Media, img.Endpoint)
	}
	vid, ok := byID["happyhorse11"]
	if !ok {
		t.Fatalf("清单缺少视频模型 happyhorse11: %v", byID)
	}
	if vid.Media != "video" || vid.Endpoint != "/v1/videos/generations" {
		t.Fatalf("视频模型条目不对: media=%q endpoint=%q", vid.Media, vid.Endpoint)
	}
	if vid.Params["resolutions"] == nil {
		t.Fatalf("视频模型应带档位信息: %v", vid.Params)
	}
	// 文本模型不能被生成模型挤掉（上游拉不到时用内置清单兜底）。
	if _, ok := byID["qwen3.7-plus"]; !ok {
		t.Fatal("清单里丢了文本模型 qwen3.7-plus")
	}
}

// TestChatRejectsMediaModel 拿生成模型调 chat 要 400 + 指路，
// 而不是把模型名甩给上游换回一个看不懂的 Bad_Request。
func TestChatRejectsMediaModel(t *testing.T) {
	up := newRoutingUpstream(t)
	h := newRoutingServer(t, up)

	cases := []struct{ model, wantEndpoint string }{
		{"qwen-image-3.0", "/v1/images/generations"},
		{"Qwen-Image 3.0", "/v1/images/generations"}, // 上游 UI 的展示名
		{"happyhorse11", "/v1/videos/generations"},
		{"HappyHorse 1.1", "/v1/videos/generations"},
	}
	for _, c := range cases {
		rec := doJSON(h, "/v1/chat/completions",
			`{"model":`+fmt.Sprintf("%q", c.model)+`,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 调 chat 应 400，得到 %d: %s", c.model, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), c.wantEndpoint) {
			t.Fatalf("%s 的错误文案应指向 %s，实际: %s", c.model, c.wantEndpoint, rec.Body.String())
		}
	}
	// 全程不该打上游 —— 拦截的意义就是别白跑一次。
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.newChats) != 0 {
		t.Fatalf("被拦截的请求不该建会话，实际 %d 次", len(up.newChats))
	}
}

// TestImageModelSelectsKernel 传图片模型名时：
//   - 建会话仍用默认 chat 模型（上游模型表里没有图片模型 id）
//   - extra.meta.model 用传进来的图片内核
func TestImageModelSelectsKernel(t *testing.T) {
	up := newRoutingUpstream(t)
	h := newRoutingServer(t, up)

	// 用上游 UI 的展示名，验证归一化（Qwen-Image 3.0 → qwen-image-3.0）。
	rec := doJSON(h, "/v1/images/generations", `{"prompt":"一只猫","model":"Qwen-Image 3.0"}`)
	// 假上游不产 image_gen 帧，最终会 502；这里只关心发出去的字段。
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("不该 400: %s", rec.Body.String())
	}

	nc := up.lastNewChat(t)
	models, _ := nc["models"].([]any)
	if len(models) != 1 || models[0] != "qwen3.7-plus" {
		t.Fatalf("建会话应用默认 chat 模型，实际 %v", nc["models"])
	}
	if nc["chat_type"] != "t2i" {
		t.Fatalf("chat_type 应为 t2i，实际 %v", nc["chat_type"])
	}

	meta := metaOf(t, up.lastSubmit(t))
	if meta["model"] != "qwen-image-3.0" {
		t.Fatalf("extra.meta.model 应为 qwen-image-3.0，实际 %v", meta["model"])
	}
}

// TestImageChatModelKeepsDefaultKernel 老行为不能坏：
// 传 chat 模型（或不传）时，内核仍是配置的 image_model。
func TestImageChatModelKeepsDefaultKernel(t *testing.T) {
	up := newRoutingUpstream(t)
	h := newRoutingServer(t, up)

	doJSON(h, "/v1/images/generations", `{"prompt":"一只猫","model":"qwen3.6-plus"}`)

	nc := up.lastNewChat(t)
	models, _ := nc["models"].([]any)
	if len(models) != 1 || models[0] != "qwen3.6-plus" {
		t.Fatalf("建会话应保留请求里的 chat 模型，实际 %v", nc["models"])
	}
	meta := metaOf(t, up.lastSubmit(t))
	if meta["model"] != "qwen-image-3.0" {
		t.Fatalf("extra.meta.model 应回落配置值，实际 %v", meta["model"])
	}
}

// TestVideoModelSelectsRoot 传视频模型名时：
//   - 建会话用默认 chat 模型
//   - biz_data.rootModel 用传进来的内核
func TestVideoModelSelectsRoot(t *testing.T) {
	up := newRoutingUpstream(t)
	h := newRoutingServer(t, up)

	rec := doJSON(h, "/v1/videos/generations",
		`{"prompt":"一只猫","model":"HappyHorse 1.1","resolution":"720P","duration":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("视频提交应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	nc := up.lastNewChat(t)
	models, _ := nc["models"].([]any)
	if len(models) != 1 || models[0] != "qwen3.7-plus" {
		t.Fatalf("建会话应用默认 chat 模型，实际 %v", nc["models"])
	}
	if nc["chat_type"] != "t2v" {
		t.Fatalf("chat_type 应为 t2v，实际 %v", nc["chat_type"])
	}

	meta := metaOf(t, up.lastSubmit(t))
	biz, _ := meta["biz_data"].(string)
	if !strings.Contains(biz, `"rootModel":"happyhorse11"`) {
		t.Fatalf("biz_data.rootModel 应为 happyhorse11，实际 %s", biz)
	}
	if meta["subChatType"] != "zaodian_generate_video" {
		t.Fatalf("subChatType 不对: %v", meta["subChatType"])
	}
}

func TestSameModelNameNormalizes(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"qwen-image-3.0", "Qwen-Image 3.0", true},
		{"happyhorse11", "HappyHorse 1.1", true},
		{"qwen3.7-plus", "qwen3.7-plus", true},
		{"qwen3.7-plus", "qwen3.6-plus", false},
		{"", "", false}, // 空名不能互相"匹配"，否则空 model 会被当成生成模型
	}
	for _, c := range cases {
		if got := sameModelName(c.a, c.b); got != c.want {
			t.Fatalf("sameModelName(%q,%q)=%v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}
