package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// quota_tier_test.go —— 额度更新策略（T-029 定稿）的守门用例。
//
// # 背景
//
//	用户："如果说回值里面是可以计算出来的，我们就计算…这个定时可以延得很长，比如说一天。"
//
// ⇒ 两种模式：**有回执**（余额算得出来 ⇒ 只在 24h / 200 次调用增量时校正）、
// **没回执**（只能刷 ⇒ 热 10m / 温 30m / 冷 6h）。
//
// # 为什么每种组合都要单独钉
//
// `dueForRefresh` 的两个错方向**都不报错**：
//
//	判太勤 → 请求量白烧（用户这次报的问题）
//	判太懒 → 有回执的号偏差**无界**（算出来的余额越飘越远，界面看着还挺合理）
//
// 所以这里不是"测一个大概能用"，而是把每条闸门的边界都断言掉。

// tierT0 固定基准时刻（不用 time.Now()：用例必须与运行时刻无关）。
var tierT0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// ── ②③ 有回执：两条闸门（24h / 200 次）────────────────────────────────────

// TestDueForRefreshTapBackedReconcile 有回执的号：24 小时内不刷，除非调用增量够。
func TestDueForRefreshTapBackedReconcile(t *testing.T) {
	cases := []struct {
		name         string
		lastQuotaAgo time.Duration
		callsSince   int64
		want         bool
		why          string
	}{
		{"刚校正过", 1 * time.Minute, 0, false, "有回执 ⇒ 余额靠算，刚校正过就不该再刷"},
		{"23 小时 59 分", 23*time.Hour + 59*time.Minute, 5, false, "还没到 24 小时闸门"},
		{"满 24 小时", quotaReconcileEvery, 0, true, "到点必须校正一次 —— 防偏差无界累积"},
		{"调用增量 199", time.Hour, quotaReconcileCallCount - 1, false, "差一次不到阈值"},
		{"调用增量 200", time.Hour, quotaReconcileCallCount, true, "误差是累加的 ⇒ 攒够 200 次也要校正"},
		{"两条都不满足", time.Hour, 1, false, "常态：让它自己算着，不刷"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lastUsed := tierT0.Add(-2 * time.Minute) // 有回执不看活跃度
			lastQuota := tierT0.Add(-c.lastQuotaAgo)
			if got := dueForRefresh(true, lastUsed, lastQuota, c.callsSince, tierT0); got != c.want {
				t.Errorf("dueForRefresh(有回执, 距上次校正=%v, 调用增量=%d) = %v，want %v —— %s",
					c.lastQuotaAgo, c.callsSince, got, c.want, c.why)
			}
		})
	}
}

// TestDueForRefreshTapBackedIgnoresTier 有回执的号**不看**活跃度分层
// （余额是算出来的；冷号也不需要更勤，反而只有它需要"别刷"）。
func TestDueForRefreshTapBackedIgnoresTier(t *testing.T) {
	cold := tierT0.Add(-5 * time.Hour)
	// 冷号 + 有回执 + 距上次校正 7 小时：按分层（冷 6h）会刷；有回执只认 24h ⇒ 不刷
	if dueForRefresh(true, cold, tierT0.Add(-7*time.Hour), 0, tierT0) {
		t.Error("有回执的号不该按活跃度分层刷（它只认 24h / 200 次两条闸门）")
	}
	// 对照：同一个"距上次校正 7 小时"，没回执的冷号**会**刷（6 小时一层到了）
	if !dueForRefresh(false, cold, tierT0.Add(-7*time.Hour), 0, tierT0) {
		t.Error("没回执的冷号过了 6 小时应当刷（对照组）")
	}
}

// ── ④ 没回执：10 / 30 / 360 分钟分层 ─────────────────────────────────────

