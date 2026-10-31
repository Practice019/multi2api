// quota_revive_test.go 守「额度回来了 ⇒ 自动禁用的号自动解封」。
//
// # 用户报障（2026-11-01）
//
//	「monkeycode 会自动封禁账号，并且第二天的免费额度恢复之后不会重新变成
//	  正常的账号。loomy 根本不会封禁账号……你想办法怎么统一起来。」
//
// 根因（两侧都读过了）：
//
//	① applyErrorPolicy 把 HardCredit（额度耗尽）计进了自动禁用线 —— 见
//	   server/autodisable_hardcredit_test.go 的那条守卫。
//	② 即使禁了，恢复通道也只有两条：一次请求成功（号已被摘出候选集 ⇒ 永远
//	   等不到）与定期解封探测（默认 24 小时、配置里常是 0=关）。
//	   ⇒ 「额度次日恢复」这件**我们本来就会观察到**的事实，没被用来解封。
//
// 本文件钉 ② 的那条新通道：额度刷新成功且**确实可用**时立刻解封。
package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// autoDisableForTest 连续喂够阈值次失败，把账号真正禁掉（返回是否达到了禁用）。
func autoDisableForTest(t *testing.T, p *Pool, uid string) {
	t.Helper()
	disabled := false
	for i := 0; i < DefaultAutoDisableThreshold; i++ {
		if p.RecordAutoDisableFailure(uid, "upstream hard credit") {
			disabled = true
		}
	}
	if !disabled || !p.IsAutoDisabled(uid) {
		t.Fatalf("夹具前提不成立：连续 %d 次失败后账号应处于自动禁用，实际 disabled=%v",
			DefaultAutoDisableThreshold, p.IsAutoDisabled(uid))
	}
}

// 额度刷新拿到**可用**额度 ⇒ 立刻解封并回到候选集。
func TestQuotaReviveUnionAutoDisabledAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("monkeycode")
	p.SyncToDirFor("monkeycode", []*auth.Auth{{UID: "mc-1"}})
	autoDisableForTest(t, p, "mc-1")

	// 额度过期前：不可用
	if st, _ := p.AutoDisableStatus("mc-1"); !st.Disabled {
		t.Fatal("被禁用时 Status.Disabled 应为 true")
	}

	// 额度回来了（这就是次日免费额度重置的样子）
	p.SetQuota("mc-1", FromCredits(5000))

	if p.IsAutoDisabled("mc-1") {
		t.Error("★ 额度已恢复可用，账号仍处于自动禁用 —— 正是用户报的「第二天不会变回正常」")
	}
	if st, _ := p.AutoDisableStatus("mc-1"); st.Disabled {
		t.Error("★ entry.disabled 也应被清掉（否则号不进候选集）")
	}
	if a := p.PickFor("monkeycode", "", nil); a == nil || a.UID != "mc-1" {
		t.Errorf("解封后应能重新被选中，实际 %v", a)
	}
}

// 没查到 / 额度为 0 **不算可用** —— 不许解封。
//
// 放行它们的后果是"真的没额度的号被反复放回池子、每次白跑一趟"，
// 而界面上的禁用状态与事实不符。
func TestQuotaReviveRequiresUsableQuota(t *testing.T) {
	cases := []struct {
		name string
		q    QuotaView
	}{
		{"没查到（HasData=false）", QuotaView{Kind: QuotaKindCredits, HasData: false}},
		{"额度为 0", FromCredits(0)},
		{"per_model 全 0", FromPerModel(map[string]int64{"m": 0})},
	}
	for _, c := range cases {
		p := New("")
		p.SetDefaultProvider("monkeycode")
		p.SyncToDirFor("monkeycode", []*auth.Auth{{UID: "mc-1"}})
		autoDisableForTest(t, p, "mc-1")

		p.SetQuota("mc-1", c.q)

		if !p.IsAutoDisabled("mc-1") {
			t.Errorf("%s：不该解封（那会让没有额度的号反复被选中）", c.name)
		}
	}
}

// 人工禁用的号**不**被额度恢复翻案（那是操作员的决定）。
func TestQuotaReviveDoesNotOverrideManualDisable(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("monkeycode")
	p.SyncToDirFor("monkeycode", []*auth.Auth{{UID: "mc-1"}})
	p.Disable("mc-1", "人工禁用：这个号要下线了")
	if st, _ := p.AutoDisableStatus("mc-1"); !st.Disabled {
		t.Fatal("夹具前提不成立：Disable 之后 Status.Disabled 应为 true")
	}

	p.SetQuota("mc-1", FromCredits(99999))

	if st, _ := p.AutoDisableStatus("mc-1"); !st.Disabled {
		t.Error("★ 人工禁用不该被一次额度刷新解除 —— 那等于自动机制推翻人的决定")
	}
}
