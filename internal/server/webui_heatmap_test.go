package server

import (
	"strings"
	"testing"
)

// TestWebUIHeatmapUsesRealSpan 热力图必须用**真实跨度**，且 0/0 的缓存桶不画点（T-388）。
//
// # 背景
//
// 数据源已从 request-log.jsonl（DefaultKeepDays=7）换成 log_daily 日聚合表（永不裁剪），
// 所以 range=365d 现在真有数据了。但升级第一天只有几天 —— 标题若硬编码「近 365 天」就是**骗人**。
//
// # 新语义：0 有两种含义（最容易做错的一处）
//
//	cache_hit == 0 && cache_miss == 0  ⇒ 该桶**没有缓存数据**（老日志没这两列）
//	                                      ⇒ 不画点、不参与命中率、不显示 0%
//	cache_hit > 0 || cache_miss > 0    ⇒ 有数据，按 hit/(hit+miss) 算
//
// T-386/T-387 之前写入的历史行这两列恒为 0；当成 0% 画出来会让用户以为"以前完全没命中过"。
//
// # 四条判据
//
//	1 标题不再硬编码「近 365 天」，必须由动态跨度算（spanDays 或等价变量）
//	2 跳过判据必须是 `=== 0`（或 `<= 0`）的**数值比较**，不能是 `!x`（0 与 undefined 会混）
//	3 by_model_out（纯输出）要被消费（后端日聚合档新补的字段）
//	4 source 判据存在（log_daily / logfile 二选一的分支）
func TestWebUIHeatmapUsesRealSpan(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// 1 动态跨度
	if !strings.Contains(html, "spanDays") {
		t.Error("热力图标题缺动态跨度计算（spanDays 或等价变量）—— 硬编码「近 365 天」在只有 7 天数据时是假的")
	}
	if strings.Contains(html, ">近 365 天 · 按天 Token 用量着色（档位按本窗口峰值自动定）<") {
		t.Error("标题仍是硬编码「近 365 天」字面 ⇒ 升级当天会显示假的跨度")
	}
	// 1b 三档文案都要有
	for _, want := range []string{"暂无数据", "近 ' + spanDays + ' 天"} {
		if !strings.Contains(html, want) {
			t.Errorf("缺跨度文案档：%q（应有「暂无数据」/ 近 N 天 / 近 365 天 三档）", want)
		}
	}
	// 2 跳过判据：数值比较
	if !strings.Contains(html, "=== 0") && !strings.Contains(html, "<= 0") {
		t.Error("缺 0/0 跳过判据（必须是 `hit + miss === 0` 这类数值比较）")
	}
	// ★ T-420：判据形态允许"先算成变量再判"—— 语义必须相同，判据不许成为摆设。
	//   原来只认内联形态（`hit + miss === 0` / `(hit + miss) === 0`），
	//   而 T-419 把空桶改写成了 `const empty = (r.cumHit + r.cumMiss) === 0;`（语义完全一致）
	//   ⇒ 字面形态变了就误红 ⇒ 这里补上"提取成变量"的形态。
	//   ⚠ **没有放宽**：下面 2c 另外要求这个判据的结果**真的作用到点位上**（empty 标记），
	//     所以"把判据删掉"照旧会红。
	if !strings.Contains(html, "hm === 0") && !strings.Contains(html, "hit + miss === 0") &&
		!strings.Contains(html, "(hit + miss) === 0") && !strings.Contains(html, "r.cumHit + r.cumMiss) === 0") {
		t.Error("缺「hit+miss === 0 ⇒ 无缓存数据」的显式判据 ⇒ 0/0 会被当成 0% 画成压底的线")
	}
	// 2c 判据必须**作用到画线上**（否则它只是个没人用的变量，0/0 照样被画出来）
	if !strings.Contains(html, "empty:") || !strings.Contains(html, ".empty") {
		t.Error("0/0 的判据没有作用到点位上（应把 empty 标进点、并在画线时按它决定实线/虚线）" +
			"—— 判据成了摆设，空桶仍会被当成 0% 画出来")
	}
	// 2b 不许把 0/0 当 0 画：命中率计算必须与跳过判据同处
	if !strings.Contains(html, "命中率") {
		t.Error("找不到命中率相关文案（判据失效）")
	}
	// 3 by_model_out
	if !strings.Contains(html, "by_model_out") {
		t.Error("没消费 by_model_out（后端日聚合档新补的「纯输出」字段）⇒ 堆叠柱/输出口径拿不到它")
	}
	// 4 source 判据
	if !strings.Contains(html, "log_daily") {
		t.Error("缺 source === 'log_daily' 的判据（明细回落时标题要给出极淡提示）")
	}
	if !strings.Contains(html, "logfile") {
		t.Error("缺 source === 'logfile' 的判据")
	}
	// 5 图例那句
	if !strings.Contains(html, "灰格") {
		t.Error("左下角图例缺「灰格 = 当天无请求」的说明（升级第一天 364 个空格子会被误解成 bug）")
	}
}

