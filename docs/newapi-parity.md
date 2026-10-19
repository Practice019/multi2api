# 对照 new-api 的逐模块结论

⚠ 本文件是「把成熟项目的实现抄过来替换我们的」这件事的**决策记录**。
每决定一项就来这里加一行 —— ⚠ 目的：**避免重复对照**，也避免"为抄而抄"。

知识库入口：`python tools/ovq.py <ls|tree|read|grep|find> …`（DSH 的 MCP 断线时用这个直连）
主参照：`viking://resources/repos/QuantumNous/new-api/…`

## 结论表

| 序 | 模块 | 结论 | 依据 |
|---|---|---|---|
| **1** | 选号 + 重试 | ⚠ **我们早抄过了 ⇒ 不换** | `pool.go` 的 `PickForTier` 注释写着"对标 new-api 的 retry→降档"，且已在 `handler.go:1488` **接线**；`service/channel_select.go:114 CacheGetRandomSatisfiedChannel` / `RetryParam` 的语义与我们一致 |
| **2** | 冷却 / 熔断 | ⚠ **我们更细 ⇒ 不换** | 见下面「冷却对照」 |
| **3** | 失败分类 | ⚠ **我们更细 ⇒ 不换**；但**判决留痕该抄、已抄** | `internal/gateway/errorclassifier.go:99` 六类 vs 它的 `ErrorKind`；留痕见 `logbuf.Entry.Decision`（依据 `service/request_policy.go:19` + `api.ts:7`） |
| **4** | 额度 / 配额视图 | ⚠ **价值低 ⇒ 不换** | 它是**多用户计费网关**（user/token/channel 三层额度 + 预扣 + 结算 + 充值）；我们是**单用户自用**（只有账号额度一层）。真正相关的只有"**怎么判断一个渠道还有额度可用**"，而那条**第 1 项已查过**（`abilities` 表 ⇒ 从候选消失） |
| **5** | 请求日志 / 统计 | ⚠ 未查 | — |
| **6** ⚠ | **请求体转换失败的处理**（清单外，从 `relay/` 目录发现） | ⚠✅ **该抄** | 见下面「转换失败对照」 |

## 转换失败对照（第 6 项 · ⚠ 已确认我们缺）

new-api（`relay/convert_request_error.go` · `relay/param_override_error.go`）：

- 转换失败一律用**专门错误码**（`ErrorCodeConvertRequestFailed` /
  `ErrorCodeChannelParamOverrideInvalid`），而不是笼统的 500
- ⚠⚠ **一律带 `types.ErrOptionWithSkipRetry()`** —— "这类错误重试没有意义"是**显式声明**的
- ⚠ 若损失可诊断（`types.ConversionLossError`）⇒ `info.RecordConversionDiagnostics(c, loss.Diagnostics)`
  把诊断记下来，再返回 400

我们（`internal/minimax/provider.go:164` · `client.go:438`）：

```go
anthroBody, err := anthroconv.OpenAIToAnthropic(body)
if err != nil {
    return gateway.ChatStream{}, fmt.Errorf("minimax: 请求体转换失败: %w", err)   // ⚠ 裸 error
}
```

⇒ ⚠ 出口层分类靠 `classifyErr(...)`（按 **status code + body**），而转换失败是**本地**失败、
**没有 status code** ⇒ ⚠ 可能落到"未分类"⇒ 被当**可重试** ⇒ ⚠ **每次请求白换 N 个号**（MaxRotate 次）。

⚠ 我们**有**"终态"机制，但只在**另一个域**：`internal/cline/errors.go` 的 `ErrRefreshExpired`
（凭证**续期**的终态）。⇒ **两个域都有"终态"概念，而请求转换这个域没标。**

**该做的**：给转换失败打一个明确分类（送进 `ErrorKind` 体系，与 `ErrKindContentBlocked`
那种"不该换号"同类），⚠ 并在判决留痕里体现（`convert_failed:system`）。

## 冷却对照（第 2 项）

new-api（`service/channel.go` 等）：

- 状态只有三档：`ChannelStatusEnabled` / `ManuallyDisabled = 2` / `AutoDisabled = 3`
  （`common/constants.go:269-270`）
- `DisableChannel(channelError, reason)` ⇒ `UpdateChannelStatus(…, ChannelStatusAutoDisabled)`
- `ShouldDisableChannel(err)`（`service/channel.go:72`）**按错误类型**决定封不封
- ⚠ `ShouldEnableChannel`（:94）第一行就是
  `if !common.AutomaticEnableChannelEnabled { return false }`，
  而 `common/constants.go:135  var AutomaticEnableChannelEnabled = false`
  ⇒ **它默认不自动恢复**（要靠探活或人工）

