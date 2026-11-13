package loomy

import (
	"testing"
	"time"
)

// TestPanelWarmupPaceBacksOffWhenNothingChanges 钉住两件事：
//
//	① 待办数**没有变化**时，间隔必须乘性拉长到上限；
//	② 一旦变化，必须立刻回到追赶间隔（不能"退避了就一直在慢车道"）。
//
// 背景（2026-11-12，用户报"日志在刷屏"）：死号永远补不上 ⇒ 待办恒定 ⇒
// 旧实现每 5 秒一轮、每轮 12 个上游请求、每轮一条同样的日志，永不停止。
func TestPanelWarmupPaceBacksOffWhenNothingChanges(t *testing.T) {
	// ⚠ 样例改用**新格式**（2026-11-13 精简后）；`next` 把整行当不透明 key，
	//   所以格式本身不影响判定 —— 这一点由 TestPanelWarmupPaceIgnoresLogFormat 单独锁住。
	const line = "loomy: 预热 · 未缓存 t0/i0 · 过期 t6/i18 · 号24 · 发起12"
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	p := &panelWarmupPace{}

	// 首轮：追赶间隔，必须打日志（内容第一次出现）。
	if got, logNow := p.next(24, line, now); got != panelWarmupBusyInterval || !logNow {
		t.Fatalf("首轮：sleep=%v log=%v，want %v true", got, logNow, panelWarmupBusyInterval)
	}
	// 待办不变 ⇒ 逐轮翻倍，直到上限；且内容没变、未到心跳 ⇒ **不再打日志**（这就是"不刷屏"）。
	want := panelWarmupBusyInterval
	for i := 0; i < 12; i++ {
		want *= 2
		if want > panelWarmupMaxRetryInterval {
			want = panelWarmupMaxRetryInterval
		}
		// ⚠ 假时钟**独立于**返回的 sleep 前进（每秒一格）：
		//   若按 want 前进，累计到 panelWarmupLogHeartbeat 就会**正确地**触发心跳 ——
		//   那是心跳在起作用，不是「不刷屏」失效（第一版就是这么误判的）。
		now = now.Add(time.Second)
		got, logNow := p.next(24, line, now)
		if got != want {
			t.Fatalf("第 %d 轮退避：sleep=%v want %v", i+1, got, want)
		}
		if logNow {
			t.Fatalf("第 %d 轮：内容没变、也没到心跳，不该打日志（否则就是刷屏）", i+1)
		}
	}
	if p.interval != panelWarmupMaxRetryInterval {
		t.Fatalf("退避应封顶在 %v，实际 %v", panelWarmupMaxRetryInterval, p.interval)
	}

	// 待办降了（补上了）⇒ 立刻回到追赶间隔，并且内容变了要立刻打日志。
	got, logNow := p.next(23, line+" · 发起1", now)
	if got != panelWarmupBusyInterval {
		t.Fatalf("待办变化后应回到追赶间隔 %v，实际 %v", panelWarmupBusyInterval, got)
	}
	if !logNow {
		t.Fatal("待办变化后必须立刻打日志（否则「变化」看不见）")
	}

	// 待办归零 ⇒ 稳定态间隔（与追赶/退避无关）。
	if got, _ := p.next(0, line, now); got != panelWarmupIdleInterval {
		t.Fatalf("待办归零应等稳定态 %v，实际 %v", panelWarmupIdleInterval, got)
	}
}

// TestPanelWarmupPaceLogsHeartbeatWhenIdle 钉住心跳：内容长期不变时也必须偶尔出声，
// 否则排障看到的就是"日志停了"，分不清"在退避"和"goroutine 死了"。
func TestPanelWarmupPaceLogsHeartbeatWhenIdle(t *testing.T) {
	const line = "loomy: 预热 · 未缓存 t0/i0 · 过期 t6/i18 · 号24 · 发起12"
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	p := &panelWarmupPace{}
	if _, logNow := p.next(24, line, now); !logNow {
		t.Fatal("首轮应打日志")
	}
	// 心跳前 1 秒：不打。
	if _, logNow := p.next(24, line, now.Add(panelWarmupLogHeartbeat-time.Second)); logNow {
		t.Fatal("未到心跳不该打")
	}
	// 到了心跳：打。
	if _, logNow := p.next(24, line, now.Add(panelWarmupLogHeartbeat)); !logNow {
		t.Fatal("到心跳应打一条（用来区分「在退避」与「goroutine 死了」）")
	}
}

