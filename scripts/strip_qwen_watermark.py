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

用法：
  python3 strip_qwen_watermark.py in.mp4 -o out.mp4            # 默认：按短边推算（推荐）
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
            "-show_entries", "stream=width,height", "-of", "json", path])
    if r.returncode != 0:
        raise SystemExit("ffprobe failed: " + r.stderr[-400:])
    s = json.loads(r.stdout)["streams"][0]
    return int(s["width"]), int(s["height"])


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
    """四周内缩 EDGE、全偶数对齐，保证 delogo 可执行。"""
    x, y, w, h = [int(v) for v in box]
    x = max(EDGE, x)
    y = max(EDGE, y)
    x -= x % 2
    y -= y % 2
    w = min(w, W - x - EDGE)
    h = min(h, H - y - EDGE)
    w -= w % 2
    h -= h % 2
    if w < 16 or h < 16:
        raise SystemExit("bbox 归一化后过小 (%d,%d,%d,%d)，拒绝执行" % (x, y, w, h))
    return x, y, w, h


def remove(path, out, box, crf=18):
    x, y, w, h = box
    vf = "delogo=x=%d:y=%d:w=%d:h=%d" % (x, y, w, h)
    r = sh(["ffmpeg", "-y", "-v", "error", "-i", path,
            "-vf", vf, "-c:v", "libx264", "-crf", str(crf),
            "-preset", "medium", "-pix_fmt", "yuv420p",
            "-c:a", "copy", out])
    if r.returncode != 0:
        raise SystemExit("encode failed: " + r.stderr[-500:])
    return out


def crop_out(path, out, box, W, H, crf=18):
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
    r = sh(["ffmpeg", "-y", "-v", "error", "-i", path,
            "-vf", vf, "-c:v", "libx264", "-crf", str(crf),
            "-preset", "medium", "-pix_fmt", "yuv420p",
            "-c:a", "copy", out])
    if r.returncode != 0:
        raise SystemExit("crop encode failed: " + r.stderr[-500:])
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
    ap.add_argument("--crf", type=int, default=18)
    ap.add_argument("--no-verify", action="store_true")
    a = ap.parse_args()
    if a.detect_only and not (a.box or a.preset):
        a.detect = True

    W, H = probe(a.input)
    print("分辨率 %dx%d" % (W, H))

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
        nH = crop_out(a.input, out, nb, W, H, a.crf)
        print("已输出 %s (%d bytes)  裁剪 %dx%d -> %dx%d"
              % (out, os.path.getsize(out), W, H, W, nH))
        return

    remove(a.input, out, nb, a.crf)
    size = os.path.getsize(out)
    print("已输出 %s (%d bytes)" % (out, size))

    if not a.no_verify:
        d = verify(a.input, out, nb, W, H)
        if d is None:
            print("[自检] 跳过（抽帧失败）")
        elif d < 2.0:
            print("[自检] ⚠️ bbox 内原片/产片差异仅 %.2f，delogo 疑似没生效（坐标打空？）" % d,
                  file=sys.stderr)
        else:
            print("[自检] bbox 内平均差 %.1f（区域已重绘）" % d)


if __name__ == "__main__":
    main()
