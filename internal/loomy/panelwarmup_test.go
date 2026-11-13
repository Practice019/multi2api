// panelwarmup_test.go 面板预热的**判断**层测试（不联网、不碰磁盘）。
//
// # 为什么这些断言值得写（2026-11-02 用户报的真 bug）
//
// 用户贴的日志：
//
//	loomy: 预热 · 未缓存 t0/i0 · 过期 t46/i46 · 号44 · 发起12
//
// 「过期 46」> 「共 44 个号」：多出来的 2 个是**已删账号在缓存里的残留行**（幽灵行）——
// 缓存是落盘的，删号不会清缓存，而"过期集合"遍历的是缓存 map 的全部 key。
//
// ⚠ 症状不只是难看：幽灵行永远补不上 ⇒ 待办恒 > 0 ⇒ 调度器 5 秒一轮空转
//
//	（用户日志里连着 5 轮、间隔 5~8 秒就是证据）。
//
// ⚠ 空转那条断言（left 能归零）与"幽灵行"是同一条因果链上的两端，
//
//	所以这里**两头都钉**：不数幽灵（planPanelWarmup）+ 不留幽灵（pruneExcept）。
package loomy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// warmupCacheProvider 造一个"面板缓存已经灌好内容"的 Provider。
//
// ⚠ 直接塞缓存对象、**不走 `tasksPanel()`**：那条路会从磁盘读 panels.json
//
//	并挂上 onChange（落盘回调），测试里既不需要也不该碰磁盘。
func warmupCacheProvider(t *testing.T, tasks, invite map[string]time.Time) *Provider {
	t.Helper()
	tp := newPanelRowCache[accountTasksView]()
	for uid, at := range tasks {
		tp.set(uid, accountTasksView{UID: uid, OK: true, FetchedAt: at})
	}
	ip := newPanelRowCache[accountInviteView]()
	for uid, at := range invite {
		ip.set(uid, accountInviteView{UID: uid, OK: true, FetchedAt: at})
	}
	return &Provider{panelTasks: tp, panelInvite: ip}
}

func acctRefs(uids ...string) []accountRef {
	out := make([]accountRef, 0, len(uids))
	for _, u := range uids {
		out = append(out, accountRef{UID: u, Session: "sess-" + u})
	}
	return out
}

func cacheSize[T any](c *panelRowCache[T]) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// TestPlanPanelWarmupExcludesGhostUIDs ⚠ 核心断言：缓存里有幽灵行时，过期数**不能**虚高。
//
// 场景：缓存 3 行（2 个活号 + 1 个已删号），账号池只有 2 个号。
// 修之前 expT/expI = 3（用户看到的 46），修之后必须是 **2**。
func TestPlanPanelWarmupExcludesGhostUIDs(t *testing.T) {
	now := time.Now()
	stale := now.Add(-2 * time.Hour) // 远超 TTLTasks(30m) 与 TTLInvite(6h)？→ invite 用更久的
	veryStale := now.Add(-24 * time.Hour)
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": stale, "u2": stale, "ghost": stale},
		map[string]time.Time{"u1": veryStale, "u2": veryStale, "ghost": veryStale},
	)
	accts := acctRefs("u1", "u2")

	pl := p.planPanelWarmup(accts, now)

	if pl.expT != 2 {
		t.Errorf("tasks 过期数 = %d，want 2（⚠ 幽灵行 ghost 不该被数进来；修之前是 3）", pl.expT)
	}
	if pl.expI != 2 {
		t.Errorf("invite 过期数 = %d，want 2（同理）", pl.expI)
	}
	for _, j := range pl.jobs {
		if j.a.UID == "ghost" {
			t.Errorf("幽灵 uid 不该进待办列表（它连账号都不存在，补不上）")
		}
	}
}

