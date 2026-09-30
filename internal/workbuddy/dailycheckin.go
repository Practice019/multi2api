// dailycheckin.go workbuddy 的**跨上游全量签到**入口（gateway.DailyCheckinExt）。
//
// # 为什么需要它（用户本轮要求）
//
//	"账号池水平的位置只放一个全部签到，是签到所有的上游
//	 也就是触发所有上游的全部签到"
//
// 以前「全部签到」是**前端写死**的一个按钮：它打 POST /admin/checkin，
// 而那条路由是 workbuddy 挂的 —— 两个上游时它的作用域就说不清了
// （codearts 的行也被它扫进去，而 codearts 根本没有这个能力位）。
//
// 现在分两级：
//
//	分组头「全部签到」  → 打本上游的 AllURL（只管自己那几个号）
//	顶部「全部签到」    → 打核心端点，由核心遍历**所有**上游各自的执行体
//
// 本文件是第二级在本上游的实现：核心不认识 workbuddy，只认识
// gateway.DailyCheckinExt 这个接口。
//
// # ⚠ 必须与 AllURL 落在**同一个函数**上
//
// DailyAction 报的 AllURL = /admin/checkin（不带 uid 就是全量），
// 它走 adminendpoints.go 的 runCheckinAll() → RunCheckinAll("manual")。
// 这里**复用同一个** RunCheckinAll，不另写一份遍历 ——
// 另写一份就会出现"点分组按钮和点顶部按钮结果不同"，
// 而那种差异没有任何测试会发现（两个入口各绿各的）。
package workbuddy

import (
	"context"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// DailyCheckinAll 本上游一趟全量签到的**唯一**实现（供两处入口共用）。
//
// # 为什么抽成函数而不是写在 CheckinAll 里
//
// adminendpoints.runCheckinAll 要的是 `[]map[string]any`（既有 JSON 形状，
// 那边不能动），而 gateway.DailyCheckinReport 是新契约。两者的**业务**
// 必须是同一段，所以抽出来让两边都调它 —— 形状转换各管各的。
func (p *Provider) DailyCheckinAll(trigger string) []CheckinOutcome {
	out := make([]CheckinOutcome, 0)
	if p == nil {
		return out
	}
	for _, st := range p.ownAccounts() {
		if st.Disabled {
			continue
		}
		out = append(out, p.checkinOne(st.UID, trigger))
	}
	return out
}

// CheckinAll gateway.DailyCheckinExt：核心触发"所有上游签到"时调它。
//
// # trigger 为什么是 manual
//
// 它总是由**用户点按钮**触发（顶部「全部签到」）。自动那条路走
// JobExt/RunSlot，不经过这里 —— 两条路的标签必须分开，
// 否则历史表里"自动签到"会被记成手动，排障时无从分辨。
//
// # 取消语义
//
// 顶部按钮一次要跑 N 个上游，用户在途中再点（任务槽会拒绝）或关页面时
// ctx 会被取消。这里每个号查一次，尽早返回；已经签到过的号**保留结果**
// （它们的动作已经真实发生，抹掉等于谎报）。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	out := make([]gateway.DailyCheckinResult, 0)
	if p == nil {
		return gateway.DailyCheckinReport{Results: out}
	}
	// 玩法被裁剪的实例（海外版 workbuddy-intl）**没有**签到这回事：
	// 它的路由里没有 /admin/checkin，DailyActions 也不报签到动作。
	// 这里如实回空 —— 若照样遍历，它会拿本实例的账号去跑签到，
	// 而那些账号属于另一个上游（海外版没有签到玩法）。
	if p.PlayFeaturesDisabled() {
		return gateway.DailyCheckinReport{Results: out}
	}
	for _, st := range p.ownAccounts() {
		if ctx.Err() != nil {
			break
		}
		if st.Disabled {
			continue
		}
		res := p.checkinOne(st.UID, triggerManual)
		out = append(out, gateway.DailyCheckinResult{
			UID:    res.UID,
			Status: res.Status,
			Detail: res.Detail,
		})
	}
	return gateway.DailyCheckinReport{Results: out}
}

// 编译期断言：Provider 实现了跨上游全量签到的扩展点。
//
// 与 dailyactions.go 那条分开写（那是按钮契约、这是执行契约）：
// 少了这条，"顶部全部签到扫不到 workbuddy"不会有任何编译错误。
var _ gateway.DailyCheckinExt = (*Provider)(nil)

// 断言本文件用到的状态常量与 gateway 的字符串约定一致。
//
// checkinlog 是本包自己的依赖，gateway 刻意不引用它 —— 所以"两边说的
// ok/already/fail/skip 是不是同一套"只能由这里钉住。写错会编译失败，
// 而不是等到界面上的「今日签到」列整片空白。
var (
	_ = checkinlog.StatusOK
	_ = checkinlog.StatusAlready
	_ = checkinlog.StatusFail
	_ = checkinlog.StatusSkip
)
