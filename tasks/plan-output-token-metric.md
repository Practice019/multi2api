# Plan：全面板统一「AI 生成 token」口径 + 照 new-api 补齐图表

> 目标 `goal-d58fb97b`。用户定的口径是**唯一的**：

## 口径定义（不可协商）

**「消耗」= AI 生成的 token（`completion_tokens`），不是 total。**

用户原话：「我需要的数据永远都是一个模型：它生成了多少 Token」「Token 数全部都计算 Token 数，不要计算消耗」
「用户知道他某个时候消耗的 Token 数是多少，也就是 AI 生成的 Token 数就是消耗的」

依据（实测）：一条请求 `total=37938`，但 `prompt=898 + 缓存读取=36864 + 输出=176`。
用 total 画图 ⇒「一小时消耗 2.9M」，而真正生成的只有约 1.3 万 —— 差 200 倍。

**规则**：
1. 任何显示"token 数"的位置，一律用输出 token。
2. 历史行没有 `out_tokens`（2026-10-20 才加）⇒ 显示空/无数据，**绝不回退成 total**。
3. 需要表达"总输入量"时，用**独立字段 + 独立标签**（如「输入 token（含缓存读取）」），不与输出混。

## 现状盘点（本会话已完成）

| 层 | 状态 |
|---|---|
| `wire.UsageExtras.CompletionTokens` | ✔ 已提取 `completion_tokens` |
| `logbuf.Entry.OutTokens`（`out_tokens`） | ✔ 已落盘 |
| 渠道采集器 `streamUsage.Completion()` | ✔ 纯输出 |
| `series[].by_model_out` | ✔ 纯输出分桶（20cce65） |
| 堆叠柱 | ✔ 已按输出画（柱高只取 Σby_model_out） |
| 日志表 tok/s | ✔ `completion_tokens / (total_ms − ttfb_ms)`，非流式不显示 |
| UID 列 | ✔ 渠道 key 中间打码 |

## 待做

### P1 · `/admin/stats` 增加输出汇总，看板所有"token 数"改用输出

- [ ] **T1** 后端 `aggregateChatLog` 增加 `out_tokens`（Σ `e.OutTokens`）与 `by_model_out`（按模型 Σ 输出）· 规模 S · 依赖 — ｜ 验收：合成 3 条（out=176/824/500）⇒ `out_tokens=1500`、`by_model_out={m-a:1000, m-b:500}` ｜ 验证：`go test ./internal/admin/ -run TestAggregateOutTokens -v`
- [ ] **T2** 指标卡：`输出 token` 卡改用 `out_tokens`；`推理 token` 保持（它是输出的子集，语义要写清）· 规模 S · 依赖 T1 ｜ 验收：合成数据下卡片值 = 手算值；卡标题/tooltip 明确"仅输出" ｜ 验证：CDP 读卡片文本
- [ ] **T3** 环图（上游用量 / 模型用量）改用输出量 · 规模 S · 依赖 T1 ｜ 验收：环图各扇区合计 = `out_tokens` ｜ 验证：CDP 读图例数字

> **Checkpoint A**：build exit=0 · 门禁① 6/6 · test 全绿 · 用真实实例（只读一份日志尾部）确认卡片与环图的数与手算一致。

### P2 · 照 new-api 补齐图表（**先查知识库再实现**）

- [ ] **T4** 查知识库列出 new-api 的图表清单（`viking://resources/repos/QuantumNous/new-api`，重点 `web/src/features/usage-logs`、dashboard 相关）· 规模 S · 依赖 — ｜ 验收：产出清单文件 `tasks/newapi-charts.md`，每项标注"有/无"我们已实现、对应源文件与行号 ｜ 验证：文件存在且每行可追溯到知识库路径
- [ ] **T5** 按 T4 清单逐个实现缺失的图（每图一个独立 commit）· 规模 M · 依赖 T4 ｜ 验收：每个图在 CDP 下能取到 SVG 子元素 > 0，且空数据时不画坐标系 ｜ 验证：CDP 断言

> **Checkpoint B**：逐个图 CDP 截图/断言；确认没有引入外部依赖（grep `http(s)://` 新增行 = 0）。

### P3 · 速度指标一致性

- [ ] **T6** 检查所有出现速度的位置（日志表、看板 hover、次数排行、图表 tooltip）口径是否统一为 `output / 生成阶段`；不一致的改齐 · 规模 S · 依赖 — ｜ 验收：三处显示同一请求的速度值相同 ｜ 验证：CDP 读三处文本比对

### P4 · 收尾

- [ ] **T7** 端到端：临时实例 + 合成日志（含 out_tokens）+ CDP 全页快照，逐项核对数字 · 规模 M · 依赖 T1–T6 ｜ 验收：每个数字都能指到手算依据 ｜ 验证：报告里列"屏幕上 N ≈ 手算 N"
- [ ] **T8** 发版（tag + release，说明只写本版改动）· 规模 S · 依赖 T7 ｜ 验收：release 成功、5 平台产物齐全 ｜ 验证：`gh release view`

## 范围边界（不做）

- 不删历史日志、不回填旧行的 `out_tokens`（那时确实没记录，回填是造假）。
- 不引图表库（内联 SVG）。
- 不改账号池/渠道的选路逻辑（只改统计与展示）。
- 不动 7863 线上实例的数据与配置。

## 风险

| 风险 | 对策 |
|---|---|
| 旧数据全为空，用户以为"坏了" | 面板加一行说明「输出 token 自 2026-10-20 起记录，更早的数据无此字段」 |
| 图表改口径后柱子骤降 | 这是预期（差 200 倍），提交信息与 release 说明都要写清原因 |
| 一次改太多导致回归 | 每图/每卡一个 commit，Checkpoint 逐个验 |