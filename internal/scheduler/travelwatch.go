// travelwatch.go 猫猫旅行自动领奖守卫 + 状态快照缓存。
//
// 为什么需要它：上游 travel/status 会返回精确的 arrive_at（Unix 秒）与 server_now，
// 所以「什么时候该领奖」是可预知的，不必盲轮询。这里把每个账号的下次检查时刻排好：
//   - traveling → 睡到 arrive_at（+小缓冲）再查，中间完全不打扰上游；
//   - arrived   → 立即领奖（自动模式下），然后今日不再针对该账号查询；
//   - idle      → 30 分钟后复查（兜住「已到站但当时没查到」的情况）。
//
// 同一份快照同时供管理台读取：UI 每次刷新读内存缓存，不发上游请求；
// 只有用户显式点「刷新」或 ?refresh=1 才强制全量回源。这样 N 个账号的
// 上游开销固定为「每个到站时刻一次」，与页面刷新频率无关。
package scheduler

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/upstream"
)

// 触发来源：自动（守卫领奖）。
const triggerAuto = "auto"

// travelIdleRecheck 空闲账号的复查间隔（无猫或已领完时的兜底）。
const travelIdleRecheck = 30 * time.Minute

// travelClaimedRecheck 领奖成功后的复查间隔（今日已无事可做）。
const travelClaimedRecheck = 6 * time.Hour

// travelArriveBuffer 到站时刻之后再多等一点：上游 arrive_at 是秒级整数，
// 且服务端结算可能有毫秒级延迟，踩点领奖偶发会拿到「还没到站」。
const travelArriveBuffer = 20 * time.Second

// TravelSnapshot 单个账号的旅行状态快照（管理台列表的数据单元）。
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
	RemainingSec      int64     `json:"remaining_sec,omitempty"` // >0 距到站，<=0 已到站
	DurationHours     int       `json:"duration_hours,omitempty"`
	RewardCredit      int64     `json:"reward_credit,omitempty"`
	DailyLimitReached bool      `json:"daily_limit_reached"`
	ObservedAt        time.Time `json:"observed_at"`
	Error             string    `json:"error,omitempty"`
}

// TravelActionResult 单账号旅行操作结果（管理台回显）。
type TravelActionResult struct {
	UID      string `json:"uid"`
	Action   string `json:"action"` // depart | claim | none
	Status   string `json:"status"` // ok | skip | fail
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits,omitempty"`
	State    string `json:"state,omitempty"`
	ArriveAt int64  `json:"arrive_at,omitempty"`
}

// travelWatchState 守卫的可变状态。
type travelWatchState struct {
	mu        sync.Mutex
	snapshots map[string]TravelSnapshot
	due       map[string]time.Time // 每个账号的下次检查时刻
	autoClaim bool
}

func newTravelWatchState(autoClaim bool) *travelWatchState {
	return &travelWatchState{
		snapshots: make(map[string]TravelSnapshot),
		due:       make(map[string]time.Time),
		autoClaim: autoClaim,
	}
}

// TravelAutoClaimEnabled 报告自动领奖是否开启。
func (s *Scheduler) TravelAutoClaimEnabled() bool {
	if s.travel == nil {
		return false
	}
	s.travel.mu.Lock()
	defer s.travel.mu.Unlock()
	return s.travel.autoClaim
}

// WatchInterval 返回守卫轮间隔（供管理台展示；<=0 表示回落默认）。
func (s *Scheduler) WatchInterval() time.Duration {
	if s.cfg.TravelWatchInterval > 0 {
		return s.cfg.TravelWatchInterval
	}
	return time.Minute
}

// SetTravelAutoClaim 运行时开关自动领奖。
func (s *Scheduler) SetTravelAutoClaim(on bool) {
	if s.travel == nil {
		return
	}
	s.travel.mu.Lock()
	s.travel.autoClaim = on
	s.travel.mu.Unlock()
	log.Printf("scheduler: 猫猫旅行自动领奖 -> %v", on)
}

