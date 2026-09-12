// job.go 调度器的**框架部分**：注册上游自带的 JobExt 任务，按各任务自己的
// 到期判断错峰执行。本文件刻意不认识任何具体任务名。
//
// # 为什么要有这一层
//
// 改造前本包里混着某个上游的专属业务（成长任务、猫猫旅行），
// 于是核心调度器被迫理解 CodeBuddy 的任务体系（审计 354 处上游概念）。
// 加第二个上游时，这些逻辑要么被复制一份，要么被塞进一堆 if。
//
// 现在改成：上游在自己的包里实现 gateway.JobExt（返回 []gateway.Job），
// 核心只做三件事 —— 取任务、按 Due/Interval 判断到期、调用 Run 并记日志。
// **核心不认识任何具体任务名**，加新上游时本文件零改动。
//
// # 两类任务的差异（为什么 Job 有 Due）
//
//	固定间隔类（如保活轮询）：给 Interval，Due 留 nil，核心按间隔判断
//	错峰类（如按每个账号的到期时刻）：给 Interval 兜底 + Due，
//	  核心每 Interval 醒一次问一句"现在有活干吗"，真正的节流由上游自己掌握
//
// 后者是"按账号错峰"的真实形态：每个账号的下次检查时刻不同（按到站时刻、
// 按上次探测时刻），用一个全局固定间隔表达不了。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// jobTickInterval 错峰类任务的兜底轮询周期。
//
// 为什么需要一个兜底而不是让上游自己起 goroutine：
// 上游若自己跑循环，退出、日志、并发纪律就各写一套；由核心统一轮询后，
// ctx 取消一处生效，日志格式也统一。代价是探活粒度受它限制 ——
// 取 30 秒是因为守卫轮判的是"分钟级"的到期（抵达时刻 +20s 缓冲、空闲 30 分钟复查），
// 30 秒粒度对它们完全够，且空转成本仅一次 map 查询。
const jobTickInterval = 30 * time.Second

// Jobs 全部已注册的上游任务（按 Name 稳定排序，便于日志与测试断言）。
type Jobs struct {
	mu   sync.Mutex
	list []registeredJob
}

// registeredJob 一条已注册任务及其运行状态。
type registeredJob struct {
	job gateway.Job
	// providerID 该任务属于哪个上游。仅用于日志与状态展示 ——
	// 核心不因此对任务做任何差异化处理。
	providerID string
	// lastRun 上次执行完成时刻；零值表示从未执行。
	lastRun time.Time
	// lastErr 上次执行返回的错误（仅记录）。
	lastErr string
}

// NewJobs 建一个空的任务集合。
func NewJobs() *Jobs { return &Jobs{} }

// Discover 从注册表里发现所有实现 JobExt 的上游，收集它们的任务。
//
// 返回新增的任务数。**重复注册同名任务会被拒绝**（记日志并跳过）——
// 静默接受会让两个上游抢同一个任务名，日志里分不清是谁在跑。
func (j *Jobs) Discover(reg *gateway.Registry) int {
	if reg == nil {
		return 0
	}
	n := 0
	for _, p := range reg.All() {
		ext, ok := gateway.ExtOf[gateway.JobExt](p)
		if !ok {
			continue
		}
		for _, job := range ext.Jobs() {
			if j.add(p.ID(), job) {
				n++
			}
		}
	}
	return n
}

// add 注册一条任务；名称为空或重名时拒绝。
func (j *Jobs) add(providerID string, job gateway.Job) bool {
	if job.Name == "" {
		log.Printf("scheduler: 上游 %s 注册了无名任务，已跳过", providerID)
		return false
	}
	if job.Run == nil {
		log.Printf("scheduler: 任务 %s（上游 %s）没有 Run，已跳过", job.Name, providerID)
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, r := range j.list {
		if r.job.Name == job.Name {
			log.Printf("scheduler: 任务名 %s 重复（上游 %s 与 %s），已跳过后者",
				job.Name, r.providerID, providerID)
			return false
		}
	}
	j.list = append(j.list, registeredJob{job: job, providerID: providerID})
	return true
}

// Names 返回全部已注册任务名（稳定排序）。
func (j *Jobs) Names() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, 0, len(j.list))
	for _, r := range j.list {
		out = append(out, r.job.Name)
	}
	sort.Strings(out)
	return out
}

// Len 已注册任务数。
func (j *Jobs) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.list)
}

// Run 跑主循环，阻塞直到 ctx 取消。
//
// 每轮做的事固定为三步：取一份到期快照 → 逐个执行 → 睡到下一轮。
// 取快照而不是持锁执行：任务动辄打上游、耗时以秒计，持锁会让整轮串行化，
// 而各任务之间本无依赖（守卫轮是错峰的，同一时刻通常只有一个到期）。
func (j *Jobs) Run(ctx context.Context) {
	if j.Len() == 0 {
		// 没有任何任务时不空转，但仍要能被 ctx 唤醒 —— 直接阻塞等退出。
		<-ctx.Done()
		return
	}
	log.Printf("scheduler: 已注册 %d 个上游任务: %v", j.Len(), j.Names())
	for {
		j.runDue(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(jobTickInterval):
		}
	}
}

// RunOnce 立刻按到期判断执行一轮（不做间隔等待）。
//
// 供测试与「立即执行」入口使用：测试要能确定性地触发一轮，
// 而不是 sleep 一个 jobTickInterval 去碰运气。
func (j *Jobs) RunOnce(now time.Time) {
	j.runDue(now)
}

// dueList 挑出此刻到期的任务。
func (j *Jobs) dueList(now time.Time) []registeredJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]registeredJob, 0, len(j.list))
	for _, r := range j.list {
		if jobDue(r, now) {
			out = append(out, r)
		}
	}
	return out
}

