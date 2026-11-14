# 企业级修复计划（依据 2026-11-14 差距评审）

> 状态：**计划，未实施**。日期 2026-11-14，基线 `HEAD = 134fbf1`。
> 来源：`docs/reports/enterprise-gap-review-2026-11-14.md`（下称"评审"），条目 ID 与评审一致。
> 与 `docs/enterprise-readiness-plan.md`（下称"可靠性计划"，任务 A-xx…E-xx）的关系：**本计划不重复其中的可靠性/安全任务**，只在依赖处引用；两份计划的任务可以并行，但同一文件的改动要先协调。
> 负责人：待分配。不承诺工期，"量级"只表示相对大小（S ≈ 1 个小 PR，M ≈ 2–4 个 PR，L ≈ 跨多周）。

---

## 0. 原则

1. **小 PR、单一行为变更**：每个任务拆成可独立回退的 PR；提交信息遵循 `AGENTS.md` §1（中文 + Conventional 前缀）。
2. **不破坏既有契约**：`/v1/*` 的 OpenAI 兼容格式、Provider 接缝、`arch_test.go` 依赖守卫、老 `/ui` 的行为（`tests/frontend/` 的 163 个脚本守着）在各自明确下线前保持不变。
3. **依赖铁律**：后端不引入新 Go 依赖（`/metrics` 手写、压缩用标准库）。前端构建依赖只在批准 Q-FE1 后引入，且**只存在于构建期**。
4. **验证口径**：沿用可靠性计划 §11 的 DoD——在固化快照里跑出绿色；改前失败、改后通过的回归证据；动了 go:embed 资源必须重建二进制并实测一条真实请求（`AGENTS.md`「运维」节）。
5. **决策先行**：阶段 1 起依赖 §1 的拍板项；未拍板的阶段不开工，**阶段 0 不依赖任何拍板**，可以立刻开始。

---

## 1. 开工前需拍板（阻塞关系）

| ID | 问题 | 阻塞 | 未拍板时的默认处理 |
|---|---|---|---|
| Q-01 | 定位改为"单组织内部网关"？ | 阶段 3 的 RBAC（P3-2） | 保持单管理员，P3-2 不开工 |
| Q-FE1 | 前端走方案 B（新控制台 + Vite/React/TS/antd，构建期引入 Node）？ | 阶段 1 前端部分、阶段 2/3 前端 | 只做方案 A 的原地改进（P0-1、P2-5、P2-6） |
| Q-FE2 | 新控制台视觉：企业中性风 vs 延续 MAXIMALISM | P1-4 的主题配置 | 用 antd 默认主题 + 品牌色，可后改 |
| Q-API1 | 允许新增 `/api/admin/v1`，老路径做兼容别名？ | P3-1 | 不开新前缀，只做 P1-1/P1-2 的就地统一 |
| Q-ENG1 | 同意清理本地杂物（逐项确认）？ | P0-9 | 只出清单，不删任何东西 |

---

## 2. 阶段 0：低风险快修（不依赖拍板）

**目标**：不改技术栈，先修掉体验、可观测性、安全头和仓库卫生里"便宜且确定"的问题。
**退出条件**：本阶段全部任务验收通过；`go build ./... && go vet ./... && go test ./... -count=1`（快照内）全绿；`node tests/frontend/run_frontend_suites.js` 全绿；7863 实例换新二进制后真实请求正常。

### P0-1 · 控制台 hash 路由（FE-R1）· S

- **改动**：`internal/server/ui/app.js` 的 `showPanel`（约 `:4672`）在切换时写 `location.hash = '#/' + navKey(sec)`；新增 `hashchange` 监听 → 按 key 找 section 调 `showPanel`；启动时优先读 hash，没有 hash 再回落 `localStorage['wb2api.panelkey']`（保留现有行为）。iframe 页（status/channels/groups/tokens/models）本轮只做到"主台能深链到该面板"，iframe 内部的子状态不处理。
- **注意**：上游面板的 key 形如 `workbuddy:travel`，要 `encodeURIComponent`；未知 key 回落仪表盘，不报错。
- **验收**：① 打开 `/ui#/logs` 直接进入请求日志；② 连续切 3 个面板后按后退键能逐个退回；③ 两个标签页分别停在不同面板，刷新后各自保持；④ 无 hash 时行为与现在一致。
- **测试**：在 `tests/frontend/` 新增一个离线行为测试（参照 `verify_first_batch_behavior.js` 的抽函数 + DOM 桩写法），覆盖上面 4 条；再跑一次真浏览器 e2e。