// TestWebUITokenLineSkipsEmptyHours Token 折线也必须跳过"无请求的整点"（T-390）。
//
// # 事故（实测）
//
//	perHour 05:00~11:00 全部 total=0 hit=0 miss=0（has=false）；12:00 才有 total=2
//	SVG 里 `var(--warn)`（Token 折线）pts=8，前 4 个点 y 都在图表底部
//	⇒ 画出一条**贴着底的假线**，用户读成"那段时间有数据但都是 0"
//
// 根因：Token 折线用 `rows.map(...)` 把**每个**桶都变成一个点，无请求的桶变成 [x, 0]。
// 缓存线（--acc）在 T-388 已按 `hit+miss===0` 切段 ✓ —— 这条是**同一条纪律**用在 Token 线上。
//
// # 还有一处语义错
//
//	`has` 原本写的是 `one.has`（= cache_hit+cache_miss > 0 = "该整点有没有**缓存**数据"），
//	而它的注释与用途都写着"该整点有没有**请求**"。两者在有请求但日志没写缓存列时正好相反。
//	⇒ 改成 `(p.total || 0) > 0`：有请求但 usage 缺失（tokens = -1）**仍该连线**。
func TestWebUITokenLineSkipsEmptyHours(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// 1 空桶被**标出来** + 逐段画（T-419/T-420：由"切段断开"改成"标空桶 + 虚线插值连线"）
	//   ⚠ 意图完全不变：无请求的整点**不许被画成贴底的点**（那会变成一条假线）。
	//   ⚠ 判据形态跟着实现走：原来认 `tokSegs`（切段），现在认 `tokPts`（连续点 + empty 标记）。
	if !strings.Contains(html, "tokPts") {
		t.Error("Token 折线没有按桶标记空桶（缺 tokPts）⇒ 无请求的整点会被画成贴底的点")
	}
	if strings.Contains(html, "dcLine(svg, rows.map((r, i) => [xAt(i), yTok(") {
		t.Error("Token 折线仍用 `rows.map(...)` 一次性铺满 ⇒ 无请求的桶会变成 [x, 0] 贴在底部（实测假线）")
	}
	if !strings.Contains(html, "for (let i = 1; i < tokPts.length; i++)") {
		t.Error("缺 Token 折线的逐段绘制循环（应按相邻两点逐段画，空档那几段用虚线）")
	}
	// 1a 空档必须是**虚线**（既不是断开、也不是看不出是插值的实线）—— T-419 的核心
	if !strings.Contains(html, "tokPts[i - 1].empty") || !strings.Contains(html, "dash: '4 3'") {
		t.Error("Token 折线的空档没有用虚线（缺 `tokPts[i-1].empty` 判据或 `dash: '4 3'`）" +
			"⇒ 用户仍会看到断线，或者看到一条被当成真值的实线")
	}
	// 1b 判据必须是 r.has（不是 tokens>0 —— usage 缺失时 tokens=-1 仍该连线）
	if !strings.Contains(html, "!r.has") {
		t.Error("空桶判据必须是 `!r.has` —— 用 tokens>0 会把「有请求但 usage 缺失」的桶也丢掉")
	}
	// 2 has 的语义：有请求（total>0），不是有 token/有缓存数据
	if !strings.Contains(html, "has: (p.total || 0) > 0") {
		t.Error("`has` 必须是 `(p.total || 0) > 0`（该整点**有请求**）—— 原先是 `one.has`（有没有缓存数据），" +
			"与它的注释/用途相反；用 tokens>0 也不对（usage 缺失时 tokens=-1 仍该连线）")
	}
	// 3 过时文案
	if strings.Contains(html, "无请求的整点按 0% 计") {
		t.Error("仍有过时文案「无请求的整点按 0% 计」—— 现在的行为是**不画点**（线在那里断开）")
	}
	if !strings.Contains(html, "无请求的整点不画点") {
		t.Error("图例缺新文案「无请求的整点不画点（线在那里断开）」")
	}
}
