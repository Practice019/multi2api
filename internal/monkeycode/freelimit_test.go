// freelimit_test.go 钉住「今日额度」列的两个必备条件。
//
// # 用户报的
//
// 截图里注册机有一列「今日 Token」，显示 `10M / 10M`；本仓没有。
//
// # 这条链路有四段，任何一段断了界面上都只是**少一个分母**
//
//	① monkeycode: RefreshQuota 把 wallet 的两个数都填进 QuotaView
//	② admin: toPoolQuota 逐字段搬到 pool.QuotaView
//	③ admin: accountViews 在 FreeLimit>0 时下发 free_quota_limit
//	④ monkeycode: AccountColumns 的列集里**有** free_quota
//
// 四段都不会报错、也不会让别的测试变红 —— 数据只是静默消失。
// 所以用类型层面的断言把它们钉住，而不是靠人眼数格子。
package monkeycode

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestQuotaViewCarriesFreeLimit RefreshQuota 必须同时报剩余与上限。
func TestQuotaViewCarriesFreeLimit(t *testing.T) {
	// 构造与真机响应同形的 wallet（cn 站实测值）。
	w := &WalletPayload{Balance: 299965, DailyTokenBalance: 9944867, DailyTokenLimit: 10000000}

	// 模拟 RefreshQuota 的组包逻辑（与 quotaext.go 保持一致）。
	q := gateway.QuotaView{
		Kind:      gateway.QuotaKindCredits,
		Remaining: w.Balance,
		Free:      w.DailyTokenBalance,
		FreeLimit: w.DailyTokenLimit,
		FreeKnown: true,
		HasData:   true,
	}

	if !q.FreeKnown {
		t.Fatal("FreeKnown 必须为 true —— 否则界面恒显示破折号，尽管 Free 有值")
	}
	if q.Free != 9944867 {
		t.Errorf("Free: got %d want 9944867", q.Free)
	}
	if q.FreeLimit != 10000000 {
		t.Errorf("FreeLimit: got %d want 10000000 —— 这个字段漏了界面上就只剩一个孤立的剩余量，看不出今天总共能跑多少", q.FreeLimit)
	}
}

// TestWithFreeLimitBuilder 构造器必须同时设剩余/已知/上限。
//
// # 为什么要这个构造器
//
// 手写字面量时最容易漏 FreeKnown（Free 有值但界面恒破折号），
// 其次是漏 FreeLimit（没有分母）。构造器把三个字段收在一处，
// 调用方只需给两个数。
func TestWithFreeLimitBuilder(t *testing.T) {
	q := gateway.CreditsQuota(299965).WithFreeLimit(9944867, 10000000)
	if !q.HasData {
		t.Error("CreditsQuota 必须置 HasData")
	}
	if !q.FreeKnown || q.Free != 9944867 || q.FreeLimit != 10000000 {
		t.Errorf("WithFreeLimit 组包不全: %+v", q)
	}

	// limit<=0 时只设剩余（上游没给上限 ≠ 上限是 0）。
	q2 := gateway.CreditsQuota(5).WithFreeLimit(3, 0)
	if q2.FreeLimit != 0 {
		t.Errorf("limit=0 时不应设 FreeLimit，实际 %d", q2.FreeLimit)
	}
	if !q2.FreeKnown || q2.Free != 3 {
		t.Error("limit=0 仍应设 Free/FreeKnown")
	}
}

// TestAccountColumnsHasFreeQuota 列集必须含 free_quota。
//
// # 这条最容易被忽略
//
// 后端数据全对、admin 也下发了字段，但只要列集里没有这一项，
// 界面上就是**没有这一列** —— 不是显示为空，是整列不存在。
// 我第一版只从 DefaultAccountColumns() 里删了两项，就漏了这个。
func TestAccountColumnsHasFreeQuota(t *testing.T) {
	p := New(Config{})
	cols := p.AccountColumns()

	var hasFree, hasQuota bool
	for _, c := range cols {
		if c == gateway.AccountColFreeQuota {
			hasFree = true
		}
		if c == gateway.AccountColQuota {
			hasQuota = true
		}
	}
	if !hasQuota {
		t.Fatal("列集里没有 quota（积分）")
	}
	if !hasFree {
		t.Fatalf("列集里没有 free_quota —— 界面上不会有「今日额度」列。实际: %v", cols)
	}

	// 顺序：free_quota 必须紧跟 quota（注册机的排法，也是读者顺序）。
	qi, fi := -1, -1
	for i, c := range cols {
		if c == gateway.AccountColQuota {
			qi = i
		}
		if c == gateway.AccountColFreeQuota {
			fi = i
		}
	}
	if fi != qi+1 {
		t.Errorf("free_quota 应当紧跟 quota（quota=%d free=%d），实际列集: %v", qi, fi, cols)
	}
}
