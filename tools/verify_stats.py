# -*- coding: utf-8 -*-
"""
A2 · 独立核对脚本（只读 data/request-log.jsonl，不碰网络、不碰接口逻辑）

用法：
    python tools/verify_stats.py                 # 人类可读报表
    python tools/verify_stats.py --json          # 机器可比对 JSON（stdout 只有 JSON）
    python tools/verify_stats.py --top 20        # Top N（默认 10）
    python tools/verify_stats.py --file PATH     # 指定日志文件

⚠ 本脚本纯标准库；⚠ 只读文件，不写任何东西。
⚠ 所有分组都会打印「用的是哪个字段名」——字段名来自真实数据的探针，不是硬编码假设。
"""

import argparse
import collections
import datetime
import json
import os
import sys

DEFAULT_REL = os.path.join("data", "request-log.jsonl")

# 探针：口径需要的字段 → 中文说明。脚本会核对它们在真实数据里的存在率。
PROBE_FIELDS = [
    ("status", "状态码（成功率分母/分子）"),
    ("tokens", "token 总量"),
    ("think_tokens", "思考 token 总量"),
    ("model", "按模型分组 · 同时是上游前缀的来源"),
    ("uid", "按账号分组"),
    ("provider", "上游（独立字段，用于核对前缀法）"),
    ("at", "时间戳（本脚本未做按天聚合）"),
    ("seq", "行序号（用于判断日志是否在实时追加）"),
]


def human(n):
    """千分位；非 int 原样返回。"""
    if isinstance(n, bool) or not isinstance(n, int):
        return str(n)
    return "{:,}".format(n)


def rate(success, total):
    """⚠ 防分母为 0：返回 (字符串, 浮点或 None)。"""
    if not total:
        return "N/A（分母 0）", None
    v = success * 100.0 / total
    return "%.2f%%" % v, round(v, 4)


def as_int(v):
    """宽松取整数：None/非数字 → (0, 标记)。返回 (值, 是否异常)。"""
    if isinstance(v, bool):
        return 0, True
    if isinstance(v, int):
        return v, False
    if isinstance(v, float):
        return int(v), False
    if isinstance(v, str):
        try:
            return int(v), False
        except ValueError:
            return 0, True
    return 0, v is not None


def upstream_prefix(model):
    """上游前缀 = model 的 'provider/name' 前缀。⚠ 无 '/' 时判为不可靠，单独落桶。"""
    if not isinstance(model, str) or not model:
        return None, "空 model"
    if "/" not in model:
        return None, "无 '/' 前缀"
    head = model.split("/", 1)[0].strip()
    if not head or head == "-":
        return None, "空前缀"
    return head, None


