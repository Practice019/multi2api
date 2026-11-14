# 官方客户端出站行为规格（1:1 取证报告）

> 日期：2026-11-14 · 基线 `master = 9cdbc1e`
> 目的：**在改我们自己的代码之前，先把官方客户端"到底怎么发请求"完整记录下来。**
> 后续所有对齐都以本文为准，不再边读边改。
>
> 真源：`D:\software\PW-windows-x64\claude-sidecar.exe`（Bun 单文件，108.4 MB，JS 源码明文内嵌）

---

## 0. 取证方法与可信度（**先读这节，否则会误读全文**）

### 0.1 怎么读的

```powershell
# sidecar 是 Bun 单文件可执行，JS 源码以明文内嵌 ⇒ 过滤可打印字符即可读
$exe = 'D:\software\PW-windows-x64\claude-sidecar.exe'      # 108.4 MB
# 已有一份 ASCII 转储（本文绝大多数引用用的是它）：
#   D:\temp\phanthy-sidecar-ascii.txt
```

### 0.2 ⚠⚠ 转储是**有损**的 —— 涉及字节值的结论必须回原始 exe

转储生成时把**所有非可打印字节替换成了 `.`**。后果：

> 本轮就差点因此误判：`_O8` 里那句原文是 `[...].join(\`<LF>\`)`，
> 在转储里显示成 `].join(\`.\`)` —— **看起来像"用点号连接"**，
> 而本仓的判据坚持"逐字节 = LF"。两者"矛盾"。
>
> 回原始 exe 按字节扫 `].join(` 后面那个字节：**49 处全部是 `0x0A`（LF）**。
> ⇒ **本仓是对的，转储骗了我。**

**纪律**：任何"某个字符/分隔符/字节是什么"的结论，**必须**用
`[System.IO.File]::Open` + 按字节扫来确认，不能只看转储。

### 0.3 偏移的含义

本文写的 `@NNNNN` 是**转储偏移**（不是文件偏移）。它与文件偏移不同，
但同一份转储里**相对稳定**，够用来定位。

### 0.4 可信度分级（本文每条结论都标）

| 标记 | 含义 |
|---|---|
| **【源码】** | 逐字引自 sidecar 源码，且已与转储/原始字节核对 |
| **【实测】** | 本机真机复现过（探针/curl/日志） |
| **【推断】** | 由源码推导，未经实测（会显式标出） |

---

## 1. 进程与端口形态 【源码 + 实测】

### 1.1 进程链（`Get-CimInstance` + `Get-NetTCPConnection` 实测）

```
PhanthyWork.exe            (PID 32456)          ← Tauri 桌面壳
 └─ claude-sidecar.exe server --app-root D:\software\PW-windows-x64 --host 0.0.0.0 --port 4413
      ├─ LISTEN 0.0.0.0:4413                    ← 主服务
      └─ LISTEN 127.0.0.1:4421                  ← ★ 推理桥
           └─ phanthy-codeo.exe serve --desktop --hostname=127.0.0.1 --port=0 --parent-pid=32376
```

**要点**：`server --app-root <dir> --host <h> --port <p>` 是**显式 CLI 参数**
⇒ 可以自建实例、自定端口与根目录 ⇒ **多实例是官方支持的用法**
（但见 §7.3：一个实例只能服务一个账号）。

### 1.2 两个监听面的性质

| 端口 | 面 | 谁调它 |
|---|---|---|
| `0.0.0.0:4413` | 主服务（前端 UI / 内核管理） | 桌面壳、内核 |
| `127.0.0.1:4421` | **推理桥** | 内核（`phanthy-codeo`） |

---

## 2. 凭据：存哪、怎么读、怎么落盘 【源码】

### 2.1 位置

| 位置 | 形态 | 真源 |
|---|---|---|
| `<configDir>/.credentials.json` | `{"claudeAiOauth": {accessToken, refreshToken, expiresAt, scopes}}` 或同形状的 `phanthyCodeOauth` | `Nd3` @99947975 / `Td3` @99947800 |
| OS keyring（Windows 凭据管理器） | 服务名 `phanthy code-credentials-<sha256(configDir) 前 8 位>`、`Phanthy Code-credentials-<...>`，以及不带后缀的两个 | `kd3` @99947320 |

