# 企业级差距评审：前端 / 后端 / 工程化（对标成熟开源网关）

> 日期：2026-11-14 · 性质：**只读评审 + 建议**，本次未改任何代码、未跑测试、未启动实例。
> 基线：`HEAD = ac56f8d`，工作区干净（`git status --short` 为空）。
> 姊妹文件：`docs/enterprise-readiness-plan.md`（偏**后端可靠性/安全**的分阶段计划，任务号 A-xx…E-xx）。
> 本报告**不重复**那份计划，凡已覆盖的条目只给引用；新增内容集中在**控制台前端、管理 API 形态、可观测性、仓库卫生**四块。

---

## 0. 一页结论

**后端的骨架已经是企业级水准，前端还停在"个人工具"阶段。** 用户"前端不太行"的直觉是对的，而且问题主要在架构，不在配色。

- **后端强项**（不用推倒，继续守住）：Provider 接缝 + 37 个扩展点 + `arch_test.go` 包级依赖守卫；错误分类体系（`ErrorKind`）比 new-api 更细；有审计（`admin.dispatch` → `store.AuditLog`）、HttpOnly + SameSite 会话、定长比较的管理令牌、`MaxBytesReader` 限流体；Go 测试 662 个文件（约 14.3 万行）。
- **前端主要短板**：一个 14,012 行 / 830 KB 的 `app.js` 主台，加上 6 个**各自独立**的 120–135 KB HTML 页，靠 `<iframe>` 拼在一起；页面**没有路由**（刷新会丢当前页，也没法分享链接或用后退键）；渲染全靠拼字符串后塞 `innerHTML`，转义只能靠人记得写；还有 4 个手写的"同步脚本"在充当构建系统。
- **管理 API 的短板**：83 条 `/admin/*` 路由全部是"POST + 动词路径"风格，没有版本号；错误格式至少 3 种，分页参数 3 套；`openapi.yaml`（89 条 path）**没有任何测试**跟真实路由对账。
- **可观测性短板**：没有 `/metrics`，没有请求 ID，代码里 529 处 `log.Printf` 对 65 处结构化 `logx`；`/healthz` 把存活检查和就绪检查混成一个。

**建议的总方向**：后端做"**补齐**"（可观测性、API 规范、安全头），前端做"**绞杀式重建**"（strangler）——新控制台用正经的构建链和组件库开发，最后仍用 `go:embed` 打进二进制，**保留"Release 下载即用"**；老 `/ui` 在新控制台功能对齐前继续可用。

**Top 10 动作**（按投入产出比排序，详见 §4–§6）：

| # | 动作 | 面 | 量级 | 对应 |
|---|---|---|---|---|
| 1 | 控制台加 hash 路由（刷新保留页面、能深链、能后退） | 前端 | S | FE-R1 |
| 2 | 静态资源 gzip/br 压缩 + 安全响应头（CSP / frame-ancestors / Referrer-Policy） | 后端 | S | BE-S1 |
| 3 | `/livez` 与 `/readyz` 拆开；加 `X-Request-Id` 贯穿日志 | 后端 | S | BE-O1 |
| 4 | `/metrics`（Prometheus 文本格式，标准库手写，不加依赖） | 后端 | M | BE-O2 |
| 5 | 管理 API 统一错误信封 + 分页参数；openapi ↔ 路由对账测试 | 后端 | M | BE-A1/A2 |
| 6 | 仓库瘦身：12 个 `_backup-*`、根目录 exe、145 MB 测试残留浏览器 profile | 工程 | S | ENG-1 |
| 7 | 定下前端技术栈（建议 Vite + React + TS + antd），搭好 `web/` → `go:embed` 管线 | 前端 | M | FE-N0 |
| 8 | 新控制台先做"账号池 + 请求日志"两页（使用频率最高） | 前端 | L | FE-N1 |
| 9 | 角色模型从"单管理员"扩为 viewer / operator / admin（**需要先拍板**） | 全栈 | L | Q-01 / C-02 |
| 10 | 管理 API 迁到 `/api/admin/v1`，资源化（老路径保留做兼容别名） | 后端 | L | BE-A3 |

---

## 1. 范围与方法

