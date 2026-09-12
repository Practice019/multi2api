// checkin.go workbuddy 的**账号级定时业务**：签到、保活、余额刷新。
//
// # 为什么它们属于本包而不是核心调度器
//
// 改造前这三段逻辑长在 internal/scheduler 里（核心包），于是核心被迫知道：
//
//	s.cfg.Upstream.DailyCheckin(a)     ← 打哪个上游端点
//	isAlreadyCheckin(msg)              ← 上游的中文错误文案「已签到」
//	ReenableIfCredits(uid, remain)     ← "余额 > 0 就能用"这条上游规则
//
// 这三条全是 workbuddy 的业务。核心调度器只该知道"有个槽位到点了，
// 喊一声谁去干活" —— 而"干活"这件事现在在本文件里。
//
// # 与核心的关系
//
// 本包不得 import scheduler（架构约束）。方向是：
//
//	核心 → 本包：scheduler.SlotRunner.RunSlot(name, trigger)
//	本包 → 核心：把执行结果交给 coreService（消费方接口，见下）写历史
//
// 接线在 cmd/server：它把 *scheduler.Scheduler 适配成 coreService 注入进来。
package workbuddy

import (
	"errors"
	"log"
	"strings"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/upstream"
)

// 槽位名。由 cmd/server 装配时用同一组常量（它们是对外契约的一部分：
// /admin/schedule 的响应里出现这些名字，前端据此取字段）。
const (
	// SlotCheckin 签到槽位。
	SlotCheckin = "checkin"
	// SlotKeepalive token 保活槽位。
	SlotKeepalive = "keepalive"
)

// coreService 核心调度器在本包看来是什么样（本包需要它做的一件事）。
//
// # 为什么这里只需要"记历史"
//
// 签到/保活的结果要进统一的观测面（管理台的历史表）。历史由核心落库
// （跨上游统一的格式），本包只提供"这一条是什么"。
//
// Nickname 补全也留在核心：上游拿不到"昵称"这个展示字段的权威来源
// （它在账号池里）。所以本包交出去的这条不含 Nickname，核心落库时补。
type coreService interface {
	// Record 写一条任务历史（未注入 Log 时核心静默丢弃）。
	Record(uid, kind, status, detail string, credits int64, trigger string)
}

// CheckinOutcome 单账号签到/保活结果。
//
// 与 scheduler.TaskResult 逐字段对应（含 json tag）：
// 两边各自声明是为了让本包不 import scheduler，转换由 cmd/server 的适配器做，
// 与新上游接入时的做法一致（见 cmd/server/upstream_business.go）。
type CheckinOutcome struct {
	UID      string `json:"uid"`
	Status   string `json:"status"` // ok | already | fail | skip
	Detail   string `json:"detail,omitempty"`
	Credits  int64  `json:"credits"`
	HasQuota bool   `json:"has_quota"`
}

// core 取核心服务的消费方视图（未接线时返回 nil）。
func (p *Provider) coreService() coreService {
	if p == nil {
		return nil
	}
	return p.cfg.Core
}

// record 写一条任务历史：优先经核心（跨上游统一），未接线时退回本地日志。
func (p *Provider) recordViaCore(uid, kind, status, detail string, credits int64, trigger string) {
	if c := p.coreService(); c != nil {
		c.Record(uid, kind, status, detail, credits, trigger)
		return
	}
	p.record(uid, kind, status, detail, credits, trigger)
}

// ---------------------------------------------------------------------------
// SlotRunner 实现：核心到点后喊一声，本包决定"这个名字对应哪段业务"
// ---------------------------------------------------------------------------

// RunSlot 对全部账号执行一次某槽位的动作。
//
// # 为什么本包要认得出槽位名
//
// 核心刻意不认识 "checkin"/"keepalive" 是什么（它只转发名字）。
// 认名字这件事发生在**本包**：这是 workbuddy 自己的任务清单。
// 加第二个上游时，它的 JobExt/SlotRunner 认它自己的名字，核心零改动。
func (p *Provider) RunSlot(name, trigger string) {
	switch name {
	case SlotCheckin:
		p.RunCheckinAll(trigger)
	case SlotKeepalive:
		p.RunKeepaliveAll(trigger)
	default:
		// 核心给了本包不认识的槽位名：记日志但不猜（猜错会打错上游端点）。
		log.Printf("workbuddy: 未知槽位 %s，已跳过", name)
	}
}

// RunSlotFor 对单个账号执行某槽位的动作。
//
// 账号不存在时 ok=false（与改造前的 RunCheckinFor/RunKeepaliveFor 一致：
// 管理台据此回 404）。**未接线的槽位同样返回 ok=false** —— 改造前
// "调度器未接线"走的就是这条路（单账号 404）。
func (p *Provider) RunSlotFor(name, uid, trigger string) (CheckinOutcome, bool) {
	switch name {
	case SlotCheckin:
		return p.RunCheckinFor(uid, trigger)
	case SlotKeepalive:
		return p.RunKeepaliveFor(uid, trigger)
	default:
		return CheckinOutcome{UID: uid, Status: statusFail, Detail: "未知任务: " + name}, false
	}
}

// ---------------------------------------------------------------------------
// 签到
// ---------------------------------------------------------------------------

// RunCheckinAll 全量签到。
//
// 顺序是刻意的：**先逐账号签到（含解冻），再跑搭车任务**。
// 搭车的旅行守卫需要看到本轮刚被解冻的账号，晚跑才能覆盖到它们。
// 钩子的触发由核心负责（scheduler.RunSlot 在本方法之后统一喊），
// 所以这里只管账号本身。
//
// ⚠ 账号集必须来自 ownAccounts（本上游）而不是 Pool.List（全池）：
// 这里正是"自动调度给别家上游账号跑签到"的那条路径 ——
// 池是全上游共用的，遍历全池会让本上游的任务作用在 codearts 的号上。
func (p *Provider) RunCheckinAll(trigger string) {
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		p.checkinOne(st.UID, trigger)
	}
}

