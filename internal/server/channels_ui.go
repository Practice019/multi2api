// channels_ui.go 把渠道管理页接进控制台：GET /ui/channels。
//
// # 为什么单独一个文件、而不是往 webui.go 里加
//
// webui.go 管的是"控制台本体"（登录引导、身份解析、外壳）。渠道页是**另一个页面**，
// 它的存在不该让 `webui.go` 每次都多读一段。独立文件的边界也更清楚：
// 渠道页有任何问题，删掉这一个文件即可，控制台本体一行都不用动。
//
// ⚠ 权限判据刻意与 `/ui` **同一份实现**（h.uiIdentify）：渠道页能看见渠道的 key 掩码、
// Base URL、映射关系 —— 它是管理面，不是公开页。另写一套判断等于给自己留一条
// "两处判据哪天分叉了"的路。
// ⚠ 内嵌指令与变量**刻意放在 webui.go**（与 webui.html / webui-login.html / favicon.svg 同处）：
// 前端语法门禁（tests/frontend/verify_inline_script_syntax.js）的扫描清单就是从 webui.go 的
// //go:embed 行推导出来的。放在这里的话，这一页会**静默逃过门禁** —— 门禁是全绿的，
// 但根本没扫它。我上一轮正是这么写的，所以这轮把它挪过去了。
package server

import (
	"net/http"
)

// channelsUI 提供渠道管理页。
//
// 未通过身份判据 ⇒ 302 回 /ui（那里有登录表单），而不是回 401：
// 这是给人看的入口，人需要一张能填账号口令的表单，而不是一段 JSON。
func (h *Handler) channelsUI(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.uiIdentify(r); !ok {
		http.Redirect(w, r, "/ui", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 页面里含渠道配置的读取逻辑（不含明文 key，取明文要走显式 /reveal）⇒ 不让中间层缓存。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(channelsUIPage)
}