// TestPanelWarmupPaceIgnoresLogFormat ★ 功能回归锁：日志换格式**不许**改变节奏判定。
//
// 为什么要有这条：`next` 把日志行当"内容是否变化"的 key（`p.lastLine != line`）⇒
// 2026-11-13 那次精简（整行重写）**有可能**悄悄改掉这个行为（例如顺手把 key 抽成某个数字），
// 而症状是"该打日志时不打"或"每轮都打"—— 两种都不报错。
//
// 判据：同一串 left 与时间序列，**旧格式与新格式的 (sleep, logNow) 必须逐项相同**。
func TestPanelWarmupPaceIgnoresLogFormat(t *testing.T) {
	const oldLine = "loomy: 面板预热一轮 —— 待办：未缓存(tasks=0 invite=0) · 过期(tasks=6 invite=18) · 共 24 个号 · 本轮发起 12 个"
	const newLine = "loomy: 预热 · 未缓存 t0/i0 · 过期 t6/i18 · 号24 · 发起12"
	oldP, newP := &panelWarmupPace{}, &panelWarmupPace{}
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	// ⚠ 序列里同时含"不变"(24 反复)、"变了"(23)、"归零"(0) 三种情形。
	for i, left := range []int{24, 24, 24, 23, 23, 0, 0} {
		sOld, lOld := oldP.next(left, oldLine, now)
		sNew, lNew := newP.next(left, newLine, now)
		if sOld != sNew || lOld != lNew {
			t.Fatalf("第 %d 步（left=%d）：旧格式 (%v,%v) ≠ 新格式 (%v,%v) —— 换格式改变了节奏/打日志判定",
				i+1, left, sOld, lOld, sNew, lNew)
		}
		now = now.Add(time.Second)
	}
	// 再单独钉一次语义：新格式下"内容变了"必须立刻打（否则变化看不见）。
	if _, logNow := newP.next(23, newLine+" · 发起1", now); !logNow {
		t.Error("新格式：内容变化后必须立刻打日志")
	}
}

// TestPanelWarmupPaceBackoffIgnoresLeftJitter ★ 缺陷 A：`left` 抖动**不许**把退避清零。
//
// 用户日志实测的序列（20~40 秒一轮、永不停止）：
//
//	10:16:30 过期 t7/i18 ⇒ 10:16:50 过期 t6/i18 ⇒ 10:21:50 过期 t32/i18 ⇒ 10:22:10 过期 t6/i18
//
// 过期数本来就会来回跳（一轮里刷过的号下一轮又到期、或者新号从别处掉进来），
// 而旧判据是"`left` 变了就回到追赶间隔" ⇒ **退避永远清零** ⇒ 5 秒一轮打上游。
//
// 判据：喂一串"降一点又弹回去"的 left ⇒ 间隔必须**继续增长**（不许回到 5s）。
func TestPanelWarmupPaceBackoffIgnoresLeftJitter(t *testing.T) {
	const line = "loomy: 预热 · 未缓存 t0/i0 · 过期 t6/i18 · 号44 · 发起12"
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	p := &panelWarmupPace{}
	// 首轮：追赶间隔。
	if got, _ := p.next(20, line, now); got != panelWarmupBusyInterval {
		t.Fatalf("首轮 sleep=%v，want %v", got, panelWarmupBusyInterval)
	}
	// ⚠ 上面那圈只是"跑一跑"，真判据在下面：连续"涨"必须**不退避也不清零**。
	p = &panelWarmupPace{}
	now = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	_, _ = p.next(20, line, now)
	now = now.Add(time.Second)
	// 先退避两轮（left 不变），拿到一个明显大于 5s 的间隔。
	_, _ = p.next(20, line, now)
	now = now.Add(time.Second)
	big, _ := p.next(20, line, now)
	if big <= panelWarmupBusyInterval {
		t.Fatalf("两轮没变化后应当已经退避（>5s），实际 %v", big)
	}
	// 现在喂"涨"（新出现过期的）—— 修之前这里会掉回 5s。
	now = now.Add(time.Second)
	got, _ := p.next(21, line, now)
	if got == panelWarmupBusyInterval {
		t.Fatalf("`left` 只是**涨**了一下，间隔就掉回追赶态 %v —— 这正是用户日志里「永不停止」的成因", got)
	}
	if got < big {
		t.Errorf("`left` 涨了之后间隔不该变小：之前 %v，现在 %v", big, got)
	}
	// 而真的"降"（补上了）必须**立刻**回到追赶间隔。
	now = now.Add(time.Second)
	if got, _ := p.next(10, line, now); got != panelWarmupBusyInterval {
		t.Errorf("`left` 降了（补上了）就该回到追赶间隔 %v，实际 %v", panelWarmupBusyInterval, got)
	}
}
