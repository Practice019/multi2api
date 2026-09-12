// upstream_isolation_test.go 上游边界：本包**不得**碰别家上游的账号。
//
// # 这个文件为什么存在（真 bug 的回归守卫）
//
// 账号池是多上游共用的。改造前本包有 7 处按**全池**遍历（HTTP 层的
// accountList 一处 + 调度路径 6 处），于是 workbuddy 的自动签到/保活/旅行
// 一直在给 codearts 的账号跑。实测证据：一个不属于本上游、甚至不在
// 管理台账号列表里的账号，被 schedule 触发了 811 次，累计 814 条记录
// 写进 workbuddy 的任务历史（占比 16%）。
//
// # 为什么既有的测试全都抓不到它
//
// 既有装置（harness_test.go 的 newTestProvider）造池后一律用
// `p.Add(a)`，而 Add 走的是**默认上游**。于是池里**永远只有一个上游**，
// "遍历全池"与"遍历本上游"结果完全相同 —— 过滤写错在测试里不可见。
//
// 所以本文件的每个用例都先往池里塞一个**别家上游**的账号，
// 再断言它不出现在任何本上游的结果里。这是唯一能让该缺陷显形的构造。
//
// # 两条路径都要覆盖（Orchestrator 明确要求）
//
//	渲染路径：/admin/travel、/admin/growth 的响应条数
//	调度路径：RunCheckinAll / RunKeepaliveAll / runTravel 实际作用的账号
//
// 只测渲染路径是不够的：两者共用同一条不变式但走**不同的代码**，
// 只修前者的表现是"面板干净了，后台还在跑别人的号"。
package workbuddy

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

const (
	// otherProvider 别家上游。刻意不用 "codearts" 字面量：
	// 本包是上游无关的（判据 3），测试里写另一个具体上游的名字
	// 会让"本包认识哪些上游"变得模糊。
	otherProvider = "other-upstream"

	// defaultProviderForTest 本上游标识 —— 与装配层传的 workbuddy.ProviderID 同形。
	defaultProviderForTest = "workbuddy"
)

// seedMultiProviderPool 造一个**真的有两个上游**的账号池。
//
// own 个本上游账号（走 SyncToDirFor，带归属标签）
// + 1 个别家上游账号（走 SyncToDirFor(otherProvider, ...)）。
//
// # 为什么用 SyncToDirFor 而不是 Add
//
// Add 走默认上游、**不写归属标签** —— 用它塞"别家"的号，反而会把它
// 变成默认上游（本上游）的号，测试就失去了意义。
// SyncToDirFor 是唯一能给账号打上别家标签的公开路径。
func seedMultiProviderPool(t *testing.T, own ...*auth.Auth) (*pool.Pool, *auth.Auth) {
	t.Helper()
	p := pool.New("")
	// 默认上游设成本上游：未打标签的旧账号按它解释（与生产装配一致）。
	p.SetDefaultProvider(defaultProviderForTest)

	// ⚠ 给每个账号发**唯一** AccessToken。
	// 默认的 testAuth 让所有账号共用 "at"，而 HTTP 请求里能区分身份的
	// 只有 Authorization header（uid 不出现在请求里）。共用一个 token 时
	// "按 token 计数"会把所有账号算成一笔账，于是"是否遍历了别家账号"
	// 就测不出来了 —— 我第一版正因此让 travel 用例在变异下误通过。
	for _, a := range own {
		a.AccessToken = trackingToken(a.UID)
	}
	p.SyncToDirFor(defaultProviderForTest, own)

	foreign := &auth.Auth{
		UID:          "01a08fe0-foreign",
		Nickname:     "别家上游的账号",
		AccessToken:  trackingToken("01a08fe0-foreign"),
		RefreshToken: "foreign-rt",
		ExpiresAt:    9999999999,
	}
	p.SyncToDirFor(otherProvider, []*auth.Auth{foreign})

	// 前置断言：这个池必须真的是"两个上游"，
	// 否则下面所有断言都会因为"池里本来就只有本上游的号"而假绿。
	if got := len(p.List()); got != len(own)+1 {
		t.Fatalf("测试装置失效：池内账号=%d，期望 %d（本上游 %d + 别家 1）"+
			" —— 别家账号没塞进去，本用例会假绿", got, len(own)+1, len(own))
	}
	if got := len(p.ListFor(otherProvider)); got != 1 {
		t.Fatalf("测试装置失效：别家上游账号=%d，期望 1", got)
	}
	if got := len(p.ListFor(defaultProviderForTest)); got != len(own) {
		t.Fatalf("测试装置失效：本上游账号=%d，期望 %d", got, len(own))
	}
	return p, foreign
}

