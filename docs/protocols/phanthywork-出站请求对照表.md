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
| **rewards** | GET | `/api/oauth/rewards` | — | ❌ **缺** |
| **referral-relations** | GET | `/api/oauth/referrals/relations` | —（只在注释里） | ❌ **缺** |
| **onboarding/reconcile** | POST | `/api/oauth/activities/onboarding/reconcile` | — | ❌ **缺** |
| **onboarding/tasks/{id}/{start\|complete}** | POST | `/api/oauth/activities/onboarding/tasks/...` | — | ❌ **缺** |
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

真源：magpie-PhanthyCode `src/activity.mjs` 的文件头（从前端 bundle `App-Y9eSJNhB.js`
的 `Co()` 调用表静态提取）+ `src/referrals.mjs`。

| 操作 | 官方幂等键 | 本仓 | 判定 |
|---|---|---|---|
| 每日签到 claim | `daily:<installationId>:<t>` | `daily:<installationId>:<serverDate>` | ✅ 一致（我们用服务端日键，比本地算日期更权威） |
| 取邀请码 | `referral-code:<installationId>:<ms>` | `referral-code:<installationId>:<YYYY-MM-DD>` | ⚠️ **后缀形态不同**（见下） |
| 注册设备 | `register:<installationId>` | 同 | ✅ |
| `activities/summary` | **不发**（canonical 第 7 段空串） | 同 | ✅ |
| onboarding start | `onboarding:start:<id>:<task>` | — | ❌ 未实现 |
| onboarding complete | `onboarding:complete:<id>:<task>` | — | ❌ 未实现 |

**⚠ 取邀请码那条的形态冲突（留待新号复验，先不动）**：本仓注释记的是"报告 §1.1/§1.2 实测"
的日期形态，magpie 记的是毫秒形态并称"官方前端同形"。两者都被真机跑通过其中一条路径，
但**我们拿不到对照实验**（账号已封）。既然现实现**能用**，就不按推测去改 —— 记在这里等复验。

## 五、★ 一个会让签名必错的陷阱：canonical 的 path 段**不能带 query**

magpie `src/referrals.mjs` 的原文警告：

> ⚠ `request()` 的 v1 canonical 用的是 `new URL(url).pathname`。本模块的两个端点都**没有 query**，
> 所以签出来是对的；将来若要加 `referrals/relations?limit=…`，必须先修 `upstreamPath`，否则必然 401。

**URL 可以带 query，签名的 path 不能带。** 本仓当前所有 `applyV1` 调用点都传无 query 的常量
（现在是对的），但这个陷阱的代价是"加个 `?limit=` 就 401、而错误码与签名错无法区分"。

⇒ 已在 `signV1` 里加了**当场断言**（`strings.Contains(p.Path, "?")` 直接报错），
判据 `TestSignV1RejectsQueryInPath` + `TestV1CanonicalPathSegmentHasNoQuery`。
将来加 `rewards?type=daily_login` / `referrals/relations?limit=…` 时，会立刻被拦住而不是等真机 401。

## 六、还没做的端点：实现前必须先取证的部分

### 6.1 四个缺口

```
GET  /api/oauth/rewards                       ← 官方用它读「今日签到状态」：?type=daily_login
GET  /api/oauth/referrals/relations           ← 带 cursor（响应含 next_cursor）
POST /api/oauth/activities/onboarding/reconcile
POST /api/oauth/activities/onboarding/tasks/{id}/{start|complete}
```

另有 `POST /referrals/bind` —— 官方存在，但 magpie **刻意不实现、也不导出路径**，本仓同样不做。

### 6.2 已确定的（可以照做的）

- **onboarding 幂等键格式**：`onboarding:start:<id>:<task>` / `onboarding:complete:<id>:<task>`
- **任务 id 形状**：上游路径里的 `<id>` 必须匹配 `/^[a-z0-9_]{1,80}$/`（sidecar 的校验）
- **rewards 读取方式**：`GET /api/oauth/rewards?type=daily_login`，**v1 签名**（不是只读 Bearer）
- **钱包字段**：`wallet{availablePoints,totalPoints,balancePoints,reservedPoints}`

### 6.3 还不知道的（**不许猜，必须抓包**）

- `onboarding/reconcile` 的 **body 形状**（sidecar 只透传，body 由客户端定义；客户端代码不在这个二进制里）
- `onboarding/tasks/{id}/{start|complete}` 的 body
- `reconcile` 的语义（它靠什么判定"该做哪些任务"）
- rewards 响应里 `wallet` 与 `credits` 的关系（我们 summary 里读的是 `credits.available`）

抓包路径：在官方客户端（桌面端 / Phanthy Code CLI）里走一遍新手任务流程，
用 `D:\temp\ph-test` 里既有的 CDP 抓包手法（见 `step91-register-capture.mjs` 的做法）。

## 七、纪律（这份协议的核心）

1. **改出站请求之前，先对这张表**；表里没有的，先去官方二进制/源码找真源，**别照名字编**。
2. **判据必须反向验证**：故意造一个违规，确认它真的会红。没有反向验证过的判据不算数
   （本次两条故障判据都栽在这上面：一条锁死幽灵头、一条从没被写出来）。
3. **头/串的差异优先按字节核对**：空格 vs LF、`x-phanthy-client` vs `x-phanthy-client-source`
   这种差异渲染出来一模一样，只有按字节比对才拦得住。
4. **拿不准就不改**：两个来源冲突、又无法用真机分辨时，**保持现状并记档**，
   等有账号能复验再动。今天的封号就是"按推测改"的代价。

## 八、判据索引

| 判据 | 守什么 |
|---|---|
| `TestInferenceHeadersMatchOfficialClient` | 推理头**双向** 1:1（官方有的必须在、官方没有的必须不在） |
| `TestCanonicalSeparatorsArePerString` | 三条 canonical 分隔符逐字节 = LF |
| `TestCommonHeadersOnEveryRequest` | `/api/*` 公共头三项（UA = `phanthy-code-cli/2.1.0`） |
| `TestChatSendsNoSignatureHeaders` | chat 路径不带任何证明头 |
| `TestDefaultUAIsBun` | 默认 UA 仍是 `Bun/1.3.14`（推理路径那条） |
| `TestChatUsesChatCompletionsPath` | 推理走 `/v1/chat/completions` |
