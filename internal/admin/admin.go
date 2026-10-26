// Package admin 管理台后端：账号增删、模型目录、日志与统计。
//
// # Task 3c 之后的职责边界
//
// 本包只剩**上游无关**的通用端点：
//
//	/admin/accounts*   /admin/login/*   /admin/logs*   /admin/settings
//	/admin/stats       /admin/models/*  /healthz       /ui
//
// 平台特殊端点（workbuddy 的签到/成长/旅行/本机登录 22 条）**不在本包**：
// 它们由上游通过 gateway.AdminExt.AdminRoutes() 自注册，
// 本包在 New 里遍历已注册 Provider 挂载（见 mountUpstreamRoutes）。
// **加新上游时本文件零改动。**
//
// 安全模型：整个 /admin/* 子树**一律校验凭据**（浏览器会话 或 admin_token），
// 且身份只有一种 —— **管理员**（`canAccess`）。
//
// ⚠ 2026-10-19 用户新口径：登录系统收成单账号管理员，"成员"角色已取消。
// 原先的成员白名单（memberRules）连同它那套角色分流**整张删掉**：没有任何
// 请求能带着非管理员身份走到门禁（非管理员的历史记录在 Authenticate /
// SessionStore.Lookup 就被拒，见 internal/account），留着它就是一条永远为假
// 的角色判据 —— 下一个人读代码时会以为成员还有身份。
//
// ⚠ 改造前这里写的（也是实现的）是「只接受 loopback 直连，非本机一律 403，
// 本机免令牌」—— 那条路径把"能连上来"当成了"有权限"，于是本机任何进程
// （含浏览器里的任意页面发起的请求）都能读到并改写管理面，包括轮换端点
// 回吐完整 api_key。用户报障「填错 admin token 居然能进 UI」就是它。
// 现在回环地址**不是凭据**：绑回环只让"少暴露一个端口"这一条成立。
package admin

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/groupstore"

	"workbuddy2api/internal/account"
	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/store"
)

// Config 管理台依赖。
type Config struct {
	Pool *pool.Pool
	// ⚠ 这里**没有** Upstream 字段 —— 已删除（它曾是 `*upstream.Client`，workbuddy 的私有 SDK）。
	//
	// 删除理由（两层，第二层是架构约束）：
	//
	//  1. 那个字段声明了却**从未被本包读取**（生产代码零引用），是个死字段 ——
	//     注入方 cmd/server 一直在传，管理台从不消费。
	//  2. 它让 admin 这条**核心线**依赖了某个上游的私有 SDK，直接打破
	//     「加新上游核心零改动」这条判据（arch_test.go 的
	//     TestPrivateSDKIsNotConsumedByOtherUpstreams 现在会拦下它）。
	//
	// 若将来管理台真的需要某个上游的能力，正确入口是 gateway 的扩展点
	// （AdminExt 或新加一个），而不是把某个具体上游的客户端塞进来 ——
	// 后者会让管理台长出只对某一个上游成立的行为。
	OAuth *oauth.Client
	// ChannelProvider 给 (渠道id, BaseURL) 造一个 gateway.Provider —— 供「模型自动发现」
	// 等"管理台要用某个上游的能力"的场景使用。
	//
	// ⚠ 为什么是 `gateway.Provider` 而不是 `*openaicompat.OpenAICompat`：本结构体上方那段
	// 注释（曾经的 Upstream 字段被删的理由）已经把规矩写死了 —— 管理台若直接认识某个具体上游，
	// 就会长出"只对某一个上游成立的行为"，并打破「加新上游核心零改动」。用 gateway 的契约做
	// 注入点，管理台只认 `Models()` / `Chat()` 这些方法，换任何 Key 型上游都不用改这一层。
	ChannelProvider func(id, baseURL string) gateway.Provider
	Log             *checkinlog.Log
	Ring            *logbuf.Ring
	AuthDir         string
	// AuthsBase 各上游凭证目录的**父目录**（= 配置里 auth_dir 的原值）。
	//
	// 与 AuthDir 的关系：`AuthDir = AuthsBase/<默认上游>`。
	// 迁移期凭证可能还在 AuthsBase 根下，兼容扫描（LoadDirCompat）
	// 需要它才能两处都看。
	//
	// 空串时回落 AuthDir（单上游旧形态，行为不变）。
	AuthsBase string

	// Registry 已注册的上游。本包遍历它，把每个上游通过 AdminExt
	// 声明的管理端点挂上来，并把通过 SettingsExt 声明的设置项合并进设置页。
	// nil = 不挂任何上游端点（测试路径）。
	//
	// 用 *gateway.Registry 而不是 []gateway.Provider：注册表是所有核心包
	// 共用的同一份事实来源，传列表会让"谁注册了什么"出现第二个来源。
	Registry *gateway.Registry

	// SettingsExts 上游设置扩展的**适配器**（由 cmd/server 注入）。
	//
	// # 为什么不直接从 Registry 里断言 SettingsExt
	//
	// 因为 workbuddy 的设置项用的是它自己的 SettingField 类型
	// （上游包不得 import admin，所以类型必须各声明一份），
	// 而 Go 的方法集是精确匹配的：[]workbuddy.SettingField ≠ []admin.SettingField，
	// 断言必然失败 —— 而且是**静默**失败（设置页少几个键，没有任何报错）。
	//
	// 所以这条线显式走适配器：cmd/server 把上游的 Fields() 逐字段转换后传进来。
	// AdminExt / JobExt / LoginFlow 不需要这一层，因为它们的接口全部定义在
	// gateway 里（只有一种类型，上游直接实现即可）。
	SettingsExts []SettingsExt

	// ResetModelsCache 清空模型目录缓存（由 server 包注入，避免 admin 反向依赖 server）。
	ResetModelsCache func()
	// ModelCatalog 取模型目录快照（成本系数来源）。nil = 未接线，统计里报 unavailable。
	//
	// 签名带 provider：多上游部署下每个上游的 /v3/config 是各自的事实
	// （workbuddy 与 workbuddy-intl 的模型互不相干），按上游取目录才能
	// 给每个上游的模型都标上倍率。由 cmd/server 注入 server.(*Handler).ModelCatalogFor
	// —— 它内部按 provider 分缓存（1h TTL / 5min 失败负缓存）。
	ModelCatalog func(provider string) *gateway.ModelCatalog
	// ModelCatalogState 只读的目录缓存状态（ok/stale/unavailable）。nil = 未接线。
	// 与 ModelCatalog 分开注入是刻意的：状态查询**绝不能**触发上游请求，
	// 而取目录会。把两者混成一个函数，就没法在 /admin/stats 里安全地只要状态。
	ModelCatalogState func() ModelCatalogState
	// Settings 设置页的读写契约（由 cmd/server 实现并注入；nil = 关闭设置页）。
	Settings SettingsStore
	// BuildTime 进程启动时间，UI 用来算运行时长。
	StartedAt time.Time

	// Scheduler 核心调度器的只读视图，供 /admin/schedule 使用。
	//
	// # 为什么这两条端点属于核心（而不是某个上游）
	//
	// `/admin/schedule`（签到/保活的启停、时点、下次唤醒）与
	// `/admin/task`（全量任务槽状态）**不表达任何上游身份** ——
	// 它们读的是核心自己排的班。
	//
	// Task 3c 曾把它们随其它 20 条一起放进 workbuddy（理由：写方
	// checkin/keepalive 在那，读写不宜分家）。阶段 0 评审指出这会**真出问题**：
	// 第二个上游若不声明 CapCheckin，这两条路由就**没人服务**，
	// 而它们本是全上游通用的。所以移回核心。
	//
	// nil = 未接线（测试路径），返回"未接线"降级。
	Scheduler SchedulerView
	// TaskSlot 全量任务槽，供 /admin/task 使用。nil = 返回空快照。
	TaskSlot TaskSlot

	// DefaultProvider 缺省上游标识（裸模型名走它）。
	//
	// 用于 /admin/providers 标出哪个是默认 —— 前端据此把它的模型
	// 以**裸名**展示（其余上游带前缀）。空串 = 没有默认。
	DefaultProvider string

	// ServiceName 网关身份标识，经 /admin/ui/manifest 下发给控制台。
	//
	// # 为什么要注入而不是在这里定义
	//
	// 真实来源是 server.ServiceName（/healthz 的 service 字段与 X-Service 头
	// 用的同一个常量）。admin 不得 import server（架构约束：server 依赖 admin，
	// 反向依赖会成环），所以由 cmd/server 把值传进来。
	//
	// 空串时 manifest 里 service 为空，前端退回显示 "gateway" ——
	// **不在这里兜默认值**：那样会让"忘了注入"这个装配错误永远无法被发现。
	ServiceName string

	// ReloadProvider `AuthDir` 里的凭证**属于哪个上游**。
	//
	// # 为什么必须显式配置（评审 F3）
	//
	// 早先 reload 调裸 `SyncToDir`，它归一成"当前默认上游"。那在本部署里
	// 碰巧正确 —— AuthDir 只装 workbuddy 凭证，而 workbuddy 恰好是第一个注册的。
	// 评审证明：一旦默认上游不是 workbuddy，同一个 reload 会
	// **扫描 workbuddy 文件却按别的域剔除**，把那个上游的账号全删掉并落盘。
	//
	// 所以域由配置给出。空串时回落 DefaultProvider（兼容旧装配），但会记日志 ——
	// **静默回落正是这个 bug 当初能藏住的原因**。
	ReloadProvider string

	// APIKeys 多 API key 管理存储（对标 new-api 令牌体系）。
	// nil = 不注册 /admin/apikeys 路由（未启用该功能，与旧行为一致）。
	APIKeys *apikey.Store
	// Channels 渠道存储（中转站方向：能力索引 + 每渠道模型映射）。
	// nil = 不注册 /admin/channels 路由（未启用该功能，与 APIKeys 同一条判据）。
	Channels *channelstore.Store

	// Groups 分组存储（PLAN-groups.md P1）。nil = 未启用分组功能。
	//
	// 分组是**独立实体**（计划 Q1）：它持有「这个分组包含哪些模型」，
	// 供 ApiKey 引用。与 Channels 的区别：渠道回答「谁提供这个模型」，
	// 分组回答「这把 key 能用哪些模型」。
	Groups *groupstore.Store

	// ModelNames 取「当前对外模型目录」（内部名），供分组一致性检查判断分组内容是否陈旧。
	//
	// nil = 未接线 ⇒ 一致性报告里 stale_checkable=false（前端显示"未检查"）。
	//
	// # 为什么单独一个字段，而不是复用 ModelCatalog
	//
	// ModelCatalog(provider) 会**发起上游请求**去取目录 —— 而管理页会反复轮询
	// 这个检查端点，那样等于每次刷新都打一遍上游。ModelNames 期望是一个**读缓存**
	// 的函数（server 侧已有目录缓存）。
	//
	// ⚠ 未接线时**不能**退化成"空目录"：空目录会让每一分组里的每个模型都被判成陈旧。
	// 所以 nil 与"返回空切片"必须分开 —— 前者是"没查"，后者是"查了，什么都没有"。
	ModelNames func() ModelCatalogSnapshot
	// APIKey 管理钥匙（config.api_key 原值）：/admin/apikeys 返回它的掩码，
	// 让管理页与仪表盘「API 接入信息」显示同一把钥匙（一个事实来源）。
	// 空 = 未配置管理钥匙。
	//
	// ⚠ 它**不再**用于门禁 —— 门禁认 AdminToken（见下）。这里保留它只是
	// 为了让管理页能显示"对话接口用的那把 key 是什么"（掩码形式）。
	APIKey string

	// AdminToken 管理台门禁令牌（与 api_key 完全独立）。
	//
	// # 为什么必须独立（用户报障：门禁 token 不能和 api 一样）
	//
	// 改造前门禁认的就是 APIKey，也就是对话接口那把 —— 后果是"给某个
	// 客户端配一把 key 调对话"等于"把整个管理台交出去"。详见
	// ServeHTTP / adminTokenOK 的注释。
	//
	// 空 = 没有远程管理面（只有本机免令牌那条路，且要求监听在回环地址）。
	AdminToken string

	// Accounts / Sessions 控制台账号体系（由装配层注入；两者一起给或一起不给）。
	//
	// # 为什么要加它（用户要求：抄 new-api 的登录系统）
	//
	// 改造前管理面只有**一把令牌**一个身份：谁拿到 admin_token，谁就是管理员。
	// 现在两条路并存（见 principalOf），但**身份只有一种**：
	//
	//	浏览器：/ui 用账号口令登录 → 会话 cookie
	//	脚本：  Authorization: Bearer <admin_token>（等价管理员，无会话状态）
	//
	// ⚠ 这两条路都只通向管理员：控制台只有一个账号，且那个账号就是管理员
	// （成员角色已取消，见 account 包）。原来这里写的"按角色分流见 memberRules"
	// 随 memberRules 一起删掉了。
	Accounts *account.Store
	Sessions *account.SessionStore

	// ListensLoopback 本进程是否**只**监听回环地址。
	//
	// ⚠ 它**不再参与放行判定**（改造前是「本机免令牌」的前提，见被删掉的
	// localExempt —— 那条路径让本机任何进程都能进管理面，用户报障"错令牌
	// 也能进 UI"就是指它）。现在它只用于**提示文案**：绑了非回环地址时
	// 更该配凭据，启动提示与拒绝 hint 会据此说得更具体。
	ListensLoopback bool

	// ConfigPath config.json 路径（轮换管理钥匙时写回 api_key 字段）。
	// 空 = 不支持写回（轮换端点返回错误）。
	ConfigPath string
	// OnAPIKeyRotated 轮换成功后的通知（装配层注入，更新 handler 内存钥匙，
	// 让新 key 立即生效无需重启）。nil = 只写文件不更新内存。
	OnAPIKeyRotated func(newKey string)

	// OnAdminTokenRotated 管理令牌轮换成功后的通知。
	//
	// ⚠ 与 OnAPIKeyRotated **分开**（用户报障：轮换之后还要重启才有用）。
	//
	// 原因是两条内存副本原本不同步：
	//
	//	server.Handler.apiKey  → 有 SetAPIKey，轮换后立即生效
	//	admin.Handler.cfg.APIKey → **构造期快照**，永不更新
	//
	// 而门禁恰恰读的是后者 ⇒ 轮换写成功、内存里门禁还是旧值 ⇒ 表现为
	// "轮换后必须重启"。现在两条各有各的更新回调，且**都接上**。
	OnAdminTokenRotated func(newKey string)

	// OpenAuthURL 用**无痕/隐私窗口**打开一条授权链接，返回浏览器的展示名。
	//
	// # 为什么由装配层注入，而不是在 admin 里直接 exec
	//
	// 1. admin 包要保持"可测且无副作用"：这个函数一调就真的会弹窗口，
	//    测试必须能替换它。注入让"不弹窗口"成为默认（nil）。
	// 2. 具体怎么开（哪个浏览器、要不要独立 profile、平台差异）是
	//    部署环境的事，属于 `internal/browseropen`；admin 只该知道
	//    "有这么一个动作"，并把结果回传给前端。
	//
	// 非空时，「添加账号」会**默认**调用它一次（用户要求：默认就是无痕模式），
	// 并在响应里回带 browser / browser_error。
	//
	// nil = 本部署不自动开浏览器（无 GUI 服务器，或配置里关了）——
	// 此时行为与改造前逐字节一致：只回 auth_url，让用户自己复制。
	OpenAuthURL func(rawURL string) (browser string, err error)

	// Audit 审计写入回调（Wave1-T07）。**每一次管理面写操作**（POST/PUT/PATCH/
	// DELETE）落一条 audit_log：谁、什么时候、对哪条路由、做了什么动作、结果如何。
	//
	// # 为什么是注入而不是本包直接写库
	//
	// 与 ChannelProvider / OpenAuthURL 同一条理由：admin 只该知道「有这么一件事
	// 要发生」，不该知道它落到哪个库、什么时候写。注入让「不写库」成为默认
	// （nil），测试因此不需要真库；而 store 未打开（store.ErrNotOpen）时怎么处理
	// 也由装配层决定 —— 审计写失败**不许**影响主流程（见 store.AppendAudit）。
	//
	// nil = 不记录：测试路径与尚未接线的部署，行为与改造前完全一致。
	//
	// # 装配（Wave 1 收口，T11）
	//
	//	cfg.Audit = store.AppendAudit
	//
	// 一行即可：store.AuditLog 与本回调同型，中间**不需要**适配层 —— 少一层
	// 字段映射就少一处「漏赋值一个字段」的机会，而漏字段的审计是静默错的
	// （行数对了、内容错了，行数判据看不出来）。
	Audit func(store.AuditLog) error

	// RedisMode 当前生效的 Redis 模式（"" / "noop" / "redis"），由装配层注入。
	//
	// 状态页需要它来给出一句**准确的**话：没配 Redis 是本仓的**配置事实**
	// （有 Noop + 本地文件降级，见 PLAN §1 硬结论 2），不是故障 —— 所以它只影响
	// 一句 warning 文案与依赖卡上的一个标，**不参与 service.status 判档**。
	//
	// 空串 = 装配层没注入 ⇒ 按 noop 呈现（这正是未配置 Redis 时的真实形态，
	// 与 internal/server 的 /status 用同一个默认值）。
	RedisMode string
}

// SetOnAPIKeyRotated 注入管理钥匙轮换后的通知（装配层在 handler 构造完成后调用，
// 因为回调要引用 handler —— 它不能在 admin.New 的 Config 里直接闭包捕获）。
func (h *Handler) SetOnAPIKeyRotated(fn func(newKey string)) {
	h.cfg.OnAPIKeyRotated = fn
}

// SchedulerView 核心调度器在本包看来是什么样（只保留 /admin/schedule 读的字段）。
type SchedulerView interface {
	// NextWake 下一次唤醒时刻与将要执行的任务名。
	NextWake() (time.Time, []string)
	// Hours 签到时点与保活时点。
	Hours() (checkinHours, keepaliveHours []int)
	// CheckinEnabled 签到是否启用。
	CheckinEnabled() bool
	// KeepaliveEnabled 保活是否启用。
	KeepaliveEnabled() bool
}

// JobStatusView 调度器的**可选**扩展：暴露已注册任务的运行状态。
//
// # 为什么是可选接口而不是塞进 SchedulerView
//
// SchedulerView 是 /admin/schedule 的契约，那边只读"排的什么班"。
// 把任务状态塞进去会让所有实现 SchedulerView 的测试替身都必须实现它，
// 而绝大多数测试根本不关心任务状态。
//
// 可选接口 + 类型断言是**本仓库既有的模式**（见 gateway.ExtOf 的注释）：
// 核心按需发现能力，不强迫每个实现都写空方法。
type JobStatusView interface {
	// JobStatuses 各已注册任务的运行状态（按任务名稳定排序）。
	JobStatuses() []scheduler.Status
}

// TaskSlot 全量任务槽：同一时刻只允许一个全量任务。
//
// 与改造前 h.task 的行为逐字一致 —— 只是改由 cmd/server 注入，
// 因为任务槽的生命周期归属核心调度器，不属于某个上游。
type TaskSlot interface {
	// Snapshot 当前任务槽状态（与改造前 /admin/task 的响应体一致）。
	Snapshot() map[string]any
}

// Handler 管理台路由（/admin/ 子树）。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	// adminTokenMu / adminToken 当前生效的管理令牌（支持运行时轮换）。
	//
	// # 为什么它是**可变**的而不是直接用 cfg.AdminToken
	//
	// cfg 是 admin.New 时的结构体快照。改造前门禁读 cfg.APIKey，
	// 于是轮换写了盘、内存里门禁还是旧值 ⇒「轮换后要重启才生效」（用户报障）。
	// 把门禁判据挪到这个可变字段，轮换时 SetAdminToken 一下即可。
	//
	// 空串 = 还没人轮换过 ⇒ 回落 cfg.AdminToken（普通部署行为不变）。
	adminTokenMu sync.RWMutex
	adminToken   string

	// patterns 已注册的通用端点 pattern（"METHOD /path" 形式）。
	//
	// 为什么必须单独记一份而不是问 mux：Go 1.22 的 ServeMux 没有"列出已注册
	// pattern"的接口，也没有注册失败回滚。上游端点挂载前要判断冲突，
	// 就得有一个可枚举的来源 —— 而它必须由 register 统一维护，
	// 漏记一条就等于那条可以被上游静默覆盖。
	patterns []string

	// quotaAutoRefreshOnce 额度自动刷新循环只启动一次（见 quota_autorefresh.go）。
	//
	// ⚠ 它解决的问题是**用户报的那个现象**：「前端显示的额度是过时的」——
	//   因为 chat 回执里没有余额（client.go 的响应结构里确认过），
	//   真实余额只能靠"有人去查"来更新，而原来**只有用户点按钮**才查。
	//   ⇒ 这个 Once 保住的是那个"没人点也会查"的循环。
	quotaAutoRefreshOnce sync.Once

	// 统计聚合的短缓存：聚合要扫整个落盘文件，而前端按轮询节奏调用，
	// 缓存让「扫盘频率」与「轮询频率」解耦（见 statsCacheTTL）。
	statsMu    sync.Mutex
	statsCache []logbuf.Entry
	// ⚠⚠ 聚合产物的缓存（2026-10-18，治 `/admin/stats` 每次重算 28000 行那 ~3 秒）。
	//
	// 与上面 `statsCache` 的分工：
	//   statsCache 缓存的是**原始行** ⇒ 省掉"读盘"，但**每次请求仍要把全部行重新聚合一遍**；
	//   aggCache   缓存的是**聚合产物** ⇒ 连重算也省掉。
	//
	// ⚠⚠ 关键前提（打破它就会静默污染，见 statsAggCached 的注释）：
	//   `aggCache` 里的 map **只读共享**；嵌套值（所有 `by_*` / `by_*_detail`）
	//   **只被读、不被改**，所以调用方每次拿到的只需是**一层浅拷贝**。
	aggMu    sync.Mutex
	aggCache map[string]any
	aggAt    time.Time
	aggCount int // ⚠ 建它时的 entries 条数：条数变了就必须重建（日志在追加/轮转）
	// ⚠⚠ 建它时的**时间档**（2026-10-18 加）。
	// 为什么必须有：`?range=` 会改变参与聚合的条目集合，而**不同档完全可能条数相同**
	//（例如"本月"与"全部"在我们这种"数据全在当月"的场景下条数一样）
	// ⇒ 只用 `aggCount` 做键会让两个档**互相命中彼此的缓存** ⇒ 表面上"某个档不过滤"。
	// ⚠ 这正是我上一轮遇到的怪象（同一函数、同一字符串，测出来时好时坏）。
	aggRange    string
	aggErr      error
	aggBuilding bool
	statsSource string
	statsErr    error
	statsAt     time.Time

	// multMu / multTbl / multAt / multBuilding：**全上游合并后的倍率表**的缓存。
	//
	// ⚠⚠ 这一套是为一个实测缺陷加的：`/admin/stats` 里 `modelMultipliers` 会
	// **逐家上游**调 `ModelCatalog(id)`，而它可能同步回源（最多 MaxRotate 次上游请求）
	// ⇒ 实测单次 `/admin/stats` **7 秒**、而前端一次 refresh 里调两次 ⇒ 界面卡 14 秒。
	//
	// ⚠ 与 `statsMu` 那套的关键不同：这里**过期时不在请求里重建**，而是丢给后台 goroutine
	// 重建、本次先返回旧表。于是 requests 路径上永远只是"读一张内存表"（微秒级）。
	// ⚠ 仍会调用 `ModelCatalog`（只是在后台）—— 这正是避开"自锁死循环"的那个坑：
	// 若因为"怕慢"而**从不**调用它，目录缓存就永远不会被拉起来（见 modelMultipliers 的长注释）。
	multMu       sync.Mutex
	multTbl      map[string]float64
	multAt       time.Time
	multBuilding bool

	// issuedMu / issuedAuth：本进程**签发过**的授权链接（值 = 签发时刻）。
	//
	// # 为什么需要它（安全）
	//
	// 「再开一次无痕窗口」需要一条接受 URL 的端点。若接受任意 URL，
	// 这条端点就等于「让本机浏览器访问任意地址」—— 而本机浏览器里
	// 带着用户的全部登录态，打开攻击者给的地址就够钓鱼了。
	// 只认自己刚签发过的链接，把可打开的集合收窄到"我们发出的授权页"。
	//
	// 只存内存、进程重启即失效：授权链接的有效期本来就跟 state 绑定
	//（分钟级），长期保存反而是把过期凭证留在内存里。
	issuedMu   sync.Mutex
	issuedAuth map[string]time.Time
}

