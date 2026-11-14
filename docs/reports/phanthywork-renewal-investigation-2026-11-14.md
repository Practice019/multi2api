# 调查报告：phanthywork 自动续期"看着正常，隔一个令牌寿命就死"

> 日期：2026-11-14 · 作者：本项目主会话
> 性质：**调查交接**。本文只陈述事实、证据与待验证假设，**不含未验证的结论**。
> 基线：`master = 9721d4d`（工作区干净）。目标上游：`phanthywork`。
> 读者：接手的 agent。你的任务是 **§5 的假设逐条证伪**，并给出可落地的处置。

---

## 0. 一句话现状

**续期代码在跑、也成功过、也落盘了。** 但账号的 `refresh_token` 在上游被作废
（`400 invalid_grant`），于是 access_token 到期后再也续不上。

**本地代码已排除的项见 §4**（都带证据与复现命令，请不要重做）。
**真正未解的是 §5**：上游为什么作废它。

---

## 1. 症状（用户视角）

控制台账号列表里 `phanthywork` 的一个号显示：

```
已禁用 · 已过期 · 失败 · credits=3299
reason: 凭证续期连续失败 3 次（refresh token 失效，需重新登录）
```

用户原话：「**之前自动续期都是没有问题的。自动续期之后不是要更新 Refresh Token 吗？
它确实会换啊，但是可以本地落盘。**」

即：用户认为这是"轮换后的新 refresh_token 没落盘"导致的。**这条假设本轮已被证伪，见 §4.2。**

---

## 2. 时间线（全部为 +08，毫秒级）

| 时刻 | 事件 | 证据 |
|---|---|---|
| 2026-11-14 **17:05:09.242** | 账号注册成功 | 凭据文件 `auth.registration.registered_at_ms = 1794647109242` |
| 17:05:36 | **推理成功**（HTTP 200，stream，56 out tokens） | `/admin/logs` 环形缓冲 `seq=15611` |
| 17:07:04 | **推理成功**（HTTP 200，stream） | `/admin/logs` `seq≈15612`；`/status` 的 `last_success=17:07:04`、`success_count=2` |
| **17:45:05** | 凭据文件被写入（**一次续期成功**） | 文件 `mtime=17:45:05` |
| ~17:46:13 | 该文件里 access_token 的签发时刻 | JWT `iat=1794649573` |
| **18:15:05.010** | 该 access_token 到期 | `auth.oauth.expires_at_ms = 1794651305010` |
| 18:16:13 | JWT 自身的 `exp`（与上一行差 68 秒，同一份令牌） | JWT `exp=1794651373`；`exp-iat = 1800` 秒 |
| 18:50:46 → 18:56:32 | 三次请求全部 503，`refresh_token` **被上游拒绝** | `/admin/logs` 三条 503；`err_total` 递增 |
| ~18:56 | 连续 3 次续期失败 ⇒ 池子按设计禁用该号 | `reason=凭证续期连续失败 3 次` |

**关键推论**：17:45 的那次续期买到 30 分钟。按代码里的提前量公式（`§3.3`），
18:05 那一轮就应该再刷一次 —— 而它没有成功。所以**失败点落在 17:45 之后、18:15 之前**。

---

## 3. 涉及的事实与代码位置

### 3.1 账号与凭据

```
uid            = cmwg6l3pu2dxrr53jyqrpz6uo
nickname       = +86 192****4007
文件            = auths/phanthywork/phanthywork-cmwg6l3pu2dxrr53jyqrpz6uo.json
credits        = 3299
文件结构        = { "account": {uid, nickname}, "auth": {oauth, device, registration, inference, ...} }
refresh_token  = 64 字符 base64url（**值不入本文**）
```

同目录另一个号（**同日 13:04 已死**，同一形态）：

```
uid     = cmwfxhqzl12tkr53jjqq3dhad
nickname= +86 192****5621
credits = 6000
reason  = 自动禁用：账号层失效（一次即摘）… 额度刷新：账号层失效（登录已失效 / 账号已被停用）
```

`auths/phanthywork/_disabled-20261114/` 下还有 **7 个**更早的号，其 `README.md` 写着
「refresh_token **全部**回 400 invalid_grant（逐个实测）」。

⇒ **这是第 9、第 10 个以同样方式死亡的 phanthywork 号。**

