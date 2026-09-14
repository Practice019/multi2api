// cooldown_softrate_test.go 软限流指数退避 / 6004 模型级豁免 / 12153 三次门控。
//
// # 三条判据共同的主题：**冷却的"程度"必须与实际故障的"程度"匹配**
//
//	软退避   屡次被限流 → 冷得越来越久（而不是每次都 60s）
//	模型豁免 只被单模型限流 → 不该把整个账号冷掉
//	三次门控 瞬时抖动 → 不该把健康的号永久禁用
//
// 三条的反面都是"过度惩罚"：一个健康的（或只是局部受限的）账号
// 被当成整体故障处理。这类错误在界面上表现为"这个号废了"，
// 而真因（抖动 / 单模型限额）完全不可见。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newSoftPool 单个账号的池子，软限流基数为 d。
func newSoftPool(t *testing.T, d time.Duration) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.SetCredits("u1", 1000)
	return p
}

// cooldownLeft 返回账号当前冷却剩余时长（不在冷却返回 0）。
func cooldownLeft(t *testing.T, p *Pool, uid string) time.Duration {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("池里没有 %s", uid)
	}
	return time.Until(st.Until)
}

// TestSoftCooldownFirstIsUnchanged 首次软冷却时长**与改造前逐字节一致**。
//
// 这是"指数退避"必须守住的边界：改造前的语义是"429 → 冷却 soft_rate"，
// 一条 429 的部署行为不能因为引入退避而变化，否则第一次撞限流的号
// 会突然被冷得比预期久 —— 一个没人会往"退避"上联想的回归。
func TestSoftCooldownFirstIsUnchanged(t *testing.T) {
	const base = 60 * time.Second
	p := newSoftPool(t, base)
	p.Cooldown("u1", CoolSoft, base, "429")

	left := cooldownLeft(t, p, "u1")
	// 允许一点执行耗时误差；关键是"约等于 base"而不是 2×base。
	if left > base || left < base-2*time.Second {
		t.Errorf("首次软冷却剩余 %v，期望约 %v（第一次不得被放大）", left, base)
	}
}

// TestSoftCooldownBacksOffExponentially 连续软限流按 2 的幂放大。
func TestSoftCooldownBacksOffExponentially(t *testing.T) {
	const base = time.Minute
	p := newSoftPool(t, base)
	p.SetSoftRateMax(time.Hour) // 抬高封顶，让前几档不被夹住

	want := []time.Duration{base, 2 * base, 4 * base, 8 * base}
	for i, w := range want {
		p.Cooldown("u1", CoolSoft, base, "429")
		left := cooldownLeft(t, p, "u1")
		if left > w || left < w-2*time.Second {
			t.Fatalf("第 %d 次软冷却剩余 %v，期望约 %v（应逐次翻倍）", i+1, left, w)
		}
	}
}

// TestSoftCooldownCappedAtSoftRateMax 指数放大被 softRateMax 夹住。
func TestSoftCooldownCappedAtSoftRateMax(t *testing.T) {
	const base = time.Minute
	const max = 10 * time.Minute
	p := newSoftPool(t, base)
	p.SetSoftRateMax(max)

	// 连打足够多次，让 2 的幂远超封顶。
	for i := 0; i < 12; i++ {
		p.Cooldown("u1", CoolSoft, base, "429")
	}
	left := cooldownLeft(t, p, "u1")
	if left > max || left < max-2*time.Second {
		t.Errorf("多次软冷却后剩余 %v，期望被夹在 %v（封顶失效会让号被无限冷下去）", left, max)
	}
}

// TestSoftCooldownNeverNegativeFromOverflow 极端连续次数不得因左移溢出变成"过去时刻"。
//
// # 这是最容易静默出错的一条
//
// time.Duration 是 int64 纳秒，无上限左移会溢出成**负数** ——
// 冷却截止落在过去，账号立刻又可选，且没有任何日志线索。
// 表现为"这个被限流几十次的号突然满血复活"，极难归因。
func TestSoftCooldownNeverNegativeFromOverflow(t *testing.T) {
	p := newSoftPool(t, time.Minute)

	// 直接构造极大 streak：反复调用会受封顶影响，但溢出发生在左移那一步，
	// 因此这里绕过封顶检查，用最小封顶 + 极多调用把 streak 推高。
	p.SetSoftRateMax(time.Nanosecond)
	for i := 0; i < 200; i++ {
		p.Cooldown("u1", CoolSoft, time.Minute, "429")
	}
	st, _ := p.Status("u1")
	if !st.Until.After(time.Now().Add(-time.Second)) {
		t.Fatalf("冷却截止落在过去（%v）—— 左移溢出成负数了", st.Until)
	}
	// 也必须仍在冷却中（封顶是最小值，但不应被解读成"不冷"）。
	if !st.Cooling {
		t.Errorf("200 次连续软限流后仍在冷却中，得到 %+v", st)
	}
}

