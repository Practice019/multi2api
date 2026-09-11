// growthwatch.go 成长中心守卫：状态快照缓存 + 自动领取/兑换/补签/开盲盒。
//
// 与 travelwatch.go 同构（快照缓存 + 到期门控 + 单一扫描入口），但成长中心的
// 动作比旅行多且**会消耗资源**，因此这里把每个动作拆成独立开关：
//
//	默认开启（纯收益，无机会成本）：领任务奖励、补签
//	默认关闭（要花资源，需用户明确同意）：连登兑换、开盲盒、抽奖
//
// 之所以把「会花资源的」默认关掉：自动开盲盒会消耗能量、自动兑换会消耗连登天数，
// 这些是用户的资产，不该由一次「保存设置」之外的默认值替他决定。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/upstream"
)

// GrowthStatusReason 快照里的错误原因上限，避免把上游长文本灌进快照。
const growthErrMax = 160

// GrowthTaskView 界面用的任务视图。
//
// 关键：快照里存**全部**任务，而不只是「待接单」队列。早先只存待接单的，
// 用户点完接单后明细表立刻变空，看起来像数据丢了——实际是队列被消费完了。
// 存全量之后，接单只是让某行状态从 not_accepted 变成 accepted，行本身还在。
type GrowthTaskView struct {
	TaskCode string `json:"task_code"`
	Title    string `json:"title"`
	// Description 是任务的**达成条件**（上游 task_desc），例如
	// 「在「设计创意」模式下成功创建1个画布。」
	Description string `json:"description,omitempty"`
	// HowTo 是**怎么做**的操作指引（上游 description），例如
	// 「前往Workbuddy，进入「新建任务」，切换滑块到「设计创意」模式……」
	// 两个字段在上游是分开的，之前只带了达成条件，界面上缺「怎么完成」这一步。
	HowTo        string `json:"how_to,omitempty"`
	Status       string `json:"status"` // not_accepted | accepted | in_progress | completed | claimed
	Current      int64  `json:"current,omitempty"`
	Target       int64  `json:"target,omitempty"`
	RewardCredit int64  `json:"reward_credit,omitempty"`
	RewardEnergy int64  `json:"reward_energy,omitempty"`
	Tag          string `json:"tag,omitempty"`
	Locked       bool   `json:"locked,omitempty"`
	// Claimable 为真表示这个任务的奖励现在可以领（即 status == completed）。
	// 把语义算在服务端，界面只负责显示「领取」按钮，不必各自理解状态含义。
	Claimable bool `json:"claimable,omitempty"`
}

