package server

// 去除 Qwen 生成图右下角的可见水印。
//
// 几何规律（2026-11-12 实测，跨 720 / 960 / 1536 三种短边）：
//
//	来源              短边   水印宽   宽/短边
//	1280x720  t2v      720    133     18.5%
//	720x1280  竖屏     720    133     18.5%
//	960x960   t2v      960    180     18.75%
//	2752x1536 t2i     1536    285     18.55%   ← 图片样本，决定性证据
//
// 🔴 按**短边**等比缩放 ~18.5%，不是固定像素。中途被"前三个样本短边都=720"
// 误导过，2752x1536 的图直接证伪。锚点始终是右下角。
//
// 算法选型（实测对比同一张苔藓图）：
//   - 边界插值类（cv2.inpaint TELEA/NS、ffmpeg delogo）→ 都糊成带色偏的矩形，
//     因为它们只用边界像素外推，不含纹理信息
//   - **镜像克隆 + 四边羽化** → 把紧邻的同质纹理搬过来，纹理连续，几乎看不出
//
// ⚠️ 去可见水印**必然有损**：被 logo 覆盖的像素原内容已不存在，只能推测。
// 能做到的是"视觉不可辨"，不是"比特一致"。真正的无损只有裁剪（丢弃而非修改）。

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"math"
)

const (
	wmRatioW      = 0.185  // 水印宽 ≈ 短边 * 0.185
	wmRatioH      = 0.054  // 水印高 ≈ 短边 * 0.054
	wmRatioRight  = 0.030  // 距右 ≈ 短边 * 0.030
	wmRatioBottom = 0.030  // 距底 ≈ 短边 * 0.030
	wmRatioPad    = 0.012  // 四边外扩，吸收测量误差与半透明边缘
	wmFeather     = 14     // 羽化宽度（px），与 Python 版一致
)

// wmBox 按短边比例推算水印框 [x,y,w,h]。
func wmBox(W, H int) (x, y, w, h int) {
	sm := W
	if H < sm {
		sm = H
	}
	pad := int(math.Round(float64(sm) * wmRatioPad))
	w0 := int(math.Round(float64(sm) * wmRatioW))
	h0 := int(math.Round(float64(sm) * wmRatioH))
	x = W - int(math.Round(float64(sm)*wmRatioRight)) - w0 - pad
	y = H - int(math.Round(float64(sm)*wmRatioBottom)) - h0 - pad
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return x, y, w0 + 2*pad, h0 + 2*pad
}

// stripWatermark 返回去水印后的 PNG 字节。
//
// box 为 nil 时按 wmBox 推算；给了就直接用（用于非标准画幅或手工校正）。
func stripWatermark(src image.Image, box []int) ([]byte, error) {
	b := src.Bounds()
	W, H := b.Dx(), b.Dy()

	// 统一转 RGBA：后续直接操作 Pix 数组，避免 image.At() 的接口开销
	// （2752x1536 下 At() 慢一个量级）。
	rgba := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)

	var x, y, w, h int
	if len(box) == 4 {
		x, y, w, h = box[0], box[1], box[2], box[3]
	} else {
		x, y, w, h = wmBox(W, H)
	}
	// 夹紧，绝不越界（越界会让 Pix 索引越界 panic）
	if x < 0 || y < 0 || w <= 0 || h <= 0 || x+w > W || y+h > H {
		x, y, w, h = wmBox(W, H)
	}
	// 小图退化保护：短边 < 12px 时比例算出来的框可能只有 0~1px，
	// alpha 数组会退化成空数组 → 越界 panic。这种尺寸本来也不可能有水印。
	if w < 2 || h < 2 || x+w > W || y+h > H {
		var buf bytes.Buffer
		if err := png.Encode(&buf, rgba); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// 克隆源：优先左侧镜像（水印贴右，左侧空间通常充足）
	useLeft := x-w >= 0
	if !useLeft && y-h < 0 {
		// 左、上都不够，放弃（宁可返回原图也不出错图）
		var buf bytes.Buffer
		if err := png.Encode(&buf, rgba); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// 羽化 alpha：四边依次累乘（角上叠加两次，与 Python 版一致）
	alpha := make([]float64, w*h)
	for i := range alpha {
		alpha[i] = 1
	}
	f := wmFeather
	if lim := w / 3; f > lim {
		f = lim
	}
	if lim := h / 3; f > lim {
		f = lim
	}
	if f < 1 {
		f = 1
	}
	for i := 0; i < f; i++ {
		v := float64(i+1) / float64(f+1)
		for xx := 0; xx < w; xx++ {
			alpha[i*w+xx] *= v
			alpha[(h-1-i)*w+xx] *= v
		}
		for yy := 0; yy < h; yy++ {
			alpha[yy*w+i] *= v
			alpha[yy*w+(w-1-i)] *= v
		}
	}

	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			var sx, sy int
			if useLeft {
				sx, sy = x-1-i, y+j // 左侧区域水平镜像
			} else {
				sx, sy = x+i, y-1-j // 上方区域垂直镜像
			}
			di := rgba.PixOffset(x+i, y+j)
			si := rgba.PixOffset(sx, sy)
			a := alpha[j*w+i]
			for c := 0; c < 3; c++ {
				rgba.Pix[di+c] = uint8(float64(rgba.Pix[si+c])*a + float64(rgba.Pix[di+c])*(1-a))
			}
		}
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&buf, rgba); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
