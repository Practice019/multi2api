# `phanthy-code-launcher`（`pcl`）完整规格

> 日期：2026-11-15
> 真源：npm `phanthy-code-launcher@0.4.3`，`dist/cli.js`（42.4 KB，448 行还原后）
> 探测：`https://code.phanthy.com` **无鉴权**公开探测（不涉及任何账号）
> 校验：tarball shasum `db868d2795874555cad5056b173a16bc430fc724` = registry 声明

---

## 0. ⚠ 先分清两样东西（本文只讲后者）

之前有一份报告把下面两样混在一起讲，是错的。它们是**两个完全不同的东西**，
只是**打到同一个上游**（`code.phanthy.com`）：

| | **Windows 客户端** | **本文主题：CLI** |
|---|---|---|
| 名称 | `PhanthyWork.exe` | `phanthy-code-launcher`（命令 `pcl`） |
| 形态 | Tauri 桌面壳 + `claude-sidecar.exe`（Bun 单文件，108 MB）+ `phanthy-codeo.exe` | **Node.js CLI**，`dist/cli.js` 42 KB |
| 来源 | **官方** | **第三方社区**（作者 `spalxyz13`，MIT） |
| 安装 | 安装包 | `npm install -g phanthy-code-launcher` |
| 凭据 | `<configDir>/.credentials.json`（`claudeAiOauth`） | `~/.phanthy-code-launcher/credentials.json` |
| 签名 | **有**（ed25519，`_O8` 12 段 canonical，4 个 `x-phanthy-proof-*`） | **完全没有** |
| 设备注册 | 有（`/api/oauth/desktop-installations/register`） | **完全没有** |
| 推理令牌 | 有（180 秒两跳） | **完全没有** |
| 本地服务 | 主服务 `:4413` + 推理桥 `127.0.0.1:4421` | 一个**纯转发代理**（随机端口） |
| 面向 | 自家 GUI + 内核 | Claude Code / Codex / OpenCode / phanthy-codeo |

**⇒ 判断"反代走哪条路更容易"时，必须先说清在问哪一个。**

本文只讲 CLI。Windows 客户端见
`phanthywork-official-client-outbound-spec.md`。

---

## 1. 包与溯源（可信度划界）

| 项 | 值 |
|---|---|
| 包名 / 命令 | `phanthy-code-launcher` / `pcl` |
| 版本 | 0.4.3（共 0.4.1 / 0.4.2 / 0.4.3） |
| 作者 / 维护者 | `spalxyz13`；仓库 `github.com/yizhixu/phanthy-code-launcher` |
| 许可 | MIT |
| 依赖 | `undici`（HTTP）、`proper-lockfile`（文件锁）、`@inquirer/select`（菜单）；**无原生模块** |
| Node | `>=23.5.0 \|\| ^22.13.0 \|\| ^20.17.0` |
| 发布 | 2026-11-06 创建，2026-11-12 更新 |
| 自述局限 | README：「真实 agent 的调用验证**主要在 macOS 上完成**，Windows 和 Linux 的具体组合仍需测试」 |

**⇒ 它是「证据」，不是「规格」。** 它证明"某条路存在"，不证明"那条路对我们成立"。

---

## 2. 命令面（`In()` 的分派表）

```
pcl login          登录（PKCE）
pcl logout         删凭据 + account-identity + models 缓存
pcl status         是否已登录 + 四个 agent 是否在 PATH
pcl account        拉 profile / usage / rewards，写 account-identity.json
pcl models         拉模型目录，写 models.json
pcl update         自更新（npm i -g phanthy-code-launcher@latest）
pcl telemetry      只支持 status（上报是固定开启的）
pcl [claude|codex|opencode|phanthy-codeo] [原生参数…]
pcl                交互菜单
```

---

## 3. 凭据

### 3.1 怎么拿到（`ne()`，两条路径）

**公共部分**（PKCE）：

```js
verifier  = randomBytes(32).toString("base64url")
challenge = sha256(verifier).digest("base64url")
state     = randomBytes(16).toString("hex")
```