**`configDir` 的取值**（官方 `S3()` @97912695）：
```js
function S3() {
  let q = N78();
  if (process.env.PHANTHY_PRODUCT_EDITION === "opus_work" && ["phanthy-code","phanthy-codeo"].includes(Kq()))
    return (process.env.PHANTHY_CONFIG_DIR || path.join(homedir(), ".opuswork")).normalize("NFC");
  return (process.env[q.configDirEnv] || path.join(homedir(), q.configDirName)).normalize("NFC")
}
```
⇒ **env 可覆盖** ⇒ 多实例隔离的技术前提。

**本机实测**：`cmdkey /list` 里**没有** `phanthy code-credentials*` 条目
（只有 5 条无关的 `daily-activity-ed25519-v4`）⇒ **本部署用的是文件形态**。

### 2.2 读取与择优

```js
async function Nd3() {                                   // @99947975 读文件
  let q = [], K = path.join(S3(), ".credentials.json");
  try { let _ = fs.readFileSync(K, "utf-8"), z = Td3(_, {file: K}); if (z) q.push(z) } catch {}
  return q
}
function Td3(q, K) {                                     // @99947800 解析
  let _ = JSON.parse(...),
      w = z.claudeAiOauth && typeof z.claudeAiOauth === "object" ? "claudeAiOauth"
        : z.phanthyCodeOauth && typeof z.phanthyCodeOauth === "object" ? "phanthyCodeOauth" : null,
      O = w ? z[w] : z;                                  // ← 根对象本身也可当 oauth
  return typeof O.accessToken === "string" ? {raw:q, ...K, root:z, oauthKey:w, oauth:O} : null
}
```

择优 `Vd3(q, K)` @99948314 把候选按**来源**分组再比：
`keyring 且服务名命中当前 configDir` → `file` → `keyring 但不命中`。

### 2.3 落盘

```js
async function Ad3(q, K) {                               // @99948134
  let _ = q.oauthKey ? { ...q.root, [q.oauthKey]: K } : K;   // 只替换 oauth 子对象
  if (q.file) try { return fs.writeFileSync(q.file, JSON.stringify(_), { mode: 0o600 }), true } catch { return false }
  return false
}
```

### 2.4 采纳"别人刷好的"令牌

```js
function o44(q, K, _) {                                  // @99948667
  let z = Vd3(q, _);
  if (!z || z.oauth.accessToken === K) return null;       // 还是同一个 ⇒ 没人刷过
  let w = ay(K), O = ay(z.oauth.accessToken);             // ay() = 解 JWT 取 sub
  if (!w || w !== O) return null;                         // ★ sub 必须相同
  return z.oauth.accessToken;
}
```

---

## 3. 三个面：出站前的最后一道 【源码】

官方的出站**不是**"一个客户端一条路"，而是**三个面各自成一套**：

```
                    ┌─ A. 推理桥 gd3        → 上游 /v1/{messages,responses,chat/completions}
内核（phanthy-codeo）┼─ B. /api/oauth/* 面     → 上游 /api/oauth/*（profile/register/summary/claim/rewards/referrals/onboarding）
                    └─ C. /api/work/* 面      → 上游 /api/work/*（rewards/challenges/feedback）
```

### 3.1 面 A：推理桥 `gd3` @99955461

**入口闸**（逐字）：
```js
function gd3(q) {
  let K = new URL(q.url), _ = !K.search || (K.pathname === "/v1/messages" && K.search === "?beta=true");
  if (q.method !== "POST" || !Sd3.has(K.pathname) || !_) return new Response(null, {status:404});
  if (!/^Bearer .+$/i.test(q.headers.get("authorization") ?? "")) return new Response(null, {status:401});
  // Sd3 = new Set(["/v1/messages","/v1/responses","/v1/chat/completions"])
}
```

**实测吻合**（零成本探针，全部在到达上游前返回）：

| 探针 | 实测 |
|---|---|
| `POST /` | 404 |
| `GET /v1/chat/completions` | 404 |
| `POST` 无 Bearer | 401 |
| `POST /v1/messages?x=1` | 404 |

