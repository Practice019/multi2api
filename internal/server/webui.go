// Package server — 内嵌本地控制台 /ui。
// 单文件 HTML，零外部依赖、零构建步骤；与网关同源，因此不需要 CORS 头。
package server

import (
	"bytes"
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
	{"__COLS_GROWTH__", "12"},
	{"__COLS_LOGS__", "10"},
	{"__COLS_HIST__", "7"},
	{"__COLS_JOBS__", "6"},
}

// renderUI 把页面里的占位符替换成运行时值。
//
// 纯函数（不碰 w/r），便于测试直接断言输出。
func renderUI(page []byte, apiKey string, injectKey bool) []byte {
	out := page
	if injectKey && apiKey != "" {
		// 只替换「带双引号的字面量」`"__WB2API_KEY__"` —— 也就是赋值语句的**值**。
		//
		// # 为什么不是 ReplaceAll（试过，是错的）
		//
		// 裸的 `__WB2API_KEY__` 在页面上还出现在两个**不该被替换**的位置：
		//   1. JS 属性名：`window.__WB2API_KEY__` —— 替换会直接把代码改坏
		//   2. 注释与文档字符串 —— 替换会污染说明文字
		// 所以必须限定成"带引号的那个字面量"，而不是"所有出现"。
		//
		// # 守卫为什么用独立哨兵
		//
		// 判断"是否已注入"需要一个对比基准。旧实现让它和赋值用**同一个**
		// 字面量 `'__WB2API_KEY__'`，于是正确性依赖于"那一处恰好没被替换"
		//（`Replace(..., 1)` 只改第一处）—— 一个看不出来的巧合。
		// 现在基准改为独立的 `__WB2API_KEY_SENTINEL__`，两处职责互不串扰：
		// 就算有人日后把这里改成 ReplaceAll，也只能改到属性名与注释，
		// 不会再出现"静默杀死自动连接"这种看不见的失效。
		//
		// 语义由 TestRenderUIKeyInjection 守住。
		// json.Marshal 负责转义：密钥含引号/反斜杠/换行时也不会破坏内联脚本。
		if quoted, err := json.Marshal(apiKey); err == nil {
			out = bytes.ReplaceAll(out, []byte(`"`+keyPlaceholder+`"`), quoted)
		}
	}
	for _, p := range colPlaceholders {
		out = bytes.ReplaceAll(out, []byte(p.token), []byte(p.value))
	}
	return out
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

// ui 提供内嵌控制台。
//
// 两条分支都是为了「密钥不出本机」：
//   - 本机访问：把网关自己的 api_key 注入页面内联脚本，省掉手输 Key 的往返；
//   - 非本机访问：仅在未启用鉴权时放行；启用鉴权时必须带 Bearer，否则 401。
//     否则任何能连到 7863 的局域网设备都能白拿一个内嵌了密钥的管理页面。
func (h *Handler) ui(w http.ResponseWriter, r *http.Request) {
	local := isLoopback(r.RemoteAddr)
	if !local && !h.validBearer(r) {
		http.Error(w, "401 unauthorized: /ui 仅本机直连；远程访问请带 Authorization: Bearer <api_key>", http.StatusUnauthorized)
		return
	}

	page := renderUI(webuiHTML, h.cfg.APIKey, local)

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
