# -*- coding: utf-8 -*-
"""
A1 · 独立核对脚本（只读 JSONL 日志，不碰网络、不引用被测 Go 代码）

用途：把 `/admin/stats` 里新增的三个富化维度
（`by_model_detail` / `by_uid_detail` / `by_provider_detail`）
与**用 Python 从头算出来的值**逐项比对，并顺手核对旧字段
（`by_model` / `by_uid` / `by_provider`）没有被改动。

用法：
    # 只看数据事实（不比对接口）
    python tools/verify_byuid_detail.py --file <冻结的日志副本>

    # 逐项比对：--api 传接口响应的 JSON 转储
    python tools/verify_byuid_detail.py --file <冻结副本> --api <stats.json>

退出码：0 = 全部一致；1 = 有差异（差异逐条打印，不只打印"不一致"）。

⚠ 口径（与 Go 侧逐条对齐，任一处理解不同都会导致比对失败 —— 这正是它有意义的原因）：
    * 分桶字段：by_model_detail → `model`（**含空串**，与旧 by_model 一致）
                by_uid_detail   → `uid`（**跳过空串**，与旧 by_uid 一致）
                by_provider_detail → `provider`（空串归 "(未标注)"，与旧 by_provider 一致）
    * req   = 该键的条数
    * ok    = 2xx 条数；fail = status>=400 条数；其余状态码(1xx/3xx)两个都不记
    * tokens      = 只累加 tokens>0（-1 = usage 缺失，不当 0 也不当负数）← 与顶层 tokens 同口径
    * tokens_raw  = 原始和（含负数）
    * think_tokens= 只累加 >0
    * avg_ttfb_ms = 只按 ttfb_ms>0 的样本求平均（整除）；无样本则**缺键**

⚠ 纯标准库；⚠ 只读文件，不写任何东西。
"""

import argparse
import collections
import hashlib
import json
import os
import sys

UNLABELED = "(未标注)"
# (维度名, 日志字段, 是否跳过空值, 空值兜底键)
DETAIL_DIMS = [
    ("by_model_detail", "model", False, ""),
    ("by_uid_detail", "uid", True, ""),
    ("by_provider_detail", "provider", False, UNLABELED),
]
LEGACY_DIMS = [
    ("by_model", "model", False, ""),
    ("by_uid", "uid", True, ""),
]


def load_rows(path):
    """读 JSONL；返回 (条目列表, 坏行数)。坏行与 Go 侧 readAllLocked 一样被跳过。"""
    rows, bad = [], 0
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except Exception:
                bad += 1
                continue
            if not isinstance(obj, dict):
                bad += 1
                continue
            rows.append(obj)
    return rows, bad


def as_int(v, default=0):
    if isinstance(v, bool):
        return default
    if isinstance(v, int):
        return v
    if isinstance(v, float) and v == int(v):
        return int(v)
    return default


def bucket_key(row, field, skip_empty=False, empty_fill=""):
    """取分桶键。空值语义与 Go 侧一致：
    uid 空 ⇒ None（旧 by_uid 就是跳过的）；model 空 ⇒ 保留空串键；provider 空 ⇒ "(未标注)"。
    """
    v = row.get(field)
    s = "" if v is None else str(v)
    if s == "":
        return None if skip_empty else empty_fill
    return s


