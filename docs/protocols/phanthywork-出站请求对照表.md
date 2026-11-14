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

## 四、纪律（这三条是这份协议的核心）

1. **改出站请求之前，先对这张表**；表里没有的，先去官方二进制/源码找真源，**别照名字编**。
2. **判据必须反向验证**：故意造一个违规，确认它真的会红。没有反向验证过的判据不算数
   （本次两条故障判据都栽在这上面）。
3. **头/串的差异优先按字节核对**：空格 vs LF、`x-phanthy-client` vs `x-phanthy-client-source`
   这种差异渲染出来一模一样，只有按字节比对才拦得住。

## 五、判据索引

| 判据 | 守什么 |
|---|---|
| `TestInferenceHeadersMatchOfficialClient` | 推理头**双向** 1:1（官方有的必须在、官方没有的必须不在） |
| `TestCanonicalSeparatorsArePerString` | 三条 canonical 分隔符逐字节 = LF |
| `TestCommonHeadersOnEveryRequest` | `/api/*` 公共头三项（UA = `phanthy-code-cli/2.1.0`） |
| `TestChatSendsNoSignatureHeaders` | chat 路径不带任何证明头 |
| `TestDefaultUAIsBun` | 默认 UA 仍是 `Bun/1.3.14`（推理路径那条） |
| `TestChatUsesChatCompletionsPath` | 推理走 `/v1/chat/completions` |
