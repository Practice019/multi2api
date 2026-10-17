# 报表任务 · 背景简报（子代理**只读这一个文件**就够开工）

> ⚠ **这份文件的目的**：让子代理**不必重新阅读背景**。用户原话：
> 「如果是单个工作流总是要**重新阅读背景**，我工作流是希望**并发**完成任务，
>   而且是可以**直接开工不用阅读背景**的。**我的目标是快**。」
> ⇒ 你只需要：**读本文件 → 做你那一个小目标 → 按「验收」自测 → 写报告**。
> ⚠ **不要**再去通读 `webui.html`（9500 行）或任何大文件；本文件已给出全部前提。

---

## 1 · 项目与产物

* 项目：`D:\project_GIT\workbuddy2api`（产品名 **multi2api**）· Go 后端 + **单文件前端** `internal/server/webui.html`。
* 构建：`$env:GOROOT='D:\go-path\go'; $env:GOPATH='C:\Users\21877\go'; $env:GOPROXY='https://goproxy.cn,direct'`
  · 然后用 `D:\go-path\go\bin\go.exe build ./...`（**退出码必须 0**）
  · 产出：`D:\go-path\go\bin\go.exe build -o bin\wb2api-server.new.exe .\cmd\server`
* 二进制替换：`Move-Item bin\wb2api-server.exe bin\wb2api-server.exe.old-<时间戳>` → `Copy-Item 新 → 旧`。
* 日志格式：`node "$env:TEMP\chk_js.js" internal\server\webui.html`（若该脚本不存在，跳过 —— 它只在内联 JS 被改时才需要）。

## 2 · ⚠ 硬约束（违反即返工）

1. ⚠⚠ **不许改 `internal/server/webui.html`** —— 它是单文件，**只有主脑改**。两个代理同时改必冲突
   （2026-10-18 已经因此被用户骂过一次「工作流效率怎么这么低」）。
2. ⚠ **绝不碰端口 7863**（用户在跑的实例）与 **3080**。要起服务**只用 7864**：
   * 复制 `config.json`（**只改 `listen` 为 `127.0.0.1:7864`**）⇒ 存成 `config-7864-probe.json`
   * 启动：`.bin\wb2api-server.exe -config config-7864-probe.json`
   * ⚠ **用完立刻停**（`Get-NetTCPConnection -LocalPort 7864` 拿 pid ⇒ `Stop-Process`）· ⚠ 删掉那份临时 config。
   * 令牌：`(Get-Content config.json -Raw | ConvertFrom-Json).admin_token` ⇒ 放 header `X-Admin-Token`。
3. ⚠ **不许 git commit / push / 切分支**；改动留在工作区。
4. ⚠ **不许"顺手重构"** —— 只做你认领的那一个小目标。
5. ⚠ PowerShell 里含嵌套引号/正则/数组的复杂命令会「无输出 + exit 1」⇒ 检查逻辑**写成 Python 文件再跑**。
6. ⚠ 中文注释；中文引用用「」；**不许把"没验过"当结论**。

## 3 · 报表要补什么（主脑已查清，不是猜）

### ⚠⚠ 三条**实测发现**（2026-10-18 · 由 A2 代理实测得出，**先看这三条再看下面**）

1. ⚠ **日志里已有独立的 `provider` 字段** —— **不要**从 `model` 取 `provider/name` 前缀当上游。
   实测代价：`model="-"`（无 `/`）的 2 行取不到前缀 ⇒ `workbuddy` 少计 2 次请求，
   **成功率被算高**（前缀法 99.96% vs `provider` 字段 99.87%）。**以 `provider` 字段为准，前缀只兜底。**
   ⚠ 但**务必自己 `Get-Content data\request-log.jsonl -Tail 3` 核对字段名**再动手
   （主脑已在转述字段上错过两次）。
2. ⚠ **`tokens = -1` 是脏数据：共 4,126 行**（其中 **44 行 `status=200`**）。
   两条口径实测差 4,126：原始和 `26,579,751` vs 剔除负数 `26,583,877`。
   ⇒ **选一个并在报告里写明"哪个、为什么、差多少"**；建议两个都输出
   （`tokens` 保持原始和以不惊动老代码，另加 `tokens_clean`）。
3. ⚠ **日志是活的**（7863 一直在追加）⇒ 任何"数字比对"**先把日志 `Copy-Item` 冻结成快照**，
   再让被测方读那份。否则"对不上"不等于写错了。
