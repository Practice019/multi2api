# Codar 官方端点完整报告（T-344 实测）

> 探测时间 2026-11-08 06:1x · 账号两个（老 `user_1508…7985` / 新 `user_1927…8275`）·
> 方式：`curl.exe`（PowerShell 的 HTTP 栈在本沙箱内出站被断，curl 正常）· 只读端点 · 共 18 次请求 ·
> token 全部脱敏为前 8 位。

## 0. 结论摘要（直接回答用户的两个质疑）

| 质疑 | 答案 | 铁证 |
|---|---|---|
| **新账号"没有额度"** | **它有额度 —— 只是不在我们读的那个桶里** | `POST /v1/gettoken` ⇒ `{"token":500,"agent_token":0,"rewrite_token":0}`；`/v1/agent/ledger` ⇒ `{"change":500,"point_type":"general","note":"新用户赠送"}` |
| **1713 数值不对** | **1713 只是三个桶之一**（`agent` 桶），主额度是 `token` 桶 | `POST /v1/gettoken` ⇒ `{"token":2500,"agent_token":1713,"rewrite_token":0}` |

**⇒ 根因一句话**：我们实现的额度探测用的是 `POST /v1/agent/balance`（它只返回 `agent_balance` 一个值），
而官方客户端读的是 `POST /v1/gettoken`（返回 **三个**桶：`token` / `agent_token` / `rewrite_token`）。
在 codar2api 的源码里，`/v1/agent/balance` 被明确标注为**兜底**路径（`b["partial"] = true`）：

```go
case "balance":
    out, e = call("POST", a.cfg.PayBase+"/v1/gettoken", M{"username": user, "token": 0})   // ★ 主路径
    if e == nil {
        acc["balance"] = M{"token": …, "agent_token": …, "rewrite_token": …}
    } else {                                                                             // 主路径失败才兜底
        if fallback, fe := call("POST", a.cfg.PayBase+"/v1/agent/balance", …); fe == nil {
            b["agent_token"] = obj(fallback["data"])["agent_balance"]
            b["partial"] = true          // ← 我们显示的是这条"partial"路径的值
        }
    }
```

## 1. 基址与鉴权

| 基址 | 用途 | 鉴权 |
|---|---|---|
| `https://api.claw.speedai.com` | 模型目录 / 账号信息 / 活动 / 用量统计 / 对话 | `Authorization: Bearer <JWT>` |
| `https://api.kuaipaper.com` | 登录 / 余额 / 积分流水 / 签到 / 邀请 / 兑换 | `Authorization: Bearer <JWT>`（个别端点用 `token:` 头，见 §2 备注） |
| `https://static.alicaptcha.com/v4/ct4.js` | 人机验证脚本 | — |

公共请求头（实测需要）：`Accept-Encoding: identity`（不带它部分端点会压缩导致解析失败）、`User-Agent: codar-desktop/0.3.3`（桌面端 UA）。

## 2. ★ 完整端点清单（实测）