// TestDueForRefreshNonTapTiers 没回执的号：热 10m / 温 30m / 冷 6h。
func TestDueForRefreshNonTapTiers(t *testing.T) {
	cases := []struct {
		name         string
		lastUsedAgo  time.Duration
		lastQuotaAgo time.Duration
		want         bool
		why          string
	}{
		{"hot/未到点", 2 * time.Minute, 9 * time.Minute, false, "热号 10 分钟一层：9 分钟还不到"},
		{"hot/到点", 2 * time.Minute, 10 * time.Minute, true, "热号到 10 分钟必须刷（额度真的在变）"},
		{"warm/未到点", 30 * time.Minute, 29 * time.Minute, false, "温号 30 分钟一层"},
		{"warm/到点", 30 * time.Minute, 30 * time.Minute, true, "温号到 30 分钟刷"},
		{"cold/未到点", 5 * time.Hour, 5*time.Hour + 59*time.Minute, false, "冷号 6 小时一层（省下来的请求量全在这）"},
		{"cold/到点", 5 * time.Hour, quotaColdEvery, true, "冷号满 6 小时也要刷一次（余额可能被手工充值改过）"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lastUsed := tierT0.Add(-c.lastUsedAgo)
			lastQuota := tierT0.Add(-c.lastQuotaAgo)
			// ⚠ 故意传一个巨大的 callsSince：没回执的号**不看**它（那是回执模式专用的代理指标）
			if got := dueForRefresh(false, lastUsed, lastQuota, 9999, tierT0); got != c.want {
				t.Errorf("dueForRefresh(没回执, idle=%v, 距上次=%v) = %v，want %v —— %s",
					c.lastUsedAgo, c.lastQuotaAgo, got, c.want, c.why)
			}
		})
	}
}

// TestQuotaTierOfBoundaries 分层边界（10 分钟 / 60 分钟）—— 差一秒也要分对。
func TestQuotaTierOfBoundaries(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want string
	}{
		{0, "hot"},
		{quotaHotWindow - time.Second, "hot"},
		{quotaHotWindow, "warm"}, // 边界归温号（判据是 `<`）
		{quotaWarmWindow - time.Second, "warm"},
		{quotaWarmWindow, "cold"},
		{24 * time.Hour, "cold"},
	}
	for _, c := range cases {
		if got := quotaTierOf(tierT0.Add(-c.idle), tierT0); got != c.want {
			t.Errorf("idle=%v ⇒ 层=%q，want %q", c.idle, got, c.want)
		}
	}
}

// ── ⑤ 首轮全刷 ────────────────────────────────────────────────────────────

func TestDueForRefreshFirstRoundRefreshesEverything(t *testing.T) {
	var never time.Time
	for _, tapBacked := range []bool{true, false} {
		for _, lastUsed := range []time.Time{never, tierT0.Add(-time.Second), tierT0.Add(-10 * time.Hour)} {
			if !dueForRefresh(tapBacked, lastUsed, time.Time{}, 0, tierT0) {
				t.Errorf("从没校正过的账号首轮必刷（否则面板长期空白）：tapBacked=%v lastUsed=%v", tapBacked, lastUsed)
			}
		}
	}
}

// ── ⑥ 死号跳过 + 恢复 ────────────────────────────────────────────────────

// TestQuotaStaleRoundSkipsThenRecovers 连续 3 轮 HasData=false ⇒ 暂停；一次 true ⇒ 立刻恢复。
func TestQuotaStaleRoundSkipsThenRecovers(t *testing.T) {
	resetQuotaTierStateForTest()
	defer resetQuotaTierStateForTest()

	const uid = "260913075224031074"
	// 前两轮：还不到阈值 ⇒ 仍要刷（不能一轮就判死：上游偶发抽风很常见）
	for i := 1; i <= quotaStaleRounds-1; i++ {
		if quotaTier.noteRound(uid, "codearts", false) {
			t.Fatalf("第 %d 轮就暂停了？阈值是 %d 轮", i, quotaStaleRounds)
		}
		if quotaTier.isDead(uid) {
			t.Fatalf("第 %d 轮不该进入跳过集", i)
		}
	}
	// 第 3 轮：到阈值 ⇒ 暂停，且**必须**报"状态变化"（日志只在变化时打）
	if !quotaTier.noteRound(uid, "codearts", false) {
		t.Fatal("第 3 轮应当报告状态变化（暂停）")
	}
	if !quotaTier.isDead(uid) {
		t.Fatal("连续 3 轮拿不到数据 ⇒ 必须进入跳过集")
	}
	// 继续刷不到 ⇒ 不再是"变化"（否则日志每拍刷屏）
	if quotaTier.noteRound(uid, "codearts", false) {
		t.Error("已在跳过集里的号再刷不到，不该再报状态变化（日志会刷屏）")
	}
	// ★ 一次拿到数据 ⇒ 立刻恢复
	if !quotaTier.noteRound(uid, "codearts", true) {
		t.Error("恢复也应当算状态变化（要打一行日志）")
	}
	if quotaTier.isDead(uid) {
		t.Error("任何一次 HasData=true ⇒ 必须立刻恢复刷新（不是永久黑名单）")
	}
}