// GrowthSnapshot 单账号成长计划快照。
type GrowthSnapshot struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`

	// 任务
	TasksTotal int `json:"tasks_total"`
	// TasksCompleted 只数 **completed**（条件已达成、奖励尚未领取）。
	// 注意不含 claimed（已领取）—— 所以「已完成=0」并不代表没有做成的任务，
	// 可能只是奖励都领完了（那时该看 Tasks 里各行的 status，或 ClaimableCount=0）。
	TasksCompleted int `json:"tasks_completed"`
	// TasksAccepted 已接单、进度未达标，含 accepted 与 in_progress 两种状态。
	TasksAccepted int `json:"tasks_accepted"`
	// AcceptableCount/…… 指「未接单、可接单」的任务数。
	// 与 Claimable* 是两件事：接单是把任务接进来开始记进度，领奖是把已达成任务的奖励拿回来。
	AcceptableCount  int              `json:"acceptable_count"`
	AcceptableCredit int64            `json:"acceptable_credit"` // 这些任务完成后可得（不是现在可领）
	AcceptableEnergy int64            `json:"acceptable_energy"`
	Tasks            []GrowthTaskView `json:"tasks,omitempty"` // 全部任务（含各状态）

	// ClaimableCount/…… 指「条件已达成、奖励还没领」的任务数，也就是**现在就能领到的**。
	// 上游把「达成」和「发奖」拆成两步：completed 只是达成，必须调 claim 才真的到账，
	// 领完状态变 claimed。所以这个数才是用户关心的「还有多少分能拿」。
	ClaimableCount  int   `json:"claimable_count"`
	ClaimableCredit int64 `json:"claimable_credit"`
	ClaimableEnergy int64 `json:"claimable_energy"`

	// 连登
	StreakDays        int      `json:"streak_days"`
	NextTier          string   `json:"next_tier,omitempty"`
	NextTierRemaining int      `json:"next_tier_remaining"`
	MakeupCards       int      `json:"makeup_cards"`
	MakeupDates       []string `json:"makeup_dates,omitempty"`
	RemainingDays     int      `json:"remaining_days"`

	// 资源
	Energy             int64 `json:"energy"`
	BlindBoxAffordable int   `json:"blind_box_affordable"`
	BlindBoxCost       int64 `json:"blind_box_cost"`
	LotteryChances     int   `json:"lottery_chances"`

	// ObservedAt 是**数据**的观测时刻；刷新失败时不会被更新（配合 Stale 使用）。
	ObservedAt time.Time `json:"observed_at"`
	Error      string    `json:"error,omitempty"`
	// Stale 为真表示本次刷新失败、下面展示的是上一次成功的数据。
	Stale bool `json:"stale,omitempty"`
}

// GrowthActionResult 单账号成长操作结果。
type GrowthActionResult struct {
	UID    string `json:"uid"`
	Action string `json:"action"` // accept | claim | redeem | makeup | open | draw
	Status string `json:"status"` // ok | skip | fail
	Detail string `json:"detail,omitempty"`
	// Credits 是本次**实际到账**的信用分。领奖时用上游的 already_claimed 区分：
	// 早已领过的任务返回 0，不计入。
	Credits int64 `json:"credits,omitempty"`
	Energy  int64 `json:"energy,omitempty"`
	Count   int   `json:"count,omitempty"`
	// AlreadyClaimed 为真表示这次调用是幂等空转（奖励之前就领过了）。
	AlreadyClaimed bool `json:"already_claimed,omitempty"`
}

type growthWatchState struct {
	mu        sync.Mutex
	snapshots map[string]GrowthSnapshot
	due       map[string]time.Time

	// 六个独立开关。
	// autoClaim 默认开：completed 只代表条件达成，不 claim 就永远拿不到分。
	// autoAccept 默认关：它是状态变更且不直接产出收益。
	// 后面三个会花资源（连登天数/能量/抽奖次数），默认关。
	autoClaim  bool // 自动领奖
	autoAccept bool // 自动接单
	autoMakeup bool // 补签（默认开，只花补签卡）
	autoRedeem bool // 连登兑换
	autoOpen   bool // 开盲盒
	autoDraw   bool // 抽奖
}

func newGrowthWatchState(accept, makeup, redeem, open, draw, claim bool) *growthWatchState {
	return &growthWatchState{
		snapshots:  make(map[string]GrowthSnapshot),
		due:        make(map[string]time.Time),
		autoClaim:  claim,
		autoAccept: accept,
		autoMakeup: makeup,
		autoRedeem: redeem,
		autoOpen:   open,
		autoDraw:   draw,
	}
}

// GrowthToggles 返回六个自动动作的开关状态。
func (s *Scheduler) GrowthToggles() (accept, makeup, redeem, open, draw, claim bool) {
	if s.growth == nil {
		return
	}
	s.growth.mu.Lock()
	defer s.growth.mu.Unlock()
	return s.growth.autoAccept, s.growth.autoMakeup, s.growth.autoRedeem,
		s.growth.autoOpen, s.growth.autoDraw, s.growth.autoClaim
}

// SetGrowthToggles 运行时设置六个自动动作开关。
func (s *Scheduler) SetGrowthToggles(accept, makeup, redeem, open, draw, claim bool) {
	if s.growth == nil {
		return
	}
	s.growth.mu.Lock()
	s.growth.autoAccept, s.growth.autoMakeup = accept, makeup
	s.growth.autoRedeem, s.growth.autoOpen, s.growth.autoDraw = redeem, open, draw
	s.growth.autoClaim = claim
	s.growth.mu.Unlock()
	log.Printf("scheduler: 成长中心自动动作 -> 领奖=%v 接单=%v 补签=%v 兑换=%v 开盲盒=%v 抽奖=%v",
		claim, accept, makeup, redeem, open, draw)
}

// GrowthWatchInterval 返回扫描间隔（供管理台展示）。
func (s *Scheduler) GrowthWatchInterval() time.Duration {
	if s.cfg.GrowthWatchInterval > 0 {
		return s.cfg.GrowthWatchInterval
	}
	return 10 * time.Minute
}

// GrowthSnapshots 返回缓存快照（按 uid 排序），不发上游请求。
func (s *Scheduler) GrowthSnapshots() []GrowthSnapshot {
	if s.growth == nil {
		return nil
	}
	s.growth.mu.Lock()
	defer s.growth.mu.Unlock()
	out := make([]GrowthSnapshot, 0, len(s.growth.snapshots))
	for _, v := range s.growth.snapshots {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (s *Scheduler) storeGrowthSnapshot(uid string, snap GrowthSnapshot) {
	if s.growth == nil {
		return
	}
	s.growth.mu.Lock()
	s.growth.snapshots[uid] = snap
	s.growth.mu.Unlock()
}

// probeGrowth 拉一次成长中心全量状态（5 个 GET），组装快照。
// 任一子项失败只记录 error，不阻断其它子项——部分数据也比整行空白有用。
func (s *Scheduler) probeGrowth(uid string) *GrowthSnapshot {
	snap := GrowthSnapshot{UID: uid}
	a := s.travelFirstCreds(uid)
	if a == nil {
		snap.Error = "无可用凭证"
		return s.failGrowthSnapshot(uid, snap, "无可用凭证")
	}
	snap.ObservedAt = time.Now()
	snap.Nickname = a.Nickname

	tasks, err := s.cfg.Upstream.GrowthTasks(a)
	if err != nil {
		return s.failGrowthSnapshot(uid, snap, "任务列表: "+err.Error())
	}
	snap.TasksTotal = len(tasks)
	snap.Tasks = make([]GrowthTaskView, 0, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		switch t.AcceptStatus {
		case upstream.GrowthStatusCompleted:
			snap.TasksCompleted++
		case upstream.GrowthStatusAccepted, upstream.GrowthStatusInProgress:
			// in_progress 也是「已接单、进度未达标」，和 accepted 归一类。
			// 漏掉它会让这类任务在界面上不属于任何视图。
			snap.TasksAccepted++
		}
		v := GrowthTaskView{
			TaskCode: t.TaskCode, Title: t.Title,
			Description: t.TaskDesc, HowTo: t.Description,
			Status: t.AcceptStatus, RewardCredit: t.RewardCredit,
			RewardEnergy: t.RewardEnergy, Tag: t.Tag, Locked: t.Locked,
			Claimable: t.Claimable(),
		}
		if t.Progress != nil {
			v.Current, v.Target = t.Progress.Current, t.Progress.Target
		}
		snap.Tasks = append(snap.Tasks, v)

		// 可领奖：条件已达成、奖励还没拿。这是「现在能拿到的分」。
		if t.Claimable() {
			snap.ClaimableCount++
			snap.ClaimableCredit += t.RewardCredit
			snap.ClaimableEnergy += t.RewardEnergy
		}
		if !t.Acceptable() {
			continue
		}
		snap.AcceptableCount++
		snap.AcceptableCredit += t.RewardCredit
		snap.AcceptableEnergy += t.RewardEnergy
	}

	if st, err := s.cfg.Upstream.GrowthStreak(a); err == nil && st != nil {
		snap.StreakDays = st.Streak.Days
		snap.NextTier = st.Streak.NextTier
		snap.NextTierRemaining = st.Streak.NextTierRemaining
		snap.MakeupCards = st.MakeupCards.Balance
		snap.MakeupDates = st.Streak.MakeupDates
		snap.RemainingDays = st.RedemptionStatus.RemainingDays
	} else if err != nil {
		snap.Error = trunc("连登: " + err.Error())
	}

	if e, err := s.cfg.Upstream.GrowthEnergy(a); err == nil && e != nil {
		snap.Energy = e.Balance
	}
	if q, err := s.cfg.Upstream.GrowthBuddyQuota(a); err == nil && q != nil {
		snap.BlindBoxAffordable = q.Affordable
		snap.BlindBoxCost = q.CostPerOpen
	}
	if l, err := s.cfg.Upstream.GrowthLotteryChances(a); err == nil && l != nil {
		snap.LotteryChances = l.Balance
	}

	snap.Stale = false
	s.storeGrowthSnapshot(uid, snap)
	return &snap
}

// failGrowthSnapshot 探测失败时的收敛：**保留上一次成功的数据**，只标注错误与过期。
//
// 为什么不能直接覆盖成一份只有 error 的快照：上游抖一下就会把整个面板抹白，
// 而守卫在 due 窗口（默认 10 分钟）内不会重试，用户会长时间盯着空表，
// 且完全无从判断是「本来就没有」还是「这次没查到」。
func (s *Scheduler) failGrowthSnapshot(uid string, fresh GrowthSnapshot, msg string) *GrowthSnapshot {
	msg = trunc(msg)
	if prev, ok := s.growthSnapshot(uid); ok && prev.TasksTotal > 0 {
		prev.Error = msg
		prev.Stale = true
		// ObservedAt 保持为「数据实际观测时刻」，前端据此显示数据有多旧。
		s.storeGrowthSnapshot(uid, prev)
		return &prev
	}
	fresh.Error = msg
	fresh.Stale = false
	s.storeGrowthSnapshot(uid, fresh)
	return &fresh
}

// growthSnapshot 读取单个账号的缓存快照。
func (s *Scheduler) growthSnapshot(uid string) (GrowthSnapshot, bool) {
	if s.growth == nil {
		return GrowthSnapshot{}, false
	}
	s.growth.mu.Lock()
	defer s.growth.mu.Unlock()
	v, ok := s.growth.snapshots[uid]
	return v, ok
}

func trunc(s string) string {
	if len(s) > growthErrMax {
		return s[:growthErrMax]
	}
	return s
}

// growthDue / scheduleGrowthNext 与旅行守卫同构：决定何时再查这个账号。
func (s *Scheduler) growthDue(uid string, now time.Time) bool {
	if s.growth == nil {
		return true
	}
	s.growth.mu.Lock()
	defer s.growth.mu.Unlock()
	at, ok := s.growth.due[uid]
	return !ok || !now.Before(at)
}

func (s *Scheduler) scheduleGrowthNext(uid string) {
	if s.growth == nil {
		return
	}
	s.growth.mu.Lock()
	s.growth.due[uid] = time.Now().Add(s.GrowthWatchInterval())
	s.growth.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 单账号动作
// ---------------------------------------------------------------------------

// GrowthClaimFor 领奖：把**条件已达成但奖励未领**的任务奖励领回来
// （taskCode 非空时只领指定任务，为空则领该账号全部可领任务）。
//
// 这是唯一真正让信用分到账的动作。上游把「达成」与「发奖」拆成两步：
// completed = 已达成待领取，claim 成功后状态变 claimed。已领过的任务返回
// already_claimed=true（幂等），此时 Credits 记 0，不算收益也不算失败。
func (s *Scheduler) GrowthClaimFor(uid, taskCode, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "claim"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	tasks, err := s.cfg.Upstream.GrowthTasks(a)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "拉取任务失败: "+err.Error()
		s.recordGrowth(uid, res, trigger)
		return res
	}

	var codes []string
	for _, t := range tasks {
		if taskCode != "" {
			if t.TaskCode == taskCode {
				codes = append(codes, t.TaskCode)
			}
			continue
		}
		if t.Claimable() {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		res.Status, res.Detail = checkinlog.StatusSkip, "没有待领取的任务"
		if taskCode != "" {
			res.Detail = "任务「" + taskCode + "」当前不可领取"
		}
		s.recordGrowth(uid, res, trigger)
		return res
	}

	// 逐个领：上游只有单任务领奖接口，没有批量版。
	var credit, energy int64
	okN, failN, alreadyN := 0, 0, 0
	var lastErr string
	for _, code := range codes {
		r, err := s.cfg.Upstream.GrowthClaim(a, code)
		if err != nil {
			failN++
			lastErr = code + ": " + shortErr(err)
			continue
		}
		if r.AlreadyClaimed {
			alreadyN++
			continue
		}
		okN++
		credit += r.Credit
		energy += r.Energy
	}

	res.Credits, res.Energy = credit, energy
	res.Count = okN
	res.AlreadyClaimed = alreadyN > 0 && okN == 0

	switch {
	case okN > 0:
		res.Status = checkinlog.StatusOK
		res.Detail = fmt.Sprintf("领取 %d 个任务奖励，到账 %d 信用分", okN, credit)
		if energy > 0 {
			res.Detail += fmt.Sprintf(" +%d 能量", energy)
		}
		if alreadyN > 0 {
			res.Detail += fmt.Sprintf("（%d 个此前已领）", alreadyN)
		}
		if failN > 0 {
			res.Detail += fmt.Sprintf("；失败 %d 个", failN)
		}
	case failN > 0:
		res.Status, res.Detail = checkinlog.StatusFail, "领取失败: "+lastErr
	case alreadyN > 0:
		// 全部都是早已领过的：不是失败，但也没什么可报的，记为跳过更贴切。
		res.Status = checkinlog.StatusSkip
		res.Detail = "奖励此前已领取"
	}
	s.recordGrowth(uid, res, trigger)
	return res
}

// GrowthAcceptFor 接单：把未接单的任务接进账号（taskCode 非空时只接指定任务）。
//
// 语义提醒（实测）：接单**不发奖励**，只让任务开始计进度。
// 因此返回值里的 Credits/Energy 只是「这些任务完成后可得」，不代表本次到账，
// 记账时不要把它算成收益。真正到账的是 GrowthClaimFor。
func (s *Scheduler) GrowthAcceptFor(uid, taskCode, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "accept"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	tasks, err := s.cfg.Upstream.GrowthTasks(a)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "拉取任务失败: "+err.Error()
		s.recordGrowth(uid, res, trigger)
		return res
	}

	var codes []string
	var potentialCredit, potentialEnergy int64
	for _, t := range tasks {
		if taskCode != "" {
			if t.TaskCode == taskCode {
				codes = append(codes, t.TaskCode)
			}
			continue
		}
		if t.Acceptable() {
			codes = append(codes, t.TaskCode)
			potentialCredit += t.RewardCredit
			potentialEnergy += t.RewardEnergy
		}
	}
	if len(codes) == 0 {
		res.Status, res.Detail = checkinlog.StatusSkip, "没有待接单的任务"
		if taskCode != "" {
			res.Detail = "任务「" + taskCode + "」当前不可接单"
		}
		s.recordGrowth(uid, res, trigger)
		return res
	}

	results, err := s.cfg.Upstream.GrowthAcceptTasks(a, codes)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "接单请求失败: "+err.Error()
		s.recordGrowth(uid, res, trigger)
		return res
	}
	okN, skipN, failN := 0, 0, 0
	var lastErr, blockedBy string
	for _, r := range results {
		if r.Status == "accepted" {
			okN++
			continue
		}
		// 上游的拒绝分两类，必须区分开 —— 混在一起会把"只差一个前置任务"
		// 报成"接单失败"，用户完全看不出该做什么。
		if reason, skipped := acceptRejectionReason(r.Message); skipped {
			skipN++
			if reason != "" && blockedBy == "" {
				blockedBy = reason
			}
			continue
		}
		failN++
		lastErr = r.TaskCode + ": " + r.Message
	}
	res.Count = okN
	res.Credits = potentialCredit
	res.Energy = potentialEnergy

	// 文案按实际发生的组合拼，不出现"跳过 N（失败 0）"这种噪音。
	var parts []string
	parts = append(parts, fmt.Sprintf("接单 %d 个", okN))
	if skipN > 0 {
		parts = append(parts, fmt.Sprintf("跳过 %d", skipN))
	}
	if failN > 0 {
		parts = append(parts, fmt.Sprintf("失败 %d", failN))
	}
	res.Detail = strings.Join(parts, "（") + strings.Repeat("）", len(parts)-1)
	if blockedBy != "" {
		res.Detail += "；被前置任务 " + blockedBy + " 挡住"
	}
	if lastErr != "" {
		res.Detail += "；最后错误 " + trunc(lastErr)
	}
	// 判定：
	//   有真失败 → fail（哪怕也接过一些，避免"全成功"错觉）
	//   没真失败、接过一些 → ok
	//   没真失败、一个没接但确有跳过 → skip（这是"被前置挡住"，不是故障）
	//   什么都没发生（上游返回空 results）→ skip
	switch {
	case failN > 0:
		res.Status = checkinlog.StatusFail
	case okN > 0:
		res.Status = checkinlog.StatusOK
	default:
		res.Status = checkinlog.StatusSkip
		if res.Detail == "" || okN == 0 && skipN == 0 {
			res.Detail = "没有可接单的任务"
		}
	}
	s.recordGrowth(uid, res, trigger)
	s.scheduleGrowthNext(uid)
	return res
}

// acceptRejectionReason 判定上游的接单拒绝是否属于**预期内**（应记为跳过而非失败）。
//
// 返回 (前置任务名, 是否跳过)。前置任务名仅对 prerequisite 类拒绝非空。
//
// 三种预期拒绝（均由线上实测归纳，见对应测试）：
//
//	prerequisite not met: first_buddy
//	    前置任务未完成。典型场景：成长中心要求先完成 first_buddy（领取一只 Buddy），
//	    其余任务才允许接单。这不是故障，用户去把前置任务做掉即可。
//	    注意 first_buddy 自身会回 "task does not require acceptance" —— 它由
//	    上游自动派生，不需要也不能手动接单。
//	task does not require acceptance
//	    该任务本来就不需要接单（上游已自动纳入，或属于活动类任务）。
//	message 为空
//	    任务已经是 accepted 状态，重复接单。上游对重复接单返回空 message 的 error，
//	    语义上等价于"无操作成功"，不该报错。
func acceptRejectionReason(msg string) (string, bool) {
	m := strings.TrimSpace(msg)
	if m == "" {
		return "", true
	}
	lower := strings.ToLower(m)
	if idx := strings.Index(lower, "prerequisite not met:"); idx >= 0 {
		code := strings.TrimSpace(m[idx+len("prerequisite not met:"):])
		return code, true
	}
	if strings.Contains(lower, "does not require acceptance") {
		return "", true
	}
	return "", false
}

// GrowthMakeupFor 补签。date 为空时取上游给出的可补签日期列表里的第一个。
func (s *Scheduler) GrowthMakeupFor(uid, date, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "makeup"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	if date == "" {
		st, err := s.cfg.Upstream.GrowthStreak(a)
		if err != nil {
			res.Status, res.Detail = checkinlog.StatusFail, "查询连登失败: "+err.Error()
			s.recordGrowth(uid, res, trigger)
			return res
		}
		if st.MakeupCards.Balance <= 0 {
			res.Status, res.Detail = checkinlog.StatusSkip, "没有补签卡"
			s.recordGrowth(uid, res, trigger)
			return res
		}
		if len(st.Streak.MakeupDates) == 0 {
			res.Status, res.Detail = checkinlog.StatusSkip, "没有可补签的日期"
			s.recordGrowth(uid, res, trigger)
			return res
		}
		date = st.Streak.MakeupDates[0]
	}
	if err := s.cfg.Upstream.GrowthMakeup(a, date); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "补签 "+date+" 失败: "+err.Error()
		s.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, "已补签 "+date, 1
	s.recordGrowth(uid, res, trigger)
	s.scheduleGrowthNext(uid)
	return res
}

// GrowthRedeemFor 连登兑换。tier 为空时自动挑「剩余天数够、且未领过」的最高档。
func (s *Scheduler) GrowthRedeemFor(uid, tier, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "redeem"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	st, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil || st == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "查询连登失败"
		if err != nil {
			res.Detail = "查询连登失败: " + err.Error()
		}
		s.recordGrowth(uid, res, trigger)
		return res
	}
	remaining := st.RedemptionStatus.RemainingDays
	if tier == "" {
		// 档位按天数从高到低挑：高档次单位奖励更好，优先花掉。
		tiers := append([]upstream.RedeemTier(nil), st.RedemptionStatus.Tiers...)
		sort.Slice(tiers, func(i, j int) bool { return tiers[i].Days > tiers[j].Days })
		for _, t := range tiers {
			if t.Days > 0 && remaining >= t.Days && !redeemTierClaimed(st, t.Tier) {
				tier = t.Tier
				break
			}
		}
		if tier == "" {
			res.Status, res.Detail = checkinlog.StatusSkip,
				fmt.Sprintf("可兑换天数 %d 天，不足以兑换任何档位", remaining)
			s.recordGrowth(uid, res, trigger)
			return res
		}
	}
	if err := s.cfg.Upstream.GrowthRedeem(a, tier); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "兑换 "+tier+" 失败: "+err.Error()
		s.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, "连登兑换 "+tier, 1
	for _, t := range st.RedemptionStatus.Tiers {
		if t.Tier == tier {
			res.Credits, res.Energy = t.Credit, t.Energy
			break
		}
	}
	s.recordGrowth(uid, res, trigger)
	s.scheduleGrowthNext(uid)
	return res
}

// redeemTierClaimed 报告某档位本期是否已兑换过。
// 实测有 tier_<n>d_status（locked/…）与 tier_<n>d_count 两套口径，两者取「已领」的并集，
// 任一显示已领就跳过——重复兑换只会被服务端拒绝并制造一条假失败记录。
func redeemTierClaimed(st *upstream.StreakState, tier string) bool {
	r := st.RedemptionStatus
	switch tier {
	case "7d":
		return r.Tier7dCount > 0 || r.Tier7dStatus == "claimed"
	case "14d":
		return r.Tier14dCount > 0 || r.Tier14dStatus == "claimed"
	case "28d":
		return r.Tier28dCount > 0 || r.Tier28dStatus == "claimed"
	}
	return false
}

// GrowthOpenFor 开盲盒 count 次。count<=0 时按 quota.affordable 与单次上限取小值。
func (s *Scheduler) GrowthOpenFor(uid string, count int, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "open"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	q, err := s.cfg.Upstream.GrowthBuddyQuota(a)
	if err != nil || q == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "查询盲盒额度失败"
		s.recordGrowth(uid, res, trigger)
		return res
	}
	if count <= 0 {
		count = q.Affordable
		if q.MaxOpenCount > 0 && count > q.MaxOpenCount {
			count = q.MaxOpenCount
		}
	}
	if count <= 0 {
		res.Status, res.Detail = checkinlog.StatusSkip,
			fmt.Sprintf("能量不足（余额 %d，单次消耗 %d）", q.Balance, q.CostPerOpen)
		s.recordGrowth(uid, res, trigger)
		return res
	}
	if err := s.cfg.Upstream.GrowthOpenBuddy(a, count); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, fmt.Sprintf("开盲盒 %d 次失败: %v", count, err)
		s.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, fmt.Sprintf("开盲盒 %d 次", count), count
	res.Energy = -int64(count) * q.CostPerOpen
	s.recordGrowth(uid, res, trigger)
	s.scheduleGrowthNext(uid)
	return res
}

// GrowthDrawFor 抽奖一次。
func (s *Scheduler) GrowthDrawFor(uid, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "draw"}
	a := s.travelFirstCreds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	l, err := s.cfg.Upstream.GrowthLotteryChances(a)
	if err != nil || l == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "查询抽奖次数失败"
		s.recordGrowth(uid, res, trigger)
		return res
	}
	if l.Balance <= 0 {
		res.Status, res.Detail = checkinlog.StatusSkip, "没有抽奖次数"
		s.recordGrowth(uid, res, trigger)
		return res
	}
	if err := s.cfg.Upstream.GrowthLotteryDraw(a); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "抽奖失败: "+err.Error()
		s.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, "抽奖 1 次", 1
	s.recordGrowth(uid, res, trigger)
	s.scheduleGrowthNext(uid)
	return res
}

func (s *Scheduler) recordGrowth(uid string, res GrowthActionResult, trigger string) {
	if s.cfg.Log == nil {
		return
	}
	s.record(uid, checkinlog.KindGrowth, res.Status,
		fmt.Sprintf("[%s] %s", res.Action, res.Detail), res.Credits, trigger)
}

// ---------------------------------------------------------------------------
// 全量扫描 / 守卫轮
// ---------------------------------------------------------------------------

// RefreshGrowth 扫描账号并刷新快照，按开关执行自动动作。
// force=false 时只查「到期」的账号。auto 为 nil 表示只探测不动作。
func (s *Scheduler) RefreshGrowth(force bool, autoActions bool) []GrowthSnapshot {
	now := time.Now()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if !force && !s.growthDue(st.UID, now) {
			continue
		}
		snap := s.probeGrowth(st.UID)
		if autoActions {
			s.runGrowthAutoActions(snap)
		}
		s.scheduleGrowthNext(st.UID)
	}
	return s.GrowthSnapshots()
}

// runGrowthAutoActions 依据快照与开关执行自动动作。
// 顺序有意为之：先接单/补签（纯状态与收益）→ 再开销类动作，
// 这样同一轮里刚领到的能量能立刻用于开盲盒，不必等下一轮。
func (s *Scheduler) runGrowthAutoActions(snap *GrowthSnapshot) {
	if snap == nil || snap.Error != "" {
		return
	}
	accept, makeup, redeem, open, draw, claim := s.GrowthToggles()
	// 先领奖再去忙别的：这是唯一真正让分数到账的动作，
	// 而且领完会刷新快照，后面的判断用的是最新状态。
	if claim && snap.ClaimableCount > 0 {
		res := s.GrowthClaimFor(snap.UID, "", triggerAuto)
		if res.Status == checkinlog.StatusOK {
			// 领奖改变了账号的信用分，池里缓存的是旧值，刷新一下，
			// 否则界面上的积分要等下一次定时刷新才对得上。
			s.RefreshCredits(snap.UID, triggerAuto)
			s.probeGrowth(snap.UID)
		}
	}
	if accept && snap.AcceptableCount > 0 {
		s.GrowthAcceptFor(snap.UID, "", triggerAuto)
		s.probeGrowth(snap.UID) // 接完刷新快照，供后续动作判断
	}
	if makeup && snap.MakeupCards > 0 && len(snap.MakeupDates) > 0 {
		s.GrowthMakeupFor(snap.UID, "", triggerAuto)
	}
	if redeem {
		s.GrowthRedeemFor(snap.UID, "", triggerAuto)
	}
	if open && snap.BlindBoxAffordable > 0 {
		s.GrowthOpenFor(snap.UID, 0, triggerAuto)
	}
	if draw && snap.LotteryChances > 0 {
		s.GrowthDrawFor(snap.UID, triggerAuto)
	}
}

// RunGrowthWatcher 常驻守卫：启动即全量扫一次填满缓存，之后按间隔复查。
func (s *Scheduler) RunGrowthWatcher(ctx context.Context, interval time.Duration) {
	if s.growth == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	s.RefreshGrowth(true, true)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshGrowth(false, true)
		}
	}
}