// RunCheckinFor 单账号签到（管理台「单账号签到」入口）。
//
// 账号不存在时 ok=false；账号已禁用时按 fail 记录（不静默跳过，
// 否则界面看不出为什么没反应 —— 与改造前逐字一致）。
func (p *Provider) RunCheckinFor(uid, trigger string) (CheckinOutcome, bool) {
	if !p.accountExists(uid) {
		return CheckinOutcome{UID: uid, Status: checkinlog.StatusFail, Detail: "账号不存在"}, false
	}
	return p.checkinOne(uid, trigger), true
}

// accountExists 账号池里有没有这个号（池未接线时一律"不存在"）。
func (p *Provider) accountExists(uid string) bool {
	if p.cfg.Pool == nil {
		return false
	}
	return p.cfg.Pool.Has(uid)
}

// checkinOne 单账号签到：签到 → 查余额 → 解冻；结果写历史。
func (p *Provider) checkinOne(uid, trigger string) CheckinOutcome {
	res := CheckinOutcome{UID: uid}
	if p.cfg.Pool == nil {
		res.Status, res.Detail = checkinlog.StatusSkip, "无可用凭证"
		p.recordViaCore(uid, checkinlog.KindCheckin, res.Status, res.Detail, 0, trigger)
		return res
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		res.Status, res.Detail = checkinlog.StatusSkip, "无可用凭证"
		p.recordViaCore(uid, checkinlog.KindCheckin, res.Status, res.Detail, 0, trigger)
		return res
	}

	status, detail := checkinlog.StatusOK, ""
	if err := p.client.DailyCheckin(a); err != nil {
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

	remain, err := p.client.UserResource(a)
	if err != nil {
		log.Printf("user-resource %s: %v", uid, err)
		p.recordViaCore(uid, checkinlog.KindCheckin, res.Status, res.Detail, 0, trigger)
		return res
	}
	res.Credits, res.HasQuota = remain, true
	// 「能不能用」由本包判断：余额查到了且 > 0。核心不再内置这条规则。
	p.cfg.Pool.ReenableIfUsable(uid, remain > 0, FromCredits(remain))
	p.recordViaCore(uid, checkinlog.KindCheckin, res.Status, res.Detail, remain, trigger)
	return res
}

// isAlreadyCheckin 判定「今日已签到」这类业务错误（上游返回 code!=0）。
//
// # 为什么这条必须在上游包里
//
// 它匹配的是**腾讯上游的中文错误文案**与它的私有错误码格式。
// 改造前它在核心调度器里，于是核心认识 "已签到" 三个字 —— 换一个上游
// 就完全不成立（codearts 没有签到）。现在它跟着业务一起在本包。
//
// 判据刻意宽松（"already"/"checkin" 也认）：上游的措辞在不同版本里变过，
// 宁可把"确实已签到"认成 already（结果正确），也不要把"已签到"
// 误判成 fail（用户会看到一条假故障）。
func isAlreadyCheckin(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

// ---------------------------------------------------------------------------
// 保活
// ---------------------------------------------------------------------------

// RunKeepaliveAll 全量 token 保活；trigger 为 "schedule" 或 "manual"。
//
// ⚠ 同 RunCheckinAll：账号集必须按上游取。这里也是调度路径 ——
// 实测那批"别家上游账号被 schedule 触发"的记录里，保活占 110 条。
func (p *Provider) RunKeepaliveAll(trigger string) {
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		p.keepaliveOne(st.UID, trigger)
	}
}

// RunKeepaliveFor 单账号 token 刷新（管理台入口）。
func (p *Provider) RunKeepaliveFor(uid, trigger string) (CheckinOutcome, bool) {
	if !p.accountExists(uid) {
		return CheckinOutcome{UID: uid, Status: checkinlog.StatusFail, Detail: "账号不存在"}, false
	}
	return p.keepaliveOne(uid, trigger), true
}

// keepaliveOne 单账号 token 刷新；session 死亡的自动禁用。
func (p *Provider) keepaliveOne(uid, trigger string) CheckinOutcome {
	res := CheckinOutcome{UID: uid}
	if p.cfg.Pool == nil {
		res.Status, res.Detail = checkinlog.StatusSkip, "无可用凭证"
		p.recordViaCore(uid, checkinlog.KindKeepalive, res.Status, res.Detail, 0, trigger)
		return res
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		res.Status, res.Detail = checkinlog.StatusSkip, "无可用凭证"
		p.recordViaCore(uid, checkinlog.KindKeepalive, res.Status, res.Detail, 0, trigger)
		return res
	}
	if err := p.client.RefreshToken(a); err != nil {
		log.Printf("keepalive %s: %v", uid, err)
		res.Status, res.Detail = checkinlog.StatusFail, shortErr(err)
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			p.cfg.Pool.Disable(uid, "12153 session dead")
			res.Detail = "session dead，已禁用"
		}
		p.recordViaCore(uid, checkinlog.KindKeepalive, res.Status, res.Detail, 0, trigger)
		return res
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("keepalive %s save: %v", uid, err)
		res.Detail = "刷新成功但落盘失败: " + shortErr(err)
	}
	res.Status = checkinlog.StatusOK
	p.recordViaCore(uid, checkinlog.KindKeepalive, res.Status, res.Detail, 0, trigger)
	return res
}

const statusFail = "fail"