| # | 端点 | 方法 | 基址 | 用途 | 鉴权 | 请求体 | 响应（实测） |
|---|---|---|---|---|---|---|---|
| 1 | `/v1/gettoken` | POST | pay | ★ **真实余额（主端点）** | Bearer | `{"username":"<uid>","token":0}` | `{"code":200,"token":2500,"agent_token":1713,"rewrite_token":0}` |
| 2 | `/v1/agent/balance` | POST | pay | 余额（**兜底**，只有 agent 桶） | Bearer | `{"username":"<uid>"}` | `{"code":200,"message":"Success","data":{"username":"…","agent_balance":1713}}` |
| 3 | `/api/user/self` | GET | api | 账号信息 | Bearer | — | `{"success":true,"data":{"id":30892,"username":"…","display_name":"…","role":1,"status":1,"email":"","quota":0,"used_quota":0,"request_count":13,"group":"default","aff_code":"4I7R","aff_count":0,"aff_quota":0,…}}` |
| 4 | `/v1/agent/ledger` | POST | pay | 积分流水（含消耗明细） | Bearer | `{"username":"<uid>","page":1,"size":3}` | `{"code":200,"data":{"total":12,"items":[{"id":131088568,"change":-1,"point_type":"agent","note":"glm-5.3-flash \| token：输入 13，输出 10，缓存 0","expire_at":"…","created_at":"…"}]}}` |
| 5 | `/api/codar/activities/mine` | GET | api | 我参与的活动 | Bearer | — | `{"success":true,"data":[]}` |
| 6 | `/api/codar/activities/free-model` | GET | api | 免费模型活动状态 | Bearer | — | `{"success":true,"data":{"visible":false,"status":"unavailable","activity_id":1,"name":"免费领取 AI 模型","model_ids":["deepseek-v4.1-flash","auto"],"duration_seconds":604800,"claimed_at":null,"free_until":null}}` |
| 7 | `/api/codar/activities/free-model/claim` | POST | api | 领取免费模型（**写操作，本轮未调用**） | Bearer | — | — |
| 8 | `/v1/promotion/info` | POST | pay | 邀请信息 | Bearer | `{"username":"<uid>","token":0}` | `{"code":200,"data":{"invite_code":"BB5VYS","invite_link":"https://kuaipaper.com/?i=BB5VYS"}}` |
| 9 | `/v1/promotion/stats` | POST | pay | 邀请统计 | Bearer | 同上 | （本轮未单独测；codar2api 与 info 合并进 `invite`） |
| 10 | `/v1/daily-sign/status` | POST | pay | 签到状态 | Bearer | `{"username":"<uid>"}` | 老：`{"code":200,"message":"Already signed","data":{"isSigned":true,"retry_after":66837}}` · 新：`{"isSigned":false,"retry_after":null}` |
| 11 | `/v1/daily-sign` | POST | pay | 签收（**写操作，未调用**） | Bearer | `{"username":"<uid>"}` | — |
| 12 | `/v1/codar/usage/daily` | GET | api | 每日用量统计 | Bearer | `?start_date=&end_date=`（≤31 天） | `{"success":true,"data":{"timezone":"Asia/Shanghai","start_date":"…","end_date":"…","generated_at":1794089839,"cache_ttl_seconds":30,"models":["deepseek-v4.1-flash","glm-5.3-flash","gpt-6-astra"],"summary":{"calls":13,"input_tokens":…}}}` |
| 13 | `/v1/codar/models` | GET | api | 模型目录（含 pricing / protocol） | **无需鉴权** | — | 15 个模型，`schema_version:2`（T-307/T-318 已实测） |
| 14 | `/v1/token/refresh` | POST | pay | 续期令牌 | Bearer | （codar2api）| 距到期远时 `400 {"code":400,"message":"Token is not close to expiration"}`（T-315 实测） |
| 15 | `/v1/redeem-code` | POST | pay | 兑换码 | Bearer | — | （写操作，未调用） |
| 16 | `/api/codar/announcements` | GET | api | 公告 / 版本 | Bearer | — | （本轮未测） |
| 17 | `/v1/user/verify_ip` | POST | pay | IP 验证（登录第二步） | **`token:` 头** | `{"username","phone","code"}` | （T-324 实现，未实机触发） |
| 18 | `/v1/verification-code` · `/v1/login` · `/v1/wechat/login/{qrcode,status}` · `/v1/bind-phone` | POST | pay | 登录相关 | 无 / 临时 token | — | T-322/T-324 已实测 |

## 3. ★ 额度相关端点的完整对比（谁是"真实可用额度"）

| 端点 | 返回的桶 | 老账号 | 新账号 | 说明 |
|---|---|---|---|---|
| `/v1/gettoken` | `token`（general）/ `agent_token` / `rewrite_token` | 2500 / 1713 / 0 | **500** / 0 / 0 | ★ 官方主端点，三个桶**分开记账** |
| `/v1/agent/balance` | `agent_balance` | 1713 | 0 | 只有 agent 桶 ⇒ 我们误当成了"全部额度" |
| `/v1/agent/ledger` | `items[].point_type` | 12 条（`agent`，多为 -1 消耗） | 1 条（`general`，+500「新用户赠送」） | 桶的区分在这里得到交叉印证 |
| `/api/user/self` | `quota` / `used_quota` | 都是 0 | 都是 0 | 该站没在用这两个字段（不要拿它当余额） |
| `/v1/codar/usage/daily` | `summary.calls` 等 | calls 13 | calls 0 | 用量口径，不是余额 |

**推断**（标注为推断，不是实测）：`token` = 通用积分（general 桶，签到/赠送/充值进这里），`agent_token` = 智能体专用积分（agent 桶，agent 类模型消耗）。
⇒ "总可用"是否等于 `token + agent_token + rewrite_token`，只有官方 UI 的显示口径能定 —— 见 §7 未决①。

