// Package scheduler 定时任务**框架**：按整点执行签到/保活，并按到期判断执行
// 各上游通过 gateway.JobExt 注册的守卫任务（见 job.go）。
//
// # 本包不认识任何具体上游
//
// 改造前这里还装着「成长中心守卫」「猫猫旅行守卫」这些 CodeBuddy 专属业务
// （审计 354 处上游概念）。它们已搬进 internal/workbuddy，本包只留框架：
// 注册 Job、判到期、错峰执行、记日志。
//
// **本包不得 import 任何 internal/<上游> 包**（由 gateway 的架构约束测试强制）。
// 接线发生在 cmd/server：调度器通过 gateway.ExtOf[gateway.JobExt](provider)
// 发现任务。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即两类任务都启用，
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHours []int // 默认 [9, 21]

	// KeepaliveHours 默认 [22]。
	KeepaliveHours []int

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	CheckinDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool

	// Log 任务结果历史（可选；nil = 不记录）。管理台的「今日签到了吗」也读它。
	Log *checkinlog.Log

	// Registry 上游注册表。非 nil 时 New 会自动发现各上游的 JobExt 任务
	// （见 job.go 的 Jobs.Discover）—— 这是"加新上游时核心零改动"的接线点。
	//
	// 为 nil 时调度器只有签到/保活两类内置任务，行为与改造前一致
	// （既有测试全部走这条路径，无需改动）。
	Registry *gateway.Registry
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu 保护下面四个运行时覆盖指针（管理台/设置页在请求路径上改它们）。
	mu sync.Mutex

	// checkinOverride/keepaliveOverride 运行时开关（管理台用）。
	// 用 *bool 而非 bool：零值表示「未覆盖」，此时回落到 cfg 的初始值——
	// 这样直接构造 &Scheduler{cfg: ...}（既有测试的写法）行为完全不变。
	checkinOverride   *bool
	keepaliveOverride *bool

	// checkinHoursOverride/keepaliveHoursOverride 运行时时点覆盖（设置页用）。
	// 与布尔开关同理用指针：nil = 未覆盖，回落到 cfg 初值，既有测试行为不变。
	checkinHoursOverride   *[]int
	keepaliveHoursOverride *[]int

	// jobs 上游注册的守卫任务集合（成长/旅行/…）。
	//
	// 为 nil 时（直接构造 &Scheduler{cfg: ...} 的既有测试）所有相关方法
	// 都做了 nil 保护，行为退化为"没有上游任务"，其余不受影响。
	jobs *Jobs

	// hooks 签到收尾时顺带推进的上游任务（见 CheckinHook 的注释）。
	// 在 cmd/server 的接线处注册；构造期之后不再改动，无需加锁。
	hooks []CheckinHook
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	s := &Scheduler{
		cfg:  cfg,
		jobs: NewJobs(),
	}
	// 从注册表发现各上游的 JobExt 任务。放在构造期而不是 Run 里：
	// 管理台的"调度状态"要能立刻看到任务清单。
	s.jobs.Discover(cfg.Registry)
	return s
}

// CheckinEnabled 报告签到排程当前是否生效（未覆盖时取 cfg 初值）。
func (s *Scheduler) CheckinEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkinOverride != nil {
		return *s.checkinOverride
	}
	return !s.cfg.CheckinDisabled
}

// KeepaliveEnabled 报告保活排程当前是否生效（未覆盖时取 cfg 初值）。
func (s *Scheduler) KeepaliveEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keepaliveOverride != nil {
		return *s.keepaliveOverride
	}
	return !s.cfg.KeepaliveDisabled
}

// SetCheckinEnabled 运行时开关签到排程，立即影响下一次 nextWake。
func (s *Scheduler) SetCheckinEnabled(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := on
	s.checkinOverride = &v
}

// SetKeepaliveEnabled 运行时开关 token 保活排程。
func (s *Scheduler) SetKeepaliveEnabled(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := on
	s.keepaliveOverride = &v
}

// Hours 返回两类任务的时点配置（只读拷贝）。
func (s *Scheduler) Hours() (checkin, keepalive []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := append([]int(nil), s.effectiveHoursLocked(hoursCheckin)...)
	k := append([]int(nil), s.effectiveHoursLocked(hoursKeepalive)...)
	return c, k
}

type hourKind int

const (
	hoursCheckin hourKind = iota
	hoursKeepalive
)

// effectiveHours 取覆盖值（若有）否则取 cfg 初值。
func (s *Scheduler) effectiveHours(k hourKind) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveHoursLocked(k)
}

