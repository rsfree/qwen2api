#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""qwen2api /v1/images 全形态测试。

两条线：
  A. 契约形态（零出图）：参数校验、载体歧义、错误码 —— 断言 4xx 且不打上游。
  B. 生成形态（计费）：t2i / i2i 各载体 × 画幅 × 张数 × 输出格式 × 模型名。

产物校验不看"返回了 200"，看**图本身**：
  · magic bytes（PNG/JPEG）—— 不看 Content-Type
  · IHDR 宽高 —— 证明 size 映射真的生效
  · 象限/半幅颜色采样 —— 证明参考图真的被上游消费（而不是被悄悄丢掉当 t2i）

用法：
  QWEN2API_BASE=https://qwen2api.1task.cn \
  QWEN2API_KEY=sk-... \
  python3 scripts/images_fulltest.py [--only A|B|all] [--workers 8]
"""

import argparse
import base64
import io
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from concurrent.futures import ThreadPoolExecutor

from PIL import Image

BASE = os.environ.get("QWEN2API_BASE", "https://qwen2api.1task.cn").rstrip("/")
KEY = os.environ.get("QWEN2API_KEY", "")
WORK = "/tmp/qwen2api-images"
os.makedirs(WORK, exist_ok=True)

# 环境里的 HTTP(S)_PROXY 会把内网/公网请求都劫走，且会吞掉带 Authorization 的 POST。
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

_print_lock = threading.Lock()


def log(msg):
    with _print_lock:
        print(msg, flush=True)


# ---------------- HTTP ----------------


def http(method, path, body=None, headers=None, timeout=300):
    url = BASE + path
    data = None
    hdrs = {"Authorization": "Bearer " + KEY} if KEY else {}
    if headers:
        hdrs.update(headers)
    if body is not None:
        if isinstance(body, (dict, list)):
            data = json.dumps(body, ensure_ascii=False).encode()
            hdrs.setdefault("Content-Type", "application/json")
        else:
            data = body
    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    t0 = time.time()
    try:
        with OPENER.open(req, timeout=timeout) as resp:
            raw = resp.read()
            return {"status": resp.status, "ms": int((time.time() - t0) * 1000), "raw": raw,
                    "json": _try_json(raw), "headers": dict(resp.headers)}
    except urllib.error.HTTPError as e:
        raw = e.read()
        return {"status": e.code, "ms": int((time.time() - t0) * 1000), "raw": raw,
                "json": _try_json(raw), "headers": dict(e.headers)}
    except Exception as e:  # 超时 / 连接断
        return {"status": 0, "ms": int((time.time() - t0) * 1000), "raw": b"", "json": None,
                "error": "%s: %s" % (type(e).__name__, e)}


def _try_json(raw):
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:
        return None


def multipart(fields, files):
    """fields: [(name, value)]，files: [(name, filename, mime, bytes)]"""
    boundary = "----qwen2api" + uuid.uuid4().hex
    buf = io.BytesIO()
    for name, value in fields:
        buf.write(("--%s\r\n" % boundary).encode())
        buf.write(('Content-Disposition: form-data; name="%s"\r\n\r\n' % name).encode())
        buf.write(str(value).encode() + b"\r\n")
    for name, filename, mime, raw in files:
        buf.write(("--%s\r\n" % boundary).encode())
        buf.write(('Content-Disposition: form-data; name="%s"; filename="%s"\r\n'
                   % (name, filename)).encode())
        buf.write(("Content-Type: %s\r\n\r\n" % mime).encode())
        buf.write(raw + b"\r\n")
    buf.write(("--%s--\r\n" % boundary).encode())
    return buf.getvalue(), "multipart/form-data; boundary=" + boundary


# ---------------- 产物校验 ----------------


def png_size(raw):
    if raw[:8] == b"\x89PNG\r\n\x1a\n" and len(raw) >= 24:
        w, h = int.from_bytes(raw[16:20], "big"), int.from_bytes(raw[20:24], "big")
        return w, h
    if raw[:2] == b"\xff\xd8":
        i = 2
        while i < len(raw) - 9:
            if raw[i] != 0xFF:
                i += 1
                continue
            m = raw[i + 1]
            if m in (0xC0, 0xC1, 0xC2):
                h = int.from_bytes(raw[i + 5:i + 7], "big")
                w = int.from_bytes(raw[i + 7:i + 9], "big")
                return w, h
            seg = int.from_bytes(raw[i + 2:i + 4], "big")
            i += 2 + seg
    return None


def magic(raw):
    if raw[:8] == b"\x89PNG\r\n\x1a\n":
        return "PNG"
    if raw[:2] == b"\xff\xd8":
        return "JPEG"
    if raw[:4] == b"RIFF" and raw[8:12] == b"WEBP":
        return "WEBP"
    return "?"


def fetch_image(url, timeout=120):
    """CDN 有防盗链：必须带 Referer，否则 404 + 226 字节 HTML。"""
    req = urllib.request.Request(url, headers={
        "Referer": "https://chat.qwen.ai/",
        "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
                      "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36",
    })
    with OPENER.open(req, timeout=timeout) as resp:
        return resp.read()


CLUSTERS = {"red": (255, 0, 0), "green": (0, 255, 0), "blue": (0, 0, 255),
            "yellow": (255, 255, 0), "white": (255, 255, 255), "black": (0, 0, 0)}


def nearest(rgb):
    r, g, b = rgb[:3]
    best, bd = "", 1e9
    for name, (cr, cg, cb) in CLUSTERS.items():
        d = (r - cr) ** 2 + (g - cg) ** 2 + (b - cb) ** 2
        if d < bd:
            best, bd = name, d
    return best


def avg(img, box):
    r, g, b = img.crop(box).resize((32, 32)).convert("RGB").resize((1, 1)).getpixel((0, 0))
    return (r, g, b)


def quadrants(raw):
    img = Image.open(io.BytesIO(raw)).convert("RGB")
    w, h = img.size
    boxes = {"TL": (0, 0, w // 2, h // 2), "TR": (w // 2, 0, w, h // 2),
             "BL": (0, h // 2, w // 2, h), "BR": (w // 2, h // 2, w, h)}
    return {k: avg(img, v) for k, v in boxes.items()}


def halves(raw):
    img = Image.open(io.BytesIO(raw)).convert("RGB")
    w, h = img.size
    return {"left": avg(img, (0, 0, w // 2, h)), "right": avg(img, (w // 2, 0, w, h))}


# ---------------- 参考图 ----------------


def make_refs():
    quad = Image.new("RGB", (768, 768))
    px = quad.load()
    for y in range(768):
        for x in range(768):
            if x < 384 and y < 384:
                px[x, y] = (220, 40, 40)
            elif x >= 384 and y < 384:
                px[x, y] = (40, 180, 60)
            elif x < 384 and y >= 384:
                px[x, y] = (40, 70, 220)
            else:
                px[x, y] = (240, 200, 40)
    qp = os.path.join(WORK, "ref_quad.png")
    quad.save(qp)
    red = Image.new("RGB", (768, 768), (210, 35, 35))
    rp = os.path.join(WORK, "ref_red.png")
    red.save(rp)
    blue = Image.new("RGB", (768, 768), (35, 55, 210))
    bp = os.path.join(WORK, "ref_blue.png")
    blue.save(bp)
    return qp, rp, bp


def data_uri(path, mime="image/png"):
    return "data:%s;base64,%s" % (mime, base64.b64encode(open(path, "rb").read()).decode())


def raw_b64(path):
    return base64.b64encode(open(path, "rb").read()).decode()


# ---------------- 结果收集 ----------------

RESULTS = []
R_LOCK = threading.Lock()


def record(case, status, ms, ok, detail, extra=None):
    row = {"case": case["id"], "name": case["name"], "billed": case["billed"],
           "status": status, "ms": ms, "ok": ok, "detail": detail}
    if extra:
        row.update(extra)
    with R_LOCK:
        RESULTS.append(row)
    log("  %-4s %-38s %s %5.1fs  %s" % (case["id"], case["name"], status, ms / 1000, detail))


def err_info(resp):
    j = resp.get("json") or {}
    e = j.get("error") or {}
    return e.get("type", ""), e.get("param", ""), (e.get("message") or resp.get("error") or "")[:120]


# ---------------- A. 契约形态 ----------------


def phase_a(refs):
    qp, rp, bp = refs
    tiny = data_uri(rp)
    cases = [
        ("A1", "空 prompt", {"prompt": "   "}, 400, "prompt"),
        ("A2", "只有 mask、无 image", {"prompt": "x", "mask": tiny}, 400, "mask"),
        ("A3", "image 为空字符串", {"prompt": "x", "image": ""}, 400, "image"),
        ("A4", "image 类型错误（数字）", {"prompt": "x", "image": 123}, 400, "image"),
        ("A5", "image 数组含空串", {"prompt": "x", "image": ["a", ""]}, 400, "image"),
        ("A6", "n 超上限 5", {"prompt": "x", "n": 5}, 400, "n"),
        ("A7", "参考图 21 张（>20）", {"prompt": "x", "image": [tiny] * 21}, 400, "image"),
        ("A8", "非法 JSON", None, 400, ""),
        ("A9", "无 API Key", {"prompt": "x"}, 401, ""),
        ("A10", "生成模型调 chat", {"model": "qwen-image-3.0",
                                    "messages": [{"role": "user", "content": "hi"}]}, 400, ""),
        ("A11", "GET 打 POST 路由", None, 405, ""),
        ("A12", "mask + image（文档说应忽略）", {"prompt": "x", "image": tiny, "mask": tiny}, 200, ""),
    ]
    out = []
    for cid, name, body, want, want_param in cases:
        case = {"id": cid, "name": name, "billed": False}
        if cid == "A8":
            resp = http("POST", "/v1/images/generations", b"{not json",
                        {"Content-Type": "application/json"})
        elif cid == "A9":
            global KEY
            keep, KEY = KEY, ""
            try:
                resp = http("POST", "/v1/images/generations", body)
            finally:
                KEY = keep
        elif cid == "A10":
            resp = http("POST", "/v1/chat/completions", body)
        elif cid == "A11":
            resp = http("GET", "/v1/images/generations")
        else:
            resp = http("POST", "/v1/images/generations", body)

        typ, param, msg = err_info(resp)
        if cid == "A12":
            ok = resp["status"] == 200
            detail = "200 收下（mask 被忽略）" if ok else \
                     "违反文档：%d %s" % (resp["status"], msg)
        elif cid == "A9":
            ok = resp["status"] == want
            detail = "401" if ok else "%d" % resp["status"]
        elif cid == "A11":
            ok = resp["status"] == want
            detail = "405" if ok else "%d" % resp["status"]
        else:
            ok = resp["status"] == want and (not want_param or param == want_param)
            detail = "%d %s/%s" % (resp["status"], typ, param)
            if resp["status"] == want and want_param and param != want_param:
                detail += " ← 期望 param=%s" % want_param
        record(case, resp["status"], resp["ms"], ok, detail)
        out.append(case["id"])
    return out


# ---------------- B. 生成形态 ----------------


def datum_of(resp):
    j = resp.get("json") or {}
    d = j.get("data")
    return d if isinstance(d, list) else []


def check_image(resp, want_ratio=None, expect_colors=None):
    """返回 (ok, detail, 附加信息)。"""
    datums = datum_of(resp)
    if resp["status"] != 200:
        typ, param, msg = err_info(resp)
        return False, "%d %s %s" % (resp["status"], typ, msg), {}
    if not datums:
        return False, "200 但 data 为空", {}

    info = {"count": len(datums)}
    d0 = datums[0]
    if d0.get("b64_json"):
        raw = base64.b64decode(d0["b64_json"])
        src = "b64"
    elif d0.get("url"):
        try:
            raw = fetch_image(d0["url"])
        except Exception as e:
            return False, "拿到 url 但下载失败: %s" % e, info
        src = "url"
    else:
        return False, "既无 url 也无 b64_json", info

    info["bytes"] = len(raw)
    info["magic"] = magic(raw)
    if info["magic"] not in ("PNG", "JPEG"):
        return False, "不是图片（%s, %d 字节）" % (info["magic"], len(raw)), info
    size = png_size(raw)
    if not size:
        return False, "读不到宽高", info
    info["size"] = "%dx%d" % size

    detail = "%s %s %.2fMB" % (src, info["size"], len(raw) / 1048576)
    if d0.get("url") and d0.get("b64_json"):
        return False, "url 与 b64_json 同时返回（应互斥）", info

    if want_ratio:
        w, h = size
        got = w / h
        exp = {"1:1": 1.0, "16:9": 16 / 9, "9:16": 9 / 16, "4:3": 4 / 3, "3:4": 3 / 4}[want_ratio]
        if abs(got - exp) / exp > 0.03:
            return False, detail + " 比例不符（期望 %s）" % want_ratio, info
        detail += " 比例✓" + want_ratio

    if expect_colors:
        try:
            if "quad" in expect_colors:
                q = quadrants(raw)
                info["quad"] = {k: list(v) for k, v in q.items()}
                got = {k: nearest(v) for k, v in q.items()}
                info["quad_names"] = got
                exp = expect_colors["quad"]
                bad = {k: (got[k], exp[k]) for k in exp if got[k] != exp[k]}
                if bad:
                    return False, detail + " 象限色不符 %s" % bad, info
                detail += " 四色象限✓"
            if "halves" in expect_colors:
                hv = halves(raw)
                info["halves"] = {k: list(v) for k, v in hv.items()}
                got = {k: nearest(v) for k, v in hv.items()}
                info["halves_names"] = got
                if got != expect_colors["halves"]:
                    return False, detail + " 左右色不符 %s" % got, info
                detail += " 左红右蓝✓"
            if "bluish" in expect_colors:
                r, g, b = avg(Image.open(io.BytesIO(raw)).convert("RGB"),
                              (0, 0) + Image.open(io.BytesIO(raw)).size)
                info["avg"] = [r, g, b]
                if not (b > r + 25 and b > g + 25):
                    return False, detail + " 主色调不是蓝 (%d,%d,%d)" % (r, g, b), info
                detail += " 主色蓝✓"
        except Exception as e:
            return False, detail + " 颜色采样失败: %s" % e, info

    if len(datums) > 1:
        urs = {x.get("url") or x.get("b64_json", "")[:24] for x in datums}
        info["distinct"] = len(urs)
        if len(urs) != len(datums):
            return False, detail + " 多张结果重复", info
    return True, detail, info


def phase_b(refs):
    qp, rp, bp = refs
    P = "一只橘猫坐在窗台上，水彩插画风格"
    P_EDIT = "保持这张图四个色块的布局与颜色不变，只在每个色块内部加上细密的纸张纹理"
    P_RED = "保持这张纯红色图片的构图，把整体颜色改成深蓝色"
    P_2REF = "把这两张图并排合成一张：左半边保持红色，右半边保持蓝色"

    quad_uri = data_uri(qp)
    red_uri = data_uri(rp)
    red_b64 = raw_b64(rp)

    cases = []

    def add(cid, name, body=None, ratio=None, colors=None, form=None, check=None,
            expect_b64=False):
        cases.append({"id": cid, "name": name, "billed": True, "body": body,
                      "ratio": ratio, "colors": colors, "form": form, "check": check,
                      "expect_b64": expect_b64})

    add("B1", "t2i 默认（无 size）", {"prompt": P})
    add("B2", "t2i 1024x1024 → 1:1", {"prompt": P, "size": "1024x1024"}, "1:1")
    add("B3", "t2i 1792x1024 → 16:9", {"prompt": P, "size": "1792x1024"}, "16:9")
    add("B4", "t2i 1024x1792 → 9:16", {"prompt": P, "size": "1024x1792"}, "9:16")
    add("B5", "t2i 640x480 → 4:3", {"prompt": P, "size": "640x480"}, "4:3")
    add("B6", "t2i 480x640 → 3:4", {"prompt": P, "size": "480x640"}, "3:4")
    add("B7", "t2i auto", {"prompt": P, "size": "auto"})
    add("B8", "t2i 1600x900（非精确比例 → 最近档）", {"prompt": P, "size": "1600x900"}, "16:9")
    add("B9", "t2i b64_json", {"prompt": P, "response_format": "b64_json"}, expect_b64=True)
    add("B10", "t2i n=2", {"prompt": P, "n": 2})
    add("B11", "t2i n=-1（归一为 1）", {"prompt": P, "n": -1})
    add("B12", "t2i 模型名=UI 文案 Qwen-Image 3.0",
        {"prompt": P, "model": "Qwen-Image 3.0"})
    add("B13", "t2i thinking=thinking（覆盖默认 fast）",
        {"prompt": P, "thinking": "thinking"})
    add("B14", "i2i · data URI", {"prompt": P_EDIT, "image": quad_uri},
        colors={"quad": {"TL": "red", "TR": "green", "BL": "blue", "BR": "yellow"}})
    add("B15", "i2i · 裸 base64（红→蓝）", {"prompt": P_RED, "image": red_b64},
        colors={"bluish": True})
    add("B16", "i2i · 数组单元素", {"prompt": P_EDIT, "image": [quad_uri]},
        colors={"quad": {"TL": "red", "TR": "green", "BL": "blue", "BR": "yellow"}})
    add("B17", "i2i · 双参考图（左红右蓝）",
        {"prompt": P_2REF, "image": [red_uri, data_uri(bp)]},
        colors={"halves": {"left": "red", "right": "blue"}})
    add("B18", "edits · multipart image", form=("edit", [("prompt", P_EDIT)],
                                                [("image", "ref_quad.png", "image/png", open(qp, "rb").read())]),
        colors={"quad": {"TL": "red", "TR": "green", "BL": "blue", "BR": "yellow"}})
    add("B19", "edits · multipart image[] ×2", form=("edit", [("prompt", P_2REF)],
                                                     [("image[]", "ref_red.png", "image/png", open(rp, "rb").read()),
                                                      ("image[]", "ref_blue.png", "image/png", open(bp, "rb").read())]),
        colors={"halves": {"left": "red", "right": "blue"}})
    add("B20", "edits · multipart 文本 image=URL", form=("edit_url", [("prompt", P_EDIT), ("image", "__URL__")], []))

    # ---- 先跑 B1 拿一条 CDN URL，给 B20 文本 URL 形态用；顺带验证整条链是否活着
    b1 = cases[0]
    resp = http("POST", "/v1/images/generations", b1["body"], timeout=300)
    ok, detail, info = check_image(resp, None, None)
    record(b1, resp["status"], resp["ms"], ok, detail, info)
    url = (datum_of(resp) or [{}])[0].get("url", "")
    log("  ↳ 参考 URL: %s" % (url[:90] + "…" if url else "(无)"))

    # B20 用 B1 的真实 CDN 直链当"文本 image"（走 URL 下载分支，且要带 Referer 才下得到）。
    for c in cases[1:]:
        if c["id"] == "B20":
            if url:
                c["form"] = ("edit", [("prompt", P_EDIT), ("image", url)], [])
            else:
                c["skip"] = "B1 没拿到 URL"
    return cases


def run_case(c):
    if c.get("skip"):
        record(c, 0, 0, False, "跳过：" + c["skip"])
        return c["id"], False
    if c["form"]:
        kind, fields, files = c["form"]
        body, ct = multipart(fields, files)
        resp = http("POST", "/v1/images/edits", body, {"Content-Type": ct}, timeout=300)
    else:
        resp = http("POST", "/v1/images/generations", c["body"], timeout=300)
    ok, detail, info = check_image(resp, c.get("ratio"), c.get("colors"))
    extra = dict(info)
    if c.get("expect_b64"):
        d0 = (datum_of(resp) or [{}])[0]
        if not d0.get("b64_json") or d0.get("url"):
            ok, detail = False, detail + " b64_json 形态不符（url/b64 应互斥）"
        else:
            detail += " 仅 b64✓"
    if c["id"] == "B10":
        if extra.get("count") != 2:
            ok, detail = False, detail + " n=2 只回 %s 张" % extra.get("count")
        else:
            detail += " n=2✓"
    record(c, resp["status"], resp["ms"], ok, detail, extra)
    return c["id"], ok


def main():
    global ARGS
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="all", choices=["A", "B", "all"])
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--retry", type=int, default=1)
    ARGS = ap.parse_args()

    if not KEY:
        print("缺少 QWEN2API_KEY", file=sys.stderr)
        return 2

    refs = make_refs()
    log("base=%s  workers=%d" % (BASE, ARGS.workers))

    before = http("GET", "/stats")["json"] or {}
    used_before = sum(a.get("image_today", 0) for a in before.get("pool", []))

    if ARGS.only in ("A", "all"):
        log("\n== A. 契约形态（零出图） ==")
        phase_a(refs)

    if ARGS.only in ("B", "all"):
        log("\n== B. 生成形态（计费） ==")
        cases = phase_b(refs)
        # phase_b 内部只同步跑了 B1；其余在下面并发
        rest = [c for c in cases[1:]]
        with ThreadPoolExecutor(max_workers=ARGS.workers) as ex:
            list(ex.map(run_case, rest))

    after = http("GET", "/stats")["json"] or {}
    used_after = sum(a.get("image_today", 0) for a in after.get("pool", []))

    # ---- 汇总
    rows = sorted(RESULTS, key=lambda r: (r["case"][0], int(r["case"][1:])))
    passed = sum(1 for r in rows if r["ok"])
    log("\n== 汇总 %d/%d 通过 · 上游新增出图 %d 张 ==" %
        (passed, len(rows), max(0, used_after - used_before)))
    log("| # | 形态 | HTTP | 用时 | 结果 | 证据 |")
    log("|---|---|---|---|---|---|")
    for r in rows:
        log("| %s | %s | %s | %.1fs | %s | %s |" %
            (r["case"], r["name"], r["status"], r["ms"] / 1000,
             "✅" if r["ok"] else "❌", r["detail"]))

    out = os.path.join(WORK, "result.json")
    json.dump({"base": BASE, "ts": time.strftime("%F %T"), "results": rows,
               "image_used_delta": max(0, used_after - used_before)},
              open(out, "w"), ensure_ascii=False, indent=2)
    log("\n明细: " + out)
    return 0 if passed == len(rows) else 1


if __name__ == "__main__":
    sys.exit(main())
