# v2.0.5 — 2026-11-14

> 格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

补丁版，**相对 v2.0.4 的增量**（24 个提交 / 51 文件 / +7371 −175）。
主题：**把 phanthywork 的出站请求逐项对齐到官方客户端，并真机验证通过** —— 期间连续挖出四类"看起来对、其实不对"的缺陷，每一类的判据原先都在守护错误行为。

## 修复

### ★ phanthywork 推理路径缺 4 个证明头 —— 封号的直接原因

这是本轮最重要的一处。此前 `POST /v1/chat/completions` **完全不签名**，注释还写着"签名相关的一个都不要加"，理由是"抓包实证客户端全程零签名"。

那个实证**只覆盖了链路的一跳**：

```
官方：内核（Phanthy Codeo）→ 本地桥（sidecar, 127.0.0.1）→ 上游
抓到：内核 → 本地桥        ← 这一跳确实零签名
漏掉：本地桥 → 上游        ← 桥会补 12 段签名 + 4 个证明头
```

官方桥的路径集合逐字包含 chat（`Sd3 = new Set(["/v1/messages","/v1/responses","/v1/chat/completions"])`），而它的 proof 构造器 `_O8` 与本包 `InferenceCanonical` **逐段一致** —— 也就是说 12 段签名算法早就写对了，缺的只是"用在 chat 上"。

- 现在 chat 路径：`ensureInferenceToken` → `buildProof` → 挂 4 个 `x-phanthy-desktop-session` / `x-phanthy-proof-*`。
- `x-request-id` / `x-claude-code-session-id` 改取 `proof` 的值（它们同时是 canonical 第 9/10 段，签名与发头必须同源；此前各自随机生成）。
- **真机验证通过**：新注册账号，`registered_at_ms` 齐全，`deepseek-v4.1-flash-1017` 流式返回 200（93 tok / 1696 ms / 33 tok·s⁻¹）。
- 两条**在守护错误行为**的判据已反转：`TestChatSendsNoSignatureHeaders` → `TestChatPathCarriesDesktopInferenceProof`；`TestChatDoesNotFetchInferenceToken` → `TestChatFetchesInferenceToken`。

### phanthywork 三条 canonical 的分隔符是 LF，不是空格

`device/v1` 两条串曾按空格拼，导致设备注册恒 `401 invalid_desktop_signature` —— 而错误码完全指不到根因（凭证里从未出现过 `registered_at_ms`，表现成"账号被封"）。按官方源码逐字节核实：三条串**全部** `.join(LF)`。

### phanthywork 推理头与官方 sidecar 1:1

去掉 3 个官方根本不存在的头（`x-phanthy-client` / `x-phanthy-project` / `x-phanthy-product`），`client-source` 由 `cli` 改为 `desktop`（与注册声称的 `edition=phanthy_work` 一致），补设备指纹与版本头。

### phanthywork `/api/*` 的 UA 分档

UA 是按路径分档的，不是全局一个值：`/api/oauth/*` 官方发 `phanthy-code-cli/<APP_VERSION>`，而推理路径官方**不设**（Bun 下即 `Bun/1.3.14`）。此前全局用后者，`/api/*` 那一族是错的。

### phanthywork 取邀请码的幂等键多了尾巴

官方是 `referral-code:<installationId>`（两段），本仓与第三方插件记的都是三段（多日期/毫秒段）。多一段就与官方不是同一个键，上游的去重语义整个失效，且**不会有任何报错**。

### workbuddy-intl 目录与登录弹窗

模型目录改由 `/v3/config` 兜底；修掉"无法创建数据目录 `.browser-profiles\…`"那个 Edge 弹窗（`profileRoot` 恒取绝对路径）。

### loomy 模型目录改为并集缓存

从「每次现查单账号」改为「多账号并集 + 24h 定时刷新」，并修掉生图模型重复追加（`handler.go` 加 `seenAt` 去重）。

## 新增

### phanthywork 三个活动面端点

从官方桌面端前端 bundle（`_up_\dist\assets\App-dSHMvpu3.js`，明文可读，不需抓包）逐字取出定义后补齐：

```
GET  /api/oauth/rewards                        FetchRewards
GET  /api/oauth/referrals/relations            FetchReferralRelations
POST /api/oauth/activities/onboarding/reconcile            ReconcileOnboarding
POST /api/oauth/activities/onboarding/tasks/<id>/start     StartOnboardingTask
POST /api/oauth/activities/onboarding/tasks/<id>/complete  CompleteOnboardingAction
```

响应一律返回 `json.RawMessage` —— 官方前端只是把 JSON 往下传，bundle 里没有展开字段；**给未知结构编字段就是重复本轮的错误**。