// New 构建管理台 handler。
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}

	h.register("GET /admin/accounts", h.accounts)
	h.register("POST /admin/accounts/reload", h.accountsReload)
	// 全上游通用的**批量粘贴导入**。
	//
	// 放在通用段而不是某个上游里（与 quota/refresh、checkin/all 同一取舍）：
	// 它是**所有上游共用**的动作，分派目标由请求体里的 provider 决定，
	// 而上游**只负责把那段 JSON 解释成自己的凭证**
	//（gateway.AccountImportExt）。详见 account_import.go。
	h.register("POST /admin/accounts/import", h.accountsImport)
	// 全上游通用的额度刷新（把上游自报的额度写回池）。
	//
	// 放在通用段而不是某个上游里：它是**所有上游共用**的动作，
	// 分派目标由每个账号自己的 provider 标签决定（见 quota_refresh.go）。
	h.register("POST /admin/accounts/quota/refresh", h.accountsQuotaRefresh)
	// 跨上游的**全量签到**：触发所有上游各自的全量签到（后台任务）。
	//
	// 与上面那条额度刷新是同一类端点：作用域是"整池 / 所有上游"，
	// 不表达任何单个上游的身份，所以属于核心 —— 上游之间互不可见，
	// 没人能自己遍历"所有上游"。
	//
	// 账号池顶部那个唯一的「全部签到」打它；各上游卡片顶部那个
	// 「全部签到」打的是上游自己的 AllURL（见 checkin_all.go）。
	h.register("POST /admin/accounts/checkin/all", h.accountsCheckinAll)
	h.register("POST /admin/accounts/{uid}/enable", h.accountEnable)
	h.register("POST /admin/accounts/{uid}/disable", h.accountDisable)
	h.register("POST /admin/accounts/{uid}/cooldown/clear", h.accountClearCooldown)
	h.register("DELETE /admin/accounts/{uid}", h.accountDelete)

	h.register("POST /admin/login/start", h.loginStart)
	h.register("POST /admin/login/poll", h.loginPoll)
	// 重新用无痕窗口打开**刚刚签发的**那条授权链接。
	//
	// 为什么需要这一条：start 已经会自动打开（默认无痕），但用户可能
	// 手滑关掉了窗口。此时让他重走 start 是错误的 —— 那会作废旧 state、
	// 并且上游那边多一次无用的授权申请。正确做法是重开同一条链接。
	h.register("POST /admin/login/open", h.loginOpenBrowser)

	h.register("POST /admin/models/refresh", h.modelsRefresh)
	h.register("GET /admin/models/preview", h.modelsPreview)

	h.register("GET /admin/logs", h.logs)
	h.register("GET /admin/logs/history", h.logsHistory)
	h.register("GET /admin/stats", h.stats)
	h.register("GET /admin/stats/series", h.statsSeries)
	// 系统状态页的取数端点（T12）：宿主资源 + 进程运行时长 + 依赖健康 + 请求口径。
	// 与上面几条同门禁（/admin/* 子树），实现见 system_status.go。
	h.register("GET /admin/system-status", h.systemStatus)

	h.register("GET /admin/settings", h.settings)
	h.register("PUT /admin/settings", h.settingsUpdate)

	// 核心调度相关的两条只读端点。
	//
	// # 为什么它们在通用段（而不是由上游自注册）
	//
	// 它们读的是**核心自己排的班**（签到/保活时点、下次唤醒、全量任务槽），
	// 不表达任何上游身份 —— 任何上游都该有这两条。
	//
	// Task 3c 曾把它们随另外 20 条搬进 workbuddy；阶段 0 评审指出
	// 第二个上游若不声明 CapCheckin 就**没人服务**这两条路由，
	// 而它们本该对所有上游可见。移回核心。
	//
	// 放在 mountUpstreamRoutes 之前是刻意的：通用端点优先，
	// 上游即便声明同名路由也无法覆盖（见 mountUpstreamRoutes 的冲突规则）。
	h.register("GET /admin/schedule", h.schedule)
	h.register("GET /admin/task", h.taskStatus)

	// 已注册上游清单 + 能力位（Task 8）。
	//
	// 前端按它渲染：账号池按上游分组、入口按**能力位**显隐。
	// 能力位由后端下发而不是前端硬编码 —— 否则加第三个上游还要改前端，
	// 那正是判据 1（加新上游核心零改动）要避免的。
	h.register("GET /admin/providers", h.providers)

	// 控制台渲染契约（Task 1）。
	//
	// 它把「有哪些上游、各自有什么能力、各自声明了哪些管理端点、有哪些定时任务」
	// 一次下发 —— 前端据此渲染导航与面板，**不认识任何上游名字**。
	// 放在 mountUpstreamRoutes 之前：通用端点优先，上游不得覆盖它。
	h.register("GET /admin/ui/manifest", h.uiManifest)

	// API key 管理（对标 new-api 令牌体系）：未注入 Store 时不注册路由。
	if h.cfg.APIKeys != nil {
		h.registerAPIKeys()
	}

	// 渠道管理（中转站方向：能力索引 + 每渠道模型映射）：**各自的判据**，同样"未注入就不注册"。
	// ⚠ 不能挂在上面那个 if 里 —— 渠道功能与 API key 体系是两件独立的事，
	// 共用一个门会让"只配了渠道、没配多 key 存储"的部署**静默看不到渠道管理**（路由 404，
	// 而前端按 manifest 只会觉得"这个功能不存在"，完全没有报错线索）。
	if h.cfg.Channels != nil {
		h.registerChannels()
	}

	// 分组管理（PLAN-groups.md P1）。同样是**独立判据**：没配分组存储就不注册。
	//
	// 为什么与渠道分开：分组是「这把 key 能用哪些模型」，渠道是「谁提供这个模型」，
	// 两件事可以单独启用。挂在一起会让「只有分组、没有渠道」的部署 404。
	if h.cfg.Groups != nil {
		h.registerGroups()
	}

	// 令牌（API Key）管理（Wave B4）：截图里「创建令牌」抽屉背后的语义。
	//
	// 判据比上面三条**更宽**：令牌数据在 SQL 真源里（store.DB），apikey.Store 只是
	// 同一张表的老视图 —— 只配了 SQL 存储的部署也要能用令牌管理，所以门开在
	// registerTokens 内部（APIKeys != nil || store.DB != nil），与「未注入就不注册」同一条约定。
	h.registerTokens()

	// 上游自注册的管理端点。放在最后：它**不得**覆盖上面的通用路由，
	// 所以冲突时以先注册的为准（见 mountUpstreamRoutes）。
	h.mountUpstreamRoutes()

	// ⚠⚠ 启动预热（2026-10-18）。实测：`/admin/stats` **首次调用 4634ms**，
	// 第二/三次 268ms / 248ms —— 那个 4.6 秒是**冷 IO**（`sink.LoadAll()` 读
	// 28000 行 + 首次全量解析），而它正好落在**用户打开仪表盘的第一眼**上。
	//
	// ⇒ 在进程启动后**空闲时先把这一遍跑掉**，把代价从首屏挪走。
	// ⚠ 这是本项目既有的做法，不是新发明：见 `config.go` 里
	//   "续期往返（实测 1-3 秒）。后台预热把这个代价挪到空闲时间"，
	//   以及 `main.go` 的 "预热能消掉『空闲后首个请求』"。
	//
	// ⚠ 三条约束（别改坏）：
	//   1. **只填缓存、不改行为**：`statsItems()` 本就是带 TTL 缓存的取数函数，
	//      这里调它一次等于把缓存先填好；后续请求走的是同一条路，看到的是同一份数据。
	//   2. **失败/panic 绝不影响启动**：读盘失败或边界情况都不该让进程起不来 ——
	//      出错时就当没预热过，下次请求自己会再试（有 `recover`）。
	//   3. **不 await**：启动流程不能为它多等哪怕一毫秒。
	go func() {
		defer func() { _ = recover() }()
		_, _, _ = h.statsItems()
		// ⚠⚠ 这里**刻意不再预热聚合** —— 试过了，实测无效，别再加回来：
		//   只预热取数：首屏 3078ms · 第二发 163ms
		//   连聚合也预热：首屏 3156ms · 第二发 231ms   ← 没有改善（差异属噪声）
		// ⇒ 结论：那 3 秒是**纯计算**（`aggregateChatLog` 逐行遍历 28000 条），
		//   **不是**首次的内存分配 / map 扩容 / GC —— 所以"先算一遍"救不了它。
		// ⇒ 真要把它降下来，只能**缓存聚合产物**（⚠ 而那会让
		//   "调用方就地改那个 map"（`resp["source"]=…`、`resp["model_multipliers"]=…`）
		//   变成污染问题，必须深拷贝 ⇒ 代价与风险都不小，**需要单独决定**）。
	}()

	return h
}

// mountUpstreamRoutes 遍历已注册上游，挂载它们声明的管理端点。
//
// # 为什么冲突时保留**先注册的**（通用端点）
//
// 通用端点在前，上游端点在后。若某个上游声明了一条与通用端点同名的路由，
// 保留通用端点并记日志 —— 静默覆盖会让"某个面板突然变成另一个上游的实现"，
// 那是最难查的一类故障。宁可少挂一条并让日志说话。
//
// 实现上必须**先探测再注册**：Go 的 ServeMux 在重复注册时会 panic，
// 而 panic 发生在注册过程中，届时已经注册的 pattern 无法回滚。
// 探测的代价是注册两次同一 pattern —— 由 muxConflict 里的 recover 兜住，
// 它自己不会真的占用路由（用 HandleFunc 注册后再注册会 panic，
// 所以探测走的是一个**独立的一次性 mux**，见下）。
func (h *Handler) mountUpstreamRoutes() {
	if h.cfg.Registry == nil {
		return
	}
	// taken 记录已占用的 pattern（含探测失败的）。
	// 用独立集合判断而不是靠 mux 的 panic 回滚：mux 是 append-only 的。
	taken := map[string]bool{}
	for _, p := range h.cfg.Registry.All() {
		ext, ok := gateway.ExtOf[gateway.AdminExt](p)
		if !ok {
			continue
		}
		for _, r := range ext.AdminRoutes() {
			if r.Path == "" || r.Handler == nil {
				log.Printf("admin: 上游 %s 声明了一条不完整的管理端点（path=%q method=%q），已跳过",
					p.ID(), r.Path, r.Method)
				continue
			}
			pattern := r.Path
			if r.Method != "" {
				pattern = r.Method + " " + r.Path
			}
			if taken[pattern] || h.routeConflict(pattern) {
				log.Printf("admin: 上游 %s 的管理端点 %s 与已有端点冲突，保留已有端点", p.ID(), pattern)
				continue
			}
			// 先试注册：若与**已有** pattern 冲突，ServeMux 会 panic。
			// 这里用一个独立的一次性 mux 做同样的注册来判定，
			// 避免把半注册的状态留在真正的 mux 上。
			h.mux.HandleFunc(pattern, r.Handler)
			taken[pattern] = true
		}
	}
}

// routeConflict 报告 pattern 是否与已注册的某条路由冲突。
//
// 手法：在一个独立的一次性 ServeMux 上同时注册**全部既有 pattern** 与该
// pattern。既有 pattern 不可能互相冲突（它们已经成功注册过），
// 所以唯一的 panic 来源就是新来的这一条 —— 正是我们要判定的东西。
func (h *Handler) routeConflict(pattern string) (conflict bool) {
	defer func() {
		if rec := recover(); rec != nil {
			conflict = true
		}
	}()
	probe := http.NewServeMux()
	for _, existing := range h.patterns {
		probe.HandleFunc(existing, h.conflictProbe)
	}
	probe.HandleFunc(pattern, h.conflictProbe)
	return false
}

// conflictProbe 只在冲突探测里被临时注册，永远不会被真正调用。
func (h *Handler) conflictProbe(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "占位路由")
}

// register 注册一条通用端点，同时记录 pattern 供冲突探测使用。
//
// 所有通用端点都经它注册 —— 少记一条就会让那条被上游静默覆盖。
func (h *Handler) register(pattern string, fn http.HandlerFunc) {
	h.mux.HandleFunc(pattern, fn)
	h.patterns = append(h.patterns, pattern)
}

// ServeHTTP 先做凭据校验，再进路由。
//
// 这里统一把请求体读完再回应：Go 的 http server 在 body 未被耗尽时会直接关闭连接
// 而非复用，客户端可能观察到 ECONNRESET（表现为「服务端日志成功、浏览器/脚本报连接重置」）。
// 不要求每个 handler 自己记得 drain，放在唯一入口一次做掉。
//
// # 门禁判据（2026-10 重做，用户报障「没授权却能改设置」）
//
// 改造前的判据是：
//
//	if !isLoopback(r.RemoteAddr) && !h.remoteAdminAuthorized(r) { 403 }
//	// 其中 remoteAdminAuthorized 认的是 **config.api_key**
//
// 两处都有问题，且会叠加成一个**完全敞开**的部署：
//
//	① 凭据就是对话接口那把 api_key —— 给任何客户端配一把 key 调对话，
//	   就同时把整个管理台交出去了（能改配置、删账号、轮换钥匙）
//	② `RemoteAddr` 只反映"**上一跳**是谁"。任何本机反代
//	   （nginx / caddy / frp / 各种内网穿透）从 127.0.0.1 连过来时，
//	   RemoteAddr 就是回环 ⇒ 判定为"本机" ⇒ 免凭据放行。
//	   于是"我只在局域网/公网暴露这个域名"变成了"管理台对全网敞开"。
//	③ /ui 在本机访问时还会把 api_key **明文注入页面**（见 server/ui 的
//	   injectKey 逻辑），所以那个"敞开的页面"自己就带着钥匙。
//
// 现在的判据是「任何请求都必须出示凭据」，两条路：
//
//	浏览器会话   /ui 用账号口令登录 → HttpOnly 会话 cookie
//	admin_token  机器凭据（脚本/CI）→ Bearer 或 ?token=，等价管理员
//
// ⚠ 两条路都只通向**管理员**：控制台只有一个账号，且它就是管理员
// （2026-10-19 用户新口径：成员角色已取消，见 canAccess 的注释）。
//
// 本机（含绑回环）**不再享受任何豁免**：回环地址只说明"上一跳是谁"，
// 不说明"你有没有权限" —— 把它当凭据正是上面 ② 那条洞。
// 没配任何凭据时不是"回落用 api_key"，而是"没有管理面"：去 /ui 建第一个
// 管理员账号（该入口只在账号库为空时出现一次），而不是把 api_key 当钥匙。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
	}()
	p, ok := h.principalOf(r)
	if !ok {
		// 401 而不是 403：语义是"你没有（有效）凭据"，前端据此跳登录页。
		// 改造前这里是 403（因为那时只有"令牌对不对"一种判据），而 403
		// 会让前端去显示"你没权限"而不是"请登录"。
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "管理接口需要授权。" + h.denyHint(r),
			"code":  "auth_required",
			"hint":  h.denyHint(r),
		})
		return
	}
	if !canAccess(p) {
		// 403 与 401 分开：**有**身份但角色不够。
		//
		// ⚠ 2026-10-19 单账号口径下这条分支**不可达**（非管理员身份在
		// account 那层就被拒了 ⇒ 上面那段 401 先返回）。保留而不删：
		// 它是 fail-closed 的那一半 —— 万一将来有别的身份来源接进
		// Identify，这里仍然只放行管理员，而不是"没写就默认放行"。
		// 文案里不再提"成员"：那个角色已经不存在了。
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": fmt.Sprintf("当前账号（%s，角色 %s）无权访问 %s。该功能仅管理员可用。",
				p.Username, account.RoleName(p.Role), r.URL.Path),
			"code":  "forbidden_role",
			"role":  p.Role,
			"scope": "admin",
		})
		return
	}
	h.dispatch(w, r, p)
}

// ---------------------------------------------------------------------------
// 审计：把管理面的**写操作**落一行 audit_log（Wave1-T07）
// ---------------------------------------------------------------------------
//
// 语义照行为重写自 hyxapi/middleware/audit.go:101-172（beginAdminAudit /
// finishAdminAudit）：只借判断，不抄代码；本仓的挂钩点是自己的门禁。
//
// # 为什么挂在 ServeHTTP 里，而不是每个 handler 自己记
//
// 与 hyxapi 把兜底审计内聚进鉴权链路同一个理由：**新增接口自动留痕**。
// 若让每个 handler 自己调一次，漏掉的那一个不会报错、不会有测试红 —— 它只是
// 「这条操作不在审计里」，而这正是审计最不能有的失效方式（要等到真出事、去查
// 的时候才发现那一条根本不存在）。
//
// # 挂钩点在门禁**之后**（这一点是判据，不是实现细节）
//
// 走到 dispatch 的请求已经过了 principalOf（身份）与 canAccess（角色）两道门，
// 所以落下来的每一条都带**可信的 actor**。挂在门禁之前会把 401/403 也记成
// 「某人做了某事」，而那时 principal 是零值 —— 审计里出现空 actor 比没有这一条
// 更糟（它看起来像一条事实，实际什么都没说明）。
//
// # 为什么「是否成功」要看响应体，而不只看状态码
//
// 本仓的成功判据在**响应体**里（writeJSON 的 "ok": true），失败同时体现为
// 非 2xx 与 {"error": ...}。只看状态码会漏掉「200 但 ok:false」这一类；
// hyxapi 的 auditResponseSuccess 出于同一原因优先解析响应字段（这里换成本仓
// 的 ok 字段名）。
//
// # 只对写方法生效
//
// GET/HEAD/OPTIONS 是读操作：记进审计表会让「谁改了什么」淹没在访问记录里。
// 副作用是好的：只包写方法的 ResponseWriter，静态资源与流式 GET 一概不经过它。

// auditBodyLimit 响应体的**采样**上限（判成功只需看开头）。
//
// 对齐 hyxapi 的 auditResponseWriter.maxSize = 64 KiB。超过就不再看体：
// 审计不允许为了记录而把大响应留在内存里。
const auditBodyLimit = 64 * 1024

// 审计列宽上限（对齐 models.go 的 AuditLog 标签）。
//
// ⚠ 这些数必须与 models.go 一致；不一致在 SQLite 上**看不出来**（SQLite 不强制
// varchar 长度），换 MySQL 才会在写入时截断/报错 —— 所以这里显式截断，把
// 「超长」变成可预期的行为，而不是依赖某个驱动的宽容度。
const (
	auditActorMax     = 64  // Actor      varchar(64)
	auditAuthMax      = 24  // AuthMethod varchar(24)
	auditCategoryMax  = 24  // Category   varchar(24)
	auditActionMax    = 128 // Action     varchar(128)
	auditIPMax        = 64  // IP         varchar(64)
	auditRouteMax     = 255 // Route      varchar(255)
	auditUserAgentMax = 512 // UserAgent  varchar(512)
)

