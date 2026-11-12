package server

// 视频去水印：几何标定 + YUV 填充 + ffmpeg 转码。
//
// 这套参数与填充算法是 2026-11-12 在真实产物上逐项调出来的，
// 每一项都对应一个实测踩过的坑，改动前先读注释。

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os/exec"
	"sort"
	"strconv"
)

// 水印图层几何（**固定像素**，不随画幅缩放）。
//
// 720p 横屏实测 x1125..1258 / y663..696 = 133x33，距右 22、距底 24；
// 720x1280 竖屏实测距右 22（与横屏完全一致）、距底 27、宽 123（阈值偏高截边）。
// ⇒ 它是锚定右下角的固定像素图层，不是按宽度百分比缩放。
// 早期按"88%~98% 宽"写死，在 1280x720 上偏 50px、960x960 上偏更多。
const (
	wmW       = 133
	wmH       = 33
	wmRight   = 22
	wmBottom  = 27 // 横屏 24 / 竖屏 27，取大者（保守）
	wmPad     = 4
	baseShort = 720
	edgeGuard = 2
)

// watermarkBox 按短边推算水印框（含外扩）。
//
// 短边 == 720 时直接用实测像素；其它短边按比例外推 —— 那部分**没有实测依据**，
// 属于兜底，调用方应记录一条警告。
func watermarkBox(W, H int) (int, int, int, int, bool) {
	short := W
	if H < short {
		short = H
	}
	k := float64(short) / float64(baseShort)
	bw := int(math.Round(wmW * k))
	bh := int(math.Round(wmH * k))
	br := int(math.Round(wmRight * k))
	bb := int(math.Round(wmBottom * k))
	x := W - br - bw - wmPad
	y := H - bb - bh - wmPad
	return x, y, bw + 2*wmPad, bh + 2*wmPad, short == baseShort
}

// normalizeBox 内缩到画面内 + 全偶数对齐。
//
// 🔴 取偶必须**向外**：`h -= h%2` 是向内收缩，41 会变成 40，
// 而水印底边在 697、bbox 停在 696 —— 底部那 1px 抗锯齿会留成亮线/白点。
func normalizeBox(x, y, w, h, W, H int) (int, int, int, int, error) {
	if x < edgeGuard {
		x = edgeGuard
	}
	if y < edgeGuard {
		y = edgeGuard
	}
	x -= x % 2
	y -= y % 2
	x1 := x + w + (w % 2)
	y1 := y + h + (h % 2)
	if x1 > W-edgeGuard {
		x1 = W - edgeGuard
	}
	if y1 > H-edgeGuard {
		y1 = H - edgeGuard
	}
	w = x1 - x
	h = y1 - y
	if w < 16 || h < 16 {
		return 0, 0, 0, 0, fmt.Errorf("水印框归一化后过小 (%d,%d,%d,%d)", x, y, w, h)
	}
	return x, y, w, h, nil
}

// medianFilterRow 一维中值滤波（edge padding）。
//
// 专治垂直插值的副作用：某一列的上边界若落在高对比像素上，整列会被"染亮"
// 成一条竖直渐变带。中值能在不损横向低频结构的前提下把孤立列剔掉。
func medianFilterRow(row []float32, k int) []float32 {
	n := len(row)
	if k < 3 || n < k {
		return row
	}
	pad := k / 2
	ext := make([]float32, n+2*pad)
	for i := 0; i < pad; i++ {
		ext[i] = row[0]
		ext[n+pad+i] = row[n-1]
	}
	copy(ext[pad:], row)

	out := make([]float32, n)
	buf := make([]float32, k)
	for i := 0; i < n; i++ {
		copy(buf, ext[i:i+k])
		sort.Slice(buf, func(a, b int) bool { return buf[a] < buf[b] })
		out[i] = buf[k/2]
	}
	return out
}