// TestPlanPanelWarmupExpiredNeverExceedsAccountCount ⚠ 用户看到的那条现象本身：
// 「过期数」**永远不该超过账号数**（46 > 44 就是 bug 的可见形态）。
func TestPlanPanelWarmupExpiredNeverExceedsAccountCount(t *testing.T) {
	now := time.Now()
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": now.Add(-2 * time.Hour), "u2": now.Add(-2 * time.Hour), "g1": now.Add(-2 * time.Hour), "g2": now.Add(-2 * time.Hour)},
		map[string]time.Time{"u1": now.Add(-2 * time.Hour), "u2": now.Add(-2 * time.Hour), "g1": now.Add(-2 * time.Hour), "g2": now.Add(-2 * time.Hour)},
	)
	accts := acctRefs("u1", "u2")

	pl := p.planPanelWarmup(accts, now)

	if pl.expT > len(accts) || pl.expI > len(accts) {
		t.Errorf("过期数(tasks=%d invite=%d) 超过了账号数 %d —— 这正是用户报的「过期 46 · 共 44 个号」",
			pl.expT, pl.expI, len(accts))
	}
}

// TestPanelWarmupLeftCanReachZero ⚠ 因果链的另一端：待办数必须能归零（否则调度器空转）。
//
// 修之前：pending = 4 个活号的活 + 2 个幽灵 = 6，而一轮最多做 panelWarmupBatch 个 job，
// 做完 4 个真活之后 left 仍 ≥ 2 ⇒ 永远"还有活" ⇒ 5 秒一轮地空转。
func TestPanelWarmupLeftCanReachZero(t *testing.T) {
	now := time.Now()
	// ⚠ invite 的 TTL 是 6 小时（TTLInvite）⇒ "过期"必须用 24 小时前的时刻；
	//   用 2 小时的话它还在 TTL 内，本用例就只剩 tasks 那一半了。
	old := now.Add(-24 * time.Hour)
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": old, "u2": old, "ghost": old},
		map[string]time.Time{"u1": old, "u2": old, "ghost": old},
	)
	accts := acctRefs("u1", "u2")

	pl := p.planPanelWarmup(accts, now)
	pending := pl.pending()
	if pending != 4 {
		t.Fatalf("待办数 = %d，want 4（2 个号 × tasks+invite；⚠ 幽灵行不能计入，修之前是 6）", pending)
	}
	// 模拟"本轮把这 4 个 job 全做完"（sweepPanels 里就是 pending - done）。
	left := pending - len(pl.jobs)
	if len(pl.jobs) != 4 {
		t.Fatalf("本轮应当发起 4 个 job，实得 %d", len(pl.jobs))
	}
	if left != 0 {
		t.Errorf("做完本轮后 left = %d，want 0 —— 不为 0 就意味着调度器会继续 5 秒一轮地空转", left)
	}
}

// TestGhostRowsWouldKeepLeftPositive ⚠ 上一条的**对照**：幽灵行确实会让 left 永远归不了零。
//
// 手工把"幽灵行被数进待办"这件事复现一遍（+2 = 两个面板各一个幽灵），
// 断言它产生的 left > 0 —— 否则上面那条"能归零"就可能只是"怎么算都是 0"的巧合。
func TestGhostRowsWouldKeepLeftPositive(t *testing.T) {
	now := time.Now()
	old := now.Add(-24 * time.Hour)
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": old, "u2": old, "ghost": old},
		map[string]time.Time{"u1": old, "u2": old, "ghost": old},
	)
	pl := p.planPanelWarmup(acctRefs("u1", "u2"), now)

	// 修之前：expT/expI 会把 ghost 也数进去 ⇒ 待办是 6 而不是 4。
	leftWithGhosts := (pl.pending() + 2) - len(pl.jobs)
	if leftWithGhosts <= 0 {
		t.Fatalf("按修之前的口径，幽灵行应当让 left 恒 > 0（实得 %d）—— 对照不成立", leftWithGhosts)
	}
}