`GET ${BASE}/oauth/authorize` 的查询参数（逐字）：

```
code=true
client_id=phanthy-code-cli
response_type=code
redirect_uri=<见下>
scope=org:create_api_key user:profile user:inference user:sessions:claude_code
      user:mcp_servers user:file_upload
code_challenge=<challenge>
code_challenge_method=S256
state=<state>
```

**路径 A — 有桌面（Windows/macOS/带 DISPLAY 的 Linux）**

```js
本地起 http server，listen(0, "127.0.0.1")
redirect_uri = `http://127.0.0.1:${port}/oauth/callback`
校验：pathname === "/oauth/callback" && state 相符 && 有 code
120 秒超时（Login timed out）
```

**路径 B — 无桌面（Linux 且无 DISPLAY/WAYLAND_DISPLAY）**

```js
redirect_uri = `${BASE}/oauth/code/success`      // 官方托管的成功页
让用户把结果粘回终端；
可接受两种输入：① 完整 URL（取 code + state）  ② `code#state`
```

**换 token**（两条路径都一样，**form-urlencoded**）：

```
POST ${BASE}/oauth/token
Content-Type: application/x-www-form-urlencoded
grant_type=authorization_code & code=<code> & redirect_uri=<同上>
& client_id=phanthy-code-cli & code_verifier=<verifier> & state=<state>
```

⚠ 注意：换码这一跳**体里带 `state`**（一般 OAuth 不需要，多带了）。

### 3.2 存哪、长什么样

`~/.phanthy-code-launcher/credentials.json`（可用 `PHANTHY_LAUNCHER_HOME` 改目录）

```json
{
  "modelCacheScope": "<随机 24 字节 hex>",
  "accessToken":  "<JWT>",
  "refreshToken": "<64 char base64url> | null",
  "expiresAt":    <毫秒时间戳>
}
```

落盘方式（`Re()`）：**临时文件 → chmod 0600 → rename**，原子替换：

```js
let t = `${te()}.${randomBytes(8).hex}.tmp`
await writeFile(t, JSON.stringify({...}), {mode: 0o600})
await chmod(t, 0o600); await rename(t, te())
```

### 3.3 同目录下另外三个文件

| 文件 | 内容 | 用途 |
|---|---|---|
| `account-identity.json` | `{scope, profile:{account:{uuid,email,display_name}, organization:{uuid,name}}}` | 避免重复拉 profile；`scope` 不符即失效 |
| `models.json` | `{scope, catalog:<服务端模型目录>}` | 模型列表缓存，换号即失效 |
| `telemetry-status.json` | `{last_attempt,last_status,last_success}` | 上报状态（`pcl telemetry status` 读它） |
| `telemetry/<sha256(account_uuid)>.json` | 事件队列（最多 2000 条） | 待上报事件 |

### 3.4 账号指纹 = `B()`

```js
function B(e) { let n = e?.refreshToken || e?.accessToken; return e?.modelCacheScope || (n ? sha256(n).hex : null) }
```

**所有缓存（models / account-identity）都挂在 `scope` 上** ⇒ 换号自动失效。
续期时若发现文件里的 `scope` 变了，抛 `login_changed`（401），要求重启 agent。

---

## 4. 续期

### 4.1 触发条件（与官方/我们都不一样）

```js
// Zt()：每轮取 access token 时
e.expiresAt > Date.now() + 60_000     → 直接用缓存
否则                                   → 续期
```

**⇒ 提前量只有 60 秒**（官方桥是 ≤5 秒或 401；我们是 900 秒）。
而且**没有后台定时任务** —— 只在"要发请求了"时懒续期。

### 4.2 请求形态（`me()`）

```js
POST ${BASE}/oauth/token
headers: {
  ...M(),                          // 3 个头（见 §5.1）
  ...await Y(),                    // x-phanthy-cli-device
  "content-type": "application/json"      // 续期用 JSON
}
body: {"grant_type":"refresh_token","refresh_token":"<rt>",
       "client_id":"phanthy-code-cli","scope":"<de 那 6 个>"}