def read_log(path):
    """单遍流式读取；返回统计所需的原始累积结构 + 文件快照信息。"""
    st_before = os.stat(path)
    raw = {
        "lines": 0,
        "blank": 0,
        "bad_lines": 0,
        "bad_line_samples": [],
        "non_dict": 0,
        "field_present": collections.Counter(),
        "value_type_bad": collections.Counter(),
        "statuses": collections.Counter(),
        "by_model": collections.defaultdict(lambda: [0, 0, 0, 0]),
        "by_uid": collections.defaultdict(lambda: [0, 0, 0, 0]),
        "by_prefix": collections.defaultdict(lambda: [0, 0, 0, 0]),
        "by_provider_field": collections.defaultdict(lambda: [0, 0, 0, 0]),
        "model_provider_pairs": collections.Counter(),
        "prefix_provider_mismatch": collections.Counter(),
        "prefix_fail_rows": 0,
        "prefix_fail_reasons": collections.Counter(),
        "total": 0,
        "success": 0,
        "tokens_sum": 0,
        "tokens_sum_nonneg": 0,
        "tokens_negative_rows": 0,
        "tokens_missing_rows": 0,
        "think_sum": 0,
        "think_missing_rows": 0,
        "seq_min": None,
        "seq_max": None,
    }

    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            raw["lines"] += 1
            line = line.strip()
            if not line:
                raw["blank"] += 1
                continue
            try:
                d = json.loads(line)
            except Exception as exc:  # 坏行不能让整页挂（口径 7：降级不抛）
                raw["bad_lines"] += 1
                if len(raw["bad_line_samples"]) < 3:
                    raw["bad_line_samples"].append("%s | %s" % (type(exc).__name__, line[:120]))
                continue
            if not isinstance(d, dict):
                raw["non_dict"] += 1
                continue

            for k, _cn in PROBE_FIELDS:
                if k in d:
                    raw["field_present"][k] += 1

            raw["total"] += 1

            status = d.get("status")
            if isinstance(status, bool) or not isinstance(status, int):
                raw["value_type_bad"]["status"] += 1
                s_int = None
            else:
                s_int = status
            raw["statuses"][s_int] += 1
            ok = 1 if (s_int is not None and 200 <= s_int < 300) else 0
            raw["success"] += ok

            if "tokens" in d:
                t, bad = as_int(d["tokens"])
                if bad:
                    raw["value_type_bad"]["tokens"] += 1
                raw["tokens_sum"] += t
                if t < 0:
                    raw["tokens_negative_rows"] += 1
                else:
                    raw["tokens_sum_nonneg"] += t
            else:
                raw["tokens_negative_rows"] += 0
                raw["tokens_missing_rows"] += 1
                t = 0

            if "think_tokens" in d:
                th, bad = as_int(d["think_tokens"])
                if bad:
                    raw["value_type_bad"]["think_tokens"] += 1
                raw["think_sum"] += th
            else:
                raw["think_missing_rows"] += 1
                th = 0

            model = d.get("model")
            uid = d.get("uid")
            provider = d.get("provider")

            mk = model if isinstance(model, str) and model else "(无 model 字段)"
            uk = uid if isinstance(uid, str) and uid else "(无 uid 字段)"
            pk = provider if isinstance(provider, str) and provider else "(无 provider 字段)"

            for bucket, key in (
                (raw["by_model"], mk),
                (raw["by_uid"], uk),
                (raw["by_provider_field"], pk),
            ):
                a = bucket[key]
                a[0] += 1
                a[1] += ok
                a[2] += t
                a[3] += th

            head, why = upstream_prefix(model)
            if head is None:
                raw["prefix_fail_rows"] += 1
                raw["prefix_fail_reasons"]["%s（model=%r）" % (why, model)] += 1
            else:
                a = raw["by_prefix"][head]
                a[0] += 1
                a[1] += ok
                a[2] += t
                a[3] += th
                if head != pk:
                    raw["prefix_provider_mismatch"]["%s → %s" % (head, pk)] += 1

            raw["model_provider_pairs"]["%s | %s" % (mk, pk)] += 1

            sq = d.get("seq")
            if isinstance(sq, int) and not isinstance(sq, bool):
                raw["seq_min"] = sq if raw["seq_min"] is None else min(raw["seq_min"], sq)
                raw["seq_max"] = sq if raw["seq_max"] is None else max(raw["seq_max"], sq)

    st_after = os.stat(path)
    raw["_snapshot"] = {
        "size_before": st_before.st_size,
        "size_after": st_after.st_size,
        "mtime_before": st_before.st_mtime,
        "mtime_after": st_after.st_mtime,
        "grew_bytes": st_after.st_size - st_before.st_size,
    }
    return raw


def topn(bucket, n):
    """排序：请求数 desc → tokens desc → key asc（稳定、可复现）。"""
    items = list(bucket.items())
    items.sort(key=lambda kv: (-kv[1][0], -kv[1][2], str(kv[0])))
    out = []
    for key, (req, succ, tok, think) in items[:n]:
        r_str, r_val = rate(succ, req)
        out.append({
            "key": key,
            "requests": req,
            "success": succ,
            "success_rate": r_val,
            "success_rate_str": r_str,
            "tokens": tok,
            "think_tokens": think,
        })
    return out


def group_rows(field_name, note, rows, show_think=True):
    lines = []
    lines.append("  字段名：`%s`%s" % (field_name, note))
    head = "  %-4s %-34s %9s %9s %9s %14s" % ("#", "key", "请求数", "成功数", "成功率", "tokens")
    if show_think:
        head += " %12s" % "think_tokens"
    lines.append(head)
    lines.append("  " + "-" * (len(head) - 2))
    for i, r in enumerate(rows, 1):
        key = r["key"]
        # 纯 ASCII 的 key 过长时截断，避免表格散架
        disp = key if len(key) <= 34 else key[:31] + "..."
        s = "  %-4d %-34s %9s %9s %9s %14s" % (
            i, disp, human(r["requests"]), human(r["success"]),
            r["success_rate_str"], human(r["tokens"]))
        if show_think:
            s += " %12s" % human(r["think_tokens"])
        lines.append(s)
    return lines