4. ✅ **现成的核对工具已就位**：`tools\verify_stats.py`（纯标准库 · 只读 · `--json` / `--top N` / `--file`）
   ⇒ ⚠ **直接用它，不要再自己写一个**。

### 现状（主脑实探，2026-10-18）

* `GET /admin/stats` **已经有三个维度**：`by_model` · `by_provider` · `by_uid`
  （⚠ **不是**"只有 by_model" —— 我早先说过一次，那是错的）
  —— ⚠ 但它们只是「名称 → **请求数**」的计数，**没有 tokens / 成功数 / 耗时**。
* `GET /admin/stats/series?range=24h` **已经存在**，返回 `series`（25 个桶）：
  `{ ts, total, ok, fail, tokens }` ⇒ ⚠ **"趋势"这一层我们不缺**。
* ❌ 真缺的是**表现层**：Top 榜（带 tokens 占比）· 环形图 · **双栏布局** · 时间范围切换。
**数据源够用**：`data/request-log.jsonl`，每行一个 JSON，字段实例如下（真实样本）：

```json
{"model":"loomy/DeepSeek-V4.1-Flash","mode":"stream","status":200,
 "ttfb_ms":17530,"tokens":1166,"think_tokens":698,
 "cache_hit_tokens":0,"cache_miss_tokens":0,"uid":"26091..."}
```
（还有时间字段，用于按天分桶；⚠ **先自己 `Get-Content ... -Tail 1` 看清真实字段名，别信我转述**。）

**要补的三个维度**（对标 agent2api 的 `daily / model / provider / account`）：
* **按 `uid`**（= 它是 `AccountAccum`）—— 用于「Top 账号」榜；
* **按上游**（= `ProviderAccum`）—— 用于「Top 提供商」；⚠ 上游要从 `model` 的 `provider/name` 前缀取，或从 uid→上游映射拿（**先确认哪种可行再动手**）；
* **按天**（= `DailyEntry`）—— 用于趋势；⚠ 它是最贵的一层，**本批可以不动**（见下）。

## 4 · ⚠⚠ 八条"报表最容易错"的口径（**抄自 agent2api 的 `report.rs` 文件头注释**）

1. ⚠ **零除**：算成功率 / 占比前必须防分母为 0。
2. ⚠ **补零**：区间内"没有请求的那天"要**补 0**，否则趋势图会**跳过**那天。
3. ⚠ **`activeDays` 只数区间内有请求的天数** —— 补零的日子**不算「活跃」**。
4. ⚠ **区间反向要交换**（`start > end`），否则前端会画出**负长度时间轴**。
5. ⚠ **超长区间要有循环上限**（`all` 可能一年以上；手改配置能造出更长的）。
6. ⚠ **口径不一致是最常见的错**：同一个数在不同卡片里必须同一个算法算出来。
7. ⚠ **宁可给一个"能显示的口径"，也不要让整页报表 500**（失败要降级，不要抛）。
8. ⚠ **精度分层**：按天聚合给不出"本地整点"级别的曲线 —— 需要整点就走明细，别用天数据凑。

## 5 · 验收（**每一条都要过，缺一条就是没做完**）

* `go build ./...` **退出码 0**（纯前端改动则看你改的层）。
* ⚠ **数字必须对得上**：你新加的聚合要用**独立算法**核对 ——
  用 Python **直接读 `data/request-log.jsonl`** 自己求和，**与接口返回值逐项比对**，两边一致才算过。
  ⚠ **不许只检查"字段存在"**。
* ⚠ 改了要起 **7864** 实测（见 §2 硬约束）· ⚠ 用完停掉。
* 报告写 `docs/report/<你的编号>.md`，**正文 ≤300 行**，⚠ **必须有一节「哪些没验」**。

## 6 · 交付格式（报告里必须有这四段）

1. **改前是什么**（贴关键代码/字段的原文）· 2. **改后是什么**（同样贴原文）·
3. **怎么验的**（命令 + 真实输出片段，含数字）· 4. **⚠ 哪些没验**（诚实列）。

---

## 7 · 可选：这份简报怎么用（给主脑自己看）

⚠ 派活时 prompt 里**只写**：「先 `read docs/report/BRIEF.md`，然后做 <你认领的那一条>」——
**不要在 prompt 里复述背景**（那正是用户抱怨的慢的来源）。
