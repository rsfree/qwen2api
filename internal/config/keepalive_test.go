package config

import "testing"

// 零值/缺失必须回落到默认，否则 tick_s=0 会让保活循环变成 busy loop。
func TestKeepaliveEffectiveFallsBackOnZero(t *testing.T) {
	var k Keepalive
	e := k.Effective()
	d := DefaultKeepalive()
	if e.TickS != d.TickS || e.TokenWindowS != d.TokenWindowS ||
		e.AdoptBatch != d.AdoptBatch || e.Parallel != d.Parallel ||
		e.BatchMax != d.BatchMax || e.TargetMax != d.TargetMax ||
		e.ConcFactor != d.ConcFactor {
		t.Fatalf("零值应全部回落默认，得到 %+v", e)
	}
}

// 续期窗口必须大于一个 tick，否则"刚热完就过期"，等于没热。
func TestKeepaliveWindowMustExceedTick(t *testing.T) {
	k := Keepalive{TickS: 60, TokenWindowS: 30}
	e := k.Effective()
	if e.TokenWindowS <= e.TickS {
		t.Fatalf("窗口(%d) 必须大于 tick(%d)", e.TokenWindowS, e.TickS)
	}
}

// 面板热更要能写到三层路径 pool.keepalive.*，且标成 Hot。
func TestApplyKeepaliveIsHotAndWritable(t *testing.T) {
	raw := map[string]any{}
	applied, restart, err := Apply(raw, map[string]any{
		"pool.keepalive.tick_s":      float64(5),
		"pool.keepalive.conc_factor": float64(3),
	})
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if len(applied) != 2 || len(restart) != 0 {
		t.Fatalf("两个键都应热更：applied=%v restart=%v", applied, restart)
	}
	if v, ok := getPath(raw, "pool.keepalive.tick_s"); !ok || v != 5 {
		t.Fatalf("tick_s 写回不对: %v (ok=%v)", v, ok)
	}
	if v, ok := getPath(raw, "pool.keepalive.conc_factor"); !ok || v != 3 {
		t.Fatalf("conc_factor 写回不对: %v (ok=%v)", v, ok)
	}
}

// 越界值必须被面板校验拦住（tick 调到 0 会让循环空转）。
func TestApplyKeepaliveRejectsOutOfRange(t *testing.T) {
	_, _, err := Apply(map[string]any{}, map[string]any{"pool.keepalive.tick_s": float64(0)})
	if err == nil {
		t.Fatal("tick_s=0 应被拒绝")
	}
}
