// Package server — 内嵌本地控制台 /ui。
// 单文件 HTML，零外部依赖、零构建步骤；与网关同源，因此不需要 CORS 头。
package server

import (
	"bytes"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

//go:embed webui.html
var webuiHTML []byte

//go:embed favicon.svg
var faviconSVG []byte

// keyPlaceholder 是 webui.html 里等待被注入的哨兵（JS 侧以同名常量比对）。
// 未被替换时说明网关未配置 api_key，页面按「无需鉴权」处理。
const keyPlaceholder = "__WB2API_KEY__"

// adminTokenPlaceholder 管理台门禁令牌的哨兵（与 api_key **分开**）。
//
// # 为什么需要第二个哨兵（用户要求：门禁 token 与 api 必须分开）
//
// 页面同时要打两类端点：
//
//	/admin/*  → 要 admin_token
//	/v1/*     → 要 api_key
//
// 而改造前只有一把钥匙、一个哨兵。现在两者独立，页面必须能分别携带 ——
// 否则会出现两种坏结果之一：管理页拿着 api_key 打 /admin/*（403），
// 或对话测试拿着 admin_token 打 /v1/*（401）。
//
// 两个哨兵各自可空：某个部署可能只配了其中一把（例如只调对话接口、
// 不需要管理台）。空的那半页面按「该面无需鉴权」处理。
const adminTokenPlaceholder = "__WB2API_ADMIN_TOKEN__"

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
	{"__COLS_LOGS__", "10"},
	{"__COLS_JOBS__", "6"},
}

// renderUI 把页面里的占位符替换成运行时值。
//
// # 两个凭据分开注入（用户要求：门禁 token 与 api 必须分开）
//
//	apiKey      → /v1/* 的凭据（对话、生图、模型目录）
//	adminToken  → /admin/* 的凭据（管理台）
//
// injectKey / injectAdminToken 分别控制"要不要把这把钥匙交给页面"。
// 之所以是**两个**开关而不是一个：调用方（ui handler）对两者的放行条件
// 不同 —— 见 ui 的注释（管理令牌只在"这次请求已经通过了门禁"时才注入）。
//
// 纯函数（不碰 w/r），便于测试直接断言输出。
func renderUI(page []byte, apiKey string, injectKey bool, adminToken string, injectAdminToken bool) []byte {
	out := page
	if injectKey && apiKey != "" {
		out = injectPlaceholder(out, keyPlaceholder, apiKey)
	}
	if injectAdminToken && adminToken != "" {
		out = injectPlaceholder(out, adminTokenPlaceholder, adminToken)
	}
	for _, p := range colPlaceholders {
		out = bytes.ReplaceAll(out, []byte(p.token), []byte(p.value))
	}
	return out
}

// injectPlaceholder 把页面里「带双引号的字面量」`"<token>"` 替换成 JSON 编码后的值。
//
// # 为什么不是 ReplaceAll（试过，是错的）
//
// 裸的哨兵在页面上还出现在两个**不该被替换**的位置：
//  1. JS 属性名（`window.__WB2API_KEY__`）—— 替换会直接把代码改坏
//  2. 注释与文档字符串 —— 替换会污染说明文字
//
// 所以必须限定成"带引号的那个字面量"，而不是"所有出现"。
//
// # 守卫为什么用独立哨兵
//
// 判断"是否已注入"需要一个对比基准。旧实现让它和赋值用**同一个**字面量，
// 于是正确性依赖于"那一处恰好没被替换"（`Replace(..., 1)` 只改第一处）
// —— 一个看不出来的巧合。现在基准改为独立的 `*_SENTINEL__` 字面量，
// 两处职责互不串扰：就算有人日后把这里改成 ReplaceAll，也只能改到
// 属性名与注释，不会再出现"静默杀死自动连接"这种看不见的失效。
//
// 语义由 TestRenderUIKeyInjection 守住。
// json.Marshal 负责转义：密钥含引号/反斜杠/换行时也不会破坏内联脚本。
func injectPlaceholder(page []byte, token, value string) []byte {
	quoted, err := json.Marshal(value)
	if err != nil {
		return page
	}
	return bytes.ReplaceAll(page, []byte(`"`+token+`"`), quoted)
}

