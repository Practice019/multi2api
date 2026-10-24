package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ui_wiring_dashchart_test.go 仪表盘图表**共享绘制层**的接线守卫。
//
// # 守的是什么
//
// 用户原话：
//
//	「上面的表格看起来很简单」
//	「下面的实现可以抽共享一起用」
//	「我喜欢下面的表格柱状图 折线图还有等分线等等」
//
// ⇒ 顶部「性能健康」那排要**照下面抄**：同一套等分网格线、同一套柱子、
// 同一套折线。做法是把下方两张趋势图里各自内联的绘制代码抽成
// internal/server/dash-chart.js（由 tools/dash_chart_sync.js 注入 webui.html），
// 然后三处（顶部健康条 / #cacheTrend / #dailyTrend）都调它。
//
// # 为什么这些断言必须是**字符串**断言
//
// 前端嵌在二进制里（webui.html 走 go:embed）。这类"抽共享"最容易出的错不是
// 编译错，而是：
//
//	① 只在顶部抄了一份新的绘制代码 —— 那就成了第三份副本，不是共享；
//	② 抽了但只让一处调用 —— 另一处还在内联 for 循环画网格线；
//	③ 共享层改了却忘了跑同步脚本 —— 页面里还是旧的。
//
// 三种都只在用户手上才暴露（图还是画得出来，只是"两处慢慢漂移"）。
// 所以这里钉：共享函数存在、**至少两处调用**、网格线还在、柱子/折线还在、
// 空数据不崩。

// dashChartBody 取 webui.html 里被注入的那段共享层正文（START/END 之间）。
//
// ⚠ 取"页面里那份"而不是只读 dash-chart.js：用户真正运行的是页面里这份。
//   源文件与页面的一致性由 TestDashChartIsInSync 单独守。
func dashChartBody(t *testing.T) string {
	t.Helper()
	ui := readWebUI(t)
	start := strings.Index(ui, "// ==== WB2API:DASH-CHART:START")
	end := strings.Index(ui, "// ==== WB2API:DASH-CHART:END ====")
	if start < 0 || end < start {
		t.Fatalf("webui.html 里找不到 DASH-CHART 注入标记（start=%d end=%d）—— "+
			"共享绘制层没注入，顶部与下方图表会各画一套", start, end)
	}
	return ui[start:end]
}

// TestDashChartSharedLayerInjected 共享层必须在页面里，且关键函数一个不少。
//
// 缺任何一个，调用处就是 **ReferenceError** —— 那会打断整个 IIFE，
// 后果不是"图不画"，而是**页面上每个按钮都点不动**（groups_ui_test.go 里
// TestWebUINoUndefinedFunctionCalls 记着同一个事故）。
func TestDashChartSharedLayerInjected(t *testing.T) {
	body := dashChartBody(t)
	for _, want := range []struct{ what, needle string }{
		{"网格等分段数", "var DC_SEGS = 4;"},
		{"配色（与命中率线同色）", "var DC_RATE_COLOR = '#58a6ff';"},
		{"横等分网格线", "function dcGrid("},
		{"竖等分网格线（横向条形用）", "function dcGridCols("},
		{"柱子/横条", "function dcRects("},
		{"折线", "function dcLine("},
		{"悬浮参考线", "function dcCursor("},
		{"SVG 外壳", "function dcSvgTag("},
		{"百分比换算（唯一入口）", "function dcPct("},
		{"按高低分档配色", "function dcTier("},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("★ 共享绘制层缺少%s（%q）—— 调用处会 ReferenceError，整页按钮失效",
				want.what, want.needle)
		}
	}
}

