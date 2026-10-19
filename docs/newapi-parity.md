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
R3 可注册 · R4 反批量防御。

⚠⚠ **同日晚些时候用户收口径**：「我们那个用户和管理员，我觉得可以只要一个管理员就行了，
只要一个账号。用户其实可以不用。」⇒ **R2（分级）与 R3（可注册）作废**，R1（必须登录）
与 R4（反批量）不变。本节的对照表与下面的实测**保留原样**（那是当时的事实与决策依据），
但**当前口径**以「⚠ 口径收窄：单账号管理员（2026-10-19）」一节为准 —— 凡与它冲突的，
都以那一节为准。逐项对照（出处 = new-api 文件:行）：

| new-api 的点 | 出处 | 我们的落地 | 抄/不抄与理由 |
|---|---|---|---|
| 角色 `RoleCommonUser=1` / `RoleAdminUser=10` | `common/constants.go:199-200` | `internal/account` 同值常量 | **抄**数值：将来加级别不用迁移，且 `myRole > targetRole` 判据天然一致。⚠ **口径收窄后**：`RoleMember=1` 只作为**只读兼容常量**留下（把盘上旧记录认出来再拒掉），没有任何建号路径会写它 |
| 注册**服务端写死**普通用户 | `controller/user.go:283`（注释："明确设置角色为普通用户"） | 注册 handler 只传 `RoleMember`，不透传请求体里的 role | **抄**：否则第一个请求就能把自己注册成管理员。⚠ **口径收窄后**：建号只剩空库首启那一次，服务端写死的是**管理员**；"不接受调用方指定角色"这条**更硬**了（请求体里带 role 在解码层就被拒 400，库非空后整个端点 403 `register_closed`） |
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
| `localExempt`：监听回环 + 上一跳回环 + 无转发头 ⇒ **直接放行** | `internal/admin/admin.go:644/669`（改造前） | "错令牌"与"不带令牌"是同一类请求 ⇒ 本机任何进程都是管理员（用户报障 ③） | ✅ **已删**（T4）：门禁改为「会话 或 admin_token」一律校验，按身份分流（`principalOf` / `canAccess`）。⚠ 口径收窄后 `memberPaths` 那张成员白名单**已整张删除**，`canAccess` 只判管理员 |
| `/ui` 把 `api_key` **明文注入页面 HTML** | `internal/server/webui.go:84`（`injectKey`，改造前） | 打开页面即拿到 /v1 钥匙 ⇒ leak 不需要"偷" | ✅ **已删**（T5）：渲染器不再接收任何凭据；未认证给登录页；`?token=` 换成 HttpOnly cookie 后 302（凭据不进 URL 历史） |
| 前端把凭据存 localStorage（`wb2api.key`） | `internal/server/webui.html:1764/11876/11888` | 轮换后页面还用旧值 ⇒ 表现成"必须重启"（用户报障 ②） | ✅ **已删**（T7）：页面不再持有任何凭据，身份走 HttpOnly 会话 cookie |
| `GET /status` 用 **api_key** 鉴权却回账号池画像 | `internal/server/handler.go`（改造前 `withAuth(h.status)`） | 发给客户端的对话钥匙能读到昵称/积分/上游熔断态 | ✅ **已改**（T7）：改成控制台身份（会话或 admin_token）且必须**管理员**；api_key ⇒ 401 `auth_required`。⚠ 口径收窄后**非管理员历史记录** ⇒ 401 `member_retired`（它在身份层就被拒，走不到 403；`forbidden_role` 只剩 fail-closed 的不可达分支） |

**T7 收口后的实测**（隔离实例 + 本次构建的探针二进制）：

> ⚠⚠ **本节数字已被取代**（同日晚些时候的"单账号口径"）：那时"第二次注册 → 成员"
> 与"成员 ⇒ 403"都是**当时的正确行为**，原样保留作为记录。**当前**口径与实测见
> 下面「⚠ 口径收窄：单账号管理员（2026-10-19）」一节。判据别读这里的第三行。

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

身份判据（⚠ 口径收窄后）：**只有管理员**能构成身份（`account.IsAdminRole` 是唯一判据）。
非管理员的历史记录在 `Authenticate` 与 `SessionStore.Lookup` 就被拒
（`ErrMemberRetired`），`account.Identify` 因此拿不到身份 ⇒ 请求在门禁的 **401** 分支
就结束了。原先那张"成员白名单"（`memberPaths` / `memberRules`）连同角色分流一起删除：
它当时防的是"新端点默认对成员开放"，而现在**根本没有第二种身份**，
留着只会让人以为"成员还有身份、只是端点少了几个"。

