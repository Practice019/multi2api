package server

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// session_death_policy_test.go —— 「上游说会话失效不可自愈时，是否一次即禁用」。
//
// # 为什么必须有这条测试
//
// 判据来自**上游自己**（gateway.SessionDeathPolicyExt），核心只做分叉。
// 而两条分支的后果都很重、且方向相反：
//
//	少走一条 ⇒ loomy 的死号永远只在计数（连续 3 次才摘），白挨三次选中；
//	多走一条 ⇒ workbuddy 的瞬时抖动（网络闪断/刷新竞态）被升级成永久禁用，
//	            而界面上只显示"已禁用"，没有任何线索指向真因。
//
// 所以两个方向都要钉：**声明了终态 ⇒ 一次即禁用；没声明 ⇒ 保持门控**。

// fakeSecret 是占位凭证（本用例不碰网络，核心也不读它）。
type fakeSecret struct{ S string }

// permanentDeathRouter 在既有假路由之上**只加一个方法**。
//
// ⚠ 用内嵌而不是重写整个 ProviderRouter：出口层要的是**可选**能力，
//
//	测试也应该只体现"多了一个方法"这一件事。
type permanentDeathRouter struct{ scriptedRouter }

func (permanentDeathRouter) SessionDeathPermanent(string) bool { return true }

// setupSessionDeath 造一个 loomy 域、单账号的 handler。
func setupSessionDeath(t *testing.T, router ProviderRouter) (*Handler, *pool.Pool, string) {
	t.Helper()
	p := pool.New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("loomy")
	uid := "loomy-uid-1"
	p.SyncToDirWithSecrets("loomy",
		[]*auth.Auth{{UID: uid, Nickname: uid}},
		map[string]any{uid: &fakeSecret{S: "s"}})
	p.SetMaxInFlight(50)
	h := NewHandler(Config{
		Pool:              p,
		Provider:          router,
		DefaultProvider:   "loomy",
		OwnedBy:           "loomy",
		DefaultClassifier: defaultClassifierOf(),
	})
	return h, p, uid
}

func disabledOf(t *testing.T, p *pool.Pool, provider, uid string) bool {
	t.Helper()
	for _, st := range p.ListFor(provider) {
		if st.UID == uid {
			return st.Disabled
		}
	}
	t.Fatalf("池里找不到 %s/%s", provider, uid)
	return false
}

// TestSessionDeathPermanentDisablesImmediately 上游声明终态 ⇒ 一次即禁用。
func TestSessionDeathPermanentDisablesImmediately(t *testing.T) {
	h, p, uid := setupSessionDeath(t, &permanentDeathRouter{})
	if disabledOf(t, p, "loomy", uid) {
		t.Fatal("前置：这个号一开始不该是禁用的")
	}
	h.applyErrorPolicy(uid, gateway.ErrKindSessionDead)
	if !disabledOf(t, p, "loomy", uid) {
		t.Error("上游声明了「会话失效不可自愈」，一次就该禁用；实际仍在池里" +
			"（这正是 loomy 死号白挨三次选中那条）")
	}
}

// TestSessionDeathWithoutPolicyKeepsGate 没声明 ⇒ 保持既有的连续计数门控。
//
// ⚠ 这条守的是**反向误伤**：workbuddy 的 12153 含抖动成分，
//
//	一次就永久禁用会把健康号打掉（历史上真发生过）。
func TestSessionDeathWithoutPolicyKeepsGate(t *testing.T) {
	// scriptedRouter 没有实现那个可选方法 ⇒ 走门控分支。
	h, p, uid := setupSessionDeath(t, &scriptedRouter{uid: "loomy"})
	h.applyErrorPolicy(uid, gateway.ErrKindSessionDead)
	if disabledOf(t, p, "loomy", uid) {
		t.Error("上游没声明终态，一次就禁用是**过度惩罚**（瞬时抖动会被升级成永久摘号）")
	}
}
