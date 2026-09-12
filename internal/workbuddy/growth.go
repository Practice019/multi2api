// growth.go 成长中心守卫：状态快照缓存 + 自动领取/兑换/补签/开盲盒。
//
// 与 travel.go 同构（快照缓存 + 到期门控 + 单一扫描入口），但成长中心的
// 动作比旅行多且**会消耗资源**，因此这里把每个动作拆成独立开关：
//
//	默认开启（纯收益，无机会成本）：领任务奖励、补签
//	默认关闭（要花资源，需用户明确同意）：连登兑换、开盲盒、抽奖
//
// 之所以把「会花资源的」默认关掉：自动开盲盒会消耗能量、自动兑换会消耗连登天数，
// 这些是用户的资产，不该由一次「保存设置」之外的默认值替他决定。
//
// # 搬运说明（Task 3b）
//
// 本文件原先在 internal/scheduler/growthwatch.go。成长中心是 CodeBuddy 专属业务
// （codearts 没有任务体系），放在核心调度器里等于让核心理解某个上游的领域概念。
// 现在它作为 workbuddy 包自己的状态，通过 gateway.JobExt 向核心注册守卫轮。
package workbuddy

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

// GrowthErrMax 快照里的错误原因上限，避免把上游长文本灌进快照。
const GrowthErrMax = 160

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

	// ExpiresAt / ExpiresInDays / Expired / ExpiringSoon 任务有效期。
	//
	// 上游只有部分任务带期限（实测 18 个里 4 个），其余为空 ——
	// 所以这些字段全部 omitempty，没有期限的任务不会在 JSON 里多出噪音。
	//
	// 为什么把「还剩几天」算在服务端：界面只需显示，不该各自做日期减法 ——
	// 时区、跨天舍入、负值处理每处都重写一遍必然漂移。
	ExpiresAt string `json:"expires_at,omitempty"`
	// ExpiresInDays 距到期的整天数；已过期为负数（界面据此显示"已过期 N 天"）。
	ExpiresInDays int `json:"expires_in_days,omitempty"`
	// Expired 已过期。上游仍会返回这些任务，但做完也拿不到奖励。
	Expired bool `json:"expired,omitempty"`
	// ExpiringSoon 即将到期（阈值见 expiringSoonDays），供界面高亮提醒。
	ExpiringSoon bool `json:"expiring_soon,omitempty"`
}

// expiringSoonDays 是「即将到期」的阈值（天）。
//
// 取 7 天：既早于多数人会主动查看的周期，又不会让"还有 60 天"的任务也来抢注意力 ——
// 实测四个带期限的任务剩余 18~62 天，7 天能准确圈出真正紧迫的那一个。
const expiringSoonDays = 7

// daysUntil 返回从 now 到 end 的整天数（向上取整到"还剩几天可用"的直觉口径）。
//
// 口径说明：采用**自然日**而非 24 小时块。用户看到「9-30 23:59 到期」时，
// 9-30 当天是可以用的一天，所以按日期差算更符合直觉；
// 用 24 小时块会出现"还剩 0 天但今天明明还能做"的矛盾。
func daysUntil(end, now time.Time) int {
	// 归一到各自时区的当天零点再做差，避开跨时区与夏令时的偏差。
	ey, em, ed := end.Date()
	ny, nm, nd := now.Date()
	e := time.Date(ey, em, ed, 0, 0, 0, 0, time.UTC)
	n := time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC)
	return int(e.Sub(n).Hours() / 24)
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

	// PendingCount/…… 指「还没做完」的任务数，即 not_accepted + accepted + in_progress。
	//
	// 为什么需要它：AcceptableCount 只数 not_accepted（未接单），
	// 于是已接单/进行中的任务在「待接单」与「待领取」两列里都不出现 ——
	// 用户看到「待完成 0」，实际还有 5 个任务要去做。
	// 界面上的「待完成 / 完成后可得」用这一对，语义是「还有多少任务没做完、做完能拿多少分」。
	//
	// 与另两组的关系（互斥且覆盖全部任务）：
	//   Pending*   还没做成的（含未接单、已接单、进行中）
	//   Claimable* 已做成、只等领奖（completed）
	//   claimed    已领完，三组都不含
	PendingCount  int   `json:"pending_count"`
	PendingCredit int64 `json:"pending_credit"`
	PendingEnergy int64 `json:"pending_energy"`

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
	// Credits 是本次**实际到账**的积分。领奖时用上游的 already_claimed 区分：
	// 早已领过的任务返回 0，不计入。
	Credits int64 `json:"credits,omitempty"`
	Energy  int64 `json:"energy,omitempty"`
	Count   int   `json:"count,omitempty"`
	// AlreadyClaimed 为真表示这次调用是幂等空转（奖励之前就领过了）。
	AlreadyClaimed bool `json:"already_claimed,omitempty"`
}

