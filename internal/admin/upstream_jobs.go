// upstream_jobs.go 管理台对「上游业务扩展」的**消费方接口**。
//
// # 为什么接口定义在 admin 而不是 workbuddy
//
// 改造前 admin 直接依赖 scheduler 里那套成长/旅行的具体方法，于是核心的
// 管理台被迫认识 CodeBuddy 的领域概念（审计 128 处）。
//
// 现在反过来：**接口由消费方（admin）声明，上游实现它**。这是 Go 里
// 惯用的"隐式接口"用法 —— workbuddy 不需要 import admin，只要方法签名对得上
// 就自动满足。于是：
//
//	admin 不 import workbuddy   → 加新上游时 admin 零改动
//	workbuddy 不 import admin   → 上游可插拔
//
// 接线在 cmd/server：把 Provider 作为 UpstreamBusiness 注入 admin.Config。
//
// # 为什么类型也要在这里重新声明一遍
//
// 快照/结果结构体（GrowthSnapshot 等）此前在 scheduler 里，admin 直接引用它们。
// 搬走后若要引用 workbuddy 的类型就违反了架构约束。
// 所以这里用 **结构完全相同的本地类型**，由 cmd/server 在注入时做一次转换。
//
// 这与项目里既有的做法一致：内部目录状态在 server 与 admin 里各有一个
// 结构相同的类型，转换只在一个地方做（见 main.go 的 modelCatalogState）。
package admin

import "time"

// GrowthTaskView 界面用的任务视图（与 workbuddy.GrowthTaskView 逐字段对应）。
type GrowthTaskView struct {
	TaskCode      string `json:"task_code"`
	Title         string `json:"title"`
	Description   string `json:"description,omitempty"`
	HowTo         string `json:"how_to,omitempty"`
	Status        string `json:"status"`
	Current       int64  `json:"current,omitempty"`
	Target        int64  `json:"target,omitempty"`
	RewardCredit  int64  `json:"reward_credit,omitempty"`
	RewardEnergy  int64  `json:"reward_energy,omitempty"`
	Tag           string `json:"tag,omitempty"`
	Locked        bool   `json:"locked,omitempty"`
	Claimable     bool   `json:"claimable,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	ExpiresInDays int    `json:"expires_in_days,omitempty"`
	Expired       bool   `json:"expired,omitempty"`
	ExpiringSoon  bool   `json:"expiring_soon,omitempty"`
}

// GrowthSnapshot 单账号成长计划快照（与 workbuddy.GrowthSnapshot 逐字段对应）。
type GrowthSnapshot struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`

	TasksTotal     int `json:"tasks_total"`
	TasksCompleted int `json:"tasks_completed"`
	TasksAccepted  int `json:"tasks_accepted"`

	AcceptableCount  int              `json:"acceptable_count"`
	AcceptableCredit int64            `json:"acceptable_credit"`
	AcceptableEnergy int64            `json:"acceptable_energy"`
	Tasks            []GrowthTaskView `json:"tasks,omitempty"`

	PendingCount  int   `json:"pending_count"`
	PendingCredit int64 `json:"pending_credit"`
	PendingEnergy int64 `json:"pending_energy"`

	ClaimableCount  int   `json:"claimable_count"`
	ClaimableCredit int64 `json:"claimable_credit"`
	ClaimableEnergy int64 `json:"claimable_energy"`

	StreakDays        int      `json:"streak_days"`
	NextTier          string   `json:"next_tier,omitempty"`
	NextTierRemaining int      `json:"next_tier_remaining"`
	MakeupCards       int      `json:"makeup_cards"`
	MakeupDates       []string `json:"makeup_dates,omitempty"`
	RemainingDays     int      `json:"remaining_days"`

	Energy             int64 `json:"energy"`
	BlindBoxAffordable int   `json:"blind_box_affordable"`
	BlindBoxCost       int64 `json:"blind_box_cost"`
	LotteryChances     int   `json:"lottery_chances"`

	ObservedAt time.Time `json:"observed_at"`
	Error      string    `json:"error,omitempty"`
	Stale      bool      `json:"stale,omitempty"`
}