### 模型目录页「行内动作」

供应商行收窄成一列，只保留「重拉」；新增 `GET /ui/models/explain` 与重拉端点（写入面单独鉴权）。

## 变更

### 协议集中到 `docs/protocols/` + `docs/decisions/`

三份协议 + 两篇 ADR + 一份交接文档入库，并从 `.gitignore` 逐条放行（目录级白名单要用「目录 + `/**`」两条）。

### 前端操作反馈统一走共享通知层

`internal/server/toast.js` 是唯一实现，由 `tools/toast_sync.js` 在标记之间注入；会切换文案的按钮固定 `min-width`（否则每次点击界面都会位移）。附 8 条结构判据。

## 判据

`go build ./...` exit 0，`go test ./... -count=1` **52 包零 FAIL**。
本轮新增/反转的判据都做过**反向验证**（注入违规 → 确认变红 → 还原），包括：
`TestChatPathCarriesDesktopInferenceProof`、`TestChatFetchesInferenceToken`、
`TestReferralCodeIdempotencyKeyIsByteExact`、`TestSignV1RejectsQueryInPath`、
`TestBuildProofRejectsQueryInPath`、`TestCanonicalSeparatorsArePerString`。

---
# v2.0.4 — 2026-11-13

> 格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

补丁版，**相对 v2.0.3 的增量**。主题：把"看起来能用"改成"能证明它真的对" —— 本轮几乎每一处都先拿到一条会失败的命令再动手。

## 新增

### 渠道页「路由诊断」（只读）

- 输入模型名即可看到：这条模型有哪些候选、各自被选中多少次、权重与可用 key 数、以及**是否均匀**。
- 均匀性用统计带宽判定：`x_i ~ Bin(n, p_i)`、`|x_i − μ_i| > 3σ` 才报偏斜（单条误报率约 0.27%）；
  `n < k×10` 一律报「样本不足，暂不判定」。**按权重折算期望** —— 9:1 权重下 90/10 是预期而非偏斜。
- 接口 `GET /ui/channels/explain` 与 `/ui/channels` 同一套身份判据（未认证 401），只查表 + 读进程内计数，**不发任何上游请求**。

## 变更

### 渠道优先级去掉，全部候选平等轮询

- `Plan` 不再按 priority 排序（只按 id，保证同输入两次计划一致）；**档位概念、`candidatesOfTier`、「档位用尽」错误**一并删除。
- 重试＝换一条没试过的（`tried` 是唯一排除条件），终止＝全部试过（防无限重试）。`channelstore.Select` 同口径，避免两条选路路径判据打架。
- `Priority` 字段保留但**不读**（零迁移回退，老配置不会让实例起不来）；渠道页移除优先级输入/批量/列/排序项。
- 实测：6 条候选**故意设成 100/90/80/70/60/50**、600 次采样 ⇒ 分布 82~111，无系统性偏斜。

### 破甲注入改为三态（无 / 启用 / 禁用只拦截）

- 禁用档命中即**不发出**，回 `prompt_armor_blocked`，且**绝不算账号失败**（不打号、不换号、不进冷却）。
- 每个状态各配「范围（全部 / 只名单内 / 除名单外）+ 名单（上游 / 渠道）」；
  名单改成**选择器 + chip**，候选永远跟随 manifest（不再被空缓存钉死）并**支持搜索**。
- 观察模式默认开：先看命中量与误判率，再手动切拦截。内置模式集收窄为 11 条**高特征句**（"什么是越狱"这类正常问句不再命中）。
- 性能（L6 实测）：138 KB 文本 1.45 ms/op（95 MB/s、1 alloc），含 JSON 解析 1.84 ms/op。

### 账户列改为字段驱动

- 邀请码面板与任务表在「余额」整列都没有有意义取值时**整列隐藏**（含表头）；0 不算有意义取值。
- 顺带修掉「字段不存在时渲染 0」的假数据 —— 违反「未探测 ≠ 0」。

### phanthywork 导入识别第四种来源格式

- 认顶层驼峰 `userid / accessToken / refreshToken / expiresAt`（此前 `userid` 没被读，uid 会回落成 token 哈希）。
- 有根键（`claudeAiOauth` 等）时仍以既有形态为准；旧三种形态行为一字未变。

## 修复

### 后台续期提前量必须大于扫描周期

- 原判据「剩余 ≤ 寿命一半」（30 分钟寿命 ⇒ 15 分钟）配 10 分钟扫描间隔 ⇒ 令牌可能在两次扫描之间过期（真机表现为 TOKEN 剩余掉到 1 分钟）。
- 改为 `max(寿命一半, 扫描周期 + 2 分钟缓冲)` = 12 分钟地板；判据仍只有一处（`needsRefresh`），并新增「出站会刷 ⇒ 后台必刷」的同源性断言。