### P0-2 · 存活 / 就绪探针拆分（BE-O1）· S

- **改动**：`internal/server/handler.go:517` 旁新增 `GET /livez`（进程在即 200，无鉴权）和 `GET /readyz`（逻辑同现 `healthz` 的 `ServableNow`，**另外**在关停开始时立即返回 503——对接可靠性计划 B-01 的"撤 readiness"）。`/healthz` 不动。
- **同步**：Dockerfile 的 `HEALTHCHECK` 改用 `/livez`（避免账号耗尽时容器被判不健康）；`docker-compose.yml`、README「API」节、`docs/openapi.yaml` 加两条 path。
- **验收**：账号池全部冷却时 `/livez`=200、`/readyz`=503、`/healthz`=503（兼容）；发送关停信号后 `/readyz` 在排空期间返回 503。
- **测试**：`handler_test.go` 现有 healthz 用例旁加对应用例。

### P0-3 · 请求 ID 贯穿（BE-O3）· S

- **改动**：在 `Handler.ServeHTTP`（`handler.go:560`）最外层包一层：读入站 `X-Request-Id`（仅接受 ≤64 字符的 `[A-Za-z0-9._-]`，否则丢弃），没有就生成（`crypto/rand` 16 字节 hex）；放进 context，并回写响应头。`logbuf.Entry`（`internal/logbuf/logbuf.go:30`）**追加**字段 `RequestID`；管理面审计 `store.AuditLog` 追加同名列（gorm AutoMigrate 可加列，确认迁移脚本 `cmd/migrate-sql` 的字段顺序约束）；`/v1` 错误信封的 message 不改，只加响应头。
- **注意**：`internal/gateway` 是核心包，request id 的 context key 放在核心侧新包或 `server` 内，**不得**让上游包依赖它（arch_test 会拦）。
- **验收**：同一请求的响应头、请求日志、审计记录（管理操作）三处 ID 一致；伪造的超长或含特殊字符的入站 ID 被替换。
- **测试**：单元测试 + `logbuf` 的 SQL 等价测试（`sqllog_equivalence_test.go`）要随新字段更新。

### P0-4 · 安全响应头（BE-S1）· S

- **改动**：新增中间件，作用于 `/ui*`、`/admin*`、`/status`：`X-Content-Type-Options: nosniff`、`Referrer-Policy: same-origin`、`Content-Security-Policy: frame-ancestors 'self'`（iframe 都同源，不受影响），HTTPS 下加 HSTS（跟随 `account` 包现有的 Secure 判定）。完整 CSP **先用 `Content-Security-Policy-Report-Only`**，策略 `default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:`，report-uri 指向一个只计数不落正文的端点。
- **不做**：本阶段不去掉 `unsafe-inline`（那是 P2-6 的事）。
- **验收**：所有页面功能不受影响（跑全量前端套件 + 对比度巡检不变）；跨源页面 iframe 嵌入 `/ui` 被浏览器拒绝；Report-Only 一周内的违规计数作为 P2-6 的输入。

### P0-5 · 静态资源预压缩（BE-S3）· S

- **改动**：`internal/server/ui_assets.go` 的 `uiAsset` 在进程启动时用标准库 `compress/gzip` 压一次（br 需第三方库，**本阶段不做**），按 `Accept-Encoding` 返回，加 `Vary: Accept-Encoding`，ETag 区分编码。7 个 HTML 页同样处理（它们 `no-store`，压缩仍有效）。
- **验收**：实测 `app.js` 传输体积（记录压缩前后数字）；`ui_assets_test.go` 的 304 用例在 gzip 下仍通过；不带 `Accept-Encoding` 的客户端拿到原文。

### P0-6 · openapi ↔ 路由对账测试（BE-A2）· S

