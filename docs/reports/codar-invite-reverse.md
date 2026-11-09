# Codar 邀请码端点逆向（T-352）

## 0. 结论

| 问题 | 答案 | 依据 |
|---|---|---|
| 有没有"绑定邀请码"的端点 | **没有**（桌面端也没有） | `codar.exe` 65.4 MB 全量字符串里 `invite_code` / `referrer_code` / `inviteCode` 命中 **0** 次；7+6 个候选路径全 404 |
| `referr` 165 次是什么 | **噪音**：`referrerPolicy` / `Referer:` / 一个第三方库字符串（`lame referral`） | 上下文片段见 §1 |
| 那 `+2000 被邀请奖励` 哪来的 | **注册时**通过邀请链接 `https://kuaipaper.com/?i=<CODE>` 注册才入账；已注册的号**补不了** | 与 T-351 的端点探测一致 |
| 新号还有别的拿点数路径吗 | **有**：桌面端有**活动体系**（我们只实现了 free-model 一个）—— 见 §3 | `/api/codar/activities/{catalog,invite-model,invite-model/claim}` |

**⇒ 对"新号肯定可以绑定"的判断：桌面端确实有**邀请相关 UI**，但那是"邀请好友领点数"（你邀请别人、
别人注册后你得奖励），**不是**"填别人的邀请码"。已注册账号没有补填入口 —— 这不是我们没实现，
是上游没有这个能力。**

## 1. 桌面端逆向（codar.exe）

关键词命中：`invite` 5 · `promotion` 1 · `referr` 165 · `invite_code` **0** · `inviteCode` **0** · `referrer` **0**

`referr` 的上下文（抽样，全是噪音）：
```
…BatchMode=yeslame referraldevice_secretlevel 3 resetsrmount errortimer…
…        referrerPolicy: null,
…rel: "noreferrer", icon: …
```
`invite` 的有意义命中（2 处）：
```
…/api/codar/activities/invite-model/api/cloud-computer/session-eventsregister…
…/api/codar/activities/invite-model/claim/api/sessions/capability-probe/subagents…
```

## 2. `/v1` 与 `/api` 路径清单

二进制里共提取 **439** 条路径（多数是该桌面端**自带 sidecar** 的 `/api/*`，与云端无关）。
筛出与计费/账号/活动相关的：

```
云端（api.claw.speedai.com）：
  /api/user/self
  /api/codar/activities/catalog                 ← ★ 新发现（活动目录）
  /api/codar/activities/free-model              /claim
  /api/codar/activities/invite-model            ← ★ 新发现（邀请活动）
  /api/codar/activities/invite-model/claim      ← ★ 新发现
  /api/codar/activities/mine
  /api/codar/announcements  /version  /dismiss
  /v1/codar/models   /v1/codar/usage/daily
  /v1/codar/remote/devices{,/list,/register}     （远程设备，与本议题无关）
计费面（api.kuaipaper.com）：
  /api/user/self   /v1/gettoken   /v1/agent/balance   /v1/agent/ledger
  /v1/promotion/info   /v1/promotion/stats
  /v1/daily-sign   /v1/daily-sign/status
  /v1/token/refresh   /v1/verification-code   /v1/bind-phone
  /v1/wechat/login/qrcode  /v1/wechat/login/status
  /v1/redeem-code                                 （T-344 已知）
  ⚠ 与"绑定邀请码"相关的路径：**一条都没有**
```

## 3. 桌面端的中文串证据（说明它有哪些"领点数"入口）

`邀请` 48 次 · `绑定` 46 次（多为"SSH 未绑定远程项目"/"账户绑定"这类无关语义）· `领取` 102 次 · `奖励` 6 次

与点数直接相关的 UI 文案：

