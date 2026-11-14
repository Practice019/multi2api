# design-notes: 模型目录页「单供应商重拉 / 单模型探测」两列行内操作（T-02）

## 0. 范围

- 要做什么：给 `/ui/models` 的「供应商」表加两列行内按钮（`重拉` / `探测`），配两个写入面端点
  `POST /ui/models/provider/refresh`、`POST /ui/models/provider/probe`；两者只作用于**指定的那一行**，
  不触发整页重取、不重排其他行。
- **不做什么**：
  - 不改 `/ui/models/data` 的语义（仍是只读取数，`catalog` 字段口径不变）。
  - 不改全量刷新路径（`fetchDynamicModels` / `modelsForProviderCached` 的 TTL、单飞、stale-while-revalidate 行为一律不动）。
  - 不新增前端框架、不加构建步骤、不引入新依赖。
  - 不做"批量重拉全部供应商"—— 那是既有 `/ui/models/data?refresh` 的职责，不在本次范围。

## 1. 热点判定

| 热点 | 调用频率 | 单次成本 | 得分 |
| --- | --- | --- | --- |
| `refreshProviderCatalog(id)`（新，单上游回源） | 用户点击级（每分钟个位数） | 0.2–23.7 s（实测画像见 docs/known-issues.md） | 高频×高成本 ⇒ **第 1** |
| `probeModel(provider, model)`（新，一次最小真实请求） | 用户点击级 | 0.8–30 s（含 30 s 超时上限） | 低频×高成本 ⇒ **第 2** |
| `modelsForProviderCached` / `fetchDynamicModels`（既有缓存层） | 每个 `/v1/models` 一次 | 命中 µs / 未命中同上 | 超高频×零成本 ⇒ **第 3**（只读，本次只"绕过"不改） |
| `renderVendors()`（前端重绘） | 每次 state 变更一次（含输入过滤的每键一次） | 14 行规模，亚毫秒 | 超高频×极低成本 ⇒ **第 4**（只加两列，不重构） |
| `catalogFreshness()` | 每次 `/ui/models/data` 一次 | µs 级（只读内存快照时刻） | **第 5**（不动，重拉后自然体现） |

不审的：`upstreamCatalogEntries` / `channelSyntheticCatalogEntries`（纯装配，与本次无交互面）、
`aggregateCatalogVendors`（纯聚合，输入未变）。

## 2. 七问答案

### Q1 能不能不做？

不能。现状是**只有全目录刷新**，而单次全量要逐个打 14 个上游（实测单上游 0.2–23.7 s，最坏一轮几十秒），
且一次刷新会把 14 份缓存的时间戳一起推新 —— 用户无法回答"到底是哪个上游坏了/变了"。
本次调试中真实发生过：一个上游的 403 被整页刷新掩盖，只能靠手工脚本逐个试。
故这一步是把"可观测单元"从「整包目录」降到「一行」。

### Q2 这份工作该做几次、在哪做？

- **一次**，在**服务端 handler 里做一次**，前端只发起 + 渲染，不自己拼上游请求。
  （理由：上游凭证/选号/前缀剥离只有出口层知道，前端不可能也不应该知道。）
- 按**供应商**为粒度：一次请求只回源一个上游，不串行、不并发地碰其他上游。
- 触发点**只有**用户点击，不挂任何定时器/轮询（与 `models_flight.go` 第 18–20 行「不引入 ticker」同一条心智）。

### Q3 输入输出是什么性质？

- 输入：`provider`（必填，字符串）、`model`（探测时必填）。
- 输出：**外部网络 IO** 的结果 ⇒ 必须可失败、可重试、可超时。
  - 重拉：`{provider, count, added, removed, ms, source}` / 失败 `{provider, error:{code,message}, ms}`。
  - 探测：**永远 200**（探测是测量而非操作），`{provider, model, ok, status, code, message, firstTokenMs, totalMs, text}`。
- 两者都**不写库**（不动 `models`/`vendors` 表）—— 只更新内存缓存快照。落库仍是 `SeedModelCatalog` 的事。

### Q4 10x 会怎样？边界在哪？

边界：14 个供应商**同时**被点「重拉」。后果与对策：
- 现状已有 `providerModelsLockFor(id)`（按 id 分域）+ `tryBeginRefresh(id)`，14 个**不同** id 会并行 14 条上游往返 ——
  最慢那个 23.7 s 决定用户感知，但**不会**互相堵（按 id 分锁）。
- 真正会坏的边界是**同一 provider 被连点**：这里用 `tryBeginRefresh(id)` 的既有语义直接复用
  （已有刷新在跑 ⇒ 回 `busy`，不再排第二个人）。
- 探测的 10x 边界是**上游风控**：`code.phanthy.com` 实测对密集请求进入 2–4 分钟 403 冷却窗。
  ⇒ 探测定为「同一 provider+model 5 秒内只打一次上游，重复点击回放上一次结果」（见 Q5）。

### Q5 假设它已经失败了，最可能因为什么？

