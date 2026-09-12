// adminenv.go 管理端点需要的**核心侧**依赖，用消费方接口声明。
//
// # 为什么接口定义在 workbuddy（消费方）而不是核心
//
// 架构约束（gateway/arch_test.go 的 TestUpstreamsDoNotDependOnCore）禁止上游包
// 依赖 internal/pool / admin / server / scheduler。但上面那 22 个端点里有两条
// （/admin/schedule、/admin/task）读的**本来就是核心调度器**的东西：
//
//	GET /admin/schedule   → 签到/保活的启停、时点、下一次唤醒时刻
//	GET /admin/task       → 全量任务的共用任务槽状态
//
// 一条路是让核心继续硬编码这两条路由（那 /admin/task 的**写**方仍在
// 上游的 checkin/keepalive 里，读写分家）；另一条是本文件这种做法：
// 由消费方声明它需要什么，核心用一个薄适配器满足它。
//
// 这里选后者，因为"统一注册、挂载逻辑只有一份"的价值更高，
// 且两条端点的行为逐字未变（同样的 JSON 形状、同样的降级语义）。
//
// 若将来出现第二个上游，更干净的形态是把这两条移回 core：
// 它们不表达任何上游身份。届时删掉本文件即可，其余 20 条不受影响。
package workbuddy

import "time"

// SchedulerView 核心调度器在本包看来是什么样（只保留两条端点真正读的字段）。
//
// cmd/server 用一个薄适配器把 *scheduler.Scheduler 包装成它。
type SchedulerView interface {
	// NextWake 下一次唤醒时刻与将要执行的任务名。
	NextWake() (time.Time, []string)
	// Hours 签到时点与保活时点。
	Hours() (checkinHours, keepaliveHours []int)
	// CheckinEnabled 签到是否启用。
	CheckinEnabled() bool
	// KeepaliveEnabled 保活是否启用。
	KeepaliveEnabled() bool
}

// TaskSlot 后台任务槽：同一时刻只允许一个全量任务。
//
// 它由**谁**创建决定了 /admin/task 能否看到 checkin/keepalive 的进度：
//   - cmd/server 注入调度器自己的任务槽（生产路径）→ 四条端点共用一个槽，
//     与改造前 h.task 的行为逐字一致；
//   - 未注入时本包自建一个（测试路径）→ /admin/task 只反映本包的动作。
//
// 接口只有两个方法：开始一个任务、读快照。快照的形状是稳定的 JSON 契约，
// 所以用 map[string]any 而不是再定义一套结构 —— 前端字段名必须与改造前一致。
type TaskSlot interface {
	// Start 尝试执行 fn；已有任务在跑时返回 false（调用方应回 409）。
	Start(kind string, fn func() []map[string]any) bool
	// Snapshot 当前任务槽状态（与改造前 /admin/task 的响应体一致）。
	Snapshot() map[string]any
}
