// models_refresh_ui_test.go —— 「目录刷新」这条路径的**前端**判据。
//
// # 为什么要写到这一层
//
// 后端把缓存清干净了，用户仍可能看不到新目录 —— 前端有三处会把旧目录画回来：
//
//	① 外壳的 localStorage 缓存（lazyCache 落盘，见 app.js 的 sharedGet 注释）；
//	② iframe 页面凭旧文档重绘；
//	③ 刷新动作**没等**后端清缓存就先重载页面（竞态：页面取数可能早于清缓存，
//	   用户看到旧目录，而页面照样打上一个新的「更新于」时刻）。
//
// 这三条都不是「文件里有没有那段字」能证明的，所以这里的判据是：
//
//	· refreshModelCatalog 的**函数体**里，三件事的顺序必须是
//	  await POST /admin/models/refresh → drop 两张缓存 → 重载 iframe；
//	· 模型目录那一节的刷新按钮**确实绑在** refreshModelCatalog 上
//	  （本仓有「悬空控件引用」的历史教训：按钮在、函数不在，点了报错）；
//	· 页面上能看出「这份目录是什么时候取的」（catalog.age_seconds/stale）；
//	· 执行式兜底：node --check 必须过（语法错是这一层最便宜能抓到的故障）。
package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// refreshModelCatalogBody 取出 app.js 里 window.refreshModelCatalog 的函数体。
//
// ⚠ 找不到就 Fatal 而不是 Skip：这条守卫的全部价值就是「这个入口还在且顺序对」，
//
//	静默跳过等于守卫消失（本仓的 fail-open 纪律）。
func refreshModelCatalogBody(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 ui/app.js 失败: %v", err)
	}
	js := string(src)
	const marker = "window.refreshModelCatalog = "
	i := strings.Index(js, marker)
	if i < 0 {
		t.Fatal("app.js 里找不到 `window.refreshModelCatalog = ` —— 模型目录面板的刷新入口被删/改名了" +
			"（改结构可以，但必须同步改这条守卫，别让它 fail-open）")
	}
	rest := js[i+len(marker):]
	j := strings.Index(rest, "\n};")
	if j < 0 {
		t.Fatal("找不到 refreshModelCatalog 的收尾 `};` —— 函数结构变了，守卫取不到边界")
	}
	body := rest[:j]
	if !strings.Contains(body, "/admin/models/refresh") {
		t.Fatalf("切出来的函数体不含 /admin/models/refresh —— 切错位置了（fail-open）:\n%s", body)
	}
	return body
}

// TestRefreshModelCatalogAwaitsBackendThenBustsCaches 钉住三件事的顺序。
//
// 顺序不是风格问题：POST 不 await 就重载 iframe 时，页面的取数请求可能**先于**
// 缓存清空到达后端 ⇒ 用户看到旧目录。
func TestRefreshModelCatalogAwaitsBackendThenBustsCaches(t *testing.T) {
	body := refreshModelCatalogBody(t)

	iPost := strings.Index(body, "/admin/models/refresh")
	iDropModels := strings.Index(body, "lazyCache.drop('ep:/v1/models')")
	iDropMult := strings.Index(body, "lazyCache.drop('ep:/admin/models/preview')")
	iFrame := strings.Index(body, "mdFrame")

	if !strings.Contains(body, "await") {
		t.Errorf("★ 刷新路径里没有 await —— 后端清缓存是**异步**动作，不等它就先重载 iframe"+
			"就是竞态（用户看到旧目录，本仓在别的面板踩过同款）:\n%s", body)
	}
	if iDropModels < 0 {
		t.Errorf("★ 没清外壳的 /v1/models 缓存（lazyCache 落 localStorage）—— 重载后 iframe 仍会拿到旧目录")
	}
	if iDropMult < 0 {
		t.Errorf("★ 没清倍率目录缓存 ep:/admin/models/preview —— 会出现「模型列表变了、倍率还是旧的」半刷新")
	}
	if iFrame < 0 {
		t.Errorf("★ 刷新路径没有重载 iframe（mdFrame）—— 页面内容不会重绘")
	}
	if iPost < 0 || iPost > iDropModels || iDropModels > iFrame || iDropMult > iFrame {
		t.Errorf("★ 顺序应为「await 后端清缓存 → 丢本地缓存 → 重载 iframe」，实际下标 %+v",
			map[string]int{"post": iPost, "dropModels": iDropModels, "dropMult": iDropMult, "frame": iFrame})
	}
	if !strings.Contains(body, "?t=") {
		t.Errorf("★ 重载 iframe 没带缓存破坏串（?t=）—— 浏览器可能直接复用旧文档")
	}
	if !strings.Contains(body, "throttled") {
		t.Errorf("★ 刷新路径没处理后端的 throttled 应答 —— 被限速时界面会说「刷新成功」而实际什么都没做")
	}
}

