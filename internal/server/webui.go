// Package server — 内嵌本地控制台 /ui。
// 单文件 HTML，零外部依赖、零构建步骤；与网关同源，因此不需要 CORS 头。
package server

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"net/http"
	"strings"

	"workbuddy2api/internal/account"
)

//go:embed webui.html
var webuiHTML []byte

//go:embed webui-login.html
var loginHTML []byte

//go:embed favicon.svg
var faviconSVG []byte

// colPlaceholders 把 HTML 静态部分的表格列数占位符替换成真实数字。
//
// # 为什么需要它
//
// webui.html 的静态 <tbody> 里写了 colspan="__COLS_LOGS__" 这类占位符，
// 本意是复用 JS 里那份 COLS 常量、避免列数写两遍。
//
// **但 JS 的 `${COLS.logs}` 在静态 HTML 里不会被求值** —— 浏览器把它们
// 当成普通文本，于是 `colspan="${COLS.logs}"` 变成一个非法属性值，
// 空态行的跨列失效（真实缺陷，浏览器实测 colspan 属性值就是那串字面量）。
//
// 所以这里在服务端把占位符替换成真实数字。数字与 webui.html 里 `COLS`
// 常量保持一致，由 internal/server 的测试守住（见 webui_cols_test.go）——
// 那是唯一能防住"两边漂移"的位置：JS 常量与 Go 侧的替换表必须相等。
var colPlaceholders = []struct{ token, value string }{
	{"__COLS_TRAVEL__", "8"},
	{"__COLS_GROWTH__", "6"},
}

// ⚠ 2026-10-19：`jobs` / `logs` 两条已从上面删掉。
//
//	任务调度页换成**卡片网格**、请求日志页换成 **grid 表格**（照抄参考项目
//	Agent2API 的 page-tasks.css / page-requests.css），两处都不再是 `<table>`，
//	没有列数可跨。webui.html 里的 `__COLS_JOBS__` / `__COLS_LOGS__` 占位符与
//	JS 的 `COLS` 常量**当时就同步删了**，只有这张 Go 表漏了 —— 于是
//	TestColPlaceholdersMatchJSCols 在 CI 上变红（本地没跑测试就推了）。
//
//	⇒ 加减「有表格空态行的页面」时**三处**都要动：HTML 占位符、JS 的 COLS、
//	  这张表。测试守得住后两者的漂移，但守住的前提是真的跑它。

// renderUI 把控制台外壳里剩下的占位符（**只有列数**）替换成运行时值。
//
// # ⚠ 2026-10-19 的关键删减：这里**不再注入任何密钥**
//
// 改造前这个函数的签名是
//
//	renderUI(page, apiKey, injectKey bool, adminToken, injectAdminToken bool)
//
// 它会把 config 里的 api_key 与 admin_token **明文写进页面 HTML**。
// 于是"打开控制台页面"这个动作本身就等于拿到两把钥匙 —— 而门禁那时也认
// api_key，两处叠加成用户报障 ③：**填错令牌也能进 UI，api_key 就泄了**。
//
// 现在浏览器不再持有任何密钥：身份是服务端下发的 HttpOnly 会话 cookie
// （`wb2api_sid`，JS 读不到、跨站带不上），页面里没有可偷的东西。
// "页面里出现密钥"这件事从"设计如此"变成"绝不允许"，由
// TestUIHTMLNeverContainsSecrets 与 verify_token_security_e2e.js 的 ① 段守住。
//
// 纯函数（不碰 w/r），便于测试直接断言输出。
func renderUI(page []byte) []byte {
	out := page
	for _, p := range colPlaceholders {
		out = bytes.ReplaceAll(out, []byte(p.token), []byte(p.value))
	}
	return out
}

