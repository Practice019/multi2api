# 实施计划：把续期链路按官方客户端 1:1 复刻

> 日期：2026-11-14 · 基线 `master = c6c8874`
> 目的：依据**官方客户端源码**（不是推断）修正我们 `phanthywork` 的续期实现。
> 前置阅读：`docs/reports/pw-renewal-investigation-2026-11-14.md`（调查交接）
> 本文是**计划**，不含已完成的改动。每一条都标了源码出处（字节偏移）。

---

## 0. 怎么拿到这份源码的（可复现）

```powershell
# 官方桌面端的 sidecar 是 Bun 单文件可执行，JS 源码以明文内嵌
$exe = 'D:\software\PW-windows-x64\_up_\claude-sidecar.exe'
# 已有一份 108 MB 的 ASCII 转储（本次分析就用它）：
#   D:\temp\phanthy-sidecar-ascii.txt
# 重新生成：把 exe 字节按 ASCII 可打印过滤后落盘即可（无需反编译）
```

**关键：不要去读 `_up_\dist\assets\*.js`。** 已实测确认那是纯前端，
`refresh_token` / `grant_type` / `oauth/token` **出现 0 次** —— 续期完全不在前端做。

---

## 1. 官方续期的完整调用链（逐字，附出处）

### 1.1 入口：`tH(q, K={})` —— 给定"当前 access_token"，返回一个可用的 access_token

出处：偏移 `@99948835`。

```js
async function tH(q, K = {}) {
  let _ = K.read    ?? Nd3,        // 读全部凭证（文件）
      z = K.refresh ?? l44,        // 发续期请求（= Pd3()，见 1.3）
      w = K.persist ?? Ad3,        // 落盘
      O = K.now     ?? Date.now,
      $ = await _(),
      Y = o44($, q, O());          // ★① 刷之前先看共享库里有没有别人刷好的
  if (Y) return Y;                 //    → 有就直接用，**一个请求都不发**

  let H = $.find((X) => X.oauth.accessToken === q);   // 用 access_token 定位自己那条
  if (!H || typeof H.oauth.refreshToken !== "string") return null;

  let J = await z(`${R1()}/oauth/token`, {
    grant_type: "refresh_token",
    refresh_token: H.oauth.refreshToken,
    client_id: YM().oauthClientId,
    scope: Array.isArray(H.oauth.scopes) ? H.oauth.scopes.join(" ") : void 0
  });
  if (!J) return o44(await _(), q, O());   // ★② 续期返回 null ⇒ **重读共享库**再试一次

  let j = {
    ...H.oauth,
    accessToken: J.access_token,
    refreshToken: J.refresh_token ?? H.oauth.refreshToken,      // ★③ 没给新的就沿用旧的
    expiresAt: O() + (typeof J.expires_in === "number" ? J.expires_in : 3600) * 1000,
    ...(typeof J.scope === "string" ? { scopes: J.scope.split(" ").filter(Boolean) } : {})
  };
  if (!(await w(H, j))) throw new Po;        // ★④ 落盘失败 ⇒ 抛错（不静默）
  return J.access_token;
}
```

### 1.2 `o44` —— "别人已经刷好了吗？"（**这一环我们完全没有**）

出处：`@99948600` 附近。

```js
function o44(q, K, _) {                    // q=全部凭证, K=当前 access_token, _=now
  let z = Vd3(q, _);                       // 从全部凭证里按"新鲜度"挑出该用的那条
  if (!z || z.oauth.accessToken === K) return null;   // 还是同一个 ⇒ 没人刷过
  let w = ay(K), O = ay(z.oauth.accessToken);         // ay() = 解 JWT 取 sub
  if (!w || w !== O) return null;          // ★ sub 不同 ⇒ 不是同一账号 ⇒ 不采用
  return z.oauth.accessToken;
}
```

`Vd3` 的分组很讲究（`@99948300`）：把候选按**来源**分成三组再择优 ——
`keyring 且 service 命中当前配置目录`、`keyring 但不命中`、`file`：

```js
function Vd3(q, K) {
  let _ = new Set(Dd3(S3())),                            // 当前配置目录派生的 keyring 名
      z = (H) => t44(H.raw),
      w = q.filter((H) => H.service && _.has(H.service)),
      O = q.filter((H) => H.service && !_.has(H.service)),
      $ = q.filter((H) => H.file),
      Y = e44(w.map(z)..., $.map(z)..., O.map(z)..., K);
  return Y ? q.find((H) => H.oauth.accessToken === Y.accessToken) ?? null : null;
}
```