- 只读审阅了：`AGENTS.md`、`README.md`、`docs/enterprise-readiness-plan.md`、`docs/newapi-parity.md`、`docs/移植-前端.md`、`docs/reports/frontend-duplication.md`、`docs/known-issues.md`，以及 `internal/server`、`internal/admin`、`internal/account`、`cmd/server` 的入口与鉴权代码；另外看了 `assets/shots/` 里的两张控制台截图。
- 下文数字都是 2026-11-14 在 `ac56f8d` 上用 PowerShell 或 Grep 实测的计数，复验命令见附录 A。**计数只说明形态，不代表缺陷已经复现。**
- 对标项目的信息来自它们的公开仓库和公开报道。外部项目更新很快，选型前请以对方仓库的当前状态为准。
- **没做的事**：没跑 `go test`、前端测试套件或 e2e；没读凭证、数据库、日志；没调用任何上游。

---

## 2. 现状画像（实测）

### 2.1 规模

| 项 | 数值 |
|---|---|
| Go 非测试代码 | 约 154,600 行 · 539 个文件（1,201 − 662） |
| Go 测试代码 | 约 143,300 行 · 662 个文件 |
| `internal/` 包 | 49 个（其中上游约 15 个） |
| 超过 2,000 行的 Go 文件 | `admin/admin.go` 3,980 · `server/handler.go` 3,975 · `pool/pool.go` 2,863 · `admin/channels.go` 2,594 · `cmd/server/main.go` 2,502 · `cmd/server/config.go` 2,206 |
| 直接依赖 | go-redis、wazero、gorm + glebarez/sqlite、x/crypto（**注意**：`AGENTS.md`「依赖铁律」写的是"只有 go-redis + wazero"，跟 `go.mod` 已经对不上） |

### 2.2 前端

| 资源 | 体积 | 说明 |
|---|---|---|
| `internal/server/ui/app.js` | 830 KB · 14,012 行 · 367 个函数 | 主台全部逻辑 |
| `internal/server/ui/app.css` | 150 KB | 主台样式 |
| `webui.html` | 118 KB | 主台外壳：16 个 `<section data-key>` + 193 处内联 `style=` |
| `webui-{channels,groups,status,models,tokens}.html` | 每个 119–134 KB | **独立页面**，通过 `<iframe>` 嵌进主台 |
| `webui-login.html` | 80 KB | 登录页 |
| `theme-max.css` | 54 KB | "唯一真相"，由 `tools/theme_sync.js` **内联复制**进 6 个页面 |
| 同步脚本 | `theme_sync` / `toast_sync` / `model_picker_sync` / `dash_chart_sync` | 实际上是一套手写的"构建系统" |

| 指标 | app.js | 6 个独立页合计 |
|---|---|---|
| `innerHTML` 赋值 | 154 | 79 |
| `esc(` 调用（手工转义） | 313 | — |
| `localStorage` 使用 | 64 | — |
| `location.hash` / `history.pushState` | **0** | — |

### 2.3 后端管理面

| 指标 | 数值 |
|---|---|
| `/admin/*` 路由 | 83 条，**全部** `METHOD /path` 写法（Go 1.22 mux）；channels 32 · accounts 10 · apikeys/groups/tokens 各 8 |
| 动词式路径 | `/delete` `/update` `/enable` `/disable` `/reload` `/rotate-*` 等至少 17 条 |
| `writeJSON(w, …, map[string]any{…})` | 238 处（响应结构靠临时拼 map，没有类型约束） |
| 分页参数 | channels：`page`/`page_size`（兼容 `ps`/`size`）；tokens：`page_size`；logs：`limit`/`offset`；另一处是 `cursor` |
| 错误格式 | `/v1/*` 用 OpenAI 信封；`admin.writeError`；`account.writeError(code,msg)`；`consoleAdminAuth` 用 `{error, code}` |
| `/metrics` · 请求 ID · OpenTelemetry | 无 · 无 · 无 |
| 安全头 | 只有 `account/http.go:137` 设了 `X-Content-Type-Options`；**没有** CSP、`X-Frame-Options` / `frame-ancestors`、`Referrer-Policy` |
| 响应压缩 | 无（830 KB 的 app.js 原样传输，只有 ETag + `max-age=300`） |
| CSRF | 只靠 `SameSite=Lax`；没有 Origin / `Sec-Fetch-Site` 校验 |
| 管理令牌来源 | Bearer → **`?token=`** → cookie（`account.TokenFromRequest`）；`?token=` 已列入计划 C-03 |

