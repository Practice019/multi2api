# 科搭 Codar 逆向分析报告

- **任务**：T-306 —— 拆解 `D:\software\Codar-Setup-latest\codar-desktop` 的 `app.asar`，探明 API 协议，为接入 workbuddy2api 作准备
- **样本**：`app.asar` 36,822,645 字节（913 个文件）+ `resources/bin/codar.exe` 68.5MB
- **方式**：**纯静态**（自写 asar 解包器 + 关键词扫 + 精读关键文件）+ **未登录端点探活** + **只读**看本地配置
- **日期**：2026-11-08
- **结论速览**：★ **可行** —— 云端是 **OpenAI 兼容**协议（`openai_chat_completions` + SSE）、模型目录**公开无需鉴权**、凭据是**一枚 JWT**（存在 `~/.codar/config.json`）

> 全文区分 **【实证】**（代码/请求原文）与 **【推测】**（据证据推断）。**所有 token 只给长度与前缀，不给值。**

---

## 0. 结论摘要

| 项目 | 结论 | 依据 |
|---|---|---|
| **API 基址（云端）** | ★ `https://api.claw.speedai.com`（可用环境变量 `CODAR_SPEEDAI_API_BASE` 覆写） | 【实证】`main.cjs` 常量 |
| **账号/鉴权基址** | `https://api.kuaipaper.com`（dev: `https://test.api.kuaipaper.com`；env `CODAR_AUTH_API_BASE`） | 【实证】同上 |
| **Web 站** | `https://codar-web.speedai.com`（另有 `codar.speedai.com` 在包内出现 9 次） | 【实证】 |
| **认证方式** | ★ **`Authorization: Bearer <JWT>`**；JWT 由登录流程得到，落在 `~/.codar/config.json` 的 `provider.speedai.token` | 【实证】`index-Cr4tsyUc.js` + config.json |
| **聊天协议** | ★ **`openai_chat_completions`**（模型目录里 `protocol.type` 明写）+ **SSE 流式** | 【实证】`/v1/codar/models` |
| **模型目录** | ★ `GET /v1/codar/models` —— **无需鉴权**，HTTP 200 / 106,465 字节 / **15 个模型** | 【实证】实打 |
| **是否有本地服务** | ★ **有**：Go sidecar `codar.exe`，`--host 127.0.0.1 --port <随机> --ready-file <文件>`；另有 browser relay `127.0.0.1:8765` | 【实证】`main.cjs` + config.json |
| **能否做 2api 上游** | ★ **能**（协议 OpenAI 兼容），但需实测确认具体聊天路径与额度口径 | 见 §6 |
| **产品形态** | Electron 37.x（`Codar科搭.exe`）+ Vite 渲染层 + **Go sidecar** + 独立 `computer-use.exe` | 【实证】 |

---

## 1. 应用结构

### 1.1 asar 解包（自写解包器，零依赖）

asar 头部实测：`4 / 239668 / 239664 / jsonSize=239659`。
★ **注意**：payload 起点 = **`8 + 头部第 2 个 uint32`**（= 239676），**不是** `16 + jsonSize`（差 1 字节就会全部错位）。
⇒ 自校验通过：解出的 `package.json` 以 `{` 开头、含 `"name":"codar-desktop"` ✓

```
name/version/main : codar-desktop  0.3.12  electron/main.cjs     （★ 包版本是 0.3.12，不是外壳的 37.10.3）
dependencies      : electron-updater, yauzl, yazl                （极少 ⇒ 业务全在 renderer/ 与 Go sidecar）
build.appId       : 无     build.protocols: null     build.publish: null
```

解包结果：**913 个文件 / 36.6MB**

| 顶层 | 体积 | 说明 |
|---|---|---|
| `renderer/` | 34.3MB | Vite 打包的 Web 应用（含 `assets/*.js`） |
| `node_modules/` | 2.0MB | 只有 electron-updater / yauzl / yazl |
| `electron/` | 0.3MB | **主进程**（32 个 `.cjs`） |
| `molecule-viewer/` | ~0 | 分子预览 |

