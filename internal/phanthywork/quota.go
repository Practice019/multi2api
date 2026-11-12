// quota.go 本上游的额度（gateway.QuotaExt + gateway.QuotaAuthFailureExt）。
//
// # 为什么额度只有一个读入口
//
// 报告 §4.7 / full-platform §2.1 实测：`POST /api/oauth/activities/summary` 是
// 全平台**信息密度最高**的端点 —— 一次调用同时给出钱包可用积分（credits.available）、
// 签到状态与**服务端日键**（daily）、活动开关（feature_flags）与 server_time。
// 所以额度、签到、时钟校准三条路共用它（见 client.go 的 FetchSummary）。
//
// # 三个维度必须分清（别把"未探测"和 0 混起来）
//
//	Remaining + HasData  钱包可用积分；查不到时 HasData=false（界面 `—`，不是 0）
//	Free + FreeKnown     每天**会重置**的那部分：今日还可领的签到积分
//	FreeLimit            那部分的周期上限（实测 300 分/天）
//
// ⚠ 本上游**不报** Free 的情形：活动关闭（feature_flags.daily_enabled=false）或
// 上游没给 daily 块 —— 那时留 FreeKnown=false（界面 `—`），**绝不填 0**：
// 0 会被读成"今天的免费额度已经用完"，而那是一个确定性的错结论。
//
// ⚠ 也**不**把 `/api/oauth/usage` 的四窗口当额度上卡片：报告 §4.3 引官方口径 ——
// 当前套餐用单一个月度积分池，5 小时/7 天那两个窗口**不是独立额度**，
// 拿它们上卡片会把同一个池重复计权。
package phanthywork

import (
	"context"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：额度与"凭据失效"两条自报面都挂在 *Provider 上。
var (
	_ gateway.QuotaExt            = (*Provider)(nil)
	_ gateway.QuotaAuthFailureExt = (*Provider)(nil)
)

// DailyCheckinPoints 每日签到的积分上限（实测 300 分/天，报告 §4.7）。
//
// ⚠ 它只是**兜底**：服务端在 daily.points / daily.next_points 里给了就用服务端的值，
// 本常量用于"服务端没给那个字段"的情况（编一个上限会让界面显示错的分母）。
const DailyCheckinPoints int64 = 300

// quotaTimeout 单次额度查询的超时（它是只读的，短一点没关系）。
const quotaTimeout = 20 * time.Second

// RefreshQuota 取某个账号的当前额度（gateway.QuotaExt）。
//
// 返回值语义（契约）：
//
//	(额度, false)  账号不存在 / 未接线 → 调用方 404 或跳过
//	(有数据, true) 查到了 → 调用方写回池（HasData=true）
//	(无数据, true) 查不到 → **不写池**，界面保持"未知"
//
// ⚠ 查不到时**不编造 0**：0 是"额度用完"的语义，会让用户误删账号。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	if uid == "" {
		return gateway.UnknownQuota(), false
	}
	a := p.cred(uid)
	if a == nil || a.OAuth.AccessToken == "" {
		// 未接线 / 找不到这个 uid：不是"额度未知"，而是"我不知道这个号"。
		return gateway.UnknownQuota(), false
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
	defer cancel()

	sum, localMS, err := p.client.FetchSummary(ctx, a)
	if err != nil {
		// 取不到是**正常情况**（凭据过期 / 上游限流 / 网络）—— 不刷错误日志，
		// 界面显示"未知"。但"是不是凭据失效"要记下来给核心（见 QuotaAuthFailed）。
		p.noteQuotaAuthFailure(uid, isAuthFailure(err))
		return gateway.UnknownQuota(), true
	}
	p.noteQuotaAuthFailure(uid, false) // 成功即清掉上一次的结论
	// 时钟顺带校准（这条响应带 server_time）—— 与签到路径同一个来源。
	if sum.ServerTime != "" {
		a.applyServerTime(sum.ServerTime, localMS)
		p.client.setSkew(a.skew())
	}
	qv := gateway.CreditsQuota(sum.Credits.Available)
	if limit, free, known := dailyFreeQuota(sum); known {
		qv = qv.WithFreeLimit(free, limit)
	}
	return qv, true
}

// dailyFreeQuota 从一次 summary 响应算出「今日还可领的签到积分」。
//
// 三态（与 gateway.QuotaView 的 FreeKnown 纪律一致）：
//
//	活动关闭 / 没有 daily 块 → known=false（界面 `—`，**不是 0**）
//	今天已领（status=granted_today） → free=0, known=true
//	今天还没领              → free=上限, known=true
//
// ⚠ "已领"的判据只认服务端给的 status（报告 §2.2：summary 用 granted_today、
// claim 用 already_granted，**两套词说同一件事**）；本函数不需要本地算日期 ——
// 状态与服务端日键都由服务端给出，这正是"不许本地算日期"那条要求的落点。
func dailyFreeQuota(sum summaryResponse) (limit int64, free int64, known bool) {
	if !sum.FeatureFlags.DailyEnabled && sum.Daily.Status == "" {
		// 上游明确说"今天不开放"（或整个 daily 块都没给）⇒ 不知道，别填 0。
		return 0, 0, false
	}
	limit = sum.Daily.Points
	if limit <= 0 {
		limit = sum.Daily.NextPoints
	}
	if limit <= 0 {
		limit = DailyCheckinPoints
	}
	if sum.Daily.Status == "granted_today" {
		return limit, 0, true // 今天已领：这一维的"剩余"就是 0（确定值）
	}
	return limit, limit, true // 还没领：今天仍可领满一份
}

// QuotaAuthFailed 报告该账号最近一次额度查询是否因**凭据失效**而失败。
//
// 契约要求纯函数、不发网络 —— 它只读上一次查询留在内存里的结论。
func (p *Provider) QuotaAuthFailed(uid string) bool {
	if p == nil || uid == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.quotaAuthFailed[uid]
}

// noteQuotaAuthFailure 记下"这次查询是不是凭据失效"。
func (p *Provider) noteQuotaAuthFailure(uid string, failed bool) {
	if p == nil || uid == "" {
		return
	}
	p.mu.Lock()
	if p.quotaAuthFailed == nil {
		p.quotaAuthFailed = map[string]bool{}
	}
	if failed {
		p.quotaAuthFailed[uid] = true
	} else {
		delete(p.quotaAuthFailed, uid)
	}
	p.mu.Unlock()
}

// isAuthFailure 判一次额度查询失败是不是**凭据**问题。
//
// 判据复用本包自己的分类器（Classify）—— 不另写一套状态码判断：
// 那正是"两处判据打架"的温床，本包已经吃过一次（见 login.go 的注释）。
//
// ⚠ 只有 ErrKindAuth 才算：ErrKindClient（403 里那一大片签名/套餐问题）
// **不算** —— 把签名错判成凭据失效会让核心摘掉一个健康的号。
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var ue *UpstreamError
	if asUpstreamError(err, &ue) && ue != nil {
		return Classify(ue.Status, ue.Body) == gateway.ErrKindAuth
	}
	return false
}

// asUpstreamError 是本包对 errors.As 的薄包装（避免在纯判据函数里 import errors）。
func asUpstreamError(err error, target **UpstreamError) bool {
	for err != nil {
		if ue, ok := err.(*UpstreamError); ok {
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
