// 独立复现 Reviewer 的 #1 发现：契约检查器的假阴性。
//
// 他的具体指控：
//
//	A. Caps 声明 CapModels，Models() 返回 (nil, nil) → 不被抓
//	D. 声明全部 7 个能力位但零实现 → 不被抓（只校验了 CapModels）
//	E. ID() 每次返回不同值 → 不被抓（只调用了一次）
//	H. 只声明 CapChat 的 Provider，Models() panic → 从不被调用
//	J. Chat 返回 Status:0 + nil Body → 不被抓
package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// quietT 只记录，不让测试失败（我们要断言的是"契约有没有报错"）。
type quietT struct {
	failed bool
	msgs   []string
}

func (q *quietT) Helper() {}
func (q *quietT) Errorf(f string, a ...any) {
	q.failed = true
	q.msgs = append(q.msgs, f)
}
func (q *quietT) Fatalf(f string, a ...any) {
	q.failed = true
	q.msgs = append(q.msgs, "FATAL "+f)
	panic(fatalSentinel{})
}

func runQuiet(p Provider) *quietT {
	q := &quietT{}
	func() {
		defer func() { recover() }()
		RunProviderContract(q, func() Provider { return p })
	}()
	return q
}

// ---- 指控 A：Models 返回空却不报错 ----
type emptyModelsProvider struct{}

func (e *emptyModelsProvider) ID() string       { return "emptymodels" }
func (e *emptyModelsProvider) Caps() Capability { return CapChat | CapModels }
func (e *emptyModelsProvider) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{Status: 200, Body: &fakeStream{body: "x"}}, nil
}
func (e *emptyModelsProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return nil, nil // 声称支持模型，却永远返回空
}

// ---- 指控 D：声明全部能力位，零实现 ----
type allCapsProvider struct{}

func (a *allCapsProvider) ID() string { return "allcaps" }
func (a *allCapsProvider) Caps() Capability {
	return CapChat | CapModels | CapCheckin | CapGrowth | CapTravel | CapWelfare | CapQuotaProbe
}
func (a *allCapsProvider) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{Status: 200, Body: &fakeStream{body: "x"}}, nil
}
func (a *allCapsProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}

// ---- 指控 E：ID 不稳定 ----
type unstableIDProvider struct{ n int }

func (u *unstableIDProvider) ID() string       { u.n++; return "id-" + string(rune('0'+u.n)) }
func (u *unstableIDProvider) Caps() Capability { return CapChat }
func (u *unstableIDProvider) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{Status: 200, Body: &fakeStream{body: "x"}}, nil
}
func (u *unstableIDProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return nil, nil
}

// ---- 指控 H：只声明 CapChat 的 Provider，Models panic 从不被调用 ----
type modelsPanicNoCapProvider struct{ modelsCalled bool }

func (m *modelsPanicNoCapProvider) ID() string       { return "nocapliar" }
func (m *modelsPanicNoCapProvider) Caps() Capability { return CapChat }
func (m *modelsPanicNoCapProvider) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{Status: 200, Body: &fakeStream{body: "x"}}, nil
}
func (m *modelsPanicNoCapProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	m.modelsCalled = true
	panic("Models 炸了")
}

// ---- 指控 J：Chat 返回零值 Status + nil Body ----
type zeroStreamProvider struct{}

func (z *zeroStreamProvider) ID() string       { return "zerostream" }
func (z *zeroStreamProvider) Caps() Capability { return CapChat }
func (z *zeroStreamProvider) Chat(ctx context.Context, c Credential, b []byte) (ChatStream, error) {
	return ChatStream{}, nil // 既没状态码也没 body
}
func (z *zeroStreamProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return nil, nil
}

// TestReviewerCP0_F2_EmptyAdminRoutesMustFail 复现阶段 0 评审的 F2 绕过。
//
// 评审的原始攻击：
//
//	声明全部 7 个能力位 + `AdminRoutes() []AdminRoute { return nil }`
//	→ `RunProviderContract` **零报错通过**
//
// 空实现比不实现更省事，所以这是"假声明"最可能的形态，必须挡住。
func TestReviewerCP0_F2_EmptyAdminRoutesMustFail(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		desc string
	}{
		{"全能力位+空路由", &allCapsEmptyRoutes{}, "声明 7 个能力位却返回 nil 路由"},
		{"路由缺 Handler", &capsWithBrokenRoute{}, "路由结构不完整（Handler=nil）"},
		{"路由重复", &capsWithDupRoute{}, "同 method+path 注册两次 → 挂载时 panic"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := runQuiet(c.p)
			if !q.failed {
				t.Errorf("契约未抓到违规【%s】：%s", c.name, c.desc)
			} else {
				t.Logf("✓ 抓到: %v", q.msgs)
			}
		})
	}
}

