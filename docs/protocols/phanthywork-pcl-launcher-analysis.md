# 第三方 CLI `phanthy-code-launcher`（pcl）源码分析

> 日期：2026-11-15
> 目的：回答「通过这个 CLI 做反代是不是更容易」
> 真源：npm 包 `phanthy-code-launcher@0.4.3`（`dist/cli.js` 42.4 KB，全部实现）
> 校验：tarball shasum = `db868d2795874555cad5056b173a16bc430fc724`，**与 registry 声明一致**

---

## 0. 溯源与可信度（先读：它不是官方的）

| 项 | 值 |
|---|---|
| 包名 | `phanthy-code-launcher` |
| 版本 | 0.4.3（`dist-tags.latest`；共 3 个版本：0.4.1/0.4.2/0.4.3） |
| 命令 | `pcl` → `dist/cli.js` |
| **作者** | `spalxyz13` —— **第三方社区项目**，仓库 `github.com/yizhixu/phanthy-code-launcher` |
| 许可 | MIT |
| 依赖 | `undici` `proper-lockfile` `@inquirer/select`（无原生模块） |
| 体积 | 解包 71.1 KB；`cli.js` 42.4 KB（**10 行**，压缩但明文） |
| 创建/更新 | 2026-11-06 / 2026-11-12 |
| 它自己的免责 | README：「当前版本仍处于实验阶段；真实 agent 的调用验证**主要在 macOS 上完成**，Windows 和 Linux 的具体组合仍需测试。」 |

**⇒ 所以它的行为是「证据」，不是「规格」。** 它能跑通说明**存在一条可行路径**，
但不能证明那条路径在所有形态下都成立。凡本文引用它的结论，都按这个分寸使用。

---

## 1. 它是什么

> README 原文：「帮助你登录 Phanthy 后，使用本机已安装的 coding agent。无需手动创建和填写
> Phanthy API key，即可将模型请求接入 `code.phanthy.com`。」

工作方式（类比 OpenRouter Ori）：

```
pcl login                     ← 浏览器 OAuth，PKCE + 本地回调
pcl                           ← 菜单选 agent
pcl claude / codex / opencode / phanthy-codeo
        │
        ├─ 起一个本地 HTTP 代理（127.0.0.1:<随机端口>）
        ├─ 把 agent 的 ANTHROPIC_BASE_URL 指过去
        └─ 代理把请求**原样转发**给 code.phanthy.com，只换 Authorization
```

---

## 2. 架构（逐段源码，全部实测引用）

### 2.1 常量

```js
var $  = "https://code.phanthy.com",
    W  = "phanthy-code-cli",                                   // client_id
    de = "org:create_api_key user:profile user:inference " +
         "user:sessions:claude_code user:mcp_servers user:file_upload";   // scopes
pe = () => process.env.PHANTHY_LAUNCHER_HOME || join(homedir(), ".phanthy-code-launcher");
te = () => join(pe(), "credentials.json");
```

**`client_id` 与 scope 跟官方/我们一致** ✅

### 2.2 登录 = PKCE + loopback（与我们一模一样）

```js
i = createServer((u, p) => {
      let a = new URL(u.url, "http://127.0.0.1");
      if (a.pathname !== "/oauth/callback" || a.searchParams.get("state") !== t || !a.searchParams.get("code")) {
        p.writeHead(400).end("Invalid callback"); return
      }
      p.end("Login complete. You can close this window.");
      o(a.searchParams.get("code"))
    });
await new Promise((u, p) => i.once("error", p).listen(0, "127.0.0.1", u));
let s = `http://127.0.0.1:${i.address().port}/oauth/callback`,
    l = new URL(`${$}/oauth/authorize`);
