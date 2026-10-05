// dailycheckin.go trae 的**跨上游全量签到**入口（gateway.DailyCheckinExt）。
//
// # 与 handleCheckinAll 的关系
//
// POST /admin/trae/checkin/all（分组头那个「全部签到」按钮）与本接口
// 是**同一个动作**的两个入口。所以两者必须落在同一个函数上 ——
// 各写一份遍历就会出现"点分组按钮和点顶部按钮结果不同"，
// 而那种差异没有任何测试会发现。
//
// 本文件把那段遍历抽成 checkinAllOnce，两条入口都调它。
package trae

import (
	"context"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// checkinAllOnce 对本上游全部账号各签一次，返回逐账号结果。
//
// trigger 由调用方给：手动按钮是 "manual-all"，核心的跨上游入口是
// "manual"（沿用既有取值，历史表按它区分来源）。
func (p *Provider) checkinAllOnce(ctx context.Context, trigger string) []gateway.DailyCheckinResult {
	out := make([]gateway.DailyCheckinResult, 0)
	if p == nil {
		return out
	}
	for _, a := range p.accounts() {
		if ctx.Err() != nil {
			break
		}
		res := p.checkinOne(ctx, a, trigger)
		out = append(out, gateway.DailyCheckinResult{
			UID:    res.uid,
			Status: res.status,
			Detail: res.detail,
		})
	}
	return out
}

// checkinOneResult 一个账号的签到结果（内部形状）。
//
// gateway.DailyCheckinResult 是**对外**契约（核心汇总用），
// 本包内部还要 credits 用于 HTTP 回执 —— 两边形状不同，
// 所以在出口处转换，而不是让内部流程迁就对外契约。
type checkinOneResult struct {
	uid     string
	status  string
	detail  string
	credits int64
}

// checkinOne 签一个账号并写历史（**唯一**的实现）。
//
// handleCheckin（单账号）与 checkinAllOnce（全量）共用它 ——
// 历史上 trae 就栽在"签到不写历史"上，共用一个函数可让
// "记得写历史"只在一处成立。
//
// # ⚠ 判据必须是「已核实到账」，不是「HTTP 通了」
//
// 这个函数曾经的写法是 `if err := CheckinClaim(...); err != nil` ——
// 而 claim 的业务失败是 **HTTP 200 + `code != 0`**（实测 9074
// "当前参与用户太多"连续 8/8 次都返回 200）。于是：
//
//	上游拒绝 → err == nil → 记 StatusOK → 界面"签到成功"、历史 `ok`
//	而**积分一分没到账** ⇒ 用户报的「签到不了」
//
// 现在两重校验：claim 的业务 code，以及**复查 status 的 checked_in**。
// 参照实现（caigee-cmd/cli2api）的结论是后者必需 —— 存在 code=0
// 但没真到账的情形。
func (p *Provider) checkinOne(ctx context.Context, a accountRef, trigger string) checkinOneResult {
	res := checkinOneResult{uid: a.UID}
	checkedIn, credits, extra, enable, err := p.client.CheckinStatus(ctx, a.Auth)
	if err != nil {
		res.status, res.detail = checkinlog.StatusFail, shortErr(err)
		p.recordCheckin(a.UID, res.status, res.detail, 0, trigger)
		return res
	}
	// ⚠ 活动关闭时**如实说**，不要继续去 claim。
	//
	// 旧版把 enable 丢掉了（`_`），于是"上游关闭签到"会走到 claim、
	// 拿一个业务错误、再被记成"签到成功"。
	if !enable {
		res.status, res.detail = checkinlog.StatusSkip, "上游当前未开启签到活动"
		p.recordCheckin(a.UID, res.status, res.detail, 0, trigger)
		return res
	}
	if checkedIn {
		res.status, res.detail, res.credits = checkinlog.StatusAlready, "今天已签到", credits+extra
		p.recordCheckin(a.UID, res.status, res.detail, res.credits, trigger)
		return res
	}

	claim, cerr := p.client.CheckinClaim(ctx, a.Auth)
	if cerr != nil {
		res.status, res.detail = checkinlog.StatusFail, shortErr(cerr)
		p.recordCheckin(a.UID, res.status, res.detail, 0, trigger)
		return res
	}
	if !claim.Confirmed {
		// code=0 但复查没看到到账 —— 不谎报成功。
		res.status, res.detail = checkinlog.StatusFail, "上游未确认到账（可能限流，稍后重试）"
		p.recordCheckin(a.UID, res.status, res.detail, 0, trigger)
		return res
	}
	res.status, res.credits = checkinlog.StatusOK, claim.Credits
	p.recordCheckin(a.UID, res.status, "", res.credits, trigger)
	return res
}

// CheckinAll gateway.DailyCheckinExt：核心触发"所有上游签到"时调它。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	if p == nil {
		return gateway.DailyCheckinReport{Results: []gateway.DailyCheckinResult{}}
	}
	// 给整趟一个总超时：核心会串行跑 N 个上游，一个上游卡住不该让
	// 后面的全都拿不到结果（与单账号端点的 30s 是两件事，这里账号更多）。
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return gateway.DailyCheckinReport{Results: p.checkinAllOnce(ctx, "manual")}
}

// 编译期断言：Provider 实现了跨上游全量签到的扩展点。
var _ gateway.DailyCheckinExt = (*Provider)(nil)