### 3.2 上游对续期的直接回答（本轮实测，逐字）

```
POST https://code.phanthy.com/oauth/token
Content-Type: application/json
User-Agent: phanthy-code-cli/2.1.0
x-phanthy-client-source: desktop
body: {"grant_type":"refresh_token","client_id":"phanthy-code-cli","refresh_token":"<文件里的值>"}

→ HTTP 400
{"error":{"code":"invalid_grant","message":"Refresh token is invalid or expired."}}
```

**带 `scope` 与不带 `scope` 的结果完全相同** ⇒ 不是 scope 的问题。

### 3.3 续期的时间参数（决定"什么时候该刷"）

| 常量 | 值 | 位置 |
|---|---|---|
| access_token 寿命 | **1800 秒**（实测 `exp-iat`） | — |
| `RefreshSkew` | 寿命一半 ⇒ **900 秒** | `internal/phanthywork/login.go` `RefreshSkew` |
| `defaultRefreshScanInterval`（扫描周期） | **10 分钟** | `internal/phanthywork/jobs.go:76` |
| `refreshScanBuffer`（提前量地板缓冲） | **2 分钟** | `internal/phanthywork/jobs.go:105` |
| 实际提前量 | `max(900s, 600+120=720s)` = **900 秒** | `jobs.go` `needsRefresh` |
| `maxRefreshFails` | **3**（连续失败即禁用） | `internal/pool/health.go:28` |

⇒ 剩余寿命 ≤ 900 秒时该刷；扫描周期 600 秒 ⇒ **每个令牌寿命内至少会碰到 2 次扫描**。
所以"没赶上扫描"解释不了这次失败。

### 3.4 代码地图（接手时按需读，不必通读）

| 关注点 | 文件:行 |
|---|---|
| 该不该刷 | `internal/phanthywork/jobs.go` `needsRefresh`（约 :276） |
| 后台扫描本体 | `internal/phanthywork/jobs.go` `runRefresh`（约 :173） |
| 候选账号枚举 | `internal/phanthywork/jobs.go` `refreshCandidates`（约 :316） |
| 续期请求与错误分类 | `internal/phanthywork/client.go` `RefreshTokens`（约 :786）、`parseTokenResponse`（约 :865） |
| 写回内存 + 落盘 | `internal/phanthywork/login.go` `applyTokenSet`（约 :715）、`a.gate`（并发闸） |
| 落盘实现 | `internal/phanthywork/credential.go` `saveAuthFile`（约 :1008） |
| 加载（**设置 FilePath**） | `internal/phanthywork/credential.go` `LoadDir`（约 :896，`a.FilePath = p` 在 :927） |
| 终态分类 | `internal/phanthywork/errors.go` `authExpiredMarkers`（约 :142）、`IsRefreshExpired`（约 :292） |
| 连续失败计数 | `internal/pool/health.go` `NoteRefreshFailure`（约 :137） |
| 出站路径的续期调用 | `internal/server/handler.go` `refreshCredential`（约 :3786）、`noteRefreshFailure`（约 :3823） |
| 装配：活凭证注入 | `cmd/server/phanthyworkcredsource.go` `phanthyworkCredSource`（:34）、`phanthyworkUIDEnumerator`（:69） |
| 装配：账户同步 | `cmd/server/phanthyworkcreds.go:75`（调 `phanthywork.LoadDir`） |
| 配置默认 | `cmd/server/config.go:2166`（`PhanthyworkAuthDir` 缺省 = `<auth_dir 原值>/phanthywork`） |

---

## 4. 已排除项（**请不要重做**，每条都带证据与复现命令）

### 4.1 ❌「后台续期任务根本没注册」

- **判定依据**：`Jobs()` 只在 `p.authDir == ""` 时返回 nil（`jobs.go:135` `refreshInterval`）。
- **已核**：`config.json` 只有全局 `"auth_dir": "./auths"`，**没有** `phanthywork_auth_dir`；
  但 `config.go:2166-2171` 会把它缺省成 `<AuthsBase>/phanthywork` ⇒ `authDir` **非空** ⇒ 任务已注册。
- **旁证**：17:45:05 确实发生过一次成功续期（文件被写 + JWT `iat` 吻合）—— 只有主代码路径能写它。

