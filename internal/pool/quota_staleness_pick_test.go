package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// quota_staleness_pick_test.go —— T-029b ④ 的三条保护（选号侧）。
//
// # 背景
//
// 冷号的额度可能已经过期，而选号是**基于额度**做决策的（`gateway/quota_consumed.go`
// 记着"拿过期值做减法 ⇒ 多扣"那次教训）。所以允许装一个"这个号额度太旧"的判据。
//
// # 为什么这三条必须逐个钉
//
// 这个判据直接挂在 `pickForTier` 的候选筛选上 —— 那是**所有上游的选号入口**。
// 判错的后果分两种，都不能发生：
//
//	太狠（把该选的都排掉）⇒ 请求失败（503 no_healthy_account）
//	太松（nil 时也生效）  ⇒ 悄悄改变了所有上游的选号行为
//
// 所以：① nil 必须逐字节不变、② 排除到空必须回退、③ 告警只打一次。

// TestStalenessCheckerNilKeepsBehaviorUnchanged 保护 1：不设判据 = 行为不变。
func TestStalenessCheckerNilKeepsBehaviorUnchanged(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetQuota("u1", QuotaView{Kind: QuotaKindCredits, HasData: true, Remaining: 10})
	p.SetQuota("u2", QuotaView{Kind: QuotaKindCredits, HasData: true, Remaining: 20})

	// 没设 checker（默认 nil）
	seen := map[string]bool{}
	for i := 0; i < 400; i++ {
		if a := p.Pick(); a != nil {
			seen[a.UID] = true
		}
	}
	for _, uid := range []string{"u1", "u2"} {
		if !seen[uid] {
			t.Errorf("nil 判据时 %s 应当照常被选中（行为必须与加这个功能之前一致）", uid)
		}
	}
}

// TestStalenessCheckerExcludesStaleAccount 判据生效：被判"太旧"的号不进候选。
func TestStalenessCheckerExcludesStaleAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "fresh"})
	p.Add(&auth.Auth{UID: "stale"})
	p.SetQuota("fresh", QuotaView{Kind: QuotaKindCredits, HasData: true, Remaining: 10})
	p.SetQuota("stale", QuotaView{Kind: QuotaKindCredits, HasData: true, Remaining: 99999})

	p.SetQuotaStalenessChecker(func(uid string, _ time.Time) bool {
		return uid == "stale"
	})
	for i := 0; i < 200; i++ {
		if a := p.Pick(); a == nil || a.UID != "fresh" {
			t.Fatalf("额度陈旧的号不该被选中（哪怕它的额度数字更大）：got %v", a)
		}
	}
}

// TestStalenessCheckerFallsBackWhenAllExcluded 保护 2：全被排除时**必须**回退。
//
// ⚠ 这是三条里最要紧的一条：没有它，"额度都旧"会变成"池子空了" ⇒ 503
// （把一种可降级的状态误报成不可用状态）。
func TestStalenessCheckerFallsBackWhenAllExcluded(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetQuota("u1", QuotaView{Kind: QuotaKindCredits, HasData: true, Remaining: 10})
	p.SetQuota("u2", QuotaView{Kind: QuotaKindCredits, HasData: true, Remaining: 20})

	// 极端判据：**所有**账号的额度都算陈旧
	p.SetQuotaStalenessChecker(func(string, time.Time) bool { return true })

	got := p.Pick()
	if got == nil {
		t.Fatal("全部被判陈旧时**必须回退为不排除** —— 宁可给一个额度旧的号，" +
			"也不能让请求失败（这正是保护 2 要挡的 503）")
	}
	if got.UID != "u1" && got.UID != "u2" {
		t.Fatalf("回退后应当给池里的正常账号，实得 %v", got.UID)
	}
	// 而且回退是**可重复**的（不是"偶尔能选到"）
	for i := 0; i < 50; i++ {
		if p.Pick() == nil {
			t.Fatalf("第 %d 次选号回退失效", i)
		}
	}
}

// TestStalenessCheckerWarnedOnce 保护 3：全排除的告警**只打一次**（不要每请求一行）。
func TestStalenessCheckerWarnedOnce(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetQuotaStalenessChecker(func(string, time.Time) bool { return true })

	if p.staleWarned {
		t.Fatal("前置条件：还没选号过，不该已告警")
	}
	_ = p.Pick()
	if !p.staleWarned {
		t.Fatal("回退发生了必须置位告警标记（否则日志每请求刷屏）")
	}
	_ = p.Pick()
	if !p.staleWarned {
		t.Fatal("标记应当保持（只打一次）")
	}
	// 重新装判据 ⇒ 允许再告警一次（新判据可能是新问题）
	p.SetQuotaStalenessChecker(func(string, time.Time) bool { return false })
	if p.staleWarned {
		t.Error("重新装判据时应当重置告警标记")
	}
}