def build_report(path, top_n):
    raw = read_log(path)
    total = raw["total"]
    succ = raw["success"]
    r_str, r_val = rate(succ, total)

    snap = raw["_snapshot"]
    lines = []
    A = lines.append

    A("=== A2 · 独立核对（只读 data/request-log.jsonl，自算，不调接口） ===")
    A("文件：%s" % path)
    A("大小：%s 字节 · mtime=%s" % (
        human(snap["size_after"]),
        datetime.datetime.fromtimestamp(snap["mtime_after"]).strftime("%Y-%m-%d %H:%M:%S")))
    A("读入行数：%s（空行 %s · JSON 解析失败 %s · 非对象 %s）" % (
        human(raw["lines"]), human(raw["blank"]), human(raw["bad_lines"]), human(raw["non_dict"])))
    if snap["grew_bytes"] > 0:
        A("⚠ 读取期间文件被追加 %s 字节（实例在跑），以下数字对应「读取起点」的快照。" % human(snap["grew_bytes"]))
    A("")

    A("[0] ⚠ 字段名核对（不照抄简报，按真实数据探针）")
    usable = raw["lines"] - raw["blank"] - raw["bad_lines"] - raw["non_dict"]
    for k, cn in PROBE_FIELDS:
        cnt = raw["field_present"][k]
        if usable == 0:
            flag = "—（无有效行，无法判断）"
        elif cnt == usable:
            flag = "✅"
        else:
            flag = "⚠ 非全覆盖"
        A("  - %-14s 出现 %8s / %8s  %s   （%s）" % ("`%s`" % k, human(cnt),
                                                   human(raw["lines"]), flag, cn))
    for k, c in raw["value_type_bad"].items():
        if c:
            A("  - ⚠ `%s` 有 %s 行类型异常（已按 0 计）" % (k, human(c)))
    A("")

    A("[1] 总量与成功率")
    A("  字段名：`status`（成功 = 200 ≤ status < 300）")
    A("  总请求数：%s" % human(total))
    A("  成功数  ：%s" % human(succ))
    A("  成功率  ：%s" % r_str)
    A("  status 分布（前 10）：")
    for st, c in sorted(raw["statuses"].items(), key=lambda kv: (-kv[1], str(kv[0])))[:10]:
        A("    status=%-6s %8s" % (st if st is not None else "None/非int", human(c)))
    A("")

    A("[2] token 总量")
    A("  字段名：`tokens` 与 `think_tokens`（逐行求和）")
    A("  tokens 总量           ：%s" % human(raw["tokens_sum"]))
    A("  think_tokens 总量     ：%s" % human(raw["think_sum"]))
    A("  ⚠ tokens 原始和里含负数行 %s 条（脏数据），剔除负数后：%s" % (
        human(raw["tokens_negative_rows"]), human(raw["tokens_sum_nonneg"])))
    if raw["tokens_missing_rows"] or raw["think_missing_rows"]:
        A("  ⚠ 缺字段行数：tokens=%s · think_tokens=%s（按 0 计）" % (
            human(raw["tokens_missing_rows"]), human(raw["think_missing_rows"])))
    A("")

    A("[3] 按 model 分组 · Top %d" % top_n)
    A("  字段名：`model`（请求数/成功数/tokens 三列同源）")
    for l in group_rows("model", "", topn(raw["by_model"], top_n))[1:]:
        A(l)
    A("  （去重后的 model 共 %s 个）" % human(len(raw["by_model"])))
    A("")

    A("[4] 按 uid 分组 · Top %d" % top_n)
    A("  字段名：`uid`（账号维度）")
    for l in group_rows("uid", "", topn(raw["by_uid"], top_n))[1:]:
        A(l)
    A("  （去重后的 uid 共 %s 个）" % human(len(raw["by_uid"])))
    A("")

    A("[5] 按上游分组")
    A("  方法 A（简报口径）：`model` 的 'provider/name' 前缀 · split('/',1)[0]")
    A("  ⚠ 消除歧义前先列出全部去重前缀（判断这招是否可靠）：")
    pref_items = sorted(raw["by_prefix"].items(), key=lambda kv: (-kv[1][0], str(kv[0])))
    for head, (req, s, tok, _th) in pref_items:
        rs, _ = rate(s, req)
        A("    %-20s 请求 %8s  成功 %8s  %9s  tokens %14s  （占全部 %.2f%%）" % (
            head, human(req), human(s), rs, human(tok),
            (req * 100.0 / total) if total else 0.0))
    A("  ⚠ 前缀法失效行数：%s / %s" % (human(raw["prefix_fail_rows"]), human(total)))
    for why, c in raw["prefix_fail_reasons"].most_common(10):
        A("      - %s × %s" % (why, human(c)))
    A("")
    A("  方法 B（对照）：独立字段 `provider`")
    for key, (req, s, tok, _th) in sorted(raw["by_provider_field"].items(),
                                          key=lambda kv: (-kv[1][0], str(kv[0]))):
        rs, _ = rate(s, req)
        A("    %-20s 请求 %8s  成功 %8s  %9s  tokens %14s" % (
            key, human(req), human(s), rs, human(tok)))
    A("")
    A("  前缀法 vs provider 字段 一致性：")
    if raw["prefix_provider_mismatch"]:
        mm = sum(raw["prefix_provider_mismatch"].values())
        A("    ⚠ 不一致 %s 行：%s" % (human(mm), dict(raw["prefix_provider_mismatch"])))
        A("    ⇒ 前缀法在真实数据里不能覆盖全部记录")
    else:
        A("    ✅ 前缀法落在桶内的记录，前缀与 `provider` 字段 100% 一致")
    if raw["prefix_fail_rows"]:
        A("    ⇒ 但仍有 %s 行无可用前缀（见上），这部分只能靠 `provider` 字段兜底" % human(raw["prefix_fail_rows"]))
    A("    结论：%s" % ("前缀可靠，可直接当上游用"
                      if raw["prefix_fail_rows"] == 0
                      else "⚠ 前缀不完全可靠（有 %s 行取不到），上游应以 `provider` 字段为准，前缀法只作兜底"
                           % human(raw["prefix_fail_rows"])))
    A("")

    A("[6] 本脚本用到的字段名清单（机器可读同见 --json）")
    A("  总请求数/成功率 → `status`")
    A("  token 总量     → `tokens`")
    A("  think 总量     → `think_tokens`")
    A("  按模型         → `model`")
    A("  按账号         → `uid`")
    A("  按上游(方法A)  → `model` 前缀")
    A("  按上游(方法B)  → `provider`")
    A("  时间           → `at`（本脚本不做按天聚合）")

    report = "\n".join(lines)

    json_doc = {
        "generator": "tools/verify_stats.py",
        "read_only": True,
        "file": path,
        "file_size": snap["size_after"],
        "file_mtime": datetime.datetime.fromtimestamp(snap["mtime_after"]).isoformat(),
        "grew_during_read_bytes": snap["grew_bytes"],
        "lines": raw["lines"],
        "blank_lines": raw["blank"],
        "bad_lines": raw["bad_lines"],
        "non_dict_lines": raw["non_dict"],
        "field_names": {
            "status": "status",
            "tokens": "tokens",
            "think_tokens": "think_tokens",
            "model": "model",
            "uid": "uid",
            "upstream_prefix": "model.split('/',1)[0]",
            "upstream_field": "provider",
            "time": "at",
        },
        "field_present": dict(raw["field_present"]),
        "totals": {
            "requests": total,
            "success": succ,
            "success_rate": r_val,
            "tokens_sum": raw["tokens_sum"],
            "tokens_sum_excluding_negative": raw["tokens_sum_nonneg"],
            "tokens_negative_rows": raw["tokens_negative_rows"],
            "think_tokens_sum": raw["think_sum"],
        },
        "status_breakdown": {str(k): v for k, v in raw["statuses"].items()},
        "by_model": topn(raw["by_model"], top_n),
        "by_model_distinct": len(raw["by_model"]),
        "by_uid": topn(raw["by_uid"], top_n),
        "by_uid_distinct": len(raw["by_uid"]),
        "upstream": {
            "method_a_model_prefix": topn(raw["by_prefix"], top_n),
            "method_a_distinct": len(raw["by_prefix"]),
            "method_a_failed_rows": raw["prefix_fail_rows"],
            "method_a_failed_reasons": dict(raw["prefix_fail_reasons"]),
            "method_b_provider_field": topn(raw["by_provider_field"], top_n),
            "method_b_distinct": len(raw["by_provider_field"]),
            "prefix_vs_provider_mismatch": dict(raw["prefix_provider_mismatch"]),
            "prefix_reliable": raw["prefix_fail_rows"] == 0 and not raw["prefix_provider_mismatch"],
        },
        "seq_range": [raw["seq_min"], raw["seq_max"]],
    }
    return report, json_doc


def main(argv=None):
    # ⚠ Windows 控制台默认 GBK，中文/⚠ 会直接抛 UnicodeEncodeError ⇒ 强制 UTF-8 输出
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass

    ap = argparse.ArgumentParser(description="A2 独立核对：只读 request-log.jsonl 自算统计")
    ap.add_argument("--file", default=None, help="日志路径（默认 <repo>/data/request-log.jsonl）")
    ap.add_argument("--json", action="store_true", help="只输出 JSON（机器比对）")
    ap.add_argument("--top", type=int, default=10, help="分组 Top N（默认 10）")
    args = ap.parse_args(argv)

    path = args.file
    if not path:
        root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        path = os.path.join(root, DEFAULT_REL)

    if not os.path.isfile(path):
        msg = "找不到日志文件：%s" % path
        if args.json:
            print(json.dumps({"error": msg}, ensure_ascii=False, indent=2))
        else:
            print("⚠ " + msg)
        return 2

    report, json_doc = build_report(path, max(1, args.top))

    if args.json:
        print(json.dumps(json_doc, ensure_ascii=False, indent=2, sort_keys=False))
    else:
        print(report)
    return 0


if __name__ == "__main__":
    sys.exit(main())