⚠ 白名单从**前缀**改成**精确的 (方法, 路径) 对**曾修掉一个真洞（T8 实测）：
`/admin/apikeys` 前缀会连带放行 `POST /admin/apikeys/rotate-admin` —— 而那个端点的
回执里带**完整的新 admin_token**，成员调一次即可提权到管理员。白名单删除后这条洞
在结构上不存在了；两个 rotate 端点的那层**管理员硬判保留**（第二层 fail-closed）。

## ⚠ 口径收窄：单账号管理员（2026-10-19，本次改动）

用户原话：「我们那个用户和管理员，我觉得可以只要一个管理员就行了，只要一个账号。
用户其实可以不用。」⇒ 这不是"暂时关掉注册"，而是**注册入口在这个部署形态里不存在**：

| 面 | 收窄前（v1.9.2 及本节上文） | 现在 |
|---|---|---|
| 建号路径 | `Store.Create(name, pass, role)`（带 role）+ 注册端点（库非空建成员） | **只剩**空库首启 `CreateBootstrap`（写死管理员）。`Create` 与 `SetRole` **删除** |
| 注册端点 | 库空 ⇒ 建管理员；库非空 ⇒ 建成员 | 库空 ⇒ 建管理员；库非空 ⇒ **整体关闭** `403 register_closed`（旧码 `register_disabled` 语义不同，刻意不沿用） |
| 角色 | 管理员 10 / 成员 1（可登录、有白名单） | 只有管理员。`RoleMember=1` 降为**只读兼容常量**，用于"认出来再拒掉" |
| 非管理员登录 | 200（进成员界面） | `401 member_retired` +「成员账号已取消，请用管理员账号登录」+ 服务端日志；**先验口令再看角色**（不构成用户名枚举口） |
| 非管理员旧会话 | 有效 | `SessionStore.Lookup` 拒 + `Revoke` |
| `GET /status` 非管理员 | `403 forbidden_role` | `401 member_retired`（身份层就断，走不到 403） |
| `/admin/apikeys` 载荷 | 带 `"member": !IsAdmin()` | 该字段**删除**（恒 false ⇒ 只是"这里还分角色"的假线索）；key 归属过滤保留为防御纵深 |
| 配置开关 `console.disables_register` | 关掉公开注册（不挡首启） | **已不改变行为**（键仍被解析、旧配置照收）；启动日志如实说明 |
| 登录页标志位 | `bootstrap` + `register` | 只剩 `bootstrap`（`register` 与占位符一起删除） |
| 前端 | 403 ⇒ 静默进"成员模式" | 没有第二态：401 ⇒ 回登录页；是管理员 ⇒ 全放行。**403 不再被吞成降级** |

**实测（本次）**：

```
空库首启 CreateBootstrap            → 角色=10（RoleName="管理员"）
库非空后再次建号                    → err=account: 管理员已存在，创建入口已关闭
库非空后注册（任意请求体，含 role）  → 403 register_closed（带 role 的连解码都过不了：400 bad_request），账号数不变
盘上 role=1 记录 + 正确口令         → 401 member_retired（文案「成员账号已取消，请用管理员账号登录」），会话数 0
盘上 role=1 记录 + 错误口令         → 401 bad_credentials（不泄露"这个用户名存在吗"）
同库 role=10 记录 + 正确口令        → 登录成功（旧数据兼容不是整库作废）
盘上 role 值                        → 未被改写（只在内存里归一化）
启动日志                            → 「账号库 … 里有 N 条**非管理员**历史记录（staff(成员（已取消）)）…」
类级静态判据                        → 扫 internal/ 与 cmd/ 全部非测试 Go 源，RoleMember/SetRole/"member"/register_disabled 零残留（含反空转自检）
```

`go test ./... -count=1` → 34 包 ok / 0 FAIL；`go vet ./...` 与 `gofmt -l .` 在干净
LF 检出树上零输出。

## 完工实测（T1–T12，2026-10-19）

### T8 归属与角色（`verify_role_split_e2e.js`，`--self-test` 6/6，`--start` 40 PASS / 0 FAIL）

> ⚠ **本条的部分判据已被单账号口径取代**（数字保留，是当时的实测）：
> "成员登录后只见自己的 key""成员越权 403""rotate 对成员 403""列表不回 member 标志"
> 这四条测的前提是"**存在成员这一种身份**" —— 现在成员身份取消，
> 它们不可复现，对应测试改写为语义相反的更强判据（非管理员历史记录连**身份**都
> 拿不到 ⇒ 401），见上文「⚠ 口径收窄」一节与 `internal/admin/apikey_single_admin_test.go`。
> 仍然成立的：key 的归属过滤（`ListFor`/`CanManage` 的边界）与"两个 rotate 端点
> 另加一层管理员硬判"。

	成员登录后：只见自己名下那 1 把 key（管理员建的那把看不见）
	越权四条（toggle/delete/reset/update）对别人的 key 全 403 forbidden_role
	★ 成员对"别人的 key"与"不存在的 id"回执**逐字节相同**（无存在性 oracle）
	★ rotate-admin / rotate-api-key 对成员 403，且拒绝回执里没有任何凭据字段
	★ 列表不回 member 标志（前端分不清"我的"还是"全部"）也有一条坏桩钉住

