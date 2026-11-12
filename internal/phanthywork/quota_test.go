// quota_test.go 额度能力（gateway.QuotaExt + QuotaAuthFailureExt）的断言。
//
// 重点守两件事：
//
//	① 三态别混：查到了 / 查到 0 / 没查到 在界面上是三种不同的显示
//	② "会按周期重置的那部分"（今日签到积分）有三态，**未知不能写成 0**
package phanthywork

import (
	"testing"
)

// quotaFixture 起一个带凭据的 Provider（额度查询要有账号）。
func quotaFixture(t *testing.T, uid string, tune func(*fakeUpstream)) (*Provider, *fakeUpstream) {
	t.Helper()
	srv, fake := startFakeUpstream(t)
	fake.creditsAvailable = 3600
	fake.dailyEnabled = true
	fake.serverDate = "2026-11-12"
	if tune != nil {
		tune(fake)
	}
	p := newFakeProvider(t, srv)
	a := newTestAuth(uid, "rt")
	fake.trust(a) // 假上游"已认识这台设备"（见 trust 的注释：它只影响验签）
	p.SetLocalCredential(uid, a)
	return p, fake
}

// TestRefreshQuotaReportsCreditsAndFree 正常路径：钱包额度 + 今日可领签到积分。
//
// 期望：Remaining = credits.available；Free = 300（今天还没领）；FreeLimit = 300。
func TestRefreshQuotaReportsCreditsAndFree(t *testing.T) {
	p, fake := quotaFixture(t, "u-1", func(f *fakeUpstream) { f.dailyStatus = "available" })
	qv, ok := p.RefreshQuota("u-1")
	if !ok {
		t.Fatal("有凭证时应当回答额度（ok=true）")
	}
	if !qv.HasData {
		t.Fatal("查到了就必须 HasData=true（否则界面显示 `—`）")
	}
	if qv.Kind != "credits" || qv.Remaining != 3600 {
		t.Errorf("Kind/Remaining = %q/%d, want credits/3600", qv.Kind, qv.Remaining)
	}
	if !qv.FreeKnown {
		t.Error("daily 块在、活动开着 ⇒ FreeKnown 必须为 true")
	}
	if qv.Free != DailyCheckinPoints || qv.FreeLimit != DailyCheckinPoints {
		t.Errorf("Free/FreeLimit = %d/%d, want %d/%d（今天还没领 ⇒ 剩余=上限）",
			qv.Free, qv.FreeLimit, DailyCheckinPoints, DailyCheckinPoints)
	}
	if fake.pathCount(pathActivitiesSummary) != 1 {
		t.Errorf("应当只打一次 summary（额度/签到/时钟共用一个读入口），实际 %d 次", fake.pathCount(pathActivitiesSummary))
	}
}

// TestRefreshQuotaFreeIsZeroWhenAlreadyClaimed 今天已领 ⇒ Free=0 且**仍然是已知值**。
//
// ⚠ 这条与下一条（未知）必须分开断言：0 与"未知"在界面上是两个不同的显示。
func TestRefreshQuotaFreeIsZeroWhenAlreadyClaimed(t *testing.T) {
	p, _ := quotaFixture(t, "u-1", func(f *fakeUpstream) { f.dailyStatus = "granted_today" })
	qv, ok := p.RefreshQuota("u-1")
	if !ok || !qv.HasData {
		t.Fatal("应当查到额度")
	}
	if !qv.FreeKnown {
		t.Error("已领是一种**确定**的状态 ⇒ FreeKnown 必须为 true（0 与未知不是一回事）")
	}
	if qv.Free != 0 {
		t.Errorf("今天已领 ⇒ Free 应为 0，得到 %d", qv.Free)
	}
	if qv.FreeLimit != DailyCheckinPoints {
		t.Errorf("FreeLimit 仍应是周期上限 %d，得到 %d", DailyCheckinPoints, qv.FreeLimit)
	}
}

// TestRefreshQuotaFreeUnknownWhenDailyDisabled 活动关闭 ⇒ FreeKnown=false（界面 `—`，**不是 0**）。
//
// 这条守的是"别把未探测和 0 混起来"：报 0 会被用户读成"今天的免费额度已经用完"。
func TestRefreshQuotaFreeUnknownWhenDailyDisabled(t *testing.T) {
	// daily_enabled=false 且**没有** daily 块 ⇒ 上游明确不开放活动
	p, _ := quotaFixture(t, "u-1", func(f *fakeUpstream) {
		f.dailyEnabled = false
		f.dailyStatus = ""
	})
	qv, ok := p.RefreshQuota("u-1")
	if !ok || !qv.HasData {
		t.Fatal("钱包额度仍然应当查到")
	}
	if qv.FreeKnown {
		t.Error("活动关闭 ⇒ FreeKnown 必须为 false（留 `—`），而不是报一个 0")
	}
	if qv.Free != 0 {
		t.Errorf("unknown 时 Free 应留零值（FreeKnown=false 才是判据），得到 %d", qv.Free)
	}
	if qv.Remaining != 3600 {
		t.Errorf("钱包额度不该被 free 的未知状态影响，得到 %d", qv.Remaining)
	}
}

// TestRefreshQuotaUnknownOnUpstreamFailure 上游失败 ⇒ (未知, true)：问了，但取不到。
//
// ⚠ 与"uid 不存在"(ok=false) 严格分开：前者会显示 `—`，后者调用方应当 404/跳过。
func TestRefreshQuotaUnknownOnUpstreamFailure(t *testing.T) {
	p, _ := quotaFixture(t, "u-1", func(f *fakeUpstream) { f.summaryStatus = 500 })
	qv, ok := p.RefreshQuota("u-1")
	if !ok {
		t.Fatal("账号在、只是上游失败 ⇒ 应当回 ok=true（未知），而不是 false")
	}
	if qv.HasData {
		t.Error("取不到时**绝不能** HasData=true（那会把未知伪装成 0）")
	}
	if qv.Kind != "" || qv.Remaining != 0 {
		t.Errorf("未知额度应当是零值形态，得到 kind=%q remaining=%d", qv.Kind, qv.Remaining)
	}
}