// TestModelCatalogRefreshButtonIsBound 钉住「按钮存在且绑对」。
//
// ⚠ 判据是**绑定关系**，不是「文件里出现过这个词」：本仓出过「控件在、函数不在」
//
//	与「控件绑到别的加载器」两种事故（用户看到的都是「点了没反应」）。
func TestModelCatalogRefreshButtonIsBound(t *testing.T) {
	src, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	html := string(src)
	// ⚠ 必须匹配**真实的标签**，不能只找 `data-key="modelcatalog"`：
	//   webui.html 上方注释里也出现过这个键名（解释「为什么 key 不能改回 models」），
	//   只找键名会切到注释里、把守卫变成一个永远对着注释比对的装饰品。
	// ⚠ 锚点必须**带行首缩进**：webui.html 的说明注释里逐字出现过
	//   「<section data-key="modelcatalog">」（解释 key 为什么不能改回 models），
	//   只按子串找会切到注释里去，守卫就变成对着注释比对的装饰品。
	const anchor = "\n  <section data-key=" + "\"modelcatalog\""
	i := strings.Index(html, anchor)
	if i < 0 {
		t.Fatal("webui.html 里找不到模型目录那一节的 modelcatalog 标签（<section data-key=...>）")
	}
	end := strings.Index(html[i:], "</section>")
	if end < 0 {
		t.Fatal("模型目录那一节没有收尾 </section>")
	}
	sec := html[i : i+end]
	if !strings.Contains(sec, "refreshModelCatalog(") {
		t.Errorf("★ 模型目录那一节的刷新按钮没绑到 refreshModelCatalog（绑定漂了 / 按钮被删了）:\n%s", sec)
	}
	if !strings.Contains(sec, "mdFrame") {
		t.Errorf("★ 模型目录那一节没有 iframe（mdFrame）—— 页面上没有任何可刷新的内容")
	}
}

// TestModelsPageShowsCatalogFetchAge 钉住 item ③：页面上能看出「这份目录什么时候取的」。
//
// 字段必须**来自后端**（catalog.age_seconds / catalog.stale）：前端自己按
// 「页面这次请求的时刻」推一个时间，正是用户报障里「感觉不实时」的来源 ——
// 缓存 1h 的旧目录会被显示成「刚刚更新」。
func TestModelsPageShowsCatalogFetchAge(t *testing.T) {
	src, err := os.ReadFile("webui-models.html")
	if err != nil {
		t.Fatalf("读 webui-models.html 失败: %v", err)
	}
	page := string(src)
	for _, needle := range []string{".catalog", "age_seconds", "stale", "分钟前"} {
		if !strings.Contains(page, needle) {
			t.Errorf("★ 模型目录页没有用到 %q —— 无法显示目录的取数时刻/是否过期", needle)
		}
	}
}

// ── 执行式判据：页脚那句「目录是什么时候取的」真的能跑出正确的字 ──────────

// modelsPageAgeSnippet 抠出 fmtAgo + catalogUpdatedText 两段（含两端语句）。
//
// ⚠ 边界取「// ── 取数」这句分区注释：它是这两个函数之后的第一处结构标记，
//
//	比行号稳（行号会随页面长大而漂 —— 本仓已因此吃过亏）。
func modelsPageAgeSnippet(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("webui-models.html")
	if err != nil {
		t.Fatalf("读 webui-models.html 失败: %v", err)
	}
	page := string(src)
	// ⚠ 从 fmtClock 开始抠：catalogUpdatedText 依赖它，只抠 fmtAgo 一段会在 node 里
	//   报 ReferenceError；而在 harness 里 stub 一个同名函数等于把被测逻辑抄第二遍（假绿）。
	start := strings.Index(page, "function fmtClock(")
	if start < 0 {
		t.Fatal("页面里找不到 function fmtClock( —— 页脚新鲜度那段被改名/搬走了（这条守卫要跟着改，别删）")
	}
	rest := page[start:]
	end := strings.Index(rest, "// ── 取数")
	if end < 0 {
		t.Fatal("找不到 fmtAgo 之后的「// ── 取数」分区注释 —— 抠不出函数边界（fail-open 的前兆）")
	}
	snippet := rest[:end]
	for _, needle := range []string{"fmtClock", "fmtAgo", "catalogUpdatedText", "age_seconds"} {
		if !strings.Contains(snippet, needle) {
			t.Fatalf("抠出来的片段不含 %q —— 切错位置了（fail-open）:\n%s", needle, snippet)
		}
	}
	return snippet
}

