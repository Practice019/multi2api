package gateway

import (
	"context"
	"net/http"
	"testing"
)

// 阶段 1 评审 F2 的回归：AdminExt 的 handler 一调就 panic，必须被抓到。
//
// 评审原话：`verifyAdminRoutes` 只验结构（Method/Path/Handler 非 nil、无重复），
// 一个声明 CapGrowth、handler 全是 `panic("boom")` 的实现**零报错通过**。
// 结构是完美的，行为是崩溃的。
//
// 修法：契约真调一次每条 handler 并 recover。

// panickyAdmin 声明能力、路由结构完整，但 handler 全 panic。
type panickyAdmin struct{ goodProvider }

func (p *panickyAdmin) ID() string       { return "panickyadmin" }
func (p *panickyAdmin) Caps() Capability { return CapChat | CapGrowth }

func (p *panickyAdmin) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}

func (p *panickyAdmin) AdminRoutes() []AdminRoute {
	boom := func(w http.ResponseWriter, r *http.Request) { panic("boom") }
	return []AdminRoute{
		{Method: "GET", Path: "/admin/growth", Handler: boom, Capability: CapGrowth},
		{Method: "POST", Path: "/admin/growth/claim", Handler: boom, Capability: CapGrowth},
	}
}

// TestReviewerCP1_F2_PanickyAdminRoutesMustFail 结构性完整但一调就炸 → 不合格。
func TestReviewerCP1_F2_PanickyAdminRoutesMustFail(t *testing.T) {
	q := runQuiet(&panickyAdmin{})
	if !q.failed {
		t.Error("契约未抓到「handler 全部 panic」的 AdminExt —— 评审 F2 复发。\n" +
			"  结构检查挡不住'挂得上去但一调就炸'，而那正是管理端点最糟的失败形态。")
	} else {
		t.Logf("✓ 抓到: %v", q.msgs)
	}
}

// TestReviewerCP1_F2_GoodAdminRoutesPass 正常 handler 不该被误伤。
//
// 反向验证：加了"真调一次"之后，合法实现必须仍然通过 ——
// 否则这条检查会逼着人为了变绿而弱化实现。
type goodAdminProvider struct{ goodProvider }

func (g *goodAdminProvider) ID() string       { return "goodadmin" }
func (g *goodAdminProvider) Caps() Capability { return CapChat | CapGrowth }

func (g *goodAdminProvider) Models(ctx context.Context, c Credential) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}

func (g *goodAdminProvider) AdminRoutes() []AdminRoute {
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	return []AdminRoute{
		{Method: "GET", Path: "/admin/growth", Handler: ok, Capability: CapGrowth},
	}
}

func TestReviewerCP1_F2_GoodAdminRoutesPass(t *testing.T) {
	q := runQuiet(&goodAdminProvider{})
	if q.failed {
		t.Errorf("合法实现被新的 handler 检查误伤: %v", q.msgs)
	}
}