### 1.2 关键文件（主进程 `electron/`）

| 文件 | 大小 | 作用 |
|---|---|---|
| `main.cjs` | 83KB | ★ 入口：API 基址常量、**云端路径白名单**、sidecar 启动、IPC |
| `cloud-network.cjs` | 4KB | ★ 云连接/代理配置（把 `PUT /api/desktop/cloud-network` 发给本地 sidecar） |
| `auth-request.cjs` | 4KB | 带超时的鉴权请求封装（1000–60000ms 夹取） |
| `chat-attachment-upload.cjs` | 5KB | 附件上传（**必须**打到 `http://127.0.0.1` 的 sidecar） |
| `sidecar-ready.cjs` | 2KB | 校验 sidecar 就绪信息（协议必须 `http:`、hostname 必须 `127.0.0.1`、必须有 port 且无 userinfo） |
| `in-app-browser-manager.cjs` | 53KB | 内置浏览器 |
| `updater.cjs` | 28KB | 更新（`https://img.kuaipaper.com/codar/desktop`） |
| `workspace-file-transfer.cjs` | 14KB | 工作区文件传输 |

### 1.3 asar 外（★ 聊天逻辑的所在）

```
resources/app.asar                36.8MB   本分析对象
resources/bin/codar.exe           68.5MB ★ 本地 Go sidecar（含 Agent/聊天/工具实现）
resources/computer-use/codar-computer-use.exe  2.65MB
resources/elevate.exe             118KB
resources/localization/uiMessages.json 1.26MB
resources/{runtimes,browser-connector}/README.md（占位）
```
⇒ 【实证】asar 里只有**桌面壳 + 渲染层**；真正的 **Agent 循环、LLM 调用、工具执行在 `codar.exe`（Go）里** ✗ —— 这也解释了为什么 asar 里搜不到 `chat/completions` ✓

---

## 2. API 端点（★ 核心）

必须把两层分开：**云端（真正的上游）** 与 **本地 sidecar**。

### 2.1 架构总览

```
Renderer(Vite SPA)
   │  所有本地调用都带: X-Codar-API-Token: <sidecar token>、X-Codar-Request-ID
   ▼
本地 Go sidecar  http://127.0.0.1:<随机端口>            ← 由 main.cjs spawn
   │  ├─ 自己直连云端（Bearer JWT，provider.api_type=openai）
   │  └─ 也替渲染层做“云端中转”
   ▼
云端:  https://api.claw.speedai.com     （target="speedai"）
       https://api.kuaipaper.com        （target="account"）
```

### 2.2 ★ 云端端点（主进程白名单 = 权威清单，实证）

`main.cjs` 里用 **白名单** 决定渲染层能中转哪些云端路径：

```js
isAllowedSpeedAIPath = request.target === "speedai" && new Set([
  "/api/user/self",
  "/api/log/self/stat",
  "/v1/codar/usage/daily",
  "/v1/codar/models",
  "/api/pricing",
  "/api/codar/announcements",
  "/api/codar/announcements/version",
  "/api/codar/announcements/dismiss",
  "/api/codar/activities/catalog",
  "/api/codar/activities/mine",
  "/api/codar/activities/free-model",
  "/api/codar/activities/free-model/claim",
  "/api/codar/activities/invite-model",
  "/api/codar/activities/invite-model/claim",
]).has(speedAIPathname)

isAllowedAccountPath = request.target !== "speedai" && pathValue.startsWith("/v1/")
```
（`api.claw.speedai.com` → target `"speedai"`；其余官方域名 → target `"account"` ✓）

### 2.3 ★ 实测的云端行为（未带任何凭据）

