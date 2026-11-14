---
name: phanthywork 出站请求对照表（官方客户端 ↔ 本网关）
whenToUse: 改动 internal/phanthywork 的任何出站请求（新增端点、改头、改 body、改签名）**之前**必读；以及排查「请求看起来对但上游不认 / 账号被风控记账」这类问题时。
真源: 官方 sidecar 2.51.0 二进制字符串转储（`phanthy-sidecar-ascii.txt`）；第三方插件 magpie-PhanthyCode（`D:\temp\magpie-PhanthyCode`，标注「抓包→逐字符复现→200 通过」）；本仓 `internal/phanthywork/client.go` 的注释逐条记着出处
---

# phanthywork 出站请求对照表

> 这份表存在的理由：2026-11-14 一天之内在这个上游挖出**三个**"看起来对、其实不对"的缺陷
> （分隔符、幽灵头、UA 分档），每一个都**通过了全部测试**。原因是判据本身在守护错误行为。
> 所以：**改这个上游的出站请求之前，先来这张表对一遍**。

## 一、端点对照（官方 `Y59` 表 + `J59` 特判，逐字）

官方 sidecar 的端点映射表（二进制转储逐字）：

```js
Y59 = {
  account:              { method:"GET",  upstream:"/api/oauth/profile" },
  register:             { method:"POST", upstream:"/api/oauth/desktop-installations/register" },
  summary:              { method:"GET",  upstream:"/api/oauth/activities/summary" },
  claim:                { method:"POST", upstream:"/api/oauth/activities/daily-login/claim" },
  rewards:              { method:"GET",  upstream:"/api/oauth/rewards" },
  "referral-summary":   { method:"GET",  upstream:"/api/oauth/referrals/me" },
  "referral-code":      { method:"POST", upstream:"/api/oauth/referrals/code" },
  "referral-relations": { method:"GET",  upstream:"/api/oauth/referrals/relations" },
}
// J59 里另有两条特判（不在表里）：
//   onboarding + POST + reconcile      → /api/oauth/activities/onboarding/reconcile
//   onboarding + POST + tasks/{id}/{start|complete} → /api/oauth/activities/onboarding/tasks/{id}/{action}
// 另外 J59 特判 summary + POST 也允许（表里写 GET，POST 同样被接受）
```

| 官方 name | 方法 | 路径 | 本网关 | 状态 |
|---|---|---|---|---|
| account | GET | `/api/oauth/profile` | `FetchAccountScope` | ✅ |
| register | POST | `/api/oauth/desktop-installations/register` | `RegisterDevice` | ✅ |
| summary | GET（POST 也允许） | `/api/oauth/activities/summary` | `FetchSummary`（**POST**） | ✅ 实测可用 |
| claim | POST | `/api/oauth/activities/daily-login/claim` | `ClaimDailyLogin` | ✅ |
| referral-summary | GET | `/api/oauth/referrals/me` | `pathReferralsMe` | ✅ |
| referral-code | POST | `/api/oauth/referrals/code` | `pathReferralsCode` | ✅ |
| rewards | GET | `/api/oauth/rewards` | `FetchRewards` | ✅ 2026-11-14 补上 |
| referral-relations | GET | `/api/oauth/referrals/relations` | `FetchReferralRelations` | ✅ 2026-11-14 补上 |
| onboarding/reconcile | POST | `/api/oauth/activities/onboarding/reconcile` | `ReconcileOnboarding` | ✅ 2026-11-14 补上 |
| onboarding/tasks/{id}/{start\|complete} | POST | `/api/oauth/activities/onboarding/tasks/...` | `StartOnboardingTask` / `CompleteOnboardingAction` | ✅ 2026-11-14 补上 |
| （本网关独有） | GET | `/api/phanthy_code/models` | `FetchModels` | ✅ 官方也有（`CB1`） |
| （推理） | POST | `/v1/chat/completions` | `ChatCompletionsStream` | ✅ 默认路径 |
| （推理·旧内核） | POST | `/v1/messages` | `ChatStream` | ✅ 非默认 |

**四个缺口**都指向同一件事：**新手任务（onboarding）没实现**。而它很可能是"账号画像正常化"的关键
—— 新号不做 onboarding、注册完就推理，是最容易被风控记账的形态。实现前需要先抓包取证（别照名字编字段）。

## 二、头对照：**按路径分档**，不存在"一个全局头集"

### 2.1 `/api/oauth/*` 一族（官方通用调用器 `wtK`）

```js
function wtK(q, K) {
  return {
    Accept: "application/json",
    "Content-Type": "application/json",
    "User-Agent": `phanthy-code-cli/${GK9}`,   // GK9 = process.env.APP_VERSION || "2.1.0"
    "x-phanthy-client-source": "desktop",
    ...q.headers,                              // 调用方传入的 x-desktop-* / idempotency-key
    Authorization: `Bearer ${K}`,
  }
}
```

