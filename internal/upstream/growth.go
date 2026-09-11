// growth.go 成长中心（/v2/activity/growth/*）接口封装。
//
// 与 travel.go 的区别要特别注意：成长中心这几个端点**必须带 /v2 前缀**。
// 上游同时存在 /activity/growth/* 与 /v2/activity/growth/* 两套互不相同的 API——
// 例如 tasks：不带 /v2 返回 5 个带 level_name 的等级任务（养虾尝试…控虾大神），
// 带 /v2 才返回 18 个带 reward_credit/progress 的成长任务。混用会拿到完全不同的结构。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// growthBase 成长中心前缀（挂在 chatBase 之后）。
const growthBase = "/v2/activity/growth"

// 任务接单状态。取值取自官方前端 bundle（growthSpace-*.js）里的枚举，五个齐全，
// 不要只按「当时观察到的四个」写：
//
//	{not_accepted, accepted, in_progress, completed, claimed}
//
// 关键语义（实测，我判错过一次，这里留痕）：
//   - completed = 任务条件已达成，但**奖励尚未发放**；
//   - claimed   = 奖励**已领取**。
//
// 所以 completed 的任务必须再调一次 POST /tasks/{code}/claim 才真正拿到积分：
// 实测对 completed 的 chat_5 调 claim 后，状态变 claimed、积分 +100。
const (
	// GrowthStatusNotAccepted 未接单：可以 POST /tasks/accept 把它接进来。
	GrowthStatusNotAccepted = "not_accepted"
	// GrowthStatusAccepted 已接单：进度开始计数。奖励不会自动发放，达标后要 claim。
	GrowthStatusAccepted = "accepted"
	// GrowthStatusInProgress 进行中：已接单且进度已推进但未达标。
	// 该状态来自官方前端枚举；早前只实现了四个状态，落在 in_progress 的任务
	// 在界面四个视图里一个都不匹配，只有「全部」能看到。
	GrowthStatusInProgress = "in_progress"
	// GrowthStatusCompleted 条件已达成，**奖励待领取**（见 Claimable）。
	GrowthStatusCompleted = "completed"
	// GrowthStatusClaimed 奖励已领取（含系统直接结算类任务，如首只 Buddy）。
	GrowthStatusClaimed = "claimed"
)

// GrowthProgress 任务进度。**未接单**的任务不带 progress 字段。
type GrowthProgress struct {
	Current int64 `json:"current"`
	Target  int64 `json:"target"`
}

// GrowthTask 成长中心任务。
type GrowthTask struct {
	TaskCode          string          `json:"task_code"`
	Title             string          `json:"title"`
	Description       string          `json:"description"`
	TaskDesc          string          `json:"task_desc"`
	TaskType          string          `json:"task_type"` // single | auto
	JumpURL           string          `json:"jump_url"`
	RewardCredit      int64           `json:"reward_credit"`
	RewardEnergy      int64           `json:"reward_energy"`
	RewardBuddy       bool            `json:"reward_buddy"`
	BadgeName         string          `json:"badge_name"`
	AcceptStatus      string          `json:"accept_status"`
	Progress          *GrowthProgress `json:"progress"`
	IsPinned          bool            `json:"is_pinned"`
	IsNew             bool            `json:"is_new"`
	IsVisible         bool            `json:"is_visible"`
	IconURL           string          `json:"icon_url"`
	Tag               string          `json:"tag"`
	Locked            bool            `json:"locked"`
	HasReward         bool            `json:"has_reward"`
	ClaimedButtonText string          `json:"claimed_button_text"`

	// ValidStart / ValidEnd 任务的**有效期窗口**。
	//
	// 实测（18 个任务里 4 个带期限）：上游回的是 RFC3339 带时区的字符串
	// （如 "2026-09-30T23:59:00+08:00"），**其余任务是 null**。
	//
	// 用 *time.Time 而不是 time.Time 是本字段最容易做错的地方：
	// 零值 time.Time 的 IsZero() 为真、Format 出来是 "0001-01-01"，
	// 一旦用它表示"没有期限"，界面上就会出现「1970 年到期」这类假信息，
	// 而且无法与"解析失败"区分。指针让"没有"这件事明确可判（== nil）。
	ValidStart *time.Time `json:"valid_start"`
	ValidEnd   *time.Time `json:"valid_end"`
}