// growthWatchState 成长守卫的可变状态。
type growthWatchState struct {
	mu        sync.Mutex
	snapshots map[string]GrowthSnapshot
	due       map[string]time.Time

	// 六个独立开关。
	// autoClaim 默认开：completed 只代表条件达成，不 claim 就永远拿不到分。
	// autoAccept 默认开：not_accepted 只在「还没领到第一只 Buddy」的窄窗口出现，
	// 官方前端连接单按钮都不给；默认开才能让那批任务自动进入可追踪状态。
	// 后面三个会花资源（连登天数/能量/抽奖次数），默认关。
	autoClaim  bool // 自动领奖
	autoAccept bool // 自动接单（默认开，纯登记、不消耗资源）
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
func (p *Provider) GrowthToggles() (accept, makeup, redeem, open, draw, claim bool) {
	if p.growth == nil {
		return
	}
	p.growth.mu.Lock()
	defer p.growth.mu.Unlock()
	return p.growth.autoAccept, p.growth.autoMakeup, p.growth.autoRedeem,
		p.growth.autoOpen, p.growth.autoDraw, p.growth.autoClaim
}

// SetGrowthToggles 运行时设置六个自动动作开关。
func (p *Provider) SetGrowthToggles(accept, makeup, redeem, open, draw, claim bool) {
	if p.growth == nil {
		return
	}
	p.growth.mu.Lock()
	p.growth.autoAccept, p.growth.autoMakeup = accept, makeup
	p.growth.autoRedeem, p.growth.autoOpen, p.growth.autoDraw = redeem, open, draw
	p.growth.autoClaim = claim
	p.growth.mu.Unlock()
	log.Printf("scheduler: 成长中心自动动作 -> 领奖=%v 接单=%v 补签=%v 兑换=%v 开盲盒=%v 抽奖=%v",
		claim, accept, makeup, redeem, open, draw)
}

// GrowthWatchInterval 返回扫描间隔（供管理台展示）。
func (p *Provider) GrowthWatchInterval() time.Duration {
	if p.cfg.GrowthWatchInterval > 0 {
		return p.cfg.GrowthWatchInterval
	}
	return 10 * time.Minute
}

// GrowthSnapshots 返回缓存快照（按 uid 排序），不发上游请求。
//
// 与 TravelSnapshots 同理：快照 map 只写不删，而探测可由任意 uid 经
// 单账号端点进入，所以必须在**出口**按归属过滤，否则一条脏快照永久可见。
func (p *Provider) GrowthSnapshots() []GrowthSnapshot {
	if p.growth == nil {
		return nil
	}
	own := p.ownUIDs()
	p.growth.mu.Lock()
	defer p.growth.mu.Unlock()
	out := make([]GrowthSnapshot, 0, len(p.growth.snapshots))
	for _, v := range p.growth.snapshots {
		if own != nil && !own[v.UID] {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (p *Provider) storeGrowthSnapshot(uid string, snap GrowthSnapshot) {
	if p.growth == nil {
		return
	}
	p.growth.mu.Lock()
	p.growth.snapshots[uid] = snap
	p.growth.mu.Unlock()
}

// probeGrowth 拉一次成长中心全量状态（5 个 GET），组装快照。
// 任一子项失败只记录 error，不阻断其它子项——部分数据也比整行空白有用。
func (p *Provider) probeGrowth(uid string) *GrowthSnapshot {
	snap := GrowthSnapshot{UID: uid}
	a := p.creds(uid)
	if a == nil {
		snap.Error = "无可用凭证"
		return p.failGrowthSnapshot(uid, snap, "无可用凭证")
	}
	snap.ObservedAt = time.Now()
	snap.Nickname = a.Nickname

	// 每次上游调用都经过共享信号量 —— 这样「账号并发 × 账号内并发」的总量
	// 始终被 GrowthProbeConcurrency 夹住，不会因为层层相乘冲破风控上限。
	//
	// 用闭包 + defer 而不是裸 acquire/release：后者在 GrowthTasks panic 时
	// 会漏掉 release，令牌永久丢失，最终把**所有**刷新卡死在 acquire 上。
	// 同一文件下面 4 个并发调用用的是 defer，这里保持一致 ——
	// 同一个资源不能有两套释放纪律。
	var tasks []upstream.GrowthTask
	var err error
	func() {
		p.probeSem.acquire()
		defer p.probeSem.release()
		tasks, err = p.client.GrowthTasks(a)
	}()
	if err != nil {
		return p.failGrowthSnapshot(uid, snap, "任务列表: "+err.Error())
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
		// 有效期：只在**未了结**的任务上带出去。
		//
		// 为什么过滤掉已领取：claimed 的任务再显示"还剩 N 天到期"是噪音 ——
		// 期限的意义是"再不做过期就没了"，奖励已到手的任务没有这个压力。
		// completed（条件达成、待领奖）**仍然显示**：奖励还没到手，期限依然相关。
		if t.ValidEnd != nil && t.AcceptStatus != upstream.GrowthStatusClaimed {
			end := *t.ValidEnd
			v.ExpiresAt = end.UTC().Format(time.RFC3339)
			v.ExpiresInDays = daysUntil(end, time.Now())
			v.Expired = v.ExpiresInDays < 0
			// 已过期且未领取 = 这个任务已经作废，界面该说清楚而不是让人去点。
			v.ExpiringSoon = !v.Expired && v.ExpiresInDays <= expiringSoonDays
		}
		snap.Tasks = append(snap.Tasks, v)

		// 可领奖：条件已达成、奖励还没拿。这是「现在能拿到的分」。
		if t.Claimable() {
			snap.ClaimableCount++
			snap.ClaimableCredit += t.RewardCredit
			snap.ClaimableEnergy += t.RewardEnergy
		}
		// 待完成：还没做成的（未接单 / 已接单 / 进行中）。
		// 与 Claimable 互斥：completed 归可领，不在这里重复计数。
		if t.Pending() {
			snap.PendingCount++
			snap.PendingCredit += t.RewardCredit
			snap.PendingEnergy += t.RewardEnergy
		}
		if !t.Acceptable() {
			continue
		}
		snap.AcceptableCount++
		snap.AcceptableCredit += t.RewardCredit
		snap.AcceptableEnergy += t.RewardEnergy
	}

	// 剩余 4 个调用并发发起。
	//
	// 为什么：实测单账号探针 2402ms，其中 /tasks 一个就占 1590ms（66%）。
	// 这 4 个加起来约 800ms，串行发等于白等。它们**互不依赖**（各返回一个
	// 独立字段），所以可以并发 —— 单账号耗时降到接近 /tasks 的 1590ms。
	//
	// 每个 goroutine 只写自己的局部变量，最后统一合并进 snap，
	// 因此不需要锁（snap 在本函数内独占，不与其它 goroutine 共享）。
	var (
		st    *upstream.StreakState
		stErr error
		e     *upstream.GrowthEnergy
		q     *upstream.GrowthBuddyQuota
		l     *upstream.GrowthLottery
	)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		p.probeSem.acquire()
		defer p.probeSem.release()
		st, stErr = p.client.GrowthStreak(a)
	}()
	go func() {
		defer wg.Done()
		p.probeSem.acquire()
		defer p.probeSem.release()
		e, _ = p.client.GrowthEnergy(a)
	}()
	go func() {
		defer wg.Done()
		p.probeSem.acquire()
		defer p.probeSem.release()
		q, _ = p.client.GrowthBuddyQuota(a)
	}()
	go func() {
		defer wg.Done()
		p.probeSem.acquire()
		defer p.probeSem.release()
		l, _ = p.client.GrowthLotteryChances(a)
	}()
	wg.Wait()

	// 合并（顺序无关，各写各的字段）。
	// 单个失败只影响它那一个字段 —— 不能让 /energy 挂掉就把整份快照抹掉。
	if stErr == nil && st != nil {
		snap.StreakDays = st.Streak.Days
		snap.NextTier = st.Streak.NextTier
		snap.NextTierRemaining = st.Streak.NextTierRemaining
		snap.MakeupCards = st.MakeupCards.Balance
		snap.MakeupDates = st.Streak.MakeupDates
		snap.RemainingDays = st.RedemptionStatus.RemainingDays
	} else if stErr != nil {
		snap.Error = trunc("连登: " + stErr.Error())
	}
	if e != nil {
		snap.Energy = e.Balance
	}
	if q != nil {
		snap.BlindBoxAffordable = q.Affordable
		snap.BlindBoxCost = q.CostPerOpen
	}
	if l != nil {
		snap.LotteryChances = l.Balance
	}

	snap.Stale = false
	p.storeGrowthSnapshot(uid, snap)
	return &snap
}

// failGrowthSnapshot 探测失败时的收敛：**保留上一次成功的数据**，只标注错误与过期。
//
// 为什么不能直接覆盖成一份只有 error 的快照：上游抖一下就会把整个面板抹白，
// 而守卫在 due 窗口（默认 10 分钟）内不会重试，用户会长时间盯着空表，
// 且完全无从判断是「本来就没有」还是「这次没查到」。
func (p *Provider) failGrowthSnapshot(uid string, fresh GrowthSnapshot, msg string) *GrowthSnapshot {
	msg = trunc(msg)
	if prev, ok := p.growthSnapshot(uid); ok && prev.TasksTotal > 0 {
		prev.Error = msg
		prev.Stale = true
		// ObservedAt 保持为「数据实际观测时刻」，前端据此显示数据有多旧。
		p.storeGrowthSnapshot(uid, prev)
		return &prev
	}
	fresh.Error = msg
	fresh.Stale = false
	p.storeGrowthSnapshot(uid, fresh)
	return &fresh
}

// growthSnapshot 读取单个账号的缓存快照。
func (p *Provider) growthSnapshot(uid string) (GrowthSnapshot, bool) {
	if p.growth == nil {
		return GrowthSnapshot{}, false
	}
	p.growth.mu.Lock()
	defer p.growth.mu.Unlock()
	v, ok := p.growth.snapshots[uid]
	return v, ok
}

func trunc(s string) string {
	if len(s) > GrowthErrMax {
		return s[:GrowthErrMax]
	}
	return s
}

// growthDue / scheduleGrowthNext 与旅行守卫同构：决定何时再查这个账号。
func (p *Provider) growthDue(uid string, now time.Time) bool {
	if p.growth == nil {
		return true
	}
	p.growth.mu.Lock()
	defer p.growth.mu.Unlock()
	at, ok := p.growth.due[uid]
	return !ok || !now.Before(at)
}

func (p *Provider) scheduleGrowthNext(uid string) {
	if p.growth == nil {
		return
	}
	p.growth.mu.Lock()
	p.growth.due[uid] = time.Now().Add(p.GrowthWatchInterval())
	p.growth.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 单账号动作
// ---------------------------------------------------------------------------

// GrowthClaimFor 领奖：把**条件已达成但奖励未领**的任务奖励领回来
// （taskCode 非空时只领指定任务，为空则领该账号全部可领任务）。
//
// 这是唯一真正让积分到账的动作。上游把「达成」与「发奖」拆成两步：
// completed = 已达成待领取，claim 成功后状态变 claimed。已领过的任务返回
// already_claimed=true（幂等），此时 Credits 记 0，不算收益也不算失败。
func (p *Provider) GrowthClaimFor(uid, taskCode, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "claim"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	tasks, err := p.client.GrowthTasks(a)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "拉取任务失败: "+err.Error()
		p.recordGrowth(uid, res, trigger)
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
		p.recordGrowth(uid, res, trigger)
		// 提前返回也要刷新：用户点了「领取」却没领到，往往正是因为快照旧了
		// （守卫轮刚替我们领过、或任务刚被别人领走）。刷一次能让界面立刻
		// 反映真实状态，而不是让用户对着"待领取"反复点。
		p.refreshOwnSnapshot(uid, res.Status)
		return res
	}

	// 逐个领：上游只有单任务领奖接口，没有批量版。
	var credit, energy int64
	okN, failN, alreadyN := 0, 0, 0
	var lastErr string
	for _, code := range codes {
		r, err := p.client.GrowthClaim(a, code)
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
		res.Detail = fmt.Sprintf("领取 %d 个任务奖励，到账 %d 积分", okN, credit)
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
	p.recordGrowth(uid, res, trigger)
	p.refreshOwnSnapshot(uid, res.Status)
	return res
}

// GrowthAcceptFor 接单：把未接单的任务接进账号（taskCode 非空时只接指定任务）。
//
// 语义提醒（实测）：接单**不发奖励**，只让任务开始计进度。
// 因此返回值里的 Credits/Energy 只是「这些任务完成后可得」，不代表本次到账，
// 记账时不要把它算成收益。真正到账的是 GrowthClaimFor。
func (p *Provider) GrowthAcceptFor(uid, taskCode, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "accept"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	tasks, err := p.client.GrowthTasks(a)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "拉取任务失败: "+err.Error()
		p.recordGrowth(uid, res, trigger)
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
		p.recordGrowth(uid, res, trigger)
		// 同 claim：快照可能已过期（守卫轮刚接过），刷一次让界面与现实一致。
		p.refreshOwnSnapshot(uid, res.Status)
		return res
	}

	results, err := p.client.GrowthAcceptTasks(a, codes)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "接单请求失败: "+err.Error()
		p.recordGrowth(uid, res, trigger)
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
		// 把「被谁挡住」翻译成「去做什么」——光报任务码 first_buddy 用户看不懂。
		res.Detail += "；需要先完成前置任务：" + prerequisiteHint(blockedBy)
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
	p.recordGrowth(uid, res, trigger)
	p.refreshOwnSnapshot(uid, res.Status)
	p.scheduleGrowthNext(uid)
	return res
}

// refreshOwnSnapshot 写操作成功后**就地刷新该账号**的快照。
//
// 为什么必须做：写操作只改上游状态，服务端的 GrowthSnapshot 仍是旧的。
// 前端为了让界面反映新状态，只能用 /admin/growth?refresh=1 **全量**重探 ——
// 为了更新 1 个账号把 N 个账号全探一遍（实测 3 账号 7.2s、1 账号 2.3s）。
// 就地刷新后前端读缓存即可（~1ms），这正是"点了要等 7 秒"的根治办法。
//
// 为什么只在非 fail 时刷：
//   - StatusSkip 表示"确实没做事"（没有可领/被前置挡住），但状态可能已被
//     上游改变（例如守卫轮刚替我们领过），所以仍值得刷一次保持一致；
//   - StatusFail 说明请求本身就没成功，重探只是白打一次上游，且会把
//     上一次的错误信息覆盖掉。
//
// 只探 uid 自己 —— 调用方传进来的就是目标账号，绝不顺带扫全量。
func (p *Provider) refreshOwnSnapshot(uid, status string) {
	if status == checkinlog.StatusFail {
		return
	}
	p.probeGrowth(uid)
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

// prerequisiteHints 把上游的前置任务码翻译成用户能照做的中文指引。
//
// 为什么需要翻译：上游只回 "prerequisite not met: first_buddy"，
// 用户看到的是个内部任务码，不知道要去客户端点哪里。
// first_buddy 尤其反直觉 —— 它在任务列表里显示为「领取一只 Buddy」，
// 但不能（也不需要）在网页上接单，必须去 WorkBuddy 客户端里真正领一只。
var prerequisiteHints = map[string]string{
	"first_buddy": "「领取一只 Buddy」—— 需在 WorkBuddy 客户端内完成（本网页无法代做），完成后其余任务才能接单",
}

// prerequisiteHint 返回可读指引。
//
// 输出形如 "first_buddy（「领取一只 Buddy」—— 需在 WorkBuddy 客户端内完成…）"：
// **任务码保留在前**，便于对着上游日志与任务列表里的 black-cat 样式 code 核对；
// 括号里是给人看的中文动作指引。未登记的码只回 code，至少不丢信息。
func prerequisiteHint(code string) string {
	if h, ok := prerequisiteHints[code]; ok {
		return code + "（" + h + "）"
	}
	return code
}

// GrowthMakeupFor 补签。date 为空时取上游给出的可补签日期列表里的第一个。
func (p *Provider) GrowthMakeupFor(uid, date, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "makeup"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	if date == "" {
		st, err := p.client.GrowthStreak(a)
		if err != nil {
			res.Status, res.Detail = checkinlog.StatusFail, "查询连登失败: "+err.Error()
			p.recordGrowth(uid, res, trigger)
			return res
		}
		if st.MakeupCards.Balance <= 0 {
			res.Status, res.Detail = checkinlog.StatusSkip, "没有补签卡"
			p.recordGrowth(uid, res, trigger)
			return res
		}
		if len(st.Streak.MakeupDates) == 0 {
			res.Status, res.Detail = checkinlog.StatusSkip, "没有可补签的日期"
			p.recordGrowth(uid, res, trigger)
			return res
		}
		date = st.Streak.MakeupDates[0]
	}
	if err := p.client.GrowthMakeup(a, date); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "补签 "+date+" 失败: "+err.Error()
		p.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, "已补签 "+date, 1
	p.recordGrowth(uid, res, trigger)
	p.refreshOwnSnapshot(uid, res.Status)
	p.scheduleGrowthNext(uid)
	return res
}

// GrowthRedeemFor 连登兑换。tier 为空时自动挑「剩余天数够、且未领过」的最高档。
func (p *Provider) GrowthRedeemFor(uid, tier, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "redeem"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	st, err := p.client.GrowthStreak(a)
	if err != nil || st == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "查询连登失败"
		if err != nil {
			res.Detail = "查询连登失败: " + err.Error()
		}
		p.recordGrowth(uid, res, trigger)
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
			p.recordGrowth(uid, res, trigger)
			return res
		}
	}
	if err := p.client.GrowthRedeem(a, tier); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "兑换 "+tier+" 失败: "+err.Error()
		p.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, "连登兑换 "+tier, 1
	for _, t := range st.RedemptionStatus.Tiers {
		if t.Tier == tier {
			res.Credits, res.Energy = t.Credit, t.Energy
			break
		}
	}
	p.recordGrowth(uid, res, trigger)
	p.refreshOwnSnapshot(uid, res.Status)
	p.scheduleGrowthNext(uid)
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
func (p *Provider) GrowthOpenFor(uid string, count int, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "open"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	q, err := p.client.GrowthBuddyQuota(a)
	if err != nil || q == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "查询盲盒额度失败"
		p.recordGrowth(uid, res, trigger)
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
		p.recordGrowth(uid, res, trigger)
		return res
	}
	if err := p.client.GrowthOpenBuddy(a, count); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, fmt.Sprintf("开盲盒 %d 次失败: %v", count, err)
		p.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, fmt.Sprintf("开盲盒 %d 次", count), count
	res.Energy = -int64(count) * q.CostPerOpen
	p.recordGrowth(uid, res, trigger)
	p.refreshOwnSnapshot(uid, res.Status)
	p.scheduleGrowthNext(uid)
	return res
}

// GrowthDrawFor 抽奖一次。
func (p *Provider) GrowthDrawFor(uid, trigger string) GrowthActionResult {
	res := GrowthActionResult{UID: uid, Action: "draw"}
	a := p.creds(uid)
	if a == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "账号不存在或无可用凭证"
		return res
	}
	l, err := p.client.GrowthLotteryChances(a)
	if err != nil || l == nil {
		res.Status, res.Detail = checkinlog.StatusFail, "查询抽奖次数失败"
		p.recordGrowth(uid, res, trigger)
		return res
	}
	if l.Balance <= 0 {
		res.Status, res.Detail = checkinlog.StatusSkip, "没有抽奖次数"
		p.recordGrowth(uid, res, trigger)
		return res
	}
	if err := p.client.GrowthLotteryDraw(a); err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, "抽奖失败: "+err.Error()
		p.recordGrowth(uid, res, trigger)
		return res
	}
	res.Status, res.Detail, res.Count = checkinlog.StatusOK, "抽奖 1 次", 1
	p.recordGrowth(uid, res, trigger)
	p.refreshOwnSnapshot(uid, res.Status)
	p.scheduleGrowthNext(uid)
	return res
}