// fillPlane 在单通道平面上就地填充矩形。
//
// 为什么不用 ffmpeg delogo：它做双向插值，对 133x33 这种**宽条形**，
// 水平跨度 133px 会把左右边界的高光整条拉进中部 —— 实测在深色木桌上
// 拉出一道明显的竖直亮条。宽条形正确的方向是垂直（跨度只有 33px）。
func fillPlane(plane []byte, W, H, x, y, w, h int, rng *rand.Rand) {
	x0, y0 := x, y
	x1, y1 := x+w, y+h
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > W {
		x1 = W
	}
	if y1 > H {
		y1 = H
	}
	ww, hh := x1-x0, y1-y0
	if ww <= 0 || hh <= 0 {
		return
	}

	// 采样带外移 2~3px：紧贴 bbox 的那一行常残留水印抗锯齿余辉，
	// 直接拿它当插值端点等于把水印的亮边重新抹回画面。
	ty := y0 - 3
	if ty < 0 {
		ty = y0 - 1
	}
	if ty < 0 {
		ty = 0
	}
	by := y1 + 2
	if by >= H {
		by = H - 1
	}
	lx := x0 - 3
	if lx < 0 {
		lx = x0 - 1
	}
	if lx < 0 {
		lx = 0
	}
	rx := x1 + 2
	if rx >= W {
		rx = W - 1
	}

	out := make([][]float32, hh)
	for j := 0; j < hh; j++ {
		out[j] = make([]float32, ww)
	}
	// 垂直插值（逐列）
	tDen := float64(hh + 1)
	for i := 0; i < ww; i++ {
		top := float32(plane[ty*W+x0+i])
		bot := float32(plane[by*W+x0+i])
		for j := 0; j < hh; j++ {
			t := float32(float64(j+1) / tDen)
			out[j][i] = top*(1-t) + bot*t
		}
	}
	// 水平插值（逐行）与权重融合
	sDen := float64(ww + 1)
	for j := 0; j < hh; j++ {
		lv := float32(plane[(y0+j)*W+lx])
		rv := float32(plane[(y0+j)*W+rx])
		dy := j + 1
		if hh-j < dy {
			dy = hh - j
		}
		wv := 1.0 / (math.Pow(float64(dy), 1.5) + 1.0)
		for i := 0; i < ww; i++ {
			s := float32(float64(i+1) / sDen)
			horiz := lv*(1-s) + rv*s
			dx := i + 1
			if ww-i < dx {
				dx = ww - i
			}
			wh := 1.0 / (math.Pow(float64(dx), 1.5) + 1.0)
			out[j][i] = float32((float64(out[j][i])*wv + float64(horiz)*wh) / (wv + wh))
		}
	}
	// 横向中值：干掉被染亮的单列
	k := ww / 6
	if k < 7 {
		k = 7
	}
	if k > 31 {
		k = 31
	}
	k |= 1
	for j := 0; j < hh; j++ {
		out[j] = medianFilterRow(out[j], k)
	}

	// 噪声匹配：从外围环带估 sigma，否则填充区比周围"过于干净"，反而显眼
	ry0, ry1 := y0-4, y1+4
	rx0, rx1 := x0-4, x1+4
	if ry0 < 0 {
		ry0 = 0
	}
	if rx0 < 0 {
		rx0 = 0
	}
	if ry1 > H {
		ry1 = H
	}
	if rx1 > W {
		rx1 = W
	}
	var ring []float32
	for yy := ry0; yy < ry1; yy++ {
		inRow := yy >= y0 && yy < y1
		for xx := rx0; xx < rx1; xx++ {
			if inRow && xx >= x0 && xx < x1 {
				continue
			}
			ring = append(ring, float32(plane[yy*W+xx]))
		}
	}
	sigma := 0.0
	if len(ring) > 64 {
		sort.Slice(ring, func(a, b int) bool { return ring[a] < ring[b] })
		med := ring[len(ring)/2]
		dev := make([]float32, len(ring))
		for i, v := range ring {
			dev[i] = float32(math.Abs(float64(v - med)))
		}
		sort.Slice(dev, func(a, b int) bool { return dev[a] < dev[b] })
		sigma = float64(dev[len(dev)/2]) * 1.4826
		if sigma > 6 {
			sigma = 6
		}
	}

	for j := 0; j < hh; j++ {
		row := (y0+j)*W + x0
		for i := 0; i < ww; i++ {
			v := out[j][i]
			if sigma > 0.3 {
				v += float32(rng.NormFloat64() * sigma * 0.6)
			}
			if v < 0 {
				v = 0
			}
			if v > 255 {
				v = 255
			}
			plane[row+i] = byte(v + 0.5)
		}
	}
}