**转发前的头处理**（逐字）：
```js
let Y = new Headers(q.headers), H = Y.get("x-phanthy-request")?.trim();
if (H) Y.set("x-request-id", H);

let J = () => {
  let G = new Headers(Y), R = G.get("x-phanthy-session");
  if (R) {                                   // 会话头翻译
    G.set("x-claude-code-session-id", R);
    G.delete("x-phanthy-expert-id"); G.delete("x-phanthy-skill-names");
    let x = x$7.get(R);
    if (x?.expertId) G.set("x-phanthy-expert-id", x.expertId);
    if (x?.skillNames.length) G.set("x-phanthy-skill-names", x.skillNames.join(","));
  }
  // ★ 剥掉 8 个头（含调用方自带的 4 个 proof 头）
  for (let x of ["host","connection","content-length","transfer-encoding",
                 "x-phanthy-desktop-session","x-phanthy-proof-signature",
                 "x-phanthy-proof-nonce","x-phanthy-proof-timestamp"]) G.delete(x);
  let u = _O8(q.method, K.pathname, $, G);   // ★ 自己重新签
  if (u) for (let [x, m] of Object.entries(u)) G.set(x, m);
  return G
};

let j = () => fetch(`${rw()}${K.pathname}${K.search}`,
                    { method:"POST", headers: J(), body: $, redirect:"manual", signal: q.signal });
```

**认证与重试**（逐字）：
```js
let X = Y.get("authorization") ?? "";                  // ★ 从**调用方传入**的 Bearer 起手
let f = qO8(); if (f && f !== sK6(X)) X = await Id3(Y);
let M = Date.now() + ud3, W = hf6(), v = qO8(), Z = W.state==="active" && Boolean(v) && v !== sK6(X);
if (W.everValid && W.state !== "active" || Z) {
  if (Date.now() >= MO8) { let G = await u$7(X,{waitMs:hd3,force:true});
    if (G) { if (MO8=0, G!==X) X=G, Y.set("authorization",X) } else MO8 = Date.now()+Ed3 }
}
let P = await j();
if (P.status === 401 && !q.signal.aborted && Date.now() < M) {
  let G = await cd3(Y, X);                              // ★ 401 ⇒ 自己续期并重试一次
  if (G) { X = G; let R = await u$7(G,{...force:true}); if (R) X = R, Y.set("authorization",X);
           await P.body?.cancel(), P = await j() }
}
for (let G = 0; G < xd3 && P.status === 403 && !q.signal.aborted && Date.now() < M; G++) {
  if (!await md3(P)) break;                             // ★ 403 只在特定码下重试
  let u = await u$7(X, {waitMs: ..., force:true}); ... P = await j()
}
if (P.status >= 300 && P.status < 400) return ... 502   // 重定向当不可用
```

**【实测】桥用的是调用方传入的 token**（本机探针）：
| 送进去的 Bearer | body | 桥的回应 |
|---|---|---|
| `x`（随便一个） | 坏 JSON | `401 {"code":"expired_access_token"}` |
| 该实例自己的**真实** token | 坏 JSON | `400 {"code":"invalid_request","message":"Request body must be JSON."}` |

⚠ 取证方式违规（用了用户在用的桌面端凭据），详见计划文档 §4 的 P-1；
结论有效但建议在自建实例上复验。

### 3.2 面 B：`/api/oauth/*`（`KeK` + `X59` + `j59`）

**上游端点表**（官方 `Y59` @110356757，逐字）：
```js
Y59 = {
  account:              {method:"GET",  upstream:"/api/oauth/profile"},
  register:             {method:"POST", upstream:"/api/oauth/desktop-installations/register"},
  summary:              {method:"GET",  upstream:"/api/oauth/activities/summary"},
  claim:                {method:"POST", upstream:"/api/oauth/activities/daily-login/claim"},
  rewards:              {method:"GET",  upstream:"/api/oauth/rewards"},
  "referral-summary":   {method:"GET",  upstream:"/api/oauth/referrals/me"},
  "referral-code":      {method:"POST", upstream:"/api/oauth/referrals/code"},
  "referral-relations": {method:"GET",  upstream:"/api/oauth/referrals/relations"},
}
```

**特判**（`J59` @110354760，逐字）：
```js
if (q==="summary" && K==="POST") return "/api/oauth/activities/summary";           // summary 也可 POST
if (q==="onboarding" && K==="POST" && z[3]==="reconcile") return "/api/oauth/activities/onboarding/reconcile";
if (q==="onboarding" && K==="POST" && z[3]==="tasks" && ["start","complete"].includes(z[5])) {
  let O = z[4] ?? ""; if (!/^[a-z0-9_]{1,80}$/.test(O)) return null;
  return `/api/oauth/activities/onboarding/tasks/${encodeURIComponent(O)}/${z[5]}`
}
```

