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

// TestQuotaRefreshPolicyDisablesThenRecovers 凭据失效达阈值 ⇒ 禁用；刷新成功 ⇒ 恢复。
func TestQuotaRefreshPolicyDisablesThenRecovers(t *testing.T) {
	const threshold = 2
	h, p, uid := policyFixture(t, threshold)

	// 连续 threshold 次"上游报告凭据失效" ⇒ 达阈值被摘。
	for i := 0; i < threshold; i++ {
		h.applyQuotaRefreshPolicy(quotaRefresher{AuthFailedUIDs: []string{uid}})
	}
	if st, ok := p.Status(uid); !ok || !st.Disabled {
		t.Fatalf("连续 %d 次凭据失效后应被自动禁用，实际 disabled=%v", threshold, st.Disabled)
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

// TestQuotaRefreshPolicyOneFailureDoesNotDisable 一次失败**不**摘号（"可能过期"≠"确定过期"）。
//
// 这条是误杀防线：把"失败即摘"接上手动的按钮很诱人，但按钮后面是网络，
// 一次上游抖动就会把整池号刷成禁用。所以复用池的阈值门控。
func TestQuotaRefreshPolicyOneFailureDoesNotDisable(t *testing.T) {
	h, p, uid := policyFixture(t, 5)
	h.applyQuotaRefreshPolicy(quotaRefresher{AuthFailedUIDs: []string{uid}})
	if st, _ := p.Status(uid); st.Disabled {
		t.Error("一次凭据失效就摘号 —— 阈值门控被绕过了，网络抖动会刷掉整池号")
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