// TestDashChartUsedByBothTrendsAndHealthBar 抽共享的**核心判据**：至少两处调用。
//
// 用户要的是「下面的实现可以抽共享一起用」，所以判据不能只是"共享函数存在"——
// 那可以被一份"抽出来但没人用"的死代码满足。必须是**下方两张趋势图**
// 与**顶部健康条**都在调同一批函数。
func TestDashChartUsedByBothTrendsAndHealthBar(t *testing.T) {
	ui := readWebUI(t)

	// ① 下方：缓存命中率趋势（双轴折线）
	cache := sliceFuncBody(t, ui, "function renderCacheTrendSVG(")
	if cache == "" {
		t.Fatal("webui.html 里找不到 renderCacheTrendSVG")
	}
	for _, n := range []string{"dcGrid(", "dcLine(", "dcCursor(", "dcSvg("} {
		if !strings.Contains(cache, n) {
			t.Errorf("★ renderCacheTrendSVG 没有调用共享层的 %s —— 它还在自己画一套", n)
		}
	}

	// ② 下方：按天 Token 趋势（柱状 + 折线）
	daily := sliceFuncBody(t, ui, "function renderDailyTrendSVG(")
	if daily == "" {
		t.Fatal("webui.html 里找不到 renderDailyTrendSVG")
	}
	for _, n := range []string{"dcGrid(", "dcRects(", "dcLine(", "dcCursor(", "dcSvg("} {
		if !strings.Contains(daily, n) {
			t.Errorf("★ renderDailyTrendSVG 没有调用共享层的 %s —— 它还在自己画一套", n)
		}
	}

	// ③ 顶部：健康条（成功率横条 + 分项条组 + 迷你折线）
	hb := sliceFuncBody(t, ui, "function renderHealthBar(")
	if hb == "" {
		t.Fatal("webui.html 里找不到 renderHealthBar")
	}
	for _, n := range []string{"hbMeter(", "hbSpark(", "hbAxis(", "dcTier("} {
		if !strings.Contains(hb, n) {
			t.Errorf("★ renderHealthBar 没有用 %s —— 顶部那排还是纯文字 chip", n)
		}
	}
	// 横条与迷你折线**本身**必须走共享层（而不是在 hb* 里再写一份 rect/path）
	for _, f := range []struct{ name, sig, needle string }{
		{"成功率横条 hbMeter", "function hbMeter(", "dcGridCols("},
		{"成功率横条 hbMeter", "function hbMeter(", "dcRects("},
		{"迷你折线 hbSpark", "function hbSpark(", "dcLine("},
		{"迷你折线 hbSpark", "function hbSpark(", "dcGrid("},
	} {
		body := sliceFuncBody(t, ui, f.sig)
		if body == "" {
			t.Fatalf("webui.html 里找不到 %s", f.sig)
		}
		if !strings.Contains(body, f.needle) {
			t.Errorf("★ %s 没有调用共享层的 %s —— 顶部与下方会各画一套，早晚漂移", f.name, f.needle)
		}
	}
}

// TestDashChartGridLinesAreEvenlyDivided 等分线必须在。
//
// 用户点名要的就是这个（「等分线」）。判据取**几何写法**而不是注释：
// 段数循环 + 同一批横线/竖线。⚠ 只判注释会假通过 —— 注释里写着"等分"、
// 代码却画一条线的情况是真实可能发生的。
func TestDashChartGridLinesAreEvenlyDivided(t *testing.T) {
	body := dashChartBody(t)

	grid := sliceFuncBody(t, body, "function dcGrid(")
	if grid == "" {
		t.Fatal("共享层里找不到 dcGrid")
	}
	for _, n := range []struct{ what, needle string }{
		{"按段数等分（4 段 = 5 条线 = 0/25/50/75/100%）", "i <= segs"},
		{"横线用主题色 var(--line)", "stroke=\"var(--line)\""},
		{"左轴刻度（双轴时染成与线同色）", "text-anchor=\"end\""},
		{"右轴刻度（可选）", "rightFmt"},
	} {
		if !strings.Contains(grid, n.needle) {
			t.Errorf("★ dcGrid 缺少%s（%q）—— 等分线画不出来", n.what, n.needle)
		}
	}

	cols := sliceFuncBody(t, body, "function dcGridCols(")
	if cols == "" {
		t.Fatal("共享层里找不到 dcGridCols（横向条形要的竖等分线）")
	}
	for _, n := range []struct{ what, needle string }{
		{"竖线按段数等分", "i <= segs"},
		{"竖线用主题色 var(--line)", "stroke=\"var(--line)\""},
		{"百分比刻度（0/25/50/75/100%）", "(100 * i) / segs"},
	} {
		if !strings.Contains(cols, n.needle) {
			t.Errorf("★ dcGridCols 缺少%s（%q）", n.what, n.needle)
		}
	}
}

