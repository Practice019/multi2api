# hyxapi → workbuddy2api 可移植清单

> 来源：对 `D:\project_GIT\hyxapi`（New API 二次开发，AGPL-3.0）的四路并行调研，
> 四份原始报告（带逐条 file:line 证据）在 `D:\project_GIT\test\` 下：
> `relaykit-portability-report.md` · `hyxapi_portability_report.md` ·
> `hyxapi-可移植性调研-model-controller-plugins-oauth.md`
>
> 本文件是**合并去重后的可勾选清单**。勾了告诉我，我按批次实施。

---

## 零、三条硬约束（动手前必须先知道）

| # | 约束 | 事实 | 影响 |
|---|---|---|---|
| 1 | **许可证** | hyxapi = **AGPL-3.0**，workbuddy2api = **MIT** | ⚠️ **只能 `require` 引用，不能把源码 copy 进仓库** —— 后者会形成混许可。所有「抄代码」项都限于**独立小文件 / 纯函数**（且严格说仍有许可问题，见每条备注） |
| 2 | **Go 版本** | workbuddy2api `go 1.22.5` < relaykit 要求 `1.25.1` | 要引 relaykit 必须**单独一个 PR 先抬 toolchain**（或 `GOTOOLCHAIN=auto`） |
| 3 | **数据形状契约错配** | 你是 **byte 进出**（`Chat(ctx, cred, body []byte)`）；relaykit 是**强类型 DTO 进出** | 引 relaykit 要么加 bytes↔DTO 适配层，要么整体迁移内部模型。**它适合补能力（Gemini/Responses），不适合替换 anthroconv** |

⚠️ 还有一条**不能破坏的既有性质**：`openaicompat/openai_compat.go:20-26` 与 `reasoning_normalize.go:43-46` 都把「不需要改就绝不重新编码」当契约。relaykit 是**全量重建 DTO → 重新 marshal**，挂到透传路径上会破坏它。

---

## A. 能直接抄（<1 天 / 零到低耦合）——性价比最高，建议全做

| # | 项 | 来源 | 解决什么 | 工时 |
|---|---|---|---|---|
| A1 | **转换损耗诊断**：`types/conversion.go:8-75`(75行) + `convdiag/collector.go`(56行) | relaykit | 你有 **4 套并行转换**（anthroconv/zcode/minimax/monkeycode），静默丢字段**完全看不见**（全库 `diagnostic` 只命中 3 处且全在测试夹具）。monkeycode/payload.go:6-10 自己都写了「再写一份就是第三份实现，必然漂移」 | **0.5** |
| A2 | **usage 非零合并** `dto/usage_merge.go:12-94`(83行) | relaykit | 「后到非零覆盖、**零不擦非零**、总量自愈」——你的流式 usage 是拼出来的（`stream_usage.go` + `wire/usage.go`，且有 3 个 `sse_usage_*` 专项测试说明出过 bug） | **0.5** |
| A3 | **审计日志**：`middleware/audit.go`(228行) + `model/audit_log.go:26-46` | service | **唯一「出了事查不出来」级缺口**：谁轮换 api_key / 改 base_url / 手动禁用号，现在只有 log 文本。14 字段结构现成，落地 JSONL 零依赖 | **0.5-1** |
| A4 | **Claude 方言 usage 规范化** `billing_usage.go:300-329`(30行) | tokenkit | ⚠️ **这是真缺陷**：`anthroconv/anthropic.go:671-687` 的 `mapUsage` **根本没读** `cache_read_input_tokens` / `cache_creation_input_tokens`，而你自己在 `usagetap.go:20` 写着 trae「有这三个字段却用不了」⇒ 走 Anthropic 协议的上游**命中缓存时输入量系统性归零** | **0.5** |
| A5 | **错误体 9 字段兜底解析** `dto/error.go:39-80`(71行) | relaykit | 上游是 openresty/APISIX 错误页，形状不可控；71 行换「错误信息永远读得出来」 | **0.5** |
| A6 | **tokenkit 估算权重表** `tokenkit.go:101-219`(120行) | tokenkit | 你只有一行启发式 `(cjk*2+2)/3 + (other+3)/4`（`stream_usage.go:390`）；它是 **5 族 × 11 特征**（Claude Word 1.24 vs OpenAI 0.75 差 65%）。**⚠️ 只抄 `EstimateFamily` 纯 `unicode` 路径**，别连 `FieldsFuncSeq`（Go 1.24+）一起抄 | **1** |
| A7 | **`alwaysSkipRetryCodes`**：给「200 但 body 解析失败」一个终态 | model | `status_code_ranges.go:36-38`。与 parity 判过的「转换失败 skipRetry」是同一动作 | **0.1** |
| A8 | **可配置重试/禁用状态码区间** `status_code_ranges.go`(208行纯函数) | model | 替换 `gateway/upstream_error.go:70-72` 的硬编码 `408/429/5xx`；含区间解析/合并/越界拒绝/全角逗号 | **0.5-1** |
| A9 | **非标位置取 cached_tokens** `relay/channel/openai/usage.go:53-133`(80行) | tokenkit | Moonshot 的键在 `choices[].usage.cached_tokens`、llama.cpp 在 `timings.cache_n`、智谱双位置。你的三级探查（`wire/usage.go:115-133`）全在标准族内 | **0.5** |
| A10 | **`common/etag.go`**(44行) | service | 弱 ETag + `If-None-Match` 弱比较，零依赖 | **0.2** |
| A11 | **`common/quota_math.go`**(142行) | service | 配额饱和转换 + 溢出/NaN 防护 + `QuotaClamp` 审计标记。你的 `apikey.limit int64` + `ApplyQuotaConsumed` 目前**没有溢出防护** | **0.2** |
| A12 | **AcSearch Aho-Corasick** `service/str.go:120` | model | 替换 `upstream/client.go:98-178` 的线性关键词扫描。**保留你的「每上游自己的词汇表」判据** | **0.3** |
| A13 | **reasonmap + shared/claude 小文件**：`reasonmap.go`(46) · `cache.go`(8) · `usage.go`(50) | relaykit | 停止原因映射 / 缓存写入拆分 | **0.5** |
| A14 | **白名单式模型名解析审计** `reasoning/suffix.go:153-202` | relaykit | 「不认识的模型名**逐字不动**」。你的模型名要**逐字转发给白牌上游**，误吃尾缀 = **静默错模型**级事故。建议回头审计 `wire/modelof_test.go` 与 `server/param_override.go` 全部模型名处理 | **0.5（审计）** |

**A 档合计 ≈ 7.4 人日。** 建议先做 A1+A2+A4+A5（2 人日，全是纯函数 / 独立文件，零风险）。

---

## B. 要改造（1-3 天 / 需接现有结构）

| # | 项 | 来源 | 解决什么 | 工时 |
|---|---|---|---|---|
| B1 | **StreamStatus 流结束分类** + 「半截流仍计输入」判据 | model | `stream_status.go` 9 种 EndReason + 5 种 Outcome。你现在只标了「用量从哪来」，没标「这条流**为什么结束**」⇒ client 断流时 tok/s 会把半截生成当**正常样本** | 1 |
| B2 | **配置「校验后再原子替换」的快照** `request_policy.go:24-38, 223-258` | model | 「先整份构建成功 → 事务写 → 才 Store 快照 ⇒ **运行时全局量与快照永不失配**」。你的 `data/*.json` 适用 | 1-2 |
| B3 | **渠道测试三模式筛选** `auto_ban_only` / `passive_recovery` | service | 你现在探测是**全量池 O(N) 次上游往返**；`auto_ban_only` 能砍掉大半。（你的 `pool/health.go` 已为同一问题写了另一条路，**只借筛选条件**） | 0.5 |
| B4 | **重试→档位下沉补成一等公民** | service | 你有 `tierOffset` 参数，但需确认调用点是否**每次重试时递增**。hyxapi 的 `retry` 直接索引优先级档（`channel_cache.go:155-158`） | 0.5 |
| B5 | **在途预订限流** `Reservation` | service | 把「已占满但尚未判定成败」的请求也计入配额 | 0.3 |
| B6 | **分面计数与列表同帧返回** `controller/channel.go:156-176` | model | 一条 GROUP BY 给出 `type_counts`，省一次往返 | 0.2/端点 |
| B7 | **幂等回滚纪律 → 免费额度池扣减** | tokenkit | 三标记幂等（`settled/refunded/fundingSettled`）。你的 `quota_consumed.go` + `pool` 额度扣减**重复扣/漏扣没有对账锚点** | 1 |
| B8 | **缓存命中率 → 账号粘性** | tokenkit | ⚠️ **口径区分是难点**：OpenAI 是 `cached/prompt`、**Claude 是 `cached/(prompt+cached)`**（因为 Claude 的 `input_tokens` 不含缓存）。保持同账号能吃到上游 prompt cache，对**免费额度池是直接省钱** | 1-2 |
| B9 | **请求 ID 贯穿** `middleware/request-id.go` | service | 你没有统一的 `X-Request-Id`；出错时无法让用户报 ID 定位 | 0.3 |
| B10 | **`ParamOverrideTemplate`**：按会话/规则向上游注入 headers | service | 你的 header 是 per-provider 写死的（`upstream/headers.go` 335 行），没法按「哪个客户端调用」动态透传。Codex/Claude CLI 透传白名单是真实需求 | 1.5-2 |
| B11 | **过载保护**（CPU/内存连续超阈 → 503） | service | 单机自用防雪崩。跨平台 cgroup 读取是主要工作量（hyxapi 已分平台） | 1-1.5 |
| B12 | **注册表模式收敛 4 套并行转换** | relaykit | 把 anthroconv/zcode/minimax/monkeycode 的方向声明集中到一张表 + **启动期校验**（注册即 panic，把矩阵错误前移到 init） | 2-3 |
| B13 | **reasoning Intent 化** | relaykit | 你现在 `wire/thinking.go:62-72` 四种情况散在分支里；Intent 化后「显式不覆盖、缺省才补」有**单一落点** | 1-2 |
| B14 | **`accessPolicy` 准入 DSL** `oauth/generic.go:38-70, 328-446` | model | 12 算子（eq/ne/gt/in/contains/exists…）+ 嵌套 and/or + gjson 取值 + 模板化拒绝文案 ⇒ 上游账号准入 | 1-3 |
| B15 | **请求体内存/磁盘双级 + 多 reader 重放** `body_storage.go` + `disk_cache.go` | service | Base64 图片输入可达 3-4 MB；**多 reader 重放**你也没有（跨重试复用同一 body 需要它） | 1 |
| B16 | **图片 token 折算** `tokenkit/image.go` | tokenkit | **前提是先能拿到图片宽高**（你现在纯字节 tee） | 1-2 |
| B17 | **Gemini 方言 usage** | tokenkit | 你现在零 Gemini 支持（grep 0 命中）。`thoughtsTokenCount` 单列一档 + 按 modality 分桶 | 1 |
| B18 | **`message_key` + `message_params` 双轨错误文案** | model | 文案与后端解耦，前端翻 | 0.5 |
| B19 | **`RWMap`** + LongText/`GormDBDataType` | model | 泛型读写锁 Map | 0.2 |
| B20 | **`gatedResponseWriter` 路由认领技巧** | model | 「命中才放行」——动态端点与静态路由**零预声明共存** | 1 |
| B21 | **`ChannelPin` 多来源约束模型** | model | Rank 决胜 + 同渠道取更严 RetryMode + 落败留痕 | 1 |

**B 档合计 ≈ 20-28 人日。** 建议按 B1+B3+B4+B9 起步（2.3 人日，都在你现有结构里加一维）。

---

## C. 大动作（5-15 天）——要么不做，要么一次到位

| # | 项 | 收益 | 成本 | 前置 |
|---|---|---|---|---|
| C1 | **引入 relaykit 做 Chat↔Responses 全量转换** | 高（你现有约 540 行 vs relaykit 约 1900 行，覆盖 tool args / reasoning / annotation） | **5-8 天** | go 1.25.1 + bytes↔DTO 适配层 |
| C2 | **引入 relaykit 开 Gemini 上游**（`gemini_chat` 全组 1471 行 + `shared/gemini/schema.go` 541 行） | 高（**全新能力**，不是补短板） | **8-15 天** | 同上。schema 互转是深坑，这是「require 而非抄」**最省钱**的场景 |
| C3 | **入站协议面补齐** `/v1/messages` · `/v1/responses` · `/v1/embeddings` · Gemini `generateContent` | 高（你现在**只有 3 个入站路由**：chat/images/models） | 随 C1/C2 | — |

---

## D. 只借思路（不抄代码）

| 项 | 借什么 |
|---|---|
| **JS 插件系统**（`pkg/jsplugin` 8961 行 + 11 个 JS 驱动） | **只在**「第三方/用户免编译自带平台驱动」或「加平台不想重发二进制」成为需求时才有净收益（代价约 1.5 万行 Go）。你的 `gateway.ExtOf[T]` 在自家十来个上游场景**更优**（类型安全、零依赖、有架构守卫）⇒ **别换**。可单借：30s 热重载闭环、命中才放行的 gated writer、插件自带 UI 元数据、插件可自渲染错误、沙箱能力面 |
| **`RegisterResource` + `init()` 自注册** `service/authz/registry.go:21-26` | 40 行的注册表模式。**不要 Casbin**（需 SQL DB，你是 JSON 文件） |
| **审计挂在鉴权链路而非路由** `middleware/auth.go:83-93` | 「新增接口自动留痕，无需在路由上再单独挂一层（避免漏挂）」的设计判断。你的挂钩点应是 `internal/admin/gate.go` |
| **fail-closed 路由声明表** `middleware/auth.go:293-319` | 未声明路由直接 403 并告警。将来做 scoped key 时这是唯一正确形态 |
| **Map/Slice 配置项 fresh decode** `setting/config/config.go:206-212` | 增量更新 map 会**保留被删键**、复用 slice 元素会保留省略字段 |
| **`ExportAllConfigs` 扁平导出** `config.go:236-255` | 你的 `settings_store.go:255-284` 与 `:290-317` 是**手工双向映射**，注释自己承认「缺字段会导致该字段每次保存都被清零」 |
| **质量分级 + Steps 可观测** `text_converter_registry.go:13-17, 49-252` | `good/fair/discouraged` + 路径回传，用于**运维说话** |
| **能力表 + 裁剪而非拒绝** `reasoning/claude.go:45-113` | 「越界就裁 + 出诊断」，永不因参数越界失败 |
| **宿主钩子注入（不反向依赖）** `convmeta/meta.go:1-5` | 转换层不认识网关，能力一律函数注入。⚠️ 你的 `anthroconv/maxoutput.go:29-31` **已经同构**（英雄所见略同） |
| **流诊断去重 + pending/take** `response_registry.go:660-691` | 同一损耗只报一次；**只在真产出下游事件时才挂**（避免无输出却报损耗） |
| **方言替换而非字段合并** `usage_merge.go:137-147` | 不同方言的 usage 快照**整体替换** |
| **i18n `message_key`/`message_params` 双轨** `common/message.go:10-14` | 你单用户中文界面 ⇒ 整套价值低 |
| **billingexpr** `pkg/billingexpr/expr.md` | 「预扣快照 + 结算修正」与「用量 schema 与计价表达式分离 + 兼容性检查」 |
| **错误分类的可配置面** `status_code_ranges.go` | 归因/留痕/可配置**你不如它**；但**处置语义**（冷却/禁用/换提示词/直接结束这 5 种账号命运）与**正文级业务错误识别**（200 内嵌业务错、中文关键词双通道）**你比它细** ⇒ 只吸收 A7/A8/A12 三个零件 |

---

## E. 明确不抄（你的更强 / 粒度不符）

| 项 | 为什么 |
|---|---|
| **SSRF 防护** `common/ssrf_protection.go`(381行) | 你的 `gateway/urlguard.go` **1035 行更强**（多了数字型主机名 / IPv4 松散解析绕过的判定），注释自己点名了「new-api 的对应机制」 |
| **出站超时** | 你的 `internal/timeouts/`（Config→Resolve→Validate→ApplyTransport 全链，zero=不限、溢出钳位）**比它系统** |
| **限流算法** | 你的 `authlimit` 是**滑动窗口**且文件头论证过「为什么不用固定窗口」并点名了 new-api 的边界突刺缺陷；它还多了「空 key 拒绝」「被拒不记账」两条它没有的取舍 |
| **账号池管理**（冷却/熔断/自动禁用/延迟） | 你的 `pool/` ≈ **4,838 行 vs 它的渠道选择 ≈ 1,694 行**。它有 **`cooldown`/`breaker`/`abnormal` 全 0 命中** —— **没有账号级熔断、没有上游级熔断、惩罚状态重启清零** |
| **错误分类整体** | 见 D 档最后一条：**各细一半**，保留你的 |
| **选号降档 / 判决留痕 / 额度订阅 / 用户登录体系 / RBAC 多租户** | 你已抄过或已判定不抄（`newapi-parity.md`） |
| **CASBIN + `service/authz` 全套** | 需 SQL DB；你单机自用无角色 |
| **`distributor.go` 的 `getModelRequest`**（700行） | 覆盖 mj/suno/video/gemini/audio/realtime 全路径；你只做 OpenAI 兼容 |
| **`billing_usage.go`(422) · `types/channel_error.go` · `dto/channel_settings.go`(625)** | 与你的号池/配额/软限流**粒度不一致**，且后者是**宿主死重**（引 relaykit 会白带） |
| **CORS 凭证白名单 · gzip 预压缩 · 请求级超时中间件 · `common/pyro.go`/`gopool.go`** | 无场景 / 已有更优 / 引第三方依赖收益低 |
| **`middleware/task_plugin.go`**(1448行) | 依赖 jsplugin 全套插件运行时 |

---

## F. 反向：hyxapi 该抄你的（供你评估要不要提 PR）

| 机制 | 你的位置 | hyxapi 现状 |
|---|---|---|
| 账号级连续失败熔断（指数退避） | `pool.go:1877-1894` | **无** |
| 上游级熔断 | `pool.go:1995-2042` | **无** |
| 冷却的模型级豁免 | `pool.go:216-255` | 无（它按 group+model 分桶天然规避） |
| 惩罚状态持久化 | `pool.go:285-343`（注释论证「重启不该忘掉退避指数」） | 冷却与 perf 全在内存，**重启清零** |
| P95 延迟（而非均值） | `pool/latency.go:1-70`（还写了「为什么只采成功样本」） | 只算均值，尾部被掩盖 |
| 账号级在途租约 + `minPickGap` | `pool.go:627-670` | 只有订阅级并发 |
| 上游 TTFB 比较（跨上游切流） | `pool.go:1903-1993` | 无 |

---

## 建议的落地顺序

```
第 1 批（2 人日，零风险，纯函数/独立文件）
  A4 Claude 方言 usage 规范化   ← 修的是真缺陷
  A1 转换损耗诊断
  A2 usage 非零合并
  A5 错误体 9 字段兜底

第 2 批（2 人日，新增不动现有）
  A3 审计日志
  A7 alwaysSkipRetryCodes + A8 状态码区间
  A10/A11/A12 三个零依赖小工具

第 3 批（2-3 人日，现有结构加一维）
  B1 流结束分类 · B3 测试筛选 · B4 档位下沉 · B9 请求 ID

第 4 批（看你要不要）
  A6 tokenkit 权重表 · A14 模型名白名单审计 · B8 缓存命中率粘性

大动作（单独规划）
  C1 relaykit 做 Responses   ← 要先抬 go 1.25.1
  C2 relaykit 开 Gemini 上游
```
