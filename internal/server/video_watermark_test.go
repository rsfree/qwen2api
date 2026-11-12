package server

// 去水印几何与填充的单测。
//
// 重点锁两类回归：
//   1. bbox 取偶方向（向内收缩会漏掉 1px 抗锯齿 → 画面留白线）；
//   2. 填充必须在「水印被完整覆盖」的同时不把边界高光拉成竖条。

import (
	"context"
	"math/rand"
	"os"
	"testing"
)

func TestWatermarkBox720pCoversLayer(t *testing.T) {
	x, y, w, h, measured := watermarkBox(1280, 720)
	if !measured {
		t.Fatal("短边 720 应命中实测标定")
	}
	nx, ny, nw, nh, err := normalizeBox(x, y, w, h, 1280, 720)
	if err != nil {
		t.Fatalf("归一化失败: %v", err)
	}
	// 实测水印图层：x 1125..1258 / y 663..696（135x33）
	if nx > 1125 || ny > 663 {
		t.Fatalf("bbox 左上未覆盖图层: 得到 (%d,%d)，水印从 (1125,663) 开始", nx, ny)
	}
	if nx+nw < 1259 || ny+nh < 697 {
		t.Fatalf("bbox 右下未覆盖图层: 得到 right=%d bottom=%d，水印到 1258/696",
			nx+nw, ny+nh)
	}
}

func TestNormalizeBoxOnlyExpands(t *testing.T) {
	// 输入高 41（奇数）：旧实现 h -= h%2 会缩成 40，导致底边少 1px。
	_, _, _, h, err := normalizeBox(1121, 656, 141, 41, 1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	if h < 41 {
		t.Fatalf("取偶不能向内收缩: 41 变成了 %d", h)
	}
	if h%2 != 0 {
		t.Fatalf("高度未偶数对齐: %d", h)
	}
}

func TestNormalizeBoxClampsInsideFrame(t *testing.T) {
	x, y, w, h, err := normalizeBox(1200, 700, 200, 200, 1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	if x+w > 1280 || y+h > 720 {
		t.Fatalf("bbox 越界: %d+%d / %d+%d", x, y, w, h)
	}
}

func TestFillPlaneRemovesBrightPatch(t *testing.T) {
	const W, H = 400, 200
	plane := make([]byte, W*H)
	for i := range plane {
		plane[i] = 40 // 均匀背景
	}
	// 水印块：133x33 纯白，位置 (200,120)
	bx, by, bw, bh := 200, 120, 133, 33
	for yy := by; yy < by+bh; yy++ {
		for xx := bx; xx < bx+bw; xx++ {
			plane[yy*W+xx] = 255
		}
	}

	rng := rand.New(rand.NewSource(1))
	fillPlane(plane, W, H, bx, by, bw, bh, rng)

	// 填充后该区域均值应回到背景附近（白块 255 必须消失）
	sum := 0
	for yy := by; yy < by+bh; yy++ {
		for xx := bx; xx < bx+bw; xx++ {
			sum += int(plane[yy*W+xx])
		}
	}
	avg := float64(sum) / float64(bw*bh)
	if avg > 70 {
		t.Fatalf("填充后区域仍偏亮: 均值 %.1f（背景 40），白色没被抹掉", avg)
	}
	if avg < 20 {
		t.Fatalf("填充后区域过暗: 均值 %.1f，插值把背景也拉走了", avg)
	}
}

func TestQualityArgs(t *testing.T) {
	for _, q := range []string{"", "near", "lossless", "high", "balanced"} {
		if _, err := qualityArgs(q); err != nil {
			t.Fatalf("%q 应被接受: %v", q, err)
		}
	}
	if _, err := qualityArgs("ultra"); err == nil {
		t.Fatal("未知画质档必须报错，不能静默回默认")
	}
}

func TestTranscodeStripLocal(t *testing.T) {
	src := os.Getenv("WM_SAMPLE")
	if src == "" {
		t.Skip("设置 WM_SAMPLE=<带水印的样例 mp4> 才跑")
	}
	dst := src + ".stripped.mp4"
	err := transcodeStrip(context.Background(), "ffmpeg", src, dst, 1280, 720, 24,
		[4]int{1120, 656, 142, 42}, "near")
	if err != nil {
		t.Fatalf("转码失败: %v", err)
	}
	st, err := os.Stat(dst)
	if err != nil || st.Size() < 10000 {
		t.Fatalf("产物异常: %v size=%v", err, st)
	}
	if os.Getenv("WM_KEEP") != "1" {
		_ = os.Remove(dst)
	}
}
