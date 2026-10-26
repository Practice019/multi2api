# PLAN-hyxapi-alignment.md — workbuddy2api × hyxapi 移植/对齐总计划

> **本文件是当前唯一的总计划**，接管此前三份 PLAN 的「下一步」（继承/作废关系见 §0.3）。
>
> **路径约定**：仓库内路径一律相对仓根、用正斜杠；跨仓路径写作 `workbuddy2api/...` 与 `hyxapi/...`，分别指 `D:/project_GIT/workbuddy2api` 与 `D:/project_GIT/hyxapi`。
>
> **证据口径**：本文件所有文件数、行号、命令输出均来自 2026-10-26 的四路只读实读（workbuddy2api 现状与三份 PLAN / 架构接缝 / hyxapi 后端 / hyxapi 前端与工程链），逐条带 file:line；凡未能实测的归因一律显式标注「未验证（归因来源：…）」。
>
> **更正与来源**：本文件 2026-10-26 经 verifier-06 独立验收后更正了 9 处数值/路径（E1–E9，已就地改正文）与前端基线口径（§8.3）。凡标注「verifier-06」或「独立验收实测」的数字，来源是该轮验收；本文件作者未复测这些数字。
>
> **许可证前提**：workbuddy2api = **MIT**（`workbuddy2api/LICENSE:1-4`，Copyright (c) 2026 Sliverkiss；Copyright (c) 2026 Practice019 — local admin console, web UI and check-in history additions）。hyxapi = **AGPL-3.0**（`hyxapi/LICENSE` 标准全文 661 行，MD5 02D4EA62D9FB26520EEE03E3923515CE；`hyxapi/NOTICE:9-25` 为 AGPLv3 §7 附加条款）。因此**任何** hyxapi 侧内容的引入都必须落到 §3 D6 定义的三类动作之一，§4 每个任务都带 AGPL 动作分类字段。

---

## 0. 范围与前提

### 0.1 本计划覆盖什么

覆盖三件事，且只覆盖这三件事：

1. **数据层接线与迁移**：把已经写好但未接入的 SQL 层（`workbuddy2api/internal/store/`、`workbuddy2api/cmd/migrate-sql/`）接进服务端、把 `data/*.json` 迁到 SQL，并给出 JSON 的降级定位。
2. **三页面对齐 hyxapi**：系统状态页 / 渠道管理页 / 模型管理页，以 hyxapi 的字段与行为为准，用本仓的技术形态（单文件无构建前端）实现。
3. **前置基线修复**：让验收命令真的可用 —— 当前 `go test ./...` 是红的（§0.4），不先修就无法用测试作为门禁。

### 0.2 本计划不覆盖什么

- 不改 `internal/gateway` 的 Provider 契约（§6.1 冻结），不新增上游，不重写账号池。
- 不引入 gin / React / Rsbuild / TanStack Query / Casbin / RBAC / 计费 / 多租户 / 插件系统（§9）。
- 不搬运 hyxapi 的 43 张表（`hyxapi/model/main.go:243-285` 主清单 41 + `:329/:364-399` + `model/audit_log.go:251-253`），只建本仓所需的 12 张（已在 `workbuddy2api/internal/store/db.go:256-271` 定义）。
- 不把 hyxapi 的 Go 1.26 语法搬到本仓（本仓 go.mod:3 声明 go 1.22.5，CI 用 1.22，见 §7 风险 R2）。

### 0.3 与三份既有 PLAN 的关系

| 既有文件 | 实测状态 | 与本计划的关系 | 处理动作 |
|---|---|---|---|
| `workbuddy2api/PLAN-groups.md`（494 行，mtime 2026-10-24 01:02） | P1–P6 全部落地（CHANGELOG v1.10.0 §分组系统；git log `3f9cf7e`/`bb61262`/`b4583a5`）；§6 四问全部标注「已定」 | **已完成的历史计划**，不再作为待办来源 | 保留原文；本计划不重开分组话题 |
| `workbuddy2api/PLAN-newapi-alignment.md`（119 行，mtime 2026-10-23 21:04） | T1–T8 全部落地（CHANGELOG v1.10.0 §健壮性 Wave1→Wave4；git log `f2a5894`/`9307243`/`3b962e3`/`36662f4`） | **能力来源已被吸收**；其 §5 第 2 条「不引入 GORM（本仓无 DB）」已作废 | 正文 L107 该句仍未被删除，本计划 §2/附录 B 明确以 `PLAN-sql-unify.md:4` 为准 |
| `workbuddy2api/PLAN-sql-unify.md`（257 行，mtime 2026-10-26 08:48） | T1/T2/T3/T8/T12 完成并存在于工作区；T9 页面已写未接线；T4/T5/T6/T7/T10/T11/T13 未做；§8 五个开放问题无用户答复 | **本计划的主要上游**：承接其未完成任务，并把其 §1 的过期现状描述更正为实测值 | 本计划 §6.0 裁定数据层真相；§10 合并其 §8 五问 |

**判定：本计划 = 对 PLAN-sql-unify.md 的延续 + 对 PLAN-newapi-alignment.md 的收编 + 对 PLAN-groups.md 的归档。** 三者都**不删除**，但此后「下一步做什么」只以本文件为准。

### 0.4 必须先声明的基线：`go test ./...` 当前是红的

实测（2026-10-26，仓根 `D:/project_GIT/workbuddy2api`）：`go test ./... -count=1` **exit=1，152.8s**；同时 `go build ./...` **exit=0，21.6s**。四类失败：

| # | 失败 | 位置 | 性质 | 处置任务 |
|---|---|---|---|---|
| ① | `[setup failed]` — 测试文件语法错误 | `internal/store/option_test.go:237`（missing comma in argument list）、`cmd/migrate-sql/main_test.go:65`（string literal not terminated） | 未提交的在制品自身的语法错 | T01 / T02 |
| ② | `TestAllUpstreamsAreCovered` FAIL | `cmd/server/account_columns_invariant_test.go:281`：`internal/` 下的 `store`、`sysmon` 未被 `allUpstreams()` 覆盖 | 新增包未登记进既有守卫 | T03 |
| ③ | `internal/codearts` 三个 `*Live` 测试 401 | `t2_quota_ext_test.go:95`、`welfare_test.go:62`、`welfare_test.go:110` | 凭证过期，环境问题 | 不修；在 §8 声明为已知环境红 |
| ④ | 其余 33 包 ok，`internal/gateway` ok（35.9s） | — | 正常 | — |

**关键推论（写进所有验收口径）**：`go build ./...` 过了**不等于**测试能编译 —— `_test.go` 不参与 `go build`。§8 的所有验收命令都必须显式带 `-count=1` 且按包运行，且 Wave 0（T00–T03）是其余一切任务的前置。**前端侧同样是红的**：`tests/frontend/run_frontend_suites.js` 自身被截断（exit=1，见 T00），另三条 runner 的当前真实退出码见 §8.3 —— 任何「套件全绿」的结论都必须先声明这两处基线。

**2026-10-26 更新（Wave 0 之后）**：①（测试文件语法错）与 ②（覆盖守卫）已由 T00–T03 消除，③ 仍是凭证到期的环境红；施工后的完整进度、仍然红着的 3 条断言级红与一条偶发红，见 **§0.6**。

### 0.5 用户口径「三页面以 hyxapi 为准」如何落实

落实为四列，逐页写死；凡是 hyxapi 有而本仓不采的部分，必须写明理由，不允许默认忽略。

| 页面 | 以 hyxapi 的什么为准（证据） | 明确不照搬的部分 | 本仓实现位置 |
|---|---|---|---|
| 系统状态页 | 字段集与语义：version/uptime/go_version/goroutines/active_connections/resources/runtime/database/log_database/redis/request_stats/request_health/disk_cache/channel_affinity（`hyxapi/controller/system_status.go:121-183` 为 c.JSON 响应字段字面量段；`:23-120` 是 GC/DB/Redis/today 计算段）；阈值三档 75/90（`:96-102`）；窗口 5min 与 `window_seconds:300`（`:85,:134`）；成功率分档 <95 warning / <90 critical（`:103-110`） | 不复刻 `active_connections` 的口径（hyxapi 是 `middleware/stats.go:14,20,24,37-41` 只统计挂 relay-router 的 `/v1` 在途请求）；不复刻 `channel_affinity`（本仓无 HybridCache 亲和缓存） | `internal/server/webui-status.html`（已存在、未接线）+ `internal/sysmon/`（已存在、无引用）+ `logs` 表聚合 |
| 渠道管理页 | 列表与筛选行为：分页 `common.GetPageQuery`（`hyxapi/controller/channel.go:85`）/`Count`（`:134`）/`Limit-Offset-Omit(key).Find`（`:140-144`）；状态筛选字典（`:52-65`）；`type_counts` 一条 GROUP BY 塞进同响应（`:156-176`）；内存搜索+内存分页（`:257-387`）；分页上限 100（`hyxapi/common/page_info.go:37-75`）；状态常量（`hyxapi/common/constants.go:211-213`）。前端列与交互以 `hyxapi/web/src/pages/Channel/`（38 文件，含 10 个 `*.test.js`；`index.tsx` 877 行、`ChannelEditor.tsx` 1182 行）为准 | 不搬 `POST /api/channel/:id/key` 的四段式高敏链路（`hyxapi/router/channel-router.go:44-50`：AdminAuth→RootAuth→CriticalRateLimit→DisableCache→SecureVerificationRequired）；不引入 Casbin 逐路由权限表（`channel-router.go:59-108`，49 条） | `internal/server/webui-channels.html`（现为既有页面，需按 §5 串行改造） |
| 模型管理页 | 两级目录（Model + Vendor）+ 合成行 + 渠道用到但目录没有的模型不可勾选：结构以 `hyxapi/web/src/features/metadata/`（7 文件：`index.tsx` 125、`catalog.ts` 34、`MetadataEditor.tsx` 57、`MetadataSyncDialog.tsx` 63、`ModelDeleteDialog.tsx` 33、`VendorOperationDialog.tsx` 42、`ui.tsx` 4）为准；后端控制器很薄（`hyxapi/controller/model_meta.go` 337 行、`vendor_meta.go` 138 行） | **不做定价**（`model_pricing.go` 61 行与 `/api/model-pricing/*` RootAuth 路由 `api-router.go:262-270` 不移植）—— 见 §10 Q5/Q9 | `internal/server/webui-models.html`（新建） |

---

### 0.6 施工进度锚点（2026-10-26 本轮施工后的回写）

| 波次 | 任务 | 状态 | 锚点（本文件作者在本机 `git log`/`git tag` 逐个确认过） |
|---|---|---|---|
| 基线 | 收口 SQL 层在制品 + 本计划 | 已推送 | `41b17d6` / tag `baseline-2026-10-26` |
| Wave 0 | T00–T03（runner 截断 + 两处测试编译错 + 覆盖守卫） | **完成** | `8ce8b30` / tag `wave0-done-2026-10-26` |
| Wave 1 | T04–T09（数据层接线） | **完成** | `195c1e1` / tag `wave1-done-2026-10-26` |
| Wave 1b | T24（`SessionStore` 换 SQL 后端）、T25 | **完成** | 并入 `195c1e1` |
| Wave 1.1 | T11（`store` 注入 `cmd/server`）、T26、T27 | **完成** | `ecc7f42` / tag `wave1.1-done-2026-10-26` |
| Wave 2 | T12/T13/T14（三页面对齐） | **完成（仅本地）** | `7ea6444` / tag `wave2-done-2026-10-26`；**⚠ 尚未推上 GitHub** —— 本文件作者只确认了「本地领先 `origin/HEAD` 恰好这一条」（`git log origin/HEAD..HEAD`）；「出网抖动」这一归因来自主脑回报，**未独立复现** |
| Wave 0b | 前端 7 条基线红（见 §8.3 基线现实） | **未开工** | 已拆成 task-13 ~ task-17（dashboard_gen / models_display_gen / settings_layout_gen / stats_disk_gen / gen_ui_manifest / audit_tracked_content / verify_probe_console_loaded） |
| T10 | 真实 `data/` 迁移 + 字节等价校验 | **未做** | 等用户给专用窗口（7863 正在用 `data/`，见 §4 T10 的执行纪律 V4） |

**仍然红着的东西（本轮施工实测，别当成已修）**：§0.4 的 ①（`internal/store/option_test.go:237` 编译错）与 ②（`TestAllUpstreamsAreCovered`）已由 Wave 0 消除；③（`internal/codearts` 三个 `*Live` 401）仍是凭证到期的环境红。此外 `internal/store` 另有 3 条**断言级**长期红：`TestBuildDSNRespectsCaller`（`db_test.go:327`，DSN 被 URL 转义成 `%28…%29`）、`TestInitializeSetupValidation`（`setup_test.go:226/229`）、`TestInitializeSetupSelfHealsRoot`（`setup_test.go:252`）；`TestInitializeSetupConcurrentProcesses` 在 6 路并发 `go test` 负载下**偶发**红（子进程 DDL 竞争，单独跑 3/3 绿）—— 已单独立 task-25 处置。

**已知的判据冲突（未解决，本轮未改它）**：`tests/frontend/verify_models_grouped.js` 的「每个声明 models 的上游分组都出现」断言与产品裁定「0 账号的上游整组不显示」（`internal/server/webui.html:13824`）互相拉扯 —— 脚本自己在 `:330-348` 写明了这一点，只有**每个上游都至少有 1 个可用账号的真实部署**才能过，半配置的隔离实例上必然红。处置：不得在半配置实例上把它当门禁。