// auditWriteMethod 报告方法是否属于「会改状态」的那一类（hyxapi 同款判据）。
func auditWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// auditRouteActions 路由模板 → 动作名（对齐 hyxapi 的 auditRouteActions）。
//
// # 为什么不能把路径原样当 action
//
// 路径里有**参数**（/admin/accounts/{uid}/enable 的 uid）：原样进表，同一类操作
// 会显示成 N 条互不相干的动作，审计页无法按动作筛选。
//
// # 未登记的路由回落 "generic" —— 这是**留痕**，不是漏记
//
// 上游经 gateway.AdminExt 自注册的端点（mountUpstreamRoutes 挂）在编译期不可能
// 被登记进来；回落保证它们照样有一条记录，只是动作名不精细。hyxapi 同样回落
// generic，并把 method/route 放进参数里（本仓不存参数：method/route 已经在列上）。
//
// ⚠ 这张表**只登记写方法**：读端点在 dispatch 里根本不会被审计，登记它们只会让
// 后来的人以为读操作也会留痕。
//
// ⚠ POST /admin/channels/reveal 是个「只读的写方法」（POST 但只回明文 key）：
// 它**照样留痕**，而且这是刻意的 —— 「谁把明文 key 取走了一次」正是最该有
// 记录的一类动作。
var auditRouteActions = map[string]string{
	// 账号池
	"POST /admin/accounts/reload":               "account.reload",
	"POST /admin/accounts/import":               "account.import",
	"POST /admin/accounts/quota/refresh":        "account.quota.refresh",
	"POST /admin/accounts/checkin/all":          "account.checkin.all",
	"POST /admin/accounts/{uid}/enable":         "account.enable",
	"POST /admin/accounts/{uid}/disable":        "account.disable",
	"POST /admin/accounts/{uid}/cooldown/clear": "account.cooldown.clear",
	"DELETE /admin/accounts/{uid}":              "account.delete",
	// 登录（category 由路由前导判成 login）
	"POST /admin/login/start": "login.start",
	"POST /admin/login/poll":  "login.poll",
	"POST /admin/login/open":  "login.open",
	// 设置与模型
	"PUT /admin/settings":        "settings.update",
	"POST /admin/models/refresh": "model.refresh",
	// 渠道
	"POST /admin/channels":             "channel.save",
	"POST /admin/channels/discover":    "channel.discover",
	"POST /admin/channels/delete":      "channel.delete",
	"POST /admin/channels/status":      "channel.status",
	"POST /admin/channels/reveal":      "channel.reveal",
	"POST /admin/channels/keys/add":    "channel.key.add",
	"POST /admin/channels/keys/status": "channel.key.status",
	"POST /admin/channels/keys/delete": "channel.key.delete",
	// 分组
	"POST /admin/groups":        "group.create",
	"POST /admin/groups/update": "group.update",
	"POST /admin/groups/delete": "group.delete",
	"POST /admin/groups/prune":  "group.prune",
	// 管理钥匙 / 管理令牌
	"POST /admin/apikeys":                "apikey.create",
	"POST /admin/apikeys/toggle":         "apikey.toggle",
	"POST /admin/apikeys/delete":         "apikey.delete",
	"POST /admin/apikeys/reset":          "apikey.reset",
	"POST /admin/apikeys/update":         "apikey.update",
	"POST /admin/apikeys/rotate-admin":   "admin_token.rotate",
	"POST /admin/apikeys/rotate-api-key": "apikey.rotate",
}

// auditRouteFor 把请求路径归一到**路由模板**，并给出动作名。
//
// # 为什么不用 net/http 的 Request.Pattern
//
// 那个字段（ServeMux 的匹配结果）是 **Go 1.23** 才进标准库的，而本模块的 go
// 指令是 1.22.5 ⇒ go vet 的 stdversion 分析器会报「requires go1.23 or later」，
// 于是 go test 直接红。改用这张显式的模板表做同一件事：行为可见、可测，且不
// 依赖工具链版本（本仓的 GOOS/GOARCH 与工具链都要能随便换）。
//
// 归一的收益是两条：① 审计页能按动作/路由聚合，而不是每个 uid 一条；
// ② 表里不出现内部标识符（models.go 要求 Route 存路由模板）。
func auditRouteFor(method, rawPath string) (route, action string) {
	if a, ok := auditRouteActions[method+" "+rawPath]; ok {
		return rawPath, a
	}
	want := strings.Split(rawPath, "/")
	for pattern, a := range auditRouteActions {
		pm, pp, ok := strings.Cut(pattern, " ")
		if !ok || pm != method {
			continue
		}
		got := strings.Split(pp, "/")
		if len(got) != len(want) {
			continue
		}
		match := true
		for i := range got {
			if strings.HasPrefix(got[i], "{") && strings.HasSuffix(got[i], "}") {
				continue
			}
			if got[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			return pp, a
		}
	}
	// 未登记（含上游自注册端点）：路径原样进表 —— 它**不含 query**
	// （r.URL.Path 里没有 ?a=b），所以不违反「query 不进审计表」。
	return rawPath, "generic"
}

// auditRecorder 记录状态码与响应体开头，供事后判定本次写操作是否成功。
//
// ⚠ 只在写方法上包一层（见 dispatch）：静态资源、SSE、文件下载都是 GET，
// 一概不经过它。
//
// Unwrap 让 http.ResponseController（Flush / Hijack / SetWriteDeadline）能穿透
// 到真正的 writer —— 缺了它，被包的 handler 一旦想 Flush 就会拿到
// "feature not supported"（Go 1.20+ 靠 Unwrap 识别包装层）。
type auditRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (a *auditRecorder) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	if room := auditBodyLimit - a.body.Len(); room > 0 {
		if room > len(b) {
			room = len(b)
		}
		a.body.Write(b[:room])
	}
	return a.ResponseWriter.Write(b)
}

// Unwrap 见上（签名必须逐字如此，http.ResponseController 才认）。
func (a *auditRecorder) Unwrap() http.ResponseWriter { return a.ResponseWriter }

// auditSucceeded 依据状态码与响应体判定是否成功。
//
//	① 状态码 >= 400 ⇒ 失败（本仓所有错误路径都带非 2xx 状态码）
//	② 响应体是 JSON 对象且**显式**带 ok ⇒ 以它为准（200 也可能是 ok:false）
//	③ 其余 ⇒ 按状态码（< 400 即成功）
//
// ⚠ 只有「字段存在」才采用它：把缺省值当 false 会把 204 / 空响应记成失败，
// 把缺省值当 true 会让 ok:false 记成成功 —— 两种都让审计说谎。
func auditSucceeded(status int, body []byte) bool {
	if status >= 400 {
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var resp struct {
			OK *bool `json:"ok"`
		}
		if err := json.Unmarshal(trimmed, &resp); err == nil && resp.OK != nil {
			return *resp.OK
		}
	}
	return true
}

// auditClientIP 审计用的客户端地址（**不含端口**）。
//
// ⚠ 刻意**不**读 X-Forwarded-For / X-Real-IP：本仓的立场是那些头只说明
// 「链路上有代理」，不构成可信来源（见 forwarded 的注释）。往审计里写一个
// 客户端可以任意伪造的 IP，比写「上一跳」更糟 —— 前者会被后续排查当成事实。
func auditClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return truncateRunes(host, auditIPMax)
}

// auditActor 审计里的「谁」。
//
// 令牌来源没有账号名（KindToken 不带 User），但 actor **不能为空**：空 actor 让
// 「谁改的」变成不可回答，而审计页正是靠它定位操作人。所以给机器凭据一个明确的
// 名字，而不是留空或写 unknown —— 后者会混进「真的没认出身份」那一类。
func auditActor(principal account.Principal) string {
	actor := principal.Username
	if actor == "" {
		if principal.Kind == account.KindToken {
			actor = "admin_token"
		} else {
			actor = "unknown"
		}
	}
	return truncateRunes(actor, auditActorMax)
}

// truncateRunes 按**字符**（不是字节）截断到列宽。
//
// ⚠ 按字节截会把一个多字节汉字切成两半 ⇒ 存进去是非法 UTF-8，导出与审计页
// 读它时表现为乱码，而不是「被截短的文本」（后者至少可读）。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// dispatch 执行路由，并在写操作上落一条审计。
//
// 它是 ServeHTTP 唯一的出口，且只在门禁全部通过之后被调用 —— 所以这里的
// principal 是可信身份（见本节顶部「挂钩点在门禁之后」）。
func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request, principal account.Principal) {
	if h.cfg.Audit == nil || !auditWriteMethod(r.Method) {
		h.mux.ServeHTTP(w, r)
		return
	}
	rec := &auditRecorder{ResponseWriter: w}
	defer func() {
		if panicked := recover(); panicked != nil {
			// handler 崩了：这次写操作**确实可能改了一半状态**，正是最该留痕的
			// 时候。不记它等于「出事的那一次没有审计」——恰好是审计最该回答的问题。
			if rec.status == 0 {
				rec.status = http.StatusInternalServerError
			}
			h.recordAudit(r, principal, rec)
			panic(panicked) // 不吞异常：原样交回 net/http（它负责 500 与日志）。
		}
		h.recordAudit(r, principal, rec)
	}()
	h.mux.ServeHTTP(rec, r)
}

// recordAudit 组装并投递一条审计。
//
// 写失败**只记日志**（审计不许反过来把主流程弄挂）：与 store.AppendLog 的调用
// 纪律一致，也与 hyxapi 把审计放在鉴权链路里兜底、失败不改变响应的做法一致。
func (h *Handler) recordAudit(r *http.Request, principal account.Principal, rec *auditRecorder) {
	status := rec.status
	if status == 0 {
		// handler 什么都没写就返回了：net/http 会补 200。
		status = http.StatusOK
	}
	route, action := auditRouteFor(r.Method, r.URL.Path)
	category := store.AuditCategoryOperation
	if strings.HasPrefix(route, "/admin/login/") {
		category = store.AuditCategoryLogin
	}
	err := h.cfg.Audit(store.AuditLog{
		At:         time.Now().Unix(),
		Actor:      auditActor(principal),
		AuthMethod: truncateRunes(principal.Kind, auditAuthMax),
		Category:   truncateRunes(category, auditCategoryMax),
		Action:     truncateRunes(action, auditActionMax),
		Method:     truncateRunes(r.Method, 16),
		Route:      truncateRunes(route, auditRouteMax),
		Status:     status,
		Success:    auditSucceeded(status, rec.body.Bytes()),
		IP:         auditClientIP(r),
		UserAgent:  truncateRunes(r.UserAgent(), auditUserAgentMax),
		// Params 刻意留空：审计不存请求体 / 参数 / query（见 models.go 的
		// AuditLog 注释）—— 「顺手存一点」会让这张表变成需要同等强度保护的副本。
	})
	if err != nil {
		log.Printf("admin: 审计写入失败（本次请求结果不受影响）: %v", err)
	}
}

// canAccess 门禁的角色判据：**只有管理员放行**。
//
// # ⚠ 2026-10-19 用户新口径：原先那张"成员白名单"（memberRules）已整张删除
//
// 删掉的那张表是"精确 (方法, 路径) 对"白名单，放行 /admin/apikeys* 与
// /admin/ui/manifest 给**成员**。单账号口径下它不可达：
//
//	控制台只有一个账号，且那个账号是管理员（建号只有 CreateBootstrap 一条路，
//	写死 RoleAdmin）；非管理员的历史记录在 Authenticate 与
//	SessionStore.Lookup 就被回 ErrMemberRetired ⇒ account.Identify 拿不到身份
//	⇒ 请求在上一层的 401 分支就结束了，**走不到** canAccess。
//
// # 为什么删而不是留着（"反正为假"）
//
//	① 它是**角色分裂的残留**：留着会被读成"成员还有身份、只是端点少了几个"，
//	   而那正是本次要消掉的概念；
//	② 白名单是"默认拒绝"的，删掉它**没有放宽任何一条通行判据** —— 反而更严：
//	   唯一身份是管理员，能过门禁就是全放行，非管理员一律 false。
//
// 判据不用删：`p.IsAdmin()` 是 fail-closed 的那一半（万一将来有别的身份
// 来源接进 Identify，这里仍然只放行管理员）。对应 admin.go 的 403 分支
// （code=forbidden_role）同样保留 —— 它是"有身份但角色不够"的唯一出口。
func canAccess(p account.Principal) bool {
	return p.IsAdmin()
}

// principalOf 解析请求身份。
//
// ⚠ 判据本身在 account.Identify（**唯一**一份）：`/ui` 在 internal/server、
// `/admin/*` 在本包，两处必须对"谁是谁"给出完全一致的答案。在这里另写一份
// 的后果是漂移 —— 而漂移的形态正是刚修完的那类漏洞（页面能开但接口敞开）。
//
// 本包只负责两件本包特有的决策：
//
//	h.adminTokenOK  传给 Identify 的 isToken 回调（读**当前**生效的令牌，
//	                所以轮换立即生效、旧副本立即失效）
//	canAccess       「必须是管理员」这一条（见上，成员白名单已删）
func (h *Handler) principalOf(r *http.Request) (account.Principal, bool) {
	return account.Identify(r, h.adminTokenOK, h.cfg.Accounts, h.cfg.Sessions)
}

// cfgHasAccounts 本实例是否装了控制台账号体系（提示文案用）。
func (h *Handler) cfgHasAccounts() bool {
	return h.cfg.Accounts != nil && h.cfg.Sessions != nil && h.cfg.Accounts.Count() > 0
}

// denyHint 给出一条**可执行**的说明，而不是让用户去猜。
//
// 四种情形的处置完全不同，混成一句话会让人反复试错的钥匙：
//
//	两样都没配      → 去 /ui 创建第一个管理员（该入口只出现一次）
//	只有账号        → 去 /ui 用账号口令登录
//	只有令牌        → 带 Bearer/query 令牌；浏览器登录需要先建账号
//	两样都有        → 二选一，说清各自的适用场景
//
// ⚠ 这里**不**再提"本机免令牌"：那条路径已经不存在了。
// 用户报障"错令牌也能进 UI"正是因为本机直连无条件放行 —— 现在回环地址
// 不再是凭据，绑回环只让"少暴露一个端口"这一条成立。
func (h *Handler) denyHint(r *http.Request) string {
	hasToken := h.currentAdminToken() != ""
	hasAccounts := h.cfgHasAccounts()
	proxyNote := ""
	if forwarded(r) {
		proxyNote = "（本请求带 X-Forwarded-* / X-Real-IP 头，像是经反向代理转发而来 —— " +
			"这不影响判定：无论直连还是经代理，凭据都必给。）"
	}
	switch {
	case hasToken && hasAccounts:
		return "浏览器请打开 /ui 用账号口令登录；脚本/CLI 用 Authorization: Bearer <admin_token>（或 ?token=<admin_token>）。" + proxyNote
	case hasAccounts:
		return "请打开 /ui 用**管理员**账号口令登录（本部署只有一个账号，就是它）。本机没有配置 admin_token。" + proxyNote
	case hasToken:
		return "带 Authorization: Bearer <admin_token> 或 ?token=<admin_token> 访问。" +
			"浏览器里的管理界面需要一个控制台账号：打开 /ui 创建第一个管理员。" + proxyNote
	default:
		return "本机既没有控制台账号也没有 admin_token：打开 /ui 创建第一个管理员账号" +
			"（这个入口只在账号库为空时出现一次）。" + proxyNote
	}
}

// forwarded 报告请求是否带有反向代理的转发痕迹。
//
// 这几个头都由代理**附加**，正常直连不该出现。出现即说明链路上有代理 ——
// 于是"本机直连"这个前提不成立（我们看不到真实客户端，只看到代理）。
func forwarded(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		if strings.TrimSpace(r.Header.Get(h)) != "" {
			return true
		}
	}
	return false
}

// adminTokenOK 给定凭据是否等于**当前**生效的管理令牌。
//
// # 与 api_key 的关系：**不认识**
//
// 这里刻意只比对 admin_token。改造前它比的是 config.api_key，
// 于是"给客户端配一把对话 key"等于"交出管理台"。现在两把钥匙各管一段：
//
//	api_key     → /v1/chat/completions、/v1/images/generations、/v1/models
//	admin_token → /admin/* 与 /ui 的远程访问
//
// 未配置 admin_token（空串）时恒 false —— 维持"没有远程管理面"的安全默认，
// 而**不是**回落到 api_key（那正是要修掉的权限倒挂）。
// adminTokenOK 给定凭据是否等于**当前**生效的管理令牌。
//
// # 与 api_key 的关系：**不认识**
//
// 这里刻意只比对 admin_token。改造前它比的是 config.api_key，
// 于是"给客户端配一把对话 key"等于"交出管理台"。现在两把钥匙各管一段：
//
//	api_key     → /v1/chat/completions、/v1/images/generations、/v1/models
//	admin_token → /admin/* 与 /ui 的远程访问
//
// 未配置 admin_token（空串）时恒 false —— 维持"没有管理面"的安全默认，
// 而**不是**回落到 api_key（那正是要修掉的权限倒挂）。
//
// ⚠ 签名收的是**凭据字符串**而不是 *http.Request：取值（Bearer → ?token= →
// 令牌 cookie）由 account.TokenFromRequest 统一负责，本函数只做比较。
// 这样"哪些字段算凭据"的知识只有一处，而本函数可以直接当
// account.Identify 的 isToken 回调传进去 —— 每次请求都读当前值，
// 所以轮换立即生效、浏览器里的旧 cookie 副本立即失效。
func (h *Handler) adminTokenOK(cred string) bool {
	want := h.currentAdminToken()
	if want == "" {
		return false
	}
	// 定长比较：避免按字节提前返回泄漏"前几位对了"的时序信息。
	return subtleEqual(cred, want)
}

// AdminTokenOK 导出版的令牌校验（内部判据仍是上面那个函数，不是复制品）。
//
// # 为什么需要它（而不是让调用方自己比字符串）
//
// 管理令牌是**运行时可变**的（`SetAdminToken`，轮换端点用），且判据里带着
// 定长比较（防时序侧信道）。如果 `account` 或 `cmd/server` 自己拿一个令牌值
// 去比，就同时复刻了两个问题：轮换之后那个副本是旧的（正是用户报障"要重启
// 才生效"的根因），以及比较退化成 `==`。
//
// 目前唯一的调用方是 `account.HTTPConfig.AdminTokenOK` —— 让 `session` 端点
// 能回答"我是谁"，从而令牌身份也能看到控制台（见 internal/account/http.go
// 里 session 那段"无限重载"的注释）。
func (h *Handler) AdminTokenOK(cred string) bool {
	return h.adminTokenOK(cred)
}

// SetAdminToken 运行时更新管理令牌（轮换端点用）。
//
// # 为什么必须有它（用户报障：轮换之后还要重启才有用）
//
// 改造前：门禁读 `h.cfg.APIKey`，那是 admin.New 时的**结构体快照**；
// 轮换端点写回了 config.json、也调了 OnAPIKeyRotated，但那个回调更新的是
// **另一个包**（server.Handler.apiKey）的内存副本。于是：
//
//	文件        → 新值（写盘成功）
//	门禁判据    → 旧值（cfg.APIKey 快照，永不更新）
//	表现为      → 用新钥匙被 403，用旧钥匙还能进 ⇒「要重启才生效」
//
// 现在门禁读这个可变字段，轮换时同步更新，**无需重启**。
func (h *Handler) SetAdminToken(k string) {
	h.adminTokenMu.Lock()
	h.adminToken = k
	h.adminTokenMu.Unlock()
}

// currentAdminToken 返回当前生效的管理令牌。
//
// 回落规则：字段为空（未调用过 SetAdminToken）时用构造时的 cfg.AdminToken。
// 这保证"没人轮换过"的普通部署行为不变，而轮换过的部署读新值。
func (h *Handler) currentAdminToken() string {
	h.adminTokenMu.RLock()
	v := h.adminToken
	h.adminTokenMu.RUnlock()
	if v != "" {
		return v
	}
	// ⚠ 这里**只**回落 cfg.AdminToken（不是 cfg.APIKey）。
	// 回落 api_key 就是改造前那个权限倒挂，绝不能回来。
	return strings.TrimSpace(h.cfg.AdminToken)
}

// subtleEqual 等时比较两个字符串（长度不同也走完整流程）。
//
// 管理令牌是**高权限**凭据，逐字节提前返回的比较会给爆破留一条侧信道。
// crypto/subtle 已经是标准库依赖，不引入新依赖树。
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// bearerOf 取出请求 Authorization 头里的 Bearer 凭据（无则空串）。
// ⚠ 本地不再保留 bearerOf：取值逻辑统一在 account.TokenFromRequest
// （Bearer → ?token= → 令牌 cookie）。本包若自己抄一份，就会出现
// "门禁认的字段"与"cookie 换手认的字段"不一致的漂移。

// ---------------------------------------------------------------------------
// 账号视图
// ---------------------------------------------------------------------------

// AccountView 是 pool.Status 的管理台增强视图（补 token 有效期、凭证文件、今日签到）。
// ModelCatalogState 模型目录缓存的只读状态视图（由 server 包注入的实现填充）。
//
// 三态语义（ok / stale / unavailable）刻意不做成 bool：
// 「有目录但已过 TTL」和「压根没拿到目录」对读报表的人是两件事 ——
// 前者可以继续用（数值可能不准），后者必须显示成缺数据。
type ModelCatalogState struct {
	State     string `json:"state"`      // ok | stale | unavailable
	Models    int    `json:"models"`     // 目录内模型数
	Stale     bool   `json:"stale"`      // 数据存在但已超出 TTL
	Cooldown  bool   `json:"cooldown"`   // 正处于失败负缓存冷却期
	FetchedAt string `json:"fetched_at"` // 最近一次成功拉取时间（RFC3339；无则空）
}

// ModelMultiplier 单模型的成本系数视图（credit 的放大倍数，来自 /v3/config）。
type ModelMultiplier struct {
	Model      string  `json:"model"`
	Multiplier float64 `json:"multiplier"`
	// Provider 该倍率所属的上游。多上游部署下每个上游的 /v3/config 是各自的事实，
	// 同名模型（如 workbuddy 与 workbuddy-intl 都有 glm-5.2）倍率互不相同，
	// 前端按 (provider, model) 组合查表。单上游（裸名）部署时省略。
	Provider string `json:"provider,omitempty"`
	// Calls 该模型在本次聚合窗口内的调用次数（来自 by_model），
	// 让前端能在同一条 chip 上同时说清「调了多少次」和「每次贵多少倍」。
	Calls int `json:"calls"`
}