---

## 3. 对标：成熟开源网关是怎么做的

| 维度 | new-api（QuantumNous） | one-api | LiteLLM Proxy | **本项目** |
|---|---|---|---|---|
| 后端 | Go + Gin | Go + Gin | Python + FastAPI | Go 标准库 mux |
| 控制台 | 独立 `web/` 工程：React + Vite + Semi Design；在 classic / default 两套前端之间迁移[^1] | React，多套主题可选 | 独立 Next.js 管理 UI | **无构建**：go:embed 进 7 个 HTML + iframe |
| 路由 / 深链 | SPA 路由，URL 对应页面 | SPA 路由 | SPA 路由 | **无** |
| 角色 | guest / user / admin / root 四档（本仓 `RoleAdmin=10` 就是照它抄的） | 同源设计 | proxy_admin / internal_user / team 等，按团队隔离 | **单管理员**（成员角色已在 2026-10-19 删除） |
| 下游密钥 | 令牌：额度、模型白名单、分组、过期时间 | 同上 | 虚拟密钥：预算、限速、模型白名单、可访问路由 | API Key：额度、RPM、模型（v2.0.2 起简化） |
| 可观测性 | 请求日志 + 数据看板 | 请求日志 | Prometheus 指标 / 回调 / 日志集成（部分功能属企业版） | 请求日志 + sysmon，**无指标端点** |
| 存储 | SQLite / MySQL / PostgreSQL + Redis | 同上 | PostgreSQL + Redis | JSON 文件 + SQLite（gorm）+ 可选 Redis |
| 上游扩展 | adaptor 目录，核心层对上游有感知 | adaptor 目录 | provider 模块 | **接缝 + 扩展点 + 架构守卫测试（更严格，这是我们的强项）** |

**值得学的**：

1. **控制台是独立工程**（new-api / LiteLLM 都是这样）。前端有自己的依赖管理、类型检查、组件库和路由，最终产物再交给后端托管。本项目坚持"零构建"的收益是 Release 不依赖 Node，但代价已经显现：14k 行单文件、4 个同步脚本、主题 CSS 复制 6 份、iframe 拼页面。**用 `go:embed` 嵌入构建产物，就能同时保住"下载即用"和工程化。**
2. **角色 + 资源范围**：new-api 的四档角色是最小可用的 RBAC。企业内部至少需要"只读审计员 / 运维操作员 / 管理员"三种身份。
3. **教训也要学**：LiteLLM 在 2026 年披露过一串高危漏洞——虚拟密钥的 `allowed_routes` 没校验调用者权限（CVE-2026-47101）、`/user/update` 缺字段级权限导致角色提升（CVE-2026-47102）、测试连接端点可执行命令（CVE-2026-42271）[^2]。启示是：**一旦引入多角色，"谁能改角色 / 谁能发钥匙 / 测试端点能访问什么"必须是服务端默认拒绝的白名单，不能只在前端隐藏按钮。** 本仓当前"单管理员 + 删掉 SetRole"的做法回避了这类风险；以后扩角色时，这条要作为设计约束写死。

**不该学的**：new-api 的充值、钱包、计费表达式、多租户注册属于对外 SaaS 形态，跟"单组织内部网关"的定位（计划 Q-01）不符，`docs/newapi-parity.md` 已有结论，不再展开。

---

## 4. 前端：问题与建议

### 4.1 架构层问题（"不好用"的根源）

