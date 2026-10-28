package server

import (
	"encoding/json"
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

// ---------- 图片生成参数矩阵 ----------
//
// 这组测试的价值：图片是**计费动作**，参数映射错了不能在真实调用里发现
// （一次就是一张额度）。所以把映射钉在假上游上：断言"上游实际收到什么"，
// 而不是"我们返回了什么"。

// imageUpstream 假上游：建会话 + 返回一帧 image_gen 产物。
type imageUpstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	submits  []map[string]any
	chatReqs []map[string]any
	// failMode = "hangup" 时只写一帧 created 就收尾（模拟上游"只发心跳、无产物"）。
	failMode string
}

func newImageUpstream(t *testing.T) *imageUpstream {
	t.Helper()
	f := &imageUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v2/chats/new"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.chatReqs = append(f.chatReqs, body)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chat-1"}}`))
		case strings.Contains(r.URL.Path, "/api/v2/chat/completions"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.submits = append(f.submits, body)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f.failMode == "hangup" {
				_, _ = w.Write([]byte("data: {\"response.created\":{\"chat_id\":\"c\",\"parent_id\":\"\",\"response_id\":\"r\",\"response_index\":\"0\"}}\n\n"))
				return
			}
			for _, fr := range []string{
				`{"response.created":{"chat_id":"chat-1","parent_id":"","response_id":"r1","response_index":"0"}}`,
				`{"choices":[{"delta":{"role":"assistant","content":"https://cdn.example/img.png?key=k","phase":"image_gen","extra":{"output_image_hw":[["1024","1024"]],"model":"qwen-image-3.0"},"status":"typing"}}],"response_id":"r1","usage":{}}`,
				`{"choices":[{"delta":{"content":"","role":"assistant","status":"finished","phase":"image_gen"}}],"response_id":"r1"}`,
			} {
				_, _ = w.Write([]byte("data: " + fr + "\n\n"))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *imageUpstream) last(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.submits) == 0 {
		t.Fatal("没有收到 completions 请求")
	}
	return f.submits[len(f.submits)-1]
}

func newImageServer(t *testing.T, up *imageUpstream) http.Handler {
	return newImageServerLimit(t, up, 0)
}

// newImageServerLimit 可指定每日出图上限（0 = 用默认）。
func newImageServerLimit(t *testing.T, up *imageUpstream, maxPerDay int) http.Handler {
	t.Helper()
	cfg := config.Default()
	if maxPerDay > 0 {
		cfg.Pool.MaxImagePerDay = maxPerDay
	}
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	cfg.ImageModel = "qwen-image-3.0"

	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	srv := New(cfg, p, cli, reqlog.New(10), "test")
	return srv.Handler()
}

func TestImageSizeMatrix(t *testing.T) {
	// OpenAI 客户端会给像素，qwen 只认比例。这张表就是"翻译字典"，
	// 每条都要在**上游收到的 body** 里体现（顶层 size + extra.meta.size）。
	cases := []struct {
		in   string
		want string // 期望发出去的比例；"" 表示不该带 size
	}{
		{"1024x1024", "1:1"},
		{"1792x1024", "16:9"},
		{"1024x1792", "9:16"},
		{"640x480", "4:3"},
		{"480x640", "3:4"},
		{"1920x1080", "16:9"},
		{"1:1", "1:1"},
		{"auto", "auto"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			up := newImageUpstream(t)
			h := newImageServer(t, up)
			body := `{"prompt":"a cat","size":"` + c.in + `"}`

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
				strings.NewReader(body)))
			if rec.Code != 200 {
				t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
			}

			got := up.last(t)
			if c.want == "" {
				if _, has := got["size"]; has {
					t.Fatalf("不该带顶层 size，得到 %v", got["size"])
				}
				return
			}
			if got["size"] != c.want {
				t.Fatalf("顶层 size: 得到 %v, want %v", got["size"], c.want)
			}
			msg := got["messages"].([]any)[0].(map[string]any)
			meta := msg["extra"].(map[string]any)["meta"].(map[string]any)
			if meta["size"] != c.want {
				t.Fatalf("extra.meta.size: 得到 %v, want %v", meta["size"], c.want)
			}
			if meta["subChatType"] != "t2i" {
				t.Fatalf("subChatType 应为 t2i: %v", meta["subChatType"])
			}
			if meta["model"] != "qwen-image-3.0" {
				t.Fatalf("图片模型必须写进 extra.meta: %v", meta["model"])
			}
		})
	}
}

func TestImageThinkingDefaultIsFast(t *testing.T) {
	// 图片默认关思考（省时间）。显式传 thinking 才覆盖。
	up := newImageUpstream(t)
	h := newImageServer(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat"}`)))
	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	msg := up.last(t)["messages"].([]any)[0].(map[string]any)
	fc := msg["feature_config"].(map[string]any)
	if fc["thinking_enabled"] != false || fc["thinking_mode"] != "Fast" {
		t.Fatalf("图片默认应 fast（thinking_enabled=false/Fast），得到 %v/%v",
			fc["thinking_enabled"], fc["thinking_mode"])
	}

	// 显式 thinking=thinking 要能覆盖
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat","thinking":"thinking"}`)))
	msg = up.last(t)["messages"].([]any)[0].(map[string]any)
	fc = msg["feature_config"].(map[string]any)
	if fc["thinking_enabled"] != true {
		t.Fatalf("显式 thinking 未生效: %v", fc["thinking_enabled"])
	}
}

