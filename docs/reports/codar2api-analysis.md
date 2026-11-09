# codar2api 源码分析（`github.com/AiXiaoc/codar2api`）

> 分析者：T-310（承接 T-307/T-309）。
> 对象：**`AiXiaoc/codar2api`**（Go，⭐17，默认分支 `main`，仓库 504 KB，更新于 2026-11-05）。
> 取码方式：直连 `git clone` 被重置（`Recv failure: Connection was reset`）、`kkgithub.com` 挂（schannel 失败），
> **最终用 `https://ghproxy.net/https://github.com/AiXiaoc/codar2api.git` 成功**；落盘 `D:\project_GIT\codar2api`（91 文件 / 725 KB / 47 个 .go）。
> 约束遵守：**未运行它、未发任何真实请求、未改它的代码**；报告内无任何真实 token（测试里只有 `fakeToken`）。

---

## 0. 结论摘要（★ 最重要放最前）

| 问题 | 答案 | 代码证据 |
|---|---|---|
| **① 聊天端点** | ★ **不存在私有 `/model` 路径** —— 网关把**客户端自己的 path 原样拼到上游**：`target = APIBase + r.URL.Path` ⇒ `POST https://api.claw.speedai.com/v1/chat/completions`（以及 `/v1/messages`、`/v1/responses`、`/v1/messages/count_tokens`） | `internal/gateway/relay.go:271`、`internal/gateway/app.go:205`、`app.go:49` |
| **② 请求体格式** | ★ **原样透传**（默认 passthrough）：**客户端 JSON 按原始字节转发**，保留扩展字段/数字精度/查询编码；只换掉 `Authorization`、按开关改 `User-Agent`，并在开启时做 prompt 身份改写 | `README.md:40`、`relay.go:277`、`internal/gateway/compat.go:106-132` |
| **③ SSE 响应** | ★ **原样透传**（先设 `Accept-Encoding: identity` 避免 gzip）⇒ 上游 SSE **字节级返回给调用方**；非流式同样原样回写（含上游 `Content-Type`）。非 passthrough 模式下会给 Chat 流式**补 usage** | `relay.go:288`、`relay.go:366`、`relay.go:397-399`、`README.md:40` |
| **④ token 用法** | `Authorization: Bearer <账号 JWT>`（**一个 token 一个账号**）；续期 = `POST https://api.kuaipaper.com/v1/gettoken {username, token:0}`；可**直接导入**桌面端 `config.json` 的 `provider.speedai.{token,username}` 形状 | `upstream.go:158`、`upstream.go:389`、`account_import_test.go:39` |
| **⑤ 风控处理** | 三层：① 上游 `account_restricted` ⇒ 账号标记受限、停止派发（可手动/批量解除）；② 上游 403 拦截页（nginx 页）⇒ 标记**该出口 IP 被封控**、换出口+换号重试（且"只有模型请求会触发封控"）；③ `retryable(status,body)` ⇒ 换账号重试；另有网关级并发上限 ⇒ `429 + Retry-After` | `README.md:48-49,254`、`relay.go:373,386-390`、`upstream.go:59-80`、`app.go:334` |
| **⑥ 多账号** | 账号池 + **权重 / 单账号最大并发** + **代理绑定（固定或轮询）** + **按会话粘滞**（`X-Session-Id`/`X-Conversation-Id`/`session_id`/`conversation_id`/`prompt_cache_key` 作 hint ⇒ 同一会话不换出口、流式中途不换）；对外用**自建 API Key（`sk-codar-…`）+ 模型范围** | `relay.go:14-20,185-187`、`README.md:64,320`、`store.go`（账号字段） |
| **⑦ 模型列表** | `GET https://api.claw.speedai.com/v1/codar/models`（**免鉴权**、可走代理）→ 拍平成 `{id,name,protocol,context_window,…}`；对外 `GET /v1/models`（按 API Key 的模型范围过滤） | `upstream.go:248,285`、`app.go:58,299-306` |
| **⑧ 签到/福利** | 签到：`POST https://api.kuaipaper.com/v1/daily-sign/status {username}`；免费模型活动：`GET api.claw…/api/codar/activities/free-model` + `/api/codar/activities/mine`；邀请/推广：`POST api.kuaipaper.com/v1/promotion/info {username,token:0}`；令牌续期：`POST /v1/gettoken`；后台**每 30 秒**巡检、`auto_sign` 开关 | `upstream.go:406,414,419,432,389`、`README.md:54,185,268`、`store.go:177` |

