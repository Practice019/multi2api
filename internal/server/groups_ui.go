// groups_ui.go 把分组管理页接进控制台：GET /ui/groups。
//
// # 为什么单独一个文件（照 channels_ui.go 的同一套理由）
//
// webui.go 管的是控制台本体（登录引导、身份解析、外壳）。分组页是**另一个页面**，
// 独立文件的边界更清楚：分组页有任何问题，删掉这一个文件即可。
//
// ⚠ 权限判据刻意与 /ui、/ui/channels **同一份实现**（h.uiIdentify）：
// 分组页会显示「哪些 key 用了哪个分组」—— 那是管理面，不是公开页。
// 另写一套判断等于给自己留一条「两处判据哪天分叉了」的路。
//
// ⚠ 内嵌指令与变量**刻意放在 webui.go**（groupsUIPage），不在这里：
// 前端语法门禁（tests/frontend/verify_inline_script_syntax.js）的扫描清单
// 是从 webui.go 的 //go:embed 行推导出来的。放这里会让这一页**静默逃过门禁**
// —— 门禁全绿，但根本没扫它。channels_ui.go 的注释里记着同一个坑。
package server

import (
	"net/http"
)

// groupsUI 提供分组管理页。
//
// 未通过身份判据 ⇒ 302 回 /ui（那里有登录表单），而不是回 401：
// 这是给人看的入口，人需要一张能填账号口令的表单，而不是一段 JSON。
func (h *Handler) groupsUI(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.uiIdentify(r); !ok {
		http.Redirect(w, r, "/ui", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 页面里含分组的读取与写入逻辑（改分组会影响哪些 key 能用什么模型）
	// ⇒ 不让中间层缓存。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(groupsUIPage)
}