func (p *Provider) recordGrowth(uid string, res GrowthActionResult, trigger string) {
	if p.cfg.Log == nil {
		return
	}
	p.record(uid, checkinlog.KindGrowth, res.Status,
		fmt.Sprintf("[%s] %s", res.Action, res.Detail), res.Credits, trigger)
}

// ---------------------------------------------------------------------------
// 全量扫描 / 守卫轮
// ---------------------------------------------------------------------------

// GrowthProbeConcurrency 是**对上游同时在途请求数**的总上限。
//
// ⚠ 这个数约束的是「同时在飞的 HTTP 请求」，不是「同时处理的账号」。
// 两层并发共享同一个信号量：
//
//	账号层（RefreshGrowth）：一批最多处理 N 个账号
//	调用层（probeGrowth）  ：每个账号内的 4 个独立 GET 并发
//
// 若两层各限各的，实际上游峰值 = 5 账号 × 4 调用 = 20 —— 那就等于没限流。
// 实测踩过：加进来后峰值从 5 涨到 20，被 TestRefreshGrowthConcurrencyCap
// 抓出来。所以两层必须共用同一预算。
//
// 为什么是 5：上游是同一个腾讯服务，账号多时同时打过去有触发风控的风险
// （旅行模块已有「账号间间隔 800ms 避免触发上游风控」的先例）。实测 3 账号
// 并发化后 7157ms → 2512ms 已经够用，再放宽只增风控面、对个位数账号无增益。
const GrowthProbeConcurrency = 5

