package server

import (
	"strings"
	"testing"
)

// TestWebUIStartsWithDoctype 内嵌控制台必须以 <!doctype html> 开头。
//
// # 为什么需要这条
//
// 缺 doctype 会让浏览器进入 **quirks mode（怪异模式）**：
//
//	· box-sizing 默认值不同
//	· 行高/字号继承规则不同
//	· 表格列宽算法退化成旧算法（table-layout:auto 分配完全不同）
//	· height:100% 链断裂
//
// 表现是"样式莫名错位"，而现有的所有结构测试**一条都抓不到** ——
// 它们断言的都是"某个片段存在"，没人看文件头。
//
// 这个缺口真实发生过一次（T-373）：从 exe 抽基线重建 webui.html 时丢了首行，
// 于是 /ui 进 quirks mode，表格列宽/间距看起来全不对，却没有任何测试变红。
//
// ⇒ 判据是**两条**：首行（trim 左空白后）必须是 `<!doctype html>`（小写，与 git HEAD 逐字一致），
// 且 `<html` 必须出现在它**之后**（防出现"doctype 被挪到 </html> 之后"这种更怪的状态）。
func TestWebUIStartsWithDoctype(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	before := html
	html = strings.TrimLeft(html, " \t\r\n\ufeff")
	if before != html {
		t.Logf("（文件首部被 trim 了 %d 个空白/BOM 字符）", len(before)-len(html))
	}
	if !strings.HasPrefix(html, "<!doctype html>") {
		head := html
		if len(head) > 48 {
			head = head[:48]
		}
		t.Errorf("webui.html 必须以 `<!doctype html>` 开头（**小写**，与 git HEAD 逐字一致）。\n"+
			"缺它浏览器进 quirks mode：表格列宽 / box-sizing / height:100%% 全退化成旧规范 ——\n"+
			"表现是「样式莫名错位」，而其它结构测试一条都抓不到（T-373 真丢过一次）。\n"+
			"当前前 48 字符：%q", head)
	}
	di := strings.Index(html, "<!doctype html>")
	hi := strings.Index(html, "<html")
	if di < 0 {
		t.Fatal("找不到 doctype（判据失效）")
	}
	if hi < 0 {
		t.Fatal("找不到 <html")
	}
	if hi < di {
		t.Errorf("`<html` 出现在 doctype 之前（位置 doctype=%d html=%d）—— 文件头结构不对", di, hi)
	}
	if strings.Count(html, "<!doctype html>") != 1 {
		t.Errorf("`<!doctype html>` 出现 %d 次，应为 1", strings.Count(html, "<!doctype html>"))
	}
}