// allCapsEmptyRoutes 声明全部能力位但 AdminRoutes 返回 nil（评审的原攻击）。
type allCapsEmptyRoutes struct{ goodProvider }

func (a *allCapsEmptyRoutes) ID() string { return "allcapsempty" }
func (a *allCapsEmptyRoutes) Caps() Capability {
	return CapChat | CapModels | CapCheckin | CapGrowth | CapTravel | CapWelfare | CapQuotaProbe
}
func (a *allCapsEmptyRoutes) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}
func (a *allCapsEmptyRoutes) AdminRoutes() []AdminRoute { return nil }

// capsWithBrokenRoute 路由结构不完整。
type capsWithBrokenRoute struct{ goodProvider }

func (c *capsWithBrokenRoute) ID() string       { return "brokenroute" }
func (c *capsWithBrokenRoute) Caps() Capability { return CapChat | CapGrowth }
func (c *capsWithBrokenRoute) Models(ctx context.Context, cr Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}
func (c *capsWithBrokenRoute) AdminRoutes() []AdminRoute {
	return []AdminRoute{{Method: "GET", Path: "/admin/x", Handler: nil}}
}

// capsWithDupRoute 重复路由（挂载时 Go 1.22 mux 会 panic）。
type capsWithDupRoute struct{ goodProvider }

func (d *capsWithDupRoute) ID() string       { return "duproute" }
func (d *capsWithDupRoute) Caps() Capability { return CapChat | CapGrowth }
func (d *capsWithDupRoute) Models(ctx context.Context, cr Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}
func (d *capsWithDupRoute) AdminRoutes() []AdminRoute {
	h := func(w http.ResponseWriter, r *http.Request) {}
	return []AdminRoute{
		{Method: "GET", Path: "/admin/x", Handler: h},
		{Method: "GET", Path: "/admin/x", Handler: h},
	}
}

// TestReviewerFindings_ContractFn 复现 Reviewer 的发现。
//
// 这个测试的**期望**是"契约应该抓到" —— 它现在会 FAIL（因为契约确实没抓到），
// 从而把假阴性固化成可见的回归测试。修好契约后它应转绿。
func TestReviewerFindings_ContractFn(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		desc string
	}{
		{"A-空模型列表", &emptyModelsProvider{}, "声明 CapModels 但 Models 永远返回空 —— 等于没实现"},
		{"D-全能力零实现", &allCapsProvider{}, "声明 5 个未经验证的能力位（checkin/growth/travel/welfare/quota）"},
		{"E-ID不稳定", &unstableIDProvider{}, "ID() 每次返回不同值 —— Registry 查找会失效"},
		{"J-零值流", &zeroStreamProvider{}, "Chat 返回 Status:0 + nil Body —— 调用方拿到空响应"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := runQuiet(c.p)
			if !q.failed {
				t.Errorf("契约未抓到违规【%s】：%s\n  → 这是假阴性，契约测试形同虚设", c.name, c.desc)
			} else {
				t.Logf("✓ 抓到了: %s", strings.Join(q.msgs, " | "))
			}
		})
	}
}

// 单独验证指控 H：只声明 CapChat 时，Models 不被调用（所以其中的 panic 不会被发现）。
func TestReviewerFinding_H_ModelsNotProbedWhenCapAbsent(t *testing.T) {
	p := &modelsPanicNoCapProvider{}
	q := runQuiet(p)
	// 契约没崩 = 没调用 Models
	if p.modelsCalled {
		t.Log("契约调用了 Models（比预期更严格）")
	}
	if q.failed {
		t.Errorf("不该因 Models panic 报错（因为它压根没被调用）: %v", q.msgs)
	}
	// 但这**是个漏洞**：一个 Models 会 panic 的实现通过了契约。
	// 修好后的契约应当：要么调用它（发现 panic），要么明确要求"没声明就别实现"。
	t.Log("⚠ 已确认：Models 会 panic 的实现通过了契约（声明里没有 CapModels）")
}