- **改动**：抽出路由注册清单（`server/handler.go:457-529` 与 `admin.register` 的 83 条），新增测试：解析 `docs/openapi.yaml` 的 paths + methods，与注册清单**双向**比对；差集非空即红。首次运行会有存量差异：**先把差异修平再合入**，不允许加豁免名单（除非逐条写明理由）。
- **注意**：解析 YAML 不引入依赖——只需识别 `^  /path:` 与其下 `    get:` 这类缩进行，写一个够用的小解析器并对它本身加测试。`.gitignore` 已放行 `docs/openapi.yaml`，CI 干净签出可读到。
- **验收**：故意在 admin 新增一个路由而不改 openapi，测试变红。

### P0-7 · 测试残留浏览器 profile（ENG-2）· S

- **改动**：`internal/monkeycode/browserlogin.go:470` 的 `profileRoot()` 已有"配了 auth_dir 就放 auth_dir 下"的分支；排查哪些测试在未配 auth_dir 时真的拉起了浏览器，让它们用 `t.TempDir()` 作为 auth_dir。加一条守卫测试：`TestMain` 结束后包目录下不得出现 `.browser-profiles`。
- **验收**：清空该目录后跑 `go test ./internal/monkeycode/ -count=1`，目录不再生成。现存的 145 MB 残留属于 P0-9 的清理清单。

### P0-8 · 版本与依赖口径对齐（ENG-4 / ENG-5，对应可靠性计划 A-01）· S

- **改动**：`go.mod` 加 `toolchain` 行作为唯一来源；`ci.yml` 改用 `go-version-file: go.mod` 并删掉过时注释；Dockerfile 的 builder 镜像版本与之对齐（加一条 CI 检查：Dockerfile 里的版本号 == go.mod）。`AGENTS.md`「依赖铁律」按实际 `go.mod` 改写（go-redis、wazero、gorm + glebarez/sqlite、x/crypto），并加一个测试锁定直接依赖白名单（`internal/store/db_test.go` 已有禁用 cgo 驱动的类似写法可参照）。
- **验收**：三处版本一致；往 `go.mod` 加一个新直接依赖，白名单测试变红。

### P0-9 · 本地杂物清单（ENG-1 / ENG-3）· S，**需 Q-ENG1**

- **改动**：只产出清单（路径、大小、最后修改时间、是否被任何脚本或文档引用），提交给用户逐项确认；确认的条目移到仓库外的归档目录（**移动而不是删除**）。候选：12 个 `_backup-*`、`_scratch/`、根目录 3 个 exe、`config.json.bak-*`、`internal/monkeycode/.browser-profiles/`、`docs/agent2api-repo/`（含嵌套 `.git`）与 `docs/agent2api-repo.tar.gz`。
- **验收**：清理后 `go build ./...`、全量测试、`start.bat` 启动均不受影响；文档中对被移文件的引用已更新或标注"已归档"。

---

## 3. 阶段 1：前端地基 + 管理 API 规范（依赖 Q-FE1）

**目标**：搭好新控制台的工程骨架和"可被类型化调用"的管理 API。
**退出条件**：`/console` 可登录，并展示布局和至少一个真实页面；`/ui` 行为不变；CI 包含前端构建与测试门禁。

### P1-1 · 管理面统一错误信封（BE-A1）· M

- **改动**：`internal/admin` 新增 `apiError{Code, Message, RequestID}` 与 `writeAPIError`；先替换 `admin.writeError`（`admin.go:4161`）的实现，让所有经它的出口自动变成新格式；`account.writeError` 与 `consoleAdminAuth` 的 `{error, code}` 改为同一结构。**兼容**：保留顶层 `error` 字符串字段一个版本（老前端读的是它），新增 `error_detail{code,message,request_id}`；老 UI 迁完后再收敛。`code` 列成常量表，进 openapi。
- **验收**：抽样 10 个错误路径的响应结构一致；老 UI 前端套件全绿（证明兼容字段有效）。

### P1-2 · 统一分页（BE-A4）· S

- **改动**：抽 `parsePage(r) (page, size)`（size 上限 200）与 `pageResult{items,total,page,page_size}`；channels、tokens 改用它（保留 `ps`/`size` 别名）；logs 保持 `cursor`（追加型数据）但响应字段名统一为 `items` + `next_cursor`，老字段保留一个版本。
- **验收**：`page_size=100000` 被截断到 200；老参数名仍可用。

