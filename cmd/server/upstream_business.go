// upstream_business.go 把 workbuddy 的业务能力适配成 admin 的消费方接口。
//
// # 为什么需要这一层薄适配
//
// 两边的类型结构完全相同，但**不能共用同一个定义**：
//   - workbuddy 不能 import admin（架构约束：上游不得依赖核心包）
//   - admin 不能 import workbuddy（否则加新上游时 admin 要改）
//
// 所以各自声明，转换只在这一个地方做。这与 main.go 里 modelCatalogState
// 的做法完全一致（server 与 admin 各有一份结构相同的目录状态类型）。
//
// 适配器很薄：每个方法只做一次逐字段转换，没有逻辑。
// 它存在的意义是让"两侧的类型各自独立演化"，代价是一处编译期检查的转换。
package main

import (
	"encoding/json"
	"errors"
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/clientlogin"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/workbuddy"
)

// ---------------------------------------------------------------------------
// 调度器适配器（Task 3c）
// ---------------------------------------------------------------------------

// schedulerAdapter 把 *scheduler.Scheduler 适配成 workbuddy 的三条消费方接口：
//
//	workbuddy.CheckinRunner   签到/保活（/admin/checkin, /admin/keepalive）
//	workbuddy.SchedulerView   时点与启停（/admin/schedule）
//
// # 为什么需要转换
//
// 两边的语义一一对应，但**类型不能共用**：workbuddy 不得 import scheduler
// （架构约束）。RunCheckinFor 的返回类型在 scheduler 里是 CheckinResult、
// 在 workbuddy 里是 CheckinOutcome —— 结构相同、定义各自独立，
// 转换只在这一个地方做（与 modelCatalogState、businessAdapter 同一模式）。
//
// 其余方法签名完全一致，直接转发。
type schedulerAdapter struct{ sch *scheduler.Scheduler }

// ---- workbuddy.CheckinRunner ----

func (a schedulerAdapter) RunCheckinFor(uid, trigger string) (workbuddy.CheckinOutcome, bool) {
	res, ok := a.sch.RunCheckinFor(uid, trigger)
	return toCheckinOutcome(res), ok
}

func (a schedulerAdapter) RunKeepaliveFor(uid, trigger string) (workbuddy.CheckinOutcome, bool) {
	res, ok := a.sch.RunKeepaliveFor(uid, trigger)
	return toCheckinOutcome(res), ok
}

// ---- workbuddy.SchedulerView ----

func (a schedulerAdapter) NextWake() (time.Time, []string) { return a.sch.NextWake() }
func (a schedulerAdapter) Hours() ([]int, []int)           { return a.sch.Hours() }
func (a schedulerAdapter) CheckinEnabled() bool            { return a.sch.CheckinEnabled() }
func (a schedulerAdapter) KeepaliveEnabled() bool          { return a.sch.KeepaliveEnabled() }

// toCheckinOutcome 逐字段转换（含 HasQuota —— 它是 /admin/checkin 单账号
// 响应体的一部分，漏掉会改变对外 JSON）。
func toCheckinOutcome(r scheduler.CheckinResult) workbuddy.CheckinOutcome {
	return workbuddy.CheckinOutcome{
		UID: r.UID, Status: r.Status, Detail: r.Detail,
		Credits: r.Credits, HasQuota: r.HasQuota,
	}
}

var (
	_ workbuddy.CheckinRunner = schedulerAdapter{}
	_ workbuddy.SchedulerView = schedulerAdapter{}
	_ workbuddy.AdminEnv      = workbuddy.AdminEnv{}
)

// ---------------------------------------------------------------------------
// 任务槽适配器（Task 3c）
// ---------------------------------------------------------------------------

// 进程内唯一的生产任务槽。
//
// # 为什么是包级变量而不是调度器上的字段
//
// 改造前这个"同一时刻只允许一个全量任务"的槽长在 internal/admin 的
// Handler 上（h.task），而签到/保活/旅行/成长四条全量端点全部住在同一个
// admin 包里，天然共享它。搬进 workbuddy 之后仍然要共享同一个槽 ——
// 但它现在必须同时能被**签到/保活**（也在 workbuddy，走同一个 h.task）
// 以及**将来的第三个上游**看见。
//
// 用包级单例最简单且语义准确：这个槽约束的是"本进程同一时刻只跑一个
// 全量维护任务"，本来就该是进程级的，而不是每个上游一份。
var sharedTaskSlot = workbuddy.NewTaskSlot()

// newTaskSlotAdapter 返回注入给上游的任务槽。
func newTaskSlotAdapter(sch *scheduler.Scheduler) workbuddy.TaskSlot {
	// sch 目前不承担任务槽（核心调度器的整点任务没有"已在执行中就 409"的语义，
	// 它由循环自己串行）。保留参数是为了让将来"核心也想暴露在途状态"时
	// 不必再改调用点。
	_ = sch
	return sharedTaskSlot
}

