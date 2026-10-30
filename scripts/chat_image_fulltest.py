#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""chat 图片输入「全形态」实测。

覆盖矩阵（每个用例都是一次真实上游调用，但都是纯文本对话，不产生出图费用）：

  A. chat 非流式
     1  image_url 对象 + data URI
     2  image_url 字符串 + data URI
     3  image_url 对象 + 裸 base64
     4  image_url 对象 + http(s) URL
     5  input_image（Responses 写法）
  B. 多图（红 + 绿，按顺序指代）
  C. 流式 chat（SSE）
  D. Anthropic /v1/messages（source.type=base64）
  E. Responses /v1/responses（input_image）
  F. 负向：坏引用 400、21 张 400

判真标准：模型答出的颜色 == 图的实际颜色。答"红"不算本事，
同一轮里红图答红、绿图答绿才算"真的看见了"（否则可能是猜的）。
另外记录 input_tokens —— 带图的输入 token 应明显高于纯文本一轮。

用法：
  QWEN2API_BASE=https://qwen2api.1task.cn \
  QWEN2API_KEY=sk-... python3 scripts/chat_image_fulltest.py

  # 连出图三个入口一起回归（会真实消耗出图配额，默认不跑）：
  ... python3 scripts/chat_image_fulltest.py --with-gen

