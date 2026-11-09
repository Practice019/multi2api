# Codar（科搭）运行时探测报告

> 探测者：T-307（动态探测）。
> 时间：2026-11-08 01:5x（本地时区 Asia/Shanghai）。
> 探测方式：**只读**（流式扫二进制字符串、读安装目录/用户数据配置与日志、
> 对**已在运行**的本地 daemon 做 **GET-only** 探测）。
> **未启动应用、未登录、未做任何写操作**，报告中所有 token / cookie / 账号标识一律脱敏。
>
> 安装位置：`D:\software\Codar-Setup-latest\codar-desktop`（版本 **0.3.12**，publisher: Beijing Feichi Xingchen Technology Co., Ltd.）
> 二元探测互补：本报告 = 运行时/动态视角；asar 静态拆解由另一 agent 负责。

---

## 1. 现成实现调研（★ 最重要结论：**有**）

| 来源 | 有什么 | 可用性 |
|---|---|---|
| **GitHub `AiXiaoc/codar2api`** | **Codar2API** —— "把科搭 Codar 账号变成 OpenAI / Anthropic 兼容 API 的**多账号网关**"，附 Web 管理面板，**支持一键签到 / 领取免费福利**（⭐17） | ★ **可直接参考/复用**（协议层现成） |
| Linux.do 原帖 | `https://linux.do/t/topic/3105413`（2026-11-01，作者 ishxwl）：动机是"wb 封号严重、找新鸡蛋渠道"；实测 **Codar 每日可领 2000 积分**、新号免费 **DeepSeek V4.1 Flash**（7 天）、另有 **Claude / GPT 系列**、无套餐限制；作者自称"找了一圈没有 2api 只好自己写" | ✓ 背景与额度事实来源 |
| 镜像/讨论 | `https://feedbbs.com/linuxdo/185478`（同帖镜像，含回复） | ✓ |
| GitHub `aimod-cc/agent2api`、`yunqiasen/Agent2API` | **Agent2API**：把**多家**桌面客户端登录态汇成一个本地 OpenAI 兼容网关（WorkBuddy / Qoder / Trae / ZCode …） | ✓ 同类多源网关，可作架构参考 |
| 社区反馈 | 该帖回复 2026-11-01：「出现 `API access is restricted. Please contact support.`」；作者原话「官方开始加风控了」 | ⚠ **风控已存在**，需低频/多号策略 |

**结论：有现成实现（codar2api），协议层不必从零逆。** 建议直接读它的接入方式，再决定本仓上游是"照抄协议"还是"复用其登录/签到逻辑"。

---

## 2. 所有相关域名（从配置 + 二进制提取）

### 2.1 业务域名（真实调用面）

| 域名 | 用途 | 证据 |
|---|---|---|
| **`https://api.claw.speedai.com`**（+ `/v1`） | ★ **OpenAI 兼容的推理 API 基址**（`api_type: openai`） | `.codar\config.json` → `provider.api_base`；asar 常量 `SPEEDAI_API_BASE = https://api.claw.speedai.com` |
| `https://api.kuaipaper.com` | 账号/鉴权面（`AUTH_API_BASE`，prod）+ 远程控制中继 `api_base_path: /v1/codar/remote` | asar 常量 + config `desktop.remote_control.relay_url` |
| `https://test.api.kuaipaper.com` | 同上（dev） | asar `DEV_AUTH_API_BASE` |
| `https://img.kuaipaper.com/codar/desktop/stable/` | 桌面端更新源（electron-updater，`latest.yml`） | `resources\app-update.yml` + `logs\updater.log` |
| `https://skill.speedai.com` | Skills 市场（SpeedSkills） | config `desktop.speedskills_api_base` / `_web_url` |
| `https://codar.speedai.com/` | 官网 / 文档 / 下载页（登录面） | 二进制字符串 + 官网 |
| `https://codar.work/` | 文档站 | 搜索 |
| `https://speedai.com/` | 母品牌（SpeedAI 科研小助手） | 搜索 |

**环境变量覆盖点（对着改 base 的入口）**：
```
CODAR_SPEEDAI_API_BASE     → 覆盖 SPEEDAI_API_BASE
CODAR_AUTH_API_BASE        → 覆盖 AUTH_API_BASE
CODAR_API_KEY              → daemon 启动时若缺它会打 WARN「starting without CODAR_API_KEY; model setup is required」
```

**asar 里主进程代理层的白名单路径**（说明这些是稳定接口）：
```
/api/user/self        /api/log/self/stat       /v1/codar/usage/daily
/api/workspace/download?session_id=…           /v1/…
hostname === "api.claw.speedai.com" → target "speedai"（否则 "account"）
```