for (let [u, p] of Object.entries({
  code: "true", client_id: W, response_type: "code", redirect_uri: s,
  scope: de, code_challenge: n, code_challenge_method: "S256", state: t
})) l.searchParams.set(u, p);
// 轮询/等待回调，120 秒超时；然后
me({ grant_type: "authorization_code", code, redirect_uri, client_id: W, code_verifier, ... })
```

⇒ 和我们完全同构（`code=true` / `S256` / `state` / `127.0.0.1` 回调）。

### 2.3 凭据存储

`~/.phanthy-code-launcher/credentials.json`（mode 0600，写临时文件 + rename 原子落盘）：

```js
await Wt(t, JSON.stringify({ modelCacheScope: randomBytes(24).toString("hex"), ...e }), { mode: 384 });
await Vt(t, 384); await Xt(t, te());      // write → chmod → rename
```

它还把 `modelCacheScope` 与 token 绑定（换号即换缓存作用域）。

### 2.4 本地代理（核心）

```js
async function et(e = {}) {
  let n = randomBytes(32).toString("hex"),        // ★ 代理自身的 secret
      r = e.token || C,                           // getAccessToken
      i = e.deviceHeaders || Y,                   // x-phanthy-cli-device

  async function u(req, res) {
    // ① 代理鉴权：agent 必须带 Bearer <secret> 或 x-api-key: <secret>（timingSafeEqual）
    let f = (req.headers.authorization || "").replace(/^Bearer\s+/i, "") || req.headers["x-api-key"] || "",
        y = Buffer.from(String(f)), g = Buffer.from(n);
    if (y.length !== g.length || !timingSafeEqual(y, g)) { res.writeHead(401).end(); return }

    // ② 只有这三条路径算"推理"（做遥测）
    let z = /^\/v1\/(messages|responses|chat\/completions)(?:\?|$)/.test(req.url)

    // ③ ★★ 构造发上游的头
    let k = await r(); if (!k) throw F();                    // 拿 access token
    let E = new Headers;
    for (let [O, J] of Object.entries(req.headers))
      J && !["host","authorization","x-api-key","connection","content-length"].includes(O)
        && E.set(O, Array.isArray(J) ? J.join(", ") : J);    // agent 的头原样透传
    for (let [O, J] of Object.entries({ ...M(), ...await i(), ...t?.headers(b) }))
      E.set(O, J);                                           // 4 个 x-phanthy-* + 设备指纹 + 遥测头
    E.set("authorization", `Bearer ${k}`);                    // ★ 只替换 Authorization

    // ④ 直接打上游
    let S = await o((e.baseURL || $) + req.url, {
      method: req.method, headers: E, body: vt, duplex: "half", redirect: "manual", signal: P.signal
    });

    // ⑤ 401/403 明确**不重试**
    if (d === 401 || d === 403) throw ...("PCL 未重放本次请求")
  }
  await new Promise((a, c) => p.once("error", c).listen(0, "127.0.0.1", a));
  return { url: `http://127.0.0.1:${p.address().port}`, secret: n, ... }
}
```

### 2.5 那 4 个头的真值

```js
headers: d => ({
  "x-phanthy-project": s.projectID,
  "x-phanthy-session": s.sessionID,
  "x-phanthy-request": d,                 // 每次请求一个 id
  "x-phanthy-client":  s.harness || "launcher"
})
```

⚠ 注意是 **`x-phanthy-client`**（值 `claude`/`codex`/`launcher` 等），
**不是** `x-phanthy-client-source`。这两个是不同的头。

### 2.6 设备指纹 `x-phanthy-cli-device`（与我们同形）

```js
{ version: 1, collectedAt, cliVersion, hostname, username, platform, arch, osRelease,
  ipCandidates, networkInterfaces, routes, defaultGateway }
