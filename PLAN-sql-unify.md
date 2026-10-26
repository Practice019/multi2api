# SQL 统一改造计划（对标 hyxapi 的数据库层）

> **用户指令（原文）**：「我希望**全部一致 SQL 和表 也创建** 整合统一优化项目 然后优化整理mdplan」
> **推翻既有决策**：`PLAN-newapi-alignment.md:107` 的「**不引入 GORM（本仓无 DB）**」作废。
> **上游依据**：hyxapi 的数据库层（5 份调研报告在 `D:\project_GIT\test\`）+ `hyxapi-三页面前后端规格.md`
> **关联**：`待移植对齐.md`（逐项对齐记录）· `移植-后端.md` · `移植-前端.md`

---

## 0. 划清边界：对齐什么、保留什么

### 0.1 对齐（用户明确要「全部一致」）

| 维度 | hyxapi | 本仓**改造后** |
|---|---|---|
| 持久化 | **SQLite** + GORM | **SQLite** + GORM（同构） |
| 建表 | `AutoMigrate(41 张)` | `AutoMigrate(<本仓所需子集>)` |
| 初始化 | `setups` 表单例 + `CheckSetup()` 自愈 | **同构** |
| 角色 | `RoleCommonUser=1 / RoleAdminUser=10 / RoleRootUser=100` | `RoleMember=1(历史) / RoleAdmin=10` → **加 root=100** |
| 指标 | 从 `logs` 表 **SQL 聚合**出今日/近5分钟 | **同构**（这是拿到系统状态页的前提） |
| 渠道 | `channels` 表 + `abilities` 展开 | 新增（本仓现在是代码内 provider） |
| 模型 | `models` / `model_pricing` / `vendors` 三表 | 新增（本仓现在只有只读 chips） |

### 0.2 保留本仓差异（**不**为了「一致」而抹掉）

| 本仓有 | hyxapi 无 | 处置 |
|---|---|---|
| **账号池**（多账号 + 免费额度 + 登录器 + 冷却/熔断） | 它是「渠道 + 多 key」 | **保留为第一等公民**，账号也要进表 |
| `pool/` ≈ 4,838 行（三层熔断 / P95 / 持久化惩罚态） | 它 ≈ 1,694 行且**无熔断** | **保留**（§5 明确反向） |
| 11 个手写 Go provider + `ExtOf[T]` 能力位 | 配置驱动 + JS 插件 | **保留**（`PLAN-newapi-alignment.md` 的精神） |
| `internal/authlimit` 滑动窗口 | 固定窗口 | **保留** |
| `webui.html` 单文件无构建 | React + Rsbuild | **保留**（见 §6） |
| `data/*.json` 的**可读可手改** | DB only | **保留为导出/备份格式**（见 §2 D3） |

### 0.3 明确不搬（hyxapi 有但本仓永远不需要）

```
计费体系：钱包/充值/订阅/6 家支付/分组倍率/表达式定价/预扣-后补
多租户：RBAC + CASBIN / 权限矩阵 / 工单 / 公告 / 邀请码
用户面：注册 / 2FA / Passkey / OAuth 用户 SSO / 邮箱验证
端点面：mj / suno / video / audio / realtime 全套
其它：task_plugin(1487) / distributor 的 getModelRequest(700) / i18n 7 语言
⇒ 这些对应的表（约 25 张）**一张都不建**
```

---

## 1. 现状差距（可验证的事实）

| # | 差距 | 证据 | 影响 |
|---|---|---|---|
| **G1** | **无任何 SQL 能力** | 全库无 `database/sql` / `gorm` 依赖 | 系统状态页的今日/近5分钟指标**整类做不出来** |
| **G2** | 请求日志是 **jsonl 追加**，不可聚合 | `internal/logbuf`（滚动 + 保留天数） | 「今日请求数 / Token / 消费」「近5分钟 RPM / 成功率 / 平均延迟」**全部只能全量扫盘** |
| **G3** | 账号在 **JSON 文件** | `data/accounts.json`（`store.go:15-30` 有「为什么不用数据库」的论证） | 无跨进程互斥、无原子事务、无法与日志联查 |
| **G4** | 号池状态在 **JSON + Redis 快照** | `data/state.json` | 同上 |
| **G5** | 无渠道表 | 11 个 provider 是**代码**，不是数据 | 改 base_url / 加渠道要改代码或改 `config.json`，**无法按 hyxapi 那样列表化** |
| **G6** | 无模型目录 | `webui.html:3367` 只是只读 chips | 模型管理页**从零做** |
| **G7** | 无 `abilities` 这类「模型↔渠道」绑定表 | 绑定关系散在代码 | 无法回答「这个模型由哪些渠道提供」 |
| **G8** | 无 `setups` 单例表 | `account/store.go:192 Count()` 当标记 | 无启动自愈、无跨进程互斥（`待移植对齐.md` §1.5-②③ 已判） |

---

## 2. 架构决策（含理由）

### D1 ★ 驱动选型：**pure-Go SQLite，不用 cgo**

```
❌ mattn/go-sqlite3（hyxapi 用的 gorm.io/driver/sqlite 底层）—— 需要 cgo ⇒ Windows 交叉编译/单二进制退化
✅ glebarez/sqlite（基于 modernc.org/sqlite，纯 Go）—— 保持「零依赖单二进制」
```
**理由**：本仓的核心部署优势是「单二进制 + 零依赖」（`account/store.go:26-30` 明确写过）。cgo 会拆掉它。
**代价**：与 hyxapi 在驱动层不完全一致（但 GORM 层 API 一致，用户可见行为一致）。

### D2 GORM + AutoMigrate（**跟 hyxapi 一致**）

**理由**：用户要「全部一致」。GORM 的 `AutoMigrate` 保证「加字段 = 改 struct」的无痛演进。
**代价**：GORM 是重依赖（含约 10 个间接包）。
**备选**：若你认为不值的，可退回 `database/sql` + 手写 DDL（本仓风格更接近这个）—— **这条要你选**。

### D3 ⚠️ JSON **不废弃**，降级为「导出/备份格式」

```
读路径：SQL 是唯一权威
写路径：SQL 事务
JSON：仅用于 ① 首次启动的自动导入 ② 手动导出备份 ③ 人工排查
```
**理由**：`data/*.json` 的「人可读、可手改、可 diff、可 git」是本仓的真优势（`account/store.go:78-84` 那条「兼容手写的账号文件：缺省给最小权限而不是给管理员」就是靠它）。
**⇒ 若你要求「全部一致」到连可读文件都不要，说一声，我把这条删掉。**

### D4 表清单：**只建 12 张**（不是 hyxapi 的 41 张）

| # | 表 | 用途 | 对应现状 |
|---|---|---|---|
| 1 | `setups` | 初始化单例（ID=1） | 新增 |
| 2 | `users` | 账号 | `data/accounts.json` |
| 3 | `user_sessions` | 登录会话 | `data/sessions.json` |
| 4 | `options` | 设置键值 | `config.json` 的**可热改**部分 |
| 5 | `channels` | 渠道 | 新增（原 provider 代码元数据） |
| 6 | `abilities` | 模型↔渠道绑定 | 新增 |
| 7 | `models` | 模型目录 | 新增 |
| 8 | `vendors` | 供应商分组 | 新增 |
| 9 | `logs` | **请求日志（指标的唯一来源）** | `internal/logbuf` 的 jsonl |
| 10 | `accounts` | 账号池的号（**本仓独有**） | `data/state.json` |
| 11 | `account_events` | 号的冷却/熔断/禁用事件（可查） | 现在只有内存 + 日志文本 |
| 12 | `audit_log` | 审计（**§3.2 的 A3 已判要做**） | 新增 |

**理由**：剩下的 29 张属于 §0.3 的「明确不搬」。

### D5 指标口径**照抄 hyxapi 的定义**（用户要「以他的为准」）

```
rpm          = 窗口内请求数 ÷ 窗口分钟数        （system_status 的后端口径）
success_rate = 成功 / 总数                      （无样本保持 0，不误报）
avg_latency  = AVG（**不是 P95**）               ← ⚠ 但本仓号池已有 P95，两处并存要写清楚
今日消费      = logs 表 type=consume 的 SUM
service_status= 内存/磁盘 >= 75 ⇒ warning       ← controller/system_status.go:94-119
⛔ 页面阈值(75/90) ≠ 熔断阈值(配置 90，且需连续 3 次采样) —— 这个区分必须保留
```

---

## 3. 任务拆分（**垂直切片**，每个任务结束时系统仍可运行）

### Wave 1 — 数据层地基（3 路并行）

| 任务 | 规模 | 文件（**文件隔离**） | 验收（二元） | 验证 |
|---|---|---|---|---|
| **T1** 引入 GORM + pure-Go SQLite，建 `openMigrate` | M | `internal/store/db.go`（新）· `go.mod` | `data/gw.db` 生成；`sqlite3 gw.db .tables` 有表 | `go build ./... && go test ./internal/store/` |
| **T2** `setups` 表 + `CheckSetup()` 自愈 + 跨进程互斥 | S | `internal/store/setup.go`（新） | ① 空库 ⇒ `Setup=false`；② 有 user 行但无 setups 行 ⇒ 启动后**自动补行**；③ 两个进程并发初始化 ⇒ **只成功一个** | `go test -run TestSetupSelfHeal ./internal/store/` + 真机双进程实测 |
| **T3** `options` 表 + 从 `config.json` 首次导入 | S | `internal/store/option.go`（新） | 首次启动把 `config.json` 的可热改键写入 options；二次启动**不重复写** | `go test -run TestOptionSeed ./internal/store/` |

> **Checkpoint 1**：`go build ./...` 通过；`data/gw.db` 存在且 `setups` 表有一行；**现有功能零回归**（`go test ./...` 全绿）

### Wave 2 — 账号与会话迁移（2 路并行，**最难的一波**）

| 任务 | 规模 | 文件 | 验收 | 验证 |
|---|---|---|---|---|
| **T4** `users` + `user_sessions` 表，`account.Store` 改为 SQL 后端，**保持公开 API 不变** | L | `internal/store/user.go`（新）· `internal/account/store.go` | ① `CreateBootstrap` 的**原子性语义不变**（并发只成功一次）；② 现有 `account` 全部测试通过；③ 首次启动自动导入 `data/accounts.json` | `go test ./internal/account/...` 全绿 + 手工删库重启看是否重建 |
| **T5** 审计日志表 + 写点 | M | `internal/store/audit.go`（新）· `internal/admin/admin.go` | 改一次 base_url ⇒ `audit_log` 出现一条含 actor/action/route/status/success | `go test -run TestAudit ./internal/admin/` + 真机点一次看行 |

**T4 拆解说明（为什么是 L 而非 XL）**：只改 `store.go` 一个文件的**内部实现**，公开方法签名（`CreateBootstrap`/`Authenticate`/`SetPassword`/…）**一个都不动** ⇒ 上层零改动。这是本轮最关键的一条纪律。

> **Checkpoint 2**：能登录、能改密；杀掉进程重启后**会话仍在**（`AuthVersion` 语义保持）；`data/accounts.json` 仍在且是**最新导出的**

### Wave 3 — 渠道 / 模型 / 日志（3 路并行）

| 任务 | 规模 | 文件 | 验收 | 验证 |
|---|---|---|---|---|
| **T6** `channels` + `abilities` 表；从 11 个 provider 的代码元数据**反向 seed** | M | `internal/store/channel.go`（新）· `internal/admin/channels.go` | 启动后 `channels` 表有 11 行（每 provider 一行）；`/ui/channels` 页仍工作 | `go test ./internal/admin/ -run TestChannelSeed` + CDP 截图比对 |
| **T7** `models` + `vendors` 表；从各 provider 的模型目录 seed | M | `internal/store/model.go`（新）· `internal/server/models_channels.go` | `models` 表行数 = 现 chips 数；`vendors` 有 11 行 | `go test ./internal/server/ -run TestModelCatalog` |
| **T8** ★ `logs` 表 + **双写**（jsonl 保留，SQL 写入）+ 三个聚合查询 | L | `internal/store/log.go`（新）· `internal/logbuf/` | ① 打 3 次 `/v1/chat/completions` ⇒ `logs` 有 3 行；② `SELECT` 能算出今日请求数/Token/消费；③ **jsonl 仍在写**（双写，可回滚） | `go test ./internal/store/ -run TestLogAggregate` + 真机打 3 次后 `SELECT` |

**T8 的双写是刻意的**：SQL 写入失败不能影响主流程 ⇒ 异步 + 失败只记日志。这样 T8 出问题时**能单独回滚**。

> **Checkpoint 3**：三个聚合数（今日请求/Token/消费）与**手工数 jsonl** 的结果一致（允许 ±0）；渠道与模型页仍可用

### Wave 4 — 三页对齐（**依赖 Wave 3，见 §4 的文件隔离**）

| 任务 | 规模 | 文件 | 验收 |
|---|---|---|---|
| **T9** 系统状态页（新增 section，**不是替换 dashboard**） | L | `internal/server/webui-status.html`（新）· `dash-chart.js` | 5 组卡片全在；倒计时从 `dataUpdatedAt` 反推；三档阈值条正常档跟随主题 |
| **T10** 渠道管理页对齐 hyxapi（12 列 + 筛选弹窗 + 拖拽 preview/undo） | L | `internal/server/webui-channels.html` | 列数与 hyxapi 一致（不适用列标注）；筛选 4 条纪律全有 |
| **T11** 模型管理页（**从零**，`models`/`vendors` 两级 + 合成行） | L | `internal/server/webui-models.html`（新） | Model 与 Vendor 两个 tab；渠道用到但目录没有的模型显示为**合成行**且不可勾选 |

> **Checkpoint 4**：三个页面在 1920×1080 与 375×812 两种宽度下各截一张；`go test ./...` 全绿；`tests/frontend/` 门禁全绿

### Wave 5 — 收尾

| 任务 | 验收 |
|---|---|
| **T12** `data/*.json` → SQL 的一次性迁移工具 + 导出回 JSON | 跑一次：行数对得上；再 `--export` 出来的 JSON 与迁移前**逐字节等价** |
| **T13** 全量验证 | 见 §7 |

---

## 4. 并行策略与文件隔离

```
Wave 1（3 路）   A1: T1 db.go      A2: T2 setup.go    A3: T3 option.go
                 （三个新文件，互不重叠；T1 先合并再让 A2/A3 基于它）
       ↓ 主脑汇总 + commit
Wave 2（2 路）   A4: T4 user.go + account/store.go    A5: T5 audit.go + admin.go
       ↓ 主脑汇总 + commit（⚠ 这一波动既有文件，必须逐个合并）
Wave 3（3 路）   A6: T6 channel.go   A7: T7 model.go   A8: T8 log.go
       ↓ 主脑汇总 + commit
Wave 4（3 路）   A9: T9 status.html  A10: T10 channels.html  A11: T11 models.html
                 （⚠ 三个不同文件 —— 这是先拆页面的理由）
       ↓ 主脑汇总
Wave 5（主脑自己）T12 / T13
```

### ⛔ 三条文件隔离铁律

1. **不改 `internal/server/handler.go`** —— 路由注册由主脑统一改（`PLAN-newapi-alignment.md:90-91` 的既有约定）
2. **不改 `internal/server/webui.html`** —— 这一轮三个新页面**各自独立文件**，用 iframe 挂进去（沿用 `webui.html:3068-3071` 已论证过的做法）
3. **公开 API 签名一个都不动** —— 特别是 `account.Store` 的方法集；只换内部实现

---

## 5. 风险与回滚

| 风险 | 等级 | 对策 |
|---|---|---|
| **GORM 引入拖垮构建/体积** | 中 | T1 单独一个 commit；测 `go build` 产物大小与时间；若超标退回 `database/sql`（D2 备选） |
| **cgo 误入** | 高 | T1 后立刻验证 `CGO_ENABLED=0 go build ./...` 通过 |
| **账号迁移丢数据** | **极高** | ① 迁移前**先备份** `data/` 到 `data.bak-<ts>/`；② 首次启动**只导入不删除** json；③ 导入后校验行数；④ 提供 `--export` 反向导出 |
| **T8 双写拖慢主流程** | 中 | 异步 channel + 有界缓冲 + 满则丢弃（丢日志不丢请求）；配一个「SQL 写入丢了多少条」计数器 |
| **既有测试大面积红** | 高 | 每波结束跑一次 `go test ./...`；红就**先修再进下一波**，不累积 |
| **指标口径与手工数不一致** | 中 | Checkpoint 3 强制手工对数 |
| **三页与 `dashboard` 重复** | 低 | 见 `待移植对齐.md` §4.1.3 的三种解读，**待你裁决** |

**每个 Wave 结束 `git commit`**，出错单独回滚该波。

---

## 6. 不做的事

```
❌ 不搬 hyxapi 的 41 张表里的 29 张（计费/多租户/用户面/端点面）
❌ 不引入 gin（本仓用 net/http）—— 这条 PLAN-newapi-alignment.md:106 仍然有效
❌ 不引入 React / Rsbuild / TanStack Query（前端仍单文件无构建）
❌ 不把 `pool/` 的冷却熔断换成 hyxapi 的弱实现（§0.2 已定）
❌ 不改 `webui.html` 里 `dashboard` 现有的 7 个业务可视化
❌ 不废弃 `data/*.json`（降级为导出格式，见 D3）
❌ 不引入 Casbin / RBAC
```

---

## 7. 验证口径

```
1. CGO_ENABLED=0 go build ./...            通过且产物仍是单文件
2. go build ./... && go vet ./... && go test ./...   全绿
3. 首次启动（空 data/）                     生成 data/gw.db + setups 一行 + 可建管理员
4. 二次启动                                 不重复 seed；Setup 自愈生效
5. 双进程并发初始化                         只一个成功（另一下报 bootstrap_done）
6. 现有账号/会话/data/*.json 全部键           迁移后仍在（逐键比对）
7. 打 3 次 /v1/chat/completions              logs 表 3 行；三个聚合数与手工数一致（±0）
8. data/*.json --export 出来                 与迁移前逐字节等价
9. 三个新页面 1920×1080 + 375×812              各截一张图
10. tests/frontend/ 派生夹具门禁              全绿
```

---

## 8. ⚠️ 待你确认的 5 个开放问题（**不猜，等你答**）

| # | 问题 | 我的建议 | 影响面 |
|---|---|---|---|
| **Q1** | **GORM 还是 `database/sql` + 手写 DDL？** hyxapi 用 GORM，但本仓风格更接近后者 | 若「全部一致」是硬要求 ⇒ **GORM**；否则 `database/sql` 更轻 | 全项目依赖图 |
| **Q2** | **`data/*.json` 保留为导出格式，还是彻底删除？** | **保留**（可读可 diff 是本仓的真优势） | D3 / T12 |
| **Q3** | **号池 `state.json` 也进 SQL 吗？** 它是高频写的运行态（每秒级） | **不进**（留 JSON + Redis 快照）；只把「号」的静态信息进 `accounts` 表 | T4 的范围 |
| **Q4** | **系统状态页 vs 现有 dashboard**：整页替换 / **并列新增** / 逐指标吸收？ | **并列新增**（见 `待移植对齐.md` §4.1.3） | Wave 4 |
| **Q5** | **模型管理的「定价」部分要不要？** 你是免费额度池，**没有计价需求** | **不要**（只做 Model + Vendor 两级目录） | T7 / T11 |

**你答完这 5 条我就开工；不答的话，我按「建议」列的默认值走。**