| ID | 问题 | 证据 | 对用户的影响 |
|---|---|---|---|
| F-A1 | **没有路由** | `location.hash` / `pushState` 计数为 0 | 刷新回到仪表盘；没法把"某渠道的日志"链接发给同事；浏览器后退键不可用 |
| F-A2 | **iframe 拼页面** | 状态 / 渠道 / 分组 / 令牌 / 模型 5 页都是 iframe（`webui.html:872/917/938/981/1302`） | 双滚动条；主题、登录态、toast 在两层之间各管各的；每切一次页重新加载 120 KB+；iframe 里的页面不知道外壳的导航状态 |
| F-A3 | **单文件巨石** | app.js 14k 行、367 个函数、4 个顶层可变状态 | 改一处牵动全局；多个 agent 并行改同一个文件时冲突频繁（`AGENTS.md` 里专门写了"中间态"警告） |
| F-A4 | **字符串拼 HTML** | `innerHTML` 233 处，靠 313 处 `esc()` 兜底 | XSS 防护**全凭约定**：漏写一个 `esc` 就会出事，而且又没有 CSP 兜底（见 BE-S1） |
| F-A5 | **手写构建系统** | 4 个 `*_sync.js` 往 HTML 里内联共享代码 | 漏跑同步就会出现"页面跟源头不一致"，只能靠 `--check` 门禁事后发现；新人很难理解 |
| F-A6 | **样式散落** | 主台 193 处内联 `style=`；`theme-max.css` 被复制成 7 份 | 视觉无法统一调整；暗色主题要逐处补 |

### 4.2 体验层问题（结合截图 `assets/shots/dash.png`、`accounts.png`）

| ID | 问题 | 成熟控制台的做法 |
|---|---|---|
| F-U1 | 仪表盘的"调用统计"是全量历史合计，**没有时间范围选择、没有趋势图放在首屏**；模型明细是一大片标签墙（30+ 个 chip） | 顶部选时间范围（1h / 24h / 7d / 自定义）；首屏放 QPS、成功率、P50/P95 延迟、token 消耗的折线图；模型用量用 Top-N 柱图加"其余"折叠 |
| F-U2 | 仪表盘里的"API 接入信息"写着 `/ui` 和 `/admin/*` 的鉴权是"**仅本机**"——跟 `admin.go:1231` 注释"本机免令牌这条路径已经不存在了"**相互矛盾** | 文案由后端 manifest 下发，避免前后端两份真相 |
| F-U3 | 账号池每行 5 个平铺按钮（额度 / 签到 / 保活 / 禁用 / 移除），"移除"这类危险操作和普通操作并列 | 行内只放 1–2 个主操作，其余收进"更多"菜单；危险操作二次确认并说明后果；支持**多选批量操作** |
| F-U4 | 账号池没有搜索、状态筛选、排序；各上游的列不一致（`AccountColumnsExt`），分组之间横向无法对齐 | 统一的 ProTable：搜索 + 筛选 + 列设置 + 分页 + 筛选条件同步到 URL |
| F-U5 | 加载、空数据、出错三种状态的表现各页自己写 | 统一的 Skeleton / Empty / Result 组件（`docs/移植-前端.md` F3/F4 已列为可移植项） |
| F-U6 | 顶栏信息密集（账号健康 / 账号 / 上游 / 加载时间 / 三个主题按钮） | 顶栏只留全局搜索、环境标识、用户菜单；主题切换收进用户菜单 |
| F-U7 | "MAXIMALISM / 外壳吵、数据静"的视觉口径 | 企业控制台的主流是"安静的中性色 + 一种品牌色 + 状态色"，信息层级靠排版而不是色块。这一条是**审美取向**，建议用户拍板（Q-FE2） |

### 4.3 路线选择

| 方案 | 做法 | 优点 | 代价 |
|---|---|---|---|
| **A. 原地渐进**（不引入构建） | 加 hash 路由；把 5 个 iframe 页并入主台 section；写一个自动转义的 `html` 标签模板替换 `innerHTML`；加 CSP | 不改技术栈，风险小，第一步就能见效 | 天花板低：14k 行的单文件只会继续变大；组件、类型、表格能力都得自己造 |
| **B. 新控制台 + 绞杀式迁移（推荐）** | 新建 `web/`：Vite + React + TypeScript + antd（含 ProLayout / ProTable）；`vite build` 输出到 `internal/server/webdist/`，由 `go:embed` 托管在 `/console`；按页迁移，功能对齐后再把 `/ui` 重定向过去 | 获得组件库（表格、表单、抽屉、日期范围、虚拟滚动）、类型检查、路由、i18n 能力；React 默认转义，XSS 面大幅缩小；和 new-api / LiteLLM 同一个物种，招人和交接都容易 | CI 和发版需要 Node；dist 要么入库，要么在 CI 构建（见下）；迁移期新老两套并存 |
| C. 一次性重写 | 停掉老 UI，整体重做 | — | 老 UI 有 163 个前端测试脚本守着的行为，一次性重写**必然**丢行为。**不建议** |