// isLoopback 判断请求是否来自本机（127.0.0.0/8 或 ::1）。
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localExempt 是否享受「本机免令牌」。
//
// # ⚠ 与 admin 包的 localExempt 是**同一条判据**（必须保持一致）
//
// 两处都得判，因为 /ui 在 server 包、/admin/* 在 admin 包，而两者都挂
// 在同一个端口上。判据若不一致，就会出现"页面能打开但每个标签页 403"
// 或反过来"页面打不开但接口敞开" —— 两种都很难查。
//
// 判据（三条必须**同时**成立）：
//
//	① 进程**只**监听回环地址（listensLoopback）
//	   绑 0.0.0.0 时"本机"没有意义：端口是公开的。
//	② 上一跳（RemoteAddr）是回环
//	③ 请求**未经反向代理转发**（无 X-Forwarded-* / X-Real-IP 头）
//
// 只判 ② 就是改造前的漏洞：nginx 从 127.0.0.1 连过来时 ② 恒成立。
//
// ⚠ 为什么不信任 X-Forwarded-For 判"真实客户端是不是本机"：那些头
// **客户端可以随便伪造**，用它做放行判据等于把门禁交给攻击者。所以这里
// 只用它做**收紧**（出现即视为经代理、要求令牌），绝不用它做放宽。
func localExempt(r *http.Request, listensLoopback bool) bool {
	if !listensLoopback {
		return false
	}
	if !isLoopback(r.RemoteAddr) {
		return false
	}
	return !forwardedRequest(r)
}

// forwardedRequest 报告请求是否带有反向代理的转发痕迹。
func forwardedRequest(r *http.Request) bool {
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		if strings.TrimSpace(r.Header.Get(name)) != "" {
			return true
		}
	}
	return false
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
func (h *Handler) adminTokenOK(cred string) bool {
	want := h.currentAdminToken()
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cred), []byte(want)) == 1
}

// ui 提供内嵌控制台。
//
// # 凭据获取（远程访问）
//
//	`Authorization: Bearer <admin_token>` 头（脚本/扩展可携带）
//	`?token=<admin_token>` 查询参数（浏览器地址栏直接打开）
//
// # ⚠ 本次重做：把「本机」这个放宽收紧到真正安全的条件（用户报障）
//
// 用户报：*"当 token 没有授权的时候，那个网页……我发现 token 没有授权的时候，
// 它竟然能修改设置里面的内容"*。
//
// 改造前的判据是 `isLoopback(r.RemoteAddr)` 就免令牌并**注入 config.api_key**。
// 两处叠加成一个完全敞开的部署：
//
//	① RemoteAddr 只反映**上一跳**。任何本机反代（nginx / caddy / frp /
//	   内网穿透）从 127.0.0.1 连过来时它就是回环 ⇒ 判定为"本机" ⇒ 放行。
//	   于是"我只在局域网/公网暴露这个域名"实际变成了"管理台对全网敞开"。
//	② 放行之后页面里还被**明文注入**了 api_key（旧实现），而那时门禁
//	   也认 api_key ⇒ 拿到页面 = 拿到管理权限。
//
// # 页面本身**总是**返回（不给 401）
//
// 这是刻意的：静态 HTML 里没有任何秘密，而"未授权时该看到什么"正是
// 用户提过的需求 —— *"前端如果说他没有这个、需要填写那个东西，你需要
// 提醒他去哪里查看"*。所以未授权访问者拿到的是**登录框 + 指路提示**，
// 而每一条取数请求都会因为没有凭据而失败。安全边界在**数据**上，不在 HTML 上。
//
// # 注入哪把钥匙（两个凭据分开）
//
// 页面同时要打两类端点，所以分别注入：
//
//	admin_token → /admin/*（管理台）
//	api_key     → /v1/*（对话测试、模型目录）
//
// 只有**已授权**的请求才注入（本机免令牌，或出示了正确的管理令牌）；
// 未授权时两个哨兵都保持占位符，前端据此显示登录框。
func (h *Handler) ui(w http.ResponseWriter, r *http.Request) {
	// 「本机」= 真正安全的那个组合，不含反代转发。判据与 admin 的门禁同源。
	local := localExempt(r, h.cfg.ListensLoopback)

	// 远程凭据：只认**管理令牌**。api_key 一律不解锁（那是本次修的权限倒挂）。
	adminTok := bearerOf(r)
	if adminTok == "" {
		adminTok = r.URL.Query().Get("token")
	}
	remoteOK := h.adminTokenOK(adminTok)
	authorized := local || remoteOK

	adminToken, injectAdmin := "", false
	apiKey, injectAPIKey := "", false
	if authorized {
		// 管理令牌：本机用服务端当前生效的那把；远程用访问者出示的那把。
		if local {
			adminToken = h.currentAdminToken()
		} else {
			adminToken = adminTok
		}
		injectAdmin = adminToken != ""

		// 对话凭据：**也**给远程管理员。
		//
		// 这里刻意不做"本机才给"的收紧，理由是一个权限比较：
		// 能进管理台的人**已经**可以新建一把 API key（POST /admin/apikeys），
		// 也**已经**能改配额与账号池 —— 把现有的 api_key 给他，不增加任何
		// 他本来做不到的事。而收紧的代价是远程管理台上"模型目录"永远空着
		// （那一页要 /v1/models），变成一个看不懂的残缺界面。
		apiKey = h.cfg.APIKey
		injectAPIKey = apiKey != ""
	}

	page := renderUI(webuiHTML, apiKey, injectAPIKey, adminToken, injectAdmin)

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
