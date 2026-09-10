// Package scheduler 定时任务：每日签到（09/21点，末尾顺带派猫/领奖）+ token keepalive（22点）。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即两类任务都启用，
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	KeepaliveHours []int // 默认 [22]

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点，搭签到便车的猫猫旅行也随之停摆。
	CheckinDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool

	// Log 任务结果历史（可选；nil = 不记录）。管理台的「今日签到了吗」也读它。
	Log *checkinlog.Log
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// checkinOverride/keepaliveOverride 运行时开关（管理台用）。
	// 用 *bool 而非 bool：零值表示「未覆盖」，此时回落到 cfg 的初始值——
	// 这样直接构造 &Scheduler{cfg: ...}（既有测试的写法）行为完全不变。
	checkinOverride   *bool
	keepaliveOverride *bool
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	return &Scheduler{cfg: cfg, adoptTried: make(map[string]string)}
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
	c := append([]int(nil), s.cfg.CheckinHours...)
	k := append([]int(nil), s.cfg.KeepaliveHours...)
	return c, k
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
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if s.KeepaliveEnabled() {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
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
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 两类任务全部禁用：不空转，但也不能永久阻塞——否则管理台在运行时
			// 重新打开开关后，没有任何东西能唤醒这个循环。改为低频复查。
			select {
			case <-ctx.Done():
				return
			case <-time.After(disabledPollInterval):
				continue
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
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

// disabledPollInterval 两类任务全禁用时的复查间隔。
const disabledPollInterval = 30 * time.Second

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻，末尾顺带跑一趟猫猫旅行。
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
	// 签到收尾（09/21 点）：顺带推进一趟旅行状态机（领养 / 派出 / 领奖）。
	s.RunTravelNow()
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
		s.record(uid, checkinlog.KindCredits, res.Status, res.Detail, 0, trigger)
		return res, true
	}
	res.Credits, res.HasQuota = remain, true
	s.cfg.Pool.SetCredits(uid, remain)
	s.cfg.Pool.ReenableIfCredits(uid, remain)
	s.record(uid, checkinlog.KindCredits, res.Status, "", remain, trigger)
	return res, true
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
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