| 请求 | 响应 | 含义 |
|---|---|---|
| `GET https://api.claw.speedai.com/v1/codar/models` | **200**，106,465 字节 | ★★★ **模型目录公开**（详见 §5） |
| `GET https://api.claw.speedai.com/api/user/self` | **401** `{"detail":"Not authenticated"}` | FastAPI 风格；账号信息需鉴权 |
| `GET https://api.claw.speedai.com/api/speedai/model-options` | **404** `{"detail":"Not Found"}` | 该路径属**本地 sidecar**，不是云端 ⇒ 印证 §2.1 分层 |
| `GET https://api.kuaipaper.com/v1/` | **200** `{"code":401,"message":"No authorization header"}` | 账号网关（`{code,message}` = one-api 风格） |
| `GET https://codar-web.speedai.com/` | 200，17,488 字节 HTML | Web 版前端（`<meta name="codar-runtime-mode" content="web">`） |

### 2.4 本地 sidecar 端点（渲染层实测用的，≈150 条中的代表）

```
# 桌面/运行时
GET  /api/status                GET  /api/backend              GET/POST /api/desktop/settings
GET  /api/desktop/runtime       POST /api/desktop/runtime/ensure     POST /api/desktop/runtime/probe
GET  /api/desktop/agents        GET  /api/desktop/provider     POST /api/desktop/provider/speedai
GET  /api/desktop/provider/key${query}
GET  /api/model-catalog         POST /api/model-selection/resolve
PUT  /api/desktop/cloud-network           ← cloud-network.cjs 用它下发代理配置
POST /api/desktop/cloud-auth-request      ← ★ 渲染层请 sidecar 代打云端的唯一入口

# 对话/会话（★ 注意：这些是本地路径，真正的 LLM 调用在 sidecar 内部）
POST /api/chat      GET /api/chat/events     POST /api/chat/runs
POST /api/chat/interrupt   POST /api/chat/steer   POST /api/chat/ask-user/response
GET  /api/sessions  GET /api/sessions/catalog-version   GET  /api/projects

# 账号/鉴权（本地侧）
GET  /api/web/me    POST /api/web/auth/request    POST /api/web/auth/speedai
GET  /api/web/account   POST /api/web/logout

# 活动/额度（本地侧也有同名镜像）
/api/codar/activities/{catalog,mine,free-model,free-model/claim,invite-model,invite-model/claim}
/api/codar/announcements{,/version,/dismiss}     /api/pricing    /api/user/self

# 工具/集成
/api/connectors{/dingtalk,/feishu,/wechat,/wechat/qrlogin,/wechat/qrstatus,/browser/enable|probe}
/api/mcp/servers   /api/skill-market/**（v4 一整套）   /api/computer-use/{enable,disable,status,permissions}
/api/cloud-computer/{sessions,request,open,session-events,catalog-version}
/api/ssh/{provider,runtime,compatibility,agent-profile}   /api/remote/{devices,settings,submissions,status}
/api/schedules{/…}   /api/tool-approval{/batch}   /api/events/{ticket,channel}
/api/third-party/{deepseek,official}/models       /api/speedai/model-options
/api/cloud/settings/mcp{,/probe}   /api/cloud/remote/devices
```
（完整 149 条路径可由渲染层 bundle 复现：`renderer/assets/index-Cr4tsyUc.js` 120 条 + `index-Cmw49_G8.js` 29 条）

---

## 3. 认证机制

### 3.1 三道凭据（实证）

| # | 凭据 | 形态 | 存放 | 用在哪 |
|---|---|---|---|---|
| 1 | ★ **用户 JWT** | `eyJhbG…`，**207 字符**（`alg` 形态 = JWT） | `~/.codar/config.json` → `provider.speedai.token` | 云端 `Authorization: Bearer <jwt>` |
| 2 | **sidecar token** | 短串（由 sidecar 就绪文件给） | 运行期，`ready.token` | 所有本地调用：`X-Codar-API-Token` |
| 3 | **临时 token** | `input.tempToken` | 运行期 | `Authorization: Bearer ${input.tempToken}`（特定流程） |

### 3.2 请求头（渲染层 bundle 里的字面量，实证）

```
X-Codar-API-Token          ← 本地 sidecar 鉴权
X-Codar-Request-ID
X-Codar-Account             X-Codar-Account-Username
X-Codar-Chat-Ack
Authorization: Bearer ${session.token}
Authorization: Bearer ${input.tempToken}
```