---

## 1. 项目概况

| 项 | 值 | 证据 |
|---|---|---|
| 仓库 / 作者 | `AiXiaoc/codar2api` | GitHub API |
| 语言 / 版本 | **Go**（`go 1.26.4`，**无第三方依赖**：`go.mod` 除 module/go 两行外为空） | `go.mod` |
| 模块名 | `github.com/codar-gateway/codar-gateway` | `go.mod:1` |
| 结构 | `cmd/codar`（入口）+ `internal/gateway`（45 个 .go，单包式实现） | 目录 |
| 规模 | 91 文件 / 725 KB（含 10 个 .js、9 个 .cjs、2 个 .html = 管理面板前端） | 目录 |
| 定位 | 多账号网关：对外 OpenAI / Anthropic 双协议，对内把请求转给 Codar 上游 | `README.md:38` |
| 许可 | MIT | `LICENSE`、`README.md:15` |
| 自更新 | 指向自己的 GitHub Release API | `internal/gateway/update.go:17-18` |

---

## 2. 协议实现详解（★ 核心）

### 2.1 上游基址（三处常量）

```go
// internal/gateway/app.go:49-58
c.APIBase = "https://api.claw.speedai.com"      // 模型/账号主体
c.PayBase = "https://api.kuaipaper.com"          // 计费/签到/续期/推广
c.WebBase = "https://codar-web.speedai.com"      // Web 面
c.CheckURL = c.APIBase + "/v1/codar/models"      // 免鉴权探活/目录
```

### 2.2 转发路径 = **客户端 path 直通**

```go
// internal/gateway/relay.go:271-302（节选）
target := a.cfg.APIBase + r.URL.Path                      // ★ 上游 URL = 基址 + 同一个 path
req, _ := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(raw))
req.Header.Set("Authorization", "Bearer "+text(chosen["token"]))
req.Header.Set("Content-Type", "application/json")
req.Header.Set("Accept-Encoding", "identity")             // 禁 gzip ⇒ SSE 才能原样透传
req.Header.Set("User-Agent", modelUserAgent(settings, r.UserAgent()))  // 默认 codar-desktop/0.3.3
copyForwardedHeaders(req.Header, r.Header)                // 透传普通头（过滤本地 Key/Cookie/代理头）
if strings.HasPrefix(r.URL.Path, "/v1/messages") && req.Header.Get("anthropic-version") == "" {
    req.Header.Set("anthropic-version", "2023-06-01")     // Anthropic 客户端兼容
}
```
⇒ 网关自己宣告的对外面（`app.go:205`）：
```
POST /v1/chat/completions     POST /v1/responses
POST /v1/messages             POST /v1/messages/count_tokens
GET  /v1/models               GET /ping /health
```

**⚠️ 与我方 T-306 记录的差异（重要更正）**：T-306 记「models 目录里 `path` 字段写 `/model`」——
但本仓库**没有任何 `/model` 上游路径**：目录项只有 `{id,name,protocol{type},context_window}`（`upstream.go:285`），
而**真正决定上游 URL 的是客户端请求的 path**（`relay.go:271`）。⇒ 接入时**不要**照 `/model` 拼，直接打标准 `/v1/chat/completions`。

### 2.3 请求体：默认**字节级透传**

`README.md:40` 原话：「默认开启，未被消息规则修改的 JSON 正文按原始字节转发，保留扩展字段、数字精度、
查询参数编码及大部分普通请求头；鉴权替换为所选账号的上游凭证，UA 按开关处理。」
⇒ 因此**扩展字段/非标准字段不会丢**；`compat.go` 只在开启时做 **prompt 身份改写**（三种协议各一套入口）：
```go
// internal/gateway/compat.go:125-132
case "/v1/chat/completions": applyMessages(payload["messages"])
case "/v1/responses":        apply(payload,"instructions"); applyMessages(payload["input"])
case "/v1/messages", "/v1/messages/count_tokens": applyMessages(payload["messages"])
```

### 2.4 SSE：原样回写（+ 可选补 usage）

```go
// relay.go:366 / 397-399
lastBody, lastStatus, lastType = body, resp.StatusCode, str(resp.Header.Get("Content-Type"),"application/json")
w.Header().Set("Content-Type", lastType); w.WriteHeader(lastStatus); w.Write(lastBody)
```
非流式走上面的"缓冲后原样回写"；流式则边读边写（`Accept-Encoding: identity` 保证不被压缩破坏帧）。
非 passthrough 模式下会给 **Chat 流式补 usage**（README:40 明确）。
⇒ 调用方**不需要**适配任何私有 SSE 格式：上游就是标准 OpenAI/Anthropic 事件流。

