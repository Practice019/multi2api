#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""回填 request-log.jsonl 里能重算的 tokens（2026-10-20）。

# 背景
上游的 total_tokens 会漏算缓存（实测 loomy 一条：缓存命中 63,360 + 未命中 722 +
输出 483 = 64,565，而 total_tokens 只报 483）。我们把 total() 改成自己加
（照 new-api 的 model/log.go:395），但那**只影响新请求** —— 已落盘的历史行
里 tokens 字段存的还是旧的错值。

# 能回填的条件
日志行里如果同时有 prompt_cache_hit_tokens 和 prompt_cache_miss_tokens，
就能还原真实处理量：
    tokens = hit + miss + out_tokens   （有 out 时，精确）
    tokens = hit + miss                 （无 out 时，下界但远好于旧值）
两者都没有的行（老日志、csg 那些不发缓存字段的）→ **跳过，保持原样**。
用户明确要求：「能回填的回填，不能回填的就算了」。

# 保真策略（重要）
不重新序列化整行 —— 那样会改变字段顺序、浮点格式、键的增删。
只做**定点文本替换**：把 `"tokens":<数字>` 换成新值，其余字节原样保留。
用负向后顾 `(?<![a-z_])"tokens":` 排除 `"out_tokens":` / `"think_tokens":` 等同后缀键。
"""
import json
import os
import re
import shutil
import sys
import time

LOG = r"D:\project_GIT\workbuddy2api\data\request-log.jsonl"

def main():
    if not os.path.exists(LOG):
        print(f"  ⚠ 找不到 {LOG}")
        return 1

    stamp = time.strftime("%Y%m%d%H%M%S")
    bak = f"{LOG}.bak-{stamp}"

    # ---- 第一遍：只统计，不写 ----
    stats = {
        "total": 0,
        "fill_exact": 0,      # 有 hit + miss + out → 精确回填
        "fill_lower": 0,      # 有 hit + miss，无 out → 下界回填
        "skip_no_cache": 0,   # 无缓存字段 → 跳过
        "skip_bad": 0,        # 解析失败 → 跳过
        "sum_before": 0,
        "sum_after": 0,
    }
    lines = []
    with open(LOG, "r", encoding="utf-8", errors="replace") as f:
        for raw in f:
            lines.append(raw)
    print(f"  读到 {len(lines)} 行")

    plan = []   # (行号, 新 tokens 或 None, 旧 tokens)
    for i, raw in enumerate(lines):
        s = raw.strip()
        if not s:
            plan.append((i, None, 0))
            continue
        stats["total"] += 1
        try:
            o = json.loads(s)
        except Exception:
            stats["skip_bad"] += 1
            plan.append((i, None, 0))
            continue

        old = o.get("tokens")
        oldv = old if isinstance(old, (int, float)) else 0
        if oldv and oldv > 0:
            stats["sum_before"] += int(oldv)

        hit = o.get("cache_hit_tokens") or 0
        miss = o.get("cache_miss_tokens") or 0
        out = o.get("out_tokens") or 0
        if not (isinstance(hit, (int, float)) and hit > 0
                and isinstance(miss, (int, float)) and miss > 0):
            stats["skip_no_cache"] += 1
            plan.append((i, None, oldv))
            continue

        if isinstance(out, (int, float)) and out > 0:
            new = int(hit) + int(miss) + int(out)
            stats["fill_exact"] += 1
        else:
            new = int(hit) + int(miss)
            stats["fill_lower"] += 1
        plan.append((i, new, oldv))
        stats["sum_after"] += new if new and new > 0 else int(oldv) if oldv else 0

    # sum_after 要把跳过的行按旧值计入
    for i, new, oldv in plan:
        if new is None and oldv and oldv > 0:
            stats["sum_after"] += int(oldv)

    print("\n=== 统计（写盘前）===")
    print(f"  总行数              {stats['total']}")
    print(f"  可精确回填(hit+miss+out) {stats['fill_exact']}")
    print(f"  可下界回填(hit+miss)     {stats['fill_lower']}")
    print(f"  无缓存字段→跳过          {stats['skip_no_cache']}")
    print(f"  解析失败→跳过            {stats['skip_bad']}")
    print(f"  Σ tokens 回填前     {stats['sum_before']}  = {stats['sum_before']/1e8:.2f} 亿")
    print(f"  Σ tokens 回填后     {stats['sum_after']}  = {stats['sum_after']/1e8:.2f} 亿")

    if stats["fill_exact"] + stats["fill_lower"] == 0:
        print("\n  ⚠ 没有可回填的行，不做任何修改")
        return 0

    # ---- 备份 ----
    shutil.copy2(LOG, bak)
    print(f"\n=== 已备份 → {os.path.basename(bak)} ===")

    # ---- 第二遍：定点替换，只改 "tokens" 字段 ----
    #  负向后顾排除 out_tokens / think_tokens 等同后缀键
    pat = re.compile(r'(?<![A-Za-z_])"tokens"\s*:\s*(-?\d+)')
    out_lines = []
    changed = 0
    for i, raw in enumerate(lines):
        new = plan[i][1]
        if new is None:
            out_lines.append(raw)
            continue
        def repl(m):
            return f'"tokens":{new}'
        newraw, n = pat.subn(repl, raw, count=1)
        if n:
            changed += 1
            out_lines.append(newraw)
        else:
            out_lines.append(raw)   # 正则没命中就原样保留（绝不猜）

    # ---- 原子替换 ----
    tmp = LOG + ".tmp"
    with open(tmp, "w", encoding="utf-8", newline="") as f:
        f.writelines(out_lines)
    os.replace(tmp, LOG)

    print(f"  实际改写行数        {changed}")
    print(f"  （预期 {stats['fill_exact'] + stats['fill_lower']}，两者应相等）")

    # ---- 回读验证 ----
    total = 0
    with open(LOG, "r", encoding="utf-8", errors="replace") as f:
        for raw in f:
            s = raw.strip()
            if not s:
                continue
            try:
                o = json.loads(s)
            except Exception:
                print("  ⚠ 回读解析失败！")
                return 2
            t = o.get("tokens")
            if isinstance(t, (int, float)) and t > 0:
                total += int(t)
    print(f"\n=== 回读验证 ===")
    print(f"  Σ tokens（重算）= {total}  = {total/1e8:.2f} 亿")
    print(f"  与统计一致: {total == stats['sum_after']}")
    return 0

if __name__ == "__main__":
    sys.exit(main())