// TestHealthBarShowsBarsAndSparkline 顶部那排必须真的是**图形**，不是纯文字。
//
// 三条具体形态（用户原话逐条对）：
//
//	成功率      → 横向条形（hbMeter 里 dcRects 画的那根）
//	分项成功率  → 条形组 + 百分比标签 + 按高低分档配色
//	趋势        → 迷你折线（hbSpark 里 dcLine 画的那条）
//
// ⚠ 判据取"渲染出来的结构"，不取文案：文案改一个字不该让守卫变红，
//   但**图形消失**必须变红。
func TestHealthBarShowsBarsAndSparkline(t *testing.T) {
	ui := readWebUI(t)

	hb := sliceFuncBody(t, ui, "function renderHealthBar(")
	if hb == "" {
		t.Fatal("webui.html 里找不到 renderHealthBar")
	}
	// ① 成功率那格带条形容器（.hb-meter），且旁边保留精确读数
	if !strings.Contains(hb, "hbMeter(rate)") {
		t.Error("★ 成功率的渲染没有调用 hbMeter(rate) —— 又退回纯文字百分比了")
	}
	if !strings.Contains(hb, "class=\"hb-meter\"") {
		t.Error("★ 成功率/分项没有 .hb-meter 容器 —— 条形画在哪儿？")
	}
	// ② 分项成功率：条形组 + 百分比标签 + 分档配色
	if !strings.Contains(hb, "class=\"hb-models\"") {
		t.Error("★ 没有 .hb-models 条组容器 —— 分项成功率还是那排小 chip")
	}
	if !strings.Contains(hb, "r.rate.toFixed(1) + '%'") {
		t.Error("★ 分项成功率没有百分比标签 —— 用户看不出差多少")
	}
	if !strings.Contains(hb, "dcTier(r.rate)") {
		t.Error("★ 分项成功率没有按高低分档配色（dcTier）—— 好/坏分不出来")
	}
	// ③ 趋势：迷你折线 + 数据源
	if !strings.Contains(hb, "hbSpark(trend)") {
		t.Error("★ 没有 hbSpark(trend) —— 趋势折线不见了")
	}
	if !strings.Contains(ui, "function loadHealthSeries(") {
		t.Error("★ 没有 loadHealthSeries —— 折线的数据源不存在（永远画不出来）")
	}
	// ④ 刻度行（0/25/50/75/100%）必须渲染出来
	if !strings.Contains(hb, "hbAxis()") {
		t.Error("★ 没有 hbAxis() —— 横条底下缺等分刻度")
	}
}

// TestDashChartIsInSync 注入产物必须与源文件逐字节一致。
//
// 与 model-picker 的同名守卫同一立场：改了 dash-chart.js 却忘了跑
// tools/dash_chart_sync.js ⇒ 页面里还是旧实现，而且**不会有任何报错**。
func TestDashChartIsInSync(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("dash-chart.js"))
	if err != nil {
		t.Fatalf("读 dash-chart.js: %v", err)
	}
	want := strings.TrimSpace(strings.ReplaceAll(string(src), "\r\n", "\n"))
	body := dashChartBody(t)
	// 去掉 START 那行本身，取中间正文。
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	}
	got := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if got != want {
		t.Error("★ 页面里的 dash-chart 与源文件不一致 —— 跑 node tools/dash_chart_sync.js 同步")
		t.Logf("源文件 %d 字节，页面内 %d 字节", len(want), len(got))
	}
}

// TestDashChartEmptyDataDoesNotCrash 空数据 / 缺字段不能崩，也不能画出假图。
//
// # 三种真实的空态
//
//	① 成功率分母为 0（没有请求）→ 读数是占位符，**不画条**（0% 会画成一根长度 0 的条，
//	   那会被读成"成功率 0%"）；
//	② 分项成功率没有模型（by_model_detail 为空）→ 整组不渲染，而不是一个空框；
//	③ 趋势没有点（series 还没回来）→ 不画折线，而不是一条贴底的直线。
//
// 判据：共享层的换算函数对"分母 <= 0"返回 **null**（不是 0），且调用处逐个判空。
func TestDashChartEmptyDataDoesNotCrash(t *testing.T) {
	body := dashChartBody(t)

	pct := sliceFuncBody(t, body, "function dcPct(")
	if pct == "" {
		t.Fatal("共享层里找不到 dcPct")
	}
	if !strings.Contains(pct, "if (d <= 0) return null;") {
		t.Error("★ dcPct 在分母 <= 0 时没有返回 null —— " +
			"「没有请求」会被渲染成「成功率 0%」，看起来像网关全挂了")
	}
	// NaN 兜底：缺字段时 undefined/NaN 会写进 SVG 属性，元素被浏览器**静默丢弃**
	numFn := sliceFuncBody(t, body, "function dcNum(")
	if numFn == "" || !strings.Contains(numFn, "isFinite") {
		t.Error("★ dcNum 没有 isFinite 兜底 —— NaN 进 SVG 属性后元素会被静默丢弃，图看起来少一块")
	}

	// 调用处的空态：健康条、横条、迷你折线三处都要判空
	hb := sliceFuncBody(t, readWebUI(t), "function renderHealthBar(")
	if !strings.Contains(hb, "if (!s) { el.innerHTML = ''; return; }") {
		t.Error("★ renderHealthBar 没有对 s 为空做守卫 —— 未取到 stats 时会抛异常")
	}
	if !strings.Contains(hb, "if (rows.length)") {
		t.Error("★ 分项成功率没有对空列表判空 —— 会渲染出一个空条组")
	}
	if !strings.Contains(hb, "trend.length >= 2") {
		t.Error("★ 迷你折线没有判「至少 2 个点」—— 单点连不成线，会画出一个孤零零的圆")
	}
	if !strings.Contains(hb, "rate === null ? '—'") {
		t.Error("★ 成功率没有占位符空态 —— 无请求时会显示成 0%")
	}
}