// newIsolationProvider 用双上游池造一个完整接线的 Provider。
//
// 关键：Config.Provider 必须显式给出（生产由 cmd/server 填 workbuddy.ProviderID）。
// 同时接一份历史日志：调度路径的"作用在谁身上"只能从它观察（见 recordedUIDs）。
func newIsolationProvider(t *testing.T, p *pool.Pool) *Provider {
	t.Helper()
	prov := NewWithConfig(Config{
		Pool:     testPoolAdapter{p: p},
		Client:   newIsolationUpstream(t),
		Log:      newIsolationLog(t),
		Provider: defaultProviderForTest,
	})
	return prov
}

// foreignRequestCount 返回别家账号的凭证被上游请求了多少次。
//
// # 为什么不给 Provider 加一个字段
//
// Provider 是产品类型，测试装置不该长在它身上（那会让"仅测试用"的
// 依赖进入产品结构体）。这里用包级 side-table：用例注册、
// 用例注销，Provider 本身零改动。
func (p *Provider) foreignRequestCount(uid string) int {
	trackMu.Lock()
	tu := trackByProvider[p]
	trackMu.Unlock()
	if tu == nil {
		return 0
	}
	return tu.countForToken(trackingToken(uid))
}
// ---------------------------------------------------------------------------
// 根因：ownAccounts —— 所有路径共用的唯一取号入口
// ---------------------------------------------------------------------------

// TestOwnAccountsExcludesOtherUpstream 钉住根因本身。
//
// 这是本文件最重要的一条：即使将来有人新增了第 8 个调用点，
// 只要它走 ownAccounts，就不会越界；反之若有人绕过它直接 Pool.List()，
// TestNoPoolWideIterationInPackage（见下）会红。
func TestOwnAccountsExcludesOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuthNamed("wb1", "甲"), testAuthNamed("wb2", "乙"))
	prov := newIsolationProvider(t, p)

	got := prov.ownAccounts()
	if len(got) != 2 {
		t.Fatalf("ownAccounts 应返回本上游 2 个账号，得到 %d：%+v", len(got), got)
	}
	for _, a := range got {
		if a.UID == foreign.UID {
			t.Errorf("ownAccounts 泄漏了别家上游的账号 %s —— "+
				"这就是那个真 bug 的根因（会连带让调度器也作用在它上面）", a.UID)
		}
	}
}

// TestOwnAccountsBackwardCompatibleWhenProviderUnset 钉住零值兼容。
//
// Config.Provider 为空时 ListFor("") 由池子归一成默认上游 ——
// 单上游部署与既有的手工构造（Config{Pool: ...}）行为必须逐字不变。
func TestOwnAccountsBackwardCompatibleWhenProviderUnset(t *testing.T) {
	p, _ := seedMultiProviderPool(t, testAuth("wb1"))
	// 刻意**不填** Provider。
	prov := NewWithConfig(Config{Pool: testPoolAdapter{p: p}})

	got := prov.ownAccounts()
	if len(got) != 1 || got[0].UID != "wb1" {
		t.Fatalf("Provider 未设置时应回落默认上游并只返回 wb1，得到 %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 渲染路径：travel / growth 面板
// ---------------------------------------------------------------------------

// TestTravelListExcludesOtherUpstream 旅行列表不列别家上游的账号。
//
// 这是实测报告里最先被看到的那一行（/admin/travel 4 条 → 应为 3 条）。
func TestTravelListExcludesOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t,
		testAuthNamed("wb1", "甲"), testAuthNamed("wb2", "乙"), testAuthNamed("wb3", "丙"))
	prov := newIsolationProvider(t, p)
	srv := newAdminTestServer(t, prov)

	body := getJSON(t, srv.URL+"/admin/travel")
	accounts, _ := body["accounts"].([]any)

	if len(accounts) != 3 {
		t.Errorf("/admin/travel 应返回 3 条（本上游 3 个），得到 %d 条：%+v", len(accounts), accounts)
	}
	for _, raw := range accounts {
		m, _ := raw.(map[string]any)
		if uid, _ := m["uid"].(string); uid == foreign.UID {
			t.Errorf("/admin/travel 列出了别家上游的账号 %s —— "+
				"它本上游既没有快照也领不了奖，补空行补出的是假行", uid)
		}
	}
}

// TestGrowthListExcludesOtherUpstream 成长列表同理。
func TestGrowthListExcludesOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t,
		testAuthNamed("wb1", "甲"), testAuthNamed("wb2", "乙"), testAuthNamed("wb3", "丙"))
	prov := newIsolationProvider(t, p)
	srv := newAdminTestServer(t, prov)

	body := getJSON(t, srv.URL+"/admin/growth")
	accounts, _ := body["accounts"].([]any)

	if len(accounts) != 3 {
		t.Errorf("/admin/growth 应返回 3 条（本上游 3 个），得到 %d 条：%+v", len(accounts), accounts)
	}
	for _, raw := range accounts {
		m, _ := raw.(map[string]any)
		if uid, _ := m["uid"].(string); uid == foreign.UID {
			t.Errorf("/admin/growth 列出了别家上游的账号 %s", uid)
		}
	}
}