// effectiveHoursLocked 同上，调用方需已持有 s.mu。
func (s *Scheduler) effectiveHoursLocked(k hourKind) []int {
	if k == hoursCheckin && s.checkinHoursOverride != nil {
		return *s.checkinHoursOverride
	}
	if k == hoursKeepalive && s.keepaliveHoursOverride != nil {
		return *s.keepaliveHoursOverride
	}
	if k == hoursCheckin {
		return s.cfg.CheckinHours
	}
	return s.cfg.KeepaliveHours
}

// SetHours 运行时修改签到时点。空切片视为「不改」；非法小时返回错误且不生效。
func (s *Scheduler) SetHours(checkin, keepalive []int) error {
	for _, h := range checkin {
		if h < 0 || h > 23 {
			return fmt.Errorf("签到时点 %d 非法（0-23）", h)
		}
	}
	for _, h := range keepalive {
		if h < 0 || h > 23 {
			return fmt.Errorf("保活时点 %d 非法（0-23）", h)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(checkin) > 0 {
		v := append([]int(nil), checkin...)
		s.checkinHoursOverride = &v
	}
	if len(keepalive) > 0 {
		v := append([]int(nil), keepalive...)
		s.keepaliveHoursOverride = &v
	}
	return nil
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
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

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskKeepalive
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 签到与保活若配到同一小时（如都含 22），该时刻两类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if s.CheckinEnabled() {
		slots = append(slots, slot{nextFire(now, s.effectiveHours(hoursCheckin)), taskCheckin})
	}
	if s.KeepaliveEnabled() {
		slots = append(slots, slot{nextFire(now, s.effectiveHours(hoursKeepalive)), taskKeepalive})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// NextWake 返回下一次唤醒时刻，以及该时刻需要执行的任务名（"checkin"/"keepalive"）。
// 管理台据此显示倒计时；两类任务都被关闭时返回零时间与 nil。
func (s *Scheduler) NextWake() (time.Time, []string) {
	at, kinds := s.nextWake(time.Now())
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.String())
	}
	return at, names
}

// String 任务名（对管理台/日志暴露的稳定标识）。
func (k taskKind) String() string {
	switch k {
	case taskCheckin:
		return "checkin"
	case taskKeepalive:
		return "keepalive"
	default:
		return fmt.Sprintf("task(%d)", int(k))
	}
}

// Run 主循环，阻塞直到 ctx 取消。
//
// 两条时间线并行推进：
//   - **整点线**：签到/保活按配置的时点唤醒（下一时点由 nextWake 算出）
//   - **守卫线**：各上游注册的 Job 按各自的 Due/Interval 判到期
//
// 为什么合成一个循环而不是各起一个 goroutine：日志顺序、退出纪律、
// "错峰"这件事都只有一处实现。守卫线每 jobTickInterval 醒一次问一句
// "有活干吗"，空转成本是一次 map/O(1) 判断，远低于再多一条 goroutine 的维护成本。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		// 守卫线本轮是否该跑。两类整点任务都禁用时 next 为零值 ——
		// 此时仍要继续推守卫线，不能整条循环停摆。
		s.runJobsOnce(time.Now())

		if next.IsZero() {
			// 两类任务全部禁用：不空转，但也不能永久阻塞——否则管理台在运行时
			// 重新打开开关后，没有任何东西能唤醒这个循环。改为低频复查。
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollInterval(s.jobs, next)):
				continue
			}
		}

		// 睡到「下一个整点时点」与「下一条守卫线轮询」中更早的那个。
		wake := next
		if tick := time.Now().Add(jobTickInterval); tick.Before(wake) && s.jobs.Len() > 0 {
			wake = tick
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 本次是守卫线轮询（而非整点）时 kinds 为空，什么都不做，
			// 回到循环顶部再判一次即可。
			for _, k := range kinds {
				switch k {
				case taskCheckin:
					s.RunCheckinNow()
				case taskKeepalive:
					s.RunKeepaliveNow()
				}
			}
		}
	}
}

// runJobsOnce 按到期判断推一轮上游注册的守卫任务。
func (s *Scheduler) runJobsOnce(now time.Time) {
	if s.jobs == nil || s.jobs.Len() == 0 {
		return
	}
	s.jobs.RunOnce(now)
}

// pollInterval 两类整点任务都禁用时的复查间隔。
//
// 有上游任务时用 jobTickInterval：否则「没有整点任务但注册了守卫任务」
// 的场景会把守卫轮的粒度也拖成 disabledPollInterval。
func pollInterval(jobs *Jobs, next time.Time) time.Duration {
	if jobs != nil && jobs.Len() > 0 {
		return jobTickInterval
	}
	return disabledPollInterval
}

