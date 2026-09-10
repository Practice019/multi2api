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

	page := webuiHTML
	if local && h.cfg.APIKey != "" {
		// json.Marshal 负责转义：密钥含引号/反斜杠/换行时也不会破坏内联脚本。
		if quoted, err := json.Marshal(h.cfg.APIKey); err == nil {
			page = bytes.Replace(page, []byte(`"`+keyPlaceholder+`"`), quoted, 1)
		}
	}

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
