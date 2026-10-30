package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 零产物超时**不再换号重试**（2026-10-31 明确要求）。
//
// 理由：预算已经放宽到可配的 300s，再等一轮等于把调用方挂 10 分钟；
// 而"换一个号更快"只是猜测 —— 实测挂起是账号额度将尽的前兆，换号大概率同样挂。
// 所以超时只做两件事：冷却该号 + 如实返回 504。
//
// 这条测试用 1s 预算把超时逼出来，断言**上游只被调用一次**。
func TestImageTimeoutDoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v2/chats/new"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chat-1"}}`))

		case strings.Contains(r.URL.Path, "/api/v2/chat/completions"):
			atomic.AddInt32(&calls, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"response.created":{"chat_id":"chat-1","parent_id":"","response_id":"r1","response_index":"0"}}` + "\n\n"))
			w.(http.Flusher).Flush()
			// 只发心跳、永不产出：超时由网关这一侧判。
			<-r.Context().Done()

		case strings.Contains(r.URL.Path, "/api/v2/chats/chat-1"):
			// 让补捞立刻失败，免得测试多等 3s 的第二次查询。
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Not_Found","details":"gone"}}`))

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Upstream.BaseURL = srv.URL
	cfg.ImageTimeoutS = 1 // 秒；把 300s 的超时逼出来
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	cfg.ImageModel = "qwen-image-3.0"

	p := pool.New(cfg.Pool, cfg.Accounts, "")
	h := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test").Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat"}`)))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("零产物超时应回 504，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("超时不该换号重试，上游实际被调用 %d 次", n)
	}
}