出图用例判真标准：**把产物下载回来解 PNG**，看尺寸与像素颜色 ——
不是"返回了 200 就算过"。尺寸比能证明画幅映射生效，颜色能证明编辑真的作用在原图。
"""
import base64
import json
import os
import struct
import sys
import time
import urllib.error
import urllib.request
import zlib

BASE = os.environ.get("QWEN2API_BASE", "https://qwen2api.1task.cn").rstrip("/")
KEY = os.environ.get("QWEN2API_KEY", "")
MODEL = os.environ.get("QWEN2API_MODEL", "qwen3.7-plus")
TIMEOUT = int(os.environ.get("QWEN2API_TIMEOUT", "180"))

# 外部可访问的图片 URL（用来测 http(s) 形态）。
# 不依赖单一站点：可被环境变量覆盖。
# 纯红 96x96（颜色参数写成同色文字 ⇒ 整张就是纯色，断言可靠）。
URL_PNG = os.environ.get("QWEN2API_URL_PNG", "https://placehold.co/96x96/dc1414/dc1414.png")

# 关掉环境代理：HTTP_PROXY 会把带 Authorization 的 POST 吞掉（实测踩过）。
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

ROWS = []


def png(w, h, rgb):
    """生成纯色 PNG（96x96 才够模型辨认，1x1 有的模型会说"看不清"）。"""
    raw = b""
    for _ in range(h):
        raw += b"\x00" + bytes(rgb) * w

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr)
            + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


RED = png(96, 96, (220, 20, 20))
GREEN = png(96, 96, (20, 160, 20))

ASK = "这张图的主色调是什么？只回答一个颜色词（中文或英文均可），不要解释。"

# 带文字的图：让模型"读出字符"是最硬的多模态证据（猜不出来）。
URL_OCR = os.environ.get("QWEN2API_URL_OCR", "https://placehold.co/240x120/ffffff/000000.png?text=HELLO42")


def _call_once(path, payload, timeout):
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(payload).encode(),
        headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.time()
    try:
        with _OPENER.open(req, timeout=timeout) as resp:
            body = resp.read()
            return resp.status, body, int((time.time() - t0) * 1000)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), int((time.time() - t0) * 1000)
    except Exception as e:  # noqa: BLE001 - 探测脚本，任何异常都要落成一行结果
        return 0, str(e).encode(), int((time.time() - t0) * 1000)


def call(path, payload, timeout=TIMEOUT, retry=2):
    """发一次请求，返回 (status, body_bytes, elapsed_ms)。

    只对**上游限流**重试：实测 502 + `quota_limit: 目前服务访问量较大`
    是账号级偶发（换个号就好），不是功能问题；400/401 这类确定性错误不重试，
    否则会把"契约不对"掩盖成"偶尔失败"。
    """
    total = 0
    status, body = 0, b""
    for attempt in range(retry + 1):
        status, body, ms = _call_once(path, payload, timeout)
        total += ms
        low = body.lower()
        transient = status in (429, 502, 503) and (b"quota_limit" in low or b"\xe8\xae\xbf\xe9\x97\xae\xe9\x87\x8f" in low
                                                   or b"upstream_error" in low)
        if status == 200 or not transient or attempt == retry:
            return status, body, total
        time.sleep(3 + attempt * 3)
    return status, body, total


def chat_text(body):
    """从非流式 chat 响应里取正文 + usage。"""
    try:
        j = json.loads(body)
    except Exception:
        return None, None, body[:200].decode("utf-8", "replace")
    if "error" in j:
        return None, None, j["error"].get("message", "")
    msg = j.get("choices", [{}])[0].get("message", {})
    return msg.get("content", ""), j.get("usage"), ""


def record(name, ok, detail, ms):
    ROWS.append({"case": name, "ok": bool(ok), "detail": detail, "ms": ms})
    print("%s %-42s %6dms  %s" % ("✅" if ok else "❌", name, ms, detail), flush=True)


def expect_color(name, body, want, ms):
    text, usage, err = chat_text(body)
    if text is None:
        record(name, False, "错误: " + (err or "无正文"), ms)
        return
    low = text.lower()
    hit = ("红" in text) if want == "red" else ("绿" in text)
    hit = hit or (want in low)
    it = (usage or {}).get("prompt_tokens")
    record(name, hit, "回答=%r tokens=%s" % (text.strip()[:40], it), ms)


def case_nonstream(name, part):
    status, body, ms = call("/v1/chat/completions", {
        "model": MODEL,
        "thinking": "fast",
        "stream": False,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": ASK}, part]}],
    })
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
        return
    expect_color(name, body, "red", ms)


def case_stream():
    name = "C. chat 流式（SSE）"
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=json.dumps({
            "model": MODEL, "thinking": "fast", "stream": True,
            "messages": [{"role": "user", "content": [
                {"type": "text", "text": ASK},
                {"type": "image_url", "image_url": {"url": "data:image/png;base64," + base64.b64encode(GREEN).decode()}},
            ]}],
        }).encode(),
        headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.time()
    text, err = "", ""
    try:
        with _OPENER.open(req, timeout=TIMEOUT) as resp:
            for line in resp:
                line = line.decode("utf-8", "replace").strip()
                if not line.startswith("data: "):
                    continue
                data = line[6:]
                if data == "[DONE]":
                    break
                try:
                    chunk = json.loads(data)
                except Exception:
                    continue
                if "error" in chunk:
                    err = chunk["error"].get("message", "")
                    break
                for ch in chunk.get("choices") or []:
                    text += (ch.get("delta") or {}).get("content") or ""
    except Exception as e:  # noqa: BLE001
        err = str(e)
    ms = int((time.time() - t0) * 1000)
    if err:
        record(name, False, "流内错误: " + err[:120], ms)
        return
    hit = "绿" in text or "green" in text.lower()
    record(name, hit, "回答=%r" % text.strip()[:40], ms)


def case_anthropic():
    name = "D. /v1/messages（Anthropic image.source）"
    status, body, ms = call("/v1/messages", {
        "model": MODEL, "max_tokens": 64, "stream": False,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": ASK},
            {"type": "image", "source": {
                "type": "base64", "media_type": "image/png",
                "data": base64.b64encode(RED).decode()}},
        ]}],
    })
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
        return
    try:
        j = json.loads(body)
        text = "".join(b.get("text", "") for b in j.get("content", []))
    except Exception:
        record(name, False, "响应不是 JSON: " + body[:120].decode("utf-8", "replace"), ms)
        return
    hit = "红" in text or "red" in text.lower()
    record(name, hit, "回答=%r in_tokens=%s" % (text.strip()[:40], (j.get("usage") or {}).get("input_tokens")), ms)


def case_responses():
    name = "E. /v1/responses（input_image）"
    status, body, ms = call("/v1/responses", {
        "model": MODEL, "stream": False,
        "input": [{"role": "user", "content": [
            {"type": "input_text", "text": ASK},
            {"type": "input_image", "image_url": "data:image/png;base64," + base64.b64encode(GREEN).decode()},
        ]}],
    })
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
        return
    try:
        j = json.loads(body)
        text = ""
        for item in j.get("output", []):
            for c in item.get("content", []):
                text += c.get("text", "")
    except Exception:
        record(name, False, "响应不是 JSON: " + body[:120].decode("utf-8", "replace"), ms)
        return
    hit = "绿" in text or "green" in text.lower()
    record(name, hit, "回答=%r" % text.strip()[:40], ms)


def case_ocr():
    """读图里的文字 —— 比"什么颜色"更难蒙对。"""
    name = "G. URL 文字图（读字符）"
    status, body, ms = call("/v1/chat/completions", {
        "model": MODEL, "thinking": "fast", "stream": False,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": "图里写的文字是什么？只回答那几个字符，不要解释。"},
            {"type": "image_url", "image_url": {"url": URL_OCR}}]}],
    })
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
        return
    text, usage, err = chat_text(body)
    if text is None:
        record(name, False, "错误: " + (err or "无正文"), ms)
        return
    low = text.lower().replace(" ", "")
    hit = "hello42" in low or "hello 42" in low
    record(name, hit, "回答=%r tokens=%s" % (text.strip()[:40], (usage or {}).get("prompt_tokens")), ms)


# ---------- 出图用例：把产物下载回来验尺寸与像素 ----------

def decode_png(raw):
    """极简 PNG 解码（8bit、非隔行、color type 0/2/4/6），返回 (w,h,ch,data)。"""
    assert raw[:8] == b"\x89PNG\r\n\x1a\n", "不是 PNG"
    pos, w, h, ct, idat = 8, 0, 0, 0, b""
    while pos < len(raw):
        ln = struct.unpack(">I", raw[pos:pos + 4])[0]
        typ = raw[pos + 4:pos + 8]
        data = raw[pos + 8:pos + 8 + ln]
        if typ == b"IHDR":
            w, h, bd, ct, _, _, inter = struct.unpack(">IIBBBBB", data)
            if bd != 8 or inter != 0:
                raise ValueError("只支持 8bit 非隔行: bd=%d inter=%d" % (bd, inter))
        elif typ == b"IDAT":
            idat += data
        elif typ == b"IEND":
            break
        pos += 12 + ln
    ch = {0: 1, 2: 3, 4: 2, 6: 4}[ct]
    stride = w * ch
    buf = zlib.decompress(idat)
    out = bytearray(h * stride)
    prev = bytearray(stride)
    p = 0
    for y in range(h):
        f = buf[p]
        p += 1
        line = bytearray(buf[p:p + stride])
        p += stride
        if f == 1:
            for i in range(ch, stride):
                line[i] = (line[i] + line[i - ch]) & 255
        elif f == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 255
        elif f == 3:
            for i in range(stride):
                left = line[i - ch] if i >= ch else 0
                line[i] = (line[i] + ((left + prev[i]) >> 1)) & 255
        elif f == 4:
            for i in range(stride):
                a = line[i - ch] if i >= ch else 0
                b = prev[i]
                c = prev[i - ch] if i >= ch else 0
                pa, pb, pc = abs(b - c), abs(a - c), abs(a + b - 2 * c)
                pr = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pr) & 255
        out[y * stride:(y + 1) * stride] = line
        prev = line
    return w, h, ch, bytes(out)


def pick(img, fx, fy):
    """按相对坐标取一个像素。"""
    w, h, ch, data = img
    x, y = min(int(w * fx), w - 1), min(int(h * fy), h - 1)
    i = (y * w + x) * ch
    return data[i], data[i + 1], data[i + 2]


def color_of(rgb):
    """按通道主导关系判色，不用固定阈值 —— 深蓝 (11,41,87) 这种亮度低但色相明确的也要判对。"""
    r, g, b = rgb
    mx, mn = max(rgb), min(rgb)
    if mx - mn < 30:                      # 无彩度：白/灰/黑
        if mx > 200:
            return "白"
        if mx < 70:
            return "黑"
        return "灰"
    if b == mx and b > r * 1.4 and b > g * 1.2:
        return "蓝"
    if g == mx and g > r * 1.3 and g > b * 1.2:
        return "绿"
    if r == mx and r > g * 1.6 and r > b * 1.4:
        return "红"
    if r > 150 and g > 130 and b < g * 0.7:
        return "黄"
    return "其它%s" % (rgb,)


def download(url, tag="", tries=6):
    """退避下载产物：刚生成的几秒 CDN 还没转存完（实测会拿到 226 字节 HTML）。
    tag 非空时顺手存一份到 /tmp/<tag>.png，方便人工复核判定是不是误杀。"""
    last = b""
    for i in range(tries):
        time.sleep(1 + 2 * i)
        req = urllib.request.Request(url, headers={
            "Referer": "https://chat.qwen.ai/",
            "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
                          "(KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36",
        })
        try:
            with _OPENER.open(req, timeout=90) as resp:
                last = resp.read()
            if last[:4] == b"\x89PNG":
                if tag:
                    try:
                        open("/tmp/%s.png" % tag, "wb").write(last)
                    except Exception:
                        pass
                return last
        except Exception:
            pass
    raise RuntimeError("产物下载失败（最后一次 %d 字节）" % len(last))


def first_url(body):
    j = json.loads(body)
    if "error" in j:
        raise RuntimeError(j["error"].get("message", "")[:160])
    data = j.get("data") or []
    if not data or not data[0].get("url"):
        raise RuntimeError("没有产物 url: %s" % body[:160])
    return data[0]["url"]


def case_gen_generations():
    """出图①：generations + URL 参考图（原图白底黑字 HELLO42，改成蓝底）。"""
    name = "GEN1. /v1/images/generations + 参考图（URL）"
    status, body, ms = call("/v1/images/generations", {
        "model": "qwen-image-3.0", "thinking": "fast",
        "prompt": "把背景颜色改成深蓝色，保留图中的文字不变",
        "image": URL_OCR,
    }, timeout=300)
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
        return
    try:
        url = first_url(body)
        img = decode_png(download(url, "gen1"))
    except Exception as e:  # noqa: BLE001
        record(name, False, "产物不可用: %s" % e, ms)
        return
    w, h, ch, _ = img
    base = pick(img, 0.02, 0.02)       # 左上角：纯背景
    ratio_ok = abs(w / h - 2.0) < 0.1  # 参考图 320x160 = 2:1
    ok = color_of(base) == "蓝" and ratio_ok
    record(name, ok, "产物 %dx%d 左上=%s(%s) 比例%s" % (
        w, h, color_of(base), base, "2:1✓" if ratio_ok else "≠2:1✗"), ms)


def case_gen_edits():
    """出图②：/v1/images/edits（multipart 真上传文件）。"""
    name = "GEN2. /v1/images/edits（multipart）"
    boundary = "----qwen2api" + str(int(time.time() * 1000))
    ref = png(320, 160, (255, 255, 255))
    parts = []
    for k, v in (("prompt", "把背景颜色改成深蓝色，保留图中的文字不变"), ("thinking", "fast")):
        parts.append(("--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n"
                      % (boundary, k, v)).encode())
    parts.append(("--%s\r\nContent-Disposition: form-data; name=\"image\"; filename=\"ref.png\"\r\n"
                  "Content-Type: image/png\r\n\r\n" % boundary).encode() + ref + b"\r\n")
    parts.append(("--%s--\r\n" % boundary).encode())
    payload = b"".join(parts)
    req = urllib.request.Request(BASE + "/v1/images/edits", data=payload, headers={
        "Authorization": "Bearer " + KEY,
        "Content-Type": "multipart/form-data; boundary=" + boundary,
    }, method="POST")
    t0 = time.time()
    try:
        with _OPENER.open(req, timeout=300) as resp:
            status, body = resp.status, resp.read()
    except urllib.error.HTTPError as e:
        status, body = e.code, e.read()
    except Exception as e:  # noqa: BLE001
        record(name, False, "请求失败: %s" % e, 0)
        return
    ms = int((time.time() - t0) * 1000)
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
        return
    try:
        img = decode_png(download(first_url(body), "gen_e2e"))
    except Exception as e:  # noqa: BLE001
        record(name, False, "产物不可用: %s" % e, ms)
        return
    w, h, ch, _ = img
    base = pick(img, 0.02, 0.02)
    ok = color_of(base) == "蓝" and abs(w / h - 2.0) < 0.1
    record(name, ok, "产物 %dx%d 左上=%s(%s)" % (w, h, color_of(base), base), ms)


def case_gen_i2i():
    """出图③：两张参考图融合（左红右绿），验方形 + 左右像素。"""
    name = "GEN3. 双参考图 i2i（左红右绿）"
    red = "data:image/png;base64," + base64.b64encode(png(160, 160, (220, 20, 20))).decode()
    green = "data:image/png;base64," + base64.b64encode(png(160, 160, (20, 120, 30))).decode()
    status, body, ms = call("/v1/images/generations", {
        "model": "qwen-image-3.0", "thinking": "fast", "size": "1:1",
        "prompt": "把这两张图融合成一张方图：左半边是红色、右半边是绿色，两色之间直接分界",
        "image": [red, green],
    }, timeout=300)
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
        return
    try:
        img = decode_png(download(first_url(body), "gen_e2e"))
    except Exception as e:  # noqa: BLE001
        record(name, False, "产物不可用: %s" % e, ms)
        return
    w, h, ch, _ = img
    left, right = pick(img, 0.25, 0.5), pick(img, 0.75, 0.5)
    ok = (color_of(left) == "红" and color_of(right) == "绿"
          and abs(w - h) <= max(8, w // 20))
    record(name, ok, "产物 %dx%d 左=%s 右=%s" % (w, h, color_of(left), color_of(right)), ms)


def case_negative():
    name = "F1. 坏引用 → 400"
    status, body, ms = call("/v1/chat/completions", {
        "model": MODEL, "stream": False,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": ASK},
            {"type": "image_url", "image_url": {"url": ""}},
        ]}],
    })
    ok = status == 400 and b"image" in body
    record(name, ok, "HTTP %d %s" % (status, body[:110].decode("utf-8", "replace")), ms)

    name = "F2. 21 张 → 400"
    parts = [{"type": "text", "text": "这些图"}]
    parts += [{"type": "image_url", "image_url": "https://x/%d.png" % i} for i in range(21)]
    status, body, ms = call("/v1/chat/completions", {
        "model": MODEL, "stream": False,
        "messages": [{"role": "user", "content": parts}],
    })
    ok = status == 400 and b"image" in body
    record(name, ok, "HTTP %d %s" % (status, body[:110].decode("utf-8", "replace")), ms)


def main():
    if not KEY:
        print("缺少 QWEN2API_KEY", file=sys.stderr)
        return 2
    print("base=%s model=%s\n" % (BASE, MODEL), flush=True)

    b64_red = base64.b64encode(RED).decode()
    uri_red = "data:image/png;base64," + b64_red

    # A. 五种写法
    case_nonstream("A1. image_url 对象 + data URI",
                   {"type": "image_url", "image_url": {"url": uri_red}})
    case_nonstream("A2. image_url 字符串 + data URI",
                   {"type": "image_url", "image_url": uri_red})
    case_nonstream("A3. image_url 对象 + 裸 base64",
                   {"type": "image_url", "image_url": {"url": b64_red}})
    case_nonstream("A4. image_url 对象 + http URL（纯红）",
                   {"type": "image_url", "image_url": {"url": URL_PNG}})
    case_nonstream("A5. input_image（Responses 写法）",
                   {"type": "input_image", "image_url": uri_red})

    # B. 多图：同一轮两张，考顺序（红在前、绿在后）
    name = "B. 多图（红+绿，按序指代）"
    status, body, ms = call("/v1/chat/completions", {
        "model": MODEL, "thinking": "fast", "stream": False,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": "我发两张图。第一张的主色调是什么？第二张呢？"
                                     "只回答两个颜色词，格式：第一张=X，第二张=Y"},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64," + b64_red}},
            {"type": "image_url", "image_url": {"url": "data:image/png;base64," + base64.b64encode(GREEN).decode()}},
        ]}],
    })
    if status != 200:
        record(name, False, "HTTP %d %s" % (status, body[:160].decode("utf-8", "replace")), ms)
    else:
        text, usage, err = chat_text(body)
        if text is None:
            record(name, False, "错误: " + (err or "无正文"), ms)
        else:
            low = text.lower()
            ok = ("红" in text or "red" in low) and ("绿" in text or "green" in low)
            record(name, ok, "回答=%r tokens=%s" % (text.strip()[:60], (usage or {}).get("prompt_tokens")), ms)

    case_stream()
    case_ocr()
    case_anthropic()
    case_responses()
    case_negative()

    if "--with-gen" in sys.argv:
        print("\n--- 出图入口（真实消耗配额）---", flush=True)
        case_gen_generations()
        case_gen_edits()
        case_gen_i2i()

    ok = sum(1 for r in ROWS if r["ok"])
    print("\n=== %d/%d 通过 ===" % (ok, len(ROWS)))
    out = os.environ.get("QWEN2API_OUT", "")
    if out:
        json.dump({"base": BASE, "model": MODEL, "ts": time.strftime("%F %T"), "rows": ROWS},
                  open(out, "w"), ensure_ascii=False, indent=2)
        print("结果写入", out)
    return 0 if ok == len(ROWS) else 1


if __name__ == "__main__":
    sys.exit(main())
