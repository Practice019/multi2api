# codar 能力对齐盘点（codar2api × 本仓扩展点）

> 角色：T-316 调研 + 规划。**本轮不写实现、不发请求、不改代码**（除本报告）。
> 数据来源（全部第一手，逐条给出处）：
> · `D:\project_GIT\codar2api`（T-310 clone，HEAD `ba05367`）—— 源码 + README
> · `D:\project_GIT\workbuddy2api\internal\gateway\*.go` —— 扩展接口定义（`go list` 级事实）
> · 本仓各上游实现（codearts / zcode / loomy / monkeycode …）—— 实现模式参照
> 标记约定：**【源】**= 源码里读过的事实；**【推】**= 我的推断/待验证。

---

## 0. 结论摘要

| 扩展点 | 其它上游谁有 | codar 现状 | codar2api 有对应上游 API | 建议 | 优先级 |
|---|---|---|---|---|---|
| `DisplayNameExt` | 多数 | ✗ | — | 加（一行） | **P0** |
| `NoticeExt` | 部分 | ✗ | — | 加（token 会过期 + 风控提醒） | **P0** |
| `CredentialTokenExt` | 部分 | ✗ | — | 加（一行：token 非空） | **P0** |
| `CredentialLifetimeExt` | 部分 | ✗ | — | 加（JWT 有 exp ⇒ `false`） | **P0** |
| `AdminExt`（AdminRoutes） | **全部 11 个** | ✗ | ✓ 签到/余额/用量/活动全有 | 加（4 条只读/低频端点） | **P0** |
| `DailyActionExt` + `CapCheckin` | 8 个 | ✗ | ✓ `/v1/daily-sign/status`、`free-model` | 加（手动按钮优先） | **P1** |
| `DailyCheckinExt`（CheckinAll） | 8 个 | ✗ | ✓ 同上 | 加（低频、可关） | **P1** |
| `QuotaExt`（RefreshQuota） | zcode / minimax 等 | ✗ | ✓ `/api/user/self` + `/v1/agent/ledger` + `usage/daily` | 加（响应结构需实测） | **P1** |
| `ModelMultiplierExt` | 少数 | ✗ | ✓ 目录 `pricing`（CNY/百万 token） | 加（成本可视化，几乎零风险） | **P1** |
| `JobExt` | 部分 | ✗ | ✓ 续期 `/v1/gettoken` | 加（本仓 scheduler 已就绪） | **P1** |
| `HealthProbeExt` | 少数 | ✗ | ✓ `GET /api/user/self` | 加（低成本） | **P2** |
| `AccountImportExt` | 部分 | ✗ | — | 加（粘贴 207 字符 JWT 即导入） | **P2** |
| `EgressExt` / `EgressCheckExt` | 少数 | ✗ | ✓ codar2api 有代理池概念 | 加（本仓已有代理池） | **P2** |
| `LoginFlow` / `ManualLoginExt` / `QRLoginExt` / `LoginOptionsExt` | codearts/monkeycode 等 | ✗ | ✓ `/phone/*`、`/wechat/*`（codar2api 已实现） | 加（成本最高、收益也高） | **P3** |
| `ResetPolicyExt` / `SoftRateExt` / `CandidateTierExt` | 少数 | ✗ | ✓（429/风控语义） | 视需要 | **P3** |
| `CapInvite` | loomy（codearts 刚删） | ✗ | ✓ `/v1/promotion/info` | ★ **不做**（用户刚删掉邀请码功能） | — |
| `ImageGenExt` / `ImageModelExt` | 无上游有 | ✗ | ✗ 目录 15 个模型**全是文本** | **不做**（无能力） | — |
| `ChatToolExt` | 少数 | ✗ | ✗ | **不做**（无注入需求） | — |
| `CapGrowth` / `CapTravel` | workbuddy | ✗ | ✗ | **不做**（无对应业务） | — |

**一句话结论**：codar 现在只做了「对话 + 目录 + 错误分类 + 续期窗口 + 到期」这一条**最小可用线**；要对齐基本盘，**P0 五项（都是小改动）** 就够了；真正有增量价值的是 **P1 的「签到/领免费模型 + 额度 + 成本倍率」**——它正好是 codar2api 存在的主要理由（每日 2000 积分、新号免费 DS V4.1 Flash）。

---

## 1. codar2api 的完整功能清单（**【源】**：路由 + 文件 + README）

