// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/clientlogin"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/prompt"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// Server 出口层（HTTP 入站）行为。
	Server struct {
		// MaxBodyMB 单次请求体上限（MiB），缺省 8，<=0 回落 8。
		//
		// # 为什么它必须存在（借鉴 workbuddy2api-panel）
		//
		// 改造前出口层写的是 `io.ReadAll(io.LimitReader(r.Body, 8<<20))` ——
		// 那是一句**静默截断**：超过 8 MiB 的请求体被无声切掉尾巴，
		// 剩下的半截 JSON 解析失败，网关把它原样转发给上游，
		// 上游回 `code 11101 Unmarshal chat params failed`。
		//
		// 后果是双向的：
		//   - 客户端拿到的是一个**指向自己的错误**（"JSON 畸形"），
		//     而真正的原因（请求太大）从头到尾没有任何地方说出来；
		//   - 出站错误分类把 11101 当成"上游业务错误"处理，
		//     多图会话/长上下文这类正常请求会被反复换号重试，白烧账号健康度。
		//
		// 改成 MaxBytesReader 之后，超限在**读的时候就报错**，
		// 出口层据此返回 413 —— 客户端立刻知道"是大小问题"，
		// 而不是去猜 JSON 哪里写错了。
		MaxBodyMB int `json:"max_body_mb"`
	} `json:"server"`

	// ServiceName 网关身份标识（顶栏标题、/healthz 的 service 字段与
	// X-Service 头都用它）。空 = 用 server.ServiceName 的编译期默认值。
	//
	// # 为什么改成可配置（T8 / 评审 F5）
	//
	// 原先它是编译期常量。那有两个问题：
	//  1. 控制台里显示的"服务名"是**前端硬编码**的字符串（3 处），
	//     后端改了名前端不会跟着变 —— 那就是 B8；
	//  2. 部署多个实例（如本机同时跑实验版与原版）时，
	//     宿主靠 X-Service 区分"是不是同一个服务"的能力失效 ——
	//     两个实例报同一个名字，探活认不出谁是谁。
	//
	// 由配置给出后，顶栏从 /admin/ui/manifest 读它，前端不再硬编码。
	ServiceName string `json:"service_name"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "60s"
		// SoftRateMax 软冷却**指数退避**的封顶（借鉴 workbuddy2api-panel）。
		//
		// 同一账号连续触发 429 软冷却时，实际时长按
		// `soft_rate × 2^(连续次数-1)` 逐次翻倍，封顶本值。
		//
		// 空/非法 → 回落内置默认（2h）。它同时是"6004 带重置时间"那条路径的上限：
		// 上游给的重置时刻再远，也不会让账号被冷超过这个时长。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		// CheckinEnabled/KeepaliveEnabled 显式禁用开关（缺省 true）。
		//
		// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
		//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
		//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
		//     则对老配置零影响，向后完全兼容。
		//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
		//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
		CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到（旅行随之停）
		KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活

		// ActivityHours 对话活跃上报的整点时点（本地时区）。
		//
		// # ⚠ 空数组 = **不启用**（与上面两项的"空 = 回落默认"语义相反）
		//
		// 签到/保活是既有功能，空配置必须保持老行为，所以它们"空 = 回落默认时点"。
		// 而活跃上报是本版本**新增**的能力 —— 若沿用同一条规则，
		// 所有老部署升级后会在 10 点自动对每个账号发上游请求，
		// 一个用户没要求的、默认开启的新行为。
		//
		// 因此它默认关闭：必须显式配了时点才跑。要恢复 B 分支的口径就写 [10]。
		ActivityHours []int `json:"activity_hours"`
		// ActivityEnabled 活跃上报总开关；缺省 true。
		// 最终是否跑 = ActivityEnabled && len(ActivityHours) > 0。
		//
		// 与签到同一条理由用独立 bool：关闭时不擦除 hours，改回 true 即恢复原时点。
		ActivityEnabled bool `json:"activity_enabled"`
		// 猫猫旅行已退役 travel_interval_minutes：派猫合并到签到时点执行（见 scheduler.RunCheckinNow）。
		// 旧 config 里的该键因 JSON 未知字段而自然忽略，不报错。
	} `json:"schedule"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`

		// ── 出站身份（全部可省略；省略时行为与改造前逐字节一致）─────────
		//
		// 见 internal/upstream/headers.go 的包级注释。这些字段让出站请求头
		// 可以按部署环境对齐官方客户端形态，而不必改代码。

		// UserAgent 出站 UA 的逐字覆盖（最高优先）。空 = 按 ClientVersion 决定。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（如 "5.5.4"）。
		//
		// ⚠ 非空会把出站 UA 从 CLI 形态（`CLI/2.63.2 CodeBuddy/2.63.2`）
		// 切换成**桌面端三段式**（`WorkBuddy/<v> WorkBuddy/<v> CLI/<cli>`）。
		// 空 = 保持 CLI 形态 —— 这是**刻意**的缺省：UA 变更属于风险控制敏感项，
		// 不该由升级默默替所有部署做掉。
		ClientVersion string `json:"client_version"`
		// CliVersion 三段式 UA 里的 CLI 段；空 = 内置默认（2.137.1）。
		CliVersion string `json:"cli_version"`
		// ClientName 用量归属名（X-IDE-Name / X-IDE-Type / X-Product / X-Agent-Purpose）。
		//
		// 官方面板「使用端」列读它。配 "WorkBuddy" 即对齐官方桌面端身份。
		// 空 = 只设 X-Product: "SaaS"（改造前行为）。
		ClientName string `json:"client_name"`
		// DeviceToken 全局设备风控令牌（X-Device-Token）。空 = 不使用全局值。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile 设备令牌文件（带 5 分钟 TTL 缓存与失败保留）。
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP 是否把入站客户端 IP 透传给上游（三个等价头）。
		//
		// ⚠ 默认 false。X-Forwarded-For 是客户端可伪造的头，是否可信
		// 完全取决于部署链路 —— 直连公网的部署**不应**打开它。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	// Prompt 系统提示词体系（借鉴 workbuddy2api-panel）。
	//
	// 客户端（Claude Code / Codex 等 CLI）注入的 system prompt 模板句
	// 会被上游内容审核**逐字精确匹配**并整单拦截（400）。本段决定网关怎么处理：
	//
	//	mode=custom（默认） 出站前用网关自有提示词替换客户端 system/developer
	//	mode=passthrough    透传客户端原始 system，撞了再靠降级兜底
	//
	// 两层分工与降级机制详见 internal/prompt 包注释。
	Prompt struct {
		// Mode "custom" / "passthrough"；空或其他值按 custom 处理。
		Mode string `json:"mode"`
		// File 自定义提示词文件路径。空 = 用内置默认。
		//
		// ⚠ 路径非空但不可读（或内容为空）→ **启动直接报错**（fail fast）。
		// 不静默回落内置默认：那会让"路径打错一个字符"表现为
		// "网关一切正常，只是我的人格没了" —— 一个完全无信号的失败。
		File string `json:"file"`
	} `json:"prompt"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// Admin 管理台（/admin/*，仅本机可访问）相关配置。
	Admin struct {
		// CheckinLogPath 签到/保活/旅行历史落盘路径，默认 "./data/checkin-log.json"。
		CheckinLogPath string `json:"checkin_log_path"`
		// CheckinLogKeepDays 历史保留天数，默认 30。
		CheckinLogKeepDays int `json:"checkin_log_keep_days"`
		// OAuthBaseURL 设备授权上游站点，默认 https://copilot.tencent.com（CN）。
		OAuthBaseURL string `json:"oauth_base_url"`
		// TravelAutoClaim 猫到站后自动领奖（默认 true）。关闭后只能手动点「领奖」。
		TravelAutoClaim *bool `json:"travel_auto_claim"`
		// TravelWatchIntervalSeconds 旅行守卫轮间隔，默认 60；同时是上游未给
		// arrive_at 时的兜底轮询周期。
		TravelWatchIntervalSeconds int `json:"travel_watch_interval_seconds"`

		// ---- 成长中心（/v2/activity/growth/*）----
		// GrowthWatchIntervalSeconds 成长中心扫描间隔，默认 600。
		GrowthWatchIntervalSeconds int `json:"growth_watch_interval_seconds"`
		// GrowthAutoClaim 自动领奖（默认 true）。这是唯一真正让信用分到账的动作：
		// completed 只代表任务条件达成，不调 claim 奖励不会发放。
		GrowthAutoClaim *bool `json:"growth_auto_claim"`
		// GrowthAutoClaimAlias 是 growth_auto_claim_tasks 的兼容别名。
		// 起因：accept 那个开关叫 growth_auto_accept_tasks（带 _tasks 后缀），
		// 而 claim 最初只认 growth_auto_claim，于是按一致性手写的
		// growth_auto_claim_tasks 会被静默忽略、只看默认值——配置改了等于没改。
		// 两个键都接受：GrowthAutoClaim 优先，本字段作回退。
		GrowthAutoClaimAlias *bool `json:"growth_auto_claim_tasks"`
		// GrowthAutoAcceptTasks 自动接单（默认 **true**）。
		//
		// 语义：接单只把任务接进列表开始计进度，**不发放奖励**。
		//
		// 为什么从 false 改成 true：实测 `not_accepted` 只在「账号还没领到第一只
		// Buddy（first_buddy）」这个窄窗口里存在 —— 官方前端干脆没有接单按钮，
		// 因为它认为那是用户不需要关心的瞬时中间态。过了这个窗口，上游会自动
		// 把其余任务接单，接单能力就再没有用武之地。
		//
		// 于是保留「默认关」的实际后果是：新账号的那 17 个任务在窗口期内连进度都不显示，
		// 用户看着一片「未接单」却不知道该做什么。默认开启后，守卫轮会顺手把它们接进来，
		// 用户只需要「去做 + 领奖」两件事 —— 正好对应官方前端的心智模型。
		//
		// 仍然是纯登记动作、无资源消耗（不像兑换/开盲盒会花资产），因此默认开的代价为零。
		GrowthAutoAcceptTasks *bool `json:"growth_auto_accept_tasks"`
		// GrowthAutoMakeup 自动补签（默认 true，只消耗补签卡且卡本身无其他用途）。
		GrowthAutoMakeup *bool `json:"growth_auto_makeup"`
		// GrowthAutoRedeem 自动连登兑换（默认 false，会消耗连登天数，属用户资产）。
		GrowthAutoRedeem *bool `json:"growth_auto_redeem"`
		// GrowthAutoOpen 自动开盲盒（默认 false，每次消耗能量）。
		GrowthAutoOpen *bool `json:"growth_auto_open"`
		// GrowthAutoDraw 自动抽奖（默认 false，消耗抽奖次数）。
		GrowthAutoDraw *bool `json:"growth_auto_draw"`

		// ---- 本机客户端登录态（「本地登录」面板）----
		// ClientAuthDir 客户端凭证目录。留空则自动探测
		// %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth；探测不到即关闭该面板。
		ClientAuthDir string `json:"client_auth_dir"`
		// ClientArchiveDir 凭证存档目录，默认 "./data/client-login"。
		// 既存「客户端曾登录过的账号」的原生凭证，也存上次切换前的备份（一键回滚）。
		ClientArchiveDir string `json:"client_archive_dir"`
		// ClientEnabled 总开关，默认 true。关掉后 /admin/client-login/* 直接 503。
		ClientEnabled *bool `json:"client_login_enabled"`
	} `json:"admin"`

	// RequestLogPath / RequestLogKeepDays 请求日志落盘（JSON Lines）。
	// 与顶部 CheckinLogPath 等一样走平铺键，避免再嵌一层。
	RequestLogPath     string `json:"request_log_path"`
	RequestLogKeepDays int    `json:"request_log_keep_days"`

	// Codearts 第二个上游（华为云 CodeArts）的配置。
	//
	// # 向后兼容（硬要求，有测试守着）
	//
	// 本段**整个缺席**时行为与改造前**逐字节一致**：不注册 codearts Provider、
	// 不加载任何 CodeArts 凭证、默认上游仍是 workbuddy。
	// 理由：现有部署的 config.json 里没有这一段，若缺省即启用会让它们
	// 突然多出一个上游（并可能因为扫不到凭证而报错）。
	//
	// 因此启用条件是**显式**的：codearts.enabled = true。
	Codearts struct {
		// Enabled 是否启用 CodeArts 上游。**缺省 false**（与"段缺席"等价）。
		Enabled bool `json:"enabled"`
		// AuthDir CodeArts 凭证目录。
		//
		// 留空则**复用顶层 auth_dir**：codearts.LoadDir 按 `codearts*.json` 通配，
		// workbuddy 的凭证是 `workbuddy-*.json`，两者前缀不同可以安全共存
		// （这也是 codearts 侧原始设计的意图，见其余 credential.go 的注释）。
		//
		// 需要物理隔离时再显式配一个目录（例如 codearts_auth_dir）。
		AuthDir string `json:"auth_dir"`
		// RefreshIntervalSeconds 后台主动续期的扫描周期，默认 60。
		//
		// 为什么需要后台续期：CodeArts 的 STS 凭证只有约 2 小时寿命，
		// 纯靠请求路径惰性续期会让"空闲两小时后的第一个请求"多付一次
		// 续期往返（实测 1-3 秒）。后台预热把这个代价挪到空闲时间。
		//
		// <=0 或未配置一律取 60；本字段**没有**"关闭"语义 ——
		// 要关就整段 enabled=false（少一个三态就少一类误配）。
		RefreshIntervalSeconds int `json:"refresh_interval_seconds"`

		// PoolAccounts 是否把 CodeArts 账号**并入核心账号池**（默认 true）。
		//
		// # 为什么需要这个开关
		//
		// 并入池子是 Task 6 的核心：只有并进去，请求才可能被路由到
		// codearts 的账号（改造前 codearts 的账号只存在它自己的目录里，
		// 池子一无所知，于是"永远选不到 codearts 账号"）。
		//
		// 但并入会让**默认上游之外**多出一批账号，属于可观测的行为变化，
		// 因此保留一个显式退出口：置 false 即回到"codearts 只用自己的
		// 管理端点、不参与选号"的旧形态。
		//
		// 默认 true：已经显式开了 codearts.enabled=true 的部署，
		// 意图就是"用起来"；再要求它们多配一个键才能生效是没必要的摩擦。
		// 注意这与"段缺席即全关"不冲突 —— 段缺席时 Enabled=false，
		// 整个 codearts 都不注册，本字段根本不会被读到。
		//
		// 用 *bool 以便区分「没配」与「显式 false」：
		// 键缺席时保持默认 true（nil → 默认）。
		PoolAccounts *bool `json:"pool_accounts"`

		// OAuthPortal 页内添加账号跳转的授权站点。
		//
		// 留空则用 codearts.DefaultPortalBase（codearts.huaweicloud.com）。
		//
		// # 为什么需要它
		//
		// 授权页地址是**部署相关**的：CN 站与国际站不同，
		// 将来也可能换域名。写死在包里意味着换站点要改代码重编译。
		//
		// # 与顶层 admin.oauth_base_url 的关系
		//
		// **完全无关**，刻意分开：那个是 workbuddy（CodeBuddy）的授权站点，
		// 这个是 CodeArts 的。两者若共用同一个键，配了 workbuddy
		// 就会把 codearts 的授权页也指过去 —— 表现为"点了添加账号
		// 打开的是另一个产品的登录页"，很难判断是配置错还是代码错。
		OAuthPortal string `json:"oauth_portal"`

		// OAuthSTS token 端点。
		//
		// 留空则用 codearts.DefaultSTSBase（sts.cn-north-4.myhuaweicloud.com）。
		// 与 Portal 同理：区域不同端点不同（cn-north-4 / cn-east-3 ...）。
		OAuthSTS string `json:"oauth_sts"`
	} `json:"codearts"`

	// 解析后
	SoftRateDur time.Duration `json:"-"`
	// SoftRateMaxDur 软冷却指数退避封顶；<=0 由 pool 用自己的默认值（2h）。
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	CheckinLogPath      string        `json:"-"`
	CheckinLogKeepDays  int           `json:"-"`
	OAuthBaseURL        string        `json:"-"`
	TravelAutoClaim     bool          `json:"-"`
	TravelWatchInterval time.Duration `json:"-"`
	GrowthWatchInterval time.Duration `json:"-"`
	GrowthAutoClaim     bool          `json:"-"`
	GrowthAutoAccept    bool          `json:"-"`
	GrowthAutoMakeup    bool          `json:"-"`
	GrowthAutoRedeem    bool          `json:"-"`
	GrowthAutoOpen      bool          `json:"-"`
	GrowthAutoDraw      bool          `json:"-"`
	ClientAuthDir       string        `json:"-"`
	ClientArchiveDir    string        `json:"-"`
	ClientEnabled       bool          `json:"-"`

	// Prompt 解析后（供 main 直接取用）。
	//
	// PromptMode 一定是 prompt.ModeCustom 或 prompt.ModePassthrough
	// （normalize 已把空/非法值收敛成 custom）。
	//
	// PromptText 是**已经加载好的提示词正文**：normalize 阶段就会读文件，
	// 因此文件不可读会在启动时报错，而不是等到第一次请求。
	// custom 模式下它一定是非空串（空文件同样被 normalize 判为错误）。
	PromptMode string `json:"-"`
	PromptText string `json:"-"`

	// Codearts 解析后（供 main 直接取用）。
	//
	// CodeartsEnabled 为 false 时下面两个字段无意义：不注册上游、不加载凭证。
	CodeartsEnabled         bool          `json:"-"`
	CodeartsAuthDir         string        `json:"-"`
	CodeartsRefreshInterval time.Duration `json:"-"`
	// CodeartsPoolAccounts 是否把 codearts 账号并入核心账号池（见 Codearts.PoolAccounts）。
	// CodeartsEnabled 为 false 时无意义。
	CodeartsPoolAccounts bool `json:"-"`
	// CodeartsOAuthPortal / CodeartsOAuthSTS 页内添加账号用的授权站点与 token 端点。
	//
	// 已填好默认值（见 normalize），所以 main 可以直接取用而不必判空。
	// CodeartsEnabled 为 false 时不会被读到。
	CodeartsOAuthPortal string `json:"-"`
	CodeartsOAuthSTS    string `json:"-"`

	// AuthsBase 各上游凭证目录的**父目录**（= 配置里写的 auth_dir 原值）。
	//
	// # 为什么保留它
	//
	// 按上游分子目录之后，`AuthDir` 是 `auths/workbuddy/`（已加后缀），
	// 而迁移期凭证可能还在 `auths/` 根。兼容扫描需要**父目录**才能两处都看。
	//
	// 全部迁完之后本字段可以删；保留没有害处（只是记录一个路径）。
	AuthsBase string `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Server.MaxBodyMB = 8
	c.Cooldown.SoftRate = "60s"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	// 开关「缺省 true」靠这两行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.KeepaliveEnabled = true
	// 活跃上报：开关缺省 true，但**时点缺省为空** ⇒ 默认不跑（见 ActivityHours 的注释）。
	c.Schedule.ActivityEnabled = true
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Features.SanitizeBlacklistFingerprints = true
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	c.Admin.CheckinLogPath = "./data/checkin-log.json"
	c.Admin.CheckinLogKeepDays = 30
	c.Admin.OAuthBaseURL = "https://copilot.tencent.com"
	c.Admin.ClientArchiveDir = "./data/client-login"
	c.RequestLogPath = "./data/request-log.jsonl"
	c.RequestLogKeepDays = 7
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// soft_rate_max：空 = 不解析（留给 pool 的默认值），非空但非法 → 报错。
	//
	// 为什么"空"与"非法"要区别对待：空是一个正常的"未配置"状态，
	// 而非法值（如 "2hours"）是用户写错了，静默回落会让他以为配置生效了。
	if s := strings.TrimSpace(c.Cooldown.SoftRateMax); s != "" {
		if c.SoftRateMaxDur, err = time.ParseDuration(s); err != nil {
			return fmt.Errorf("cooldown.soft_rate_max: %w", err)
		}
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	// 请求体上限：<=0 / 未配置一律回落 8 MiB（与改造前的硬编码常量一致，
	// 保证既有部署的**实际接受能力**不变，只是超限时从"静默截断"变成 413）。
	if c.Server.MaxBodyMB <= 0 {
		c.Server.MaxBodyMB = 8
	}
	// 上界兜底：配置里写一个荒谬的大值（如 1e6 MiB）会让 MaxBytesReader
	// 形同不存在，等于把内存交给客户端 —— 夹到 1024 MiB（1 GiB）。
	if c.Server.MaxBodyMB > 1024 {
		c.Server.MaxBodyMB = 1024
	}

	// 系统提示词：模式收敛 + **在此处就加载正文**。
	//
	// 加载放在 normalize 而不是第一次请求，是为了 fail fast：
	// 提示词文件路径写错时，进程**起不来**并直接说明原因，
	// 而不是启动成功、请求正常、只是自定义人格静默失效。
	c.PromptMode = strings.TrimSpace(c.Prompt.Mode)
	switch c.PromptMode {
	case prompt.ModeCustom, prompt.ModePassthrough:
		// 合法值原样保留
	case "":
		// 未配置 → 默认 custom（与 B 的缺省一致：从源头消灭指纹误报）
		c.PromptMode = prompt.ModeCustom
	default:
		// 非法值**报错**而不是静默当 custom：
		// 用户写 "passthru" / "Custom " 时，静默按 custom 处理会让
		// "我明明配了透传，为什么人格还是被换掉了" 变成一个无从下手的问题。
		return fmt.Errorf("prompt.mode=%q 非法：只接受 %q 或 %q",
			c.Prompt.Mode, prompt.ModeCustom, prompt.ModePassthrough)
	}
	// passthrough 模式下不需要加载正文：它不会用（降级用的 Degraded 是常量）。
	// 但**仍然校验文件**（若非空），因为那是一个明显的配置意图矛盾 ——
	// 配了自定义人格却选了透传，多半是改了一半，早点说出来比默默忽略好。
	text, err := prompt.Load(c.PromptMode, c.Prompt.File)
	if err != nil {
		return err
	}
	c.PromptText = text
	if c.PromptMode == prompt.ModePassthrough && strings.TrimSpace(c.Prompt.File) != "" {
		log.Printf("提示词：mode=passthrough 时 prompt.file 不生效（透传客户端原始 system）；" +
			"文件已校验可读，但内容不会被使用")
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 空数组与 null 反序列化后覆盖掉 Default() 的排程值（键缺席才保留），在此补齐。
	// 空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false，两者互不混淆。
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	// admin：解析后字段（供 main 直接取用），同时做保守兜底。
	c.CheckinLogPath = c.Admin.CheckinLogPath
	if c.CheckinLogPath == "" {
		c.CheckinLogPath = "./data/checkin-log.json"
	}
	c.CheckinLogKeepDays = c.Admin.CheckinLogKeepDays
	if c.CheckinLogKeepDays <= 0 {
		c.CheckinLogKeepDays = 30
	}
	c.OAuthBaseURL = c.Admin.OAuthBaseURL
	if c.OAuthBaseURL == "" {
		c.OAuthBaseURL = "https://copilot.tencent.com"
	}
	// 旅行自动领奖默认开启：用 *bool 而不是 bool，才能真正区分「没配」与「显式 false」。
	c.TravelAutoClaim = true
	if c.Admin.TravelAutoClaim != nil {
		c.TravelAutoClaim = *c.Admin.TravelAutoClaim
	}
	interval := c.Admin.TravelWatchIntervalSeconds
	if interval <= 0 {
		interval = 60
	}
	c.TravelWatchInterval = time.Duration(interval) * time.Second

	// 成长中心：扫描间隔 + 五个自动动作开关。
	//
	// 实测修正：/tasks/accept 是**接单**而非领奖 —— 它只把任务接进列表开始计进度，
	// 不发任何信用分（completed 的任务奖励早已发放）。既然它不直接产出收益、又是
	// 一次状态变更，默认关闭，由用户在「设置」里自行开启。
	// 只有补签默认开：那是纯收益（用本来只能补签的卡换回连登天数）。
	// 后三个默认关：兑换花连登天数、开盲盒花能量、抽奖花抽奖次数。
	gi := c.Admin.GrowthWatchIntervalSeconds
	if gi <= 0 {
		gi = 600
	}
	c.GrowthWatchInterval = time.Duration(gi) * time.Second
	// 领奖开关有两个可接受的键名，主键优先、别名回退（原因见 Admin.GrowthAutoClaimAlias）。
	claimFlag := c.Admin.GrowthAutoClaim
	if claimFlag == nil {
		claimFlag = c.Admin.GrowthAutoClaimAlias
	}
	c.GrowthAutoClaim = boolOr(claimFlag, true)
	// 自动接单默认开：见 Admin.GrowthAutoAcceptTasks 的注释（not_accepted 只是
	// 「还没领第一只 Buddy」的窄窗口，官方前端连按钮都不给）。
	c.GrowthAutoAccept = boolOr(c.Admin.GrowthAutoAcceptTasks, true)
	c.GrowthAutoMakeup = boolOr(c.Admin.GrowthAutoMakeup, true)
	c.GrowthAutoRedeem = boolOr(c.Admin.GrowthAutoRedeem, false)
	c.GrowthAutoOpen = boolOr(c.Admin.GrowthAutoOpen, false)
	c.GrowthAutoDraw = boolOr(c.Admin.GrowthAutoDraw, false)

	// 请求日志落盘默认值
	if c.RequestLogPath == "" {
		c.RequestLogPath = "./data/request-log.jsonl"
	}
	if c.RequestLogKeepDays <= 0 {
		c.RequestLogKeepDays = 7
	}

	// 本机客户端登录态。目录探测失败不算配置错误——该面板整体降级为不可用，
	// 其余功能不受影响（与 redis 未配置时降级成 Noop 同一思路）。
	c.ClientEnabled = boolOr(c.Admin.ClientEnabled, true)
	if c.ClientArchiveDir == "" {
		c.ClientArchiveDir = "./data/client-login"
	}
	if c.ClientAuthDir == "" {
		c.ClientAuthDir = clientlogin.DefaultClientDir()
	}

	// CodeArts 第二上游。
	//
	// 三条规则，都是为了"老配置零影响"：
	//  1. enabled 缺省 false —— 段缺席与显式 false 等价，都**不注册**上游
	//  2. auth_dir 缺省复用顶层 auth_dir —— codearts*.json 与 workbuddy-*.json
	//     前缀不同，共处一个目录互不干扰；要隔离再显式配
	//  3. refresh_interval_seconds 缺省 60；显式 <=0 表示关闭后台续期
	//     （不能把"未配置"与"关闭"混为一谈，否则老配置会被无意间开启后台任务）
	c.CodeartsEnabled = c.Codearts.Enabled
	c.CodeartsAuthDir = c.Codearts.AuthDir
	if c.CodeartsAuthDir == "" {
		// 缺省**不是**顶层 auth_dir 本身，而是它的下游子目录。
		//
		// # 为什么（这是实测踩出来的）
		//
		// 改前两个上游的凭证都往 `auths/` 根写。实测：codearts 授权
		// 成功后，凭证被写进 workbuddy 的目录 —— 而各上游的 LoadDir
		// 靠**文件名前缀**互相过滤，前缀一旦不匹配就会静默跳过。
		//
		// 现在按上游分子目录：`auths/workbuddy/`、`auths/codearts/`。
		// "哪个文件属于谁"由**位置**表达，不再依赖文件名约定。
		c.CodeartsAuthDir = filepath.Join(c.AuthDir, "codearts")
	}
	// workbuddy 侧同理：它自己的目录是 auths/workbuddy/。
	//
	// ⚠ 这是**破坏性**的目录变更（原来在 auths/ 根）。为了让迁移可逆，
	// 读取侧用 LoadDirCompat（同时扫子目录与根，按 uid 去重）——
	// 见 internal/auth/auth.go。
	//
	// AuthsBase 保留**父目录**，供兼容扫描用：
	// 迁移期凭证可能还在根下，只读子目录会看到"账号池突然空了"。
	c.AuthsBase = c.AuthDir
	c.AuthDir = filepath.Join(c.AuthDir, "workbuddy")
	// 未启用时不解析间隔 —— 避免"上游没启用却注册了后台任务"这类状态。
	// 显式 0 与未配置在**已启用**时都落到默认 60s：这个字段没有"关闭"语义
	// （关闭就是整个上游 enabled=false），因此不需要三态。
	if c.CodeartsEnabled {
		iv := c.Codearts.RefreshIntervalSeconds
		if iv <= 0 {
			iv = 60
		}
		c.CodeartsRefreshInterval = time.Duration(iv) * time.Second
	}
	// 并入账号池默认开（理由见 Codearts.PoolAccounts）。
	// 未启用时恒 false —— 不注册的上游不该在池子里留下任何痕迹。
	c.CodeartsPoolAccounts = c.CodeartsEnabled && boolOr(c.Codearts.PoolAccounts, true)

	// 页内添加账号的授权站点/端点。
	//
	// 默认值与 codearts 包里的常量**同源**（直接引用，不写第二份字面量）——
	// 两处各写一份会漂移，而漂移的表现是"登录成功但续期打到另一个区域"，
	// 极难排查。这里只是把常量"提升"到配置层，让部署可以覆盖它。
	if c.CodeartsOAuthPortal == "" {
		c.CodeartsOAuthPortal = codearts.DefaultPortalBase
	}
	if c.CodeartsOAuthSTS == "" {
		c.CodeartsOAuthSTS = codearts.DefaultSTSBase
	}
	return nil
}

// boolOr 取 *bool 的值，nil 时返回默认值。
// 用于把「没配」与「显式 false」区分开——这是所有新开关统一的做法。
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// firstNonEmpty 返回第一个非空字符串；全空时返回空串。
//
// 用于"配置可覆盖、缺省回落编译期常量"这类场景 ——
// 关键是把它写成显式的一次调用，而不是在各处散落 `if x != ""`，
// 否则迟早有一处忘了判空，表现为界面上出现一个空白的服务名。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// validateScheduleHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为关掉了签到，
// 实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误信息里指向正确的开关
// （checkin_enabled / keepalive_enabled），避免用户靠猜哨兵值来配。
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	return checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
