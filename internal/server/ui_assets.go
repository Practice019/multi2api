// ui_assets.go 控制台的**外置静态资源**（T-409：从 webui.html 里搬出来的 CSS 与主 script）。
//
// # 为什么 embed 声明不放 webui.go
//
// `webui.go` 的 `//go:embed` 清单被 `tests/frontend/verify_inline_script_syntax.js:46-57`
// **现取**来枚举"要逐个 node --check 的内联脚本"（该文件 L34-36 有明确注释）。
// 把 `ui/app.js` 混进那份清单会让扫描器把**外置资源**当成**页面内联脚本**处理 —— 名不副实，
// 且以后每加一个资源都要动那个清单。所以单开这个文件，清单一个字不动。
//
// # 为什么这两个资源不需要控制台身份
//
// 与 `favicon.svg` 同一条理由：它们是**静态资源**，不含任何秘密。
// ⚠ 关键前提已实测：主 script 内**没有** `__WB2API_*__` 占位符
// （占位符只在 webui.html 顶部那两个必须同步执行的小脚本里）⇒ 不经过 `renderUI` 替换也不会漏注入。
// 这条由 `ui_assets_test.go` 守住（它同时断言页面里不再有 `<style>` 块、只有一个 app.js 引用）。
//
// # 缓存策略（第 1 步只做简单版）
//
// ETag + `max-age=300, must-revalidate`。**没做**内容哈希文件名（那是第 3 步）——
// 现在改一次 CSS/JS 必须等 5 分钟或强刷才生效，属于已知取舍。
package server

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
)

//go:embed ui/app.css
var appCSS []byte

//go:embed ui/app.js
var appJS []byte

// uiAsset 把一个内嵌资源作为静态文件提供（ETag / 304 / 短缓存）。
//
// ETag 取 sha256 前 8 字节 = 16 位十六进制 —— 够短、且同一份内容稳定（304 生效的前提）。
func (h *Handler) uiAsset(content []byte, contentType string) http.HandlerFunc {
	sum := sha256.Sum256(content)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=300, must-revalidate")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if len(content) == 0 {
			http.Error(w, "asset is empty", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(content)
	}
}