## 4. 现有实现的偏差（★ 需修）

```
internal/codar/p1.go 的 RefreshQuota：
  用 POST {PayBase}/v1/agent/balance，只取 data.agent_balance
  ⇒ 只看到 agent 桶  ⇒ 新账号显示 0（真实 500 在 general 桶）· 老账号显示 1713（真实主额度 2500）
  ⇒ 与 codar2api 的**兜底**路径等价（它给这种情况打了 partial: true）
```

## 5. 修正建议（不在本轮实施）

1. `RefreshQuota` 改调 `POST /v1/gettoken`（body `{"username":uid,"token":0}`），解析三个字段；
2. 主值取 `token`（与"新用户赠送 500 落在 general"这一实测一致）；
3. 三个桶都拿得到时，界面上至少不要显示成"只有一个数"（可选：`agent` / `rewrite` 作为副注）；
4. 保留 `/v1/agent/balance` 作为兜底（`gettoken` 失败时），并在视图上标记为"部分数据"（对应 codar2api 的 `partial`）；
5. `QuotaView.Kind` 仍是 `credits`（单位没变，只是取值来源与口径变了）。

## 6. 原始响应（脱敏，可直接对照）

老账号：
```
[v1/gettoken]        {"code":200,"token":2500,"agent_token":1713,"rewrite_token":0}
[v1/agent/balance]   {"code":200,"message":"Success","data":{"username":"user_15083583411_7985","agent_balance":1713}}
[api/user/self]      {"success":true,"data":{"id":30892,"username":"user_15083583411_7985","display_name":"user_15083583411_798","role":1,"status":1,"request_count":13,"group":"default","aff_code":"4I7R",…}}
[v1/agent/ledger]    {"code":200,"data":{"total":12,"items":[{"id":131088568,"change":-1,"point_type":"agent","note":"glm-5.3-flash | token：输入 13，输出 10，缓存 0","expire_at":"2026-11-08T16:51:16Z","created_at":"2026-11-07T18:58:23Z"},…]}}
[v1/promotion/info]  {"code":200,"data":{"invite_code":"BB5VYS","invite_link":"https://kuaipaper.com/?i=BB5VYS"}}
[v1/daily-sign/status] {"code":200,"message":"Already signed","data":{"isSigned":true,"retry_after":66837}}
[v1/codar/usage/daily] {"success":true,"data":{"timezone":"Asia/Shanghai",…,"models":["deepseek-v4.1-flash","glm-5.3-flash","gpt-6-astra"],"summary":{"calls":13,…}}}
```
新账号：
```
[v1/gettoken]        {"code":200,"token":500,"agent_token":0,"rewrite_token":0}     ← 有额度！
[v1/agent/balance]   {"code":200,"message":"Success","data":{"username":"user_19275734986_8275","agent_balance":0}}
[v1/agent/ledger]    {"code":200,"data":{"total":1,"items":[{"id":131091480,"change":500,"point_type":"general","note":"新用户赠送","expire_at":null,"created_at":"2026-11-07T21:58:54Z"}]}}
[v1/daily-sign/status] {"code":200,"message":"Not signed","data":{"isSigned":false,"retry_after":null}}
[v1/codar/usage/daily] {"success":true,"data":{"models":[],"summary":{"calls":0,"input_tokens":0,"output_tokens":0,"cached_tokens":0,…}}}
[api/user/self]      {"success":true,"data":{"id":30941,"username":"user_19275734986_8275","request_count":0,"aff_code":"7v8Q",…}}
```

## 7. 未决

1. **"总可用额度"的官方口径未定**：三个桶是分开记账的（`point_type` 有 `agent` / `general`），
   官方 UI 把哪个当"余额"没有实测证据 ⇒ 修实现前建议先看一眼官方网页/客户端的余额显示。
2. `/v1/promotion/stats`、`/api/codar/announcements`、`/v1/redeem-code` 本轮未测（前者未单独测，后两者是写操作/低优先）。
3. **本沙箱内 PowerShell 的 HTTP 栈出站被断**（`unexpected EOF`），但 `curl.exe` 正常 ⇒
   以后台探测一律用 curl（这条值得记住：它能让"探测"继续做下去，而不必误判成"上游挂了"）。
4. 新账号未签到（`isSigned:false`）⇒ 签到后应能再拿积分（签到入口在管理台已实现）。