**推荐 B，并且先做 A 里的两件小事**（hash 路由、CSP）：它们跟 B 不冲突，而且立刻就能改善体验和安全。

B 方案的关键约束（落地前要写进 ADR，`docs/decisions/0003-*`）：

1. **"下载即用"不变**：Node 只在构建期出现，运行期仍是单个二进制。
2. **干净签出必须能 `go build`**：二选一——(a) `webdist/` 产物入库，CI 校验"产物与源码一致"；(b) 产物不入库，`go:embed` 一个占位页，Release 流水线先 `npm ci && npm run build` 再 `go build`。**建议 (b)**，再加一条 CI 门禁：Release 二进制里必须嵌有真实产物。
3. **数据层**：TanStack Query（或 antd 自带的 request）+ 一层从 `openapi.yaml` 生成的类型化客户端——这又要求先完成 BE-A2（openapi 与路由对账），否则生成出来的类型不可信。
4. **能力位驱动 UI 的机制保留**：新控制台同样读 `/admin/ui/manifest`，按 `Capability` 和扩展点显示或隐藏功能。这是本项目区别于 new-api 的核心设计，不能在重写中丢掉。
5. **迁移顺序**：账号池 → 请求日志 → 渠道 → 令牌 / 分组 → 仪表盘 → 设置 → 上游专属页 → 对话测试。每迁完一页，老页面上就加一个"去新版"的入口；老测试脚本在对应页面下线前保持绿。

> 与 `docs/enterprise-readiness-plan.md` §13 的关系：那一轮写的"不使用 antd、不换框架、不加依赖"是**那一轮修复的范围约束**，不是长期决策。是否采用 B 方案需要用户明确拍板（Q-FE1）。

### 4.4 前端条目清单

| ID | 条目 | 优先级 | 量级 | 方案 |
|---|---|---|---|---|
| FE-R1 | 主台 hash 路由：`#/accounts`、`#/logs?provider=x`；刷新保留，后退可用 | P0 | S | A/B 通用 |
| FE-R2 | CSP 落地前的准备：消掉内联 `onclick`（groups 页 12 处、主台 5 处）和内联 `<script>`，否则只能写 `unsafe-inline` | P1 | M | A |
| FE-R3 | 自动转义的 `html` 标签模板，从新写的代码开始替换 `innerHTML` | P1 | M | A |
| FE-N0 | `web/` 脚手架 + `go:embed` 管线 + CI 构建 + ADR | P0（选定 B 后） | M | B |
| FE-N1 | 新控制台第一批：布局（侧栏、面包屑、用户菜单）、账号池（ProTable + 批量操作）、请求日志（时间范围、筛选同步 URL、详情抽屉） | P0 | L | B |
| FE-N2 | 仪表盘改版：时间范围 + 趋势图 + Top-N | P1 | M | B |
| FE-N3 | 统一的加载 / 空 / 错误三态，危险操作的确认规范（对齐 `docs/protocols/前端操作反馈协议.md`） | P1 | S | B |
| FE-N4 | 全局搜索 / 命令面板（按账号 UID、模型、渠道名跳转） | P2 | M | B |
| FE-N5 | 角色感知 UI（依赖 Q-01 / C-02）：按钮按权限显示或隐藏，**服务端仍要做最终校验** | P2 | M | B |

---

## 5. 后端：问题与建议

> 可靠性（关停、背压、取消、持久化）和安全盘点见计划 B-01…B-04、C-01…C-06，这里**不重复**。

### 5.1 管理 API 规范化

