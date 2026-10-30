package upstream

// 登录链路要产出**两份**凭据：access（15 分钟）与 refresh_token（可重复使用）。
//
// 这条测试钉的是一个真实事故：第一版把 signin 的 token cookie 当成"会话"
// 返回，于是 refresh_token 一个都没存下来，每个号 15 分钟后仍要重登。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestSignInSessionCapturesRefreshToken(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/signin"):
			http.SetCookie(w, &http.Cookie{Name: "token", Value: "session-jwt"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/refresh"):
			gotCookie = r.Header.Get("Cookie")
			// refresh_token 只在 refresh 响应里下发
			http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "rt-jwt"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at-jwt"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.Default().Upstream
	cfg.BaseURL = srv.URL
	cfg.AuthBaseURL = srv.URL
	cli := New(cfg)

	access, refresh, err := cli.SignInSession(context.Background(), "a@b.c", "pw")
	if err != nil {
		t.Fatalf("signin: %v", err)
	}
	if access != "bearer:at-jwt" {
		t.Fatalf("access 形态不对: %q", access)
	}
	if refresh != "rt-jwt" {
		t.Fatalf("应带上 refresh_token（来自 refresh 响应），实际 %q", refresh)
	}
	// 换 access 时必须用 signin 的 token cookie，而不是 refresh_token。
	if !strings.Contains(gotCookie, "token=session-jwt") {
		t.Fatalf("refresh 请求的 Cookie 不对: %q", gotCookie)
	}
}

func TestRefreshWithTokenSendsRefreshCookie(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at2"}}`))
	}))
	defer srv.Close()

	cfg := config.Default().Upstream
	cfg.BaseURL = srv.URL
	cfg.AuthBaseURL = srv.URL
	cli := New(cfg)

	access, next, err := cli.RefreshWithToken(context.Background(), "rt-jwt")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if access != "at2" || next != "" {
		t.Fatalf("返回值不对: access=%q next=%q", access, next)
	}
	if gotCookie != "refresh_token=rt-jwt" {
		t.Fatalf("Cookie 头应为 refresh_token=…，实际 %q", gotCookie)
	}
}
