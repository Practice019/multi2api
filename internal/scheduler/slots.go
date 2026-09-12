// slots.go 调度器的**整点槽位**框架。
//
// # 这一层解决什么
//
// 调度器只有两种时间线：**整点槽位**（本文件）与**守卫轮**（job.go）。
// 槽位这条线改造前写死了两个具体任务（签到/保活）：核心因此知道
// "签到打哪个上游端点""已签到的错误文案长什么样""余额>0 就该解冻" ——
// 这些全是某个上游的业务，不是调度的事。
//
// 现在改成：**核心只知道"有一个叫 X 的槽位、它配在 Y 点、到点喊一声"**。
// 槽位做什么由 SlotRunner（消费方接口，见下）决定，而它的实现在上游包里。
//
// # 为什么是消费方接口而不是第四个 gateway 扩展点
//
// 槽位是**双向**的：
//
//	核心 → 上游：到点了，跑一趟（RunSlot / RunSlotFor）
//	上游 → 核心：这个槽位叫什么、配在几点、是否启用（Slot 定义）
//
// gateway 的扩展点面向"上游对上声明自己有什么"，方向是单向的。
// 而这里核心是**调用方**：它需要的是一个"到点找谁"的回调，
// 与 workbuddy/accounts.go 的做法同向 —— **接口由消费方声明，
// 实现在上游，cmd/server 装配时把两者接起来**。
//
// 于是依赖方向是：cmd/server → {scheduler, workbuddy}，
// 两侧互不 import（架构约束由 gateway/arch_test.go 强制）。
//
// # 中间类型为什么在核心
//
// TaskResult 是**对外 HTTP 契约**的一部分（/admin/checkin 的响应体直接
// 序列化它），而端点实现已在上游包里。两边各自声明一份同形结构、
// 由 cmd/server 的适配器逐字段转换 —— 这与 CheckinOutcome 的做法一致：
// 类型不得共用，形状必须一致（json tag 是用户看得见的东西）。
package scheduler

import (
	"sort"
	"time"
)

// TaskResult 一次账号级任务的结果（对外 JSON 契约，字段名不可改）。
//
// 与 workbuddy.CheckinOutcome 逐字段对应（含 json tag）：
// 两边各自声明是为了让核心不 import 上游，转换由 cmd/server 的适配器做。
type TaskResult struct {
	UID      string `json:"uid"`
	Status   string `json:"status"` // ok | already | fail | skip
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits"`
	HasQuota bool   `json:"has_quota"`
}

// Slot 一个整点槽位的定义（由装配处给出，核心不解释它的名字）。
//
//	Name   槽位的稳定标识（对前端与日志可见，如 "keepalive"）。
//	Hours  默认执行时点（本地小时 0-23）。空切片表示"没有默认时点"，
//	       此时该槽位不产生整点唤醒（与改造前"时点为空"的行为一致）。
//	Disabled 初始是否停用（零值 = 启用，与改造前用「禁用」命名的理由相同）。
type Slot struct {
	Name     string
	Hours    []int
	Disabled bool
}

// SlotRunner 核心在整点唤醒时调用的**账号级动作**（消费方接口）。
//
// 实现在上游包里（如 workbuddy.Provider），由 cmd/server 用薄适配器接上：
// 两边的返回类型各自声明，适配器只做一次逐字段转换。
//
// 为什么两个方法而不是一个：全量任务跑在后台（要占用任务槽、有 409 语义），
// 单账号任务同步返回（一次上游往返，够快）。这个区分不是上游业务，
// 是核心的调用约定，所以留在接口上。
//
// name 是槽位名：核心**原样转发**，不判断也不解释 ——
// 上游据此决定"这个名字对应哪段业务"。
type SlotRunner interface {
	// RunSlot 对全部账号执行一次该槽位的动作。
	RunSlot(name, trigger string)
	// RunSlotFor 对单个账号执行；账号不存在时 ok=false。
	RunSlotFor(name, uid, trigger string) (TaskResult, bool)
}

// slotState 一个槽位的运行时状态（定义 + 运行时的时点/启停覆盖）。
//
// 覆盖值用指针：零值（nil）表示「未覆盖」，此时回落到 Slot 的初始值 ——
// 这样直接构造 &Scheduler{cfg: ...}（既有测试的写法）行为完全不变。
type slotState struct {
	def Slot

	enabledOverride *bool
	hoursOverride   *[]int
}

// enabled 该槽位当前是否生效。
func (st *slotState) enabled() bool {
	if st.enabledOverride != nil {
		return *st.enabledOverride
	}
	return !st.def.Disabled
}

// hours 该槽位当前生效的时点。
func (st *slotState) hours() []int {
	if st.hoursOverride != nil {
		return *st.hoursOverride
	}
	return st.def.Hours
}

