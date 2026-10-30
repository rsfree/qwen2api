package panel

// 面板登录的测试。
//
// 这组测试守的是"别让人随便进面板"：面板能做导入/删除账号、能看到全部邮箱。
// 一旦鉴权写松（比如会话校验漏了签名、或空密码被当成通过），
// 表面一切正常，实际整个账号池对外敞开。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// stubBackend 只满足接口，测试不碰真实池/上游。
type stubBackend struct {
	cfg  config.Config
	pool *pool.Pool
	log  *reqlog.Log
	up   *upstream.Client
	// debugHits 记录调试转发被真正调用了几次（用来证明"未登录根本没进去"）。
	debugHits int
}

func newStub(cfg config.Config) *stubBackend {
	return &stubBackend{
		cfg:  cfg,
		pool: pool.New(cfg.Pool, cfg.Accounts, ""),
		log:  reqlog.New(10),
		up:   upstream.New(cfg.Upstream),
	}
}

func (s *stubBackend) Pool() *pool.Pool           { return s.pool }
func (s *stubBackend) Log() *reqlog.Log           { return s.log }
func (s *stubBackend) Upstream() *upstream.Client { return s.up }
func (s *stubBackend) Config() config.Config      { return s.cfg }
func (s *stubBackend) Version() string            { return "test" }
func (s *stubBackend) UptimeSeconds() int         { return 1 }
func (s *stubBackend) ImportAccounts(context.Context, []config.Account, bool) ([]pool.AddResult, error) {
	return nil, nil
}
func (s *stubBackend) DeleteAccount(string) (bool, error)        { return false, nil }
func (s *stubBackend) TestAccount(context.Context, string) error { return nil }
func (s *stubBackend) Models(context.Context) any                { return []string{} }
func (s *stubBackend) RefreshCredits(context.Context, bool)      {}

// DebugHandler 桩：只回显"被调用了"，不碰真实前门。
func (s *stubBackend) DebugHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.debugHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	})
}

func withPanel(mut func(*config.Config)) (*Panel, *stubBackend) {
	cfg := config.Default()
	cfg.StateFile = ""
	cfg.Accounts = nil
	cfg.APIKeys = nil
	cfg.Panel = config.Panel{Username: "admin", Password: "pw-test", SessionHours: 1}
	if mut != nil {
		mut(&cfg)
	}
	b := newStub(cfg)
	return New(b), b
}

func doLogin(p *Panel, user, pass string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func withCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func getWith(p *Panel, path string, ck *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestLoginIssuesSessionAndGatesApi(t *testing.T) {
	p, _ := withPanel(nil)

	// 没会话 ⇒ 面板接口 401
	if rec := getWith(p, "/panel/api/overview", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，得到 %d", rec.Code)
	}

	rec := doLogin(p, "admin", "pw-test")
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	ck := withCookie(rec)
	if ck == nil {
		t.Fatal("登录成功却没下发会话 cookie")
	}
	// HttpOnly 必须在：否则 XSS 能直接把会话读走
	if !ck.HttpOnly {
		t.Fatal("会话 cookie 必须 HttpOnly")
	}

	if rec := getWith(p, "/panel/api/overview", ck); rec.Code != http.StatusOK {
		t.Fatalf("带会话应 200，得到 %d", rec.Code)
	}
	// 静态页不需要鉴权（里面没数据，登录框要靠它渲染）
	if rec := getWith(p, "/panel/", nil); rec.Code != http.StatusOK {
		t.Fatalf("面板静态页应 200，得到 %d", rec.Code)
	}
}

func TestLoginRejectsWrongCredentials(t *testing.T) {
	p, _ := withPanel(nil)
	if rec := doLogin(p, "admin", "nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错密码应 401，得到 %d", rec.Code)
	}
	if rec := doLogin(p, "root", "pw-test"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错用户名应 401，得到 %d", rec.Code)
	}
	// 两者文案必须一致，否则等于送一个用户名枚举接口
	a := doLogin(p, "admin", "nope").Body.String()
	b := doLogin(p, "root", "pw-test").Body.String()
	if a != b {
		t.Fatalf("用户名错/密码错的响应不同，可枚举用户名：\n%s\n%s", a, b)
	}
}

func TestTamperedSessionRejected(t *testing.T) {
	p, _ := withPanel(nil)
	ck := withCookie(doLogin(p, "admin", "pw-test"))

	// 改签名
	parts := strings.SplitN(ck.Value, ".", 2)
	bad := &http.Cookie{Name: sessionCookie, Value: parts[0] + "." + strings.Repeat("0", 64)}
	if rec := getWith(p, "/panel/api/overview", bad); rec.Code != http.StatusUnauthorized {
		t.Fatalf("签名被改应 401，得到 %d", rec.Code)
	}
	// 直接丢掉签名
	if rec := getWith(p, "/panel/api/overview", &http.Cookie{Name: sessionCookie, Value: parts[0]}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无签名应 401，得到 %d", rec.Code)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	p, _ := withPanel(nil)
	tok := p.issueSession("admin", -time.Hour) // 已过期
	if rec := getWith(p, "/panel/api/overview", &http.Cookie{Name: sessionCookie, Value: tok}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("过期会话应 401，得到 %d", rec.Code)
	}
}

func TestSessionBoundToUsername(t *testing.T) {
	// 改了用户名，旧会话必须失效 —— 否则"改密码/换账号"撤不掉已发出的会话。
	p, _ := withPanel(nil)
	tok := p.issueSession("someone-else", 0)
	if rec := getWith(p, "/panel/api/overview", &http.Cookie{Name: sessionCookie, Value: tok}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("他人会话应 401，得到 %d", rec.Code)
	}
}

func TestLogoutInvalidatesCookie(t *testing.T) {
	p, _ := withPanel(nil)
	ck := withCookie(doLogin(p, "admin", "pw-test"))

	req := httptest.NewRequest(http.MethodPost, "/panel/api/logout", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登出应 200，得到 %d", rec.Code)
	}
	cleared := withCookie(rec)
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("登出必须下发 MaxAge<0 的清除 cookie: %+v", cleared)
	}
}

func TestApiKeyStillWorksWhenPanelAuthOn(t *testing.T) {
	// 脚本/CI 不该被面板登录挡住：Key 仍然是等效凭据。
	p, _ := withPanel(func(c *config.Config) { c.APIKeys = []string{"sk-x"} })

	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer sk-x")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("有效 Key 应 200，得到 %d", rec.Code)
	}

	// 错误 Key 仍然 401（别把"有 Key 头"当成通过）
	req = httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 Key 应 401，得到 %d", rec.Code)
	}
}