// jobDue 判定一条任务此刻是否该跑。
//
//	有 Due → 完全交给上游判断（核心不叠加间隔条件，
//	          否则给一个错峰任务配的 Interval 会变成隐式的额外门槛）
//	无 Due → 按 Interval 固定间隔；从未跑过则立即跑
//	Interval 与 Due 都缺省 → 视为每轮都跑（调用方自会做内部门控）
func jobDue(r registeredJob, now time.Time) bool {
	if r.job.Due != nil {
		return r.job.Due(now)
	}
	if r.job.Interval > 0 {
		return r.lastRun.IsZero() || !now.Before(r.lastRun.Add(r.job.Interval))
	}
	return true
}

// runDue 执行一轮到期任务，并记录结果。
//
// 单个任务 panic 或返回 error 都**不影响其它任务**：一个上游的任务炸了
// 不能拖累另一个上游 —— 这是"失败隔离"在调度层的具体落点。
func (j *Jobs) runDue(now time.Time) {
	for _, r := range j.dueList(now) {
		j.runOne(r.job)
	}
}

// runOne 执行单条任务并回写状态。
func (j *Jobs) runOne(job gateway.Job) {
	err := runGuarded(job)
	j.mu.Lock()
	for i := range j.list {
		if j.list[i].job.Name != job.Name {
			continue
		}
		j.list[i].lastRun = time.Now()
		if err != nil {
			j.list[i].lastErr = shortErr(err)
		} else {
			j.list[i].lastErr = ""
		}
		break
	}
	j.mu.Unlock()
	if err != nil {
		log.Printf("scheduler: 任务 %s 出错: %v", job.Name, err)
	}
}

// runGuarded 调用 Run，把 panic 收敛成 error。
//
// 为什么必须 recover：任务跑在调度器的常驻 goroutine 里，一次 panic 会
// 直接把整个调度循环带走 —— 之后所有任务（含其它上游的）永久停摆。
// 上游代码里的一个空指针不该有这种影响面。
func runGuarded(job gateway.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError{name: job.Name, val: r}
		}
	}()
	return job.Run(context.Background())
}

// panicError 任务 panic 的收敛形态。
type panicError struct {
	name string
	val  any
}

func (e panicError) Error() string {
	return "任务 " + e.name + " panic: " + fmt.Sprint(e.val)
}

// Status 单个任务的运行状态（供管理台展示）。
type Status struct {
	Name       string `json:"name"`
	Provider   string `json:"provider,omitempty"`
	IntervalMs int64  `json:"interval_ms,omitempty"`
	LastRun    string `json:"last_run,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// Statuses 返回全部任务的状态（稳定排序）。
func (j *Jobs) Statuses() []Status {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Status, 0, len(j.list))
	for _, r := range j.list {
		s := Status{Name: r.job.Name, Provider: r.providerID, LastError: r.lastErr}
		if r.job.Interval > 0 {
			s.IntervalMs = int64(r.job.Interval / time.Millisecond)
		}
		if !r.lastRun.IsZero() {
			s.LastRun = r.lastRun.Format(time.RFC3339)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out
}

// ---------------------------------------------------------------------------
// 槽位搭车钩子
// ---------------------------------------------------------------------------

// SlotHook 上游想在「整点槽位收尾」时顺带推进的任务。
//
// # 为什么需要它（以及为什么不是第三种扩展点）
//
// 有些上游任务**刻意不独立排程**：例如"每日一次、晚做不丢分"的巡检 ——
// 分钟级轮询相对整点时点没有增益，只会多打上游。这类任务与整点时点合并执行即可。
//
// 但"搭车"这件事不能由核心写死 —— 核心不知道谁有这类任务。
// 所以反过来说明：**核心定义钩子的形状，上游实现它**。
//
// # 为什么钩子的触发点是「任意槽位收尾」而不是某个具体槽位
//
// 改造前这里叫 CheckinHook，只在签到收尾时喊 —— 于是核心被迫知道
// "签到"是什么。现在改成"任何一个整点槽位跑完都喊一遍"，钩子由上游
// 自己判断这次该不该干活（它拿得到槽位名）。核心的语义因此退化成
// 「有整点任务跑完了」，与具体业务无关。
//
// 为什么不做成 gateway 的扩展点：那个接口面向的是"上游对上"，而
// 钩子的语义是"核心在某个时机喊一声"，方向相反。放在 scheduler 里，
// 由 cmd/server 在接线处桥接（上游不得 import scheduler）。
type SlotHook interface {
	// AfterSlot 在某个整点槽位跑完一轮后被调用一次；slot 是槽位名。
	AfterSlot(slot string)
	// HookName 钩子的可读名（只用于日志）。
	HookName() string
}

// AddSlotHook 注册一个槽位搭车钩子。非线程安全，只在构造期调用。
func (s *Scheduler) AddSlotHook(h SlotHook) {
	if h == nil {
		return
	}
	s.hooks = append(s.hooks, h)
}

// runSlotHooks 依次跑所有已注册的搭车钩子（panic 收敛，互不影响）。
// slot 是刚刚跑完的槽位名，原样透传给钩子。
func (s *Scheduler) runSlotHooks(slot string) {
	for _, h := range s.hooks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("scheduler: 槽位钩子 %s panic: %v", h.HookName(), r)
				}
			}()
			h.AfterSlot(slot)
		}()
	}
}

// Jobs 返回任务集合（供管理台展示调度状态）。可能为 nil（直接构造的调度器）。
func (s *Scheduler) Jobs() *Jobs { return s.jobs }
