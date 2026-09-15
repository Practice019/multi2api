# 变更日志（Changelog）

本文件记录**本仓库相对于上游 `Sliverkiss/workbuddy2api` 的增量**。
上游版本自身的变化请关注原仓库的 Release / commit 历史；本仓库的主要功能版本
仍会跟上游对齐，只是本表会列出本仓库的额外提交与里程碑。

格式参考 [Keep a Changelog](https://keepachangelog.com/)，版本号不在本表里维护
（跟着上游走），日期格式 `YYYY-MM-DD`。

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
