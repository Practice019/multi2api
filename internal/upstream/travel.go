// travel.go growth 域「猫猫旅行」接口：状态查询 / 派出 / 领奖 / 领养 / 协议。
// 全部走 chatBase（copilot.tencent.com，不带 /v2 前缀）+ BillingHeaders，信封同 doJSON。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// growth 域路径（实测）。
const (
	travelStatusPath   = "/activity/growth/buddy/travel/status"
	travelDepartPath   = "/activity/growth/buddy/travel/depart"
	travelClaimPath    = "/activity/growth/buddy/travel/claim"
	buddyInfoPath      = "/activity/growth/buddy/info"
	buddyFirstPath     = "/activity/growth/buddy/first"
	buddyAgreementPath = "/activity/growth/buddy/agreement"
)

// buddyTaskIncompleteMarker 领养门槛未达标的业务错误关键词（HTTP 400 时出现）。
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// Buddy 账号当前猫档案；nil（data.buddy 为 null）表示无猫。
//
// 字段名注意：上游实际用的是 instance_id，但历史夹具（travel_test.go）写的是 id，
// 两个都保留：Instance() 负责给出可用的那个，避免为了改 tag 而改既有测试。
type Buddy struct {
	ID           int64  `json:"id"`          // 兼容字段（旧夹具/旧上游）
	InstanceID   int64  `json:"instance_id"` // 上游实际字段
	Name         string `json:"name"`
	Personality  string `json:"personality"`
	Rarity       string `json:"rarity"`
	SoulDesc     string `json:"soul_desc"`
	ThumbnailURL string `json:"thumbnail_url"`
}

// Instance 返回可用的实例 ID（优先真实字段 instance_id，回落兼容字段 id）。
func (b *Buddy) Instance() int64 {
	if b == nil {
		return 0
	}
	if b.InstanceID != 0 {
		return b.InstanceID
	}
	return b.ID
}

// TravelLocation 目的地（四个地点收益/时长区间相同）。
type TravelLocation struct {
	ID            int    `json:"id"`
	Code          string `json:"code"`
	Name          string `json:"name"`
	DurationHours int    `json:"duration_hours"`
}

// TravelState 猫猫旅行状态。
//
// DepartAt/ArriveAt/ServerNow 是上游实测返回的 Unix 秒时间戳：
//   - ArriveAt 让「到点自动领奖」不必盲轮询，可以直接睡到到站时刻；
//   - ServerNow 让本机时钟偏差可被校正，否则本机时间不准会提前/延后领奖。
type TravelState struct {
	State             string          `json:"state"` // idle / traveling / arrived
	BuddyID           int64           `json:"buddy_id"`
	RecordID          int64           `json:"record_id"` // 在途/到站记录 id，claim 必带
	Location          *TravelLocation `json:"location"`
	DepartAt          int64           `json:"depart_at"`  // Unix 秒
	ArriveAt          int64           `json:"arrive_at"`  // Unix 秒，到站时刻
	ServerNow         int64           `json:"server_now"` // 上游当前时间，用于校正本机时钟
	DurationHours     int             `json:"duration_hours"`
	DailyLimitReached bool            `json:"daily_limit_reached"` // 今日已派出过（自然日 00:00 CST 重置）
	RewardCredit      int64           `json:"reward_credit"`       // 到站可领奖励积分
}

// ClockSkew 返回「本机 now 相对上游 server_now」的偏差（本机快则返回正）。
// 上游未返回 server_now 时返回 0（视为无偏差，退化为直接用本机时间）。
func (t *TravelState) ClockSkew(now time.Time) time.Duration {
	if t == nil || t.ServerNow <= 0 {
		return 0
	}
	return now.Sub(time.Unix(t.ServerNow, 0))
}

// ArriveAtTime 把 arrive_at 换算成本机时区的绝对时刻（已扣掉时钟偏差）。
// 无 arrive_at（未在途/上游未给）时返回零值。
func (t *TravelState) ArriveAtTime(now time.Time) time.Time {
	if t == nil || t.ArriveAt <= 0 {
		return time.Time{}
	}
	return time.Unix(t.ArriveAt, 0).Add(-t.ClockSkew(now))
}

// RemainingUntilArrive 距到站还有多久；已到站返回负值，不可知返回 0 且 ok=false。
func (t *TravelState) RemainingUntilArrive(now time.Time) (time.Duration, bool) {
	at := t.ArriveAtTime(now)
	if at.IsZero() {
		return 0, false
	}
	return at.Sub(now), true
}

// growthJSON 发 growth 域请求并解信封；body 为 nil 时不带请求体。
// 错误语义与 doJSON 一致：HTTP 非 2xx / 业务 code != 0 → *Error。
func (c *Client) growthJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	return c.doJSON(req)
}

// TravelStatus 查询猫猫旅行状态。
func (c *Client) TravelStatus(a *auth.Auth) (*TravelState, error) {
	data, err := c.growthJSON(a, http.MethodGet, travelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	var st TravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TravelDepart 派出猫旅行；locationID 实测 1~4（收益/时长区间相同）。
func (c *Client) TravelDepart(a *auth.Auth, locationID int) error {
	_, err := c.growthJSON(a, http.MethodPost, travelDepartPath, map[string]any{"location_id": locationID})
	return err
}

// TravelClaim 领取到站奖励，返回 reward_credit。
func (c *Client) TravelClaim(a *auth.Auth, recordID int64) (int64, error) {
	data, err := c.growthJSON(a, http.MethodPost, travelClaimPath, map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		// 奖励字段缺失不视为失败：调用方按 0 记日志即可。
		_ = json.Unmarshal(data, &resp)
	}
	return resp.RewardCredit, nil
}

// BuddyInfo 查询当前猫档案；返回 (nil, nil) 表示无猫（data.buddy 为 null）。
func (c *Client) BuddyInfo(a *auth.Auth) (*Buddy, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	// null / 缺字段 / 空对象都按无猫处理。
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BuddyFirst 领养第一只猫。无猫且已过 conversation 门槛时送 300 分。
// 门槛未达标返回 HTTP 400（见 IsBuddyTaskIncomplete），属预期行为，调用方静默跳过。
func (c *Client) BuddyFirst(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyFirstPath, map[string]any{})
	return err
}

// BuddyAgreement 同意协议（幂等，重复调用无副作用）。
func (c *Client) BuddyAgreement(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true})
	return err
}

// IsBuddyTaskIncomplete 判定「领养门槛未达标」：HTTP 400 + first_buddy 关键词。
// 该错误当日不应重试（避免对上游重试轰炸）。
func IsBuddyTaskIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), buddyTaskIncompleteMarker)
}
