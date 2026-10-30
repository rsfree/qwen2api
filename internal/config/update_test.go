package config

import (
	"encoding/json"
	"testing"
)

// 面板提交的 patch 只能改白名单字段：未知键必须拒绝（否则一次手滑就把配置写坏）。
func TestApplyRejectsUnknownKey(t *testing.T) {
	raw := map[string]any{}
	if _, _, err := Apply(raw, map[string]any{"listen": ":1"}); err == nil {
		t.Fatal("listen 不在白名单，应拒绝")
	}
	if _, _, err := Apply(raw, map[string]any{"panel.session_secret": "x"}); err == nil {
		t.Fatal("panel.session_secret 应拒绝（写错会锁死面板）")
	}
	if _, _, err := Apply(raw, map[string]any{"accounts": []any{}}); err == nil {
		t.Fatal("accounts 应拒绝（启动期资源，热更会抹掉整池）")
	}
}

func TestApplyValidatesType(t *testing.T) {
	raw := map[string]any{}
	if _, _, err := Apply(raw, map[string]any{"pool.max_rotate": 99}); err == nil {
		t.Fatal("超出 1~10 应拒绝")
	}
	if _, _, err := Apply(raw, map[string]any{"pool.max_rotate": 1.5}); err == nil {
		t.Fatal("非整数应拒绝")
	}
	if _, _, err := Apply(raw, map[string]any{"default_thinking": "deep"}); err == nil {
		t.Fatal("枚举外的值应拒绝")
	}
	if _, _, err := Apply(raw, map[string]any{"expose_reasoning": "yes"}); err == nil {
		t.Fatal("bool 收到字符串应拒绝")
	}
}

func TestApplyWritesNested(t *testing.T) {
	raw := map[string]any{}
	applied, restart, err := Apply(raw, map[string]any{
		"pool.max_rotate":      3,
		"pool.cooldown_soft_s": 600,
		"default_thinking":     "fast",
		"upstream.timeout_s":   90, // hot=false
	})
	if err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if len(applied) != 4 {
		t.Fatalf("applied 应 4 项，得到 %v", applied)
	}
	if len(restart) != 1 || restart[0] != "upstream.timeout_s" {
		t.Fatalf("只有 upstream.timeout_s 需重启，得到 %v", restart)
	}

	pool, ok := raw["pool"].(map[string]any)
	if !ok {
		t.Fatal("应写出 pool 子对象")
	}
	if pool["max_rotate"] != 3 || pool["cooldown_soft_s"] != 600 {
		t.Fatalf("嵌套写入不对: %+v", pool)
	}
	// 落盘后必须还能反解回 Config（否则热更会静默失效）。
	b, _ := json.Marshal(raw)
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("反解失败: %v", err)
	}
	if c.Pool.MaxRotate != 3 || c.Pool.CooldownSoftS != 600 || c.DefaultThinking != "fast" {
		t.Fatalf("解出的配置不对: %+v", c.Pool)
	}
}

// 值没变就不该算"改动"：否则每次点保存都会写文件 + 刷一堆"已生效"。
func TestApplySkipsUnchanged(t *testing.T) {
	raw := map[string]any{}
	if _, _, err := Apply(raw, map[string]any{"pool.max_rotate": 3}); err != nil {
		t.Fatal(err)
	}
	// JSON 里数字是 float64，再提交一次 int 3 应当视为无变化。
	applied, restart, err := Apply(raw, map[string]any{"pool.max_rotate": 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 || len(restart) != 0 {
		t.Fatalf("无变化应为空，得到 %v / %v", applied, restart)
	}
}

// FieldValues 必须把 Fields 里每个 key 都能从配置里取到值（否则表单会渲染成空）。
func TestFieldValuesCoversAllFields(t *testing.T) {
	vals := FieldValues(Default())
	for _, f := range Fields {
		if _, ok := vals[f.Key]; !ok {
			t.Fatalf("字段 %s 取不到值（表单会显示空）", f.Key)
		}
	}
}

// 生图预算必须是**可热更**项：面板改完立即生效（不进 need_restart）。
// 否则用户要重启才能让 300s 生效 —— 那就失去"放到配置项里"的意义了。
func TestApplyImageTimeoutIsHot(t *testing.T) {
	raw := map[string]any{}
	applied, restart, err := Apply(raw, map[string]any{"image_timeout_s": 120})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != "image_timeout_s" {
		t.Fatalf("applied 不对: %v", applied)
	}
	if len(restart) != 0 {
		t.Fatalf("image_timeout_s 应热更，不该要求重启: %v", restart)
	}
	if raw["image_timeout_s"] != 120 {
		t.Fatalf("没写进配置: %v (%T)", raw["image_timeout_s"], raw["image_timeout_s"])
	}
	if _, _, err := Apply(raw, map[string]any{"image_timeout_s": 5}); err == nil {
		t.Fatal("低于 30s 应拒绝（会把正常出图全掐死）")
	}
}
