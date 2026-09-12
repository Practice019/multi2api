package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/gateway"
)

// Task 8：面板按上游分组 + 能力位驱动显隐。
//
// # 为什么需要 /admin/providers
//
// 前端要按上游分组渲染账号池、并按**该上游声明的能力**决定显示哪些入口
// （workbuddy 有成长/旅行，codearts 有福利/配额）。
//
// 能力位必须由**后端下发** —— 前端硬编码能力表的话，
// 加第三个上游就要改前端，那正是判据 1 要避免的。

// providerInfo 的**定义在 schedule.go**（它是对外契约的一部分）。
// 测试直接用它，不重复声明 —— 重复声明会让"契约"出现两个来源，
// 改了一处另一处不同步时测试反而看不出来。

// TestProvidersEndpointListsAll 端点列出全部已注册上游及其能力。
func TestProvidersEndpointListsAll(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&stubProvider{id: "alpha", caps: gateway.CapChat | gateway.CapModels}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&stubProvider{id: "beta", caps: gateway.CapChat | gateway.CapWelfare | gateway.CapQuotaProbe}); err != nil {
		t.Fatal(err)
	}

	h := New(Config{Registry: reg, DefaultProvider: "alpha"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("GET", "/admin/providers"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Providers []providerInfo `json:"providers"`
		Default   string         `json:"default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v body=%s", err, rec.Body)
	}

	if len(resp.Providers) != 2 {
		t.Fatalf("providers 数=%d want 2（%s）", len(resp.Providers), rec.Body)
	}
	byID := map[string]providerInfo{}
	for _, p := range resp.Providers {
		byID[p.ID] = p
	}

	// 能力位必须如实下发
	alpha := byID["alpha"]
	if alpha.ID != "alpha" {
		t.Errorf("缺 alpha: %+v", resp.Providers)
	}
	if !contains(alpha.Capabilities, "chat") || !contains(alpha.Capabilities, "models") {
		t.Errorf("alpha 能力位不对: %v", alpha.Capabilities)
	}
	if contains(alpha.Capabilities, "welfare") {
		t.Errorf("alpha 不该有 welfare（它没声明）: %v", alpha.Capabilities)
	}

	beta := byID["beta"]
	for _, want := range []string{"chat", "welfare", "quota-probe"} {
		if !contains(beta.Capabilities, want) {
			t.Errorf("beta 缺能力位 %q: %v", want, beta.Capabilities)
		}
	}
	if contains(beta.Capabilities, "models") {
		t.Errorf("beta 不该有 models（它没声明）: %v", beta.Capabilities)
	}

	// default 标记
	if resp.Default != "alpha" {
		t.Errorf("default=%q want alpha", resp.Default)
	}
	if !byID["alpha"].Default {
		t.Error("alpha 应被标为 default")
	}
	if byID["beta"].Default {
		t.Error("beta 不该被标为 default")
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// TestProvidersEndpointEmptyRegistry 没有上游时返回空列表而不是报错。
//
// 前端要能在"未配置任何上游"的部署下正常渲染（显示空态），
// 而不是拿到 500 后整页崩。
func TestProvidersEndpointEmptyRegistry(t *testing.T) {
	h := New(Config{Registry: gateway.NewRegistry()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localReq("GET", "/admin/providers"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d（空注册表不该报错）", rec.Code)
	}
	var resp struct {
		Providers []providerInfo `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Providers) != 0 {
		t.Errorf("应为空列表，得到 %v", resp.Providers)
	}
}

// TestProvidersEndpointNilRegistry nil 注册表也要降级，不 panic。
func TestProvidersEndpointNilRegistry(t *testing.T) {
	h := New(Config{})
	rec := httptest.NewRecorder()
	// 不应 panic
	h.ServeHTTP(rec, localReq("GET", "/admin/providers"))
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d want 200（nil 注册表应降级为空列表）", rec.Code)
	}
}

// localReq 造一个**本机**请求。
//
// 管理端点有"仅允许本机直连"的校验（见 admin 的 403 分支），
// httptest.NewRequest 默认给的 RemoteAddr 是 192.0.2.1（TEST-NET），
// 会被拦成 403。既有测试（mount_test.go）同样显式设成 127.0.0.1。
func localReq(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	return req
}

// stubProvider 测试用的最小 Provider。
type stubProvider struct {
	id   string
	caps gateway.Capability
}

func (s *stubProvider) ID() string               { return s.id }
func (s *stubProvider) Caps() gateway.Capability { return s.caps }
func (s *stubProvider) Chat(ctx context.Context, c gateway.Credential, b []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (s *stubProvider) Models(ctx context.Context, c gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
