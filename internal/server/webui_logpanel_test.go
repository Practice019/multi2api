package server

import (
	"strings"
	"testing"
)

// TestWebUILogPanelFiltersOnServer 日志面板的筛选必须走服务端，且轮询要能短路（T-396a）。
//
// # 三个真实缺陷（用户实测）
//
//	① 点「200」只筛**本页 30 条**（后端 /admin/logs/history 当时还没有 status 入参）
//	   ⇒ 用户翻到第 2 页点「200」，看到的只是"第 2 页里的 200"，而全库有 24 条 200
//	   ⇒ 修法：setLogStatus 改成**重新请求**（带 status），并把 offset 归零（换筛选必须回第 1 页）
//	② 卡片数字是"本页统计"，却看起来像"全库统计"
//	   ⇒ 修法：改用后端给的 `r.counts`（全库状态计数，键是**字符串**）+ total_all / total
//	③ 每 30 秒轮询都把表格整块重建 ⇒ 白屏闪一下（用户报的"卡顿"有一半是它）
//	   ⇒ 修法：拉取前只在"首次/空表"时显示占位；renderLogs 加**签名短路**（seq 列表相同就一个 DOM 都不碰）
//
// ⚠ 签名必须用 seq 列表而不是下标：下标会随新增条目整体位移 ⇒ 每次轮询都"变了" ⇒ 等于没短路。
func TestWebUILogPanelFiltersOnServer(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// 1 请求带 status
	if !strings.Contains(html, "qs.set('status'") {
		t.Error("loadLogHistory 没有把筛选拼进 URL（缺 qs.set('status'）⇒ 筛选只作用于当前页那 30 条，不是全库")
	}
	if strings.Contains(html, "&offset=${logPage.offset}`)") || strings.Contains(html, "&offset=${logPage.offset}`") {
		t.Error("loadLogHistory 仍在直接拼 limit/offset（未改走 URLSearchParams）")
	}
	// 2 setLogStatus 重新请求
	si := strings.Index(html, "function setLogStatus(v) {")
	if si < 0 {
		t.Fatal("找不到 setLogStatus")
	}
	sbody := html[si:]
	if j := strings.Index(sbody, "\n  function "); j > 0 {
		sbody = sbody[:j]
	}
	if !strings.Contains(sbody, "logPage.offset = 0") {
		t.Error("setLogStatus 没有把 offset 归零 ⇒ 换筛选后可能落在越界页（空表）")
	}
	// ★ T-400 更新：判据从"精确字符串 loadLogHistory()"改成"调用了 loadLogHistory 且带 force"。
	//   为什么：T-400 给 loadLogHistory 加了 {force:true} —— 换筛选/手动刷新必须**穿透复用窗口**，
	//   否则用户点「刷新」会被 30 秒复用吞掉（本仓 loadGenericList 的注释记着同型教训：
	//   "刷新按钮确实重发了 HTTP，但发的是和首次加载完全相同的 URL"）。
	//   原意图不变：点卡片必须触发一次**真正会穿透缓存**的请求。
	if !strings.Contains(sbody, "loadLogHistory(") {
		t.Error("setLogStatus 没有重新请求（应调 loadLogHistory）—— 筛选是服务端参数，只重绘本地缓冲是错的")
	}
	if !strings.Contains(sbody, "force: true") && !strings.Contains(sbody, "force:true") {
		t.Error("setLogStatus 的请求没有传 force —— 换筛选是用户意图，必须穿透 30 秒复用窗口" +
			"（否则表现成「点了没反应」）")
	}
	if strings.Contains(sbody, "renderLogs(logEntries)") {
		t.Error("setLogStatus 仍是 `renderLogs(logEntries)` ⇒ 只筛本地缓冲（本页 30 条）")
	}
	// 3 签名短路
	if !strings.Contains(html, "lastLogSig") {
		t.Error("缺 lastLogSig 签名短路 ⇒ 每次 30 秒轮询都整块重建表格（白屏闪）")
	}
	if !strings.Contains(html, "if (sig === lastLogSig) return") {
		t.Error("renderLogs 里没有 `if (sig === lastLogSig) return` 的短路")
	}
	if !strings.Contains(html, "lastLogSig = ''") {
		t.Error("清屏/恢复显示路径必须重置 lastLogSig = ''（否则清屏后画不出来）")
	}
	// 4 卡片用服务端 counts
	if !strings.Contains(html, ".counts") {
		t.Error("renderLogStatusBar 没有消费服务端的 counts（仍是本页统计 ⇒ 数字看起来像全库的）")
	}
	if !strings.Contains(html, "total_all") {
		t.Error("没有用 total_all（全库总数）⇒ 「全部 N」的 N 说不清范围")
	}
	// 5 旧签名不得残留
	if strings.Contains(html, "renderLogStatusBar(counts, pageLen)") {
		t.Error("仍存在旧签名 `renderLogStatusBar(counts, pageLen)`（本页统计版）")
	}
	if !strings.Contains(html, "renderLogStatusBar(serverCounts") && !strings.Contains(html, "renderLogStatusBar(r.counts") {
		t.Error("renderLogStatusBar 的调用点没有传服务端 counts（应为 serverCounts / r.counts）")
	}
}

