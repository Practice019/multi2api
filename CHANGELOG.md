# 变更日志（Changelog）

本文件记录**本仓库相对于上游 `Sliverkiss/workbuddy2api` 的增量**。
上游版本自身的变化请关注原仓库的 Release / commit 历史；本仓库的主要功能版本
仍会跟上游对齐，只是本表会列出本仓库的额外提交与里程碑。

格式参考 [Keep a Changelog](https://keepachangelog.com/)，版本号不在本表里维护
（跟着上游走），日期格式 `YYYY-MM-DD`。

## v1.7.3 — 2026-10-05

> 本版主题：**修两个用户报障**（TRAE 签到"假成功" / raccoon「添加账号」打不开可用页面）
> \+ **一个跨全部上游的续期缺陷**。全部是修复，无新增上游、无配置项变更、
> 无破坏性变更。
>
> 三处缺陷有共同形态：**上游的业务失败是 HTTP 200 + body 里的业务码**，
> 而旧代码只看 HTTP 状态码 —— 于是失败被记成成功，界面显示绿色、历史表写 `ok`，
> 而实际什么也没发生。失败至少能被发现，假成功会一直骗到用户自己去核对。

### 修复 — TRAE 签到「签到不了」（closes #1）

用户报障（#1）：*trae的签到没生效*。实测复现，**两个缺陷叠加**：

**① `X-Device-Id` 发错了值**（每次都被上游拒绝）

签到接口要的"设备"是**账号 uid**，而本包一直发登录用的 32 位 hex 指纹
（`credential.DeviceID`，是 `login.go` 里 `randomHex32` 生成、参与登录 URL 的那个）。
端点、头名、body 全对，唯独这个**值**错了。真实上游三对照：

    X-Device-Id = uid(3929003642848586)   → {"code":0}      到手
    X-Device-Id = 32hex(ca2079…)          → {"code":9074}   拒绝
    不带该头                               → {"code":9004}   拒绝

设备维度的回执同样印证（`did_checked_in` 是设备级、`checked_in` 是账号级）：

    uid       → did_checked_in=true    ← 上游承认这台"设备"
    32hex     → did_checked_in=false   ← 不承认
    随机 16 位 → did_checked_in=false   ← 不承认

**② 业务失败被记成成功**

TRAE 签到的业务拒绝是 **HTTP 200 + `code != 0`**，而 `doJSON` 只在
`StatusCode >= 400` 时报错。于是：

    上游回 {"code":9074,"message":"当前参与用户太多"} → err == nil
    → 记 StatusOK → 界面"签到成功"、历史 `ok` → **积分一分没到账**

实测 9074 会**持续数分钟**（8/8 次、间隔 30 秒都不恢复），所以"重试一下就好"不成立。

改法：解析业务 `code`（9074 限流 / 9095 设备今日已签 / 9004 缺设备 / 1005 权益不足
分别可分类）；claim 后用**复查 status 的 `checked_in`** 判定成败（存在 `code=0`
但没真到账的情形）；补上 `enable` 守卫；`credits + extra_credits` 一并统计
（**额外奖励此前完全没统计**）。同一 bug 在后台定时任务里也有第四处，一并修。

实测（走真实网关）：`{"credits":200,"ok":true,"status":"already"}`。

### 修复 — raccoon「添加账号」打开的是**微信落地页**

用户报障：点「添加账号」后浏览器打开是空白/无用页。
（用户明确指出扫码与手机号都是正常路径，要修浏览器那条。）

根因：旧实现返回 `https://xiaohuanxiong.com/login/mp?code=<32hex>` ——
那是**二维码内容**，只能在微信里打开：

    官网主 bundle 里 `login/mp` 出现 **0 次**（只被当作二维码内容拼出来）
    微信 UA 与桌面 UA 请求它 → 返回**同一份 SPA 外壳**
    （3884 字节，路由表里没有 `mp`）

即"在电脑浏览器里打开它什么都没有"是**上游设计如此**，不是我们拼错了。

改法：改用商汤**官方 VS Code 扩展**自己的浏览器登录机制
（`Raccoon-VSCode/src/raccoonClient/raccoonClinet.ts` 的 getAuthUrl）：

    /login?appname=…&redirect=http://127.0.0.1:<端口>/raccoon/callback?nonce=…

官方页面上有**微信扫码**与**手机号短信**两个 Tab，过完阿里云滑块点授权后
把 `authorization_code` 回调到本机端口 —— 我们当场换凭证。
**滑块由官方页面与用户完成，我们不碰。**

实测（真实账号，端到端跑通）：

    回调收到  authorization_code=ac_TBFJ_…
    换凭证    HTTP 200 {"code":0,"message":"success","data":{"access_token":"eyJ…"}}
    user_info HTTP 200 {"code":0,"name":"RaccoonSophia","id":"7497524"}

`redirect` **没有白名单限制**。

⚠ 两个必须遵守的细节（都有实证理由）：

  - **绝不能带 `login_source=desktop`** —— 官网用它判断是否走
    `office-raccoon://` 深链；带了就**完全忽略 redirect**，永远等不到回调。
  - **防串号标识塞进 redirect 的 query（nonce），不能用 `state`** ——
    官网只转发 `authorization_code`，不复制其它参数。

⚠ 移除了 `QRLoginExt`：新 URL 编成二维码是**坏码**（手机扫会跳到
**手机自己的** localhost）。扫码能力没丢 —— 官方登录页自己就有扫码 Tab。

顺带说明：短信路径此前被认为"不可程序化"，准确说法是**"不能绕过滑块"** ——
`EncryptPhone`（AES-128-CFB）早已实现，实测加密后能通过，只差滑块；
现在滑块在官方页面里过，所以手机号登录可用。

### 修复 — 每个请求都续期（跨全部 10 个上游）

出口层判"该不该续期"读的是**池投影**的过期时刻，而各上游的池投影都**刻意不抄**
`ExpiresAt`（投影纪律：它是启动快照、活 secret 续期后不会跟新）。
实测 `/admin/accounts` 里 `expires_at` 键**一个上游都没有** ⇒ 恒为 0。

而 `auth.NeedsRefresh` 对 `ExpiresAt <= 0` **恒返回 true** ⇒
「该不该刷」对每个上游都恒真 ⇒ **每个 chat 请求先做一次续期往返**。
危害按 `refresh_token` 形态分两档：

    可重复使用（cline 等）→ 白费一次往返（实测：发一次对话后 token 被换了）
    **一次性**（raccoon / codearts）→ **每次请求烧掉一个 token**

改法：判据优先用**活凭证**的过期时刻（装配层已用 `CredentialExpiryExt`
从活 secret 补齐它，8 个上游全都实现了该扩展点）。

顺带修：`ParseJWTTimes` 缺 `iat` 时回落到 `nbf` —— raccoon 的 token
**只有 nbf 没有 iat**（抽样全部上游，只有它是这样），于是它的比例窗口
**整个空转**（`RefreshSkewExt` 形同虚设，注释却写着"iat + exp 都在"）。

实测：修前发一次 cline 对话请求 token 被换，修后不再被换。

### 已知限制（与上一版相同，未变）

- 只接了 Loomy 生图。其它上游要生图需各自实现 `ImageGenExt`。
- **图生图未实测**（上游支持 `images` 参数，本次只实测文生图）。
- 上游忽略 `response_format=b64_json` 与 `n>1`，网关如实透传、不替它纠正。
- 工具循环轮次上限 3；到顶会去掉工具定义强制收尾。
- 没有 WebUI 生图入口（只有 API）。
- 裸图片 URL 依赖"对象存储桶是公开读"这个**上游部署事实**；
  上游若改成私有读，会自动退回签名 URL（HEAD 会 403，预览器可能仍不工作）。
- raccoon 的登录依赖**上游 `/login?redirect=` 机制不做白名单校验**（本版实测可行）；
  若上游将来收紧，登录会退回"回调收不到 code"并如实报错（不静默超时）。

---
## v1.7.2 — 2026-10-05

> 本版主题：**修三个用户实际报障的缺陷**（agent 框架里生不了图 / 图片取不到 /
> raccoon token 静静过期）+ **一轮稳定性加固**。全部是修复，无新增上游、
> 无破坏性变更。

### 修复 — agent 框架（DSH / Claude Code / Cursor）里生不了图

用户报障："DSH 里生不了图"。根因不是 DSH 判断模型不支持生图，而是上一版
的一条判据**主动不注入工具**：

    客户端自带 tools → 网关一个工具都不注入

那条判据写在"普通聊天客户端"的语境里，初衷是对的（客户端带 tools 说明它
懂协议、要自己驱动工具循环）。但 **agent 框架每轮都带自己的工具**
（bash / read / write …），于是永远命中它 → 模型没有 `generate_image`
可调 → 只能吐一段 SVG 源码或说"我画不了"。

改法：边界从"整个不接管"改成**按工具名分工** ——

| 模型调谁 | 谁执行 | agent 框架看到什么 |
|---|---|---|
| 客户端自己的（`bash`/`read`/…） | **客户端** | 正常的 `tool_calls` |
| 网关注入的（`generate_image`/`web_search`） | **网关** | 只见模型"直接回了张带图的 markdown" |

于是 agent 框架侧**零配置** —— 不必在它的配置里声明生图工具。

⚠ 顺带修掉一个连带 bug：`injectTools` 原来是 `obj["tools"] = wireTools`
（**赋值 = 替换**）。在旧判据下它不出问题，判据一删就会**抹掉 agent 框架的
全部工具** —— 模型再也执行不了任何命令，而现象是"DSH 突然变笨了"，
与本功能毫无表面关联。现在改成合并 + 重名去重（以客户端声明为准，
避免工具列表重名被上游 400）。

### 修复 — 工具循环破坏了流式（"文字不是流式的了"）

上一版工具循环只有非流式一条路（内部跑完整轮再整块发出），而它对
**每个** loomy 请求都成立 —— 于是所有对话都被缓冲成一整块。
实测复现：12.6 秒的内容挤在 2 帧里。

那个设计基于一个**错误假设**（"必须先知道要不要调工具才知道给客户端什么"）。
实测推翻它（真实上游 25 帧的流）：

    第一个含 content 的帧    : 不存在（-1）
    第一个含 tool_calls 的帧 : 第 8 帧

即：模型要调工具时**正文一个字都不先发**；纯文字回复**从头到尾没有
tool_calls 帧**。所以改成边流边判断，普通文字 100% 保持逐字流式，
只在真要调工具的那一轮拦截。

修后实测：纯文字 31-88 个内容帧；流式生图 86 帧；流式搜索 569 帧。

### 修复 — 图片"无法预览" / "拿不到文件实体"

用户报障："图片无法预览"、"file unavailable"，并推断是
"上游出图→传云端的管道断了"。**那个推断是错的** —— 实测对象实体一直都在：

    HEAD → 200    GET → 200/330444
    HEAD → 200    GET → 200/217678
    HEAD → 200    GET → 200/1363756

真因是**签名 URL 对 HEAD 必然 403**（COS 把 HTTP method 也算进签名，
而 URL 是按 GET 签的）。而预览器 / 取图器 / 文件发送队列**通常先发 HEAD**
探测类型与大小 —— 一探测就 403，于是"无法预览"、"file unavailable"。
现象像管道断了，真因只是 method 不匹配。

修法：探测裸形式（去掉签名）是否可用，可用就用它（HEAD/GET 都通、
**不过期**、markdown 里也没有 `&` 与 `;`），不可用则退回签名 URL。
不硬编码"bucket 公开读"这个假设 —— 那是上游的部署事实，上游改私有后
探测会失败并自动退回。

⚠ 另有第二个根因：提示词里的 `]` 会**截断** markdown 链接
（`alt` 是用户输入，而 CommonMark 在第一个未转义的 `]` 处结束）。
用户看到的那段"图片无法预览 · 电商产品主图宣传海报…"就是截断后的 alt。
已转义 `\ [ ] ( ) < > * _ ` ~ | #`。

### 修复 — raccoon 的 Token 一直显示"已过期"（续期从不触发）

用户报障：管理台同时显示"正常"与"已过期"。实测那份凭证：

    access_token   寿命约 3 小时，**已过期 532 分钟**
    refresh_token  寿命 30 天，**还剩 29.5 天**（完全可续期）

根因：核心的续期只有两条触发路径 ——

    ① 出站请求时   handler 判断"该刷了"才刷
    ② 后台定时任务 JobExt.Jobs()

raccoon `RefreshCredential` 与 `RefreshSkew` 都有，**但没有 `Jobs()`**，
于是只有"有人拿它发对话请求"时才顺手续期。横向核对：workbuddy /
codearts / cline / lobsterai / qoder / trae 都有，**只有 raccoon 没有**。

已补 `internal/raccoon/jobs.go`（10 分钟一轮，判据复用 RefreshSkew 的
寿命比例，不另写一套）。实测：无人调用对话接口的情况下，42 秒内自动续期成功。

⚠ 这与 cline 此前那次报障（"Token 已过期，怎么不会自动刷新"）**逐字同一种病**。
教训：「具备续期能力」与「会去续期」是两件事 —— 前者是方法签名，
后者是**注册了后台任务**。缺后者时没有任何报错，只是悄悄过期。

### 稳定性加固

- **流式响应可能被写两遍**（响应损坏）。`consumeRound` 三条错误路径里
  只有两条检查了"是否已写过响应"，`status >= 400` 那条漏了 → 已写出内容后
  仍换号重试 → 往**同一个** ResponseWriter 再写一套 SSE → 客户端收到
  两段交错的流。改用统一出口 `finalize(sw)` 刷新 `committed`，
  并把"已写过"包成独立错误类型让 handler 不再重试。
- **工具执行不跟请求取消**（白扣积分）。三处都用 `context.Background()`，
  而 handler 手上明明有 `r.Context()` —— 后果是客户端断开后生图照跑满
  5 分钟、**积分照扣**。已接请求 ctx。
- **panic 有了可诊断的收场**。net/http 会 recover，但只关连接 + 打 stack：
  未写过 → 客户端看到"连接被重置"然后重试（再触发同一个 panic）；
  已写过 → 半截 SSE 挂到超时。新增 `panicRecovery` 中间件（挂在 `withAuth`，
  即 `/v1` 全部端点的唯一共同入口）。
- **服务器补了 IdleTimeout / MaxHeaderBytes**，但**刻意不设 `WriteTimeout`** ——
  它会掐断正常的长 SSE（实测搜索 35.4s、工具循环最长 15 分钟）。
  逐请求超时由 handler 自己的 ctx 管。
- **日志 TTFB / tok / tok/s 三列不再为空**。工具循环是另一条读取路径，
  绕过了 `chatStatsReader` —— 功能完全正常，只有日志悄悄空了。
  改成让每一轮的流**穿过**统计 reader（不重写一套 usage 解析）。

### 实测（真实上游）

    逐字流式      31-88 个内容帧
    流式生图      86 帧，含保活注释帧 + 图片 markdown + [DONE]
    流式搜索      569 帧
    两条生图路径  字段 created/data/points_consumed 完整，points=110，HEAD 200
    raccoon 续期  无人调用对话接口，42 秒内自动续期成功
    日志三列      TTFB=859ms tok=357 69.3tok/s
    MaxHeaderBytes 2MB header → 431

### 已知限制（与上一版相同，未变）

- 只接了 Loomy 生图。其它上游要生图需各自实现 `ImageGenExt`。
- **图生图未实测**（上游支持 `images` 参数，本次只实测文生图）。
- 上游忽略 `response_format=b64_json` 与 `n>1`，网关如实透传、不替它纠正。
- 工具循环轮次上限 3；到顶会去掉工具定义强制收尾。
- 没有 WebUI 生图入口（只有 API）。

---

## v1.7.1 — 2026-10-04

> 本版主题：**CI 发布流水线改为五平台全自动**，消除产物重复。
> 无任何功能变更。

### 修复 — Release 产物重复

v1.7.0 的 Release 上有 **7 个**资产但实际只有 5 个平台 —— 因为两套命名并存：

    CI 编的：  wb2api-server-windows-amd64.zip
               wb2api-server-linux-amd64.tar.gz
    本机编的： wb2api-server-v1.7.0-{windows,linux,darwin}-{amd64,arm64}.zip

其中 windows-amd64 与 linux-amd64 **各重复了一次**，命名风格还不一致
（一个带版本号、一个不带；一个 tar.gz、一个 zip）。下载的人分不清该拿哪个，
两个文件的内容也可能来自不同 commit。

### 变更 — CI 覆盖全部五个平台

    windows-amd64 / linux-amd64 / linux-arm64 / darwin-amd64 / darwin-arm64

命名统一为 `wb2api-server-<goos>-<goarch>.<zip|tar.gz>`（不带版本号 ——
版本在 Release tag 与二进制里，文件名带版本号只会让"同一 tag 重发"
产生两个名字）。归档格式按平台分两种（Windows 用 zip，其余 tar.gz）。
新增质量门禁 job（build/vet/test/tidy/gofmt）与残留资产清理步骤。

**本机不再手工上传任何产物。**

---
## v1.7.0 — 2026-09-30

> 本版主题：**上游清单从 4 个扩到 10 个** + **登录、签到、额度、续期四条链路的
> 系统性补齐**。
>
> 上游净变化（以**已发布的 v1.6.2 tag** 为基准）：
>
>     v1.6.2 注册 4 个：workbuddy / codearts / loomy / trae
>     本版  注册 10 个：+ workbuddy-intl / cline / raccoon / lobsterai /
>                        qoder / qodercn
>
> ⚠ 所以本版**只增不删** —— 新增 6 个，没有删除任何已发布过的上游。
> 下面「移除」一节里的 `mimo` / `buddy` 都**从未发布过**（它们在
> v1.6.2 之后的未发布提交里出现），对用户无影响，列出只为说明代码来龙去脉。
>
> ⚠ 但本版仍是**破坏性**的：多条链路的实现被重写（见各条标注），
> 且新增了 `dailycheckin` / `qrcode` 两个包与一批扩展点。

### 新增上游（+6）

- **Cline**（Cline 桌面端 / Cline API）：WorkOS 设备码轮询登录（**不起本地端口**，
  服务器部署天然可用）+ 余额查询 + 后台续期。
- **Raccoon Work**（商汤小浣熊）：**微信扫码**登录 + 积分余额 + 登录奖励 +
  onboarding 状态查询。
- **LobsterAI**（有道龙虾）：三步式签到领取积分 + 30 分钟自动签到。
- **Qoder / Qoder 中国版**（阿里系，两个实例）：PKCE 设备码登录 +
  积分余额 + 每日签到 + **WASM 加密推理桥**。
- **WorkBuddy 海外版**（`workbuddy-intl`）：双渠道账号池，按凭证 `channel` 字段
  自动选择 chat/billing/web 三个域的 base 与 Origin/Referer 头。

### 新增 — 统一的「全部签到」（两级）

- 顶部**一个**「全部签到」触发**所有**上游的全量签到（`POST /admin/accounts/checkin/all`，
  后台任务 + 进度回执）；各上游卡片头另有自己的「全部签到」（只签本上游）。
- 新增 `gateway.DailyCheckinExt`：有签到的上游都实现它，顶部那个据此发现它们。
- 为 codearts 补上全量端点 `/admin/welfare/claim/all`（此前只有单账号）。

### 新增 — 额度三层作用域 + 原始值显示

- **行内「额度」**只刷该账号；**卡片头「刷新本上游额度」**只刷该上游；
  **顶部「刷新全部额度」**刷所有账号。三者共用同一个文案渲染函数，措辞不会漂移。
- 四个上游实现 `gateway.QuotaExt`，额度列显示**上游自己的记账单位**（不做换算假设）。
- ⚠ 修 `qoder` 额度恒为 0/空：`QuotaExt` 只在**手动**刷新时被调用，全仓没有定时
  任务扫它 → 重启后额度列一直是空的。现由续期任务每轮顺带写回（新增 `QuotaSink`）。

### 新增 — Token 列 / 续期链路

- 统一「寿命 50% 续期」：各上游经 `gateway.RefreshSkewExt` 自报提前续期窗口，
  且 `RefreshSkewAtRatio` 按 **token 自己的 iat/exp** 算（不再依赖账号池投影）。
- 补齐 `gateway.CredentialExpiryExt` / `CredentialTokenExt` 的接线 ——
  「Token 到期」列此前恒为 `—`，根因是投影里没有 `ExpiresAt`（见 `HasToken` 修复）。
- ⚠ 修 `qoder` / `qodercn`「没有 Token」：续期响应字段名全错
  （读 `access_token` + `expires_in`，上游实际返回 `device_token` + `expires_at`
  ISO 字符串）→ 续期永远报"缺少访问令牌"、过期时刻从未落盘。
- ⚠ 修 `cline` 从不自动续期：`CredentialRefresher` 实现了，但**没有 `Jobs()`** ——
  核心两条续期路径（出站请求 / 后台任务）一条都不占。
- ⚠ 修 `qoder` 两个实例注册**同名**后台任务 → 调度器
  `任务名 qoder-refresh 重复（上游 qoder 与 qodercn），已跳过后者`
  → **中国版永不续期**。任务名改为 `<productID>-refresh`。

### 新增 — 批量导入统一到**全部**上游

- 新增 `gateway.AccountImportExt` + 能力位 `CapImport`，核心提供**一个**通用端点
  `POST /admin/accounts/import {provider,data}`；8 个上游各实现 `ImportCredentials`。
- 改造前只有 2 个上游有（workbuddy / loomy 各写一份）。
- 「导入」就是各上游**已有**的 `ParseCredential` + `MarshalAuthFile` 的复合，
  所以每个上游只需极少的搬运代码，不手写字段映射。

### 新增 — 机器人与登录

- **`runtime-info.exe` 实时生成设备身份**（qoder）：此前的"读磁盘缓存"路径
  在本机**文件根本不存在**（纯登录流程不产生它）→ 两个必需头从未发出 →
  每日签到**永远失败**。
- **扫码登录的二维码渲染**：新增 `internal/qrcode`（自实现二维码，零依赖，
  byte 模式 + 纠错 M + 版本 1–10）+ `gateway.QRLoginExt`。此前 raccoon 的
  「添加账号」给出一个**扫不了的链接**。
- 「添加账号」默认用**无痕窗口**打开授权页（网关自己启动浏览器 + 独立 profile）。
- 登录三步补齐**产品身份 UA**（此前全用 CLI 形态）。

### 控制台 UI

- **卡片头按钮顺序统一**（用户指定）：
  `全部签到 → 添加账号 → 批量导入 → 刷新本上游额度 → 重载 auths`。
  「添加账号」去掉 `＋` 前缀（右端多项对齐时不一致）。
- 账号池额度/状态卡**无感轮询**：只更新数值不整页重绘。
- 扫码类上游的引导语按**登录形态**分叉（qr / local / browser）——
  此前会同时显示「去浏览器登录」与「用手机微信扫码」两句矛盾的话。

### 安全 / 正确性修复

- ⚠ **导入不再丢失 `deviceToken`**：`SaveAtomic` 会把 `account.deviceToken`
  一起写回，导入不认它 = **用空串覆盖用户已有的设备令牌**（每账号一个的风控头
  丢了，表现为"导入后这个号开始被风控"）。两条导入路径各有一处同样的漏，都补了。
- ⚠ **`qoder` 中国版导入误拒**：`normalize()` 用**包级常量** `"qoder"` 兜底，
  拿不到实例产品 → CN 实例**必然误拒**没写 `product_id` 的凭证，
  且错误信息指向用户没写过的东西。判据改为读**粘贴原文**。
- ⚠ **`lobsterai` 签到永远失败**：上游返回的 `actions` 是**裸字符串数组**
  `["check_in"]`，我们按对象数组解析 → 每次签到都在第 3 步失败。
  契约测试的假上游当时也用了错形状，**把缺陷验证成了正确行为**。
- ⚠ **`refreshAccounts is not defined`**：分组行「刷新本上游额度」调用了一个
  从未定义的函数 → 点下去必抛。静态断言抓不到（文本里有这个名字）。
- ⚠ **`quotaScopeToast` 返回对象却被当字符串**：界面显示 `[object Object]`。
- ⚠ **瞬时 503 被当成永久失败**（qoder campaigns）：实测上游约 25% 请求回
  `DEPENDENCY_UNAVAILABLE`（与请求头无关，`/usage` 同时刻恒 200），
  而旧实现不重试 → 签到每 30 分钟扫一次，撞上就记一次 `fail`。现加 5xx 退避重试。

### 移除（两个都**从未发布过**，对用户无影响）

- **`mimo`（小米 MiMo）上游删除** —— 用户要求整体移除（"小米这个有问题"）。
  连带删掉 `internal/mimo/`（16 文件）、`cmd/server/mimocreds.go`、
  `scripts/mimo/`（5 脚本）、配置段与 17 个扁平字段。
  ⚠ 它是 v1.6.2 之后加进来的（commit `a796690`），**从未进过任何 Release** ——
  所以升级的用户不用担心这个功能消失，他们本来就没见过。
  ⚠ `internal/{cline,loomy}/models.go` 里的 `mimo` 是**模型名**（cline 代理的
  `xiaomi/mimo-v2.6-pro` 等），**不是**那个上游 —— 保留。
- **`buddy`（腾讯 CodeBuddy 中国版）也从未发布过**：它在**本批未发布的
  提交里加进来过**（commit `12c335d`），随后被删除（与 workbuddy 同一后端
  `copilot.tencent.com`，仅出站身份不同，用户判定重复）。
  `cmd/server/buddy_identity_test.go` / `config_buddy_test.go` 随之一并删除。
- ⚠ 配置里残留的 `mimo` 段会被**静默忽略**（JSON 未知字段不报错），
  但建议删掉以免误导。

### 破坏性变更有哪些（尽量列全）

| 变更 | 影响 |
|---|---|
| 新增 6 个上游 | 无（都在 `enabled=false` 或缺省关闭时不改变既有行为） |
| `mimo` 段失效 | 若你的 config 里有它：被静默忽略，建议删除 |
| 签到/额度/续期链路重写 | 行为变化（修的都是"本来就不工作"的路径） |
| 卡片头按钮顺序 + 去掉 `＋` | 纯 UI |
| 新增 `internal/dailycheckin`、`internal/qrcode` | 无（内部包） |

**没有任何配置字段被改名或删除** —— 旧 config.json 可直接用。

### 文档 / 工程

- `config.example.json` **补齐 4 个新上游段**（cline / raccoon / lobsterai / qoder）
  —— 它是 Release 压缩包里的配置模板，此前缺项，用户下载后看不到怎么启用它们。
  已用这份模板原样启动验证：10 个上游全部注册成功。
- `gateway` 契约测试新增：`CapImport` 声明必须实现 `AccountImportExt`
  （防"点了回 501 的假按钮"）。
- 前端探针：新增两个**真浏览器 e2e**（`verify_checkin_ui_e2e.js` /
  `verify_all_buttons_e2e.js`），验的是**行为**（点一下发几个请求、发的是哪个 URL、
  刷新后委托是否累积）—— 变异验证过（把"只绑一次"守卫去掉 → 刷新 5 次后点一次发
  **7 个**请求，而所有 DOM 静态断言全绿）。
- `internal/qrcode` 的正确性由**交叉验证**守住：真的调用 Python `qrcode` 库
  逐位比矩阵（结构断言全绿≠码能扫 —— 实测把 zigzag 方向改错时只有交叉验证会红）。

## 未发布 — 海外版 WorkBuddy AI 渠道（workbuddy-intl）

> 本版主题：**国内版 + 海外版（www.workbuddy.ai）双渠道账号池**。

### 新增

- **海外版渠道实例 `workbuddy-intl`**：同一个 workbuddy.Provider 实现注册第二个实例，
  凭证目录 `auths/workbuddy-intl/`，OAuth 站点 `https://www.workbuddy.ai`；
  配置 `workbuddy_intl.enabled=true` 显式启用（缺省关闭，老配置零影响）。
- **能力裁剪（海外版实测依据）**：海外版 product.json 显式禁用签到/成长/旅行
  （DisableCheckin=true、UserGrowth=false、DisableActivityBanner=true），
  因此 workbuddy-intl 实例**只声明对话/模型/额度探测**，不注册玩法后台任务、
  不挂玩法管理端点（DisableGrowthTravel 开关，默认关，国内版行为不变）。
- **多实例隔离**：管理端点路径按实例 ID 加 `/<id>` 前缀、后台任务名加 `<id>-` 前缀，
  同一实现注册多个实例不再互相冲突（默认实例路径/任务名逐字节不变）。
- **凭证级渠道路由**：`auth` 块新增可选 `channel` 字段（`cn`/`intl`），
  旧凭证无该字段时按 `domain` 自动推导（含 `workbuddy.ai` → intl）；
  chat/billing/web 三个域与 Origin/Referer 头随账号渠道自动选择
  （CN：copilot.tencent.com / www.codebuddy.cn / workbuddy.cn；Intl：www.workbuddy.ai）。
- **登录**：`./login.sh -intl` / `cmd/login -intl` 走海外版设备授权；
  WebUI「＋ 添加账号」按上游分派（workbuddy-intl 行内按钮用海外授权站点）。
- **本机客户端登录切换支持海外版**：按目标账号渠道写
  `workbuddy-desktop-ai.info` + `~/.workbuddy-ai`（CN 仍为
  `workbuddy-desktop.info` + `~/.workbuddy`）；进程检测覆盖 `WorkBuddyAI.exe`；
  备份/回滚带渠道元数据（last.meta.json）。
- **集成验证**：临时实例实测两个 provider（workbuddy / workbuddy-intl）账号
  正确并入同一账号池并各自路由。

### 文档 / 工程

- README / config.example.json：海外版渠道章节与配置示例。
- 凭证格式不变（account+auth 两块），channel 字段可选，旧文件零迁移。

### 新增 — 「添加账号」默认用**无痕窗口**打开授权页

- **默认无痕（`login.open_browser`，缺省 true）**：点「＋ 添加账号」时由**网关自己
  启动浏览器**打开授权页，不再把"用哪个窗口"交给用户当前浏览器状态 ——
  否则 OAuth 会拿浏览器里**已登录的账号**完成授权（"加新号"变"加老号"，且无任何报错）。
- **独立 profile（`login.isolated`，缺省 true）**：给浏览器一份临时
  `--user-data-dir`（`%TEMP%/wb2api-login-*`，48h 后自动清理），
  让这次授权从零 cookie 开始；只给无痕参数时，走 SSO 的站点仍可直接用已登录账号。
  代价是需重新输入账号密码（`isolated=false` 可关，仅开无痕窗口）。
- **新包 `internal/browseropen`**：`Plan` 是纯决策函数（平台/浏览器/参数向量
  全部可断言，测试不弹窗口），`Open` = Plan + 启动且**不等待**浏览器退出。
  候选顺序 Edge → Chrome → Brave → Chromium（Windows 上 Edge 一定存在，
  避免"点了没反应"）；macOS 走 `open -n -a <App> --args`；`login.browser` 可显式指定路径。
- **新端点 `POST /admin/login/open`**：用无痕窗口**重新**打开刚签发的授权链接
  （手滑关掉窗口时用，不会作废当前 state）。**只认本进程签发过的链接** ——
  入参是 URL 而它会被交给本机浏览器打开，不校验就等于"用本机浏览器访问任意地址"。
- **失败不拖垮主流程**：开不起来（无 GUI / 策略限制）时授权链接照常返回，
  响应带 `browser_error` 供界面显示「未能自动打开，请复制链接」；
  未接线 opener 的部署行为与改造前逐字节一致（不出现 browser 字段）。
- 前端：弹窗主状态改为「已用 X（无痕）打开」，新增「重新无痕打开」按钮，
  原「打开授权页面」降级为「普通窗口打开」并标注风险。
- 配置段 `login.{open_browser,browser,isolated}`。

## v1.6.0 — 2026-09-15

> 本版为**未发布**的本地里程碑（已 commit、未 push 远端）。主题：**新增第四个上游 TRAE** +
> **稳定性机制升级（移植自 AIClient2API）**。

### 稳定性升级（移植 AIClient2API 的成熟机制，保证账号持久稳定运行）

- **主动健康检查轮询**（新扩展点 `gateway.HealthProbeExt` + 核心任务 `pool-health-check`）：
  每 10 分钟（`pool.health_check_interval_seconds` 可配）探测冷却/熔断中的账号
  —— 用各上游的**便宜端点**（workbuddy/codearts/loomy/trae 各实现了自己的探测），
  成功即 `ClearCooldown` 提前恢复。额度"提前回血"的账号不再干等冷却到期；
  刚报错（2 分钟内）与禁用的账号不探测。
- **熔断错误计数时间窗衰减**：只有 60 秒窗口内的**突发**失败才累计到熔断阈值，
  零散错误不再慢慢攒坏一个号（此前只有成功才能清零）。
- **刷新失败上限 → 禁用**：凭证续期连续失败 3 次直接禁用（refresh token 已死，
  不再每次请求白试一次失败往返），成功清零。

### 新增（TRAE 上游，`internal/trae/`）

- **TRAE SOLO 对话通道**：`llm_utils_chat`（`function=solo_work_lite`），自定义 SOLO SSE
  实时转成 OpenAI SSE（`internal/trae/sse.go`），流式/非流式/工具调用/思考链均可用。
- **多账号 + 自动续期**：JWT + 消费型 refreshToken（与 codearts 同构），
  后台按 `refresh_interval_seconds` 扫描续期 + 请求路径惰性续期；`CredentialExpiryExt`
  提供「Token 到期」列。
- **每日自动签到**：30 分钟扫一次，查状态、未签则领（幂等、跨重启安全）；
  `DailyActionExt` 提供行内「签到」与顶部「全部签到」。
- **权益包额度**：`ide_user_ent_usage` 的 `credits_limit` 求和 → 账号池「额度」列。
- **模型目录**：实时拉 `get_detail_param`（config_name 即模型 ID），失败回落
  13+1 个已知 SOLO 免费模型的静态快照（trae-solo-unlock 实测清单 + glm-5.2）。
- **页内添加账号**：TRAE 浏览器 OAuth（`www.trae.cn/authorization` + 127.0.0.1 回调）
  完整搬进控制台 —— 「＋ 添加账号」返回**真实 TRAE 登录页**，网关在
  `127.0.0.1:18080/authorize`（可配 `trae.oauth_callback_port`）**自动接收登录回跳**，
  全程零手动步骤（无需复制链接/粘贴回调）；每次登录换新 machine/device id。
- **排队检测 + 模型分档自动降级**：监听上游 `request_wait_in_queue` 事件，
  排队位置超过阈值（默认 300）且尚未出内容时，自动换同档其它模型 → 下一档 → 兜底
  `glm-5`（`trae.fallback_enabled / queue_threshold / max_attempts` 可配）；
  已出内容后不再切换（避免客户端看到两段不连续回复）。
- **签到写历史**：「今日签到」列修复 —— trae 签到（手动/定时/批量）现在写
  checkinlog，账号表正确显示「已签到/失败/跳过」。
- 配置段 `trae.{enabled,auth_dir,refresh_interval_seconds,checkin_enabled,pool_accounts,oauth_callback_port,fallback_enabled,queue_threshold,max_attempts}`。

### 说明

- 协议来自对多个 trae 反代项目的复现研究（traework2api / trae-api / trae-local-api /
  trae-solo-unlock），核心事实已固化进 `client.go` 的常量与测试。
- 凭证两种形态都认（嵌套 `{auth,account}` / 扁平）；`trae-<uid>.json`。
- 契约测试用假上游全程执行（CI 不跳过），SSE 转换/分类/续期/额度/登录流程均有单测。

## v1.5.0 — 2026-09-15

> 本版为**自 v1.2.0 以来的累积发布**（此前 v1.3.0 / v1.4.0 两条只是里程碑记录，
> 从未发版；其内容已全部包含在本版）。主题：**Loomy 上游从"能转发"变成"能自助运营"，
> 控制台从"堆满数字"变成"一眼看清"**。仓库同步更名为 `multi2api` —— 本网关是
> 多上游聚合反代，不再是 workbuddy 专属。

### 新增（Loomy 上游完整化）

- **手机号 + 验证码登录**（`internal/loomy/smslogin.go`）：对接讯飞账号网关
  `account.xfinfr.com`，完整实现 HMAC-SHA1 请求签名；「＋ 添加账号」现在支持
  直接输手机号发短信验码登录，不再依赖本机客户端。
- **新手任务一键完成**：新标签页「新手任务 · Loomy」—— 8 个 onboarding 任务
  （共 10000 分）逐账号显示进度，支持「一键完成（N）」与「一键完成全部账号」。
- **邀请码**：新标签页「邀请码 · Loomy」—— 激活状态 / 余额 / **我生成的码**（含
  active/exhausted 状态）/ 绑定别人的码；绑定前置校验"不能用自己账号的码"，
  失败提示翻译成人话；**首登初始化自动补**（导入的号跳过客户端 first-login 时，
  邀请码与注册奖励会自动补齐，服务端幂等）。
- **批量粘贴导入**：账号池分组行「批量导入」按钮 —— 粘贴 JSON（单条/数组），
  逐条落盘 + 自动重载账号池；失败项逐条列出。
- **额度改走积分网关**：`GET /api/v1/points/records` 按 session 逐账号查
  **可用总额度（availableBalance = 总余额 + 当日剩余）** —— 修掉"没加上送的额度"、
  "只有本机那个号有值"两个问题；积分网关失败才回落到本机缓存。

### 变更（控制台 UI）

- 仪表盘：网关状态只留「账号总数 / 健康」两张卡；调用统计精简为 8 张核心卡
  （总请求 / 成功 / 失败 / 成功率 / **消耗 token** / 输出 token / 平均 TTFB /
  平均总耗时），**累计消耗从积分改为 token**（跨上游统一单位）；卡片宽度随内容
  自适应、左对齐、自动换行。
- 删除「任务历史」标签页（用户决定不需要）。
- 账号池：删除「熔断」「在途」两列（workbuddy 默认列集与 loomy 自报列集同步）；
  **所有昵称打码**（首 3 尾 2 保留，中间打 \*，已掩码的保持原样）；顶部「全部签到」
  按钮移除；codearts 按钮文案「领取福利」→「签到」（ID 与端点不变）；
  workbuddy「签到」与「额度」按钮互换位置（额度排最前）。

### 修复

- `/admin/login/poll` 回执不再把 `ExpiresAt` 零值（`0001-01-01T00:00:00Z`）下发 ——
  之前会让"添加账号"弹窗显示「已过期」，而账号表里写着「永久」，两处自相矛盾。
- 页面周期性刷新不再拆建面板、不再重复拉取已加载的面板；输入框重绘后保留已输内容。

### 说明

- 短信登录的 access key 来自 Loomy 安装包内嵌的 `.env.prod`（混淆而非真保密，
  见 `internal/loomy/smslogin.go` 的注释），可配 `loomy.sms_*` 覆盖。
- 邀请码每个 `maxUses=1`，绑定是一次性的、无法撤销；界面在绑定前强制确认。

## v1.4.0 — 2026-09-15

> 本版主题：**把 loomy 那三格空白补上，并把「上游」列统一到所有账号表**。
>
> 用户报的原文是两组现象：loomy 分组只有 2 项能力、「未声明页内登录流程」，
> 而 **额度 / Token 到期 / 今日签到三格都是空的**（"探究一下怎回事"）；
> 另外 codearts 的表格**第一列不是「上游」**，与其他上游的表格对不齐。
>
> 探究出来的根因是一个**共享的错误前提**：上一轮把"上游没有 HTTP 端点"
> 当成了"没有数据"。而实测证明 —— 登录态与积分摘要都**明文缓存在本机**
> Local Storage 里（`internal/loomy/clientstore.go`）。前提消失，能力随之补上。

### 新增

- **`gateway.CredentialLifetimeExt`（第 9 个可选扩展点）** —— 表达凭证过期信息的
  **第三态**：不是"某个到期时刻"，也不是"我们不知道"，而是**设计上就不会过期**。
  - 原来的两个指针（`token_expire_sec` + 字段缺失）表达不了它：loomy 的 session
    是服务端持久化登录态，落进"不知道"于是显示 `—`，用户读成"这功能没做"。
  - 单开接口而不是给 `TokenExpiry` 加返回值：那会与现有契约
    （"`ok=true, at<=0` 与 `ok=false` 同义"）**相反**，并强制全部实现者理解新哨兵 ——
    漏改一处就把"未知"说成"永久"，而"永久"是个**强断言**。
  - `AccountView.TokenNeverExpires`（`token_never_expires`，`omitempty`）。
  - 顺序契约：**先问时刻、拿不到才问永久**；有到期时刻时本字段恒为 false
    （反了会把 codearts 那 2 小时的 STS 渲染成「永久」）。
  - 不实现该扩展点的上游行为**逐字节不变**（未知仍渲染 `—`）。
- **loomy「＋ 添加账号」**（`gateway.LoginFlow`）：读本机客户端已登录的 session，
  **不需要浏览器**。用 `auth_url` 为空串表达"这次不用打开任何页面"——
  前端按**数据**分叉（不是按上游名），文案改成"正在从本机读取登录态…"。
- **loomy「额度」**（`gateway.QuotaExt` + `CapQuotaProbe`）：读本机缓存的
  `loomy-points-summary`，展示 `balance`（总余额，**不随自然日重置**）。
  - 为什么不是打接口：实测 17 条候选路径（`/points`、`/user/points`、`/balance`、
    `/quota`…）**全 404** —— 积分由客户端渲染层通过 IPC 问 Electron 主进程要，
    HTTP 细节在主进程 bundle 里，而结果被明文缓存在本机。
  - **uid 不匹配一律回答未知**：缓存属于本机登录的那一个账号，
    贴到别的号上会得到一个"看起来完全合理的错数"。
- **loomy 诊断端点** `GET /admin/loomy/client-store`（**Hidden**，无面板入口）：
  本机数据目录、读到的账号（脱敏）、两个账本、缓存是否今天的、以及读取失败原因。
  它同时是 `CapQuotaProbe` 的**契约要求**（`unverifiableCaps` 要求声明者实现
  `AdminExt`）—— 不是为过检查造的空壳，它回答的正是用户那句"探究一下怎回事"。
- **loomy `AccountColumnsExt`**：自报一套列，去掉两列不适用的
  （`token`：它的 `accessToken` 恒空；`checkin`：它没有签到端点），
  并把 `token_expiry` 的答案从 `—` 换成「永久」。
- **`loomy.client_data_dir` 配置**：留空 = 自动探测
  （`os.UserConfigDir()` 在 Windows/macOS/Linux 上都能拼对）。探测不到**不是错误**；
  此时按钮**不出现**（不放假按钮），启动日志会说明是哪一种情况。

### 变更

- **账号表首列统一为「上游」**：codearts 的列集补上 `provider`（用户明确要求）。
  - 原先那里是**刻意不含**的，理由是"分组后同组同名，冗余"。那条理由只看本组时成立，
    但它忽略了**同一列在不同上游的表里位置不同** —— workbuddy 走默认列集（首列就是
    「上游」），codearts 从「昵称」开头，两张表横向看过去对不齐。
  - 新增跨上游断言：两份列集的**首列必须相同**（只断言 codearts 自己的列集
    挡不住有人把 provider 从默认列集里删掉）。
- **`Caps()` 增加 `CapQuotaProbe`**（loomy 现在真的能主动拿到额度）。
- `loomy` 的启动日志新增一行客户端数据目录的结论（三种情况分别说明）。

### 修复 / 澄清（用户点名的那三格）

| 那格 | 探究结论 | 处置 |
|---|---|---|
| **额度** | 数据在，只是不在模型代理端点上 | 实现 `QuotaExt`（读本机缓存） |
| **Token 到期** | **它压根没有到期时间**（session 不绑时间、无 refresh token） | 如实渲染「永久」，不是补一个读数 |
| **今日签到** | **它没有签到动作** —— 日额度由服务端按自然日自动重置 | 从列集里去掉那一列 |

### 测试 / 守卫

- 新增 `internal/loomy/clientstore_test.go`（本机存储解析）、`login_test.go`、
  `quota_ext_test.go`、`accountview_test.go`、`adminroute_test.go`，
  `internal/admin/token_lifetime_test.go`，`internal/server/webui_token_lifetime_test.go`。
- **解析器的三条真实形状**各有断言：键值之间隔 `\x01` 等二进制（用 `\s*` 会一个都读不出来）、
  JSON 字符串里带 `}`（朴素正则会提前截断）、半截记录（leveldb 追加写的常态）。
- **积分摘要是部分更新**：最新的那条可能只有 `balance`，若"取最新一条直接解"
  会把 `dailyBalance` 覆盖成 **0** —— 而 0 的展示语义是"今日额度已用完"。
  叠加式折叠是正确行为，有专门断言。
- **凭证泄漏守卫**：诊断端点的回执按**字节**断言不含 session 的任何 8 字符连续片段，
  也不含未脱敏手机号（`Auth.Nickname` 在掩码缺失时会回落成完整号码）。
- **变异验证（8 条，全部实测变红）**：脚本 `mutate_verify_loomy_ui.ps1`。
  逐一改坏实现并断言对应测试变红 —— 包括"把 uid 校验反转"（额度贴错账号）、
  "把永久判据改成无条件"（顺序反了）、"让未知分支排到永久之前"（永久成死代码）、
  "把积分折叠退化成取最新一条"、"删掉 codearts 的 provider 首列"、
  "给一个假的授权 URL"、"把 session 原样回出去"、"把空 authURL 分支改成恒真"。
  - ⚠ 第一版脚本**自己 fail-open**：没切到模块根，每条 `go test` 都以
    "does not contain main module" 退出，而输出里没有 `FAIL` 字面量 →
    八条变异全被判成"仍然绿"。改成看**退出码**并区分"命令失败"与"断言失败"后才可信。

## v1.3.0 — 2026-09-14

> 本版主题：**新增第三个上游 Loomy**（讯飞桌面 AI 助手的模型服务）——
> 判据 1（"加一个上游 = 加一个目录 + 实现接口 + 配置加一段，核心零改动"）
> 的第二次实测，这次测的是**轻上游**形态。

### 新增

- **第三个上游：Loomy**（`internal/loomy/`）。`https://loomyad.xunfei.cn/api/v1`
  是标准 OpenAI 兼容端点，凭证就是客户端登录后写在 Local Storage 里的
  `session`（32 位 hex）。实测：12 个模型、流式 / function_calling / 视觉 / 推理全通，
  简单问答消耗 1 积分，日额度 5000 由服务端按自然日自动重置。
- **双轨鉴权的正反两向测试**。这个上游两个端点认不同的 Header
  （`GET /models` 用 `token:`，`POST /chat/completions` 用 `Authorization: Bearer`），
  而用错的那个返回 **HTTP 200** + `缺少 token` —— "看状态码判断鉴权"这条路不通。
  仓库把它写成两个各自命名的函数并配测试，改错任一方向都会红。
- **凭证两种形态都认**：直接抄客户端登录态，或用网关写盘的形态。
  UID 优先取 `userid`、其次文件里的 `uid`、最后按 session 派生（确定性）。
- **实时目录 + 静态快照兜底**：`GET /models` 拿得到就用实时的，
  拿不到回落内置快照 —— 保证目录不会因为一次抖动/session 失效而整片消失。
- **过滤上游已下架的模型**：`doubao-seedream-5-lite` / `qwen-image-3.0-pro`
  上游仍列在 `/models` 里但一用就 404「该模型暂未开放」，现在不进目录。
- **按模型裁剪超限的 `max_tokens` / `max_completion_tokens`**
  （最大的是 MiniMax-M3 的 512000），并保证 JSON 数字字面量不被改写成科学计数法。
- 配置段 `loomy.{enabled,auth_dir,base_url,pool_accounts}`；
  `auths/loomy/loomy-<uid>.json` 按上游分子目录。

### 说明（与 codearts 的关键区别）

- **没有 `CredentialRefresher`**：Loomy 的 session 无 TTL、无 refresh token
  （客户端重启 4 次未轮换、源码无续期判断），**没有可刷的东西**。
  实现一个空刷新只会让核心误判"这个号能自动恢复"。
- **session 失效 → 永久禁用**：`登录已失效，请重新登录` 只能人工重登。
  codearts 的同类错误映到 `ErrKindAuth`（换号不罚）是因为它**能**自动续期 ——
  两者不是同一件事，所以走的分类也不同。
- **刻意不实现的扩展点**：`AdminExt`（没有可用管理端点）、`JobExt`（无定时事务）、
  `LoginFlow`（登录在桌面客户端里）、`RefreshSkewExt`（无续期）、
  `SoftRateExt`（上游未给限流解除时刻）。有测试钉住这一点。
  > ⚠ **v1.4.0 订正**：`AdminExt` / `LoginFlow` 两条的理由共享一个错误前提 ——
  > 把"上游没有 HTTP 端点"当成了"没有数据"。实测证明登录态与积分摘要都明文缓存在
  > **本机 Local Storage**，于是这两条不成立、能力已补上（`JobExt` 那三条仍然成立）。
  > 详见 v1.4.0 一节与 `internal/loomy/clientstore.go` 的说明。
- 因此本包只有 ~200 行，而核心包（gateway/pool/admin/server/scheduler）**一行未改**；
  `arch_test.go` 自动把它纳入架构约束（实测发现结果 `[codearts loomy workbuddy]`）。

### 测试 / 守卫

- 契约测试（判据 2）跑**假上游**，无网络无凭证也全过程执行，CI 里不会跳过；
  假上游严格校验双轨鉴权，并有一段"负向对照"证明它真的在拦
  （否则正向断言是 fail-open）。
- 反向验证：把 `token:` 头改错 → `TestDualTrackAuth` 红；
  把分类改成"先看状态码" → `TestClassify` 在三条 200-带正文判据上红。
- `TestClassifyDoesNotBorrowOtherUpstreamsMarkers` 断言 loomy **不认**别家上游的
  特征串（workbuddy 的 `12153`、codearts 的 `insufficient quota`）——
  这正是 `gateway.ErrorClassifier` 要防的"用 A 的事实回答 B 的问题"。

### 修复（本轮顺带）

- **日志"已注册上游"打在最后一个上游注册之前**：loomy 启用时日志显示
  `已注册上游: [workbuddy]`，与事实不符 —— 一行"看起来权威"的日志会让人
  顺着它去查一个不存在的注册失败。已挪到所有注册之后。
- **测试夹具里混进了真实凭证**：Loomy 手册把作者的真实 session / 手机号 /
  userid 明文印了出来，第一版 fixture 直接照抄 —— 那等于把可用凭证提交进公开仓库
  （本仓库上一个提交正是"清掉两处真实 api_key 硬编码"）。已全部换成合成值，
  并把这条教训写进 fixture 的注释。

## v1.2.0 — 2026-09-14

> 本版主题：**移植并接通「客户端行为遥测伪造层」** —— 让网关能把一批上游
> 成长任务直接做完；同时补上**内容误报治理**（提示词体系 + 降级重试）
> 与出站身份的一致性。

### 新增

- **伪造客户端行为遥测层**：按官方桌面端 / 小程序 / Web 三个域构造并上报
  行为事件（`copilot.tencent.com` / `www.codebuddy.cn` / `www.workbuddy.cn`）。
  覆盖成长中心 17 项可自动化任务、开学季闭环、夜猫子补足。
  - **两类任务代价不同**：多数是**纯伪造**（只发事件链，无真实动作）；
    `expert_5` / `Expert_team_use_3` / `Expert_lighthouse` / `skill_1` /
    `Model_chat_GLM5.2` / `RichMeow_Chat` 必须**真发一次 chat** 拿上游签发的
    `requestId` —— 实测上游会校验专家 id 与 requestId，自造的**不计数**。
- **任务中心一键完成接上控制台**：任务明细行内「一键完成」、
  工具条「一键完成待办」（整账号）与「开学季一键完成」三个入口。
  按钮按后端能力表（`GET /admin/growth/auto/actions`）**动态出现**，
  不在前端写死；不在表里的任务（如需微信真实认证的 `Expert_Philanthropy`）
  仍显示「去客户端做」。
- **管理台新端点 7 条**（仅本机）：`/admin/growth/auto`、`/admin/growth/auto-all`、
  `/admin/growth/auto/actions`、`/admin/growth/scan`、`/admin/school`、
  `/admin/school/run`、`/admin/blackcat/run`。
- **系统提示词体系**：出站前用网关自有提示词**替换**客户端 system/developer
  （`prompt.mode=custom`，内置 2086 字节，可用 `prompt.file` 整体覆盖），
  或 `passthrough` 透传。被上游内容策略拦截时**不罚账号、不换号**，
  同请求内换极简中性提示词重试一次，并记忆到次日 00:00 CST。
- **出站身份一致性**：三段式 UA（`WorkBuddy/X WorkBuddy/X CLI/Y`）、
  设备令牌（`upstream.device_token` / `device_token_file`，5 分钟缓存）、
  客户端 IP 透传（`upstream.passthrough_ip`）、attribution 与 billing UA。
- **软限流（`code 6004`）按模型冷却**：解析上游重置时刻，指数退避、
  上限 `cooldown.soft_rate_max`（默认 2h），不牵连同账号的其它模型。
- **会话死亡计数**：同一账号连续 3 次会话失效才禁用，替代"一票否决"。
- **内容拦截成为一等错误类**（`ErrKindContentBlocked`）：与"账号故障"分开，
  避免把内容问题误报成账号池故障。
- **请求体硬上限**：`server.max_body_mb`（默认 8 MiB），超限返回 413。
- **DeepSeek thinking 注入**：按模型注入 `thinking` / `reasoning_effort=high`，
  并回填 `reasoning_content`。
- **对话活跃上报**（`schedule.activity_hours`，**空 = 关闭**）：按自然日计分，
  默认不发，避免存量部署升级后凭空产生上游请求。
- **领养前置修复**：`travel` 派猫前先补 `ensureAdoptPrereq`，解决恒失败。

### 修复

- **>8 MiB 请求体被静默截断后仍转发成功（HTTP 200）** → 改为
  `http.MaxBytesReader` + 413。原先调用方以为成功，实际上游收到的是残body。
- **`thinking` 未列入 `supportedFields`** → 注入被整体剥离，配置形同无效。
- **`sanitizeMessages` 对 `content` 键缺失的消息 `continue`**，整条消息
  （含 `tool_calls`）被跳过。
- **`resetDeviceTokenFileCache` 在持锁时替换整个结构体** →
  `fatal error: sync: unlock of unlocked mutex`（必崩）。
- **`6004` 正则会把 `"code":60040` 误判为软限流**（RE2 无环视，改用数字边界）。
- **`upstreamToGateway` 漏映射内容拦截** → 单上游部署下返回 503
  "无可用账号"，掩盖真实原因。
- **单账号部署的内容拦截降级重试无法重新选号**（唯一账号被 `tried` 挡住）。
- 任务中心四个缺陷（均为"不报错但用户会以为坏了"）：
  已领取的任务也长出按钮；`/admin/task` 完成摘要**恒报成功 0**；
  一键完成后**缓存快照不刷新**（跑完 186 秒界面仍显示原样待办）；
  toast 把奖励播报两遍。

### 测试 / 守卫

- 新增源码级守卫：`upstream_kind_mirror`（错误类镜像一致性 ——
  代码注释里声称存在的那条测试此前**并不存在**）、`maxbody`、`degrade`、
  `prompt_wire`、`desktop`、`school`、`autotask`、`taskslot_summary`、
  `webui_task_auto`、`autotask_ui`。
- 每条守卫都做了**反向验证**（把实现改回去必须变红），其中"已完成任务不得
  长按钮"用**单点回退真实文件**的方式，断言恰好只报 1 条并点名根因。

### 文档 / 工程

- README 新增：任务中心与 7 条端点表、提示词体系与降级语义、
  出站身份与设备令牌、软限流、内容拦截错误分类、对话活跃上报、
  领养前置说明、完整配置表与目录树。
- 修正 README 三处**与实现不符**的描述：内置提示词体积（1.6 KB →
  实测 2086 字节）、`passthrough` 的语义（降级期内**不再**透传）、
  降级重试的适用范围（**两种模式都生效**，不只 `passthrough`）。
- 仍然**零新增第三方依赖**（`go.mod` 只有 `go-redis`）。

## v1.1.0 — 2026-09-11

> 本版主题：**让成本可见 + 修一个统计口径的数据正确性 bug**。

### 新增

- **成本倍率**：从上游 `/v3/config` 拉取每个模型的官方积分倍率并展示
  （实测 29 个模型，最贵 `x5.00`、最便宜 `x0.00` —— 同样 token 量差 **17 倍**，
  此前完全不可见）。可用模型与对话下拉框都标上倍率，不再显示无用的 ctx 数字。
- **调用统计新增维度**：累计消耗积分、缓存命中率、缓存 token、推理 token。
  数据来自上游 usage 里此前被直接丢弃的字段（`credit` / 缓存命中未命中 / 推理 token）。
- **成长计划「到期」列**：上游部分任务带有效期（实测 18 个里 4 个），
  现按「远期（灰）/ 7 天内（黄）/ 已过期（红）」三档显示。
- **账号存活探测**：`GET /v2/plugin/accounts`。
- **额度告警接入故障诊断**：余额查询失败时附上上游的中文告警原因。

### 修复

- **调用统计被静默截断**（数据正确性）：统计取数走了分页接口，被单页上限夹到 300 条，
  一份 2000+ 行的日志只聚合了最近 300 条，**成功率 / 平均 TTFB 等全部失真**。
  改为不受分页上界约束的内部取全量，并新增 `aggregated` 字段自检窗口完整性。
- **模型目录无法自举**（功能完全失效）：状态判定与取数互相依赖形成自锁，
  冷启动时目录永远拉不起来，倍率恒为空。
- **写操作后就地刷新快照**：以前点一次操作要等 7 秒全量回源，现在读缓存（~1ms）。
- **界面操作反馈**：点按钮后立刻显示进行中状态（此前 6.9 秒页面完全静止，用户以为没反应）。
- **UTF-8 截断**：错误文案按字节截断会把中文切碎成乱码（`\ufffd`），改为按字符边界。
- 前端统计窗口文案的裸插值会让脏数据渲染成 `[object Object]`。
- 用户可见文案统一：成长计划的「信用分」改为「积分」。

### 性能

- 账号间探测并发化（上限 5）：3 账号强制刷新 **7.2s → 2.5s**。
- 单账号内 4 个上游调用并发化：探针 2402ms → 约 1600ms。
- 两层并发共享同一预算，避免"各限各的"相乘成 20 并发冲破上游风控。

### 文档 / 工程

- 设计文档：开源自审 — SECURITY.md、CI、徽章、README 配图（仪表盘 / 账号池 / 请求日志 / 设置）
- README 改为自托管 logo（`assets/logo.svg`），新增静态架构图（`assets/architecture.svg`）
- 新增 `start.bat`，修正 Windows 双击 `bin\wb2api-server.exe` 时工作目录错位
- `.github/workflows/ci.yml` —— PR 自动跑 `go mod tidy` / `build` / `vet` / `gofmt` / `test`
- `.github/workflows/release.yml` —— 打 tag 自动交叉编译 Windows/Linux 并挂 Release
- `.gitignore` 补 `logs/` 与 `tasks/`
- 隐私：移除 README 与测试注释里的真实账号标识（历史提交里仍有，见该提交说明）

### 历史遗留（未变）

- 依旧**零新增第三方依赖**：`go.mod` 只有 `go-redis` 一个直接依赖，
  产物是 `CGO_ENABLED=0` 的单文件静态二进制，下载即用。

## 历史里程碑（按时间倒序，节选）

> 完整提交历史见 `git log --oneline`。下面这些是**本仓库独有、有外部读者需要看**
> 的提交；其他纯内部重构、测试、注释类提交未列入。

### 2026-09 — 开源前准备

- `feat(ui): 设置面板组内复选框与输入框分离，不再交叉` —— 一个 fieldset 内部不再混排两类控件
- `fix(ui): 每页条数持久化到 localStorage，刷新页面不再回到 30` —— 顺手补了 TDZ 修复
- `feat(ui): 每页条数改为可手动输入，并在服务端夹住单页上限` —— 落盘上限 = 客户端上限 = 300
- `feat(ui): 调用统计的落盘占用只显示占用量；窗口文案只报条数`
- `feat(ui): 网关状态/调用统计/API 接入信息 合并为单一「仪表盘」面板`
- `feat(ui): 移除请求日志「实时」视图；成长计划刷新时一并更新额度`
- `feat(ui): 请求日志新增 tok/s 列；猫猫旅行说明自动派送的绑定关系`
- `feat(log): 统计聚合全量历史 + 前端统一排序/分页 + 旅行今日派送 + 移除调度面板`
- `feat(log): 统一分页 —— 任务历史与请求日志共用一个组件与一套语义`
- `fix(logbuf): 请求序号跨重启接续，消除落盘日志重复 seq`

### 更早

- `feat(admin): 成长计划领奖 + 本机客户端登录切换 + 设置页持久化` —— 管理台核心能力
- 初始从 `Sliverkiss/workbuddy2api` fork 并做本地化增量