def compute(rows):
    """严格按上面的口径算出全部维度。"""
    detail = {name: collections.defaultdict(lambda: {
        "req": 0, "ok": 0, "fail": 0, "tokens": 0, "tokens_raw": 0,
        "think_tokens": 0, "ttfb_sum": 0, "ttfb_n": 0}) for name, *_ in DETAIL_DIMS}
    legacy = {name: collections.defaultdict(int) for name, *_ in LEGACY_DIMS}
    legacy_provider = collections.defaultdict(
        lambda: {"calls": 0, "ok": 0, "fail": 0, "credit": 0.0})

    top = {"total": 0, "ok": 0, "fail": 0, "tokens": 0, "think_tokens": 0,
           "ttfb_sum": 0, "ttfb_n": 0, "neg_tokens_rows": 0, "neg_tokens_sum": 0,
           "uid_empty": 0, "provider_empty": 0, "model_empty": 0}

    for r in rows:
        top["total"] += 1
        st = as_int(r.get("status"))
        tok = as_int(r.get("tokens"))
        think = as_int(r.get("think_tokens"))
        ttfb = as_int(r.get("ttfb_ms"))
        credit = r.get("credit") if isinstance(r.get("credit"), (int, float)) else 0

        if 200 <= st < 300:
            top["ok"] += 1
        elif st >= 400:
            top["fail"] += 1
        if tok > 0:
            top["tokens"] += tok
        if tok < 0:
            top["neg_tokens_rows"] += 1
            top["neg_tokens_sum"] += tok
        if think > 0:
            top["think_tokens"] += think
        if ttfb > 0:
            top["ttfb_sum"] += ttfb
            top["ttfb_n"] += 1

        if "" == ("" if r.get("uid") is None else str(r.get("uid"))):
            top["uid_empty"] += 1
        if "" == ("" if r.get("provider") is None else str(r.get("provider"))):
            top["provider_empty"] += 1
        if "" == ("" if r.get("model") is None else str(r.get("model"))):
            top["model_empty"] += 1

        # 旧字段（顶层标量）的按上游计数
        pk = r.get("provider")
        pk = UNLABELED if pk is None or str(pk) == "" else str(pk)
        lp = legacy_provider[pk]
        lp["calls"] += 1
        if 200 <= st < 300:
            lp["ok"] += 1
        elif st >= 400:
            lp["fail"] += 1
        if isinstance(credit, (int, float)) and credit > 0:
            lp["credit"] += credit

        # 旧字段：名称 → 请求数（⚠ 必须走与富化维度**同一套**空值语义，否则会凭空多出 "" 键）
        for name, field, skip_empty, empty_fill in LEGACY_DIMS:
            k = bucket_key(r, field, skip_empty, empty_fill)
            if k is not None:
                legacy[name][k] += 1

        # 富化维度
        for name, field, skip_empty, empty_fill in DETAIL_DIMS:
            k = bucket_key(r, field, skip_empty, empty_fill)
            if k is None:
                continue
            d = detail[name][k]
            d["req"] += 1
            if 200 <= st < 300:
                d["ok"] += 1
            elif st >= 400:
                d["fail"] += 1
            if tok > 0:
                d["tokens"] += tok
            d["tokens_raw"] += tok
            if think > 0:
                d["think_tokens"] += think
            if ttfb > 0:
                d["ttfb_sum"] += ttfb
                d["ttfb_n"] += 1

    out_detail = {}
    for name, *_ in DETAIL_DIMS:
        o = {}
        for k, d in detail[name].items():
            item = {"req": d["req"], "ok": d["ok"], "fail": d["fail"],
                    "tokens": d["tokens"], "tokens_raw": d["tokens_raw"],
                    "think_tokens": d["think_tokens"]}
            # ⚠ 零除守卫：无样本时 avg_ttfb_ms 必须缺键（不是 0）
            if d["ttfb_n"] > 0:
                item["avg_ttfb_ms"] = d["ttfb_sum"] // d["ttfb_n"]
            o[k] = item
        out_detail[name] = o

    return {"top": top,
            "detail": out_detail,
            "legacy": {k: dict(v) for k, v in legacy.items()},
            "legacy_provider": {k: dict(v) for k, v in legacy_provider.items()}}


