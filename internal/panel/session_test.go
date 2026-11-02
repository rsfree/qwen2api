package panel

// 会话时长与签名密钥稳定性的测试。
//
// 背景：面板是 5s 轮询的，会话一过期登录框就会自己弹出来；而签名密钥要是每次启动
// 随机，一次发版/改配置就把所有浏览器踢下线 —— 两者叠加就是用户看到的
// "它老自己跳出来"。这里锁住两条：默认 TTL 足够长；同一份凭据算出的密钥跨实例一致。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// defaultPanel 造一个"没显式配 session_hours"的面板（密钥登录形态）。
func defaultPanel(c *config.Config) {
	c.Panel = config.Panel{Key: "sk-panel-test", Username: "admin"}
}

func TestDefaultSessionTTLIs30Days(t *testing.T) {
	p, _ := withPanel(defaultPanel)

	rec := postJSON(p, "/panel/api/login", `{"key":"sk-panel-test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ExpiresInS int `json:"expires_in_s"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	want := 30 * 24 * 3600
	if body.ExpiresInS != want {
		t.Fatalf("默认会话应 %d 秒（30 天），得到 %d 秒", want, body.ExpiresInS)
	}

	var found bool
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookie {
			found = true
			if ck.MaxAge != want {
				t.Fatalf("cookie MaxAge 应 %d，得到 %d", want, ck.MaxAge)
			}
			if !ck.HttpOnly {
				t.Fatal("会话 cookie 必须 HttpOnly")
			}
		}
	}
	if !found {
		t.Fatal("登录未下发会话 cookie")
	}
}

func TestSessionSecretStableAcrossRestart(t *testing.T) {
	base := config.Default()
	base.Panel = config.Panel{Key: "sk-panel-test", Username: "admin"}

	p1 := New(newStub(base))
	// 模拟重启：同一份配置重新构造一次 Panel。
	p2 := New(newStub(base))
	if !bytes.Equal(p1.secret, p2.secret) {
		t.Fatal("签名密钥应由凭据派生：重启后必须一致，否则每次发版都掉登录")
	}
	// 重启前签发的会话，重启后仍然有效 —— 这才是"不弹登录框"的关键。
	if tok := p1.issueSession("admin", time.Hour); !p2.sessionValid(tok, "admin") {
		t.Fatal("重启后旧会话应仍然有效")
	}

	// 换面板密钥 ⇒ 派生值变 ⇒ 旧会话失效（撤销语义）。
	rotated := base
	rotated.Panel.Key = "sk-panel-other"
	p3 := New(newStub(rotated))
	if bytes.Equal(p1.secret, p3.secret) {
		t.Fatal("换密钥后签名密钥必须跟着变")
	}
	if p3.sessionValid(p1.issueSession("admin", time.Hour), "admin") {
		t.Fatal("换密钥后旧会话必须失效")
	}

	// 显式配 session_secret 时以它为准（轮换/多副本场景）。
	explicit := base
	explicit.Panel.SessionSecret = "fixed-secret"
	if got := New(newStub(explicit)).secret; string(got) != "fixed-secret" {
		t.Fatalf("显式 session_secret 应优先，得到 %q", got)
	}
}