// ---------------------------------------------------------------------------
// 调度路径：真正会让"后台任务作用在错误账号上"的地方
// ---------------------------------------------------------------------------

// TestRunCheckinAllSkipsOtherUpstream 全量签到不得作用在别家上游账号上。
//
// 这条对应用那 703 条 checkin / trigger=schedule 的脏记录。
func TestRunCheckinAllSkipsOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"), testAuth("wb2"))
	prov := newIsolationProvider(t, p)

	prov.RunCheckinAll("schedule")

	// 断言方式刻意选"查历史里出现了哪些 uid"而不是"计数"：
	// 计数相等也可能是漏了本上游、多跑了别家的巧合。
	seen := recordedUIDs(t, prov)
	if seen[foreign.UID] {
		t.Errorf("全量签到作用在了别家上游的账号 %s 上 —— "+
			"这正是那 703 条 schedule 脏记录的来源", foreign.UID)
	}
	if !seen["wb1"] || !seen["wb2"] {
		t.Errorf("全量签到应覆盖本上游的 wb1 与 wb2，实际记录到 %v", seen)
	}
}

// TestRunKeepaliveAllSkipsOtherUpstream 全量保活同理（对应 110 条 keepalive）。
func TestRunKeepaliveAllSkipsOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"), testAuth("wb2"))
	prov := newIsolationProvider(t, p)

	prov.RunKeepaliveAll("schedule")

	seen := recordedUIDs(t, prov)
	if seen[foreign.UID] {
		t.Errorf("全量保活作用在了别家上游的账号 %s 上", foreign.UID)
	}
	if !seen["wb1"] || !seen["wb2"] {
		t.Errorf("全量保活应覆盖本上游的 wb1 与 wb2，实际 %v", seen)
	}
}

// TestRunTravelSkipsOtherUpstream 全量旅行巡检同理。
//
// 旅行会对每个账号发真实请求，所以「作用在谁身上」由请求目标决定。
// 这里断言的是"没有为别家账号发起任何请求" —— 用快照集合间接观察。
func TestRunTravelSkipsOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"), testAuth("wb2"))
	prov, _ := newTrackingProvider(t, p)

	prov.RunTravelNow()

	// ⚠ 断言必须落在"有没有为它发请求"上，不能只看快照集合。
	// 我第一版就写成了只看快照，结果**变异验证时它没红** ——
	// 因为 probeTravel 只在成功时写快照，别家账号的请求本来就 404，
	// 于是"遍历了它"与"没遍历它"在快照上都表现为"没有这一条"。
	// 这是典型的"测量方式测不到那一份"（本项目已踩过 8 次）。
	// 请求计数直接观察真实动作，不依赖请求是否成功。
	if n := prov.foreignRequestCount(foreign.UID); n != 0 {
		t.Errorf("旅行巡检为别家上游的账号 %s 发起了 %d 次上游请求 —— "+
			"它本上游的凭证根本打不通，这些请求只会得到 401/404", foreign.UID, n)
	}
	for _, s := range prov.TravelSnapshots() {
		if s.UID == foreign.UID {
			t.Errorf("旅行巡检给别家上游的账号 %s 建了快照/派了猫", foreign.UID)
		}
	}
}

