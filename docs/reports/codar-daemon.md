# Codar 本地 daemon（127.0.0.1:8460）鉴权与路线 B 可行性

> 探测者：T-309（承接 T-307）。
> 时间：2026-11-08 02:0x（Asia/Shanghai）。
> 方式：**只读**（读命令行 / ready-file 线索 / 主进程配置）+ 对**已在运行**的 daemon 做 **GET-only** 探测。
> **未启动/停止应用、未修改配置、未生成任何内容**（不耗额度）。所有 token 一律脱敏（只给前 6–8 位 + 长度）。
>
> 上游报告：`docs/reports/codar-probe.md`（运行时/域名/组件总览）。

---

## 1. 结论速览

| 问题 | 答案 |
|---|---|
| **8460 的鉴权方式** | ★ **`X-Codar-API-Token: <64 位 token>` 请求头**；等价地可用 **`?codar_api_token=<token>`** query（两者实测都行） |
| **token 来源** | launcher 启动 daemon 时生成 → 写入 **ready-file** → launcher 读回 → 以 `--codar-api-token` 传给**渲染进程**（也是外部可获取点） |
| **无 token 时** | 所有 `/api/*` ⇒ **401**；`/health` ⇒ 200（唯一开放端点） |
| **有 token 时** | `/api/chat/runs` ⇒ **200 `{"runs":[]}`**、`/api/sessions` ⇒ **200（真实会话列表）**；`/api/models`、`/api/v1/models`、`/api/desktop/state`、`/api/web/session`、`/api/chat` ⇒ **405**（路由存在，但**非 GET**） |
| **`cloud-auth-request` 能用吗** | 路由**存在且需同一 token**（无 token ⇒ 401）；**"能不能代理任意云端路径"本轮未验证**（见 §5 未决：需要一次 POST，而本轮约束只允许 GET） |
| **路线 B（走本地 daemon）可行吗** | **机制上可行，工程上不划算**（见 §6） |
| **daemon 能否独立运行** | 它是独立可执行文件（`codar.exe desktop --host … --port 0 --ready-file …`），**但本轮未实测手工拉起**（约束：不启动/停止 Codar） |

---

## 2. daemon 的启动形态（命令行实测）

```
resources\bin\codar.exe desktop --host 127.0.0.1 --port 0 \
    --ready-file D:\temp\codar-desktop-5400-be79b0b1-4cbd-4d4a-9691-3240fcf09373.json
```
- **`--port 0`** ⇒ 端口由系统分配，**不是固定 8460**（8460 只是本次的值）⇒ 端口必须从 ready-file 或 `/health` 反查。
- **`--ready-file <path>`** ⇒ 启动就绪信息（含 **port + token**）写进该文件。
- 父进程链：`Codar科搭.exe`(PID 5400) → `codar.exe`(PID 25420) ⇒ **daemon 由桌面应用拉起**（不是常驻服务）。
- 启动日志（`%APPDATA%\codar-desktop\logs\desktop.log`）：
  ```
  WARN  msg="starting without CODAR_API_KEY; model setup is required before chat runs"
  INFO  msg="starting server" addr=127.0.0.1:8460 mode=desktop deployment_slot=single …
  INFO  msg="Skill Market runtime started"
        daemon startup completed. launcher=direct url=http://127.0.0.1:8460
  ```
  ⇒ `deployment_slot=single` = **单实例语义**；没有 `CODAR_API_KEY` 时**模型不可用**（即 daemon 自己不会凭空拿到云端凭据）。

---

## 3. 鉴权方式（★ 本轮主要结论）

### 3.1 证据 A：asar 源码（静态）

```js
// 主进程：读 ready-file 的 token，用它调 daemon
headers: { "Content-Type": "application/json", "X-Codar-API-Token": ready.token, … }

// 渲染层：从 preload 拿到的 apiToken
const desktopApiToken = window.codarDesktop?.apiToken
// preload：readArg("codar-api-token") / readArg("codar-api-base")

// 两条挂载方式（同一个 token）
function shouldAttachApiToken(path) { return path.startsWith("/api/") || path.startsWith("/workspace/") }  // 但 "/api/","/workspace/" 本身除外
headers.set("X-Codar-API-Token", API_TOKEN)                       // ① 头
url.searchParams.set("codar_api_token", API_TOKEN)                // ② query（WebSocket 也用这条）
```

### 3.2 证据 B：运行时实测（GET-only）

```
无 token      : /health 200 | /api/models 401 | /api/v1/models 401 | /api/chat/runs 401 | /api/sessions 401
带 token(头)  : /health 200 | /api/models 405 | /api/v1/models 405 | /api/chat/runs 200 {"runs":[]}
                              /api/sessions 200 {"sessions":[…]} | /api/desktop/state 405 | /api/web/session 405
带 token(query): /api/models 405 | /api/v1/models 405        ← 与头等价
```
**⇒ 401 → 405/200 的跳变就是"鉴权通过"的直接证据**（405 表示路由存在但要求别的 HTTP 方法）。

### 3.3 token 的三个获取点（按稳定性排）

| 来源 | 可行性 | 说明 |
|---|---|---|
| **ready-file**（`D:\temp\codar-desktop-<launcherPid>-<uuid>.json`） | ⚠ 理论上最正统，但**实测已被应用删除**（launcher 读完即删）；只在 daemon 刚起的窗口期内存在 | 需要"起 daemon 后立刻读" |
| **渲染进程命令行 `--codar-api-token=<64位>`** | ✓ **实测 3 个进程带它**（同用户可读） | 最容易，但依赖桌面端在跑 |
| 应用内 `window.codarDesktop.apiToken` | ✓（需能执行 JS） | 需 DevTools/注入 |

