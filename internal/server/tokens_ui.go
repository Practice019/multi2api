// tokens_ui.go 把令牌（API Key）管理页接进控制台：GET /ui/tokens（T68 / Wave C3）。
//
// # 为什么单独一个文件（照 channels_ui.go / groups_ui.go / status_ui.go 的同一套理由）
//
// webui.go 管控制台本体（登录引导、身份解析、外壳）；令牌页是**另一个页面**，
// 独立文件的边界更清楚：这一页有问题，删掉这一个文件即可。
//
// ⚠ 内嵌指令与变量（//go:embed webui-tokens.html 与 tokensUIPage）**刻意放在 webui.go**，
// 不在这里：前端语法门禁（tests/frontend/verify_inline_script_syntax.js:46-57）的扫描清单
// 是**从 webui.go 的 //go:embed 行现取**的 —— 放这里会让这一页**静默逃过门禁**
// （门禁全绿，但根本没扫过它）。channels_ui.go / groups_ui.go / status_ui.go 都记着
// 同一个坑（已踩过三次），这次刻意不踩，并在 webui.go 的 embed 处也留了同款注释。
package server

import (
	"net/http"
)

// tokensUI 提供令牌管理页。
//
// 未通过身份判据 ⇒ 302 回 /ui（那里有登录表单），而不是 401：
// 这是给人看的入口，人需要一张能填账号口令的表单，而不是一段 JSON。
// 这一页能看令牌掩码、能取明文、能改额度 —— 那是管理面，不是公开页。
func (h *Handler) tokensUI(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.uiIdentify(r); !ok {
		http.Redirect(w, r, "/ui", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-store：页面按身份分叉（未认证会被 302），缓存住会出现「登出后仍能打开令牌页」。
	// 另外这一页会按用户点击去取明文 key —— 缓存页面本身会把「刚才那一眼」留在磁盘上。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(tokensUIPage)
}