### 1.3 `Pd3()` —— 续期传输层（重试 / 退避 / 单飞 / 错误分类）

出处：`@99945774`。

```js
function Pd3(q = {}) {
  let K = q.now ?? Date.now, _ = q.sleep ?? ((O) => new Promise(($) => setTimeout($, O))),
      z = new Map,     // 成功结果缓存（键 = sha256(url+body)）
      w = new Map;     // 失败退避表（键 → 解禁时刻）
  return (O, $) => {                                   // (url, payload)
    let Y = JSON.stringify($),                          // ★ body 是 JSON 字符串
        H = Zd3("sha256").update(O).update(Y).digest("hex"),
        J = z.get(H);
    if (J) return J;                                    // 同请求的成功结果直接复用
    if ((w.get(H) ?? 0) > K()) return Promise.reject(new Po);   // 退避期内直接拒
    w.delete(H);
    let j = (async () => {
      for (let X = 0; X < 3; X++) {                     // 最多 3 次
        let f;
        try {
          f = await (q.fetcher ?? fetch)(O, {
            method: "POST",
            headers: { "Content-Type": "application/json", Accept: "application/json" },  // ★ 只有这两个
            body: Y,
            signal: AbortSignal.timeout(8000)           // 8 秒
          })
        } catch {}
        if (f?.ok) {
          let P = await f.json().catch(() => null);
          if (!P || typeof P.access_token !== "string" || !P.access_token) throw new Po;
          return P;
        }
        if (f && f.status !== 429 && f.status < 500) {   // 其它 4xx
          let P = await f.json().catch(() => null),
              k = typeof P?.error === "string" ? P.error : P?.error?.code;
          if (f.status === 400 && k === "invalid_grant") return null;   // ★★ 返回 null，不抛错
          throw new Po;
        }
        let M = f?.headers.get("retry-after"), W = M ? Number(M) : NaN,
            v = M ? (Number.isFinite(W) ? W * 1000 : Date.parse(M) - K()) : NaN,
            Z = Math.max(500 * 2 ** X, Number.isFinite(v) ? v : 0);
        if (X === 2 || Z > 2000) {
          if (w.size >= 128) w.delete(w.keys().next().value);
          throw w.set(H, K() + Math.min(Math.max(Z, 5000), 300000)), new Po;
        }
        await _(Z)
      }
      ...
```

### 1.4 `Po` —— 失败时的错误形态

出处：`@99946968`。

```js
Po = class Po extends p6 {
  constructor() {
    super(503, "登录服务暂时不可用，请稍后重试；当前登录信息已保留。", "OAUTH_REFRESH_UNAVAILABLE")
  }
}
```

⇒ **官方把续期失败表达成 `503 OAUTH_REFRESH_UNAVAILABLE`（可重试），
并明确告诉用户「当前登录信息已保留」。它从不把 `invalid_grant` 直接升级成"你该重新登录"。**

### 1.5 凭证的存储位置（两处，不止一处）

出处：`kd3()` `@99947100`、`S3()` `@97912695`、`Nd3()` `@99947900`。

```js
function kd3(q) {                       // keyring 服务名（Windows 凭据管理器）
  let K = sha256(q).digest("hex").slice(0, 8);
  return [`phanthy code-credentials-${K}`, `Phanthy Code-credentials-${K}`,
          ...(YM().brand === "opus" ? [] : ["phanthy code-credentials", "Phanthy Code-credentials"])]
}
function S3() {                         // 配置目录
  return (process.env[q.configDirEnv] || path.join(os.homedir(), q.configDirName)).normalize("NFC")
}
async function Nd3() {                  // 文件形态：<configDir>/.credentials.json
  let K = path.join(S3(), ".credentials.json") ...
}
```

⇒ 官方凭证有 **两个可能的位置**：`<configDir>/.credentials.json`（文件）与
**OS keyring**（`phanthy code-credentials-<sha256(configDir) 前 8 位>`）。

### 1.6 落盘的形态

出处：`Ad3()` `@99948000`。

```js
async function Ad3(q, K) {              // q=原条目, K=新 oauth 对象
  let _ = q.oauthKey ? { ...q.root, [q.oauthKey]: K } : K;   // 只替换 oauth 子对象
  if (q.file) try { return fs.writeFileSync(q.file, JSON.stringify(_), { mode: 0o600 }), true } catch { return false }
  return false
}
```

---

## 2. 与我们当前实现的逐项对照

我们的实现：`internal/phanthywork/client.go` `RefreshTokens`（约 :786）、
`internal/phanthywork/login.go` `applyTokenSet`（约 :715）、
`internal/server/handler.go` `refreshCredential`（约 :3786）。