> token 形态：**64 位十六进制字符串**（本次实测 `len=64`，前 6 位 `51b130…`）。

---

## 4. 端点结构（本轮实测到的）

| 端点 | GET 结果（带 token） | 解读 |
|---|---|---|
| `/health` | **200**（免鉴权） | `{agent:"codar", deployment:{slot:"single", role:"active", ready:true, active_runs:N, data_contract:"codar-web-v1", drain…}}` |
| `/api/chat/runs` | **200** `{"runs":[]}` | 运行列表（可读） |
| `/api/sessions` | **200**（真实会话数组，含 `collaboration_mode` / `agent_profile` / `execution_target:"local"`） | 会话列表（可读） |
| `/api/models`、`/api/v1/models` | 405 | 存在，**非 GET**（很可能是 POST 查询） |
| `/api/desktop/state`、`/api/web/session`、`/api/chat` | 405 | 同上 |
| `/api/desktop/cloud-auth-request` | **401（无 token）** | ★ 路由存在且需同一 token；**未做 POST 验证**（见未决） |
| `/api/chat/events` | ⚠ **未测**（见下） | 应为 **SSE 流** |

⚠️ **踩坑警告（留给下一个探测者）**：对 `/api/chat/events` 发 **GET** 会让调用方进程 **OOM 崩溃**
（本轮实测：PowerShell 进程直接 `Out of memory` 退出）—— 那是 **SSE 长流**，别用 `Invoke-WebRequest` 裸拉，
要用流式读取 + 超时，或干脆只在需要时用支持 SSE 的客户端。

---

## 5. `cloud-auth-request`（T-306 发现的代理端点）

**已知**（来自另一 agent 的 asar 静态发现 + 本轮实测）：
```
POST ${sidecar}/api/desktop/cloud-auth-request
headers: { "X-Codar-API-Token": ready.token, "Content-Type": "application/json" }
body:    { target: "speedai"|"account", path, method, authorization, body }
路由存在性：✓（无 token ⇒ 401；与其它 /api/* 同一鉴权）
```
**⇒ 它就是"桌面端替你把请求打到云端并附上 authorization"的代理**（`target=speedai` → `api.claw.speedai.com`，
`target=account` → `api.kuaipaper.com`，与 `codar-probe.md` §2 的两个基址一一对应）。

**本轮未能确定的部分**：能否代理**任意**云端路径（白名单/参数校验/是否只放行它自己 UI 用到的那些），
需要一次 **POST** 才能定论 —— 而本轮约束是 **GET-only 且不生成内容**，故**刻意没做**（如实记录，不猜）。
> 建议的最小验证（下一步，一次即可，不耗模型额度）：
> `POST /api/desktop/cloud-auth-request` with `{target:"account", method:"GET", path:"/api/user/self"}`
> —— 只读账号信息，不触发任何模型调用。

---

## 6. 路线 B 可行性判断（结论：**能用，但不推荐**）

**支持它的理由**
- 只需**本地 HTTP + 一个头**，不碰登录流程、不碰风控（流量由官方客户端自己发）。
- 已实测：带 token 就能读到 `/api/chat/runs`、`/api/sessions`；`/health` 还能当健康检查。

**反对它的理由（都是硬约束）**
1. **daemon 不是服务**：由桌面应用拉起，应用关掉就没了 ⇒ 网关的可用性挂在"桌面端常驻"上。
2. **端口随机**（`--port 0`）⇒ 每次都要重新发现（ready-file 已删 ⇒ 只能靠扫描/`/health` 探测）。
3. **token 随启动生成且 ready-file 读完即删** ⇒ 每次重启都要重新抓（渲染进程命令行 / JS 上下文）。
4. **单实例语义**：`deployment_slot=single`、`role=active`、`active_runs`、`drain` ⇒ 与"网关多账号轮询"模型天然冲突
   （多账号要靠云端侧的账号池，而不是本地 daemon）。
5. **它自己不解决凭据问题**：没有 `CODAR_API_KEY` / provider token 时模型不可用 ⇒ 最终**还是要拿到那个云端 token**。
6. 版本会跟着桌面端更新而变（本轮 0.3.12，路由可能变）。

**⇒ 推荐路线 A（`codar-probe.md` §6）**：直接 `base_url=https://api.claw.speedai.com/v1` + `Bearer <speedai token>`，
token 复用 `%USERPROFILE%\.codar\config.json → provider.speedai.token`（或自行登录）。
路线 B 仅作为"想绕开直连风控"时的兜底，且必须接受上面 4 条脆弱性。

**daemon 能否独立运行**：命令行看是**独立可执行**（无强制依赖 Electron 的参数），
但**本轮未实测**（约束禁止启动/停止 Codar）；即便能跑，仍需要 provider 凭据才有模型。

---

## 7. 本轮未做 / 未决

- ❌ 未 POST 任何端点（含 `cloud-auth-request`）⇒ "能否代理任意路径"**未验证**。
- ❌ 未对 `/api/chat/events` 做受控探测（会 OOM；已记录踩坑）。
- ❌ 未实测手工拉起 daemon（约束禁止）。
- ⚠️ `ready-file` 已不在盘上（应用读完即删）⇒ token 的"正统来源"只在启动窗口期存在。
- ⚠️ 报告不含任何 token 明文；实测用的 token 只在内存变量里存活，未落盘、未回显。
- ⚠️ 探测过程中出现一次 **PowerShell OOM 崩溃**（原因见 §4 的 `/api/chat/events`）⇒ 后续探测请勿裸 GET SSE 端点。