// 序列化 base64url；超过 6000 字符就依次 pop routes → networkInterfaces → ipCandidates 直到塞得下
// 结果缓存 5 分钟（je = Date.now() + 3e5）
```

### 2.7 注入给 agent 的环境

| Agent | 注入 |
|---|---|
| Claude Code | `ANTHROPIC_BASE_URL=<本地代理>`、`ANTHROPIC_AUTH_TOKEN=<代理 secret>`、`ANTHROPIC_API_KEY=""`、`CLAUDE_CODE_MAX_CONTEXT_TOKENS`、`CLAUDE_CODE_USE_{BEDROCK,VERTEX,FOUNDRY}=0` |
| Codex | `model_providers.phanthy_launcher.base_url=<本地代理>`、`.env_key=PHANTHY_LAUNCHER_TOKEN`、`.wire_api=responses`、`model_provider=phanthy_launcher` |
| phanthy-codeo | `PHANTHY_CODE_OAUTH_TOKEN`、`PHANTHY_CODEO_CONFIG_CONTENT` |

---

## 3. ★★ 最关键的发现：**它完全不做签名 / 设备注册 / 推理令牌**

对 `dist/cli.js` 做全文计数（大小写不敏感）：

| 检索词 | 出现次数 |
|---|---|
| `register` | **0** |
| `desktop-installations` | **0** |
| `inference_token` | **0** |
| `proof` | **0** |
| `desktop-session` | **0** |
| `ed25519` | **0** |
| `x-desktop` | **0** |

**⇒ 它发往 `code.phanthy.com` 的推理请求，只有：**
1. `Authorization: Bearer <access_token>`
2. Agent 自带的头（`anthropic-version`、`content-type` 等）
3. `x-phanthy-project` / `x-phanthy-session` / `x-phanthy-request` / `x-phanthy-client`
4. `x-phanthy-cli-device`（设备指纹）

**没有 4 个 `x-phanthy-proof-*`、没有 180 秒推理令牌、没有设备注册、没有 12 段 canonical。**

### 3.1 这与本仓的既有结论**直接冲突**

`internal/phanthywork/client.go` 里写着（真机依据）：

> 「早先的实现断言『chat 路径全程零签名』。那个结论来自只抓到了链路的其中一跳…
>  而 `Sd3` 里明确包含 `/v1/chat/completions` —— 也就是说 chat 这条路**同样要签**。
>  旁证：上游对缺这个头的请求回的正是 403 `request_not_authorized` `missing desktop session`。」

而 PCL **不带这些头也发布并声称可用**。

### 3.2 三种可能的解释（必须实测才能定）

| # | 解释 | 怎么区分 |
|---|---|---|
| A | `/v1/messages`（Anthropic wire）不要求签名，`/v1/chat/completions` 要求 | 用同一 token 各打一条，对比 |
| B | 服务端后来放宽了要求（本仓那结论是更早的真机） | 现在打一条无签名的 `/v1/chat/completions` |
| C | PCL 实际只在 `/v1/messages` 上被验证过，`/v1/chat/completions` 只是正则覆盖、**未必真能跑** | 看 README：它只承诺 Claude Code / Codex / OpenCode / phanthy-codeo，**没有承诺 OpenAI 兼容的 `/v1/chat/completions`** |

⚠ **C 的可能性不低**：它的正则 `/^\/v1\/(messages|responses|chat\/completions)/` 更像是"通用代理的路径白名单"，而不是"这三个我都验证过"。

---

## 4. 与官方/我们的差异点（含一处我差点写错的自我更正）

### 4.0 ⚠ 先更正：本文初稿把下面这两样写成"我们不知道"，那是错的

我第一遍写完时标了两条「❌ 不知道」，随后回仓库核对，**两条都是本仓早已记录过的**：

| 我初稿的说法 | 事实 |
|---|---|
| `/api/oauth/usage` 我们不知道 | **错**。`quota.go:20` 明确提到它，而且有一条**否决性决策**（见 §4.1） |
| `/api/event_logging/batch` 我们不知道 | **错**。`provider.go:184` 与 `chatcompletions_test.go:10` 都有抓包实证记录 |

**教训（与 11-14 那次「ASCII 转储把 LF 显示成点」同类）**：
**在把某样东西称为"新发现"之前，先在自己仓库里搜一遍。** 否则会把已有知识当成空白，
据此提出"值得一试"的建议，而那条路早就被评估并否决过。

### 4.1 `/api/oauth/usage` —— 本仓已知，且**刻意不用**它当额度

PCL 用它做 usage 展示（`fe(e,"/api/oauth/usage")`，与 `/api/work/rewards?limit=5`、
`/api/oauth/profile` 并发拉，超时/网络/408/5xx 才重试一次）。

本仓 `quota.go:20-22` 的原话：

> ⚠ 也**不**把 `/api/oauth/usage` 的四窗口当额度上卡片：报告 §4.3 引官方口径 ——
> 当前套餐用单一个月度积分池，5 小时/7 天那两个窗口**不是独立额度**，
> 拿它们上卡片会把同一个池重复计权。

⇒ **分歧点不是"知不知道"，而是"怎么解释"**：PCL 把它当 usage 面板展示；
我们认为它的多窗口会重复计权，所以额度仍走 `activities/summary`。
**这不是 PCL 更优**，只是取舍不同。

### 4.2 `/api/event_logging/batch` —— 两边都在打，互相印证

PCL：

```js
await o(`${$}/api/event_logging/batch`, {
  method: "POST",
  headers: { ...M(), "Content-Type": "application/json",
             "x-service-name": "phanthy-code-launcher", Authorization: `Bearer ${h}` },
  body: JSON.stringify({ events: d }),
  signal: AbortSignal.timeout(3000)
});
// 每 1 秒 flush 一次（setInterval 1e3）；401/403/404 退避 5 分钟，其它最多 60 秒
```

本仓 `provider.go:184`（抓包 `BUN_CONFIG_VERBOSE_FETCH=curl` 跑 `phanthy-codeo run`）：

> 全程只出现两条 POST：… `POST /api/event_logging/batch`

⇒ **两个独立实现都看到同一个遥测端点** —— 这条是**互证**，不是新发现。
而且它给了我们一个额外信息：**官方内核会发遥测，我们从来不发。**
如果"异常客户端"是按行为指纹判定的，**"从不发遥测"本身就是一个差异**。
（是否重要未知，但值得记一笔。）

### 4.3 ★ PCL 发了两个**官方不发的头** —— 与我们的假设相反

PCL 的头构造器（§2.5）：

```js
{ "x-phanthy-project": ..., "x-phanthy-session": ..., "x-phanthy-request": ..., "x-phanthy-client": ... }
```

而本仓 `chatcompletions_test.go:396` 把 `x-phanthy-project` 放在**负断言列表**里：

```go
"x-phanthy-project", // 官方 sidecar 字符串里 0 次
```

`client.go:405` 的注释也写着：

> （连 `x-phanthy-project` 也是 0 次）—— 这两个头是当初照"想当然的形态"…

**⇒ 三方在这个点上分成两派：**

| | `x-phanthy-project` | `x-phanthy-client` |
|---|---|---|
| 官方 sidecar | **不发**（全文字符串 0 次） | 不发 |
| **PCL** | **发** | **发**（值 = `claude`/`codex`/`launcher`） |
| 我们 | **不发**（刻意，有负断言） | 不发 |

**这条对我们有两个方向的含义，都别急着下结论：**

- 若"多一个官方没有的头 = 异常客户端"成立 ⇒ **PCL 的形态在我们这条假设下也是异常的**，
  那它"能跑"只能说明"能跑 ≠ 不被记账"。
- 若服务端对 `x-phanthy-*` 一律宽容 ⇒ 那我们当初为了"不多发一个头"做的对齐，
  在**请求能否被接受**这个层面是过度谨慎（但在**风控**层面仍可能必要）。

**⇒ 结论：这两种解释都还没被证伪，而它们指向完全相反的策略。**
这正是 §7 的 V1/V2 要先做的原因 —— 先把"服务端要求什么"测出来，再谈"要不要对齐到洁癖"。

### 4.4 它的重试策略（与官方不同）

```js
async function fe(e, n) {                      // 账号探测类：重试一次
  try { return await e(n, { timeoutMs: 12e3 }) }
  catch (t) {
    let o = ot(t);
    if (["timeout","network"].includes(o.kind) || o.status === 408 || o.status >= 500)
      return e(n, { timeoutMs: 8e3 });
    throw t
  }
}
// 但推理路径：401/403 时**明确不重试**，并给 agent 回 x-should-retry: false
```

官方 `Pd3`：3 次重试、只对 429/5xx、退避表。我们已照官方对齐。
**三者又不一致** —— 但这一条影响的是"失败后的行为"，不是"请求形状"，暂不改。

### 4.5 续期请求的头 —— 与官方 `Pd3` **不一样**

```js
fetch(`${$}/oauth/token`, {
  method: "POST",
  headers: { ...M(), ...await Y(), "content-type": "application/json" },   // ★ 带 x-phanthy-* 与设备头
  body: JSON.stringify(e)
})
```

而官方 `Pd3` 只有 `Content-Type` + `Accept`（**我们的 D1 修复就是照官方做的**）。

⇒ **两个能跑的实现在这里不一致**。两种解释：要么服务端对此宽容，
要么 PCL 的续期在某些情况下并不生效（它把 `invalid_grant` 直接归为"重新登录"，见其 `F()`）。
**这条不构成改回带头的理由**（官方才是官方），但说明"极简头"不是唯一可行解。

---

## 5. 三方对照

| 维度 | 官方 sidecar 桥 | **PCL（社区）** | 我们的 `internal/phanthywork` |
|---|---|---|---|
| OAuth | 客户端内建 | 自己实现 PKCE+loopback | 自己实现 PKCE+loopback |
| `client_id` | `phanthy-code-cli` | `phanthy-code-cli` | `phanthy-code-cli` ✅ |
| 设备注册 | **有** | **无** | 有 |
| 180s 推理令牌 | **有** | **无** | 有 |
| proof 签名（ed25519 / 12 段） | **有** | **无** | 有 |
| 推理转发方式 | 桥重签后转发 | 本地代理**几乎原样**转发 | 自己构造请求 |
| 推理头的来源 | 桥用 `_O8` 重签 | 透传 agent 的头 + 4 个 `x-phanthy-*` | 全套自造 |
| 401 处理 | 续期后重试一次 | **不重试** | 有 |
| 403 处理 | 特定码条件重试 | **不重试** | 有 |
| 额度端点 | `activities/summary` | **`/api/oauth/usage`** | `activities/summary` |
| 续期的头 | 只 `Content-Type`+`Accept` | `x-phanthy-*` + 设备头 | 只 `Content-Type`+`Accept`（照官方） |
| 遥测 | — | `/api/event_logging/batch` | 无 |
| 每号一个实例 | 是（结构限制） | **不需要**（一个 `pcl` 进程可反复登录） | 不需要 |

**⇒ 三者里 PCL 的实现最薄。** 它赌的是"服务端只认 Bearer"。

---

## 6. 回答：反代是不是更容易？

### 6.1 如果 §3.2 的 A 或 B 成立（即无签名可用）

**会容易非常多。** 我们的工作量对比：

| 现在必须做 | 若无需签名 |
|---|---|
| PKCE 登录 + 兑换 | 仍需（一条路） |
| 三把 ed25519 密钥派生 + 12 段 canonical + 4 个 proof 头 | **全删** |
| 180 秒推理令牌两跳换取 + 缓存/并发闸 | **全删** |
| 设备注册 + 409 熔断 + accountScope canonical | **全删** |
| v1 签名（`x-desktop-*`）用于 summary/claim/referral | 取决于 `/api/*` 是否也要签 |
| 时钟偏移校准（只为 proof 时间戳） | **大幅简化** |

⇒ 代码量可以砍掉一半以上，而且**去掉的正是历史上出错最多的部分**
（坑②UA、坑④令牌过期、坑⑧body 序列化、坑⑪幂等键、以及本轮所有签名对齐工作）。

### 6.2 如果只有 `/v1/messages` 免签（A 成立）

那对我们的价值有限 —— 我们的网关是 OpenAI 兼容的 `/v1/chat/completions`。
但仍有两条可用的东西：
- 确认了 `x-phanthy-cli-device` 的构造**与我们同形**（互相印证，说明这个头我们早就是对的）
- 证明了 `Authorization: Bearer` 是**唯一必需的凭据载体**（PCL 只换这一个头就能跑通它的路径）

⚠ 注意**不要**把 `/api/oauth/usage` 当成"更轻的额度端点"捡回来 —— 见 §4.1，
本仓已有否决性决策（它的多窗口会把同一个池重复计权）。

### 6.3 PCL 本身能不能直接用？

- 它是**启动器**，不是服务：`pcl` 起代理 → 拉起 agent。**没有常驻服务/多账号管理/额度路由**。
- 一个 `pcl` 进程 = 一份 `~/.phanthy-code-launcher/credentials.json` = 一个账号
  （但它不像桥那样需要独立进程实例，可以换 `PHANTHY_LAUNCHER_HOME` 切号）
- **不能替代我们的网关**：没有 OpenAI 兼容入口、没有多号池、没有限额/计费。

⇒ **它不能当组件用，但它是极好的「可行性证据」。**

---

## 7. 必须实测的四件事（按价值排序）

| # | 实验 | 用什么 | 判据 |
|---|---|---|---|
| **V1** | **无签名打 `/v1/messages`**（Bearer + 4 个 `x-phanthy-*` + 设备头，body 最小化） | 一个活号 | 200 ⇒ 签名非必需（对 messages）；403 `request_not_authorized` ⇒ 仍需要 |
| **V2** | **无签名打 `/v1/chat/completions`** | 同上 | 200 ⇒ 我们的整套 proof 可删；403 ⇒ 保留 |
| **V3** | `GET /api/oauth/usage` | 同上 | 200 ⇒ 可替代 `activities/summary` 做额度查询（更轻、不用签名、不换令牌） |
| V4 | 续期时带 vs 不带 `x-phanthy-*` | 同上 | 两者都 200 ⇒ 说明服务端宽容；只有一种 200 ⇒ 按那种对齐 |

**V1/V2 是战略级的**：它们决定我们的 `internal/phanthywork` 能不能砍掉一半。
**成本**：各一次最小推理请求（会消耗该号额度）。

⚠ 前置：这三个实验**必须用一个可牺牲的号**，不能用在你在用的桌面号上。
（2026-11-14 我已经违反过这条纪律一次，见
`pw-bridge-architecture-plan-2026-11-14.md` 的 P-1。）

---

## 8. 风险与不确定性

| # | 风险 | 说明 |
|---|---|---|
| R1 | PCL 是**第三方个人项目**，无官方背书 | 它以 MIT 发布，作者会随时停更；不能当"官方行为"引用 |
| R2 | 它自己在 README 里说「**主要在 macOS 上验证**」 | Windows 上可能根本没跑通；不能假设它验证过 Windows |
| R3 | 它未承诺 OpenAI 兼容路径 | 正则覆盖 `chat/completions` ≠ 验证过 |
| R4 | 无签名若只是"服务端当前宽容" | 可能随时收紧；**不能把"能跑"当成"协议"** |
| R5 | 它的续期头与官方不一致 | 说明这条链路服务端不严格；反过来也说明**我们的对齐可能不是必需的** —— 但这不该成为放松对齐的理由 |
| R6 | 它的 agent 头是**透传**的 | 如果 agent 自己发的头里有异常，会被原样送上游。这解释了为什么它能"自然地对齐"——因为它根本不复刻，它就是转发 |

---

## 9. 结论

1. **PCL 证明了一件重要的事**：存在一条"只靠 Bearer + 4 个 `x-phanthy-*` + 设备指纹"就能打
   `code.phanthy.com` 的路径，**不需要 ed25519、不需要设备注册、不需要 180s 推理令牌**。
2. **但它没有证明这条路对我们的形态成立** —— 它主打 `/v1/messages`（Claude Code），
   而我们是 `/v1/chat/completions`。**V1/V2 必须实测。**
3. **它不能当组件复用**（启动器 ≠ 服务），但它的两个端点（`/api/oauth/usage`、
   `/api/event_logging/batch`）和它的"透传而非复刻"思路值得吸收。
4. **最有价值的启示不是"抄它"，而是"它为什么能这么薄"**：因为它**不去复刻协议，只转发**。
   这一点和官方 sidecar 桥是同一个设计哲学 —— 两个独立实现都选了"转发"，
   只有我们在"自己构造"。**这是本次分析最该记住的一条。**