| # | 维度 | 官方 | 我们 | 判定 |
|---|---|---|---|---|
| 1 | body 编码 | JSON（`JSON.stringify`） | JSON（`json.Marshal`） | ✅ 一致 |
| 2 | 超时 | 8000 ms | `refreshTimeout = 8s` | ✅ 一致 |
| 3 | 重试次数 | 3 | `refreshMaxAttempts = 3` | ✅ 一致 |
| 4 | 重试条件 | 只 429 / ≥500 | 只 429 / ≥500 | ✅ 一致 |
| 5 | `refresh_token` 回退 | `J.refresh_token ?? 旧值` | 同 | ✅ 一致 |
| 6 | `expires_in` 兜底 | 3600 | `refreshDefaultExpiresIn = 3600` | ✅ 一致 |
| 7 | `scope` 参数 | `scopes.join(" ")` | 同 | ✅ 一致 |
| **8** | **请求头** | **只有 `Content-Type` + `Accept`** | 多了 `User-Agent: phanthy-code-cli/2.1.0`、`x-phanthy-client-source: desktop` | ❌ **多发 2 个头** |
| **9** | **`400 invalid_grant`** | **返回 `null`**（不抛错） | 抛 `ErrRefreshExpired` ⇒ 终态 | ❌ **语义相反** |
| **10** | **刷前查共享库** | `o44()`：有同 `sub` 的新 token 就直接采用、**不发请求** | **无** | ❌ **缺一整环** |
| **11** | **`null` 之后** | 重读共享库再试一次 | 直接判失败、计入 `refreshFails` | ❌ |
| **12** | **失败的对外形态** | `503 OAUTH_REFRESH_UNAVAILABLE`（可重试，「登录信息已保留」） | 终态「需重新登录」→ 3 次后禁用 | ❌ |
| 13 | 成功结果缓存 | 同 url+body 缓存复用 | 无（`refreshGate` 只合并"进行中"，不回放） | ⚠ 差异，风险低 |
| 14 | 失败退避表 | 5s–300s 内同请求直接拒 | 无（只有熔断/冷却） | ⚠ 差异，风险中 |
| 15 | 落盘形态 | 只替换 `oauth` 子对象，`0600` | 重写整份（原子 `.tmp`+`rename`） | ⚠ 等价，但覆盖面不同 |
| 16 | 凭证来源 | 文件 **+ OS keyring** | 仅 `auths/phanthywork/*.json` | ⚠ **看不到 keyring** |

### 2.1 三条最重要的偏差（按可疑度排序）

**D1（第 8 条）：续期请求多发了两个"推理路径"的头**

我们 `applyCommonHeaders`（`client.go:222`）给**所有** `/api/*`、`/oauth/token` 请求都加上了
`User-Agent: phanthy-code-cli/2.1.0` 和 `x-phanthy-client-source: desktop`。
而官方续期**只发 `Content-Type` + `Accept`**。

这两个头在官方那边属于**推理路径**（`x-phanthy-client-source` 的取值只在推理时用）。
续期路径带上它们，等于在"换令牌"这个动作上额外声明"我是桌面端" —— 这是**风控最容易记账的形态**。

**D2（第 9、11、12 条）：我们把 `invalid_grant` 当终态，官方当"可能被别人轮换掉了"**

官方拿到 `invalid_grant` 后**不发脾气**：先重读共享凭证库，看同一 `sub` 有没有更新的 token；
有就采用（可能就被救活了），没有才回一个**可重试的 503**。

我们直接判 `ErrRefreshExpired` ⇒ `NoteRefreshFailure` ⇒ 3 次后**永久禁用**。
⇒ **一次上游抖动/一次轮换竞态就能把一个健康的号打成死号。**

**D3（第 10 条）：缺"刷前先看共享库"这一环**

官方在**每次续期之前**都先看共享库里有没有别人（桌面端 / 另一个进程）刚刷好的同账号 token，
有就直接用、**一个请求都不发**。这既是省请求，也是**避免轮换竞态**的关键。

我们每次都直接刷 ⇒ 与桌面端同时登录时必然互相把对方的 refresh_token 换掉
（这正是 `docs/agent2api-蒸馏报告.md` §L2 记下的那条教训）。

---

## 3. 实施计划

### 阶段 0 · 先确认，不改代码（半天）

