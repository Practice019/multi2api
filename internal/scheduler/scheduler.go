// Package scheduler 定时任务**框架**。
//
// # 两条时间线，零上游知识
//
//	整点槽位（slots.go）：装配处给出「有个叫 X 的槽位、配在 Y 点」，
//	                      到点核心通过 SlotRunner 喊一声，**不解释名字**。
//	守卫轮（job.go）：  上游通过 gateway.JobExt 注册，核心按 Due/Interval 判到期。
//
// # 本包不认识任何具体上游
//
// 改造前这里装着两个具体任务的全部业务（打哪个上游端点、错误文案怎么认、
// 余额多少算可用、ReenableIfCredits）。它们现在都在 internal/workbuddy，
// 本包只剩：注册表、判到期、错峰执行、任务槽、记历史、日志。
//
// **本包不得 import 任何 internal/<上游> 包**（由 gateway 的架构约束测试强制）。
// 接线发生在 cmd/server：槽位定义由它给出，SlotRunner 由它适配。
package scheduler

import (
	"context"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// Config 调度器依赖。
//
// 这里**没有**任何上游业务字段（时点、端点、开关都属于某个上游）。
// 唯一表达"要跑什么"的是 Slots —— 而它只是一份名字与时点的清单。
type Config struct {
	// Slots 整点槽位定义（见 Slot）。为空则本调度器没有整点任务，
	// 只推进守卫线 —— 循环不会因此停摆（见 Run）。
	//
	// 为什么定义由装配处给而不是核心内置：内置就意味着核心知道
	// "签到"这个概念。装配处（cmd/server）是唯一同时认识核心与上游的地方，
	// 它把上游的业务名翻译成核心听得懂的"槽位"。
	Slots []Slot

	// Runner 整点槽位的执行体（消费方接口，见 SlotRunner）。
	//
	// 为 nil 时到点只记日志、不执行 —— 既有测试（只关心排程与循环行为、
	// 直接构造 &Scheduler{cfg: ...}）因此无需任何改动。
	Runner SlotRunner

	// Log 任务结果历史（可选；nil = 不记录）。管理台的「今日跑了吗」也读它。
	Log *checkinlog.Log

	// Registry 上游注册表。非 nil 时 New 会自动发现各上游的 JobExt 任务
	// （见 job.go 的 Jobs.Discover）—— 这是"加新上游时核心零改动"的接线点。
	Registry *gateway.Registry
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu 保护 slots 与下面各槽位的运行时覆盖（管理台/设置页在请求路径上改它们）。
	mu    sync.Mutex
	slots []*slotState

	// jobs 上游注册的守卫任务集合。
	//
	// 为 nil 时（直接构造 &Scheduler{cfg: ...}）所有相关方法都做了 nil 保护，
	// 行为退化为"没有上游任务"，其余不受影响。
	jobs *Jobs

	// hooks 整点任务收尾时顺带推进的上游任务（见 SlotHook 的注释）。
	// 在 cmd/server 的接线处注册；构造期之后不再改动，无需加锁。
	hooks []SlotHook
}

// New 构建。
func New(cfg Config) *Scheduler {
	s := &Scheduler{cfg: cfg, jobs: NewJobs()}
	s.initSlots(cfg.Slots)
	// 从注册表发现各上游的 JobExt 任务。放在构造期而不是 Run 里：
	// 管理台的"调度状态"要能立刻看到任务清单。
	s.jobs.Discover(cfg.Registry)
	return s
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
//
// 空 hours 返回零时间（该槽位不产生唤醒点）。now 恰好落在整点上时**滚到明天**：
// 时点是"每天在这个小时跑一次"，不接受"此刻刚过就算今天还没跑"，
// 否则 Run 会在整点唤醒后立刻又算出同一个时刻，形成忙循环。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
//
// 两条时间线并行推进：
//   - **整点线**：各启用槽位按配置的时点唤醒（下一时点由 nextWake 算出）
//   - **守卫线**：各上游注册的 Job 按各自的 Due/Interval 判到期
//
// 为什么合成一个循环而不是各起一个 goroutine：日志顺序、退出纪律、
// "错峰"这件事都只有一处实现。守卫线每 jobTickInterval 醒一次问一句
// "有活干吗"，空转成本是一次 map/O(1) 判断，远低于再多一条 goroutine 的维护成本。
//
// ⚠ 两条时间线**共用同一次睡眠**，所以醒来时必须分清"这次是谁的闹钟"
// （见 onWake）。这里出过一次实测事故：唤醒点会被守卫线提前到 tick，
// 而槽位曾是无条件执行的 —— 于是签到（含搭车的猫猫旅行）每 30 秒打一次上游，
// 一天约 2880 次，日志里刷满"今天已签到，请明天再来"，任务历史被灌满。
func (s *Scheduler) Run(ctx context.Context) {
	s.runLoop(ctx, jobTickInterval)
}

// runLoop 主循环本体；tick 是守卫线的轮询周期。
//
// 为什么 tick 是参数而不是直接用常量：整点时点是**小时**级的，测试里没法把
// "到点"造到毫秒级；而"守卫线把唤醒提前"这个形态必须能在毫秒级复现，
// 否则这条回归只能靠 sleep 30 秒去碰运气（"等不起就干脆不测"正是本项目的教训）。
// 生产路径只由 Run 传入常量。
func (s *Scheduler) runLoop(ctx context.Context, tick time.Duration) {
	for {
		next, slots := s.nextWake(time.Now())
		// 守卫线本轮是否该跑。所有整点槽位都停用时 next 为零值 ——
		// 此时仍要继续推守卫线，不能整条循环停摆。
		s.runJobsOnce(time.Now())

		if next.IsZero() {
			// 无整点任务可等：不空转，但也不能永久阻塞——否则管理台在运行时
			// 重新打开开关后，没有任何东西能唤醒这个循环。改为低频复查。
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval(s.jobs)):
				continue
			}
		}

		// 睡到「下一个整点时点」与「下一条守卫线轮询」中更早的那个。
		wake := next
		if t := time.Now().Add(tick); t.Before(wake) && s.jobs != nil && s.jobs.Len() > 0 {
			wake = t
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点槽位在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			s.onWake(time.Now(), next, slots)
		}
	}
}