// TestWebUILogPanelRowDiffAndFetchReuse 日志面板的**行级 diff** 与**请求复用（区分轮询/手动）**（T-400）。
//
// # ① 行级 diff（用户原话："每次都会把这个页面进行重构，然后就会卡顿"）
//
// T-396a 的签名短路只救了"列表完全没变"。只要有新日志进来，旧实现就整块重建
// 15 行 × 10 列 ⇒ 重排 + 重绘 + 滚动位置跳。⇒ 按 **seq** 对齐做行级增删：
//
//	· 顶部多了 N 条（curSeq 是 wantSeq 的后缀）⇒ 只 prepend N 个节点
//	· 其余情况 ⇒ DocumentFragment + replaceChildren 一次性替换
//	⚠ 必须有**单一**的行生成函数 htmlToRow（本仓踩过"两处模板各写一遍、改一处漏一处"）
//
// # 滚动补偿（必须显式定）
//
// 用户在读第 5 条时来了 3 条新日志 ⇒ 不补偿就把正在读的内容推下去 3 行。
// 规则：贴在顶部（scrollTop <= 2）就保持顶部；否则按新增行数补偿。
//
// # ② 请求复用：必须区分**轮询**与**手动刷新**
//
// 用户点「刷新」的全部意义就是"我要看最新的"。若"同参数 30 秒内复用"把手动脉冲吞掉，
// 表现就是"点了刷新没反应" —— 本仓 exact 同型教训见 loadGenericList 的注释。
// ⇒ 轮询可不带 force（可复用）；手动刷新 / 换筛选 / 翻页 / 切面板必须 force。
func TestWebUILogPanelRowDiffAndFetchReuse(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// 1 单一行生成函数 + 不再整块 innerHTML 重建
	if !strings.Contains(html, "function htmlToRow(") {
		t.Error("缺 htmlToRow（单一行生成函数）—— 两处模板各写一遍必然改一处漏一处")
	}
	if strings.Contains(html, "host.innerHTML = shown.map") {
		t.Error("renderLogs 仍用 `host.innerHTML = shown.map(...)` 整块重建 ⇒ 有新日志就重排重绘（用户报的「卡顿」）")
	}
	if !strings.Contains(html, "replaceChildren(") {
		t.Error("缺 replaceChildren(...)—— 其余情形也要一次性替换（拼 DOM 不是逐条 append）")
	}
	if !strings.Contains(html, "htmlToRow(shown[") {
		t.Error("顶部插入分支没有用 htmlToRow 造节点（说明两套模板并存）")
	}
	// 2 顶部插入分支
	if !strings.Contains(html, "insertBefore(") {
		t.Error("缺顶部插入分支（insertBefore）—— 顶部新增 N 条时应只 prepend N 个节点")
	}
	// ★ T-402 更新：判据形态从"严格后缀"改成"按较短区间对齐"。
	//   原意图不变（要能确认"只是顶部多了几条、中间没变"），但三种旧形态**全都**
	//   在固定页大小下永远为假：① 严格后缀 ② 长度差当位移量（恒 0）③ 右端越界的 every（undefined）。
	if !strings.Contains(html, ".slice(0, cmpN).every(") {
		t.Error("缺「按较短区间对齐」判据（curSeq.slice(0, cmpN).every(…)）—— " +
			"没有它就无法确认「只是顶部多了几条」；而右端越界的 every 在固定页大小下永远为假")
	}
	if !strings.Contains(html, "wantSeq.indexOf(curSeq[0])") {
		t.Error("缺锚点位移量（newN = wantSeq.indexOf(curSeq[0])）—— " +
			"用长度差算位移在固定页大小下恒为 0 ⇒ 情形 A 永远不进")
	}
	// 3 滚动补偿
	if !strings.Contains(html, "nearTop") {
		t.Error("缺 nearTop 判据 —— 滚动位置的取舍必须显式定（贴顶就保持贴顶）")
	}
	if !strings.Contains(html, "scrollTop +=") {
		t.Error("缺滚动补偿（scrollTop += N * rowH）—— 不补偿会把用户正在读的内容推下去")
	}
	// 4 force 参数
	if !strings.Contains(html, "async function loadLogHistory(opt") {
		t.Error("loadLogHistory 没有 opt 参数 ⇒ 无法区分「轮询可复用」与「手动必须穿透」")
	}
	if !strings.Contains(html, "force") {
		t.Error("缺 force 标记（手动刷新 / 换筛选 / 翻页 / 切面板都要穿透）")
	}
	// setLogStatus 里必须 force
	si := strings.Index(html, "function setLogStatus(v) {")
	if si >= 0 {
		sbody := html[si:]
		if j := strings.Index(sbody, "\n  function "); j > 0 {
			sbody = sbody[:j]
		}
		if !strings.Contains(sbody, "force: true") {
			t.Error("setLogStatus 换筛选时必须 force（换筛选是用户意图，不能命中复用）")
		}
	}
	// 5 30 秒复用窗口
	if !strings.Contains(html, "30000") {
		t.Error("缺 30 秒复用窗口（LOGFETCH_TTL 或字面 30000）")
	}
	if !strings.Contains(html, "lastFetchAt") {
		t.Error("缺 lastFetchAt（复用判据要它）")
	}
}
