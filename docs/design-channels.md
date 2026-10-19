# 渠道 / 模型映射 / 对外模型 —— 设计（对标新版 new-api）

> 目的：把本项目做成"中转站"，实现逻辑与知识库里的**新版** new-api 一模一样。
> **纪律**：本文件只写有证据的结论，每条都带来源；证据不足的一律标「⚠ 待确认」。
> 版本：2026-10-20，作者：主脑（原派的子代理卡住未产出，已中断）。

## 0. 证据来源

| 类别 | 来源 |
|---|---|
| 概念文档 | `viking://resources/wiki/new-api/concept/channel-ability-routing.md`（已通读） |
| 同目录其它概念 | `relay-protocol-conversion.md`、`quota-and-billing-pipeline.md`、`billing-expression-system.md`、`panel-authentication-and-sessions.md`、`internationalization.md`、`task-plugin-api-v1.md` |
| 源码锚点（概念文档末尾列出） | `model/ability.go`、`service/channel_select.go`、`middleware/distributor.go`、`model/channel.go`、`constant/channel.go` |
| 交叉印证（**旧版**源码，只能当辅助） | 本地 `D:\project_GIT\new-api-main.zip\new-api-main\`（`model/channel.go` 字段、`model_mapped.go` 的环检测） |

## 1. 能力索引（ability）—— 路由与列表的**唯一数据源**

原文：ability 是"makes a channel usable for a model inside a group"的 join 记录，
字段 `Group, Model, ChannelId, Enabled, Priority, Weight, Tag`，
**复合主键 (group, model, channel_id)**，并在 priority/weight/tag 上建索引。
读变体 `AbilityWithChannel` 额外带渠道类型。维护方式：写入时按 group × model 集合
**分块展开**（忽略冲突）、状态可**按 tag 批量更新**、有修复例程
"truncates, rebuilds, and re-initializes the channel cache"。
列表侧同源：`GetGroupEnabledModels` / `GetEnabledModels` / `GetAllEnableAbilityWithChannels`。

**我方落点**：`internal/channelstore/store.go` —— 已实现同形状（含复合主键与"改渠道就重建能力"），
`EnabledModels(group)` 等价 `GetGroupEnabledModels`。

## 2. 选路：priority 是**耗尽阶梯**，weight 是**档内分布旋钮**

原文三条：
1. 候选取（group, model）的 ability，按 priority 降序 → weight 降序；
2. **重试计数选择优先级档：重试第 n 次 = 第 n 档**；
3. 档内**加权随机**采样（"with a bias applied on top of the configured weight"）。

分组：请求带 token group；group 为 `auto` 时展开成用户的 auto groups 并按序迭代；
**一个组的所有优先级走完才进下一组**；路由器记录当前组下标与该组起始的全局重试数来换算
组内优先级档；某组对该模型没渠道 ⇒ 进下一组、重置重试计数；开了 cross-group retry 时，
已耗尽预算的组仍服务当前尝试但**预置**下一次切组。获胜的组写回请求上下文（供日志与计费）。
⚠ 待确认："bias applied on top of weight" 的 bias 具体是什么（我们目前实现为纯 weight）。

**我方落点**：`internal/channelstore/select.go` —— 阶梯 + 档内加权已实现；
`group` 本轮固定 `default`（多组/auto/cross-group **未实现**，数据结构已留位）。

## 3. 一次尝试的解析顺序 + 失败语义

顺序（HTTP 分发中间件与 Responses WebSocket 中继**共用**同一解析器，所以两种传输行为一致）：
1. **channel pin**：显式 pin 优先；被覆盖的 pin 要带来源与渠道 id 记日志；pin 的渠道必须存在、
   启用且满足过滤器，否则 400/403 级失败；来自 origin task 的 pin 报固定码 `origin_task_channel_disabled`。
2. **session affinity**（**仅第一次尝试**）：先试 (model, group) 之前记录的偏好渠道；只有它存在、
   启用、满足过滤器、且对该组+模型仍可用时才生效；不可用则清缓存（除非配置要求跨停用保留）；
   strict session 模式下不可用直接报固定消息 `strict_session_binding_unavailable`，不回退。
3. **随机候选**：上面那套组迭代器给候选。
4. **filter check**：**pin 与 affinity 的候选也要过**；被拒的候选要报 filter kind 与是哪个渠道。

无渠道可用时返回 `ChannelSelectError`，携带：HTTP status、error code、可本地化 message id、
message 参数、filter kind、以及**区分"这个组+模型确实没渠道"与其它失败**的标志。
token group 为 auto 时消息里把它渲染成 `auto(<group>)`。

**我方落点**：`internal/channelstore/select.go` 的 `SelectError`（status/code/message id/filter/no-channel ✓）。
pin 与 affinity **未实现**（`Select` 现在只做第 3 步；第 4 步的 filter 只有 status 一种）。
⚠ channel trail（原文："Every attempted channel is appended to the request's channel trail"，
供重试日志与消费日志的管理信息读）**未实现**。

## 4. 渠道记录

凹原文：记录含 key（或**多个 key**）、可用模型、group 绑定、状态、以及 overrides
（参数覆盖、头部覆盖、channel info）。**multi-key 是一等公民**：channel info 记是否 multi-key、
key 数量、**每 key 状态列表**、**key 选择模式** ⇒ "key selection participates in routing rather than
being an upstream concern"。渠道状态必须是 enabled 才能被用。
渠道类型是整数：`ChannelTypeUnknown (0)` 到 `ChannelTypeSGLang (63)`，每类型有**文档化的默认 base URL**
（用于填表单占位）与显示名映射；有一个哨兵类型只用于界定枚举、不可用。

**我方落点**：`internal/channelstore/store.go` 的 `Channel`/`ChannelKey`/`Overrides`/`ChannelInfo`
（多 key 与每 key 状态、key 选择模式已建模；`PickKey` 已实现 random/round_robin）。
⚠ 待确认：每类型的默认 base_url 表与显示名映射（`constant/channel.go` 未逐条读）；
⚠ 渠道类型编号我们只用 1 个（OpenAI 兼容），编号不自造。

## 5. 模型映射（R2）

已核实：映射是**每渠道**一份 `map[string]string`（旧版 `model.Channel.ModelMapping` 字段 `json:"model_mapping"`），
请求侧按上下文键改写，且源码里有固定错误 **`model_mapping_contains_cycle`**（⇒ 有环检测）。
新版 web 侧还有规则推导工具（`web/src/features/channels/lib/model-mapping-rules.ts` 的
`deriveModelMappingPairs` / `mergeModelMappingPairs`、`model-naming-patterns.ts` 的
`detectModelNamingPatterns` / `findVerifiedAlias`）。
⚠ **待确认**：这些推导函数**具体的输入输出语义**（我只拿到函数名与一句 abstract）⇒
本轮**明确不做**"智能映射建议"，只做"人手工填的映射 + 环检测 + 预检"。

**我方落点**：`internal/modelmap/mapping.go` —— 解析（未命中透传 / 链式走到不动点）、
环检测（`Validate` 供保存前预检）、自指当空操作、反向表（一对多给冲突清单不静默挑）。
**尚未接线**（T5/T4 才把渠道的 `ModelMap` 接到请求路径与目录）。

## 6. 令牌可见模型（R1 的一半）

已核实（KB entity 记录 + 旧版源码交叉印证）：令牌有 `Models`、`ModelLimitsEnabled`、`Group`；
对外列表由"该组启用的模型（能力索引）"与"令牌自己的白名单"共同决定。
⚠ 待确认：`/v1/models` 处理里"两者相交"的确切代码位置（`controller/model.go` 的 `ListModels` 只读到函数名）。

**我方现状（好消息）**：`internal/server/apikey_models.go` **已经**实现了"按调用方 key 的白名单裁剪
`/v1/models`"且是**三态语义**（未设 / 空 = 不限制 / 非空 = 只允许这些）。
⇒ R1 只差"再与**能力索引**相交"这一步（T5）。

## 7. 渠道模型发现 / 同步

已核实函数名：`controller/model_sync.go` 的 `SyncUpstreamModels`、
`controller/channel.go` 的 `FetchUpstreamModels` / `FetchModels`、
新版 web `features/channels/hooks/use-channel-model-discovery.ts`。
⚠ 待确认：失败与部分成功的处理细节（T4 实现时先按"拉不到就让用户手填"的保守口径）。

## 8. 我方落点对照表

| new-api 概念 | 我方 | 状态 |
|---|---|---|
| ability 索引 `(group,model,channel_id)` | `internal/channelstore`（`Ability` + `EnabledModels`） | ✅ 已实现（10 测试） |
| 选路：priority 阶梯 + weight 加权随机 | `channelstore.Select` | ✅ 已实现（5 测试） |
| `ChannelSelectError`（status/code/message id/filter/no-channel） | `channelstore.SelectError` | ✅ 已实现 |
| multi-key + key 选择模式参与选路 | `Channel.Keys` + `PickKey` | ✅ 已实现 |
| 渠道记录、overrides、channel info、状态三档 | `channelstore.Channel` | ✅ 已实现 |
| 渠道类型常量 0..63 + 每类型默认 base_url | — | ⬜ T4 |
| 每渠道 `model_mapping` + 环检测 | `internal/modelmap` | ✅ 已实现（6 测试），⬜ 未接线 |
| pin / session affinity / cross-group | — | ⬜ 不做（本轮范围外，结构留位） |
| channel trail | — | ⬜ 未实现 |
| Key 型渠道（Base URL + Key，OpenAI 兼容） | `internal/upstream/openai_compat.go` | ⬜ T4 |
| 对外目录 = 能力索引 ∩ 令牌白名单 ∩ 映射 | `apikey_models.go` + `channelstore.EnabledModels` + `modelmap` | ⬜ T5 |
| 管理端两页签（渠道 / 对外模型） | `internal/server/webui.html` + `/admin/channels` | ⬜ T6 |

## 9. 与账号型上游的边界（不是 new-api 的概念，是我方的既有现实）

我方有 4 条**账号型**上游（走 `auths/` + `Pool` 选号/惩罚/轮换），new-api 只有"渠道"一种。
本轮决定：**两者并存** —— 账号型照旧不并入渠道表；Key 型渠道**不进 Pool**（没有"号"可换，
进 Pool 会把"一个 key 被限流"误判成"账号坏了"）。将来统一时，账号池应当成为"渠道的一种凭据来源"，
而不是反过来把渠道塞进 Pool。

---

# 附录：实现进度与"被守卫逼出来"的设计决定（2026-10-20）

上文的蓝图都来自知识库里的 new-api（v1.30.0）。这一节记录**实现到哪、以及哪些决定不是设计出来的、
是被本仓的架构守卫逼出来又反过来变成了正确设计**。后来者请先读这一节，能少走一遍弯路。

## 已完成（按提交顺序，全部实测）

| 切片 | 落地 | 测试 |
| --- | --- | --- |
| T1 设计文档 + 蓝图对照 | `docs/design-channels.md` | — |
| T2 渠道存储 + 能力索引 | `internal/channelstore`（`data/channels.json`、`(group,model,channel_id)` 复合键、停用即不产生能力） | 10 |
| T3 模型映射 | `internal/modelmap`（`ModelMap` 是**公开名→上游名**、解析到不动点、环检测、反向表） | 6 |
| T4 Key 型渠道 | `internal/openaicompat`（独立包，已登记为上游；逐字节透传、空 key 不打上游、错误不含 key 明文） | 4 |
| T4b 选路 | `internal/channelstore/select.go`（优先级阶梯 + 同档加权随机；`PickKey`） | 5 |
| T5 可见性 | `internal/modelvis`（能力索引 ∩ 白名单 ∩ 反向映射，默认收紧）+ `/v1/models` 接线 | 8 + 3 |
| T6 数据契约 | `GET /admin/channels`（CRUD、key 只回掩码 + 显式 reveal）· `GET /admin/channels/public-models` | 4 + 4 |
| R2 请求侧 | `internal/channelroute`（公开名→上游名规划、环跳过、优先级稳定排序） | 5 |
| R3 转发 + 入口 | `internal/server/channels_forward.go` / `channels_stream.go`，接进 `/v1/chat/completions` | 5 + 4 |

## 五个设计决定（★ 都是守卫或测试先红了才定下来的）

1. **渠道存储落地 `data/channels.json`**，与 `apikeys.json`/`accounts.json` 同惯例；损坏**显式 `log.Fatalf`** ——
   静默重建等于把用户手工配的渠道与 key 全清空。
2. **`internal/upstream` 不是上游，是上游共用的库**。把 Provider 实现放进那儿会同时踩两条守卫：
   豁免名单要求"被豁免的包不得依赖 gateway"，而一旦当上游看，`internal/workbuddy` 依赖它又违反
   "上游之间必须独立"。⇒ Key 型渠道必须住**自己的包**并显式登记进 `allUpstreams()`。
3. **核心包不得依赖上游包**（`TestCoreDoesNotDependOnUpstreams`）。`internal/server` 直接 import
   `openaicompat` 会红 ⇒ 改为经 `ChannelProvider` **工厂注入**，组合根 `cmd/server` 装具体实现。
   好处是可验证的：再加一种 Key 型上游只需换组合根一个函数。
4. **`ModelMap` 的方向是「公开名→上游名」**（与 new-api 的 `channel.model_mapping` 同向）。
   方向写反**不编译报错**，只在"目录里的名字与用户填的对不上"时才暴露 —— 目录侧用 `Reverse()`，
   请求侧用 `Resolve()`，两个方向必须来自同一份表。
5. **Key 型渠道失败时绝不回落账号池**。这是它与账号上游的根本区别：key 全停用 / 上游 401 时
   明确失败，而不是悄悄换一条池里的号（否则用户看到计费口径与身份都变了，却不知道走了哪条）。
   ⇒ 为此"模式未开启"（`errNoChannelMode`）与"真实失败"是**两个不同的错误**，入口分别走
   回落既有路径 / 404 明确拒绝 / 502 如实上报。

## 未完成（下一轮从这里接）

- `channelstore.Select` 的**优先级阶梯 + 同档加权随机 + 第 n 次重试走第 n 档**尚未接进转发
  （`channels_forward.go` 里取首选候选，有 TODO 指路）。
- **T6 界面**：`/admin/channels` 与 `/admin/channels/public-models` 只有 API，控制台还没有页签。
  ⚠ `webui.html` 已 762.6 KB / 12373 行 ⇒ 渠道界面应做成**独立的内嵌资源文件**再挂载，不要往里塞。
- **活进程端到端**尚未做：目前全部证据都是进程内（handler 级 + 假上游 httptest）。
  做的时候用**临时实例 + 临时目录**，绝不碰 7863 / `config.json` / `auths/` / `data/`。
- 前端套件里 `dashboard_gen.js`（成员模式断言，该模式已被 `9f5a503` 有意删除）等 5 条为**既存红**，
  与本功能无关（本功能分支未改任何前端字节）；门禁①（内嵌 script 语法）当前 5/5 绿，
  改前端时它才是硬门。

## 两条操作纪律（本会话踩过）

- **内嵌资源改完必须先过 `verify_inline_script_syntax.js` 再部署**：曾有一次语法错直接部署，
  控制台空白约两分钟。
- **测试红了先修再提交**：本会话有一次在全量回归有红的情况下提交，随后不得不 revert 重做。