| ID | 问题 | 建议 | 优先级 / 量级 |
|---|---|---|---|
| BE-A1 | 错误格式至少 4 种（见 §2.3） | 管理面统一为 `{"error":{"code":"…","message":"…","request_id":"…"}}`。`/v1/*` 保持 OpenAI 信封不动（那是对外兼容契约）。`code` 做成枚举常量，前端按 `code` 分支，不靠匹配文案 | P1 / M |
| BE-A2 | `openapi.yaml` 有 89 条 path，但**没有测试**引用它，跟 83 条 `/admin` 路由加 `/v1` 路由的对应关系无人守护 | 加一个对账测试：从 mux 注册表枚举 `METHOD path`，跟 openapi 的 paths 双向比对，任何一边多出来都变红（写法参照 `config_provider_sections_test.go` 的反射枚举） | P1 / S |
| BE-A3 | 动词式 POST 路由，没有版本号 | 新增 `/api/admin/v1/{resource}`，用 GET / POST / PATCH / DELETE；老路径保留为别名并在响应头加 `Deprecation`；新控制台只调新路径 | P2 / L |
| BE-A4 | 分页参数 3 套 | 统一为 `page` + `page_size`（最大值有上限），响应统一 `{items,total,page,page_size}`；日志这类追加型数据用 `cursor` | P1 / S |
| BE-A5 | 238 处 `map[string]any` 响应 | 新端点一律用具名 struct（openapi 生成类型的前提）；老端点随 BE-A3 迁移时顺带改 | P2 / L |

### 5.2 可观测性

| ID | 问题 | 建议 | 优先级 / 量级 |
|---|---|---|---|
| BE-O1 | `/healthz` 在"没有可用账号"时返回 503（`handler.go:713`）。如果被 k8s 当作 liveness 探针，账号临时耗尽就会导致**进程被反复重启** | 拆成 `/livez`（进程活着就 200）和 `/readyz`（等同现在的 `ServableNow`，关停时立刻转 503，对接计划 B-01 的"撤销 readiness"步骤）；`/healthz` 保持现状做兼容 | P0 / S |
| BE-O2 | 没有指标端点 | `/metrics` 输出 Prometheus 文本格式，**用标准库手写**，不引入 client_golang，符合依赖铁律。首批指标：请求数与耗时直方图（按 provider / model / status_class）、TTFB、在途请求数、账号池各状态数量、上游熔断状态、重试次数、SSE 中断数、日志队列丢弃数。标签只用低基数维度，**禁止**把 UID 或原始 URL 放进标签 | P1 / M |
| BE-O3 | 没有请求 ID | 入口生成或透传 `X-Request-Id`，放进 context；`logbuf.Entry`、审计、错误信封、上游调用日志都带上它。控制台的日志详情可以按 request_id 串起一次请求的完整链路（包括换号重试） | P0 / S |
| BE-O4 | 529 处 `log.Printf`，65 处 `logx` | 统一到 `logx`（它已经有 level），再加 JSON 输出模式和固定字段（ts、level、msg、request_id、provider）；按包逐步替换，每次 PR 只动一个包 | P2 / M |
| BE-O5 | 没有链路追踪 | 有明确需求再做（会引入 OTel 依赖，跟依赖铁律冲突），先用 request_id 覆盖八成需求 | P3 |

### 5.3 安全加固（计划 C 阶段之外的补充）

| ID | 条目 | 说明 |
|---|---|---|
| BE-S1 | 安全响应头中间件 | 所有 `/ui*`、`/console*`、`/admin*` 响应加 `Content-Security-Policy`（先用 Report-Only 观察一周）、`frame-ancestors 'self'`（iframe 都是同源，不受影响）、`Referrer-Policy: same-origin`、`X-Content-Type-Options: nosniff`；走 HTTPS 时加 HSTS（受 `Secure` 策略开关控制） |
| BE-S2 | 管理面变更请求校验 Origin / `Sec-Fetch-Site` | 作为 `SameSite=Lax` 之外的第二道 CSRF 防线：不同源的非 GET 请求一律 403。Bearer 调用（脚本）不带 Origin，可以放行 |
| BE-S3 | 静态资源压缩 | 构建期预压缩 br/gzip 并嵌入，按 `Accept-Encoding` 返回（830 KB 的 JS 预计能压到 150–200 KB；**估算值，未实测**） |
| BE-S4 | 角色扩展的设计约束（为 C-02 准备） | 照 §3 的 LiteLLM 教训：权限表放在服务端、默认拒绝；"发钥匙 / 改角色 / 测试连接"三类端点单独一档权限；钥匙的可用范围不能超过发放者自己的权限 |