timeout: 15 秒
```

⚠ **续期时带了 `scope`**（官方 `Pd3` 不带），而且**带了 `M()` 与设备指纹**（官方只带 2 个头）。

### 4.3 落盘与轮换

```js
await Re({
  modelCacheScope: B(old),
  accessToken:  r.access_token,
  refreshToken: r.refresh_token || old.refreshToken,   // ← 轮换：新值优先，没给就沿用
  expiresAt:    Date.now() + r.expires_in * 1000
})
```

### 4.4 并发保护（**与我们遇到的是同一个问题**）

`Q(fn)` 用 `proper-lockfile` 锁住 `credentials.json`：

```js
lock(dir/credentials.json, {
  stale: 30_000, update: 5_000,
  retries: { retries: 150, minTimeout: 100, maxTimeout: 250, factor: 1.2, maxRetryTime: 35_000 },
  onCompromised: () => { o = refresh_lock_lost(503); abort() }
})
// 拿不到锁：ELOCKED → refresh_lock_timeout(503)
```

另有**进程内去重**：`C()` 用一个模块级 promise 记忆化 —— 并发调用共享同一次续期。

⇒ **两个独立实现都做了"续期串行化"**（我们用 `refreshGate`，PCL 用文件锁 + promise 记忆化）。
这条互相印证：**refresh_token 轮换的竞态问题是真实存在的，不是我们想多了。**

### 4.5 错误分类（`me()` + `De()`）

| 上游 | 结果 |
|---|---|
| `status 401` 或 `error ∈ {invalid_grant, invalid_token}` | `F()` = `login_required`（**终态，要求重新登录**） |
| 其它非 2xx | `auth_server_rejected`（403→403，429/5xx→503，其它→502） |
| 无 `access_token` / `expires_in` 非法 | `auth_invalid_response`（502） |
| 超时 / AbortError | `refresh_timeout`（504） |
| 网络错误 | `refresh_network_error`（503） |

**⇒ 它把 `invalid_grant` 一律当"要重新登录"，没有"退避/稍后重试"这一档。**
这一点比我们粗（我们是"被拒 ≠ 终态"）。

---

## 5. 发请求

### 5.1 三层头（**这一节是最容易看错的**）

**① 基础头 `M()` —— 每一条请求都带**

```js
function M() {
  return {
    "User-Agent": `phanthy-code-launcher/${L}`,           // L = 包版本，如 0.4.3
    "X-Phanthy-Client-Source": "phanthy-code-launcher",   // ★ 官方从不用这个值
    "X-Phanthy-Product": "phanthy_code"                   // ★ 官方根本不发
  }
}
```

**② 设备指纹 `Y()` —— 缓存 5 分钟**

```js
{ version:1, collectedAt, cliVersion, hostname, username, platform, arch, osRelease,
  ipCandidates, networkInterfaces, routes, defaultGateway }
→ JSON → base64url，超过 6000 字符就依次 pop routes / networkInterfaces / ipCandidates
→ {"x-phanthy-cli-device": <b64url>}
```

⚠ 它为了拿 `routes` / `defaultGateway` 会**执行系统命令**：
`win32: route print -4` / `darwin: route -n get default` / `linux: ip route`（timeout 500ms）。

**③ 遥测上下文头（`ct()` 的 `headers()`）**

```js
{ "x-phanthy-project": <sha256(deviceId + "\0" + cwd) 前 64 hex>,
  "x-phanthy-session": <每次进程随机 uuid>,
  "x-phanthy-request": <每个请求一个 id>,
  "x-phanthy-client":  <harness 名 | "launcher"> }