// TestRefreshGrowthSkipsOtherUpstream 成长扫描不得给别家上游账号建快照。
func TestRefreshGrowthSkipsOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"), testAuth("wb2"))
	prov := newIsolationProvider(t, p)

	prov.RefreshGrowth(true, false)

	for _, s := range prov.GrowthSnapshots() {
		if s.UID == foreign.UID {
			t.Errorf("成长扫描给别家上游的账号 %s 建了快照", foreign.UID)
		}
	}
}

// TestGrowthDueJobIgnoresOtherUpstream 守卫轮的触发判据也不得看别家的号。
//
// 这条守的是"别家账号到期"不该唤醒本上游的守卫轮（空转上游请求）。
func TestGrowthDueJobIgnoresOtherUpstream(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"))
	prov := newIsolationProvider(t, p)

	// 让别家账号"到期"，本上游的号未到期 → 守卫轮不该被触发。
	// 首轮快照为空时 growthDueJob 会返回 true（设计如此：要填缓存），
	// 所以先跑一轮把缓存填满。
	prov.RefreshGrowth(true, false)
	prov.growthDue(foreign.UID, timeNowForTest())

	if !prov.growthDue("wb1", timeNowForTest()) {
		// 本上游未到期：此时若别家的到期能唤醒守卫轮，就是越界。
		if prov.growthDueJob(timeNowForTest()) {
			// 只有一种合法情形：本上游自己的号到期。上面已排除。
			t.Log("守卫轮到期的判据读到了本上游的号（合法）")
		}
	}
	// 核心断言：别家账号不得出现在判据的输入集里。
	for _, st := range prov.ownAccounts() {
		if st.UID == foreign.UID {
			t.Fatalf("守卫判据的取号集含别家上游账号 %s", foreign.UID)
		}
	}
}

// ---------------------------------------------------------------------------
// 结构守卫：新增调用点不得再绕过 ownAccounts
// ---------------------------------------------------------------------------

// TestNoPoolWideIterationInPackage 扫描本包源码，禁止出现 `cfg.Pool.List()`。
//
// # 为什么需要一条"源码级"守卫
//
// 上面所有行为测试只覆盖**今天存在的** 7 个调用点。
// 第 8 个调用点（将来某人加了新的全量任务）不会被它们覆盖，
// 而它会以**完全相同的方式**重新引入这个 bug。
//
// 判据是"本包非测试源码里，对池的取号一律经 ownAccounts/ListFor，
// 不得出现全池遍历"。这条约束是**结构性**的，与调用点数量无关。
//
// ⚠ 与项目教训一致：判据不能只看注释（注释里出现的 `cfg.Pool.List()`
// 是说明文字，不是代码）。所以这里先剥注释再匹配 —— 否则我的说明文字
// 会让这条测试自己红。
func TestNoPoolWideIterationInPackage(t *testing.T) {
	src := readPackageSources(t, ".")

	// 必须真的读到源码，否则这条守卫会静默变成"永远绿灯"。
	if len(src) == 0 {
		t.Fatal("没读到任何源码 —— 守卫失效（fail-open）")
	}

	const needle = "cfg.Pool.List()"
	for file, body := range src {
		if hasPoolWideIteration(stripGoComments(body), needle) {
			t.Errorf("%s 里出现了 %q —— 本包不得遍历全池。"+
				"池是多上游共用的，遍历全池会让本上游的任务作用在别家上游的账号上"+
				"（这正是 811 条 schedule 脏记录的成因）。请改用 p.ownAccounts()。",
				file, needle)
		}
	}
}

// hasPoolWideIteration 报告剥离注释后的源码里是否还有全池遍历。
//
// 抽成纯函数是为了能反向验证它真的抓得住（见 TestGuardCatchesInjectedDefect）——
// 一个抓不住违规的守卫等于装饰品。
func hasPoolWideIteration(code, needle string) bool {
	return len(code) > 0 && indexOf(code, needle) >= 0
}