// onWake 处理一次唤醒：**真的到了整点才跑槽位**，否则什么都不做。
//
// # 为什么必须有这个判断（2026-09-13 实测事故）
//
// next/slots 是"下一次整点"和"那一刻要跑的槽位"（见 nextWake）。
// 而循环还会为了守卫线每 tick 醒一次 —— 那种唤醒是被 tick 提前的，
// 此刻离 next 还远。旧实现无条件执行 slots，于是：
//
//	签到槽位 → 每 30 秒打一次上游签到（上游只会回"今天已签到，请明天再来"）
//	槽位钩子 → runSlotHooks 每 30 秒喊一次猫猫旅行守卫（绕过它自己的 60s 下限）
//	任务历史 → 每 30 秒一条记录，一天约 2880 条/账号，历史表被灌满
//
// 判据用 `now.Before(next)`：等于 next 也算到点（timer 不会提前触发，
// 正常路径下醒来时刻必然 >= next）。next 为零值走不到这里 ——
// 那种情况在上面的分支里已经 continue 了。
func (s *Scheduler) onWake(now, next time.Time, slots []slotAt) {
	if now.Before(next) {
		return // 守卫线轮询：还没到整点，本次不跑任何槽位
	}
	for _, sl := range slots {
		s.RunSlot(sl.name, triggerSchedule)
	}
}

// runJobsOnce 按到期判断推一轮上游注册的守卫任务。
func (s *Scheduler) runJobsOnce(now time.Time) {
	if s.jobs == nil || s.jobs.Len() == 0 {
		return
	}
	s.jobs.RunOnce(now)
}

// pollInterval 没有整点任务可等时的复查间隔。
//
// 有上游任务时用 jobTickInterval：否则「没有整点任务但注册了守卫任务」
// 的场景会把守卫轮的粒度也拖成 disabledPollInterval。
func pollInterval(jobs *Jobs) time.Duration {
	if jobs != nil && jobs.Len() > 0 {
		return jobTickInterval
	}
	return disabledPollInterval
}

