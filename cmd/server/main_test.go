package main

// 启动探测的规模约束测试。
//
// 背景：面板导入 3000+ 账号后，每个账号的 token 都是 signin 产出的
// `bearer:xxx`。如果启动时对每个号都跑一次 DetectAuthMode（每种形态一次请求），
// 重启一次就是几千次上游调用 —— 而形态其实**已经写在凭据前缀里**。

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

func TestBootstrapSkipsProbeForPrefixedToken(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Upstream.BaseURL = srv.URL
	cfg.Upstream.AuthBaseURL = srv.URL
	cfg.Accounts = []config.Account{{Name: "a", Token: "bearer:tok"}}

	p := pool.New(cfg.Pool, cfg.Accounts, "")
	bootstrapAccounts(cfg, p, upstream.New(cfg.Upstream))

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("凭据已带形态前缀，不该再探测上游，实际打了 %d 次", got)
	}
}

func TestBootstrapProbesRawToken(t *testing.T) {
	// 没有前缀的裸 token 形态未知，仍必须探测一次（这里会失败，但不该崩）。
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Upstream.BaseURL = srv.URL
	cfg.Upstream.AuthBaseURL = srv.URL
	cfg.Accounts = []config.Account{{Name: "a", Token: "raw-token"}}

	p := pool.New(cfg.Pool, cfg.Accounts, "")
	bootstrapAccounts(cfg, p, upstream.New(cfg.Upstream))

	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Fatal("裸 token 形态未知，必须探测上游")
	}
}
