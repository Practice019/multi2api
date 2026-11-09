package loomy

// manuallogin.go —— loomy 的「添加账号」走**自己的登录页**（T-332）。
//
// # 为什么要补这两个方法（用户截图报的缺陷）
//
// 本上游此前只实现了 gateway.LoginFlow：它的 `Start()` 返回
// `/admin/loomy/login?state=…` —— 那是**我们自己的登录页**，不是上游的授权 URL。
// 而前端在 manifest 里看不到 `login.manual` 时，会把 Start 的返回值当"授权链接"渲染：
//
//	「重新无痕打开」/「复制链接」/「普通窗口打开」
//	「未能自动打开浏览器（授权链接必须是 https://，实际 scheme=''）」
//	「仍在等待浏览器完成登录…」
//
// 用户看到的正是这一堆（截图）。页面本身一直是好的（T-328 起与 codar 共用
// internal/loginui 模板），缺的只是"在 manifest 里声明它是个页面"。
//
// # 与 codar 的同形关系
//
//	loomy  manual = {page:true, path:"/admin/loomy/login"}
//	codar  manual = {page:true, path:"/admin/codar/login"}
//
// 前端只按 `page` 这个布尔分流（数据驱动，不认上游名），于是两家共用同一套弹窗。
//
// # state 怎么进页面
//
// loomy 的登录页需要本次会话的 state（它随每次 POST 回传）。前端的做法是：
// **仍然调用 Start()** 拿 state，然后把 Start 返回的 `/admin/loomy/login?state=…`
// 作为 iframe 的 src；codar 的 Start 返回空串，则回落到 manual.path。

import (
	"workbuddy2api/internal/gateway"
)

var (
	_ gateway.ManualLoginExt     = (*Provider)(nil)
	_ gateway.ManualLoginPageExt = (*Provider)(nil)
)

// ManualLogin 让分组头的「添加账号」直接内嵌本上游的登录页。
//
// 返回的 path 是 login.go 里那个登录页常量（T-328 起由 internal/loginui 渲染）。
func (p *Provider) ManualLogin() (string, string) {
	return loginPagePath,
		"填手机号 → 收短信 → 输入验证码。登录成功后凭据直接写入 auths/loomy/，" +
			"再回「账号池」点一次「重载 auths」即可看到该账号。" +
			"（若本机装着 Loomy 客户端且已登录，控制台会优先复用它的登录态，不发短信。）"
}

// ManualLoginIsPage 声明 path 是**页面**（内嵌 iframe），不是"粘贴回跳 URL"的 POST 端点。
func (p *Provider) ManualLoginIsPage() bool { return true }
