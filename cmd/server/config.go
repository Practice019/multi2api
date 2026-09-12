// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/clientlogin"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "60s"
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
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

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
		// 为什么需要后台续期：CodeArts 的 STS 凭证只有约 30 分钟寿命，
		// 纯靠请求路径惰性续期会让"空闲半小时后的第一个请求"多付一次
		// 续期往返（实测 1-3 秒）。后台预热把这个代价挪到空闲时间。
		//
		// <=0 或未配置一律取 60；本字段**没有**"关闭"语义 ——
		// 要关就整段 enabled=false（少一个三态就少一类误配）。
		RefreshIntervalSeconds int `json:"refresh_interval_seconds"`
	} `json:"codearts"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
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

	// Codearts 解析后（供 main 直接取用）。
	//
	// CodeartsEnabled 为 false 时下面两个字段无意义：不注册上游、不加载凭证。
	CodeartsEnabled         bool          `json:"-"`
	CodeartsAuthDir         string        `json:"-"`
	CodeartsRefreshInterval time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "60s"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	// 开关「缺省 true」靠这两行实现：Load 先取 Default() 再 json.Unmarshal 覆盖，
	// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
	c.Schedule.CheckinEnabled = true
	c.Schedule.KeepaliveEnabled = true
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
		c.CodeartsAuthDir = c.AuthDir
	}
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