// disabledPollInterval 两类任务全禁用时的复查间隔。
const disabledPollInterval = 30 * time.Second

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻，末尾顺带跑一趟
// 各上游注册的「签到搭车」钩子（需要与签到时点成对执行的任务走这里）。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
//
// 旅行搭签到便车而非独立排程：每日上限按「派出」计 1 次/天且在派出时锁定奖励，
// 晚领不丢分，故分钟粒度巡检无增益，与签到时点（09/21 点）合并执行即可。
// 注意顺序：先签到解冻，旅行才能覆盖到本轮刚恢复的账号。
func (s *Scheduler) RunCheckinNow() {
	s.RunCheckin("schedule")
}

// RunCheckin 全量签到；trigger 为 "schedule" 或 "manual"（写入历史供区分）。
func (s *Scheduler) RunCheckin(trigger string) {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		s.checkinOne(st.UID, trigger)
	}
	// 签到收尾（09/21 点）：顺带推进一趟各上游的「搭车任务」（旅行状态机）。
	s.runCheckinHooks()
}

// CheckinResult 单账号签到结果，供管理台回显。
type CheckinResult struct {
	UID      string `json:"uid"`
	Status   string `json:"status"` // ok | already | fail
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits"`
	HasQuota bool   `json:"has_quota"`
}

// RunCheckinFor 对单个账号执行签到 + 余额刷新（管理台「单账号签到」入口）。
// 账号不存在时返回 ok=false；账号已禁用时按 fail 记录（不静默跳过，否则界面看不出为什么没反应）。
func (s *Scheduler) RunCheckinFor(uid, trigger string) (CheckinResult, bool) {
	if _, ok := s.cfg.Pool.Status(uid); !ok {
		return CheckinResult{UID: uid, Status: checkinlog.StatusFail, Detail: "账号不存在"}, false
	}
	return s.checkinOne(uid, trigger), true
}

// checkinOne 单账号签到：签到 → 查余额 → 解冻；结果写历史。
func (s *Scheduler) checkinOne(uid, trigger string) CheckinResult {
	res := CheckinResult{UID: uid}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		res.Status, res.Detail = checkinlog.StatusSkip, "无可用凭证"
		s.record(uid, checkinlog.KindCheckin, res.Status, res.Detail, 0, trigger)
		return res
	}

	status, detail := checkinlog.StatusOK, ""
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		log.Printf("checkin %s: %v", uid, err)
		// 已签到等业务错误也继续走余额查询
		if isAlreadyCheckin(err.Error()) {
			// detail 是给人在表格里看的一列，不是排障现场：这里放短句，
			// 上游 400 原文已经由上面的 log.Printf 进了进程日志。
			status, detail = checkinlog.StatusAlready, "今天已签到"
		} else {
			status, detail = checkinlog.StatusFail, shortErr(err)
		}
	}
	res.Status, res.Detail = status, detail

	remain, err := s.cfg.Upstream.UserResource(a)
	if err != nil {
		log.Printf("user-resource %s: %v", uid, err)
		s.record(uid, checkinlog.KindCheckin, res.Status, res.Detail, 0, trigger)
		return res
	}
	res.Credits, res.HasQuota = remain, true
	s.cfg.Pool.ReenableIfCredits(uid, remain)
	s.record(uid, checkinlog.KindCheckin, res.Status, res.Detail, remain, trigger)
	return res
}

// RefreshCredits 只查余额并同步进池（不签到），供管理台「刷新积分」。
func (s *Scheduler) RefreshCredits(uid, trigger string) (CheckinResult, bool) {
	if _, ok := s.cfg.Pool.Status(uid); !ok {
		return CheckinResult{UID: uid, Status: checkinlog.StatusFail, Detail: "账号不存在"}, false
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return CheckinResult{UID: uid, Status: checkinlog.StatusFail, Detail: "无可用凭证"}, true
	}
	res := CheckinResult{UID: uid, Status: checkinlog.StatusOK}
	remain, err := s.cfg.Upstream.UserResource(a)
	if err != nil {
		res.Status, res.Detail = checkinlog.StatusFail, shortErr(err)
		// 余额查询失败时补一句「上游怎么解释」。
		//
		// 为什么挂在这里而不是做成定时任务：实测该端点正常时**完全静默**
		// （三个账号都是 notifyCode=0、文案为空），平时查它只是多打一次上游。
		// 它唯一的价值是"出故障时给一句人话" —— 于是就该只在故障路径上查。
		// 失败不影响主流程：拿不到解释只是少一句话。
		if w := s.dosageHint(uid); w != "" {
			res.Detail += "；上游提示：" + w
		}
		s.record(uid, checkinlog.KindCredits, res.Status, res.Detail, 0, trigger)
		return res, true
	}
	res.Credits, res.HasQuota = remain, true
	s.cfg.Pool.SetCredits(uid, remain)
	s.cfg.Pool.ReenableIfCredits(uid, remain)
	s.record(uid, checkinlog.KindCredits, res.Status, "", remain, trigger)
	return res, true
}

