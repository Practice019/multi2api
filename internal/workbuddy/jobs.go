// jobs.go 把 workbuddy 的两个守卫轮注册给核心调度器（gateway.JobExt 实现）。
//
// # 这一步在整条解耦链里的位置
//
//	改造前：internal/scheduler 里写死 growthwatch.go / travelwatch.go，
//	        核心因此理解「成长中心」「猫猫旅行」这些 CodeBuddy 概念（354 处）
//	现在：  业务在本包，核心只通过 gateway.Job 拿到一个名字、一个间隔、
//	        一个 Run 函数，**不知道它在做什么**
//
// 加第二个上游时，核心的 scheduler 一行都不用改。
//
// # 为什么两个守卫轮都带 Due
//
// 它们都不是固定间隔任务：真正的门控是**每个账号自己的下次检查时刻**
// （旅行按 arrive_at、成长按上次探测时刻），不同账号的到期时刻不同。
// 用一个全局固定间隔表达不了这件事 —— 配短了会在途账号被反复打扰，
// 配长了该领的奖延迟才领。
//
// 所以由本包自己回答「现在有活干吗」（due），核心只按自己的轮询节奏问一句。
// Interval 同时充当下限：核心的 jobTickInterval 是 30 秒，
// 而旅行守卫轮的生产配置是 1 分钟、成长是 10 分钟 —— 用 Interval 把
// "最早可以跑"的粒度表达出来，避免 30 秒一轮把上游请求量抬高一倍。
package workbuddy

import (
	"context"
	"time"

	"workbuddy2api/internal/gateway"
)

// 任务名。用作调度器日志与状态展示的稳定标识。
const (
	// JobTravelWatch 猫猫旅行守卫：按 arrive_at 错峰检查，到站即自动领奖。
	JobTravelWatch = "workbuddy-travel-watch"
	// JobGrowthWatch 成长中心守卫：领任务奖励 / 补签 / 连登兑换 / 开盲盒 / 抽奖。
	JobGrowthWatch = "workbuddy-growth-watch"
	// JobActivity 对话活跃上报：整点窗口内每号每天 1 次。
	//
	// 它与上面两个守卫轮**形状不同**：那两者是"按每个账号自己的到期时刻错峰"，
	// 本任务是"每天在配置的整点窗口里跑一次全量"。因此它的 Due 判的是
	// 时刻窗口 + 当日是否已跑，而不是账号到期表。
	JobActivity = "workbuddy-activity"
)

// activityTickInterval 活跃上报任务的轮询粒度。
//
// 它只需要在整点窗口内被唤醒一次，因此用 5 分钟粒度足够 ——
// 核心的 jobTickInterval 是 30 秒，用 Interval 把它抬到 5 分钟，
// 避免一天里绝大多数轮次都是空转（Due 会立刻返回 false，但仍是唤醒）。
const activityTickInterval = 5 * time.Minute

// Jobs 返回本上游要注册的定时任务（gateway.JobExt）。
//
// # 与改造前的行为对齐
//
// 改造前 cmd/server 启动时：
//
//	go sch.RunTravelWatcher(ctx, cfg.TravelWatchInterval)
//	go sch.RunGrowthWatcher(ctx, cfg.GrowthWatchInterval)
//
// 两者都是「启动即全量扫一次填满缓存，之后每 interval 复查一次」。
// 现在等价地表达成两个 Job：
//
//	Run  = 一趟 Refresh（force=false，只查到期账号）
//	Due  = 有账号到期（首轮无快照视为到期 → 立即全量扫一次）
//
// 启动即全量扫这一条由 Due 的「无快照视为到期」保证 —— 调度器第一次轮询
// （Run 进入后立刻跑一轮 due）就会命中，效果与改造前的首扫一致。
//
// 注意 Refresh 的 force 参数：改造前循环里用 force=false（只查到期账号），
// 这里保持 force=false，**不**改成 true —— 改成 true 会让每一轮都全量回源，
// 把「不盲轮询」这个设计完全推翻（在途账号会被反复打扰）。
func (p *Provider) Jobs() []gateway.Job {
	return []gateway.Job{
		{
			Name:     JobTravelWatch,
			Interval: p.WatchInterval(),
			Run:      p.runTravelJob,
			Due:      p.travelDue,
		},
		{
			Name:     JobGrowthWatch,
			Interval: p.GrowthWatchInterval(),
			Run:      p.runGrowthJob,
			Due:      p.growthDueJob,
		},
		{
			Name:     JobActivity,
			Interval: activityTickInterval,
			Run:      p.runActivityJob,
			Due:      p.activityDueJob,
		},
	}
}

