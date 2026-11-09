package server

import (
	"os"
	"path/filepath"
	"testing"
)

// readFrontendSource 返回**前端全部源码**：`webui.html` + `ui/app.js`（T-409 拆包后 JS 已外置）。
//
// # 为什么需要它
//
// 拆包（T-409）之前，主 script 是**内联在** `webui.html` 里的，所以所有前端守卫都直接
// `os.ReadFile("webui.html")` 就能扫到 JS。拆包后 JS 搬到了 `internal/server/ui/app.js`
// ⇒ 只读 html 的守卫会**误报"结构被删掉了"**（实测一次红 85 条：`const ACCT_COLUMN_DEFS = {`、
// `free_quota: {`、`renderLogs(` 等等全在 app.js 里）。
//
// 把两者拼起来返回 ⇒ 扫描面与拆包前**逐字一致**（JS 原来就在 html 里，位置不同而已）。
// 这也是最诚实的一条判据：守卫问的是"**前端源码**里有没有这段结构"，不是"某个文件名里有没有"。
//
// ⚠ 顺序：html 在前、js 在后 —— 少数守卫看首行（`TestWebUIStartsWithDoctype`），
//
//	拼在后面的 JS 不会影响它们。
//
// ⚠ 只想读 HTML 本体的守卫（例如 ui_assets_test.go 断言页面里不再有 <style> 块）
//
//	应当直接 `os.ReadFile("webui.html")`，不要用这个函数。
func readFrontendSource(t *testing.T) ([]byte, error) {
	t.Helper()
	html, err := os.ReadFile("webui.html")
	if err != nil {
		return nil, err
	}
	js, err := os.ReadFile("ui/app.js")
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(html)+len(js)+1)
	out = append(out, html...)
	out = append(out, '\n')
	out = append(out, js...)
	return out, nil
}

// frontendSourceString 返回前端全部源码的字符串形式（webui.html + ui/app.js）。
//
// # 为什么要它（T-411）
//
// 拆包前 `string(webuiHTML)`（内嵌变量）能扫到 JS，因为 JS 内联在 html 里。
// 拆包后 JS 到了 ui/app.js ⇒ 直接 `string(webuiHTML)` 的 14 处守卫全部误报。
//
// ⚠ 与 readWebUIHTML() 的区别：那个是**读源文件**（占位符未替换、适合解析/求值），
// 这个是**读内嵌变量**（占位符原样、适合"页面里到底有没有这段"）。
// 两者在拆包后**都必须包含 app.js**，否则守卫失效。
// ⚠ 与 readFrontendSource(t) 的区别：那个要 *testing.T、失败即 t.Fatal；
// 这个适合在 `if !strings.Contains(...)` 这种**不会失败**的场景直接用。
func frontendSourceString() string {
	return string(webuiHTML) + "\n" + string(appJS)
}

// readFrontendCSS 读外置样式表（T-409 拆包后 CSS 已从 webui.html 搬到 ui/app.css）。
//
// # 为什么要单开一个读口
//
// CSS 守卫问的是"样式表里有没有这条规则"。若混进 html+js 的拼接串，
// "JS 里恰好有个同名字符串"也会算过 —— 那是假绿。
func readFrontendCSS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("ui", "app.css"))
	if err != nil {
		t.Fatalf("读 ui/app.css 失败: %v", err)
	}
	return string(b)
}