// disabledPollInterval 无整点任务可等时的复查间隔。
const disabledPollInterval = 30 * time.Second

// ---------------------------------------------------------------------------
// 槽位执行
// ---------------------------------------------------------------------------

// RunSlot 立即对全部账号执行一次某槽位的动作，并顺带跑一趟搭车钩子。
//
// 顺序是刻意的：**先逐账号执行，最后统一喊钩子**。钩子里的任务可能需要
// 看到本轮刚被解冻的账号，晚跑才能覆盖到它们（改造前 travel 搭签到的便车
// 依赖的正是这个顺序）。核心不知道钩子在做什么，只保证顺序。
func (s *Scheduler) RunSlot(name, trigger string) {
	if s.cfg.Runner != nil {
		s.cfg.Runner.RunSlot(name, trigger)
	}
	s.runSlotHooks(name)
}

// RunSlotFor 对单个账号执行某槽位的动作（管理台入口）。
// 账号不存在时 ok=false（由上游的 SlotRunner 判定）。
func (s *Scheduler) RunSlotFor(name, uid, trigger string) (TaskResult, bool) {
	if s.cfg.Runner == nil {
		return TaskResult{UID: uid, Status: checkinlog.StatusFail}, false
	}
	return s.cfg.Runner.RunSlotFor(name, uid, trigger)
}

// Record 写一条任务历史（供槽位实现回写结果；未注入 Log 时静默丢弃）。
//
// 为什么历史由核心写而不是上游自己写：历史是**核心的观测面**
// （管理台的历史表由 admin 展示，跨上游统一）。上游只知道
// "这次动作的结果是什么"，把结果交给核心落库，格式才不会一家一个样。
func (s *Scheduler) Record(uid, kind, status, detail string, credits int64, trigger string) {
	s.record(uid, kind, status, detail, credits, trigger)
}

// record 写一条任务历史（未注入 Log 时静默丢弃）。
//
// Nickname 由核心补：它从账号池现取（上游拿不到"昵称"这个展示字段）。
func (s *Scheduler) record(uid, kind, status, detail string, credits int64, trigger string) {
	if s.cfg.Log == nil {
		return
	}
	s.cfg.Log.Append(checkinlog.Record{
		At:      time.Now(),
		UID:     checkinlog.NormalizeUID(uid),
		Kind:    kind,
		Status:  status,
		Detail:  detail,
		Credits: credits,
		Trigger: trigger,
	})
}

// ---------------------------------------------------------------------------
// 对外状态（管理台）
// ---------------------------------------------------------------------------

// SlotsStatus 各槽位的名字、当前时点与启停（只读快照，装配顺序稳定）。
func (s *Scheduler) SlotsStatus() []SlotInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SlotInfo, 0, len(s.slots))
	for _, st := range s.slots {
		out = append(out, SlotInfo{
			Name:    st.def.Name,
			Enabled: st.enabled(),
			Hours:   append([]int(nil), st.hours()...),
		})
	}
	return out
}

// SlotInfo 一个槽位的对外状态。
type SlotInfo struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Hours   []int  `json:"hours"`
}

// JobStatuses 各已注册任务的运行状态（只读快照，按任务名稳定排序）。
//
// # 为什么需要它
//
// 在这之前，`JobExt` 注册的定时任务对使用者是**完全不可见**的：
// 守卫轮有没有在跑、上次跑是什么时候、有没有报错，只有翻日志才知道。
// `/admin/ui/manifest` 要把这份状态下发给控制台，所以在这里开一个只读口子。
//
// # 为什么复用 Jobs.Statuses 而不是自己遍历
//
// 状态（lastRun / lastErr）只存在于 Jobs 内部，Scheduler 只持有它的指针。
// 自己再遍历一遍等于维护第二份事实来源，迟早与真实运行状态不一致。
func (s *Scheduler) JobStatuses() []Status {
	if s == nil || s.jobs == nil {
		return nil
	}
	return s.jobs.Statuses()
}
