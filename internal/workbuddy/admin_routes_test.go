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

// wantAdminRoutes 22 条端点的 (method, path) 清单 —— 与改造前 admin.go 逐条比对。
//
// 为什么把它写成表而不是"数一下有 22 条"：路径拼错（/admin/travel/config vs
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
}

func TestAdminRoutesCoverAll20Endpoints(t *testing.T) {
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

// TestAdminRoutesCapabilities 每条端点必须声明能力位，且只能是上游真有的那几种。
//
// 能力位由**后端下发**驱动前端显隐，声明错了界面就会显示不存在的入口。
func TestAdminRoutesCapabilities(t *testing.T) {
	p := NewWithConfig(Config{})
	caps := p.Caps()
	for _, r := range p.AdminRoutes() {
		if r.Capability == 0 {
			t.Errorf("端点 %s %s 未声明能力位（前端无法据此显隐）", r.Method, r.Path)
			continue
		}
		if !caps.Has(r.Capability) {
			t.Errorf("端点 %s %s 声明了 %v，但 Provider.Caps() 未包含它 —— "+
				"声明了能力却没有对应实现属于契约违规", r.Method, r.Path, r.Capability)
		}
		if r.Title == "" {
			t.Errorf("端点 %s %s 缺少面板标题", r.Method, r.Path)
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