### 死号不再每 20 秒打一次上游

- 凭据类终态失败（如「登录已失效」）的号进入**死号台账**，按既有「账号解封探测」间隔（默认 24h）重试，到期放回一次；台账随 `panels.json` 落盘，重启不会把坏号重打一遍。
- 同时修掉「待办数抖动就把退避清零」：现在只在待办**下降**时回到追赶态，涨了既不奖励也不惩罚。
- 这两条是独立缺陷，叠在一起曾让预热永久空转（日志每 20~40 秒一条、每轮真打上游）。

### 懒注册导致健康账号被自动禁用

- 额度 / 签到 / 邀请码路径此前都拿**本地派生**的 installationId 直接签名 ⇒ 未注册的新号必被上游拒，而那个 `401 desktop_installation_required` 又被判成账号层失效 ⇒「一次即摘」。
- 现在统一走 `ensureReady`（注册 + 就绪）；该标记归为「只换号不罚」，并加了反向锁（`invalid_grant` 仍必须置位）。

### 控制台 `hidden` 属性对 `.field` 完全无效

- `el.hidden = true` 只是 UA 样式的 `display:none`，作者样式的 `display` 永远盖过它 ⇒「选关闭时配置区不消失」。
- 补一条**全局** `[hidden]{display:none !important}`，一处修好 11 个同类调用点（设置页显隐、日志提示、操作菜单、任务条、渠道备注…）。
- `!important` 不是保险起见：`.field{display:flex}` 与它同为作者样式、specificity 相同，不加就变成"谁写在后面谁赢"。

### 邀请码面板刷新报 `hintText is not defined`

- 提示文案的计算被误写进模板字符串**内部**（那些语句只是 HTML 文本）。已移到模板外，并新增**执行式**守卫（在 node 里真跑那段代码，未声明的名字必然报错）。

## 内部

### 护栏扩容：三处「假绿」被修

- 「前端不许按上游名分叉」的名字表从 **2 个补到 13 个**（扫各上游包源码声明，带 fail-closed 下限：扫到 <10 个直接 Fatal）；
  并补上扫描面 —— 前端拆包后 `ui/app.js` 那 **13k 行此前从未被这条护栏扫过**。
- 跨包护栏覆盖新的余额判据，并要求判据体里**不许出现字符串字面量**（出现即"按上游名分叉或写死取值"）。
- 设置页浏览器探针按现实更新：容器数改为**下限**并注明维护方式，结构性判据（未落容器、类型单一、两口径字段总数互相对上）一条未删。

### 测试确定性

- 渠道选路与诊断的随机源可注入（默认仍为全局 `rand`，生产行为不变）；原先依赖随机采样、会偶发变红的用例改为固定种子，并加「故意不均匀必须报偏斜」的**正向对照**。
- 稳定判据用 `-count=20` / `-count=10` 反复跑，而不是"跑一次绿"。

### CI 抖动的真根因

- `New()` 末尾的预热 goroutine 调 `statsItems()` **读库**，而 `store.DB` 是包级单例、文件在 `t.TempDir()`；0.01s 的用例结束后预热仍在读 ⇒ 与 `RemoveAll` 抢目录（Linux 特有的 `unlinkat: directory not empty`）。
- 测试进程内改为**同步**执行预热（`testing.Testing()`）+ 提供可等待信号；一处收口修好 6 个同因用例文件。

### 修复本轮 CI 的格式化门禁

- 三个文件（`phanthywork/provider.go`、`prompt/armor_detect.go`、`prompt/armor_detect_test.go`）未过 `gofmt`，导致连续 4 次 CI 与本次 Release 的 `Check formatting` 失败、打包步骤被跳过。已格式化。
- ⚠ Windows 上 `gofmt -l .` 会因 CRLF 全量误报（CI 注释已记），判据应以 Linux/LF 口径为准。

## 已知边界

- 破甲检测只扫提示词文本：JSON 转义串（`\u7834\u7532`）与 base64 内嵌不检出（需解码层，未做）。
- 续期提前量覆盖不了「整整漏掉一轮」：那需要地板约 2 倍扫描周期（≈22 分钟），代价是流量翻倍与桌面端互踢机会增加。
- 导入**不做**令牌有效性预检：refresh 已失效的凭证仍会导入成功，但永远查不到额度（需重新登录）。
- 渠道名单按**渠道名**匹配（名字为空时回落 id），不做第三级（模型级）。