// TravelSnapshots 返回当前缓存快照（按 uid 排序）；不触发任何上游请求。
func (s *Scheduler) TravelSnapshots() []TravelSnapshot {
	if s.travel == nil {
		return nil
	}
	s.travel.mu.Lock()
	defer s.travel.mu.Unlock()
	out := make([]TravelSnapshot, 0, len(s.travel.snapshots))
	for _, v := range s.travel.snapshots {
		// 冷却剩余时间随时间变化，读取时重算，避免展示滞后的秒数。
		if v.State == travelStateTraveling && v.ArriveAt > 0 {
			at := time.Unix(v.ArriveAt, 0)
			v.RemainingSec = int64(time.Until(at).Seconds())
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// travelFirstCreds 取账号凭证；无凭证返回 nil。
func (s *Scheduler) travelFirstCreds(uid string) *auth.Auth {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		return nil
	}
	return a
}

// probeTravel 查一次猫档案 + 旅行状态，写缓存，返回状态。
// buddyErr/stateErr 都只记录不中断：猫档案查不到不该让整行消失。
func (s *Scheduler) probeTravel(uid string) (*upstream.TravelState, *TravelSnapshot) {
	a := s.travelFirstCreds(uid)
	snap := TravelSnapshot{UID: uid, ObservedAt: time.Now()}
	if a == nil {
		snap.Error = "无可用凭证"
		s.storeSnapshot(uid, snap)
		return nil, &snap
	}
	snap.Nickname = a.Nickname

	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		snap.Error = "猫档案查询失败: " + shortErr(err)
		s.storeSnapshot(uid, snap)
		return nil, &snap
	}
	if buddy == nil {
		snap.HasBuddy = false
		s.storeSnapshot(uid, snap)
		return nil, &snap
	}
	snap.HasBuddy = true
	snap.BuddyName = buddy.Name
	snap.BuddyRarity = buddy.Rarity

	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		snap.Error = "旅行状态查询失败: " + shortErr(err)
		s.storeSnapshot(uid, snap)
		return nil, &snap
	}

	snap.State = ts.State
	snap.RecordID = ts.RecordID
	snap.DepartAt = ts.DepartAt
	snap.ArriveAt = ts.ArriveAt
	snap.DurationHours = ts.DurationHours
	snap.RewardCredit = ts.RewardCredit
	snap.DailyLimitReached = ts.DailyLimitReached
	if ts.Location != nil {
		snap.LocationName = ts.Location.Name
	}
	if rem, ok := ts.RemainingUntilArrive(time.Now()); ok {
		snap.RemainingSec = int64(rem.Seconds())
	}
	s.storeSnapshot(uid, snap)
	return ts, &snap
}

func (s *Scheduler) storeSnapshot(uid string, snap TravelSnapshot) {
	if s.travel == nil {
		return
	}
	s.travel.mu.Lock()
	s.travel.snapshots[uid] = snap
	s.travel.mu.Unlock()
}

// scheduleNextCheck 按刚查到的状态排下次检查时刻。
func (s *Scheduler) scheduleNextCheck(uid string, ts *upstream.TravelState, claimed bool) {
	if s.travel == nil {
		return
	}
	now := time.Now()
	var next time.Time
	switch {
	case claimed:
		next = now.Add(travelClaimedRecheck)
	case ts != nil && ts.State == travelStateTraveling:
		if at := ts.ArriveAtTime(now); !at.IsZero() {
			next = at.Add(travelArriveBuffer)
		} else {
			next = now.Add(time.Minute) // 上游没给 arrive_at：退化为分钟级轮询
		}
	default:
		next = now.Add(travelIdleRecheck)
	}
	s.travel.mu.Lock()
	s.travel.due[uid] = next
	s.travel.mu.Unlock()
}

// isDue 报告某账号是否到检查时刻（无记录视为立即到期）。
func (s *Scheduler) isDue(uid string, now time.Time) bool {
	if s.travel == nil {
		return true
	}
	s.travel.mu.Lock()
	defer s.travel.mu.Unlock()
	at, ok := s.travel.due[uid]
	return !ok || !now.Before(at)
}

// ---------------------------------------------------------------------------
// 单账号动作（管理台与守卫共用，保证「先查状态再动手」的逻辑只有一份）
// ---------------------------------------------------------------------------