// TestPrunePanelOrphansLoadsBothCachesBeforeSave ⚠ **回归守卫**：修剪不能让 invite 段被清空。
//
// 这条是被真机日志打出来的（2026-11-02）：修剪时写成
// `p.tasksPanel().pruneExcept(keep) + p.invitePanel().pruneExcept(keep)`，
// Go 先算左操作数 ⇒ tasks 修剪**同步落盘**时 `p.panelInvite` 还是 nil
// ⇒ 落盘把 invite 段写空 ⇒ 随后惰性加载的 invite 缓存从被截断的文件里读回 0 行。
// 症状：日志「未缓存(tasks=0 invite=44)」+ panels.json 的 invite 段 46 → 0。
//
// ⚠ 这条测试用**真实的落盘路径**（t.TempDir() 当 authDir + 真 panels.json），
// 因为 bug 恰恰出在"惰性加载 + 同步落盘"的时序上 —— 用假的 onChange 就测不到了。
func TestPrunePanelOrphansLoadsBothCachesBeforeSave(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// 盘上：两个面板各 2 行（1 个活号 + 1 个幽灵）。
	snap := map[string]any{
		"saved_at": now,
		"tasks": map[string]any{
			"u1":     map[string]any{"uid": "u1", "ok": true, "fetched_at": now},
			"ghostA": map[string]any{"uid": "ghostA", "ok": true, "fetched_at": now},
		},
		"invite": map[string]any{
			"u1":     map[string]any{"uid": "u1", "ok": true, "fetched_at": now},
			"ghostB": map[string]any{"uid": "ghostB", "ok": true, "fetched_at": now},
		},
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("造夹具失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "panels.json"), raw, 0o600); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
	// ⚠ 两个缓存都是 nil（模拟"进程刚起来、还没人访问过面板"），这正是出 bug 的时序。
	p := &Provider{authDir: dir}

	removed := p.prunePanelOrphans(acctRefs("u1"))
	if removed != 2 {
		t.Fatalf("应当删掉 2 行（两个面板各一个幽灵），实得 %d", removed)
	}
	// ① 内存里：invite 缓存必须仍然有 u1（不是空）。
	if _, ok := p.invitePanel().get("u1"); !ok {
		t.Errorf("⚠ invite 缓存被清空了 —— 修剪时的落盘把 invite 段写没了（见函数注释）")
	}
	// ② 盘上：invite 段必须仍然有 u1（不是被写成空）。**这一条才是本回归守卫的核心**：
	//   盘上丢了 u1 ⇒ 重启后那 44 个号的邀请码全变成「正在读取…」。
	after := p.loadPanelsSnapshot()
	if _, ok := after.Invite["u1"]; !ok {
		t.Errorf("⚠ panels.json 的 invite 段丢了 u1（缓存整段被清空）—— 落盘发生在 invite 惰性加载之前")
	}
	if _, ok := after.Tasks["u1"]; !ok {
		t.Errorf("tasks 段丢了 u1")
	}
	// ⚠ 不断言"幽灵行在盘上已经消失"：落盘走 throttledSavePanels，**5 秒最多一次**
	//   ⇒ 第二次修剪触发的那次落盘会被节流掉，幽灵行要等下一次落盘才消失。
	//   这不影响正确性（它已经从内存里删掉、下一轮修剪也再算不出它），
	//   而内存那侧上面已经断言过了（removed==2 + 缓存里没有 ghost）。
	if _, ok := p.panelInvite.get("ghostB"); ok {
		t.Errorf("幽灵行仍留在内存缓存里")
	}
}

// TestPrunePanelOrphansRemovesGhostRows ⚠ 病根：缓存里的幽灵行要真被删掉（不是只在计数处忽略）。
func TestPrunePanelOrphansRemovesGhostRows(t *testing.T) {
	now := time.Now()
	stale := now.Add(-2 * time.Hour)
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": stale, "u2": stale, "ghostA": stale},
		map[string]time.Time{"u1": stale, "u2": stale, "ghostB": stale},
	)
	accts := acctRefs("u1", "u2")

	removed := p.prunePanelOrphans(accts)
	if removed != 2 {
		t.Errorf("应当删掉 2 行（tasks 1 + invite 1），实得 %d", removed)
	}
	if got := cacheSize(p.panelTasks); got != 2 {
		t.Errorf("修剪后 tasks 缓存 = %d 行，want 2（幽灵行必须真的从 map 里消失）", got)
	}
	if got := cacheSize(p.panelInvite); got != 2 {
		t.Errorf("修剪后 invite 缓存 = %d 行，want 2", got)
	}
	if _, ok := p.panelTasks.get("ghostA"); ok {
		t.Errorf("ghostA 仍在 tasks 缓存里")
	}
	if _, ok := p.panelInvite.get("ghostB"); ok {
		t.Errorf("ghostB 仍在 invite 缓存里")
	}
	// 幂等：再修一次没有可删的（这一条同时守住"修剪不会误删活号"）。
	if again := p.prunePanelOrphans(accts); again != 0 {
		t.Errorf("第二次修剪 = %d，want 0（幂等）", again)
	}
	if got := cacheSize(p.panelTasks); got != 2 {
		t.Errorf("幂等修剪后 tasks 缓存 = %d 行，want 2", got)
	}
}

// TestPrunePanelOrphansKeepsRowsForLiveAccounts ⚠ 反向守卫：修剪**只删幽灵**，
// 账号池里的行一行都不能动（哪怕它很久没刷新、或当前读不到数据）。
func TestPrunePanelOrphansKeepsRowsForLiveAccounts(t *testing.T) {
	now := time.Now()
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": now.Add(-100 * time.Hour), "u2": now.Add(-100 * time.Hour)},
		map[string]time.Time{"u1": now.Add(-100 * time.Hour), "u2": now.Add(-100 * time.Hour)},
	)

	if removed := p.prunePanelOrphans(acctRefs("u1", "u2", "u3")); removed != 0 {
		t.Errorf("账号池里有的行不该被删（实删 %d 行）", removed)
	}
	if got := cacheSize(p.panelTasks); got != 2 {
		t.Errorf("tasks 缓存 = %d 行，want 2", got)
	}
}

