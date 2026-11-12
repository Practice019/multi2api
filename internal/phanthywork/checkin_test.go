// checkin_test.go 每日签到的断言（含报告 §4.7 的三条硬要求）。
//
// 三条硬要求各自有独立断言：
//
//	① body 必须是**空对象** —— 假上游对非 `{}` 直接回 403
//	② 幂等键 `daily:<installationId>:<server_date>`，且日期取**服务端**给的
//	③ "今日已领"是**幂等成功**，不是失败
package phanthywork

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/dailycheckin"
	"workbuddy2api/internal/gateway"
)

// checkinFixture 造一个"已注册 + 已接线"的 Provider，返回账号与假上游。
func checkinFixture(t *testing.T, tune func(*fakeUpstream)) (*Provider, *Auth, *fakeUpstream) {
	t.Helper()
	srv, fake := startFakeUpstream(t)
	fake.dailyEnabled = true
	fake.dailyStatus = "available"
	fake.serverDate = "2026-11-12"
	if tune != nil {
		tune(fake)
	}
	p := newFakeProvider(t, srv)
	a := newTestAuth("u-1", "rt")
	a.Reg.RegisteredAtMS = nowMS()
	fake.trust(a)
	p.SetLocalCredential(a.UID(), a)
	p.SetUIDEnumerator(func() []string { return []string{a.UID()} })
	return p, a, fake
}

// accFor 把账号包成驱动要的形状。
func accFor(a *Auth) dailycheckin.Account {
	return dailycheckin.Account{UID: a.UID(), Secret: a}
}

// TestDailyActionsDeclareMountedRoutes 每日动作声明的路径必须真的挂在 AdminRoutes 上。
//
// ⚠ 这是"假按钮"的判据：前端按 OneURL/AllURL 打过来，路由不在就是 404。
func TestDailyActionsDeclareMountedRoutes(t *testing.T) {
	p, _, _ := checkinFixture(t, nil)
	actions := p.DailyActions()
	if len(actions) != 1 {
		t.Fatalf("本上游只有签到这一个每日动作，实际 %v", actions)
	}
	act := actions[0]
	if act.ID != "phanthywork-checkin" || act.Label == "" || act.Title == "" {
		t.Errorf("ID/Label/Title 不合约定: %+v", act)
	}
	if !act.Batch || act.AllURL == "" {
		t.Error("有全量端点 ⇒ Batch 必须为 true（否则分组标题上不会出现「全部签到」）")
	}
	routes := map[string]gateway.AdminRoute{}
	for _, r := range p.AdminRoutes() {
		routes[r.Path] = r
	}
	for _, url := range []string{act.OneURL, act.AllURL} {
		r, ok := routes[url]
		if !ok {
			t.Errorf("DailyActions 声明的路径 %q 没有挂在 AdminRoutes 上（点了会 404）", url)
			continue
		}
		if r.Method != http.MethodPost || r.Handler == nil {
			t.Errorf("路由 %q 的形状不对: method=%q handler=%v", url, r.Method, r.Handler != nil)
		}
		if r.Capability != gateway.CapCheckin {
			t.Errorf("路由 %q 的能力位应当是 CapCheckin，得到 %v", url, r.Capability)
		}
	}
}