// TestRefreshQuotaUnknownUID 未接线的 uid ⇒ ok=false（不是"额度未知"）。
func TestRefreshQuotaUnknownUID(t *testing.T) {
	p, fake := quotaFixture(t, "u-1", nil)
	if _, ok := p.RefreshQuota("nope"); ok {
		t.Error("uid 不存在时应当回 ok=false（调用方据此 404/跳过）")
	}
	if _, ok := p.RefreshQuota(""); ok {
		t.Error("空 uid 应当回 ok=false")
	}
	if n := fake.pathCount(pathActivitiesSummary); n != 0 {
		t.Errorf("uid 不存在时不该打上游，实际 %d 次", n)
	}
}

// TestQuotaAuthFailureIsRememberedThenCleared 凭据失效的结论要能记住、也能被清掉。
//
// 契约：QuotaAuthFailed 是"上一次查询的结论"（纯函数、不发网络）；
// 下一次成功必须清掉它 —— 否则一个重登后的号会被一直误判成凭据失效而被摘掉。
func TestQuotaAuthFailureIsRememberedThenCleared(t *testing.T) {
	p, fake := quotaFixture(t, "u-1", func(f *fakeUpstream) { f.summaryAuthFail = true })
	if _, ok := p.RefreshQuota("u-1"); !ok {
		t.Fatal("账号在 ⇒ ok 应为 true")
	}
	if !p.QuotaAuthFailed("u-1") {
		t.Error("401 之后必须能被 QuotaAuthFailed 认出来（核心据此摘号）")
	}
	// 注意：它必须是纯函数 —— 再问一次不该产生任何请求
	before := fake.pathCount(pathActivitiesSummary)
	_ = p.QuotaAuthFailed("u-1")
	if after := fake.pathCount(pathActivitiesSummary); after != before {
		t.Errorf("QuotaAuthFailed 必须是纯函数（不发请求），请求数从 %d 变成 %d", before, after)
	}
	// 上游恢复正常 ⇒ 下一次成功后必须清掉结论
	fake.mu.Lock()
	fake.summaryAuthFail = false
	fake.mu.Unlock()
	if _, ok := p.RefreshQuota("u-1"); !ok {
		t.Fatal("恢复后 ok 应为 true")
	}
	if p.QuotaAuthFailed("u-1") {
		t.Error("成功查询后必须清掉凭据失效结论（否则重登后的号会被一直误判）")
	}
}

// TestQuotaAuthFailJudgementIsNarrow 只有"凭据类"才算 AuthFailed。
//
// 403（签名/套餐那一大片）**不算** —— 把它判成凭据失效会让核心摘掉一个健康的号。
// 判据复用本包自己的分类器，所以这里直接对着分类器的边界断言。
func TestQuotaAuthFailJudgementIsNarrow(t *testing.T) {
	if isAuthFailure(nil) {
		t.Error("nil 错误不是凭据失效")
	}
	if isAuthFailure(&UpstreamError{Status: 403, Body: `{"error":{"code":"model_not_allowed"}}`}) {
		t.Error("403 model_not_allowed 是套餐问题，**不是**凭据失效")
	}
	if isAuthFailure(&UpstreamError{Status: 403, Body: `{"error":{"code":"request_not_authorized"}}`}) {
		t.Error("403 request_not_authorized 混着签名/时钟问题，保守判**非**凭据失效（只换号不摘号）")
	}
	if !isAuthFailure(&UpstreamError{Status: 401, Body: `{"error":{"code":"desktop_installation_required"}}`}) {
		t.Error("401 desktop_installation_required 是凭据/安装态问题，应当算")
	}
	if isAuthFailure(&UpstreamError{Status: 500, Body: "boom"}) {
		t.Error("5xx 是上游抖，**绝不**算凭据失效")
	}
}

// TestDailyFreeQuotaThreeStates dailyFreeQuota 的三态（直接对着纯函数断言）。
func TestDailyFreeQuotaThreeStates(t *testing.T) {
	// 未领
	sum := summaryResponse{}
	sum.FeatureFlags.DailyEnabled = true
	sum.Daily.Status = "available"
	sum.Daily.Points = 300
	if limit, free, known := dailyFreeQuota(sum); !known || free != 300 || limit != 300 {
		t.Errorf("未领: (%d,%d,%v), want (300,300,true)", limit, free, known)
	}
	// 已领
	sum.Daily.Status = "granted_today"
	if limit, free, known := dailyFreeQuota(sum); !known || free != 0 || limit != 300 {
		t.Errorf("已领: (%d,%d,%v), want (300,0,true)", limit, free, known)
	}
	// 活动关闭
	off := summaryResponse{}
	if _, _, known := dailyFreeQuota(off); known {
		t.Error("活动关闭/没有 daily 块 ⇒ 必须是 unknown（不能报 0）")
	}
	// 服务端没给 points ⇒ 用兜底上限
	nopts := summaryResponse{}
	nopts.FeatureFlags.DailyEnabled = true
	nopts.Daily.Status = "available"
	if limit, _, _ := dailyFreeQuota(nopts); limit != DailyCheckinPoints {
		t.Errorf("服务端没给上限时应当用兜底 %d，得到 %d", DailyCheckinPoints, limit)
	}
}