### 2.2 纯噪声（库/文档链接，不表示调用面）

`github.com`(66910×) · `modernc.org`(9842×) · `golang.org`(4419×) · `www.w3.org` · `json-schema.org` ·
`prosemirror.net` · `crbug.com` · `dawn.googlesource.com` · `beacons*.gvt2.com` · `example.com`
⇒ ⚠ **注意**：`resources\bin\codar.exe`（66.9 MB）是 **Go 二进制**（`modernc.org` / `golang.org` / `rsc.io` 全是 Go 生态），
即"agent daemon"用 Go 写的；这些域名是它的依赖/文档噪声，与 Codar 服务无关。

---

## 3. 本地端口（★ 有关键发现）

| 端口 | 是什么 | 证据（实测） |
|---|---|---|
| **`127.0.0.1:8460`** | ★ **Codar daemon 的本地 HTTP 服务**（`mode=desktop`） | `logs\desktop.log`：`msg="starting server" addr=127.0.0.1:8460 mode=desktop`、`url=http://127.0.0.1:8460`；`codar.exe` PID 25420 在跑，端口**正在监听** |
| `127.0.0.1:8765` | **browser-connector** 的中继（MCP server 名 `remote_browser`） | config `desktop.browser_connector.relay_host/relay_port` |
| `23119` | **Zotero 本地 API**（Codar 的文献集成会连它） | 二进制字符串 `localhost:23119` |
| `8080` / `8000` / `9229` / `53` | 噪声（示例/调试/Node inspector/DNS） | 出现次数 1–2 次 |

### 3.1 对 8460 的 GET-only 探测（**未做任何写操作 / 未生成内容**）

```
GET /health         ⇒ 200  {"agent":"codar","deployment":{"slot":"single","role":"active","ready":true,
                            "active_runs":0,"data_contract":"codar-web-v1", …}}
GET /api/models     ⇒ 401   （存在，需鉴权）
GET /api/v1/models  ⇒ 401   （存在，需鉴权）
GET /v1/models      ⇒ 405   （路由存在但**非 GET**）
GET /version        ⇒ 405       GET /docs ⇒ 405      GET /openapi.json ⇒ 405
GET /healthz        ⇒ 405       GET / ⇒ 405
```
**解读**：daemon 是一个**自带鉴权的 HTTP API**（`active_runs`、`drain` 这类字段说明它还管运行调度）；
`/health` 开放、`/api/*` 要凭据、`/v1/models` 是 POST 类路由。**鉴权细节本轮未探**（避免越界）。

---

## 4. resources 下的组件清单

| 路径 | 大小 | 是什么 |
|---|---|---|
| `resources\app.asar` | 36.8 MB | Electron 主进程 + 渲染层（含 API 代理层、常量、UI；另一 agent 在拆） |
| `resources\default_app.asar` | 107 KB | Electron 默认应用（模板） |
| **`resources\bin\codar.exe`** | **66.9 MB** | ★ **Go 写的 agent daemon**（就是跑在 8460 的那个；`internal/*` 风格日志：`runtime initialized`、`Skill Market prepared`） |
| `resources\computer-use\codar-computer-use.exe` | 2.6 MB | Computer-use（屏幕操作）MCP server |
| `resources\browser-connector\{dist,runtime}` | — | 浏览器连接器（中继 8765，MCP 名 `remote_browser`） |
| `resources\runtimes\` | — | 运行依赖（README 2 KB，未见大文件） |
| `resources\molecule-viewer\vendor` | — | 分子结构查看器（科研向 UI 组件） |
| `resources\localization\uiMessages.json` | 1.2 MB | 全量 UI 文案（**改包/汉化/找接口名时可当字典**） |
| `resources\build\icon.png`、`elevate.exe` | — | 图标 / 提权小工具 |
| 根目录 `Codar科搭.exe` | 195.1 MB | Electron 外壳（含 Chromium） |

---

## 5. 运行时数据（现成可读，价值很高）

```
%USERPROFILE%\.codar\
  config.json          2.5 KB   ★ daemon 配置：provider / api_base / agent profile / desktop 权限
  .config.lock
  sessions\ projects\ workspace\ plugins\ literature\cache.db (SQLite) background\logs \
  cache\modelcatalog\<sha>.json  108 KB   ← 模型目录缓存（可直接看有哪些模型）
  cache\skillmarket\*.json                 ← Skills 市场缓存
  desktop\installed-plugins.json 275 KB    ← 已装插件清单