// UnmarshalJSON 自定义反序列化：valid_start/valid_end 用**宽容**解析。
//
// 为什么不能直接用 encoding/json 解 *time.Time：
// time.Time 的 UnmarshalJSON 遇到非法格式会**返回错误**，而错误会导致
// 整个 tasks 列表解析失败 —— 一个任务的坏时间戳就能让整块成长计划消失。
// 期限只是附加信息（用于提示"快到期了"），不是关键字段，
// 因此这里退化为「解析不了就当没有」，保住其余任务的可用性。
func (t *GrowthTask) UnmarshalJSON(data []byte) error {
	// 别名避免递归调用自己
	type alias GrowthTask
	aux := struct {
		*alias
		ValidStart *string `json:"valid_start"`
		ValidEnd   *string `json:"valid_end"`
	}{alias: (*alias)(t)}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	t.ValidStart = parseLooseTime(aux.ValidStart)
	t.ValidEnd = parseLooseTime(aux.ValidEnd)
	return nil
}

// parseLooseTime 解析上游的时间字符串；空/非法一律返回 nil（视为"没有期限"）。
//
// 容忍的形态：RFC3339 带时区（实测形态）、ISO8601 不带秒、纯日期。
// 上游改了格式也只是退化成"不显示期限"，不会让任务列表出错。
func parseLooseTime(s *string) *time.Time {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339,          // 2026-09-30T23:59:00+08:00（实测形态）
		"2006-01-02T15:04:05", // 无时区
		"2006-01-02 15:04:05", // 空格分隔
		"2006-01-02",          // 纯日期
	} {
		if ts, err := time.Parse(layout, v); err == nil {
			return &ts
		}
	}
	return nil
}

// Acceptable 报告任务是否可以「接单」。
//
// POST /tasks/accept 的语义是**接单**（把任务接进自己的列表开始记进度），
// **不是领奖**——返回体里的 status 是 "accepted"。领奖是另一个接口，见 Claimable。
// 只有 not_accepted 才可接单；对 completed 调 accept 会得到 "task already completed"。
//
// has_reward 不能当判据：它是「该任务设有奖励」的静态属性，实测 18 个任务全为 true。
func (t *GrowthTask) Acceptable() bool {
	return t != nil && !t.Locked && t.AcceptStatus == GrowthStatusNotAccepted
}

// Claimable 报告任务是否可以「领取奖励」。
//
// 判据是 completed：上游把「条件达成」与「奖励发放」拆成了两步，
// completed 只代表前者。领取成功后状态会变成 claimed（再领返回 already_claimed=true）。
// claimed 本身不可再领，accepted/in_progress 还没达标，都不算可领。
func (t *GrowthTask) Claimable() bool {
	return t != nil && t.AcceptStatus == GrowthStatusCompleted
}

// Pending 报告任务是否「还没做完」（界面上叫「待完成」）。
//
// 口径是「尚未达成条件」的三种状态：
//
//	not_accepted  还没接单 —— 更没开始做
//	accepted      已接单，进度为 0 或尚未推进
//	in_progress   已接单且已推进，但未达标
//
// 排除两种：
//
//	completed  条件已达成，只剩领奖 —— 归入「待领取」，不能在这里重复计数
//	claimed    已领完，彻底结束
//
// 为什么单独抽这个方法而不是在聚合处写 if：
// 之前界面的「待接单」只统计 Acceptable()（= not_accepted），
// 于是 accepted/in_progress 的任务在「待接单」和「待领取」两列里都不出现，
// 用户看到「待完成 0」但实际上还有 5 个任务要去做。
func (t *GrowthTask) Pending() bool {
	if t == nil {
		return false
	}
	switch t.AcceptStatus {
	case GrowthStatusNotAccepted, GrowthStatusAccepted, GrowthStatusInProgress:
		return true
	default:
		return false
	}
}

// GrowthAcceptResult 单个任务的接单结果。
type GrowthAcceptResult struct {
	TaskCode string `json:"task_code"`
	Status   string `json:"status"`  // accepted | error
	Message  string `json:"message"` // error 时的原因，如 "task already completed"
}

// GrowthAcceptResponse 接单接口的 data 段。
type GrowthAcceptResponse struct {
	Results []GrowthAcceptResult `json:"results"`
}

// RedeemTier 连登兑换档位。
type RedeemTier struct {
	Tier    string `json:"tier"`
	Days    int    `json:"days"`
	Credit  int64  `json:"credit"`
	Energy  int64  `json:"energy"`
	Cards   int    `json:"cards"`
	Chances int    `json:"chances"`
}