// growthProbeSem 是跨两层共享的并发预算。
//
// 为什么做成字段而不是包级变量：多个 Provider 实例（测试里很常见）应当
// 各自独立，共用包级信号量会造成测试之间互相阻塞。
type growthProbeSem struct {
	ch chan struct{}
}

func newGrowthProbeSem(n int) *growthProbeSem {
	return &growthProbeSem{ch: make(chan struct{}, n)}
}

func (g *growthProbeSem) acquire() {
	if g == nil || g.ch == nil {
		return
	}
	g.ch <- struct{}{}
}

func (g *growthProbeSem) release() {
	if g == nil || g.ch == nil {
		return
	}
	<-g.ch
}

// RefreshGrowth 扫描账号并刷新快照，按开关执行自动动作。
// force=false 时只查「到期」的账号。autoActions 表示是否执行自动动作。
//
// 账号之间**并发**执行，但所有账号、所有子调用共享同一个并发预算
// （GrowthProbeConcurrency）—— 见该常量的注释。
//
// 为什么必须并发：实测 1 账号 2331ms / 3 账号 7157ms（比值 3.07，精确线性），
// 10 个账号就是 ~23 秒。串行 for 循环在账号数一多就会让「点刷新」变成
// 一次漫长的等待（前端此前 6.9 秒无任何反应，正是这个原因）。
//
// 并发安全性：probeGrowth 只通过 storeGrowthSnapshot（持锁）与
// runGrowthAutoActions 内部的状态访问器（均持锁）触碰共享状态，
// 不直接写 p.growth.* —— 已逐个确认。
func (p *Provider) RefreshGrowth(force bool, autoActions bool) []GrowthSnapshot {
	now := time.Now()

	// 先挑出本轮要处理的账号（纯读 + 只读 growthDue，不涉及网络）。
	// 只挑本上游的号：成长中心是 workbuddy 专属能力，别家上游的账号
	// 既没有对应接口，探测也只会产出一行错误快照污染面板。
	var uids []string
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		if !force && !p.growthDue(st.UID, now) {
			continue
		}
		uids = append(uids, st.UID)
	}
	if len(uids) == 0 {
		return p.GrowthSnapshots()
	}

	// 用共享信号量控制「对上游同时在途的请求数」，不是「同时处理的账号数」。
	//
	// 账号层只负责启动 goroutine；真正的限流发生在 probeGrowth 内部 ——
	// 每次上游调用前都 acquire 一次。这样两层共用一个预算，
	// 无论账号多少、每账号几个并发调用，总在途数都 <= GrowthProbeConcurrency。
	//
	// 为什么不在这里按账号数切片：那样只能限住账号数，限不住"每账号内部又开几个并发"
	// —— 实测过，两层各限各的会让峰值从 5 涨到 20。
	var wg sync.WaitGroup
	for _, uid := range uids {
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			snap := p.probeGrowth(uid)
			if autoActions {
				p.runGrowthAutoActions(snap)
			}
			p.scheduleGrowthNext(uid)
		}(uid)
	}
	wg.Wait()

	return p.GrowthSnapshots()
}