// TestQuotaStaleDataButUnchangedValueIsNotSkipped 判据是 HasData，**不是**"数值没变"。
//
// ⚠ 最容易写错的边界：有数据但数值不变的号（真的没消耗）**绝不能**被跳过 ——
// 跳掉它，面板上的额度就再也不更新了。
func TestQuotaStaleDataButUnchangedValueIsNotSkipped(t *testing.T) {
	resetQuotaTierStateForTest()
	defer resetQuotaTierStateForTest()

	const uid = "u-unchanged"
	for i := 0; i < quotaStaleRounds*3; i++ {
		quotaTier.noteRound(uid, "workbuddy", true) // HasData=true（值没变）连刷多轮
	}
	if quotaTier.isDead(uid) {
		t.Fatal("有数据的号（哪怕数值一直不变）绝不能被跳过 —— 那会让面板永远停在旧值")
	}
}

// ── ⑦ 跳过的号不计入 considered/bad ───────────────────────────────────────

// TestDeadAccountsAreExcludedBeforeDueComputation 跳过的号在**算到期之前**就被排除，
// 因此既不进 due，也不进 considered/failed ⇒ 不会误触发降速。
func TestDeadAccountsAreExcludedBeforeDueComputation(t *testing.T) {
	resetQuotaTierStateForTest()
	defer resetQuotaTierStateForTest()

	const uid = "dead-uid"
	for i := 0; i < quotaStaleRounds; i++ {
		quotaTier.noteRound(uid, "lobsterai", false)
	}
	if !quotaTier.isDead(uid) {
		t.Fatal("前置条件：这个号应当已暂停")
	}
	// 排除发生在 RefreshDueQuotasNow 的循环里（dueForRefresh 只管时间，不认识死号）。
	// ⇒ 钉住那个 continue 的**位置**：必须在算到期之前。
	src := readSource(t, "quota_autorefresh.go")
	iDead := strings.Index(src, "quotaTier.isDead(st.UID)")
	iDue := strings.Index(src, "dueForRefresh(isTapBacked(prov)")
	if iDead < 0 || iDue < 0 {
		t.Fatalf("源码里找不到期望的两处判据（isDead=%d dueForRefresh=%d）", iDead, iDue)
	}
	if iDead > iDue {
		t.Error("isDead 的 continue 必须在算到期**之前** —— 否则死号会进 due 并被计入失败，" +
			"3 个死号就能把失败率抬到降速阈值附近")
	}
}

// ── ① tap 名单守卫 ───────────────────────────────────────────────────────

// TestQuotaTapProvidersMatchSource 名单必须与**上游源码**一致：
// 每个被列为"有回执"的 provider，它的包里必须真的声明了 UsageTapSpec + UsageConsumed。
//
// # 为什么用源码 grep 当断言（而不是调接口）
//
// 方案 (b) 的名单是"查表可得的事实"的复制品 —— 复制品会漂。gateway 没有访问器
// （见 quotaTapProviders 的注释），所以唯一可行的"真相来源"就是上游源码本身：
// 它改了 Mode / 删了 tap，这条守卫就会红。
func TestQuotaTapProvidersMatchSource(t *testing.T) {
	root := repoRoot(t)
	dirs := map[string]string{
		"loomy":   "internal/loomy",
		"qoder":   "internal/qoder",
		"qodercn": "internal/qoder", // 同一个包（两个 provider id）
	}
	for id := range quotaTapProviders() {
		dir, ok := dirs[id]
		if !ok {
			t.Errorf("名单里的 %q 在测试里没登记包目录 —— 加了新上游就补上这条映射", id)
			continue
		}
		found := false
		for _, f := range goFiles(t, filepath.Join(root, dir)) {
			src := readFile(t, f)
			if strings.Contains(src, "UsageTapSpec{") && strings.Contains(src, "UsageConsumed") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s 被列为「有回执」，但 %s 里找不到 UsageTapSpec{…Mode:UsageConsumed} —— "+
				"名单与实现漂移了：要么它不再支持回执（从名单去掉），要么声明方式变了（更新这条守卫）",
				id, dir)
		}
	}
	// 反向：**不支持回执**的上游不该在名单里（误列会让它的余额永不校正、界面长期旧值）
	for _, id := range []string{"workbuddy", "workbuddy-intl", "trae", "lobsterai",
		"raccoon", "cline", "codearts", "zcode", "monkeycode"} {
		if quotaTapProviders()[id] {
			t.Errorf("%s 不该在「有回执」名单里（它没有 UsageConsumed 回执）", id)
		}
	}
}