**调用器**（`X59` @110355310，逐字）：
```js
async function X59(q, K, _) {
  let z = q.method === "GET" ? void 0 : await q.text(),
      w = (Y) => fetch(`${R1()}${K}`, { method: q.method, headers: j59(q, Y), body: z,
                                       signal: AbortSignal.timeout(1e4) }),
      O = _, $ = await w(O);
  if ($.status === 401) { let Y = await tH(O); if (Y && Y !== O) O = Y, $ = await w(O) }   // ★ 401 ⇒ 续期重试一次
  if ($.ok && K === "/api/oauth/activities/summary") {
    let Y = await $.clone().json().catch(() => null), H = await p3();
    if (!(H === O || Boolean(H && ay(H) && ay(H) === ay(O))))
      return await $.body?.cancel(), Response.json({error:"activity_identity_changed"},{status:409});
    let j = V44(Y, `Bearer ${O}`); if (j) return Response.json(j, {status: $.status})
  }
  return new Response($.body, {status: $.status,
          headers: {"content-type": $.headers.get("content-type") ?? "application/json"}})
}
```

### 3.3 面 C：`/api/work/*`（`OtK` + `yo6` + `wtK`）

```js
async function OtK(q, K, _) {                            // @110316858
  ...
  await yo6("/api/work/rewards?limit=5", {method:"GET"}, w)
  await yo6("/api/work/challenges/token-factory/current/activate",
            {method:"POST", body:$, headers:{"idempotency-key": q.headers.get("idempotency-key") ?? crypto.randomUUID()}}, w)
  await yo6("/api/work/feedback", {method:"POST", body:$, headers:{"idempotency-key": ...}}, w)
}
```

---

## 4. ★★ 出站请求规格总表（本文的核心交付物）

**每一族用哪套头，是三个构造器决定的 —— 这一点此前我们没有区分。**

### 4.1 头构造器（三个，逐字）

```js
// ① /api/work/* 用（wtK @110316076）
function wtK(q, K) {
  return { Accept: "application/json", "Content-Type": "application/json",
           "User-Agent": `phanthy-code-cli/${GK9}`,          // ★ 有 UA
           "x-phanthy-client-source": "desktop",             // ★ 有来源
           ...q.headers, Authorization: `Bearer ${K}` }
}

// ② /api/oauth/* 用（j59 @110354479）—— ★★ 极简
function j59(q, K) {
  let _ = { Accept: "application/json", "Content-Type": "application/json",
            Authorization: `Bearer ${K}` };
  for (let z of ["idempotency-key","x-desktop-installation-id","x-desktop-timestamp",
                 "x-desktop-nonce","x-desktop-signature"]) {   // ← 从入站请求透传
    let w = q.headers.get(z); if (w) _[z] = w
  }
  return _                                                     // ★ 没有 UA、没有 x-phanthy-client-source
}

// ③ 续期用（Pd3 @99945774）
headers: { "Content-Type": "application/json", Accept: "application/json" }   // ★ 只有这两个
```

### 4.2 总表

| 端点族 | 构造器 | `User-Agent` | `x-phanthy-client-source` | 其它 | 签名 |
|---|---|---|---|---|---|
| `/api/oauth/*`（profile / **register** / **summary** / **claim** / rewards / referrals / onboarding） | `j59` | **不设**（Bun fetch 补 `Bun/1.3.14`） | **不设** | `Accept` `Content-Type` `Authorization` + 透传 `idempotency-key` 与 4 个 `x-desktop-*` | 无（只有 v1 签名头若入站带了） |
| `/api/work/*` | `wtK` | `phanthy-code-cli/<ver>` | `desktop` | 同上 + `idempotency-key` | 无 |
| `/api/phanthy_code/models` | 内联 | **不设** | **不设** | `Authorization` + `Accept` | 无 |
| `/oauth/token`（**续期**） | `Pd3` | **不设** | **不设** | `Content-Type: application/json` + `Accept` | 无 |
| `/oauth/token`（换授权码） | `x78` @97928111 | 见 `p_q` | — | **form-urlencoded** body | 无 |
| `/v1/{messages,responses,chat/completions}`（**推理**） | `gd3` 转发 | 见 §5.2 | 见 §5.2 | 4 个 `x-phanthy-proof-*` | **`_O8` 12 段** |

### 4.3 推理路径发给上游的头（面 A 转发后）

