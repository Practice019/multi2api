# v2.0.5 — 2026-11-14

> 格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

补丁版，**相对 v2.0.4 的增量**（24 个提交 / 51 文件 / +7371 −175）。
主题：**把 phanthywork 的出站请求逐项对齐到官方客户端，并真机验证通过** —— 期间连续挖出四类"看起来对、其实不对"的缺陷，每一类的判据原先都在守护错误行为。

## 修复

### ★ phanthywork 推理路径缺 4 个证明头 —— 封号的直接原因

这是本轮最重要的一处。此前 `POST /v1/chat/completions` **完全不签名**，注释还写着"签名相关的一个都不要加"，理由是"抓包实证客户端全程零签名"。

那个实证**只覆盖了链路的一跳**：

```
官方：内核（Phanthy Codeo）→ 本地桥（sidecar, 127.0.0.1）→ 上游
抓到：内核 → 本地桥        ← 这一跳确实零签名
漏掉：本地桥 → 上游        ← 桥会补 12 段签名 + 4 个证明头
```

官方桥的路径集合逐字包含 chat（`Sd3 = new Set(["/v1/messages","/v1/responses","/v1/chat/completions"])`），而它的 proof 构造器 `_O8` 与本包 `InferenceCanonical` **逐段一致** —— 也就是说 12 段签名算法早就写对了，缺的只是"用在 chat 上"。

- 现在 chat 路径：`ensureInferenceToken` → `buildProof` → 挂 4 个 `x-phanthy-desktop-session` / `x-phanthy-proof-*`。
- `x-request-id` / `x-claude-code-session-id` 改取 `proof` 的值（它们同时是 canonical 第 9/10 段，签名与发头必须同源；此前各自随机生成）。
- **真机验证通过**：新注册账号，`registered_at_ms` 齐全，`deepseek-v4.1-flash-1017` 流式返回 200（93 tok / 1696 ms / 33 tok·s⁻¹）。
- 两条**在守护错误行为**的判据已反转：`TestChatSendsNoSignatureHeaders` → `TestChatPathCarriesDesktopInferenceProof`；`TestChatDoesNotFetchInferenceToken` → `TestChatFetchesInferenceToken`。

### phanthywork 三条 canonical 的分隔符是 LF，不是空格

`device/v1` 两条串曾按空格拼，导致设备注册恒 `401 invalid_desktop_signature` —— 而错误码完全指不到根因（凭证里从未出现过 `registered_at_ms`，表现成"账号被封"）。按官方源码逐字节核实：三条串**全部** `.join(LF)`。

### phanthywork 推理头与官方 sidecar 1:1

去掉 3 个官方根本不存在的头（`x-phanthy-client` / `x-phanthy-project` / `x-phanthy-product`），`client-source` 由 `cli` 改为 `desktop`（与注册声称的 `edition=phanthy_work` 一致），补设备指纹与版本头。

### phanthywork `/api/*` 的 UA 分档

UA 是按路径分档的，不是全局一个值：`/api/oauth/*` 官方发 `phanthy-code-cli/<APP_VERSION>`，而推理路径官方**不设**（Bun 下即 `Bun/1.3.14`）。此前全局用后者，`/api/*` 那一族是错的。

### phanthywork 取邀请码的幂等键多了尾巴

官方是 `referral-code:<installationId>`（两段），本仓与第三方插件记的都是三段（多日期/毫秒段）。多一段就与官方不是同一个键，上游的去重语义整个失效，且**不会有任何报错**。

### workbuddy-intl 目录与登录弹窗

模型目录改由 `/v3/config` 兜底；修掉"无法创建数据目录 `.browser-profiles\…`"那个 Edge 弹窗（`profileRoot` 恒取绝对路径）。

### loomy 模型目录改为并集缓存

从「每次现查单账号」改为「多账号并集 + 24h 定时刷新」，并修掉生图模型重复追加（`handler.go` 加 `seenAt` 去重）。

## 新增

### phanthywork 三个活动面端点

从官方桌面端前端 bundle（`_up_\dist\assets\App-dSHMvpu3.js`，明文可读，不需抓包）逐字取出定义后补齐：

```
GET  /api/oauth/rewards                        FetchRewards
GET  /api/oauth/referrals/relations            FetchReferralRelations
POST /api/oauth/activities/onboarding/reconcile            ReconcileOnboarding
POST /api/oauth/activities/onboarding/tasks/<id>/start     StartOnboardingTask
POST /api/oauth/activities/onboarding/tasks/<id>/complete  CompleteOnboardingAction
```

响应一律返回 `json.RawMessage` —— 官方前端只是把 JSON 往下传，bundle 里没有展开字段；**给未知结构编字段就是重复本轮的错误**。

### 模型目录页「行内动作」

供应商行收窄成一列，只保留「重拉」；新增 `GET /ui/models/explain` 与重拉端点（写入面单独鉴权）。

## 变更

### 协议集中到 `docs/protocols/` + `docs/decisions/`

三份协议 + 两篇 ADR + 一份交接文档入库，并从 `.gitignore` 逐条放行（目录级白名单要用「目录 + `/**`」两条）。

### 前端操作反馈统一走共享通知层

`internal/server/toast.js` 是唯一实现，由 `tools/toast_sync.js` 在标记之间注入；会切换文案的按钮固定 `min-width`（否则每次点击界面都会位移）。附 8 条结构判据。

## 判据

`go build ./...` exit 0，`go test ./... -count=1` **52 包零 FAIL**。
本轮新增/反转的判据都做过**反向验证**（注入违规 → 确认变红 → 还原），包括：
`TestChatPathCarriesDesktopInferenceProof`、`TestChatFetchesInferenceToken`、
`TestReferralCodeIdempotencyKeyIsByteExact`、`TestSignV1RejectsQueryInPath`、
`TestBuildProofRejectsQueryInPath`、`TestCanonicalSeparatorsArePerString`。

---