### 3.3 云端中转协议（实证，`cloud-network.cjs`）

渲染层**不直连云端**，而是：

```js
POST ${sidecar.url}/api/desktop/cloud-auth-request
headers: { "Content-Type": "application/json", "X-Codar-API-Token": ready.token }
body: {
  target: hostname === "api.claw.speedai.com" ? "speedai" : "account",
  path: parsed.pathname + parsed.search,
  method: init.method || "POST",
  authorization: new Headers(init.headers).get("Authorization") || "",
  body: <原 body 的 JSON>
}
```
⇒ 【实证】云端调用的**最终发起方是 sidecar**，`Authorization` 由渲染层透传 ⇒ **鉴权就是标准的 Bearer** ✓
⇒ 官方域名白名单：`["codar-web.speedai.com","api.claw.speedai.com","api.kuaipaper.com","test.api.kuaipaper.com"]` 且必须 `https:` + 无用户信息 + **无端口** ✓

### 3.4 本地配置全文键结构（★ 值已脱敏）

文件：`C:\Users\21877\.codar\config.json`（2,517 字节，**只读**）

```
version = 1
provider.auth_type            = speedai                      ← ★ 鉴权方式是 speedai 自有
provider.active_profile       = speedai
provider.speedai.token        = eyJhbG…(len=207)             ← ★ 用户 JWT
provider.speedai.username     = user_1508****…_7985        ← 用户名形态：user_<手机号>_<4位>
provider.speedai.login_at     = 1794070329644
provider.api_base             = https:…(len=31)              ← ★ 长度 31 = "https://api.claw.speedai.com/v1"
provider.api_type             = openai                       ← ★★ OpenAI 兼容
provider.model                = auto
provider.provider_profiles.speedai.* （同上，profile 副本）
agent.profile                 = general       agent.context_manager = codar（options 12 项）
desktop.default_model         = gpt-6-astra   desktop.default_reasoning_effort = high
desktop.permission_mode       = confirm
desktop.permissions.*         = browser/clipboard/desktop/downloads/mcp/shell = false；files/filesystem/network = true
desktop.speedskills_api_base  = https:…(len=25)   speedskills_web_url = https:…(len=25)   ← 25 = api.kuaipaper.com
desktop.remote_control.relay_url = https:…(len=25)   api_base_path = /v1/codar/remote
desktop.browser_connector.relay_host = 127.0.0.1   relay_port = 8765
memory.enabled = False
```
⇒ 【实证】**`provider.api_type = openai` + `api_base` 长度 31** ⇒ 与 §0 的判断互证：**上游就是 OpenAI 兼容的 `https://api.claw.speedai.com/v1`** ✓（长度 31 与该串完全吻合 ✓）
⇒ 【推测】`provider.api_base` 允许用户改成任意 OpenAI 兼容端点（"第三方 provider"能力），所以它才写成可配置项 ✓

---

## 4. 端口 / 本地服务

| 服务 | 端口 | 依据 |
|---|---|---|
| ★ **Go sidecar** | **随机端口**（`--port portArg`，由主进程挑空端口） | 【实证】`main.cjs`：`spawn(codar.exe, ["desktop","--host","127.0.0.1","--port",portArg,"--ready-file",readyFile])` |
| 开发态 sidecar | 同参数，但 `go run ./cmd/codar desktop --host 127.0.0.1 --port …` | 【实证】`main.cjs` 兜底分支 |
| **browser connector relay** | **127.0.0.1:8765** | 【实证】config.json |
| 主进程内的本地 HTTP | `electron-updater` 的 MacUpdater 会在 `127.0.0.1:0` 起临时服务（仅 macOS 更新用） | 【实证】`node_modules/electron-updater/out/MacUpdater.js` |
| 渲染层本地静态资源 | 打包态走 `file://`（`renderer/index.html`） | 【推测】 |