// ---------------------------------------------------------------------------
// 客户端登录态适配器（Task 3c）
// ---------------------------------------------------------------------------

// clientLoginAdapter 把 *clientlogin.Manager 适配成 workbuddy.ClientLoginManager。
//
// 三个方法直接转发，但两个返回结构要**逐字段**搬运：它们的 json tag 是对外契约，
// 上游包各自声明了一份同形结构（它不得 import clientlogin）。
type clientLoginAdapter struct{ m *clientlogin.Manager }

func (a clientLoginAdapter) Status() (*workbuddy.ClientLoginStatus, error) {
	st, err := a.m.Status()
	if err != nil {
		return nil, err
	}
	out := &workbuddy.ClientLoginStatus{
		Enabled:       st.Enabled,
		ClientDir:     st.ClientDir,
		ArchiveDir:    st.ArchiveDir,
		ClientFile:    st.ClientFile,
		SnapshotFile:  st.SnapshotFile,
		ClientRunning: st.ClientRunning,
		HasBackup:     st.HasBackup,
		BackupUID:     st.BackupUID,
		BackupNick:    st.BackupNick,
		BackupAt:      st.BackupAt,
		Error:         st.Error,
	}
	if st.Current != nil {
		c := toClientLoginCandidate(*st.Current)
		out.Current = &c
	}
	if len(st.Candidates) > 0 {
		out.Candidates = make([]workbuddy.ClientLoginCandidate, 0, len(st.Candidates))
		for _, c := range st.Candidates {
			out.Candidates = append(out.Candidates, toClientLoginCandidate(c))
		}
	}
	return out, nil
}

func toClientLoginCandidate(c clientlogin.Candidate) workbuddy.ClientLoginCandidate {
	return workbuddy.ClientLoginCandidate{
		UID: c.UID, Nickname: c.Nickname, Source: c.Source,
		ExpiresAt: c.ExpiresAt, ExpiresAtText: c.ExpiresAtText,
		Valid: c.Valid, Current: c.Current, Restorable: c.Restorable,
		TokenHint: c.TokenHint,
	}
}

func (a clientLoginAdapter) Switch(uid string) (*workbuddy.ClientLoginSwitchResult, error) {
	res, err := a.m.Switch(uid)
	if err != nil {
		return nil, translateClientLoginErr(err)
	}
	return toClientLoginResult(res), nil
}

func (a clientLoginAdapter) Restore() (*workbuddy.ClientLoginSwitchResult, error) {
	res, err := a.m.Restore()
	if err != nil {
		return nil, translateClientLoginErr(err)
	}
	return toClientLoginResult(res), nil
}

// translateClientLoginErr 把 clientlogin 的哨兵错误映射成 workbuddy 的同义错误。
//
// 为什么必须映射而不是直接返回：上游包用 errors.Is 判定这三类"预期失败"
// （同号 / 无意义回滚 / 客户端在跑），而两边是**各自声明**的哨兵值 ——
// 直接透传会让 errors.Is 失配，三种情况全部落到 default 分支，
// 变成"切换失败: ..."的 409，错误文案就与改造前不一致了。
func translateClientLoginErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, clientlogin.ErrSameAccount):
		return workbuddy.ErrSameAccount
	case errors.Is(err, clientlogin.ErrAlreadyBackedUp):
		return workbuddy.ErrAlreadyBackedUp
	case errors.Is(err, clientlogin.ErrClientRunning):
		return workbuddy.ErrClientRunning
	default:
		return err
	}
}

func toClientLoginResult(r *clientlogin.SwitchResult) *workbuddy.ClientLoginSwitchResult {
	if r == nil {
		return nil
	}
	return &workbuddy.ClientLoginSwitchResult{
		UID: r.UID, Nickname: r.Nickname, Source: r.Source,
		Changed: r.Changed, Message: r.Message, BackupPath: r.BackupPath,
	}
}

var _ workbuddy.ClientLoginManager = clientLoginAdapter{}

// ---------------------------------------------------------------------------
// 账号池适配器
// ---------------------------------------------------------------------------

// poolAdapter 把 *pool.Pool 适配成 workbuddy.AccountPool。
//
// 只有 List 需要转换（pool.Status → workbuddy.Account）；其余方法签名完全一致，
// 直接转发。放在 cmd/server 是因为这里是唯一同时认识两侧的地方。
type poolAdapter struct{ p *pool.Pool }

func (a poolAdapter) List() []workbuddy.Account {
	src := a.p.List()
	out := make([]workbuddy.Account, 0, len(src))
	for _, st := range src {
		out = append(out, workbuddy.Account{
			UID:      st.UID,
			Nickname: st.Nickname,
			Disabled: st.Disabled,
			Cooling:  st.Cooling,
		})
	}
	return out
}

func (a poolAdapter) AuthByUID(uid string) *auth.Auth { return a.p.AuthByUID(uid) }

