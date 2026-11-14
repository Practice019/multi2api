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

**这条路径发**：`x-phanthy-client-source: desktop`、`x-phanthy-cli-device`、`x-claude-code-session-id`、
`x-request-id`，**外加 4 个 `x-phanthy-proof-*` 证明头**（见 §十一）。

> ⚠⚠ **2026-11-14 第二次更正**：这里原先写着「这条路径不发：任何证明头」，
> **那半是错的**。原因是当时只抓到了链路的一跳（内核 → 本地桥），
> 而桥转发到上游时会补 12 段签名 + 4 个证明头。详见 §十一。
> 结论：`X()` 只是 **Auto 路由器**那条调用的头，不是发往上游的完整形态。

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
| `TestChatPathCarriesDesktopInferenceProof` | chat 路径**必须带齐** 4 个 `x-phanthy-proof-*`；且不许带 v1 的 `x-desktop-*`（**原 `TestChatSendsNoSignatureHeaders` 已在守护错误行为，已反转**） |
| `TestChatFetchesInferenceToken` | chat 路径**必须**调 `activities/summary` 换令牌（原 `TestChatDoesNotFetchInferenceToken` 已反转） |
| `TestDefaultUAIsBun` | 默认 UA 仍是 `Bun/1.3.14`（推理路径那条） |
| `TestChatUsesChatCompletionsPath` | 推理走 `/v1/chat/completions` |

## 十、官方推理桥（sidecar `gd3`）的行为对照

真源：`D:\temp\phanthy-sidecar-ascii.txt`（官方 sidecar 2.51.0 二进制字符串转储）里
`gd3` / `_O8` / `md3` / `cd3` 四个函数，逐字。**2026-11-14 逐条核对过我们实现。**

### 10.1 桥的入口闸（逐字）

```js
let K = new URL(q.url),
    _ = !K.search || (K.pathname === "/v1/messages" && K.search === "?beta=true");
if (q.method !== "POST" || !Sd3.has(K.pathname) || !_) return new Response(null, {status:404});
if (!/^Bearer .+$/i.test(q.headers.get("authorization") ?? "")) return new Response(null, {status:401});
// Sd3 = new Set(["/v1/messages","/v1/responses","/v1/chat/completions"])
```

| 桥的规则 | 本仓 | 判定 |
|---|---|---|
| 只接受 POST | 我们只发 POST | ✅ |
| pathname ∈ 那三个之一 | 三个路径都是常量 | ✅ |
| **query 必须为空**（唯一例外 `/v1/messages?beta=true`） | `signV1` 有 query 断言 | ✅ |
| `Authorization` 必须匹配 `^Bearer .+$` | `AuthorizationHeaderValue` 恒加 `Bearer ` 前缀 | ✅ |

### 10.2 proof 构造器 `_O8`（12 段）逐段核对

```js
function _O8(q, K, _, z){                       // q=req, K=pathname, _=body, z=headers
  if (!iy) return null;                          // 无推理令牌 ⇒ 不发任何 proof 头
  if (!KO8(iy).valid) return null;               // 令牌无效/过期 ⇒ 不发
  let w = String(Math.round(N44())),             // 服务端口径毫秒
      O = pp3(24).toString("base64url"),         // nonce 24B → 32 字符
      $ = (H) => D44("sha256").update(H).digest("base64url");
  let Y = ["desktop-inference-v2", q.toUpperCase(), K, w, O, $(_),
           sK6(z.get("authorization") ?? ""), $(iy),
           z.get("x-request-id") ?? "", z.get("x-claude-code-session-id") ?? "",
           z.get("x-phanthy-expert-id") ?? "", z.get("x-phanthy-skill-names") ?? ""].join(`<LF>`);
  return { "x-phanthy-desktop-session": iy, "x-phanthy-proof-timestamp": w,
           "x-phanthy-proof-nonce": O, "x-phanthy-proof-signature": ed25519(Y) }
}
function sK6(q){ return D44("sha256").update(q).digest("base64url") }   // ← 逐字
```

| 段 | 官方 | 本仓 `InferenceCanonical` | 判定 |
|---|---|---|---|
| 1 | `desktop-inference-v2` | `inferenceCanonicalPrefix` | ✅ |
| 2 | `method.toUpperCase()` | `strings.ToUpper(method)` | ✅ |
| 3 | **`pathname`** | 传常量路径（无 query） | ✅ |
| 4 | 服务端口径毫秒 | `serverNowMS(localMS, a.skew())` | ✅ |
| 5 | nonce **24 字节 base64url**（32 字符） | `newInferenceNonce()` | ✅ |
| 6 | `sha256(body)` base64url | `sha256B64URL(body)` | ✅ |
| 7 | `sK6(authorization)` = **sha256 base64url 的整串**（含 `Bearer `） | `sha256B64URL([]byte(authorization))` | ✅ |
| 8 | `sha256(推理令牌)` | 同 | ✅ |
| 9 | `x-request-id ?? ""` | `reqID` | ✅ |
| 10 | `x-claude-code-session-id ?? ""` | `sessionID` | ✅ |
| 11/12 | expert / skill-names，缺则空串 | 恒空串（本包不发） | ✅ |

