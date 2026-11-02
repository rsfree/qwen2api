#!/usr/bin/env python3
"""chat 首字速度测量（通过网关，端到端）。

口径（三个数各说明一件事）：

    hdr      请求发出 → 收到响应头。含选号、建会话（未命中时 3s）、
             建连（冷连接 1.7s）、上游排队。这一段与模型几乎无关。
    content  请求发出 → 第一个正文增量。这是用户真正感知的「首字」。
    gap      content - hdr。上游发完响应头之后多久才吐第一个字，
             这一段才反映模型快慢。

用法：
    python3 scripts/chat_ttft.py --base https://qwen2api.1task.cn --key sk-xxx
    python3 scripts/chat_ttft.py --base https://qwen2api.1task.cn --key sk-xxx \
        --model qwen3.7-plus -n 10

想对比「网关优化前后」，用同一台机器、同一个 key、连续跑：
数字的绝对值受本机到网关的网络影响，看的是**同一口径下的相对变化**。
"""

import argparse
import json
import statistics
import sys
import time
import urllib.error
import urllib.request

DEFAULT_PROMPT = "用一句话解释什么是TCP三次握手"


def one(base, key, model, prompt, timeout):
    """打一发，返回各阶段的时间戳（毫秒，相对请求发出）。"""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
    }).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/chat/completions",
        data=body,
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
    )
    # 不走环境代理：否则本机 HTTP_PROXY 会把 localhost/内网也劫走。
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    t0 = time.time()
    r = opener.open(req, timeout=timeout)
    t_hdr = time.time()

    first_content = first_reason = None
    chunks = 0
    for raw in r:
        ts = time.time()
        line = raw.decode("utf-8", "ignore").strip()
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            break
        try:
            obj = json.loads(payload)
        except ValueError:
            continue
        delta = (obj.get("choices") or [{}])[0].get("delta") or {}
        if delta.get("content"):
            chunks += 1
            if first_content is None:
                first_content = ts
        if delta.get("reasoning_content") and first_reason is None:
            first_reason = ts

    ms = lambda t: None if t is None else round((t - t0) * 1000)
    return {
        "hdr": ms(t_hdr),
        "reasoning": ms(first_reason),
        "content": ms(first_content),
        "chunks": chunks,
    }


def pct(values, p):
    v = sorted(x for x in values if x is not None)
    if not v:
        return None
    return v[min(len(v) - 1, int(len(v) * p / 100))]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="https://qwen2api.1task.cn", help="网关地址")
    ap.add_argument("--key", required=True, help="网关 API Key")
    ap.add_argument("--model", default="qwen3.8-omni-flash", help="模型名")
    ap.add_argument("-n", "--rounds", type=int, default=5, help="采样次数")
    ap.add_argument("--prompt", default=DEFAULT_PROMPT, help="提示词")
    ap.add_argument("--timeout", type=float, default=120, help="单次超时（秒）")
    args = ap.parse_args()

    rows = []
    for i in range(args.rounds):
        try:
            row = one(args.base, args.key, args.model, args.prompt, args.timeout)
        except urllib.error.HTTPError as e:
            print(f"#{i+1} HTTP {e.code}: {e.read()[:200].decode('utf-8', 'ignore')}", file=sys.stderr)
            continue
        except Exception as e:  # noqa: BLE001 - 命令行工具，任何异常都只记一行
            print(f"#{i+1} FAIL: {e}", file=sys.stderr)
            continue
        rows.append(row)
        print(f"#{i+1} hdr={row['hdr']}ms content={row['content']}ms "
              f"chunks={row['chunks']}", flush=True)

    if not rows:
        print("没有任何成功样本", file=sys.stderr)
        return 1

    hdr = [r["hdr"] for r in rows]
    content = [r["content"] for r in rows]
    gap = [r["content"] - r["hdr"] for r in rows if r["content"] and r["hdr"]]

    print()
    print(f"样本 {len(rows)}  模型 {args.model}")
    print(f"hdr      P50={pct(hdr, 50)}ms  P95={pct(hdr, 95)}ms")
    print(f"content  P50={pct(content, 50)}ms  P95={pct(content, 95)}ms   <- 用户感知的首字")
    print(f"gap      P50={pct(gap, 50)}ms  P95={pct(gap, 95)}ms   <- 上游吐字延迟")
    print(f"总时长   P50={pct([r['content'] for r in rows], 50)}ms（仅到首字）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
