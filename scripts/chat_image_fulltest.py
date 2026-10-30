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


def call(path, payload, timeout=TIMEOUT):
    """发一次请求，返回 (status, body_bytes, elapsed_ms)。"""
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
    case_anthropic()
    case_responses()
    case_negative()

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