// TestCatalogUpdatedTextRendersAge 执行式：四种形态下必须说出**不同**的话。
//
// 反例形态（这条存在的理由）：只把「更新于 HH:MM:SS」加上「目录取自上游 0 秒前」
// 这种恒真文案 —— 结构式断言全绿，而用户依旧分不出新旧。
func TestCatalogUpdatedTextRendersAge(t *testing.T) {
	snippet := modelsPageAgeSnippet(t)
	node, lerr := exec.LookPath("node")
	if lerr != nil {
		t.Skip("没有 node，跳过执行式判据（结构式判据仍然生效）")
	}
	harness := strings.Join([]string{
		"let state = { updated: new Date(2026, 0, 1, 9, 5, 3), catalog: null, source: 'memory' };",
		"function fmtAgoCheck(age, stale, source) {",
		"  state.source = source;",
		"  state.catalog = (age === null) ? null : { age_seconds: age, stale: !!stale, ttl_seconds: 3600 };",
		"  return catalogUpdatedText();",
		"}",
		snippet,
		"const t1 = fmtAgoCheck(125, false, 'memory');",
		"const t2 = fmtAgoCheck(30, false, 'memory');",
		"const t3 = fmtAgoCheck(7000, true, 'memory');",
		"const t4 = fmtAgoCheck(-1, false, 'memory');",
		"const t5 = fmtAgoCheck(-1, false, 'store');",
		"const bad = [];",
		"if (!t1.includes('2 分钟前')) bad.push('125 秒应显示 2 分钟前，实际: ' + t1);",
		"if (!t1.includes('缓存 60 分钟')) bad.push('没有说明缓存时长: ' + t1);",
		"if (t1.includes('已过期')) bad.push('未过期却说已过期: ' + t1);",
		"if (!t2.includes('30 秒前')) bad.push('30 秒应显示 30 秒前，实际: ' + t2);",
		"if (!t3.includes('已过期')) bad.push('2 小时前的目录必须标已过期: ' + t3);",
		"if (!t4.includes('未知') || t4.includes('秒前')) bad.push('不知道时不许编一个新鲜度: ' + t4);",
		"if (!t5.includes('SQL')) bad.push('SQL 档要说清来源: ' + t5);",
		"if (!t1.includes('更新于 09:05:03')) bad.push('丢了页面这次取数的时刻: ' + t1);",
		"if (bad.length) { console.error(bad.join(' | ')); process.exit(1); }",
		"console.log('OK');",
	}, "\n")
	out, runErr := exec.Command(node, "-e", harness).CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if runErr != nil || !strings.Contains(outStr, "OK") {
		t.Errorf("页脚新鲜度文案在 node 里跑不过（%v）:\n%s\n———— 被执行的片段 ————\n%s", runErr, outStr, snippet)
	}
}

// ── 执行式兜底：语法必须过 ────────────────────────────────────────────────

var inlineScriptRe = regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

// nodeCheck 用 node --check 校验一段 JS 的**语法**（不执行）。
//
// 没有 node 时 Skip：这条是兜底判据，结构式判据（上面几条）永远生效。
func nodeCheck(t *testing.T, name, js string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node，跳过执行式判据（结构式判据仍然生效）")
	}
	f := filepath.Join(t.TempDir(), name)
	if werr := os.WriteFile(f, []byte(js), 0o600); werr != nil {
		t.Fatalf("写临时文件失败: %v", werr)
	}
	if out, cerr := exec.Command(node, "--check", f).CombinedOutput(); cerr != nil {
		t.Errorf("node --check 失败（%s）: %v\n%s", name, cerr, out)
	}
}

// TestModelCatalogRefreshAssetsParse 执行式：外壳脚本与页面内联脚本都必须过语法检查。
func TestModelCatalogRefreshAssetsParse(t *testing.T) {
	js, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 ui/app.js 失败: %v", err)
	}
	nodeCheck(t, "app.js", string(js))

	html, err := os.ReadFile("webui-models.html")
	if err != nil {
		t.Fatalf("读 webui-models.html 失败: %v", err)
	}
	n := 0
	for _, m := range inlineScriptRe.FindAllStringSubmatch(string(html), -1) {
		if strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) == "" {
			continue
		}
		n++
		nodeCheck(t, "inline-"+strconv.Itoa(n)+".js", m[2])
	}
	if n == 0 {
		t.Errorf("webui-models.html 里没找到任何内联脚本 —— 抽取正则失效了（fail-open）")
	}
}