// TestPrunePanelOrphansEmptyPoolPrunesEverything 账号池为空时，缓存里的行全是孤儿。
//
// ⚠ 这一条是**边界**（别把"读不到账号"当成"没有孤儿"）：sweepPanels 在账号池为空时
// 会提前 return（什么都不做），所以这条路只可能被显式调用 —— 断言它不会 panic、
// 且行为可预期即可。
func TestPrunePanelOrphansEmptyPoolPrunesEverything(t *testing.T) {
	now := time.Now()
	p := warmupCacheProvider(t,
		map[string]time.Time{"u1": now},
		map[string]time.Time{"u1": now},
	)
	if removed := p.prunePanelOrphans(nil); removed != 2 {
		t.Errorf("空账号池 ⇒ 两行都是孤儿，实删 %d", removed)
	}
}

// panelWarmupLogLine 的两条硬要求：无错时**不打**错误段；有错时必须带上（截短后的）错误。
//
// ⚠ 2026-11-13 用户报「废话太多了」⇒ 整行重写成**数字优先、文字最少**（见 panelWarmupLogLine 的注释）。
//
//	这条断言跟着改成新形态；「无错时不出现」「有错时出现且可读」两条要求**没变**。
func TestPanelWarmupLogLineOmitsEmptyFirstError(t *testing.T) {
	pl := panelPlan{todoT: 0, todoI: 0, expT: 44, expI: 44}

	clean := panelWarmupLogLine(pl, 44, 12, "")
	if strings.Contains(clean, "错") {
		t.Errorf("本轮无错时不该出现任何错误段（那是噪音，用户报过两次）：\n%s", clean)
	}
	// 四个数字都必须在一行里（运维只看这四个）—— 用**字段名+值**成对断言，
	// 免得将来有人把某段删了而断言仍然绿。
	for _, want := range []string{"t0/i0", "t44/i44", "号44", "发起12"} {
		if !strings.Contains(clean, want) {
			t.Errorf("日志缺少数字段 %q：\n%s", want, clean)
		}
	}

	// 真机上那条长错误：必须**截短**成「码 + 短语」，而不是原样贴。
	realErr := "loomy: 积分接口 /onboarding/tasks 返回 100002: 登录已失效，请重新登录"
	withErr := panelWarmupLogLine(pl, 44, 12, realErr)
	if !strings.Contains(withErr, "错 100002 登录已失效") {
		t.Errorf("有错时要给出「码 + 短语」（排障够用且不刷屏）：\n%s", withErr)
	}
	if strings.Contains(withErr, "请重新登录") {
		t.Errorf("错误的说明性尾巴应当被截掉（用户报的就是这类长中文串）：\n%s", withErr)
	}
}

