// panelwarmup_dead_test.go 「死号别每 20 秒打一次上游」的判据（2026-11-13 用户报障）。
//
// # 用户原话
//
//	「这些是在做什么死号根本不要这么频繁测试啊 差不多12h或者6h或者24h测试就行了不是都统一了吗」
//
// # 两个独立缺陷（一处改动同治）
//
//	A. `left` 抖动把退避反复清零（过期数在 t6/t7/t32 之间跳 ⇒ 永远回不到 5min 上限）；
//	B. 死号（凭据已废）永远补不上 ⇒ `left` 恒 > 0 ⇒ 每 20~40 秒一轮、每轮真打一次上游。
//
// # 已定决策（主脑拍板，照做）
//
//  1. 死号重试间隔**复用** `pool_revive_probe_hours`（默认 24h，config 里存秒）⇒ 不新增配置键；
//  2. 死号判定**只认凭据类终态**（`100002 登录已失效` 这种"重试必然同样失败"的）；
//     ⚠ **网络类失败（超时/连接失败/5xx）绝不算死号** —— 否则一次抖动就把号停 24 小时；
//  3. 缺陷 A 一起修（退避不得因 `left` 抖动归零）。
//
// ⚠ 全部用**假时钟**推进，不真 sleep。
package loomy

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// deadFixture 造一个"缓存空 + 两个号"的 Provider（时间由调用方给，不看真实时钟）。
func deadFixture(t *testing.T) (*Provider, []accountRef) {
	t.Helper()
	p := warmupCacheProvider(t, map[string]time.Time{}, map[string]time.Time{})
	return p, acctRefs("u1", "u2")
}

// ① 死号在重试间隔内**只被尝试一次**（假时钟推进验证）。
func TestPanelWarmupDeadAccountRetriedOncePerInterval(t *testing.T) {
	p, accts := deadFixture(t)
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	const terminal = "loomy: 积分接口 /onboarding/tasks 返回 100002: 登录已失效，请重新登录"

	// 第 1 轮：还没进缓存 ⇒ 两个号都在待办里（这是修之前的"每轮都发"）。
	pl := p.planPanelWarmup(accts, t0)
	if len(pl.jobs) == 0 {
		t.Fatal("首轮应当有待办")
	}
	// 这一轮真打了上游，且回的是"凭据类终态" ⇒ 记为死号。
	p.notePanelOutcome(acctRefs("u1")[0], terminal, t0)

	// 20 秒后（用户日志里就是这么快）**不许**再打 u1。
	pl = p.planPanelWarmup(accts, t0.Add(20*time.Second))
	for _, j := range pl.jobs {
		if j.a.UID == "u1" {
			t.Fatalf("死号在 20 秒后又被排进待办（用户报的就是这个）—— jobs=%v", pl.jobs)
		}
	}
	// 6 小时后仍然不许（用户说"6h 也行"，但我们是 24h ⇒ 6h 处必须安静）。
	pl = p.planPanelWarmup(accts, t0.Add(6*time.Hour))
	for _, j := range pl.jobs {
		if j.a.UID == "u1" {
			t.Fatalf("死号 6 小时后又被排进待办（重试间隔应当是 24h）")
		}
	}

	// 到期（24h + 1s）⇒ 放回待办**试一次**。
	pl = p.planPanelWarmup(accts, t0.Add(panelWarmupDeadRetryDefault+time.Second))
	found := false
	for _, j := range pl.jobs {
		if j.a.UID == "u1" {
			found = true
		}
	}
	if !found {
		t.Fatal("到期后应当放回待办试一次")
	}
	// 试一次又失败 ⇒ 再停一个间隔（不是"到期后每轮都试"）。
	p.notePanelOutcome(acctRefs("u1")[0], terminal, t0.Add(panelWarmupDeadRetryDefault+time.Second))
	pl = p.planPanelWarmup(accts, t0.Add(panelWarmupDeadRetryDefault+time.Hour))
	for _, j := range pl.jobs {
		if j.a.UID == "u1" {
			t.Fatal("再次失败后应当重新计时，而不是每轮都试")
		}
	}
}