// 登录页的两个标志位占位符（webui-login.html 里以**带双引号的字面量**出现）。
//
//	bootstrap  账号库为空（首次部署）⇒ 主 CTA 是「创建第一个管理员」
//	register   开放注册 ⇒ 显示注册入口（成员只能用自己名下的 API key）
const (
	loginBootstrapPlaceholder = "__WB2API_LOGIN_BOOTSTRAP__"
	loginRegisterPlaceholder  = "__WB2API_LOGIN_REGISTER__"
)

// renderLoginPage 把两个标志位替换成真实布尔值。
//
// 为什么用"带引号的字面量"做替换锚点：裸占位符在页面上还会出现在注释与
// 属性名里，ReplaceAll 会改坏它们（旧 renderUI 的注释里记着这个坑）。
func renderLoginPage(page []byte, bootstrap, registerOpen bool) []byte {
	out := injectBool(page, loginBootstrapPlaceholder, bootstrap)
	return injectBool(out, loginRegisterPlaceholder, registerOpen)
}

// injectBool 把 `"<TOKEN>"` 换成 JSON 布尔字面量（true / false）。
func injectBool(page []byte, token string, v bool) []byte {
	lit := []byte("true")
	if !v {
		lit = []byte("false")
	}
	return bytes.ReplaceAll(page, []byte(`"`+token+`"`), lit)
}

// adminTokenOK 报告出示的凭据是否等于**当前生效的管理令牌**。
//
// # 与 api_key 的关系：**不认识**
//
// 改造前这里认的是 api_key，于是"给客户端配一把对话 key"等于"交出管理台"。
// 现在只认 admin_token（用户明确要求：门禁 token 不能和 api 一样）。
//
// 未配 admin_token 时恒 false —— 保持"没有远程管理面"的安全默认，
// 而**不是**回落到 api_key。
//
// ⚠ 收凭据字符串而不是 *http.Request：取值统一在 account.TokenFromRequest，
// 本函数只做比较，于是可以直接当 account.Identify 的 isToken 回调。
func (h *Handler) adminTokenOK(cred string) bool {
	want := h.currentAdminToken()
	if want == "" {
		return false
	}
	// 定长比较：避免按字节提前返回泄漏"前几位对了"的时序信息。
	return subtle.ConstantTimeCompare([]byte(cred), []byte(want)) == 1
}

// uiIdentify 解析 `/ui` 请求的身份（与会话/令牌判据**同一份实现**）。
func (h *Handler) uiIdentify(r *http.Request) (account.Principal, bool) {
	return account.Identify(r, h.adminTokenOK, h.cfg.Accounts, h.cfg.Sessions)
}

// loginBootstrap 账号库是否为空（首次部署 ⇒ 登录页显示「创建第一个管理员」）。
func (h *Handler) loginBootstrap() bool {
	return h.cfg.Accounts != nil && h.cfg.Accounts.Count() == 0
}

// loginRegisterOpen 登录页要不要显示注册入口。
//
// 取不到判定（装配层没接线）时按**关闭**处理：宁可不显示入口，
// 也不要显示一个点了必然失败的按钮。
func (h *Handler) loginRegisterOpen() bool {
	if h.cfg.RegisterOpen == nil {
		return false
	}
	return h.cfg.RegisterOpen()
}