// TestPanelWarmupLogLineIsCompact ★ 数字优先、文字最少、有硬长度上限。
//
// 用户原话：「这个 loomy 的日志打印 直接统计数字简介一点就行了 废话太多了」⇒
// 把"说明性文字"当成**要删的东西**来断言（不是靠人眼看着短）。
func TestPanelWarmupLogLineIsCompact(t *testing.T) {
	pl := panelPlan{todoT: 0, todoI: 0, expT: 46, expI: 46}
	clean := panelWarmupLogLine(pl, 44, 12, "")
	t.Logf("无错时（%d 字）: %s", utf8.RuneCountInString(clean), clean)
	if n := utf8.RuneCountInString(clean); n > 100 {
		t.Errorf("日志 %d 字，超过 100 字上限（用户要求精简）：\n%s", n, clean)
	}
	// "废话"清单：旧格式里的这些词一个都不许回来。
	for _, banned := range []string{"待办：", "未缓存(tasks=", "过期(tasks=", "首个错误", "共 ", " 个号", "本轮发起"} {
		if strings.Contains(clean, banned) {
			t.Errorf("日志里仍有说明性文字 %q（用户要求砍掉）：\n%s", banned, clean)
		}
	}

	// 有错时上限放宽到 140（错误段那点字不能省 —— 它正是排障要看的东西）。
	withErr := panelWarmupLogLine(pl, 44, 12, "loomy: 积分接口 /onboarding/tasks 返回 100002: 登录已失效，请重新登录")
	t.Logf("有错时（%d 字）: %s", utf8.RuneCountInString(withErr), withErr)
	if n := utf8.RuneCountInString(withErr); n > 140 {
		t.Errorf("带错日志 %d 字，超过 140 字上限：\n%s", n, withErr)
	}
}

// TestPanelWarmupErrShort 错误截短（纯函数，逐条钉住边界）。
func TestPanelWarmupErrShort(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		// 真机那条（用户截图里的原文）：码在前、短语在后，尾巴的"请重新登录"要丢掉。
		{"loomy: 积分接口 /onboarding/tasks 返回 100002: 登录已失效，请重新登录", "100002 登录已失效"},
		// 头一个 token 就是码（别拼成 "502 502 bad gateway"）。
		{"tasks: 502 bad gateway", "502 bad gateway"},
		// 没有码：只保留短语。
		{"invite: 连接超时", "连接超时"},
	} {
		if got := panelWarmupErrShort(c.in); got != c.want {
			t.Errorf("panelWarmupErrShort(%q) = %q，want %q", c.in, got, c.want)
		}
	}
	// 超长无分隔串：必须被硬截到 ≤40 字（否则"截短"是假的）。
	long := strings.Repeat("甲", 100)
	if got := panelWarmupErrShort(long); utf8.RuneCountInString(got) > 40 {
		t.Errorf("超长错误没被截短：%d 字", utf8.RuneCountInString(got))
	}
}

// 幽灵行不进待办之后，两个"未缓存"计数也不能被它影响（同一段代码的另一个分支）。
//
// ⚠ 这里同时**钉住一个既有语义**（不是本轮改的）：`planPanelWarmup` 的"从未缓存"那段是
// `if tasks 缺 { … } else if invite 缺 { … }` ⇒ **一个号一轮只补一个面板**
// （下一轮 tasks 有了，才轮到它的 invite）。所以两个都缺时 todoT=2 / todoI=0。
// 这条与"幽灵行"无关，但把它写成断言能防止有人顺手改成两个都补而没意识到配额影响。
func TestPlanPanelWarmupTodoCountsIgnoreGhostUIDs(t *testing.T) {
	now := time.Now()
	// 缓存里只有幽灵行；两个活号都"从没读到过"。
	p := warmupCacheProvider(t,
		map[string]time.Time{"ghost": now},
		map[string]time.Time{"ghost": now},
	)
	pl := p.planPanelWarmup(acctRefs("u1", "u2"), now)

	if pl.todoT != 2 || pl.todoI != 0 {
		t.Errorf("未缓存数 = tasks:%d invite:%d，want 2/0（一个号一轮只补一个面板，见上面的注释）", pl.todoT, pl.todoI)
	}
	if pl.expT != 0 || pl.expI != 0 {
		t.Errorf("幽灵行不该被算成过期：tasks:%d invite:%d", pl.expT, pl.expI)
	}
	if len(pl.jobs) != 2 {
		t.Errorf("本轮应当发起 2 个 job（2 号 × tasks），实得 %d", len(pl.jobs))
	}
	// 幽灵 uid 一行都不该进 job。
	for _, j := range pl.jobs {
		if j.a.UID == "ghost" {
			t.Errorf("幽灵 uid 进了 job 列表")
		}
	}
}