对应本仓：`apiCallUA` + `applyCommonHeaders` + 各调用点的 `applyV1`。

### 2.2 设备注册（`/api/oauth/desktop-installations/register`）

body 七字段**顺序即官方注入顺序**（签名覆盖这个串）：`public_key, edition, platform, version,
device_public_key, device_signature, inference_public_key`。

- `edition = "phanthy_work"`
- `platform` 走**映射**：`win32→windows` / `darwin→macos` / 其它 `linux`
- `version = 2.51.0`
- 头用 `applyV1`（`x-desktop-installation-id/-timestamp/-nonce/-signature` + `idempotency-key`）

### 2.3 推理（`/v1/chat/completions`）

官方头构造器（二进制逐字）：

```js
X = () => ({
  Authorization: `Bearer ${token}`,
  "Content-Type": "application/json",
  Accept: "application/json",
  "x-phanthy-client-source": "desktop",
  "x-phanthy-cli-device": <设备指纹>,
  "x-claude-code-session-id": sessionId,
})
// 调用处再补 "x-request-id": <uuid>；Auto 路由另加 "x-phanthy-auto-mode-router":"1"
```

**这条路径不发**：`x-app` / `x-phanthy-client-version` / 任何证明头。
（证明头是 `/v1/messages` 那条 12 段 canonical 才要的。）

官方**不存在**的头（曾经我们都发过，是封号成因）：
`x-phanthy-client`、`x-phanthy-project`、`x-phanthy-product`。

### 2.4 UA 分档表（★ 最容易搞错的一张）

| 路径 | 官方 UA | 本仓 | 依据 |
|---|---|---|---|
| `/api/oauth/*` | `phanthy-code-cli/<APP_VERSION>`（默认 `2.1.0`） | `apiCallUA` ✅ | 官方 `wtK` |
| `/api/phanthy_code/models` | **不设**（fetch 默认） | 走 `applyCommonHeaders` → `phanthy-code-cli/...` ⚠️ 多带 | 官方 `CB1` 只带 `Authorization` + `Accept` |
| `/v1/chat/completions` | **不设**（Bun 下即 `Bun/1.3.14`） | `Bun/1.3.14` ✅ | 官方 `X()` 不设 UA |
| `/v1/messages` | `claude-cli/<ver> (<USER_TYPE>, cli)` | `Bun/1.3.14` ⚠️ 未对齐 | 官方 `OR()`；版本段与 USER_TYPE **拿不到**，不发明 |

**教训**：`Bun/1.3.14` 在推理路径上**是对的**（官方也不设，Bun fetch 默认就是它）——
这解释了为什么"坑②"多轮真机验证都没报错。但它**不能**被当成"全局 UA"，
`/api/*` 那一族官方发的是 `phanthy-code-cli/<APP_VERSION>`。

## 三、两个已修复的"看起来对、其实不对"（留作反例）

| # | 缺陷 | 表现 | 怎么发现的 |
|---|---|---|---|
| 1 | device/v1 canonical 分隔符写成**空格**（官方是 **LF**） | 设备注册恒 401 `invalid_desktop_signature` ⇒ 凭证永远没有 `registered_at_ms` ⇒ 表现成"账号被封" | 逐字符对照官方 `proof.mjs`（三条 canonical 全部 `.join(LF)`） |
| 2 | 推理头多 3 个幽灵头 + `client-source` 写成 `cli` + 无设备指纹 | 403 `disabled_account`（真机封号） | 对照官方 sidecar 二进制；`x-phanthy-project` / `-product` / `-client` 在转储里出现 **0 次** |

**两次都通过了全部测试** —— 因为判据本身在守护错误行为：
一条**断言了**那两个幽灵头，另一条**从来没被写出来**（注释里引用了它）。

## 四、幂等键对照（写操作最容易错的一处）

**★ 真源升级（2026-11-14）**：官方**桌面端前端 bundle 就在本机**，不必再靠第三方插件转述 ——

```
D:\software\PhanthyWork-windows-x64\_up_\dist\assets\App-dSHMvpu3.js   (1.9 MB, Vite 产物)
```

它就是 magpie 当年引用的 `_up_\dist\assets\App-*.js`。整组活动面 API 在里面的一个对象里（下称 `Po`），
邀请面在另一个对象里（下称 `Id`）。**读它是"直接读源码"，不是抓包、也不用逆向 exe。**
（另：运行中的 sidecar 在 `127.0.0.1:38011` 上就服务这套前端，`curl` 即可取任意 chunk。）