// TestSoftStreakClearedBySuccess 一次成功清零退避指数。
func TestSoftStreakClearedBySuccess(t *testing.T) {
	const base = time.Minute
	p := newSoftPool(t, base)
	p.SetSoftRateMax(time.Hour)

	p.Cooldown("u1", CoolSoft, base, "429")
	p.Cooldown("u1", CoolSoft, base, "429")
	p.Cooldown("u1", CoolSoft, base, "429") // streak=3 → 4×base

	p.NoteSuccess("u1")
	p.Cooldown("u1", CoolSoft, base, "429")

	left := cooldownLeft(t, p, "u1")
	if left > base || left < base-2*time.Second {
		t.Errorf("成功后应回到基数 %v，得到 %v（退避未清零 → 已恢复的号被过度惩罚）", base, left)
	}
}

// ── 6004 模型级限流 ──────────────────────────────────────────────────────

// TestCooldownSoftForModelUsesResetWallClock 带重置时刻 → 冷却精确到那个时刻。
func TestCooldownSoftForModelUsesResetWallClock(t *testing.T) {
	p := newSoftPool(t, time.Minute)
	p.SetSoftRateMax(time.Hour)

	resetAt := time.Now().Add(25 * time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.2", "429 6004")

	left := cooldownLeft(t, p, "u1")
	if left > 25*time.Minute || left < 25*time.Minute-2*time.Second {
		t.Errorf("剩余 %v，期望约 25m（上游给的重置时刻是权威，不该被指数退避放大）", left)
	}
}

// TestCooldownSoftForModelCappedBySoftRateMax 上游给的重置时刻也不会超过封顶。
func TestCooldownSoftForModelCappedBySoftRateMax(t *testing.T) {
	p := newSoftPool(t, time.Minute)
	const max = 10 * time.Minute
	p.SetSoftRateMax(max)

	// 上游说 3 小时后重置 —— 远超封顶。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(3*time.Hour), "glm-5.2", "429 6004")

	left := cooldownLeft(t, p, "u1")
	if left > max || left < max-2*time.Second {
		t.Errorf("剩余 %v，期望被夹在 %v（封顶必须同时管住上游给的时刻）", left, max)
	}
}

// TestCooldownSoftForModelPastResetImmediate 重置时刻已过 → 几乎不冷却。
//
// 用 1ms 而不是 0：0 会让 until=now，"是否在冷却中"变成与调用时刻的纳秒竞态。
func TestCooldownSoftForModelPastResetImmediate(t *testing.T) {
	p := newSoftPool(t, time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(-time.Hour), "glm-5.2", "429 6004")

	if left := cooldownLeft(t, p, "u1"); left > time.Second {
		t.Errorf("重置时刻已过时应立即恢复，剩余 %v", left)
	}
}

// TestModelLevelCoolingAllowsOtherModel 模型级冷却中，**换模型**该号仍可选。
//
// # ⚠ 为什么要"直接看 healthyForModel"而不是"单账号 Pick 得到 nil"
//
// 池子里有一个**有意的兜底**：无 healthy 候选时从冷却账号里选
// until 最早到期的一个（`pickEarliestExpiryLocked`）—— 那是为了
// "全部号都在冷却"时不返回 nil（宁可试一个快要恢复的号，也不要 503）。
//
// 于是"单账号 + 冷却中"的 Pick **永远不为 nil**，用 nil 做断言测不到东西。
// 真正该验的是**决策点**：同一份冷却状态，换个模型问它健不健康，答案不同。
func TestModelLevelCoolingAllowsOtherModel(t *testing.T) {
	p := newSoftPool(t, time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "429 6004")

	p.mu.RLock()
	e := p.byUID["u1"]
	now := time.Now()
	sameModelOK := e.healthyForModel(now, "glm-5.2")
	otherModelOK := e.healthyForModel(now, "deepseek-v4-flash")
	emptyModelOK := e.healthyForModel(now, "")
	p.mu.RUnlock()

	if sameModelOK {
		t.Error("同模型应被冷却挡住")
	}
	if !otherModelOK {
		t.Error("换模型时该账号应仍可用 —— " +
			"模型级限流不该把整个账号冷掉（单账号部署下那等于整个网关不可用）")
	}
	// 无模型上下文（额度刷新/保活）不豁免：无从判断"换了哪个模型"。
	if emptyModelOK {
		t.Error("无模型上下文时不该豁免（保守按账号级冷却处理）")
	}
}

// TestModelLevelCoolingIsLocalToThatModel 端到端：模型级冷却只挡该模型。
//
// 两个账号：u1 被 glm-5.2 模型级限流，u2 健康。
//
//	请求 glm-5.2 → 必须总是 u2（u1 被挡）
//	请求别的模型 → u1 也进入候选（多次取样应能看到它）
func TestModelLevelCoolingIsLocalToThatModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1000)

	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "429 6004")

	// 同模型：u1 绝不能被选中（u2 健康，兜底不该被触发）。
	for i := 0; i < 200; i++ {
		got := p.PickForModel("glm-5.2", nil)
		if got == nil {
			t.Fatal("u2 健康，不该选不出号")
		}
		if got.UID == "u1" {
			t.Fatalf("第 %d 次选中了被同模型限流的 u1", i)
		}
	}

	// 换模型：u1 应重新进入候选。
	seenU1 := false
	for i := 0; i < 200 && !seenU1; i++ {
		if got := p.PickForModel("deepseek-v4-flash", nil); got != nil && got.UID == "u1" {
			seenU1 = true
		}
	}
	if !seenU1 {
		t.Error("换模型后 u1 应重新进入候选（模型豁免未生效）—— " +
			"表现为「被单模型限流后整个号都换不动」")
	}
}