### 4.2 ❌「轮换后的 refresh_token 没落盘」（**用户的核心假设**）

- **判定依据**：`saveAuthFile` 在 `FilePath == ""` 时静默跳过（`credential.go:1009`），
  而落盘只在 `FilePath` 非空时发生。
- **已核**：生产加载路径是 `phanthywork.LoadDir`（`cmd/server/phanthyworkcreds.go:75`），
  它**设置** `a.FilePath`（`credential.go:927`）⇒ 生产凭证的 `FilePath` 非空。
- **新增判据已通过**：`TestRefreshRotationLandsOnDisk`（`internal/phanthywork/refresh_persist_test.go`）
  造一份 `FilePath` 有值的凭证、假上游回 `refresh_token="refresh-rotated"`，
  续期后**读回文件**断言是新值 ⇒ **通过**。
- **复现**：`go test ./internal/phanthywork/ -run TestRefreshRotationLandsOnDisk -count=1 -v`

> ⚠ 但本轮确实在**同一片区域**修掉一个真隐患：以前**整个包没有任何判据检查过磁盘**
> （四条既有断言只断言内存里的 `*Auth`，而夹具 `FilePath` 恰好为空 ⇒ `saveAuthFile` 静默跳过）。
> 已补判据，并让 `applyTokenSet` 在"轮换了却落不了盘"时**报错**。见 §7。

### 4.3 ❌「拿错号 / 串了凭据」

- 两个号的 `refresh_token` 前缀与值都不同（`8h9Q9lQkWj-P…` vs `IUmqBfkfHvB_…`）。
- 枚举器 `phanthyworkUIDEnumerator` 用的是 **`p.ListFor`**（带 provider 过滤、**不过滤禁用/冷却**），
  不是会滤掉禁用号的 `AvailableUIDsFor`（`cmd/server/phanthyworkcredsource.go:64-77` 的注释专门写了这条）。

### 4.4 ❌「scope 传错导致 400」

带 scope / 不带 scope 打上游，响应完全相同（§3.2）。

### 4.5 ❌「5xx 被误判成终态，抖一次就永久废号」

`RefreshTokens` 的分类（`client.go:817-839`）：`200-2xx` 成功；`400 + invalid_grant 等 marker`
⇒ 终态；**`429 / ≥500` ⇒ 退避重试**（最多 3 次）；其它 4xx ⇒ 立即失败但**不是**终态。
另有判据 `TestRefreshServerErrorIsRetriable`（本轮新增）钉住"5xx 不是终态"。

### 4.6 ❌「账号没被池子用到 / 界面读数错」

`success_count=2`（17:05:36、17:07:04 两次真实 200 流式推理）证明链路通、读数来自池内活对象。

### 4.7 ⚠ 已修但**与本次死因无关**的一个真 bug（记录在此避免重复）

`pool.Enable` 原来不复位 `refreshFails` ⇒ 手动"启用"后**下一次失败立刻再禁用**
（真机实测：18:50 启用 → 18:56 又禁用）。**已修**（`internal/pool/pool.go` `Enable`）。

---

## 5. 未解的核心问题与候选假设（**这是你要干的活**）

> 核心问题：**上游为什么在 17:45 之后开始拒绝这个 refresh_token？**

按可能性排序。每条都给了**决定性实验**——请优先做能一次性杀掉多条的那种。

### H1（最可能）共享凭据的**轮换竞态**

同一账号若同时登录在**官方 PhanthyWork 桌面端**（本机就有：`D:\software\PhanthyWork-windows-x64`），
两边各自续期会互相把对方的 `refresh_token` 换掉。

**本仓自己记过这条教训**（`docs/agent2api-蒸馏报告.md` §L2）：

> 共享凭证（网关与桌面客户端共用 refresh_token）的一方必须拒绝主动刷新 ——
> 网关与桌面端共用同一个 refresh_token，网关侧刷新会造成**轮换竞态**。

**决定性实验**：
1. 问用户该手机号（`+86 192****4007`）是否正在官方桌面端登录着。
2. 若在：退出桌面端 → 重新登录网关侧 → 观察能否活过 **3 个令牌周期（≈90 分钟）**。
3. 若不在：直接否掉 H1。

### H2 上游侧停用 / 风控记账