%APPDATA%\codar-desktop\
  logs\desktop.log     11 KB   ★ 主进程日志（daemon stdout 全在里面，最有用的诊断入口）
  logs\updater.log     1.6 KB
  Network\Cookies      20 KB   ← ★ 会话 cookie（Web 面登录态）
  Partitions\codar-in-app-browser\           ← 内置浏览器分区（DIPS 等）
```

### 5.1 `.codar\config.json` 关键字段（**已脱敏**）

```jsonc
{
  "provider": {
    "auth_type": "speedai",
    "active_profile": "speedai",
    "speedai": { "token": "<MASKED>", "username": "<MASKED>", "login_at": 1794070329644 },
    "api_base": "https://api.claw.speedai.com/v1",     // ★ OpenAI 兼容基址
    "api_type": "openai",
    "model": "auto"
  },
  "agent": { "profile": "general", "context_manager": "codar" /* 另有 codex/claude-code/vanilla/… 共 12 种 */ },
  "desktop": {
    "default_model": "gpt-6-astra", "default_reasoning_effort": "high",
    "speedskills_api_base": "https://skill.speedai.com",
    "remote_control": { "relay_url": "https://api.kuaipaper.com", "api_base_path": "/v1/codar/remote" },
    "computer_use": { "mcp_server_name": "computer-use" },
    "browser_connector": { "mcp_server_name": "remote_browser", "relay_host": "127.0.0.1", "relay_port": 8765 },
    "permissions": { "files": true, "filesystem": true, "network": true, "shell": false, "browser": false, "mcp": false }
  }
}
```
⇒ 桌面端自己就是"**拿 speedai token 调 OpenAI 兼容 API**"这一套 ⇒ 这正好是上游接入要复刻的最小面。

---

## 6. 结论：**能做上游**，两条路

### 路线 A（推荐，与 Codar2API 同路）：**远程 OpenAI 兼容 API**
```
上游实现所需：base_url = https://api.claw.speedai.com/v1  +  Bearer <speedai token>
token 来源：① 桌面端登录（走 api.kuaipaper.com 鉴权面）
            ② 直接复用桌面端已登录的 %USERPROFILE%\.codar\config.json 里的 provider.speedai.token
            （★ 该文件就是现成的"登录态落盘"，无需重做登录流程）
模型：config 里 default_model=gpt-6-astra；模型清单见 .codar\cache\modelcatalog\*.json
```
- 优点：与现有上游（codearts 等）**同构**（OpenAI 兼容 + 一个 token），接入成本最低。
- 风险：**有风控**（linux.do 回复出现 `API access is restricted`）⇒ 需低频、多账号、按上游错误分类做冷却。

### 路线 B（备选）：**本地 daemon**
```
GET http://127.0.0.1:8460/health  ⇒ 200（免鉴权，可作健康检查）
其余 /api/*、/v1/* 需鉴权（401/405）⇒ 鉴权材料与调法本轮未探
```
- 优点：绕过远程风控（流量走官方客户端自身）。
- 缺点：**依赖桌面端进程在跑**、鉴权未明、`drain`/`active_runs` 说明它是**单机单例**语义 ⇒ 与"网关多账号轮询"模型不匹配。

### 建议的下一步（给主脑）
1. **读 `github.com/AiXiaoc/codar2api` 源码**（协议/登录/签到怎么做的，省一轮逆向）。
2. 想走路线 A：先确认"登录拿 token"的端点（`api.kuaipaper.com` 鉴权面）与 token 生命周期；
   想省事可直接读桌面端 `.codar\config.json` 的 token 做最小可用验证。
3. 想走路线 B：先探 8460 的鉴权（读 `app.asar` 主进程中 daemon 的调用处最省事）。

---

## 7. 本轮未做 / 未决

- ❌ 未启动 Codar 应用、未登录、未对任何接口做写操作（含未生成任何内容，避免消耗额度）。
- ❌ 未细读 `codar2api` 源码（只确认存在与定位）。
- ⚠️ `8460` 的**鉴权方式**未探（`/api/*` 401 的凭据是什么）。
- ⚠️ `/v1/models` 返回 405（路由存在但非 GET）⇒ 真实方法未确认。
- ⚠️ 报告刻意**不含**任何 token / cookie / 账号标识原文（`config.json` 与 leveldb 日志里都有会话 JWT）。
- ⚠️ 二进制扫描是**字符串级**（8 MB 分块 + 300 字符重叠）；若某处域名被字符串拼接/加密，
  本方法会漏 ⇒ 遗漏风险主要在 `app.asar` 的混淆 JS 里。