// dosageHint 查询上游的额度告警文案，拿不到或没有告警时返回空串。
//
// 刻意返回 string 而不是 (*DosageWarning, error)：调用方只想要"一句话"，
// 而这里所有失败分支的正确行为都一样 —— 静默跳过。让调用方去判断
// nil/err/nil-err 三种情况只会把一条提示变成三个分支。
//
// 入参是 uid 而不是 *auth.Auth：这样不必在本文件 import auth 包，
// 且凭证在探测时刻从池里现取（余额查询刚失败，池里可能已标记该号状态）。
//
// defer recover 的考量：这是诊断路径，任何意外都不该把一次本该只是
// "余额查询失败"的操作升级成 panic。
func (s *Scheduler) dosageHint(uid string) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = ""
		}
	}()
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return ""
	}
	w, err := s.cfg.Upstream.DosageNotify(a)
	if err != nil || w == nil {
		return ""
	}
	// 走 shortErr 压行：这个文案要进历史文件，不能让上游的长文本撑爆一行。
	return shortErr(fmt.Errorf("%s", w.Message()))
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	s.RunKeepalive("schedule")
}

// RunKeepalive 全量 token 保活；trigger 为 "schedule" 或 "manual"。
func (s *Scheduler) RunKeepalive(trigger string) {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		s.keepaliveOne(st.UID, trigger)
	}
}

// RunKeepaliveFor 单账号 token 刷新（管理台入口）。
func (s *Scheduler) RunKeepaliveFor(uid, trigger string) (CheckinResult, bool) {
	if _, ok := s.cfg.Pool.Status(uid); !ok {
		return CheckinResult{UID: uid, Status: checkinlog.StatusFail, Detail: "账号不存在"}, false
	}
	return s.keepaliveOne(uid, trigger), true
}

func (s *Scheduler) keepaliveOne(uid, trigger string) CheckinResult {
	res := CheckinResult{UID: uid}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		res.Status, res.Detail = checkinlog.StatusSkip, "无可用凭证"
		s.record(uid, checkinlog.KindKeepalive, res.Status, res.Detail, 0, trigger)
		return res
	}
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("keepalive %s: %v", uid, err)
		res.Status, res.Detail = checkinlog.StatusFail, shortErr(err)
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			s.cfg.Pool.Disable(uid, "12153 session dead")
			res.Detail = "session dead，已禁用"
		}
		s.record(uid, checkinlog.KindKeepalive, res.Status, res.Detail, 0, trigger)
		return res
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("keepalive %s save: %v", uid, err)
		res.Detail = "刷新成功但落盘失败: " + shortErr(err)
	}
	res.Status = checkinlog.StatusOK
	s.record(uid, checkinlog.KindKeepalive, res.Status, res.Detail, 0, trigger)
	return res
}

// ---------------------------------------------------------------------------
// 历史记录辅助
// ---------------------------------------------------------------------------

// record 写一条任务历史（未注入 Log 时静默丢弃）。
func (s *Scheduler) record(uid, kind, status, detail string, credits int64, trigger string) {
	if s.cfg.Log == nil {
		return
	}
	nick := ""
	if a := s.cfg.Pool.AuthByUID(uid); a != nil {
		nick = a.Nickname
	}
	s.cfg.Log.Append(checkinlog.Record{
		At:       time.Now(),
		UID:      checkinlog.NormalizeUID(uid),
		Nickname: nick,
		Kind:     kind,
		Status:   status,
		Detail:   detail,
		Credits:  credits,
		Trigger:  trigger,
	})
}

// isAlreadyCheckin 判定「今日已签到」这类业务错误（上游返回 code!=0）。
func isAlreadyCheckin(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

// shortErr 把错误压成一行短文本，避免历史文件被长堆栈撑爆。
//
// 截断按**字符边界**退让，不直接切字节：上游的额度告警是中文（3 字节/字符），
// 按字节切 120 极易落在字符中间，产生非法 UTF-8 —— 实测 6 组样本里 3 组中招，
// 序列化进 JSON 后变成 \ufffd（"�"），用户看到的就是乱码尾巴。
//
// 这个值会经 res.Detail 落进签到历史，所以必须干净。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	// 换行与回车都压平：只替 \n 会留下裸 \r（"a\r\nb" → "a\r b"），
	// 而 \r 进落盘文件会让行式解析器/编辑器把一行当两行。
	s := strings.ReplaceAll(err.Error(), "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	const max = 120
	if len(s) <= max {
		return s
	}
	// 从 max 往前退，直到前缀是合法 UTF-8。最多退 3 字节（UTF-8 单字符上限）。
	n := max
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}
