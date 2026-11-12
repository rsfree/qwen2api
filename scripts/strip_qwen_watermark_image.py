#!/usr/bin/env python3
"""去除 Qwen 生成图片右下角的可见水印（镜像克隆 + 羽化融合）。

⚠️ 先说清楚「无损」的边界：
  - 剥离 PNG 元数据（TC260/XMP）是**真无损** → 用 strip_aigc_meta.py
  - 去可见水印**必然有损**。水印覆盖的那 285x83 个像素，原始内容已经不存在了，
    任何算法都只能根据周围纹理"猜"。本脚本做到的是**视觉上不可辨**。

为什么不用 inpaint / delogo（实测对比，2026-11-12）：
  同一张苔藓背景的图上，cv2.inpaint 的 TELEA 与 NS 都会糊成一个带色偏的矩形，
  ffmpeg delogo 同理（它俩本质都是边界插值）。而**镜像克隆**是把紧邻的
  同质纹理搬过来，纹理连续性远好于插值 —— 实测几乎看不出补丁。

几何规律（跨 720/960/1536 短边验证）：
  水印宽 = 短边 * 0.185，高 = 短边 * 0.054，锚定右下角，
  距右/距底 = 短边 * 0.030。

用法：
  python3 strip_qwen_watermark_image.py in.png -o out.png
  python3 strip_qwen_watermark_image.py in.jpg --box 1686,979,200,63
  python3 strip_qwen_watermark_image.py in.png --inspect   # 只打印推算框
"""
import argparse
import sys

import cv2
import numpy as np

RATIO_W = 0.185
RATIO_H = 0.054
RATIO_RIGHT = 0.030
RATIO_BOTTOM = 0.030
RATIO_PAD = 0.012


def auto_box(W, H):
    sm = min(W, H)
    pad = int(round(sm * RATIO_PAD))
    w0 = int(round(sm * RATIO_W))
    h0 = int(round(sm * RATIO_H))
    x = W - int(round(sm * RATIO_RIGHT)) - w0 - pad
    y = H - int(round(sm * RATIO_BOTTOM)) - h0 - pad
    return max(0, x), max(0, y), w0 + 2 * pad, h0 + 2 * pad


def clone_fix(img, box, feather=14):
    """从邻近同质区域镜像搬运纹理，四边羽化融合。"""
    x, y, w, h = box
    H, W = img.shape[:2]
    if x - w >= 0:                      # 优先左侧（水印通常贴右，左侧空间充足）
        patch = img[y:y + h, x - w:x][:, ::-1]
        src = "left"
    elif y - h >= 0:                    # 否则上方
        patch = img[y - h:y, x:x + w][::-1, :]
        src = "top"
    elif x + w + w <= W:                # 再退到右侧
        patch = img[y:y + h, x + w:x + 2 * w][:, ::-1]
        src = "right"
    else:
        raise SystemExit("四周空间不足，请用 --box 缩小框")

    alpha = np.ones((h, w), np.float32)
    f = max(1, min(feather, w // 3, h // 3))
    for i in range(f):
        v = (i + 1) / (f + 1.0)
        alpha[i, :] *= v
        alpha[h - 1 - i, :] *= v
        alpha[:, i] *= v
        alpha[:, w - 1 - i] *= v

    out = img.copy()
    a3 = alpha[:, :, None]
    region = out[y:y + h, x:x + w].astype(np.float32)
    blended = patch.astype(np.float32) * a3 + region * (1 - a3)
    out[y:y + h, x:x + w] = np.clip(blended, 0, 255).astype(np.uint8)
    return out, src


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("input")
    ap.add_argument("-o", "--output")
    ap.add_argument("--box", help="手工指定 x,y,w,h")
    ap.add_argument("--feather", type=int, default=14)
    ap.add_argument("--quality", type=int, default=96, help="jpg 输出质量")
    ap.add_argument("--mode", choices=["clone", "crop"], default="clone",
                    help="clone=补像素（视觉不可辨，但有损）；"
                         "crop=裁掉底部一条（保留区像素与原图逐位相同，但画幅变小）")
    ap.add_argument("--inspect", action="store_true")
    a = ap.parse_args()

    img = cv2.imread(a.input, cv2.IMREAD_COLOR)
    if img is None:
        raise SystemExit("读不到图片（路径？格式？）")
    H, W = img.shape[:2]
    print("图像 %dx%d  短边 %d" % (W, H, min(W, H)))

    if a.box:
        parts = [int(v) for v in a.box.replace(":", ",").split(",")]
        if len(parts) != 4:
            raise SystemExit("--box 需要 4 个整数: x,y,w,h")
        box = tuple(parts)
        print("框 [--box]: x=%d y=%d w=%d h=%d" % box)
    else:
        box = auto_box(W, H)
        print("框 [短边推算]: x=%d y=%d w=%d h=%d  (x %.1f%%-%.1f%%, y %.1f%%-%.1f%%)"
              % (box[0], box[1], box[2], box[3],
                 box[0] / W * 100, (box[0] + box[2]) / W * 100,
                 box[1] / H * 100, (box[1] + box[3]) / H * 100))

    if a.inspect:
        return

    if a.mode == "crop":
        # 裁到水印框上沿之上：被裁掉的像素直接丢弃，保留的像素一个都不改
        keep_h = max(1, box[1] - max(2, int(round(min(W, H) * 0.004))))
        out = img[0:keep_h, :].copy()
        print("裁剪 %dx%d -> %dx%d（保留区像素与原图逐位相同）" % (W, H, W, keep_h))
    else:
        out, src = clone_fix(img, box, a.feather)
        print("克隆源: %s 侧" % src)

    dst = a.output or (a.input.rsplit(".", 1)[0] + "_clean.png")
    ext = dst.rsplit(".", 1)[-1].lower()
    if ext in ("jpg", "jpeg"):
        cv2.imwrite(dst, out, [cv2.IMWRITE_JPEG_QUALITY, a.quality])
    else:
        cv2.imwrite(dst, out)
    print("已输出", dst)

    if a.mode == "crop":
        same = (out == img[0:out.shape[0], :]).all()
        print("[自检] 保留区与原图逐像素相同: %s" % same)
        if not same:
            print("[自检] ⚠️ 保留区被改动，crop 实现有问题", file=sys.stderr)
        return

    # 自检：框内平均绝对差，接近 0 说明克隆没生效
    x, y, w, h = box
    d = float(np.abs(out[y:y + h, x:x + w].astype(np.int16)
                     - img[y:y + h, x:x + w].astype(np.int16)).mean())
    if d < 2:
        print("[自检] ⚠️ 框内差异仅 %.1f，克隆疑似没生效" % d, file=sys.stderr)
    else:
        print("[自检] 框内平均差 %.1f（区域已重绘）" % d)


if __name__ == "__main__":
    main()