| 编号 | 动作 | 产出 |
|---|---|---|
| P0-1 | 确认官方续期**确实只发 2 个头**：把 `Pd3` 的 headers 与我们的 `applyCommonHeaders` 并列贴进协议文档 | 一页对照表 |
| P0-2 | 确认 `R1()`（base URL）的取值来源 —— `R1()` 的定义没直接抓到，但已见 `PHANTHY_BASE_URL` / `PHANTHY_CODE_BASE_URL` 两个 env（`@100039833`）；建议按 `PHANTHY_BASE_URL` 优先实现并留注释 | 一条结论 |
| P0-3 | 摸清 keyring 那一侧：`phanthy code-credentials-<sha8>` 在 Windows 凭据管理器里到底有没有条目（`cmdkey /list`）；有的话说明桌面端把凭证也写在那里 | 有/无 |
| P0-4 | 读 `Vd3`/`e44`/`C$7`/`Rd3` 的择优算法并写成规格（"多个来源都有凭证时选哪个"） | 一段规格 |

**退出条件**：D1/D2/D3 三条都有源码依据 + 我们能复述"官方刷新一次的完整决策树"。

### 阶段 1 · 修 D1（最小、最可能直接止血）

**改动**：`RefreshTokens` 不再走 `applyCommonHeaders`，改用独立的最小头集合
（`Content-Type: application/json` + `Accept: application/json`）。

**判据**：
- `TestRefreshSendsOnlyContentTypeAndAccept`（新）—— 断言续期请求**恰好**这 2 个头，
  且**不含** `User-Agent` / `x-phanthy-client-source`。
- 反向验证：把 `applyCommonHeaders` 加回去 ⇒ 判据红。

**风险**：极低。若上游对缺 UA 的请求有额外策略，一次真机续期即可证伪。

### 阶段 2 · 修 D2（把 `invalid_grant` 从终态改成"先找别人刷好的"）

**改动**（`internal/phanthywork/client.go` + `login.go`）：

1. `RefreshTokens` 在 `400 + invalid_grant` 时返回一个**可识别但不致命**的结果
   （建议：`(TokenSet{}, errRefreshRejected)`，其中 `errRefreshRejected` **不是** `ErrRefreshExpired`）。
2. `RefreshCredential` 收到它之后：**先重读凭证**（见阶段 3 的共享库读取），
   若同一 `sub` 有了新的 accessToken ⇒ **直接采用并返回成功**；
   否则返回一个**可重试**的错误（对应官方 `503 OAUTH_REFRESH_UNAVAILABLE`），
   并**不计入** `refreshFails`。
3. `ErrRefreshExpired` 只保留给"**凭证里连 refresh_token 都没有**"这种真正的终态
   （官方 `tH` 里 `if (!H || typeof H.oauth.refreshToken !== "string") return null`）。

**判据**：
- `TestInvalidGrantAdoptsRotatedTokenFromStore`：共享库里放一个同 `sub`、更新、不同的
  accessToken ⇒ 续期**不发请求**、直接返回那个 token、**不计失败**。
- `TestInvalidGrantIsNotTerminal`：共享库无新 token ⇒ 返回可重试错误，
  **`refreshFails` 不增**，账号**不被禁用**。
- `TestDifferentSubIsNotAdopted`：库里是**另一个账号**的 token ⇒ 不采用。
- 反向验证：三条各自注入违规（跳过 sub 校验 / 直接判终态 / 计一次失败）⇒ 各自变红。

**风险**：中。`refreshFails` 不再因 `invalid_grant` 增长，意味着**真死的号会被反复尝试**
—— 必须同时引入官方那套**失败退避表**（阶段 4），否则会打上游。

### 阶段 3 · 修 D3（刷前查共享库）

**改动**：新增"凭证共享库读取"层，来源按官方优先级：

1. 我们自己维护的 `auths/phanthywork/*.json`（现状已有）
2. **官方客户端的 `<configDir>/.credentials.json`**（`<configDir>` 默认 `~/.phanthy-code`，
   可用 `PHANTHY_CONFIG_DIR` 覆盖）
3. （可选）OS keyring `phanthy code-credentials-<sha8>` —— 先只做**探测**，不做读写

在 `RefreshCredential` **发请求之前**调用一次"择优"（阶段 0 的 P0-4 规格），
若已有同 `sub` 且更新的 accessToken ⇒ 采用它并落盘到我们自己的文件，**不发请求**。

**判据**：
- `TestAdoptsFresherTokenFromOfficialStore`：在临时目录造一份 `.credentials.json`
  （同 `sub`、`expiresAt` 更晚）⇒ 续期不发请求、采用它。
