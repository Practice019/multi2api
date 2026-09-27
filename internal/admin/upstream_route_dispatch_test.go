package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestUpstreamRoutesActuallyDispatch 上游声明的管理端点必须**真的能路由到**。
//
// # 这条测试的来历（含一次**误判**，如实记下）
//
// 给 cline/raccoon/qoder 加余额端点后，我用 `?uid=x`（一个**不存在的账号**）
// 探测，得到 404，于是判断"路由没挂上"，并花了几轮去查 mountUpstreamRoutes。
//
// 真相是：**那 404 是我自己的 handler 返回的** ——
// `resolveCred` 找不到账号时就是回 404（"账号不在池里"）。
// 换成真实 uid 后同一个端点回 **HTTP 200**。路由一直是好的。
//
// 教训：**探测一个端点是否可达，不能用会让它"合理地失败"的输入**。
// 404 既可以表示"没有这条路由"，也可以表示"有路由但资源不存在"，
// 两者在只看状态码时不可区分。
//
// 所以这条测试用**桩上游 + 永远成功的 handler**：它只回答一个问题 ——
// "AdminRoutes 里声明的东西，mux 里到底有没有"。那是 mountUpstreamRoutes
// 的职责，与任何 handler 内部的业务判断无关。
//
// # 它仍然值得存在（不是"因为搞错了才补的"）
//
// manifest（/admin/ui/manifest）与路由表是**两条独立的路径**：
//
//	manifest  只读 AdminRoutes()，不看 mux
//	路由表    由 mountUpstreamRoutes 挂载，可能因冲突被跳过
//
// 所以"manifest 里有、路由表里没有"是**可能存在的状态**，而此前
// 没有任何测试把后者钉住 —— 那种状态下前端会渲染出按钮、点下去 404，
// 且日志里只有一行"与已有端点冲突"（很容易被忽略）。
func TestUpstreamRoutesActuallyDispatch(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(stubAdminProvider{}); err != nil {
		t.Fatal(err)
	}

	h := New(Config{Registry: reg})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, stubPath, nil)
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound {
		t.Fatalf("上游声明的 %s 返回 404 —— "+
			"说明 AdminRoutes 被声明了但**没有真的挂上路由**。"+
			"manifest 里能看到它，前端会渲染出按钮，点下去 404。", stubPath)
	}
}

// stubPath 测试桩声明的端点路径。
const stubPath = "/admin/stubroute/probe"

// stubAdminProvider 一个只声明一条管理端点的最小上游。
type stubAdminProvider struct{}

func (stubAdminProvider) ID() string               { return "stubroute" }
func (stubAdminProvider) Caps() gateway.Capability { return gateway.CapChat }
func (stubAdminProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (stubAdminProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: "x"}}, nil
}
func (stubAdminProvider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{{
		Method: http.MethodGet,
		Path:   stubPath,
		Handler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}}
}