### P1-3 · `/metrics`（BE-O2）· M

- **改动**：新包 `internal/metrics`（核心包，登记进 `arch_test.go` 的 corePackages），手写 counter / gauge / histogram 与 Prometheus 文本格式输出。首批指标：`wb2api_requests_total{provider,model,code_class}`、`wb2api_request_duration_seconds`（直方图）、`wb2api_ttfb_seconds`、`wb2api_inflight_requests`、`wb2api_pool_accounts{provider,state}`、`wb2api_provider_circuit_open{provider}`、`wb2api_retries_total{provider,reason}`、`wb2api_stream_aborted_total{reason}`、`wb2api_logsink_dropped_total`。`model` 标签只取模型目录里已知的名字，未知一律记为 `other`（防基数爆炸）。
- **鉴权**：默认需要管理令牌（Bearer）；配置项 `server.metrics_public` 可设为无鉴权（仅建议内网）。
- **验收**：Prometheus 能抓取；用 `promtool check metrics` 校验格式（本机没有就在 CI 里跑）；压测 1 分钟后标签组合数不超过预估上限。

### P1-4 · 新控制台脚手架（FE-N0）· M

- **决策记录**：先写 `docs/decisions/0003-console-build-pipeline.md`（ADR：为什么引入构建期 Node、为什么产物不入库、为什么选该技术栈、被否决的方案 A/C）。
- **目录**：`web/`（Vite + React + TypeScript + antd + react-router + TanStack Query）；`vite build` 输出到 `internal/server/webdist/`（gitignore）。
- **嵌入**：`internal/server/console.go` 用 `//go:embed all:webdist`；**干净签出没有产物时**嵌入一个占位 `index.html`（提示"本二进制未包含新控制台，请用 Release 版或先 npm run build"），保证 `go build ./...` 永远可过。`/console/*` 未命中文件时回落 `index.html`（SPA）。哈希文件名资源 `Cache-Control: immutable, max-age=31536000`，`index.html` 为 `no-store`。
- **鉴权**：复用现有会话 cookie 与 `/admin/auth/*`，不另造登录。
- **类型化客户端**：从 `docs/openapi.yaml` 生成 TS 类型（`openapi-typescript`，构建期依赖）；P0-6 的对账测试是它可信的前提。
- **CI / 发版**：`ci.yml` 新增 job：`npm ci && npm run typecheck && npm run lint && npm run test && npm run build`；`release.yml` 先构建前端再 `go build`，并加一条断言"二进制里嵌的不是占位页"。
- **验收**：Release 产物在无 Node 的机器上运行，`/console` 可登录；干净签出直接 `go build` 成功，`/console` 显示占位页；`/ui` 不受影响。

### P1-5 · 新控制台第一批页面（FE-N1）· L

- **布局**：antd ProLayout——侧栏导航按 `/admin/ui/manifest` 的能力位动态生成（与老 UI 同一数据源，**不在前端写死上游名**）；顶栏只保留服务状态、用户菜单（含主题切换、退出）。
- **账号池**：ProTable，按上游分组 tab + 全局搜索（UID / 昵称）+ 状态筛选 + 列设置（列来自 `AccountColumnsExt`）；行内只放 1–2 个主操作，其余收进"更多"；多选批量禁用 / 启用 / 刷新额度；"移除"二次确认并说明后果。
- **请求日志**：时间范围选择、按 provider / model / 状态码 / request_id 筛选，筛选条件同步到 URL；详情抽屉展示 decision 留痕与重试链路（依赖 P0-3）。
- **入口**：老 UI 对应面板顶部加"试用新版 →"链接；新版加"返回旧版"。
- **验收**：两页覆盖老 UI 对应面板的全部操作（逐项对照清单，附在 PR 里）；组件测试（vitest + Testing Library）覆盖筛选、批量操作、危险操作确认；一条 Playwright/CDP e2e 跑通"登录 → 账号池 → 禁用 → 启用"。

---

## 4. 阶段 2：迁移主体页面 + 老 UI 收紧