// TestIsTapBackedUnknownProviderIsFalse 名单外一律"不支持回执"（保守方向）。
func TestIsTapBackedUnknownProviderIsFalse(t *testing.T) {
	if isTapBacked("") || isTapBacked("some-new-upstream") {
		t.Error("名单外的上游必须按「不支持回执」处理（宁可刷勤一点，也不要余额永不校正）")
	}
	for _, id := range []string{"loomy", "qoder", "qodercn"} {
		if !isTapBacked(id) {
			t.Errorf("%s 应当在「有回执」名单里", id)
		}
	}
}

// ── 记账与 nil 安全 ──────────────────────────────────────────────────────

// TestQuotaTierStateMarksSeen 记账（含失败）：刷过就记，否则死号会被每拍都撞。
func TestQuotaTierStateMarksSeen(t *testing.T) {
	resetQuotaTierStateForTest()
	defer resetQuotaTierStateForTest()

	if !quotaTier.lastSeen("u1").IsZero() {
		t.Fatal("初始状态必须是零值（== 从没刷过）")
	}
	// ⚠ 2026-11-02：markSeen 增加了 used 参数（选号侧判分层用）。这里传 nil = 不更新 LastUsed。
	quotaTier.markSeen([]string{"u1"}, map[string]int64{"u1": 42}, nil, tierT0)
	if got := quotaTier.lastSeen("u1"); !got.Equal(tierT0) {
		t.Errorf("lastSeen = %v，want %v", got, tierT0)
	}
	if got := quotaTier.callsSeen("u1"); got != 42 {
		t.Errorf("callsSeen = %d，want 42（下一次要拿它算增量）", got)
	}
	if dueForRefresh(false, time.Time{}, quotaTier.lastSeen("u1"), 0, tierT0.Add(time.Minute)) {
		t.Error("刚刷过的号在一分钟后不该到期")
	}
	if !dueForRefresh(false, time.Time{}, quotaTier.lastSeen("u1"), 0, tierT0.Add(quotaColdEvery)) {
		t.Error("冷号满 6 小时应当到期")
	}
}

// TestRefreshDueQuotasNowWithoutPoolIsSafe 未接线池子时安全返回（不 panic、不发请求）。
func TestRefreshDueQuotasNowWithoutPoolIsSafe(t *testing.T) {
	resetQuotaTierStateForTest()
	defer resetQuotaTierStateForTest()

	var h *Handler
	res, due, total := h.RefreshDueQuotasNow(tierT0)
	if due != 0 || total != 0 || res.Updated != 0 || res.Failed != 0 {
		t.Fatalf("nil 接收者必须返回零值，实得 res=%+v due=%d total=%d", res, due, total)
	}
}

// ── 小工具 ───────────────────────────────────────────────────────────────

// repoRoot 从包目录往上找仓库根（`go test` 的工作目录就是包目录本身）。
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败：%v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// goFiles 列出目录里的**非测试** Go 文件（守卫要读上游实现，不是它的测试）。
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// readSource 读本包目录下的某个源文件（源码级断言用）。
func readSource(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败：%v", err)
	}
	return readFile(t, filepath.Join(wd, name))
}