// AccountView 是 pool.Status 的管理台增强视图（补 token 有效期、凭证文件、今日签到）。
type AccountView struct {
	pool.Status
	HasToken bool `json:"has_token"`

	// TokenExpireAt / TokenExpireSec 用**指针**表达"未知"。
	//
	// # 为什么必须是指针，而不是 int64 + omitempty（T5 的根因）
	//
	// 改造前是 `int64` + `omitempty`，于是：
	//
	//	ExpiresAt = 0（上游根本没给过期时间）
	//	  → TokenExpireSec = 0
	//	  → omitempty 把**整个字段**从 JSON 里删掉
	//	  → 前端 `a.token_expire_sec || 0` 得到 0
	//	  → 显示 "0d"
	//
	// 实测（2026-09-13，端口 18080）：
	//
	//	workbuddy: "token_expire_sec": 5154511      ← 有值才带
	//	codearts : （字段根本不存在）                 ← omitempty 吃掉
	//
	// 用户看到 codearts 那一格是 `0d`，读成"这个号的 token 还剩 0 天、
	// 马上就要过期"。**真相是它压根没有可读的过期时间** —— 界面把一个
	// "不知道"渲染成了一个具体的、且是惊悚的值。
	//
	// # 为什么不用哨兵值（-1 / math.MinInt64）
	//
	// 哨兵把"未知"编码进**数值域**，于是每个读它的人都必须先知道
	// "有个魔法数要特判"。漏判一处就退化成本 bug（-1 被当成"已过期 1 秒"），
	// 而且没有任何编译期/类型层面的保护 —— 它只是一条口头约定。
	//
	// 指针把"未知"编码进**类型**：
	//   · Go 侧：`nil` 与 `0` 在语言层面就不同，写 `*v.TokenExpireSec`
	//     前必须显式处理 nil 分支，漏了会 panic 而不是静默出错
	//   · JSON 侧：`nil` → 字段**不出现**（保持 omitempty 的下线形状，
	//     老前端读不到就还是 `undefined`，走 `|| 0` 的老路）；
	//     而 `0` → 显式输出 `"token_expire_sec":0`（真的刚过期）
	//
	// 即：**保留 omitempty 的线格式，但修好它的语义** ——
	// `字段不存在` 现在严格等价于 `未知`，`0` 只可能来自真实的 0。
	//
	// # 前端契约
	//
	//	undefined / null → 未知 → 显示 `—`
	//	< 0              → 已过期
	//	>= 0             → 按 N 天 / N 小时 / N 分钟 渲染
	TokenExpireAt  *int64 `json:"token_expire_at,omitempty"`
	TokenExpireSec *int64 `json:"token_expire_sec,omitempty"`

	// TokenNeverExpires 这份凭证**设计上就没有过期时间**（不是"我们不知道"）。
	//
	// # 为什么必须有第三个字段（用户实测报的缺口）
	//
	// 上面两个指针表达的是"有到期时刻"与"不知道"。但 loomy 的 session
	// 既不是某个时刻、也不是信息缺失 —— 它是**服务端持久化登录态，不绑时间**。
	// 只用两个指针，它只能落进"不知道" → 界面 `—` → 用户读成"这功能没做"。
	//
	// 真相是**信息不缺失**：结论就是"不会到期自动失效"。
	//
	// # 与 TokenExpireSec 互斥
	//
	// 有到期时刻时本字段恒为 false（见 accountViews 的填写顺序）：
	//   · 先问时刻（CredentialExpiryExt）
	//   · 只有拿不到时刻才问"是不是永久"（CredentialLifetimeExt）
	//
	// 反过来的后果是把一个 2 小时后失效的 STS 说成"永久"。
	//
	// # 前端契约
	//
	//	true             → 渲染「永久」（带 title 说明失效率来源）
	//	false / 缺字段   → 走原有逻辑（时刻 / `—`），行为逐字节不变
	TokenNeverExpires bool `json:"token_never_expires,omitempty"`

	File           string `json:"file,omitempty"`
	TodayCheckin   string `json:"today_checkin,omitempty"`
	TodayCheckinAt int64  `json:"today_checkin_at,omitempty"`

	// TodayWelfare 今天有没有领过福利（codearts 的每日动作）。
	//
	// # 取值与 today_checkin 同口径
	//
	//	"ok"      → 领到了（今天这次领到了东西）
	//	"skip"    → 点过，但一项都没领到
	//	"fail"    → 请求失败
	//	字段缺失  → **未知**（今天没点过，或该账号不属于任何有 welfare 的上游）
	//
	// ⚠ 缺失**不等于**"未领取"。上游的 `/admin/welfare` 只回 `claimable`，
	// 无法区分"今日已领完"与"资格不符"—— 所以界面在拿不到记录时显示 `—`，
	// 绝不写"未领取"（那是把未知说成事实）。
	//
	// # 为什么它的来源与 today_checkin 不同
	//
	// `today_checkin` 查的是 checkinlog 里的 `KindCheckin`；这里查 `KindWelfare`。
	// 两者是**不同上游的同位动作**（每天一次、点一下领东西），
	// 混用一个 kind 会让 codearts 被渲染成"已签到"——它根本没有签到端点。
	TodayWelfare   string `json:"today_welfare,omitempty"`
	TodayWelfareAt int64  `json:"today_welfare_at,omitempty"`

	// FreeQuota 会**按周期重置**的那部分额度还剩多少（Loomy 的「每日赠送」）。
	//
	// # 契约（三态，与「额度」列同一条纪律）
	//
	//	字段缺失        → 该上游不区分 / 这次没查到 → 界面 `—`
	//	指向 0          → 确实用完了
	//	指向 N > 0      → 还剩 N
	//
	// ⚠ 用**指针**而不是 int64：后者的零值无法与"没有这个信息"区分，
	// 而这两件事在界面上必须长得不一样 —— 把"没查到"显示成 0 会让用户
	// 得出「免费额度已用完、正在烧永久积分」这个**看起来完全合理但错误**的
	// 结论。（与 TokenExpireSec 用指针是同一条判据。）
	//
	// 来源：`pool.Status.Quota` 的 Free/FreeKnown ——
	// 即上游通过 `QuotaExt.RefreshQuota` 报上来的那部分。
	FreeQuota *int64 `json:"free_quota,omitempty"`

	// FreeQuotaLimit 「免费额度」这一项的**周期上限**（与 FreeQuota 配对下发）。
	//
	// # 为什么与 FreeQuota 分开两个字段而不是打包成一个对象
	//
	// FreeQuota 已经上线并被前端 `freeQuotaCellHTML` 读取。改成对象会让
	// 那一处（以及它的契约测试）全部失效 —— 而这里要表达的只是"再加一个数"。
	//
	// 三态：nil = 上游没给上限；非 nil = 显示「剩余 / 上限」。
	FreeQuotaLimit *int64 `json:"free_quota_limit,omitempty"`
}

// credentialExpiryOf 问**拥有这份凭证的上游**：它什么时候过期。
//
// # 为什么必须经过上游，而不是核心自己判
//
// 核心拿到的 secret 是**不透明**的（`any`，各上游结构完全不同：
// workbuddy 是 accessToken，codearts 是 AK/SK/SecurityToken 三元组）。
// 核心不许读它的字段（判据 3：核心不认识上游凭证结构）。
//
// 所以走 `gateway.CredentialExpiryExt` —— 与 `CredentialRefresher` 完全同构的
// "把凭证交回给它的上游，让上游自己解释"。
//
// # 三种"拿不到"
//
//	账号不存在 / 没有 secret      → 未知
//	provider 未注册（config 未启用）→ 未知
//	上游没实现 CredentialExpiryExt → 未知
//
// 三种都返回 false，前端一律显示 `—`。**不区分**它们：对界面来说
// "不知道什么时候过期"就是同一件事，把它们渲染成三种文案只会增加噪音。
// （排障需要区分时看日志与 /admin/providers，不该占界面。）
func (h *Handler) credentialExpiryOf(uid, providerID string) (int64, bool) {
	if providerID == "" || h.cfg.Pool == nil {
		return 0, false
	}
	secret, ok := h.cfg.Pool.SecretOf(uid)
	if !ok || secret == nil {
		return 0, false
	}
	p, ok := h.providerByID(providerID)
	if !ok {
		return 0, false
	}
	ext, ok := gateway.ExtOf[gateway.CredentialExpiryExt](p)
	if !ok {
		return 0, false
	}
	at, ok := ext.TokenExpiry(gateway.Credential{
		Provider: providerID,
		UID:      uid,
		Secret:   secret,
	})
	if !ok || at <= 0 {
		// at<=0 与 ok=false 同义（见接口注释）：绝不能把 0 当成
		// "1970 年就过期了" 渲染出去 —— 那比不显示更糟。
		return 0, false
	}
	return at, true
}

// credentialHasToken 问**拥有这份凭证的上游**：它带不带可用的 access token。
//
// # 与 credentialExpiryOf 完全同构
//
// 同样的三道未知（账号不存在 / 上游未注册 / 未实现扩展点）都返回 false
// —— 界面显示 `—`（未知），**不假装**"这个号没有 token"。
//
// # 为什么需要（用户实测：workbuddy-intl 登录后 token 列恒为 `—`）
//
// 账号列表的 has_token 原来只读核心投影 `Pool.AuthByUID()`。但按上游
// 分子目录之后，管理台两条入池路径交给池子的都是**裸投影**（只有
// UID/Nickname），真凭证走池的不透明 secret 通道 —— 于是第二个实例
// （workbuddy-intl）的投影里永远没有 token：
//
//	workbuddy-intl: has_token=false   ← 实测（secret 里明明有 accessToken）
//	workbuddy:      has_token=true    ← 默认上游，投影里就带着 token
//
// 核心不解释 secret（判据 3），所以问上游 —— 与 credentialExpiryOf 同一条。
func (h *Handler) credentialHasToken(uid, providerID string) bool {
	if providerID == "" || h.cfg.Pool == nil {
		return false
	}
	secret, ok := h.cfg.Pool.SecretOf(uid)
	if !ok || secret == nil {
		return false
	}
	p, ok := h.providerByID(providerID)
	if !ok {
		return false
	}
	ext, ok := gateway.ExtOf[gateway.CredentialTokenExt](p)
	if !ok {
		return false
	}
	return ext.HasToken(gateway.Credential{
		Provider: providerID,
		UID:      uid,
		Secret:   secret,
	})
}

// credentialNeverExpires 问**拥有这份凭证的上游**：它是不是设计上不过期。
//
// # 与 credentialExpiryOf 完全同构
//
// 同样的三道未知（账号不存在 / 上游未注册 / 未实现扩展点）都返回 false
// —— 对界面来说"不确定"与"有到期时间"一样，都只能显示 `—` 或时刻，
// **绝不能**显示「永久」。把不确定报成永久是**强断言**，说错了会让
// 用户以为手里的号永远可用（见 gateway.CredentialLifetimeExt 的注释）。
//
// # 只有拿不到到期时刻时才会被调用
//
// 调用点保证了两者互斥（见 accountViews）。这里再判一次没有意义 ——
// 只会多一个"谁先谁后"的第二份事实。
func (h *Handler) credentialNeverExpires(uid, providerID string) bool {
	if providerID == "" || h.cfg.Pool == nil {
		return false
	}
	secret, ok := h.cfg.Pool.SecretOf(uid)
	if !ok || secret == nil {
		return false
	}
	p, ok := h.providerByID(providerID)
	if !ok {
		return false
	}
	ext, ok := gateway.ExtOf[gateway.CredentialLifetimeExt](p)
	if !ok {
		return false
	}
	return ext.NeverExpires(gateway.Credential{
		Provider: providerID,
		UID:      uid,
		Secret:   secret,
	})
}

