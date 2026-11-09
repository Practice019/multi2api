package server

import (
	"strings"
	"testing"
)

// ui_wiring_logstatus_test.go 请求日志「状态码卡片」的接线守卫。
//
// # 用户要求（2026-10-25 原话）
//
//	「关于这个状态我希望可以列出 请求日志出现过的状态码例如200 429等等
//	 然后用户可以点击列出的状态卡片进行筛选
//	 然后用户可以方便查看错误的状态码请求日志进行纠错」
//
// # 守的是什么
//
// 卡片是纯前端拼字符串生成的，点击靠事件委托。任何一环改名都**不会报错**：
//
//	生成处少写 data-logst  → 点击时 dataset.logst 是 undefined → 筛选静默失效
//	点击处理被删/改回      → 卡片变成纯装饰，点了没反应
//	setLogStatus 不重绘表格 → 卡片亮着、下面的行没变（比不筛选更让人困惑）
//
// 三种都只在用户手上才暴露，所以逐个钉住。

// TestLogStatusBarRendersChips 卡片条必须真的被画出来。
//
// 判据取三个**同时存在**的特征：
//
//	① 有 renderLogStatusBar 函数（画卡片）
//	② 生成的按钮带 data-logst（点击时能取到值）
//	③ 有「全部」那一张（否则用户无法取消筛选）
func TestLogStatusBarRendersChips(t *testing.T) {
	src := readWebUI(t)
	for _, want := range []struct{ name, needle string }{
		{"renderLogStatusBar 函数", "function renderLogStatusBar("},
		{"按钮带 data-logst", `data-logst="${val}"`},
		{"「全部」卡片", "chip('', '全部',"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Errorf("缺少 %s（找 %q）—— 卡片条不会渲染，或点了取不到值", want.name, want.needle)
		}
	}
}

// TestLogStatusClickFilters 点卡片必须真的筛选表格。
//
// 关键：setLogStatus 必须走 renderLogs(logEntries) 重绘。
// 只改 class 不改表格 = 卡片亮着、下面的行没变，用户会以为筛选坏了。
func TestLogStatusClickFilters(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, "function setLogStatus(") {
		t.Fatal("缺少 setLogStatus —— 点击卡片不会改变筛选状态")
	}
	body := sliceFuncBody(t, src, "function setLogStatus(")
	if body == "" {
		t.Fatal("切不到 setLogStatus 函数体")
	}
	// ★ T-396a 更新：筛选改成**服务端**参数 ⇒ 判据从"重绘表格"变成"重新请求 + 回第 1 页"。
	// 原意图不变（点了卡片下面的行必须真的变），只是实现路径变了：
	// 本地缓冲只有当前页 30 条，只重绘会让用户以为"全库只有这几个 200"。
	// ★ T-400：调用形态变成 loadLogHistory({force:true}) ⇒ 判据允许参数，但必须同时断言 force。
	if !strings.Contains(body, "force: true") && !strings.Contains(body, "force:true") {
		t.Error("setLogStatus 的请求没有传 force —— 换筛选是用户意图，必须穿透 30 秒复用窗口")
	}
	if !strings.Contains(body, "loadLogHistory(") {
		t.Error("setLogStatus 没有重新请求（应调 loadLogHistory）—— 筛选是服务端参数，" +
			"只重绘本地缓冲会把「本页里的 200」当成「全库的 200」")
	}
	if !strings.Contains(body, "logPage.offset = 0") {
		t.Error("setLogStatus 没有把 offset 归零 —— 换筛选后可能落在越界页（空表）")
	}
	if strings.Contains(body, "renderLogs(logEntries)") {
		t.Error("setLogStatus 仍是本地重绘（renderLogs(logEntries)）—— 应改为重新请求")
	}
}

// TestLogStatusBarIsScopedInCSS 卡片的选中样式必须限定在 #logStatusBar 内。
//
// .chip 同时被模型面板用着（`.chips .chip`）。一条全局 `.chip.on` 会在
// 那边将来加 on 类时莫名变色 —— 一处样式影响两个面板，这种 bug 极难查。
func TestLogStatusBarIsScopedInCSS(t *testing.T) {
	src := readFrontendCSS(t)
	if !strings.Contains(src, "#logStatusBar .chip.on{") {
		t.Error("缺少 #logStatusBar .chip.on 的**限定作用域**样式 —— " +
			"全局 .chip.on 会波及模型面板")
	}
	// 反例：不允许出现裸的 .chip.on{...}（不带父选择器）。
	if strings.Contains(src, "\n  .chip.on{") || strings.Contains(src, "\n.chip.on{") {
		t.Error("出现了全局 .chip.on 样式 —— 会波及模型面板，应限定在 #logStatusBar 内")
	}
}

// TestLogStatusBarStatesItsScope 卡片必须**诚实标注统计范围**。
//
// 日志是服务端分页的，卡片只统计**当前页**。不写明这一点，用户会以为
// 「429 只有 2 条」是全局结论，而实际全量里可能有几百条。
// 这是本项目既有纪律：数字必须能自我解释，不许骗人。
func TestLogStatusBarStatesItsScope(t *testing.T) {
	src := readWebUI(t)
	// ⚠ 判据必须落在**渲染出来的字符串**里，不能只搜全文：
	//   这几个字在注释里也出现过（解释为什么要有这句），
	//   只搜全文的话「把渲染那行删掉」也能通过 —— 变异验证时正是这么漏的。
	//
	//   渲染行的形态是：+ `本页 ${pageLen} 条 · 共 ${total} 条 · 卡片只统计当前页</span>`;
	//   取「反引号 + 本页」作为定位锚点，避免命中注释。
	// ★ T-396a 更新：数字来源从"本页统计"改成**服务端全库** ⇒ 渲染文案随之改变。
	// 原意图不变（数字必须自己说清范围，否则用户会当成全局结论）。
	if !strings.Contains(src, "`本页 ${pageLen} 条 · 当前筛选 ${serverTotalFiltered} 条 · 全库 ${serverTotalAll} 条") {
		t.Error("卡片条没有**渲染**「本页 N 条 · 当前筛选 M 条 · 全库 K 条」—— " +
			"三个数字各表一件事（本页渲染行数 / 筛选后总数 / 全库总数），少了范围说明就会被当成全局结论")
	}
}

// TestLogStatusPageLenIsRenderedRows 「本页 N 条」必须用渲染出来的行数。
//
// 末页往往不足一页（比如每页 15 条但只剩 3 条），用「每页条数设置」会与实际
// 行数对不上 —— 用户数一下表格发现只有 3 行，卡片却写 15。
func TestLogStatusPageLenIsRenderedRows(t *testing.T) {
	src := readWebUI(t)
	body := sliceFuncBody(t, src, "function renderLogStatusBar(")
	if body == "" {
		t.Fatal("切不到 renderLogStatusBar 函数体")
	}
	// 参数名必须是 pageLen（渲染行数），不能是每页设置值。
	// ★ T-396a 更新：签名变成 (serverCounts, serverTotalAll, serverTotalFiltered, pageLen)，
	// pageLen 仍是**最后一个**参数 —— 原意图不变（必须是渲染行数，不能是每页设置值）。
	if !strings.Contains(src, "function renderLogStatusBar(serverCounts, serverTotalAll, serverTotalFiltered, pageLen)") {
		t.Error("renderLogStatusBar 的签名不对 —— 应为 (serverCounts, serverTotalAll, serverTotalFiltered, pageLen)，" +
			"其中末位的 pageLen 必须是渲染行数（用每页设置值会让末页的「本页 N 条」与实际行数不符）")
	}
}
