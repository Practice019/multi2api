// freepool_consumed_test.go 用对话回执里的消耗驱动池子的守卫（池子侧）。
//
// 接缝层（`gateway.NewUsageTap`）负责"把回执里的数读出来"，
// 这里负责"读出来之后怎么用它" —— 而后者是**最容易编数据**的地方：
// 一个号还剩多少、扣到什么程度算用完，猜错就会让一个还有额度的号被跳过。
package loomy

import (
	"testing"
	"time"
)

// ★ 核心效果：扣到 ≤0 ⇒ 这个号**从池子里出去**（用户要的那条）。
func TestPoolConsumedMovesAccountOut(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	po.noteQuota("HAVE", 3, true, now) // 剩 3
	po.noteQuota("KEEP", 100, true, now)

	if got := p.CandidateTiers("", []string{"HAVE"}); got["HAVE"] != tierFree {
		t.Fatalf("前置：HAVE 应在池子里（实际 %d）", got["HAVE"])
	}

	po.noteConsumed("HAVE", 2, now) // 剩 1
	if got := p.CandidateTiers("", []string{"HAVE"}); got["HAVE"] != tierFree {
		t.Errorf("还剩 1 时应当仍在池子里（实际 %d）", got["HAVE"])
	}

	po.noteConsumed("HAVE", 2, now) // 1-2 = -1 ≤ 0 ⇒ 用完
	if got := p.CandidateTiers("", []string{"HAVE", "KEEP"}); got["HAVE"] != tierPaid {
		t.Errorf("★ 扣到 ≤0 之后该号应当落 tierPaid(%d)（实际 %d）——\n"+
			"\n"+
			"用户要的正是「免费额度为0就从池子里出去」。\n"+
			"不移出去它会继续被优先选中，也就是继续烧永久额度。",
			tierPaid, got["HAVE"])
	}
}

// ⚠ 没有绝对值基准时**什么都不做**：凭空减等于造数，而造出来的"已用完"
// 会让一个还有免费额度的号被跳过。
func TestPoolConsumedWithoutBaselineDoesNothing(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	po.mu.Lock()
	po.have["A"] = true // 在池子里，但**没有** free 绝对值
	po.mu.Unlock()

	po.noteConsumed("A", 9999, time.Now())

	if have, _ := po.snapshot(time.Now()); !have["A"] {
		t.Error("★ 没有绝对值基准时不该把账号移出池子 ——\n" +
			"那等于凭一个猜出来的数说「免费额度用完了」，\n" +
			"而它可能还有很多。")
	}
}

// ⚠ 对**完全未知**的账号扣减不该把它标成 known-0（未知 ≠ 已用完）。
func TestPoolConsumedIgnoresKnownZero(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	po.noteConsumed("ZERO", 5, time.Now())
	if _, zero := po.snapshot(time.Now()); zero["ZERO"] {
		t.Error("★ 对未知账号扣减把它标成了 known-0 ——\n" +
			"「未知」与「已用完」是两件事。标成已用完会让一个**可能还有免费额度**的号\n" +
			"被跳过（而当池子里再没有别的号时，整池都会被判成「免费额度用完」，\n" +
			"于是直接开始烧永久额度）。")
	}
}

// ⚠ 跨自然日：绝对值基准必须清空。
//
// 昨天的余额今天不成立 —— 拿一个过期的基准继续做减法，会把今天的额度
// 判错方向（可能早用完，也可能明明用完了还留着）。
func TestPoolConsumedBaselineClearedOnNewDay(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	yesterday := time.Now().AddDate(0, 0, -1)
	po.noteQuota("A", 100, true, yesterday)
	po.mu.Lock()
	po.day = naturalDay(yesterday) // 假装池子记的是昨天
	po.have["A"] = true
	po.free["A"] = 100
	po.mu.Unlock()

	po.noteConsumed("A", 1, time.Now()) // 跨天后的第一次扣减

	po.mu.Lock()
	left, hasBaseline := po.free["A"]
	po.mu.Unlock()
	if hasBaseline {
		t.Errorf("★ 跨自然日之后不该还留着昨天的绝对值基准（剩 %d）——\n"+
			"拿过期的基准做减法会把今天的额度判错方向。", left)
	}
}