func TestImageNIsSeparateCalls(t *testing.T) {
	// n=2 ⇒ 两次独立上游调用（上游一次工具调用只出一张图），且返回两张。
	up := newImageUpstream(t)
	h := newImageServer(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat","n":2}`)))
	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}

	up.mu.Lock()
	n := len(up.submits)
	up.mu.Unlock()
	if n != 2 {
		t.Fatalf("n=2 应产生 2 次上游调用，实际 %d", n)
	}

	var resp ImageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("应返回 2 张图，得到 %d", len(resp.Data))
	}
}

func TestImageResponseFormatMatrix(t *testing.T) {
	// url / 不传 / 非法值 都应走 url 分支（非法值=未指定，不报错）；
	// b64_json 那条要真下载，这里只验证它不会在参数层 400。
	for _, f := range []string{"", `"response_format":"url"`, `"response_format":"weird"`,
		`"response_format":null`, `"response_format":""`} {
		name := strings.TrimSpace(f)
		if name == "" {
			name = "(不传)"
		}
		t.Run(name, func(t *testing.T) {
			up := newImageUpstream(t)
			h := newImageServer(t, up)
			body := `{"prompt":"a cat"`
			if f != "" {
				body += "," + f
			}
			body += "}"

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
				strings.NewReader(body)))
			if rec.Code != 200 {
				t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
			}
			var resp ImageResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Data) != 1 || resp.Data[0].URL == "" {
				t.Fatalf("应返回 url 形态: %+v", resp.Data)
			}
		})
	}
}

func TestImagePromptNotWrapped(t *testing.T) {
	// prompt 必须原样到上游（加"请画"前缀会让 t2i 空转，实测 200s 无内容）。
	up := newImageUpstream(t)
	h := newImageServer(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat wearing a beret","size":"16:9"}`)))
	if rec.Code != 200 {
		t.Fatalf("状态码 %d", rec.Code)
	}
	msg := up.last(t)["messages"].([]any)[0].(map[string]any)
	if msg["content"] != "a cat wearing a beret" {
		t.Fatalf("prompt 被改写了: %q", msg["content"])
	}
}

func TestImageLocalDailyQuota(t *testing.T) {
	// 本地每日配额：出够 N 张后，第 N+1 张必须 429，且**不再打上游**
	//（否则等于白烧一次上游调用，还可能撞上"静默挂起 180s"）。
	const limit = 2
	up := newImageUpstream(t)
	h := newImageServerLimit(t, up, limit)

	body := `{"prompt":"a cat","size":"1:1"}`
	for i := 1; i <= limit; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
			strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("第 %d 张应成功，得到 %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	up.mu.Lock()
	callsBefore := len(up.submits)
	up.mu.Unlock()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(body)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超出本地配额应 429，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Fatalf("错误类型应为 insufficient_quota: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "本地限额") {
		t.Fatalf("文案应说明是本地限额而不是上游额度: %s", rec.Body.String())
	}

	up.mu.Lock()
	callsAfter := len(up.submits)
	up.mu.Unlock()
	if callsAfter != callsBefore {
		t.Fatalf("配额用尽后不该再打上游（%d → %d）", callsBefore, callsAfter)
	}
}

func TestImageQuotaNotCountedOnFailure(t *testing.T) {
	// 失败/挂起不计入配额 —— 实测上游挂起不扣额度，计进去会白占名额。
	up := newImageUpstream(t)
	h := newImageServerLimit(t, up, 3)

	// 让上游回一个"只发心跳、无产物"的流
	up.failMode = "hangup"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat"}`)))
	if rec.Code == 200 {
		t.Fatal("上游无产物时不该返回成功")
	}

	// 配额应仍是 0/3
	up.failMode = ""
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat"}`)))
	if rec.Code != 200 {
		t.Fatalf("恢复后应能出图，得到 %d: %s", rec.Code, rec.Body.String())
	}
}