- `TestIgnoresStaleOfficialToken`：官方那份更旧 ⇒ 仍然自己刷。
- 反向验证：把 sub 比较改成恒真 ⇒ 第一条绿、`TestDifferentSubIsNotAdopted` 红。

**风险**：中。要处理"官方文件不存在 / 格式变化 / 被占用"三种容错，全部按"读不到就当没有"处理。

### 阶段 4 · 补失败退避表（配合阶段 2）

**改动**：按官方 `Pd3` 的 `w` map 语义 —— 失败后对**同一份请求**（url+body 哈希）
在 `clamp(退避, 5s, 300s)` 内**直接拒绝、不发请求**；成功后才清。

**判据**：
- `TestRefreshBackoffSuppressesRepeatedFailure`：连续触发 4 次，第 2–3 次在同一退避窗口内
  ⇒ 实际只打了**最多 3 次**上游（而不是 4×3=12 次）。
- 反向验证：去掉退避表 ⇒ 上游调用次数翻倍 ⇒ 红。

**风险**：低。

### 阶段 5 · 把结论回写文档与判据索引

- `docs/protocols/phanthywork-出站请求对照表.md` 新增一节"续期链路（官方逐字）"，
  含 1.1–1.6 的全部片段与偏移。
- 更新 `docs/reports/pw-renewal-investigation-2026-11-14.md` 的 §4.5/§5：
  原来写的「`invalid_grant` 是终态、本地无解」**依据不足**，应改为
  「官方的处理是先重读共享库、再无解才回 503 可重试」。
- 判据索引登记阶段 1–4 的 6 条新判据。

---

## 4. 明确不做的事

- **不改** `maxRefreshFails = 3` 这个阈值本身（阶段 2 只改"哪种错误才计数"）。
- **不做** keyring 的**写入**（只做探测/读取可选）。风险/收益不划算。
- **不碰** 推理路径（`/v1/chat/completions`）的头与签名 —— 那部分已在 `9f4f402` 真机验证。
- **不引入** 第三方库；官方用的 `AbortSignal.timeout` 对应 Go 的 `context.WithTimeout`，
  sha256 用标准库。

---

## 5. 待确认 / 未知

| 编号 | 未知 | 影响 | 怎么确认 |
|---|---|---|---|
| U1 | `R1()` 的确切取值（env 名与默认值） | 阶段 0 的 P0-2 | 在转储里搜 `R1` 的赋值形态；或看 `PHANTHY_BASE_URL` 的读取处 |
| U2 | 官方是否**也**把凭证写进 keyring（还是只读） | 阶段 3 的可选来源 | `cmdkey /list` 看有没有 `phanthy code-credentials*` |
| U3 | `Vd3`/`e44` 的完整择优算法（多来源冲突时选谁） | 阶段 3 的正确性 | 读 `@99948000`–`@99949000` 全段 |
| U4 | 桌面端与本网关**同时登录**时，谁先刷 | 决定阶段 3 能救回多少场景 | 真机实验：两边同时放着，看 `.credentials.json` 的 mtime 变化 |
| U5 | 我们的 `invalid_grant` 到底是"被轮换"还是"被吊销" | 决定阶段 2/3 能否救活 | 用阶段 3 实现后观察：若重读能拿到新 token ⇒ 是被轮换；拿不到 ⇒ 是被吊销 |

---

## 6. 验收标准

**整体验收（一条即可）**：

> 一个**新注册**的 phanthywork 账号，在**同时登录官方桌面端**的前提下，
> 连续存活 **≥3 个令牌周期（≥90 分钟）**，期间 `success_count` 增长、
> `refreshFails` 始终保持 0，且 `/admin/logs` 里不出现
> `invalid_grant` / `需重新登录` / `已禁用`。

**分阶段验收**：见阶段 1–4 各自的判据。

**回滚**：每个阶段单独提交，可独立 `git revert`。阶段 1 与阶段 4 互不依赖，
阶段 2 依赖阶段 3 的读取层（若阶段 3 未完成，阶段 2 退化为"不计数、按 503 重试"，
仍比现状安全）。

---

## 7. 建议的动手顺序

```
阶段 0（确认） → 阶段 1（去掉多余的头，最小改动、最可能止血）
              → 阶段 3（共享库读取，阶段 2 的前置）
              → 阶段 2（invalid_grant 语义修正）
              → 阶段 4（失败退避）
              → 阶段 5（回写文档）
```

**理由**：阶段 1 只改 3 行、当天可验证；阶段 3 是阶段 2 的前置（没有"重读"能力，
阶段 2 只能退化成"无限重试"）；阶段 2 风险最高放后面；阶段 4 是阶段 2 的安全网。