我们（`internal/pool/pool.go`）：

- `until`：即时冷却（`CoolSoft` = 429 短冷却；`CoolHard` = 额度耗尽，到上游给的重置时刻）
- `breakerUntil` + `retryCount`：连续失败 **熔断 + 指数退避**
- `softStreak` + `softRateModel`：**429 的模型级指数退避，且持久化**
  （注释写明：不落盘的话一次重启就把退避指数清零，而上游的限流并未因我们重启而消失）
- `fails`：连续失败计数，跨入口累计，成功 / 熔断 / 统一复活时清零

⇒ **我们在四件事上更强**：瞬时冷却、熔断退避、自动恢复、模型级限流豁免。
⇒ 唯一可借鉴的是"**按标签批量禁用 / 启用**"（`EnableChannelByTag` / `DisableChannelByTag`，
`model/channel.go:819/828`）—— ⚠ 但那是**运维操作**、需要界面入口，不是核心逻辑。

## 登录与账号（2026-10-19 起 · 用户要求"把 new-api 的登录系统抄过来"）

用户原话：「直接把 newapi 的登录系统抄过来，但是实现可以最简单实现，也就是访问前端需要登录账号，
然后需要区分管理员账号和普通成员账号，这些都可以注册，但是为了保证批量注册发送请求
所以需要有一些防御 全部都抄 newapi。」⇒ 四条要求：R1 访问前端要登录 · R2 管理员/成员分级 ·
R3 可注册 · R4 反批量防御。逐项对照（出处 = new-api 文件:行）：

| new-api 的点 | 出处 | 我们的落地 | 抄/不抄与理由 |
|---|---|---|---|
| 角色 `RoleCommonUser=1` / `RoleAdminUser=10` | `common/constants.go:199-200` | `internal/account` 同值常量 | **抄**数值：将来加级别不用迁移，且 `myRole > targetRole` 判据天然一致 |
| 注册**服务端写死**普通用户 | `controller/user.go:283`（注释："明确设置角色为普通用户"） | 注册 handler 只传 `RoleMember`，不透传请求体里的 role | **抄**：否则第一个请求就能把自己注册成管理员 |
| 请求体字段约束 `min=8,max=128` | `model/user.go:82` | 口令 8–128；用户名额外白名单（3–32，字母数字与 `_-.@`） | **抄 + 收紧**：用户名会进日志与界面，放行 Unicode 会让零宽/全角变成看不见的冒充 |
| bcrypt 口令哈希 | `common/crypto.go:26/34`（`x/crypto/bcrypt`，默认 cost 10） | 同库，**cost 12** | **抄库、调参**：安全清单要求 bcrypt ≥12；单机控制台登录频率极低，多几十毫秒换 ×4 破解成本 |
| 会话是一条服务端记录（`SID/Version/UserAuthVersion/ExpiresAt/LastActiveAt/Status`） | `model/user_session.go` | `data/sessions.json` + HttpOnly cookie `wb2api_sid` | **抄模型、换存储**：它落 MySQL/Redis，我们 JSON 单文件（原则 2） |
| `user.auth_version` 改凭据即让旧会话全失效 | `model/user.go` + `middleware/auth.go` | 同上：账号记 `AuthVersion`，会话校验时比对 | **抄**：比"遍历会话表逐个删"少一类竞态 |
| ★ `AdvanceUserSessionAuthVersion`：提升版本踢掉别人，但**就地升级当前会话** | `model/user_session.go` | session.go 的同名语义 | **抄**：这是用户报障「轮换/改密后必须重启」的正解 |
| 会话签发频率上限 → 429 | `service/auth_session.go:468`（`AUTH_SESSION_ISSUANCE_LIMIT`） | 抄进签发路径 | **抄**：防止拿正确口令无限刷会话 |
| 路由级四件套：`CriticalRateLimit` + `anonymousRequestBodyLimit` + `TurnstileCheck` + `DisableCache` | `router/api-router.go:79/81` | 注册/登录端点同一套 | **抄结构**：Turnstile 默认关（要 CF 账号），另配**本地人机校验**默认开 |
| 阈值：关键路径 20 次 / 20 分钟；全局 web 120 / 180s | `common/constants.go:230-232`、`common/init.go:129-131` | 直接采这组数 | **抄**：有实测依据的默认值，不自己拍 |
| 401 稳定错误码 → 前端按码跳登录页 | `middleware/auth.go`（`writeDashboardAuthError`） | 错误体带 `code`：`auth_required`/`auth_invalid`/`session_expired` | **抄**：前端靠字符串判断会随文案漂移而失效 |
| 邮箱验证 / OAuth / passkey / 2FA | `controller/login_verification.go` 等 | 只留开关位，标"无 SMTP 不启用" | **不抄**：单机控制台没有这些前提，硬抄是负债 |
| PAT 的 scope 路由表、RBAC、多租户、额度订阅 | `middleware/auth.go`、`service/authz/` | 不做（三把钥匙已够分层） | **不抄**：那是多租户网关的复杂度 |

