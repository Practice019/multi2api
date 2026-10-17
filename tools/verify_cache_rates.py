# -*- coding: utf-8 -*-
"""
按 agent2api 的 CACHE_WINDOWS 口径，从我们的日志算四档缓存命中率（独立靶子）。

⚠ 它的口径（抄自 report-page.tsx 的 TIP 与 report.rs 的 CACHE_WINDOWS）：
  · 四档窗口 = last10m / last1h / last24h / last7d
  · 命中率 = 缓存读取 Token / 输入 Token        （输入 = 缓存命中 + 未命中）
  · ⚠ 可能 > 100%，**不夹取值、如实展示**
⚠ 本脚本只读，不改任何东西。
"""
import json
import time
from datetime import datetime, timezone

PATH = r"data\request-log.jsonl"
NOW = time.time()
WINDOWS = [("last10m", 10 * 60), ("last1h", 60 * 60), ("last24h", 24 * 60 * 60), ("last7d", 7 * 24 * 60 * 60)]

acc = {k: {"hit": 0, "miss": 0, "n": 0, "tok": 0} for k, _ in WINDOWS}
total = 0
has_hit = has_miss = 0
nonzero = 0
sample = None

with open(PATH, "r", encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            o = json.loads(line)
        except Exception:
            continue
        total += 1
        at = o.get("at") or o.get("At") or o.get("time")
        # ⚠ 时间字段是 RFC3339（带 +08:00）⇒ fromisoformat 在 3.11+ 能吃带偏移的
        t = None
        if isinstance(at, str):
            try:
                t = datetime.fromisoformat(at).timestamp()
            except Exception:
                t = None
        if t is None:
            continue
        h = o.get("cache_hit_tokens")
        m = o.get("cache_miss_tokens")
        if isinstance(h, (int, float)):
            has_hit += 1
        if isinstance(m, (int, float)):
            has_miss += 1
        if (h or 0) or (m or 0):
            nonzero += 1
            if sample is None:
                sample = (at, h, m, o.get("tokens"))
        tk = o.get("tokens")
        for key, win in WINDOWS:
            if NOW - t <= win:
                a = acc[key]
                a["n"] += 1
                a["hit"] += h if isinstance(h, (int, float)) and h > 0 else 0
                a["miss"] += m if isinstance(m, (int, float)) and m > 0 else 0
                if isinstance(tk, (int, float)) and tk > 0:
                    a["tok"] += int(tk)

print("=== 字段可用性（先确认数据在不在）===")
print(f"  总行 = {total}")
print(f"  有 cache_hit_tokens 的行 = {has_hit}")
print(f"  有 cache_miss_tokens 的行 = {has_miss}")
print(f"  ⚠ 任一非 0 的行 = {nonzero}")
print(f"  第一个非 0 样本 = {sample}")

print("\n=== 四档命中率（agent2api 口径：hit / (hit+miss)，不夹取值）===")
print(f"  {'窗口':<10}{'行数':>8}{'hit':>14}{'miss':>14}{'输入':>14}{'命中率':>12}{'tokens':>14}")
for key, _ in WINDOWS:
    a = acc[key]
    den = a["hit"] + a["miss"]
    rate = f"{a['hit']/den*100:.2f}%" if den > 0 else "N/A（分母 0）"
    print(f"  {key:<10}{a['n']:>8}{a['hit']:>14}{a['miss']:>14}{den:>14}{rate:>12}{a['tok']:>14}")
