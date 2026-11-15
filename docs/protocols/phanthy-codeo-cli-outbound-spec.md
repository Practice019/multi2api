# 官方终端 CLI `@phanthy/phanthy-codeo` 出站规格

> 日期：2026-11-15
> 真源：**本机已安装的 npm 包** `D:\npm-global\node_modules\@phanthy\phanthy-codeo`
> 版本：**0.2.13**（`package.json` 逐字）· 许可 MIT · `dist/cli.js` 20.9 MB **未压缩可读**
> 方法：直接读源码 + 正则定位，**不需要反编译、不需要抓包**

---

## 0. ⚠ 先分清三样东西（又是三个不同的东西）

| | **Windows 客户端** | **官方终端 CLI**（本文） | **第三方 launcher** |
|---|---|---|---|
| 名字 | `PhanthyWork.exe` + `claude-sidecar.exe` | `@phanthy/phanthy-codeo` → 命令 `phanthy-codeo` | `phanthy-code-launcher` → 命令 `pcl` |
| 来源 | **官方** | **官方** | 第三方（`spalxyz13`，MIT） |
| 形态 | Tauri + Bun 单文件 108 MB | **Node 包，源码可读** | Node CLI 42 KB |
| 与上游的关系 | 内核 → **本地桥 4421** → 上游 | **直接** → 上游 | 本地代理 → 上游 |
| 签名 | **桥补 12 段 ed25519 + 4 个 proof 头** | **没有** | 没有 |
| 设备注册 | 有 | **没有** | 没有 |

**本机装的是中间那个**（用户说"本地我已经安装了 CLI 版本"）。

**⇒ 它的意义：这是一个官方出品、源码可读、且直连上游的客户端。**
它发的每一个头，就是"官方认可的出站形态"的一个**活样本**。

---

## 1. 端点表（源码逐字）

```js
aiA = {
  authorize:  "https://code.phanthy.com/oauth/authorize",
  token:      "https://code.phanthy.com/oauth/token",
  profile:    "https://code.phanthy.com/api/oauth/profile",
  models:     "https://code.phanthy.com/api/phanthy_code/models",
  modelStats: "https://code.phanthy.com/api/phanthy_code/models/stats",   // ★ 我们不知道这个
  messages:   "https://code.phanthy.com/v1/messages"
}
```

同处还定义了：

```js
Q_A = "https://code.phanthy.com"
B26 = "phanthy-code-cli"          // client_id
fB  = "0.2.13"                    // 版本
AG  = { name: "phanthy-codeo", ... }
dF1 = ["org:create_api_key","user:profile","user:inference",
       "user:sessions:claude_code","user:mcp_servers","user:file_upload"]   // scope
```

**与我们一致**：`client_id` / scope / 主要端点。
**新增**：`/api/phanthy_code/models/stats`（模型统计，我们没实现）。

---

## 2. ★★ 核心结论：**官方 CLI 不签名、不注册、不换推理令牌**

对 20.9 MB 的 `dist/cli.js` 做全文计数（区分大小写）：

| 检索词 | 次数 | 含义 |
|---|---|---|
| `x-phanthy-proof` | **0** | 没有 4 个证明头 |
| `x-phanthy-desktop-session` | **0** | 同上 |
| `desktop-installations` | **0** | **不注册设备** |
| `inference_token` / `inferenceToken` | **0** / **0** | **没有 180 秒推理令牌** |
| `activities/summary` | **0** | 不查额度摘要 |
| `ed25519` | 1 | 只出现在 `sshpk` 库的密钥解析里，**与请求签名无关** |

**⇒ 我们仓库里那套"设备注册 + 推理令牌 + 12 段签名 + 4 个 proof 头"
只存在于 Windows 客户端 → sidecar 桥 这一跳，官方终端 CLI 完全没有。**

**这是目前最硬的一条证据**：不是推断、不是第三方实现，是**官方自己的 CLI 源码**。

---

## 3. 它到底发什么头（两处来源，逐字）

### 3.1 请求包装器

```js
let u = new Headers(F?.headers);
u.delete("x-api-key");
u.set("Authorization", `Bearer ${B.value}`);
u.set(ok.product, "phanthy_code");        // → X-Phanthy-Product: phanthy_code
u.set(ok.clientSource, "cli");            // → X-Phanthy-Client-Source: cli
u.set("User-Agent", `${AG.name}/${fB}`);  // → phanthy-codeo/0.2.13
let G = u.get(ok.request), q = crypto.randomUUID();
if (G) u.set(ok.request, q), wuA.record(G, new Headers, q);   // 重写 x-phanthy-request
```

### 3.2 推理头（`LLMRequestPrep.prepare`）

```js
headers: {
  ...(A.model.providerID === "phanthy" ? {
    ...($ ? { [ok.project]: $ } : {}),      // x-phanthy-project
    [ok.session]: A.sessionID,              // x-phanthy-session
    [ok.request]: A.user.id,                // x-phanthy-request
    [ok.client]:  A.flags.client,           // x-phanthy-client（harness 名）
    ...hsA.headers(),                       // = { [ok.device]: <b64url 设备指纹> }
    "User-Agent": wI4                       // = phanthy-codeo/0.2.13
  } : { ... })
}
```

### 3.3 头名表（它自己源码里的常量）