**目标**：新控制台覆盖日常使用的全部页面；老 UI 只剩兜底作用。

| ID | 任务 | 量级 | 要点 / 验收 |
|---|---|---|---|
| P2-1 | 仪表盘改版（FE-N2） | M | 时间范围（1h/24h/7d/自定义）；首屏趋势图（请求量、成功率、P50/P95、token）；模型用量 Top-N + "其余"折叠。数据来自现有 `/admin/stats/*`（`series.go`），缺的聚合在后端补，**不在前端算** |
| P2-2 | 渠道 / 分组 / 令牌迁移 | L | 三页目前是独立 HTML + iframe，迁移后老页保留到 P3-3；表单校验规则从 openapi schema 取 |
| P2-3 | 设置页迁移 | M | 有"脏数据离开提示"（老 UI 已有 `settings_dirty` 测试守着这条行为，新版要等价）；保存期间禁用表单（对齐可靠性计划 §13 FE-03 的行为） |
| P2-4 | 三态与操作反馈规范（FE-N3） | S | 统一 Skeleton / Empty / Result 组件；所有异步按钮"进行时措辞 + disabled"；遵守 `docs/protocols/前端操作反馈协议.md` |
| P2-5 | 老 UI：消除内联事件（FE-R2） | M | groups 页 12 处、主台 5 处 `onclick=` 改为 `addEventListener`；为 P2-6 去掉 `unsafe-inline` 铺路 |
| P2-6 | CSP 由 Report-Only 转强制 | S | 依据 P0-4 收集的违规报告；新控制台天然无内联脚本，可直接用严格策略；老 UI 若仍有内联 `<script>`，改为 nonce |
| P2-7 | 管理面 CSRF 第二道防线（BE-S2） | S | 非 GET 的 `/admin/*`：带 `Origin` 或 `Sec-Fetch-Site` 且不同源 → 403；纯 Bearer 调用（无 Origin）放行。测试覆盖：同源浏览器、跨站表单、curl |
| P2-8 | `log.Printf` → `logx` 结构化（BE-O4） | M | 先给 `logx` 加 JSON 输出与固定字段（ts/level/msg/request_id/provider）；按包逐个替换，每个 PR 一个包，从 `server`、`admin`、`pool` 开始 |

可靠性计划的 B-01…B-04、C-01 在本阶段并行推进（不在本计划内追踪）。

---

## 5. 阶段 3：API 资源化、角色与老 UI 下线

| ID | 任务 | 依赖 | 量级 | 要点 / 验收 |
|---|---|---|---|---|
| P3-1 | `/api/admin/v1` 资源化（BE-A3 + BE-A5） | Q-API1、P1-1/P1-2 | L | 资源 + 标准动词（`DELETE /api/admin/v1/channels/{id}` 等），响应用具名 struct；老路径成为别名，响应头加 `Deprecation` 与 `Link: rel="successor-version"`；新控制台全部切到新路径；对账测试（P0-6）同时覆盖新老两套 |
| P3-2 | 角色模型扩展（Q-01 / 可靠性计划 C-02，遵守 BE-S4） | Q-01、C-01 | L | viewer / operator / admin 三档；权限表在服务端，**默认拒绝**；"发钥匙 / 改角色 / 测试连接"单独一档；钥匙可用范围不得超过发放者权限；角色变更与越权尝试全部进审计。必须有越权测试矩阵（每个端点 × 每个角色），参照 LiteLLM CVE-2026-47101/47102 的形态写反例 |
| P3-3 | 老 UI 下线 | P2 全部页面迁移完 | M | `/ui` 302 到 `/console`（保留一个版本的 `?legacy=1` 逃生口）；删除 iframe 页与 4 个 `*_sync.js`；`tests/frontend/` 里只守老 UI 的脚本随之下线（逐个列出，不批量删） |
| P3-4 | 角色感知 UI（FE-N5） | P3-2 | M | 菜单与按钮按权限显隐；**服务端仍做最终校验**（测试：直接调接口越权必须 403） |
| P3-5 | 大文件按职责拆分（评审 §5.4 / 可靠性计划 E-01） | 各阶段改动稳定后 | L | `handler.go`、`admin.go`、`main.go`、`config.go` 按职责拆；每个 PR 只搬代码不改行为，搬前搬后测试集与结果一致 |
| P3-6 | 全局搜索 / 命令面板（FE-N4） | P1-5 | M | 按 UID、模型、渠道名、request_id 跳转 |