---

## 3. 认证与 token 管理

- **上游调用**：`Authorization: Bearer <账号 JWT>`（`upstream.go:158`）；JWT 内含 `username`（`account_catalog_test.go:35-36` 直接解 JWT 取 username）。
  ⇒ 与我方 T-309 的发现一致：桌面端 `.codar\config.json` 的 `provider.speedai.token` 就是这种 JWT（我方实测长度 207 字符级别）。
- **导入方式（★ 与我方资产直接对接）**：
  - 粘贴 `Authorization: Bearer` 后的 JWT，多行即多账号（`README.md:111`）；
  - **直接读桌面端 `config.json`**：`{"provider":{"speedai":{"token":…,"username":…}}}`（`account_import_test.go:38-39`）；
  - 面板可**扫码登录**（`login_test.go:78` 的 `scene/qr_url`）、**短信**（`login.go:33 sendSMS`）自动取号；
  - 导入**不做上游校验**，靠后续 refresh（`README.md:52`）。
- **续期**：`POST {PayBase}/v1/gettoken {username, token:0}`（`upstream.go:389`），后台"令牌临近到期时续期"（`README.md:268`）。
- **对外鉴权**：网关自建 API Key（形如 `sk-codar-…`，`README.md:64`），可**限定模型范围**（`/v1/models` 按 Key 过滤，`README.md:276`）。

---

## 4. 风控应对（本项目最值得抄的一章）

| 机制 | 实现 | 证据 |
|---|---|---|
| 账号受限 | 上游明确回 `account_restricted` ⇒ 立即标记受限并持久化，之后**不再派发**；可手动/批量解除 | `README.md:48`、`upstream.go:128,215`、`relay.go:386` |
| **IP 封控** | 上游 403 拦截页（nginx `403 Forbidden`）⇒ 把**当前出口 IP** 标记封控、停止经它调度、**换其它出口的账号重试**；改代理地址可自动清除封锁；可配置成"403 原样返回给调用方" | `README.md:49,254`、`upstream.go:59-80`、`relay.go:373` |
| 只让模型请求触发封控 | 「只有模型请求会触发封控，签到/刷新余额等遇到 403 页面时只报错」 | `README.md:254` |
| 换号重试 | `retryable(status, body)` 成立 ⇒ 记失败并换下一个账号（同一请求内多次尝试，`attempts` 落日志） | `relay.go:389-390`、`relay.go:123` |
| 网关级并发闸 | 并发达上限 ⇒ `429 + Retry-After` + 中文提示 | `app.go:334`、`concurrency_test.go:65` |
| 代理池 | 固定代理 / 轮询代理；**流式请求中途不换出口**；轮询账号共享轮询位置 | `README.md:320`、`proxy.go:20`、`proxy_retry.go:28` |
| 会话粘滞 | 用 `X-Session-Id`/`X-Conversation-Id`/`Session-Id`（头）或 `session_id`/`conversation_id`/`prompt_cache_key`（body）作 hint ⇒ 同会话走同账号；**无 hint 才轮询** | `relay.go:14-20,185-187` |
| 失败面收敛 | 请求黑名单（`request_blocklist.go`）记录被拦的 path+model+reason，避免反复踩同一雷 | `request_blocklist.go:54`、`relay.go:387` |
| 诊断 | `--request-diagnostics` ⇒ `request-diagnostics.jsonl`（含实际转发头与正文）；请求调试日志可选"全部/正常/错误" | `README.md:44-45` |

---

## 5. 与 workbuddy2api 的对比

| 维度 | codar2api | 本仓（workbuddy2api） |
|---|---|---|
| 规模 | 单包 `internal/gateway` 45 文件、零依赖 | 多上游 + `gateway` 接缝 + 28 扩展点 + 架构测试守约 |
| 上游抽象 | 只有 Codar 一个上游（写死三常量） | provider 接口 + 注册表（加上游零改核心） |
| 转发策略 | **原样透传**（默认）+ 可选身份改写 | 各上游 `PrepareBody`/改写 + 核心统一出口 |
| 凭据 | 直接存上游 JWT（导入即用） | `Credential{Secret}` 不透明 + 池投影 + 扩展点 |
| 风控 | 账号受限 + **IP 封控** + 代理池 + 会话粘滞 | `ErrorKind` 分类 + 冷却/熔断 + 代理池（有）但**无"出口 IP 封控"这一层** |
| 对外协议 | OpenAI + Anthropic 双协议 | OpenAI 兼容为主（`/v1/chat/completions` 等） |
| 自更新 | 内置（读自己 Release） | 无 |