```
「邀请好友领点数」 / 「邀请好友，一起获得点数」 / 「CODAR 邀请活动」
「分享真实任务领点数」 / 「分享任务奖励方案」 / 「发帖即送」 / 「10+ 赞」 / 「20+ 赞」
「提交作品链接」→「链接已提交，等待审核」
「每期每人限领一次」 / 「点击立即领取」 / 「从领取时刻开始计算 7 天」 / 「有效期内按 0 倍率计费」
「检查邀请状态失败」 / 「邀请信息不完整」
```

⇒ 桌面端有**三类活动**：`free-model`（免费模型，我们已实现）、`invite-model`（邀请活动）、**分享/集赞活动**
（提交作品链接、等审核、按赞数给奖励）。

## 4. 官方网页

未做（需要登录态；本轮已有二进制 + 云端实测两条更硬的证据，且用户已确认可删 bin 归档、优先要结论）。

## 5. 结论与后续建议

1. **绑定邀请码：上游不存在该能力** —— 桌面端、codar2api、云端探测三处一致。
   已注册账号无法补 +2000。想要那笔奖励只能在**注册时**走邀请链接。
2. **可以做的补全（真能拿点数）**：把活动体系补上 —— 至少加
   `/api/codar/activities/catalog`（目录）+ `/invite-model`（状态）+ 各自的 `/claim`，
   与现有 free-model 走同一套 `DailyAction`（前端会自动出按钮，就像签到那样）。
3. **不要做**：任何"绑定邀请码"的界面 —— 会画一个点了必然失败的按钮（与 T-351 的 `bind_supported:false` 一致）。

---

## 6. 追加：活动体系实测（T-352 第二轮，只读）

### 6.1 活动目录（`GET /api/codar/activities/catalog`，两个账号都 200）

```json
[{"key":"invitation","name_zh":"邀请好友","name_en":"Invite friends",
  "summary_zh":"邀请好友获得点数","summary_en":"Invite friends to earn points",
  "owner":"codar","display_mode":"custom_page","control_scope":…}, …]
```

⇒ `display_mode:"custom_page"` 说明：官方客户端为邀请活动做了**自定义页面**（不是通用领取按钮）
⇒ 这正是"桌面端有邀请 UI"的由来 —— 但它是**你去邀请别人**，不是"填别人的码"。

### 6.2 两个活动对两个账号的状态（实测）

| 活动 | 老账号 `…7985` | 新账号 `…8275` |
|---|---|---|
| `free-model` | visible=false · status=unavailable | visible=false · status=unavailable |
| `invite-model` | **visible=true · status=locked**（activity_id=1191） | **visible=true · status=locked** |

`invite-model` 详情：`{"name":"邀请 3 位新用户，领取模型 7 天体验","model_ids":["deepseek-v4.1-flash","auto"],
"duration_seconds":604800,"required_invites":3,"qualified_invites":0}`
⇒ **两账号都可见、都锁着**；解锁条件 = 邀请 3 位新用户（此刻 0/3）。
⇒ 即：**新号想拿更多，靠的是"邀请别人"，而不是"被别人邀请"。**

`activities/mine` = `[]`（两账号都未参与任何活动）· `announcements` = `[]`

### 6.3 "绑定邀请码"端点的完整探测记录（累计 13 个候选，全 404）

```
/v1/promotion/{apply,bind,redeem,invite-code}     404
/v1/invite/{apply,bind}                            404
/v1/invite-code/bind                               404
/v1/user/{bind-invite,invite}                      404
/v1/bind-invite                                    404
/api/user/invite                                   404
/api/codar/activities/invite-model/bind            404
/v1/invitation/bind                                404
对照（证明 404 判据可靠）：已知的 /v1/promotion/info 即使传空 username 也返回 200 + 业务错误
二进制侧：codar.exe 里 `invite_code` / `inviteCode` / `referrer_code` 命中 **0**
```

⇒ **三处独立证据（桌面端二进制 / codar2api 源码 / 云端探测）一致：codar 没有"绑定邀请码"能力。**

