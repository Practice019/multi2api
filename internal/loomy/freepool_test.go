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
// localNoon 把"当前时刻"钉到**当天中午**，给所有要 `now + 一段` 的用例当基准。
//
// ⚠⚠ 这是本文件**窗口型 flaky** 的解药，而那个窗口叫"午夜前 5 分钟"。
//
// # 机制（2026-10-18 实测，同一类坑踩了两次）
//
// 池子按**自然日**重置（`resetIfNewDay`）。所以任何"拿 `time.Now()` 再加一段时间"
// 的断言，都在**隐含地断言"加完之后还在同一天"**。而这句话在 23:55 之后不成立：
//
//	now = 23:59:40  ⇒  now + freeFailBackoff(5min) + 1s = 次日 00:04:41
//	⇒ dueForRecheck 里 resetIfNewDay 判定跨日 ⇒ **清空整个池子** ⇒ 返回 []
//	⇒ "退避窗口过后应当挑出该重查的账号"失败，而**被测代码完全正确**
//
// ⚠ 各用例的红线窗口：`TestPoolFailBackoff` 是 **23:55:00–24:00:00**；
//
//	`TestPoolRecheckThrottled` 是 **23:58:30–24:00:00**（它是 90 秒）。
//	即**每天有一个 5 分钟的窗口会红，而白天跑永远绿** —— 这正是它活得久的原因。
//
// ⚠ 钉到中午的余量：本文件的加时最长 2 小时，正午 ± 12 小时都还在当天之内。
func localNoon(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, t.Location())
}

// ⚠ 退避的**实现细节**（noteFail 不删 needRecheck 标记，只靠 failAt 判窗口）
// 由下面这个用例守着 —— 它是本文件里唯一会碰"午夜红线"的用例之一。
func TestPoolFailBackoff(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	po := p.pool()
	now := localNoon(time.Now())
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
	now := localNoon(time.Now())
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
	// ⚠⚠ **用"当天中午"而不是 time.Now()**（2026-10-18 修，本测试的 flaky 根因）。
	//
	// 原来用 `time.Now()`，而它后面要 +1h / +2h：
	// 本地时间**22:00 之后**跑，`now+2h` 就跨过自然日 ⇒ `dueForRecheck` 里的
	// `resetIfNewDay` 把池子整体清空（`zero` 集合没了）⇒ `zero[uid]` 不再是 true
	// ⇒ A 被当成"待重查" ⇒ **本测试只在晚上红**。
	//
	// ⚠ 这不是实现的问题：`noteUsed` 早就对 `f.zero[uid]` 直接 return（跳过重查），
	//   实现是对的。⚠ 教训：**凡是要对时间做加减的测试，基准必须钉在"当天中间"**，
	//   否则跑测试的钟点会变成一个隐形的输入。
	midnight := time.Now()
	now := localNoon(midnight)
	po.noteQuota("A", 0, true, now)
	po.noteUsed("A", now.Add(time.Hour))
	if got := po.dueForRecheck(4, now.Add(2*time.Hour)); len(got) != 0 {
		t.Errorf("已知为 0 的账号不该再被重查（实际 %v）", got)
	}
}