```js
ok = {
  client:       "x-phanthy-client",
  clientSource: "X-Phanthy-Client-Source",
  device:       "x-phanthy-cli-device",
  product:      "X-Phanthy-Product",
  project:      "x-phanthy-project",
  request:      "x-phanthy-request",
  session:      "x-phanthy-session"
}
```

### 3.4 设备指纹（与我们、与 PCL 三者同形）

```js
function ab7() {
  return { version:1, collectedAt:new Date().toISOString(), cliVersion:fB,
           hostname, username, platform, arch, osRelease,
           ipCandidates, networkInterfaces, routes, defaultGateway }
}
function ob7() { return Buffer.from(JSON.stringify(ab7()), "utf8").toString("base64url") }
function fC9() { return { [ok.device]: ob7() } }
```

上限：接口 32 个、路由 32 条、候选 256 个（源码里的 `YC9=32, QC9=32, WC9=256`）。

---

## 4. 🔴 这条推翻了本仓两处旧结论

| 旧结论（照 **sidecar 桥** 判断） | 实际（照 **内核源码**） |
|---|---|
| 「官方**从不发** `x-phanthy-product`」（我们因此把它删了，还留了负断言） | **发**。`profile` / `models` / 包装器三处都发，值 `phanthy_code` |
| 「`x-phanthy-client-source` 应为 `desktop`」 | **终端 CLI 发的是 `cli`** |

**错的根因**：我们一直在读**桥**，而真正发请求的是**内核**。
桥只是 Windows 那台的分发器；内核（这个 npm 包）才是客户端本体。

**旁证**：第三方 launcher（PCL）也发 `X-Phanthy-Product: phanthy_code` ——
两个独立实现互相印证，说明这个头是**官方认可的**，不是我们想当然加出来的。

---

## 5. 🔴 第二条：推理路径是**按模型选的**，不是固定 chat

```js
q = j.supported_endpoints?.includes("/v1/messages"),
D = q                              ? "messages"
  : j.supported_endpoints?.includes("/responses")      ? "responses"
  : j.supported_endpoints?.includes("/chat/completions") ? "chat" : void 0
```

全文计数也印证：`/v1/messages` **16 次**、`/v1/chat/completions` **0 次**。

**⇒ 我们仓库里写着「新版 Phanthy Codeo（0.2.13）抓包实证从不使用 `/v1/messages`
（全程只发 `/v1/chat/completions`）」—— 与实际源码相反。**

真实情况：**由模型的 `supported_endpoints` 决定**。当前目录里的模型大多声明
`/v1/messages`，所以内核走 messages。

**这有实际影响**：如果我们走 `/v1/chat/completions` 而官方走 `/v1/messages`，
那"路径本身"就是一个可区分的特征。**但我们不能据此断定它会被封** ——
账号 #4 零推理也被封，说明推理路径不是必要条件。

---

## 6. 对我们项目的三条可执行结论

### 6.1 已落地：新增 `RequestProfile = kernel`（默认）

`internal/phanthywork/requestprofile.go`：

```
形态          设备注册 推理令牌 签名 设备指纹 额外身份头            说明
signature       ✅     ✅    ✅    ✅     官方 Windows 桥那套     保留以便回退
kernel(默认)    ❌     ❌    ❌    ✅     product + source=cli   ★ 本文的形态
pcl             ❌     ❌    ❌    ✅     launcher 三头           第三方 launcher
relay           ❌     ❌    ❌    ✅     —                      最小可用
bare            ❌     ❌    ❌    ❌     —                      只 Bearer
```

`kernel` 形态实际发送：

```
Authorization: Bearer <access_token>
Content-Type: application/json
Accept: text/event-stream
User-Agent: phanthy-codeo/0.2.13
X-Phanthy-Product: phanthy_code
X-Phanthy-Client-Source: cli
x-phanthy-session: ses_…
x-phanthy-request: <uuid>
x-phanthy-client: phanthy-codeo
x-phanthy-cli-device: <b64url 设备指纹>
```

**没有** `x-phanthy-proof-*`、**没有** `x-phanthy-desktop-session`、
**没有** `x-app`、**没有** `x-phanthy-client-version`。

### 6.2 待决定：我们的 `protocol` 默认要不要改成 `messages`

现在我们的默认是 `chat`（`/v1/chat/completions`），而官方 CLI 走 `messages`。
改成 messages 需要把 Anthropic→OpenAI 的翻译接回 relay 链路
（目前那条翻译挂在带签名的 `chatMessages` 上）。

**建议先不动**：账号 #4 零推理也被封 ⇒ 推理路径不是封禁的必要条件，
改它收益不明；而改动面不小。

### 6.3 待补：`/api/phanthy_code/models/stats`

官方 CLI 会拉这个端点，我们从没碰过。用途未知，值得单独探一次（无鉴权可探存在性）。

---

## 7. 这份文档**不能**回答的

| # | 问题 | 为什么 |
|---|---|---|
| 1 | 上游**要不要**签名 | 只能说"官方 CLI 不发签名且能工作（按它自己的设计）" —— **但我们没有一次实测证明上游接受无签名的请求** |
| 2 | 账号为什么被封 | 与本文无关；仍缺服务端视野 |
| 3 | `x-phanthy-product` / `client-source` 有没有风控含义 | 完全未知 |

**⇒ 下一步仍是那五个实测（V1–V5），只是现在有了更好的形态定义（`kernel`）。**
清单见 `pcl-phanthy-code-launcher-spec.md` §11，
形态用 `phanthywork.request_profile = kernel`（默认值，无需配置）。
