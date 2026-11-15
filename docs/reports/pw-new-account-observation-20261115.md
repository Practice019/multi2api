# 新号 `cmwh9skx34qioq83jwm7nling` 生命周期观察（2026-11-15）

> 目的：在「移除自动签到 + `/api/oauth/*` 改用官方 `j59` 极简头」之后，
> 用一个全新账号观察**它到底会被我们打几次上游、什么时候打、成不成功**。
> 方法：**只读本地痕迹**（凭据文件、checkin-log、`/status`、`/admin/ui/manifest`），
> 除了一次人为的面板操作外，不发任何补充探测。

---

## 0. ⚠ 本文包含一处对我自己先前结论的更正

我在观察过程中**先把结论说错了**，必须放在最前面说明：

| 我先前的说法 | 事实 | 错因 |
|---|---|---|
| 「我方对这个号发出的上游请求数 = 0」 | **错，当时已有 2 次** | 用 `$j.auth.reg.registered_at_ms` 读注册状态，但真实键名是 **`auth.registration`** ⇒ 读到 `$null` ⇒ 一直显示"空" |
| 「device 注册一次都没发 ✅」 | **错，11:25:13 就注册了** | 同上 |
| 「credits 从 0 变 6000 是个谜」 | 谜底就是注册之后紧跟的那次 summary | 同上 |

**这类错误在本会话已出现三次，是同一个模式：没读实际结构就假设字段名。**

| # | 错误 | 正确做法 |
|---|---|---|
| ① | 读 manifest 用 `intervalMs`，真实是 `interval_ms` | 误报"所有定时任务间隔为 0" |
| ② | 说 `/api/oauth/usage`、`/api/event_logging/batch`「本仓不知道」 | 先搜仓库（`quota.go:20`、`provider.go:184` 都有） |
| ③ | 读 `auth.reg`，真实是 `auth.registration` | 误报"设备从未注册" |

**⇒ 纪律：报告任何"状态为空/从未发生"之前，先把原始 JSON 打出来看一眼。**

---

## 1. 账号

| 项 | 值 |
|---|---|
| uid | `cmwh9skx34qioq83jwm7nling` |
| 昵称 | `+86 192****5217` |
| scope | `org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload` |
| 凭据文件 | `auths/phanthywork/phanthywork-cmwh9skx34qioq83jwm7nling.json` |

---

## 2. 真实时间线（全部来自落盘痕迹，可复核）

| 时刻 | 事件 | 证据 |
|---|---|---|
| 11:22:33 | 登录完成，凭据首次落盘 | 文件 mtime（已被后续覆盖） |
| 11:23:41 | access_token 签发（服务端时间） | JWT `iat` |
| **11:25:12.8** | ★ **设备注册成功** | `registration.registered_at_ms = 1794713112819`，且 `last_attempt_ms` 与它**完全相同** ⇒ 一次尝试即成，无重试 |
| **11:25:13+** | ★ **查询了一次 `activities/summary`** | `/status` 的 `credits` 从 `0` → `6000`（该字段只能由 `RefreshQuota → FetchSummary` 填充） |
| 11:38:41 | 进入"该续期"窗口（`exp - 900s`） | 由 `needsRefresh` 的 900 秒判据算出 |
| **11:41:14** | ★ **续期成功** | 文件被重写；`iat` 前进到 **11:42:24**，`exp` 到 **12:12:24**，`refresh_token` 末尾 `4gKiZHwK` → **`XteaqxHb`（已轮换）** |
| 11:51:40 | `phanthywork-refresh` 又跑了一轮 | manifest `last_run`；但 token 还很新 ⇒ **跳过，零请求** |
| 11:56:08 | 复查点 | `disabled=False`、`credits=6000`、`checkin-log` **0 条** |

**注**：`iat`（11:42:24）比文件 `mtime`（11:41:14）晚约 70 秒 —— 这是**时钟偏移**的体现
（`iat` 是服务端时间，`mtime` 是本机时间）。本包有 `NormalizeClockSkewMS` 就是在处理这件事。

---

## 3. 至今一共打过几次上游？**3 次**