**⇒ 本仓可以直接吸收的两点**：
1. **"IP 出口封控"独立于"账号受限"**（本仓目前只按账号/错误分类，出口维度没有一等公民）；
2. **会话粘滞**（`session_id`/`prompt_cache_key` ⇒ 同会话不换号，避免缓存失效与出口抖动）。

---

## 6. ★ 接入建议

**结论：参考它自己写，不要直接用它。**

理由：
1. 它是**独立网关**（自带面板/API Key/统计/自更新），与本仓"多上游聚合"的定位重叠 ⇒ 直接跑会变成两套网关串联，运维与排障翻倍。
2. 但它把**最脏的活**都试过了，且都是**可搬的常量与流程**（下表），照抄成本极低：
```
基址：        APIBase = https://api.claw.speedai.com        PayBase = https://api.kuaipaper.com
聊天：        POST {APIBase}/v1/chat/completions（标准 OpenAI，含 SSE；Anthropic 用 /v1/messages）
鉴权：        Authorization: Bearer <账号 JWT>（JWT 带 username claim）
目录：        GET  {APIBase}/v1/codar/models（免鉴权）→ {id, display_name, protocol.type, context_window}
资料/活动：    GET  {APIBase}/api/user/self      GET {APIBase}/api/codar/activities/free-model|mine
签到：        POST {PayBase}/v1/daily-sign/status {username}
续期：        POST {PayBase}/v1/gettoken {username, token:0}
余额/推广：    POST {PayBase}/v1/agent/balance {username}      POST {PayBase}/v1/promotion/info {username, token:0}
UA：          默认伪装 codar-desktop/0.3.3（可关）
凭据来源：    桌面端 %USERPROFILE%\.codar\config.json → provider.speedai.{token,username}（我方 T-309 已确认）
```
3. **落地上游的最短路径（建议）**：
   - `internal/codar/`：`provider.go`（4 方法）+ `credential.go`（读上面那份 config.json 或直接收 JWT）+ `errorclassifier.go`（把 `account_restricted`→`ErrKindFatalClient`/`SessionDead`、403 拦截页→`ErrKindAuth` 或新出口冷却、429→`ErrKindSoftRate`）。
   - 出站只需标准 OpenAI 路径 ⇒ **不需要** `PrepareBody` 特判（Codar 上游就是 OpenAI 兼容）。
   - 若要复用现有 `codearts` 的鉴权风格：这里更简单（只有 Bearer，无 DPoP）。

**成本对比**：
| 方案 | 成本 | 风险 |
|---|---|---|
| 直接用 codar2api | 低（部署即可） | 两套网关叠加；它的面板/Key 成为新运维面；无法复用本仓账号池/统计 |
| **参考实现自己写（推荐）** | 中（~1 个上游包 + 一个 classifier） | 上手即 OpenAI 兼容，几乎无协议风险；风控逻辑建议抄"IP 封控 + 会话粘滞" |
| 用本地 daemon（T-309 路线 B） | 高 | 端口随机/token 随启动生成/单实例/依赖桌面端常驻 |

---

## 7. 未决

1. **未实测任何真实请求**（约束禁止）⇒ 上述端点/字段全部来自源码与 README，未与运行时对照（我方 T-307/T-309 已独立确认基址与桌面端 token 形状，互相印证）。
2. **T-306 记录的 `/model` path 未在源码中出现** ⇒ 建议回头核对那条记录的来源（可能是上游目录里某项字段，或 T-306 观察的是别的对象）。
3. 未细读它的**代理池实现**（`proxy.go`/`proxy_retry.go`）与**面板前端**（`internal/gateway/web/*`）⇒ 若要把"IP 封控"搬进本仓，值得再读一轮。
4. 未确认它的**上游用量/额度接口**（`/v1/codar/usage/daily`，`upstream.go:669`）返回结构 ⇒ 接余额显示时需要。
5. 它 README 自述风险：「网关会以你的账号令牌访问 Codar 上游，产生的调用、扣费与风控后果由使用者自行承担」（`README.md:25`）——与 T-307 发现的"官方已加风控"一致，接入前应确认频控策略。