**"无推理令牌就不发 proof 头"** 我们结构上已满足：`chatMessages` 先 `ensureInferenceToken`，
拿不到就提前返回，`buildProof` 永远不会收到空 token。

### 10.3 ★ 403 只在**一个错误码**下重试（这条最能说明"封号不能自愈"）

```js
async function md3(q){                            // 403 重试门
  ...
  return JSON.parse(body)?.error?.code === "request_not_authorized"
}
// 桥里：
for (let G = 0; G < xd3 /* =2 */ && P.status === 403 && ...; G++) {
  if (!await md3(P)) break;                       // ← 不是这个码就立刻放弃
  ... 强制刷 token ...
  P = await j();                                  // 重试
}
```

⇒ **`disabled_account` 官方一次都不重试** —— 与我们把它当终态**一致**。

这两个码在本仓早已分开（`internal/phanthywork/errors.go`）：
`request_not_authorized` → 可自愈档（换号但不罚号）；`disabled_account` → 终态。
守它们的判据：`TestRequestNotAuthorizedStaysRecoverable`。

### 10.4 仍然存在的**有意差异**（不是缺陷，记档以免将来误判）

| # | 官方桥 | 本仓 | 为什么这样 |
|---|---|---|---|
| 1 | 403 `request_not_authorized` **就地重试 2 次 + 强制刷 token**（20s 预算 / 8s 等待 / 60s 冷却） | 交给**网关核心**处理（换账号） | 两边恢复意图相同；核心有账号池，换号比原地重刷更可能成功。且原地重刷对"签名/时钟"类根因无效 |
| 2 | 入站若带 `x-phanthy-request`，把它写成 `x-request-id`（canonical 第 9 段随之） | 恒生成新 uuid | 我们不发 `x-phanthy-request`，没有可沿用的值；只影响链路追踪关联 |
| 3 | 转发前删掉入站的 4 个 proof 头再重算 | 恒重算（我们本来就是自己算） | 等价 |
| 4 | 响应侧删 `content-encoding/content-length/transfer-encoding/connection` | 交给 Go 的 `http.Client` 自动解压/去分块 | 等价 |

### 10.5 这一节的来源与可信度

上述四个函数来自官方 sidecar 二进制，`sK6` 的定义、`md3` 的门判、入口的 `Sd3` 集合都是**逐字**。
它们最初是从一个本地会话记录里定位到的（该会话在读完这段源码后被上游内容过滤器中断），
本仓随后逐条与自己的实现核过，**没有发现新的缺陷**。
## 十一、★★ 两跳链路：`/v1/chat/completions` **也要签名**（2026-11-14 最重要的更正）

### 11.1 错误的来源：只抓到链路的一跳

官方客户端发推理请求是**两跳**，不是一跳：

```
内核（Phanthy Codeo）  ──①──▶  本地桥（claude-sidecar, 127.0.0.1）  ──②──▶  上游 code.phanthy.com
                        不签名                                    补 12 段签名 + 4 个证明头
```

本仓早先的结论（大量注释都这么写）是「chat 路径全程零签名，签名相关的一个都不要加」——
那是因为**只抓到了 ①**。而真正决定上游怎么判的是 **②**。

### 11.2 真源：桥的路径集合里**明确包含 chat**

官方 sidecar（`claude-sidecar.exe` 字符串转储）里，本地推理桥 `gd3` 逐字：

```js
Sd3 = new Set(["/v1/messages", "/v1/responses", "/v1/chat/completions"])

// 转发前：先删掉入站的 4 个证明头，再重算一份覆盖上去
for (let x of ["host","connection","content-length","transfer-encoding",
               "x-phanthy-desktop-session","x-phanthy-proof-signature",
               "x-phanthy-proof-nonce","x-phanthy-proof-timestamp"]) G.delete(x);
let u = _O8(q.method, K.pathname, $, G);
if (u) for (let [x, m] of Object.entries(u)) G.set(x, m);
```

而 `_O8` 的拼装（§10.2）与本仓 `InferenceCanonical` **逐段一致** ——
也就是说 **12 段签名算法早就写对了，缺的只是「把它用在 chat 路径上」**。

### 11.3 旁证：上游就是按"缺这个头"报错的

本仓假上游里早就写着上游的原文：

```
403 {"code":"request_not_authorized","message":"missing desktop session"}
```

`x-phanthy-desktop-session` 正是 4 个证明头之一。

### 11.4 两条**在守护错误行为**的判据（已反转）

