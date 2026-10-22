package monkeycode

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestLoginFlowDiscoverable LoginFlow 必须能被 ExtOf 类型断言发现。
//
// # 为什么单独钉这一条
//
// 扩展点靠**类型断言**发现，而不是访问器方法。若把 Start/Poll/Configured
// 写在内部类型上（比如 loginFlow 结构体）而不转发到 *Provider，
// 本包测试可能全绿，但核心 ExtOf[LoginFlow](p) 返回 false ——
// 表现是 manifest 里 login:null、界面上没有「添加账号」按钮。
// raccoon 实测踩过这个形态。
func TestLoginFlowDiscoverable(t *testing.T) {
	p := New(Config{})
	lf, ok := gateway.ExtOf[gateway.LoginFlow](p)
	if !ok {
		t.Fatal("ExtOf[LoginFlow] 没发现 *Provider —— 「添加账号」按钮不会出现")
	}
	if !lf.Configured() {
		t.Fatal("Configured() 应为 true：粘贴流程不需要任何部署侧配置")
	}
}

// TestManualLoginDiscoverable ManualLoginExt 也必须能被发现。
func TestManualLoginDiscoverable(t *testing.T) {
	p := New(Config{})
	me, ok := gateway.ExtOf[gateway.ManualLoginExt](p)
	if !ok {
		t.Fatal("ExtOf[ManualLoginExt] 没发现 *Provider —— 粘贴框不会渲染")
	}
	path, instructions := me.ManualLogin()
	if path == "" {
		t.Error("ManualLogin 的 path 为空，前端不会渲染粘贴框")
	}
	if path != adminManualCompletePath {
		t.Errorf("ManualLogin 的 path 与注册的路由不一致：%q != %q", path, adminManualCompletePath)
	}
	if instructions == "" {
		t.Error("instructions 为空，用户不知道要粘什么")
	}
}

// TestStartReturnsEmptyAuthURL authURL 必须为空串。
//
// 前端对空串有专门分支（不渲染「打开授权页」）；编一个假链接会让
// 用户点开一个 404，而且不报错。
func TestStartReturnsEmptyAuthURL(t *testing.T) {
	p := New(Config{})
	state, authURL, err := p.Start()
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if state == "" {
		t.Error("state 为空")
	}
	if authURL != "" {
		t.Errorf("authURL 应为空（本上游没有授权页），实际 %q", authURL)
	}
}

// TestPollAlwaysPending Poll 恒回 ErrLoginPending。
func TestPollAlwaysPending(t *testing.T) {
	p := New(Config{})
	state, _, _ := p.Start()
	if _, err := p.Poll(state); err != gateway.ErrLoginPending {
		t.Fatalf("Poll 应回 ErrLoginPending，实际 %v", err)
	}
}

// TestManualCompleteEndpointRegistered 粘贴端点必须挂在 AdminRoutes 上。
func TestManualCompleteEndpointRegistered(t *testing.T) {
	p := New(Config{})
	found := false
	for _, rt := range p.AdminRoutes() {
		if rt.Path == adminManualCompletePath {
			found = true
			if !rt.Hidden {
				t.Error("粘贴端点应为 Hidden（它是按钮的落地端点，不是导航页）")
			}
		}
	}
	if !found {
		t.Errorf("AdminRoutes 里没有 %s —— 前端粘完会 POST 到 404", adminManualCompletePath)
	}
}