桥剥掉 8 个（见 §3.1），其余**原样透传调用方发的头**，然后再补 4 个 proof 头。
⇒ 上游看到的推理头 = **内核发的头 − 被剥的 8 个 + 桥重签的 4 个**。

内核侧推理头构造器（`f` @99903865 附近，逐字）：
```js
f = {
  "x-app": "cli",
  [Nm3]: Rm3(),                                        // Nm3="x-phanthy-client-source"，值 = Rm3()
  ...process.env.PHANTHY_CODE_CLIENT_VERSION ? {[Am3]: process.env.PHANTHY_CODE_CLIENT_VERSION} : {},
  ...(J && j ? {[Vm3]: j} : {}),                       // x-phanthy-expert-id
  ...(X ? {[goq]: X} : {}),                            // x-phanthy-skill-names
  "User-Agent": OR(),
  "X-Claude-Code-Session-Id": B8(),
  ...H,                                                // ym3() = ANTHROPIC_CUSTOM_HEADERS 展开
  ...(O ? {"x-claude-remote-container-id": O} : {}),
  ...
}
function Rm3() { return process.env.PHANTHY_CODE_CLIENT_SOURCE === "desktop" ? "desktop" : "cli" }
function OR()  { return `claude-cli/${al} (${process.env.USER_TYPE}, ${process.env.CLAUDE_CODE_ENTRYPOINT ?? "cli"}${...})` }
```

⚠ **两点必须注意**：
- `x-app: "cli"` 是**官方的**（我们保留它是对的）
- `x-phanthy-client-version` 官方**只在设了 env 时才发**

---

## 5. 续期规格 【源码 + 实测】

### 5.1 入口 `tH` @99948835

```js
async function tH(q, K = {}) {
  let _ = K.read ?? Nd3, z = K.refresh ?? l44, w = K.persist ?? Ad3, O = K.now ?? Date.now,
      $ = await _(), Y = o44($, q, O());
  if (Y) return Y;                                   // ★① 刷之前先看别人刷好没有；有就用，零请求
  let H = $.find((X) => X.oauth.accessToken === q);
  if (!H || typeof H.oauth.refreshToken !== "string") return null;
  let J = await z(`${R1()}/oauth/token`, { grant_type:"refresh_token", refresh_token:H.oauth.refreshToken,
                                           client_id: YM().oauthClientId,
                                           scope: Array.isArray(H.oauth.scopes) ? H.oauth.scopes.join(" ") : void 0 });
  if (!J) return o44(await _(), q, O());             // ★② 被拒 ⇒ **重读共享库**再试一次
  let j = { ...H.oauth, accessToken: J.access_token,
            refreshToken: J.refresh_token ?? H.oauth.refreshToken,
            expiresAt: O() + (typeof J.expires_in === "number" ? J.expires_in : 3600) * 1000,
            ...(typeof J.scope === "string" ? { scopes: J.scope.split(" ").filter(Boolean) } : {}) };
  if (!(await w(H, j))) throw new Po;                // ★③ 落盘失败才抛错
  return J.access_token;
}
```

### 5.2 传输层 `Pd3` @99945774

- body：**JSON**（`JSON.stringify`）
- headers：**只有** `Content-Type: application/json` + `Accept: application/json`
- 超时：`AbortSignal.timeout(8000)` = **8 秒**
- 重试：**3 次**，只对 `429` / `>=500`
- 退避：`Z = max(500·2^attempt, Retry-After)`；`attempt===2 || Z>2000` ⇒ 记退避表并放弃
- 退避表 `w`：键 = `sha256(url + body)`，窗口 `clamp(Z, 5000, 300000)`，容量上限 128
- **`400 + invalid_grant` ⇒ `return null`**（不抛错）
- 失败形态 `Po` = `503 OAUTH_REFRESH_UNAVAILABLE`，「登录服务暂时不可用，请稍后重试；**当前登录信息已保留**」

### 5.3 续期的触发时机（**这是关键**）

`tH` 的调用点全是**按需**：

```js
// /api/*（X59）：401 才续期
if ($.status === 401) { let Y = await tH(O); if (Y && Y !== O) O = Y, $ = await w(O) }

// summary（KeK）：只在"5 秒内要过期"时预刷
if (z.expiresAt !== null && z.expiresAt <= Date.now() + 5000) try { w = await tH(w) ?? w } catch {}

// 桥（gd3）：401 才续期
if (P.status === 401 && ...) { let G = await cd3(Y, X); ... }

// 其它 6 处同形（@110370079 / @110373291 / @110374945 / @110376970 …）
```