// TravelDepartFor 单账号派猫：先查状态，不满足条件记 skip 并返回原因。
func (s *Scheduler) TravelDepartFor(uid, trigger string) TravelActionResult {
	res := TravelActionResult{UID: uid, Action: "depart"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	ts, _ := s.probeTravel(uid)
	if ts == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "旅行状态查询失败"
		return res
	}
	res.State, res.ArriveAt = ts.State, ts.ArriveAt
	switch {
	case ts.DailyLimitReached:
		res.Status, res.Detail = checkinlog.StatusSkip, "今日已派出（每日 1 次）"
	case ts.State == travelStateTraveling:
		res.Status, res.Detail = checkinlog.StatusSkip, "猫还在路上"
	case ts.State == travelStateArrived:
		res.Status, res.Detail = checkinlog.StatusSkip, "猫已到站，先领奖再派"
	default:
		if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
			res.Status, res.Detail = checkinlog.StatusFail, "派出失败: "+shortErr(err)
		} else {
			res.Status, res.Detail = checkinlog.StatusOK, "已派出"
			// 派出成功：立刻重新排下次检查到到站时刻，自动领奖才能踩点。
			if st, _ := s.probeTravel(uid); st != nil {
				res.ArriveAt = st.ArriveAt
				s.scheduleNextCheck(uid, st, false)
			} else {
				s.scheduleNextCheck(uid, nil, false)
			}
		}
	}
	s.recordTravel(uid, res, trigger)
	return res
}

// TravelClaimFor 单账号领奖：先查状态，未到站记 skip。
func (s *Scheduler) TravelClaimFor(uid, trigger string) TravelActionResult {
	res := TravelActionResult{UID: uid, Action: "claim"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	ts, _ := s.probeTravel(uid)
	if ts == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "旅行状态查询失败"
		return res
	}
	res.State, res.ArriveAt = ts.State, ts.ArriveAt
	switch {
	case ts.State != travelStateArrived:
		res.Status, res.Detail = checkinlog.StatusSkip, "猫未到站（"+ts.State+"），无奖可领"
	case ts.RecordID == 0:
		res.Status, res.Detail = checkinlog.StatusSkip, "上游未返回 record_id，无法领奖"
	default:
		reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
		if err != nil {
			res.Status, res.Detail = checkinlog.StatusFail, "领奖失败: "+shortErr(err)
		} else {
			res.Status, res.Detail, res.Credits = checkinlog.StatusOK, "已领奖", reward
			// 领完立刻把余额同步进池，界面马上能看到积分变化。
			if remain, qerr := s.cfg.Upstream.UserResource(a); qerr == nil {
				s.cfg.Pool.SetCredits(uid, remain)
			}
			s.scheduleNextCheck(uid, nil, true)
		}
	}
	s.recordTravel(uid, res, trigger)
	return res
}

func (s *Scheduler) recordTravel(uid string, res TravelActionResult, trigger string) {
	if s.cfg.Log == nil {
		return
	}
	kind := checkinlog.KindTravel
	if res.Status == checkinlog.StatusSkip {
		// 跳过也记：否则界面上「点了没反应」无从解释。
		s.record(uid, kind, res.Status, res.Detail, 0, trigger)
		return
	}
	s.record(uid, kind, res.Status, res.Detail, res.Credits, trigger)
}

// ---------------------------------------------------------------------------
// 全量扫描 / 自动领奖守卫
// ---------------------------------------------------------------------------

// RefreshTravel 扫描账号并刷新快照。force=false 时只查「到期」的账号。
// autoClaim 为真且发现已到站时立即领奖。返回扫描后的全量快照。
func (s *Scheduler) RefreshTravel(force, autoClaim bool) []TravelSnapshot {
	now := time.Now()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if !force && !s.isDue(st.UID, now) {
			continue
		}
		ts, _ := s.probeTravel(st.UID)
		claimed := false
		if autoClaim && ts != nil && ts.State == travelStateArrived && ts.RecordID != 0 {
			res := s.TravelClaimFor(st.UID, triggerAuto)
			claimed = res.Status == checkinlog.StatusOK
			if res.Status == checkinlog.StatusSkip {
				// 到站却领不到（比如 record_id 缺失）：退化为半小时后再试，不空转。
				s.scheduleNextCheck(st.UID, nil, false)
				continue
			}
		}
		s.scheduleNextCheck(st.UID, ts, claimed)
	}
	return s.TravelSnapshots()
}

// RunTravelWatcher 常驻守卫：启动即全量扫一次填满缓存，之后按每个账号的
// 到站时刻错峰检查。ctx 取消即退出。
func (s *Scheduler) RunTravelWatcher(ctx context.Context, interval time.Duration) {
	if s.travel == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	s.RefreshTravel(true, s.TravelAutoClaimEnabled())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshTravel(false, s.TravelAutoClaimEnabled())
		}
	}
}
