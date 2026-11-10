package admin

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// quota_refresh_policy_test.go —— 「刷新失败⇒禁用 / 刷新成功⇒恢复」两个方向。
//
// # 为什么两个方向都要钉
//
// 用户原话：「刷新失败了需要把它禁用掉，因为刷新失败有可能是账号过期了；
// 但是有可能禁用的账号刷新成功，就要解除禁用。」
//
// 两个方向的代价不对称，且都很重：
//
//	少摘一次 ⇒ 过期号留在池里，每次选中都白打一次往返（用户报的就是这个）
//	误摘一次 ⇒ 好号掉出池子；而"刷新成功却不恢复"会让它**永久**留在外面
//
// 第三条同样重要：**人工禁用**的号不该被一次刷新成功解禁 ——
// 那会让操作员的动作被系统悄悄撤销（用户刚手工摘掉的 4 个死号就是这种）。

// policyFixture 一个上游（loomy）、一个账号的 handler。
func policyFixture(t *testing.T, threshold int) (*Handler, *pool.Pool, string) {
	t.Helper()
	reg := gateway.NewRegistry()
	if err := reg.Register(quotaScopeStub{id: "loomy", asked: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	const uid = "lo-1"
	p.AddFor("loomy", &auth.Auth{UID: uid, Nickname: uid}, nil)
	p.SetAutoDisable(threshold, pool.DefaultAutoDisableWindow)
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Registry: reg, Pool: p, DefaultProvider: "loomy"})
	return h, p, uid
}

// TestQuotaRefreshPolicyDisablesThenRecovers 上游报"账号层失效" ⇒ 一次即摘；刷新成功 ⇒ 恢复。
func TestQuotaRefreshPolicyDisablesThenRecovers(t *testing.T) {
	// 阈值给个大的，用来证明"一次即摘"**不是**因为阈值小。
	h, p, uid := policyFixture(t, 99)

	// 用户点一次按钮 ⇒ 上游明说凭据失效 ⇒ 立刻摘号。
	disabled, _ := h.applyQuotaRefreshPolicy(quotaRefresher{AuthFailedUIDs: []string{uid}})
	if disabled != 1 {
		t.Fatalf("一次账号层失效就该摘号（用户点一次按钮要看到效果），实际摘了 %d 个", disabled)
	}
	if st, ok := p.Status(uid); !ok || !st.Disabled {
		t.Fatalf("应被禁用，实际 disabled=%v", st.Disabled)
	}

	// 刷新成功 ⇒ 解除（这正是"重登之后要能自己回来"）。
	_, recovered := h.applyQuotaRefreshPolicy(quotaRefresher{FreshUIDs: []string{uid}})
	if recovered != 1 {
		t.Errorf("刷新成功应恢复 1 个账号，实际 %d", recovered)
	}
	if st, _ := p.Status(uid); st.Disabled {
		t.Error("刷新成功后仍是禁用 —— 用户重登了却回不到池子")
	}
}

// TestQuotaRefreshPolicyOnlyAccountLevelFailuresReachIt 「一次即摘」的前提是判据明确。
//
// 这条守的是另一半：**没有"账号层失效"结论时，策略什么都不做**。
// 它等价于"模糊失败（网络/超时/5xx/Unknown）不会摘号" —— 因为那些失败
// 在 refreshQuotas 里根本进不了 AuthFailedUIDs（上游不会为它们报账号层失效）。
//
// ⚠ 所以"一次即摘"**不是**放宽了误杀防线，而是把防线前移到判据本身：
//   闸门是"上游是否明说是账号层失效"，不是"失败了几次"。
func TestQuotaRefreshPolicyOnlyAccountLevelFailuresReachIt(t *testing.T) {
	h, p, uid := policyFixture(t, 5)
	// 装了失败与未知，但**没有**账号层结论 ⇒ 不该摘。
	disabled, recovered := h.applyQuotaRefreshPolicy(quotaRefresher{})
	if disabled != 0 || recovered != 0 {
		t.Errorf("没有账号层结论时不该有任何处置，实际 disabled=%d recovered=%d", disabled, recovered)
	}
	if st, _ := p.Status(uid); st.Disabled {
		t.Error("没有任何账号层证据就把号摘了")
	}
}

// TestQuotaRefreshPolicyKeepsManualDisable 人工禁用的号**不**被"刷新成功"解禁。
func TestQuotaRefreshPolicyKeepsManualDisable(t *testing.T) {
	h, p, uid := policyFixture(t, 1)
	p.Disable(uid, "manual disable")

	_, recovered := h.applyQuotaRefreshPolicy(quotaRefresher{FreshUIDs: []string{uid}})
	if recovered != 0 {
		t.Errorf("人工禁用不该被认定为「已恢复」（实际 recovered=%d）", recovered)
	}
	if st, _ := p.Status(uid); !st.Disabled {
		t.Error("人工禁用的账号被一次刷新成功解禁了 —— 操作员的动作被系统悄悄撤销")
	}
}

// TestQuotaRefreshPolicyUnknownIsNotSuccess Unknown（问了但没数据）**不算**成功。
//
// 它没有证明凭据可用（loomy 的失效走的正是 Unknown）⇒ 拿它解禁会把"查不到"当成"好了"。
func TestQuotaRefreshPolicyUnknownIsNotSuccess(t *testing.T) {
	h, p, uid := policyFixture(t, 1)
	p.Disable(uid, "auto: upstream session_dead")

	// Unknown 不进 FreshUIDs（由 refreshQuotas 决定）⇒ 这里传空 ⇒ 什么都不该发生。
	_, recovered := h.applyQuotaRefreshPolicy(quotaRefresher{})
	if recovered != 0 {
		t.Errorf("没有成功证据却报恢复 %d 个", recovered)
	}
	if st, _ := p.Status(uid); !st.Disabled {
		t.Error("没有成功证据却解除了禁用")
	}
}