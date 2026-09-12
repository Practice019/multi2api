// uimanifest_test.go /admin/ui/manifest 的契约测试。
//
// # 这组测试要守住的东西
//
// 判据 1「加新上游核心零改动」在前端上的落点，就是这条端点：
// 前端渲染什么面板**完全由 manifest 决定**。所以测试的重点不是"字段有没有值"，
// 而是三条会真正退化的性质：
//
//  1. 上游声明的每一条管理端点都出现在 manifest 里（漏一条 = 前端少一个面板）
//  2. manifest 里所有能力位都能翻译成名字（翻不出 = 前端渲染出无名导航项）
//  3. 新增上游时 manifest **自动**包含它，且不需要动 admin 包任何一行
//
// # 为什么第 3 条要用"假上游"而不是真 workbuddy
//
// 用真上游验证"零改动"是同义反复 —— 那本来就在注册表里。
// 只有注册一个 admin **从未见过**的 Provider，才能证明发现逻辑是通用的。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// manifestFor 对给定 handler 打一次 /admin/ui/manifest 并解码。
//
// 必须设 RemoteAddr：admin 全部端点都限本机，默认的 192.0.2.1 会拿到 403，
// 那样测出来的是"权限拒绝"而不是我们关心的渲染契约。
func manifestFor(t *testing.T, h *Handler) uiManifest {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200（body=%s）", rec.Code, rec.Body.String())
	}
	var m uiManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest 不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	return m
}

// TestUIManifestEmpty 未接线任何上游时必须是**可渲染的空态**，不是 500。
//
// 前端要能在"未配置任何上游"的部署下正常显示空页面。返回 500 会让整页崩，
// 而那正是新装用户的首次体验。
func TestUIManifestEmpty(t *testing.T) {
	m := manifestFor(t, New(Config{ServiceName: "svc"}))

	if m.Service != "svc" {
		t.Errorf("service=%q want %q", m.Service, "svc")
	}
	// nil 与空切片在前端都是"没有"，但 JSON 里 null 与 [] 的行为不同
	// （null 会让 .map() 直接抛错）。所以断言的是"能安全遍历"。
	if m.Providers == nil || m.AdminRoutes == nil || m.Jobs == nil || m.Capabilities == nil {
		t.Errorf("空注册表时必须返回空数组而不是 null（否则前端 .map() 抛错）: %+v", m)
	}
	// 能力位字典与上游无关，必须**始终**全量下发。
	if len(m.Capabilities) != len(gateway.AllCapabilities()) {
		t.Errorf("能力位字典=%d 项，want %d（应与 AllCapabilities 同步）",
			len(m.Capabilities), len(gateway.AllCapabilities()))
	}
}

// TestUIManifestAggregatesEveryProvider 主聚合循环的**完备性**断言。
//
// # 为什么必须单独有这一条（评审 F1 的落点）
//
// 下面所有测试都只注册**一个**上游并断言"它出现了"。那种断言只能证明
// "至少聚合了一个"，**证明不了"每个都被聚合了"** —— 循环里写一个
// `if i > 0 { continue }` 就能让第 2..N 个上游整体消失，而全部测试仍绿。
//
// 这不是理论担忧：评审在整树副本上实测过，注入该 bug 后
// `go test ./internal/admin/` 全部 PASS，而 manifest 真的少了一个上游
// （生产语义：codearts 的 4 条端点消失，24 → 20，codearts 组与福利面板整体不见）。
//
// 所以这里断言的是**集合相等**，不是"存在"。
func TestUIManifestAggregatesEveryProvider(t *testing.T) {
	reg := gateway.NewRegistry()
	want := []struct {
		id string
		n  int
	}{{"alpha", 2}, {"beta", 1}, {"gamma", 3}}

	for _, spec := range want {
		routes := make([]gateway.AdminRoute, 0, spec.n)
		for i := 0; i < spec.n; i++ {
			routes = append(routes, gateway.AdminRoute{
				Method:     "GET",
				Path:       spec.id + "-" + strconv.Itoa(i),
				Capability: gateway.CapChat,
				Title:      spec.id,
			})
		}
		if err := reg.Register(&fakeUpstream{id: spec.id, routes: routes}); err != nil {
			t.Fatal(err)
		}
	}
	m := manifestFor(t, New(Config{Registry: reg}))

	// 1) providers 数必须等于注册数
	if len(m.Providers) != len(want) {
		t.Fatalf("providers=%d，want %d —— 主聚合循环漏了上游", len(m.Providers), len(want))
	}

	// 2) 每个上游的路由条数都要对上（防"上游在但端点少")
	got := map[string]int{}
	for _, p := range m.Providers {
		got[p.ID] = 0
	}
	for _, rt := range m.AdminRoutes {
		got[rt.Provider]++
	}
	total := 0
	for _, spec := range want {
		total += spec.n
		c, ok := got[spec.id]
		if !ok {
			t.Errorf("上游 %q 整体消失在 manifest 里", spec.id)
			continue
		}
		if c != spec.n {
			t.Errorf("上游 %q 的路由=%d 条，want %d", spec.id, c, spec.n)
		}
	}

	// 3) 总数也要对上（防"分组数对但总数少"）
	if len(m.AdminRoutes) != total {
		t.Errorf("admin_routes 总数=%d，want %d", len(m.AdminRoutes), total)
	}
}