// TestCheckinClaimUsesEmptyObjectAndServerDateKey ★ 硬要求①②。
//
// 假上游对这两条是**强制**的：body 不是 `{}` 回 403；幂等键不等于
// `daily:<installationId>:<服务端日键>` 也回 403。所以这条用例绿 = 两条硬要求都成立。
func TestCheckinClaimUsesEmptyObjectAndServerDateKey(t *testing.T) {
	p, a, fake := checkinFixture(t, nil)
	res := p.checkin.CheckinOne(context.Background(), accFor(a), "manual")
	if res.Status != "ok" {
		t.Fatalf("签到应当成功，得到 status=%q detail=%q", res.Status, res.Detail)
	}
	if res.Credits != DailyCheckinPoints {
		t.Errorf("领到积分 = %d, want %d", res.Credits, DailyCheckinPoints)
	}
	fake.mu.Lock()
	body, key := fake.lastClaimBody, fake.lastClaimKey
	fake.mu.Unlock()
	if body != "{}" {
		t.Errorf("硬要求①：claim 的 body 必须是**空对象**，实际 %q", body)
	}
	instID, _ := a.installationID()
	want := DailyLoginIdempotencyKey(instID, "2026-11-12")
	if key != want {
		t.Errorf("硬要求②：幂等键 = %q, want %q（日期必须是服务端给的）", key, want)
	}
	if strings.Contains(key, "/") || strings.HasPrefix(key, "daily:0") {
		t.Errorf("幂等键形态不对: %q", key)
	}
}

// TestCheckinAlreadyClaimedFromStatusIsNoOp ★ 状态已领 ⇒ 连 claim 都不打。
func TestCheckinAlreadyClaimedFromStatusIsNoOp(t *testing.T) {
	p, a, fake := checkinFixture(t, func(f *fakeUpstream) { f.dailyStatus = "granted_today" })
	res := p.checkin.CheckinOne(context.Background(), accFor(a), "manual")
	if res.Status != "already" {
		t.Errorf("今天已领应当记 already（不是 fail、也不是 ok），得到 %q", res.Status)
	}
	if n := atomicLoad(&fake.claimCalls); n != 0 {
		t.Errorf("状态已领时不该再打 claim，实际 %d 次", n)
	}
}

// TestCheckinAlreadyGrantedFromClaimIsIdempotentSuccess ★ 硬要求③。
//
// 竞态：查状态时还没领（所以会去打 claim），而 claim 回 already_granted。
// 契约要求这是**成功**（幂等），不是失败。
func TestCheckinAlreadyGrantedFromClaimIsIdempotentSuccess(t *testing.T) {
	p, a, fake := checkinFixture(t, func(f *fakeUpstream) { f.claimStatus = "already_granted" })
	res := p.checkin.CheckinOne(context.Background(), accFor(a), "manual")
	if res.Status != "ok" {
		t.Errorf("already_granted 必须是幂等**成功**，得到 status=%q detail=%q", res.Status, res.Detail)
	}
	if res.Credits != 0 {
		t.Errorf("已领时积分为 0（分在上一次领到手了），得到 %d", res.Credits)
	}
	if n := atomicLoad(&fake.claimCalls); n != 1 {
		t.Errorf("应当恰好打一次 claim，实际 %d 次", n)
	}
}

// TestCheckinRefusesLocalDateWhenServerDateMissing ★ 硬要求②的反面。
//
// 上游没给 server_date 时**不许**用本地日期拼幂等键（本仓明确要求）——
// 正确行为是拒绝发送并给出可读原因。
func TestCheckinRefusesLocalDateWhenServerDateMissing(t *testing.T) {
	p, a, fake := checkinFixture(t, func(f *fakeUpstream) { f.omitServerDate = true })
	res := p.checkin.CheckinOne(context.Background(), accFor(a), "manual")
	if res.Status != "fail" {
		t.Errorf("拿不到服务端日键时应当失败（而不是用本地日期凑），得到 %q", res.Status)
	}
	if !strings.Contains(res.Detail, "server_date") && !strings.Contains(res.Detail, "日键") {
		t.Errorf("失败原因应当说清「缺服务端日键」，实际 %q", res.Detail)
	}
	if n := atomicLoad(&fake.claimCalls); n != 0 {
		t.Errorf("不该发出 claim 请求（本地日期会签错幂等键），实际 %d 次", n)
	}
}