### 6.4 若要补全（真能拿点数/体验的）

1. `GET /api/codar/activities/catalog`（目录，含 display_mode/control_scope ⇒ 决定怎么渲染）
2. `GET /api/codar/activities/invite-model`（状态：visible/locked/required_invites/qualified_invites）
3. 各自的 `/claim`（写操作）：`/api/codar/activities/{free-model,invite-model}/claim`
   ⇒ 与现有 free-model 同一套 `DailyAction` 即可（前端自动出按钮，如签到那样）
4. **不做**任何"绑定邀请码"界面 —— 上游没有这个能力（本次逆向的结论）。

---

## 7. ★ 更正（T-353 / T-354）—— 上面 §0/§5「没有绑定端点」的结论是**错的**

**真端点在桌面端的【前端 JS】里**，不在二进制里：

```
文件：asar\renderer\assets\index-Cr4tsyUc.js（1.77 MB）

async function addUserInvite(session, inviteCode, captcha) {
  const response = await requestAuth("/v1/user/add-invite", {
    username: session.username, invite_code: inviteCode,
    lot_number: captcha.lotNumber, captcha_output: captcha.captchaOutput,
    pass_token: captcha.passToken, gen_time: captcha.genTime,
  }, authHeaders(session));
  assertSuccessCode(response, t("邀请码绑定失败"));
}
async function checkUserInvited(session) {
  const response = await requestAuth("/v1/user/check-invited", { username: session.username }, authHeaders(session));
  return response.data === true;
}
```

**实测确认（2026-11-08）**：

| 调用 | 结果 |
|---|---|
| `POST /v1/user/check-invited`（老账号） | `{"code":200,"message":"用户已被邀请","data":true}` |
| `POST /v1/user/check-invited`（新账号） | `{"code":200,"message":"用户未被邀请","data":false}` ⇒ **新号还能被邀请** |
| `POST /v1/user/add-invite`（非法码、无 captcha） | `400 请完成人机验证，刷新页面后重试` ⇒ captcha 是硬要求 |
| `POST /v1/user/add-invite`（非法码、假 captcha） | `500 添加邀请关系失败: 'result'` ⇒ 四字段被接受，卡在码本身 |

**微信绑定**：官方文案说「老用户首次绑定邀请码**并完成微信绑定**后同样计入邀请人进度」——
它影响的是"这笔邀请算不算邀请人进度"，**不是绑定动作的前置条件**（绑定路径上没有微信检查，
实测已越过 captcha 关直接进到"添加邀请关系失败"）。且微信是桌面端**本地 sidecar** 的能力
（`/api/connectors/wechat/*`），与我们无关。

## 8. ★ 教训（这条比结论本身值钱）

**逆向上游能力时，前端 JS 比 Go 二进制权威。**

T-351/T-352 搜了 `codar.exe`（65.4 MB）的字符串常量、搜了 codar2api 源码，两处都没有这个端点，
于是得出"上游不支持"的错误结论。实际原因：Go 二进制里路由常量会被**拆分/拼接/内联优化**，
不一定以完整字符串存在；而**前端 JS 里的 fetch 调用是完整的、带请求体形状的**。

⇒ 下次逆向的顺序应当是：① 前端 JS（asar/renderer/assets）→ ② 抓包 → ③ 二进制 → ④ 第三方实现。

## 9. T-354 的实现落点

```
internal/codar/invite.go        CheckInvited / BindInvite / MissingCaptchaFields
internal/codar/invite_page.go   邀请页（RenderInvitePage + handleInvitePage）：码/记录/绑定表单 + 阿里云 captcha
GET  /admin/codar/invite        返回 invited / invite_records / page_url / bind_supported:true
POST /admin/codar/invite/bind   绑定（缺 captcha ⇒ **本地** 400，不发上游请求）
GET  /admin/codar/invite-page   邀请页本体（webui 邀请面板里按 page_url 给出"打开邀请页"）
```
