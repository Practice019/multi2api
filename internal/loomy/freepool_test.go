// freepool_test.go 免费额度轮询池的语义守卫。
//
// # 这里守的是用户的一条明确规则（原话）
//
//	「我们不是要求优先使用免费的额度吗？免费额度使用，所有账号的免费额度使用
//	  完之后才能使用永久的额度，现在好像还是做不到… 因为我们是可以查询每个
//	  账号它的免费额度是多少的，当那个免费额度是0的时候，那就轮询的时候就不
//	  要使用这个账号，就应该有一个池吧，轮询池，然后它就从池子里出去，
//	  然后当所有的这个轮询池都没有账号了之后，那就说明所有的免费额度都没了，
//	  那再把那个所有的账号再重新添加回去。」
//
// # 为什么必须补这组守卫
//
// 这条规则**失效过一次**：改造前档位缓存的唯一写入方是按需的 RefreshQuota，
// 而 CandidateTiers 只读不查 ⇒ 重启后所有账号都是"不确定" ⇒ 档位完全相同
// ⇒ 毫无偏好 ⇒ 永久额度被随机烧掉。整组用例就是钉住"池子必须真的起作用"。
package loomy

import (
	"testing"
	"time"
)

func tiersOf(p *Provider, uids ...string) map[string]int {
	return p.CandidateTiers("", uids)
}

// ⚠ 一个账号都没查过时**不能**说"免费额度用完了"。
//
// 那会正好烧在用户最不想烧的地方（启动后第一波请求）。此时应落 tierUnsure：
// 不构成偏好，但也不"确认可以烧"。
func TestPoolNobodyCheckedIsUnsureNotFree(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	got := tiersOf(p, "A", "B")
	for _, uid := range []string{"A", "B"} {
		if got[uid] != tierUnsure {
			t.Errorf("★ 一个账号都没查过时 %s 应为 tierUnsure(%d)，实际 %d ——\n"+
				"\n"+
				"若这里报 tierFree，等于说\"免费额度已经用完了、可以动永久额度\"，\n"+
				"而那正是一个**还没查过任何余额**的状态 —— 启动后的第一波请求\n"+
				"会直接把永久额度烧掉。", uid, tierUnsure, got[uid])
		}
	}
}

// ⚠ 核心判据：池子里的号是唯一优先档；已知为 0 的号落到最低档。
func TestPoolMembersWinOverKnownZero(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	po.noteQuota("HAVE", 1434, true, now)  // 还有免费额度
	po.noteQuota("ZERO", 0, true, now)     // 已用完
	po.noteQuota("UNKNOWN", 0, false, now) // 没读到 ⇒ 什么都不该记

	got := tiersOf(p, "HAVE", "ZERO", "UNKNOWN")
	if got["HAVE"] != tierFree {
		t.Errorf("池子成员 HAVE 应为 tierFree，实际 %d", got["HAVE"])
	}
	if got["ZERO"] != tierPaid {
		t.Errorf("★ 已知免费额度为 0 的号应为 tierPaid(%d)，实际 %d ——\n"+
			"它是用户要求\"从池子里出去\"的那个；不落最低档就会继续被优先选中，\n"+
			"也就是继续烧永久额度。", tierPaid, got["ZERO"])
	}
	if got["UNKNOWN"] != tierUnsure {
		t.Errorf("★ DailyKnown=false 的账号应为 tierUnsure(%d)，实际 %d ——\n"+
			"「没读到」与「确实是 0」是两件事（见 points.go 的 Points.DailyKnown）。\n"+
			"把没读到记成\"已用完\"，会让一个**可能还有免费额度**的号被永久跳过。",
			tierUnsure, got["UNKNOWN"])
	}
}