| 操作 | 官方幂等键（逐字，来自 `Po`/`Id`） | 本仓 | 判定 |
|---|---|---|---|
| 每日签到 claim | `daily:${e.installationId}:${t}` | `daily:<installationId>:<serverDate>` | ✅ |
| 取邀请码 | `referral-code:${e.installationId}`（**两段，无日期/毫秒**） | 同（已修正） | ✅ |
| 注册设备 | `register:${e.installationId}` | 同 | ✅ |
| `activities/summary` | **不发**（canonical 第 7 段空串） | 同 | ✅ |
| onboarding start | `onboarding:start:${e.installationId}:${t}` | 同（已实现） | ✅ |
| onboarding complete | `onboarding:complete:${e.installationId}:${t}` | 同（已实现） | ✅ |
| onboarding reconcile | `onboarding:reconcile:${e.installationId}:${t??"latest"}:${Date.now()}` | 同（已实现） | ✅ |
| rewards（GET） | **不发** | 同 | ✅ |

**★ 两条键的语义差别（不要互相类推）**：

- `daily:…:<日>` —— **带**日期，表达"这一天"
- `referral-code:…` —— **不带**任何时间，表达"这个安装一个码"
- `onboarding:reconcile:…:<ms>` —— **带**毫秒，表达"这一次对账"（不是去重语义）

**取邀请码那条曾经错过**（2026-11-14 修正）：本仓原先是 `referral-code:<installationId>:<YYYY-MM-DD>`，
第三方插件 magpie 记的是 `…:<ms>` —— **两者都多了尾巴**。官方是两段。多一段就与官方不是同一个键，
上游的去重语义对我们整个失效，且**不会有任何报错**（只表现为"每次取码都可能建新码"）。
判据 `TestReferralCodeIdempotencyKeyIsByteExact` 已逐字节钉死，并做过反向验证（改回三段即红）。

## 五、★ 三条硬事实（从官方 `No()` 逐字读出，容易踩）

1. **GET 也要 v1 签名。** 官方 `No()` 对所有方法都算 `sign_desktop_activity`；
   GET 的 body 是 `undefined` → canonical 第 6 段是 `sha256("")` 的 base64url
   （`47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU`）。所以 `rewards` / `relations`
   两个 GET **同样带四个 `x-desktop-*` 头**，不是"只读就不用签"。
2. **签名用 `upstreamPath`，不是 `localPath`。** `rewards` 的 localPath 带 `?type=daily_login`，
   upstreamPath 是干净的 `/api/oauth/rewards` —— 那个 query 是**本地代理侧**的过滤意图，
   不发往上游、也不进 canonical。
3. **body 一律是字面量。** `start`/`complete`/`claim`/`referral-code` 的 body 是 `{}`
   （不是空串、不是 `null`）；`reconcile` 是 `{}` 或 `{"session_id":"…"}`；
   `summary` 是 `{"inference_public_key":"…"}`。

## 六、★ 一个会让签名必错的陷阱：canonical 的 path 段**不能带 query**

magpie `src/referrals.mjs` 的原文警告（已被官方前端第 2 条事实正面印证）：

> ⚠ `request()` 的 v1 canonical 用的是 `new URL(url).pathname`。本模块的两个端点都**没有 query**，
> 所以签出来是对的；将来若要加 `referrals/relations?limit=…`，必须先修 `upstreamPath`，否则必然 401。

**URL 可以带 query，签名的 path 不能带。** 本仓当前所有 `applyV1` 调用点都传无 query 的常量
（现在是对的），但这个陷阱的代价是"加个 `?limit=` 就 401、而错误码与签名错无法区分"。

⇒ 已在 `signV1` 里加了**当场断言**（`strings.Contains(p.Path, "?")` 直接报错），
判据 `TestSignV1RejectsQueryInPath` + `TestV1CanonicalPathSegmentHasNoQuery`。
将来加 `rewards?type=daily_login` / `referrals/relations?limit=…` 时，会立刻被拦住而不是等真机 401。

## 七、四个缺口已于 2026-11-14 补齐（实现 + 判据）

### 7.1 实现位置

`internal/phanthywork/onboarding.go`（判据 `onboarding_test.go`）：

```
FetchRewards              GET  /api/oauth/rewards
FetchReferralRelations    GET  /api/oauth/referrals/relations
ReconcileOnboarding       POST /api/oauth/activities/onboarding/reconcile
StartOnboardingTask       POST /api/oauth/activities/onboarding/tasks/<id>/start
CompleteOnboardingAction  POST /api/oauth/activities/onboarding/tasks/<id>/complete
```

两个通用发送器 `signedGetJSON` / `signedPostJSON` 对应官方 `No()` 那条**单一代码路径**
（官方把所有方法都塞进同一个 `No()`，我们照搬这个形状，减少"某条路径忘了签名"的可能）。

### 7.2 仍未做的（有意为之，不是遗漏）

