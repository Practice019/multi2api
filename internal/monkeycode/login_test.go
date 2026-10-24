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

// TestStartReturnsEmptyAuthURL Start 返回空 authURL 是**设计如此**，不是缺陷。
//
// # 为什么判据又反回来了（第二次反转，两次都有实测依据）
//
// 这条判据改过两次，方向相反，每次都是被真实缺陷推动的：
//
//	第 1 版：authURL 必须为空
//	  —— 理由「本上游没有授权页，登录态在本机桌面客户端里」。
//	  错在：MonkeyCode 的登录确实有授权页，前端见到空串会走
//	  「读本机 workbuddy 客户端」那条分支，与本上游无关（用户实测报的缺陷）。
//
//	第 2 版：authURL 必须是真实 authorize URL（本用例的上一版）
//	  —— 理由「Start 打上游 login 入口取 302」。
//	  错在：5baef8d 之后登录整体改成**浏览器登录**（StartBrowserLogin），
//	  由服务端起 Chromium 打开登录页、CDP 读 cookie，不再需要把 URL 交给前端。
//	  此时返回空串是**正确**的：前端据空串走「轮询等结果」分支，
//	  而不是「打开链接」分支 —— 两者是不同语义，不能混。
//
// 所以现在钉的是：**state 非空（可用于轮询）+ authURL 为空（走轮询分支）+ 不报错**。
//
// ⚠ 这条用例会真的起浏览器（StartBrowserLogin），所以必须能在无头/CI 环境跳过。
//
//	它不是纯逻辑用例，别把它当单元测试用。
func TestStartReturnsEmptyAuthURL(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过：Start 会真起浏览器（StartBrowserLogin）")
	}
	p := New(Config{})
	state, authURL, err := p.Start()
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// state 是轮询句柄，必须有 —— 否则前端无法查登录结果。
	if state == "" {
		t.Error("state 为空 —— 前端拿不到轮询句柄，登录结果永远查不到")
	}
	// authURL 必须为空：这是「浏览器登录」与「把链接交给用户点」的语义分界。
	// 非空会让前端走「打开链接」分支，与服务端已经起好的浏览器冲突（开两个窗口）。
	if authURL != "" {
		t.Errorf("authURL 应为空（浏览器登录由服务端起，前端只轮询），实得: %q", authURL)
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
