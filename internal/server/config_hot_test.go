package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 面板保存配置：写回文件 + 立即替换运行时快照（不用重启）。
func TestUpdateConfigWritesAndApplies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":":8400","pool":{"max_rotate":3,"max_image_per_day":6}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Pool.MaxRotate = 3
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(5), "test")
	s.SetConfigPath(path)

	applied, restart, err := s.UpdateConfig(map[string]any{
		"pool.max_rotate":       5,
		"pool.max_image_per_day": 9,
	})
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("应生效 2 项，得到 %v", applied)
	}
	if len(restart) != 0 {
		t.Fatalf("这两项都支持热更，不该要重启: %v", restart)
	}

	// ① 运行时快照已变（选号立刻按新值走）
	if got := s.conf().Pool.MaxRotate; got != 5 {
		t.Fatalf("热更未生效: MaxRotate=%d", got)
	}
	// ② 池自己的快照也要同步（否则选号还用旧限额）
	if got := p.ImageQuotaLeft(); got != 9 {
		t.Fatalf("池的限额没跟上: ImageQuotaLeft=%d", got)
	}
	// ③ 文件真的写进去了
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"max_rotate": 5`) {
		t.Fatalf("配置文件没更新:\n%s", b)
	}
	// ④ accounts 不能被热更抹掉（它是启动期资源）
	if len(s.conf().Accounts) != 1 {
		t.Fatalf("账号被清空了: %+v", s.conf().Accounts)
	}
}

// 不支持的字段必须拒绝，且**不改文件**（半改状态最坑）。
func TestUpdateConfigRejectsUnsafeKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := `{"listen":":8400","api_keys":["sk-a"]}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	p := pool.New(cfg.Pool, nil, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(5), "test")
	s.SetConfigPath(path)

	for _, bad := range []string{"listen", "api_keys", "panel.session_secret"} {
		if _, _, err := s.UpdateConfig(map[string]any{bad: "x"}); err == nil {
			t.Fatalf("%s 应被拒绝", bad)
		}
	}
	b, _ := os.ReadFile(path)
	if string(b) != orig {
		t.Fatalf("被拒绝的请求不该改文件:\n%s", b)
	}
}

// 没给 -config 时只能看不能改（纯环境变量部署）。
func TestUpdateConfigWithoutPath(t *testing.T) {
	cfg := config.Default()
	p := pool.New(cfg.Pool, nil, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(5), "test")
	if _, _, err := s.UpdateConfig(map[string]any{"pool.max_rotate": 2}); err == nil {
		t.Fatal("没有配置文件路径时应报错")
	}
}

// 需要重启的字段：照写文件，但要在 needRestart 里说清楚。
func TestUpdateConfigMarksRestartOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"upstream":{"timeout_s":180}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	p := pool.New(cfg.Pool, nil, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(5), "test")
	s.SetConfigPath(path)

	applied, restart, err := s.UpdateConfig(map[string]any{"upstream.timeout_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || len(restart) != 1 || restart[0] != "upstream.timeout_s" {
		t.Fatalf("应写入但标记需重启，得到 applied=%v restart=%v", applied, restart)
	}
}
