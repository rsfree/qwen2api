package config

import (
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("s3cret-密码")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2$") {
		t.Fatalf("格式不对: %s", h)
	}
	if !VerifyPanelPassword(Panel{PasswordHash: h}, "s3cret-密码") {
		t.Fatal("正确密码没通过")
	}
	if VerifyPanelPassword(Panel{PasswordHash: h}, "s3cret-密码x") {
		t.Fatal("错误密码通过了")
	}
	// 同一密码两次哈希必须不同（盐随机）
	h2, _ := HashPassword("s3cret-密码")
	if h == h2 {
		t.Fatal("两次哈希相同 ⇒ 盐没随机")
	}
}

func TestHashTakesPrecedenceOverPlain(t *testing.T) {
	// 同时配了明文与 hash 时以 hash 为准：这样"改了明文但忘了删旧 hash"
	// 不会让旧密码继续可用。
	h, _ := HashPassword("from-hash")
	p := Panel{Password: "from-plain", PasswordHash: h}
	if VerifyPanelPassword(p, "from-plain") {
		t.Fatal("明文不该再生效")
	}
	if !VerifyPanelPassword(p, "from-hash") {
		t.Fatal("hash 没生效")
	}
}

func TestMalformedHashNeverPasses(t *testing.T) {
	bad := []string{
		"pbkdf2$abc$aa$bb", // 迭代次数不是数字
		"pbkdf2$100$aa$bb", // 迭代次数过低
		"pbkdf2$120000$zz$" + strings.Repeat("a", 64),               // 盐不是 hex
		"pbkdf2$120000$0011223344556677$short",                      // 输出长度不对
		"bcrypt$120000$0011223344556677$" + strings.Repeat("a", 64), // 算法不对
	}
	for _, h := range bad {
		if VerifyPanelPassword(Panel{PasswordHash: h}, "") {
			t.Fatalf("坏 hash 竟然通过了: %s", h)
		}
		if VerifyPanelPassword(Panel{PasswordHash: h}, "whatever") {
			t.Fatalf("坏 hash 竟然通过了: %s", h)
		}
	}
}

func TestEmptyPanelAuthDisabled(t *testing.T) {
	// 不配密码 = 不启用面板登录（保持老行为），而不是"空密码即可登录"。
	if (Panel{}).AuthEnabled() {
		t.Fatal("空配置不该算启用")
	}
	if VerifyPanelPassword(Panel{}, "") {
		t.Fatal("空密码不该通过 —— 否则等于人人可登")
	}
	if VerifyPanelPassword(Panel{Password: "  "}, "") {
		t.Fatal("空白密码不该通过")
	}
}

func TestPanelDefaults(t *testing.T) {
	p := Panel{}
	if p.EffectiveUsername() != "admin" {
		t.Fatalf("默认用户名应为 admin，得到 %s", p.EffectiveUsername())
	}
	// 默认 30 天：面板轮询、会话一短就变成"登录框自己弹出来"。
	if p.EffectiveSessionHours() != 720 {
		t.Fatalf("默认会话 720h（30 天），得到 %d", p.EffectiveSessionHours())
	}
	if (Panel{Username: "ops", SessionHours: 3}).EffectiveSessionHours() != 3 {
		t.Fatal("显式配置未生效")
	}
}

func TestPanelSessionSeed(t *testing.T) {
	// 面板密钥优先：改密码不该让密钥派生值漂移。
	if got := (Panel{Key: "sk-panel", Password: "pw"}).SessionSeed(); got != "key:sk-panel" {
		t.Fatalf("应以面板密钥为种子，得到 %q", got)
	}
	// 没有密钥时退到密码 hash，最后才是明文。
	if got := (Panel{PasswordHash: "pbkdf2$x"}).SessionSeed(); got != "hash:pbkdf2$x" {
		t.Fatalf("应回落到密码 hash，得到 %q", got)
	}
	if got := (Panel{Password: " pw "}).SessionSeed(); got != "pass:pw" {
		t.Fatalf("应回落到明文密码并去空白，得到 %q", got)
	}
	// 什么都没配 ⇒ 空种子（由调用方退回随机）。
	if got := (Panel{}).SessionSeed(); got != "" {
		t.Fatalf("无凭据时应为空，得到 %q", got)
	}
}

func TestPanelPasswordFromEnv(t *testing.T) {
	t.Setenv("QWEN2API_PANEL_USERNAME", "ops")
	t.Setenv("QWEN2API_PANEL_PASSWORD", "envpass")
	c := Default()
	applyEnv(&c)
	if c.Panel.Username != "ops" || c.Panel.Password != "envpass" {
		t.Fatalf("环境变量未生效: %+v", c.Panel)
	}
	if !c.Panel.AuthEnabled() {
		t.Fatal("应视为已启用")
	}
}