### 5.4 代码结构（对计划 E-01 的具体化）

| 文件 | 行数 | 拆分方向（按职责，不按层） |
|---|---|---|
| `server/handler.go` | 3,975 | 鉴权中间件 / `/v1` 对话出口 / 状态与健康 / UI 托管，各成一个文件 |
| `admin/admin.go` | 3,980 | 门禁与审计 / 账号操作 / 设置 / manifest；路由注册表集中到 `routes.go`，顺便给 BE-A2 当枚举来源 |
| `cmd/server/main.go` | 2,502 | 装配按"存储 → 池 → 上游注册 → HTTP → 后台任务"分段，抽成 `wire_*.go` |
| `cmd/server/config.go` | 2,206 | 每个上游的配置解析块挪到 `config_<id>.go`（反射测试已经在守，挪动是安全的） |

另外一个观察：代码注释非常密集（大量 ⚠ 和事故复盘写在源码里）。这对 AI 协作者友好，但也让单个文件变得很长。建议把"为什么"的长篇复盘迁到 `docs/decisions/`，源码只留一行指针。这一条是可选项，不在 Top 10 里。

---

## 6. 工程化与仓库卫生

| ID | 问题 | 证据 | 建议 |
|---|---|---|---|
| ENG-1 | 根目录杂物 | 12 个 `_backup-*` 目录、`_scratch/`、`auditreport.exe` / `migrate-sql.exe` / `server.exe`、`config.json.bak-*`、根目录 4 份 `PLAN-*.md`、`dashboard_gen.js` 等一次性脚本 | 都已被 gitignore，不影响仓库本身，但影响本机检索和 agent 判断。建议移到仓库外，或统一放进 `.local/`（**删除前需要用户确认**） |
| ENG-2 | 测试残留浏览器 profile | `internal/monkeycode/.browser-profiles/` 下有 3 个 Edge profile，共 **145 MB**（测试在包目录下运行时落到了 CWD） | 测试里让 profileRoot 指向 `t.TempDir()`，再加一个断言："测试结束后包目录下不能出现 `.browser-profiles`" |
| ENG-3 | docs 里混入了大文件和嵌套仓库 | `docs/agent2api-repo.tar.gz` 32 MB；`docs/agent2api-repo/` 里还有一个完整的 `.git` | 移到仓库外的参考资料目录 |
| ENG-4 | Go 版本有三个口径 | `go.mod` 写 1.22.5，CI 用 `'1.27'`（注释还写着"与 1.22.5 对齐"），Dockerfile 用 `golang:1.23-alpine` | 归入计划 A-01：定一个唯一来源（`go.mod` 的 `toolchain` 行），CI 和 Dockerfile 都从它读 |
| ENG-5 | `AGENTS.md` 依赖铁律已过时 | 文档写"只有 go-redis + wazero"，实际还有 gorm、sqlite、x/crypto | 更新文档，或加一个 `go.mod` 直接依赖白名单测试 |
| ENG-6 | 前端测试没进 CI | `ci.yml` 只跑 Go；`tests/frontend/` 下有 163 个脚本 | 计划 A-02 已覆盖；B 方案落地后，新控制台的 `vitest` 和类型检查要直接作为 CI 门禁 |
| ENG-7 | 发布物没有 SBOM、签名和校验和 | — | 计划 D-03 已覆盖 |

---

## 7. 路线图（跟现有计划合并后的顺序）

