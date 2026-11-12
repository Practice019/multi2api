package phanthywork

import (
	"net/http"

	"workbuddy2api/internal/gateway"
)

// adminroute.go 把「粘贴回跳地址完成登录」暴露成一条管理端点。
//
// # 为什么必须有这条路由（否则 ManualLoginExt 是个死胡同）
//
// gateway.ManualLoginExt 只声明"我有这条逃生路径"（path + instructions），
// **它本身不提供端点** —— 核心会把 path 下发到前端、由前端 POST 它。
// 那个 path 由谁实现？由**上游自己**挂到 AdminRoutes 上
// （形态抄 internal/monkeycode/login.go 与 internal/codearts/manuallogin.go）。
//
// 少了这条路由的后果是完全静默的：manifest 里 login.manual 有值、
// 前端渲染出「粘贴回跳 URL 完成」输入框，用户粘完点提交得到 404。
//
// # 编译期断言
//
// AdminExt 与 ManualLoginExt 是**两个独立**接口，少实现任何一个都只会
// 静默失效（ExtOf 断言失败不报错）。两条都钉住。
var (
	_ gateway.AdminExt       = (*Provider)(nil)
	_ gateway.ManualLoginExt = (*Provider)(nil)
)

// AdminRoutes 返回本上游的管理端点。
//
// 挂三类（2026-11-12 更新 —— 此前只有手动兜底一条）：

//	· 签到两条（`checkinRoutes`，Capability: CapCheckin）
//	· 邀请码面板（`inviteRoute`，Capability: CapInvite）
//	· 手动兜底登录完成（Capability: 0 —— 它属于登录流程，不是能力位入口）

// ⚠ 纪律不变：**声明之外的路由不许挂** —— 能力位没声明却挂了端点，就是"隐藏入口"，
//
//	前端看不见、只有知道路径的人能用（那不是设计，那是后门）。
//	所以上面的路由与 `Caps()` 的声明一一对应，有断言守着（invite_test.go）。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	out := p.checkinRoutes()
	// 邀请码面板（CapInvite）—— 入口由能力位驱动，前端不认上游名。
	// ⚠ 它是**读了会写**的面板（平台的取码只有 POST，没有只读字段）；护栏见 invite.go 的文件头。
	out = append(out, p.inviteRoute())
	return append(out, gateway.AdminRoute{
		Method: http.MethodPost,
		Path:   ManualLoginPath,
		Handler: func(w http.ResponseWriter, r *http.Request) {
			p.handleManualComplete(w, r)
		},
		// Capability 为 0：这是登录流程的一部分，不是任何能力位的入口。
		Capability: 0,
		Title:      "粘贴回跳地址完成登录",
		// Hidden：它是**纯动作**端点，不该在左侧导航占一个标签页。
		Hidden: true,
	})
}
