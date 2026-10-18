# -*- coding: utf-8 -*-
"""给 DSH 的 llm-pi-ai 配置注入 reasoningEfforts（思考档位）。

⚠ 背景（2026-10-18 查清）：
  · 用户走的是 **llm-pi-ai** 路（~/.dsh/profiles/web/cordis.patch.yml 挂了它）
  · pi-ai 的规则：model 条目**没有** `reasoningEfforts` ⇒ 判定 `reasoning: false` ⇒ **选不了思考深度**
  · 而 `fillReasoning` 那个自动填充开关在 README 里写明"**当前恒为 false**"（供应商 wire 拼写不可移植）
  ⇒ 只能逐条目写。配置里 ~80 个条目，所以用脚本。

⚠ 形状（从 dsh-llm-pi-ai/lib/index.js 的 schema 反推）：
    reasoningEfforts:
      off: "off"          # ⚠ 源码 :577 明确"off 档不需要 wire value"
      low: "low"
      high: "high"
      max: "max"
  ⚠ 档位名必须来自 THINKING_LEVELS（off/minimal/low/medium/high/xhigh/max），
    这里按用户要求只给 **DSH 内置的那 4 档**：off / low / high / max。
  ⚠ wire 值就是发给上游的 `reasoning_effort` 字面量 —— 我们的网关原样透传
    （internal/cline/parse.go:30），而上游对不认识的档位**静默忽略**（cline.go:17）⇒ 填错也不炸。

⚠ 只改**指定的 provider 段**（默认 test = 我们自己的网关），不碰别的 provider。
⚠ 幂等：已有 reasoningEfforts 的条目会跳过（重跑不会加两遍）。
"""
import io
import re
import sys

PATH = r"C:\Users\21877\.dsh\profiles\web\cordis.patch.yml"
TARGET_PROVIDER = sys.argv[1] if len(sys.argv) > 1 else "test"
LEVELS = ["off", "low", "high", "max"]
# ⚠⚠ 这几个档要**显式声明不支持**（值写 null）。
#
# 起因（2026-10-18，用 pi-ai 的 getSupportedThinkingLevels 实测）：
#   pi-ai 的规则是 —— `mapped === null` ⇒ 跳过该档；`xhigh`/`max` ⇒ 必须显式声明；
#   **其余档 ⇒ 默认支持**。所以只写 off/low/high/max 时，它把
#   `minimal` 与 `medium` 也算成"支持" ⇒ 界面上会出现 6 档，
#   而那两个字面量上游并不认（只会静默忽略）⇒ 用户选了等于没选。
#   ⇒ 要精确落到 DSH 内置的那 4 档，必须把多出来的档写成 null ✓
NULL_LEVELS = ["minimal", "medium"]
APPLY = "--apply" in sys.argv

lines = io.open(PATH, encoding="utf-8").read().split("\n")

# ---- 1) 定位目标 provider 段（顶层键下缩进 6 空格的那种）----
seg_start = seg_end = None
for i, ln in enumerate(lines):
    if re.match(r"^ {6}%s:\s*$" % re.escape(TARGET_PROVIDER), ln):
        seg_start = i
        for j in range(i + 1, len(lines)):
            # 段结束 = 回到 6 空格缩进的下一个键，或文件尾
            if re.match(r"^ {6}\S", lines[j]):
                seg_end = j
                break
        else:
            seg_end = len(lines)
        break
if seg_start is None:
    print("⚠ 没找到 provider 段 %r（缩进 6 空格）" % TARGET_PROVIDER)
    raise SystemExit(1)
print("    provider 段 %r：行 %d ~ %d" % (TARGET_PROVIDER, seg_start + 1, seg_end))

# ---- 2) 在段内找每个 model 条目（`- id:` 起、缩进 10 空格）----
#      ⚠ 只在 models: 子段里动手，不碰 modelOverrides / compat 等同缩进的东西。
out, i, added, skipped, in_models = [], 0, 0, 0, False
while i < len(lines):
    ln = lines[i]
    if seg_start <= i < seg_end:
        if re.match(r"^ {8}models:\s*$", ln):
            in_models = True
        elif re.match(r"^ {8}\S", ln) and not re.match(r"^ {8}models:", ln):
            in_models = False   # 离开 models 子段
    if in_models and re.match(r"^ {10}- id:\s*", ln):
        # ⚠ 收集这个条目，直到下一个 `- id:` 或缩进回到 8 空格的键
        item, k = [], i
        while k < len(lines):
            if k > i and (re.match(r"^ {10}- id:", lines[k]) or re.match(r"^ {8}\S", lines[k])):
                break
            item.append(lines[k])
            k += 1
        if any(re.match(r"^ {12}reasoningEfforts:", x) for x in item):
            skipped += 1
        else:
            # ⚠ 插在 `name:` 之后（读数好看；位置本身对 YAML 无要求 —— 都是同一 map 的键）
            pos = 1
            for idx, x in enumerate(item):
                if re.match(r"^ {12}name:", x):
                    pos = idx + 1
                    break
            # ⚠⚠ **键必须加引号**：`off` 是 YAML 1.1 的布尔字面量（同 on/yes/no）——
            #   写成 `off: "off"` 会被解析成 `{False: "off"}`，而 pi-ai 的 schema 要的是
            #   THINKING_LEVELS 里的**字符串** "off" ⇒ 校验会失败。
            #   ⚠ 这正是 dsh-model-meta-autofill 的 README 说"各供应商 wire 拼写不可移植、
            #     宁可不动"的一个具体实例 —— 连键名的引号都能决定 YAML 的类型。
            block = ([' ' * 12 + "reasoningEfforts:"]
                     + [' ' * 16 + '"%s": "%s"' % (lv, lv) for lv in LEVELS]
                     + [' ' * 16 + '"%s": null' % lv for lv in NULL_LEVELS])
            item = item[:pos] + block + item[pos:]
            added += 1
        out.extend(item)
        i = k
        continue
    out.append(ln)
    i += 1

print("    新增 %d 个条目 · 已有档位跳过 %d 个" % (added, skipped))
if not APPLY:
    print("    (dry-run：没写文件。加 --apply 才写入)")
    raise SystemExit(0)

io.open(PATH, "w", encoding="utf-8", newline="\n").write("\n".join(out))
print("    ✓ 已写入（原文件已有 .bak-* 备份）")