// runActivityJob 一趟活跃上报（只处理当日未报过的账号）。
func (p *Provider) runActivityJob(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.markJobRun(&p.activityLastRun)
	p.RunActivityNow()
	return nil
}

// activityDueJob 活跃上报任务是否该跑一轮。
//
// # 判据（两条都要满足）
//
//	① 当前处于配置的整点窗口内（与签到同口径：本地时区的小时整数）
//	② 今天还没跑过（按 CST 自然日去重 —— 与上游的日活跃重置口径一致）
//
// # 为什么用"当日已跑"而不是"距上次超过 24h"
//
// 上游的日活跃奖励按**自然日**去重。用 24h 间隔会出现：
// 某天 23:00 跑过之后，次日 22:00 才跑（漏了次日的窗口），
// 或者一天内跑两次（第一次在窗口外的人工触发把定时轮压到第二天）。
// 按自然日记一次，语义与上游一致。
func (p *Provider) activityDueJob(now time.Time) bool {
	if !p.ActivityEnabled() {
		return false
	}
	if !p.activityHourMatches(now) {
		return false
	}
	p.mu.Lock()
	lastDay := p.activityLastDay
	p.mu.Unlock()
	return lastDay != travelDay(now)
}

// activityHourMatches 当前小时是否落在配置的上报时点里。
//
// 空列表 = 未配置 → **不跑**（与签到/保活的"空 = 回落默认"不同）。
//
// # 为什么这里反过来了
//
// 签到/保活是既有功能，空配置必须保持老行为（回落默认时点）；
// 而活跃上报是本版本**新增**的能力，既有部署的 config 里没有它的键。
// 若沿用"空 = 回落默认 [10]"，所有老部署升级后会在 10 点自动开始
// 对每个账号发一条上游请求 —— 一个用户没要求的、默认开启的新行为。
//
// 因此它**默认关闭**：必须显式配 `schedule.activity_hours` 才跑。
// 这与"新增能力应当 opt-in"一致，也让升级行为可预测。
func (p *Provider) activityHourMatches(now time.Time) bool {
	p.mu.Lock()
	hours := append([]int(nil), p.activityHours...)
	p.mu.Unlock()
	if len(hours) == 0 {
		return false
	}
	h := now.Hour()
	for _, x := range hours {
		if x == h {
			return true
		}
	}
	return false
}

// SetActivitySchedule 注入活跃上报的时点与开关（main 从 config 解析后调用）。
//
// hours 为空表示**不启用**（见 activityHourMatches 的注释：本能力默认关闭）。
// enabled=false 显式关闭，且不擦除 hours（与签到的开关语义一致：
// 关掉再打开不需要重新配时点）。
func (p *Provider) SetActivitySchedule(hours []int, enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if hours != nil {
		p.activityHours = append([]int(nil), hours...)
	}
	p.activityEnabled = enabled
}

// ActivityEnabled 报告活跃上报当前是否启用。
func (p *Provider) ActivityEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activityEnabled && len(p.activityHours) > 0
}

// ActivityHours 返回配置的上报时点（副本，供管理台展示）。
func (p *Provider) ActivityHours() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.activityHours...)
}

// markActivityRan 记下"今天跑过了"（由 runActivity 结束后调用）。
func (p *Provider) markActivityRan(now time.Time) {
	p.mu.Lock()
	p.activityLastDay = travelDay(now)
	p.mu.Unlock()
}

// runTravelJob 一趟旅行守卫：只查到期账号，到站即领（受自动领奖开关约束）。
//
// 与改造前 RunTravelWatcher 的 ticker 分支逐字一致：force=false。
func (p *Provider) runTravelJob(ctx context.Context) error {
	if p.travel == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.markJobRun(&p.travelLastRun)
	p.RefreshTravel(false, p.TravelAutoClaimEnabled())
	return nil
}

// runGrowthJob 一趟成长守卫：只查到期账号，按开关执行自动动作。
//
// 与改造前 RunGrowthWatcher 的 ticker 分支逐字一致：force=false、autoActions=true。
func (p *Provider) runGrowthJob(ctx context.Context) error {
	if p.growth == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.markJobRun(&p.growthLastRun)
	p.RefreshGrowth(false, true)
	return nil
}

