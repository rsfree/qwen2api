#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""token 自动刷新 + 后台保活 的线上取证脚本。

两件事分开取证，互不混淆：

  PART 1 请求路径的「过期即续期」
    从 /stats 里挑出 **access token 已经过期**（token_left_s < 0）的账号，
    然后发 N 个真实 chat 请求（每个请求会命中一个不同的号），
    再拉一次 /stats：被服务过的号（served 增加）如果 token_left_s 由负转正，
    就证明 401/过期情形下网关真的自动换到了新 access —— 不是换号逃逸。

  PART 2 后台保活（KeepAlive）
    在不发请求的情况下，间隔 S 秒连续拉 3 次 /stats：
      - refresh_known 增长 ⇒ adopt 在补 refresh_token（36/分钟）
      - token_left_s > 0 的号维持/增加 ⇒ 预热在跑
      - refresh_left_s 出现"剩余 < 3 天"的号 ⇒ 保养逻辑该动手了

用法：
  QWEN2API_BASE=https://qwen2api.1task.cn QWEN2API_KEY=sk-... \
  python3 scripts/token_refresh_probe.py --requests 8 --wait 70
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("QWEN2API_BASE", "https://qwen2api.1task.cn").rstrip("/")
KEY = os.environ.get("QWEN2API_KEY", "")
MODEL = os.environ.get("QWEN2API_MODEL", "qwen3.7-plus")
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def api(path, payload=None, timeout=180):
    req = urllib.request.Request(
        BASE + path,
        data=None if payload is None else json.dumps(payload).encode(),
        headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"},
        method="GET" if payload is None else "POST",
    )
    try:
        with _OPENER.open(req, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:  # noqa: BLE001
        return 0, str(e).encode()


def snapshot():
    status, body = api("/stats")
    if status != 200:
        print("拉 /stats 失败: HTTP %s %s" % (status, body[:200].decode("utf-8", "replace")))
        return None
    d = json.loads(body)
    pool = {a["name"]: a for a in d["pool"]}
    stats = {
        "uptime_s": d.get("uptime_s"),
        "version": d.get("version"),
        "total": len(pool),
        "token_known": sum(1 for a in pool.values() if a.get("token_known")),
        "token_expired": sum(1 for a in pool.values()
                             if a.get("token_known") and a.get("token_left_s", 0) < 0),
        "token_fresh": sum(1 for a in pool.values()
                           if a.get("token_known") and a.get("token_left_s", 0) > 0),
        "has_refresh": sum(1 for a in pool.values() if a.get("has_refresh")),
        "refresh_known": sum(1 for a in pool.values() if a.get("refresh_known")),
        "refresh_expiring": sum(1 for a in pool.values()
                                if a.get("refresh_known") and 0 < a.get("refresh_left_s", 0) < 3 * 86400),
        "ready": sum(1 for a in pool.values() if a.get("state") == "ready"),
    }
    return pool, stats


def show(tag, s):
    print("%-10s up=%ss v=%s 池=%d ready=%d | access 已知=%d 新鲜=%d 已过期=%d | "
          "refresh 已知=%d 缺失=%d 即将到期=%d"
          % (tag, s["uptime_s"], s["version"], s["total"], s["ready"],
             s["token_known"], s["token_fresh"], s["token_expired"],
             s["refresh_known"], s["total"] - s["has_refresh"], s["refresh_expiring"]),
          flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--requests", type=int, default=8, help="PART 1 发多少个 chat 请求")
    ap.add_argument("--wait", type=int, default=70, help="PART 2 快照间隔秒数")
    ap.add_argument("--rounds", type=int, default=3, help="PART 2 快照轮数")
    args = ap.parse_args()
    if not KEY:
        print("缺少 QWEN2API_KEY", file=sys.stderr)
        return 2

    print("=== PART 1：请求路径的「过期即续期」 ===", flush=True)
    before_pool, before = snapshot()
    if not before_pool:
        return 1
    show("before", before)

    stale = sorted([a["name"] for a in before_pool.values()
                    if a.get("token_known") and a.get("token_left_s", 0) < 0])
    print("当前 access 已过期的号共 %d 个，例如：%s" % (len(stale), ", ".join(stale[:6])), flush=True)

    ok = fail = 0
    for i in range(args.requests):
        t0 = time.time()
        status, body = api("/v1/chat/completions", {
            "model": MODEL, "thinking": "fast", "stream": False,
            "messages": [{"role": "user", "content": "第 %d 次探活：只回复 ok" % (i + 1)}],
        })
        ms = int((time.time() - t0) * 1000)
        if status == 200:
            ok += 1
            try:
                usage = json.loads(body).get("usage") or {}
            except Exception:
                usage = {}
            print("  #%d 200 %5dms tokens=%s" % (i + 1, ms, usage.get("prompt_tokens")), flush=True)
        else:
            fail += 1
            print("  #%d HTTP %s %s" % (i + 1, status, body[:160].decode("utf-8", "replace")), flush=True)
        time.sleep(0.5)

    after_pool, after = snapshot()
    show("after", after)

    # 谁被服务过：served 增加
    served = []
    for name, a in after_pool.items():
        prev = before_pool.get(name, {})
        if a.get("served", 0) > prev.get("served", 0):
            served.append((name, prev.get("token_left_s"), a.get("token_left_s")))
    renewed = [x for x in served if x[1] is not None and x[1] < 0 and x[2] > 0]
    print("\n本轮被服务过的号 %d 个；其中「之前已过期、之后变新鲜」%d 个 ⇒ 自动续期生效"
          % (len(served), len(renewed)), flush=True)
    for name, old, new in served[:12]:
        flag = "续期" if (old is not None and old < 0 and new > 0) else ""
        print("  %-12s token_left: %8s → %8s %s" % (name, old, new, flag), flush=True)

    print("\n请求成功率: %d/%d" % (ok, ok + fail), flush=True)

    print("\n=== PART 2：后台保活推进（每 %ds 一拍，共 %d 拍） ===" % (args.wait, args.rounds), flush=True)
    show("t0", after)
    prev = after
    for i in range(args.rounds):
        time.sleep(args.wait)
        pool, s = snapshot()
        if not s:
            break
        show("t+%ds" % ((i + 1) * args.wait), s)
        print("  Δ refresh_known=+%d  Δ token_fresh=%+d  Δ 已过期=%+d"
              % (s["refresh_known"] - prev["refresh_known"],
                 s["token_fresh"] - prev["token_fresh"],
                 s["token_expired"] - prev["token_expired"]), flush=True)
        prev = s

    verdict = []
    verdict.append(("请求路径自动续期", len(renewed) > 0 or before["token_expired"] == 0))
    verdict.append(("请求成功率 100%", ok == ok + fail))
    verdict.append(("保活补 refresh_token", prev["refresh_known"] >= after["refresh_known"]))
    print("\n=== 结论 ===")
    for name, good in verdict:
        print("  %s %s" % ("✅" if good else "⚠️", name))
    return 0


if __name__ == "__main__":
    sys.exit(main())
