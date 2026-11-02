package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// 连接保活必须满足三条：走 HEAD（不拿正文）、不带凭据（不碰账号）、
// 405 之类的结果不算失败 —— 我们要的只是那条 TCP+TLS 不要被回收。
func TestWarmConnHitsRootWithoutCredential(t *testing.T) {
	var gotMethod, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("<html>405</html>"))
	}))
	defer srv.Close()

	c := New(config.Upstream{BaseURL: srv.URL})
	if err := c.WarmConn(context.Background()); err != nil {
		t.Fatalf("405 不该当错误（连接已经建好了）：%v", err)
	}
	if gotMethod != http.MethodHead {
		t.Fatalf("应是 HEAD，得到 %s", gotMethod)
	}
	if gotAuth != "" {
		t.Fatalf("保活不该带凭据，得到 %q", gotAuth)
	}
}