```

### 5.2 ★ PCL **不伪装成官方客户端**

由 §5.1① 可见：它主动把自己标识成 `phanthy-code-launcher`，
并且发了两个官方**从不发送**的头（`X-Phanthy-Product`、`X-Phanthy-Client-Source: phanthy-code-launcher`）。

**这一点非常重要**：它说明服务端**不要求"长得像官方客户端"**才能通过（至少对它能跑通的那条路）。
本仓 `client.go` 里"多一个官方没有的头 = 异常客户端"的假设，在**请求能否被接受**这个层面上
被这个第三方实现削弱了（但**风控层面仍未证伪** —— 见 §11）。

### 5.3 API 调用（`oe()`）

```js
async function oe(path, {timeoutMs = 15_000} = {}) {
  let t = await C();                                  // 取/续期 access token
  let o = await fetch(`${BASE}${path}`, {
    dispatcher: Ie,                                   // undici Agent：connections 2, pipelining 1,
                                                      //   connect timeout 7s, keepAlive 30s
    signal: AbortSignal.timeout(timeoutMs),
    headers: { ...M(), ...await Y(), Authorization: `Bearer ${t}` }
  })
  if (!o.ok) throw Object.assign(Error(`Phanthy request failed (${o.status})`), {status: o.status})
  return o.json()
}
```

**⇒ 这一族（`/api/*`）的头 = `M()` 3 个 + 设备指纹 1 个 + `Authorization`。**
没有 `x-phanthy-project` / `-session` / `-request` / `-client`（那是遥测专用）。

### 5.4 重试（`fe()`）

```js
async function fe(call, path) {
  try { return await call(path, {timeoutMs: 12_000}) }
  catch (t) {
    let o = classify(t);
    if (["timeout","network"].includes(o.kind) || o.status === 408 || o.status >= 500)
      return call(path, {timeoutMs: 8_000});     // 只重试一次，且用更短的超时
    throw t
  }
}
```

只用于 `pcl account` 的三个探测端点（profile / usage / rewards）。

### 5.5 推理转发代理（`et()`）

```js
secret = randomBytes(32).hex            // 代理自己的口令
代理鉴权：agent 必须带 Bearer <secret> 或 x-api-key: <secret>（timingSafeEqual）
转发逻辑：
  E = new Headers
  // ① agent 的头原样透传，但去掉 host/authorization/x-api-key/connection/content-length
  // ② 叠加 M() + 设备指纹 + 遥测上下文头
  E.set("authorization", `Bearer ${realToken}`)
  fetch((baseURL || BASE) + req.url, { method, headers: E, body: stream, duplex:"half", redirect:"manual" })
上游 401/403 → 抛错并**明确不重试**（回给 agent `x-should-retry: false`）
响应头回传时剔除 content-encoding/length/transfer-encoding/connection
透传 `x-phanthy-cost-points` 给 agent
```

**推理路径白名单（仅用于遥测判定，不影响转发）：**

```
/^\/v1\/(messages|responses|chat\/completions)(?:\?|$)/
```

⚠ 这**只是遥测的白名单**，代理本身是**全路径转发**的。

### 5.6 注入给 agent 的环境

| Agent | 注入 |
|---|---|
| claude | `ANTHROPIC_BASE_URL=<代理>`、`ANTHROPIC_AUTH_TOKEN=<secret>`、`ANTHROPIC_API_KEY=""`、`CLAUDE_CODE_USE_{BEDROCK,VERTEX,FOUNDRY}=0`、`CLAUDE_CODE_MAX_CONTEXT_TOKENS`、`CLAUDE_CODE_DISABLE_1M_CONTEXT=0`；args `["--model", <默认模型>]`；settings 里替换模型选择器 |
| codex | `PHANTHY_LAUNCHER_TOKEN=<secret>`；`-c model_provider=phanthy_launcher`、`-c model_providers.phanthy_launcher.base_url=<代理>/v1`、`.name=Phanthy`、`.env_key=PHANTHY_LAUNCHER_TOKEN`、`.wire_api=responses`、`-m <模型>`；另给一份 `catalog` |
| opencode / phanthy-codeo | `PHANTHY_CODE_OAUTH_TOKEN` + `PHANTHY_CODEO_CONFIG_CONTENT`（写 `phanthy_launcher_<anthropic\|openai\|compatible>` 三个 provider，`baseURL=<代理>/v1`、`apiKey=<secret>`） |

---

## 6. 端点清单

### 6.1 源码里出现的（带用途）

| 方法 | 路径 | 用途 | 头 |
|---|---|---|---|
| GET | `/oauth/authorize` | 登录跳转 | 无 |
| POST | `/oauth/token` | 换码（form）／续期（JSON） | `M()` + 设备 |
| GET | `/oauth/code/success` | 无桌面时的 `redirect_uri` | 无 |
| GET | `/api/oauth/profile` | 账号信息 | `M()` + 设备 + Bearer |
| GET | `/api/oauth/usage` | 用量 | 同上 |
| GET | `/api/phanthy_code/models` | 模型目录 | 同上 |
| GET | `/api/work/rewards?limit=5` | 奖励/余额 | 同上 |
| POST | `/api/event_logging/batch` | 遥测 | `M()` + 设备 + Bearer + `x-service-name` |
| 任意 | `${BASE}${req.url}` | 代理转发（含 `/v1/*`） | agent 头 + `M()` + 设备 + 遥测 + Bearer |

### 6.2 ★ 实测（**无鉴权**，不涉及任何账号）

对 `https://code.phanthy.com` 直连（不过代理）：

| 请求 | 状态 | 响应 |
|---|---|---|
| `GET /api/oauth/profile` | **401** | `missing_login` "Bearer access token is required." |
| `GET /api/oauth/usage` | **401** | `missing_login` |
| `GET /api/phanthy_code/models` | **401** | `missing_login` |
| `GET /api/work/rewards?limit=5` | **401** | **`authentication_required`**（错误码不同） |
| `POST /api/oauth/activities/summary` | **401** | `missing_login` |
| `POST /api/oauth/desktop-installations/register` | **401** | `missing_login` |
| `POST /api/oauth/activities/daily-login/claim` | **401** | `missing_login` |
| **`POST /api/event_logging/batch`** | **404** | **HTML**（`code.phanthy.com` 上**不存在**） |
| `POST /oauth/token` `{}` | **400** | `unsupported_grant_type`「Only authorization_code and refresh_token are supported.」 |
| `GET /oauth/authorize` 裸调 | **400** | `unsupported_response_type`「Only authorization code flow is supported.」 |
| `GET /oauth/code/success` | **200** | HTML 成功页 |
| `POST /v1/messages` | **401** | `missing_login` |
| `POST /v1/chat/completions` | **401** | `missing_login` |
| `POST /v1/responses` | **401** | `missing_login` |
| `GET /api/__does_not_exist__`（对照） | **404** | HTML ⇒ 对照有效，上面的 401 都是"端点存在但缺鉴权" |

**⇒ 两个重要结论：**

1. **`/api/event_logging/batch` 在 `code.phanthy.com` 上是 404。**
   PCL 把遥测发到这里 ⇒ 它的遥测**应该是静默失败的**（它对 404 有 5 分钟退避）。
   本仓 `provider.go:184` 记的抓包实证里它出现过，说明**官方是发到别的主机**。
2. `/api/work/*` 与 `/api/oauth/*` 的**错误码不同**（`authentication_required` vs `missing_login`）
   ⇒ 两族确实走不同的鉴权层。这**印证**了官方客户端给这两族用两套请求头的设计。

### 6.3 `Bearer` 语义（**实测**）

| 请求 | 状态 | 响应 |
|---|---|---|
| `POST /v1/chat/completions` + `Bearer x` | **401** | `expired_access_token`「Login session has expired. Run /login to sign in again.」 |
| `POST /v1/messages` + `Bearer x` | **401** | 同上 |
| `GET /api/oauth/usage` + `Bearer x` | **401** | 同上 |
| `GET /api/work/rewards` + `Bearer x` | **401** | 同上（过了第一层之后） |
| `POST /oauth/token` `refresh_token=garbage` | **400** | **`invalid_grant`**「Refresh token is invalid or expired.」 |
| `POST /oauth/token` `authorization_code=garbage`（JSON） | **400** | `invalid_client`「Invalid client or redirect URI.」 |
| 同上（**form-urlencoded**） | **400** | `invalid_client` —— **两种编码结果一致 ⇒ 服务端都接受** |
| `GET /oauth/authorize` + 合法参数 | **307** | → `/login?next=<原 URL>` |

**⇒ 极其重要的一条：**

> **任何无效 Bearer（包括随便一个 `Bearer x`）都返回 `expired_access_token`。**
> 服务端**不区分**"垃圾 token"和"过期 token"。

**这条对我们意义很大**：我们之前看到死号返回 `expired_access_token`，
据此判断"会话过期而不是被封"。但**垃圾 token 也是这个码** ⇒
**`expired_access_token` 只能证明"这个 token 现在不被接受"，不能证明"账号还活着"。**
（不过 `disabled_account` 是另一个独立码，那一个仍然是被封的强证据。）

### 6.4 网关指纹（响应头）

```
Server: TencentEdgeOne
Via: kong/2.5.1
www-authenticate: Bearer realm="phanthy-code", error="invalid_request",
                  error_description="Bearer access token is required."
X-Kong-Upstream-Latency / X-Kong-Proxy-Latency
EO-LOG-UUID / EO-Cache-Status
```

⇒ **Kong 2.5.1 做网关，TencentEdgeOne 做 CDN。** 鉴权在 Kong 层（`www-authenticate` 表明它自己就是资源服务器）。

---

## 7. 凭据能做什么（能力矩阵）

一份 `credentials.json`（= 一个账号）能做到：

| 能力 | 端点 | 是否需签名 |
|---|---|---|
| 读账号信息 | `GET /api/oauth/profile` | 否 |
| 读用量 | `GET /api/oauth/usage` | 否 |
| 读模型目录 | `GET /api/phanthy_code/models` | 否 |
| 读奖励/余额 | `GET /api/work/rewards?limit=5` | 否 |
| 续期自己 | `POST /oauth/token`（refresh_token） | 否 |
| **推理** | `POST /v1/{messages,responses,chat/completions}` | **否**（PCL 实测——按它的设计） |
| 上报遥测 | `POST /api/event_logging/batch` | 否（但 404，主机不对） |
| ~~设备注册~~ | ~~`/api/oauth/desktop-installations/register`~~ | **PCL 从不调用** |
| ~~签到领积分~~ | ~~`/api/oauth/activities/daily-login/claim`~~ | **PCL 从不调用** |
| ~~额度摘要~~ | ~~`/api/oauth/activities/summary`~~ | **PCL 从不调用** |

**⇒ 关键结论：PCL 只用了"读"和"推理 + 续期"。**
它**从不**注册设备、从不签到、从不换推理令牌、从不发 proof 签名。

而本仓 `internal/phanthywork` 把 `register` / `summary` / `claim` / `referral` 全都实现了。
**如果 PCL 这条路成立，那么这些实现对我们来说可能整体是多余的**
（除非我们要做签到/邀请这些功能）。

---

## 8. 三方对照

| 维度 | 官方 sidecar | **PCL** | 我们 |
|---|---|---|---|
| 形态 | Tauri + Bun 单文件 | Node CLI | Go 网关 |
| 登录 | 客户端内建 | PKCE + loopback | PKCE + loopback |
| 登录重定向 | — | `127.0.0.1:<port>/oauth/callback` 或 `${BASE}/oauth/code/success` | 同左 |
| 换码编码 | — | **form-urlencoded**（带 `state`） | 需核对 |
| 续期编码 | JSON | **JSON** | JSON ✅ |
| 续期提前量 | ≤5 秒 或 401 | **60 秒** | 900 秒 |
| 续期是否带设备头 | 否（只 2 个头） | **是** | 否（照官方） |
| 续期是否带 scope | 否 | **是** | 是 |
| 后台定时续期 | 无 | **无** | 有（10 分钟扫） |
| 续期并发保护 | 内存 + 共享库 | **文件锁 + promise 记忆化** | `refreshGate` |
| `invalid_grant` 语义 | 被拒（可重读共享库） | **终态，要重新登录** | 被拒（不计数） |
| 设备注册 | **有** | **无** | 有 |
| 推理令牌 | **有（180s）** | **无** | 有 |
| proof 签名 | **有（ed25519/12 段）** | **无** | 有 |
| 自我标识 | `desktop` / `cli` | **`phanthy-code-launcher`** | `desktop`（照官方） |
| 多发官方没有的头 | — | `X-Phanthy-Product` 等 | **不发**（有负断言） |
| 签到 | 客户端内 | **无** | 自动（**已完成移除**） |

---

## 9. 实测记录（含我的两次工具失真，必须留档）

### 9.1 失真一：全部误报 200

第一版探测脚本用 `curl -D -` 抓头、再用正则 `HTTP/[0-9.]+ (\d{3})` 取码。
但**经本地代理时，第一条状态行是代理的 `HTTP/1.1 200 Connection established`**，
正则匹配到它 ⇒ **13 个端点全部误报 200**。

**修法**：用 `-w "%{http_code}"`（权威），并直连不过代理。

### 9.2 失真二：把已知写成未知

初稿把 `/api/oauth/usage` 与 `/api/event_logging/batch` 标成"本仓不知道"。
回仓库一查：`quota.go:20` 有对 usage 的**否决性决策**；`provider.go:184` 有 event_logging 的**抓包实证**。

**教训**：**称某样东西为"新发现"之前，先在自己仓库里搜一遍。**

### 9.3 失真三：把头集合说错

初稿说 PCL 的推理请求带"4 个 `x-phanthy-*`"。
还原源码后发现**基础头是另外 3 个**（`M()`），那 4 个是**遥测专用**。
`x-phanthy-cli-device` 才是每条请求都带的。

**⇒ 这一条改变了判断**：PCL 不是"伪装成官方"，而是**主动自报第三方**（§5.2）。

---

## 10. 这份文档**不能**回答的问题

| # | 问题 | 为什么不能 |
|---|---|---|
| 1 | **PCL 这条路会不会被封号** | 没有任何数据。它能跑通只证明"请求被接受" |
| 2 | `/v1/chat/completions` 无签名是否真能用 | PCL 的 README 只承诺 Claude Code / Codex / OpenCode / phanthy-codeo，**没承诺 OpenAI 兼容**；它的正则覆盖更像是通用白名单 |
| 3 | Windows 上是否真跑通 | 它自己说"主要在 macOS 验证" |
| 4 | 我们的账号为什么被封 | 与本文无关，仍然没有服务端视野 |
| 5 | `/api/event_logging/batch` 的真实主机 | 只知道 `code.phanthy.com` 上是 404 |

---

## 11. 建议的验证清单（**必须用可牺牲的号**）

| # | 实验 | 判据 | 成本 |
|---|---|---|---|
| V1 | 无签名 `POST /v1/messages`（Bearer + `M()` 那 3 个头 + 设备指纹） | 200 ⇒ 签名非必需 | 一次最小推理 |
| V2 | 无签名 `POST /v1/chat/completions`，同上 | 200 ⇒ **我们整套 proof 可以删** | 一次最小推理 |
| V3 | 同上但**只带 Bearer**（一个额外头都不加） | 200 ⇒ 头也非必需 | 一次最小推理 |
| V4 | 续期：只带 `Content-Type`+`Accept` vs 带全套 | 两者都 200 ⇒ 服务端宽容 | 一次续期（轮换 token） |
| V5 | `GET /api/oauth/usage` 用真 token | 看它返回什么形状（**仅作观察，不作额度决策依据**，见 `quota.go:20`） | 一次只读 |

⚠ **不得用任何在用账号做上述实验。**
2026-11-14 已经违反过一次（见 `pw-bridge-architecture-plan-2026-11-14.md` 的 P-1）。
