package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestWMBox 几何推算必须与已验证的 Python 参考实现逐位一致
// （qwen2strip/stripper.py，2026-11-12 跨 720/960/1536 短边实测标定）。
func TestWMBox(t *testing.T) {
	cases := []struct {
		name           string
		W, H           int
		x, y, w, h     int
	}{
		{"1280x720", 1280, 720, 1116, 650, 151, 57},
		{"720x1280", 720, 1280, 556, 1210, 151, 57},
		{"2752x1536", 2752, 1536, 2404, 1389, 320, 119},
		{"960x960", 960, 960, 741, 867, 202, 76},
	}
	for _, c := range cases {
		x, y, w, h := wmBox(c.W, c.H)
		if x != c.x || y != c.y || w != c.w || h != c.h {
			t.Errorf("%s: 得到 (%d,%d,%d,%d) 期望 (%d,%d,%d,%d)",
				c.name, x, y, w, h, c.x, c.y, c.w, c.h)
		}
	}
}

// TestStripWatermarkKeepsSize 去水印不得改变画幅，且必须真的改了框内像素。
func TestStripWatermarkKeepsSize(t *testing.T) {
	const W, H = 640, 480
	src := image.NewRGBA(image.Rect(0, 0, W, H))
	// 铺一层渐变纹理，避免纯色让"改没改"无法区分
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			src.Set(x, y, color.RGBA{uint8(x * 255 / W), uint8(y * 255 / H), 128, 255})
		}
	}

	out, err := stripWatermark(src, nil)
	if err != nil {
		t.Fatalf("stripWatermark: %v", err)
	}
	got, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("输出不是合法 PNG: %v", err)
	}
	if got.Bounds().Dx() != W || got.Bounds().Dy() != H {
		t.Fatalf("画幅被改变: %v", got.Bounds())
	}

	// 框内至少要有像素发生变化；框外必须一个都没动
	x, y, w, h := wmBox(W, H)
	changed := 0
	for j := 0; j < H; j++ {
		for i := 0; i < W; i++ {
			r1, g1, b1, _ := src.At(i, j).RGBA()
			r2, g2, b2, _ := got.At(i, j).RGBA()
			in := i >= x && i < x+w && j >= y && j < y+h
			same := r1 == r2 && g1 == g2 && b1 == b2
			if in && !same {
				changed++
			}
			if !in && !same {
				t.Fatalf("框外像素 (%d,%d) 被改动", i, j)
			}
		}
	}
	if changed == 0 {
		t.Fatal("框内没有任何像素被改动，去水印没生效")
	}
}

// TestStripWatermarkTinyImage 极小图不能 panic（box 会退化到 0 或越界）。
func TestStripWatermarkTinyImage(t *testing.T) {
	for _, sz := range [][2]int{{1, 1}, {8, 8}, {32, 16}} {
		src := image.NewRGBA(image.Rect(0, 0, sz[0], sz[1]))
		if _, err := stripWatermark(src, nil); err != nil {
			t.Errorf("%dx%d: %v", sz[0], sz[1], err)
		}
	}
}

// TestStripWatermarkCustomBox 显式 box 生效；越界 box 自动退回推算（不 panic）。
func TestStripWatermarkCustomBox(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 200))
	if _, err := stripWatermark(src, []int{10, 10, 50, 50}); err != nil {
		t.Fatalf("自定义 box: %v", err)
	}
	if _, err := stripWatermark(src, []int{190, 190, 50, 50}); err != nil {
		t.Fatalf("越界 box 应退回推算而非报错: %v", err)
	}
}