服务端主动吊销（判定滥用），与"推理请求形态"有关。

**决定性实验**（干净对照）：
- 重新登录一个新号 → **完全不做推理**，只让它静默续期 → 看能否活过 3 个周期。
  - 能 ⇒ 死因与**推理流量**有关（转 H4）。
  - 不能 ⇒ 登录后即被标记（转 H5）。

### H3 落盘仍是问题（**理论上已排除，但缺生产侧直接证据**）

§4.2 的判据是合成测试。生产侧缺一条直接证据：**17:45 写盘时，磁盘上的值是否真的被换成了新的**。

**决定性实验**（低成本、强烈建议做）：
在 `applyTokenSet` 落盘前后各打一行日志，内容是 refresh_token 的**指纹**
（`sha256 前 8 位十六进制`，不泄露原值），例如：

```
phanthywork: uid=xxx 续期 refresh_token 指纹 3f9a1c2d → 7b4e08a6（已落盘）
```

- 指纹**变了** ⇒ 轮换被正确持久化 ⇒ H3 死。
- 指纹**没变** ⇒ 上游没轮换（那 §4.2 的假上游行为与真实不同）⇒ 需要重新审视整个前提。

### H4 推理请求形态仍与官方不一致，触发连带风控

本轮已把 4 个 `desktop-inference-v2` 证明头补齐（见 `docs/protocols/phanthywork-出站请求对照表.md` §11），
且 17:07 的 200 就是补齐之后发生的。但仍可能有残余差异。

**决定性实验**：
用**当前二进制**发一次推理，把实际出站头逐项与官方 `_O8` 的 12 段 canonical 对照，
特别检查：`x-request-id` / `x-claude-code-session-id` 是否与签名时同源；
`x-phanthy-work-cli-device` 的 `osRelease` 是否与真实系统一致。

### H5 refresh 请求本身的形态被风控

`refresh_token` 续期只发 3 个头（`User-Agent` / `x-phanthy-client-source` / `Accept`）+ JSON body。
而官方桌面端续期时可能还带别的头（如设备指纹）。

**决定性实验**：读官方 sidecar 里续期那一跳的头构造，与本仓
`applyCommonHeaders`（`client.go:222`）+ `RefreshTokens` 比对。

### H6 上游对 refresh_token 设了**很短的寿命令牌**

即 `refresh_token` 本身只活 1 个 access_token 周期，那么"一刷新就换、换了还能用一次"就成了设计。

**决定性实验**：看登录响应的原始 body 有没有 `refresh_token_expires_at`（或类似字段）。
本仓当前**不解析**这个字段（`parseTokenResponse` 只取 `access_token` / `refresh_token` /
`expires_in` / `scope`）。若上游确实给了它，那 H6 立刻成立，并且处置方案完全不同
（需要比 access_token 更早地续、或接受"必须常驻续期"）。

> 这条值得**最先做**：它只需要一次登录响应的原文，成本最低，且能直接把问题定型。

---

## 6. 复现与纪律（**做实验前必读**）

### 6.1 危险操作：不要随便手工打 OAuth 端点

`refresh_token` 是**轮换**的。若用 curl 直接打 `/oauth/token` 并成功，
旧 token 立刻作废，而**新的那个只在你的 curl 输出里** —— 不写回文件、不重启进程，
账号就进入"磁盘旧 / 内存更旧 / 上游已换"的三方不一致，比现在更难救。

- **只读排查**用 `/admin/logs`、`/status`、`/ui/*`。
- 需要真刷时，走**网关自己的路径**（后台任务或发一次推理），它会正确落盘。
- 若确实要手工 curl：**成功后必须**把新的 `access_token` / `refresh_token` /
  `expires_at_ms` 写回凭据文件，**然后重启 7863**（让池子从磁盘重载）。

### 6.2 常用命令（只用 GET，安全）

```powershell
$cfg = Get-Content D:\project_GIT\workbuddy2api\config.json -Raw | ConvertFrom-Json

# 池内状态（含 disabled / reason / credits / last_success / err_total）
curl.exe -s -H "Authorization: Bearer $($cfg.admin_token)" http://127.0.0.1:7863/status

# 请求日志环形缓冲（JSON，items[] 里含 status / uid / provider / at）
curl.exe -s -H "Authorization: Bearer $($cfg.admin_token)" http://127.0.0.1:7863/admin/logs

# 三条探针
curl.exe -s http://127.0.0.1:7863/livez
curl.exe -s http://127.0.0.1:7863/readyz
```