def report_facts(ref, bad_lines, path, sha, rows):
    t = ref["top"]
    print("=" * 78)
    print("【数据事实】文件: %s" % path)
    print("  sha256 = %s" % sha)
    print("  行数(有效 JSON) = %d · 坏行 = %d" % (len(rows), bad_lines))
    print("  合计: total=%d ok=%d fail=%d tokens=%d tokens_raw(含负)=%d think=%d"
          % (t["total"], t["ok"], t["fail"], t["tokens"],
             t["tokens"] + t["neg_tokens_sum"], t["think_tokens"]))
    print("  ⚠ tokens<0 的行 = %d 行，合计 %d ⇒ 口径甲(原始和)与口径乙(剔负)相差 %d"
          % (t["neg_tokens_rows"], t["neg_tokens_sum"], -t["neg_tokens_sum"]))
    print("  空值行: uid=%d provider=%d model=%d" %
          (t["uid_empty"], t["provider_empty"], t["model_empty"]))
    print("  维度键数: by_model=%d by_uid=%d by_provider=%d"
          % (len(ref["legacy"]["by_model"]), len(ref["legacy"]["by_uid"]),
             len(ref["legacy_provider"])))
    # 交叉校验：明细求和必须等于顶层标量
    print("  ---- 交叉校验（明细求和 vs 顶层标量）----")
    for name, *_ in DETAIL_DIMS:
        m = ref["detail"][name]
        s = {k: sum(d[k] for d in m.values())
             for k in ("req", "ok", "fail", "tokens", "think_tokens")}
        flags = []
        if s["ok"] != t["ok"]:
            flags.append("ok %d≠%d" % (s["ok"], t["ok"]))
        if s["fail"] != t["fail"]:
            flags.append("fail %d≠%d" % (s["fail"], t["fail"]))
        if s["tokens"] != t["tokens"]:
            flags.append("tokens %d≠%d" % (s["tokens"], t["tokens"]))
        if s["think_tokens"] != t["think_tokens"]:
            flags.append("think %d≠%d" % (s["think_tokens"], t["think_tokens"]))
        print("    %-20s req=%d ok=%d fail=%d tokens=%d think=%d  %s"
              % (name, s["req"], s["ok"], s["fail"], s["tokens"], s["think_tokens"],
                 ("✅一致" if not flags else "❌" + " ".join(flags))))
    print("=" * 78)


