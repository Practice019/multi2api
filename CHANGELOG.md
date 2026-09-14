# 变更日志（Changelog）

本文件记录**本仓库相对于上游 `Sliverkiss/workbuddy2api` 的增量**。
上游版本自身的变化请关注原仓库的 Release / commit 历史；本仓库的主要功能版本
仍会跟上游对齐，只是本表会列出本仓库的额外提交与里程碑。

格式参考 [Keep a Changelog](https://keepachangelog.com/)，版本号不在本表里维护
（跟着上游走），日期格式 `YYYY-MM-DD`。

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
