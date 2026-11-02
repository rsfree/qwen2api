package server

// 预热续期失败的处置：401 清票、其余退避。
//
// 两条都是 2026-11-02 线上的直接教训：同一个号 timeout 之后 5 分钟变 401
// 「令牌已撤销」。旧代码既不清票也不退避 —— 结果每 20s 拿同一张废票去打一次
// auth 域，而失败日志被 5 分钟去重，看上去只是"偶发"，把问题藏了起来。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// refreshTestServer 起一台只应答刷新请求的假上游，返回 Server 和命中计数。
func refreshTestServer(t *testing.T, h http.HandlerFunc) (*Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Upstream.BaseURL = srv.URL
	cfg.Upstream.AuthBaseURL = srv.URL
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.Accounts = []config.Account{{
		Name: "a0", Token: "bearer:old", Email: "a0@x.com",
		Password: "pw", RefreshToken: "rt-old",
	}}

	p := pool.New(cfg.Pool, cfg.Accounts, cfg.StateFile)
	return New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test"), &hits
}

// setRefreshFailAt 直接摆一条失败时间戳（模拟"上一轮失败过"）。
// 表由 noteRefreshErr 懒建，这里必须自己补 nil 判断，否则写 nil map 会 panic。
func setRefreshFailAt(t *testing.T, s *Server, name string, at time.Time) {
	t.Helper()
	s.creditErrMu.Lock()
	defer s.creditErrMu.Unlock()
	if s.refreshErrAt == nil {
		s.refreshErrAt = map[string]time.Time{}
	}
	s.refreshErrAt[name] = at
}

func TestRefresh401ClearsToken(t *testing.T) {
	s, hits := refreshTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Unauthorized","details":"令牌已撤销，请重新登录。"}}`))
	})

	if s.refreshTokenOnly(context.Background(), "a0") {
		t.Fatal("401 不应算续期成功")
	}
	if got := s.pool.RefreshToken("a0"); got != "" {
		t.Fatalf("401（令牌已撤销）后必须清空 refresh_token，实际仍是 %q —— "+
			"留着它只会让预热每 20s 撞一次，且永远进不了 adoptOnce 的重登队列", got)
	}
	// 清空之后这张票就该彻底退出预热：再调一次不能再发请求。
	_ = s.refreshTokenOnly(context.Background(), "a0")
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("清票后仍发出了请求（共 %d 次，期望 1 次）", n)
	}
}

func TestRefreshFailureBacksOff(t *testing.T) {
	s, hits := refreshTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	})

	if s.refreshTokenOnly(context.Background(), "a0") {
		t.Fatal("502 不应算续期成功")
	}
	// 非 401 不能清票：上游可能只是抖了一下，票还是好的。
	if got := s.pool.RefreshToken("a0"); got != "rt-old" {
		t.Fatalf("非 401 不该清空 refresh_token，实际 %q", got)
	}

	// 静默期内不得重试（这就是原来每 20s 打一次 auth 域的病根）。
	_ = s.refreshTokenOnly(context.Background(), "a0")
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("静默期内不该重试，实际发了 %d 次", n)
	}

	// 退避到期后恢复重试。
	setRefreshFailAt(t, s, "a0", time.Now().Add(-refreshRetryBackoff-time.Minute))
	_ = s.refreshTokenOnly(context.Background(), "a0")
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Fatalf("静默期过后应重试一次（共 2 次），实际 %d 次", n)
	}
}

func TestRefreshSuccessClearsBackoff(t *testing.T) {
	s, _ := refreshTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at-new"}}`))
	})

	// 造一条"静默期已过"的失败记录：这次成功之后必须把它抹掉，
	// 否则下一次真需要续期时会被旧时间戳白挡 5 分钟。
	setRefreshFailAt(t, s, "a0", time.Now().Add(-refreshRetryBackoff-time.Minute))

	if !s.refreshTokenOnly(context.Background(), "a0") {
		t.Fatal("200 + access_token 应算续期成功")
	}
	if !s.refreshDue("a0", time.Now()) {
		t.Fatal("续期成功后应清掉退避记录，否则下一次真需要续期时会被误挡")
	}
	if got := s.pool.Credential("a0"); got != upstream.ModeBearer+":at-new" {
		t.Fatalf("新凭据没写回池子，实际 %q", got)
	}
}