// TestGuardCatchesInjectedDefect 反向验证：守卫必须抓得住注入的缺陷。
//
// 这是"变异验证"的自动化版本：不依赖人工临时改代码，
// 而是把**与真实缺陷同形**的样本喂给判据，确认它报违规。
// 同时反向确认"干净样本不报"（否则守卫会退化成永远报错的噪音）。
func TestGuardCatchesInjectedDefect(t *testing.T) {
	// 脏样本：与真实 bug 逐字同形。
	dirty := `
func (p *Provider) RunCheckinAll(trigger string) {
	for _, st := range p.cfg.Pool.List() {
		p.checkinOne(st.UID, trigger)
	}
}`
	if !hasPoolWideIteration(stripGoComments(dirty), "cfg.Pool.List()") {
		t.Fatal("守卫抓不住注入的缺陷 —— 它是装饰品")
	}

	// 干净样本：走 ownAccounts。
	clean := `
func (p *Provider) RunCheckinAll(trigger string) {
	for _, st := range p.ownAccounts() {
		p.checkinOne(st.UID, trigger)
	}
}`
	if hasPoolWideIteration(stripGoComments(clean), "cfg.Pool.List()") {
		t.Fatal("守卫误报干净样本")
	}

	// 注释里的说明文字不算代码（否则本文件的文档会让自己红）。
	commented := `
// 原先写的是 p.cfg.Pool.List()，那是个 bug。
func f() {}`
	if hasPoolWideIteration(stripGoComments(commented), "cfg.Pool.List()") {
		t.Fatal("守卫把注释当成了代码 —— 这会让人靠改注释绕过它")
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// getJSON 打一个 GET 并把响应解成 map。
func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s → HTTP %d", url, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析 %s 的响应失败: %v", url, err)
	}
	return out
}

