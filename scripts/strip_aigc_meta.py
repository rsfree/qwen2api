#!/usr/bin/env python3
"""剥离 AI 生成图片的隐式标识元数据（TC260 / C2PA / XMP）。

背景（2026-11-12 实测）：
  Qwen t2i 产物（2048x2048 PNG）画面**没有可见水印**，但 PNG 里带一个
  630 字节的 iTXt chunk，内容是 TC260 AIGC 元数据：

    <TC260:AIGC>{"Label":"1","ContentProducer":"...",
                  "ProduceID":"R-...","ReservedCode1":"L-..."}</TC260:AIGC>

  这是 GB 45438 的隐式标识（Label=1 表示 AI 生成）。视频走的是另一条路 ——
  像素上烧可见水印，mp4 容器反而干净（只有 encoder=Lavf）。

为什么用 chunk 级删除而不是重编码：
  PNG 是 chunk 流，删掉元数据 chunk 后 IHDR/IDAT/IEND 原封不动，
  **像素比特级不变、零画质损失、秒级完成**。重编码没必要且会掉画质。

用法：
  python3 strip_aigc_meta.py in.png              # 输出 in_clean.png
  python3 strip_aigc_meta.py in.png -o out.png
  python3 strip_aigc_meta.py in.png --inspect    # 只看有什么元数据
  python3 strip_aigc_meta.py in.jpg --inspect    # jpg 走 exif 提示（见文末）

⚠️ 合规提示：剥离标识后，内容在《人工智能生成合成内容标识办法》意义上
   不再带隐式标识。自行确认使用场景是否需要保留标识。
"""
import argparse
import struct
import sys
import zlib

PNG_SIG = b"\x89PNG\r\n\x1a\n"
# 承载元数据的 chunk：iTXt/zTXt/tEXt 是文本，eXIf 是 EXIF
META_CHUNKS = {b"iTXt", b"zTXt", b"tEXt", b"eXIf"}
# 结构性 chunk，必须保留
KEEP = {b"IHDR", b"PLTE", b"IDAT", b"IEND", b"tRNS", b"gAMA", b"cHRM",
        b"sRGB", b"iCCP", b"bKGD", b"pHYs", b"sBIT", b"hIST", b"tIME",
        b"acTL", b"fcTL", b"fdAT", b"sPLT"}


def iter_chunks(data):
    i = 8
    while i < len(data) - 8:
        ln = struct.unpack(">I", data[i:i + 4])[0]
        typ = data[i + 4:i + 8]
        body = data[i + 8:i + 8 + ln]
        yield typ, body, data[i:i + 12 + ln]
        if typ == b"IEND":
            return
        i += 12 + ln


def decode_text(typ, body):
    try:
        if typ == b"tEXt":
            k, v = body.split(b"\x00", 1)
            return k.decode("latin1"), v.decode("latin1", "replace")
        if typ == b"zTXt":
            k, rest = body.split(b"\x00", 1)
            return k.decode("latin1"), zlib.decompress(rest[1:]).decode("latin1", "replace")
        if typ == b"iTXt":
            k, rest = body.split(b"\x00", 1)
            comp = rest[0]
            rest = rest[2:]
            lang, rest = rest.split(b"\x00", 1)
            trans, text = rest.split(b"\x00", 1)
            if comp == 1:
                text = zlib.decompress(text)
            return k.decode("latin1"), text.decode("utf-8", "replace")
    except Exception as e:
        return "?", "<解析失败: %s>" % e
    return "?", ""


def inspect(path):
    data = open(path, "rb").read()
    if not data.startswith(PNG_SIG):
        print("不是 PNG（jpg 请见 --inspect 末尾提示）")
        return
    print("%-28s size=%d" % (path, len(data)))
    n_meta = 0
    for typ, body, _ in iter_chunks(data):
        t = typ.decode("latin1")
        if typ in META_CHUNKS:
            n_meta += 1
            k, v = decode_text(typ, body)
            print("  [元数据] %s  %d bytes  key=%s" % (t, len(body), k))
            for line in v.splitlines()[:6]:
                print("      " + line[:160])
        else:
            print("  [结构]   %s  %d bytes" % (t, len(body)))
    print("  => 元数据 chunk %d 个" % n_meta)


def strip(path, out):
    data = open(path, "rb").read()
    if not data.startswith(PNG_SIG):
        raise SystemExit("只支持 PNG；jpg/webp 请用："
                         "ffmpeg -i in.jpg -map_metadata -1 -c copy out.jpg")
    buf = bytearray(PNG_SIG)
    dropped = []
    for typ, body, raw in iter_chunks(data):
        if typ in META_CHUNKS:
            dropped.append((typ.decode("latin1"), len(body)))
            continue
        buf += raw
    if not dropped:
        print("没有元数据 chunk，原样复制")
    open(out, "wb").write(buf)
    print("已输出 %s  (%d -> %d bytes)" % (out, len(data), len(buf)))
    for t, n in dropped:
        print("  移除 %s  %d bytes" % (t, n))
    return len(dropped)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("input")
    ap.add_argument("-o", "--output")
    ap.add_argument("--inspect", action="store_true")
    a = ap.parse_args()

    if a.inspect:
        inspect(a.input)
        if a.input.lower().endswith((".jpg", ".jpeg")):
            print("\njpg 的 XMP/EXIF 在 APP1 段，手工删会破坏其他 EXIF；"
                  "\n用：ffmpeg -i in.jpg -map_metadata -1 -c copy out.jpg")
        return

    out = a.output or (a.input.rsplit(".", 1)[0] + "_clean.png")
    strip(a.input, out)


if __name__ == "__main__":
    main()
