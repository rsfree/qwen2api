package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	// 默认值必须与实测抓包同一代（0.3.26 / /api/v2/models/ 那一代）。
	if cfg.Upstream.Version != "0.3.26" {
		t.Fatalf("默认 version 头不对: %q", cfg.Upstream.Version)
	}
	if cfg.Upstream.BaseURL != "https://chat.qwen.ai" {
		t.Fatalf("默认 base_url 不对: %q", cfg.Upstream.BaseURL)
	}
	if cfg.Upstream.AuthBaseURL != "https://auth.qwen.ai" {
		t.Fatalf("默认 auth 域不对（鉴权已与业务域分离）: %q", cfg.Upstream.AuthBaseURL)
	}
	if cfg.DefaultModel != "qwen3.7-plus" {
		t.Fatalf("默认模型不对: %q", cfg.DefaultModel)
	}
	if !cfg.ReasoningEnabled() || !cfg.NewChatEach() {
		t.Fatal("默认应透出思考、每请求新建会话")
	}
	// 生图预算默认 300s：实测出图 60s、最慢双垫图 137s，150s 会把"快好了"的请求掐死。
	if cfg.ImageTimeoutS != DefaultImageTimeoutS {
		t.Fatalf("生图默认预算应为 %ds，得到 %d", DefaultImageTimeoutS, cfg.ImageTimeoutS)
	}
}

// 显式写 0 / 负数都是"没配"，归默认 —— 绝不能变成"立即超时"。
func TestImageTimeoutZeroFallsBack(t *testing.T) {
	for _, raw := range []string{`{"image_timeout_s":0}`, `{"image_timeout_s":-5}`} {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if cfg.ImageTimeoutS != DefaultImageTimeoutS {
			t.Fatalf("%s 应归默认 %d，得到 %d", raw, DefaultImageTimeoutS, cfg.ImageTimeoutS)
		}
	}
}

func TestRejectBadThinking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(`{"default_thinking":"乱写"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("非法档位必须报错，不能静默回默认")
	}
}

func TestAccountsDedupAndTrim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	// 重复 token 与空 token 都要被清掉；空名自动补。
	body := `{"accounts":[{"token":"a"},{"token":"a"},{"token":"  "},{"name":"x","token":" b "}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("应剩 2 个账号，得到 %d: %+v", len(cfg.Accounts), cfg.Accounts)
	}
	if cfg.Accounts[0].Name != "acct-1" {
		t.Fatalf("空名应自动编号: %q", cfg.Accounts[0].Name)
	}
	if cfg.Accounts[1].Token != "b" {
		t.Fatalf("token 未 trim: %q", cfg.Accounts[1].Token)
	}
}

func TestEnvOverridesAccountsWholesale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	if err := os.WriteFile(path, []byte(`{"accounts":[{"token":"from-file"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QWEN2API_ACCOUNTS", "t1,t2")
	t.Setenv("QWEN2API_API_KEYS", "k1 k2")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// 🔴 环境变量是整体替换而非追加：否则"我以为清空了旧号"会变成安全事故。
	if len(cfg.Accounts) != 2 || cfg.Accounts[0].Token != "t1" {
		t.Fatalf("账号未被整体替换: %+v", cfg.Accounts)
	}
	if len(cfg.APIKeys) != 2 {
		t.Fatalf("api_keys 解析错误: %+v", cfg.APIKeys)
	}
}

func TestNormalizeURLs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	body := `{"upstream":{"base_url":"https://chat.qwen.ai/"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.BaseURL != "https://chat.qwen.ai" {
		t.Fatalf("尾斜杠未去掉: %q", cfg.Upstream.BaseURL)
	}
}