// StreakState 连登计划状态。
type StreakState struct {
	Streak struct {
		Days              int      `json:"days"`
		MonthTotalDays    int      `json:"month_total_days"`
		MonthConsumedDays int      `json:"month_consumed_days"`
		NextTier          string   `json:"next_tier"`
		NextTierRemaining int      `json:"next_tier_remaining"`
		MakeupDates       []string `json:"makeup_dates"` // 可补签日期
	} `json:"streak"`
	MakeupCards struct {
		Balance int `json:"balance"`
		Max     int `json:"max"`
	} `json:"makeup_cards"`
	RedemptionStatus struct {
		Tier7dCount    int          `json:"tier_7d_count"`
		Tier14dCount   int          `json:"tier_14d_count"`
		Tier28dCount   int          `json:"tier_28d_count"`
		Tier7dStatus   string       `json:"tier_7d_status"`
		Tier14dStatus  string       `json:"tier_14d_status"`
		Tier28dStatus  string       `json:"tier_28d_status"`
		RemainingDays  int          `json:"remaining_days"`
		Tiers          []RedeemTier `json:"tiers"`
		TotalConsumed  int          `json:"total_consumed"`
		MonthTotalDays int          `json:"month_total_days"`
	} `json:"redemption_status"`
	Timezone   string `json:"timezone"`
	LaunchDate string `json:"launch_date"`
}

// GrowthEnergy 能量账户。
type GrowthEnergy struct {
	Balance       int64 `json:"balance"`
	TotalConsumed int64 `json:"total_consumed"`
	TotalEarned   int64 `json:"total_earned"`
}

// GrowthBuddyQuota 开盲盒额度。
type GrowthBuddyQuota struct {
	Affordable   int   `json:"affordable"`
	Balance      int64 `json:"balance"`
	CostPerOpen  int64 `json:"cost_per_open"`
	MaxOpenCount int   `json:"max_open_count"`
}

// GrowthLottery 抽奖次数。
type GrowthLottery struct {
	Balance int `json:"balance"`
}

// GrowthRedeemSummary 连登兑换概览（三档）。
type GrowthRedeemSummary struct {
	StarterCount    int    `json:"starter_count"`
	AdvancedCount   int    `json:"advanced_count"`
	LegendaryCount  int    `json:"legendary_count"`
	StarterStatus   string `json:"starter_status"`
	AdvancedStatus  string `json:"advanced_status"`
	LegendaryStatus string `json:"legendary_status"`
	TotalConsumed   int    `json:"total_consumed"`
	RemainingDays   int    `json:"remaining_days"`
	MonthTotalDays  int    `json:"month_total_days"`
}