按可能性排序：
1. **把上游错误码吞成 502/500** —— 本次调试的最大时间黑洞就是这个（一个 `request_not_authorized`
   被网关折成"502 空消息"，排查方向整整偏了几小时）。⇒ 契约硬要求：`error.code` 原样透传。
2. **重拉把别的供应商也刷新了** —— 那会让"哪个上游变了"重新不可知，等于白做。⇒ 必须有测试守着。
3. **点了没反应/卡在 loading** —— 慢上游 23.7 s。⇒ 前端按钮 disabled + 结论落行内，超时由服务端 30 s 兜住。
4. **上游 403 冷却窗被误读成"这个模型坏了"** —— ⇒ 探测结果区显示 `status` + `code` 原文，而不是二值 ok/不 ok。

### Q6 有没有更简单的？这个复杂度值得吗？

更简单的做法：前端直接对 `/v1/chat/completions` 发一次真实请求当"探测"。
否决理由：那会走完整鉴权/计费/日志链路，等于**用用户额度做健康检查**，且拿不到"这是上游第几跳出的错"
（错误会在网关层被折形）。服务端内测能拿到 `gateway.ChatStream.Status`（上游原始状态码）+ 首包时间。
复杂度增量 = 一个 handler + 两列按钮，值得。

### Q7 哪条命令能证明我错了？

- `go test ./internal/server/ -run TestProviderRefreshOnlyTouchesOneVendor -v`
  —— 若它绿不了，说明"只改一行"这个结论是错的。
- `go test ./internal/server/ -run TestProbePassthroughUpstreamCode -v`
  —— 若它绿不了，说明错误码透传没做到。
- `go test ./internal/server/ -run TestProbeDedupedWithinFiveSeconds -v`
  —— 若它绿不了，说明5秒去重不存在，密集点击会打出冷却窗。

## 3. 性能决策

### 3.1 落在哪一级

**L2（少做）**：把"回源"从「一次刷新打 14 个上游」降到「一次打 1 个」；
探测额外叠加**去重**（5 秒窗内同一目标只发一次），即同一份工作不重复做。

### 3.2 为什么不用更低一级（必答）

- 不能 **L0（改配置）**：TTL 已经是 1 小时，调小只会让全量刷新更频繁 —— 方向相反。
- 不能 **L1（别做）**：这件事必须做（Q1：现状不可观测）。
- 所以最低可行级就是 **L2**：不减少调用次数做不到（用户必须能主动触发），
  不改复杂度（O(1) 个上游），不下沉平台（没有平台层可给）。
- 明确**不做 L3/L4/L5**：不做并发聚合与优先级队列（14 行规模不需要），不引入后台 worker（没有 ticker 的既有约定）。

### 3.3 留给测量的部分（L6）

"14 个供应商同时点重拉时，页面最大等待是否被最慢上游拖到 23 s" —— 属 L6 微观测，
前置不优化；要测就用真实实例点一轮，记录每次 `ms` 字段。测不了就标 `estimated`。

## 4. 方案取舍

- 采用的方案：
  1. 服务端统一入口 `refreshProviderCatalog(ctx, id)`：默认上游走 `h.cfg.Upstream.FetchModels`
     （刷新 `dynamicModelsCache`），其余上游走 `h.cfg.Provider.Models` + `storeProviderModels`
     —— 原因：目录有**两份缓存**（默认上游一份、其余按 id 一份），只在其中一份上"刷新"会导致页面不变。
  2. 探测走「池子选号 → `credentialFor` → `chatProvider`」，body 固定 `max_tokens:16 / stream:true / 单条 hi`，
     读上游流首包测 `firstTokenMs`，`Status` 非 2xx 时把 body（截断 300 字）里的 `error.code` 原样带回。
- 被否决的方案：
  - 前端直连 `/v1/chat/completions` 做探测（Q6）。
  - 新开一个 `POST /ui/models/refresh`（不带 provider）做单行刷新 —— 语义上会诱导前端传空、
    退化成全量刷新，正是要避免的。
- 否决理由：见 Q6 与"被否决方案"逐条。

## 5. 证伪命令

| 结论 | 会失败的检查（命令 / 断言） |
| --- | --- |
| 重拉只影响指定供应商 | `go test ./internal/server/ -run TestProviderRefreshOnlyTouchesOneVendor -v`：断言另一 provider 的缓存时间戳与条目数**逐字不变** |
| 上游错误码原样透传 | `go test ./internal/server/ -run TestProbePassthroughUpstreamCode -v`：fake 上游回 `{"error":{"code":"request_not_authorized"}}`，断言响应里 `code == "request_not_authorized"` |
| 5 秒去重真的存在 | `go test ./internal/server/ -run TestProbeDedupedWithinFiveSeconds -v`：同一目标连点两次，断言 fake 上游计数 == 1 |
| 未认证不可用 | `go test ./internal/server/ -run TestProviderActionsRequireUIIdentity -v`：无 cookie/token 时两条端点均 401 |
| 前端两列真的渲染 | `go test ./internal/server/ -run TestModelsPageHasProviderActionColumns -v`：页面源码里存在 `data-act="refresh"` / `data-act="probe"` 两个稳定钩子 |