坏桩实测红条数：①不过滤归属 → 6 条 ②不判归属 → 3 条 ③成员可轮换 → 4 条
④成员可读 stats → 1 条 ⑤不回 member 标志 → 2 条（证明判别力来自断言本身）。
⚠ 单账号口径后：`member` 字段**删除**（恒 false），⑤ 那条坏桩随之消失；
①②③④ 对应的"成员视角"整体不存在，改由"非管理员不构成身份"的判据接管。

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

### 无头浏览器视觉 E2E 挖出的三条真缺陷（已修，`a53bb2b`）

新增 `tests/frontend/verify_login_visual_e2e.js`（自带 CDP + 截图 + 几何判据，带判别力
自检：好桩 0 红 + 三个坏桩各自让目标判据变红）。它一次跑出三条**静态阅读看不见**的缺陷 ——
三条都只在特定身份/时间窗里出现：

| # | 现象（探针实测） | 真因 | 修法 |
|---|---|---|---|
| ① | 成员会话里 `TypeError: Cannot read properties of null (reading 'accounts')` ⇒ 面板保持开着等 35s 也不刷新 | `/admin/accounts` 对成员 403（`admin()` 抛），而回落那句 `s.accounts` 里 `s` **也是 null**（`/status` 同样只给管理员）⇒ catch 自身再抛，**没有外层 try** ⇒ `refresh()` 整段中断，含 30s 轮询注册 | 成员**跳过**那次注定 403 的请求；回落改 `(s && s.accounts) \|\| []` |
| ② | 新建 API key 后 3.2s 列表仍是 0 条；**重开面板** 10ms 就看到 | 创建路径漏了 `writeInvalidate([EP_APIKEYS])`（删除/轮换都有，注释里还写着这条的代价）⇒ `loadAPIKeys()` 用 30s 旧快照重画 | 创建/更新后先丢缓存再拉列表 |
| ③ | 同一浏览器换身份后，「管理钥匙」按**上一个身份**的快照渲染（成员→令牌时文案自相矛盾；反方向会把管理员的 key 列表画给成员看） | `lazyCache` 的 key 只有逻辑名（`ep:/admin/apikeys`），却**落 localStorage 且跨导航存活**、TTL 30s ⇒ 键里没有身份 | 缓存加身份命名空间 `wb2api.pcache.<scope>.<key>`（`token` / `u:<id>:<role>` / `anon`），身份一确定就切，登出退回 anon |

修完该探针 **95 PASS / 0 FAIL**，自检 4/4 PASS；`console.error` / 未捕获异常 /
`Network.loadingFailed` 三条硬判据全空。第 ③ 条的反方向（管理员 → 成员）窗口只有 30 秒，
**人工几乎撞不到** —— 这是"必须有无头浏览器验收"的最好论据之一。

> ⚠ **单账号口径后的两处余波**（同日晚些：成员角色取消）：
>
> ① 与 ③ 里"成员会话 / 成员身份"这个**前提**没有了（没有第二种身份），所以那两条
> 具体现象不再可复现；但它们挖出的**两条修法仍然有效且必须保留**：
> "面板请求 403 时的回落不能自己再抛（`(s && s.accounts) || []` + 外层 try）"
> 与"面板缓存必须带身份命名空间（`u:<id>:<role>` / `token` / `anon`）"。
> 后者在单账号部署里表现为：**登出后退回 `anon`**、令牌与会话身份不共用一份缓存
> （换身份当场换缓存，不靠 30 秒 TTL 兜底）。
>
> ⚠ 更重要的一条：**403 不再被静默降级**。原先 403 会被前端解释成"进成员模式"
> （`applyMemberMode()`），于是"权限不足"看起来像"换了个界面"；现在只有
> "未登录（401 ⇒ 回登录页）"与"就是管理员"两种状态，`.member-mode` 与
> `applyMemberMode()` 一起删除。上表 ① 的"成员跳过注定 403 的请求"那一半也随前提消失，
> 留下的通用教训是：**前端不许把 403/404 吞成降级**，该报错就报错。

## 原则（写下来免得后人误读）

1. ⚠ **不是所有东西都该换** —— 每项先**证伪我们是否早就有了**（第 1、2、3 项都证明了有，且有的更强）。
2. ⚠ **抄逻辑、不抄数据模型** —— 它 MySQL + GORM，我们内存 + JSON 文件。
3. ⚠ **每处改动在注释里写出处**（文件 + 行号），否则后人不知道依据在哪。
4. ⚠ **抄也要留我们的接缝** —— 例如判决留痕只存 `reason:source`，`action` 交给 `Status` 表达。