⚠️ **没有固定端口** ✗ ⇒ 想抓包/联调必须读 sidecar 就绪文件里的 `url` ✓（`main.cjs` 用 `--ready-file` 传 ✓）
✅ sidecar 就绪校验很严：必须 `http:` + hostname `127.0.0.1` + 有 port + 无 userinfo ✓（`sidecar-ready.cjs`）

---

## 5. 模型列表

★ **`GET https://api.claw.speedai.com/v1/codar/models` —— 无需鉴权**（实测 200 / 106,465 字节）

```json
{
  "object": "list",
  "schema_version": 2,
  "config_version": "8e4bfff9354983a10f83618025c60fdededb56bb899504324c290169202b30cf",
  "data": [ /* 15 条 */ ]
}
```

单条模型的结构（第一条，实证）：

| 字段 | 值/说明 |
|---|---|
| `id` / `display_name` / `sort_order` | 模型标识 / 展示名 / 排序 |
| `context_window` | 例：**1,000,000** |
| `input_modalities` / `output_modalities` | `[text, image]` / `[text]` |
| ★ `protocol` | **`{type: "openai_chat_completions", method: "POST", path: "/model"}`** |
| ★ `defaults` | `{path: "/model", mode: "fixed"}` |
| `request_schema` | JSON Schema（`type: object`, `allOf: [...]`） |
| `capabilities` | **`{streaming: true, stream_format: "sse"}`** |
| `reasoning` | `{levels: {high: {id: "off", label, …}}}` |
| `pricing` | `{currency: CNY, unit: per_million_tokens, input: …}` |
| `rate_badge` | 例：`"0.20x"`（倍率标签） |
| `config_version` / `updated_at` | 目录版本与时间戳 |

**15 个模型 id（实证）**：
```
auto
claude-opus-5-5            claude-opus-5-5-kiro
claude-sonnet-5-5          claude-sonnet-5-5-kiro
deepseek-v4.1-flash        deepseek-v4-pro        deepseek-v4-flash
gpt-5.6-sol                gpt-6-astra            gpt-6.1-sol        gpt-6-sol
glm-5.2                    glm-5.3                glm-5.3-flash
```

⇒ 【实证】目录里明写 **`openai_chat_completions` + `stream_format: sse`** ⇒ 上游协议就是 **OpenAI Chat Completions（SSE 流式）** ✓✓
⇒ 【未决】`protocol.path` 是 **`/model`** ✗ —— 结合 `api_base` 已含 `/v1`，聊天路径可能是 **`POST /v1/model`**（而非 `/v1/chat/completions`）✗ **必须实测确认**（见 §7）

---

## 6. 与 workbuddy2api 的对接方案

### 6.1 可行性：★ **高**
上游是 **OpenAI 兼容 + SSE** ✓、模型目录**公开** ✓、凭据是**一枚可存储的 JWT** ✓ ⇒ 与现有上游（codearts 等）的形态同构 ✓

### 6.2 需要实现的东西

| # | 项 | 内容 |
|---|---|---|
| 1 | **base URL** | `https://api.claw.speedai.com/v1`（env `CODAR_SPEEDAI_API_BASE` 可覆写 ✓ 便于联调） |
| 2 | **鉴权** | `Authorization: Bearer <JWT>`（JWT 来自 `~/.codar/config.json` 的 `provider.speedai.token`） |
| 3 | **模型目录** | 拉 `GET /v1/codar/models`（**无需鉴权**）→ 映射成 2api 的模型列表；`id` 直接用，`display_name` 做展示，`pricing` 可做倍率，`rate_badge` 可做标签 |
| 4 | ★ **聊天端点** | **待实测**：`POST /v1/model`（目录里 `path:"/model"`）还是 `/v1/chat/completions` ✗ |
| 5 | **用量/额度** | `GET /v1/codar/usage/daily`（白名单内 ✓） |
| 6 | **账号信息** | `GET /api/user/self`（401 需鉴权 ✓） |
| 7 | **凭证来源** | 桌面端登录后从 `~/.codar/config.json` 取 JWT（或走登录流程复现，**未研究**） |
| 8 | **额度/活动** | `/api/codar/activities/{free-model,invite-model}(/claim)`、`/api/pricing` —— 与 codearts 的 welfare 同构，可做成"领取"按钮 |