// qualityArgs 画质档 → x264 参数。
func qualityArgs(q string) ([]string, error) {
	switch q {
	case "", "near":
		return []string{"-qp", "8", "-preset", "medium"}, nil
	case "lossless":
		return []string{"-qp", "0", "-preset", "medium"}, nil
	case "high":
		return []string{"-crf", "18", "-preset", "medium"}, nil
	case "balanced":
		return []string{"-crf", "22", "-preset", "veryfast"}, nil
	}
	return nil, fmt.Errorf("未知画质档 %q（支持 near/lossless/high/balanced）", q)
}

// transcodeStrip 解码 → 逐帧填充 → 重编码。yuv420p 空间内处理，不做 RGB 往返。
//
// 用管道而不是落盘中间文件：一条 15s 1080p 的裸帧是 1.1GB，落盘既慢又占空间。
func transcodeStrip(ctx context.Context, ffmpegPath, src, dst string,
	W, H int, fps float64, box [4]int, quality string) error {

	qargs, err := qualityArgs(quality)
	if err != nil {
		return err
	}

	dec := exec.CommandContext(ctx, ffmpegPath,
		"-v", "error", "-i", src, "-f", "rawvideo", "-pix_fmt", "yuv420p", "-")
	decOut, err := dec.StdoutPipe()
	if err != nil {
		return err
	}
	decErr := &limitedBuf{}
	dec.Stderr = decErr

	encArgs := []string{
		"-y", "-v", "error",
		"-f", "rawvideo", "-pix_fmt", "yuv420p",
		"-s", strconv.Itoa(W) + "x" + strconv.Itoa(H),
		"-r", strconv.FormatFloat(fps, 'f', 6, 64),
		"-i", "-", "-i", src,
		"-map", "0:v", "-map", "1:a?",
		"-c:v", "libx264",
	}
	encArgs = append(encArgs, qargs...)
	encArgs = append(encArgs,
		"-pix_fmt", "yuv420p", "-c:a", "copy",
		"-movflags", "+faststart", dst)
	enc := exec.CommandContext(ctx, ffmpegPath, encArgs...)
	encIn, err := enc.StdinPipe()
	if err != nil {
		return err
	}
	encErr := &limitedBuf{}
	enc.Stderr = encErr

	if err := dec.Start(); err != nil {
		return fmt.Errorf("启动解码器失败: %w", err)
	}
	if err := enc.Start(); err != nil {
		_ = dec.Process.Kill()
		return fmt.Errorf("启动编码器失败: %w", err)
	}

	x, y, w, h := box[0], box[1], box[2], box[3]
	cx, cy := x/2, y/2
	cw2, ch2 := w/2, h/2
	if cw2 < 2 {
		cw2 = 2
	}
	if ch2 < 2 {
		ch2 = 2
	}

	fsz := W * H * 3 / 2
	ysz := W * H
	uvsz := ysz / 4
	buf := make([]byte, fsz)
	rng := rand.New(rand.NewSource(20261112))

	for {
		if _, err := io.ReadFull(decOut, buf); err != nil {
			break
		}
		fillPlane(buf[:ysz], W, H, x, y, w, h, rng)
		fillPlane(buf[ysz:ysz+uvsz], W/2, H/2, cx, cy, cw2, ch2, rng)
		fillPlane(buf[ysz+uvsz:ysz+2*uvsz], W/2, H/2, cx, cy, cw2, ch2, rng)
		if _, err := encIn.Write(buf); err != nil {
			break
		}
	}
	_ = encIn.Close()
	encWErr := enc.Wait()
	decWErr := dec.Wait()

	if encWErr != nil {
		return fmt.Errorf("编码失败: %v (%s)", encWErr, encErr.String())
	}
	if decWErr != nil {
		return fmt.Errorf("解码失败: %v (%s)", decWErr, decErr.String())
	}
	return nil
}

// limitedBuf 只保留最后 N 字节的 stderr，防止 ffmpeg 刷屏把日志撑爆。
type limitedBuf struct{ b []byte }

func (l *limitedBuf) Write(p []byte) (int, error) {
	l.b = append(l.b, p...)
	if len(l.b) > 2048 {
		l.b = l.b[len(l.b)-2048:]
	}
	return len(p), nil
}

func (l *limitedBuf) String() string { return string(l.b) }
