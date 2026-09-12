// mount_test.go 上游管理端点的**动态挂载**契约（Task 4 的落点）。
//
// # 这一层为什么要单独测
//
// 22 个端点搬进 workbuddy 之后，它们能不能被访问**取决于 admin 有没有把它们
// 从注册表里捞出来挂上**。这段胶水代码的价值全在"真的挂上了"，
// 而它自己不会因为单元测试而正确 —— 所以这里用假 Provider 做端到端验证。
//
// 更关键的是反向验证：如果 mountUpstreamRoutes 因为某个条件提前 return，
// 上面那条测试会变成"零上游 → 零端点 → 静默通过"。所以本文件同时断言
// **假 Provider 的端点真的可达**，而不是只断言没有报错。
package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/gateway"
)

// fakeUpstream 一个只实现 Provider + AdminExt 的最小上游。
//
// 刻意不 import workbuddy：这条测试要证明的是 **admin 不认识任何具体上游**
// 也能把端点挂上 —— 用真 workbuddy 反而会掩盖这一点。
type fakeUpstream struct {
	id     string
	routes []gateway.AdminRoute
}

func (f *fakeUpstream) ID() string               { return f.id }
func (f *fakeUpstream) Caps() gateway.Capability { return gateway.CapChat }
func (f *fakeUpstream) Chat(ctx context.Context, c gateway.Credential, b []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (f *fakeUpstream) Models(ctx context.Context, c gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (f *fakeUpstream) AdminRoutes() []gateway.AdminRoute { return f.routes }

// plainUpstream 不实现 AdminExt 的上游（应被安全跳过）。
//
// ⚠ 必须**不内嵌** fakeUpstream：内嵌会把它的 AdminRoutes 方法一并提升上来，
// 于是这个"没有 AdminExt"的测试替身实际上有 AdminExt —— 测试变成空转。
// 这也是为什么 ExtOf 的判定要用类型断言而不是"方法集里有没有"。
type plainUpstream struct{ id string }

func (p *plainUpstream) ID() string               { return p.id }
func (p *plainUpstream) Caps() gateway.Capability { return gateway.CapChat }
func (p *plainUpstream) Chat(ctx context.Context, c gateway.Credential, b []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (p *plainUpstream) Models(ctx context.Context, c gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

func TestMountUpstreamRoutes(t *testing.T) {
	reg := gateway.NewRegistry()
	up := &fakeUpstream{
		id: "fake",
		routes: []gateway.AdminRoute{
			{Method: "GET", Path: "/admin/fake-probe", Handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}, Capability: gateway.CapChat, Title: "假端点"},
			{Method: "POST", Path: "/admin/fake-action", Handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"posted":true}`))
			}},
		},
	}
	if err := reg.Register(up); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg})

	// 上游端点必须可达 —— 这是"挂载真的发生了"的唯一证据
	req := httptest.NewRequest(http.MethodGet, "/admin/fake-probe", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("上游端点未挂上：HTTP %d（挂载逻辑提前退出了）", rec.Code)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("body=%q，端点被别的东西覆盖了", got)
	}

	// 方法必须被尊重：GET 挂上的端点用 POST 打应该 405
	req2 := httptest.NewRequest(http.MethodPost, "/admin/fake-probe", nil)
	req2.RemoteAddr = "127.0.0.1:12345"
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 端点被 POST 命中（HTTP %d）—— 方法没进 pattern", rec2.Code)
	}
}

// TestMountUpstreamRoutesNilRegistry 未接线注册表时必须安全（测试路径与降级路径）。
func TestMountUpstreamRoutesNilRegistry(t *testing.T) {
	h := New(Config{}) // Registry == nil
	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("未接线注册表不该影响通用端点：HTTP %d", rec.Code)
	}
}

// TestMountUpstreamRoutesSkipsConflict 上游端点与通用端点同名时**保留通用端点**。
//
// 为什么这条重要：静默覆盖会让"账号列表突然变成某个上游的实现"，
// 而那是最难查的一类故障 —— 界面还能打开，数据却来自别处。
func TestMountUpstreamRoutesSkipsConflict(t *testing.T) {
	reg := gateway.NewRegistry()
	up := &fakeUpstream{
		id: "hijack",
		routes: []gateway.AdminRoute{
			{Method: "GET", Path: "/admin/stats", Handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"hijacked":true}`))
			}},
		},
	}
	if err := reg.Register(up); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg})

	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if got := rec.Body.String(); got == `{"hijacked":true}` {
		t.Fatal("通用端点被上游端点覆盖了 —— 冲突时必须保留通用端点")
	}
	// 真实 /admin/stats 的响应里有 total 字段
	if body := rec.Body.String(); len(body) < 2 || body[:2] != `{"` {
		t.Errorf("通用端点响应异常: %s", body)
	}
}

// TestMountUpstreamRoutesSkipsIncomplete 不完整的声明（空路径/空 handler）应被跳过而不是 panic。
//
// 一个生产上游写错一行不该让整个网关起不来。
func TestMountUpstreamRoutesSkipsIncomplete(t *testing.T) {
	reg := gateway.NewRegistry()
	up := &fakeUpstream{
		id: "broken",
		routes: []gateway.AdminRoute{
			{Method: "GET", Path: "", Handler: func(w http.ResponseWriter, r *http.Request) {}},
			{Method: "GET", Path: "/admin/broken", Handler: nil},
		},
	}
	if err := reg.Register(up); err != nil {
		t.Fatal(err)
	}
	// 不 panic 即通过；这里再确认通用端点照常
	h := New(Config{Registry: reg})
	req := httptest.NewRequest(http.MethodGet, "/admin/logs", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("残留断言：HTTP %d", rec.Code)
	}
}

// TestMountUpstreamWithoutAdminExt 未实现 AdminExt 的上游被安全跳过。
//
// 目标只是"没实现 AdminExt 的上游不会让 New 崩"，所以只构造 Handler 即可 ——
// 不设断言会让这条测试变成空壳，所以额外确认注册表里确实有那个上游。
func TestMountUpstreamWithoutAdminExt(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&plainUpstream{id: "plain"}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg})
	if h == nil {
		t.Fatal("New 返回了 nil")
	}
	if reg.Len() != 1 {
		t.Fatalf("注册表应有 1 个上游，实际 %d", reg.Len())
	}
	if _, ok := gateway.ExtOf[gateway.AdminExt](reg.All()[0]); ok {
		t.Fatal("plainUpstream 不该被发现为 AdminExt")
	}
}

// TestGenericRoutesSurviveWithoutAnyUpstream 没有上游时 23 条通用端点仍在。
//
// 这是"核心不依赖任何上游"的直接体现：拔掉所有上游，管理台仍可用。
func TestGenericRoutesSurviveWithoutAnyUpstream(t *testing.T) {
	h := New(Config{Registry: gateway.NewRegistry()})
	for _, path := range []string{"/admin/settings", "/admin/logs"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("通用端点 %s 在上游全拔掉后 404 了 —— 核心不该依赖上游", path)
		}
	}
}
