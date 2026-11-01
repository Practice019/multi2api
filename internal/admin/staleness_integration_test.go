package admin

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// staleness_integration_test.go —— T-029d ③：把 admin 的陈旧判据**接到 pool 上**跑一遍。
//
// # 为什么需要集成级用例（而不是只测 they 各自的纯函数）
//
// 两侧各自绿是**不够**的：判据的签名、语义、以及"排除到空要回退"这三件事，
// 只有在真的把它塞进 `pool.Pick()` 里才验证得到（编译期只保签名的形状，
// 保不住"stale 的到底被排除了没有"）。
//
// ⚠ 不联网：池子是内存构造的，判据只看 admin 的账本。

// TestStalenessCheckerIntegrationWithPick 严重过期被排除、新鲜的仍可选中。
func TestStalenessCheckerIntegrationWithPick(t *testing.T) {
	resetQuotaTierStateForTest()
	defer resetQuotaTierStateForTest()

	now := time.Now()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "fresh"})
	p.Add(&auth.Auth{UID: "stale"})
	p.SetQuota("fresh", pool.QuotaView{Kind: pool.QuotaKindCredits, HasData: true, Remaining: 10})
	// ⚠ stale 那个号的额度数字**更大** —— 这样"没选中它"只能用陈旧判据解释，
	//   不能被"额度小所以没人选"混淆（这条正是判据存在的理由：别拿旧数字决策）。
	p.SetQuota("stale", pool.QuotaView{Kind: pool.QuotaKindCredits, HasData: true, Remaining: 999999})

	const uidFresh, uidStale = "fresh", "stale"
	// ① 两个号都"刚刚刷过"（冷号：容忍 2×6h=12h）⇒ 都不该被判陈旧
	quotaTier.markSeen([]string{uidFresh, uidStale}, nil,
		map[string]time.Time{uidFresh: now.Add(-5 * time.Hour), uidStale: now.Add(-5 * time.Hour)}, now)
	p.SetQuotaStalenessChecker(StalenessChecker())

	seenFresh, seenStale := false, false
	for i := 0; i < 200; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatal("两个号都新鲜 ⇒ 不该选不到号")
		}
		if a.UID == uidFresh {
			seenFresh = true
		}
		if a.UID == uidStale {
			seenStale = true
		}
	}
	if !seenFresh || !seenStale {
		t.Fatalf("都新鲜时必须都能被选中（fresh=%v stale=%v）—— 否则判据太狠了", seenFresh, seenStale)
	}

	// ② 把 stale 那个改成"严重过期"（冷号容忍 12h ⇒ 距上次刷新 20h 算严重）
	quotaTier.markSeen([]string{uidStale}, nil,
		map[string]time.Time{uidStale: now.Add(-20 * time.Hour)}, now.Add(-20*time.Hour))
	for i := 0; i < 100; i++ {
		a := p.Pick()
		if a == nil || a.UID != uidFresh {
			t.Fatalf("严重过期的号不该被选中（哪怕它额度数字更大）：第 %d 次 got %v", i, a)
		}
	}
}

// TestStalenessCheckerIntegrationAllStaleFallsBack 两个都严重过期 ⇒ 回退必须生效。
//
// ⚠ 这是本次接线最危险的一种输入：**全池都被排除**。若回退失效，表现是
// `Pick()==nil` ⇒ 请求失败（把"额度旧"误报成"没号可用"）。
func TestStalenessCheckerIntegrationAllStaleFallsBack(t *testing.T) {
	resetQuotaTierStateForTest()
	defer resetQuotaTierStateForTest()

	now := time.Now()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetQuota("a", pool.QuotaView{Kind: pool.QuotaKindCredits, HasData: true, Remaining: 1})
	p.SetQuota("b", pool.QuotaView{Kind: pool.QuotaKindCredits, HasData: true, Remaining: 2})
	// 两个都是冷号 + 距上次刷新 20 小时（> 12h 容忍）⇒ 判据会排除它们两个
	old := now.Add(-20 * time.Hour)
	quotaTier.markSeen([]string{"a", "b"}, nil, map[string]time.Time{"a": old, "b": old}, old)
	p.SetQuotaStalenessChecker(StalenessChecker())

	for i := 0; i < 50; i++ {
		if a := p.Pick(); a == nil {
			t.Fatalf("全池都被判严重过期时**必须回退**（第 %d 次拿到 nil）——"+
				"宁可给一个额度旧的号，也不能让请求失败", i)
		}
	}
}