// ui 提供内嵌控制台。
//
// # 三条路（按顺序）
//
//	① `?token=<admin_token>`  → 校验通过则下发令牌 cookie 并 302 回 /ui
//	② 已认证（会话 cookie 或令牌 cookie）→ 控制台外壳
//	③ 未认证                  → **登录页**
//
// # ⚠ 本次重做：删掉「本机免令牌 + 明文注入密钥」（用户报障 ③）
//
// 用户报：*"当 token 没有授权的时候，那个网页……我发现 token 没有授权的时候，
// 它竟然能修改设置里面的内容"*，以及本轮的 *"填错 admin token 居然能进 UI，
// api_key 就泄了"*。
//
// 改造前的判据是 `isLoopback(r.RemoteAddr) && 进程只绑回环` 就免令牌，
// 并**把 config.api_key 明文注入页面**。两处叠加成一个完全敞开的部署：
//
//	① RemoteAddr 只反映**上一跳**。任何本机反代（nginx / caddy / frp /
//	   内网穿透）从 127.0.0.1 连过来时它就是回环 ⇒ 判定为"本机" ⇒ 放行。
//	② 放行之后页面里还被明文注入了 api_key，而那时门禁也认 api_key
//	   ⇒ 拿到页面 = 拿到管理权限。
//
// 现在回环地址**不是凭据**（绑回环只让"少暴露一个端口"这一条成立），
// 页面里**不含任何密钥**。
//
// # 未认证时返回 200（登录页），而不是 401
//
// 刻意的：`/ui` 是给**人**的入口，未登录的人需要一张能填账号口令的表单
// —— 返回 401 + JSON 会让浏览器显示一坨原始错误。真正的安全边界在**数据**
// 上（每一条 /admin/* 取数都会因为没有凭据而 401），不在这一张静态页上。
// 页面里也没有任何秘密，所以提前给它不构成泄露。
func (h *Handler) ui(w http.ResponseWriter, r *http.Request) {
	// ① `?token=<admin_token>`：换一次性 cookie 后 302。
	//
	// 为什么必须换：浏览器地址栏没法自定义请求头，令牌只能走查询参数；
	// 而把令牌留在 URL 里会顺着**历史记录 / Referer / 代理日志 / 截图**
	// 一路漏出去。换手之后 URL 是干净的 /ui。
	if tok := strings.TrimSpace(r.URL.Query().Get("token")); tok != "" {
		if h.adminTokenOK(tok) {
			// no-store 也要给 302：这条响应带 Set-Cookie，不该被中间层缓存复用
			// （作者本人换的是自己的令牌，缓存住就可能发给下一个人）。
			w.Header().Set("Cache-Control", "no-store")
			account.SetAdminTokenCookie(w, r, h.cfg.CookieSecure, tok)
			// 302 而不是 200：必须让浏览器地址栏换成干净 URL，
			// 否则"URL 里有令牌"这件事没被消除。
			http.Redirect(w, r, "/ui", http.StatusFound)
			return
		}
		// ⚠ 无效令牌**不**单独报错：那会把这个端点变成"令牌对不对"的判定
		// oracle（攻击者可以拿它离线试令牌）。与"压根没带令牌"走同一条路。
	}

	// ② 已认证 ⇒ 控制台外壳（**不注入任何密钥**）
	if _, ok := h.uiIdentify(r); ok {
		writeHTML(w, renderUI(webuiHTML))
		return
	}

	// ③ 未认证 ⇒ 登录页。顺手清掉失效的令牌 cookie（轮换后浏览器还揣着旧副本，
	// 每次请求白带一个不匹配的凭据）。
	if c, err := r.Cookie(account.AdminTokenCookie); err == nil && c != nil && c.Value != "" {
		account.ClearAdminTokenCookie(w, r, h.cfg.CookieSecure)
	}
	writeHTML(w, renderLoginPage(loginHTML, h.loginBootstrap(), h.loginRegisterOpen()))
}

// writeHTML 写一段内嵌页面。
//
// no-store：这些页面按身份分叉（登录页 / 控制台），缓存住会出现
// "登录成功后打开的还是登录页"或反过来"登出后仍能打开控制台外壳"。
func writeHTML(w http.ResponseWriter, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// favicon 站点图标：内嵌 SVG，不依赖任何外部 CDN，离线可用。
// 浏览器优先走 <link rel="icon" href="/favicon.svg">；这里同时兜住直接请求
// /favicon.ico 的场景（Safari、部分抓取器），302 到 SVG 而不是返回 404/204。
func (h *Handler) favicon(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, ".svg") {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconSVG)
		return
	}
	http.Redirect(w, r, "/favicon.svg", http.StatusFound)
}