| 原名 | 原断言 | 现在 |
|---|---|---|
| `TestChatSendsNoSignatureHeaders` | chat 不带任何签名头 | → `TestChatPathCarriesDesktopInferenceProof`：**必须带齐 4 个**，且 `x-phanthy-desktop-session` == 推理令牌原值；同时仍**不许**带 v1 的 `x-desktop-*`（那 4 个属于 `/api/*` 一族，两套头不能混） |
| `TestChatDoesNotFetchInferenceToken` | chat 不调 `activities/summary` | → `TestChatFetchesInferenceToken`：**必须调**（12 段第 8 段是令牌哈希、第 4 个头是令牌原值）；且 summary 那条**不带**幂等键 |

⚠ 这是本项目第三次出现「判据锁死错误行为」：前两次是幽灵头与本仓那条从未写出来的判据，
这一次是**两条判据联手把"少 4 个头"锁成了"正确"**。

### 11.5 实现改动

- `ChatCompletionsStream(ctx, a, path, body, proof proofValues)` —— 新增 `proof` 参数，
  在末尾挂 4 个证明头；`x-request-id` / `x-claude-code-session-id` 改取 `proof` 的值
  （它们同时是 canonical 第 9/10 段，**签名与发头必须同源**）。
- `chatCompletions` —— 先 `ensureInferenceToken`（180s TTL + 缓存 + 串行闸），
  再 `buildProof`，再发。与 `chatMessages` 同一条链路，差异只剩 body 与响应格式。
- `chatFakeUpstream` —— 补 `/api/oauth/profile`、`/api/oauth/desktop-installations/register`、
  `/api/oauth/activities/summary` 三个应答（换令牌的前置是 `ensureInstallation`）。

### 11.6 ⚠ 这条改动**没有**真机验证

当前 phanthywork 账号已封（403 `disabled_account`），所以本轮只能验证到
「对官方源码 + 对假上游」这一层。**拿到新号后的第一件事**是看这条路径是否 200 ——
如果仍然 403 `request_not_authorized`，下一个要查的是 `buildProof` 的第 4 段时钟口径
（`serverNowMS`）与第 7 段 `Authorization` 是否与实发头逐字节一致。

### 11.7 出处

这一结论最初由一个本地 agent 会话独立得出（它在写完这段后被上游内容过滤器中断，
改动只落了一半）。本仓随后独立复核了 `gd3` / `_O8` / `Sd3` 三处逐字源码，
并补齐实现与判据。会话记录可从 WebView2 HTTP 缓存恢复（见 §12）。
## 十二、附：怎么从本地磁盘恢复官方客户端的会话记录

（这一节与协议正文无关，但它记录了一条**可复用的取证手段**：想看清"官方客户端到底做了什么"，
不必抓包 —— 它会把自己的会话历史落在本地。）

### 12.1 位置（按信息量排序）

| 位置 | 里面有什么 | 能读出吗 |
|---|---|---|
| `%LOCALAPPDATA%\com.phanthy.work.desktop\EBWebView\Default\Cache\Cache_Data\f_*` | **★ 对话全文**：`{"messages":[{id,timestamp,type,content,...}]}`，明文 JSON，未压缩 | ✅ 直接 `ConvertFrom-Json` |
| `...\EBWebView\Default\IndexedDB\http_tauri.localhost_0.indexeddb.leveldb\000004.log` | V8 序列化的**结构化对象**（字段名与值之间夹二进制标签）：运行时问题记录、消息投递状态键、用户输入文本 | ⚠ 只能按"串段"还原，拿不到完整 JSON |
| `%USERPROFILE%\.phanthy-code\desktop\diagnostics\*.jsonl` / `runtime-errors.log` | 控制台告警、`client_api_request_failed`（如 `/api/token-factory/activate` 409 `lead_not_linked`）、`CLIENT_ERROR` | ✅ 明文 |
| `%USERPROFILE%\.phanthy-code\desktop\spaces.json` / `experts\session-agents.json` | 会话分组、sessionId→agentId 绑定 | ✅ 明文，但**没有标题** |
| `...\desktop\message-delivery-*.sqlite(-wal)` | 只有投递状态，**没有正文** | — |

**会话标题不在本地**（服务端下发）；本地只有正文字段。

### 12.2 关键操作要点

1. **文件被运行中的 app 锁着** → 用 `FileShare.ReadWrite` 共享读，不要 `ReadAllBytes`：
   ```powershell
   $fs = [System.IO.File]::Open($p, 'Open', 'Read', 'ReadWrite')
   ```
2. **编码是混的**：ASCII 字段按单字节存、含中文的字段按 **UTF-16LE** 存。
   按 UTF-8 搜中文或按 UTF-16 搜英文都会 0 命中 —— 这是最容易误判成"本地没有"的原因。
3. **`assistant_text` / `transcriptMessageId` 这些名字在缓存里是 0 命中**：
   缓存里用的是 `"type":"assistant"` / `"type":"tool_use"` / `"type":"tool_result"`。
   用 `"id":"codeo:ses_` 或 `tool_result` 当锚点更可靠。
4. 缓存文件可能是**分页的**（同一次会话被切成多个 `f_*`）→ 按 `messages[].id` 去重、
   按 `timestamp` 排序，就能拼回完整时间线。