// setEnabled 运行时开关。
func (st *slotState) setEnabled(on bool) {
	v := on
	st.enabledOverride = &v
}

// setHours 运行时改时点（值已校验）。
func (st *slotState) setHours(hours []int) {
	v := append([]int(nil), hours...)
	st.hoursOverride = &v
}

// slotAt 某个槽位在某个时刻产生的唤醒点。
type slotAt struct {
	at   time.Time
	name string
}

// initSlots 按装配处给的定义建槽位表（保持给定顺序，便于稳定输出）。
func (s *Scheduler) initSlots(defs []Slot) {
	s.slots = make([]*slotState, 0, len(defs))
	for _, d := range defs {
		dd := d
		dd.Hours = append([]int(nil), d.Hours...)
		s.slots = append(s.slots, &slotState{def: dd})
	}
}

// slotByName 按名取槽位（大小写敏感：名字是契约）。找不到返回 nil。
func (s *Scheduler) slotByName(name string) *slotState {
	for _, st := range s.slots {
		if st.def.Name == name {
			return st
		}
	}
	return nil
}

// SlotEnabled 报告某槽位当前是否生效（未定义的槽位返回 false）。
func (s *Scheduler) SlotEnabled(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slotByName(name)
	return st != nil && st.enabled()
}

// SetSlotEnabled 运行时开关某槽位，立即影响下一次 nextWake。
// 未定义的槽位静默忽略（装配处还没登记它，没有可开关的东西）。
func (s *Scheduler) SetSlotEnabled(name string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.slotByName(name); st != nil {
		st.setEnabled(on)
	}
}

// SlotHours 某槽位当前生效的时点（只读拷贝）。未定义时返回 nil。
func (s *Scheduler) SlotHours(name string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.slotByName(name)
	if st == nil {
		return nil
	}
	return append([]int(nil), st.hours()...)
}

// SetSlotHours 运行时改某槽位的时点。
//
// 空切片视为「不改」（与改造前的语义一致：管理台不传就是不动它）；
// 非法小时返回错误且不生效。错误文案里的槽位名由**调用方**给的中文名决定 ——
// 核心不认识 "checkin" 是什么，写不出"签到时点 25 非法"这种话，
// 所以错误里用槽位名本身。
func (s *Scheduler) SetSlotHours(name string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return &HourRangeError{Slot: name, Hour: h}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.slotByName(name); st != nil && len(hours) > 0 {
		st.setHours(hours)
	}
	return nil
}

// HourRangeError 时点越界。
//
// 为什么不在这里拼中文：核心不认识槽位的业务名，"签到时点 25 非法"
// 这句话属于上游。核心给出槽位名与越界值，由上游/装配处决定怎么讲。
type HourRangeError struct {
	Slot string
	Hour int
}

func (e *HourRangeError) Error() string {
	return "槽位 " + e.Slot + " 的时点 " + itoa(e.Hour) + " 非法（0-23）"
}

// itoa 极小的整数转字符串（避免为一个错误文案 import strconv 之外的东西）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// slotWakeups 列出 now 之后各启用槽位的唤醒点（未启用的不进候选）。
func (s *Scheduler) slotWakeups(now time.Time) []slotAt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]slotAt, 0, len(s.slots))
	for _, st := range s.slots {
		if !st.enabled() {
			continue
		}
		out = append(out, slotAt{at: nextFire(now, st.hours()), name: st.def.Name})
	}
	return out
}

// NextWake 返回下一次唤醒时刻，以及该时刻需要执行的槽位名。
// 管理台据此显示倒计时；所有槽位都停用时返回零时间与 nil。
func (s *Scheduler) NextWake() (time.Time, []string) {
	at, slots := s.nextWake(time.Now())
	names := make([]string, 0, len(slots))
	for _, sl := range slots {
		names = append(names, sl.name)
	}
	return at, names
}

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部槽位。
//
// 两个槽位若配到同一小时（如都含 22），该时刻两个都要执行 ——
// 判据是**时刻相等**而不是"同一次 nextFire 调用"，因为不同槽位的
// 时点列表不同，各自的最近点可能恰好落在同一时刻。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []slotAt) {
	cands := s.slotWakeups(now)
	var earliest time.Time
	for _, c := range cands {
		if c.at.IsZero() {
			continue
		}
		if earliest.IsZero() || c.at.Before(earliest) {
			earliest = c.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var out []slotAt
	for _, c := range cands {
		if !c.at.IsZero() && c.at.Equal(earliest) {
			out = append(out, c)
		}
	}
	// 同一时刻多个槽位时按名稳定排序：日志与测试断言才可预期。
	sort.Slice(out, func(i, k int) bool { return out[i].name < out[k].name })
	return earliest, out
}