### 对照现状（本次要替换掉的东西，实测证据）

| 现状 | 位置 | 问题 | 状态 |
|---|---|---|---|
| `localExempt`：监听回环 + 上一跳回环 + 无转发头 ⇒ **直接放行** | `internal/admin/admin.go:644/669`（改造前） | "错令牌"与"不带令牌"是同一类请求 ⇒ 本机任何进程都是管理员（用户报障 ③） | ✅ **已删**（T4）：门禁改为「会话 或 admin_token」一律校验，按角色分流（`principalOf` / `memberPaths` / `canAccess`） |
| `/ui` 把 `api_key` **明文注入页面 HTML** | `internal/server/webui.go:84`（`injectKey`，改造前） | 打开页面即拿到 /v1 钥匙 ⇒ leak 不需要"偷" | ✅ **已删**（T5）：渲染器不再接收任何凭据；未认证给登录页；`?token=` 换成 HttpOnly cookie 后 302（凭据不进 URL 历史） |
| 前端把凭据存 localStorage（`wb2api.key`） | `internal/server/webui.html:1764/11876/11888` | 轮换后页面还用旧值 ⇒ 表现成"必须重启"（用户报障 ②） | ✅ **已删**（T7）：页面不再持有任何凭据，身份走 HttpOnly 会话 cookie |
| `GET /status` 用 **api_key** 鉴权却回账号池画像 | `internal/server/handler.go`（改造前 `withAuth(h.status)`） | 发给客户端的对话钥匙能读到昵称/积分/上游熔断态 | ✅ **已改**（T7）：改成控制台身份（会话或 admin_token）且必须**管理员**；api_key ⇒ 401，成员 ⇒ 403 `forbidden_role` |

**T7 收口后的实测**（隔离实例 + 本次构建的探针二进制）：

```
首次注册                 → role=10 bootstrap=true（第一个账号是管理员）
第二次注册               → role=1（成员）
成员会话   GET /status   → 403 {"code":"forbidden_role"}          ★
api_key    GET /status   → 401 {"code":"auth_required"}           ★（改造前是 200！）
admin_token GET /status  → 200
管理员会话 GET /status   → 200
GET /ui        无凭据    → 200 登录页（10126 B，含 id="form"，不含 probeSession）
GET /ui        管理员会话 → 200 控制台外壳（527506 B，含 probeSession，正文无密钥）
GET /ui?token=<正确>     → 302 Location:/ui + Set-Cookie wb2api_admin_token（Path=/ Max-Age=43200 HttpOnly SameSite=Lax）+ Cache-Control: no-store
GET /ui?token=<错误>     → 200 登录页（与"无凭据"逐字节相同）
```

**T5 收口后的实测**（隔离实例，真机 curl 等价调用）：

```
GET /ui                        无凭据          → 200  登录页（10125 B）
GET /ui?token=<正确>                          → 302  Location: /ui + Set-Cookie wb2api_admin_token（HttpOnly/Lax/Path=/）
GET /ui?token=<错误>                          → 200  登录页（与"无凭据"逐字节相同 ⇒ 不是判定 oracle）
GET /ui   Cookie: 令牌                       → 200  控制台外壳（523958 B）
GET /ui   Cookie: 陈旧令牌                    → 200  登录页 + 清掉那个 cookie
四类访问者的响应正文里：api_key / admin_token 出现次数 = 0
```

**T4 收口后的实测**（`WB2API_EXE=bin/wb2api-server-probe.exe node tests/frontend/verify_token_security_e2e.js`）：

```
① 无令牌：/admin/settings /admin/apikeys /admin/accounts 全 401 auth_required   PASS（改造前 3 个全 200）
② 错令牌：同样 401，且回执里不含任何密钥                                        PASS（改造前 200 + 掩码外泄）
④ 轮换 admin_token：新令牌立即可用、旧令牌立即 401                              PASS（改造前旧令牌仍 200）
剩余 2 红属后续任务：/ui HTML 仍注入密钥（T5）、config.example.json 缺 admin_token（T11）
```

轮换回执里那句「本机直连不受影响」也是旧语义，已随 T4 一并改掉（现在本机同样要凭据）。

### 三把钥匙的最终分工（本改造的骨架）

```
浏览器登录（人）  username + password → 会话 cookie wb2api_sid（HttpOnly/SameSite=Lax，24h 滑动）
管理接口（脚本）  admin_token         → Authorization: Bearer / ?token=（一次性换 cookie）
对话接口（客户端）api_key             → Authorization: Bearer（语义不变）
```