// GrowthActionResult 单账号成长操作结果（与 workbuddy.GrowthActionResult 逐字段对应）。
type GrowthActionResult struct {
	UID            string `json:"uid"`
	Action         string `json:"action"`
	Status         string `json:"status"`
	Detail         string `json:"detail,omitempty"`
	Credits        int64  `json:"credits,omitempty"`
	Energy         int64  `json:"energy,omitempty"`
	Count          int    `json:"count,omitempty"`
	AlreadyClaimed bool   `json:"already_claimed,omitempty"`
}

// TravelSnapshot 单个账号的旅行状态快照（与 workbuddy.TravelSnapshot 逐字段对应）。
type TravelSnapshot struct {
	UID               string    `json:"uid"`
	Nickname          string    `json:"nickname,omitempty"`
	HasBuddy          bool      `json:"has_buddy"`
	BuddyName         string    `json:"buddy_name,omitempty"`
	BuddyRarity       string    `json:"buddy_rarity,omitempty"`
	State             string    `json:"state,omitempty"`
	LocationName      string    `json:"location_name,omitempty"`
	RecordID          int64     `json:"record_id,omitempty"`
	DepartAt          int64     `json:"depart_at,omitempty"`
	ArriveAt          int64     `json:"arrive_at,omitempty"`
	RemainingSec      int64     `json:"remaining_sec,omitempty"`
	DurationHours     int       `json:"duration_hours,omitempty"`
	RewardCredit      int64     `json:"reward_credit,omitempty"`
	DailyLimitReached bool      `json:"daily_limit_reached"`
	ObservedAt        time.Time `json:"observed_at"`
	Error             string    `json:"error,omitempty"`
}

// TravelActionResult 单账号旅行操作结果（与 workbuddy.TravelActionResult 逐字段对应）。
type TravelActionResult struct {
	UID      string `json:"uid"`
	Action   string `json:"action"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits,omitempty"`
	State    string `json:"state,omitempty"`
	ArriveAt int64  `json:"arrive_at,omitempty"`
}

// UpstreamBusiness 上游的账号级业务能力（成长/旅行/签到搭车…）。
//
// admin 只通过它调用，**不认识具体是哪个上游**。
// 没有这些能力的上游可以不实现（注入 nil，相关面板降级）。
type UpstreamBusiness interface {
	// ---- 成长中心 ----

	RefreshGrowth(force, autoActions bool) []GrowthSnapshot
	GrowthSnapshots() []GrowthSnapshot
	GrowthToggles() (accept, makeup, redeem, open, draw, claim bool)
	GrowthWatchInterval() time.Duration
	SetGrowthToggles(accept, makeup, redeem, open, draw, claim bool)
	GrowthClaimFor(uid, taskCode, trigger string) GrowthActionResult
	GrowthAcceptFor(uid, taskCode, trigger string) GrowthActionResult
	GrowthRedeemFor(uid, tier, trigger string) GrowthActionResult
	GrowthMakeupFor(uid, date, trigger string) GrowthActionResult
	GrowthOpenFor(uid string, count int, trigger string) GrowthActionResult
	GrowthDrawFor(uid, trigger string) GrowthActionResult

	// ---- 猫猫旅行 ----

	RefreshTravel(force, autoClaim bool) []TravelSnapshot
	TravelSnapshots() []TravelSnapshot
	TravelAutoClaimEnabled() bool
	SetTravelAutoClaim(on bool)
	WatchInterval() time.Duration
	RunTravelManual()
	TravelDepartFor(uid, trigger string) TravelActionResult
	TravelClaimFor(uid, trigger string) TravelActionResult

	// ---- 额度 ----

	// RefreshCredits 只查余额并同步进池（不签到）。
	RefreshCredits(uid, trigger string) (CheckinView, bool)
}

// CheckinView 账号级动作结果（与 scheduler.CheckinResult 逐字段对应）。
//
// 放在这里而不是引用 scheduler：admin 已经 import scheduler（用它的
// NextWake/Hours 等**纯框架**能力），但把结果类型统一成 admin 自己的会让
// "上游业务"与"调度框架"两条线互不牵扯。
type CheckinView struct {
	UID      string `json:"uid"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits"`
	HasQuota bool   `json:"has_quota"`
}