```text
阶段 0（1–2 周，几乎零风险）
  FE-R1 hash 路由 · BE-O1 livez/readyz · BE-O3 request_id · BE-S1 安全头（CSP 先用 Report-Only）
  BE-S3 预压缩 · ENG-2 测试 profile 泄漏 · ENG-4/5 口径对齐 · BE-A2 openapi 对账测试
  ── 同时进行：用户拍板 Q-01 / Q-FE1 / Q-FE2

阶段 1（选定 B 方案后，约 3–5 周）
  FE-N0 web/ 脚手架 + embed 管线 + ADR-0003
  BE-A1 统一错误信封 · BE-A4 统一分页 · BE-O2 /metrics
  FE-N1 新控制台：布局 + 账号池 + 请求日志（/console 与老 /ui 并存）

阶段 2（对齐计划的 B / C 阶段）
  计划 B-01…B-04 可靠性 · C-01 身份盘点 → C-02 RBAC（落地时遵守 BE-S4）
  FE-N2 仪表盘 · FE-N3 三态规范 · 迁移渠道 / 令牌 / 分组 / 设置

阶段 3
  BE-A3 /api/admin/v1 资源化 · FE-N5 角色感知 UI · 老 /ui 下线
  计划 D 阶段：SLO、备份、供应链 · E-01 大文件拆分
```

每个阶段的"完成"沿用计划 §11 的 DoD：在固化快照里跑出绿色、改前失败改后通过的证据、不把局部检查当成整体验收。

---

## 8. 需要用户拍板的事项

| ID | 问题 | 本报告的建议 |
|---|---|---|
| Q-01（计划已有） | 定位是"个人自用"还是"单组织内部网关"？README 和 v2.0.2 的口径都是个人自用 | 要做"企业级"，前提是先把定位改成单组织内部网关；否则 RBAC、预算这些都没有意义 |
| Q-FE1 | 前端走方案 A 还是方案 B？能否接受 CI 和发版引入 Node 构建（运行期不变）？ | 选 B；技术栈 Vite + React + TS + antd（备选 Vue 3 + Element Plus / Arco，团队更熟哪个就选哪个） |
| Q-FE2 | 视觉方向：保留 MAXIMALISM，还是改为企业中性风？ | 新控制台用 antd 默认的中性主题加品牌色，"MAXIMALISM"只留在老 UI |
| Q-API1 | 管理 API 是否允许新增 `/api/admin/v1`（老路径保留别名、至少保留两个版本）？ | 允许 |
| Q-ENG1 | 是否同意清理或迁出 §6 列出的本地杂物（逐项确认后再动） | 同意后列清单逐项执行 |

---

## 附录 A：复验命令（PowerShell，在仓库根执行）

```powershell
# 前端体积与形态
Get-ChildItem internal\server -Recurse -Include *.html,*.css,*.js | Select Name,Length
$js = Get-Content internal\server\ui\app.js -Encoding UTF8
($js | Select-String 'innerHTML').Count; ($js | Select-String '\besc(apeHtml)?\(').Count
($js | Select-String 'location\.hash|history\.(push|replace)State|hashchange').Count   # 期望 0 → 加路由后 > 0
# 管理路由数量与形态
Get-ChildItem internal\admin\*.go | ? Name -notlike '*_test.go' |
  Select-String 'h\.register\("([^"]+)"' -AllMatches | % { $_.Matches.Groups[1].Value }
# 可观测性 / 安全头
# 用 Grep 工具搜索：Content-Security-Policy|X-Frame-Options|/metrics|X-Request-Id（internal/**.go）
# 测试残留
"{0:N1} MB" -f ((Get-ChildItem internal\monkeycode\.browser-profiles -Recurse -File | Measure Length -Sum).Sum/1MB)
```

## 附录 B：参考

[^1]: new-api 前端双轨（classic / default）、React + Semi Design 等信息见公开报道：[3天重写39万行代码（CSDN, 2026-07）](https://blog.csdn.net/yaoshuweilai/article/details/162816213)；仓库：[QuantumNous/new-api](https://github.com/QuantumNous)。
[^2]: LiteLLM 漏洞链：[从低权限 Key 到 AI Gateway 接管：LiteLLM 漏洞链完整剖析（腾讯云开发者, 2026-06）](https://cloud.tencent.com/developer/article/2696177)；[CVE-2026-47101 预警（中国矿业大学）](https://xhnic.cumt.edu.cn/95/4a/c20548a693578/page.htm)。

本仓库内相关文档：`docs/enterprise-readiness-plan.md`（可靠性/安全分阶段计划）· `docs/newapi-parity.md`（逐模块对照结论）· `docs/移植-前端.md`（hyxapi 前端可移植项）· `docs/reports/frontend-duplication.md`（T-340 不抽结论）· `docs/protocols/前端操作反馈协议.md`。
