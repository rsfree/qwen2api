package panel

// 面板「密钥登录」的测试。
//
// 面板现在默认就是密钥（config.panel.key），账密只是可选后备。
// 这组用例守三件事：
//  1. 密钥能换到 HttpOnly 会话，浏览器里不留明文；
//  2. 错误密钥绝不能下发会话（不能因为"配了密码"就走空通过）；
//  3. 只配密钥时，账密入口必须明确关闭。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// postJSON 发一个 JSON POST（登录等写操作）。
func postJSON(p *Panel, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// 默认形态：只配 panel.key，不配密码。
func keyOnly(c *config.Config) {
	c.Panel = config.Panel{Key: "sk-panel-test", SessionHours: 1}
}

func TestPanelKeyLoginIssuesSession(t *testing.T) {
	p, _ := withPanel(func(c *config.Config) {
		keyOnly(c)
		c.APIKeys = []string{"sk-front"}
	})

	// 没有会话 ⇒ 面板接口 401
	if rec := getWith(p, "/panel/api/overview", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，得到 %d", rec.Code)
	}

	rec := postJSON(p, "/panel/api/login", `{"key":"sk-panel-test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("密钥登录应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	ck := withCookie(rec)
	if ck == nil {
		t.Fatal("密钥登录没下发会话 cookie")
	}
	// 🔴 HttpOnly 是这条改造的意义所在：明文密钥只出现在登录那一次请求里。
	if !ck.HttpOnly {
		t.Fatal("会话 cookie 必须 HttpOnly（否则 XSS 能直接读走）")
	}

	if rec := getWith(p, "/panel/api/overview", ck); rec.Code != http.StatusOK {
		t.Fatalf("带会话应放行，得到 %d", rec.Code)
	}
}

func TestPanelKeyRejectsWrongKey(t *testing.T) {
	// 同时配密码：确认走的是 key 分支，不会掉进"密码分支空通过"。
	p, _ := withPanel(func(c *config.Config) {
		c.Panel = config.Panel{Key: "sk-panel-test", Password: "pw-test", SessionHours: 1}
	})
	rec := postJSON(p, "/panel/api/login", `{"key":"sk-nope"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误密钥应 401，得到 %d", rec.Code)
	}
	if ck := withCookie(rec); ck != nil {
		t.Fatal("错误密钥不该下发会话")
	}
}

// 单 Key 部署（只配 api_keys）的老用户升级后不用先改配置才能进面板。
func TestPanelKeyFallsBackToAPIKeys(t *testing.T) {
	p, _ := withPanel(func(c *config.Config) {
		c.Panel = config.Panel{SessionHours: 1} // 没有 panel.key、没有密码
		c.APIKeys = []string{"sk-front"}
	})
	rec := postJSON(p, "/panel/api/login", `{"key":"sk-front"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("前门令牌应能登面板，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if ck := withCookie(rec); ck == nil {
		t.Fatal("没下发会话")
	}
}

// 脚本 / curl / 书签：请求头或 ?key= 直接带密钥也要能用。
func TestPanelKeyAsHeader(t *testing.T) {
	p, _ := withPanel(keyOnly)

	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer sk-panel-test")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Authorization 带密钥应放行，得到 %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/panel/api/overview?key=sk-panel-test", nil)
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("?key= 应放行，得到 %d", rec.Code)
	}
}

// 只配密钥时，账密入口必须明确关闭（不能"空密码即通过"）。
func TestPasswordLoginDisabledWithKeyOnly(t *testing.T) {
	p, _ := withPanel(keyOnly)
	rec := postJSON(p, "/panel/api/login", `{"username":"admin","password":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未配密码时账密登录应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if ck := withCookie(rec); ck != nil {
		t.Fatal("未配密码时不该下发会话")
	}
}

// 前端靠这个接口决定"默认显示密钥框、是否给账密切换入口"。
func TestSessionEndpointMarksKeyOnly(t *testing.T) {
	p, _ := withPanel(keyOnly)
	rec := getWith(p, "/panel/api/session", nil)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["need_auth"] != true {
		t.Fatalf("配了密钥就必须要求登录: %v", out)
	}
	if out["password_enabled"] != false || out["key_only"] != true {
		t.Fatalf("只配密钥时应 password_enabled=false key_only=true: %v", out)
	}
	if out["panel_key"] != true {
		t.Fatalf("应告知已配了自定义面板密钥: %v", out)
	}
}

// 密钥登出后会话立刻失效（cookie 清空 + 服务端只认签名与 TTL）。
func TestPanelKeyLogout(t *testing.T) {
	p, _ := withPanel(keyOnly)
	ck := withCookie(postJSON(p, "/panel/api/login", `{"key":"sk-panel-test"}`))
	if ck == nil {
		t.Fatal("登录没拿到会话")
	}
	rec := postJSON(p, "/panel/api/logout", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("登出应 200，得到 %d", rec.Code)
	}
	gone := withCookie(rec)
	if gone == nil || gone.MaxAge >= 0 {
		t.Fatalf("登出应下发过期的同名 cookie: %+v", gone)
	}
}