// ② 回归锁：正常号（凭据好、只是缓存过期）的节奏一个字不许变。
func TestPanelWarmupHealthyAccountUnaffectedByDeadLedger(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	old := now.Add(-24 * time.Hour)
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": old, "u2": old},
		map[string]time.Time{"u1": old, "u2": old})
	accts := acctRefs("u1", "u2")

	want := p.planPanelWarmup(accts, now)
	// 另一个号死了，不该连带影响 u2（防"顺手把所有号都隔离"）。
	p.notePanelOutcome(acctRefs("u1")[0], "loomy: 100002: 登录已失效", now)
	got := p.planPanelWarmup(accts, now)

	// ⚠ 口径：摘掉一个死号 ⇒ 总数**应当**少一（这是设计要的，不是回归）。
	//   真正要锁的是"**正常号**的节奏没被牵连"。
	if got.expT != want.expT-1 || got.expI != want.expI-1 {
		t.Errorf("摘掉一个死号后过期计数应为 %d/%d，实际 %d/%d",
			want.expT-1, want.expI-1, got.expT, got.expI)
	}
	jobs := 0
	for _, j := range got.jobs {
		if j.a.UID == "u2" {
			jobs++
		}
	}
	if jobs != 2 {
		t.Errorf("正常号 u2 应当仍有 tasks + invite 两个待办，实际 %d 个", jobs)
	}
	if jobs == 0 {
		t.Error("正常号 u2 仍应被排进待办（它只是缓存过期）")
	}
	// 成功一次必须**清掉**死号标记（用户手动重登后不该再等 24h）。
	p.notePanelOutcome(acctRefs("u1")[0], "", now.Add(time.Minute))
	if _, dead := p.panelDeadUntil("u1", now.Add(time.Minute)); dead {
		t.Error("成功之后死号标记必须被清掉（否则手动重登的号要白等一个周期）")
	}
}

// ③ 回归锁：**网络类失败绝不算死号**（一次抖动不许把号停 24 小时）。
func TestPanelWarmupNetworkFailureIsNotDead(t *testing.T) {
	p, accts := deadFixture(t)
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	netErrors := []string{
		"loomy: 请求失败: context deadline exceeded",
		"loomy: 连接失败: dial tcp 1.2.3.4:443: connect: connection refused",
		"loomy: 上游 502 Bad Gateway",
		"loomy: 请求失败: EOF",
	}
	for i, msg := range netErrors {
		p.notePanelOutcome(acctRefs("u1")[0], msg, t0)
		pl := p.planPanelWarmup(accts, t0.Add(time.Duration(i+1)*time.Minute))
		found := false
		for _, j := range pl.jobs {
			if j.a.UID == "u1" {
				found = true
			}
		}
		if !found {
			t.Fatalf("网络类失败 %q 被判成死号了 —— 一次抖动就停一个周期，用户会看到「号莫名其妙的没了」", msg)
		}
	}
}

// TestPanelWarmupLogShowsDeadCount `停 N` 只在真有死号时出现（与"错误段"同一条纪律）。
func TestPanelWarmupLogShowsDeadCount(t *testing.T) {
	clean := panelWarmupLogLine(panelPlan{todoT: 0, todoI: 0, expT: 6, expI: 18}, 44, 12, "")
	if strings.Contains(clean, "停") {
		t.Errorf("没有死号时不该出现 `停 N`（噪音）：\n%s", clean)
	}
	withDead := panelWarmupLogLine(panelPlan{expT: 6, expI: 18, dead: 2}, 44, 12, "")
	if !strings.Contains(withDead, "停2") {
		t.Errorf("有死号时必须报出来（否则分不清\"补完了\"与\"被静默了\"）：\n%s", withDead)
	}
	if n := utf8.RuneCountInString(withDead); n > 100 {
		t.Errorf("带 `停 N` 仍须 ≤100 字，实际 %d", n)
	}
}