---

## 1. 两个项目的真实基线

| 维度 | workbuddy2api | hyxapi | 证据 |
|---|---|---|---|
| 形态 | 多上游聚合反代网关（13 个上游注册点），OpenAI 兼容 + Anthropic Messages | New API 分叉（`module github.com/QuantumNous/new-api`），多租户计费网关 | `workbuddy2api/cmd/server/main.go` 13 处 `registry.Register`（L275/347/433/523/609/664/711/781/861/890/979/1012/1058）；`hyxapi/go.mod:1-7` |
| 规模 | 881 个 .go；`internal/` 顶层 43 个目录；`internal/gateway` 52 文件（非测试 33） | 1,216 个 .go 总数；不含 relaykit+tokenkit 为 1,072 | 实测文件计数；`workbuddy2api/internal/` 目录枚举 |
| 分支与 HEAD | `master`，`d1fc314`（2026-10-26 05:19） | `7ab8937d` | `git log -1` |
| Go 版本 | `go.mod:3` → **go 1.22.5**；CI 用 1.22（`.github/workflows/ci.yml:30`）；本机 go1.27 | **root go 1.26**；`relaykit` go 1.25.1；`tokenkit` go 1.26 | `hyxapi/{go.mod,relaykit/go.mod,tokenkit/go.mod}`；`hyxapi/AGENTS.md:12` 写 1.25.1+ 与 `README.md:106` 写 1.26+ **互相冲突**，源码为准 = 1.26/1.25.1 |
| 构建链 | 纯 Go：`go build ./...`（实测 exit=0 / 21.6s），无前端构建步骤 | Go + 独立前端：**Rsbuild 2**（不是 Vite）；必须先 `cd web && bun install --frozen-lockfile && bun run build`（`hyxapi/Dockerfile:26` 先 COPY dist 再 go build）；`web/dist` 已存在且 `go:embed`（`hyxapi/main.go:41,44`） | `hyxapi/web/package.json:7-10`、`rsbuild.config.ts`、`.storybook/main.js:9`；实测 `bun 1.4.4` / `node v24.15.0` 可用 |
| 依赖真相（本仓） | **已提交**：`redis/go-redis/v9 v9.18.0`、`tetratelabs/wazero v1.8.2`。**未提交（工作区）**：`gorm.io/gorm v1.25.12`、`glebarez/sqlite v1.11.0`（+10 间接包） | 重依赖栈：gin + GORM + Casbin + 43 张表 + relaykit/tokenkit 两个嵌套 module | `workbuddy2api/go.mod`（HEAD 版）与 `git diff go.mod`；`hyxapi/go.mod:1-7` |
| **存储真相（本仓）** | **运行时 JSON 是唯一权威**：`data/accounts.json`（`cmd/server/main.go:1411`）、`data/sessions.json`（`:1415`）、`data/apikeys.json`（`:1287`）、`<凭据根>/channels.json`（`cmd/server/main.go:1294-1295` 迁移说明、`:1315 channelsPath := filepath.Join(authsRoot, "channels.json")`；**`data/channels.json` 已废**：`Test-Path data/channels.json` = False，`:1316/:1319` 只是旧位置兼容迁移代码）、`data/state.json`（`:1167`）、`data/request-log.jsonl`（`internal/logbuf`）。**Redis 已接线**（`internal/redisstore`，注入点 `cmd/server/main.go:100`；未配置/连不上降级 Noop 并落 `data/sticky-bindings.json`，`:102`）。**SQL 已实现但未接线**：`internal/store/` 有 12 张表 struct（`db.go:256-271`），import 方只有 `cmd/migrate-sql`（业务引用 `main.go:37`；同包测试 `main_test.go:29` 也 import 同一包），`cmd/server` 与其它包零引用，`data/gw.db` 不存在 | 单一权威 DB：SQLite（默认 DSN `one-api.db?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate`，`hyxapi/common/database.go:19`），43 张表（`model/main.go:243-285` + `:329/:364-399` + `model/audit_log.go:251-253`）；`one-api.db` 917,504B 存在（被 `hyxapi/.gitignore:8 *.db` 忽略） | 逐条见左列 file:line |
| 许可证 | **MIT**（`LICENSE:1-4`） | **AGPL-3.0**（`hyxapi/LICENSE` 661 行标准全文；`hyxapi/NOTICE:9-25` 为 §7 附加条款：修改版必须在 UI 醒目处保留 `Frontend design and development by New API contributors.` 与指向 `https://github.com/QuantumNous/new-api` 的可见链接，并按 §7(c) 标注改动）；`relaykit`/`tokenkit` 无独立 LICENSE；`web/package.json` 无 license 字段；逐文件版权头覆盖率 Go 2/1216=0.16%、`web/src` 53/430=12.3% | `hyxapi/NOTICE:1,3` 仍署名上游 |
| 运行方式 | 单二进制；默认端口 7863（控制台 `/ui`），测试实例常用 7864/7868/18080/18081 | 默认端口 3000（`hyxapi/common/init.go:18`），本检出 `.env` 覆盖 `PORT=3001`（本机 3000 被上游 New API 占用） | `workbuddy2api/README.md`、`hyxapi/main.go:177-179` |
| 前端形态 | **单文件无构建**：`internal/server/webui.html` 16,101 行 / 924.7KB（`go:embed`），另有 `webui-login.html`、`webui-channels.html`、`webui-status.html`（新增未接线）；共享层 `dash-chart.js` / `model-picker.js` / `theme-max.css` | React 19 + Rsbuild 2 + Tailwind 4；24 deps / 14 devDeps / 7 scripts；自研 `components/ui` 10 子目录；无任何第三方 UI 组件套件 | `workbuddy2api/internal/server/`；`hyxapi/web/package.json` |
| 测试形态 | 461 个 `_test.go`；契约测试 `internal/gateway/contract.go:22-47` 8 条 + 反射发现（`:192-256`）；架构守卫 `internal/gateway/arch_test.go` 1,072 行 10 个 Test（`go test -v` 实测 125 PASS / 0 FAIL）；前端 5 类 runner | 单元测试为主；`web` 侧 67 个 `.test.js`，**0 个 import react / 0 个 render( / 0 个 @testing-library**，jsdom/playwright/happy-dom/vitest/jest 全部不存在 ⇒ **无 DOM 测试**（结论成立，可保留） | `hyxapi/web/src/**/*.test.js`；`modelPricingFrontend.test.js:10-16` 唯一浏览器模拟是手写 `globalThis.window` 桩；`tsconfig.json:22` 排除 `**/*.test.js` |
| 实测构建/测试命令 | `go build ./...` exit=0 / 21.6s；`go test ./... -count=1` **exit=1** / 152.8s（四类失败见 §0.4）；`go vet ./internal/gateway/...` exit=0 | root `go build ./...` exit=0；root `go test ./...` 默认并发 **OOM 失败**（`fatal error: runtime: out of memory` / `VirtualAlloc errno=1455`，非代码错）；`go test -p 2 ./...` **exit=0 全绿**（controller 147s、model 37s、middleware 7.5s、router 10.5s、service 11.2s、service/authz 12.1s、setting 7.9s）；`relaykit`/`tokenkit` 各自 build+test `-p 2` 双 exit=0；两侧 `git status --porcelain` 前后完全一致 | 实测输出 |

**§1 的三条硬结论（后续所有章节都建立在这三条上）**：

1. **SQL 已实现、未接线**：`internal/store/`（12 表）+ `cmd/migrate-sql/`（三段子命令）存在于工作区且可构建，但服务端零引用、`data/gw.db` 不存在。
2. **Redis 已接线**：`internal/redisstore` 是 `cmd/server/main.go:100` 的真实依赖，且带 Noop + 本地文件降级；它不是权威存储，只做粘性绑定与池状态快照。
3. **JSON 仍是运行时权威**：`data/*.json` 与 `request-log.jsonl` 是当前唯一被服务端读写的持久层；SQL 与 JSON 当前是「并存但只有 JSON 在跑」。

---

## 2. 能力重合度判定

### 2.A 根本不需要移植（形态不匹配）

| # | hyxapi 能力 | 证据 | 为什么不移植 |
|---|---|---|---|
| A1 | 计费体系（钱包/充值/订阅/支付/预扣-后补） | `hyxapi/model/main.go:243-285` 的 41 张表清单含 quota/subscription 系列；`controller/model_pricing.go` 61 行 | 本仓是免费额度池，无计价需求（`PLAN-sql-unify.md:38-43` §0.3 已判） |
| A2 | Casbin RBAC 权限矩阵 | `hyxapi/middleware/auth.go:428-436` → `service/authz/resolver.go:10-35`；`router/channel-router.go:59-108` 表驱动 49 条 | 本仓是单账号管理员模型（`workbuddy2api/README.md:303-334`），多一层权限层是空转（`PLAN-groups.md` §7 同判） |
| A3 | 多租户 / 工单 / 公告 / 邀请码 / 用户注册 / 2FA / Passkey / 邮箱验证 | `hyxapi/model/main.go:243-285` 对应表；`router/api-router.go` 用户面路由 | 本仓注册入口在账号库非空后整体关闭（`README.md:316-331`），无用户面 |
| A4 | 端点面 mj / suno / video / audio / realtime 全套 | `PLAN-sql-unify.md:41` §0.3 明确不搬 | 本仓协议面 = OpenAI 兼容 + Anthropic Messages |
| A5 | `task_plugin`(1487) / `distributor.getModelRequest`(700) / i18n 7 语言 | `PLAN-sql-unify.md:42` | 与本仓无对应形态 |
| A6 | gin 路由与中间件栈 | hyxapi 全仓用 gin；`relaykit/relayconvert/boundary_test.go:30-33` 甚至禁止 kit import gin | 本仓用 `net/http`（`PLAN-sql-unify.md:220` 该条至今有效） |
| A7 | React + Rsbuild + TanStack Query 前端构建链 | `hyxapi/web/package.json:7-10`；`Dockerfile:26` | 本仓前端是单文件零构建（§3 D2），引入即为架构级回退 |
| A8 | 定价与倍率（group_ratio 等） | `PLAN-groups.md:466` §7；`hyxapi/controller/model_pricing.go` | 本仓无计费，加了是死配置 |
| A9 | 渠道密钥的高敏四段式链路 | `hyxapi/router/channel-router.go:44-50`（AdminAuth→RootAuth→CriticalRateLimit→DisableCache→SecureVerificationRequired） | 本仓无 Root 概念（`internal/store/role.go:31` 新增 `RoleRoot=100` 仅为 setups 自愈判据），不引入 SecureVerification 体系；只保留二次确认 UI |
| A10 | logs 表之外的日志库抽象（LOG_DB 双库连接池） | `hyxapi/controller/system_status.go:52-63` | 本仓 SQLite 单文件单连接池，无第二个日志库 |

### 2.B 本仓已更强（保留本仓，不得为「一致」而抹掉）

| # | 本仓能力 | 证据 | 为什么更强 |
|---|---|---|---|
| B1 | 账号池：三层熔断 / P95 / 持久化惩罚态 | 非测试口径 **4,833 行 / 5 文件**（旧文档 `PLAN-sql-unify.md:29` 记 4,838，属**同一非测试口径、未过时**；verifier-06 复测非测试 splitLF = 4,838）；含测试口径 **11,195 行 / 29 文件**；`pool.go` 单文件 **2,812 行**。锚点：`internal/pool/pool.go:390/470/2708`（快照镜像接口） | hyxapi 旧计数 1,694 行且无熔断（同源计数，未复测） |
| B2 | 10 个 `ErrorKind` 的稳定分类 + 只追加不插入铁律 | `internal/gateway/errorclassifier.go:99-209`（None=0…FatalClient=9；铁律 `:184-189,:207`） | 追加式枚举保证线上错误码不漂移 |
| B3 | 32 个扩展点（AGENTS.md §4 写 28 个，已过期） | 多出 4 个：`CredentialRefresher`（`credential_refresher.go:69`）、`QuotaConsumedSink`（`quota_consumed.go:36`）、`LoginOptionsExt`（`login_options.go:29`）、`LoginFlowWithOptions`（`login_options.go:149`） | 能力位的可发现性由反射守卫兜底 |
| B4 | 契约测试 8 条 + 反射发现「有访问器但 ExtOf 发现不了」 | `internal/gateway/contract.go:22-47`、`:192-256`（该守卫抓到过 cline/raccoon/lobsterai/qoder 四上游缺「＋添加账号」按钮的真实事故） | hyxapi 无对应机制 |
| B5 | 架构守卫：包级依赖图 + 上游包自动发现（12 个） | `internal/gateway/arch_test.go` 10 个 Test（1,072 行，`go test -v` 实测 125 PASS / 0 FAIL）；`corePackages :29-38`、`discoveryExemptPackages :66-69`、发现结果 `:206` | 新增上游「核心零改动」是可执行断言，不是约定 |
| B6 | 10 个能力位（CapChat…CapImport） | `internal/gateway/capability.go:21-55` | 声明即承诺，契约测试会抓 |
| B7 | 滑动窗口限速 `internal/authlimit` | `PLAN-sql-unify.md:31` | hyxapi 是固定窗口 |
| B8 | 剪贴板 `execCommand` 兜底（http 明文站可用） | `internal/server/webui.html:11720-11736`（copyText）、`:7471-7486`（登录页复制链接），失败显式 toast | hyxapi 是 https 语境，无此兜底需求 |
| B9 | 骨架屏 | `internal/server/webui.html:9027`（panelSkeleton） | 旧调研文档称「无骨架屏」为错（附录 B C6） |
| B10 | 会话粘性 + Redis 异步镜像 + Noop 降级 | `internal/session/session.go:20/34/58/103/196`；`cmd/server/main.go:100-102` | 无 Redis 也能跑 |
| B11 | 单文件前端零构建 + 共享绘制层守卫 | `internal/server/webui.html`（16,101 行）；`dash-chart.js` 共享层守卫（git log `ce8ad91`/`2448d9d`） | 无 node 构建链也能交付 |
| B12 | 多进程安全的 SQLite DSN 补全（PRAGMA 只补缺不覆盖） | `internal/store/db.go:273-290` | 运维自定义 DSN 与默认值共存 |

---

## 3. 目标形态与架构决策

### D1 数据层：SQLite + GORM 成为目标权威，JSON 降级为导出/备份格式

- **决策**：沿用 `PLAN-sql-unify.md` 的 D1/D2/D3（glebarez/sqlite 纯 Go 驱动 + GORM + AutoMigrate；JSON 不废弃但降级）。**新增约束**：接线必须按「用户/会话 → 日志双写 → 渠道/模型」的顺序垂直推进，每一步结束时系统仍可运行。
- **理由**：12 张表结构已写好（`internal/store/db.go:256-271`）且 `go build` 通过；驱动选纯 Go 是为保住「单二进制 + 零依赖」部署优势（`internal/store/db.go:11` 原文；`PLAN-sql-unify.md:65-72`）。
- **被否决的备选**：① `database/sql` + 手写 DDL（`PLAN-sql-unify.md:78` 的备选）—— 已被工作区实现事实否决；② 继续纯 JSON —— 系统状态页的三个聚合数（今日请求/Token/消费、近 5 分钟 RPM/成功率/平均延迟）在 jsonl 上只能全量扫盘（`PLAN-sql-unify.md:53` G2）。
- **约束的任务**：T04/T05/T06/T07/T08/T09/T10/T11。

### D2 前端形态：保持单文件无构建；三页面各自独立文件

- **决策**：不引入构建链。三个新/改页面各自独立文件，`webui-status.html` 已是既有成果（未接线），`webui-channels.html` 就地改造，`webui-models.html` 新建；`webui.html` 只做「入口与 iframe 挂载」的最小改动，沿用 `webui.html:3068-3071` 已论证过的 iframe 做法。
- **理由**：`internal/server/webui.html` 是 `go:embed` 的单文件（16,101 行），引入 React/Rsbuild 会同时破坏「单二进制」与「零构建」两条既有判据（`PLAN-sql-unify.md:221`）。
- **被否决的备选**：把 hyxapi 的 `web/dist` 产物内联进单文件 —— 否决理由三条：产物体积与 924.7KB 现状叠加后失控；AGPL 前端产物归属义务（D6）；本仓无 bun/node 构建链的 CI 入口。
- **约束的任务**：T12/T13/T14/T15，以及 §5 的「webui.html 单写者」纪律。

### D3 系统状态页数据源：本仓自采，不复刻 hyxapi 的进程级单例

- **决策**：字段集以 hyxapi 为准（§0.5），数据源换成本仓 `internal/sysmon`（CPU/内存/磁盘）+ 进程 runtime + `store` 的 `logs` 表 SQL 聚合；database / log_database / redis / disk_cache / channel_affinity 五组按本仓实际能力裁剪并在 UI 标注「本仓不适用」。
- **理由**：hyxapi 的 `controller/system_status.go:3-14` import common/middleware/model/service，数据源全是进程级单例（DB 连接池、runtime.MemStats、atomic 计数器、HybridCache）⇒ 逻辑可复刻、包不可直接搬。
- **被否决的备选**：把 `controller/system_status.go` 整体移植 —— 会连带引入 gin 中间件与 model 单例（违反 A6/A10）。
- **约束的任务**：T06（logs 聚合）、T12（页面）、T15（验收）。

### D4 指标口径：照抄 hyxapi 定义，但与既有 P95 并存并标注

- **决策**：rpm = 窗口内请求数 ÷ 窗口分钟数；success_rate = 成功/总数（无样本保持 0）；avg_latency = AVG(use_time)（**不是 P95**，`hyxapi/model/log.go:526-554`）；今日消费 = logs 表 type=consume 的 SUM（`hyxapi/model/log.go:486-508`）。本仓 `internal/pool` 已有的 P95 保留，两处在 UI 上分区展示，禁止混用同一个「延迟」标题。
- **理由**：用户口径要「以 hyxapi 为准」（`PLAN-sql-unify.md:20` D5）；同时本仓 P95 是账号池选号依据，不能删。
- **被否决的备选**：把账号池 P95 换成 AVG —— 会削弱选号质量。
- **约束的任务**：T06/T12/T15。

### D5 权限与审计：不引入 Casbin，但引入「审计挂钩」的语义

- **决策**：权限保持本仓单账号管理员模型；把 hyxapi 的审计语义（`middleware/auth.go:83-93` + `middleware/audit.go:101-172` + `model/audit_log.go:74/134/251-253`）复刻为本仓 `internal/store/audit.go` + `internal/admin` 挂钩，落 `audit_log` 表。
- **理由**：hyxapi 的审计只对写方法（POST/PUT/PATCH/DELETE）生效并缓冲 64KiB 响应，语义简单、可照行为重写；而 Casbin 依赖 `service/authz` 13 文件/1059 行，与本仓单管理员模型不匹配。
- **被否决的备选**：引入 Casbin —— 见 A2。
- **约束的任务**：T07。**注意更正**：旧文档写的 `internal/admin/gate.go` 在两侧仓库都不存在（glob **/gate*.go = 0；本仓门禁在 `internal/admin/admin.go:742-759`，hyxapi 在 `middleware/auth.go` + `service/authz/*`）。

### D6 AGPL 策略：三类动作 + 强制归属义务

hyxapi 与 relaykit/tokenkit/web 是 **AGPL-3.0**，本仓是 **MIT**。三类动作定义如下，§4 每个任务必须标注其一：

| 分类 | 含义 | 允许范围 |
|---|---|---|
| **require** | 直接依赖 hyxapi 侧模块（require 进 go.mod / import 其包） | 仅限 `hyxapi/relaykit`、`hyxapi/tokenkit` 这类可独立编译的 module，且**必须**先评估：本仓（MIT）并入 AGPL 代码后，分发物整体须以 AGPL-3.0 提供。本计划**不建议**任何 require（见 R1） |
| **照行为重写** | 只读其源码理解行为，在本仓用本仓的技术栈重新实现（clean-room 式：不看其函数体逐行翻译，只对齐输入输出与边界） | 系统状态页字段与阈值、审计挂钩语义、渠道列表分页与筛选行为、模型/供应商两级目录行为 |
| **只借判断** | 只采纳其结论/判据（如「窗口 5 分钟」「分页上限 100」），不复制代码与文案 | 阈值数值、window_seconds:300、上限 100、状态码分档 |

**强制归属义务（AGPLv3 §7）**：`hyxapi/NOTICE:9-25` 要求修改版在 UI 醒目处保留 `Frontend design and development by New API contributors.` 与指向 `https://github.com/QuantumNous/new-api` 的可见链接，并按 §7(c) 标注改动。**只要三页面的视觉/结构取自 hyxapi，本仓就必须在页脚加这条归属与链接**，这是任务 T12/T13/T14 的硬验收项（写进 §4 各任务的验收命令（**本机 Windows 没有 `grep`**，Get-Command grep 为空、只有 findstr，故用跨平台等价写法）：`(Select-String -Path <页面>.html -Pattern 'New API contributors' | Measure-Object).Count` 必须 ≥1）。

### D7 字节透传契约不得被 DTO 化改造侵蚀

- **决策**：`internal/gateway` 的 `Provider.Chat(ctx, cred, body []byte) (ChatStream, error)`（`provider.go:41-62`）保持 `[]byte` 透传；hyxapi 的 DTO 化中间层（`relaykit/dto` 26 文件/5465 行、167 导出）**不引入**。需要参数级能力（如渠道级 override）时，只允许新增旁路扩展点，不得改 `Chat` 签名。
- **理由**：现契约是「上游原始请求体直通」的判据，DTO 化会让 13 个上游的字节级行为一次性暴露在重构风险中；且 `relaykit` 是独立 module 且 AGPL。
- **被否决的备选**：require `relaykit` 做协议转换 —— 触发 D6 的许可证后果 + `relaykit/relayconvert/boundary_test.go:44-82` 的边界断言（kit 不得 import 宿主与 gin）说明它是为 hyxapi 宿主设计的。
- **约束的任务**：全部任务（作为「不可触碰区」，进 §6 冻结清单）。

### D8 Go 版本门槛：以本仓 1.22.5 为准，hyxapi 的 1.26 语法不进入

- **决策**：新增代码必须通过 CI 的 go1.22 门禁；本机 go1.27 只用于本地验证，提交前按 1.22 规则 gofmt（历史返工：`a012f4b`、`2b87352`）。
- **理由**：`.github/workflows/ci.yml:30` 固定 `go-version: 1.22`，且 CI 有 `gofmt -l` 门禁（`:46-56`）。
- **被否决的备选**：随 hyxapi 升到 1.26 —— 会同时破坏 CI 与本机 gofmt 判据。
- **约束的任务**：全部任务。

---

## 4. 任务分解（垂直切片）

字段固定：**ID / 目标 / 产出文件 / 前置依赖 / 工时（估算）/ 验收命令（可执行）/ 文件冲突面 / 可否并行 / AGPL 动作分类**。

工时规则：S=2h、M=6h、L=16h、XL=32h（估算值，非实测；实测工时在每轮 progress-log 回填）。

### Wave 0 — 让验收命令可用

#### T00 修复前端静态套件 runner（`tests/frontend/run_frontend_suites.js` 被截断）
- 目标：让前端静态套件入口重新可运行 —— 该文件在**第 263 行被截断**（末行 `if (odd) {`），`node` 报 `SyntaxError: Unexpected end of input` 并 exit=1；该文件本身**干净且已提交**（最后提交 `495a2fda`），不是工作区脏改。
- 产出文件：`workbuddy2api/tests/frontend/run_frontend_suites.js`（补全该文件的**文件尾部**：`for` 循环剩余体 + 闭合花括号 + 进程退出码汇总逻辑；只修这一处，不改其它逻辑、不重构 runner）。
- 前置依赖：无（与 T01/T02/T03 并行）。**它是 T12/T13/T14/T15 与 §6.6 的前置**：在该文件不能 exit=0 之前，一切以它为判据的「套件全绿」都是假的。
- 工时：S。
- 验收命令：`node --check tests/frontend/run_frontend_suites.js && node tests/frontend/run_frontend_suites.js`（期望：`--check` 无输出、runner exit=0；runner 内部有起实例的项，先执行 §8.3 的前置构建 `go build -o bin/wb2api-server-probe.exe ./cmd/server`）。
- 文件冲突面：`tests/frontend/run_frontend_suites.js`（独占；T15 只允许追加登记项，不得与其并发）。
- 可否并行：可（与 T01/T02/T03 并行）。
- AGPL 动作分类：不适用（本仓自有文件）。

#### T01 修复 `internal/store` 测试编译错误
- 目标：让 `internal/store` 包可被 `go test` 装载，消除 `[setup failed]`。
- 产出文件：`workbuddy2api/internal/store/option_test.go`（`:237` missing comma in argument list）。
- 前置依赖：无。
- 工时：S。
- 验收命令：`go vet ./internal/store/... && go test ./internal/store/ -count=1`（期望：vet exit=0、test 无 `[setup failed]`）。
- 文件冲突面：`internal/store/option_test.go`（仅此文件）。
- 可否并行：可（与 T02/T03 并行）。
- AGPL 动作分类：不适用（本仓自有文件）。

#### T02 修复 `cmd/migrate-sql` 测试编译错误
- 目标：让迁移工具的测试可编译。
- 产出文件：`workbuddy2api/cmd/migrate-sql/main_test.go`（`:65` string literal not terminated）。
- 前置依赖：无。
- 工时：S。
- 验收命令：`go vet ./cmd/migrate-sql/... && go test ./cmd/migrate-sql/ -count=1`（期望：vet exit=0、test 装载成功）。
- 文件冲突面：`cmd/migrate-sql/main_test.go`（仅此文件）。
- 可否并行：可。
- AGPL 动作分类：不适用。

#### T03 让新增包通过上游覆盖守卫
- 目标：`TestAllUpstreamsAreCovered` 由 FAIL 转 PASS。
- 产出文件：`workbuddy2api/cmd/server/account_columns_invariant_test.go`（`:281` 的 `allUpstreams()` 判据，按 `internal/store`、`internal/sysmon` 的真实定位归类：二者不是上游包）。
- 前置依赖：无（但要先确认 T01/T02 不在同一包，无文件重叠）。
- 工时：S。
- 验收命令：`go test ./cmd/server/ -run TestAllUpstreamsAreCovered -count=1`（期望：ok）。
- 文件冲突面：`cmd/server/account_columns_invariant_test.go`。
- 可否并行：可。
- AGPL 动作分类：不适用。

> **Wave 0 出口判据（Checkpoint 0）**：① T00 的 `node tests/frontend/run_frontend_suites.js` exit=0；② `go test ./... -count=1` 的四类失败中，① 与 ② 消失；`internal/codearts` 的三个 `*Live` 401 是已知环境红（不计入）。**Wave 0 未过之前，Wave 2 的任何「套件全绿」结论都不成立。**

### Wave 1 — 数据层接线（SQL 成为权威，JSON 降级）

#### T04 `users` + `user_sessions` 接线（`account.Store` 换 SQL 后端）
- 目标：`internal/account` 的公开方法签名一个不动，内部后端由 JSON 换为 `internal/store`；首次启动自动导入 `data/accounts.json`（只导入不删除）。
- 产出文件：`workbuddy2api/internal/store/user.go`（新建）、`workbuddy2api/internal/account/store.go`（改内部实现）；引用点 `cmd/server/main.go:1411`、`:1415`。
- 前置依赖：T01、T11 的库骨架（`store.Open/Migrate` 已存在，无需等 T11 注入）。
- 工时：L。
- 验收命令：`go test ./internal/account/... ./internal/store/... -count=1`（期望：ok）；二元行为判据：删掉 `data/gw.db` 后重启，`users` 行数与 `data/accounts.json` 元素数相等。
- 文件冲突面：`internal/account/store.go`（独占）、`internal/store/user.go`（新建，独占）；**不得碰** `internal/store/db.go`。
- 可否并行：可与 T05/T06/T07 并行（文件不重叠）。
- AGPL 动作分类：只借判断（setups/users 行为参考 hyxapi `model/setup.go:22-92` 的结论）。

#### T05 `options` 表接线 + `settings_store.go` 真事故修复
- 目标：`config.json` 的可热改键落 `options` 表；修掉「只改 `checkin_enabled` 会把 `upstream.timeout_seconds` / `pool.max_in_flight` / `cooldown.soft_rate` / `session_sticky` 全部清零」的事故，并区分「热生效 / 需重启」。
- **事实修正（2026-10-26 施工后回写）**：上面那句「真事故」在**基线里已经修好**了 —— `parse` 已改为以 `currentSettings()` 为基底、再用补丁键覆盖（`cmd/server/settings_store.go:252-269`），不再是把 patch 解成一个零值结构体。T05 实际补的是 **`options` 表接线**（写路径 + 首次导入 + 镜像）与**两层新守卫**（文件级 / options 级），本任务块里的事故描述到此仅作历史背景。
- 产出文件：`workbuddy2api/cmd/server/settings_store.go`（事故点 `:216-248`、热生效分区 `:173-183`）、`internal/store/option.go`（补接线与首次导入）。
- 前置依赖：T01、T11 的库骨架。
- 工时：M。
- 验收命令：`go test ./cmd/server/ -run Settings -count=1`（期望：ok）；二元行为判据：PATCH 单个键后，其余键的读回值逐个不变（用现有设置接口手工走一遍并记录前后 JSON）。
- 文件冲突面：`cmd/server/settings_store.go`、`internal/store/option.go`。
- 可否并行：可与 T04/T06/T07 并行。
- AGPL 动作分类：只借判断。

#### T06 `logs` 表双写 + 三个聚合查询
- 目标：请求日志在写 jsonl 的同时异步写 `logs` 表（SQL 写失败只记日志、不影响主流程），并实现今日请求数/Token/消费与近 5 分钟 RPM/成功率/AVG 延迟的 SQL 聚合。
- 产出文件：`workbuddy2api/internal/store/log.go`（已有，补聚合 API）、`workbuddy2api/internal/logbuf/`（加双写钩子）。
- 前置依赖：T11 的库骨架；与 T04/T05 无文件重叠。
- 工时：L。
- 验收命令（**2026-10-26 施工后修正**）：`go test ./internal/store/ -run TestLog -count=1`（期望：ok）；**真实的 jsonl ↔ SQL 对照**用 `go test ./internal/logbuf/ -run TestJSONLAndSQLAggregatesAgree -count=1`（同一条目分别写 jsonl 与 `logs` 表，三个聚合数 ±0；实现见 `internal/logbuf/sqllog_equivalence_test.go`）。
  - ⚠ 本文原写的 `node tests/frontend/run_http_suites.js --quick` **是错的**：按源码 `--quick` 只跑 `--self-test`（`tests/frontend/run_http_suites.js:109`、`:142`），**不起实例、不打流量**，因此不可能产出用于对照的日志。
  - HTTP 层的端到端对照（起实例打流量 → 比 `data/request-log.jsonl` 与 SQL 聚合）需等 T11 注入（已完成）+ T12 状态页接线之后再立命令；本轮**未做**，不得写成已验证。
- 文件冲突面：`internal/store/log.go`、`internal/logbuf/*`；**不得碰** `internal/server/webui.html`。
- 可否并行：可与 T04/T05/T07 并行；**必须在 T12 之前完成**。
- AGPL 动作分类：只借判断（指标口径来自 hyxapi 结论：`model/log.go:486-508`、`:526-554`）。

#### T07 `audit_log` 接线（承接 PLAN-sql-unify T5）
- 目标：写操作（POST/PUT/PATCH/DELETE）落一条审计：actor/action/route/status/success。
- 产出文件：`workbuddy2api/internal/store/audit.go`（新建）、`workbuddy2api/internal/admin/admin.go`（挂钩，参考 `:742-759` 的门禁位置）。
- 前置依赖：T11 的库骨架。
- 工时：M。
- 验收命令（两条都必须 exit=0，**不得用 `;` 串联**：verifier-06 实测 `cmd1 ; cmd2` 会只回显后一条的 EXIT=0 而掩盖前一条失败）：
  - `go test ./internal/store/ -run TestAudit -count=1`
  - `go test ./internal/admin/ -count=1`
- 二元行为判据：改一次渠道 `base_url` 后 `audit_log` 行数 +1；且 `-run TestAudit` 必须命中 >0 条测试。
- 文件冲突面：`internal/store/audit.go`（新建）、`internal/admin/admin.go`。
- 可否并行：可与 T04/T05/T06 并行。
- AGPL 动作分类：照行为重写（审计挂钩语义来源 `hyxapi/middleware/audit.go:101-172`，本仓挂钩点是自己代码）。

#### T08 `channels` + `abilities` 表反向 seed
- 目标：启动时按 `cmd/server/main.go` 的 13 处 `registry.Register` 反向 seed 渠道行，并展开「模型↔渠道」绑定；`/ui/channels` 页保持可用。**seed 时不得按 provider ID 去重**：`main.go:275` 的 `wb` 与 `:347` 的 `wbIntl` 是同一个上游（WorkBuddy）的两个渠道实例（`:347` 注释原文「海外版 WorkBuddy AI（workbuddy-intl 渠道实例）」），按 ID 去重会只建 1 行。
- 产出文件：`workbuddy2api/internal/store/channel.go`（新建）、`workbuddy2api/internal/admin/channels.go`。
- 前置依赖：T11 的库骨架。
- 工时：M。
- 验收命令：`go test ./internal/admin/ -run TestChannelSeed -count=1`（期望：ok，且 `-run` **必须命中 >0 条测试** —— 当前该 `-run` 输出 `[no tests to run]` 且 exit=0，属**空过**，不算通过）；计数判据：`channels` 行数 = 13（**不得按 provider ID 去重**，见目标栏的 `wb`/`wbIntl` 双实例；例外：`internal/openaicompat` 是第 12 个上游包但不进 registry，作为渠道工厂注入 `main.go:1535-1537` 与 `:1666-1668`，seed 时按「工厂」而非「注册点」处理）。
- 文件冲突面：`internal/store/channel.go`（新建）、`internal/admin/channels.go`。
- 可否并行：可与 T04–T07 并行。
- AGPL 动作分类：只借判断（表结构与 abilities 展开思路来自 hyxapi，实现自写）。

#### T09 `models` + `vendors` 表 seed
- 目标：从各上游模型目录 seed 两级目录；提供「渠道用到但目录没有」的合成行 API。
- 产出文件：`workbuddy2api/internal/store/model.go`（新建）、`workbuddy2api/internal/server/models_channels.go`。
- 前置依赖：T11 的库骨架。
- 工时：M。
- 验收命令：`go test ./internal/server/ -run TestStoreModelCatalogSeed -count=1`（期望：ok，且 `-run` 必须命中 >0 条；**注意**：既有的 `-run TestModelCatalog` 命中的是 `internal/server/models_catalog_wiring_test.go:54` 那个既有测试，不是本任务要新建的目录 API 测试，故本任务的新测试命名为 `TestStoreModelCatalogSeed` 并用精确名运行）；计数判据：`models` 行数 = 当前 `/v1/models` 可见 chips 基数（基线口径见 `PLAN-newapi-alignment.md:119` 的 701 条，须以本仓实测为准并在提交说明里记录实际数）。
- 文件冲突面：`internal/store/model.go`（新建）、`internal/server/models_channels.go`。
- 可否并行：可与 T04–T08 并行。
- AGPL 动作分类：只借判断。

#### T10 真实数据迁移与字节等价校验（承接 PLAN-sql-unify T12 的「真跑」）
- 目标：对真实 `data/` 跑 import → verify → export，并把「导出与迁移前逐字节等价」变成可复现命令。
- 产出文件：`workbuddy2api/cmd/migrate-sql/main.go`（仅在需要时补排序/键序缺陷）。
- 前置依赖：T01、T02、T04、T05。
- **执行纪律（V4）**：本任务会写真实 `data/`，**禁止在常规开发轮次直接执行**；必须同时满足三条：① 已存在非空的 `data.bak-<ts>/` 备份；② 处于**专用窗口**（服务端已停、无其它进程持有 `data/`）；③ 先跑一次 `--dry-run`（`import` 支持该开关，`cmd/migrate-sql/main.go:65`）确认无写动作，再真跑。
- 工时：M。
- 验收命令（三步，全部可执行）：`go run ./cmd/migrate-sql import --data data --db data/gw.db`（exit=0）→ `go run ./cmd/migrate-sql verify --data data --db data/gw.db`（exit=0）→ `go run ./cmd/migrate-sql export --data data --db data/gw.db --out data.export-check` 后与迁移前的备份逐文件比较。**退出码语义**：0 成功 / 1 执行失败 / 2 用法错误 / 3 校验不一致（`cmd/migrate-sql/main.go:74`）。
- 文件冲突面：`cmd/migrate-sql/main.go`（与 T02 同目录但不同文件）；**迁移前必须先把 `data/` 备份到 `data.bak-<ts>/`**。
- 可否并行：**不可**，必须串行在 T04/T05 之后、T11 收口之前。
- AGPL 动作分类：不适用（迁移工具为本仓代码）。

#### T11 `store` 注入 `cmd/server` + 首启自愈（Wave 1 收口）
- 目标：服务端真正打开 `data/gw.db`，空 data/ 首次启动生成库与 `setups` 一行，二次启动不重复 seed，双进程并发只有一个成功。
- 产出文件：`workbuddy2api/cmd/server/main.go`（注入点；这是唯一允许改 `main.go` 的任务）。
- 前置依赖：T04/T05/T06/T07/T08/T09（这些任务先把包写好，本任务只做注入）。
- 工时：M。
- 验收命令：`go build ./... && go test ./internal/store/ -run TestSetup -count=1`（期望：ok）；三元行为判据：① 空 `data/` 首启后 `data/gw.db` 存在且 `setups` 有 1 行；② 二次启动不新增行；③ 两个进程并发初始化只有一个成功（`internal/store/setup_multiprocess_test.go` 已覆盖 DDL 竞争）。**2026-10-26 施工后澄清（主脑 D2 裁决）**：① 的语义是「首启**并完成建号**后 `setups` 恰好 1 行」—— 裸启动空库时它是 **0**（实测见 §0.6），而强行在裸启动时写 1 会把 setup 页关死（谁都建不了第一个管理员），所以那条路被否决；`setups` 行现在由建号路径显式写下（`internal/account/store.go` 调 `store.MarkSetupInitialized()`），库里 `role` 仍是 10（不回写成 100）。
- 文件冲突面：`cmd/server/main.go`（**独占**，T12–T14 不得改它）。
- 可否并行：**不可**，串行。
- AGPL 动作分类：只借判断（CheckSetup 三态语义来源 `hyxapi/model/main.go:57-82`）。

> **Checkpoint 1**：`go build ./...` exit=0；`data/gw.db` 存在且 `setups` 一行；**现有功能零回归**（`go test ./... -count=1` 的失败集合不超出 §0.4 的已知四类）。

### Wave 2 — 三页面对齐（以 hyxapi 为准）

#### T12 系统状态页挂载与对齐
- 目标：把 `internal/server/webui-status.html` 接进控制台（iframe 挂载），字段与阈值按 §0.5 对齐；数据源用 `internal/sysmon` + T06 的三个聚合；页脚加 AGPL 归属。
- 产出文件：`workbuddy2api/internal/server/webui-status.html`（改）、`workbuddy2api/internal/sysmon/`（接线，当前零引用）、`workbuddy2api/internal/server/webui.html`（**仅**加入口与 iframe 挂载，行数改动上限 50 行）。
- 前置依赖：T06（聚合）、T11（库与注入）。
- 工时：L。
- 验收命令：`(Select-String -Path internal/server/webui-status.html -Pattern 'New API contributors' | Measure-Object).Count`（期望 ≥1）与 `node tests/frontend/run_frontend_suites.js`（期望 exit=0，**依赖 T00 先绿**）。视觉判据：先跑 `go build -o bin/wb2api-server-probe.exe ./cmd/server`（消除「构建过期」门禁），再 `node tests/frontend/run_e2e.js http://127.0.0.1:18080/ui`，该面板相关套件通过，并在 1920×1080 与 375×812 各留一张截图。
- 文件冲突面：`internal/server/webui.html`（**与 T13/T14 冲突，见 §5**）、`internal/server/webui-status.html`（独占）、`internal/sysmon/*`。
- 可否并行：与 T13/T14 **不可并行**（同碰 `webui.html`），三者必须串行。
- AGPL 动作分类：照行为重写 + 只借判断（阈值 75/90 与 window_seconds:300 属只借判断）。

#### T13 渠道管理页对齐
- 目标：按 `hyxapi/web/src/pages/Channel/` 的列表行为对齐本仓渠道页：状态筛选字典、type_counts 同响应、分页上限 100、内存搜索+分页；密钥等高敏操作只加二次确认 UI。
- 产出文件：`workbuddy2api/internal/server/webui-channels.html`（改）、`workbuddy2api/internal/admin/channels.go`（分页与筛选 API，与 T08 同目录不同函数——**必须与 T08 串行**）。
- 前置依赖：T08（先完成 seed 与 API 骨架）。
- 工时：L。
- 验收命令：`node tests/frontend/run_http_suites.js`（渠道相关探针为 `tests/frontend/verify_channels_save.js`；**该 runner 当前 exit=1 / 8 次运行中 6 条失败，见 §8.3** —— 本任务的判据是「先把该条探针所属子集修到全绿」，不是「它天生全绿」）与 `node tests/frontend/run_frontend_suites.js`（期望 exit=0，**依赖 T00 先绿**）。
- 文件冲突面：`internal/server/webui-channels.html`、`internal/admin/channels.go`、`internal/server/webui.html`（入口）。
- 可否并行：与 T12/T14 不可并行（`webui.html`）。
- AGPL 动作分类：照行为重写（列表行为）+ 只借判断（上限 100、状态常量）。

#### T14 模型管理页（新建）
- 目标：Model / Vendor 两个 tab；渠道用到但目录没有的模型显示为合成行且不可勾选；不做定价。
- 产出文件：`workbuddy2api/internal/server/webui-models.html`（新建）、`workbuddy2api/internal/server/webui.html`（入口）、`internal/server/models_channels.go`（与 T09 串行）。
- 前置依赖：T09（先完成目录 API）、T11。
- 工时：L。
- 验收命令：`node tests/frontend/verify_models_grouped.js`（期望通过）与 `node tests/frontend/run_frontend_suites.js`（期望 exit=0，**依赖 T00 先绿**）。
- 文件冲突面：`internal/server/webui-models.html`（独占）、`internal/server/webui.html`、`internal/server/models_channels.go`。
- 可否并行：与 T12/T13 不可并行。
- AGPL 动作分类：照行为重写 + 只借判断。

#### T15 三页面统一验收（独立 verifier 执行）
- 目标：把三页面的验收固化为可重复命令，并做反例验证（故意破坏一处，确认对应套件真的会红）。
- 产出文件：`workbuddy2api/tests/frontend/`（仅新增/更新与三页面相关的套件与 `run_frontend_suites.js` 登记项）。
- 前置依赖：T12/T13/T14。
- 工时：M。
- 验收命令（**前置：T00 先绿 + 先跑 `go build -o bin/wb2api-server-probe.exe ./cmd/server`**）：`node tests/frontend/run_frontend_suites.js && node tests/frontend/run_http_suites.js && node tests/frontend/run_e2e_family3.js --repeat=2`（期望：三条 exit=0，且 `--repeat=2` 的 `REDS=` 行逐字相同）。`node tests/frontend/mutation_sweep.js static` 属**守卫有效性验证**（会变异源码再回滚），**不计入只读验收**，必须在专门授权窗口内执行。
- 文件冲突面：`tests/frontend/`（与 Wave 1 无重叠）。
- 可否并行：不可（收口任务）。**必须由未参与 T12–T14 的 verifier 执行**。
- AGPL 动作分类：只借判断。

### Wave 3 — 收尾

#### T16 文档落账 + 契约守卫化
- 目标：把 §6 的契约变成可执行断言（防回归），并补账本：`CHANGELOG.md` 新条目、`docs/progress-log.md` 补 2026-10-22→10-26 的缺口（该文件停在 2026-10-21）、新建 `docs/open-questions.md` 承接 §10。
- 产出文件：`workbuddy2api/CHANGELOG.md`、`workbuddy2api/docs/progress-log.md`、`workbuddy2api/docs/open-questions.md`（新建）、`workbuddy2api/internal/gateway/*_test.go` 或 `cmd/server/*_test.go`（契约守卫）。
- 前置依赖：全部。
- 工时：M。
- 验收命令：`go test ./cmd/server/ ./internal/gateway/ -count=1`（期望：ok）+ `(Select-String -Path docs/progress-log.md -Pattern '2026-10-2' | Measure-Object).Count`（期望 ≥1，证明 10-22→10-26 已入账；本机无 `grep`，故用 PowerShell 等价写法）。**注意**：`.gitignore:76` 是 `*.md`，这三个 md 默认不进版本库，落账后必须按 §10 Q10 的裁决确认入库策略。
- 文件冲突面：`CHANGELOG.md`、`docs/progress-log.md`、`docs/open-questions.md`。
- 可否并行：不可（收口）。
- AGPL 动作分类：不适用。

---

## 5. 并行策略与文件隔离

### 5.1 写范围表

| 任务 | 写范围（仓根相对路径） |
|---|---|
| T00 | `tests/frontend/run_frontend_suites.js` |
| T01 | `internal/store/option_test.go` |
| T02 | `cmd/migrate-sql/main_test.go` |
| T03 | `cmd/server/account_columns_invariant_test.go` |
| T04 | `internal/store/user.go`、`internal/account/store.go` |
| T05 | `internal/store/option.go`、`cmd/server/settings_store.go` |
| T06 | `internal/store/log.go`、`internal/logbuf/` |
| T07 | `internal/store/audit.go`、`internal/admin/admin.go` |
| T08 | `internal/store/channel.go`、`internal/admin/channels.go` |
| T09 | `internal/store/model.go`、`internal/server/models_channels.go` |
| T10 | `cmd/migrate-sql/main.go` |
| T11 | `cmd/server/main.go` |
| T12 | `internal/server/webui-status.html`、`internal/sysmon/`、`internal/server/webui.html` |
| T13 | `internal/server/webui-channels.html`、`internal/admin/channels.go`、`internal/server/webui.html` |
| T14 | `internal/server/webui-models.html`、`internal/server/models_channels.go`、`internal/server/webui.html` |
| T15 | `tests/frontend/` |
| T16 | `CHANGELOG.md`、`docs/progress-log.md`、`docs/open-questions.md`、`internal/gateway/*_test.go` |

### 5.2 冲突文件清单（必须显式排班）

| 冲突文件 | 争用任务 | 处置（强制） |
|---|---|---|
| `internal/server/webui.html`（16,101 行 / 924.7KB，单文件） | T12、T13、T14 | **同一时间只允许一个写者**。三任务按 T12→T13→T14 串行；每个写者只允许改「入口 + iframe 挂载」区块（改动上限 50 行/次），改完立即提交并让下一个写者基于新版本开工。**禁止**任何任务重构 `webui.html` 的既有绘制层与 `COLS` 定义（`const COLS` 仅 1 处：`webui.html:4889`，只含 2 个字段 travel:8 / growth:6，`:4890-4891`；`COLS.` 引用 10 处）。 |
| `cmd/server/settings_store.go` | T05（唯一） | 其余任务只读不写；T05 必须同时修 `:216-248` 的整键清零事故与 `:173-183` 的热生效分区。 |
| `cmd/server/main.go` | T11（唯一） | 其余任务只读。T11 之外任何任务都不得触碰（13 处 `registry.Register` 与注入点 `:1535-1537`/`:1666-1668` 属冻结区）。 |
| `internal/admin/channels.go` | T08、T13 | **串行**：T08 先完成 seed 与 API 骨架，T13 再改分页/筛选。 |
| `internal/server/models_channels.go` | T09、T14 | **串行**：T09 先完成目录 API，T14 再接 UI。 |
| `internal/store/db.go` | 无任务 | **冻结**：12 表定义（`:256-271`）与 `Open/Migrate/Close` 签名不得改动；如需加表必须新开任务并更新 `allModels()`。 |
| `internal/gateway/provider.go` | 无任务 | **冻结**（§6.1）。 |
| `internal/gateway/arch_test.go` | 无任务 | **冻结**：守卫判据不得为了让新任务通过而放宽。 |

### 5.3 必须串行的链条

```
Wave 0  T00 ‖ T01 ‖ T02 ‖ T03      （四者文件互不重叠，可并发；T00 是 Wave 2 一切「套件全绿」判据的前提）
Wave 1  T11 骨架（store.Open/Migrate 已存在）─► T04 ‖ T05 ‖ T06 ‖ T07 ‖ T08 ‖ T09
                                            └─► T10（真迁移，串行在 T04/T05 之后）
                                            └─► T11 注入 cmd/server（串行在 T04–T09 之后，唯一写 main.go）
Wave 2  T12 ─► T13 ─► T14（webui.html 单写者，严格串行）
        T15（三页面收口，独立 verifier）
Wave 3  T16（账本与守卫）
```

### 5.4 建议并发分组（并发上限 6）

| 组 | 成员 | 并发数 | 备注 |
|---|---|---|---|
| G0 | T00、T01、T02、T03 | 4 | 全部 S 工时，一轮内完成；T00 必须先于任何前端套件验收 |
| G1 | T04、T05、T06、T07、T08、T09 | 6 | 写范围互不重叠（见 5.1） |
| G2 | T10 | 1 | 真迁移，必须独占（会写 data/） |
| G3 | T11 | 1 | 独占 `cmd/server/main.go` |
| G4 | T12 | 1 | `webui.html` 单写者 |
| G5 | T13 | 1 | 等 T12 提交 |
| G6 | T14 | 1 | 等 T13 提交 |
| G7 | T15、T16 | 2 | 必须由 verifier / 非实现者执行 |

**并行纪律**：`webui.html` 与 `cmd/server/main.go` 是仅有的两个「全局单写者」文件；除此之外的并行上限是 6。任何一轮在派发前必须先确认写范围不重叠（5.1），重叠即改串行。

---

## 6. 需要冻结的契约

### 6.0 数据层真相裁定（本节是全文最重要的一条裁定）

| 问题 | 裁定 | 证据 |
|---|---|---|
| 当前谁是权威？ | **JSON 是运行时唯一权威**。`data/accounts.json`、`data/sessions.json`、`data/apikeys.json`、`<凭据根>/channels.json`、`data/state.json`、`data/request-log.jsonl` 全部是服务端真实读写路径（**`data/channels.json` 已于 2026-10-20 迁走、现不存在**） | `cmd/server/main.go:1411/1415/1287/1294-1295/1315/1167`；`internal/logbuf` |
| Redis 是什么角色？ | **粘性绑定与池状态快照的非权威镜像**，已接线；未配置或连接失败降级 `Noop`，并以 `data/sticky-bindings.json` 兜底 | `cmd/server/main.go:100-102`、`:131`；`internal/redisstore/redisstore.go:50/57/67` |
| SQL 现在是什么？ | **已实现未接线的在制品**：12 张表定义 + 迁移工具三段子命令存在且可构建，服务端零引用，`data/gw.db` 不存在 | `internal/store/db.go:256-271`；`cmd/migrate-sql/main.go:37` 是 `internal/store` 唯一的**业务** import 方（`cmd/server` 与其它包零引用；同包 `main_test.go:29` 除外）；`data/` 无 `gw.db` |
| 迁移方向？ | **单向 JSON → SQL**，且首次导入只读不删；反向能力是 `migrate-sql export`（用于备份与人工排查），导出必须与迁移前逐字节等价 | `cmd/migrate-sql/main.go:62-74`（import/verify/export 与退出码）、`:86-88`（键序即契约） |
| `state.json` 进 SQL 吗？ | **不进**（高频写运行态）；只把「号」的静态信息进 `accounts` 表 | 承接 `PLAN-sql-unify.md:253` Q3 的建议；`Account` struct 已在 `internal/store/models.go` 定义 |
| 切换点在哪？ | **T11 完成的那一刻**：服务端开始读写 `data/gw.db`。切换前必须做 `data/` 全量备份；切换后 JSON 只作为导出格式，不得再被应用写回 | 本计划 T10/T11 的验收条款 |

**2026-10-26 施工后更新**：**T11 已完成**（`ecc7f42`）—— 切换点已过。隔离实例实测（临时工作目录 + `TcpListener(0)` 空闲端口）：首启生成 `data/gw.db`（12 张表）；**首启并完成建号后** `setups` 恰好 1 行；`data/` 下**不再生成** `accounts.json` / `sessions.json`（SQL 权威、JSON 不写回）；重启后账号仍能登录（来自 SQL）。反向导出实测：`go run ./cmd/migrate-sql export` 导出的管理员 `role` 仍是 **10**（与改造前一致 ⇒ 字节等价判据未被破坏）。

### 6.1 Go 接口签名冻结

```go
// internal/gateway/provider.go:41-62 —— 不得改动
type Provider interface {
    ID() string
    Caps() Capability
    Chat(ctx context.Context, cred Credential, body []byte) (ChatStream, error)
    Models(ctx context.Context, cred Credential) ([]ModelInfo, error)
}
```

- `ChatStream` 定义 `provider.go:65-74`；`Credential` 定义 `provider.go:92-122`；扩展点发现 `ExtOf[T]` 定义 `provider.go:132-142`。
- 能力位 10 个定义 `internal/gateway/capability.go:21-55`：CapChat/CapModels/CapCheckin/CapGrowth/CapTravel/CapWelfare/CapQuotaProbe/CapTasks/CapInvite/CapImport。**只增不改**。
- `ErrorKind` 10 个定义 `internal/gateway/errorclassifier.go:99-209`：None=0 / HardCredit / SoftRate / SessionDead / NotFound / Auth / Server / Client / ContentBlocked / FatalClient=9。**铁律：只追加不插入**（`:184-189`、`:207`）。
- **跨文件配对硬约束**（AGENTS.md §7 十步清单未写，必须补进契约）：`RefreshSkew` 回正数 ⇒ 必须实现 `CredentialRefresher`；实现 `CredentialRefresher` ⇒ 必须实现 `JobExt`。守卫在 `cmd/server/refresh_pairing_test.go` 与 `cmd/server/jobs_pairing_test.go`。

### 6.2 JSON 字段与键序冻结

- `data/accounts.json` 的键序与 `omitempty` 是导出字节等价契约的一半（另一半是记录顺序）：见 `cmd/migrate-sql/main.go:86-104`，其中 `last_login_at` / `last_login_ip` 必须 `omitempty` 且与 `internal/account.User` 一致。
- 新增字段时：**先在导出结构里加，再在运行时结构里加，顺序必须一致**；否则第 N 次备份恢复才发现 diff。

### 6.3 路由冻结

- `internal/server/handler.go` 的既有路由不得重构（沿用 `PLAN-sql-unify.md:194` 的铁律）；新页面通过 iframe 挂载，沿用 `webui.html:3068-3071` 已论证的做法。
- 对外 API 面冻结：`/v1/chat/completions`、`/v1/models`、`/ui/*` 的既有行为不得改变；三页面全部挂在控制台内部路径。

### 6.4 状态页字段契约（以 hyxapi 为准的部分）

**2026-10-26 施工后改定（契约名以本仓代码为准，这是有意的偏离）**：`GET /admin/system-status` 的**实际**顶层字段是

`service` / `resource` / `health` / `deps` / `cache`

（`internal/sysmon/status.go:242-249` 的 `StatusPayload` json tag）。**不是** hyxapi 那套 `version` / `uptime` / `go_version` / `goroutines` / `active_connections` / `resources` / `runtime` / `database` / `log_database` / `redis` / `request_stats` / `request_health` / `disk_cache` / `channel_affinity`。T12 的实现与 T15 的 `tests/frontend/verify_wave2_pages.js` 都以这一套为准。

- hyxapi 侧字段（`hyxapi/controller/system_status.go:121-183` 的 `c.JSON` 字面量）保留为**语义参照**：阈值三档 75/90、窗口 `window_seconds:300`、成功率分档 <95 warning / <90 critical 照抄（见 §0.5 的四列表）。
- 本仓**没有** HybridCache 亲和缓存，也**没有**磁盘缓存层 ⇒ `channel_affinity` / `disk_cache` 属于「本仓不适用」：按 `internal/sysmon/status.go:251-253` 的约定「传入 nil 的指针 ⇒ 对应分块整块不渲染」，**显式置空并在 UI 标注「本仓不适用」，不得编造 0**（0 与「没有这一层」在页面上是两种事实）。

### 6.5 新表与 store API 契约

- `store.Open(path) error` / `store.Migrate() error` / `store.Close() error`（`internal/store/db.go:95/208/219`）；`store.DB` 是**包级句柄**（`:50`），测试必须串行并 `Close`（`cmd/migrate-sql/main_test.go:14-15`）。
- 12 张表 = `allModels()`（`:256-271`）；角色 `RoleRoot=100`（`internal/store/role.go:31`）。**新增表 = 改 `allModels()` + 新 struct + 新测试**，不得只建表不进 `allModels()`。

### 6.6 前端 DOM/ID 契约

- `tests/frontend/settings-baseline-ids.txt` 与 UI manifest 守卫是既有的可执行契约；改 `webui*.html` 后 `node tests/frontend/run_frontend_suites.js` 必须仍 exit=0（**该 runner 自身当前被截断、exit=1，必须先完成 T00**）。
- 30s 轮询的严格 digest 守卫当前 3 处（`webui.html:10414` `pollAPIKeys`、`:14436` `loadTrendSeries`、`:12305` `renderStatsReport`），放宽判据后为 5 处（再加 `pollPoolValues`、`pollStatsCards`）。新增轮询必须落入这 5 处之一，不得新增第 6 条独立轮询。

---

## 7. 风险与回滚

| # | 风险 | 触发条件 | 影响 | 回滚动作 |
|---|---|---|---|---|
| R1 | **AGPL 污染（最高风险）** | 任何把 hyxapi 侧代码（Go 或前端产物）复制/改写进本仓；或 require `relaykit`/`tokenkit` | 本仓是 MIT，并入 AGPL 代码后分发物整体须以 AGPL-3.0 提供，且必须履行 `NOTICE:9-25` 的 UI 归属义务 | ① 移植前先决定动作分类（§3 D6），禁止未分类的代码进入；② 一旦发现混入，`git revert` 该提交并重写为「照行为重写」；③ 若三页面视觉取自 hyxapi，则**必须**保留页脚归属与链接（不可回滚项，只能选择不做） |
| R2 | **go 版本门槛** | 使用 go1.26 才有的语法/标准库；或提交本机 go1.27 的 gofmt 结果 | CI `Check formatting` 或 `Build` 红（历史已发生两次：`a012f4b`、`2b87352`） | `gofmt -l .` 在 LF 归一化后必须为空；CI 红了立刻原地修，不进入下一波 |
| R3 | **单文件前端膨胀/被并发覆盖** | 三个任务同时改 `webui.html`；或把三页面写进同一个文件 | 16,101 行的单文件冲突，历史上已因「第二个写者」丢失过改动 | 严格按 §5.2：`webui.html` 单写者、每次改动上限 50 行、改完立即提交；页面各自独立文件 |
| R4 | **字节透传契约被破坏** | 有人为了参数覆盖去改 `Provider.Chat` 的 `body []byte` 为 DTO | 13 个上游的请求体构造全部受影响 | `git revert`；改走新增旁路扩展点（§3 D7） |
| R5 | **迁移不可逆点** | T11 完成（服务端开始读写 `gw.db`）之后，用户手工改 `data/*.json` 不再生效 | 用户以为改了 JSON 就改了系统 | ① 切换前 `data/` 备份为 `data.bak-<ts>/`；② `migrate-sql export` 提供反向导出；③ **README 必须明写「JSON 自本版起仅为导出格式」**（属 T16 范围） |
| R6 | **基线红掩盖新红** | 不先做 Wave 0 就开新任务，`go test ./...` 的既有失败被当成「本来就红」 | 新引入的失败被忽略 | 先完成 T01/T02/T03；此后每波结束记录失败集合，**只允许是 §0.4 的子集** |
| R7 | **本机 OOM / 并发过高** | 在本机用默认并发跑大测试（hyxapi 已实测 OOM：`VirtualAlloc errno=1455`）；或本仓并发派发超过 6 | 命令失败被误判为代码错 | 大仓测试一律 `-p 2`；并发派发上限 6（§5.4） |
| R8 | **端口与实例互相干扰** | 验收命令未指定端口，撞上正在运行的实例（hyxapi 默认 3000 已被上游 New API 占用；本仓常用 7863/7864/7868/18080/18081） | 探针红但代码无辜 | 一律用带隔离实例的 runner（`run_http_suites.js` 自带 `--start`、`run_e2e.js` 传显式 URL）；hyxapi 侧只读验证时用 `PORT=3001` |
| R9 | **`*.md` 不入库导致计划失效** | 新写的 `PLAN-hyxapi-alignment.md`、`docs/open-questions.md` 默认被 `.gitignore:76` 的 `*.md` 忽略 | 计划与开放问题不进版本库，协作者看不到 | 二选一：`git add -f <file>`，或在 `.gitignore` 加 `!PLAN-*.md`/`!docs/open-questions.md`（属 T16，需用户裁决，见 §10 Q10） |
| R10 | **测试口径不可复现** | 验收只写「跑一遍测试全绿」 | 结论无法复现（本仓无一条全量命令，见 §8.3） | 所有验收必须用 §8.3 的具名命令；`-race` 不得作为门禁 |
| R11 | **改了 `//go:embed` 的 HTML 却拿旧探针断言（假红）** | 改 `internal/server/webui*.html` 后没重建探针就重跑前端套件 | 4 条套件出现与改动无关的失败，把「页面改坏了」与「二进制旧」混成一条（T12 实测 22/33；重建后 26/33） | 改 HTML 后**先** `go build -o bin/wb2api-server-probe.exe ./cmd/server` 再跑套件；`tests/frontend/lib_instance.js:151` 的「构建过期」门禁会直接拒绝旧探针 |
| R12 | **runner 默认打「用户正在跑的那份二进制」** | `WB2API_EXE` 未设且默认值指向 `bin/wb2api-server.exe` | 探针会去起/锁用户那份二进制，甚至按旧页面断言 | T15 已把 `run_http_suites.js:65-66` 的默认改成 `bin/wb2api-server-probe.exe`（一旦指向 `wb2api-server.exe`，`:69-70` 会打印警告）；仍建议每次显式 `WB2API_EXE=bin/wb2api-server-probe.exe` |
| R13 | **`internal/store` 是共享包：一个语法错阻塞同包全部任务** | 并行写者里任一文件出现语法错/非法字节 | 同包多路任务一起 `[build failed]`（实测：`internal/store/channel.go` 混入 2 个**真实 NUL 字节**，一次挡住 3 路） | 纪律：**每个文件写完立刻** `gofmt -e <file>` + `go build ./...` 自检再往下做；共享包（`internal/store`、`cmd/server`）不并发改同一文件 |
| R14 | **文档与 `*_gen.js` 的入库陷阱** | 收口时用 `git add -A`；或以为新写的 `.md` 会自动入库 | ① `.gitignore:76` 是 `*.md` ⇒ 计划/调研文档默认**不进**版本库（本轮是用 `git add -f` 强制加入的）；② 跑过前端 runner 之后 `tests/frontend/*_gen.js` 会出现 **15 个幻影 M**（T15 实测：内容 diff 为空） | 收口时逐文件 `git add <file>`，**不得** `git add -A`；幻影 M 先 `git checkout -- tests/frontend` 清掉再提交 |
| R15 | **`-race` 在本机 Windows 不可用** | 想把 `-race` 当门禁 | 见 §8.3 基线现实第 8 条（归因来自仓库文档，**未独立联网复核**） | 继续当 advisory；要升级门禁必须先在有 gcc 的非 Windows 环境跑通并确认全绿 |

---

## 8. 验证口径

### 8.1 每任务的二元验收

§4 每个任务的「验收命令」字段就是二元判据：命令 exit=0（或输出指定计数）为通过，否则不通过。**不接受**「人工看过没问题」作为通过证据。

### 8.2 独立 verifier 安排

| 验收对象 | 实现者 | 必须由谁验 | 验什么 |
|---|---|---|---|
| T04–T09（数据层） | 各任务写者 | **未参与该任务的 verifier** | 复跑 `go test ./internal/... -count=1`；反例：手工改一行 `data/accounts.json` 后重启，确认 SQL 侧行为符合裁定（§6.0） |
| T10（真迁移） | 迁移写者 | 独立 verifier | 复跑 import/verify/export；**独立**对 `data.bak-<ts>/` 与导出目录做逐文件字节比较 |
| T12–T14（三页面） | 页面写者 | **未参与 T12–T14 的 verifier** | 复跑 T15 的三条只读命令（前置：T00 已绿 + 探针二进制已重建）；反例：故意破坏一处（如把状态页阈值常量改错）确认对应套件变红 —— 反例验证用 `tests/frontend/mutation_sweep.js`（会变异源码再回滚，必须在专门授权窗口内执行，不得当只读验收随手跑） |
| T16（账本） | 文档写者 | 主脑 | 检查 §6 契约已成断言、账本缺口已补 |

### 8.3 真实的多条命令清单（本仓没有「一条命令跑全量」）

```bash
# 前置动作（③④⑤ 都依赖探针二进制；过期会被 runner 直接拒绝）
go build -o bin/wb2api-server-probe.exe ./cmd/server

# ① Go 全量（461 个 _test.go；当前 exit=1，基线四类失败见 §0.4）
go build ./... && go vet ./... && go test ./... -count=1

# ② 前端静态（18 套件，无需浏览器；当前 exit=1 —— runner 自身被截断，见 T00）
node tests/frontend/run_frontend_suites.js

# ③ 浏览器 E2E（24 条；**条数未复核**：tests/frontend/README.md:13 记为 14 个浏览器 E2E，与本文 24 冲突，实施前须以 runner 源码为准；需运行中的实例 18080 + Chrome，不传可用实例则整片 FAIL）
node tests/frontend/run_e2e.js http://127.0.0.1:18080/ui

# ④ HTTP 层探针（自带 --start 起隔离实例；当前 exit=1 / 8 次运行中 6 条失败）
node tests/frontend/run_http_suites.js

# ⑤ 第三族 25 条（**条数未复核**：tests/frontend/README.md:13 记为 14 个浏览器 E2E，与本文 25 冲突，实施前须以 runner 源码为准）+ 可复现性比对（当前 exit=1：被「构建过期」门禁拦下）
node tests/frontend/run_e2e_family3.js --repeat=2

# ⑥ Wave 2 三页面契约（T15 新增；自带隔离实例 + 空闲端口现场探测 + 临时目录 + Chrome；**不需要 --start**，也绝不碰 7863）
node tests/frontend/verify_wave2_pages.js

# 附加（非只读：会变异源码再回滚，需专门授权窗口；真身在 tests/frontend/，仓根没有该文件）
node tests/frontend/mutation_sweep.js static
```

**基线现实（九条；前端部分是 verifier-06 的独立验收实测输出，必须写进任何验收报告；第 9 条为 2026-10-26 施工后新增）**：

1. **① `go test ./... -count=1` 当前 exit=1**：四类失败见 §0.4；其中 ① ② 由 Wave 0 修掉，③（`internal/codearts` 三个 `*Live` 401）是凭证到期的环境红，④ 是正常。
2. **② `run_frontend_suites.js` 当前 exit=1**：文件在第 263 行被截断（末行 `if (odd) {`），node 报 `SyntaxError: Unexpected end of input`；文件干净且已提交（最后提交 `495a2fda`）⇒ 修它本身就是一个任务：**T00**。
3. **③ `run_e2e.js` 需真实实例**：默认 `http://127.0.0.1:18080/ui`；不传可用实例则整片 FAIL。该 runner 只做编排，不自起实例。
4. **④ `run_http_suites.js` 当前 exit=1**：8 次运行中 6 条失败（含 `console_auth` self-test FAIL、`login_flow` live FAIL、`role_split` exit=2 已退役），并报告「探针二进制比源码旧」。**2026-10-26 更新（T15）**：该 runner 的默认 `WB2API_EXE` 已改成 `bin/wb2api-server-probe.exe`（`tests/frontend/run_http_suites.js:65-66`）；一旦显式指向 `bin/wb2api-server.exe`，脚本会打印「那通常是**用户正在跑**的二进制」的警告（`:69-70`）。
5. **⑤ `run_e2e_family3.js` 当前 exit=1**：被 `EXCEPTION: 构建过期，拒绝在旧页面上断言` 拦下（`bin/wb2api-server-probe.exe` 2026-10-21 03:05 旧于 `internal/server/webui.html` 2026-10-26 05:16，判据在 `tests/frontend/lib_instance.js:151`）。
6. **前置动作（④⑤ 的共同解锁条件）**：先 `go build -o bin/wb2api-server-probe.exe ./cmd/server` 再重跑；否则 ④⑤ 的失败与本轮改动无关。
7. **没有一键全量**：全量 = ①–⑤ 五条命令 + 前置构建；① 与 ② 无外部依赖，③ 需要 Chrome + 实例，④ 自带实例，⑤ 需要实例 + 新鲜探针二进制。
8. **`-race` 在本机不可用**：归因为「TSan 影子内存基址超出用户态地址上限（`error code: 87`）」，**该归因是本仓文档结论**（`tests/frontend/README.md:60-83` 并列出上游 issue），**未验证（归因来源：仓库文档，本次未独立联网复核）**；CI 中 `-race` 也是 `continue-on-error: true`（`.github/workflows/ci.yml:61-71`）。
9. **⑥ `verify_wave2_pages.js`（T15 新增，2026-10-26）**：Wave 2 实现提交（`7ea6444`）里 `tests/frontend/` 一个文件都没动 —— T12/T13/T14 任务块里写的 `run_frontend_suites.js` / `run_http_suites.js` **都不覆盖新三页面**，于是「三页面对齐」在实现侧没有可执行判据。本脚本补上：覆盖三入口挂载、未认证 302/401（`/ui/models/data` 未认证必须 401 而不是 200+空数组）、状态页阈值 75/90 与 `window_seconds:300`、渠道页 `page_size` 夹到 100 与「翻页/搜索 0 次 fetch」、模型页合成行不可勾选/全选只勾可选行/无定价字样。它自带隔离实例与空闲端口、临时目录与 Chrome，**不需要 `--start`**；退出码 0 全绿 / 1 有断言红 / 2 环境前提不成立。判据来源：`tests/frontend/verify_wave2_pages.js:11-29`。**接入位置 = 直接单跑（direct probe）**：全仓 `grep verify_wave2` 只命中它自己（2 处，均在本文件内），**没有登记进任何 runner 汇总**，所以必须单跑、不能指望跑 `run_frontend_suites.js` 顺带覆盖。

### 8.4 门禁升级路径

先清 Wave 0 的四项（T00 的前端 runner 截断、T01/T02 的测试编译错、T03 的覆盖守卫红）→ 再解锁前端基线（`go build -o bin/wb2api-server-probe.exe ./cmd/server` 后按 §8.3 的 ③④⑤ 复测，把 ④ 的 6 条失败收敛到「本任务相关子集全绿」）→ 然后把 `go test ./... -count=1` 作为每波出口门禁（失败集合只允许是 §0.4 ③ 的子集）→ 最后才谈把 `-race` 升级为必过门禁（需先在有 gcc 且非 Windows 的环境跑通并确认全绿）。

---

## 9. 不做的事

| # | 不做 | 原因/依据 |
|---|---|---|
| N1 | 不搬 hyxapi 的 43 张表里的 31 张（计费/多租户/用户面/端点面/定价） | `PLAN-sql-unify.md:216-226` §6；§2.A |
| N2 | 不引入 gin | 本仓用 `net/http`；`PLAN-sql-unify.md:220` 该条仍然有效 |
| N3 | 不引入 React / Rsbuild / TanStack Query / bun 构建链 | §3 D2；`PLAN-sql-unify.md:221` |
| N4 | 不引入 Casbin / RBAC / 多租户 | §2.A A2/A3 |
| N5 | **不改用 `database/sql` + 手写 DDL 的路线** | `PLAN-newapi-alignment.md:107`「不引入 GORM（本仓无 DB）」已被 `PLAN-sql-unify.md:4` 作废，且工作区已用 GORM 实现（12 表 + AutoMigrate） |
| N6 | 不废弃 `data/*.json` | 降级为导出/备份格式（§6.0） |
| N7 | 不把 `internal/pool` 的冷却熔断换成 hyxapi 的弱实现 | §2.B B1 |
| N8 | 不改 `webui.html` 里 dashboard 现有的 7 个业务可视化 | `PLAN-sql-unify.md:223` |
| N9 | 不动 `Provider` 4 方法签名与 `ErrorKind` 既有值 | §6.1；只追加不插入铁律 |
| N10 | 不把 `-race` 作为门禁 | `tests/frontend/README.md:60-83`；CI 现为 advisory |
| N11 | 不为了让测试变绿而放宽 `arch_test.go` / `contract.go` 的判据 | 这两处是本仓「加上游零改动」的唯一可执行保证（§2.B B4/B5） |
| N12 | 不把 hyxapi 的 i18n / 插件 / 计费倍率 / `auto` 分组带进来 | `PLAN-groups.md:466-469`；§2.A |

---

## 10. 待用户拍板的开放问题

说明：Q1–Q5 来自 `PLAN-sql-unify.md:247-257`（合并去重后保留）；Q6–Q11 是本计划从两个仓库源码推导出的三页面对齐裁决点（每条都带 file:line 证据）。**未答复时按「建议默认值」执行**。

| 编号 | 问题 | 选项 | 影响 | 建议默认值 |
|---|---|---|---|---|
| **Q1** | GORM 还是 `database/sql` + 手写 DDL？（来源：PLAN-sql-unify §8 Q1） | A. GORM（现状）；B. `database/sql` | 全项目依赖图 | **A（追认现状）**：工作区已用 GORM + glebarez/sqlite 实现 12 表（`internal/store/db.go:36-38`、`:109`、`:212`），改回 B 等于重写 |
| **Q2** | `data/*.json` 保留为导出格式，还是彻底删除？（来源：§8 Q2） | A. 保留；B. 删除 | D3 / 迁移工具 | **A**：`migrate-sql export` 的字节等价契约以此为前提（`cmd/migrate-sql/main.go:86-88`） |
| **Q3** | 号池 `state.json` 也进 SQL 吗？（来源：§8 Q3） | A. 不进（留 JSON + Redis 快照）；B. 进 SQL | T04 范围 | **A**：它是每秒级运行态；只把「号」的静态信息进 `accounts`（§6.0） |
| **Q4** | 系统状态页 vs 现有 dashboard：整页替换 / 并列新增 / 逐指标吸收？（来源：§8 Q4；与 Q7 是同一裁决的两面） | A. 整页替换；B. 并列新增；C. 逐指标吸收 | Wave 2 全波 | **B**：`internal/server/webui-status.html` 已是既有成果且从未挂载（未被任何代码引用），挂载即最小代价 |
| **Q5** | 模型管理的「定价」部分要不要？（来源：§8 Q5；与 Q9 是同一裁决的两面） | A. 要；B. 不要 | T14 | **B**：本仓是免费额度池；`hyxapi/controller/model_pricing.go` 61 行与 `/api/model-pricing/*`（`hyxapi/router/api-router.go:262-270`）不移植 |
| **Q6** | 状态页两类阈值如何呈现：页面阈值（75/90）与熔断阈值（配置 90 + 连续 3 次采样） | A. 两处阈值分开标注；B. 统一成一个数 | 页面可信度与排障 | **A**：hyxapi 的 75/90 是页面阈值（`hyxapi/controller/system_status.go:96-102`），本仓熔断是配置项，两者混用会误导排障（`PLAN-sql-unify.md:117` 已明确要求保留这个区分） |
| **Q7** | 三页面与 dashboard 的并存形态（= Q4 的落实细节：入口放哪、iframe 还是内联） | A. 侧栏并列入口 + iframe 独立文件；B. 内联进 `webui.html` | `webui.html` 冲突面、Wave 2 串行度 | **A**：沿用 `webui.html:3068-3071` 已论证的 iframe 做法，避免 16,101 行单文件继续膨胀 |
| **Q8** | 延迟指标口径：hyxapi 用 `AVG(use_time)`，本仓账号池已有 P95 | A. 分区展示（状态页用 AVG、选号用 P95）；B. 统一为 AVG | 指标可解释性 | **A**：`hyxapi/model/log.go:526-554` 是 AVG；本仓 P95 是选号依据（§3 D4） |
| **Q9** | 渠道页高敏操作（密钥查看/重置）是否需要二次验证 | A. 只加二次确认 UI；B. 复刻 hyxapi 的四段式（RootAuth + CriticalRateLimit + SecureVerification） | 安全面与实现量 | **A**：本仓无 Root 体系与 SecureVerification（`hyxapi/router/channel-router.go:44-50` 是 hyxapi 专属链路）；B 会引入 §2.A A9 的整套依赖 |
| **Q10** | 计划文档与账本是否入版本库（`.gitignore:76 *.md` 当前排除了一切 md） | A. `git add -f` 逐文件放行；B. 改 `.gitignore` 加 `!PLAN-*.md` / `!docs/progress-log.md` / `!docs/open-questions.md`；C. 维持不入库 | 协作者能否看到计划与开放问题 | **B**：显式放行比逐文件 `-f` 更不易漏（三份 PLAN 与 `docs/待移植对齐.md` 当前都因该规则不在库里） |
| **Q11** | hyxapi 的 `disk_cache` / `channel_affinity` / `active_connections` 三组字段（本仓无对应物） | A. 置 `null` + 标注「本仓不适用」；B. 从响应里删掉 | 字段契约一致性 | **A**：§6.4 要求字段名不得删，删字段会让「以 hyxapi 为准」的验收失真 |

---

附录 A 事实索引（file:line 速查）

| 主题 | 位置 |
|---|---|
| 本仓 Provider 4 方法 | `workbuddy2api/internal/gateway/provider.go:41-62` |
| ChatStream / Credential / ExtOf | `provider.go:65-74` / `:92-122` / `:132-142` |
| 能力位 10 个 | `internal/gateway/capability.go:21-55` |
| ErrorKind 10 个 + 只追加铁律 | `internal/gateway/errorclassifier.go:99-209`（铁律 `:184-189,:207`） |
| 架构守卫（10 Test） | `internal/gateway/arch_test.go`；corePackages `:29-38`、discoveryExemptPackages `:66-69`、上游发现 `:206` |
| 契约测试 8 条 + 反射发现 | `internal/gateway/contract.go:22-47`、`:192-256` |
| 跨文件配对硬约束 | `cmd/server/refresh_pairing_test.go`、`cmd/server/jobs_pairing_test.go`；扩展点 `internal/gateway/credential_refresher.go:69`、`quota_consumed.go:36`、`login_options.go:29/149` |
| 13 处上游注册 | `cmd/server/main.go:275/347/433/523/609/664/711/781/861/890/979/1012/1058` |
| openaicompat 作为渠道工厂 | `cmd/server/main.go:1535-1537`、`:1666-1668` |
| 门禁位置（本仓） | `internal/admin/admin.go:742-759` |
| Redis 接线与降级 | `cmd/server/main.go:100-102`、`:131`；`internal/redisstore/redisstore.go:50/57/67` |
| 会话粘性镜像 | `internal/session/session.go:20/34/58/103/196` |
| JSON 权威路径 | `cmd/server/main.go:1411`（accounts）、`:1415`（sessions）、`:1287`（apikeys）、`:1294-1295/:1315`（channels → `<凭据根>/channels.json`；`data/channels.json` 已废）、`:1167`（state） |
| SQL 12 表 | `internal/store/db.go:256-271`；Open/Migrate/Close `:95/:208/:219`；DB 包级句柄 `:50`；DSN 补全 `:273-290` |
| SQL 表结构定义 | `internal/store/models.go`；RoleRoot=100 `internal/store/role.go:31`；setups 自愈 `internal/store/setup.go:73/105/128` |
| 迁移工具与退出码 | `cmd/migrate-sql/main.go:62-74`；键序契约 `:86-104`；测试串行说明 `main_test.go:14-15` |
| settings 事故点 | `cmd/server/settings_store.go:216-248`（整键清零）、`:173-183`（热生效分区） |
| 前端 `webui.html` 关键锚点 | `:3068-3071`（iframe 论证）、`:3379`（上游筛选）、`:3343-3345`（日志状态码筛选）、`:4889-4891`（`const COLS` 1 处 / 2 字段；`COLS.` 引用 10 处）、`:8663`（panelPolls）、`:9027`（panelSkeleton）、`:10414/:14436/:12305`（严格 digest 3 处）、`:11720-11736`、`:7471-7486`（剪贴板兜底）、`:15454-15459`（30s 轮询） |
| CI 五项门禁 | `.github/workflows/ci.yml:37/41/44/51/59/71`；go-version `:30` |
| 前端测试入口与 -race 归因 | `tests/frontend/README.md:9-15`；`-race` 归因 `:60-83` |
| `.gitignore` md 规则 | `.gitignore:76`（`*.md`）、`:82-83`、`:85`（`!README.md`） |
| hyxapi 系统状态页 | `hyxapi/controller/system_status.go:121-183`（c.JSON 字段字面量段；`:23-120` 为计算段）；阈值 `:96-102`；窗口 `:85,:134`；成功率 `:103-110`；池压力 `:186-199` |
| hyxapi 状态页数据源 | `hyxapi/middleware/stats.go:14,20,24,37-41`；`common/system_monitor.go:139-141`；`common/system_monitor_windows.go:12-49`；`model/log.go:486-508`、`:526-554`；`common/disk_cache_config.go:91-106`；`service/channel_affinity.go:112-196` |
| hyxapi CheckSetup 三态 | `hyxapi/model/main.go:57-82`；`model/setup.go:22-92`；`constant/setup.go:3` |
| hyxapi 表清单 | `hyxapi/model/main.go:243-285`、`:329/:364-399`；`model/audit_log.go:251-253` |
| hyxapi 鉴权两层 | `hyxapi/middleware/auth.go:63-66`、`:130-140`、`:416-436`；`common/constants.go:147-151`；`service/authz/resolver.go:10-35` |
| hyxapi 审计挂钩 | `hyxapi/middleware/audit.go:43-92`、`:101-113`、`:117-172`、`:208-262`；`model/audit_log.go:74/134` |
| hyxapi 渠道控制器 | `hyxapi/controller/channel.go:52-65`、`:85`、`:134`、`:140-144`、`:156-176`、`:257-387`；`common/page_info.go:37-75`；`common/constants.go:211-213` |
| hyxapi 路由与鉴权 | `router/channel-router.go:41-42`、`:44-50`、`:59-108`；`router/api-router.go:357`、`:433-459`、`:262-270`、`:20`、`:30`、`:35` |
| hyxapi 前端 | `hyxapi/web/package.json:7-10`；`pages/SystemStatus/`（`index.tsx` 265 等）；`pages/Channel/`（38 文件，含 10 个 `*.test.js`）；`features/metadata/`（7 文件）；`.storybook/main.js:9`；`tsconfig.json:22` |
| hyxapi 运行与构建 | `hyxapi/common/init.go:18`；`main.go:177-179`、`:41,44`；`common/database.go:19`；`Dockerfile:26`；`.env`（PORT=3001）；`AGENTS.md:7,12,15,16`（Vite/版本/输出名三处与源码冲突） |
| hyxapi relaykit 边界 | `hyxapi/relaykit/relayconvert/boundary_test.go:30-33`、`:42`、`:44-82`；relaykit go 1.25.1 |
| 许可证锚点 | `workbuddy2api/LICENSE:1-4`（MIT）；`hyxapi/LICENSE`（AGPL-3.0，661 行）；`hyxapi/NOTICE:1,3,9-25` |

---

附录 B 与 12 份旧调研文档的差异与更正表

| # | 旧调研文档的结论 | 更正（以源码为准） | 证据 |
|---|---|---|---|
| C1 | 「本仓无 Redis」 | **错**。`redis/go-redis/v9 v9.18.0` 在 HEAD 提交的 go.mod 内，`internal/redisstore` 已接线进服务端 | `workbuddy2api/go.mod`（HEAD 版 require 段）；`cmd/server/main.go:43/98/100/131`；`internal/redisstore/redisstore.go:17/55/62` |
| C2 | 「本仓无数据库 / 无 GORM / JSON 落盘」 | **半错**。SQL 层已写完 12 表（含迁移工具），但未提交、未接线、`data/gw.db` 不存在。正确表述 = 运行时 JSON 权威，SQL 是**已实现未接入的在制品** | `internal/store/db.go:256-271`；`cmd/migrate-sql/main.go:37` 是唯一**业务** import 方（`cmd/server` 与其它包零引用；同包 `main_test.go:29` 除外）；`data/` 无 `gw.db` |
| C3 | 「`internal/admin/gate.go` 是审计挂钩点」 | **文件不存在**。两侧 `glob **/gate*.go` = 0。本仓门禁在 `internal/admin/admin.go:742-759`；hyxapi 在 `middleware/auth.go` + `service/authz/*` | 两侧 glob 实测 0 命中；`internal/admin/admin.go:742-759` |
| C4 | 「本仓复制只用 `navigator.clipboard` ⇒ http 下静默失效」 | **错**。两处各有 `execCommand` 兜底且失败显式 toast。仅剩两条边界：`writeText` reject 时不回退；`:7481` 不校验返回值可假成功 | `internal/server/webui.html:11720-11736`、`:7471-7486` |
| C5 | 「30s 轮询只有 `pollAPIKeys`/`pollPoolValues` 有 digest」 | **错**。严格判据实测 3 处，放宽判据 5 处 | 严格：`webui.html:10414`（pollAPIKeys）、`:14436`（loadTrendSeries）、`:12305`（renderStatsReport）；放宽再加 `pollPoolValues`、`pollStatsCards` |
| C6 | 「本仓没有骨架屏」 | **错**。`panelSkeleton` 存在 | `internal/server/webui.html:9027` |
| C7 | 「hyxapi 用 go 1.25.1」 | **不准确**。root 与 tokenkit 是 **1.26**，只有 relaykit 是 1.25.1；`hyxapi/AGENTS.md:12` 与 `README.md:106` 互相冲突 | `hyxapi/go.mod`、`hyxapi/relaykit/go.mod`、`hyxapi/tokenkit/go.mod` |
| C8 | 「hyxapi 用 Vite」 | **错（hyxapi 自己的文档也错：`AGENTS.md:7,16` 写 Vite）**。实际是 **Rsbuild 2** | `hyxapi/web/package.json:7-10`、`rsbuild.config.ts`、`.storybook/main.js:9`、`README.md:19`、`web/README.md` |
| C9 | 「hyxapi 每文件带 AGPL 头 / 前端零第三方 UI 相关依赖」 | **须收窄**。版权头覆盖率 Go 2/1216=0.16%、`web/src` 53/430=12.3%；且存在 `lucide-react` 与 `@paper-design/shaders-react`（唯一使用点 `web/src/pages/Home/GrainBanner.tsx:3`） | 覆盖率实测；`web/package.json`；`GrainBanner.tsx:3`；`THIRD-PARTY-LICENSES.md` 漏 `@paper-design/shaders-react` 0.0.82 |
| C10 | 「hyxapi 无 DOM 测试」 | **成立（保留）**。67 个 `.test.js`，0 个 `import react`、0 个 `render(`、0 个 `@testing-library`；jsdom/playwright/happy-dom/vitest/jest 全不存在 | `web/src/**/*.test.js`；`modelPricingFrontend.test.js:10-16` 唯一浏览器模拟是手写 `globalThis.window` 桩；`tsconfig.json:22` 排除 `**/*.test.js` |
| C11 | 「AGPL ⇒ 只能照行为重写」 | **成立，但必须补义务**：`hyxapi/NOTICE:9-25` 的 AGPLv3 §7 附加条款要求修改版在 UI 醒目处保留 New API 归属与原文链接，并按 §7(c) 标注改动 | `hyxapi/NOTICE:9-25`；`hyxapi/LICENSE` 661 行 |
| C12 | 「`go test ./...` 全绿可当门禁」 | **本仓当前基线即红**。`go test ./... -count=1` exit=1 / 152.8s，四类失败（两处测试编译错 + 覆盖守卫 FAIL + codearts 三个 Live 401） | §0.4；`internal/store/option_test.go:237`、`cmd/migrate-sql/main_test.go:65`、`cmd/server/account_columns_invariant_test.go:281`、`internal/codearts/t2_quota_ext_test.go:95` |
| C13 | 「AGENTS.md §4 说扩展点 28 个」 | **过期**。实测 32 个（多出 `CredentialRefresher`、`QuotaConsumedSink`、`LoginOptionsExt`、`LoginFlowWithOptions`）；且 §7 十步清单漏了跨文件配对硬约束 | `internal/gateway/credential_refresher.go:69`、`quota_consumed.go:36`、`login_options.go:29/149`；`cmd/server/refresh_pairing_test.go`、`jobs_pairing_test.go` |
| C14 | 「本仓有 `internal/admin/gate.go`；hyxapi 有 `internal/admin/`」 | **两侧均不成立**。hyxapi 根目录**没有** `internal/`，其 admin 门在 `middleware/auth.go` + `service/authz/*` | hyxapi 根目录枚举；`hyxapi/middleware/auth.go:63-66/130-140` |
| C15 | 「`/admin/system-status` 用 hyxapi 那套 `version`/`resources`/`request_health`/`database`/`disk_cache`/`channel_affinity` 字段名」 | **错（本计划 §6.4 原文也错，已就地改定）**。本仓实际的顶层字段是 `service` / `resource` / `health` / `deps` / `cache`；其中 `channel_affinity` / `disk_cache` 在本仓**没有对应层**（无 HybridCache 亲和缓存、无磁盘缓存层），按「不适用 ⇒ 对应分块整块不渲染」处理并显式标注，**不编造 0** | `internal/sysmon/status.go:242-249`、`:251-253`；hyxapi 侧仅作语义参照 `hyxapi/controller/system_status.go:121-183` |
| C16 | 「logs 双写的验收可以跑 `node tests/frontend/run_http_suites.js --quick`」 | **错**。`--quick` 按源码只跑 `--self-test`（不起实例、不打流量），拿不到任何可对照的日志 | 改为 `go test ./internal/logbuf/ -run TestJSONLAndSQLAggregatesAgree -count=1`；源码依据 `tests/frontend/run_http_suites.js:109`、`:142` |
| C17 | 「T05 要修 `settings_store.go` 的『真事故』」 | **半错：基线里已经修好** —— `parse` 已改为以 `currentSettings()` 为基底、再用补丁键覆盖（不再把 patch 解成零值结构体）。T05 实际交付的是 `options` 表接线 + 文件级/options 级两层新守卫 | `cmd/server/settings_store.go:252-269`；§4 T05 的施工后回写行 |
| C18 | 「本仓剪贴板兜底 / 骨架屏 / 30s digest 守卫」三则旧结论 | **经本轮施工复核仍然成立，C4–C6 无需更正**（这三处代码路径本轮未被改动）。本轮新增的同类实况是「探针必须是**新构建**的二进制，否则 4 条套件假红」 | C4–C6；`internal/server/webui.html:11720-11736`、`:7471-7486`、`:9027`、`:10414`/`:14436`/`:12305`；见 §7 R11 |
| C19 | 「收口可以直接 `git add -A`」 | **错**。① `.gitignore:76` 是 `*.md` ⇒ 计划/调研文档默认不进版本库（本轮是用 `git add -f` 强制加入的）；② 跑过前端 runner 之后 `tests/frontend/*_gen.js` 会出现 **15 个幻影 M**（T15 实测：内容 diff 为空）⇒ `git add -A` 会把噪声拍进提交 | `.gitignore:76`；见 §7 R14 |

（C1–C19 共 19 条，覆盖任务要求的 12 条并多出 C13–C19 七条；**C15–C19 为 2026-10-26 本轮施工后的回写增补**。）