// TestUIManifestFakeUpstream 核心判据：注册一个 admin **从未见过**的上游，
// 它的端点必须自动出现在 manifest 里 —— admin 包一行都不用改。
//
// 这是判据 1（加新上游核心零改动）在**前端契约层**的可执行证明。
func TestUIManifestFakeUpstream(t *testing.T) {
	reg := gateway.NewRegistry()
	up := &fakeUpstream{
		id: "ghost",
		routes: []gateway.AdminRoute{
			{Method: "GET", Path: "/admin/ghost-list", Capability: gateway.CapWelfare, Title: "幽灵福利"},
			{Method: "POST", Path: "/admin/ghost-claim", Capability: gateway.CapWelfare, Title: "幽灵领取"},
		},
	}
	if err := reg.Register(up); err != nil {
		t.Fatal(err)
	}
	m := manifestFor(t, New(Config{Registry: reg, DefaultProvider: "ghost"}))

	// 1) 上游本身出现，并被标为默认
	var found bool
	for _, p := range m.Providers {
		if p.ID == "ghost" {
			found = true
			if !p.Default {
				t.Error("ghost 未被标为默认上游（DefaultProvider=\"ghost\"）")
			}
		}
	}
	if !found {
		t.Fatalf("假上游 ghost 没出现在 manifest.providers 里：%+v", m.Providers)
	}

	// 2) 它声明的**两条**端点都出现，且能力位与标题原样带出
	got := map[string]uiAdminRoute{}
	for _, rt := range m.AdminRoutes {
		if rt.Provider == "ghost" {
			got[rt.Path] = rt
		}
	}
	if len(got) != 2 {
		t.Fatalf("ghost 的端点=%d 条，want 2（漏端点=前端少面板）: %+v", len(got), got)
	}
	if r := got["/admin/ghost-list"]; r.Capability != "welfare" || r.Title != "幽灵福利" {
		t.Errorf("/admin/ghost-list 契约不符: cap=%q title=%q，want cap=welfare title=幽灵福利",
			r.Capability, r.Title)
	}
}