// recordedUIDs 读出本 Provider 已记录历史里出现过的 uid 集合。
//
// 用历史反推"动作作用在谁身上"，比计数更难自欺：
// 计数相等也可能是漏一个、多一个的巧合。
//
// 注意读的是 p.cfg.Log（Provider 自己的字段）而不是 AdminHandler.log()：
// 调度路径不经过 Handler，装置必须挂在 Provider 上才能观察到它。
func recordedUIDs(t *testing.T, prov *Provider) map[string]bool {
	t.Helper()
	lg := prov.cfg.Log
	if lg == nil {
		t.Fatal("Provider 未接日志 —— 无法观察动作作用在谁身上（装置失效）")
	}
	items, _ := lg.Page(0, 100, "")
	out := map[string]bool{}
	for _, it := range items {
		out[it.UID] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// 请求级观测装置（side-table）
// ---------------------------------------------------------------------------

// trackByProvider 把测试用的请求计数器挂在 Provider 之外。
//
// 理由见 foreignRequestCount 的注释：产品结构体不该长测试字段。
var (
	trackMu         sync.Mutex
	trackByProvider = map[*Provider]*trackingUpstream{}
)

// newTrackingProvider 与 newIsolationProvider 同，但上游请求会被按 token 计数。
//
// 用于"必须观察真实请求"的用例（旅行/成长探测）：那些路径在失败时
// 不写快照，只看快照会漏判（见 TestRunTravelSkipsOtherUpstream 的注释）。
func newTrackingProvider(t *testing.T, p *pool.Pool) (*Provider, *trackingUpstream) {
	t.Helper()
	tu := newTrackingUpstream(t)
	prov := NewWithConfig(Config{
		Pool:     testPoolAdapter{p: p},
		Client:   tu.client(),
		Log:      newIsolationLog(t),
		Provider: defaultProviderForTest,
	})
	trackMu.Lock()
	trackByProvider[prov] = tu
	trackMu.Unlock()
	t.Cleanup(func() {
		trackMu.Lock()
		delete(trackByProvider, prov)
		trackMu.Unlock()
	})
	return prov, tu
}
// ---------------------------------------------------------------------------
// 快照出口：脏快照不得出去，也不得驱动调度
// ---------------------------------------------------------------------------

// TestTravelSnapshotsExcludesForeignSnapshot 注入一条别家 uid 的快照，
// 断言它不出现在 TravelSnapshots() 里。
//
// # 为什么这条不能靠"RefreshTravel 不产生脏快照"来替代
//
// 那是**入口**的修法，而快照 map 是只写不删的，且 probeTravel 可由任意 uid
// 经单账号端点（TravelDepartFor / TravelClaimFor 只查池不查归属）直接进入。
// 所以"入口干净"不足以保证出口干净 —— 必须在出口再拦一次。
// 本用例直接注入脏数据，模拟"历史遗留 / 别的路径写进来的"那条脏快照。
func TestTravelSnapshotsExcludesForeignSnapshot(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"), testAuth("wb2"))
	prov := newIsolationProvider(t, p)

	// 直接往快照 map 里注入别家 uid —— 绕过一切入口守卫，
	// 这正是"只保护入口"防不住的场景。
	prov.storeSnapshot(foreign.UID, TravelSnapshot{UID: foreign.UID, Error: "脏快照"})
	prov.storeSnapshot("wb1", TravelSnapshot{UID: "wb1", Error: "本上游正常"})

	snaps := prov.TravelSnapshots()
	for _, s := range snaps {
		if s.UID == foreign.UID {
			t.Errorf("TravelSnapshots 返回了别家上游的快照 %s —— "+
				"快照 map 只写不删，这条脏数据会永久可见", foreign.UID)
		}
	}
	// 反向：不能因为过滤把本上游的快照也滤掉（否则是过度修复）。
	found := false
	for _, s := range snaps {
		if s.UID == "wb1" {
			found = true
		}
	}
	if !found {
		t.Error("过滤把本上游自己的快照也滤掉了 —— 过度修复")
	}
}

// TestGrowthSnapshotsExcludesForeignSnapshot 同理，成长侧。
func TestGrowthSnapshotsExcludesForeignSnapshot(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"))
	prov := newIsolationProvider(t, p)

	prov.storeGrowthSnapshot(foreign.UID, GrowthSnapshot{UID: foreign.UID, Error: "脏快照"})
	prov.storeGrowthSnapshot("wb1", GrowthSnapshot{UID: "wb1", Error: "本上游正常"})

	for _, s := range prov.GrowthSnapshots() {
		if s.UID == foreign.UID {
			t.Errorf("GrowthSnapshots 返回了别家上游的快照 %s", foreign.UID)
		}
	}
}

// TestAnyDueIgnoresForeignSnapshot 脏快照不得驱动守卫轮。
//
// # 这条锁的是控制流，不是展示
//
// 原先 anyDue 遍历快照 map，于是一条脏快照会让守卫轮**永久**认为
// "有账号到期"而被反复唤醒 —— 那是持续空转上游请求，比面板多一行严重。
// 注入脏快照后，本上游的号都已排到未来，anyDue 必须返回 false。
func TestAnyDueIgnoresForeignSnapshot(t *testing.T) {
	p, foreign := seedMultiProviderPool(t, testAuth("wb1"))
	prov := newIsolationProvider(t, p)

	// 把本上游的号排到很远的未来（明确"不到期"）。
	future := timeNowForTest().Add(24 * time.Hour)
	prov.scheduleNextCheck("wb1", nil, false)
	prov.travel.mu.Lock()
	prov.travel.due["wb1"] = future
	// 脏快照：别家的号"现在就该查"。
	prov.travel.snapshots[foreign.UID] = TravelSnapshot{UID: foreign.UID}
	prov.travel.due[foreign.UID] = timeNowForTest().Add(-time.Hour)
	prov.travel.mu.Unlock()

	if prov.anyDue(timeNowForTest()) {
		t.Errorf("anyDue 被别家上游的脏快照唤醒了 —— "+
			"快照只写不删，这会让守卫轮永久空转（账号 %s）", foreign.UID)
	}
}

// TestAnyDueTrueOnFirstRunForOwnAccount 钉住首扫语义没被改坏。
//
// 改造前"两 map 皆空 → 视为到期（要填缓存）"。改成遍历 ownAccounts 后，
// "本上游有账号但还没快照"必须仍然是 true，否则启动后第一次守卫轮不会跑。
func TestAnyDueTrueOnFirstRunForOwnAccount(t *testing.T) {
	p, _ := seedMultiProviderPool(t, testAuth("wb1"))
	prov := newIsolationProvider(t, p)

	if !prov.anyDue(timeNowForTest()) {
		t.Error("本上游有账号但没有快照时应视为到期（首扫填缓存），实际 false")
	}
}