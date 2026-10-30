package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 调试转发的测试守两件事：
//  1. 它不是公开入口（只能经面板会话进来）；
//  2. 媒体代取有主机白名单（否则就是一台 SSRF 跳板）。

func TestAllowedMediaURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://cdn.qwenlm.ai/output/a/b.png?key=x", true},
		{"https://qwenlm.ai/a.png", true},
		{"https://CDN.QWENLM.AI/a.png", true},
		{"http://cdn.qwenlm.ai/a.png", false},           // 只许 https
		{"https://evil.com/a.png", false},               // 非白名单
		{"https://cdn.qwenlm.ai.evil.com/a.png", false}, // 后缀伪装
		{"https://evil@cdn.qwenlm.ai/a.png", false},     // userinfo 混淆
		{"https://cdn.qwenlm.ai:8443/a.png", true},      // 端口不影响主机判定
		{"https://127.0.0.1/a.png", false},              // 内网
		{"", false},
	}
	for _, c := range cases {
		if got := allowedMediaURL(c.in); got != c.want {
			t.Fatalf("allowedMediaURL(%q)=%v, want %v", c.in, got, c.want)
		}
	}
}

func newDebugServer(t *testing.T) *Server {
	t.Helper()
	up := newFakeUpstream(t, 0)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	return New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test")
}

func TestDebugHandlerRejectsUnknownPath(t *testing.T) {
	s := newDebugServer(t)
	rec := httptest.NewRecorder()
	s.DebugHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/debug/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知调试端点应 404，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "未知的调试端点") {
		t.Fatalf("错误文案应指明端点: %s", rec.Body.String())
	}
}

func TestDebugMediaRejectsForeignHost(t *testing.T) {
	s := newDebugServer(t)
	rec := httptest.NewRecorder()
	s.DebugHandler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/panel/api/debug/media?url=https://example.com/a.png", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非白名单主机应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "qwenlm.ai") {
		t.Fatalf("应说明白名单范围: %s", rec.Body.String())
	}
}

// 公开 mux 上不能出现调试路径 —— 那是"绕过 API Key 直接调上游"的后门。
func TestDebugPathAbsentFromPublicMux(t *testing.T) {
	s := newDebugServer(t)
	for _, p := range []string{
		"/panel/api/debug/v1/models",
		"/panel/api/debug/media?url=https://cdn.qwenlm.ai/a.png",
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("公开路由不该响应 %s", p)
		}
	}
}
