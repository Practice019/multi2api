package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestThemeSyncTargetsAppCSS theme_sync 的目标列表必须跟着拆包走（T-421/T-423）。
//
// # 守的是什么
//
// 拆包（T-409）把 webui.html 的 `<style>` 块整体外置成了 ui/app.css，
// 而 theme_sync.js 的目标列表没跟着改 ⇒ 两个后果（T-421 实测）：
//
//	· webui.html 里已没有 <style> ⇒ 脚本在**第一个目标**就抛
//	  `Error: 页面里没有 </style>` ⇒ **后面 6 个页面全都同步不到**；
//	· ui/app.css 里的那份 CSS **永远不同步** ⇒ 改了 theme-max.css 也不生效。
//
// 这条是本轮的**防回归**（T-421 已修好 ⇒ 现在就是绿的）。
func TestThemeSyncTargetsAppCSS(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "tools", "theme_sync.js"))
	if err != nil {
		t.Fatalf("读 tools/theme_sync.js 失败：%v", err)
	}
	src := string(b)
	if !strings.Contains(src, "'internal/server/ui/app.css'") {
		t.Error("theme_sync.js 的 PAGES 里没有 'internal/server/ui/app.css' —— " +
			"拆包后 CSS 外置成了它，不把它当目标 ⇒ 改了 theme-max.css 也不会落到 app.css")
	}
	// ⚠ 只检查"页面清单里没有 webui.html"：注释与说明里可以（也应该）提到它
	//   （那里记着为什么把它删掉），所以按"行"判，不按整串判。
	for _, ln := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "'internal/server/webui.html'," {
			t.Error("theme_sync.js 的 PAGES 里仍有 'internal/server/webui.html' —— " +
				"它已经**没有 <style>** 了，留着它脚本会在第一个目标就抛异常，" +
				"导致后面 6 个页面全都同步不到")
		}
	}
}

// TestRangeNoteIsAIconNotInlineText 时间范围口径提示改成 ⓘ 图标（T-422/T-423）。
//
// # 用户报障
//
//	「还有就这这个小字一直变动占位置太多了」
//
// 改造前它是 h3 里的**内联 11px 文字**，长度在档位间从 0 字跳到 27 字
// ⇒ 整行布局跟着变（这就是"一直变动"）。
// 现在：14px 圆圈 + title 悬浮说明；没说明的档位 `hidden`（不占任何宽度）。
//
// # 为什么"不占宽度"必须被守卫钉住
//
// 只要它还有可见宽度，标题行就会随档位变宽变窄 —— 那正是用户报的那个毛病。
// ⇒ 判据两层：① 它是图标类（不是内联文字）② 没说明时 hidden。
func TestRangeNoteIsAIconNotInlineText(t *testing.T) {
	html, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	page := string(html)
	// ① 不再是内联 11px 文字
	if strings.Contains(page, `id="statsRangeNote" style="font-weight:400;font-size:11px;margin-left:8px"`) {
		t.Error("webui.html 里 #statsRangeNote 仍是**内联 11px 文字** —— " +
			"文字长度在档位间从 0 跳到 27 字，标题行会跟着跳（用户报「一直变动占位置太多了」）")
	}
	if !strings.Contains(page, `class="range-note-icon"`) {
		t.Error("webui.html 里 #statsRangeNote 没有用 .range-note-icon 类（应改成圆圈图标）")
	}
	if !strings.Contains(page, `id="statsRangeNote" class="range-note-icon" hidden`) {
		t.Error("webui.html 里 #statsRangeNote 没有初始 hidden —— " +
			"初始可见会让标题行先宽一下再收（正是「跳动」的来源）")
	}
	// ② 样式规则存在（改在 theme-max.css 这个**源**里，再 sync 到 app.css）
	tm, err := os.ReadFile("theme-max.css")
	if err != nil {
		t.Fatalf("读 theme-max.css 失败：%v", err)
	}
	if !strings.Contains(string(tm), ".range-note-icon") {
		t.Error("theme-max.css 里没有 .range-note-icon 规则（圆圈图标样式）—— " +
			"它是**新规则**，新规则必须写在源文件里再 sync，不能只手改 app.css")
	}
	// ③ app.css 里也必须真的有（说明 sync 跑过）
	ac, err := os.ReadFile(filepath.Join("ui", "app.css"))
	if err != nil {
		t.Fatalf("读 ui/app.css 失败：%v", err)
	}
	if !strings.Contains(string(ac), ".range-note-icon") {
		t.Error("ui/app.css 里没有 .range-note-icon —— 改了 theme-max.css 但**没跑 sync**，" +
			"浏览器拿不到这条规则（跑：node tools/theme_sync.js）")
	}
	// ④ setStatsRange 用 title + hidden 驱动它（不是写文本）
	jsB, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 ui/app.js 失败：%v", err)
	}
	js := string(jsB)
	i := strings.Index(js, "function setStatsRange(")
	if i < 0 {
		t.Fatal("找不到 setStatsRange")
	}
	body := js[i:]
	if j := strings.Index(body, "\n  function "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "note.title") || !strings.Contains(body, "note.hidden") {
		t.Error("setStatsRange 仍用 textContent 写文字 —— 应改成 note.title + note.hidden " +
			"（有说明才显示图标；没说明 hidden，不占宽度）")
	}
	if !strings.Contains(body, "aria-label") {
		t.Error("setStatsRange 没有设 aria-label —— 读屏软件只会读到一个字母「i」")
	}
}

// TestHeatStatsTableHasColumnGap 热力图右侧明细表的列间距（T-422）。
//
// # 用户报障
//
//	「还有这个字显示不全」（截图：表头 请求/命中 挤在一起）
//
// 实测：各列 `scrollWidth == 宽度`（**没有** ellipsis 截断），
// 真因是 `td{padding:0}` ⇒ 相邻列文字视觉相连。
//
// ⚠ 这些规则在 **app.css 的块外区**（页面自有样式，不是生成物 —— 见文件头注释）。
//
//	本条是防回归（T-422 已改好）。
func TestHeatStatsTableHasColumnGap(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("ui", "app.css"))
	if err != nil {
		t.Fatalf("读 ui/app.css 失败：%v", err)
	}
	css := string(b)
	if !strings.Contains(css, "padding:0 0 0 6px") {
		t.Error("app.css 里 `.heatstats .hdet td` 没有左内距（应为 padding:0 0 0 6px）—— " +
			"10px 的字会黏成一团（用户报「字显示不全」）")
	}
	if !strings.Contains(css, "padding:0 0 3px 6px") {
		t.Error("app.css 里 `.heatstats .hdet th` 没有左内距（应为 padding:0 0 3px 6px）")
	}
	if !strings.Contains(css, "flex:1 1 760px") {
		t.Error("app.css 里 `.heatmain` 的 flex-basis 不是 760px —— " +
			"795px 时分给右侧明细表的宽度不够（实测只有 263px，而 7 列需要 ≈290px）")
	}
	if !strings.Contains(css, "min-width:290px") {
		t.Error("app.css 里 `.heatstats` 没有 min-width:290px —— 没有它 flex 会把右侧压回 263px")
	}
	// 段首注释：块外区是手写区，必须写明（防后来者以为"手改生成物"而不敢动 / 或改错到块内）
	if !strings.HasPrefix(css, "/*") || !strings.Contains(css[:1200], "页面自有样式") {
		t.Error("app.css 开头没有「页面自有样式」段首注释 —— " +
			"块外区是**手写区**（无 sync 管辖），不写明会让后来者困惑" +
			"（甚至误以为它是生成物而不敢改、或把手写内容当生成物覆盖）")
	}
}