func (h *Handler) accountViews() []AccountView {
	list := h.cfg.Pool.List()
	today := checkinlog.TodayStart()
	out := make([]AccountView, 0, len(list))
	for _, st := range list {
		v := AccountView{Status: st}
		// 「免费额度」列的数据源：池里那份额度视图的分项。
		//
		// ⚠ 只在**上游真的报过分项**（FreeKnown）时才取址 —— 否则留 nil
		// ⇒ 字段不进 JSON ⇒ 前端显示 `—`。
		// 无条件取址会把"该上游不区分这个分项"渲染成一个确定的 `0`
		//（读作「免费额度已用完，正在烧永久积分」）—— 那是把未知说成事实，
		// 也正是这一列最容易骗人的形态（它看起来完全合理）。
		if st.Quota.FreeKnown {
			free := st.Quota.Free
			v.FreeQuota = &free
			// 上限只在**上游真的给了**（>0）时才下发，与 FreeQuota 同一条纪律：
			// 造一个 0 出来会让前端渲染成「剩余 / 0」，那是把未知说成事实。
			if st.Quota.FreeLimit > 0 {
				limit := st.Quota.FreeLimit
				v.FreeQuotaLimit = &limit
			}
		}
		if a := h.cfg.Pool.AuthByUID(st.UID); a != nil {
			// 必须问**上游**，不能直接读 a.AccessToken。
			//
			// # 这是一个真实的断点（实测）
			//
			// 上一版写的是 v.HasToken = a.AccessToken != "" —— 直接读通用凭证的
			// Bearer 字段。而只有 workbuddy 系的上游用那个形态：
			//
			//	monkeycode  session cookie + 自签密钥  → AccessToken 恒空 → 显示「无 Token」
			//	codearts    AK/SK/STS 三元组            → 同上
			//
			// 于是这些上游的账号在界面上全都像「凭证坏了」，尽管它们能正常对话。
			// 而 credentialHasToken（问上游）早就写好了、CredentialTokenExt 也早就
			// 定义了 —— 只是**这一行没调用它**。定义在、实现在、没人调，
			// 是本仓反复出现的一类失效。
			// ★ 顺序：**投影优先，上游兜底**。
			//
			// # 为什么不能直接问上游（第一版那样改，被测试拦住）
			//
			// 默认上游（workbuddy 系）的 token 一直在通用投影里（*auth.Auth.AccessToken）。
			// 若先问上游、而那个上游的 HasToken 恰好回 false，就会把投影里正确的
			// true 覆盖成 false —— 那是一次可见的回归（好号显示成无凭据）。
			//
			// # 为什么还要问上游（原来的写法只有前半句）
			//
			// 原来只读 a.AccessToken，于是不用 Bearer 形态的上游全都显示「无 Token」：
			//
			//	monkeycode  session cookie + 自签密钥   → AccessToken 恒空
			//	codearts    AK/SK/STS 三元组            → 同上
			//
			// 它们的账号在界面上像「凭证坏了」，尽管能正常对话 —— 实测用户会因此
			// 以为注册机导入失败，反复重导。
			v.HasToken = a.AccessToken != ""
			if !v.HasToken {
				// 投影里没有 → 问拥有这份凭证的上游（它自己才知道 secret 里有什么）。
				v.HasToken = h.credentialHasToken(st.UID, st.Provider)
			}
			// ⚠ 只在**上游真的给了**过期时间（ExpiresAt > 0）时才填指针。
			//
			// ExpiresAt == 0 的含义是"这个凭证没有可读的过期时间"
			//（codearts 的 STS 走另一套字段；某些凭证文件就是不带 exp），
			// 保持 nil → 字段不出现在 JSON 里 → 前端显示 `—`。
			//
			// 绝不要在这里写 `sec := a.ExpiresAt - time.Now().Unix()` 再无条件取址 ——
			// 那会在 ExpiresAt=0 时造出 `now` 量级的负值（约 -17.9 亿），
			// 前端会把它渲染成"已过期"，比原来的 `0d` 更糟。
			if a.ExpiresAt > 0 {
				at := a.ExpiresAt
				sec := a.ExpiresAt - time.Now().Unix()
				v.TokenExpireAt = &at
				v.TokenExpireSec = &sec
			}
			if a.FilePath != "" {
				v.File = filepath.Base(a.FilePath)
			}
		}
		// 过期时刻的**第二来源**：问上游（gateway.CredentialExpiryExt）。
		//
		// # 为什么必须有这条（用户实测：codearts 那一列一直是「—」）
		//
		// 上面那段读的是 `Pool.AuthByUID()` —— 那是**核心的通用凭证投影**
		// （`*auth.Auth`）。而 codearts 在池子里的真实持有形态是
		// **不透明的 secret**（`pool.SecretOf` 通道），投影里只有 {UID, Nickname}：
		//
		//	codearts: has_token=false, 没有 token_expire_sec   ← 实测
		//	workbuddy: has_token=true,  token_expire_sec≈5180335
		//
		// 于是 codearts 明明有 STS 有效期（约 2 小时一轮），界面上却显示「—」。
		//
		// # 为什么不是"把 ExpiresAt 补进那份投影"
		//
		// 那会立刻过期：续期**原地**更新 `*codearts.Auth`（secret 那份），
		// 而投影是启动时建的快照 —— 两者必然分叉。这正是上一轮刚修完的
		// "续期写到另一个对象上"的同一形态。
		//
		// 所以权威只能是活的 secret，而核心不许解释它（判据 3）。
		// 问**拥有它的上游** —— 与 RefreshCredential 完全同构。
		//
		// ★ has_token 的**第二来源**：问上游「这份凭证有可用 token 吗」。
		//
		// 上面读的是 `Pool.AuthByUID()` —— 核心的通用投影（`*auth.Auth`）。
		// 但按上游分子目录之后，管理台两条入池路径（accountsReload /
		// pollViaFlow）交给池子的都是**裸投影**（只有 UID/Nickname），
		// 真凭证走池的不透明 secret 通道。
		//
		// 于是第二个实例（workbuddy-intl）的账号投影里**永远没有 token**：
		//
		//	workbuddy-intl: has_token=false   ← 实测（secret 里明明有 accessToken）
		//	workbuddy:      has_token=true    ← 默认上游，投影里就带着 token
		//
		// 界面把「我们没在投影里读到」说成了「这个号没有 token」，
		// 用户据此以为登录失败 —— 而凭证完全正常。
		//
		// ⚠ 只在上面确实没读到 token 时才问（与 token_expire_sec 同一条判据）：
		// 默认上游的行为必须**逐字段不变**。
		pid := st.Provider
		if pid == "" {
			pid = h.cfg.DefaultProvider
		}
		if !v.HasToken {
			if h.credentialHasToken(st.UID, pid) {
				v.HasToken = true
			}
		}
		if v.TokenExpireSec == nil {
			if at, ok := h.credentialExpiryOf(st.UID, pid); ok {
				sec := at - time.Now().Unix()
				v.TokenExpireAt = &at
				v.TokenExpireSec = &sec
			}
		}
		// 到期信息的**第三态**：上游自报"这份凭证没有过期时间"（如 loomy 的 session）。
		//
		// ⚠ 只在**确实没有到期时刻**时才问。顺序不能反：
		// 反过来会把一个有明确失效时刻的凭证（codearts 的 2 小时 STS）
		// 渲染成「永久」—— 那是一个**更强、且错误**的断言。
		if v.TokenExpireSec == nil {
			v.TokenNeverExpires = h.credentialNeverExpires(st.UID, pid)
		}
		// 今日签到结果：从历史里取当天该 uid 的最近一条 checkin。
		if h.cfg.Log != nil {
			for _, rec := range h.cfg.Log.Since(today) {
				if rec.UID == st.UID && rec.Kind == checkinlog.KindCheckin {
					v.TodayCheckin = rec.Status
					v.TodayCheckinAt = rec.At.UnixMilli()
				}
				// 福利领取：**同一个循环里**再取一次，但按 KindWelfare 过滤。
				//
				// 不合成一个"最近一条每日动作"的判据 —— 两个 kind 属于
				// 不同上游，混起来会让 codearts 的领取显示成 workbuddy 的签到。
				//（上面 `today_checkin` 那条注释已经踩过一次这个形态。）
				if rec.UID == st.UID && rec.Kind == checkinlog.KindWelfare {
					v.TodayWelfare = rec.Status
					v.TodayWelfareAt = rec.At.UnixMilli()
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.accountViews(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"auth_dir":       h.cfg.AuthDir,
	})
}

// reloadProvider `AuthDir` 所属的上游标识。
//
// 配置了就用配置的；没配置回落默认上游，但**记一条日志** ——
// 静默回落正是"reload 按错误域剔除账号"这个 bug 当初能藏住的原因。
func (h *Handler) reloadProvider() string {
	if h.cfg.ReloadProvider != "" {
		return h.cfg.ReloadProvider
	}
	log.Printf("admin: 未配置 ReloadProvider，回落到默认上游 %q —— "+
		"多上游部署下应显式配置，否则可能按错误的域剔除账号", h.cfg.DefaultProvider)
	return h.cfg.DefaultProvider
}

// credentialsOf 找到某个上游的凭证加载器（`gateway.CredentialLoader`）。
//
// 返回 (加载函数, 上游是否存在且实现了加载器)。
//
// # 为什么要经过扩展点，而不是核心写死解析器
//
// 核心原来写死用 `auth.LoadDirCompat` —— 那**恰好**是 workbuddy 的格式，
// 所以 workbuddy 那条路径"碰巧是对的"，而 codearts 那条**彻底坏了**
// （用户实测：重载 codearts 时扫到的是 3 个 workbuddy 旧凭证）。
//
// 凭证格式是**上游的事实**（与凭证目录同理），由上游自报：
//
//	workbuddy → auth.LoadDirCompat（`workbuddy*.json`）
//	codearts  → codearts.LoadDir（`codearts*.json`）
//
// 这样核心不解释任何上游的凭证格式，**加新上游核心零改动**。
//
// 上游没实现 → 返回 false，调用方**明确报错**而不是回落成某个写死的解析器
// （回落正是上面那个 bug 的形态）。
func (h *Handler) credentialsOf(providerID string) (func(string) ([]gateway.Credential, error), bool) {
	p, ok := h.providerByID(providerID)
	if !ok {
		return nil, false
	}
	ext, ok := gateway.ExtOf[gateway.CredentialLoader](p)
	if !ok {
		return nil, false
	}
	return ext.LoadCredentials, true
}

// credentialSecretsOf 取该上游的「带 secret」扫描器（**可选**能力）。
//
// 没实现 `gateway.CredentialSecretLoader` 时返回 false —— 调用方回落成
// `credentialsOf` + `SyncToDirFor`。对凭证就装在 `*auth.Auth` 里的上游
// （workbuddy）那正是正确形态：池子自己保存的就是它的凭证。
//
// 核心在这里依然不认识任何上游类型：secret 是不透明的 `any`，
// 由 callers 原样交给池子保管（见 gateway.CredentialSecret 的注释）。
func (h *Handler) credentialSecretsOf(providerID string) (func(string) ([]gateway.CredentialSecret, error), bool) {
	p, ok := h.providerByID(providerID)
	if !ok {
		return nil, false
	}
	ext, ok := gateway.ExtOf[gateway.CredentialSecretLoader](p)
	if !ok {
		return nil, false
	}
	return ext.LoadCredentialsWithSecrets, true
}

// poolAuthsFromCreds 把扫描到的凭证投影成账号池条目的形状。
//
// # 为什么必须是**一个**共用函数（而不是各路径自己写一遍）
//
// 两条入池路径（accountsReload 的「重载 auths」、oauthFlow 的「登录成功后入池」）
// 原先各自内联了一份一模一样的投影。结果是同一个缺陷修了一条、漏了另一条：
//
//	第一次：reload 路径投影掉 token → 导入的号报「无可用凭证」（已修）
//	第二次：oauth 路径**照抄了同样的投影** → OAuth 登录的新号额度探测恒 401、
//	        旅行/成长报「无可用凭证」，而重启网关即恢复
//
// 抽成一处之后，投影规则只有一个来源，加第三条第入池路径时也不会再漏。
//
// # 投影规则
//
// 默认投影成 uid + nickname（池条目只需要身份；真凭证走 secret 通道，
// 这是 codearts 那类"凭证不在 *auth.Auth 里"的上游的形态）。
//
// ⚠ 例外：secret 本身就是**带凭证的 `*auth.Auth`** 时，直接用它当池条目。
// 这是 workbuddy 这类"凭证即 *auth.Auth"上游的形态
// （见 workbuddy.LoadCredentialsWithSecrets：Secret 就是同源的 *auth.Auth）。
//
// 为什么必须这样：workbuddy 的取号点（creds() / authOf() / 各处 AuthByUID）
// **只读池条目的 e.a，不读 secret 通道**。若 e.a 是空投影，RefreshToken 为空
// → 一律判定「无可用凭证」。而额度刷新/签到走 SecretOf 那条路，于是表现为
// "凭据明明能用、部分功能却报无可用凭证"这种最难查的割裂。
//
// pool 侧那条 `!authHasCreds(a) && authHasCreds(e.a)` 保护覆盖不到：
// 新号首次入池时，池里那份本来就是空投影（两边都无凭证）→ 保护不生效。
//
// 对 secret 非 *auth.Auth 的上游（codearts 的 STS 等），类型断言不成立，
// 仍走投影路径 → 行为与改动前逐字相同。
func poolAuthsFromCreds(creds []gateway.Credential, secrets map[string]any) []*auth.Auth {
	auths := make([]*auth.Auth, 0, len(creds))
	for _, c := range creds {
		if c.UID == "" {
			continue
		}
		if secrets != nil {
			if sa, ok := secrets[c.UID].(*auth.Auth); ok && sa != nil &&
				(sa.AccessToken != "" || sa.RefreshToken != "") {
				auths = append(auths, sa)
				continue
			}
		}
		auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname, FilePath: c.FilePath})
	}
	return auths
}

// accountsReload 重新扫描 auths 目录并对齐池（手工拷入凭证后无需重启网关）。
//
// # ⚠ 必须显式指定**这个目录属于哪个上游**
//
// 早先这里调的是裸 `SyncToDir(auths)`，它归一成"**当前默认上游**"。
// 那个语义在本部署里"碰巧"正确 —— 因为 `h.cfg.AuthDir` 只装 workbuddy 凭证，
// 而 workbuddy 恰好是第一个注册的（于是也是默认）。
//
// 评审证明了这是个**潜在 bug**：一旦默认上游变成 codearts（改注册顺序、
// 或将来有第三个上游先注册），同一个 reload 调用会：
//
//	扫描的是 workbuddy 文件（`auth.LoadDir` 只 glob workbuddy*.json）
//	却按 codearts 的域去剔除 → **把 codearts 账号全部删掉并落盘**。
//
// 所以域必须由配置显式给出，不能依赖"谁是默认"。
// 缺省回落到 DefaultProvider 只为兼容旧装配（并会在日志里留痕）。
//
// # 请求体里的 provider：这条按钮终于有了它本该有的语义
//
// 上面那段只解决了"域不能被默认上游带跑偏"，但**域本身仍是配置里的
// 一个固定值**（`ReloadProvider` = workbuddy）。于是账号池页面上
// codearts 那行的「重载 auths」按钮**扫的一直是 workbuddy 目录**，
// 而 toast 却说「对齐 codearts」。
//
// 这不是"少个功能"，而是**这条按钮的存在意义被架空了** ——
// 它的用途正是"往 `auths/<provider>/` 手工拷入凭证后不用重启网关"
// （见 auth.UpstreamDir 的注释），而对 codearts 来说它**根本不工作**，
// 界面还说成功了。用户实测的回执是三条一模一样的 `{"provider":"workbuddy"}`，
// 连不存在的上游 `ghost` 都被默默接受。
//
// 所以要害不只是"读请求体"，而是**读不到时必须失败**（见下面的 404 分支）。
func (h *Handler) accountsReload(w http.ResponseWriter, r *http.Request) {
	// 宽容解析：空体 / 坏体都**不算错**。
	//
	// 老客户端（以及缓存里的旧页面）发的是 `{}` 或空体 ——
	// 解析失败不该让这条端点挂掉，那会把"重载"整个功能打没。
	// 与 accountDisable / loginPoll 同一范式。
	var body struct {
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// 没带 provider = 老调用方：行为**逐字不变**（配置回落）。
	// 这是向后兼容，不是重复实现 —— `reloadProvider()` 的语义一个字没动。
	provider := body.Provider
	if provider == "" {
		provider = h.reloadProvider()
	} else if _, ok := h.providerByID(provider); !ok {
		// ⚠ 显式传了 provider 却查不到 → **404**，绝不回落默认目录。
		//
		// 回落正是要修的那个 bug 形态：用户点 `ghost` 那行、
		// 或前端传了个拼错的上游名，界面会收到 200 + 一个别的上游的
		// scanned —— 看起来"重载成功"，实际动的是别人的池子。
		// **宁可失败得刺眼，也不要成功得可疑。**
		//
		// 404 而不是 400：这是"这个上游不存在"（资源问题），
		// 不是"provider 字段格式不对"（参数问题）。
		writeError(w, http.StatusNotFound, "上游不存在: "+provider)
		return
	}

	// 目录解析 + 扫凭证 + 灌池，全在 `reloadProviderInto` 里 ——
	// ⚠ 与**启动时的同步共用同一份实现**（见 pool_sync.go 的注释）。
	//   抽它的理由不是「好看」，而是：抄第二份的代价是**将来只改对一处**，
	//   而本文件下面那段历史（LoadDirCompat 扫错上游）就是这么来的。
	scanned, before, after, dir, err := h.reloadProviderInto(provider)
	if err != nil {
		switch {
		case errors.Is(err, errNoCredentialLoader):
			writeError(w, http.StatusNotImplemented,
				"上游 "+provider+" 没有实现 gateway.CredentialLoader，"+
					"无法按它自己的格式扫描凭证（核心不硬编码任何上游的凭证格式）")
		default:
			writeError(w, http.StatusInternalServerError, "读取凭证失败: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned": scanned, "before": before, "after": after, "provider": provider,
		// dir 回执：排障时「我点的那个上游到底扫了哪个目录」一眼可见。
		"dir": dir,
	})
}

func (h *Handler) accountEnable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !h.cfg.Pool.Enable(uid) {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	log.Printf("admin: enable %s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": false})
}

func (h *Handler) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Reason == "" {
		body.Reason = "manual disable"
	}
	if _, ok := h.cfg.Pool.Status(uid); !ok {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	h.cfg.Pool.Disable(uid, body.Reason)
	log.Printf("admin: disable %s reason=%s", uid, body.Reason)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": true, "reason": body.Reason})
}

func (h *Handler) accountClearCooldown(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !h.cfg.Pool.ClearCooldown(uid) {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	log.Printf("admin: clear cooldown %s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "cleared": true})
}

// accountDelete 见 account_delete.go（2026-10-19 从本文件搬出去：那个缺陷值得
// 一个独立文件把「为什么静默空转」讲清楚，而本文件已经太长）。

// ---------------------------------------------------------------------------
// 添加账号（OAuth 设备授权）
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 添加账号（按上游分派）
//
// # 为什么要按上游分派
//
// 添加账号是**上游专属**动作：workbuddy 是 OAuth 设备码，
// codearts 是 OAuth + DPoP（还要生成密钥对、签名请求、~2 小时凭证）——
// 交互步骤数都不同（见 gateway.LoginFlow 的注释）。
// 前端"账号池"的每个上游分组行都有自己的「＋ 添加账号」，
// 点哪个上游就该走哪个上游的流程。
//
// # 向后兼容
//
// 请求体不带 `provider` 时，**完全走原路径**（`h.cfg.OAuth`，
// 也就是装配时注入的那个客户端）。现有前端与脚本不受影响。
//
// # 找不到该上游的 LoginFlow 时返回 501
//
// 不是 500 也不是静默回落 —— "这个上游不支持页内添加"是**能力问题**，
// 501 Not Implemented 是准确的语义。静默回落到默认上游更糟：
// 用户在 codearts 那行点「添加账号」，账号会加进 workbuddy。
// ---------------------------------------------------------------------------

func (h *Handler) loginStart(w http.ResponseWriter, r *http.Request) {
	// provider 可选：不带就走旧的硬接线路径（向后兼容）。
	var body struct {
		Provider string `json:"provider"`
		// Options 用户在弹窗里选的登录选项（上游自报 LoginOptionsExt 时才有）。
		//
		// 形状：{"region":"intl"}。没有选项的上游会收到 nil，行为不变。
		Options map[string]string `json:"options"`
	}
	// 解析失败不算错误 —— 老客户端发的可能是 `{}` 或空体。
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.Provider != "" {
		// 指定了 provider → 必须有它自己的 LoginFlow，否则明确 501。
		flow, ok, _ := h.wantsFlowDispatch(body.Provider)
		if !ok {
			writeError(w, http.StatusNotImplemented,
				"上游 "+body.Provider+" 不支持在页面内添加账号（未实现登录流程）")
			return
		}
		// ⚠ 优先走「带选项」的入口（如果该上游实现了它）。
		//
		// 两个接口的分工：
		//
		//	LoginFlow.Start()                     无参数，老上游照旧
		//	gateway.LoginFlowWithOptions.StartWith()  带用户选的选项
		//
		// 用**两个**而不是把 Start 改成带参数：后者要动 12 个上游的签名，
		// 而它们都不需要选项 —— 那是一次纯粹的连带改动。
		var state, authURL string
		var err error

		// 优先问**同一个 Provider 实例**有没有带选项的入口。
		// （不能对 flow 做断言：ExtOf 要的是 Provider，而 flow 只是 LoginFlow。）
		if prov, pok := h.providerByID(body.Provider); pok {
			if wf, ook := gateway.ExtOf[gateway.LoginFlowWithOptions](prov); ook {
				state, authURL, err = wf.StartWith(body.Options)
			} else {
				state, authURL, err = flow.Start()
			}
		} else {
			state, authURL, err = flow.Start()
		}
		if err != nil {
			writeError(w, http.StatusBadGateway, "向上游申请授权链接失败: "+err.Error())
			return
		}
		log.Printf("admin: oauth start provider=%s state=%s", body.Provider, shortState(state))
		browser, browserErr := h.openAuthBrowser(authURL)
		resp := map[string]any{
			"state":    state,
			"auth_url": authURL,
			"provider": body.Provider,
			// 各上游的 state 有效期可能不同；LoginFlow 接口没暴露它，
			// 这里沿用核心的 oauth.StateTTL（前端只用它做倒计时提示）。
			"expires_in_sec": int64(oauth.StateTTL().Seconds()),
		}
		// 扫码流程（raccoon 那类）：把 auth_url 编成二维码一起下发。
		//
		// ⚠ 判据是**上游自报**（QRLoginExt），不是"链接长得像不像二维码"
		// —— 后者要求前端认识上游或 URL 形态，而"加一个上游时前端一行
		// 都不该改"是本项目最硬的禁忌。判错了也不报错，只会静默渲染出
		// 一个扫不了的链接（正是用户报的这个缺陷）。
		//
		// 走这条分支时**不开浏览器**：扫码流程的链接是给手机扫的，
		// 在本机打开它只会看到"请在微信中打开"之类的页面。
		if qr := qrLoginSVGOf(h.cfg.Registry, body.Provider, authURL); qr != "" {
			resp["qr_svg"] = qr
			// 有二维码时不下发 browser/browser_error：让前端聚焦"扫码"，
			// 而不是同时看到"已用浏览器打开"这种与扫码无关的提示。
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// browser / browser_error 只在有话说的时候出现 ——
		// 未接线时响应里多两个空字段会让前端去渲染一个不存在的动作。
		if browser != "" {
			resp["browser"] = browser
		}
		if browserErr != "" {
			resp["browser_error"] = browserErr
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 不带 provider：旧路径（向后兼容）。
	//
	// ⚠ 若没配 OAuth 客户端就明确报 503，**不能 panic** ——
	// 老客户端发的就是不带 provider 的请求，崩了整片功能都没了。
	if h.cfg.OAuth == nil {
		writeError(w, http.StatusServiceUnavailable,
			"未配置默认 OAuth 客户端；请指定 provider（该上游需实现登录流程）")
		return
	}
	state, authURL, err := h.cfg.OAuth.Start()
	if err != nil {
		writeError(w, http.StatusBadGateway, "向上游申请授权链接失败: "+err.Error())
		return
	}
	log.Printf("admin: oauth start state=%s", shortState(state))
	browser, browserErr := h.openAuthBrowser(authURL)
	resp := map[string]any{
		"state":          state,
		"auth_url":       authURL,
		"expires_in_sec": int64(oauth.StateTTL().Seconds()),
	}
	if browser != "" {
		resp["browser"] = browser
	}
	if browserErr != "" {
		resp["browser_error"] = browserErr
	}
	writeJSON(w, http.StatusOK, resp)
}

// qrLoginSVGOf 取某上游自报的登录二维码 SVG（未实现该扩展点则空串）。
//
// # 为什么按 provider 反查注册表
//
// `loginStart` 拿到的是 `gateway.LoginFlow`（接口），而 QRLoginExt 挂在
// **Provider** 上 —— LoginFlow 的返回值看不见它。所以这里按 id 从注册表
// 取回 Provider，再做类型断言。
//
// ⚠ 编不出来（内容过长等）时返回空串，调用方回落到"显示链接"——
// 不产出扫不出来的坏码。
func qrLoginSVGOf(reg *gateway.Registry, providerID, authURL string) string {
	if reg == nil || providerID == "" || authURL == "" {
		return ""
	}
	p, ok := reg.Get(providerID)
	if !ok {
		return ""
	}
	ext, ok := gateway.ExtOf[gateway.QRLoginExt](p)
	if !ok {
		return ""
	}
	return ext.QRLoginSVG(authURL)
}

// issuedAuthTTL 已签发授权链接的可重开窗口。
//
// 不需要很长：授权链接的有效期由上游的 state 决定（分钟级），
// 过期之后重开也只会看到一个"state 无效"的页面。2h 足够覆盖
// "用户去干别的、回来接着弄"的场景，又不会把过期凭证长期留在内存。
const issuedAuthTTL = 2 * time.Hour

// openAuthBrowser 记录本次签发的授权链接，并**默认用无痕窗口打开**它。
//
// # 为什么默认就开（而不是等用户点）
//
// 用户的要求是「默认打开的就是无痕模式」。若把"用哪个窗口"留给前端
// 的 `target=_blank`，那开的窗口属于用户当前的浏览器状态 ——
// OAuth 会拿**已登录的账号**完成授权，账号串号，而整个过程"成功"了，
// 没有任何报错。所以打开动作必须由网关自己做，且默认无痕。
//
// # 失败**不**改变主流程
//
// 无 GUI 的服务器、受策略限制的进程都可能开不起来。此时授权链接照常
// 返回（那是用户唯一的退路），另外把失败原因如实回传，让界面显示
// 「未能自动打开，请复制链接到无痕窗口」。两种更糟的写法：
//   - 整个请求 5xx → 用户连链接都拿不到；
//   - 静默吞掉 → 界面显示"已用无痕窗口打开"而窗口不存在。
//
// 返回 (展示名, 错误文本)；两者最多一个非空。
func (h *Handler) openAuthBrowser(authURL string) (browser, browserErr string) {
	if authURL == "" {
		return "", ""
	}
	// 先记账再打开：即使打开失败，"这条链接是本进程签发的"这个事实
	// 仍然成立，用户还可以走 /admin/login/open 重试。
	h.noteIssuedAuthURL(authURL)
	if h.cfg.OpenAuthURL == nil {
		return "", ""
	}
	name, err := h.cfg.OpenAuthURL(authURL)
	if err != nil {
		log.Printf("admin: 无痕打开授权页失败（连接照常返回，请手动复制）: %v", err)
		return "", err.Error()
	}
	if name == "" {
		name = "浏览器"
	}
	log.Printf("admin: 已用无痕窗口打开授权页（%s）", name)
	return name, ""
}

// noteIssuedAuthURL 记下一条"本进程签发的"授权链接（顺便清理过期的）。
func (h *Handler) noteIssuedAuthURL(authURL string) {
	if authURL == "" {
		return
	}
	now := time.Now()
	h.issuedMu.Lock()
	defer h.issuedMu.Unlock()
	if h.issuedAuth == nil {
		h.issuedAuth = make(map[string]time.Time, 4)
	}
	for k, t := range h.issuedAuth {
		if now.Sub(t) > issuedAuthTTL {
			delete(h.issuedAuth, k)
		}
	}
	h.issuedAuth[authURL] = now
}

// isIssuedAuthURL 判断这条链接是不是本进程刚刚签发过的。
func (h *Handler) isIssuedAuthURL(authURL string) bool {
	h.issuedMu.Lock()
	defer h.issuedMu.Unlock()
	t, ok := h.issuedAuth[authURL]
	if !ok {
		return false
	}
	return time.Since(t) <= issuedAuthTTL
}

// loginOpenBrowser 用无痕窗口**重新**打开一条已签发的授权链接。
//
// 见路由注册处的注释：用于"用户手滑关掉了窗口"这种情况，
// 避免重走 start（那会作废旧 state 并多申请一次授权）。
//
// ⚠ 只认 `isIssuedAuthURL` 里的链接。这条端点的入参是 URL，
// 而它最终会把 URL 交给本机浏览器打开 —— 不加这道校验，
// 它就等于「让本机浏览器访问任意地址」，而那是在用户带着
// 全部登录态的浏览器里打开攻击者给的页面。
func (h *Handler) loginOpenBrowser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体无法解析: "+err.Error())
		return
	}
	authURL := strings.TrimSpace(body.URL)
	if authURL == "" {
		writeError(w, http.StatusBadRequest, "缺少 url")
		return
	}
	// 能力检查放在链接校验**之前**：没接线 opener 的部署
	// 无论传什么链接都做不到，"能力不存在"（501）才是准确语义。
	if h.cfg.OpenAuthURL == nil {
		writeError(w, http.StatusNotImplemented,
			"本部署未启用「自动打开浏览器」（配置里关掉了 login_open_browser，或运行在无 GUI 环境）")
		return
	}
	if !h.isIssuedAuthURL(authURL) {
		writeError(w, http.StatusForbidden,
			"只允许打开本网关刚刚签发的授权链接（未签发或已过期）")
		return
	}
	name, err := h.cfg.OpenAuthURL(authURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "打开浏览器失败: "+err.Error())
		return
	}
	if name == "" {
		name = "浏览器"
	}
	log.Printf("admin: 已用无痕窗口重新打开授权页（%s）", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "browser": name})
}

// wantsFlowDispatch 判断这次请求是否应该走「上游自己的 LoginFlow」。
//
// # 这里曾经写错过（测试抓到的）
//
// 第一版是 `body.Provider != h.providerOfDefaultOAuth()`，而
// `providerOfDefaultOAuth()` 的两个分支**返回同一个值** —— 于是
// "provider == DefaultProvider" 时条件恒假，请求掉进旧路径，
// 而测试里没配 OAuth 客户端 → **panic**。
//
// 更要紧的是语义：**指定的上游实现了 LoginFlow 就该走它**，
// 与"它是不是默认上游"无关。默认上游同样可能（且应该）用自己的 flow。
// `h.cfg.OAuth` 只是**过渡期**的旧客户端，不该抢在 flow 前面。
//
// 所以判据只有一条：**该上游有没有实现 LoginFlow**。
//
//   - 指定了 provider 且它有 LoginFlow → 走 flow
//   - 指定了 provider 但它没有      → 501（明确失败，不回落）
//   - 没指定 provider               → 旧路径（向后兼容）
func (h *Handler) wantsFlowDispatch(providerID string) (gateway.LoginFlow, bool, bool) {
	// 返回 (flow, 有flow, 该上游存在)
	if providerID == "" {
		return nil, false, false
	}
	if h.cfg.Registry == nil {
		return nil, false, false
	}
	for _, p := range h.cfg.Registry.All() {
		if p.ID() != providerID {
			continue
		}
		// ⚠ 两道判据缺一不可：
		//   1. `ExtOf` —— 该上游把 Start/Poll 挂上了（类型层面的能力）
		//   2. `Configured` —— 这次部署**真的配了**登录客户端
		//
		// 只看第 1 条会让"没配 OAuth 的部署"也通过，前端显示按钮、
		// 点下去才报错（假按钮）。只实现了 Start/Poll 却返回
		// Configured=false 的上游应当被当作**不支持** ——
		// 与 schedule.go / uimanifest.go 里那两处判据逐字一致。
		fl, ok := gateway.ExtOf[gateway.LoginFlow](p)
		if !ok || !fl.Configured() {
			return nil, false, true
		}
		return fl, true, true
	}
	// 上游不存在：既没 flow 也没这个上游
	return nil, false, false
}

// providerByID 按上游标识在注册表里找一个上游。
//
// # 为什么不能只拿 LoginFlow（实测的编译期约束）
//
// `gateway.ExtOf[T]` 的签名是 `ExtOf[T any](p Provider)` —— 它吃的是
// **Provider**，不是任意接口值。而 `pollViaFlow` 过去只持有
// `flow gateway.LoginFlow`，于是想在那里问 `AuthDirExt` 时会直接编译失败：
//
//	cannot use flow (variable of interface type gateway.LoginFlow) as
//	gateway.Provider value in argument to ExtOf[AuthDirExt]:
//	gateway.LoginFlow does not implement gateway.Provider (missing method Caps)
//
// 这不是需要绕开的麻烦 —— 它恰好说明**问错对象了**：
// "我的凭证目录在哪"是 **Provider 这个实体**自报的事实，
// 而 `LoginFlow` 只是它身上挂着的一个交互能力。
// 把 `*Provider` 传下去才是对的（见 pollViaFlow 的注释）。
func (h *Handler) providerByID(providerID string) (gateway.Provider, bool) {
	if providerID == "" || h.cfg.Registry == nil {
		return nil, false
	}
	for _, p := range h.cfg.Registry.All() {
		if p.ID() == providerID {
			return p, true
		}
	}
	return nil, false
}

// shortState 日志里只打前 8 位 —— 完整的 state 是凭据的一部分，
// 不该整条进日志（本项目清理过一次真实密钥泄漏）。
func shortState(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func (h *Handler) loginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State    string `json:"state"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State == "" {
		writeError(w, http.StatusBadRequest, "缺少 state")
		return
	}
	// provider 指定了且不是默认 → 走该上游自己的 LoginFlow。
	// 与 loginStart 同一条分派规则，避免"开始走 A、轮询走 B"。
	if body.Provider != "" {
		// 与 loginStart **同一条**判据：指定了上游就必须有它自己的 flow。
		// 两者不一致会产生"用 A 的 state 去问 B"的诡异现象。
		flow, ok, _ := h.wantsFlowDispatch(body.Provider)
		if !ok {
			writeError(w, http.StatusNotImplemented,
				"上游 "+body.Provider+" 不支持在页面内添加账号（未实现登录流程）")
			return
		}
		// 同一个上游的 Provider 面：落盘目录要问它（见 pollViaFlow 的注释）。
		// 这里**必定**找得到 —— 上一行已经从注册表里认出了该上游。
		p, pok := h.providerByID(body.Provider)
		if !pok {
			writeError(w, http.StatusNotFound, "上游不存在: "+body.Provider)
			return
		}
		if err := h.pollViaFlow(w, p, flow, body.State); err != nil {
			return
		}
		return
	}
	// 不带 provider：旧路径（向后兼容）。没配客户端时明确报错，不 panic。
	if h.cfg.OAuth == nil {
		writeError(w, http.StatusServiceUnavailable,
			"未配置默认 OAuth 客户端；请指定 provider（该上游需实现登录流程）")
		return
	}
	cred, err := h.cfg.OAuth.Poll(body.State)
	switch {
	case errors.Is(err, oauth.ErrPending):
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		return
	case errors.Is(err, oauth.ErrStateUnknown):
		writeError(w, http.StatusGone, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "轮询失败: "+err.Error())
		return
	}

	path, err := cred.SaveToDir(h.cfg.AuthDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "凭证落盘失败: "+err.Error())
		return
	}
	// 落盘后重扫目录对齐池：新账号立即参与选号，无需重启。
	//
	// ⚠ 与 accountsReload 同一个坑：必须显式指定域。
	// 这里落盘的凭证进的是 `h.cfg.AuthDir`，那属于 ReloadProvider ——
	// 用裸 SyncToDir（=当前默认上游）在默认上游不是它时会**误删**。
	//
	// 兼容读：迁移期凭证可能还在 `auths/` 根，只读子目录会看不到。
	base := h.cfg.AuthsBase
	if base == "" {
		base = h.cfg.AuthDir
	}
	auths, lerr := auth.LoadDirCompat(base, h.reloadProvider())
	if lerr != nil {
		writeError(w, http.StatusInternalServerError, "凭证已写入但重扫目录失败: "+lerr.Error())
		return
	}
	// ⚠ 池子可能为 nil（同 pollViaFlow / accountsReload 的注释）。
	// 漏判的后果是 nil pointer panic。
	if h.cfg.Pool != nil {
		h.cfg.Pool.SyncToDirFor(h.reloadProvider(), auths)
	} else {
		log.Printf("admin: oauth 凭证已落盘，但没有账号池可同步（uid=%s）", cred.UID)
	}
	log.Printf("admin: oauth 成功 uid=%s nick=%s file=%s", cred.UID, cred.Nickname, filepath.Base(path))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"uid":           cred.UID,
		"nickname":      cred.Nickname,
		"enterprise_id": cred.EnterpriseID,
		"domain":        cred.Domain,
		"expires_in":    cred.ExpiresIn,
		"file":          filepath.Base(path),
	})
}

// pollViaFlow 用某个上游自己的 LoginFlow 轮询，并把拿到的凭证落盘、对齐池。
//
// # 为什么不能沿用旧路径的收尾
//
// 旧路径直接 `cred.SaveToDir(...)`（`*oauth.Credential` 的方法），
// 它自带宽 workbuddy auth 文件格式的知识（`MarshalAuthFile` 决定文件名与内容）。
// 而 `gateway.LoginFlow.Poll` 返回**通用** `gateway.Credential`，
// 它的 `Secret` 是 `any` —— 各上游结构完全不同（见该类型注释）。
//
// # 本函数要求 Secret 实现 authFileWriter
//
// 落盘这件事只有上游自己知道怎么做（文件名规则、字段形状、
// codearts 还要写 DPoP 私钥）。所以定义一个**窄接口**：
//
//	type authFileWriter interface {
//	    MarshalAuthFile() (name string, raw []byte, err error)
//	}
//
// 与 `*oauth.Credential` 已有的方法**同名同签名** —— 所以 workbuddy
// 的适配器只要把 `*oauth.Credential` 放进来就自动满足，零转换代码。
//
// Secret 不满足时**501 并说明原因**，而不是猜字段映射：
// 把未知结构"尽力映射"会产生看起来成功、实际字段错位的凭证文件 ——
// 那比明确失败糟得多（本项目反复的教训）。
//
// # 为什么要多收一个 p（Provider）
//
// 落盘目录必须问 `AuthDirExt`，而 `gateway.ExtOf` 只接受 `Provider`
// （见 providerByID 的注释）。`flow` 与 `p` 是**同一个上游**的两个面：
// `flow` 负责"怎么拿到凭证"，`p` 负责"凭证该落在哪"。
// 由调用方（loginPoll，从注册表里遍历出来的那个 p）一并传入，
// 而不是在这里反查 —— 反查要多一次遍历，且容易传进不一致的两个上游。
func (h *Handler) pollViaFlow(w http.ResponseWriter, p gateway.Provider, flow gateway.LoginFlow, state string) error {
	cred, err := flow.Poll(state)
	switch {
	case errors.Is(err, gateway.ErrLoginPending):
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		return errHandled
	case err != nil:
		// LoginFlow 的契约只有 ErrLoginPending 一个哨兵，
		// 无法区分"state 未知"与"轮询失败"，统一 502 并带上游原文。
		writeError(w, http.StatusBadGateway, "轮询失败: "+err.Error())
		return errHandled
	}

	mw, ok := cred.Secret.(authFileWriter)
	if !ok {
		writeError(w, http.StatusNotImplemented,
			"该上游的凭证结构尚未接入落盘"+
				"（LoginFlow 的 Secret 需要实现 MarshalAuthFile）")
		return errHandled
	}
	name, raw, merr := mw.MarshalAuthFile()
	if merr != nil {
		writeError(w, http.StatusInternalServerError, "凭证序列化失败: "+merr.Error())
		return errHandled
	}
	// ⚠ 落盘目录用**上游自报的**，不是 h.cfg.AuthDir。
	//
	// 我第一版用的是 h.cfg.AuthDir —— 那是**默认上游（workbuddy）的目录**。
	// 于是 codearts 授权成功后凭证被往 workbuddy 的 `./auths` 写。
	//
	// 按上游分子目录之后（`auths/workbuddy/`、`auths/codearts/`），
	// 目录是**上游的事实**（它知道自己从哪读凭证），核心不该猜。
	//
	// ⚠ 目录从 `AuthDirExt` 问，**不是**从 `flow` 问 ——
	// 它原来挂在 `LoginFlow` 上，那等于"不实现登录流程的上游连
	// 自己的凭证目录都答不出来"，而按上游重载 auths **不需要登录流程**
	//（手工拷凭证是常见路径）。见 gateway.AuthDirExt 的注释。
	//
	// 问的是 p（同一个上游的 Provider 面）：`ExtOf` 只接受 Provider。
	// 上游返回空串 = "用核心默认目录"（单上游部署的旧形态）。
	dir := ""
	if ext, ok := gateway.ExtOf[gateway.AuthDirExt](p); ok {
		dir = ext.AuthDir()
	}
	if dir == "" {
		dir = h.cfg.AuthDir
	}
	path, werr := writeAuthFile(dir, name, raw)
	if werr != nil {
		writeError(w, http.StatusInternalServerError, "凭证落盘失败: "+werr.Error())
		return errHandled
	}
	// 重扫**该上游的**目录，**用它自己的解析器**。
	//
	// ⚠ 与落盘目录必须一致 —— 扫错目录会得到"写成功但池子里没有"
	//（用户看到账号没进池，而日志说成功）。
	//
	// # 这里原来错得离谱（与 `accountsReload` 是同一个 bug 的另一半）
	//
	// 旧代码只有一行：
	//
	//	auths, lerr := auth.LoadDirCompat(h.cfg.AuthDir, cred.Provider)
	//
	// 上面几行刚刚**问对了**落盘目录（`gateway.AuthDirExt`），
	// 紧接着这一行却用回了 **workbuddy 的目录 + workbuddy 的解析器**。
	// 三个事实叠加成 bug：
	//
	//   1. `h.cfg.AuthDir` = `auths/workbuddy/`（`cmd/server/config.go` 显式加了后缀）
	//   2. `auth.LoadDirCompat(base, id)` = `LoadDir(base/<id>)` **加上**
	//      `LoadDir(base)`，而 `auth.LoadDir` 的 Glob **写死** `workbuddy*.json`
	//      （`internal/auth/auth.go` L192）
	//   3. `codearts.LoadDir` 的 Glob 是 `codearts*.json`
	//      （`internal/codearts/credential.go`）
	//
	// → **一个都匹配不到** codearts 的凭证。于是 codearts 走
	// `/admin/login/poll?provider=codearts` 时：凭证**正确写进**
	// `auths/codearts/`，重扫却拿 workbuddy 的解析器扫 workbuddy 的目录，
	// 得到的是**根目录遗留的 workbuddy 旧凭证**；紧接着
	// `Pool.SyncToDirFor(cred.Provider /* =codearts */, auths)`
	// **把它们灌进了 codearts 域**。回执仍然是 `status:"ok"`。
	//
	// # 正确判据（与 `accountsReload` **同一条**，不要发明第二套）
	//
	//   1. **只扫 `dir`**（上面刚问到的上游自报目录），不扫父目录。
	//   2. **用上游自己的解析器**（`gateway.CredentialLoader`）——
	//      凭证格式是**上游的事实**，核心不该知道 `workbuddy*.json`
	//      还是 `codearts*.json`。这也让"加新上游核心零改动"重新成立。
	//
	// 上游没实现 `CredentialLoader` 时**明确报错**（501，与
	// `accountsReload` 同口径），不回落成"用某个写死的解析器"——
	// 那正是这个 bug 的形态。
	//
	// ⚠ 报 501 而不是 200：凭证**已经落盘**了，但"没能即时进池"这件事
	// 必须让调用方知道（否则用户以为账号加好了、池子里却没有）。
	// 错误文案里说清"凭证已写入"，避免用户以为落盘也失败了。
	loaded, exists := h.credentialsOf(cred.Provider)
	if !exists {
		writeError(w, http.StatusNotImplemented,
			"凭证已写入 "+dir+"，但上游 "+cred.Provider+
				" 没有实现 gateway.CredentialLoader，"+
				"无法按它自己的格式重扫凭证（核心不硬编码任何上游的凭证格式）")
		return errHandled
	}
	// ★ 与 accountsReload **同一条**：优先用「带 secret」的扫描器。
	//
	// # 这是 accountsReload 已经修过、而这条路径漏修的另一半
	//
	// 只走 `credentialsOf`（不带 secret）时，池里新增的账号 secret 恒为 nil：
	//
	//	workbuddy-intl 登录成功 → 落盘正确 → 入池但 **secret=nil**
	//	→ `/v1/models` 的 router 路径取不到凭证（类型断言失败）
	//	→ **模型目录恒为空**；界面 has_token 也显示 `—`
	//
	// 这正是 credentialloader.go 里 LoadCredentialsWithSecrets 的注释
	// 警告过的形态（"没有这个加强版时……模型目录为空（实测 0 个模型）"）。
	//
	// 上游没实现 CredentialSecretLoader 时回落成旧行为（SyncToDirFor），
	// 对"凭证就装在 *auth.Auth 里"的默认上游那正是正确形态。
	var creds []gateway.Credential
	var secrets map[string]any
	if withSecrets, ok := h.credentialSecretsOf(cred.Provider); ok {
		items, serr := withSecrets(dir)
		if serr != nil {
			writeError(w, http.StatusInternalServerError, "凭证已写入但重扫目录失败: "+serr.Error())
			return errHandled
		}
		secrets = make(map[string]any, len(items))
		for _, it := range items {
			creds = append(creds, it.Credential)
			if it.Secret != nil {
				secrets[it.Credential.UID] = it.Secret
			}
		}
	} else {
		var lerr error
		creds, lerr = loaded(dir)
		if lerr != nil {
			writeError(w, http.StatusInternalServerError, "凭证已写入但重扫目录失败: "+lerr.Error())
			return errHandled
		}
	}
	// 投影成账号池要的形状（uid + nickname）—— 与 accountsReload 共用同一函数。
	//
	// ⚠ 这条路径原先自己内联了一份投影，于是**同一个缺陷复发**：
	// 用户在 OAuth 登录一个新号后，号在池里可见、大模型请求也能通，
	// 但额度探测恒 401（HTML 网关错误页）、猫猫旅行/成长计划报「无可用凭证」，
	// 且重启网关即恢复 —— 正是"内存 e.a 与磁盘凭证不同步"的形态。
	//
	// 教训：投影规则只能有一份实现。抽成 poolAuthsFromCreds 后，
	// 两条入池路径（reload / oauth）自动保持一致，不会再各漏一半。
	auths := poolAuthsFromCreds(creds, secrets)
	// ⚠ 池子可能为 nil —— 本包其它地方（schedule.go / uimanifest.go）
	// 都判了空，说明"Pool 可缺省"是**本包自己的设计假设**；
	// 这条落盘路径原来漏判了，后果是 **nil pointer panic（进程级）**。
	//
	// 实测抓到：我为此写的测试在 Pool=nil 时直接 panic 在
	// `pool.(*Pool).SyncToDirFor` 里。
	//
	// 凭证**已经落盘**（那一步不依赖池子），所以这里只是"没能即时进池"——
	// 告诉调用方重启即可，而不是 500 让它以为凭证写失败了。
	if h.cfg.Pool != nil {
		// 拿到了 secret 就走带 secret 的同步 —— 否则池里新账号的
		// secret 恒为 nil，第二个实例的模型目录会恒为空（见上面的注释）。
		if secrets != nil {
			h.cfg.Pool.SyncToDirWithSecrets(cred.Provider, auths, secrets)
		} else {
			h.cfg.Pool.SyncToDirFor(cred.Provider, auths)
		}
	} else {
		log.Printf("admin: oauth(flow) 凭证已落盘，但没有账号池可同步（provider=%s uid=%s）",
			cred.Provider, cred.UID)
	}
	log.Printf("admin: oauth(flow) 成功 provider=%s uid=%s file=%s dir=%s",
		cred.Provider, cred.UID, filepath.Base(path), dir)
	// ⚠ 回执必须带上 `dir`。
	//
	// 前端弹窗原来硬编码 `auths/${p.file}` 拼"凭证文件"那行 ——
	// 而 `file` 只有 basename。按上游分子目录之后，
	// 实际路径是 `auths/codearts/codearts-xxx.json`，
	// 界面却显示成 `auths/codearts-xxx.json`。
	//
	// 用户会照着那句提示去找文件、往那儿放 —— **而那是错的地方**。
	//（实测：用户确实被这句误导，以为"少了根目录"。）
	//
	// 所以把**实际落盘目录**回传，前端直接用它拼，不再自己猜。
	//
	// `domain` / `expires_in` 不在这里返回：
	// `gateway.Credential` **没有** domain 字段（各上游凭证结构不同，
	// 强行统一会造出"什么字段都有、每个上游只填三个"的超集结构体），
	// 而 codearts 也不提供有效期。**不假装有** —— 前端对缺失字段显示 `—`。
	out := map[string]any{
		"status":   "ok",
		"uid":      cred.UID,
		"nickname": cred.Nickname,
		"provider": cred.Provider,
		"dir":      dir,
		"file":     filepath.Base(path),
	}
	// ⚠ `expires_at` 只在**真的有**到期时刻时才下发。
	//
	// # 为什么不能无条件带上（用户实测报的"前端添加账号不对"）
	//
	// `gateway.Credential.ExpiresAt` 是 `time.Time`，**零值表示上游没给有效期**。
	// 无条件序列化会把零值写成 `"0001-01-01T00:00:00Z"` —— 而前端的判据是
	// `if (!p.expires_at) return '—'`：那是一个**非空字符串**，于是它继续往下走
	//
	//	Date.parse("0001-01-01T00:00:00Z") → 约 -6.2e13（truthy，跳过 !t 那道）
	//	days = (t - now)/86400000 → 远小于 0
	//	→ 渲染出「已过期」
	//
	// 于是 loomy 的弹窗在"授权成功"之后写着 **`有效期: 已过期`** ——
	// 与同一份凭证在账号表里显示的「永久」**自相矛盾**，而用户第一眼看到的
	// 就是这句（他会以为这个号不能用了）。
	//
	// 与 T5 那次的 `token_expire_sec` 是同一个形态：**用字段的存在性表达"未知"，
	// 却把一个零值当成有效值发了出去**。所以判据必须是"零值不下发"。
	if cred.ExpiresAt.Unix() > 0 {
		out["expires_at"] = cred.ExpiresAt
	}
	// 过期信息的**第三态**：上游自报"这份凭证设计上不会过期"。
	//
	// 与账号表那一列走**同一个扩展点**（`gateway.CredentialLifetimeExt`），
	// 所以两处不会各说一套：表里显示「永久」时弹窗里也显示「永久」。
	// 拿不到凭证 / 上游没实现 / 上游不确定 → 都不下发（前端显示 `—`），
	// 绝不把"不确定"说成"永久"（见 credentialNeverExpires（Handler 方法）的注释）。
	if credentialNeverExpires(p, cred) {
		out["token_never_expires"] = true
	}
	writeJSON(w, http.StatusOK, out)
	return errHandled
}

// credentialNeverExpires 问**上游自己**：刚拿到的这份凭证会不会过期。
//
// # 为什么不复用 h.credentialNeverExpires(uid, providerID)
//
// 那个方法从**账号池**里按 uid 取 secret（账号表渲染路径上只有 uid 可用）。
// 而这里是"凭证刚落盘、池子可能还没对齐"的时刻 —— 手里就有 `cred`。
// 去池里绕一圈不仅多一次查找，还可能取到**上一版**的 secret，
// 于是弹窗与表格可能对同一份凭证给出不同答案。
func credentialNeverExpires(p gateway.Provider, cred gateway.Credential) bool {
	if p == nil || cred.Secret == nil {
		return false
	}
	ext, ok := gateway.ExtOf[gateway.CredentialLifetimeExt](p)
	if !ok {
		return false
	}
	return ext.NeverExpires(cred)
}

// authFileWriter 上游凭证"能自己序列化成 auth 文件"的窄接口。
//
// 与 `*oauth.Credential` 已有的 `MarshalAuthFile` 同名同签名 ——
// 所以那个类型天然满足，不需要包装。
type authFileWriter interface {
	MarshalAuthFile() (name string, raw []byte, err error)
}

// writeAuthFile 原子写一个凭证文件。
//
// 单独抽出来是为了**与旧路径用同一套写盘规则**（0600 权限、先写 .tmp
// 再 rename）。旧路径在 `oauth.Credential.SaveToDir` 里直接把这几行写在
// 自己内部；这里复刻同样语义，保证两条路径产出的文件权限与原子性一致。
//
// 不共用那个方法的原因：它在 oauth 包，签名绑着 *Credential；
// 为了共用而让 admin 依赖 oauth 的具体类型，会把"通用分派"又焊回单一上游。
func writeAuthFile(dir, name string, raw []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// errHandled 表示"响应已经写过了" —— 调用方据此直接 return，
// 不要再写第二次（重复 WriteHeader 会打日志且状态码以第一次为准）。
var errHandled = errors.New("响应已写")

// ---------------------------------------------------------------------------
// 模型目录 / 日志 / 统计
// ---------------------------------------------------------------------------

func (h *Handler) modelsRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ResetModelsCache == nil {
		writeError(w, http.StatusNotImplemented, "模型缓存刷新未接线")
		return
	}
	h.cfg.ResetModelsCache()
	log.Printf("admin: 模型目录缓存已清空")
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// modelsPreview 返回**全部目录模型**的 id 与成本倍率，供前端在模型名称旁标注倍率。
//
// 为什么不把倍率塞进 /v1/models：
//   - 那是**对外**的 OpenAI 兼容端点。客户端按规范解析 `data[]`，
//     塞自定义字段属于污染公共契约（部分客户端会对未知字段告警，甚至有严格模式直接报错）。
//   - 倍率是网关自己的观测信息，只对本地管理界面有意义 —— 该走 admin 面。
//
// 与 /admin/stats 里的 model_multipliers 的分工（两者都保留）：
//   - stats 那份只含**窗口内被调用过**的模型 —— 报表口径，回答"钱花在哪"
//   - 这份是**全部目录模型** —— 选择口径，回答"这个模型多贵"，下拉框需要给所有选项标注
//
// 降级：未接线或目录拿不到时返回**空数组**而不是报错 ——
// 前端拿不到倍率就只显示模型名，不该让整个模型列表渲染失败。
func (h *Handler) modelsPreview(w http.ResponseWriter, r *http.Request) {
	out := []ModelMultiplier{}
	// 遍历所有已注册上游，把各自的 /v3/config 倍率合并下发 ——
	// 只查默认上游会让其余上游的模型在模型面板上永远没有倍率。
	// 每条带 Provider：多上游同名模型（workbuddy 与 workbuddy-intl 都有 glm-5.2）
	// 倍率互不相同，前端按 (provider, model) 组合查表。
	for _, id := range h.registeredProviderIDs() {
		if h.cfg.ModelCatalog == nil {
			break
		}
		cat := h.cfg.ModelCatalog(id)
		if cat == nil {
			continue
		}
		for _, m := range cat.Models {
			if m.ID == "" {
				continue
			}
			// 与 stats 那份不同：这里**保留 0 倍率**。
			// x0.00 是"免费"这个有意义的事实，不是"没有数据"。
			out = append(out, ModelMultiplier{
				Model:      m.ID,
				Multiplier: m.Multiplier,
				Provider:   id,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "cursor": 0})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	items, cursor := h.cfg.Ring.Snapshot(since)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    items,
		"cursor":   cursor,
		"capacity": h.cfg.Ring.Cap(),
		"held":     h.cfg.Ring.Len(),
	})
}

// logsHistory 从落盘文件读历史请求日志（进程重启后仍可回溯）。
//
// 支持 offset/limit 分页，契约与 /admin/checkin/history 一致：
// 时间倒序（最新在前），返回过滤后总数，前端用同一个分页组件接两个数据源。
func (h *Handler) logsHistory(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil || h.cfg.Ring.Sink() == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = logbuf.DefaultPageSize
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	items, total, err := h.cfg.Ring.Sink().Page(offset, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志文件失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   items,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
		"enabled": true,
		"file":    h.requestLogStats(),
	})
}

// statsCacheTTL 统计聚合结果的缓存时长。
// 聚合要扫整个落盘文件，而前端是轮询调用；缓存让「扫描频率」与「轮询频率」解耦。
const statsCacheTTL = 5 * time.Second

// multCacheTTL 倍率表（全上游合并）的缓存时长。
//
// ⚠ 为什么它比 statsCacheTTL 长得多：这张表来自各家上游的模型目录，**变化极慢**；
// 而重建它要逐家调 `ModelCatalog`，实测会让 `/admin/stats` 卡 **7 秒**
// （见 Handler 的 multMu 字段注释）。表本身与"本次统计窗口"无关 ⇒ 偶尔重建即可。
const multCacheTTL = 10 * time.Minute

// stats 汇总请求日志，给出「这个网关到底跑了多少、跑得怎么样」的只读视图。
//
// 数据源优先级：**落盘日志** > 内存环形缓冲。
// 历史实现只读内存缓冲（上限 2000 条），于是重启后立刻显示「暂无请求」，
// 且窗口被限制在「进程启动至今」——那既不是全部历史，也不是用户以为的统计范围。
// 现在改为聚合落盘文件，重启后依然覆盖全部保留期内的历史；
// 落盘不可用（未启用/读失败）时回落到内存缓冲，并在响应里说明当前范围。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0, "source": "none"})
		return
	}

	items, source, err := h.statsItems()
	// ⚠⚠ 浅拷贝一层再就地写。
	// 为什么可以只拷一层：`statsAggCached` 返回的那份是**只读共享**的，
	// 而下面这十几处 `resp[...] = …` **全是顶层新键**；聚合出的嵌套结构（`by_*`）
	// 在这条路径上只被读。⚠ 前提与后果写在 `statsAggCached` 的注释里 ——
	// 谁要是开始改动嵌套 map 的内容，就得把这里升级成深拷贝。
	// ⚠ 时间档（照 agent2api 的 `range` 档位）。空/未知 ⇒ 不过滤，保持既有"全部落盘历史"。
	// ⚠ 归一后的档名要与缓存键一起传给 `statsAggCached` —— 只靠条数判缓存会让两个档互相命中
	//   （见 `aggRange` 字段的注释：那正是上一轮"某个档看起来不过滤"的根因）。
	// ⚠ 过滤必须发生在 `statsAggCached` **之前**（它内部按 len(items) 与 rangeKey 判缓存）。
	rangeKey := statsRangeKey(r.URL.Query().Get("range"))
	if from, ok := statsRangeFrom(rangeKey); ok {
		kept := make([]logbuf.Entry, 0, len(items))
		for _, e := range items {
			if !e.At.Before(from) {
				kept = append(kept, e)
			}
		}
		items = kept
	}
	agg := h.statsAggCached(items, rangeKey)
	resp := make(map[string]any, len(agg)+16)
	for k, v := range agg {
		resp[k] = v
	}
	resp["source"] = source
	resp["capacity"] = h.cfg.Ring.Cap()
	resp["held"] = h.cfg.Ring.Len()
	// aggregated 显式说明「本次聚合了多少条」，与 total 语义不同：
	//   total      窗口内命中某种条件的条数（aggregateChatLog 里就是 len(items)）
	//   aggregated 实际参与聚合的条数
	// 正常情况两者相等。分开暴露是为了让「显示的窗口 ≠ 实际聚合窗口」这类
	// 静默偏差可见 —— 曾经 statsItems 用 Page(0,1<<20) 想取全量却被分页上界
	// 夹到 300，界面仍显示「全部落盘历史，共 300 条」，看不出是截断。
	resp["aggregated"] = len(items)
	if err != nil {
		// 读盘失败时把原因带上，避免用户对着「总数变小」猜原因。
		resp["error"] = "读取落盘日志失败，已回落到内存缓冲: " + err.Error()
	}
	if f := h.requestLogStats(); f != nil {
		resp["file"] = f
		// 自检：聚合条数与文件总条数不一致时明确标注，不静默。
		if n, ok := f["count"].(int); ok && source == "file" && n != len(items) {
			resp["truncated"] = true
			resp["error"] = fmt.Sprintf(
				"聚合窗口不完整：文件 %d 条，本次只聚合了 %d 条", n, len(items))
		}
	}
	// 成本系数与目录状态（B1.2）：放在同一个响应里而不是新开端点。
	//
	// 为什么不新开 /admin/models/catalog：
	//   - 前端渲染「调用统计」时**同时**需要 by_model（调用次数）与 multiplier（每次多贵），
	//     拆成两个端点会让同一次渲染出现「系数到了但次数还没到」的中间态，
	//     还会多一次轮询往返（仪表盘本来就是 5s 轮询）；
	//   - 两个数据源的生命周期不同（日志聚合 5s 缓存 vs 目录 1h TTL），
	//     但**读口径统一**：都是「展示现状」，没有写语义，合并不引入权限/一致性问题；
	//   - 失败降级是局部的：目录拿不到只让 model_catalog.state=unavailable，
	//     其余统计字段照常返回（不会被 5xx 连坐）。
	// 真正需要独立端点的是「刷新目录」这种**写**动作 —— 那已经由 /admin/models/refresh 承担。
	// 顺序很重要：**先**取倍率（可能触发一次惰性回源），**再**读状态。
	// 反过来会让首次响应自相矛盾 —— 倍率已经拿到了，state 却还报 unavailable。
	//
	// ⚠ 已知代价（评审指出，见代码注释中的 HIGH-2）：ModelCatalog() 可能同步阻塞
	// 至多 MaxRotate 次上游请求。若不接受，正确做法是把回源挪到后台 goroutine、
	// 本接口只读缓存 —— 那会改变「首次点击需要等一次拉取」的现行行为，
	// 是产品取舍而非 bug 修复，故留待专门决定（详见 tasks/board.md）。
	resp["model_multipliers"] = h.modelMultipliers(resp)
	resp["model_catalog"] = h.modelCatalogState()
	writeJSON(w, http.StatusOK, resp)
}

// modelCatalogState 取目录缓存状态；未接线或实现返回零值时统一归到 unavailable。
//
// 与 modelMultipliers 分开是因为调用时机不同：状态每次统计都要报（哪怕系数表没取到），
// 而系数表要按 by_model 过滤，且允许整体缺失。
func (h *Handler) modelCatalogState() ModelCatalogState {
	if h.cfg.ModelCatalogState == nil {
		return ModelCatalogState{State: "unavailable"}
	}
	st := h.cfg.ModelCatalogState()
	if st.State == "" {
		// 实现方漏填状态：按最保守的语义处理，不假装有数据。
		st.State = "unavailable"
	}
	return st
}

// modelMultipliers 返回**在本次统计窗口里真的被调用过**的模型的成本系数。
//
// 为什么要按 by_model 过滤，而不是把整个目录倒出来：
// 目录有 30 个模型，而一个网关上通常只跑其中几个；把没调用过的模型也塞进响应，
// 前端就得自己判断「哪些 chip 该显示」——而那正是这份数据存在的理由。
// 过滤后「有 chip = 有调用」，前端不需要第二套判断。
//
// 只输出**窗口里出现过**且**目录里有系数**的模型：报表要回答的是"钱花在哪"，
// 不是"目录里有什么"。
//
// 触发上游的边界（三条），都在 ModelCatalog 内部自限：
//
//  1. 窗口里没有任何 by_model 时直接跳过 —— 没调过模型就没什么可归因的；
//  2. 单测/未接线（nil）时直接返回空切片；
//  3. 其余情况交给 ModelCatalog()，它自带 1h TTL + 5min 失败负缓存，
//     自己决定要不要真的回源。
//
// ⚠ **不要**在这里按 state.State == "unavailable" 提前返回。
// 曾经这么写过，导致功能无法自举 —— 自锁死循环：
//
//	state 在缓存为空时返回 "unavailable"
//	→ 这里提前返回，从不调用 ModelCatalog()
//	→ 缓存永远为空 → state 永远 unavailable
//
// 冷启动时目录**永远不会**被拉取。而当时的测试全把 state 桩成 "ok"，
// 唯一直接调 ModelCatalog() 的 e2e 又绕开了 /admin/stats，所以全绿但功能是死的
// （由独立评审发现，回归测试见 catalog_bootstrap_test.go）。
//
// 正确做法：把"要不要回源"的决策权交给 ModelCatalog 自己 ——
// 它才知道自己是不是在冷却期、缓存是否过期。状态只用于**展示**，不用于决策。
func (h *Handler) modelMultipliers(resp map[string]any) []ModelMultiplier {
	out := []ModelMultiplier{}
	byModel, ok := resp["by_model"].(map[string]int)
	if !ok || len(byModel) == 0 || h.cfg.ModelCatalog == nil {
		return out
	}
	// 合并所有已注册上游的倍率表：每个上游的 /v3/config 是各自的事实，
	// 只查默认上游会让其余上游被调用的模型倍率全部缺失。
	// 键 = `provider/裸名`（同名模型跨上游互不覆盖）；默认上游额外保留裸名键，
	// 兼容统计键为裸名的老形态。
	//
	// ⚠⚠ **这一张表现在是缓存来的**（`multTableCached`）。原先那段"逐家调
	// ModelCatalog 建表"挪到了 `buildMultTable`，只在后台跑 —— 因为实测它会让
	// 本接口卡 7 秒（详见 Handler 的 multMu 字段注释与 buildMultTable 的说明）。
	tbl := h.multTableCached()
	if len(tbl) == 0 {
		// 表还没建好（冷启动第一次）或确实取不到：如实返回空 ——
		// 前端据此显示"不知道"，而不是把它当成"免费"（见下面 `!ok` 那一支的注释）。
		return out
	}
	// 按模型名排序输出：map 遍历顺序随机，固定顺序让响应可 diff（与目录排序同一动机）。
	names := make([]string, 0, len(byModel))
	for id := range byModel {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		// 统计键形态：带前缀（provider/裸名）直接用；裸名按默认上游组合查。
		m, ok := tbl[id]
		if !ok && !strings.Contains(id, "/") {
			m, ok = tbl["/"+id]
		}
		if !ok {
			// 目录里没有这个模型（或系数为 0）：不输出条目，而不是输出 0 ——
			// 0 系数在前端会显示成「免费」，而真相是「不知道」。
			continue
		}
		out = append(out, ModelMultiplier{Model: id, Multiplier: m, Calls: byModel[id]})
	}
	return out
}

// multTableCached 取「全上游合并后的倍率表」，必要时**在后台**重建。
//
// # 为什么要有它（实测缺陷）
//
// 原先这段逻辑直接写在 `modelMultipliers` 里：逐家上游调 `ModelCatalog(id)`，
// 而它会同步回源（最多 MaxRotate 次上游请求）⇒ 实测单次 `/admin/stats` **7 秒**，
// 前端一次 refresh 调两次 ⇒ 界面卡 14 秒（用 Playwright 量出来的：26 个请求里
// 两个 stats 占 14.6/15.9 秒，其余全是 10~391ms）。
//
// # 契约（三条，别改坏）
//
//  1. **请求路径上永远不重建**：只读 `h.multTbl`，微秒级返回。过期就丢给后台。
//  2. ⚠ **仍然会调用 `ModelCatalog`**（在后台 goroutine 里）—— 这一点是刻意的：
//     若为了"怕慢"而干脆不调，目录缓存就永远不会被拉起来，
//     正好落进 `modelMultipliers` 长注释里记的那个**自锁死循环**（全绿但功能是死的）。
//  3. **同一个时刻只允许一个重建在跑**（`multBuilding` 闸），否则每次请求都会
//     起一个 goroutine，把上游打成一堆并发回源 —— 那比原来的同步阻塞更糟。
func (h *Handler) multTableCached() map[string]float64 {
	h.multMu.Lock()
	tbl, at := h.multTbl, h.multAt
	fresh := !at.IsZero() && time.Since(at) < multCacheTTL
	needBuild := !fresh && !h.multBuilding && h.cfg.ModelCatalog != nil
	if needBuild {
		h.multBuilding = true
	}
	h.multMu.Unlock()

	if needBuild {
		// ⚠⚠ **首次必须同步建**（回归修复，2026-10-18）。
		//
		// 原来这里是"一律丢后台、本次先把现有表交出去"—— 那条对**过期**的表是对的，
		// 但表为 **nil**（进程刚起来、还没建过）时，调用方会拿到**空表**。
		// ⚠ 这不是"慢一点"，而是**错的**：`modelMultipliers` 是个**读接口**，
		// 返回空意味着界面上"全模型都没有倍率标记"，而且两个单测直接红：
		//   TestStatsModelMultipliersFilteredByByModel：`model_multipliers=[]`
		//   TestStatsModelMultipliersOrderIsStable：   第 0 次顺序 `[]`
		// ⚠ 那次改动是我做的，欠的账在这里补。
		//
		// 同步建只发生在**进程启动后的第一次**调用；之后每次都会走到上面的 fresh 分支，
		// 或走下面的后台重建 ⇒ 请求路径依然是微秒级（这才是当初要治的东西）。
		if tbl == nil {
			t := h.buildMultTable()
			h.multMu.Lock()
			h.multTbl, h.multAt, h.multBuilding = t, time.Now(), false
			h.multMu.Unlock()
			return t
		}
		// ⚠ 表**已过期但仍非空**：这才是"先把旧表交出去、后台重建"的正确场景 ——
		//   调用方拿到的是**上一个版本的真数据**，不是空。
		go func() {
			t := h.buildMultTable()
			h.multMu.Lock()
			h.multTbl, h.multAt, h.multBuilding = t, time.Now(), false
			h.multMu.Unlock()
		}()
	}
	return tbl
}

// buildMultTable 真去逐家上游取倍率并合并成一张表。**慢，只该在后台跑。**
func (h *Handler) buildMultTable() map[string]float64 {
	tbl := map[string]float64{}
	for _, id := range h.registeredProviderIDs() {
		cat := h.cfg.ModelCatalog(id)
		if cat == nil {
			continue
		}
		for k, v := range cat.MultiplierTable() {
			tbl[id+"/"+k] = v
			if id == "" {
				tbl[k] = v
			}
		}
	}
	return tbl
}

// registeredProviderIDs 返回注册表里全部上游 id（按注册顺序）。
// 注册表未接线（单上游部署/测试桩）时返回 []string{""} —— 空串 = 默认上游，
// 与 ModelCatalogFor("") 的语义一致，让无 registry 的调用方照常取到默认目录。
func (h *Handler) registeredProviderIDs() []string {
	if h.cfg.Registry == nil {
		return []string{""}
	}
	ids := h.cfg.Registry.IDs()
	if len(ids) == 0 {
		return []string{""}
	}
	return ids
}

// statsItems 取出用于聚合的条目及其来源标识。
// 带 TTL 缓存：聚合是 O(文件行数)，而调用方是轮询。
func (h *Handler) statsItems() ([]logbuf.Entry, string, error) {
	h.statsMu.Lock()
	if !h.statsAt.IsZero() && time.Since(h.statsAt) < statsCacheTTL {
		items, err := h.statsCache, h.statsErr
		h.statsMu.Unlock()
		if err != nil {
			return items, "memory", err
		}
		return items, h.statsSource, nil
	}
	h.statsMu.Unlock()

	var (
		items  []logbuf.Entry
		source = "memory"
		err    error
	)
	if sink := h.cfg.Ring.Sink(); sink != nil {
		// 用 LoadAll 而不是 Page(0, sinkCountAll)：
		// Page 会按 MaxPageSize 夹紧 limit，而它分页上界是给 HTTP 调用方设的。
		// 曾经这里传 1<<20 想取全量，被静默夹到 300 —— 一份 1855 行的日志
		// 只聚合了最近 300 条，界面上的成功率/平均 TTFB 全是那 300 条的。
		// 内部聚合走旁路，不受分页上界约束（见 logbuf.Sink.LoadAll 的注释）。
		if all, e := sink.LoadAll(); e != nil {
			err = e
		} else if len(all) > 0 {
			items, source = all, "file"
		} else {
			source = "file" // 文件存在但为空：如实说是文件来源，而不是内存
		}
	}
	if source == "memory" {
		items, _ = h.cfg.Ring.Snapshot(0)
	}

	h.statsMu.Lock()
	h.statsCache, h.statsSource, h.statsErr, h.statsAt = items, source, err, time.Now()
	h.statsMu.Unlock()
	return items, source, err
}

// statsAggCached 返回**聚合产物**的缓存（照 multTableCached 那套写）。
//
// # 治的是什么
//
// 实测 `/admin/stats` 热态 163~337ms，而 `statsItems` 已经缓存了**原始行** ——
// 所以那些毫秒几乎全花在 `aggregateChatLog` 逐行遍历上（28000 条）。
// ⚠ "预热一遍"治不了它（实测 3078ms vs 3156ms，见 admin.New 里的注释）⇒ 它是**纯计算**。
// ⇒ 只能真的把产物缓存起来。
//
// # ⚠⚠ 契约：返回给调用方的**必须**是浅拷贝，而缓存里那一份**只读**
//
// 为什么浅拷贝就够：`stats()` 在拿到产物之后就地写入的十几处
// （`resp["source"]` / `resp["capacity"]` / `resp["model_multipliers"]` …）
// **全部是顶层新键**；而聚合出来的嵌套结构（`by_model` / `by_uid` / `by_*_detail`）
// 在这条路径上**只被读**。
//
// ⚠ **打破这个前提的后果是静默污染**：若将来有谁在这条路径上改动/删除嵌套 map 的内容
// （例如 `delete(resp["by_model"], k)`、或往某个 `by_*` 里塞东西），
// 那么下一次请求会拿到**被上一次改过**的那份 —— 而症状只是"数字偶尔不对"。
// ⇒ 所以那条回归测试（见 docs/report/P2-design.md §5-3）是这条前提的**守门人**，
//
//	改动这段代码时**必须**先看它。
//
// # 三条与 multTableCached 一致的行为（别改坏）
//
//  1. **过期时不在请求里重建**：丢给后台 goroutine，本次先把旧产物交出去
//     （⚠ 旧数据 << 空数据：空会让界面上的卡片/榜/环整块消失）；
//  2. ⚠ **条数变了立刻重建**（`aggCount != len(items)`）—— 日志在实时追加，
//     若只看 TTL，`total` 会停在旧值，用户会看到"刚发的请求没被统计"；
//  3. ⚠ **首次（缓存为 nil）同步建一次** —— 与 multTableCached 的修法同理：
//     "返回上一个版本的真数据"可以接受，"返回空"不行。
func (h *Handler) statsAggCached(items []logbuf.Entry, rangeKey string) map[string]any {
	h.aggMu.Lock()
	cached, at, cnt, rg := h.aggCache, h.aggAt, h.aggCount, h.aggRange
	// ⚠ 三个判据缺一不可：新鲜度 · 条数 · **时间档**（见 aggRange 字段的注释）
	fresh := cached != nil && at != zeroTime() && time.Since(at) < statsCacheTTL
	fresh = fresh && cnt == len(items) && rg == rangeKey
	needBuild := !fresh && !h.aggBuilding
	if needBuild {
		h.aggBuilding = true
	}
	h.aggMu.Unlock()

	// ⚠⚠ **只有四项全对才复用**（2026-10-18 修，这是本轮的关键一处）。
	//
	// 原来这里是"只要没人在建就 `return cached`" —— 而那份 `cached` 可能是**别的档**的产物
	// ⇒ 换档时先到的请求会拿到上一个档的数字。实测症状：
	//   `?range=today` 给 1735 / 29006（两次不同）· `?range=7` 给 29017（其实是"全部"）
	//   · `?range=bogus` 给 1735 —— **三个值在各档间串位**。
	//
	// ⚠ 同一个病上一轮就出现过，当时是"**条数相同**的两个档互相命中"（还没引入 rangeKey）；
	//   这次的乱序探针（同一个档在不同访问顺序下比对取值集合）才算把它钉死。
	if fresh {
		return cached // ⚠ 调用方负责浅拷贝（本函数**不**替它拷）
	}
	// ⚠ 不 fresh 的三种情形 —— **首次 / 条数变了 / 换了时间档** —— 一律**同步建**：
	//   · "稍旧的**同一档**数据"可以接受（旧数据 << 空数据）；
	//   · 但"**别的档**的数据"是**错的**，不只是旧 —— 用户会对着"近 7 天"读"全部"的数字。
	//   ⚠ 代价：换档时付一次 ~3 秒。那是用户显式点档位按钮的动作，可接受。
	//   ⇒ `aggBuilding` 因此不再参与判据（字段保留，仅为不破坏其它引用）。
	out := aggregateChatLog(items)
	h.aggMu.Lock()
	h.aggCache, h.aggAt, h.aggCount, h.aggRange, h.aggBuilding = out, time.Now(), len(items), rangeKey, false
	h.aggMu.Unlock()
	return out
}

// zeroTime 只是把"零值时间"这个判断收在一处，免得各处自己写 time.Time{}。
func zeroTime() time.Time { return time.Time{} }

// cacheWindowKeys / cacheWindowDur 四档缓存命中率窗口。
//
// ⚠ **口径照抄 agent2api 的 `CACHE_WINDOWS`**（见它 `report.rs:64`）：
//
//	("last10m", 10*60_000) · ("last1h", 60*60_000) · ("last24h", …) · ("last7d", …)
//
// ⚠ 键名也照抄（它前端读的就是 `cacheRates.last10m` 这些字段）。
var cacheWindowKeys = []string{"last10m", "last1h", "last24h", "last7d"}

var cacheWindowDur = []time.Duration{
	10 * time.Minute, time.Hour, 24 * time.Hour, 7 * 24 * time.Hour,
}

// statsRangeKey 归一化时间档名（⚠ 只做 `trim + lower`，未知值原样返回）。
//
// ⚠ 为什么单独一个函数（2026-10-18 的教训）：把"档名 → 缓存键"收在一处，
// 单测才能直接断言"同一档永远得到同一个键"。上一轮我把归一并进取边界那个函数、
// **没有单测就上线**，结果出现"同一字符串时好时坏"，我查了四轮才定性。
//
// ⚠ 未知值**不做兜底**（`"bogus"` 就返回 `"bogus"`）：它要参与缓存键，
// 不同的未知值不该互撞；"是否过滤"由 `statsRangeFrom` 说了算。
func statsRangeKey(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}

// statsRangeFrom 把**归一化后**的档名解析成"从这个时刻起（含）"。
//
// ⚠ 第二个返回值为 false ⇒ **不过滤**（调用方保持原行为：全部落盘历史）。
//
// ⚠ 口径照 agent2api 的 `range_bounds`（report.rs:96-145）：
//
//	today ⇒ 今天 00:00（本地）
//	7 / 30 ⇒ 从今天往前 days-1 天，**含今天** —— 它明确写了"含今天，所以减 days-1"
//	month ⇒ 本月 1 号 00:00（本地）
//	其它（含空、"all"）⇒ 不过滤
//
// ⚠ 它那两条防御我们天然不需要（我们只做"起点"、不做"终点"）：
// start>end 要交换、end 要夹到今天 —— 没有终点就都不存在。
// 注意："未来时间戳的行"仍可能进来（时钟被往前调过），那与不加档时行为一致。
func statsRangeFrom(key string) (time.Time, bool) {
	now := time.Now()
	y, m, d := now.Date()
	todayMidnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch key {
	case "today":
		return todayMidnight, true
	case "7":
		return todayMidnight.AddDate(0, 0, -6), true
	case "30":
		return todayMidnight.AddDate(0, 0, -29), true
	case "month":
		return time.Date(y, m, 1, 0, 0, 0, 0, now.Location()), true
	}
	return time.Time{}, false
}

// cacheRates 四档缓存命中率（每档含 rate / hit / miss）。
//
// # 口径（与它一字不差）
//
//   - 命中率 = 缓存读取 Token / 输入 Token（输入 = 命中 + 未命中）
//   - ⚠ **不夹取值**：上游把「缓存读取」与「输入」分开上报时，这个比值**合法地可能超过 100%**，
//     照它 `report-page.tsx:124` 的 TIP —— **如实展示**，不人为截到 100%。
//   - ⚠ 某档分母为 0 ⇒ **不输出那一档**（与顶层 `cache_hit_rate` 同一约定）⇒ 前端如实显示 N/A。
//
// # 为什么写成独立函数而不是塞进 aggregateChatLog 的循环
//
// ⚠ 塞进去能少扫一遍，但那样**动的是最核心的那个聚合循环**（全站最热接口的算术全在那儿）。
// 这里只是一次额外遍历（实测 28900 行 ≈ 几毫秒，而那个循环本身要 ~3 秒）——
// 用几毫秒换"核心逻辑零改动"，值得。⚠ 而且独立函数可以直接单测（见同目录的测试）。
//
// ⚠ 实测记录（2026-10-18）：我们日志里这两个字段**每行都有，但最近 24 小时内全是 0**
// （只有最早那批 2265 行有值）⇒ last10m/last1h/last24h 三档会如实显示 N/A，
// 只有 last7d 有数（97.96%，与当时 /admin/stats 的 cache_hit_rate 一致）。
// ⚠ 那说明**上游现在不再上报这两个字段** —— 不是本函数算错。
func cacheRates(items []logbuf.Entry, now time.Time) map[string]any {
	// ⚠ wins 里多一个 `n`：该窗口里**真的带缓存信息的行数**（供前端显示样本量）。
	wins := make([]struct {
		hit, miss, n int64
	}, len(cacheWindowDur))
	longest := cacheWindowDur[len(cacheWindowDur)-1]
	for _, e := range items {
		age := now.Sub(e.At)
		if age < 0 || age > longest { // ⚠ age<0：时钟回拨/未来时间戳，跳过（不然它会落进所有档）
			continue
		}
		h, m := int64(e.CacheHitTokens), int64(e.CacheMissTokens)
		// ⚠ 量纲守卫（2026-10-20 实测）：有的上游把 cache_hit 按完全不同的量纲报 ——
		// 实测 7382 条记录里 hit 高达 100 万+，而同一行的 tokens 只有几百（比值 346~656 倍）。
		// 直接累加会得出"缓存命中 21 亿 token"这种明显荒谬的总量，并把命中率推到 99.9%。
		// 判据：hit 远超该行 tokens（> tokens*10，且 tokens>0）⇒ 这行缓存数据不可信，不计入。
		// 宁可少算 —— 不要给出一个看不出错、但完全失真的百分比。
		if e.Tokens > 0 && h > int64(e.Tokens)*10 {
			continue
		}
		if h <= 0 && m <= 0 {
			continue
		}
		for i, d := range cacheWindowDur {
			if age <= d {
				if h > 0 {
					wins[i].hit += h
				}
				if m > 0 {
					wins[i].miss += m
				}
				wins[i].n++ // ⚠ 行数（这一行真的带缓存信息）
			}
		}
	}
	out := map[string]any{}
	for i, k := range cacheWindowKeys {
		r, ok := cacheHitRate(wins[i].hit, wins[i].miss)
		if !ok {
			continue // ⚠ 分母 0 ⇒ 不输出这一档（前端显示 N/A，而不是一个假的 0%）
		}
		// ⚠ `n` = 这个窗口里**真的带缓存信息的行数**（2026-10-18 加）。
		//   为什么必须给前端：用户看到「近 10 分钟 82%」而「近 7 天 98%」会问"是不是不统一" ——
		//   ⚠ 其实两个窗口的**样本完全不同**（几行 vs 几千行）。把样本量摆出来，
		//   读者才能自己判断"这个百分比有多可信"，而不是把两者当同一件事比。
		out[k] = map[string]any{"rate": r, "hit": wins[i].hit, "miss": wins[i].miss, "n": wins[i].n}
	}
	return out
}

// cacheHitRate 缓存命中率 = 命中 / (命中 + 未命中)。//
// 分母为 0 时返回 (0, false)：调用方据此决定「输出 0」还是「不输出该字段」。
// 之所以必须显式挡掉 0/0，是因为 IEEE-754 下 float64(0)/float64(0) = NaN，
// NaN 经 encoding/json 序列化成 JSON 字面量 `null`（Marshal 对 NaN/Inf 返回
// 不支持值错误，writeJSON 里 `raw, _ :=` 把错误吞掉后会写出一个空 body）——
// 前端拿到 null 再做算术就是 NaN，页面上会出现 "NaN%"。
// 宁可少一个字段，也不要让「没有缓存数据」伪装成一个数值。
func cacheHitRate(hit, miss int64) (float64, bool) {
	den := hit + miss
	if den <= 0 {
		return 0, false
	}
	return float64(hit) / float64(den), true
}

// aggregateChatLog 对条目做纯聚合（无 IO），便于单测直接覆盖算术。
//
// 新增的 usage 派生字段（Credit/ThinkTokens/CacheHitTokens/CacheMissTokens）
// 一律只在 >0 时累加：旧格式行没有这些 JSON 键，反序列化后是 Go 零值，
// 天然贡献 0；-0.0 / 负数这类病态值（理论上解析层已挡掉）也和 0 等价，
// 不会污染其它聚合量（它们各走各的累加器，唯一的交汇点是 cache_hit_rate 的分母，
// 而那里对 <=0 有显式兜底）。
// providerStat 单个上游在聚合窗口内的表现。
//
// # 为什么要按上游分组（Task 7）
//
// one-api 社区的实证教训："先把观测建好"。多上游之后，若统计不带 provider 维度，
// 两个上游的调用次数与消耗混在一个数里，面板**无法归因** ——
// 看不出成本花在哪个上游、哪个上游在报错。
//
// 字段刻意保持最小：调用次数 / 成功 / 失败 / 累计消耗。
// 各自的模型分布已经由 by_model 覆盖（模型名本身带前缀即可区分上游）。
type providerStat struct {
	Calls  int64   `json:"calls"`
	OK     int64   `json:"ok"`
	Fail   int64   `json:"fail"`
	Credit float64 `json:"credit"`
}

// unlabeledProvider 无 provider 标记的行（历史日志）归入这一桶。
//
// 为什么不丢弃它们：旧日志文件里的行没有 provider 键，反序列化得空串。
// 丢弃会让"历史调用了多少次"这个数**变小**，是数据失真；
// 归到一个明确的桶里既保住了总数，又如实说明"这些行不知道是哪个上游"。
const unlabeledProvider = "(未标注)"

// dimStat 富化维度（by_model_detail / by_uid_detail / by_provider_detail）里的一个桶。
//
// # 为什么是"再加一份"，而不是把旧映射就地改成对象
//
// by_model / by_uid / by_provider 这三个「名称 → 请求数」的映射**已经在被消费**：
// 前端拿 by_model 去做倍率表过滤，`tools/verify_stats.py` 直接读 by_uid 的计数。
// 就地改成对象会同时打断两边，所以富化一律走**新增字段**，旧字段一字不动。
//
// # 口径（与顶层标量逐条对齐，避免"同一个数两种算法"）
//
//   - req：窗口内该键的条数，**等于旧映射里那个数**（同一个循环里一起加）；
//   - ok / fail：2xx 记 ok、>=400 记 fail，其余状态码（1xx/3xx）两个都不记 ——
//     于是 ok+fail 可能小于 req，这是刻意与顶层 ok/fail 保持同一口径；
//   - tokens：只在 Tokens>0 时累加（-1 = usage 缺失，不当 0 也不当负数扣）；
//   - tokens_raw：**原始和**（含负数原样累加），只为让"脏数据到底差多少"可量化；
//     它与 tokens 的差 = 该桶里所有负数 tokens 之和。⚠ 选 tokens 当主口径的理由：
//     顶层标量 tokens 就是"跳过负数"，两个口径混用会出现"明细求和 ≠ 卡片上的总数"；
//     tokens_raw 是附加观测，不参与任何推导。
//   - think_tokens：只在 >0 时累加（与顶层同规则）；
//   - avg_ttfb_ms：只按 TTFBMS>0 的样本求平均，**没有样本时不输出该键**。
//     不输出 0 是刻意的：0ms 会被读成"快得离谱"，缺字段才是"不知道"
//     （与顶层 avg_ttfb_ms、cache_hit_rate 的同一约定）。
type dimStat struct {
	Req       int64 `json:"req"`
	OK        int64 `json:"ok"`
	Fail      int64 `json:"fail"`
	Tokens    int64 `json:"tokens"`
	TokensRaw int64 `json:"tokens_raw"`
	// OutTokens 本维度下的**纯输出** token（2026-10-20 加）。
	//
	// ⚠ 与 Tokens 的区别是本轮改造的核心：Tokens 是 total（输入+输出，含缓存读取），
	// 实测一条请求 total=37938 而输出仅 176。用户定义的"消耗"是后者。
	// 环图与排行要显示"用量"时取这个字段，不是 Tokens。
	OutTokens   int64  `json:"out_tokens"`
	ThinkTokens int64  `json:"think_tokens"`
	AvgTTFBMS   *int64 `json:"avg_ttfb_ms,omitempty"`

	ttfbSum int64 // 内部累加器：参与平均的 ttfb 之和
	ttfbN   int64 // 内部累加器：参与平均的样本数
}

// add 把一条日志并入本桶。规则必须与顶层标量逐条一致（见类型注释）。
func (d *dimStat) add(e logbuf.Entry) {
	d.Req++
	if e.Status >= 200 && e.Status < 300 {
		d.OK++
	} else if e.Status >= 400 {
		d.Fail++
	}
	if e.Tokens > 0 {
		// ⚠ 只累加**有效** token 数（2026-10-20 实测修的 bug）：
		// Tokens 的 -1 是"上游没给 usage"的哨兵值（见 logbuf.Entry.Tokens 的约定），
		// 无条件 += 会把它当成 -1 个 token 加进去 —— 实测 34945 条里有 4321 条（12%）是 -1，
		// 于是按 provider 聚合出现了 `codearts tokens = -2` 这种负数，总量也被压低。
		// 哨兵必须**排除**，不能当 0（那是"真有 0 token"，语义不同），更不能当 -1。
		if e.Tokens > 0 {
			d.Tokens += int64(e.Tokens)
			// 纯输出同步累加到本维度（环图/排行要用它，不能用 Tokens）
			if e.OutTokens > 0 {
				d.OutTokens += int64(e.OutTokens)
			}
		}
	}
	d.TokensRaw += int64(e.Tokens) // 原始和：负数原样进
	if e.ThinkTokens > 0 {
		d.ThinkTokens += int64(e.ThinkTokens)
	}
	if e.TTFBMS > 0 {
		d.ttfbSum += e.TTFBMS
		d.ttfbN++
	}
}

// finalize 结算派生量。除法**只在这里做一次**，且分母为 0 时直接不置 AvgTTFBMS
// ⇒ 序列化后该键消失，前端显示"—"而不是一个 0（零除在这里就被挡掉了）。
func (d *dimStat) finalize() {
	if d.ttfbN > 0 {
		v := d.ttfbSum / d.ttfbN
		d.AvgTTFBMS = &v
	}
}

// detailFor 取桶，没有就就地建一个（与 byProvider 的惰性建桶同一手法）。
func detailFor(m map[string]*dimStat, k string) *dimStat {
	d, ok := m[k]
	if !ok {
		d = &dimStat{}
		m[k] = d
	}
	return d
}

// detailOut 把内部指针表结算成可直接序列化的值表（并触发 finalize）。
func detailOut(m map[string]*dimStat) map[string]dimStat {
	out := make(map[string]dimStat, len(m))
	for k, v := range m {
		v.finalize()
		out[k] = *v
	}
	return out
}

// aggregateChatLog 把日志行聚合成 /admin/stats 的响应。
func aggregateChatLog(items []logbuf.Entry) map[string]any {
	byModel := map[string]int{}
	byStatus := map[string]int{}
	byUID := map[string]int{}
	byProvider := map[string]*providerStat{}

	// 富化维度（纯新增，见 dimStat 的类型注释）：
	// by_model_detail / by_uid_detail / by_provider_detail。
	byModelDetail := map[string]*dimStat{}
	byUIDDetail := map[string]*dimStat{}
	byProviderDetail := map[string]*dimStat{}
	var okN, failN, tokens int64
	var ttfbSum, totalSum int64
	var ttfbN int64
	var credit float64
	var thinkTokens, cacheHitTokens, cacheMissTokens int64
	// thinkTokensWithOut 只在**有输出记录**的行上累加的推理量（与 out_tokens 同期可比）。
	// 见下面累加处那段注释：think 是 out 的子集，跨期并列会得到自相矛盾的读数。
	var thinkTokensWithOut int64
	// outTokens / byModelOut：**纯输出** token 汇总（2026-10-20 加）。
	//
	// ⚠ 与 tokens / byModel 并存而不是替换它们：tokens 是 total（输入+输出），
	// 它的用途是回答"这条请求碰了多少上下文"（含缓存读取）；而用户要的"消耗"
	// 定义是**AI 生成了多少 token**。两者差几十倍（实测 total=37938 / 输出=176），
	// 挤在一个字段里必然有一个是错的。
	// ⚠ 旧日志没有 OutTokens ⇒ 这两个值对历史区间为 0，前端应显示"无数据"，
	// 绝不回退成 total —— 那正是"一小时 2.9M"这个误读的来源。
	var outTokens int64
	byModelOut := map[string]int64{}
	var oldest, newest time.Time

	for _, e := range items {
		byModel[e.Model]++
		byStatus[strconv.Itoa(e.Status)]++
		if e.UID != "" {
			byUID[e.UID]++
		}
		// 富化维度：与上面两行**同一次遍历**里加，保证口径不会漂。
		// ⚠ uid 为空的条目不进 by_uid_detail —— 与 by_uid 的"跳过空 uid"完全一致，
		// 否则两个键集不一样，前端对不上账。
		detailFor(byModelDetail, e.Model).add(e)
		if e.UID != "" {
			detailFor(byUIDDetail, e.UID).add(e)
		}

		// 按上游累计。空 provider = 历史行 → 归入"未标注"桶（不丢弃）。
		pk := e.Provider
		if pk == "" {
			pk = unlabeledProvider
		}
		ps, exists := byProvider[pk]
		if !exists {
			ps = &providerStat{}
			byProvider[pk] = ps
		}
		ps.Calls++
		// 富化维度：pk 与 ps 同一个键（含 "(未标注)" 桶），不另起口径。
		detailFor(byProviderDetail, pk).add(e)

		switch {
		case e.Status >= 200 && e.Status < 300:
			okN++
			ps.OK++
		case e.Status >= 400:
			failN++
			ps.Fail++
		}
		if e.Tokens > 0 {
			tokens += int64(e.Tokens)
		}
		// 纯输出 token（2026-10-20）：用户定义的"消耗"就是它。
		if e.OutTokens > 0 {
			outTokens += int64(e.OutTokens)
			if e.Model != "" {
				byModelOut[e.Model] += int64(e.OutTokens)
			}
		}
		// Credit 用 >0 而不是 !=0：负数/负零属于上游异常数据，
		// 让它进入累加会把「累计消耗」变成负值，比丢弃它更难解释。
		if e.Credit > 0 {
			credit += e.Credit
			ps.Credit += e.Credit
		}
		// ⚠ 2026-10-20：两个累加器，因为「全部历史」与「同期可比」是两个不同的问题。
		//
		//   thinkTokens（原有语义，不动）：所有有推理记录的行。
		//     实测 2026-10-20 全库 35,620 行 ⇒ Σ = 32,147,395。
		//     它覆盖 62% 的行（think_tokens 早就存在）。
		//
		//   thinkTokensWithOut（新增）：只在**有输出记录**的行上累加。
		//     实测 Σ out_tokens = 1,188,551，但 out_tokens 是 10-20 才引入的，
		//     只覆盖 0.6% 的行 ⇒ 同期 think 只有很小一部分。
		//
		//   为什么需要两个：推理是输出的**子集**，把 32M 与 1.2M 并列显示
		//   就是"推理比输出大 27 倍"这种自相矛盾的读数（用户报的"数据不准"）。
		//   但直接改掉 thinkTokens 的语义会让历史推理量凭空消失 ——
		//   那是另一种失真。所以保留原值，另给一个可比的值，让前端自己选。
		//   ⚠ 单行永远正常（0 行 think > out），问题只在聚合范围，别去查解析层。
		if e.ThinkTokens > 0 {
			thinkTokens += int64(e.ThinkTokens)
			if e.OutTokens > 0 {
				thinkTokensWithOut += int64(e.ThinkTokens)
			}
		}
		// ⚠⚠ 量纲守卫（与 cacheRates 里那条同一理由，但这里是**主累加**，漏了它整个
		// cache_hit_tokens / cache_hit_rate 就还是错的）：有的上游把 cache_hit 按完全不同的
		// 量纲报 —— 实测 7382 条记录里 hit 高达 100 万+，而同一行 tokens 只有几百（差 346~656 倍）。
		// 这行的 hit 与 miss 出自**同一份 usage**，所以要么都信、要么都不信。
		// 判据：hit > tokens*10 且 tokens>0 ⇒ 整行缓存字段丢弃。
		// ⚠ 这条是**守卫单测抓出来的**：我上一轮只改了 cacheRates（多窗口那处），
		//   主累加没改，于是命中率仍是 0.9987 —— 测试当场判红，不是靠看图发现的。
		cacheRowTrusted := !(e.Tokens > 0 && int64(e.CacheHitTokens) > int64(e.Tokens)*10)
		if cacheRowTrusted && e.CacheHitTokens > 0 {
			cacheHitTokens += int64(e.CacheHitTokens)
		}
		if cacheRowTrusted && e.CacheMissTokens > 0 {
			cacheMissTokens += int64(e.CacheMissTokens)
		}
		totalSum += e.TotalMS
		if e.TTFBMS > 0 {
			ttfbSum += e.TTFBMS
			ttfbN++
		}
		if oldest.IsZero() || e.At.Before(oldest) {
			oldest = e.At
		}
		if e.At.After(newest) {
			newest = e.At
		}
	}

	resp := map[string]any{
		"total":                 len(items),
		"ok":                    okN,
		"fail":                  failN,
		"by_model":              byModel,
		"by_status":             byStatus,
		"by_uid":                byUID,
		"tokens":                tokens,
		"out_tokens":            outTokens,
		"by_model_out":          byModelOut,
		"credit_total":          credit,
		"think_tokens":          thinkTokens,
		"think_tokens_with_out": thinkTokensWithOut,
		"cache_hit_tokens":      cacheHitTokens,
		"cache_miss_tokens":     cacheMissTokens,
	}
	// cache_hit_rate 只在分母 >0 时输出（空集、全为旧格式行、或只有 miss 也为 0 时都不输出）：
	// 前端据此显示「—」而不是一个 0%，避免把「没有数据」读成「命中率真的是 0」。
	if r, ok := cacheHitRate(cacheHitTokens, cacheMissTokens); ok {
		resp["cache_hit_rate"] = r
	}
	// ⚠ 四档缓存命中率（照 agent2api 的 CACHE_WINDOWS 口径）——
	//   顶层那个是"全部落盘历史"，这四档才是"近 10 分钟 / 1 小时 / 24 小时 / 7 天"。
	//   ⚠ 独立遍历一遍，不动上面那个核心循环（理由见 cacheRates 的注释）。
	resp["cache_rates"] = cacheRates(items, time.Now())
	// by_provider 同样只在有数据时输出（与 cache_hit_rate 一致的约定）：
	// 空集时给一个空对象会让前端把"没有数据"渲染成"所有上游都是 0"。
	// 单上游部署下它仍会有一个桶 —— 那样前端不必分两种形态渲染。
	if len(byProvider) > 0 {
		out := make(map[string]providerStat, len(byProvider))
		for k, v := range byProvider {
			out[k] = *v
		}
		resp["by_provider"] = out
	}
	// 三个富化维度**恒输出**（含空对象），与 by_model/by_uid/by_status 恒输出同一约定：
	// 形状固定，前端不必为"键在不在"分两种分支渲染；空对象渲染出来就是空榜。
	resp["by_model_detail"] = detailOut(byModelDetail)
	resp["by_uid_detail"] = detailOut(byUIDDetail)
	resp["by_provider_detail"] = detailOut(byProviderDetail)
	if len(items) > 0 {
		resp["avg_total_ms"] = totalSum / int64(len(items))
	}
	if ttfbN > 0 {
		// ⚠ 整数除法 = **向下截断**，不是四舍五入（2026-10-20 对账时发现的 1ms 差异来源）。
		// 用 2000 条尾部日志独立复算核对：total / ok / tokens / avg_total_ms 逐位一致，
		// avg_ttfb 我方 16104、手算四舍五入 16105 —— 差的这 1ms 是取整方式，不是数据错误。
		// 留着这个行为（不改成 Round）：截断对"平均耗时"更保守，且改了会让历史对比失真。
		resp["avg_ttfb_ms"] = ttfbSum / ttfbN
	}
	if !oldest.IsZero() {
		resp["window_from"] = oldest
		resp["window_to"] = newest
	}
	return resp
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