### 6.3 建议的实现顺序

```
① 实测聊天端点（§7-#1 是唯一硬阻塞）
② 用 JWT 打一次 chat，确认：请求体形状 / SSE 事件形状 / usage 字段 / 错误码
③ 按 codearts 上游的结构建 internal/codar/{client,auth,models}.go
   （签名层不需要 —— 这里只有 Bearer）
④ 接模型目录（无需鉴权，可做成缓存）
⑤ 额度/活动端点按需接
```

### 6.4 与既有上游的差异（要点）

- **没有请求签名**（不是华为云 SDK-HMAC ✓）⇒ 只要 Bearer ✓ 最简单的一类上游 ✓
- **没有 OAuth 刷新链**（未观察到 `refresh_token` 字样 ✓）⇒ JWT 过期后需要**重新登录** ✗（或观察是否有刷新端点 ✓）
- **本地 sidecar 不参与 2api**（2api 直连云端即可 ✓）

---

## 7. 未决 / 需要实测验证的

| # | 未决 | 为什么重要 | 怎么验 |
|---|---|---|---|
| 1 | ★★★ **聊天端点路径**：`/v1/model` 还是 `/v1/chat/completions`？ | 决定能不能发请求 | 用 config.json 的 JWT `POST` 两条各一次（**会消耗额度，需你批准**） |
| 2 | ★★ **JWT 的续期/过期机制** | 决定上游能不能长期无人值守 | 解 JWT 的 `exp`（**不落盘、只读长度与 exp**），或抓一次登录 |
| 3 | ★ **登录流程**（JWT 怎么来的） | 要做"账号池"就必须能自动登录 | 未研究：`codar-web.speedai.com` 的登录页 + `api.kuaipaper.com` 的 `{code,message}` 网关 |
| 4 | **额度/限流口径** | 决定能否多账号轮询 | `GET /v1/codar/usage/daily`（需 JWT） |
| 5 | **SSE 事件形状** | 接入时要正确转发 | 一次真实流式请求 |
| 6 | `protocol.path = "/model"` 的语义（是相对 `api_base` 还是另有前缀） | 同 #1 | 同 #1 |
| 7 | **`resources/bin/codar.exe`（68.5MB Go）内部** —— 真正的 chat 调用、system prompt、工具循环 | 想模仿它的行为/工具链时需要 | Go 二进制逆向（strings / GoReSym）——**本轮未做** |
| 8 | `computer-use.exe` 的 MCP 协议 | 若要支持"computer use"能力 | `resources/computer-use/README.md` 仅 229 字节，未展开 |
| 9 | `resources/runtimes/README.md`（1.5KB）与 `browser-connector/README.md`（365B） | 运行时依赖清单 | 直接读（**本轮未读**） |

### 本轮**没有**做的事（按约束）
- ❌ 未启动 Codar 应用
- ❌ 未修改任何文件（含 config.json —— **只读**）
- ❌ 未登录、未注册、未用 JWT 发任何请求
- ❌ 未 commit

### 复现命令（自写解包器）

```js
// asar：头部 4 个 uint32 中，第 4 个是 JSON 长度；payload 起点 = 8 + 第 2 个 uint32
//       ⚠ 不要用 16 + jsonSize（差 1 字节全错）
//       ⚠ 目录里的 file.offset 是**字符串**，必须 Number() 转换
```
```powershell
# 全套脚本与日志
D:\temp\t306-asar.mjs        解包器（913 文件 → D:\temp\codar-asar）
D:\temp\t306-sweep.mjs       关键词扫（756 个文本文件）
D:\temp\t306-deep.mjs        精读关键文件（主进程/渲染层）
D:\temp\t306-*.log           全部原始输出
D:\temp\codar-asar-filelist.txt  解包清单（913 行）
```