// TestCheckinAllReportsPerAccount 全量签到：逐账号结果，不是只回一个总数。
func TestCheckinAllReportsPerAccount(t *testing.T) {
	p, a1, fake := checkinFixture(t, nil)
	a2 := newTestAuth("u-2", "rt-2")
	a2.Reg.RegisteredAtMS = nowMS()
	fake.trust(a2) // 第二个账号也要"被上游认识"（真实世界里它各自注册过设备密钥）
	p.SetLocalCredential(a2.UID(), a2)
	p.SetUIDEnumerator(func() []string { return []string{a1.UID(), a2.UID()} })
	rep := p.CheckinAll(context.Background())
	if rep.Error != "" {
		t.Fatalf("全量签到报错: %s", rep.Error)
	}
	if len(rep.Results) != 2 {
		t.Fatalf("应当逐账号回结果（2 个），实际 %d 个: %+v", len(rep.Results), rep.Results)
	}
	for _, r := range rep.Results {
		if r.Status != "ok" {
			t.Errorf("uid=%s 应当签到成功，得到 %q（%s）", r.UID, r.Status, r.Detail)
		}
	}
	if n := atomicLoad(&fake.claimCalls); n != 2 {
		t.Errorf("两个账号各领一次，实际 %d 次", n)
	}
}

// TestCheckinSkipsDisabledAccounts 已禁用的号**一个上游请求都不发**。
//
// 驱动侧的 L1 判据（"别做这件事"）：它已经不可用了，打上游只会白喂风控。
func TestCheckinSkipsDisabledAccounts(t *testing.T) {
	p, a, fake := checkinFixture(t, nil)
	p.SetDisabledChecker(func(uid string) bool { return uid == a.UID() })
	if err := p.checkin.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll 报错: %v", err)
	}
	if n := atomicLoad(&fake.claimCalls); n != 0 {
		t.Errorf("已禁用的号不该被签到，实际打了 %d 次 claim", n)
	}
	if n := fake.pathCount(pathActivitiesSummary); n != 0 {
		t.Errorf("已禁用的号连状态都不该查，实际打了 %d 次 summary", n)
	}
}

// TestCheckinHTTPRoutesRespond 两条端点在 HTTP 层真的能用（不只是"挂上了"）。
//
// 单账号端点要 body {"uid": "..."}（驱动契约），全量端点无参。
func TestCheckinHTTPRoutesRespond(t *testing.T) {
	p, a, _ := checkinFixture(t, nil)
	routes := map[string]http.HandlerFunc{}
	for _, r := range p.AdminRoutes() {
		routes[r.Path] = r.Handler
	}
	one, all := checkinDescriptor.OneURL, checkinDescriptor.AllURL
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, one, strings.NewReader(`{"uid":"`+a.UID()+`"}`))
	routes[one](rec, req)
	if rec.Code != 200 {
		t.Errorf("单账号端点 HTTP %d（期望 200）：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("单账号端点回执里应当是 ok 的结果（驱动的 HandlerOne 形状）: %s", rec.Body.String())
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, all, nil)
	routes[all](rec2, req2)
	if rec2.Code != 200 {
		t.Errorf("全量端点 HTTP %d（期望 200）：%s", rec2.Code, rec2.Body.String())
	}
}

// TestCheckinWorksWithoutHistoryLog 没接线签到日志也能跑（只是不写历史）。
//
// 契约允许 Log=nil（见驱动 Options 的注释）；这里断言"签到本身仍然成功"，
// 因为"接线漏了"与"功能坏了"是两件事 —— 前者表现为列空，后者表现为按钮失败。
func TestCheckinWorksWithoutHistoryLog(t *testing.T) {
	p, a, _ := checkinFixture(t, nil)
	p.SetCheckinLog(nil) // 显式不接线
	res := p.checkin.CheckinOne(context.Background(), accFor(a), "manual")
	if res.Status != "ok" {
		t.Errorf("没接线历史日志时签到仍应成功，得到 %q（%s）", res.Status, res.Detail)
	}
}

// atomicLoad 读一个 int32 计数（测试里用得着的原子读）。
func atomicLoad(p *int32) int32 { return atomic.LoadInt32(p) }
