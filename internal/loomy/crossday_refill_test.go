package loomy

import (
	"testing"
	"time"
)

// TestResetClearsHaveAndZero 跨自然日：have / zero / free **全部清空**。
//
// # 契约变更（2026-11-10）
//
// 此前 have 是**保留**的（2026-11-02 起），那是一条绕开 empty 缺陷的权宜之计：
// 那时的 empty 判据把"没有任何号被确认有免费额度"当成了"所有号都没有"，
// 清空 have 正好让它成立 ⇒ 全体放开 ⇒ 烧永久额度。
//
// empty 已修（要求本批候选一个未探测的都没有，见 TestPoolEmptyRequiresNoUnknown），
// 于是保留 have 只剩坏处：它的号**没有今日基准**（free 已清）⇒ noteConsumed 扣不动，
// 而 noteFail 又不会把它移出 have ⇒ 查询持续失败的号会**长期**停在被声称
// "有免费额度"那一档，每次都被优先选中、消耗记录却是空的。
//
// 新不变式：**have[uid] ⇒ free[uid] 存在（今日基准）** —— 声称有免费额度就必须
// 能被扣减。
//
// 变异验证：把 resetIfNewDay 里的 `f.have = map[string]bool{}` 去掉 ⇒ 本测试红。
func TestResetClearsHaveAndZero(t *testing.T) {
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

	if len(f.have) != 0 {
		t.Errorf("跨日没清 have（剩 %d 个）—— 那些号没有今日基准、扣不动，"+
			"却仍被声称「有免费额度」并优先选中", len(f.have))
	}
	if f.zero["USEDUP"] {
		t.Error("跨日没清 zero —— 昨天用完的号今天可能又有，会被一直跳过")
	}
	if f.free["HASFREE"] != 0 {
		t.Error("跨日没清 free 绝对值 —— 会拿昨天的基准做减法")
	}
	// 不变式：不存在"声称有免费额度但没有基准"的号。
	for uid := range f.have {
		if _, ok := f.free[uid]; !ok {
			t.Errorf("不变式被破坏：%s 在 have 里但没有 free 基准", uid)
		}
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