// TestPlainCooldownClearsModelExemption 非模型级冷却必须清掉豁免痕迹。
//
// # 不清会怎样
//
// 账号先因 glm-5.2 被模型级限流（softRateModel=glm-5.2）→
// 随后因别的原因进入**账号级**冷却 → 但 exempt 判定仍看到 softRateModel 非空
// → 请求别的模型时被错误豁免，继续撞账号级故障。
func TestPlainCooldownClearsModelExemption(t *testing.T) {
	p := newSoftPool(t, time.Minute)

	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "429 6004")
	// 随后一次普通软冷却（无模型语义）。
	p.Cooldown("u1", CoolSoft, time.Hour, "429 普通限流")

	p.mu.RLock()
	e := p.byUID["u1"]
	model := e.softRateModel
	otherModelOK := e.healthyForModel(time.Now(), "deepseek-v4-flash")
	p.mu.RUnlock()

	if model != "" {
		t.Errorf("普通冷却应清空 softRateModel，仍为 %q", model)
	}
	if otherModelOK {
		t.Error("账号级冷却不该被上一次的模型豁免放过（会继续撞账号级故障）")
	}
}

// TestHealthyForModelBreakerAlwaysBlocks 熔断优先级高于模型豁免。
//
// 熔断是**账号级**事实（连续失败），与"请求哪个模型"无关 ——
// 模型豁免只能绕过软冷却那条 until，绝不能绕过熔断。
func TestHealthyForModelBreakerAlwaysBlocks(t *testing.T) {
	p := newSoftPool(t, time.Minute)
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "429 6004")
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断

	p.mu.RLock()
	otherModelOK := p.byUID["u1"].healthyForModel(time.Now(), "deepseek-v4-flash")
	p.mu.RUnlock()

	if otherModelOK {
		t.Error("熔断中的账号不得被模型豁免放过 —— " +
			"那会让一个连续失败的号靠换个模型名就绕过熔断")
	}
}