### 1.1 它对外提供的协议面（**源**：README「🔌 API 端点」+ `app.go`）

| 端点 | 说明 |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions（流式/非流式） |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/messages/count_tokens` | Anthropic token 计数（**不计入统计**） |
| `GET /v1/models` | 当前 Key 可用的模型（按 Key 的模型范围过滤） |
| `GET /ping`、`/health` | 健康检查（免鉴权、不返回账号数据） |

### 1.2 账号管理（**源**：`/accounts/*` 路由）

```
/accounts/add  /import  /import-batch  /export  /delete  /batch  /update
/accounts/test /refresh /renew(续期)   /sign(签到) /claim(领免费模型) /redeem(兑换)
/accounts/usage(用量) /catalog(目录)   /requests(请求记录) /proxy(代理)
/accounts/concurrency(并发) /cooldown/clear(解除封控)
```

### 1.3 登录方式（★ 三种，**源**：路由）

```
本地登录：/local-login、/local-login/import         ← 读本机桌面端登录态（≈ 我们 T-313 手工做的事）
手机登录：/phone/login、/phone/code、/phone/captcha、/phone/verify-ip
微信扫码：/wechat/login/qrcode、/wechat/login/status
上游侧   ：/v1/login、/v1/verification-code、/v1/bind-phone、/v1/user/verify_ip
```

### 1.4 额度 / 账务 / 活动（★ 与"白嫖价值"直接相关）

| 能力 | 上游端点 | 价值 |
|---|---|---|
| 签到 | `POST {PayBase}/v1/daily-sign/status` `{username}` | **每日 2000 积分**（T-307 社区实测） |
| 领免费模型 | `GET {APIBase}/api/codar/activities/free-model` + `/api/codar/activities/mine` | 新号免费 **DeepSeek V4.1 Flash 7 天** |
| 兑换 | codar2api `/accounts/redeem` | 积分→额度 |
| 续期 | `POST {PayBase}/v1/gettoken` `{username, token:0}` | token 不过期（否则 207 字符 JWT 到期即废） |
| 余额 | `POST {PayBase}/v1/agent/balance` `{username}` | 显示余额 |
| 账单台账 | `GET {APIBase}/v1/agent/ledger` | 逐笔消耗 |
| 用量 | `GET {APIBase}/v1/codar/usage/daily?start_date=&end_date=` | 按日用量 |
| 价格 / 倍率 | codar2api `/prices`、`/credit-rate` | 成本可视化 |
| 邀请/推广 | `POST {PayBase}/v1/promotion/info` `{username, token:0}` | ⚠ 见 §5（用户刚删 codearts 的邀请码功能） |

### 1.5 调度与风控（**源**：README「核心行为语义」）

```
选号策略：权重 + 单账号最大并发
会话粘滞：X-Session-Id / X-Conversation-Id / Session-Id（头）
          session_id / conversation_id / prompt_cache_key（body）⇒ 同会话同账号；无 hint 才轮询
错误分类：account_restricted ⇒ 标记受限、停止派发（可批量解除）
IP 封控 ：上游 403 拦截页 ⇒ 标记**出口 IP**、换出口换号重试；只有模型请求会触发
代理池  ：固定 / 轮询；流式请求中途不换出口；改代理自动清除该出口封控
请求黑名单：被拦的 (path, model) 不再反复踩
定时任务：每 30s 巡检（续期 / 领免费模型 / 签到 / 刷新余额·活动·邀请·签到状态）
请求统计 + Key 计数：每 3s 批量落盘
```

### 1.6 面板与运维（**源**：`web/*` 约 200KB 前端 + 路由）

```
面板   ：dashboard.html + accounts.js / drawer.js / detail-diff.js / manage.js / add.js / pages.js / ui.js
        /stats(+request-detail) /settings /announcements /catalog /keys /proxy-pool(+check,manage) /about(+check-update)
诊断   ：--request-diagnostics ⇒ request-diagnostics.jsonl（入站/转发头与正文/上游状态）
        请求调试日志（全部/正常/错误三档，详情含请求体与响应，仅管理员可见）
自更新 ：读自己 GitHub Release（update.go:17-18）
部署   ：Dockerfile + compose.yaml；`.env` 配置
```

### 1.7 **它没有**的（明确排除，**源**）

```
✗ 生图（无任何 image 路由；Codar 目录 15 个模型全是文本）
✗ 多协议之外的 Provider（只有 Codar 一个上游）
✗ 图片/文件输入（目录里有 input_modalities: ["text","image"]，但它没有转发文件上传的端点）
```

---

## 2. 本仓扩展点清单（**源**：`internal/gateway/*.go` 接口定义）

### 2.1 必实现的四个（`Provider`）

```
ID() string · Caps() Capability
Chat(ctx, cred Credential, body []byte) (ChatStream, error)
Models(ctx, cred Credential) ([]ModelInfo, error)
```

### 2.2 可选扩展点（类型断言发现，**共 27 个接口名**）

| 接口 | 方法（**源**，部分签名以 `gateway` 内定义为准） | 本仓谁有 |
|---|---|---|
| `AdminExt` | `AdminRoutes() []AdminRoute` | **11/11 全部上游** |
| `JobExt` | `Jobs() []Job` | 部分 |
| `AuthDirExt` | `AuthDir() string` | 全部 |
| `CredentialLoader` | `LoadCredentials(dir) ([]Credential, error)` | 全部 |
| `CredentialSecretLoader` | `LoadCredentialsWithSecrets(dir) ([]CredentialSecret, error)` | 全部 |
| `ErrorClassifier` | `Classify(status int, body string) ErrorKind` | 全部 |
| `CredentialExpiryExt` | `TokenExpiry(cred) (int64, bool)` | 多数 |
| `CredentialLifetimeExt` | `NeverExpires(cred) bool` | 部分 |
| `CredentialTokenExt` | `HasToken(cred) bool` | 部分 |
| `RefreshSkewExt` | `RefreshSkew(cred) (time.Duration, bool)` | 多数 |
| `QuotaExt` | `RefreshQuota(uid) (QuotaView, bool)` | zcode/minimax 等 |
| `ResetPolicyExt` | `ResetAt(cred) (time.Time, bool)` | 少数 |
| `SoftRateExt` | `SoftRateReset(status, body) (time.Time, bool)` | 少数 |
| `CandidateTierExt` | `CandidateTiers(model, uids) map[string]int` | 少数 |
| `ModelMultiplierExt` | `ModelMultipliers(ctx, cred) (map[string]float64, error)` | 少数 |
| `DailyActionExt` | `DailyActions() []DailyAction` | 8 个 |
| `DailyCheckinExt` | `CheckinAll(ctx) DailyCheckinReport` | 8 个 |
| `AccountColumnsExt` | `AccountColumns() []string` | 3 个 |
| `AccountImportExt` | `ImportCredentials(pasted)` | 部分 |
| `DisplayNameExt` | `DisplayName() string` | 多数 |
| `NoticeExt` | `Notice() string` | 部分 |
| `HealthProbeExt` | `ProbeHealth(ctx, cred) error` | 少数 |
| `LoginFlow` | `Start() (state, authURL, err)` / `Poll(state) (Credential, error)` / `Configured()` | codearts/monkeycode |
| `ManualLoginExt` | `ManualLogin() (path, instructions string)` | codearts/monkeycode |
| `QRLoginExt` | `QRLoginSVG(authURL) string` | 少数 |
| `LoginOptionsExt` | `LoginOptions() []LoginOption` | 少数 |
| `EgressExt` / `EgressCheckExt` | `Egress() EgressInfo` / `EgressCheck(ctx) []EgressProbe` | 少数 |
| `ImageGenExt` / `ImageModelExt` | `GenerateImage(...)` / `ImageModels() []ImageModel` | **无上游有** |
| `ChatToolExt` | `ChatTools() []ChatTool` | 少数 |

能力位（`Capability`，**源**）：`CapChat`（必须）· `CapModels` · `CapCheckin` · `CapGrowth` · `CapTravel` · `CapWelfare` · `CapQuotaProbe` · `CapTasks` · `CapInvite` · `CapImport`

### 2.3 codar 当前实现的（**源**：`internal/codar/*.go`）

```
必需：ID · Caps（= CapChat|CapModels）· Chat · Models
扩展：AuthDir · LoadCredentials · LoadCredentialsWithSecrets · Classify · RefreshSkew · TokenExpiry
⇒ 共 10 个；缺 27 个可选接口里的 20 个（其中真正有价值的见 §0/§4）
```

---

## 3. codar 可实现的项（API 依据 + 实现草图 + 成本）

> 依据列里 **[源]** = 端点存在于 codar2api 源码或 README；**[测]** = T-311 我实测过；**[推]** = 需新实测。

### 3.1 `AdminExt`：4 条管理端点（P0）

```go
func (p *Provider) AdminRoutes() []gateway.AdminRoute   // 参照 internal/codearts/admin.go:96
  GET  /admin/codar/usage   → GET  {APIBase}/v1/codar/usage/daily?start_date=&end_date=   [源][推结构]
  GET  /admin/codar/quota   → POST {PayBase}/v1/agent/balance {username}                   [源][推]
  POST /admin/codar/sign    → POST {PayBase}/v1/daily-sign/status {username}               [源][推]
  POST /admin/codar/claim   → GET  {APIBase}/api/codar/activities/free-model               [源][推]
```
- 成本：**低**（1 个文件 ~150 行；HTTP 客户端已有，鉴权头已有）
- 风险：**响应结构未实测** ⇒ 先各打一次抓结构（≤4 次请求，不耗模型额度）→ 再定 JSON 解析
- 收益：界面能看到**用量/余额/签到状态/可领免费模型** ⇒ 这是 codar2api 的立身之本

### 3.2 `DailyActionExt` + `DailyCheckinExt` + `CapCheckin`（P1）

```go
func (p *Provider) DailyActions() []gateway.DailyAction        // 参照 internal/codearts/dailyactions.go:79
func (p *Provider) CheckinAll(ctx) gateway.DailyCheckinReport  // 参照 internal/codearts/dailycheckin.go:42
  "签到"      → POST {PayBase}/v1/daily-sign/status
  "领免费模型" → GET  {APIBase}/api/codar/activities/free-model
```
- 成本：**中**（要接 `checkinlog` 记历史 + 与 scheduler 的节奏对齐 —— codearts 有现成模式可抄）
- ⚠️ **风控**：T-307 社区已见 `API access is restricted` ⇒ 建议**默认只给手动按钮**、自动节奏放最长档

### 3.3 `QuotaExt`（P1）

```go
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool)   // 参照 internal/zcode/extensions.go:276
```
- 数据源三条（优先级从高到低）：`/v1/agent/ledger`（最细）→ `/api/user/self`（账号面）→ `/v1/codar/usage/daily`（日粒度）
- 另外：**T-311 实测响应里有 `billing{quota, cost_cny}`** ⇒ 即使在**流式**响应里也能累计本次花费 ⇒ 可做"网关侧自记用量"的兜底（不依赖上游额度接口）**[测]**
- 成本：**中**（要实测 3 个端点的结构 + 决定优先级）；收益：界面显示额度/用量曲线

### 3.4 `ModelMultiplierExt`（P1，几乎零风险）

```go
func (p *Provider) ModelMultipliers(ctx, cred) (map[string]float64, error)
```
- 依据：模型目录每项带 `pricing{currency:"CNY", unit:"per_million_tokens", input, output}` **[测]**（`glm-5.3-flash` 0.8/2.8 最便宜）
- 成本：**极低**（目录已经拿到，只是把它投影成倍率）；收益：成本可视化 + 调度时可偏向便宜模型

### 3.5 `JobExt`（P1）

```go
func (p *Provider) Jobs() []gateway.Job   // 续期：POST {PayBase}/v1/gettoken {username, token:0}
```
- 成本：**低**（本仓 `scheduler` + 各上游 `Jobs()` 模式现成；`Due` 支持事件驱动 —— 见 codearts 的 refresh 修复）
- 收益：**token 不再需要人工重导**（现在 `expiresAt=2026-11-15` 到期就废）

### 3.6 其余（P2/P3）

| 项 | 成本 | 备注 |
|---|---|---|
| `DisplayNameExt` / `NoticeExt` / `CredentialTokenExt` / `CredentialLifetimeExt` | 极低（各几行） | 界面文案与"这个号有没有 token" |
| `AccountImportExt` | 低 | 粘贴 `Bearer` 后的 207 字符 JWT；已有 `ImportFromDesktopConfig` |
| `HealthProbeExt` | 低 | `GET /api/user/self` 探活 |
| `EgressExt` / `EgressCheckExt` | 中 | 本仓已有代理池；codar2api 的"IP 封控"经验可直接复用（我方 `Classify` 已区分 IP/账号 ✓） |
| `LoginFlow` / `ManualLoginExt` / `QRLoginExt` | **高** | 手机验证码 / 微信扫码登录要逆向 `/phone/*`、`/wechat/*`（codar2api 已实现，可参考）；收益：能自助加号 |
| `ResetPolicyExt` / `SoftRateExt` / `CandidateTierExt` | 中 | 只在有多号+精确限流时才值得 |

---

## 4. ★ 实现建议（分批）

### 第一批（P0 · 对齐基本盘，预计 0.5 天，零风险）
```
1. DisplayNameExt        → "科搭 Codar"
2. NoticeExt             → "凭证来自桌面端会话（会过期）；上游已有风控记录，请低频使用"
3. CredentialTokenExt    → token 非空
4. CredentialLifetimeExt → false（JWT 有 exp）
5. AdminExt 的 4 条只读端点（usage / quota / sign / claim）
   ⚠ 先各打 1 次抓响应结构（≤4 次请求，不耗模型额度），再定解析
```
**理由**：其它 11 个上游**全部**有 `AdminRoutes`；缺它等于"这个上游在界面里没有入口"。而这 5 项全是"读一读、报一报"，不动调度、不动风控。

### 第二批（P1 · 有真实收益，预计 1.5–2 天）
```
6. DailyActionExt + DailyCheckinExt + CapCheckin   ← 签到 + 领免费模型（codar2api 的主要价值）
     · 默认手动按钮；自动节奏给最长档（风控）
     · 接 checkinlog 记历史（照 codearts）
7. QuotaExt（RefreshQuota）                        ← 额度/用量显示
8. ModelMultiplierExt                              ← 用目录 pricing 做成本倍率（几乎零风险）
9. JobExt（token 续期）                             ← 解决"token 到期即废"
```
**理由**：4/6/8 直接对应"每日 2000 积分 / 新号免费 DS V4.1 Flash / 成本可见"这三件让 codar 值得接入的事；9 是**可用性刚需**（不续期就得人工重导）。

### 第三批（P2/P3 · 可选）
```
10. HealthProbeExt / AccountImportExt / EgressExt(+Check)
11. LoginFlow / ManualLoginExt / QRLoginExt（最高成本，参考 codar2api 的 /phone/*、/wechat/*）
12. ResetPolicyExt / SoftRateExt / CandidateTierExt（多号精细化调度时才做）
```

---

## 5. 明确**不建议**做的（及理由）

| 项 | 理由 |
|---|---|
| `CapInvite` / 邀请码 | ★ **用户刚把 codearts 的邀请码功能删掉**（T-304）—— 现在再加一个上游的邀请码是逆着用户意图走。codar2api 有 `/v1/promotion/info`，但**不做**。 |
| `ImageGenExt` / `ImageModelExt` | Codar 目录 15 个模型**没有生图模型**（T-311 实测）；本仓也**没有任何上游**实现生图。声明它 = 画一个必然失败的按钮。 |
| `ChatToolExt` | 无"往对话里注入工具"的产品需求；且该能力在客户端侧更合适。 |
| `CapGrowth` / `CapTravel` | workbuddy 专有业务（拉新/差旅），codar 无对应上游接口。 |
| 自动高频签到/领取 | 上游已有风控迹象（T-307 社区 `API access is restricted`）；高频自动化会把**唯一一个号**打进受限，得不偿失。做"手动 + 低频"即可。 |
| 把 codar2api 整套搬进来（自建 Key/面板/代理池/统计） | 与本仓"多上游聚合"重叠 ⇒ 两套网关串联，运维翻倍（T-310 已论证）。只搬**协议与流程**，不搬**架构**。 |

---

## 6. 未决（下轮开工前要补的实测）

1. **4 个管理端点的响应结构未知**（`usage/daily`、`agent/balance`、`daily-sign/status`、`activities/free-model`）⇒ 需各打 1 次（≤4 次请求，不耗模型额度）。
2. `/v1/agent/ledger`（逐笔账单）是否可用、是否需额外鉴权 —— **未验**。
3. 签到/领取的**频率上限与奖励发放口径**（积分何时到账）—— **未验**；codar2api 每 30s 巡检，我方不应照抄。
4. 手机/微信登录端点（`/phone/*`、`/wechat/*`）的请求体与风控（滑块/验证码）—— **未验**，是第三批能否做的前提。
5. `SoftRateExt` 是否值得做：取决于上游 429 是否带"某模型某时刻"的精确信息 —— **未验**。
6. 本报告 §2 的**部分接口签名**是从 `gateway` 里截取的片段（若方法体跨行会被我截断）⇒ 开工时以 `internal/gateway/*.go` 原文为准。