func TestNoPanelPasswordKeepsOldBehavior(t *testing.T) {
	// 不配面板密码时：只配了 api_keys ⇒ 认 Key；都没配 ⇒ 开放（本机场景）。
	p, _ := withPanel(func(c *config.Config) {
		c.Panel = config.Panel{}
		c.APIKeys = []string{"sk-x"}
	})
	if rec := getWith(p, "/panel/api/overview", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("有 Key 无会话应 401，得到 %d", rec.Code)
	}

	p2, _ := withPanel(func(c *config.Config) {
		c.Panel = config.Panel{}
		c.APIKeys = nil
	})
	if rec := getWith(p2, "/panel/api/overview", nil); rec.Code != http.StatusOK {
		t.Fatalf("全空应放行（本机场景），得到 %d", rec.Code)
	}
}

func TestLoginThrottledAfterFailures(t *testing.T) {
	p, _ := withPanel(nil)
	for i := 0; i < loginFailMax; i++ {
		if rec := doLogin(p, "admin", "bad"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次应是 401，得到 %d", i+1, rec.Code)
		}
	}
	// 达到阈值后即使密码正确也拒绝（窗口内）—— 防爆破优先于体验
	if rec := doLogin(p, "admin", "pw-test"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("应被节流为 429，得到 %d", rec.Code)
	}
}

func TestSessionEndpointTellsFrontendWhatToDo(t *testing.T) {
	p, _ := withPanel(nil)
	rec := getWith(p, "/panel/api/session", nil)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["need_auth"] != true || out["authed"] != false {
		t.Fatalf("未登录时应 need_auth=true authed=false: %v", out)
	}

	ck := withCookie(doLogin(p, "admin", "pw-test"))
	rec = getWith(p, "/panel/api/session", ck)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["authed"] != true || out["method"] != "session" {
		t.Fatalf("登录后应 authed=true method=session: %v", out)
	}
}

func TestLoginDisabledWhenNoPassword(t *testing.T) {
	// 没配密码时登录端点应明确拒绝，而不是"空密码可登"。
	p, _ := withPanel(func(c *config.Config) { c.Panel = config.Panel{} })
	if rec := doLogin(p, "admin", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("未启用登录应 400，得到 %d", rec.Code)
	}
}

// TestDebugProxyGatedBySession 调试转发不能绕开面板鉴权。
//
// 这是"面板改用密码登录后调试台 401"的修复带来的新入口：一旦它被放到鉴权之前，
// 等于给公网开了一个"以服务端身份调上游"的免登录通道（能烧账号额度）。
func TestDebugProxyGatedBySession(t *testing.T) {
	p, b := withPanel(nil)

	rec := getWith(p, "/panel/api/debug/v1/models", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录访问调试转发应 401，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if b.debugHits != 0 {
		t.Fatalf("未登录时不该进入转发（hits=%d）", b.debugHits)
	}

	ck := withCookie(doLogin(p, "admin", "pw-test"))
	if ck == nil {
		t.Fatal("登录没拿到会话")
	}
	rec = getWith(p, "/panel/api/debug/v1/models", ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("已登录应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if b.debugHits != 1 {
		t.Fatalf("已登录应转发 1 次，实际 %d", b.debugHits)
	}
	if !strings.Contains(rec.Body.String(), "/panel/api/debug/v1/models") {
		t.Fatalf("路径应原样转发（由服务端负责改写）: %s", rec.Body.String())
	}
}
