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
//
// # 搬运说明（Task 3b）
//
// 本文件原先在 internal/scheduler/travelwatch.go。
package workbuddy

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
func (p *Provider) TravelAutoClaimEnabled() bool {
	if p.travel == nil {
		return false
	}
	p.travel.mu.Lock()
	defer p.travel.mu.Unlock()
	return p.travel.autoClaim
}

// WatchInterval 返回守卫轮间隔（供管理台展示；<=0 表示回落默认）。
func (p *Provider) WatchInterval() time.Duration {
	if p.cfg.TravelWatchInterval > 0 {
		return p.cfg.TravelWatchInterval
	}
	return time.Minute
}

// SetTravelAutoClaim 运行时开关自动领奖。
func (p *Provider) SetTravelAutoClaim(on bool) {
	if p.travel == nil {
		return
	}
	p.travel.mu.Lock()
	p.travel.autoClaim = on
	p.travel.mu.Unlock()
	log.Printf("scheduler: 猫猫旅行自动领奖 -> %v", on)
}

// TravelSnapshots 返回当前缓存快照（按 uid 排序）；不触发任何上游请求。
//
// # 为什么这里也要按归属过滤（而不是只靠 RefreshTravel 不产生脏快照）
//
// 快照 map 是**只写不删**的（全文件没有一处 delete），而 probeTravel
// 可以由**任意 uid** 经单账号端点直接进入（TravelDepartFor / TravelClaimFor
// 都只校验"池里有凭证"，不校验归属）。于是只要别家上游的 uid 曾经进来过一次，
// 它就会**永久**留在这个 map 里 —— 刷新界面对它无效，因为补空行那条路径
// （accountList）已经修好了，脏数据是从**快照**里出来的。
//
// 判据与 accountList 一致：出口处收口。快照留在 map 里无害，
// 只要它不再出去（也不再驱动 anyDue 的控制流，见那里的注释）。
func (p *Provider) TravelSnapshots() []TravelSnapshot {
	if p.travel == nil {
		return nil
	}
	// 归属判据来自账号池。池未接线时（ownAccounts 返回 nil）不过滤 ——
	// 那对应"没有池"的部署，快照本来也不可能存在。
	own := p.ownUIDs()
	p.travel.mu.Lock()
	defer p.travel.mu.Unlock()
	out := make([]TravelSnapshot, 0, len(p.travel.snapshots))
	for _, v := range p.travel.snapshots {
		if own != nil && !own[v.UID] {
			continue
		}
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

// creds 取账号凭证；无凭证返回 nil。
func (p *Provider) creds(uid string) *auth.Auth {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		return nil
	}
	return a
}

// probeTravel 查一次猫档案 + 旅行状态，写缓存，返回状态。
// buddyErr/stateErr 都只记录不中断：猫档案查不到不该让整行消失。
func (p *Provider) probeTravel(uid string) (*upstream.TravelState, *TravelSnapshot) {
	a := p.creds(uid)
	snap := TravelSnapshot{UID: uid, ObservedAt: time.Now()}
	if a == nil {
		snap.Error = "无可用凭证"
		p.storeSnapshot(uid, snap)
		return nil, &snap
	}
	snap.Nickname = a.Nickname

	buddy, err := p.client.BuddyInfo(a)
	if err != nil {
		snap.Error = "猫档案查询失败: " + shortErr(err)
		p.storeSnapshot(uid, snap)
		return nil, &snap
	}
	if buddy == nil {
		snap.HasBuddy = false
		p.storeSnapshot(uid, snap)
		return nil, &snap
	}
	snap.HasBuddy = true
	snap.BuddyName = buddy.Name
	snap.BuddyRarity = buddy.Rarity

	ts, err := p.client.TravelStatus(a)
	if err != nil {
		snap.Error = "旅行状态查询失败: " + shortErr(err)
		p.storeSnapshot(uid, snap)
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
	p.storeSnapshot(uid, snap)
	return ts, &snap
}

func (p *Provider) storeSnapshot(uid string, snap TravelSnapshot) {
	if p.travel == nil {
		return
	}
	p.travel.mu.Lock()
	p.travel.snapshots[uid] = snap
	p.travel.mu.Unlock()
}

// scheduleNextCheck 按刚查到的状态排下次检查时刻。
func (p *Provider) scheduleNextCheck(uid string, ts *upstream.TravelState, claimed bool) {
	if p.travel == nil {
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
	p.travel.mu.Lock()
	p.travel.due[uid] = next
	p.travel.mu.Unlock()
}

// IsDue 报告某账号是否到检查时刻（无记录视为立即到期）。
func (p *Provider) IsDue(uid string, now time.Time) bool {
	if p.travel == nil {
		return true
	}
	p.travel.mu.Lock()
	defer p.travel.mu.Unlock()
	at, ok := p.travel.due[uid]
	return !ok || !now.Before(at)
}

// anyDue 报告是否有任一账号到期（守卫轮的 Due 判据）。
//
// 为什么需要它：守卫轮不是固定间隔任务 —— 每轮该不该打上游，取决于
// 「有没有账号到了它的下次检查时刻」。用一个全局固定间隔会让在途账号
// 被反复打扰（实测这会明显抬高上游请求量）。所以把判断交给上游自己。
//
// 无快照（首次）视为到期：启动时要先全量扫一趟把缓存填满。
//
// # 为什么遍历 ownAccounts() 而不是 snapshots map
//
// 原先遍历 snapshots。那让"快照里恰好有什么"变成了**控制流判据**：
// 快照 map 只写不删，而探测可由任意 uid 经单账号端点进入，于是
// 一个别家上游的 uid 一旦留下快照，就会让守卫轮**永久**认为"有账号到期"
// 而被反复唤醒（空转上游请求）。把判据换回"本上游的账号"之后，
// 快照重新只是**数据**，不再能驱动调度。
//
// 首扫语义保持不变：本上游**有账号但无快照**时走到 !ok 分支仍返回 true，
// 与原先"两 map 皆空则 true"的效果一致（有测试钉住）。
func (p *Provider) anyDue(now time.Time) bool {
	if p.travel == nil {
		return false
	}
	// 先取本上游账号（在锁外调用 ownAccounts，避免持 travel 锁去读账号池）。
	accs := p.ownAccounts()
	if len(accs) == 0 {
		// 没有本上游的账号：无守卫轮可言。与"池未接线"一致。
		return false
	}
	p.travel.mu.Lock()
	defer p.travel.mu.Unlock()
	for _, a := range accs {
		at, ok := p.travel.due[a.UID]
		if !ok || !now.Before(at) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 单账号动作（管理台与守卫共用，保证「先查状态再动手」的逻辑只有一份）
// ---------------------------------------------------------------------------

// TravelDepartFor 单账号派猫：先查状态，不满足条件记 skip 并返回原因。
func (p *Provider) TravelDepartFor(uid, trigger string) TravelActionResult {
	res := TravelActionResult{UID: uid, Action: "depart"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	ts, _ := p.probeTravel(uid)
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
		if err := p.client.TravelDepart(a, TravelLocationID); err != nil {
			res.Status, res.Detail = checkinlog.StatusFail, "派出失败: "+shortErr(err)
		} else {
			res.Status, res.Detail = checkinlog.StatusOK, "已派出"
			// 派出成功：立刻重新排下次检查到到站时刻，自动领奖才能踩点。
			if st, _ := p.probeTravel(uid); st != nil {
				res.ArriveAt = st.ArriveAt
				p.scheduleNextCheck(uid, st, false)
			} else {
				p.scheduleNextCheck(uid, nil, false)
			}
		}
	}
	p.recordTravel(uid, res, trigger)
	return res
}

// TravelClaimFor 单账号领奖：先查状态，未到站记 skip。
func (p *Provider) TravelClaimFor(uid, trigger string) TravelActionResult {
	res := TravelActionResult{UID: uid, Action: "claim"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	ts, _ := p.probeTravel(uid)
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
		reward, err := p.client.TravelClaim(a, ts.RecordID)
		if err != nil {
			res.Status, res.Detail = checkinlog.StatusFail, "领奖失败: "+shortErr(err)
		} else {
			res.Status, res.Detail, res.Credits = checkinlog.StatusOK, "已领奖", reward
			// 领完立刻把余额同步进池，界面马上能看到积分变化。
			if remain, qerr := p.client.UserResource(a); qerr == nil {
				p.cfg.Pool.SetCredits(uid, remain)
			}
			p.scheduleNextCheck(uid, nil, true)
		}
	}
	p.recordTravel(uid, res, trigger)
	return res
}

func (p *Provider) recordTravel(uid string, res TravelActionResult, trigger string) {
	if p.cfg.Log == nil {
		return
	}
	kind := checkinlog.KindTravel
	if res.Status == checkinlog.StatusSkip {
		// 跳过也记：否则界面上「点了没反应」无从解释。
		p.record(uid, kind, res.Status, res.Detail, 0, trigger)
		return
	}
	p.record(uid, kind, res.Status, res.Detail, res.Credits, trigger)
}

// ---------------------------------------------------------------------------
// 全量扫描 / 自动领奖守卫
// ---------------------------------------------------------------------------

// RefreshTravel 扫描账号并刷新快照。force=false 时只查「到期」的账号。
// autoClaim 为真且发现已到站时立即领奖。返回扫描后的全量快照。
func (p *Provider) RefreshTravel(force, autoClaim bool) []TravelSnapshot {
	now := time.Now()
	// 只扫本上游的号：给别家上游的账号建快照毫无意义（它的凭证打不通
	// workbuddy 的旅行接口），还会把那些号塞进本上游的面板。
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		if !force && !p.IsDue(st.UID, now) {
			continue
		}
		ts, _ := p.probeTravel(st.UID)
		claimed := false
		if autoClaim && ts != nil && ts.State == travelStateArrived && ts.RecordID != 0 {
			res := p.TravelClaimFor(st.UID, triggerAuto)
			claimed = res.Status == checkinlog.StatusOK
			if res.Status == checkinlog.StatusSkip {
				// 到站却领不到（比如 record_id 缺失）：退化为半小时后再试，不空转。
				p.scheduleNextCheck(st.UID, nil, false)
				continue
			}
		}
		p.scheduleNextCheck(st.UID, ts, claimed)
	}
	return p.TravelSnapshots()
}

// RunTravelWatcher 常驻守卫：启动即全量扫一次填满缓存，之后按每个账号的
// 到站时刻错峰检查。ctx 取消即退出。
//
// 注意：生产路径**不再**由 cmd/server 直接调用它 —— 它现在通过 Provider.Jobs()
// 注册给核心调度器，由调度器按 Due 判断错峰执行。保留这个方法是为了让
// 既有调用点（以及"立即跑一轮"的语义）零改动。
func (p *Provider) RunTravelWatcher(ctx context.Context, interval time.Duration) {
	if p.travel == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	p.RefreshTravel(true, p.TravelAutoClaimEnabled())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.RefreshTravel(false, p.TravelAutoClaimEnabled())
		}
	}
}