规则：只要配了任意凭据，`/ui` 与 `/admin/*` **一律校验**（含本机、含经反代）；
回环监听只保留为"少暴露一个端口"的部署建议，**不再当凭据用**。

角色分流（抄 new-api 的 `RoleMember=1 / RoleAdmin=10` 定义，不抄它的 RBAC 引擎）：
管理员全部放行；成员只放行 `memberPaths` 白名单（`/admin/apikeys`、`/admin/ui/manifest`），
其余默认拒绝 ⇒ 新增端点默认只有管理员能进。

⚠ 白名单从**前缀**改成**精确的 (方法, 路径) 对**（T8 实测发现的真洞）：
`/admin/apikeys` 前缀会连带放行 `POST /admin/apikeys/rotate-admin` —— 而那个端点的
回执里带**完整的新 admin_token**，成员调一次即可提权到管理员。现在两个 rotate
端点另加一层管理员硬判。

## 完工实测（T1–T12，2026-10-19）

### T8 归属与角色（`verify_role_split_e2e.js`，`--self-test` 6/6，`--start` 40 PASS / 0 FAIL）

	成员登录后：只见自己名下那 1 把 key（管理员建的那把看不见）
	越权四条（toggle/delete/reset/update）对别人的 key 全 403 forbidden_role
	★ 成员对"别人的 key"与"不存在的 id"回执**逐字节相同**（无存在性 oracle）
	★ rotate-admin / rotate-api-key 对成员 403，且拒绝回执里没有任何凭据字段
	★ 列表不回 member 标志（前端分不清"我的"还是"全部"）也有一条坏桩钉住

坏桩实测红条数：①不过滤归属 → 6 条 ②不判归属 → 3 条 ③成员可轮换 → 4 条
④成员可读 stats → 1 条 ⑤不回 member 标志 → 2 条（证明判别力来自断言本身）。

### T9 改口令免重启（`change_password_test.go` 4 条 + `verify_token_security_e2e.js` ④⑤）

	POST /admin/auth/password：验旧口令 → SetPassword（AuthVersion+1）
	                          → AdvanceAuthVersion(自己的 sid, 新版本号)
	★ 改完**当前**浏览器仍 200（用户报障"必须重启"的那一条）
	★ 另一台设备 401 session_revoked；旧口令登不进、新口令登得进

抄的是 new-api `model/user_session.go` 的 `AdvanceUserSessionAuthVersion` ——
它的用途与我们完全相同（改密后只保留当前会话）。

### T10 反批量四件套（实测）

	① 限速   20 次/20 分钟/IP：注册 1–20 → 200，**第 21–23 次 → 429**（带 Retry-After）
	         ⚠ 登录与注册是**两个独立桶**：打满注册额度不影响登录（实测 200）
	② 请求体 8KB：20KB → 413 body_too_large；正常体 → 200
	③ 人机校验（默认开）：未答/答错/复用同一题 → 400 captcha_required；
	         未启用的部署上发题端点 → 404 captcha_disabled（前端据此隐藏整块）
	         题面 SVG 只含 svg/rect/path（无 text/title/desc），答案不在响应里
	④ 会话签发上限（SessionStore 自带）：短时间疯狂登录 → 429

⚠ 探针起实例时**关掉**人机校验（算术题是给人看的，自动脚本答不了），
"开着"那条路由不关闭它的实例覆盖 —— 口径与计划一致："不开验证码时其余三层仍拦得住"。

### T12 回归挖出的真 bug（已修，`6e6bda1`）

	/ui?token=K        → 302 + Set-Cookie ✓
	/admin/*           → 200 ✓（管理面认令牌 Cookie）
	/admin/auth/session→ **401** ✗ ← 根因：只认会话 Cookie

令牌用户被前端判成"会话失效" ⇒ `location.replace('/ui')` ⇒ 又拿到控制台外壳 ⇒
**无限重载**。修法：`session` 端点接受令牌身份（回 `kind:"token"`、`user:null`），
`logout` 同时清令牌 Cookie。见 `internal/account/session_token_test.go` 的 6 条。

## 原则（写下来免得后人误读）

1. ⚠ **不是所有东西都该换** —— 每项先**证伪我们是否早就有了**（第 1、2、3 项都证明了有，且有的更强）。
2. ⚠ **抄逻辑、不抄数据模型** —— 它 MySQL + GORM，我们内存 + JSON 文件。
3. ⚠ **每处改动在注释里写出处**（文件 + 行号），否则后人不知道依据在哪。
4. ⚠ **抄也要留我们的接缝** —— 例如判决留痕只存 `reason:source`，`action` 交给 `Status` 表达。