// TestPanelWarmupLogShapeUnderFakeClock ★ 硬要求：**新实现下你那种日志会长什么样**。
//
// 用假时钟把"用户那张截图里的 3 分钟"跑一遍（不真 sleep），打印每一轮真实会输出的行。
// 修之前的形态：3 分钟里 ≈ 6~9 行、每行都带错、每轮都真打上游。
// 修之后：第 1 行报出死号，之后**内容不再变** ⇒ 一行都不打（下一个心跳是 5 分钟后）。
func TestPanelWarmupLogShapeUnderFakeClock(t *testing.T) {
	p, accts := deadFixture(t)
	const terminal = "loomy: 积分接口 /onboarding/tasks 返回 100002: 登录已失效，请重新登录"
	t0 := time.Date(2026, 11, 13, 10, 16, 30, 0, time.UTC)
	now := t0
	pace := &panelWarmupPace{}
	logged := 0
	upstreamCalls := 0

	// 用户截图里的那一屏：20~40 秒一轮，跑 3 分钟。
	for i := 0; i < 6; i++ {
		pl := p.planPanelWarmup(accts, now)
		line := panelWarmupLogLine(pl, len(accts), len(pl.jobs), func() string {
			if i == 0 {
				return terminal // 第一轮真打了上游、回的是凭据类终态
			}
			return "" // 之后死号已被摘出待办 ⇒ 本轮没有错误
		}())
		_, logNow := pace.next(pl.pending(), line, now)
		if logNow {
			logged++
			t.Logf("第 %d 轮（%s）: %s", i+1, now.Format("15:04:05"), line)
		}
		if i == 0 {
			upstreamCalls += len(pl.jobs) // 只有第一轮真打了上游
			p.notePanelOutcome(accts[0], terminal, now)
		} else {
			for _, j := range pl.jobs {
				if j.a.UID == accts[0].UID {
					t.Fatalf("第 %d 轮又排了死号 %s —— 用户报的就是这个", i+1, j.a.UID)
				}
			}
		}
		now = now.Add(30 * time.Second)
	}
	t.Logf("3 分钟里：日志 %d 行，死号的上游调用 %d 次（修之前是每轮 %d 个 job 全打一遍）",
		logged, upstreamCalls/2, 4)
	if logged > 2 {
		t.Errorf("3 分钟里打了 %d 行日志（应当只有第 1 行 + 可能一条心跳）—— 刷屏没治好", logged)
	}
}

// TestPanelWarmupDeadRetryIntervalComesFromInjection 间隔由**装配层注入**（复用既有配置）。
//
// 判据两半：
//
//	· 注入了 6h ⇒ 6h 内不再试、6h 后放回待办；
//	· 注入 0（= 配置里"关闭解封探测"）⇒ 用默认 24h，**不是**"每轮都试"。
func TestPanelWarmupDeadRetryIntervalComesFromInjection(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	const terminal = "loomy: 100002: 登录已失效"

	for _, c := range []struct {
		name     string
		injected time.Duration
		want     time.Duration
	}{
		{"注入 6h", 6 * time.Hour, 6 * time.Hour},
		{"注入 12h", 12 * time.Hour, 12 * time.Hour},
		{"注入 0 ⇒ 默认 24h", 0, panelWarmupDeadRetryDefault},
	} {
		p, accts := deadFixture(t)
		p.deadRetry = c.injected
		p.notePanelOutcome(accts[0], terminal, t0)

		if _, dead := p.panelDeadUntil(accts[0].UID, t0.Add(c.want-time.Second)); !dead {
			t.Errorf("%s：%v 内不该放回待办", c.name, c.want)
		}
		pl := p.planPanelWarmup(accts, t0.Add(c.want+time.Second))
		found := false
		for _, j := range pl.jobs {
			if j.a.UID == accts[0].UID {
				found = true
			}
		}
		if !found {
			t.Errorf("%s：到期（%v）后应当放回待办试一次", c.name, c.want)
		}
	}
}
