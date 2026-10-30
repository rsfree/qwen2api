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

// ---------- 断流后的两条活路 ----------
//
// 线上样本（2026-10-31 img-ace1f950）：25.8s 时 `chat 流读取: unexpected EOF`，
// 返回 502 + 零张。但断流 ≠ 没画成 —— 这里把两种可救的情况钉死：
//  1. 图片帧在断点**之前**已经到达 ⇒ 必须按成功返回（早先被 return nil 丢掉了）；
//  2. 一张都没到 ⇒ 图可能已落进会话历史，补捞回来（别让调用方白花钱重画）。

// eofUpstream 假上游：发完指定帧后直接掐断 chunked 流（模拟 unexpected EOF）。
type eofUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	// partial=true：先发一帧完整图片帧再断流；false：只发 created 就断。
	partial bool
	// history 会话历史里"最新一条带图回复"的图片（空 = 历史里也没有）。
	history []string
	// historyCalls 历史端点被调用次数（用来断言"该不该补捞"）。
	historyCalls int
}

func newEOFUpstream(t *testing.T, partial bool, history ...string) *eofUpstream {
	t.Helper()
	f := &eofUpstream{partial: partial, history: history}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v2/chats/new"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chat-1"}}`))

		case strings.Contains(r.URL.Path, "/api/v2/chat/completions"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, ok := w.(http.Flusher)
			if !ok {
				t.Error("假上游需要 Flusher")
				return
			}
			_, _ = w.Write([]byte(`data: {"response.created":{"chat_id":"chat-1","parent_id":"","response_id":"r1","response_index":"0"}}` + "\n\n"))
			if f.partial {
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"https://cdn.example/stream.png?key=k1","phase":"image_gen","status":"typing"}}]}` + "\n\n"))
			}
			fl.Flush()
			// 🔴 掐断连接而不是正常 return：后者会被当成"流正常结束"，
			// 而线上那条 502 的形态是 unexpected EOF（chunked 没收尾）。
			panic(http.ErrAbortHandler)

		case strings.Contains(r.URL.Path, "/api/v2/chats/chat-1"):
			f.mu.Lock()
			f.historyCalls++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			items := make([]string, 0, len(f.history))
			for _, u := range f.history {
				items = append(items, `{"content":"`+u+`","phase":"image_gen","status":"finished"}`)
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"chat":{"history":{"messages":{` +
				`"m1":{"role":"assistant","timestamp":1,"content_list":[` + strings.Join(items, ",") + `]}` +
				`}}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *eofUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.historyCalls
}

// newImageServerFor 用 eofUpstream 搭一个最小网关（单账号）。
func newImageServerFor(t *testing.T, up *eofUpstream) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	cfg.ImageModel = "qwen-image-3.0"

	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	return New(cfg, p, cli, reqlog.New(10), "test").Handler()
}

func postImage(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat"}`)))
	return rec
}

// 断点之前已收到图片帧 ⇒ 200 + 那张图，且**不该**再去查历史（白花一次往返）。
func TestImageStreamKeepsImageDeliveredBeforeEOF(t *testing.T) {
	up := newEOFUpstream(t, true)
	h := newImageServerFor(t, up)

	rec := postImage(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("断流前已拿到图，应返回 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp ImageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].URL != "https://cdn.example/stream.png?key=k1" {
		t.Fatalf("图被丢了: %+v", resp.Data)
	}
	if n := up.calls(); n != 0 {
		t.Fatalf("已有产物时不该再查历史，实际调了 %d 次", n)
	}
}

// 断点时一张都没有 ⇒ 从会话历史补回。
func TestImageStreamRecoversFromHistoryAfterEOF(t *testing.T) {
	up := newEOFUpstream(t, false, "https://cdn.example/recovered.png?key=k2")
	h := newImageServerFor(t, up)

	rec := postImage(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("补捞成功应返回 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp ImageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].URL != "https://cdn.example/recovered.png?key=k2" {
		t.Fatalf("应从历史补回图片，得到 %+v", resp.Data)
	}
	if up.calls() == 0 {
		t.Fatal("没有调用过历史端点")
	}
}

// 历史里也没有 ⇒ 保持失败（绝不返回空 data 假装成功）。
func TestImageStreamFailsWhenHistoryHasNoImage(t *testing.T) {
	up := newEOFUpstream(t, false)
	h := newImageServerFor(t, up)

	rec := postImage(t, h)
	if rec.Code == http.StatusOK {
		t.Fatalf("没有任何产物时不能返回 200: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("应返回错误体，实际: %s", rec.Body.String())
	}
}