// TestUIManifestCapabilityTitles 每个已定义能力位都必须在 manifest 里翻译出名字。
//
// # 反向验证
//
// 若 capTitles 漏登记一个能力位，capTitle 会**静默退回 id**（如显示 "quota-probe"
// 而不是「额度探测」）—— 页面不会报错，只是多了个英文导航项。
// 那种缺陷只能靠这条断言抓住，所以它比"标题好不好看"重要得多。
func TestUIManifestCapabilityTitles(t *testing.T) {
	m := manifestFor(t, New(Config{}))

	byID := map[string]string{}
	for _, c := range m.Capabilities {
		if c.ID == "" {
			t.Error("能力位字典里有空 id")
		}
		if c.Title == "" {
			t.Errorf("能力位 %q 没有标题（前端会渲染出无名导航项）", c.ID)
		}
		byID[c.ID] = c.Title
	}

	// 每一个已定义能力位都必须在字典里
	for _, c := range gateway.AllCapabilities() {
		name := gateway.String(c)
		if _, ok := byID[name]; !ok {
			t.Errorf("能力位 %q 不在 manifest.capabilities 里", name)
		}
	}

	// 已登记的能力位必须给出**中文**标题，而不是退回 id。
	// 这条是真正的守门：capTitle 的退回行为让"漏登记"不会自我暴露。
	//
	// 例外：CoreCapability（"core"）**不是真实能力位**，它不出现在
	// manifest.capabilities 里（那份列表来自 gateway.AllCapabilities），
	// 只在 admin_routes[].capability 上出现。所以这里跳过它。
	for id := range capTitles {
		if id == CoreCapability {
			if capTitle(id) == id {
				t.Errorf("保留名 %q 必须有中文标题（如「通用端点」），否则前端显示英文", id)
			}
			continue
		}
		if got := byID[id]; got == "" {
			t.Errorf("capTitles 里有 %q 但 manifest 没带出", id)
		} else if got == id {
			t.Errorf("能力位 %q 的标题就是它自己的 id —— capTitle 退回了兜底值", id)
		}
	}
}

// TestUIManifestCorRoutesUseReservedName 未声明能力位的路由必须用保留名 core，
// 且该名字**不能**与任何真实能力位重名。
//
// # 为什么这条重要（评审 F2 的收口）
//
// 前端靠 "core" 把这类端点归入通用区；一旦它与某个真实能力位重名，
// 那些端点会被错误地当成"某个能力的面板"，而不会报任何错。
func TestUIManifestCorRoutesUseReservedName(t *testing.T) {
	for _, c := range gateway.AllCapabilities() {
		if gateway.String(c) == CoreCapability {
			t.Fatalf("保留名 %q 与真实能力位重名了 —— 前端将无法区分两者", CoreCapability)
		}
	}
	// routeCapName(0) 必须是保留名本身
	if got := routeCapName(0); got != CoreCapability {
		t.Errorf("routeCapName(0)=%q，want %q", got, CoreCapability)
	}
	// 保留名必须能翻译出标题（前端会渲染它）
	if capTitle(CoreCapability) == CoreCapability {
		t.Errorf("保留名 %q 没有中文标题", CoreCapability)
	}
}

// TestUIManifestRoutesAreSorted 端点顺序必须**稳定**，否则前端导航项每次刷新都跳位。
//
// # 为什么断言的是"同一上游内按 Path 排序"而不是"按上游排序"
//
// 第一版写的是"alpha 必须排在 zeta 前面"，那条断言是**空转的**：
// gateway.Registry.All() 内部已经 `sort.Strings(ids)`，上游本来就按字典序返回，
// 所以即使把 manifest 里的排序整段删掉，那条断言照样通过。
// （实测确认：注入"禁用 Provider 排序键"的 bug 后测试仍然绿。）
//
// 现在断言**同一上游内部**的多条路由按 Path 升序 —— 这个顺序只可能来自
// manifest 自己的 sort.SliceStable，注册顺序是它唯一的对手。
//
// 反向验证：删掉 uimanifest.go 里的 sort.SliceStable，本测试变红。
func TestUIManifestRoutesAreSorted(t *testing.T) {
	reg := gateway.NewRegistry()
	// 一个上游，三条**逆序声明**的路由。若 manifest 不排序，
	// 输出顺序会是 z, m, a（声明顺序），与期望的 a, m, z 不符。
	up := &fakeUpstream{
		id: "solo",
		routes: []gateway.AdminRoute{
			{Method: "GET", Path: "/admin/z-route", Capability: gateway.CapChat, Title: "Z"},
			{Method: "GET", Path: "/admin/m-route", Capability: gateway.CapChat, Title: "M"},
			{Method: "GET", Path: "/admin/a-route", Capability: gateway.CapChat, Title: "A"},
		},
	}
	if err := reg.Register(up); err != nil {
		t.Fatal(err)
	}
	m := manifestFor(t, New(Config{Registry: reg}))

	if len(m.AdminRoutes) != 3 {
		t.Fatalf("路由数=%d want 3", len(m.AdminRoutes))
	}
	got := []string{m.AdminRoutes[0].Path, m.AdminRoutes[1].Path, m.AdminRoutes[2].Path}
	want := []string{"/admin/a-route", "/admin/m-route", "/admin/z-route"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("路由顺序=%v，want %v —— 声明顺序泄漏到输出（前端导航会跳位）", got, want)
		}
	}
}

