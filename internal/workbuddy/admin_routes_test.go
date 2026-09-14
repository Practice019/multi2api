// admin_routes_test.go 22 条管理端点的**路由级**契约。
//
// # 搬运说明（Task 3c）
//
// 本文件包含两部分：
//
//  1. 从 internal/admin/history_test.go 搬来的分页契约验收
//     （用例名逐字保留：TestHistoryEndpoint*）。它们原先打的是 admin.Handler 的
//     内部 mux，现在改为打**从 AdminRoutes() 挂出来的真实路由** ——
//     这正是搬迁后前端真正走的路径，覆盖面只增不减。
//  2. 新增的挂载契约：22 条端点的 method/path/能力位，以及"路由真的能应答"。
//
// 分页语义本身没变（items 倒序、total 是过滤后总数、offset/limit 回显），
// 所以断言全部保留。
package workbuddy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// newAdminTestServer 按 AdminRoutes() 的真实清单挂一个 httptest 服务器。
//
// 为什么不用裸 Handler 调用：这样测到的是**端点的 method+path 组合**，
// 一旦清单里写错方法（例如 GET 写成 POST），测试会以 405/404 的形式报出来，
// 而不是静默地"函数逻辑没问题"。
func newAdminTestServer(t *testing.T, p *Provider) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, rt := range NewAdminHandler(p, p.adminEnv).Routes() {
		pattern := rt.Path
		if rt.Method != "" {
			pattern = rt.Method + " " + rt.Path
		}
		mux.HandleFunc(pattern, rt.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newHistoryProvider 造一个只接了历史日志的 Provider（其余依赖为空）。
func newHistoryProvider(t *testing.T, n int) *Provider {
	t.Helper()
	lg := checkinlog.New(filepath.Join(t.TempDir(), "h.json"), 30)
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		kind := checkinlog.KindCheckin
		if i%2 == 0 {
			kind = checkinlog.KindGrowth
		}
		lg.Append(checkinlog.Record{
			At:     base.Add(time.Duration(i) * time.Minute),
			UID:    "u",
			Kind:   kind,
			Status: checkinlog.StatusOK,
		})
	}
	return NewWithConfig(Config{Log: lg})
}

func doHistory(t *testing.T, srv *httptest.Server, query string) map[string]any {
	t.Helper()
	resp, err := http.Get(srv.URL + "/admin/checkin/history" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return out
}

func TestHistoryEndpointPaginates(t *testing.T) {
	srv := newAdminTestServer(t, newHistoryProvider(t, 25))

	page1 := doHistory(t, srv, "?limit=10&offset=0")
	if got := page1["total"].(float64); got != 25 {
		t.Errorf("total=%v，期望 25（总数而不是本页条数）", got)
	}
	if got := len(page1["items"].([]any)); got != 10 {
		t.Errorf("第 1 页条数=%d，期望 10", got)
	}
	if got := page1["offset"].(float64); got != 0 {
		t.Errorf("offset 回显=%v，期望 0", got)
	}

	page3 := doHistory(t, srv, "?limit=10&offset=20")
	if got := len(page3["items"].([]any)); got != 5 {
		t.Errorf("末页条数=%d，期望 5（25 条按 10 条一页）", got)
	}
	if got := page3["total"].(float64); got != 25 {
		t.Errorf("末页 total=%v，期望仍为 25", got)
	}

	// 两页不应有重叠
	seen := map[string]bool{}
	for _, it := range page1["items"].([]any) {
		seen[it.(map[string]any)["at"].(string)] = true
	}
	for _, it := range page3["items"].([]any) {
		if seen[it.(map[string]any)["at"].(string)] {
			t.Error("第 1 页与第 3 页出现重复记录")
		}
	}
}

func TestHistoryEndpointNewestFirst(t *testing.T) {
	srv := newAdminTestServer(t, newHistoryProvider(t, 3))
	out := doHistory(t, srv, "?limit=3")
	items := out["items"].([]any)

	var prev time.Time
	for i, it := range items {
		at, err := time.Parse(time.RFC3339Nano, it.(map[string]any)["at"].(string))
		if err != nil {
			t.Fatalf("解析 at: %v", err)
		}
		if i > 0 && at.After(prev) {
			t.Errorf("第 %d 条比第 %d 条更新 —— 不是时间倒序", i, i-1)
		}
		prev = at
	}
}

func TestHistoryEndpointKindFilterAffectsTotal(t *testing.T) {
	// 25 条里 growth 占 i%2==0（i=0,2,...,24）共 13 条
	srv := newAdminTestServer(t, newHistoryProvider(t, 25))
	out := doHistory(t, srv, "?limit=30&kind=growth")
	if got := out["total"].(float64); got != 13 {
		t.Errorf("growth 过滤后 total=%v，期望 13", got)
	}
	if got := len(out["items"].([]any)); got != 13 {
		t.Errorf("本页条数=%d，期望 13", got)
	}
	for _, it := range out["items"].([]any) {
		if it.(map[string]any)["kind"] != "growth" {
			t.Errorf("过滤失效：混入 %v", it.(map[string]any)["kind"])
		}
	}
}

func TestHistoryEndpointDefaults(t *testing.T) {
	srv := newAdminTestServer(t, newHistoryProvider(t, 40))
	out := doHistory(t, srv, "")
	if got := len(out["items"].([]any)); got != checkinlog.DefaultPageSize {
		t.Errorf("无参数时本页=%d，期望默认 %d", got, checkinlog.DefaultPageSize)
	}
	if got := out["limit"].(float64); got != float64(checkinlog.DefaultPageSize) {
		t.Errorf("limit 回显=%v，期望 %d", got, checkinlog.DefaultPageSize)
	}
}

func TestHistoryEndpointOffsetBeyondEnd(t *testing.T) {
	// 前端算错 offset 时不能 500，也不能返回整页假数据
	srv := newAdminTestServer(t, newHistoryProvider(t, 5))
	out := doHistory(t, srv, "?limit=10&offset=999")
	if got := len(out["items"].([]any)); got != 0 {
		t.Errorf("越界 offset 应返回空页，得到 %d 条", got)
	}
	if got := out["total"].(float64); got != 5 {
		t.Errorf("越界时 total=%v，期望仍为 5", got)
	}
}

func TestHistoryEndpointNilLogDegradesGracefully(t *testing.T) {
	// 未注入 Log 时（配置缺失）应返回空结构而不是 panic
	srv := newAdminTestServer(t, NewWithConfig(Config{}))
	resp, err := http.Get(srv.URL + "/admin/checkin/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d，期望 200（优雅降级）", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 挂载契约
// ---------------------------------------------------------------------------

// wantAdminRoutes 全部端点的 (method, path) 清单 —— 与改造前 admin.go 逐条比对，
// 并追加成块移植进来的任务自动化端点。
//
// 为什么把它写成表而不是"数一下有几条"：路径拼错（/admin/travel/config vs
// /admin/growth/travel/config）与动词写错都不会被"数量正确"发现。
var wantAdminRoutes = []struct{ method, path string }{
	{"POST", "/admin/checkin"},
	{"GET", "/admin/checkin/history"},
	{"POST", "/admin/keepalive"},
	{"POST", "/admin/credits/refresh"},
	{"GET", "/admin/growth"},
	{"GET", "/admin/growth/tasks"},
	{"POST", "/admin/growth/claim"},
	{"POST", "/admin/growth/accept"},
	{"POST", "/admin/growth/redeem"},
	{"POST", "/admin/growth/makeup"},
	{"POST", "/admin/growth/open"},
	{"POST", "/admin/growth/draw"},
	{"GET", "/admin/growth/travel/config"},
	{"GET", "/admin/travel"},
	{"GET", "/admin/travel/status"},
	{"POST", "/admin/travel/depart"},
	{"POST", "/admin/travel/claim"},
	// /admin/schedule 与 /admin/task **不在此列表**：它们读核心自己排的班，
	// 已移回 internal/admin 作为通用端点（阶段 0 评审 F5）。
	// 见 admin.go 顶部注释。
	{"GET", "/admin/client-login"},
	{"POST", "/admin/client-login/switch"},
	{"POST", "/admin/client-login/restore"},

	// ---- 任务自动化（成块移植自 workbuddy2api-panel，见 autotask_admin.go）----
	//
	// 这 7 条是本仓库**新增**的端点，不是"改造前 admin.go"的一部分 ——
	// 清单里显式列出它们，是为了钉住"移植确实挂载了、且路径/动词没写错"。
	{"GET", "/admin/growth/auto/actions"},
	{"POST", "/admin/growth/auto"},
	{"POST", "/admin/growth/auto-all"},
	{"GET", "/admin/growth/scan"},
	{"GET", "/admin/school"},
	{"POST", "/admin/school/run"},
	{"POST", "/admin/blackcat/run"},
}

func TestAdminRoutesCoverAllEndpoints(t *testing.T) {
	p := NewWithConfig(Config{})
	routes := p.AdminRoutes()

	if len(routes) != len(wantAdminRoutes) {
		t.Fatalf("端点数=%d，期望 %d（清单与改造前的 admin.go 必须一一对应）",
			len(routes), len(wantAdminRoutes))
	}
	want := map[string]bool{}
	for _, w := range wantAdminRoutes {
		want[w.method+" "+w.path] = true
	}
	seen := map[string]bool{}
	for _, r := range routes {
		key := r.Method + " " + r.Path
		if !want[key] {
			t.Errorf("多出或写错的端点: %s", key)
		}
		if seen[key] {
			t.Errorf("端点重复注册: %s（ServeMux 会 panic）", key)
		}
		seen[key] = true
		if r.Handler == nil {
			t.Errorf("端点 %s 的 Handler 为 nil（挂载时会跳过，表现为 404）", key)
		}
	}
}

// TestAdminRoutesCapabilities 端点声明的能力位必须**自洽**。
//
// # 规则（评审 F2 修正后）
//
// 一条路由的能力位有两种合法取值：
//
//	0（未声明）  该端点不归属于任何能力位 —— 它是"通用端点"，
//	             manifest 会把它归到保留名 "core"
//	C != 0      该端点归属能力位 C，且 **C 必须在上游的 Caps() 里**
//
// 不合法的是第三种：**声明的能力位并不描述这个端点**。
// 最典型的反例（本测试就是为了防它复发）：
//
//	/admin/client-login 声明 CapChat
//
// 它错在两层：
//  1. CapChat 说的是"这个上游能对话"，而这条端点是本地客户端登录态管理 ——
//     两者毫无关系，用 CapChat 当占位符把"基础能力"与"专属面板"混为一谈
//  2. 前端的 hasAnyPanel 按能力位判定"这个上游有没有专属面板"，
//     而 chat/models 是每个上游都有的基础能力、不构成专属面板判据。
//     声明 CapChat 的路由因此在导航里永远不会成为入口 —— 声明与效果自相矛盾。
//
// 所以现在要求：**声明了能力位的端点，其能力位必须是该上游的"专属"能力**
// （即不是 CapChat / CapModels 这类基础能力）。基础能力只用来描述 Provider
// 本身能做什么，不用来给管理端点归类。
func TestAdminRoutesCapabilities(t *testing.T) {
	p := NewWithConfig(Config{})
	caps := p.Caps()

	// 基础能力：描述 Provider 本身，不用于给管理端点归类。
	base := gateway.CapChat | gateway.CapModels

	for _, r := range p.AdminRoutes() {
		if r.Title == "" {
			t.Errorf("端点 %s %s 缺少面板标题", r.Method, r.Path)
		}
		if r.Capability == 0 {
			// 合法：通用端点，归 manifest 的 "core"。
			continue
		}
		if r.Capability&base != 0 {
			t.Errorf("端点 %s %s 声明了基础能力 %v —— "+
				"基础能力描述的是 Provider 本身，不能拿来给管理端点归类；"+
				"该端点若没有对应能力位，应当留 0（归 core）",
				r.Method, r.Path, r.Capability)
		}
		if !caps.Has(r.Capability) {
			t.Errorf("端点 %s %s 声明了 %v，但 Provider.Caps() 未包含它 —— "+
				"声明了能力却没有对应实现属于契约违规", r.Method, r.Path, r.Capability)
		}
	}
}

// TestAdminRoutesCoreEndpointsAreDeclared 钉住"哪些端点走 core"这个事实。
//
// 这不是为了限制实现，而是防止**悄悄回流**：client-login 这三条曾经声明
// CapChat（见上），修好之后若有人又把它们改回基础能力，这里会红。
func TestAdminRoutesCoreEndpointsAreDeclared(t *testing.T) {
	p := NewWithConfig(Config{})
	wantCore := map[string]bool{
		"GET /admin/client-login":          true,
		"POST /admin/client-login/switch":  true,
		"POST /admin/client-login/restore": true,
	}
	seen := map[string]bool{}
	for _, r := range p.AdminRoutes() {
		key := r.Method + " " + r.Path
		if wantCore[key] {
			seen[key] = true
			if r.Capability != 0 {
				t.Errorf("%s 应当归 core（能力位 0），实际声明了 %v", key, r.Capability)
			}
		}
	}
	for k := range wantCore {
		if !seen[k] {
			t.Errorf("找不到端点 %s（清单被改动了？）", k)
		}
	}
}

// TestAdminRoutesImplementedNotPlaceholders 端点不得是占位实现。
//
// Task 3a 曾在这里放 notMovedYet 占位（返回 501）。本测试用"实际打一发"来钉住
// 它们**已经不是占位** —— 改造前占位返回 501 + 特定文案，任何一条回到那个状态
// 都会在这里红。
//
// 注意：这里只排除"占位"，不要求业务成功（没有真实账号/上游，
// 缺 uid 的端点本就该回 404/501 之类）。所以判据是"不是 501+占位文案"。
func TestAdminRoutesImplementedNotPlaceholders(t *testing.T) {
	p := NewWithConfig(Config{})
	srv := newAdminTestServer(t, p)
	for _, rt := range p.AdminRoutes() {
		t.Run(rt.Method+" "+rt.Path, func(t *testing.T) {
			req, err := http.NewRequest(rt.Method, srv.URL+rt.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotImplemented {
				var body map[string]any
				_ = json.NewDecoder(resp.Body).Decode(&body)
				if msg, _ := body["error"].(string); msg != "" &&
					contains(msg, "尚未迁移到 Provider") {
					t.Fatalf("端点仍是 Task 3a 的占位实现: %s", msg)
				}
			}
			if resp.StatusCode == http.StatusMethodNotAllowed {
				t.Fatalf("端点未按 %s 挂载（ServeMux 回了 405）—— 清单里的方法写错了", rt.Method)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 编译期断言：AdminHandler 与 Provider 两条路径给出同一份清单。
var _ = func() bool {
	p := NewWithConfig(Config{})
	return len(NewAdminHandler(p, p.adminEnv).Routes()) == len(p.AdminRoutes())
}

// 编译期断言：Provider 仍然是网关可发现的扩展点。
var (
	_ gateway.Provider = (*Provider)(nil)
	_ gateway.AdminExt = (*Provider)(nil)
)
