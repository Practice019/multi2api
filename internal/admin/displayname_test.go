package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestProviderDisplayNameReachesManifest 上游自报的展示名要**真的下发到 manifest**。
//
// # 这条修的是用户报的问题
//
// 界面此前只显示 `Provider.ID()`，于是 `qoder` 与 `qodercn` 只差一个后缀 ——
// 而它们是两个不同的服务（不同域名、不同 clientId）。用户看不出后者是
// 「中国版」，加账号时不知道该点哪个。
//
// 判据：实现了 DisplayNameExt 的上游，`display_name` 出现在 manifest 里。
func TestProviderDisplayNameReachesManifest(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(namedProvider{id: "fakecn", name: "Fake (中国版)"}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq(http.MethodGet, "/admin/ui/manifest"))

	var m uiManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest 不是 JSON: %v", err)
	}
	if len(m.Providers) != 1 {
		t.Fatalf("provider 数 = %d，want 1", len(m.Providers))
	}
	if got := m.Providers[0].DisplayName; got != "Fake (中国版)" {
		t.Errorf("display_name = %q，want %q —— "+
			"上游自报的展示名没有下发到 manifest，界面上仍只会看到裸 id", got, "Fake (中国版)")
	}
}

// TestProviderWithoutDisplayNameOmitsField 未实现扩展点时**字段不下发**。
//
// # 这是向后兼容的落点
//
// 既有部署（上游都没实现这个扩展点）升级后，manifest 必须与改造前
// **逐字节相同** —— 所以用 `omitempty`，并在这里钉住"字段确实不出现"。
//
// ⚠ 判据是"JSON 里没有这个键"，不是"字段值为空串"：
// 后者会让前端拿到 `display_name: ""` 而不是 `undefined`，
// 回落逻辑就得同时判两种空 —— 不如不发。
func TestProviderWithoutDisplayNameOmitsField(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(plainNoNameProvider{id: "plain"}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq(http.MethodGet, "/admin/ui/manifest"))

	// 直接看原始 JSON：断言**键不存在**。
	var raw struct {
		Providers []map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Providers) != 1 {
		t.Fatalf("provider 数 = %d", len(raw.Providers))
	}
	if _, has := raw.Providers[0]["display_name"]; has {
		t.Error("未实现 DisplayNameExt 的上游不该下发 display_name —— " +
			"既有部署的 manifest 必须与改造前逐字节相同")
	}
}

// TestProviderEmptyDisplayNameFallsBackToOmit 报回空串 = 不实现（同样不下发）。
//
// 区分"实现了但返回空"与"没实现"没有意义：两者都表示"没有更好的名字"。
// 让它们走同一条路，少一个状态。
func TestProviderEmptyDisplayNameFallsBackToOmit(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(namedProvider{id: "blank", name: "   "}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq(http.MethodGet, "/admin/ui/manifest"))

	var raw struct {
		Providers []map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, has := raw.Providers[0]["display_name"]; has {
		t.Error("空串（含纯空白）应视为「没有名字」→ 不下发，与未实现走同一条路")
	}
}

// TestProvidersEndpointAlsoCarriesDisplayName /admin/providers 也要带展示名。
//
// 两个端点各自组装 providerInfo（schedule.go 与 uimanifest.go）——
// 只接一个的话，界面上会"某个面板显示中文名、另一个显示裸 id"。
func TestProvidersEndpointAlsoCarriesDisplayName(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(namedProvider{id: "fakecn", name: "Fake (中国版)"}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq(http.MethodGet, "/admin/providers"))

	var out struct {
		Providers []providerInfo `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("provider 数 = %d，want 1", len(out.Providers))
	}
	if got := out.Providers[0].DisplayName; got != "Fake (中国版)" {
		t.Errorf("/admin/providers 的 display_name = %q，want %q —— "+
			"两个端点各组装一次 providerInfo，只接一个会让界面不一致", got, "Fake (中国版)")
	}
}

// adminReq 造一个**本机**请求（httptest 默认的 192.0.2.1 会被 admin 的
// 本机校验挡成 403，拿不到 manifest —— 那不是被测的事）。
func adminReq(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "127.0.0.1:12345"
	return r
}

// namedProvider 实现了 DisplayNameExt 的桩上游。
type namedProvider struct {
	id   string
	name string
}

func (p namedProvider) ID() string               { return p.id }
func (p namedProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p namedProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (p namedProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (p namedProvider) DisplayName() string { return p.name }

// plainNoNameProvider 没实现 DisplayNameExt 的桩上游。
type plainNoNameProvider struct{ id string }

func (p plainNoNameProvider) ID() string               { return p.id }
func (p plainNoNameProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p plainNoNameProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (p plainNoNameProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