// TestUIManifestCoreCapabilityForUndeclaredRoute 未声明能力位的上游端点
// 必须归到保留字 "core"，而不是空串。
//
// 空串会让前端把它们全归进一个没有 id 的分组（导航上表现为一个空白组）。
// "core" 是保留字，不可能是真实能力位名，所以前端可以安全地用它表示"归通用组"。
func TestUIManifestCoreCapabilityForUndeclaredRoute(t *testing.T) {
	reg := gateway.NewRegistry()
	up := &fakeUpstream{
		id: "bare",
		routes: []gateway.AdminRoute{
			{Method: "GET", Path: "/admin/bare", Title: "无能力位"}, // Capability 留 0
		},
	}
	if err := reg.Register(up); err != nil {
		t.Fatal(err)
	}
	m := manifestFor(t, New(Config{Registry: reg}))

	if len(m.AdminRoutes) != 1 {
		t.Fatalf("路由数=%d want 1", len(m.AdminRoutes))
	}
	if got := m.AdminRoutes[0].Capability; got != "core" {
		t.Errorf("未声明能力位的端点 cap=%q，want \"core\"（空串会让前端多一个空白分组）", got)
	}
	// "core" 必须不是真实能力位名 —— 否则上面那条断言会把真能力误判成通用
	for _, c := range gateway.AllCapabilities() {
		if gateway.String(c) == "core" {
			t.Fatal("「core」被用作真实能力位名了，保留字冲突")
		}
	}
}

// TestUIManifestAccountCount 每个上游带出账号数（导航分组标题要显示它）。
func TestUIManifestAccountCount(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&fakeUpstream{id: "wb"}); err != nil {
		t.Fatal(err)
	}
	// 建一个带 2 个 wb 账号 + 1 个 other 账号的池。
	//
	// 用 AddFor（而不是 Add）是刻意的：Add 会把账号归到**默认上游**，
	// 而这条测试要验证的正是"按 provider 过滤"，必须让两个上游都有账号。
	p := pool.New("")
	p.AddFor("wb", &auth.Auth{UID: "u1", Nickname: "一号"}, nil)
	p.AddFor("wb", &auth.Auth{UID: "u2", Nickname: "二号"}, nil)
	p.AddFor("other", &auth.Auth{UID: "c1", Nickname: "三号"}, nil)

	m := manifestFor(t, New(Config{Registry: reg, Pool: p, DefaultProvider: "wb"}))

	counts := map[string]int{}
	for _, pi := range m.Providers {
		counts[pi.ID] = pi.AccountCount
	}
	if counts["wb"] != 2 {
		t.Errorf("wb 账号数=%d want 2（应按 provider 过滤，不是全池 3 个）", counts["wb"])
	}
}

// TestUIManifestCapTitleFallbackIsVisible 记录 capTitle 的**已知局限**：
// 未登记的能力位会静默退回 id。
//
// 这里不试图消灭这个行为 —— 在 manifest 里显示 "quota-probe" 比显示空白好，
// 前端至少还能给出一个可点的入口。但必须让"退回发生了"这件事有据可查：
// TestUIManifestCapabilityTitles 负责在**已定义**能力位漏登记时报错，
// 本测试则把兜底行为本身钉住，防止有人把它改成返回空串。
func TestUIManifestCapTitleFallbackIsVisible(t *testing.T) {
	if got := capTitle("some-brand-new-cap"); got != "some-brand-new-cap" {
		t.Errorf("未登记能力位的标题=%q，want 退回 id 本身（返回空串会让前端渲染出无名项）", got)
	}
}

// 让编译器确认 fakeUpstream 满足 gateway.Provider + AdminExt，
// 免得将来 Provider 接口变化时这里的替身悄悄失配。
var _ gateway.Provider = (*fakeUpstream)(nil)
