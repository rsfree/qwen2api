#!/usr/bin/env python3
"""去掉 Qwen 视频右下角水印（平台硬编码，不在 API 参数里）。

⚠️ 这不是"开关没打开"，是上游压根没有这个开关：
   biz_data 只有 resolution/size/duration/audio/attachments，
   产物却带 Qwen logo，说明水印叠在端到端的转码链上，只能后处理。

v2（2026-11-12）基于 960x960 t2v 实测修订，修掉三个会静默出错的坑：

  1. 搜索窗比例错了。旧版假设水印在 88%~98% 宽，实测左沿在 78.4%
     （960 宽画面：x=753），窗口把水印左半截切掉了，检测出来的框必然偏窄。
  2. 浅色静止背景下"近白 + 低饱和"判据失效。白墙/浅地板本身就满足，
     bbox 会一路吞到窗口边界。现在检测加了可信度判定：掩膜触到搜索窗
     左/上边缘，说明窗口外还有同质区域 —— 直接判"不可信"并报错，
     绝不给一个看起来合理但会让水印残留在左侧的假框。
  3. delogo 区域贴边会直接 exit 1（"Logo area is outside of the frame"）。
     旧版只 clamp 了 x/y，没管右下边界。现在统一归一化：四周内缩 EDGE，
     且 x/y/w/h 全偶数对齐（yuv420p 下不取偶，delogo 采样会偏半像素）。

画质档（--quality）：
  lossless  x264 qp=0   水印区外像素与解码帧**逐位一致**，体积约 20x（默认）
  near      qp=8        视觉无损，体积约 6x
  high      crf=18      常规高质量，体积约 1.7x
  balanced  crf=22      体积中性（+8%）
  （实测 5.16s/720p/922KB：qp0=21.2MB x23、qp8=5.7MB x6.1、crf22=1.0MB x1.08）

🔴 为什么"无损"只能靠 qp=0，不能靠调 crf 逼近：
   H.264 不存在"只改右下角几块宏块"的合法路径（P/B 帧依赖 + CABAC 上下文），
   所以要么整片重编、要么不改。qp=0 是唯一能让未触碰区域逐位不变的方式，
   已用解码帧比对验证：水印区外 Y 平面总差 = 0。

用法：
  python3 strip_qwen_watermark.py in.mp4 -o out.mp4            # 默认：按短边推算 + lossless
  python3 strip_qwen_watermark.py in.mp4 -o out.mp4 -q near    # 体积友好
  python3 strip_qwen_watermark.py in.mp4 -o out.mp4 --preset t2v-960
  python3 strip_qwen_watermark.py in.mp4 -o out.mp4 --box 736,864,218,90
  python3 strip_qwen_watermark.py in.mp4 --detect-only         # 研究用
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile

import numpy as np

# 右下角搜索窗（占画面比例）。实测水印左沿在 78.4% 宽、上沿在 91.5% 高，
# 窗口必须完整包住它，否则检测一定偏窄。
SEARCH_X = 0.75
SEARCH_Y = 0.85
DETECT_FRAMES = 16
PAD = 4
EDGE = 2          # delogo 距画面边缘的最小距离
TOUCH_TOL = 3     # 掩膜离搜索窗边缘小于这个像素数，视为"触边"→ 不可信

# --- 720p 精确标定（2026-11-12）------------------------------------------
# 1280x720：三条视频各自「时间中位数 - 局部背景」后归一化叠加去噪，
#           th=0.45 与 th=0.60 给出完全相同的框 → x 1125..1258 / y 663..696
#           = 133x33，距右 22、距底 24。
# 720x1280：单条测得距右 22（与横屏**完全一致**）、距底 27、宽 123（阈值偏高截边）。
# ⇒ 水印是**固定像素图层 133x33**，锚定右下角，与画幅长边无关。
#   早期按"宽度百分比 88%~98%"写死，在 1280x720 上偏 50px、在 960x960 上偏更多，
#   就是因为把它当成了等比缩放。
WM_W = 133
WM_H = 33
WM_RIGHT = 22
WM_BOTTOM = 27        # 横屏测 24、竖屏测 27，取大者（保守）
WM_PAD = 4            # 每边外扩，吸收 ±2px 抖动
BASE_SHORT = 720      # 标定基准短边

# 画质档：lossless 用 qp（不是 crf）—— crf 0 在 x264 里等价于 qp 0，
# 但显式写 qp 更不容易被误改成"接近 0 的 crf"。
QUALITY = {
    "lossless": ["-qp", "0", "-preset", "medium"],
    "near":     ["-qp", "8", "-preset", "medium"],
    "high":     ["-crf", "18", "-preset", "medium"],
    "balanced": ["-crf", "22", "-preset", "veryfast"],
}

# 实测预设（--preset 手工用）
PRESETS = {
    "t2v-960": (736, 864, 218, 90),
    "720p-16x9": (1121, 656, 141, 41),
    "720p-9x16": (561, 1216, 141, 41),
}


def auto_box(W, H):
    """按 720p 标定值推算水印框（含外扩）。

    短边 = 720 时直接用实测像素（精度 ±2px）；其它短边按比例缩放兜底，
    但那部分没有实测依据，会打印警告。
    """
    s = min(W, H)
    k = s / float(BASE_SHORT)
    w0 = int(round(WM_W * k))
    h0 = int(round(WM_H * k))
    r0 = int(round(WM_RIGHT * k))
    b0 = int(round(WM_BOTTOM * k))
    x = W - r0 - w0 - WM_PAD
    y = H - b0 - h0 - WM_PAD
    return (x, y, w0 + 2 * WM_PAD, h0 + 2 * WM_PAD)


def sh(cmd):
    return subprocess.run(cmd, capture_output=True, text=True)


def probe(path):
    r = sh(["ffprobe", "-v", "error", "-select_streams", "v:0",
            "-show_entries", "stream=width,height,pix_fmt",
            "-of", "json", path])
    if r.returncode != 0:
        raise SystemExit("ffprobe failed: " + r.stderr[-400:])
    s = json.loads(r.stdout)["streams"][0]
    return int(s["width"]), int(s["height"]), s.get("pix_fmt", "yuv420p")


def detect(path, W, H):
    """右下角粗定位。返回 (x, y, w, h, ok, why)；ok=False 时 box 不可信。"""
    # yuv420p 上 crop 的 x/y/w/h 必须是偶数，否则 ffmpeg 静默改写尺寸，
    # 后续所有坐标映射都会错位一格。
    x0 = (int(W * SEARCH_X) // 2) * 2
    y0 = (int(H * SEARCH_Y) // 2) * 2
    cw = ((W - x0) // 2) * 2
    ch = ((H - y0) // 2) * 2
    with tempfile.TemporaryDirectory() as td:
        raw = os.path.join(td, "corner.raw")
        r = sh(["ffmpeg", "-v", "error", "-i", path,
                "-vf", "crop=%d:%d:%d:%d,select='not(mod(n\\,6))'" % (cw, ch, x0, y0),
                "-vsync", "0", "-frames:v", str(DETECT_FRAMES),
                "-f", "rawvideo", "-pix_fmt", "rgb24", raw])
        if r.returncode != 0:
            raise SystemExit("ffmpeg decode failed: " + r.stderr[-400:])
        d = np.fromfile(raw, dtype=np.uint8)
    fsz = cw * ch * 3
    n = d.size // fsz
    if n == 0:
        return None, False, "解码不到帧"
    v = d[:n * fsz].reshape(n, ch, cw, 3).astype(np.int16)
    med = np.median(v, axis=0)
    sd = v.std(axis=0).mean(axis=2)

    mx = med.max(axis=2)
    mn = med.min(axis=2)
    sat = (mx - mn) / np.maximum(mx, 1)
    # 半透明白：亮度够高、饱和度够低、且时间上稳定（水印是静态叠加层）
    mask = (mn > 55) & (sat < 0.38) & (sd < 12)
    cnt = int(mask.sum())
    if cnt < 250:                 # 少于 250 px 视为没有水印
        return None, False, "掩膜仅 %d px，判为无水印" % cnt

    ys, xs = np.where(mask)
    bx0, bx1 = np.percentile(xs, 1), np.percentile(xs, 99)
    by0, by1 = np.percentile(ys, 1), np.percentile(ys, 99)

    # 可信度：掩膜触到窗口左/上缘 ⇒ 窗口外还有同质区域，bbox 必然被截断。
    # 浅色静止背景（白墙、浅色地板）是典型触发场景。
    if bx0 < TOUCH_TOL or by0 < TOUCH_TOL:
        return None, False, (
            "掩膜触到搜索窗边界 (bx0=%.0f by0=%.0f)，背景与半透明白水印无法区分，"
            "此框不可信" % (bx0, by0))
    frac = cnt / float(cw * ch)
    if frac > 0.55:
        return None, False, "掩膜占搜索窗 %.0f%%，大面积误判" % (frac * 100)

    gx0 = max(0, int(bx0) + x0 - PAD)
    gy0 = max(0, int(by0) + y0 - PAD)
    gx1 = min(W, int(bx1) + x0 + PAD + 1)
    gy1 = min(H, int(by1) + y0 + PAD + 1)
    return (gx0, gy0, gx1 - gx0, gy1 - gy0), True, ""


def normalize_box(box, W, H):
    """四周内缩 EDGE、全偶数对齐，保证 delogo 可执行。

    🔴 取偶必须**向外**：旧版 `h -= h % 2` 是向内收缩，41 会变成 40，
       而水印实测底边在 697、bbox 停在 696 —— 底部那 1px 抗锯齿会留成
       一条亮线（实测在 4x 放大下是个明显的白点）。这里统一改成只扩不缩。
    """
    x, y, w, h = [int(v) for v in box]
    x = max(EDGE, x)
    y = max(EDGE, y)
    x -= x % 2
    y -= y % 2
    x1 = min(W - EDGE, x + w + (w % 2))
    y1 = min(H - EDGE, y + h + (h % 2))
    w = x1 - x
    h = y1 - y
    if w < 16 or h < 16:
        raise SystemExit("bbox 归一化后过小 (%d,%d,%d,%d)，拒绝执行" % (x, y, w, h))
    return x, y, w, h


def remove(path, out, box, quality="lossless", pix_fmt="yuv420p"):
    x, y, w, h = box
    vf = "delogo=x=%d:y=%d:w=%d:h=%d" % (x, y, w, h)
    cmd = (["ffmpeg", "-y", "-v", "error", "-i", path, "-vf", vf,
            "-c:v", "libx264"] + QUALITY[quality] +
           ["-pix_fmt", pix_fmt,
            "-map_metadata", "0",     # 保留 creation_time/location 等容器元数据
            "-c:a", "copy",           # 音频不重编，零损失
            "-movflags", "+faststart",
            out])
    r = sh(cmd)
    if r.returncode != 0:
        raise SystemExit("encode failed: " + r.stderr[-600:])
    return out


def crop_out(path, out, box, W, H, quality="lossless", pix_fmt="yuv420p"):
    """裁掉底部一条（含整块水印），零瑕疵但分辨率变小。

    delogo 靠四周像素插值，遇到高频纹理（岩石、草地、硬边缘）会沿插值方向
    拉出条纹 —— 实测 4 条 720p 里 2 条有可见痕迹。裁剪是唯一保证干净的方案。

    🔴 裁剪高度必须取 bbox **上沿**再往上留一点，取 bbox 下沿等于没裁
       （水印上沿以下的部分仍在画面内）。这个坑踩过。
    """
    x, y, w, h = box
    margin = WM_PAD
    keep_h = y - margin
    keep_h -= keep_h % 2
    if keep_h < H * 0.5:
        raise SystemExit("裁剪后高度 %d 过小（<50%%），改用 delogo" % keep_h)
    vf = "crop=%d:%d:0:0" % (W, keep_h)
    cmd = (["ffmpeg", "-y", "-v", "error", "-i", path, "-vf", vf,
            "-c:v", "libx264"] + QUALITY[quality] +
           ["-pix_fmt", pix_fmt, "-map_metadata", "0",
            "-c:a", "copy", "-movflags", "+faststart", out])
    r = sh(cmd)
    if r.returncode != 0:
        raise SystemExit("crop encode failed: " + r.stderr[-600:])
    return keep_h


def verify(src, out, box, W, H):
    """对比原片与产片在 bbox 内的平均绝对差。

    delogo 生效则该区域必然被重绘；差值接近 0 说明坐标打空了（静默失败）。
    这是防"ffmpeg 返回 0 但其实没处理到水印"的兜底。
    """
    x, y, w, h = box
    vals = []
    for p in (src, out):
        with tempfile.TemporaryDirectory() as td:
            raw = os.path.join(td, "v.raw")
            cf = "crop=%d:%d:%d:%d" % (w, h, x, y)
            r = sh(["ffmpeg", "-v", "error", "-i", p,
                    "-vf", cf + ",select='eq(n\\,0)'", "-frames:v", "1",
                    "-f", "rawvideo", "-pix_fmt", "rgb24", raw])
            if r.returncode != 0:
                return None
            vals.append(np.fromfile(raw, dtype=np.int16))
    a, b = vals
    n = min(a.size, b.size)
    if n == 0:
        return None
    return float(np.abs(a[:n] - b[:n]).mean())


def _medfilt1(a, k):
    """沿 axis=1 的一维中值滤波（edge padding）。

    专门对付垂直插值的副作用：某一列的上边界若落在高对比像素上，
    整列会被"染亮"成一条竖直渐变带。中值能在不损失横向低频结构的前提下
    把这种孤立列剔掉 —— 背景是低频的，中值不会吃掉内容。
    """
    if k < 3 or a.shape[1] < k:
        return a
    pad = k // 2
    ap = np.pad(a, ((0, 0), (pad, pad)), mode="edge")
    win = np.lib.stride_tricks.sliding_window_view(ap, k, axis=1)
    return np.median(win, axis=-1).astype(np.float32)


def _fill_plane(plane, x, y, w, h, rng):
    """在单通道平面上就地填充矩形。

    为什么不能只用 delogo：它把边界像素做双向插值，对 133x33 这种**宽条形**
    区域，水平跨度 133px 会把左/右边界的高光整条拉进画面中部（实测会拉出
    一道竖直亮条纹）。宽条形的正确方向是**垂直**插值 —— 跨度只有 33px，
    且上下边界通常是同一块背景的自然延续。

    这里做加权融合：离上下边界近 → 信垂直；离左右边界近 → 信水平。
    最后补一层匹配噪声，否则填充区比周围"过于干净"，一眼能看出被抹过。
    """
    H, W = plane.shape
    x0, y0 = max(0, x), max(0, y)
    x1, y1 = min(W, x + w), min(H, y + h)
    ww, hh = x1 - x0, y1 - y0
    if ww <= 0 or hh <= 0:
        return

    # 采样带外移 2px：紧贴 bbox 的那一行常残留水印的抗锯齿余辉，
    # 直接拿它当插值端点，等于把水印的亮边重新抹回画面里。
    ty = y0 - 3 if y0 >= 3 else max(0, y0 - 1)
    by = y1 + 2 if y1 + 2 < H else H - 1
    lx = x0 - 3 if x0 >= 3 else max(0, x0 - 1)
    rx = x1 + 2 if x1 + 2 < W else W - 1
    top = plane[ty, x0:x1].astype(np.float32) if y0 > 0 else None
    bot = plane[by, x0:x1].astype(np.float32) if y1 < H else None
    left = plane[y0:y1, lx].astype(np.float32) if x0 > 0 else None
    right = plane[y0:y1, rx].astype(np.float32) if x1 < W else None

    t = (np.arange(1, hh + 1, dtype=np.float32)[:, None]) / (hh + 1.0)
    if top is not None and bot is not None:
        vert = top[None, :] * (1 - t) + bot[None, :] * t
    elif top is not None:
        vert = np.broadcast_to(top[None, :], (hh, ww)).copy()
    elif bot is not None:
        vert = np.broadcast_to(bot[None, :], (hh, ww)).copy()
    else:
        return

    s_ = (np.arange(1, ww + 1, dtype=np.float32)[None, :]) / (ww + 1.0)
    if left is not None and right is not None:
        horiz = left[:, None] * (1 - s_) + right[:, None] * s_
    elif left is not None:
        horiz = np.broadcast_to(left[:, None], (hh, ww)).copy()
    elif right is not None:
        horiz = np.broadcast_to(right[:, None], (hh, ww)).copy()
    else:
        horiz = vert

    # 权重：距离越近越可信，用 1/d^1.5 衰减（比线性更"认近邻"）
    dy = np.minimum(np.arange(1, hh + 1), np.arange(hh, 0, -1)).astype(np.float32)[:, None]
    dx = np.minimum(np.arange(1, ww + 1), np.arange(ww, 0, -1)).astype(np.float32)[None, :]
    wv = 1.0 / (dy ** 1.5 + 1.0)
    wh = 1.0 / (dx ** 1.5 + 1.0)
    out = (vert * wv + horiz * wh) / (wv + wh)

    # 干掉"单列被染亮"的竖条：横向中值，窗口随区域宽度自适应
    k = int(max(7, min(31, ww // 6))) | 1     # 取奇数
    out = _medfilt1(out, k)

    # 噪声匹配：从 bbox 外围一圈估计局部噪声水平
    ry0, ry1 = max(0, y0 - 4), min(H, y1 + 4)
    rx0, rx1 = max(0, x0 - 4), min(W, x1 + 4)
    ring = plane[ry0:ry1, rx0:rx1].astype(np.float32)
    if ring.size > 64:
        # 去趋势后估 sigma，避免把结构当成噪声
        k = ring - np.median(ring)
        sigma = float(np.median(np.abs(k)) * 1.4826)
        sigma = min(sigma, 6.0)
        if sigma > 0.3:
            out += rng.normal(0, sigma * 0.6, out.shape).astype(np.float32)

    plane[y0:y1, x0:x1] = np.clip(out, 0, 255).astype(np.uint8)


def remove_inpaint(src, out, box, W, H, quality, pix_fmt="yuv420p"):
    """自研填充：解码 -> 逐帧修 -> 重编码。yuv420p 空间内处理，不做 RGB 往返。"""
    if not pix_fmt.startswith("yuv420"):
        raise SystemExit("inpaint 仅支持 yuv420p（当前 %s），请用 --fill delogo" % pix_fmt)

    r = sh(["ffprobe", "-v", "error", "-select_streams", "v:0",
            "-show_entries", "stream=r_frame_rate,nb_frames",
            "-of", "json", src])
    st = json.loads(r.stdout)["streams"][0]
    num, den = st["r_frame_rate"].split("/")
    fps = float(num) / float(den)

    fsz = W * H * 3 // 2
    ysz = W * H
    uvsz = ysz // 4

    dec = subprocess.Popen(
        ["ffmpeg", "-v", "error", "-i", src, "-f", "rawvideo",
         "-pix_fmt", "yuv420p", "-"],
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    enc = subprocess.Popen(
        ["ffmpeg", "-y", "-v", "error",
         "-f", "rawvideo", "-pix_fmt", "yuv420p", "-s", "%dx%d" % (W, H),
         "-r", "%.6f" % fps, "-i", "-", "-i", src,
         "-map", "0:v", "-map", "1:a?", "-c:v", "libx264"] + QUALITY[quality] +
        ["-pix_fmt", "yuv420p", "-c:a", "copy", "-map_metadata", "1",
         "-movflags", "+faststart", out],
        stdin=subprocess.PIPE, stderr=subprocess.PIPE)

    x, y, w, h = box
    # 色度平面坐标减半
    cx, cy, cw2, ch2 = x // 2, y // 2, max(2, w // 2), max(2, h // 2)

    rng = np.random.default_rng(20261112)
    n = 0
    try:
        while True:
            buf = dec.stdout.read(fsz)
            if len(buf) < fsz:
                break
            frame = np.frombuffer(buf, dtype=np.uint8).copy()
            Y = frame[:ysz].reshape(H, W)
            U = frame[ysz:ysz + uvsz].reshape(H // 2, W // 2)
            V = frame[ysz + uvsz:ysz + 2 * uvsz].reshape(H // 2, W // 2)
            _fill_plane(Y, x, y, w, h, rng)
            _fill_plane(U, cx, cy, cw2, ch2, rng)
            _fill_plane(V, cx, cy, cw2, ch2, rng)
            enc.stdin.write(frame.tobytes())
            n += 1
    finally:
        try:
            enc.stdin.close()
        except Exception:
            pass
        dec.wait()
        err = enc.stderr.read().decode("utf-8", "replace")
        enc.wait()
    if enc.returncode != 0:
        raise SystemExit("inpaint encode failed: " + err[-600:])
    return n


def verify_lossless(src, out, box, W, H, max_frames=40):
    """解码两份文件，比对 bbox 外 Y 平面是否逐位一致。

    这是"无损"唯一可信的证明方式 —— 不能看文件大小、不能看 crf，
    只能看解码后的像素。取前 max_frames 帧即可（编码器不会只在前几帧有损）。
    """
    x, y, w, h = box
    fsz = W * H * 3 // 2          # yuv420p
    ysz = W * H
    total = 0
    px = 0
    for f in range(max_frames):
        bufs = []
        for p in (src, out):
            # 裸帧是二进制，必须不带 text=True（否则 stdout 会被当 UTF-8 解码而崩）
            r = subprocess.run(
                ["ffmpeg", "-v", "error", "-i", p,
                 "-vf", "select='eq(n\\,%d)'" % f, "-vsync", "0",
                 "-frames:v", "1", "-f", "rawvideo",
                 "-pix_fmt", "yuv420p", "-"],
                capture_output=True)
            if r.returncode != 0 or len(r.stdout) < ysz:
                return None
            bufs.append(np.frombuffer(r.stdout[:fsz], dtype=np.uint8))
        a, b = bufs
        ya = a[:ysz].reshape(H, W).astype(np.int16)
        yb = b[:ysz].reshape(H, W).astype(np.int16)
        m = np.ones((H, W), dtype=bool)
        m[max(0, y - 2):min(H, y + h + 2), max(0, x - 2):min(W, x + w + 2)] = False
        d = np.abs(ya - yb)
        total += int(d[m].sum())
        px += int(m.sum())
    return total, px


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("input")
    ap.add_argument("-o", "--output")
    ap.add_argument("--preset", choices=sorted(PRESETS),
                    help="使用实测预设 bbox，跳过推算")
    ap.add_argument("--box", help="手工指定 x,y,w,h（优先级最高）")
    ap.add_argument("--detect", action="store_true",
                    help="用像素统计法检测（深色高对比背景可用；浅色背景不可靠，不推荐）")
    ap.add_argument("--detect-only", action="store_true")
    ap.add_argument("--mode", choices=["delogo", "crop"], default="delogo",
                    help="delogo=保持分辨率，纹理背景可能留涂抹痕；"
                         "crop=裁掉底部一条，绝对干净但分辨率变小")
    ap.add_argument("--fill", choices=["inpaint", "delogo"], default="inpaint",
                    help="inpaint=自研垂直优先填充+噪声匹配（默认，宽条形不留竖条）; "
                         "delogo=ffmpeg 原生滤镜（快，但 133x33 宽条形会拉出竖条纹）")
    ap.add_argument("-q", "--quality", default="lossless",
                    choices=list(QUALITY),
                    help="lossless=qp0 逐位无损（默认，体积约 20x）; "
                         "near=qp8 视觉无损(6x); high=crf18(1.7x); balanced=crf22(1.08x)")
    ap.add_argument("--crf", type=int,
                    help="兼容旧参数：给定则覆盖 --quality 的码率控制（有损）")
    ap.add_argument("--no-verify", action="store_true")
    ap.add_argument("--verify-lossless", action="store_true",
                    help="解码比对 bbox 外像素是否逐位一致（慢，但这是无损的唯一证明）")
    a = ap.parse_args()
    if a.detect_only and not (a.box or a.preset):
        a.detect = True

    W, H, pix_fmt = probe(a.input)
    print("分辨率 %dx%d  pix_fmt=%s  quality=%s" % (W, H, pix_fmt, a.quality))
    if a.crf is not None:
        QUALITY["__adhoc__"] = ["-crf", str(a.crf), "-preset", "medium"]
        a.quality = "__adhoc__"
        print("[--crf %d] 覆盖画质档（有损）" % a.crf)

    box = None
    if a.box:
        parts = [int(v) for v in a.box.replace(":", ",").split(",")]
        if len(parts) != 4:
            raise SystemExit("--box 需要 4 个整数: x,y,w,h")
        box = tuple(parts)
        src = "--box"
    elif a.preset:
        box = PRESETS[a.preset]
        src = "--preset " + a.preset
    elif a.detect:
        box, ok, why = detect(a.input, W, H)
        if not ok:
            if box is None and "无水印" in why:
                print("[检测] " + why)
                if a.detect_only:
                    return
                out = a.output or a.input.replace(".mp4", "_clean.mp4")
                sh(["cp", a.input, out])
                print("原样复制 ->", out)
                return
            print("[检测] 失败：" + why, file=sys.stderr)
            print("     像素统计法在浅色/低对比背景上不可靠，改用默认的短边推算",
                  file=sys.stderr)
            raise SystemExit(2)
        src = "自动检测"
    else:
        box = auto_box(W, H)
        src = "720p 标定"
        if min(W, H) != BASE_SHORT:
            print("[注意] 短边 %d != 720，按比例外推（无实测依据），"
                  "建议先用 --detect-only 或手工 --box 核对" % min(W, H),
                  file=sys.stderr)

    nb = normalize_box(box, W, H)
    if nb != tuple(box):
        print("bbox 归一化 %s -> %s（内缩+偶数对齐）" % (str(tuple(box)), str(nb)))
    x, y, w, h = nb
    print("水印框 [%s]: x=%d y=%d w=%d h=%d  (x %.1f%%-%.1f%%, y %.1f%%-%.1f%%)"
          % (src, x, y, w, h, x / W * 100, (x + w) / W * 100,
             y / H * 100, (y + h) / H * 100))
    if a.detect_only:
        return

    out = a.output or a.input.replace(".mp4", "_clean.mp4")
    if a.mode == "crop":
        nH = crop_out(a.input, out, nb, W, H, a.quality, pix_fmt)
        print("已输出 %s (%d bytes)  裁剪 %dx%d -> %dx%d"
              % (out, os.path.getsize(out), W, H, W, nH))
        return

    if a.fill == "inpaint":
        nf = remove_inpaint(a.input, out, nb, W, H, a.quality, pix_fmt)
        print("填充 inpaint（垂直优先 + 噪声匹配）处理 %d 帧" % nf)
    else:
        remove(a.input, out, nb, a.quality, pix_fmt)
    size = os.path.getsize(out)
    src_size = os.path.getsize(a.input)
    print("已输出 %s  %.2f MB (源 %.2f MB, x%.1f)"
          % (out, size / 1e6, src_size / 1e6, size / float(src_size)))

    if not a.no_verify:
        d = verify(a.input, out, nb, W, H)
        if d is None:
            print("[自检] 跳过（抽帧失败）")
        elif d < 2.0:
            print("[自检] ⚠️ bbox 内原片/产片差异仅 %.2f，delogo 疑似没生效（坐标打空？）" % d,
                  file=sys.stderr)
        else:
            print("[自检] bbox 内平均差 %.1f（区域已重绘）" % d)

    if a.verify_lossless:
        r = verify_lossless(a.input, out, nb, W, H)
        if r is None:
            print("[无损] 跳过（抽帧失败）")
        else:
            total, px = r
            if total == 0:
                print("[无损] ✅ bbox 外 Y 平面逐位一致（%d 像素采样，总差 0）" % px)
            else:
                print("[无损] ❌ bbox 外仍有差异：总差 %d / %d 像素（平均 %.4f）"
                      % (total, px, total / float(px)), file=sys.stderr)


if __name__ == "__main__":
    main()