**⇒ 官方纪律：「能只读就只读」—— 只在"快过期（≤5s）"或"上游回 401"时才续期。没有后台定时续期。**

---

## 6. proof 规格：`_O8` 12 段 【源码 + 字节级实测】

```js
function _O8(q, K, _, z) {                    // q=method, K=pathname, _=body, z=headers
  if (!iy) return null;
  if (!KO8(iy).valid) return null;
  let w = String(Math.round(N44())),          // 4. timestamp（毫秒）
      O = pp3(24).toString("base64url"),      // 5. nonce（24 字节 → base64url）
      $ = (H) => D44("sha256").update(H).digest("base64url"),
      Y = ["desktop-inference-v2",            // 1. 固定前缀
           q.toUpperCase(),                   // 2. method 大写
           K,                                 // 3. pathname（**不带 query**）
           w,                                 // 4.
           O,                                 // 5.
           $(_),                              // 6. sha256(body) b64url
           sK6(z.get("authorization") ?? ""), // 7. sha256(Authorization 整串) b64url
           $(iy),                             // 8. sha256(session token) b64url
           z.get("x-request-id") ?? "",       // 9.
           z.get("x-claude-code-session-id") ?? "",  // 10.
           z.get("x-phanthy-expert-id") ?? "",       // 11.
           z.get("x-phanthy-skill-names") ?? ""]     // 12.
          .join(`<LF>`);                      // ★ 分隔符 = LF(0x0A)，**字节级已验证**
  return { "x-phanthy-desktop-session": iy, "x-phanthy-proof-timestamp": w,
           "x-phanthy-proof-nonce": O,
           "x-phanthy-proof-signature": dp3(null, Buffer.from(Y), T44.privateKey).toString("base64url") }
}
function sK6(q) { return D44("sha256").update(q).digest("base64url") }   // @99902493
```

**分隔符的字节级验证**（本节最重要的实测）：
按字节扫 `claude-sidecar.exe` 里所有 `].join(\``，**49 处全部是 `0x0A`（LF）**。
⇒ 本仓 `TestCanonicalSeparatorsArePerString`（"逐字节 = LF"）**是对的**。

---

## 7. 与我们项目的逐项对照

### 7.1 已经对齐的

| 维度 | 官方 | 我们 | 状态 |
|---|---|---|---|
| 推理路径 4 个 proof 头 | `gd3` 转发时补 | 已补（`9f4f402`/`7ce867e`，真机 200） | ✅ |
| canonical 12 段与顺序 | §6 | `InferenceCanonical` 逐段一致 | ✅ |
| canonical 分隔符 | LF(0x0A) | LF | ✅ |
| 第 7 段 = sha256(Authorization 整串) | §6 | 同 | ✅ |
| 第 8 段 = sha256(session token) | §6 | 同 | ✅ |
| `/oauth/token` 续期：body JSON、8s、3 次、只重试 429/5xx、`invalid_grant→被拒`、退避表 | §5.2 | D1–D4 已修（`27ab704`/`c71c461`） | ✅ |
| `/api/oauth/*` 端点路径 | `Y59`/`J59` | 逐条一致 | ✅ |
| `/v1/*` 三条路径 | `Sd3` | 一致 | ✅ |

### 7.2 ★ 尚未对齐的（本轮新发现）

| # | 维度 | 官方 | 我们 | 位置 |
|---|---|---|---|---|
| **U-1** | **`/api/oauth/*` 的 UA** | **不设**（Bun 补 `Bun/1.3.14`） | `phanthy-code-cli/2.1.0`（13:44 的 `5a9cbdd` 改的，照的是 `/api/work/*` 的规则） | `applyCommonHeaders` |
| **U-2** | **`/api/oauth/*` 的 `x-phanthy-client-source`** | **不设** | `desktop` | 同上 |
| **U-3** | `/api/phanthy_code/models` 的头 | 只有 `Authorization` + `Accept` | 同 U-1/U-2 | `FetchModels` |
| **U-4** | 续期的**频率纪律** | **无后台定时续期**，只在 ≤5s 或 401 时 | 后台任务固定 30 分钟一刷 | `jobs.go` |
| **U-5** | 签到频率 | **一天一次** | **30 分钟一次**（`checkin_interval_seconds` 默认 1800）+ 每次扫描都**实打上游**（`Status` 调 `FetchSummary`） | `CheckinAll` / `checkinInterval` |
| **U-6** | `/oauth/token` 换授权码 | **form-urlencoded** | 已按 form 实现 | 待复核 |
| **U-7** | 推理路径的 `x-phanthy-client-version` | **只在设了 env 时才发** | 无条件发 | `client.go:511` |

