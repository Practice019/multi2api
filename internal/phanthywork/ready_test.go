// ready_test.go 「设备身份就绪」这条中心化入口的断言（2026-11-13 用户报障）。
//
// # 报障形态（一句话）
//
//	本包的设备注册是**懒注册**，原先只有 Chat 会走；额度探测/签到/取邀请码都直接拿
//	"本地派生的 installationId"去签名 ⇒ **从未注册过**的新账号必然被上游拒，
//	而那个拒（401 desktop_installation_required）又被判成"账号层失效" ⇒ 核心"一次即摘"，
//	一个 oauth 完全健康的账号（access_token 还有 29 分钟）被自动禁用。
//
// 断言四条：
//
//	① 未注册 ⇒ 额度探测**先注册**（假上游看到 register）且探测成功
//	② 未注册 ⇒ 签到 / 邀请码 两条路同样先注册
//	③ 「未注册」这类失败**不得**归类成账号层失效（不触发一次即摘）
//	④ 已注册 ⇒ 行为不变（**不许多打一次 register**）
package phanthywork

import (
	"context"
	"testing"
	"time"
	"workbuddy2api/internal/gateway"
)

// readyFixture 造一个"未注册"或"已注册"的账号 + 假上游。
func readyFixture(t *testing.T, registered bool) (*Provider, *Auth, *fakeUpstream) {
	t.Helper()
	inviteCodeSpacing = 0
	t.Cleanup(func() { inviteCodeSpacing = time.Second })
	srv, fake := startFakeUpstream(t)
	fake.dailyEnabled = true
	fake.dailyStatus = "available"
	fake.serverDate = "2026-11-13"
	fake.eligibleToInvite = true
	fake.creditsAvailable = 3872
	p := newFakeProvider(t, srv)
	a := newTestAuth("u-new", "rt")
	if registered {
		a.Reg.RegisteredAtMS = nowMS()
	}
	fake.trust(a)
	p.SetLocalCredential(a.UID(), a)
	p.SetUIDEnumerator(func() []string { return []string{a.UID()} })
	return p, a, fake
}

// ① 未注册 ⇒ 额度探测先注册，且探测成功。
func TestQuotaProbeRegistersUnregisteredAccount(t *testing.T) {
	p, a, fake := readyFixture(t, false)
	qv, ok := p.RefreshQuota(a.UID())
	if !ok {
		t.Fatal("账号在 ⇒ ok 应为 true")
	}
	if got := atomicLoad(&fake.registerCalls); got != 1 {
		t.Fatalf("额度探测前必须先注册一次（假上游应看到 register），实际 %d 次", got)
	}
	// 顺序：register 必须**早于** summary。
	paths := fake.pathsSnapshot()
	var iReg, iSum = -1, -1
	for i, pth := range paths {
		if pth == pathRegister && iReg < 0 {
			iReg = i
		}
		if pth == pathActivitiesSummary && iSum < 0 {
			iSum = i
		}
	}
	if iReg < 0 || iSum < 0 || iReg > iSum {
		t.Errorf("顺序不对：register 下标=%d，summary 下标=%d（paths=%v）—— 必须是先注册再探测", iReg, iSum, paths)
	}
	if !qv.HasData {
		t.Errorf("注册后探测应当拿到数据，实际 %+v", qv)
	}
	if a.Reg.RegisteredAtMS == 0 {
		t.Error("注册成功后应当把 RegisteredAtMS 落到凭证上（否则每次都要重注册）")
	}
}

// ② 签到与邀请码两条路同样先注册。
func TestCheckinAndInviteRegisterUnregisteredAccount(t *testing.T) {
	// 签到（status + claim 两条都在 CheckinOne 里）
	p, a, fake := readyFixture(t, false)
	res := p.checkin.CheckinOne(context.Background(), accFor(a), "manual")
	if res.Status != "ok" {
		t.Errorf("未注册账号的签到应当先注册再成功，实际 status=%q detail=%q", res.Status, res.Detail)
	}
	if got := atomicLoad(&fake.registerCalls); got != 1 {
		t.Errorf("签到路径应当先注册一次，实际 %d 次", got)
	}

	// 邀请码面板（取码那条 POST 也是签名请求）
	p2, _, fake2 := readyFixture(t, false)
	_, body := inviteFetch(t, p2, "")
	rows := inviteRows(t, body)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d", len(rows))
	}
	if ok, _ := rows[0]["ok"].(bool); !ok {
		t.Errorf("未注册账号的面板应当先注册再拿到码，实际 %#v", rows[0])
	}
	if got := atomicLoad(&fake2.registerCalls); got != 1 {
		t.Errorf("邀请码路径应当先注册一次，实际 %d 次", got)
	}
}

// ③ 「未注册」这类失败不得归类成账号层失效。
//
// ⚠ 这是"一次即摘"的闸门：核心按 ErrorKind 决定摘不摘号 ——
//
//	ErrKindAuth / ErrKindSessionDead 会摘，ErrKindClient **只换号不罚**。
func TestNotInitializedIsNotAccountLayerFailure(t *testing.T) {
	const body = `{"error":{"code":"desktop_installation_required"}}`
	if got := Classify(401, body); got == gateway.ErrKindAuth || got == gateway.ErrKindSessionDead {
		t.Errorf("Classify(401, desktop_installation_required) = %v —— 那是「未注册」，不是账号层失效；"+
			"判成它会让核心一次即摘掉健康的号", got)
	}
	if isAuthFailure(&UpstreamError{Status: 401, Body: body}) {
		t.Error("未注册不许被 isAuthFailure 认成凭据失效（那正是报障的链路）")
	}
	// 端到端：额度探测真的收到这条 401 时，**不许**留下"凭据失效"的结论。
	p, a, fake := readyFixture(t, true)
	fake.mu.Lock()
	fake.summaryAuthFail = true
	fake.summaryAuthFailCode = "desktop_installation_required"
	fake.mu.Unlock()
	if _, ok := p.RefreshQuota(a.UID()); !ok {
		t.Fatal("账号在 ⇒ ok 应为 true")
	}
	if p.QuotaAuthFailed(a.UID()) {
		t.Error("未注册的 401 不许记成「凭据失效」—— 那会让核心一次即摘（报障形态）")
	}
	// 反过来锁：真·凭据失效**必须**记（否则重登的号也救不回来）。
	fake.mu.Lock()
	fake.summaryAuthFailCode = "invalid_grant"
	fake.mu.Unlock()
	if _, ok := p.RefreshQuota(a.UID()); !ok {
		t.Fatal("账号在 ⇒ ok 应为 true")
	}
	if !p.QuotaAuthFailed(a.UID()) {
		t.Error("401 invalid_grant 必须被记成凭据失效")
	}
}

// ④ 已注册 ⇒ 不许多打一次 register（回归锁）。
func TestRegisteredAccountDoesNotRegisterAgain(t *testing.T) {
	p, a, fake := readyFixture(t, true)
	if _, ok := p.RefreshQuota(a.UID()); !ok {
		t.Fatal("额度探测 ok 应为 true")
	}
	_ = p.checkin.CheckinOne(context.Background(), accFor(a), "manual")
	_, _ = inviteFetch(t, p, "")
	if got := atomicLoad(&fake.registerCalls); got != 0 {
		t.Errorf("已注册的账号不许再打 register，实际 %d 次（三条路径合计）", got)
	}
}