- **`POST /referrals/bind`** —— 官方存在，但 magpie 刻意不实现、也不导出路径；本仓同样不做。
- **响应结构** —— 四个端点都返回 `json.RawMessage`：官方前端只是把 JSON 往下传，
  bundle 里没有展开字段。**给未知结构编字段就是重复今天的错误**，所以不臆造。
  已知的只有钱包字段名 `wallet{availablePoints,totalPoints,balancePoints,reservedPoints}`
  与邀请关系的 `{schema_version, next_cursor, relations:[]}`（这是本包既有实测记录）。
- **`CapTasks` 能力声明与面板** —— 能力声明是"承诺"，要有 UI 才能兑现
  （见 `Caps()` 附近的断言 `TestCapsDoNotPromiseTasksYet`）。本轮只做**地基**：
  方法能被调用、能被判据守住。UI 单独一轮。

### 7.3 这一轮的取证方式（值得复用）

**没有抓包、也没有逆向 exe** —— 官方前端 bundle 在安装目录里就是解压好的：

```
D:\software\PhanthyWork-windows-x64\_up_\dist\          ← 解压好的前端（430 个 .js / 17.9 MB）
  └ assets\App-dSHMvpu3.js                              ← 活动面 + 邀请面全部 API 定义
```

补充两条更容易的取法（都不需要动官方进程）：

1. 运行中的 sidecar 在 `http://127.0.0.1:38011/` **服务这套前端**，任意 chunk 直接 `curl`。
2. 想改它的 base URL 做抓包：`PUT /api/settings/phanthy-base-url`（官方接口，返回
   `requiresRestart: true`；写入 `~/.claude/settings.json` 的 `phanthyBaseUrl`）。

**教训**：这个上游的关键事实几乎都能从**本地明文**拿到。之前绕了远路（抓包脚本、
WebView2 缓存、exe 解压）都是因为没先 `ls` 安装目录里的 `_up_`。

## 八、纪律（这份协议的核心）

1. **改出站请求之前，先对这张表**；表里没有的，先去官方二进制/源码找真源，**别照名字编**。
2. **判据必须反向验证**：故意造一个违规，确认它真的会红。没有反向验证过的判据不算数
   （本次两条故障判据都栽在这上面：一条锁死幽灵头、一条从没被写出来）。
3. **头/串的差异优先按字节核对**：空格 vs LF、`x-phanthy-client` vs `x-phanthy-client-source`
   这种差异渲染出来一模一样，只有按字节比对才拦得住。
4. **拿不准就不改**：两个来源冲突、又无法用真机分辨时，**保持现状并记档**，
   等有账号能复验再动。今天的封号就是"按推测改"的代价。

## 九、判据索引

| 判据 | 守什么 |
|---|---|
| `TestInferenceHeadersMatchOfficialClient` | 推理头**双向** 1:1（官方有的必须在、官方没有的必须不在） |
| `TestCanonicalSeparatorsArePerString` | 三条 canonical 分隔符逐字节 = LF（官方三处都是 `` .join(`<LF>`) ``） |
| `TestCommonHeadersOnEveryRequest` | `/api/*` 公共头三项（UA = `phanthy-code-cli/2.1.0`） |
| `TestSignV1RejectsQueryInPath` | canonical 的 path 段带 query ⇒ 当场报错（不含反向即算未验证） |
| `TestV1CanonicalPathSegmentHasNoQuery` | canonical 第 3 段就是 pathname |
| `TestReferralCodeIdempotencyKeyIsByteExact` | 取码幂等键逐字节 = `referral-code:<installationId>`（**不许带日期/毫秒**） |
| `TestFetchRewardsIsSignedGetOnCleanPath` | rewards：GET + 干净路径 + v1 签名 |
| `TestFetchReferralRelationsIsSignedGet` | relations：GET + v1 签名 |
| `TestStartAndCompleteOnboardingTask` | 任务动作：路径 / 字面量 `{}` / 稳定幂等键 / v1 签名 |
| `TestOnboardingTaskIDValidatedLocally` | 非法任务 id ⇒ **零请求**（本地挡掉） |
| `TestReconcileOnboardingBodyAndKey` | reconcile body 两态 + 键含 `latest`/sessionId |
| `TestOnboardingReconcileKeyIsUniquePerCall` | reconcile 键含 `Date.now()` ⇒ 每次不同 |
| `TestSignedGetHashesEmptyBody` | GET 的 canonical 第 6 段 = `sha256("")` base64url |
| `TestChatSendsNoSignatureHeaders` | chat 路径不带任何证明头 |
| `TestDefaultUAIsBun` | 默认 UA 仍是 `Bun/1.3.14`（推理路径那条） |
| `TestChatUsesChatCompletionsPath` | 推理走 `/v1/chat/completions` |