// ⚠ 用户明确要的那条：池子空了 ⇒ 把所有账号放回来（允许动永久额度）。
func TestPoolEmptyMeansEveryoneAllowed(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	now := time.Now()
	po := p.pool()
	po.noteQuota("A", 0, true, now)
	po.noteQuota("B", 0, true, now)

	got := tiersOf(p, "A", "B")
	for _, uid := range []string{"A", "B"} {
		if got[uid] != tierFree {
			t.Errorf("★ 池子空了（所有已知账号的免费额度都用完）时 %s 应为 tierFree(%d)，实际 %d ——\n"+
				"用户原话：\"当所有的这个轮询池都没有账号了之后…那再把那个所有的账号\n"+
				"再重新添加回去\"。不全员放开就永远选不出号（转发会失败）。",
				uid, tierFree, got[uid])
		}
	}
}

// ⚠ 自然日一变，池子整体失效 —— 否则第二天永久额度从早烧到晚。
func TestPoolResetsOnNewDay(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	yesterday := time.Now().AddDate(0, 0, -1)
	po.noteQuota("A", 0, true, yesterday)
	po.noteQuota("B", 0, true, yesterday)

	// 同一天里读到的是"已知为 0"
	if got := tiersOf(p, "A"); got["A"] != tierFree { // 池子空 → 全员放开
		t.Logf("同一天（池子空）：A = %d（应为 %d）", got["A"], tierFree)
	}

	// 跨天之后昨天的"已用完"必须不再成立。
	// 判据：快照里(day 变了)不该再有 zero 记录 —— 用"是否仍报 paid"来看。
	po.mu.Lock()
	po.day = naturalDay(yesterday) // 假装池子记的是昨天
	po.zero["A"] = true            // 昨天用完的
	po.zero["B"] = true
	po.mu.Unlock()

	have, zero := po.snapshot(time.Now())
	if len(have) != 0 || len(zero) != 0 {
		t.Errorf("★ 跨自然日之后池子应当清空（赠送额度按自然日重置），"+
			"实际 have=%d zero=%d ——\n"+
			"不清的话，第二天所有账号都停在\"已知为 0\"那一档 ⇒ 永久额度从早用到晚，\n"+
			"正好与用户的要求相反。", len(have), len(zero))
	}
}

// ⚠ 失败不写池子，且要有退避（否则"越失败越查"）。
func TestPoolFailBackoff(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	now := time.Now()
	po.noteUsed("A", now)
	po.noteFail("A", now)

	if got := po.dueForRecheck(4, now.Add(time.Second)); len(got) != 0 {
		t.Errorf("★ 刚失败过的账号不该立刻重查（实际挑出 %v）——\n"+
			"没有这条退避就会\"越失败越查、越查越失败\"，日志里一片查询失败。", got)
	}
	if got := po.dueForRecheck(4, now.Add(freeFailBackoff+time.Second)); len(got) != 1 {
		t.Errorf("退避窗口过后应当挑出该重查的账号，实际 %v", got)
	}
}

// ⚠ 重查要节流：同一个账号 90 秒内不重复查。
//
// 这条替掉"每次对话都查一次"—— 用法驱动的查询频率必须与用量脱钩。
func TestPoolRecheckThrottled(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	now := time.Now()
	po.noteQuota("A", 100, true, now) // 刚查过 ⇒ checkedAt = now
	po.noteUsed("A", now)

	if got := po.dueForRecheck(4, now.Add(time.Second)); len(got) != 0 {
		t.Errorf("★ 刚查过的账号不该被重查（实际 %v）——\n"+
			"没有这条节流就退化成\"每次用过都查一次\"，那正是用户报的\"查询太多\"。", got)
	}
	if got := po.dueForRecheck(4, now.Add(freeRecheckEvery+time.Second)); len(got) != 1 {
		t.Errorf("节流窗口过后应当挑出该重查的账号，实际 %v", got)
	}
}

// ⚠ 已知为 0 的号被用到（说明池子空了、正在用永久额度）不必重查。
func TestPoolNoRecheckForKnownZero(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	now := time.Now()
	po.noteQuota("A", 0, true, now)
	po.noteUsed("A", now.Add(time.Hour))
	if got := po.dueForRecheck(4, now.Add(2*time.Hour)); len(got) != 0 {
		t.Errorf("已知为 0 的账号不该再被重查（实际 %v）", got)
	}
}
