# 变更日志（Changelog）

本文件记录**本仓库相对于上游 `Sliverkiss/workbuddy2api` 的增量**。
上游版本自身的变化请关注原仓库的 Release / commit 历史；本仓库的主要功能版本
仍会跟上游对齐，只是本表会列出本仓库的额外提交与里程碑。

格式参考 [Keep a Changelog](https://keepachangelog.com/)，版本号不在本表里维护
（跟着上游走），日期格式 `YYYY-MM-DD`。

## 未发布 — 2026-10-21

> 本版主题：**前端视觉语言 v3 · MAXIMALISM（霓虹油墨）**。用户要求
> 「用 designforai.dev 的 maximalism 风格重构前端」。三个页面（控制台 / 登录 /
> 渠道）换成同一套：奶油纸底 + 墨线 + 五个冲突色 + 硬投影 + 零圆角 +
> 颗粒纹理 + 跑马灯 + 旋转贴纸。

### 变更

- **新增 `internal/server/theme-max.css`（唯一真相）** + `tools/theme_sync.js`：
  把设计系统内联进 `webui.html` / `webui-login.html` / `webui-channels.html`
  的 `<style>` 末尾（幂等，`--check` 可进 CI）。三个页面仍是单文件、零外部依赖。
- **外壳吵、数据静**：页头 / 侧栏 / 分区标题带 / 统计卡 / 分组头 / 按钮 / 空态 /
  弹窗走冲突色与硬投影；表格内容区、表单、日志行、代码块只留墨线与等宽字。
- **字体零外部请求**：展示字用系统超重黑体栈（Anton → Arial Black/Impact/PingFang SC），
  标签与数字走等宽，正文仍是系统 UI 字。**没有**引 Google Fonts（会被
  「零外部依赖」判据判红，离线部署也会退化成 Arial）。
- **分组头四色轮转**（粉 / 黄 / 酸绿 / 橙）：账号池一屏 12 个上游，同一个粉色
  重复 12 遍会糊成一片；轮转既够吵，又让"换了一组"一眼可见。墨字在四种亮底上 >= 5.4:1。
- **新增装饰**：页头旋转贴纸（`maxsticker`）、全宽跑马灯（`maxmarq`，40s 线性循环，
  hover 暂停、`prefers-reduced-motion` 静止）、空态半调网点、整页颗粒纹理
  （`body::after`，pointer-events:none，压在通知区之下）。
- **深色主题不套亮卡**：`.card` 在深色下仍是深底（否则 `final_visual` 的亮度方向判据红），
  改成深底 + 亮描边；浅色下统计卡四张四色。
- 新增 `tests/frontend/shot_maximalism.js`：一拍四屏两主题 + 登录页，产物在
  `.task/maximalism-v3/shots/`（不进版本库）。

- **可读性判据升级：抽样 5 点 → 全量逐元素。** 新增
  `tests/frontend/audit_contrast_pages.js`（+ `lib_contrast_audit.js`）：
  遍历 13 个面板 × 浅/深两套主题的每个可见文本元素，先按 alpha 合成真实底色再算
  WCAG 对比度，正文阈值 4.5、大字号 3，不达标点名 CSS 路径 + 文本 + 比值，非零退出。
  它直接抓出了抽样判据够不着的真缺陷：分组头的次要文字在粉底上 2.72:1、
  酸绿底上 1.49:1。修复后 **104 处 → 0 处**。
- **亮底文字规则**：亮色块（分组头 / 分区标题带 / 统计卡 / 侧栏分组头）内部
  文字一律墨色，层级改用字号/字重/字距表达；深色主题下 `.card` 仍是深底，
  该处的次要文字回到 `--dim`（1.07:1 的坑就是这么踩出来的）。
- **修好侧栏分组头从未生效的粉色**：`.navgrouphd`(0,1,0) 被
  `.sidebar button{background:transparent}`(0,1,1) 拍平（computed 透明），
  提到 `.sidebar button.navgrouphd` 后才是真的粉底，里面"0"徽章随之达标。
- **清掉 v2 残留写死色**：`#58a6ff`（2.24:1）与 `#0f8a4d`（3.9:1）用属性选择器
  接到 `--acc` / `--ok`；那几处是 SVG presentation attribute，属性里不认 `var()`，
  所以不动 JS。

- **「可用模型」标明来源**：无可用账号时，上游的内置手册快照（如 loomy 的 3 个生图模型）
  不再被说成"上游实时目录"—— 分组标题写「N 项内置声明 · 无可用账号」，正文上方说明
  「不是实时目录、当前不可调用」，顶栏按实际计数点名。数据保留（内置表是上游抖动时的
  目录兜底），错的是**没标来源**。
- **分组头改「墨底白字」+ 彩色边条**：白字在各冲突色上只有 1.15～2.55:1（比墨字更糟），
  所以把色从"底"移到"边"—— 底色成墨（白字 16.4:1），每个上游用 8px 左侧色条 +
  3px 底部色条轮转标记；深色主题压到纯黑（页面底色太近），靠彩条分层。
  分组头按钮反过来用奶油底墨字。

- **字体栈修正：小字号不再用窄展示字。** 上一版 `--font-display` 在 Windows 上命中
  Impact（窄体、字母侧边距小），12~15px 时相邻字母笔画相接 —— 对比度达标（16.4:1）
  但读不出来。现在 Arial Black 排第一（同属超重但字面更宽），**<=15px 一律退回系统 UI 字**，
  层级改由字重 700/800 + 字距 0.03~0.14em 表达；分组头 gap 10→12px、
  标题按钮内 gap 6→9px。顺带把「可用模型」顶栏文案从 27 字缩到 15 字。

- **分组头恢复「亮底墨字」**（用户裁决）：撤掉上一版的墨底白字 + 彩色边条，
  回到粉/黄/酸绿/橙四色轮转底 + 墨字，只保留字体与间距修复（系统 UI 字 700、
  字距 .03em、gap 12px、说明文字 500）。

- **「可用模型」按账号折叠**：没有可用账号的上游**整组隐藏**（含内置声明），
  面板、上游下拉、模型下拉一起过滤；一组不剩时给一句「先在「账号池」添加账号」。
  `accountCount` 改成三态（数字 / null=未知），只在**明确 0**时隐藏 —— 否则 manifest
  到达前会把全部上游误伤。`/v1/models` 不动（客户端仍需知道上游支持生图）。
- 顺带修好一条既有红：`models_display_gen.js` 的 `ReferenceError: KEY is not defined`
  （生成器的桩清单漏了「对话密钥」变量），34 项失败 → 全部通过。

- **渠道页操作列按钮可辨**：三个按钮原本 1px 边框互相重合、零间距，远看是一整块灰条
  （实测对比度 13:1、未禁用 —— 不是看不清，是边界消失）。改为 2px 墨线 + 相邻 6px 间距 +
  悬停反色 + 操作列 min-width:190px；只作用于渠道页，账号池的高密度表格不动（行高 40px 是硬约束）。

- **渠道页按钮「还是有问题」的三层修复**：① 删掉操作列按钮的内联 `padding/height`
  （内联会压过样式表 —— 上一版只改了 border，尺寸根本没生效）；② 按钮底色从 `--panel`
  改成 `--panel2` 并加 2px 硬投影（原来与行底色同色，只剩边框能区分）；③ 构建必须输出到
  不被占用的文件名 —— 上一轮 `go build` 因 7863 正在运行该 exe 而**失败**，验证用的是旧
  二进制，等于没测。现改为 `bin\wb2api-server-next.exe`，验证通过后再替换。

### 未改动（刻意）

- 类名、DOM 结构、JS 逻辑、热力图 5 档色阶（`--hm0..--hm4` 仍严格照抄 openViking 的绿阶）、
  留白紧度（上一轮刚按"更紧凑"收紧过）、任何后端代码。

### 复验

    node tools/theme_sync.js --check
    go test ./internal/server/... -count=1
    node tests/frontend/shot_maximalism.js
    # 需真实例：contrast_e2e（AA 实算）/ final_visual（亮度方向）/ theme_e2e（三态 + 对比度）

对照度实算（浅 / 深）：thead 14.1/15.0 · tbody 15.4/16.5 · .pill 14.2/14.2 ·
.pill.dim 12.7/12.6 · pre 7.8/9.9 —— 全部 >= 4.5:1（AA）。

## 未发布 — 2026-10-19

> 本版主题：**登录系统收成单账号管理员**。用户原话：「我们那个用户和管理员，
> 我觉得可以只要一个管理员就行了，只要一个账号。用户其实可以不用。」
>
> ⚠ 这是**减少一种身份**，不是"暂时关掉注册"：成员（角色 1）从模型里取消，
> 所以注册入口在这个部署形态里**不存在**（唯一剩下的那次建号是空库首启，
> 建的是管理员）。上一版 v1.9.2 描述的"管理员 / 成员分级 + 注册建成员"
> 是当时的实现，条目原文保留不动。

### 变更（破坏性：成员角色取消）

- **只有一个账号，且它一定是管理员。** 建号只剩**一条**路：账号库为空时的
  首启 bootstrap（服务端写死管理员，角色 10）。原先带 `role` 参数的建号口
  `Store.Create` 与改角色的 `Store.SetRole` **整条删除** —— 它们就是"能建出/
  能改成非管理员"的两条路径。仓库里留了一条**类级静态判据**
  （`internal/account/account_test.go` 的 `TestNoRoleSplitResidueInSource`）：
  扫 `internal/` 与 `cmd/` 下全部非测试 Go 源，出现 `RoleMember`（除只读兼容
  的那两个文件）、`SetRole`、`json:"member"`、`register_disabled` 就红，
  并用合成坏桩反向验证该判据不是空转。
- **注册只在首次，之后端点整体关闭。** 账号库为空时 `POST /admin/auth/register`
  建出那个管理员；库非空之后同一端点一律 **`403` + 稳定码 `register_closed`**
  + 明确文案（「本部署为单账号管理员模式：注册只在账号库为空时开放（用于建
  第一个管理员），账号已存在则注册入口关闭。请用管理员账号登录。」）。
  ⚠ 刻意**不沿用**旧的 `register_disabled`：那个码的语义是"管理员在配置里关掉了
  公开注册（成员还能不能建是另一回事）"，与本条不是同一件事，混用会让排查方向
  跑偏。请求体里带 `role` 之类字段在**解码层**就被拒（400 `bad_request`）
  ⇒ 没有任何姿势能注入角色；有一条用例连打 6 种请求体断言账号数一个不变。
- **`console.disables_register` 在本版本已不改变行为。** 键仍被解析（旧配置文件
  照收，不报错），但既不挡首启也不影响登录页；`cmd/server` 启动日志会如实说明
  （不静默忽略）。注册入口与否只由"账号库空不空"决定 —— 改造前那是**两个来源**
  （一个读配置、一个查库），必然有一天互相矛盾（配置说"开"、库里已有账号
  ⇒ 页面给出一个点了必然 403 的入口）。登录页的第二个标志位（`register`）
  与占位符一起删除，`__WB2API_LOGIN_FLAGS__` 里只剩 `bootstrap`。
- **盘上的非管理员历史记录：认出来、拒不收、不崩。** 加载时把非法/成员角色
  归一化到最小权限，并打一条启动日志点名（「…里有 N 条**非管理员**历史记录…」）；
  登录回 **`401 member_retired`** +「成员账号已取消，请用管理员账号登录」
  + 一条服务端日志。⚠ 判定顺序是**先验口令再看角色**，所以配错口令仍然是
  `bad_credentials` —— `member_retired` 不构成"这个用户名存在吗"的枚举口。
  同库的管理员照常登录；盘上的 `role` 值**不被静默改写**（只在内存里归一化）。
- **旧会话在 `SessionStore.Lookup` 就被拒并顺手撤销。** 只堵登录口不够：
  改造前签发的成员会话（或账号文件被回滚、被人为编辑过）如果还能通过 Lookup，
  `/ui` 会照样把控制台外壳发出去（`account.Identify` 走的就是 Lookup）
  ⇒ 等于"成员面通过一条旧 cookie 又活了"。现在那条会话返回 `ErrMemberRetired`
  并被 `Revoke`。
- **管理面的角色分流删除。** `internal/admin` 那张"成员白名单"
  （`memberRules`，7 个精确 (方法, 路径) 对）**整张删除**，`canAccess` 只判管理员。
  判据 `p.IsAdmin()` 与历史遗留的 `403 forbidden_role` 分支**保留**为 fail-closed
  （口径下不可达 —— 非管理员在 `account` 那层就换不到身份，请求在 401 分支
  就结束了），代码里有注释写明"为什么删表、为什么留判据"。`GET /status` 的
  403 文案不再提"成员"。
- **`GET /admin/apikeys` 载荷里的 `member` 字段删除。** 它原先只服务「我的 API Key」
  标题，恒等于 `!IsAdmin()` —— 单账号口径下恒为 false，留着就是一条"这里还分角色"
  的假线索。`keys` / `admin_key` / `admin_token` 三个前端必需的键不动；
  前端读到 `undefined` 会走管理员渲染路径（与改造前管理员的行为一致）。
  key 的**归属过滤**（`ListFor`/`CanManage`：空 owner + 非管理员 ⇒ 空列表、
  身份缺失 ⇒ 管不了任何 key）**保留**为防御纵深 —— 它是"装配层忘了接线"时的
  最后一道闸门，与"成员是否存在"无关。
- **前端：注册入口与"成员模式"去掉，403 不再静默降级。** 登录页不再有注册/
  角色开关（`__WB2API_LOGIN_FLAGS__` 只剩 `bootstrap`）；控制台不再有
  `.member-mode` 与 `applyMemberMode()`。⚠ 关键的一条是**语义**：改造前
  "403 ⇒ 静默进成员模式"，现在没有"权限不足但还能用"的第二态 —— 只有
  "未登录（401 ⇒ 回登录页）"与"就是管理员"。404/403 不再被吞成降级。

### 验收（本次改动）

- `go build ./...` exit 0；`go vet ./...` exit 0（零输出）；干净 LF 检出树
  `gofmt -l .` **0 文件**。
- `go test ./... -count=1` → **34 包 ok / 0 FAIL**（37 个包里余下 3 个是无测试
  文件的 `cmd/*`；工作树里的 `docs/agent2api-repo` 是未跟踪的本地产物，不在
  提交范围内，故按包排除后统计）。
- 定向实测（`go test -v -run`）：空库首启建号 → 角色 10；库非空后注册 →
  `403 register_closed`；盘上 `role=1` 记录登录 → `401 member_retired`、
  不下发会话、同库管理员照常登录。
- 测试改写纪律：凡是"测成员这个身份"的用例，要么改成语义相反、更强的判据
  （成员历史记录**不构成身份** ⇒ 401），要么删除并在文件里写明"它测的那个面
  已经不存在、由谁接管"。没有为了变绿而放宽任何断言。

## v1.9.2 — 2026-10-19

> 本版主题：**抄 new-api 的登录系统**（你原话：「直接把 newapi 的登录系统抄过来」），
> 顺带把你早先报的三条旧障一起收口 —— 它们同根：控制台没有"你是谁"这个概念，
> 凭据只有一把 `api_key`，它同时是对话钥匙、门禁、和写在页面里的那一行明文。
>
> 现在分三把：**账号口令**（浏览器，HttpOnly 会话 cookie）、**`admin_token`**
> （脚本/CI 进管理面）、**`api_key`**（只给客户端调 `/v1`，进不了管理面）。
>
> ⚠ 一处**破坏性变更**：用 `api_key` 打 `/status` 或 `/admin/*` 现在会 401。
> 有脚本这么用的，改成 `Authorization: Bearer <admin_token>`。

### 三条旧报障的收口

**① `config.example.json` 里没有 `admin_token`**（新部署照示例配，永远只拿到一把钥匙）

`admin_token` 已写进示例文件并带中文说明（它为什么必须与 `api_key` 分开、
留空是什么语义、怎么免重启轮换）。验收：

    node tests/frontend/verify_token_security_e2e.js     # ⑥ 段：PASS config.example.json 写出了 admin_token

**② 轮换/改密之后必须重启**（"轮换之后网页加载失败，必须重启"）

三个动作全部**免重启、立即生效**：

| 动作 | 在哪 | 立即生效的机制 |
|---|---|---|
| 轮换 `admin_token` | API 管理页 | 写回 config.json + 更新内存副本（`SetAdminToken`） |
| 轮换 `api_key` | API 管理页 | 同上（`OnAPIKeyRotated` 回调） |
| 改自己口令 | 顶栏「改密码」 | 账号 AuthVersion +1，**当前会话就地升级**（抄 new-api 的 `AdvanceUserSessionAuthVersion`）⇒ 本人不掉线、其它设备立即失效 |

改口令那一条是本次新加的端点 `POST /admin/auth/password`：验旧口令 → 改 → 把
**执行改动的这条**会话升到新版本号。验收：

    node tests/frontend/verify_token_security_e2e.js     # ④⑤ 段：新旧令牌/密钥立即生效与失效
    go test ./internal/account/ -run TestChangePassword -v
        # ★★ 改完口令当前会话仍有效（用户报障的那一条）+ 另一台设备 401 session_revoked

**③ 错误的管理令牌也能进控制台**（页面把钥匙注入 HTML，一打开就泄露）

`/ui` 的响应体里**不再有任何凭据**（三种情形都实测搜过：未登录 / 已登录 /
`?token=` 换手），登录状态改由 HttpOnly cookie 承载；控制台脚本里也不再有
`wb2api.key` 这类 localStorage 记忆。验收：

    node tests/frontend/verify_console_cookie_auth_e2e.js --start
    node tests/frontend/verify_token_security_e2e.js

### 登录系统（新）

- **端点**：`/admin/auth/{register,login,logout,session,password,captcha}`，
  挂在管理端门禁**之外**（否则"没登录所以打不开登录接口"死锁）。
- **角色**：管理员 10 / 成员 1；注册一律成员（服务端写死，不接受调用方指定）；
  成员只能看/管**自己名下**的 API key，其余管理面 403 `forbidden_role`。
- **首启建管理员**：账号库为空时第一个注册成为管理员（关闭注册也**不**挡这一条，
  否则"关了注册又没建号"会把控制台锁死）。
- **反批量四层**：限速 20 次/20 分钟/IP（第 21 次 429）、请求体 8 KB（413）、
  本地 SVG 人机校验（**默认开**，题一次性、答错也作废）、会话签发上限。
  四层拦的形态不同（同 IP 高频 vs 多 IP 各几次），故而不是"开一层就够"。
- **`/status` 收归控制台身份**：它回的是**账号池画像**（上游账号列表 + 熔断态），
  以前 `api_key` 就能读 —— 那是"发给客户端的钥匙能看运营数据"。现在只认会话或
  `admin_token`，且必须是管理员。
- 控制台顶栏新增当前登录者 / 角色标记 / **改密码** / **登出**。

### 验收（一条命令）

    node tests/frontend/run_http_suites.js
    # 4 条 HTTP 探针 × 2 遍（--self-test 判别力 + --start 真实例）= 8 次运行，全绿 17 秒
    #   login_flow    登录流程正路
    #   abuse         限速 429 / 请求体 413 / 人机校验
    #   console_auth  /ui 三条路 + /status 四种身份 + 会话升级与撤销
    #   role_split    key 归属过滤 + /admin/* 角色分流 + rotate 仅管理员

    全仓 go test（37 包）全绿；gofmt 合规。

⚠ 探针起实例时**关掉人机校验**（算术题是给人看的，自动脚本答不了）——
`lib_instance.js` 里写明并留了"想验开着的那条路"的开关。

### 顺带修掉的一个真洞

管理面白名单原来是**前缀**匹配：`/admin/apikeys` 会连带放行
`POST /admin/apikeys/rotate-admin` —— 而那个端点的回执里带**完整的新
`admin_token`**。成员调一次即可提权到管理员。现改成精确的「方法 + 路径」对，
两个 rotate 端点另加一层管理员硬判。回归测试把这条钉死了
（`internal/admin/gate_test.go` 的 ★★ 断言 + `verify_role_split_e2e.js` 的坏桩③）。

### 发版前：无头浏览器视觉验收又挖出三条（同一批修掉）

新探针 `tests/frontend/verify_login_visual_e2e.js`（自带 CDP + 30 张截图 + 几何判据 +
**判别力自检**：好桩 0 红、三个坏桩各自让目标判据变红）在真实例上跑出三条**静态阅读
看不见**的缺陷 —— 三条都只在特定身份或特定时间窗里出现：

| # | 现象（探针实测） | 真因 | 
|---|---|---|
| ① | 成员会话里 `TypeError: Cannot read properties of null (reading 'accounts')`，面板保持开着等 35s 也不刷新 | `/admin/accounts` 对成员 403，而 catch 回落那句 `s.accounts` 的 `s` **也是 null**（`/status` 同样只给管理员）⇒ catch 自身再抛、且**没有外层 try** ⇒ `refresh()` 整段中断（含 30s 轮询） |
| ② | 新建 API key 后 3.2s 列表仍是 0 条；**重开面板 10ms** 就看到 | 创建路径漏了 `writeInvalidate([EP_APIKEYS])`（删除/轮换都有）⇒ `loadAPIKeys()` 用 30s 旧快照重画 |
| ③ | 同一浏览器换身份后，「管理钥匙」按**上一个身份**的快照渲染 | 面板缓存（落 localStorage、TTL 30s、跨导航存活）的 key 只有 URL、**没有身份** |

③ 的反方向是：**管理员先登录、30 秒内成员登录 ⇒ 成员界面上会画出管理员的 key 列表**
（服务端拦得住请求，界面已经在显示别人的数据）。窗口只有 30 秒，**人工几乎撞不到** ——
这是"视觉验收不能省"的最硬证据。修法：缓存加身份命名空间
`wb2api.pcache.<scope>.<key>`（`token` / `u:<id>:<role>` / `anon`），身份一确定就切。

修完该探针 **95 PASS / 0 FAIL**、自检 4/4，`console.error` / 未捕获异常 /
`Network.loadingFailed` 三条硬判据全空。

### 发版前回归实测（2026-10-19）

    go build ./...                                                 ok
    go test（37 包，排除未跟踪的 docs/）                            0 FAIL / 34 ok
    go vet / go mod tidy 后 git diff                               ok / 无改动
    node tests/frontend/run_http_suites.js                         8 次运行全绿
    node tests/frontend/verify_login_visual_e2e.js [--self-test]    95/0  ·  4/0
    node tests/frontend/verify_models_grouped.js                    43 / 0
    node tests/frontend/verify_model_provider_filter.js             14 / 0（连跑 6 次稳定）
    node tests/frontend/verify_delete_buttons_ui_e2e.js             19 / 0
    node tests/frontend/verify_all_buttons_e2e.js                   42 / 0
    node tests/frontend/audit_dead_buttons.js                       6/6 自检 + 40 绑定 / 0 死按钮

⚠ 探针层的"假红"也一并清了（那些不是产品问题，是探针前提过期）：产品自 T5 起不再把
凭据注入页面 ⇒ 探针自己带 key（`lib_cdp.probeApiKey` / `injectProbeKey`）并像人一样
粘一次（`pasteConsoleKey`）；静态检查段改走**已认证**取源码（`consoleHtmlAuthed`，
裸 `http.get` 不跟 302 会拿到登录页，反向断言在空串上还会空洞通过）；固定 `sleep`
一律换成有界轮询（同环境曾出现 4/3、13/1、14/0 三种结果）。

## v1.9.1 — 2026-10-19

> 本版主题：**修「导入之后删除不掉」**（你带着截图报的那条），以及它牵出来的
> 界面侧两个并列缺陷。
>
> 值得单独说一句：这三条缺陷**各自都能独立造成同一个现象** —— 后端把文件留着、
> 界面拿旧快照重画、成功提示被一条红条盖掉。只修一条，你点下去仍然会觉得
> 「改了还是那样」。所以三条都修，并且都留下探针与回归测试。
>
> 全程用**合成数据**（临时目录里当场编凭证 + 真浏览器点按钮），没有碰你的账号池。

### 缺陷一（后端）：loomy 漏传「凭证文件在哪」⇒ 删号删不掉文件

现象：账号池点「移除」，回执 200、那一行也从池子里没了 —— 但**凭证文件还在盘上**，
下次「重载 auths」或重启，`LoadDir` 又把它装回池子。读起来就是「删了又复活」。

实测（`tests/frontend/verify_delete_flow_e2e.js`，合成凭证 / 临时目录 / 端口 7881）：

    DELETE /admin/accounts/{uid}?purge_file=1
    → 200 {"file":"","purged_file":"","removed":true}      服务端日志 deleted=""

根因：`internal/loomy/provider.go` 的两条投影都只给了 `{Provider,UID,Nickname}`，
**漏了 `FilePath`** —— 其余 9 个上游全带了。而装配层 `poolAuthsFromCreds` 只在
「secret 恰好是 `*auth.Auth`」时直接用 secret 里那份（workbuddy 是这种），
loomy 的 secret 是自己的 `*loomy.Auth` ⇒ 类型断言不成立 ⇒ **落到投影** ⇒
池条目的 `FilePath` 恒空 ⇒ `purge_file` 静默空转。
（`gateway.Credential.FilePath` 的注释里早就写着这条后果，只是没落到代码。）

修法三条一起上：

- 两条投影都带上 `FilePath`（并注释后果，防住下一个人顺手删掉）
- `accountDelete` 加**兜底定位**：池条目路径为空时，用**上游自己的扫描器**
  （`gateway.CredentialLoader`）重新定位一次；目录判据与导入 / 重载共用同一份实现
  （新 `credentialdir.go`，原 `importDirFor` 正名为 `credentialDirFor`）。
  这条兜底不只修 loomy：池子从旧 `state.json` 恢复、上游将来又漏传，都能救
- 删不掉时**如实回执** `purge_error`（路径未知 / 越界拒绝 / 删失败），前端据此
  把「已彻底删除」换成如实提示 —— 静默失败正是这条缺陷的核心

### 缺陷二（界面）：写完了就刷新，可刷新读到的是 30 秒前的旧快照

这是**真浏览器点按钮**才暴露的那条（HTTP 探针看不到）：`sharedGet` 的缓存 TTL 是
30 秒，而且落 localStorage。删除走 `admin()`（不过缓存，这是对的），但紧跟着的
`refresh()` 走 `sharedGet('ep:/admin/accounts')` ⇒ 命中旧快照 ⇒
把刚删掉的那一行**原样画回来**。

实测（`tests/frontend/verify_delete_buttons_ui_e2e.js`，真 Chrome、合成数据）：

    点「移除」   → DELETE 10ms 就回了 200
    表格那一行   → 20 秒不消失
    apiKey 删除 → 后端已经空了，列表还显示那把 key

修法：`writeInvalidate()` —— 写操作之后、刷新之前丢缓存（顺序不能反，反了等于没丢）。
调用点覆盖账号池「移除」、`act()` 收尾（禁用 / 启用 / 清冷却 / 每日动作）、
apikey 删除与重置。

### 缺陷三（界面）：一条红条盖掉了成功提示

`loadClientLogin()` 打 `/admin/client-login`，而本机登录管理是**可选功能**：
未启用时回 503「本控制台未启用客户端登录管理」—— 那是正常状态，不是失败。
但 `admin()` 默认把任何非 2xx 播成红色 toast：

    +20ms   已彻底删除 xxx                          ← 用户该看到的
    +130ms  GET 失败：本控制台未启用客户端登录管理    ← 把它盖掉了

用户看到的是后者 ⇒ 读成「点了移除之后报失败」。修法：该调用传 `{silent:true}`
（失败仍被 catch 降级成控制条「不可用」，只是不再播报）。

### 检查范围（你要求的「检查一下所有的删除按钮」）

全应用**只有两个销毁性按钮**，两个都实测过：

    ① 账号池 · 移除   → DELETE /admin/accounts/{uid}?purge_file=1
    ② API 管理 · 删除 → POST   /admin/apikeys/delete

其余看着像删除的都**不是**销毁操作：`enable`/`disable` 只改状态、`cooldown/clear`
只清冷却、`apikeys/reset` 是换 key 不删条目、「清空」是清选择。

### 验证

- 新增两个探针（合成数据）：`verify_delete_flow_e2e.js`（后端行为）、
  `verify_delete_buttons_ui_e2e.js`（真浏览器点按钮，断言落在可观测量上：
  请求 / 回执、**磁盘上文件在不在**、**表格行多久消失**、toast 时间轴含"谁盖了谁"）
- 新增回归测试：`internal/loomy/credentialloader_test.go`（两条投影必须带 FilePath）、
  `internal/admin/account_delete_test.go`（正常删 / **没路径时靠扫描找回** /
  找不回必须报 `purge_error` / **拒绝删凭证树之外的文件** / 不带参数保持老语义 /
  未知 uid → 404）
- `tests/frontend/lib_cdp.js` 补对话框自动接受 —— 没有它，任何被 `confirm()`
  守卫的按钮都测不了（headless 里没人处理对话框时页面会卡住，请求压根不发，
  看起来像"按钮坏了"，那是测量假象）
- 修前 6 条 FAIL → 修后全绿：行 2ms 消失（点击到消失 874ms）、成功提示在、
  0 条失败提示、0 条未捕获异常；既有「全部按钮」套件与三个 Go 包全绿

## v1.9.0 — 2026-10-19

> 本版主题：**控制台重做 + 支撑它的面板缓存分层**。
>
> 出发点是你的一句话：「优化一下前端有关运维的东西，参考知识库中的项目实现，
> 看这来能 1 比 1 抄就抄」。所以这一版的控制台几乎每个页面都在**照抄成熟实现**
> —— 任务调度页 / 请求日志页抄 Agent2API 的 `page-tasks.css` / `page-requests.css`，
> 账号池的列宽拖拽抄它的 `table-columns.js`（连那六条铁律一起抄过来：
> 只动被拖的那一列、表级 min-width 必写、松手必须收得到、按列键寻址而非下标、
> 双击夹子复原、上限只是护栏），热力图与环图抄 openViking。
> **没有新增任何第三方依赖** —— 连图表库都没引。
>
> 后端做的是同一件事的另一半：面板数据不再"打开才算"。分层 TTL
> （额度 3 分钟 / 任务 30 分钟 / 邀请码 6 小时）+ 服务端持续预热 + 缓存落盘，
> 目标是**打开页面毫秒出整页**（验收脚本 `tools/verify_millisecond_open.py`）。
>
> 另外修掉一批你当场指出来的毛病。其中两个值得单独说，因为它们都属于
> 「看起来在工作、其实没有」那一类：
>
>   · 账号池左上角那块**黑色的东西** —— 是 12 个关着的 `⋯` 菜单全部可见、
>     并且整齐堆在坐标 (0,0)
>   · 「刷新」按钮点了界面**一动不动** —— 查下去发现那个按钮**从来没有刷新过**，
>     它发出的请求和首次加载一模一样

### 修复 — 账号池左上角的黑色方块（一个 CSS 覆盖链）

现象：切到**账号池**时左上角出现一块黑色矩形，别的页面没有。

前两轮我都判断错了方向（先去删顶栏品牌图标，再在没有弹出层的状态下扫
`elementFromPoint`）。真正的线索是你补的那句「**只有选择账号池才会出现**」——
账号池是唯一有 `⋯` 菜单的面板。查下去是一条覆盖链：

    .ops-menu { display: flex; }        ← 这条写在外层

原生 popover 的 `display: none` 是**UA 样式表**给的，而作者样式优先级更高 ⇒
12 个处于"关闭"状态的菜单**全部被渲染出来**，各自 `position: fixed` 且没有
inset，于是都落在 (0,0) 叠成一块黑。

修法：`display` 只写在 `:popover-open` 里，外层不写。同一次还修了 `.colset`
浮层的定位 —— 它自己写了 `margin: 0`，把 UA 用来居中的 `margin: auto` 盖掉了，
于是 `inset: 0` + `margin: 0` 也落到 (0,0)。

> ⚠ 教训：popover 的 `display` 写进 `:popover-open`，**不要写外层** ——
> 外层那条会盖掉 UA 的 `display: none`，而关着的元素一旦被渲染就是可见的。

### 修复 — 「刷新」按钮其实从来没刷新过

你的两次报障是「这个刷新按钮修复了吗」和「那个刷新按钮还是没用」。

第一次我查的是**它有没有发出请求**，结论是"发了、按钮是好的"—— 技术上没错，
但**语义上是错的**。第二次你把话说得更直，才查到底：

    /admin/loomy/invite?page=1             170ms    from_cache=true    ← 刷新一直走的这条
    /admin/loomy/invite?page=1&refresh=1  17110ms   from_cache=false   ← 真的去查上游

`?page=` 那条分支是刻意的「**只读缓存、零上游请求**」（为了打开页面先出上次的数据），
而刷新按钮发的 URL 与首次加载**一字不差** ⇒ 拿回同一份 JSON，界面当然一点不变。

同一页 25 行的实测对比：

    from_cache             true → **false**
    balance_known = false     5 → **0**
    有码的行数               23 → **25**

而 `fetched_at` 停在 23:38、你 04:08 点刷新纹丝不动 —— 这是铁证。

根因有三层，缺一层都不成立：

    ① 后端压根没有"强制重查"的入口
       expiredInvites(now, force) 的 force **当时没有任何 HTTP 入口**，
       panelwarmup.go 两处硬编码 false；而 TTLInvite = 6 小时，
       所以定时扫描也不会碰它 —— 6 小时内怎么点都不会变。
    ② 前端 force 被吃掉两处
       loadProviderPanel 的 `fn(providerId, cap)` 没往下传；
       PROVIDER_PANEL_RENDERERS 里的 `(pid) => loadLoomyInvite(pid)`
       自己只接两个参数，把第三个 force 吞了。
    ③ tasks 与 invite 是**逐字相同**的两份
       4 处 URL 拼装（各 2 处：fresh 与 d）都要改。

这类 bug 阴的地方在于：**不报错、有请求、有回执，只是回执是旧的**。

改动：后端加 `refreshRows[T]` + 两个 handler 的 `?refresh=1` 分支（限并发沿用
`panelFillConcurrency` —— 一页 25 号、每号 3 个上游接口，全量刷会给上游砸 132 个
并发，而那个常量存在的理由正是"打太狠会让整列变成查询失败"）；前端把
`refresh=1` 从按钮一路透到 URL，并把 URL 拼装抽成 `panelPageURL()` 一处，
从结构上杜绝"改一处漏一处"。

> ⚠ 教训：跨层透传参数要逐层核对 —— 一个 `fn(a, b)` 和一层箭头函数就能把它
> 静默吃掉。表现是"功能像是好的，只是数据永远是旧的"。

### 前端 — 照抄清单（AGENTS 第 8 条）

    任务调度页      6 列表格 → 卡片网格       抄 Agent2API page-tasks.css
    请求日志页      table   → grid 表格       抄 Agent2API page-requests.css
    账号池列宽      拖拽 + 横向滚动容器       抄 table-columns.js（六条铁律一并抄）
    账号池列设置    每上游自选列 + 拖动排序   抄 Agent2API 的列设置浮层
    账号池对齐      8 列跨组错位              列宽收归一处说了算
    操作列          一堆按钮 → 3 直显 + ⋯ 收纳
    弹窗/浮层       提示不被盖住、跟着滚动重定位
    表格            一律自适应列宽；行高 45 → 40px（AGENTS 第 5 条"固定高度一行"）
    热力图          尺寸/配色与 openViking 对齐，加悬停浮层与近 7 天明细
    仪表盘          缓存命中率四格、双轴趋势、环图、按天柱状图
                    （照 agent2api 的报表页，零依赖手写）
    统计范围档位    今天 / 7 天 / 30 天 / 本月 / 全部 → /admin/stats?range=
    概览卡          口径用 `?` 悬停（照 agent2api 的 TipQ）

顺带修掉的静默毛病：设置页 subgroup 虚线失效、热力图周标签错位、
Top 模型/活跃天数三张卡被 30 秒轮询擦成「—」、余额为 0 时显示成「—」。

### 新增 — 面板缓存分层（怎么权衡写进了设计文档）

`docs/cache-refresh-design.md` 是这一块的设计稿。落地分五步：

    ① 面板行带 FetchedAt           分层过期判定的唯一输入
    ② 分层 TTL（纯函数）           额度 3 分钟 / 任务 30 分钟 / 邀请码 6 小时
    ③ 扫描"未缓存优先 + 过期次之"  on-read 与定时兜底的共同落点
    ④ 服务端**持续**预热           让"打开页面毫秒出整页"不再有洞
    ⑤ 面板缓存**落盘**             前端读本地文件 ⇒ 毫秒级（AGENTS 第 9 条）

预热从"顺序跑"改成**限并发 4**：44 个号从约 7 分钟降到约 2 分钟；间隔改成动态的
（有活就等 5 秒，没活就等 5 分钟）。`tools/verify_millisecond_open.py` 把
「打开页面毫秒加载整页」变成可跑的验收。

顺带修掉三处「查失败被显示成 0」：`balance` 补上"有没有读到"（`balance_known`），
没读到显示 `—` 而不是 0；余额真是 0 时正常显示 0（原来的 falsy 判断把 0 当成了"没有值"）。

### 额度刷新与熔断 — 让"上游整体坏了"看得见

    额度自动刷新       每 3 分钟一轮，修"前端显示的额度是过时的"
    自适应降速         全局预算的最后一道闸门
    按"最近用过"排队   事件驱动 —— 一次调用无论成败都花钱，所以用 LastUsed 排序
    选号排除无额度     明确知道没额度的号不再尝试
    上游级熔断         上游整体坏了，不必等 44 个号逐个熔断
    /status 带上游维度 让"上游级熔断"看得见
    首字节速度 EMA     各上游实测速度 + "明显更慢"的判据
    顶栏「上游健康」   熔断中的或最慢的那家，一眼可见

### 路由与留痕 — 判决要能复盘

    decision 列        判决留痕（抄 new-api 的 PolicyDecision）
    pre_ms 列          把「首字耗时」拆成"我们花的时间"与"上游花的时间"
    client_ip/client_ua 指认"谁在直连网关发非流式"
    reasoning_efforts  /v1/models 每条补上，让客户端能选思考深度
    确定性错误独立成类 参数错误/上下文超限不再换号重试
    预算用尽也留痕     attempt_budget_exhausted

⚠ **一处行为变更**：停用「换上游」—— 客户端点哪个模型就只在那个上游的号里选，
不再按速度优选改派（这是你明确要求的：「我不需要这个优选的逻辑」）。
但保留了一条**兜底**：选定上游**一个号都选不出**时，自动换一个同模型的其它上游再试
（`78e3ec3` / `97f0a30`），避免"某个上游的号恰好全在冷却"就整体 503。

### 修复 — 一批"静默失效"，以及五个 flaky 测试

    b1cabab  cmd 的三段启动动作原来写在 `if err != nil` 的**错误分支**里
             —— 从来没有执行过。这是本版最典型的一处"看起来在工作"
    3d6bd3d  在途名额被自己挡住导致工具循环必 503（还带误导性提示）
    03df122  MiniMax 转换失败不带哨兵，白试 N 个账号后才报"池子故障"
    99d0f12  缓存命中率读不到数 —— 漏认嵌套键 prompt_tokens_details.cached_tokens
    157435f  撤销"在预热循环里刷额度"—— loomy 这层写不了余额（编译期就断）
    5a135b9  三个"午夜窗口"flaky 测试（同一类坑的第三、四、五次）
    b946a7c  TestPoolNoRecheckForKnownZero 是时间敏感的 flaky

### 发布前的一件小事 — 13 个文件让 CI 的格式门禁会红

`release.yml` 的 verify job 里有 `gofmt -l .`，非空即 `exit 1`。而仓库里
**存的字节**有 13 个文件不符合 gofmt（blob 里是 CRLF，违反本仓"CRLF 工作树、
LF blob"的约定）⇒ 打 tag 后 release 会卡在 verify，构建与资产上传一步都不会跑。

在 `core.autocrlf=true` 下这种"仅换行符不同"的改动**完全隐形**：`git diff` 看不见、
`git add` 存不进去 ⇒ 所以 `gofmt -w` 之后仓库里的字节一个都没变，本地怎么查都以为
是好的。可靠判据只有一个 —— 把**仓库里存的字节**按 LF 导出再跑 gofmt：

    git -c core.autocrlf=false -c core.eol=lf checkout-index -f -a --prefix=tmpcheck/
    gofmt -l tmpcheck/          # ← 等价于 CI 的 gofmt -l .

它给出真值 13 个（本机直接读 gofmt 会得到"10 个""8 个"两个都错的数）。
修法：`git -c core.autocrlf=false add` 把这 13 个文件按工作区字节写进索引，
blob 才真正变成 LF。改动**纯注释与空白**（`git diff -w` 里没有任何非注释行）。

### 本版还包含

    08a964e 账号池「列设置」—— 每上游可自选显示哪些列、拖动排序
    b8859e9 账号池列宽拖拽 + 横向滚动容器
    1b05af3 去掉顶栏品牌方块 + 所有表格改为自适应列宽
    b0c00ac 设置页 subgroup 虚线静默失效 + 数字框收窄居中
    0261035 请求日志页从 table 换成 grid 表格
    d720223 任务调度页从 6 列表格换成卡片网格
    bb97d0d 热力图尺寸与 openViking 对齐 + 右侧补近 7 天明细表
    98b54a1 热力图配色严格照抄 openViking + 修周标签错位/图例色块
    19e35fc 活跃热力图加悬停浮层与右侧统计卡
    6454008 任务调度页任务名显示成「中文 (英文)」+ 悬浮详细描述
    e9747f9 cache_rates 每档带样本量 n（供前端显示「· N 次」）
    ed4aa29 /v1/models 每条补上 reasoning_efforts
    e018392 用测试守住 jobLabels / jobDescs 两张表
    22984a1 入库 tools/ovq.py —— 直连知识库的查询器
    3864ba8 / 642300d 入库 docs/known-issues.md 并记录未解项
    d99126a 对照 new-api 的逐模块结论

## v1.8.0 — 2026-10-14

> 本版主题：**新增两个上游** —— 第十一个 **ZCode**（Z.ai / 智谱 GLM
> Coding Plan）与第十二个 **MiniMax Code**（中国版）。两者都是
> **非 OpenAI 协议**（Anthropic Messages）的上游，所以本版也带来了
> 本仓的协议转换层，并在第二个消费者出现时把它**抽成了共享包**。
>
> 另外修了一批用户实际报障的缺陷（序号见文末「本版还包含」），
> 其中「20715653 天」那条是我自己写错单位、且**被自己的测试背书**的
> 一个真实 bug。
>
> ⚠ **本版发布过一次又撤回了**（2026-10-10）：初版打完 tag 后用户要求
> 「再测试一下」，于是 Release 与 tag 都已撤销。此后又积累了一批改动
> （见下方「配置与可观测性」与「修复 — 续期链路」两节），本次一并发布。
>
> ⚠ **有一个行为变更**（唯一的破坏性变更，见下）：上游不再有
> `enabled` 开关，**一律启用**。

### 行为变更 — 上游 `enabled` 开关被彻底移除（一律启用）

**这是本版唯一的破坏性变更**，且改变的是**默认行为的方向**：

    改前：  config 里 `"<上游>": {"enabled": ... }` 缺席 = **不启用**该上游
    改后：  `enabled` 字段被移除；上游**一律启用**，段缺席也照常启用

对**已经显式写 `enabled: true`** 的部署（例如本仓自己的 `config.json`）
行为不变。会受影响的是"靠**不写** `enabled` 来关掉某个上游"的部署 ——
升级后那个上游会**开始跑**。配置里残留的 `"enabled": false` 会被当成
未知字段**忽略**（与其它拼错的键一样）。

这么改是用户要求的（原话：*"我不需要这个凭证全失效，安静下来停一个上游，
不需要。就是默认全部停"*）。要停用一个上游，把它的凭证从 `auths/` 移走。

相关：启动时逐个上游都会打一行「已启用」，所以"它到底跑没跑"始终可见
（原先那个 `else { 未启用 }` 分支已成为死代码，一并删掉）。

### 配置与可观测性

#### config.json 支持注释（JSONC 子集）

`//` 与 `/* */` 都可以写了。**这是"重写一份说人话的配置"的前提** ——
标准库 `encoding/json` 不认注释，不支持就只能是"把字段排整齐"，
而后一个人（包括几个月后的自己）仍然不知道 `soft_rate` 是什么。

剥离器是**字符串感知**的：`"https://..."` 里的 `//` 不会被误当注释
（这是最容易写错的一条，配了 8 个用例）。剥离**保持等长**（注释位置换成
空格、换行原样保留），否则报错行号会整体前移 —— 按坐标去文件里找会找到
别的东西，比不报位置更糟。

#### 轮换管理钥匙不再弄乱 config.json

用户报：*"我发现这个 Config 配置有点太丑了，不知道怎么回事啊，太乱了我只能说。"*

查下去发现根因是 `POST /admin/apikeys/rotate-admin` 的旧实现
（`map[string]any` → `MarshalIndent` **整个文件重写**）：**注释全丢、
顺序全变、空行/缩进风格全变**。而轮换一把钥匙只需要改一个字符串。

改成**字节级替换那一处的值**，其余逐字节保留。实测（带注释的配置轮换一次）：

    注释行数  73 → 73        行数 165 → 165
    不同行数  1              ← 只有 api_key 那一行

另外 `config.json` 本身也重写了一份：去掉所有"写了等于没写"的默认值、
去掉空字符串字段、按逻辑分组排序，加上 73 行中文说明。

#### 启动横幅

用户要求：*"运行之后，命令行里需要打印出网关地址是多少"* +
*"前端如果说他没有这个、需要填写那个东西，你需要提醒他去哪里查看"*。

改造前只有一行 `listening on 127.0.0.1:7863`（没说 http 还是 https、
没给控制台路径）。现在是：

    multi2api 已启动
      控制台   http://127.0.0.1:7863/ui
      API 基址 http://127.0.0.1:7863/v1
      管理钥匙  未打印（避免留在终端历史/日志里）—— 在 config.json 的顶层 "api_key" 字段；
                或本机打开控制台 → 仪表盘 → 「API 接入信息」→ 点「显示」

绑非回环地址时额外给一行局域网地址；`0.0.0.0` 会被换成 `127.0.0.1`
（那个地址点不开，给了反而误导）。**管理钥匙本身刻意不打印** ——
它是权限最大的那把（不限额度、还能调 `/admin/*`），打进日志等于写进
scrollback、写进截图、写进任何一份 `start.bat > log.txt`。

### 修复 — 续期链路（两个上游，同一种"静默失效"形态）

见下方「修复 — MiniMax 没有后台续期 + TRAE 续期刷错了对象」一节。

### 新增 — ZCode 上游

Z.ai（智谱关联）的 AI coding agent，官方仓库
[`zai-org/ZCode`](https://github.com/zai-org/ZCode) 是**完整开源的
TypeScript monorepo**（Apache-2.0，7433★）。这意味着本上游的协议事实
**全部来自一手证据**，不是逆向猜测：

    官方源码   config/provider/zcode-builtin.json   端点与协议类型
               packages/shared/src/zcodeEndpoint.ts URL 构造
               packages/shared/src/zcode-source-headers.ts 来源头全集
    Go 参照    D3-vin/Zcode2Api          头集合 field-by-field 移植（含实证风控警告）
               genevatrkassulkusc82-collab/zcode-proxy  三协议转换 + 验证码求解
    我的实测   每个端点都用真实 HTTP 请求验过存在性与鉴权形态

#### 两条通道（默认只开不需要验证码的那条）

    通道       协议       验证码   协议转换   额外依赖
    api-key   OpenAI      不需要   不需要     零
    jwt       Anthropic   每请求要  需要       浏览器（可选）

**API Key 通道是本版的主路径**：`api.z.ai/api/paas/v4`（或
`/api/coding/paas/v4`，Coding Plan 的 Key 专用 —— 官方文档明示两者
**额度不互通**）。零依赖、服务器可用、粘贴即用。

#### 一个实测否证过的假设（值得记）

我一开始推断"Coding Plan 应该也有 OpenAI 形态"（那样能省掉转换层），
**实测否证**：

    /api/v1/zcode-plan/paas/v4/chat/completions   → 404   ← 想当然的路径
    /api/v1/zcode-plan/v1/chat/completions        → 404
    /api/v1/zcode-plan/anthropic/v1/messages      → 401   ← 只有这个
    /api/v1/off-peak/anthropic/v1/messages        → 401
    /api/v1/ultra-zai/anthropic/v1/messages       → 401   ← 服务端改写目标

所以 JWT 通道**必须**做协议转换（本仓第一份，见
`internal/zcode/anthropic.go`）。这个代价换的是**订阅额度可用**。

#### 抄来的两条实证规则（自己绝无可能推断出来）

**① 风控敏感：多发一个头就触发 3012**

参照实现里的逐字警告：

    JWT (start-plan) channel rules from the reference:
      - x-api-key, x-query-id, x-session-id are NOT sent — the official
        start-plan client omits them; sending x-query-id/x-session-id here
        triggers upstream 3012 "unusual activity" (empirically).

即"补齐看起来该有的头"会触发风控。本包按通道分支发头，并有测试钉住。

**② 设备指纹每账号独立**

参照实现原话：`X-Device-Mid` 是 "the account's own persisted uuid4
('a fresh install on this machine' per account), not a shared/derived id."
共用会让上游把多个账号看成同一台设备。

#### 关于验证码（诚实标注）

JWT 通道每请求要一个新的阿里云验证码参数。**官方仓库搜
`captcha`/`aliyun` 零命中** —— 说明这是纯第三方逆向的机制，
失效风险高于本包其它部分。所以：

- 求解失败**如实报错**，绝不静默去掉请求头（那会被上游当风控）
- 求解器做成接口（`CaptchaSolver`），缓存 45s + 宽限 300s + 并发去重
- 默认**不启用**（`zcode.jwt_captcha: false`）—— 它需要图形界面
- 本包用**系统浏览器开本机回环页**求解（对齐本仓其它上游的做法），
  而不是参照项目的 go-rod（那会引入浏览器自动化依赖树且要求部署机有 Chrome）

⚠ 若上游校验页面来源域，本机回环页会被拒 —— 此时错误信息会**直接
建议改用 API Key 通道**。

#### 配额与诊断端点

    GET /admin/zcode/quota?uid=…      额度（JWT 走计量端点，Key 通道如实报"没有"）
    GET /admin/zcode/diagnose?uid=…   令牌生命周期诊断（把三种 401 分开）

诊断端点回显令牌**只给长度与尾 4 位**（它的输出常被贴到 issue 里求助）。

### 新增 — MiniMax Code 上游（第十二个）

MiniMax（中国版）的 AI coding agent。**它是本仓第二个 Anthropic Messages
协议族的上游**，而且与 ZCode 不同 —— 它**只有** Anthropic 一条通道：

```
/mavis/api/v1/llm/v1/messages     Anthropic Messages（唯一形态）
```

没有 OpenAI 形态的端点可退。所以它的协议转换是**无条件**的。

#### 因此本版把协议转换层抽成了共享包

ZCode 的 JWT 通道是本仓第一个 Anthropic 消费者，当时的决定是**不做**
通用抽象（参照项目自己的判据：「不做通用 Anthropic 层抽象 —— 只有一个
消费者，抽象是凭空多一层间接；同族第二个产品出现时再抽」）。

现在第二个出现了，于是：

```
internal/zcode/anthropic.go  →  internal/anthroconv/anthropic.go
```

一处实现、两个上游共用 —— **判据不会分叉**。这也是"加一个上游，
核心零改动"这条硬判据的延伸：共享的是**协议**，不是**上游**。

#### 协议事实来自开源参照实现的源码逐行提取

`deepseek-harness-codearts` 的 `src/minimax*.ts`（约 130KB，含
`-auth` / `-credits` / `-messages` / `-oauth` / `-product` / `-adapter`）。
每条"不能凭直觉改"的点都带源码行号，逐条照搬并各配守卫。

#### 抄下来的坑（每条都有测试与变异验证）

**① 余额取 `Σ details[].remaining_amount`，不是 `total_count`**

`total_count` 是 `details[]` 的**记录条数**。它俩在「余额为 0」时
**偶然相等**（都是 0）—— 所以任何只看 0 的断言都是**同义反复**，
无法区分两个语义。参照项目为此专门写了「必须构造让两者分叉的样本」。

本版的用例就是那种样本（一条 800 的记录 ⇒ 期望 800 而非 1）。
变异验证确认它有判别力：改成 `total_count` 后报「余额 = 1，期望 800」。

**② 「还没授权」是 `HTTP 200` + `status:"pending"`（不是 OAuth 标准的 400）**

只认标准形态会**立刻抛错**，用户来不及授权；症状是
「令牌响应缺少 access_token」。**两种形态都认**。

**③ `points` 是总数，`bonus_points` 含在其中，不得相加**

实测 points 800 / bonus_points 400 时客户端显示 **800**。
相加（1200）会让展示金额虚高一倍。

**④ 上下文窗口取档位表最大值，不是 `limit.context`**

M3.1 的 `limit.context` 是 512000，而 `context_window_options` 是
`[512000, 1000000]`。填 512K 会让客户端**远早于官方能力**触发压缩。

**⑤ 幂等判据是 `claim_result`，不是 HTTP 状态码**

`1`=真领到 / `2`=已领过（**必须**与"领到"区分）/ 其他一律**失败**。
重复领取同样返回 200。

⚠ 这里我第一版写错了：用宽容解析（接受数字字符串），于是
`claim_result:"1"` 被当成 1 ⇒ **虚报成功**。参照项目明确要求
「字符串时一律判 failed」。是**我自己的测试**抓到的。

**⑥ `timezone_id` 是 query 参数，放错位置也返回 HTTP 200**

放请求头上会回 `1406010011 invalid timezone_id`，**仍然是 200**。
所以「200 = 成功」在这个端点上不成立 —— 必须查业务码
`base_resp.status_code`（不是 `code`）。

**⑦ `credit/details` 是平铺响应，签到两端点是信封形**

同一个上游两种形状，共用一个解包函数。

**⑧ 推理端点不需要 `anthropic-version` 头**（实测）

只带 `Authorization` + `Content-Type` + `Accept: text/event-stream`。
加未经验证的头是猜测。

**⑨ `access_token` 不是 JWT**（`mmoat_` 前缀、0 个点）

所以过期时刻**只能**来自 `expires_in`，不能像 ZCode 那样解 `exp`。

#### 登录：设备码 + PKCE，服务器可用

`POST /oauth2/device/code` → 用户在浏览器授权 → 轮询 `/oauth2/token`。
**不起本地监听端口**（与 Cline / Qoder 同型），所以服务器 / 容器部署
也能完成登录 —— 这一点比 ZCode 的 JWT 通道（要过滑块）好得多。

### 修复 — 契约测试逼出来的两个真实缺口

这两处都是"写了但没人能用"，而**单测全绿**：

1. **`AccountImportExt` 实现了但 `AdminExt` 没有** ——
   导入解析写得再对，界面上**没有任何入口**。契约测试的判定
   （"声明了能力位就必须有 AdminRoutes"）抓到了它。
2. **诊断端点返回空数组** —— `knownUIDs` 读的是 Provider 本地表，
   而真实运行时凭证在 `pool` 里，本地表是空的。于是端点返回
   `200 + {"accounts":[]}`：看起来"没有账号"，而账号好好的。
   **一个静默的空列表比报错更难查。**

### 关于"能抄则抄"的一次实践

本版是这条原则最完整的一次落地：官方源码给了协议事实，
Go 参照给了头集合与风控警告，我的实测用来**否证推断**与
**确认端点存在性**。三者的分工在文件头注释里都标了来源。

值得单独说：**参照实现不一定对**。它用 go-rod 驱动 Chrome 解验证码
（因为它面向桌面部署），而本仓的判据是对齐既有做法 —— 所以这里
选了系统浏览器 + 本机回环页，代价是需要一次人工交互，但零新依赖。

### 本版还包含（v1.7.3 之后的其余改动）

#### 批量导入支持「多个独立 JSON 文件」

用户报：*"我通常导入的时候会导入多个独立的 JSON 文件，那这个时候它就不行了。
希望能够自动组合成一个完整的大的 JSON 文件再导入"* —— 并且要求
**对所有上游生效**。

拆条逻辑早就抽到共享判据 `gateway.SplitAccountImportItems` 了，
**8 个上游全部走它**，所以这件事**只改一个文件**就覆盖了全部上游。

改造前它只认 `[{...},{...}]` 与 `{...}` 两种形状，而用户手上是 N 个
**独立文件** —— 恰好落在两者**之间**。

现在多认三种分隔形态：

```
{...}\n{...}   依次换行粘贴（最常见）
{...}{...}     无分隔拼接
{...},{...}    带逗号（从数组里复制掉方括号）
```

两种刻意的保守：

- **不做「统一先包一层 `[ ]`」** —— 那会**静默改写输入**：
  `"sk-abcdef"` / `123` / `true` 这类本身就是合法 JSON 的值套上括号也合法，
  会被当成"一个条目"交给上游。所以只有起点是 `{` 的输入才进这条路径。
- **尾部残渣必须报错**，不能"能读几条算几条"。否则
  `{好}\n{好}\n{粘坏的` 会变成"导入 2 个、第 3 个**静默消失**"。

顺带修掉 ZCode 的一份重复判据（它没走共享 helper，于是"多个独立对象"
**只在别家上游生效**），并删掉随之变成死代码的 `importJSONArray`。

#### 每个上游都补上「成功」列

用户要求「每一个上游都需要有成功列」。改造前 `success_count` 的
`omitempty` 让它在为 0 时**整个字段消失**，界面于是显示空白而不是 `0` ——
而"0 次成功"是个有意义的状态。加了跨上游不变量断言
（`TestEveryUpstreamShowsSuccessColumn`）守住它，新加上游也跑不掉。

#### 修「20715653 天」（用户实测报的）

用户看到 MiniMax 的 token 过期时间显示**两千多万天**，问
*"这个 token 过期时间这么长没问题吗"*。

真相是**单位错了**：`gateway.CredentialExpiryExt` 的契约是
**Unix 秒**（字段名也是 `TokenExpireSec`），而 **ZCode 与 MiniMax 都回了
毫秒**（两个都是我写的，ZCode 的注释里甚至写着"本仓的约定是毫秒"——
那句错注释就是这个 bug 的源头）。

```
毫秒值 1.79e12 被当成秒 → 剩余 ≈ 1.79e12 秒 ÷ 86400 ≈ 2070 万天
（真实剩余：约 35 分钟）
```

**为什么它读起来不像 bug**：1000 倍偏大的时间戳看起来像
"这个上游的 token 有效期特别长"，而不是像错误 —— 所以它不会引发排查，
只会引发疑问。

⚠ 最难堪的一点：**我自己的测试曾经断言毫秒**
（`TestTokenExpiryIsMilliseconds`，断言 `got == sec*1000`），
于是实现回毫秒时测试是**绿的** —— 测试把 bug 固化成了"正确"。

修了三处：ZCode、MiniMax，以及**核心的量级哨兵**：

```
判据  1e11 秒（公元 5138 年）：真实凭证不可能这么久，而毫秒恒 >= 1e12
处置  **按未知处理**（显示 —）+ 一行指明上游的日志
```

刻意**不自动除以 1000**：那会掩盖上游的 bug（它的实现仍然是错的），
而且无法区分"写错了单位"与"上游真的给了远期时刻"。
**显示 `—` 比显示 1000 倍错的数字好得多：前者用户会去查，后者用户会信。**

各包一条测试只能守住已知的两个上游；核心哨兵能看到**全部**上游，
包括将来才加的。

#### 成长守卫轮：先补前置、再接单（WorkBuddy）

用户报：*"先进行领取一只 Buddy，然后接单所有任务，这个设置成循环任务，
只要有一个账号有单可接就执行"*。

原实现遇到「没有 Buddy」时只会把 `blocked_by: first_buddy` 如实报出来，
提示用户去客户端手动点 —— 而**那个前置是可自动完成的**。
现在守卫轮检测到阻塞后调 `healGrowthPrerequisite` 自动补前置并重试一次。
同时改掉一条**假文案**（原来写"需在 WorkBuddy 客户端完成"，
而那件事我们其实能代做）。

#### ZCode：去掉多余的「额度探测」标签页 + 去掉「测试中」提醒

前者是前端为"非 hidden 的 GET 路由"自动生成的，而余额已经在账号池的
「额度」列 → 改成 `Hidden: true`。
后者按用户要求先加、后又要求去掉 —— **删干净**（连方法、编译期断言一起），
并留一条**反向**断言（`TestNoticeIsDeliberatelyAbsent`）防住"有人以为这里漏了"。

### 修复 — MiniMax 没有后台续期 + TRAE 续期刷错了对象

两个都是**用户报障**，而且都属于同一类最隐蔽的缺陷：**链路断掉时不报任何错**。

#### ① MiniMax：完全没有后台续期任务

用户报：*"MiniMax 的账号没有自动续期"*。

根因不是"续期写错了"，而是**没人会去续期**。核心的凭证续期只有两条触发路径：

    ① 出站请求时   handler 的 needsRefreshVia 判"该刷了"才刷
    ② 后台定时任务 上游通过 gateway.JobExt 自报的 Jobs()

MiniMax **满足** `CredentialRefresher`（`refresh.go` 的 `RefreshCredential`
签名正确、原地写回、也落了盘）、也**满足** `RefreshSkewExt`（正确报了
"已用寿命 ≥50% 该刷"）—— 但**没有 `Jobs()`**。于是它只有"有人拿 MiniMax 号
发对话请求"时才会顺手续期；没人用时 token 静静过期，界面上「Token」列
一直显示「已过期」，而 `refresh_token` 明明还能用。

修法：照 `internal/raccoon/jobs.go`（同一个坑的第二次踩）补
`internal/minimax/jobs.go`。判据**复用 `RefreshSkew`**，与出站路径同源 ——
另写一个判据的后果是要么后台永远不刷（等于没加），要么刷得比出站还勤。

> **为什么这个上游特别容易漏**：其余上游都有开源参照实现可以逐行照抄，
> 而参照项目的 `src/` 里**没有 minimax** —— 它是照官方桌面端 asar 与实测
> 推出来的自研实现。没有参照时，"照着别人的结构补齐一个 Jobs()"这件事
> 就没有样本可抄。

#### ② TRAE：后台续期把新 token 写到了**另一个对象**上

用户点名要查 TRAE，查下去发现缺陷方向相反但同样是"静默"的：

    runRefresh 走 localAccounts() = LoadDir(authDir)
    → LoadDir 用 Parse(raw) **每次新建对象**
    → 而池子持有的是启动时 syncTraeAccounts 放进 secrets[uid] 的**另一个** *Auth

实测证据（不是推理）：

    上游交换次数=1
    池里那份 AccessToken = "old-access"   ← 没更新
    磁盘那份 AccessToken = "new-access"   ← 更新了

也就是说**续期成功了**，只是写到了磁盘副本上。而界面读的是池
（`TokenExpiry` 拿到的 `cred.Secret` 就是池里那个指针）⇒
**落盘已续期、界面仍显示「已过期」**。

对 TRAE 尤其致命：它的 `RefreshSkew` 返回 **(0, true)** —— 显式声明
"请求路径不预检续期"（理由见 `extensions.go`：refreshToken 是消费型的，
预检会让每个请求都轮换一次）。所以**后台是唯一的续期路径**：
一旦它刷错对象，那个账号的界面状态就**永久**停在过期上，直到进程重启。

修法照参照项目的 `resolve` → `set` 语义（保证读写同一份）：
`localAccounts()` 改为**磁盘负责"有哪些 uid"、池子负责"那个 uid 的对象"**，
未接线时回落磁盘（旧行为）。

> **TRAE 是 8 个能续期的上游里唯一漏掉这条接线的**（codearts 用
> `SetAccounts`、workbuddy 用 `Pool.AuthByUID`、其余用 `SetCredentialSource`）。
> 原因是它写得比 raccoon / minimax 早（那时还没有这个约定），
> 后台续期是后来补的 —— 补的时候只接了"失败通知"（`OnRefreshFailure`），
> 漏了"活对象"。

#### 守卫（重点：这类缺陷必须靠**成对不变量**发现）

上一版为 MiniMax 的 refresher 缺口加了
`refresh_pairing_test.go`（`RefreshSkew 回正数 ⇒ 必须实现 CredentialRefresher`），
但它**没拦住这次的两个**：

    MiniMax  两个扩展点都有，缺的是**第三件东西**（后台任务）⇒ 那条守卫天然为真
    TRAE     两个扩展点都有、后台任务也有，缺的是**接线** ⇒ 上游包内测不到

补的守卫（**全部经过变异验证，8/8 条变异都会变红**）：

| 守卫 | 抓什么 |
|---|---|
| `cmd/server/jobs_pairing_test.go` | 能续期 ⇒ 必须有后台任务（补上"另一半"） |
| `internal/minimax/jobs_test.go` | 8 条行为断言：新鲜的不刷 / 到期的必刷 / **必须写回池里那个对象** / 刷不了的不空转 |
| `internal/trae/refresh_pool_object_test.go` | 上游包内：接线后落池、未接线时如实分叉 |
| `cmd/server/traecreds_test.go` | **装配层**：用真实池子跑真正的后台任务（"忘了接线"只可能在这里暴露） |

两条经验写进了注释：

- **"具备能力"与"会去做"是两件事** —— 前者是方法签名，后者是**注册了后台任务**
  并**接上了活对象**。缺任一条都不报错，只是悄悄失效。
  这已是同一形态的**第三次**（cline「Token 已过期，怎么不会自动刷新」→
  raccoon「这个 token 续期逻辑有 bug」→ 本轮 MiniMax / TRAE）。
- **变异验证会逼出"等价变异"**：我把 MiniMax 的"刷不了就不刷"过滤去掉后，
  测试仍然绿 —— 一度以为"测试判别力不够"，查下去才发现真差异只在**日志量**
  （`RefreshCredential` 对无 refresh_token 的凭证提前返回，本就不发请求；
  禁用计数也只走请求路径）。于是换成了有判别力的判据（失败日志条数），
  并**改正了我原先在注释里写错的两处成本**（原文声称"反复打上游""累计禁用账号"，
  两条查证都不成立）。写准理由很重要：被夸大的注释会让人以为它拦住了大问题。

#### 顺带修的既有缺陷

`internal/pool` 的 `TestCandidateRankerKeepsAllWithinSameTier` 是 **flaky**
（10 次约 1 次红，失败值正是 `u3:199`，差 1 就过；已确认在改动前就存在）。
实测 2000 组样本量出真实分布（`mean=213  P(u3<200)=3.5%`）。

> ⚠ 该文件在我改完之后、提交之前，**上游 `b3b0af4` 已经用另一种方式修了它**
> —— 那版把"额度"整个移出档内选号权重（语义变更），断言也从"高额度应占多数"
> 改成了"三者都要被用到且无绝对多数"。合并时我**丢弃了自己那版**（它基于旧语义），
> 采用上游的。

### 修复 — loomy「赠送积分用完了还是会被调用」

用户报障原文：*「loomy 如果赠送积分没有了还是会调用，也就是优先免费额度的功能没起作用」*。

这是同一块逻辑的**第三次**报障（前两次见 `internal/loomy/candidate_tier.go` 文件头），
但洞与前两次不同：前两次修的是**新鲜度**，这次修的是**两条把"已知"降级成"未知"的路径**。

#### ① 把「没读到 dailyBalance」读成了「确认只剩永久」

`internal/loomy/points.go` 里的 `Points.DailyKnown` 就是为这条存在的，它自己的注释写着：

> 只回 int64 的话"字段缺失"会被读成 0 —— 界面显示「免费额度已用完」，而真相是
> "这次没读到" …… 这是**把未知说成确定的零**。

而档位刷新走的却是 `Balance` —— `PointsSnapshot` 的**薄封装**，恰好把这面旗帜丢掉了：

    return pts.Available, pts.Daily, nil     // ← DailyKnown 丢了

后果：`dailyBalance` 没读到时 `daily = 0` ⇒ `tierOf` 判 **tierPaid（确认只剩永久）**
⇒ 这个号与**真正**用完免费额度的号**同档** ⇒ 硬分档（`pool.pickForTier` 只留
最优那一档）选不出任何差别 ⇒ 一个**还有免费额度**的号被与"该烧永久"的号一起抽中。

修法：档位刷新改用 `PointsSnapshot`，新增 `tierOfPoints` 按三态判档
（`DailyKnown == false` ⇒ `tierUnsure`，**不是** `tierPaid`）。

#### ② 把「已确认只剩永久」因为缓存过期/刚用过而降级成「不确定」

档位顺序是 `tierFree(0) < tierUnsure(1) < tierPaid(2)`，而 `tierUnsure` **排在
`tierPaid` 前面**。于是"降级成不确定"= 让一个已知没有免费额度的号**变得更优先**。

两条降级路径都有这个问题：

- `CandidateTiers` 过 TTL ⇒ 报 `tierUnsure`
- `invalidateTier`（`Chat` 返回后）⇒ 无条件 `drop`

修法：**「只剩永久」在同一个自然日内是稳定结论** —— 赠送额度只按自然日
重置，所以它今天不会变回"有"。于是：

- 同一天的 `tierPaid` 过 TTL 后**仍报 `tierPaid`**，只按 `tierPaidRecheck`（30 分钟）
  复查一次（万一当初读错了，当天要能自愈，而不是一错一整天）
- `invalidateTier` 对同一天的 `tierPaid` **不标脏**（用它只消耗赠送额度）
- 自然日一变（新增 `naturalDay` 与 `tierEntry.day`）立刻重新评估

⚠ 反向也守住了：**用完一个 `tierFree` 的号必须标脏**（它可能刚刚变成 paid）。
两个方向都有守卫，否则只修好一半。

#### 守卫（新增 `candidate_tier_tristate_test.go`，6 条，全部变异验证过）

| 用例 | 抓什么 |
|---|---|
| `TestTierOfPointsDailyUnknownIsNotPaid` | dailyBalance 缺席 ⇒ 必须 `tierUnsure` |
| `TestTierOfPointsDailyKnownZeroIsPaid` | 确实是 0 ⇒ 才该 `tierPaid`（反向） |
| `TestPaidTierSurvivesTTLWithinSameDay` | 同一天内过期**不降级** |
| `TestPaidTierReevaluatedAfterDayRollover` | 跨自然日**必须重评**（反向） |
| `TestInvalidateKeepsPaidTierSameDay` | 用过不标脏 |
| `TestInvalidateDropsFreeTier` | 用完 `tierFree` 必须标脏（反向） |

变异验证 3/3 全部变红：不看 `DailyKnown` / 过期即降级 / 无条件标脏。

### 已知限制

- **JWT 通道未在真账号上端到端跑通**（需要有 z.ai 订阅 + 过滑块）。
  API Key 通道的形状与错误路径都已实测（假 Key → 如实报 401）。
- **验证码求解未在真上游验证过**（同上）。
- 官方 `official-coding-plan-gateway.ts` 揭示 Coding Plan 的请求会被
  服务端**动态改写**到 `/api/v1/ultra[-zai]/...` —— 所以 base URL
  做成了可配置覆盖（`zcode.origin`），硬编码有失效风险。
- 不做生图、没有每日签到（Z.ai 没有签到概念，额度由订阅周期决定）。
- JWT **不可刷新**：官方明确"暂未提供 refresh token 交换接口"，
  过期只能重新登录。
- **MiniMax 未在真实上游做过对话验证**（本机没有任何 MiniMax 凭证）。
  OAuth 握手、目录 / 积分 / 签到的解析、SSE 转换、错误分类都已用
  hermetic 假上游覆盖；**推理端点一次都没打过真上游**。
  上游分组旁的提醒就是这么写的。

---
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