// runGrowthAutoActions 依据快照与开关执行自动动作。
// 顺序有意为之：先接单/补签（纯状态与收益）→ 再开销类动作，
// 这样同一轮里刚领到的能量能立刻用于开盲盒，不必等下一轮。
func (p *Provider) runGrowthAutoActions(snap *GrowthSnapshot) {
	if snap == nil || snap.Error != "" {
		return
	}
	accept, makeup, redeem, open, draw, claim := p.GrowthToggles()
	// 先领奖再去忙别的：这是唯一真正让分数到账的动作，
	// 而且领完会刷新快照，后面的判断用的是最新状态。
	if claim && snap.ClaimableCount > 0 {
		res := p.GrowthClaimFor(snap.UID, "", triggerAuto)
		if res.Status == checkinlog.StatusOK {
			// 领奖改变了账号的积分，池里缓存的是旧值，刷新一下，
			// 否则界面上的积分要等下一次定时刷新才对得上。
			p.RefreshCredits(snap.UID, triggerAuto)
			p.probeGrowth(snap.UID)
		}
	}
	if accept && snap.AcceptableCount > 0 {
		p.GrowthAcceptFor(snap.UID, "", triggerAuto)
		p.probeGrowth(snap.UID) // 接完刷新快照，供后续动作判断
	}
	if makeup && snap.MakeupCards > 0 && len(snap.MakeupDates) > 0 {
		p.GrowthMakeupFor(snap.UID, "", triggerAuto)
	}
	if redeem {
		p.GrowthRedeemFor(snap.UID, "", triggerAuto)
	}
	if open && snap.BlindBoxAffordable > 0 {
		p.GrowthOpenFor(snap.UID, 0, triggerAuto)
	}
	if draw && snap.LotteryChances > 0 {
		p.GrowthDrawFor(snap.UID, triggerAuto)
	}
}

// RunGrowthWatcher 常驻守卫：启动即全量扫一次填满缓存，之后按间隔复查。
//
// 注意：生产路径**不再**由 cmd/server 直接调用它 —— 它现在通过 Provider.Jobs()
// 注册给核心调度器，由调度器按 Due 判断错峰执行。保留这个方法是为了让
// 既有调用点（以及"立即跑一轮"的语义）零改动。
func (p *Provider) RunGrowthWatcher(ctx context.Context, interval time.Duration) {
	if p.growth == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	p.RefreshGrowth(true, true)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.RefreshGrowth(false, true)
		}
	}
}