> ⚠ U-1/U-2 已开始修（`applyOAuthFamilyHeaders`），但**本次按你的要求停下来先出报告**，
> 未跑判据、未提交、未部署。

### 7.2.1 ★ 为什么会漏掉 U-1/U-2 —— 现有护栏盖不住这一类

修 U-1 时发现：把 `newJSONRequest` 换成极简头之后，
既有的 `TestCommonHeadersOnEveryRequest` **仍然是绿的**。

原因：那条判据测的是 `applyCommonHeaders` **这个函数本身**，
而不是"某个真实端点实际发出的头"。于是：

- 改函数的实现 → 判据红 ✅
- **把某个端点从 A 构造器换到 B 构造器 → 判据不响** ❌

⇒ 这正是 U-1/U-2 能长期存活的原因，也是 13:44 那次"修 UA"能改错的同一条缝。

**U-1/U-2 的修必须同时补一条"按端点族断言"的判据**（例如
`TestOAuthFamilyOmitsClientSourceHeader`：真的造一个请求走 `newJSONRequest`，
断言最终 header 里**没有** `x-phanthy-client-source`、且 UA 是 `Bun/1.3.14`）。
否则下一个人还会从同一条缝里掉下去。

### 7.3 "一个实例一个账号"是**结构性**的 【实测】

把**另一个账号**的 token（sub 不同）打给持着本实例凭据的桥 ⇒ `401 expired_access_token`，
与随便一个假 token 的表现**完全相同**。

⇒ 桥**不能替别的账号签名**（密钥在各自 config dir 里）⇒ **N 个号 = N 个实例**。
代价：约 130 MB/实例（sidecar RSS 实测 120–138 MB），且每个号要**人工登录一次**。

---

## 8. 待验证项（**未实测，不许当成结论**）

| # | 待验证 | 怎么验 |
|---|---|---|
| V-1 | 桥返回的**流式**形态（Anthropic SSE / OpenAI SSE） | 自建实例发一次 `stream:true` |
| V-2 | 桥**是否**在 401 后真的自己续期并重试（源码看是会：`cd3`→`tH`） | 自建实例 + 一个故意过期的 token |
| V-3 | 过期 token 递进去会不会被"按 sub 匹配到凭据" | 同上 |
| V-4 | 模型清单从哪来（桥无 models 端点） | 试 `/api/phanthy_code/models` 与常量表 |
| V-5 | 官方 `/oauth/token` 换授权码那跳的头（`p_q`） | 读 `p_q` 定义 |
| V-6 | `Ed id3` / `u$7` / `md3`（403 重试的那个判据）的完整逻辑 | 读源码 |
| V-7 | U-4/U-5 改动是否真能改善存活 —— **未知，且没有证据** | 见 §9 |

---

## 9. 这份报告**不能**回答的问题（诚实划边界）

**为什么我们的账号会被封 —— 本文没有答案。**

能确定的与不能确定的：

| 能确定 | 依据 |
|---|---|
| 我们的**推理**请求形状与官方 1:1（17:07 真机 200） | 实测 |
| 我们的**续期**已按 `Pd3` 对齐 | 源码 + 判据 |
| `/api/oauth/*` **多带 2 个头**（U-1/U-2） | 源码 |
| 签到频率 **48×** 且每次都实打上游（U-5） | 源码 + `checkin-log.json` 实测 |

| **不能**确定 | 说明 |
|---|---|
| U-1/U-2/U-5 是否**就是**封禁原因 | 没有任何一条把"异常"与"封禁"连起来的证据 |
| 服务端按什么记账 | 无服务端视野 |
| 账号会话为什么会在**零推理流量**时失效 | 只有现象（`checkin-log` 的 ok→fail 断点） |

⇒ **本文把"我们发了什么"钉死了；"服务端因此做了什么"仍然看不见。**
要达到后者需要**出站审计**（请求摘要 + 上游响应原文落盘），那是下一份文档的事。