func (a poolAdapter) Has(uid string) bool {
	_, ok := a.p.Status(uid)
	return ok
}

func (a poolAdapter) SetCredits(uid string, credits int64) { a.p.SetCredits(uid, credits) }

func (a poolAdapter) ReenableIfCredits(uid string, remain int64) {
	a.p.ReenableIfCredits(uid, remain)
}

func (a poolAdapter) Disable(uid, reason string) { a.p.Disable(uid, reason) }

var _ workbuddy.AccountPool = poolAdapter{}

// ---------------------------------------------------------------------------
// 上游设置适配器（Task 3c）
// ---------------------------------------------------------------------------

// upstreamSettingsAdapter 把 workbuddy 的设置项接进 admin 的设置页与宿主持久化。
//
// # 它同时满足两个接口
//
//	admin.SettingsExt      Fields() / ApplySettings()   —— 让设置页显示与写入
//	main.UpstreamSettings  Apply()  / Values()          —— 让宿主落盘
//
// 两个接口的形状不同是刻意的：admin 要的是"键 + 值 + 是否需重启"（渲染需要分类），
// 宿主落盘要的是"键 → 当前值"的 map（写配置只要值）。
// 让一个适配器同时满足两者，比在上游包里实现两套更省 —— 上游只提供一份 Fields()。
//
// # 为什么要 applyToMemory
//
// 宿主在保存后会调 applyToMemory 把新值同步进内存 cfg，供下一轮 Snapshot 回显。
// 那段代码原本在 settings_store.go 里，逐行写着 "growth_auto_open" 之类的键名 ——
// 那是核心认识上游字段的最后一处。现在改由本适配器按自己的键表同步。
type upstreamSettingsAdapter struct {
	p   *workbuddy.Provider
	cfg *Config
}

func newUpstreamSettingsAdapter(p *workbuddy.Provider, cfg *Config) *upstreamSettingsAdapter {
	return &upstreamSettingsAdapter{p: p, cfg: cfg}
}

// ---- admin.SettingsExt ----

func (a *upstreamSettingsAdapter) Fields() []admin.SettingField {
	src := a.p.SettingsFields()
	out := make([]admin.SettingField, 0, len(src))
	for _, f := range src {
		out = append(out, admin.SettingField{
			Key: f.Key, Value: f.Value, RestartRequired: f.RestartRequired,
		})
	}
	return out
}

func (a *upstreamSettingsAdapter) ApplySettings(patch json.RawMessage) ([]string, error) {
	return a.p.ApplySettings(patch)
}

// ---- main.UpstreamSettings ----

func (a *upstreamSettingsAdapter) Apply(patch json.RawMessage) ([]string, error) {
	return a.p.ApplySettings(patch)
}

func (a *upstreamSettingsAdapter) Values() map[string]any {
	out := map[string]any{}
	for _, f := range a.p.SettingsFields() {
		out[f.Key] = f.Value
	}
	return out
}

// applyToMemory 把刚保存的上游设置同步进内存 cfg（供 Snapshot 回显）。
//
// 与 settings_store.go 里通用段的同名方法分工：那里管签到/超时/限流，
// 这里管 workbuddy 的成长/旅行开关。核心不认识下面这些键名 ——
// 它们只在**上游适配器**这一层出现，而上游包自己才知道它们的含义。
func (a *upstreamSettingsAdapter) applyToMemory(values map[string]any) {
	c := a.cfg
	if v, ok := values["travel_auto_claim"].(bool); ok {
		travelAuto := v
		c.Admin.TravelAutoClaim = &travelAuto
	}
	if v, ok := intValue(values["travel_watch_interval_seconds"]); ok {
		c.Admin.TravelWatchIntervalSeconds = v
	}
	if v, ok := values["growth_auto_claim"].(bool); ok {
		c.GrowthAutoClaim = v
	}
	if v, ok := values["growth_auto_accept_tasks"].(bool); ok {
		c.GrowthAutoAccept = v
	}
	if v, ok := values["growth_auto_makeup"].(bool); ok {
		c.GrowthAutoMakeup = v
	}
	if v, ok := values["growth_auto_redeem"].(bool); ok {
		c.GrowthAutoRedeem = v
	}
	if v, ok := values["growth_auto_open"].(bool); ok {
		c.GrowthAutoOpen = v
	}
	if v, ok := values["growth_auto_draw"].(bool); ok {
		c.GrowthAutoDraw = v
	}
	if v, ok := intValue(values["growth_watch_interval_seconds"]); ok && v > 0 {
		c.Admin.GrowthWatchIntervalSeconds = v
	}
}

// intValue 把 any 收成 int（JSON 反序列化给的是 float64，我们写的是 int）。
func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

var (
	_ admin.SettingsExt = (*upstreamSettingsAdapter)(nil)
	_ UpstreamSettings  = (*upstreamSettingsAdapter)(nil)
)