// markJobRun 记下本轮守卫的执行时刻（供 Due 的间隔下限使用）。
func (p *Provider) markJobRun(last *time.Time) {
	p.mu.Lock()
	*last = time.Now()
	p.mu.Unlock()
}

// travelDue 旅行守卫是否该跑一轮。
//
// 判据与 RefreshTravel(force=false) 内部的跳过条件完全一致：
// 「至少有一个账号到了它的下次检查时刻」。这样 Due 返回 true 时，
// 这一轮 Run 一定真的会打上游 —— 不会出现"核心以为有活、实际空转"的浪费。
//
// 首轮（还没有任何快照）返回 true：把缓存填满，等价于改造前的首扫。
//
// 额外叠加 Interval 下限：核心的 jobTickInterval 是 30 秒，而生产配置的
// 守卫间隔是 1 分钟。没有这道下限，守卫轮会被抬到 30 秒一轮 ——
// 虽不改变"哪些账号会被查"（Due 内部还有账号级门控），但空转频率翻倍。
// 取二者较大值，语义是「不早于配置的守卫间隔，也不早于账号到期」。
func (p *Provider) travelDue(now time.Time) bool {
	if p.travel == nil {
		return false
	}
	if !p.jobIntervalElapsed(&p.travelLastRun, p.WatchInterval(), now) {
		return false
	}
	return p.anyDue(now)
}

// growthDueJob 成长守卫是否该跑一轮。判据同 travelDue。
func (p *Provider) growthDueJob(now time.Time) bool {
	if p.growth == nil {
		return false
	}
	if !p.jobIntervalElapsed(&p.growthLastRun, p.GrowthWatchInterval(), now) {
		return false
	}
	// 快照为空视为到期（首轮全量扫，填满缓存）。
	p.growth.mu.Lock()
	empty := len(p.growth.snapshots) == 0
	p.growth.mu.Unlock()
	if empty {
		return true
	}
	// 只按本上游的号判到期：拿别家上游的账号当判据会让守卫轮
	// 被"别人的号到期了"错误地唤醒（空转上游请求）。
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		if p.growthDue(st.UID, now) {
			return true
		}
	}
	return false
}

// jobIntervalElapsed 判断距上次运行是否已过一个 GuardInterval。
//
// last 为 nil 或零值时返回 true（从未跑过 → 立即可跑）。
func (p *Provider) jobIntervalElapsed(last *time.Time, interval time.Duration, now time.Time) bool {
	if interval <= 0 {
		return true
	}
	p.mu.Lock()
	at := *last
	p.mu.Unlock()
	return at.IsZero() || !now.Before(at.Add(interval))
}

// 编译期断言：Provider 实现三个扩展点。
var (
	_ gateway.Provider = (*Provider)(nil)
	_ gateway.JobExt   = (*Provider)(nil)
)

// ---------------------------------------------------------------------------
// 槽位搭车（scheduler.SlotHook）
// ---------------------------------------------------------------------------

// AfterSlot 在核心跑完一轮整点槽位后被调用一次。
//
// # 为什么旅行不做成独立排程
//
// 旅行每日上限按「派出」计 1 次且在派出时锁定奖励，晚领不丢分；
// 分钟级巡检相对整点时点没有增益，只会多打上游。
// 所以它搭便车 —— 而且必须**在那一轮账号动作之后**跑：签到会解冻刚充值的
// 账号，晚跑一趟才能把本轮刚恢复的账号一起覆盖到。
//
// 顺序由核心保证：scheduler.RunSlot 先调 RunSlot，最后统一喊钩子。
//
// # 为什么只有签到槽位才推进旅行
//
// 核心会为**每一个**整点槽位喊钩子（它不认识槽位名）。"只在签到后跑"
// 这条判断属于本包 —— 它认识自己的槽位名。
//
// 挂到保活上会让旅行一天多跑一趟，且与签到时的账号解冻顺序脱节：
// 签到的收尾顺序是"先解冻、再旅行"，保活没有这个语义。
func (p *Provider) AfterSlot(slot string) {
	if slot != SlotCheckin {
		return
	}
	p.RunTravelNow()
}

// HookName 钩子的可读名（日志用）。
func (p *Provider) HookName() string { return "workbuddy-travel-after-slot" }