def compare(path, ref, api):
    """逐项比对。返回差异条数（0 = 全一致）。"""
    bad = [0]
    ok = [0]

    def chk(cond, msg):
        if cond:
            ok[0] += 1
        else:
            bad[0] += 1
            if bad[0] <= 40:
                print("  ❌ %s" % msg)

    print("\n【逐项比对】接口 JSON vs Python 独立算法")
    # 窗口一致性：接口的 total 必须等于我这份文件的有效行数
    api_total = as_int(api.get("total"), -1)
    chk(api_total == ref["top"]["total"],
        "total 不一致：接口=%s 文件=%s（文件没冻结住？）" % (api_total, ref["top"]["total"]))
    for f in ("ok", "fail", "tokens", "think_tokens"):
        chk(as_int(api.get(f), -1) == ref["top"][f],
            "顶层 %s 不一致：接口=%s 本地=%s" % (f, api.get(f), ref["top"][f]))

    # 三个富化维度逐键逐字段
    for name, *_ in DETAIL_DIMS:
        local = ref["detail"][name]
        got = api.get(name)
        if not isinstance(got, dict):
            chk(False, "%s 缺失或类型不对：%r" % (name, got))
            continue
        only_local = sorted(set(local) - set(got))
        only_api = sorted(set(got) - set(local))
        chk(not only_local, "%s 有 %d 个键 Python 独有，例: %s"
            % (name, len(only_local), only_local[:5]))
        chk(not only_api, "%s 有 %d 个键 接口独有，例: %s"
            % (name, len(only_api), only_api[:5]))
        for k in sorted(set(local) & set(got)):
            for field in ("req", "ok", "fail", "tokens", "tokens_raw", "think_tokens"):
                if field not in got[k]:
                    chk(False, "%s[%s] 缺字段 %s（接口=%r）" % (name, k, field, got[k]))
                    continue
                chk(as_int(got[k].get(field), -999999) == local[k][field],
                    "%s[%s].%s 不一致：接口=%s Python=%s"
                    % (name, k, field, got[k].get(field), local[k][field]))
            # avg_ttfb_ms：本地有则必须相等；本地无则接口必须缺键
            if "avg_ttfb_ms" in local[k]:
                chk("avg_ttfb_ms" in got[k],
                    "%s[%s].avg_ttfb_ms 缺失（Python=%s）" % (name, k, local[k]["avg_ttfb_ms"]))
                if "avg_ttfb_ms" in got[k]:
                    chk(as_int(got[k]["avg_ttfb_ms"], -1) == local[k]["avg_ttfb_ms"],
                        "%s[%s].avg_ttfb_ms 不一致：接口=%s Python=%s"
                        % (name, k, got[k]["avg_ttfb_ms"], local[k]["avg_ttfb_ms"]))
            else:
                chk("avg_ttfb_ms" not in got[k],
                    "%s[%s].avg_ttfb_ms 不该出现（该桶没有 ttfb 样本），接口给了 %s"
                    % (name, k, got[k].get("avg_ttfb_ms")))
        print("  %-20s 键数=%d 字段逐项已比 (%d ok / %d bad)"
              % (name, len(local), ok[0], bad[0]))

    # 旧字段不许动
    for name, *_ in LEGACY_DIMS:
        got = api.get(name)
        local = ref["legacy"][name]
        if not isinstance(got, dict):
            chk(False, "旧字段 %s 缺失或类型变了：%r" % (name, got))
            continue
        chk(set(got) == set(local),
            "旧字段 %s 键集变了：接口独有 %s · Python 独有 %s"
            % (name, sorted(set(got) - set(local))[:5], sorted(set(local) - set(got))[:5]))
        diff = [(k, got[k], local.get(k)) for k in set(got) & set(local)
                if as_int(got[k], -1) != local[k]]
        chk(not diff, "旧字段 %s 有 %d 个键的值变了，例 %s" % (name, len(diff), diff[:3]))
        chk(all(isinstance(v, int) for v in got.values()),
            "旧字段 %s 不再是「名称→数字」的形状" % name)
    # by_provider：旧字段的核心计数（calls/ok/fail）也要对得上
    got = api.get("by_provider")
    if isinstance(got, dict):
        local = ref["legacy_provider"]
        chk(set(got) == set(local),
            "by_provider 键集变了：接口=%s Python=%s" % (sorted(got), sorted(local)))
        for k in sorted(set(got) & set(local)):
            for field in ("calls", "ok", "fail"):
                chk(as_int(got[k].get(field), -1) == local[k][field],
                    "by_provider[%s].%s 不一致：接口=%s Python=%s"
                    % (k, field, got[k].get(field), local[k][field]))
    else:
        chk(False, "by_provider 缺失或类型变了：%r" % (got,))

    print("-" * 78)
    print("比对结果：断言通过 %d 条 · 失败 %d 条 ⇒ %s"
          % (ok[0], bad[0], "✅ 全部一致" if bad[0] == 0 else "❌ 有差异"))
    return bad[0]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--file", default=os.path.join("data", "request-log.jsonl"))
    ap.add_argument("--api", default=None, help="/admin/stats 响应的 JSON 文件")
    ap.add_argument("--top", type=int, default=0, help="打印 Top N uid（按 tokens）")
    a = ap.parse_args()

    if not os.path.exists(a.file):
        print("找不到日志文件: %s" % a.file)
        return 2
    with open(a.file, "rb") as f:
        sha = hashlib.sha256(f.read()).hexdigest()
    rows, bad = load_rows(a.file)
    ref = compute(rows)
    report_facts(ref, bad, a.file, sha, rows)

    if a.top:
        m = ref["detail"]["by_uid_detail"]
        print("\n【Top %d uid（按 tokens）】" % a.top)
        for k, v in sorted(m.items(), key=lambda kv: -kv[1]["tokens"])[:a.top]:
            print("  %-22s req=%-7d ok=%-7d fail=%-5d tokens=%-10d ttfb=%s"
                  % (k, v["req"], v["ok"], v["fail"], v["tokens"], v.get("avg_ttfb_ms")))
    if not a.api:
        return 0
    with open(a.api, "r", encoding="utf-8") as f:
        api = json.load(f)
    return 1 if compare(a.file, ref, api) else 0


if __name__ == "__main__":
    sys.exit(main())