// GrowthLocation 旅行地点配置（比 travel/status 里的内联 location 更全）。
type GrowthLocation struct {
	ID               int    `json:"id"`
	Code             string `json:"code"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	DurationHoursMin int    `json:"duration_hours_min"`
	DurationHoursMax int    `json:"duration_hours_max"`
	RewardCreditMin  int64  `json:"reward_credit_min"`
	RewardCreditMax  int64  `json:"reward_credit_max"`
	Sort             int    `json:"sort"`
}

// GrowthTravelConfig 旅行活动配置。
type GrowthTravelConfig struct {
	Locations []GrowthLocation `json:"locations"`
	ServerNow int64            `json:"server_now"`
}

// GrowthTaskList 任务列表响应体。
type GrowthTaskList struct {
	Tasks []GrowthTask `json:"tasks"`
}

// newClientToken 生成活动接口要求的防重放 token。
// 上游用 crypto.randomUUID() 拼成 "u-<uuid>"，服务端只做幂等去重、不校验格式
// （缺了它 /lottery/draw 直接 400）。这里用 crypto/rand 手搓 UUIDv4，
// 避免为一行代码引入新依赖。
func newClientToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下退化为固定前缀 + 全零：服务端只要求字段存在，
		// 幂等性由服务端按 token 去重，重复 token 只会让那次请求被当成重放。
		return "u-00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return "u-" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// GrowthTasks 拉取成长任务列表。
func (c *Client) GrowthTasks(a *auth.Auth) ([]GrowthTask, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBase+"/tasks", nil)
	if err != nil {
		return nil, err
	}
	var resp GrowthTaskList
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

// GrowthStreak 拉取连登/补签/兑换档位状态。
func (c *Client) GrowthStreak(a *auth.Auth) (*StreakState, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBase+"/streak", nil)
	if err != nil {
		return nil, err
	}
	var st StreakState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// GrowthEnergy 查询能量余额。
func (c *Client) GrowthEnergy(a *auth.Auth) (*GrowthEnergy, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBase+"/energy", nil)
	if err != nil {
		return nil, err
	}
	var e GrowthEnergy
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// GrowthBuddyQuota 查询开盲盒额度。
func (c *Client) GrowthBuddyQuota(a *auth.Auth) (*GrowthBuddyQuota, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBase+"/buddy/quota", nil)
	if err != nil {
		return nil, err
	}
	var q GrowthBuddyQuota
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// GrowthLotteryChances 查询抽奖次数。
func (c *Client) GrowthLotteryChances(a *auth.Auth) (*GrowthLottery, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBase+"/lottery/chances", nil)
	if err != nil {
		return nil, err
	}
	var l GrowthLottery
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// GrowthRedeemSummary 连登兑换概览。
func (c *Client) GrowthRedeemSummary(a *auth.Auth) (*GrowthRedeemSummary, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBase+"/redeem/summary", nil)
	if err != nil {
		return nil, err
	}
	var s GrowthRedeemSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GrowthTravelConfig 旅行活动配置（含各地点时长/奖励区间）。
func (c *Client) GrowthTravelConfig(a *auth.Auth) (*GrowthTravelConfig, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthBase+"/buddy/travel/config", nil)
	if err != nil {
		return nil, err
	}
	var cfg GrowthTravelConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// GrowthBadge 领取任务时可能附带的徽章。
type GrowthBadge struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// GrowthClaimResult 是领取任务奖励的返回。
//
// 实测形态：
//
//	{"code":0,"data":{"already_claimed":false,"credit":100,"energy":5,
//	                  "badge":{"name":"尝鲜达人",...},"share_uuid":"..."}}
//
// 已领过的任务返回 already_claimed=true 且 credit/energy 均为 0 —— 这是**幂等成功**，
// 不是错误，自动领奖要靠这个字段区分「这次真领到了」和「早就领过」。
type GrowthClaimResult struct {
	AlreadyClaimed bool         `json:"already_claimed"`
	Credit         int64        `json:"credit"`
	Energy         int64        `json:"energy"`
	Badge          *GrowthBadge `json:"badge"`
	ShareUUID      string       `json:"share_uuid"`
}

// GrowthClaim 领取某个已完成任务的奖励。
//
// 路径挂在 /v2/activity/growth 之下（官方前端用的是不带 /v2 的
// /activity/growth/tasks/{code}/claim，实测两种都能通，这里跟随本包其余端点统一用 /v2）。
// 请求体为空对象；对不存在的任务返回 HTTP 400。
func (c *Client) GrowthClaim(a *auth.Auth, taskCode string) (*GrowthClaimResult, error) {
	if taskCode == "" {
		return nil, fmt.Errorf("claim: 缺少 task_code")
	}
	data, err := c.growthJSON(a, http.MethodPost,
		growthBase+"/tasks/"+url.PathEscape(taskCode)+"/claim", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res GrowthClaimResult
	// data 为空（例如已被领取且上游不返回明细）时按「无事发生」处理，
	// 不让解析失败把一次成功的领取变成错误。
	if len(data) > 0 {
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, err
		}
	}
	return &res, nil
}

// GrowthAcceptTasks 批量接单，返回每个任务的逐条结果。
//
// 请求体必须是数组形状 {"task_codes": ["a","b"]}。实测把单个写成 {"task_code": "a"}
// 会被服务端判为参数校验失败，返回 HTTP 400 {"code":400,"msg":"invalid request"}——
// 注意这种 code=400 是**形状错误**，与业务拒绝（code=10001 带中文原因）形态不同，
// 排查时可用这个差异快速区分。
func (c *Client) GrowthAcceptTasks(a *auth.Auth, codes []string) ([]GrowthAcceptResult, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	data, err := c.growthJSON(a, http.MethodPost, growthBase+"/tasks/accept",
		map[string]any{"task_codes": codes})
	if err != nil {
		return nil, err
	}
	var resp GrowthAcceptResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// GrowthMakeup 补签指定日期，消耗一张补签卡。
func (c *Client) GrowthMakeup(a *auth.Auth, targetDate string) error {
	_, err := c.growthJSON(a, http.MethodPost, growthBase+"/makeup",
		map[string]any{"target_date": targetDate, "client_token": newClientToken()})
	return err
}

// GrowthRedeem 连登兑换。tier 取 "7d" / "14d" / "28d"（上游另有 starter/advanced/legendary
// 的概览口径，实测 /redeem 收的是天数档）。
func (c *Client) GrowthRedeem(a *auth.Auth, tier string) error {
	_, err := c.growthJSON(a, http.MethodPost, growthBase+"/redeem",
		map[string]any{"tier": tier, "client_token": newClientToken()})
	return err
}

// GrowthLotteryDraw 抽奖一次，消耗一次抽奖机会。
func (c *Client) GrowthLotteryDraw(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, growthBase+"/lottery/draw",
		map[string]any{"client_token": newClientToken()})
	return err
}

// GrowthOpenBuddy 开盲盒 count 次，每次消耗 quota.cost_per_open 能量。
func (c *Client) GrowthOpenBuddy(a *auth.Auth, count int) error {
	if count <= 0 {
		count = 1
	}
	_, err := c.growthJSON(a, http.MethodPost, growthBase+"/buddy/open",
		map[string]any{"count": count, "client_token": newClientToken()})
	return err
}