// TestSoftRateStateSurvivesFlush 软限流退避状态必须落盘。
//
// 不落盘的话一次重启就把退避指数清零，而上游的限流并未随我们重启消失 ——
// 表现是"重启后立刻又被限流一次"（正是这个指数要防的）。
func TestSoftRateStateSurvivesFlush(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	p.Cooldown("u1", CoolSoft, time.Minute, "429") // streak=2
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "429 6004")
	p.Flush()

	p2 := New(fp)
	p2.mu.RLock()
	e, ok := p2.byUID["u1"]
	var streak int
	var model string
	if ok {
		streak, model = e.softStreak, e.softRateModel
	}
	p2.mu.RUnlock()

	if !ok {
		t.Fatal("重启后账号丢失")
	}
	if streak < 3 {
		t.Errorf("重启后 softStreak=%d，期望 ≥3 —— "+
			"未落盘会让退避指数每次重启都归零", streak)
	}
	if model != "glm-5.2" {
		t.Errorf("重启后 softRateModel=%q，期望 glm-5.2 —— "+
			"未落盘会让模型豁免在重启后失效（被单模型限流的号又变成整体不可用）", model)
	}
}

// ── 12153 三次门控 ──────────────────────────────────────────────────────

// TestSessionDeadNeedsThreeStrikes 连续 3 次才禁用。
func TestSessionDeadNeedsThreeStrikes(t *testing.T) {
	p := newSoftPool(t, time.Minute)

	for i := 1; i <= 2; i++ {
		if disabled := p.NoteSessionDead("u1"); disabled {
			t.Fatalf("第 %d 次就禁用了 —— 一次 12153 多为瞬时抖动", i)
		}
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("第 %d 次后账号已禁用", i)
		}
	}
	if disabled := p.NoteSessionDead("u1"); !disabled {
		t.Fatal("第 3 次应禁用（真正失效的 session 必须被停掉）")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("第 3 次后应禁用: %+v", st)
	}
}

// TestSessionDeadStreakClearedBySuccess 抖动自愈：一次成功清零计数。
//
// # 这是门控能工作的关键
//
// 没有这条，一个每天抖一次的账号会在三天后被误禁 ——
// 三次**无关的**抖动叠加成一次"连续失效"。清零让它永远攒不满阈值。
func TestSessionDeadStreakClearedBySuccess(t *testing.T) {
	p := newSoftPool(t, time.Minute)

	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if got := p.SessionDeadStreak("u1"); got != 2 {
		t.Fatalf("计数=%d，期望 2", got)
	}
	p.NoteSuccess("u1")
	if got := p.SessionDeadStreak("u1"); got != 0 {
		t.Errorf("成功后计数应归零，得到 %d —— "+
			"不清零会让跨天的无关抖动叠加成误禁", got)
	}
	// 再连打两次仍不应禁用。
	p.NoteSessionDead("u1")
	if p.NoteSessionDead("u1") {
		t.Error("清零后两次不该触发禁用")
	}
}

// TestSessionDeadStreakSurvivesRestart 计数必须落盘。
//
// 不落盘的话，重启永远把它清零 —— 那个号永远攒不满阈值，
// 即"真正失效的 session 永远不会被停掉"，每次请求都白跑一趟。
func TestSessionDeadStreakSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.Flush()

	p2 := New(fp)
	if got := p2.SessionDeadStreak("u1"); got != 2 {
		t.Fatalf("重启后计数=%d，期望 2（未落盘/未读回）", got)
	}
	// 第三次（跨重启）应禁用。
	if !p2.NoteSessionDead("u1") {
		t.Error("跨重启累计到第 3 次应禁用 —— 计数没落盘的话永远到不了阈值")
	}
}

// TestClearCooldownResetsStreaks 人工"清冷却"必须把两条退避线都清掉。
//
// 用户点这个按钮的语义是"人工宣布这个号没问题"。留着计数会让
// 解冻后的第一次 429/12153 立刻吃满档位 —— 一个说不通的行为。
func TestClearCooldownResetsStreaks(t *testing.T) {
	p := newSoftPool(t, time.Minute)
	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")

	if !p.ClearCooldown("u1") {
		t.Fatal("ClearCooldown 应返回 true")
	}
	if got := p.SessionDeadStreak("u1"); got != 0 {
		t.Errorf("清冷却后 session 计数=%d，期望 0", got)
	}
	// 退避也回到基数。
	p.SetSoftRateMax(time.Hour)
	p.Cooldown("u1", CoolSoft, time.Minute, "429")
	left := cooldownLeft(t, p, "u1")
	if left > time.Minute || left < time.Minute-2*time.Second {
		t.Errorf("清冷却后应回到基数 1m，得到 %v（退避指数未被清）", left)
	}
}