| # | 请求 | 时刻 | 触发方 | 结果 |
|---|---|---|---|---|
| 1 | `POST /api/oauth/desktop-installations/register` | 11:25:13 | `ensureReady`（由 `RefreshQuota` 触发） | 成功（一次即成） |
| 2 | `POST /api/oauth/activities/summary` | 11:25:13+ | 同上，`FetchSummary` | 成功（credits=6000） |
| 3 | `POST /oauth/token` | 11:41:14 | 后台续期任务 | 成功（token 轮换） |

**其中 1 和 2 是同一次 `RefreshQuota` 造成的**（它先 `ensureReady` 再 `FetchSummary`）。

**1 和 2 的触发明面上是"人工"**：`RefreshQuota` 只有一个入口 ——
`POST /admin/accounts/quota/refresh`（面板上的「刷新额度」按钮）。
我核实过：

- `/status` 不查额度
- `/admin/ui/manifest` 不查额度（只读 `lf.Configured()`，纯本地）
- `app.js`（811 KB）里 `quota/refresh` 出现 12 次，但**没有任何定时器、页面加载时也不自动打**
- `refreshQuotas(` 只被那一个 HTTP 处理器调用

⇒ **最可能是观察期间在面板上点了一次「刷新额度」。** 这一步本身是正常功能，
但它说明一件事：**面板上一次点击 = 每个账号一次真实上游请求**（这里是 register + summary 两条）。

---

## 4. 这次观察证实了什么

### 4.1 ✅ 自动签到确实不再发生

`checkin-log` 里这个账号**至今 0 条**。而按旧行为，登录后 5 秒内就会有一条。

### 4.2 ✅ 续期能跑通，而且 `refresh_token` 会轮换

`iat` 前进、`exp` 后移、`refresh_token` 末尾从 `4gKiZHwK` 变成 `XteaqxHb`。
⇒ 续期链路（含 `applyOAuthFamilyHeaders` 那套极简头）在这个账号上**工作正常**。

### 4.3 ✅ 账号没有被禁用

`disabled=False`，且 summary 能成功返回 `credits=6000`（真实读到了上游数据）。

### 4.4 ✅ 设备注册在真机上能一次成功

`registered_at_ms == last_attempt_ms` ⇒ **没有重试、没有 409**。
这条在昨天是不可靠的（那时频繁看到注册相关的坑）。

---

## 5. 这次观察**没有**证实什么（别过度解读）

| # | 不能推出 | 为什么 |
|---|---|---|
| 1 | "这样就不会被封" | 才 33 分钟、3 次请求。昨天那个号 26 秒就被封，说明封禁可能来得很快；但也可能根本不来 —— **样本太小** |
| 2 | "极简头是关键" | 这次只发了 register / summary / refresh，**没发过推理**。极简头只影响前两者，推理那条还没被验证 |
| 3 | "注册顺序无所谓" | 注册确实晚了 2 分钟（11:23 登录 → 11:25 注册），但那是**人为操作**造成的延迟，不是设计 |
| 4 | "续期频率没问题" | 见 §6 —— 我们的续期仍然比官方频繁 |

---

## 6. 顺带量化出一个数字：**续期频率仍是官方的 ~30 倍**

| | 官方客户端 | 我们 |
|---|---|---|
| 续期触发 | **按需**：只在"剩余 ≤5 秒"或"上游回 401" | 后台任务每 10 分钟扫一次，剩余 ≤900 秒就刷 |
| 等效频率 | 只在真要用时（可能几小时不发一条） | **约每 25 分钟一次 ⇒ 约 2.4 次/小时 ⇒ 约 58 次/天** |

这个数字是 `U-4`（续期频率）讨论的直接输入 —— 按用户要求，**这条先不动**。

---

## 7. 下一步

1. **等**：让这个号继续只被"续期"碰，看它能不能活过几小时/一天。
   代码侧无需改动。
2. **然后**可以拿它做 V1/V2（无签名打 `/v1/messages` 与 `/v1/chat/completions`）——
   见 `pcl-phanthy-code-launcher-spec.md` §11。它是可牺牲的号，正好合适。
3. **顺手该做的**：**出站审计**。本次判断"打了哪 3 次"全靠落盘痕迹**反推**，
   而 register/summary 的具体请求体、响应码我们都看不到。
   如果当时有出站审计，我一开始就不会把"设备未注册"说错。
