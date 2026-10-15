package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// quota_consumed_test.go `ApplyQuotaConsumed` 的守卫。
//
// # 这里守的是"上游上报了消耗之后，怎么应用到额度上"
//
// 这是**最容易编数据**的一步：扣多了会把还有额度的账号显示成用完，
// 扣错了形态（unlimited / per_model）会凭空造出一个可减的数。
func quotaFixture(t *testing.T, kind string) (*Pool, string) {
	t.Helper()
	const uid = "u-consume"
	p := New("")
	p.SetDefaultProvider("qoder")
	p.SyncToDirFor("qoder", []*auth.Auth{{UID: uid}})
	switch kind {
	case "credits":
		p.SetQuota(uid, FromCredits(100))
	case "per_model":
		p.SetQuota(uid, QuotaView{Kind: QuotaKindPerModel,
			ByModel: map[string]int64{"m": 50}, HasData: true})
	case "unlimited":
		p.SetQuota(uid, QuotaView{Kind: QuotaKindUnlimited, HasData: true})
	}
	return p, uid
}

func quotaOf(t *testing.T, p *Pool, uid string) QuotaView {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("找不到账号 %s", uid)
	}
	return st.Quota
}

// ★ 基本效果：单值额度按单位递减。
func TestApplyQuotaConsumedDecrementsCredits(t *testing.T) {
	p, uid := quotaFixture(t, "credits")
	before := quotaOf(t, p, uid).Remaining
	if before != 100 {
		t.Fatalf("前置不成立：初始额度 %d，期望 100", before)
	}

	p.ApplyQuotaConsumed(uid, 3)

	if got := quotaOf(t, p, uid).Remaining; got != 97 {
		t.Errorf("★ 扣 3 之后应剩 97，实际 %d", got)
	}
}

// ⚠ 扣过头要**钳到 0**，不能显示负数。
//
// 负余额是记账噪声（多扣、跨进程竞态都会造成），而它在界面上会被读成
// "这个号欠费了" —— 一个看起来合理但完全错误的结论。
func TestApplyQuotaConsumedClampsAtZero(t *testing.T) {
	p, uid := quotaFixture(t, "credits")
	p.ApplyQuotaConsumed(uid, 999)

	if got := quotaOf(t, p, uid).Remaining; got != 0 {
		t.Errorf("★ 扣过头应钳到 0，实际 %d ——负余额会被读成「欠费」", got)
	}
}

// ⚠ **形态不对就不许扣**：per_model 没有"一个可减的数"，unlimited 减一个
// 不限额的额度更是荒谬。硬减就是编数据。
//
// ⚠ 夹具刻意造得**自相矛盾**（kind=per_model/unlimited，但 Remaining 非零）——
// 因为**只有这样才能判别**：
//
//	若 Remaining 是 0（正常形态），扣它之后钳到 0 仍是 0 ⇒ 看不出任何区别
//	⇒ 去掉形态检查的变异**也能通过**（我第一版就是这样，变异验证把它抓出来了）。
//
// 而"kind 与字段矛盾"恰恰是**有 bug 的写入方**会产生的形状（例如某个上游
// 用 SetQuota 塞了一个带 Remaining 的 per_model 视图）—— 断言我们不去动它，
// 就是在守"不编数据"这条线。
func TestApplyQuotaConsumedSkipsWrongKinds(t *testing.T) {
	for _, tc := range []struct {
		kind string
		q    QuotaView
	}{
		{"per_model", QuotaView{Kind: QuotaKindPerModel,
			Remaining: 42, ByModel: map[string]int64{"m": 50}, HasData: true}},
		{"unlimited", QuotaView{Kind: QuotaKindUnlimited,
			Remaining: 42, HasData: true}},
	} {
		const uid = "u-consume"
		p := New("")
		p.SetDefaultProvider("qoder")
		p.SyncToDirFor("qoder", []*auth.Auth{{UID: uid}})
		p.SetQuota(uid, tc.q)

		before, _ := p.Status(uid)
		p.ApplyQuotaConsumed(uid, 5)
		after, _ := p.Status(uid)

		if after.Quota.Kind != before.Quota.Kind {
			t.Errorf("%s：额度形态被改成了 %s（不许动形态）", tc.kind, after.Quota.Kind)
		}
		if after.Quota.Remaining != before.Quota.Remaining {
			t.Errorf("★ %s 形态不该被扣减：Remaining %d → %d ——\n"+
				"per_model 没有「一个可减的数」；unlimited 减一个不限额的额度\n"+
				"更是荒谬。硬减就是编数据（而这条只有 Remaining 非零时才看得出来）。",
				tc.kind, before.Quota.Remaining, after.Quota.Remaining)
		}
		if after.Quota.ByModel["m"] != before.Quota.ByModel["m"] {
			t.Errorf("%s：ByModel 被改了（%d → %d）",
				tc.kind, before.Quota.ByModel["m"], after.Quota.ByModel["m"])
		}
	}
}

// ⚠ 未知 uid / 非正数：静默 no-op（不 panic、不改任何东西）。
func TestApplyQuotaConsumedNoOps(t *testing.T) {
	p, uid := quotaFixture(t, "credits")
	before := quotaOf(t, p, uid).Remaining

	p.ApplyQuotaConsumed("不存在的-uid", 3) // 不存在的账号
	p.ApplyQuotaConsumed(uid, 0)        // 0
	p.ApplyQuotaConsumed(uid, -5)       // 负数
	p.ApplyQuotaConsumed("", 3)         // 空 uid

	if got := quotaOf(t, p, uid).Remaining; got != before {
		t.Errorf("这些调用都该是 no-op（%d → %d）", before, got)
	}
}
