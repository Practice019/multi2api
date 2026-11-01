package loomy

import (
	"testing"
	"time"
)

// TestResetKeepsHaveClearsZero 跨自然日：have **保留**、zero 清空。
//
// 这是修「跨日后池子空 ⇒ empty 分支全体放开 ⇒ 烧永久额度」的第一半。
// 用户的规则是"所有号免费额度用完才动永久"，而跨日那一刻我们只是
// **还不知道**今天的余额，不是"已经用完了"。
func TestResetKeepsHaveClearsZero(t *testing.T) {
	f := newFreePool()
	now := time.Now()
	f.noteQuota("HASFREE", 100, true, now)
	f.noteQuota("USEDUP", 0, true, now)

	if !f.have["HASFREE"] {
		t.Fatal("夹具错：HASFREE 应当有免费额度")
	}
	if !f.zero["USEDUP"] {
		t.Fatal("夹具错：USEDUP 应当已知用完")
	}

	tomorrow := now.Add(24 * time.Hour)
	f.resetIfNewDay(tomorrow)

	if !f.have["HASFREE"] {
		t.Error("跨日把 have 清空了 —— 池子会变空 ⇒ empty 分支全体放开 ⇒ 烧永久额度（这就是用户报的 bug）")
	}
	if f.zero["USEDUP"] {
		t.Error("跨日没清 zero —— 昨天用完的号今天可能又有，会被一直跳过")
	}
	if f.free["HASFREE"] != 0 {
		t.Error("跨日没清 free 绝对值 —— 会拿昨天的基准做减法")
	}
}

// TestCrossDayDoesNotTriggerEmptyBranch 跨日 + 池子被清空时，**不得**判成"全体 tierFree"。
//
// 判据：empty 分支把每个号都报成 tierFree（= 可以随便烧永久）。
// 修复后跨日未重填完成 ⇒ 不能宣布 empty ⇒ 号应落 tierUnsure。
func TestCrossDayDoesNotTriggerEmptyBranch(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()

	// 造：昨天所有号都"确认用完"
	p.pool().noteQuota("A", 0, true, now)
	p.pool().noteQuota("B", 0, true, now)
	// 跨到明天，并让 have 为空（模拟极端：名单也被清掉的情形）
	tomorrow := now.Add(24 * time.Hour)
	p.pool().mu.Lock()
	p.pool().resetIfNewDay(tomorrow)
	p.pool().zero["A"] = true // 让 len(zero)>0 成立，旧判据就会命中 empty
	p.pool().mu.Unlock()

	tiers := p.CandidateTiers("", []string{"A", "B"})

	for uid, tier := range tiers {
		if tier == tierFree {
			t.Errorf("跨日未重填完成却把 %s 报成 tierFree —— 这会烧永久额度；"+
				"应落 tierUnsure（我们只是还不知道）", uid)
		}
	}
}

// TestHasRefilledTodayGatesEmpty 只有「今天重填过」之后，池子空才算真的空。
func TestHasRefilledTodayGatesEmpty(t *testing.T) {
	f := newFreePool()
	now := time.Now()

	// 启动即标记"本日已填"（StartFreePoolFill 的前提）
	if !f.hasRefilledToday(now) {
		t.Fatal("新建的池子应当已标记本日已填（避免启动瞬间重复填充）")
	}

	// 跨日 ⇒ 需要重填
	tomorrow := now.Add(24 * time.Hour)
	f.resetIfNewDay(tomorrow)
	if f.hasRefilledToday(tomorrow) {
		t.Error("跨日之后 hasRefilledToday 应当为 false —— 否则 empty 判据会过早成立")
	}

	// 抢占并完成重填
	if !f.readyToRefill(tomorrow) {
		t.Fatal("跨日之后 readyToRefill 应当返回 true")
	}
	if !f.hasRefilledToday(tomorrow) {
		t.Error("readyToRefill 之后应当已标记本日已填")
	}
}

// TestReadyToRefillIsIdempotent 同一天多个并发调用只能有一个抢到（防查询洪水）。
func TestReadyToRefillIsIdempotent(t *testing.T) {
	f := newFreePool()
	tomorrow := time.Now().Add(24 * time.Hour)
	f.resetIfNewDay(tomorrow)

	wins := 0
	for i := 0; i < 10; i++ {
		if f.readyToRefill(tomorrow) {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("同一天抢到重填的次数 = %d，want 1 —— 多个会变成查询洪水（44 个号 × N）", wins)
	}
}
