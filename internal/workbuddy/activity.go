// activity.go 对话活跃上报（workbuddy 侧）：对池内账号发 `chat_request_send` 事件。
//
// # 为什么它不属于"旅行"，也不属于"订阅"
//
// 它是**独立的一件事**：点亮连登（streak）+ 解锁 first_buddy 的领养前置。
// 改造前本仓库没有任何地方发这个事件，于是：
//
//	first_buddy 的门槛永远不满足
//	  → 猫猫旅行领养恒失败于 HTTP 400 "first_buddy task not completed yet"
//	  → 而 first_buddy 是成长计划其余 17 个任务的**前置**
//
// 结果就是"17 个任务全被 first_buddy 挡住"，且界面上看起来像是在正常等待。
//
// # 与旅行的关系：上报是旅行的**前置条件**，不是它的替代
//
// 所以 travelAdopt 之前会先补一次上报（见 travel.go 的 ensureAdoptPrereq）。
// 这一条在 B 分支上已实测：补上报后领养 +300 到账（3/3 账号）。
package workbuddy

import (
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/upstream"
)

// ActivityAccountDelay 账号间限速（与旅行同口径 800ms）。
//
// 上报本身很轻，但它是"每日一次"的动作，没必要为省几秒把账号堆在一个时间点上 ——
// 上游对同一秒内的批量同质请求更敏感。
var ActivityAccountDelay = 800 * time.Millisecond

// activityReported 记录每个账号**当日已上报**的日期（CST）。
//
// 内存态即可：上报是幂等的（服务端按天去重），重启后多报一次没有副作用，
// 只是多一个请求。为它落盘不划算 —— 而漏记会导致重启后重复上报，
// 那是无害方向。
type activityTracker struct {
	mu   sync.Mutex
	days map[string]string // uid -> YYYY-MM-DD (CST)
}

func newActivityTracker() *activityTracker {
	return &activityTracker{days: map[string]string{}}
}

// markIfNew 若该账号今天还没报过则标记并返回 true。
func (t *activityTracker) markIfNew(uid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	today := travelDay(time.Now())
	if t.days[uid] == today {
		return false
	}
	t.days[uid] = today
	return true
}

// ReportedToday 报告该账号今天是否已上报（供管理台/日志展示）。
func (p *Provider) ReportActivityReportedToday(uid string) bool {
	if p.activity == nil {
		return false
	}
	p.activity.mu.Lock()
	defer p.activity.mu.Unlock()
	return p.activity.days[uid] == travelDay(time.Now())
}

// ReportActivityFor 对一个账号发一条活跃上报（不判"今天报过没有"）。
//
// 返回 error 供调用方决定怎么记录；成功/失败都会打一行日志 ——
// 这个端点"200 静默丢弃"的特性使得**没有日志就等于没有观测**。
func (p *Provider) ReportActivityFor(a *auth.Auth) error {
	err := p.client.ReportChatActivity(a, upstream.NewReportConversationID(), "")
	if err != nil {
		log.Printf("activity %s: report failed: %v", a.UID, err)
		return err
	}
	log.Printf("activity %s: report ok", a.UID)
	return nil
}

// RunActivityNow 对池内所有可用账号做一轮活跃上报（定时任务入口）。
//
// 每号每天 1 次：日活跃奖励按天去重，重复上报没有额外收益，
// 只会给上游留下高频同质请求的画像。因此用 activityTracker 去重。
func (p *Provider) RunActivityNow() {
	p.runActivity(triggerSchedule)
	// 记下"今天跑过了"：定时任务的 Due 靠它去重（见 activityDueJob）。
	p.markActivityRan(time.Now())
}

// RunActivityManual 手动触发全量上报（管理台入口）。
//
// ⚠ 与定时入口的区别：手动**也**走去重。用户点这个按钮的意图通常是
// "我的号怎么还没解锁领养"，而重复上报解决不了这个问题 ——
// 让它跳过并记"跳过"比再发一遍更有信息量。
func (p *Provider) RunActivityManual() { p.runActivity(triggerManual) }

func (p *Provider) runActivity(trigger string) {
	if p.activity == nil {
		p.activity = newActivityTracker()
	}
	first := true
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if !first {
			time.Sleep(ActivityAccountDelay)
		}
		first = false
		p.activityOne(a, trigger)
	}
}

// activityOne 单账号一次上报（带当日去重与历史记录）。
func (p *Provider) activityOne(a *auth.Auth, trigger string) {
	if !p.activity.markIfNew(a.UID) {
		// 今天报过了：不重发，但留一条 skip 记录，便于回答
		// "为什么手动点了没反应"。
		p.record(a.UID, checkinlog.KindActivity, checkinlog.StatusSkip, "今日已上报", 0, trigger)
		return
	}
	if err := p.ReportActivityFor(a); err != nil {
		p.record(a.UID, checkinlog.KindActivity, checkinlog.StatusFail, "上报: "+shortErr(err), 0, trigger)
		return
	}
	p.record(a.UID, checkinlog.KindActivity, checkinlog.StatusOK, "已上报活跃", 0, trigger)
}

// ensureAdoptPrereq 确保领养前置已满足：先补一次活跃上报。
//
// # 为什么必须在 BuddyFirst 之前调用（这是"领养恒失败"的真因）
//
// 上游对 first_buddy 的门槛判定读的是**网关从未发过**的那个行为事件。
// 缺它时 BuddyFirst 返回 HTTP 400 "first_buddy task not completed yet"，
// 而改造前本仓库把这个 400 当作"上游的合理门槛"记一次跳过
// —— 于是每天试一次、每天失败一次，永远不会成功。
//
// B 分支已验证：补上报后领养 +300 到账（3/3 账号）。
//
// # 失败不阻塞领养
//
// 上报失败（网络/限流）时**仍然继续尝试领养**：如果该账号的门槛此前
// 已经被其它途径满足（例如用户自己在客户端里聊过），领养仍会成功。
// 反过来"上报失败就不领养"会白白浪费一次机会。
func (p *Provider) ensureAdoptPrereq(a *auth.Auth) {
	if err := p.ReportActivityFor(a); err != nil {
		log.Printf("travel %s: 领养前置上报失败（仍继续尝试领养）: %v", a.UID, err)
		return
	}
	if p.activity != nil {
		// 记为今日已报，避免同一趟里再报一次（runActivity 也会看这个表）。
		p.activity.markIfNew(a.UID)
	}
}
