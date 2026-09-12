// checkin.go workbuddy 在管理端点里需要的**调度器能力消费方视图**。
//
// # 为什么签到/保活还要经接口，而不是本包自己实现
//
// 签到与保活用的是**核心调度器**的判定与状态机（签到落历史、查余额、
// 解冻账号、保活的 token 刷新窗口），它不是 CodeBuddy 独有的业务语义 ——
// 换任何一个 OAuth 上游都长这样。Task 3c 只搬"谁来回答 HTTP"，
// 不搬"签到怎么回事"：后者留在 scheduler，由核心继续拥有。
//
// 但架构约束禁止本包 import scheduler，所以同样用消费方接口：
// 接口由本包声明，*scheduler.Scheduler 已经满足它（方法名与返回结构一致），
// 不需要核心做任何改动。
//
// # 关于 CheckinOutcome 的形状
//
// 它必须与 scheduler.CheckinResult 的 JSON 形状一致 ——
// 单账号签到/保活的响应体直接序列化它，任何字段名变化用户都看得见。
package workbuddy

// CheckinRunner 核心调度器的账号级动作在本包看来是什么样。
type CheckinRunner interface {
	// RunCheckinFor 单账号签到；账号不存在时 ok=false。
	RunCheckinFor(uid, trigger string) (CheckinOutcome, bool)
	// RunKeepaliveFor 单账号保活；账号不存在时 ok=false。
	RunKeepaliveFor(uid, trigger string) (CheckinOutcome, bool)
}

// CheckinOutcome 单账号签到/保活结果。
//
// 与 scheduler.CheckinResult 逐字段对应（含 json tag）：
// 两边各自声明是为了让本包不 import scheduler，转换由 cmd/server 的适配器做，
// 与新上游接入时的做法一致（见 cmd/server/upstream_business.go）。
type CheckinOutcome struct {
	UID      string `json:"uid"`
	Status   string `json:"status"` // ok | already | fail
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits"`
	HasQuota bool   `json:"has_quota"`
}

// CheckinRunnerOf 取本 Provider 的调度器视图。
//
// 未接线时返回 nil，签到/保活端点降级为 501（与改造前"未接线"的语义一致，
// 只是改造前没有单独的调度器视图概念）。
func (p *Provider) checkinRunner() CheckinRunner {
	if p == nil {
		return nil
	}
	return p.cfg.Checkin
}

// RunCheckinFor 单账号签到（调度器未接线时 ok=false）。
func (p *Provider) RunCheckinFor(uid, trigger string) (CheckinOutcome, bool) {
	if r := p.checkinRunner(); r != nil {
		return r.RunCheckinFor(uid, trigger)
	}
	return CheckinOutcome{UID: uid, Status: statusFail, Detail: "调度器未接线"}, false
}

// RunKeepaliveFor 单账号保活（调度器未接线时 ok=false）。
func (p *Provider) RunKeepaliveFor(uid, trigger string) (CheckinOutcome, bool) {
	if r := p.checkinRunner(); r != nil {
		return r.RunKeepaliveFor(uid, trigger)
	}
	return CheckinOutcome{UID: uid, Status: statusFail, Detail: "调度器未接线"}, false
}

const statusFail = "fail"