可靠性计划的 D 阶段（SLO、备份、供应链）在本阶段并行。

---

## 6. 依赖关系一览

```text
P0-6 openapi 对账 ──► P1-4 类型化客户端 ──► P1-5 / P2-x 新页面
P0-3 request_id ──► P1-5 日志详情链路 · P1-1 错误信封里的 request_id
P0-4 CSP Report-Only ──► P2-5 去内联事件 ──► P2-6 CSP 强制
P0-2 /readyz ──► 可靠性计划 B-01（关停时撤 readiness）
P1-1 / P1-2 ──► P3-1 资源化
Q-FE1 ──► P1-4 ──► P1-5 ──► P2-1…P2-4 ──► P3-3 老 UI 下线
Q-01 + 可靠性计划 C-01 ──► P3-2 RBAC ──► P3-4
```

---

## 7. 风险与回退

| 风险 | 缓解 | 回退 |
|---|---|---|
| hash 路由跟老 UI 的 `localStorage` 恢复逻辑冲突 | hash 优先、localStorage 兜底，保留现有测试 | 去掉 hashchange 监听即恢复原状 |
| `/readyz` 被误配为 liveness | 文档与 Dockerfile 明确区分；`/healthz` 语义不变 | 编排侧改回 `/healthz` |
| CSP 误杀导致页面白屏 | 先 Report-Only 一周；对比度巡检与全量前端套件作门禁 | 回到 Report-Only |
| 新增日志 / 审计字段影响迁移脚本或 SQL 等价测试 | 只追加列；同步更新 `migrate-sql` 与等价测试 | 新列可空，老代码忽略即可 |
| 引入 Node 后 Release 流水线变脆 | 锁定 Node 版本与 `package-lock.json`；占位页保证 `go build` 永远可过 | Release 回退为占位页 + 老 `/ui`（老 UI 在 P3-3 前一直可用） |
| 新老控制台功能不一致，用户困惑 | 每迁一页附功能对照清单；双向入口链接 | 隐藏"试用新版"入口 |
| 错误信封变更打断老前端或外部脚本 | 兼容字段保留一个版本；release notes 写清楚 | 恢复旧 `writeError` 实现 |
| 多 agent 并行改同一文件（`app.js`、`handler.go`） | 任务开工前在 `docs/progress-log.md` 登记占用的文件；在快照里验证 | 按 PR 粒度回退 |

---

## 8. 每个任务的交付检查单

- [ ] PR 只包含一个任务 ID 的改动，描述里写明任务 ID、改前行为、改后行为
- [ ] 新增或更新的测试先在改前红、改后绿（附命令和输出）
- [ ] 快照内 `go build ./...`、`go vet ./...`、`gofmt -l`（输出为空）、`go test ./... -count=1` 通过；16GB 机器 OOM 时按 `docs/known-issues.md` 用 `GOFLAGS=-p=1` 复验
- [ ] 动了前端：`node tests/frontend/run_frontend_suites.js`、各 `*_sync.js --check` 通过；动了颜色就跑对比度巡检
- [ ] 动了 go:embed 资源或出口行为：重建 `bin\wb2api-server.exe`，换上 7863 并实测一条 `/v1/chat/completions`
- [ ] `docs/openapi.yaml`、README、`CHANGELOG.md` 同步；新增文档已在 `.gitignore` 放行
- [ ] `docs/progress-log.md` 追加一条（含 `[决策]` / `[否决]`），影响长期规则的同步改 `AGENTS.md`

---

## 9. 建议的开工顺序（阶段 0 内部）

1. P0-8（口径对齐，最小、能顺手修 CI 注释）
2. P0-6（对账测试，后面所有 API 改动都靠它兜底）
3. P0-3 → P0-2（同在 `handler.go`，连着做，减少冲突）
4. P0-4 → P0-5（同在 UI 托管层）
5. P0-1（纯前端，可以跟 3、4 并行）
6. P0-7，P0-9 等用户确认清单后再执行