```powershell
# 跑相关判据
go test ./internal/phanthywork/ -count=1
go test ./internal/pool/ -count=1
go test ./cmd/server/ -count=1
```

### 6.3 ⚠ 进程日志只打到控制台

`start.bat` **不重定向** stdout/stderr（文件里明确写了这一条）。所以
`log.Printf` 的东西（后台续期的成功/失败行）**没有落盘**，`logs/server.log` 最新只到 11-12。

⇒ 想让后台续期可观测，必须**先把日志落盘**（`start.bat >> logs\server.log 2>&1`），
否则你只能靠 §5-H3 那种"往代码里加指纹日志"的办法。

### 6.4 环境

- 服务：`127.0.0.1:7863`，由 `start.bat` 拉起（`bin\wb2api-server.exe`）。
- 改代码后**必须重新编译并重启**，运行中的进程不会自动加载新二进制。
- 部署：`go build -o bin\wb2api-server.exe.tmp .\cmd\server` → 停 7863 → `Move-Item` → `.\start.bat`。
- git 已配全局代理 `http://127.0.0.1:10808`（否则 push 会被 reset）。

---

## 7. 本轮已改动的代码（避免与你冲突 / 重复）

`master = 9721d4d`，相关三个提交：

| 提交 | 内容 |
|---|---|
| `9f4f402` | ★ 推理路径补 4 个 `desktop-inference-v2` 证明头（两跳链路的更正）——**真机验证 200** |
| `1c959ed` | ① `pool.Enable` 复位 `refreshFails`/`sessionDeadStreak`（"启用等于白按"）② `ErrRefreshExpired` 包裹上游原文（`invalid_grant` 现在可辨）③ 探针三兄弟 `/livez` `/readyz` |
| `9721d4d` | ① 新增**落盘判据** `TestRefreshRotationLandsOnDisk` ② `applyTokenSet` 在"轮换了但落不了盘"时**报错** ③ 测试夹具改成与生产同形（`withFilePath`） |

新增判据（都可反向验证，注入即红）：

```
TestRefreshRotationLandsOnDisk         轮换必须写进磁盘
TestRefreshWithoutFilePathIsLoud       轮换了却落不了盘必须报错
TestRefreshExpiredCarriesUpstreamBody  终态错误必须带上游 code/message
TestRefreshClientErrorCarriesCode      invalid_client 必须可辨（本地配置错，重登无用）
TestRefreshServerErrorIsRetriable      5xx 不是终态
TestEnableResetsDisableProgressCounters 启用必须复位禁用进度
```

**若你要改 `applyTokenSet` / `saveAuthFile` / 探针，请先跑上面这些判据，别把它们改回红。**

---

## 8. 什么算"解决"

按价值排序，完成任意一条即可交付：

1. **能说出上游为什么作废那个 refresh_token**，并给出**可复现的证据**
   （不能是"大概是风控"）。
2. 给出一个**能让一个 phanthywork 号活过 ≥3 个令牌周期（≥90 分钟）**的配置或改动，
   并附观测记录。
3. 若结论是"上游策略所致、本地无解"，则给出**最小代价的运维处置**：
   例如失败时自动邮件/面板告警 + 一键换号，而不是等到用户发现"已过期"。

**不建议**的交付：再补一层重试。`invalid_grant` 是终态，重试只会每次多打一次上游。

---

## 9. 边界

- 本机 `auths/phanthywork/` 下已有 **9 个死号**（1 个在目录里被禁用 + 7 个在
  `_disabled-20261114/` + 1 个今天 13:04 死的）。**不要删它们** —— 它们是唯一的封号样本，
  `_disabled-20261114/README.md` 明确要求保留以便做封号机制分析。
- 需要新号请让用户提供（手机号 + 短信验证码，`+86 192****4007` 那个已死）。
- 不要为了"让判据变绿"去放宽 `maxRefreshFails` 或把 `invalid_grant` 改成可重试 ——
  那会把"死号被反复刷新"的老问题重新引入（`docs/protocols/` 与 `errors.go` 的注释都写过这条）。
