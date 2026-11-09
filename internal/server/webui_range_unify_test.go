package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWebUITimeRangeIsUnified 时间筛选**两排合成一套 4 档**（T-413）。
//
// # 用户原话
//
//	「它的筛选的按钮有今天、全部、7天、30天、本月，但是后面又有一个24小时、7天、30天、365天的，
//	  它是控制下面的那个表格。那我觉得这两个其实是可以融在一起的……统一起来起来会比较好一点」
//	「我觉得这个365天可以不要，就保留热力图的就可以了」
//
// ⇒ 定案：一套 4 档（全部 · 24小时 · 7天 · 30天），一次驱动
//
//	指标卡/Top 榜/环图/缓存四格（/admin/stats）+ 消耗分布图/趋势图（/admin/stats/series）；
//	热力图**不跟**（始终 365 天，有自己的动态标题）。
//
// # 为什么必须有一条守卫钉住"两排"
//
// 这两排控件在 2026-10-18 到 2026-11-09 之间**并存了近三周**，而且 HTML 里专门写了
// 一段注释解释「它与下面那个趋势范围是两回事」——
// **一个界面需要注释解释"这两排不是一回事"，本身就是设计缺陷**。
// 这条测试就是把"不许再长回两排"钉死。
func TestWebUITimeRangeIsUnified(t *testing.T) {
	html, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	page := string(html)
	jsB, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 ui/app.js 失败：%v", err)
	}
	js := string(jsB)

	// ① 旧的「统计范围」排已彻底删除
	if strings.Contains(page, `id="statsRange"`) {
		t.Error("webui.html 里仍有 id=\"statsRange\" —— 旧的「统计范围」排没删干净（两排并存正是本次要修的）")
	}

	// ② trendRange 恰好 4 个按钮，data-range 依次是 all / 24h / 7d / 30d
	span := regexp.MustCompile(`(?s)<span class="tog" id="trendRange".*?</span>`).FindString(page)
	if span == "" {
		t.Fatal("找不到 #trendRange 控件块")
	}
	btns := regexp.MustCompile(`data-range="([^"]+)"`).FindAllStringSubmatch(span, -1)
	got := make([]string, 0, len(btns))
	for _, m := range btns {
		got = append(got, m[1])
	}
	want := []string{"all", "24h", "7d", "30d"}
	if len(got) != len(want) {
		t.Errorf("#trendRange 按钮数 = %d（%v），应恰好 4 个 %v", len(got), got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("第 %d 个按钮 data-range=%q，应为 %q（顺序也是判据的一部分：全部在最左）", i+1, got[i], want[i])
			}
		}
	}

	// ③ 「365d」按钮不许再出现（用户明确不要）—— 但后端对 365d 的支持要保留（热力图用）
	if strings.Contains(page, `data-range="365d"`) {
		t.Error(`webui.html 里仍有 data-range="365d" 按钮 —— 用户明确说「365天可以不要，只保留热力图的」`)
	}
	if strings.Contains(js, `data-range="365d"`) {
		t.Error(`app.js 里仍有 data-range="365d"`)
	}
	// ③b 热力图仍在用 series?range=365d（别把后端那档也删了）
	if !strings.Contains(js, "range=365d") {
		t.Error("app.js 里找不到 range=365d —— 热力图读的就是那一档，删按钮不等于删支持")
	}

	// ④ STATS_RANGE_MAP 四个键齐全
	for _, k := range []string{"'all':", "'24h':", "'7d':", "'30d':"} {
		if !strings.Contains(js, k) {
			t.Errorf("STATS_RANGE_MAP 缺档位 %s —— 映射表是全套统一的关键（两个后端参数名不同，见注释）", k)
		}
	}

	// ⑥ 「全部」档的图表必须是 **24h**，不是 365d（T-414）。—— **反直觉映射，必须单独钉一条**
	//
	// 名字叫"全部"，图表却只画 24 小时 —— 后来者第一反应是"这不合理"，顺手改成 365d。
	// 而那是用户 2026-11-09 明确否掉的：
	//   「我点击全部的按钮的时候，它下面那个表格不要365天，它就显示24小时的就可以了。」
	//
	// # 为什么这个反直觉映射是对的（理由要留在代码里，否则活不过下一次"清理"）
	//   「全部」说的是**卡片的统计口径**（累计至今）；而 365 天窗口里绝大部分天是空的，
	//   柱子被摊平成一片、看不出任何东西 ⇒ 24 小时才是**有形状**的那一段。
	//   ⚠ 热力图仍独立看 365 天（它本来就是这个用途），不受本表影响。
	//
	// 判据两条：
	//   · STATS_RANGE_MAP 里 'all' 那一项的 series 必须是 '24h'
	//   · 全文**不得**出现 `series: '365d'`（任何档位都不许 —— 365d 只留给热力图的
	//     直接 URL 调用 `range=365d`，那走的是另一处代码）
	if !regexp.MustCompile(`(?s)'all'\s*:\s*\{[^}]*series:\s*'24h'`).MatchString(js) {
		t.Error("STATS_RANGE_MAP 的 'all' 档 series 不是 '24h' —— 用户明确否掉了 365 天：" +
			"「点全部的时候下面那个表格不要365天，显示24小时就可以了」")
	}
	if regexp.MustCompile(`series:\s*'365d'`).MatchString(js) {
		t.Error("STATS_RANGE_MAP 里出现了 series: '365d' —— 365 天只留给热力图的直接调用；" +
			"档位映射表里不许有（用户的「全部」= 卡片算全部历史，但图表看最近 24 小时）")
	}
	// ⑥b 每一档都必须有 note 字段（口径差异要能说明，不许静默）
	for _, k := range []string{"'all'", "'24h'", "'7d'", "'30d'"} {
		if !regexp.MustCompile(`(?s)` + regexp.QuoteMeta(k) + `\s*:\s*\{[^}]*note:`).MatchString(js) {
			t.Errorf("STATS_RANGE_MAP 的 %s 档缺 note 字段", k)
		}
	}

	// ⑤ setStatsRange 同时驱动统计侧与趋势侧
	i := strings.Index(js, "function setStatsRange(")
	if i < 0 {
		t.Fatal("找不到 setStatsRange —— 统一 setter 是这次改动的核心")
	}
	body := js[i:]
	if j := strings.Index(body, "\n  function "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "STATS_RANGE_MAP[") {
		t.Error("setStatsRange 没有查 STATS_RANGE_MAP —— 档位与后端参数必须走映射表")
	}
	if !strings.Contains(body, "loadTrendSeries(") {
		t.Error("setStatsRange 没有驱动趋势图（应调 loadTrendSeries）⇒ 切档只变卡片、图不变，用户要的正是两处一起变")
	}
	if !strings.Contains(body, "statsRangeNote") {
		t.Error("setStatsRange 没有更新 #statsRangeNote —— 24h 档的口径差异必须显式说明（不许显示假数字）")
	}
	// ⑤b 趋势加载确实打的是 series 端点
	if !strings.Contains(js, "/admin/stats/series?range=") {
		t.Error("趋势加载没有打 /admin/stats/series?range= —— 映射表的 series 参数没有落地")
	}
	// ⑤c 24h 档必须带那句口径提示（不许假装它等于统计侧的 24 小时）
	if !strings.Contains(js, "卡片按「今日」统计") {
		t.Error("24h 档缺少口径提示文案 —— /admin/stats 只有 today（本地 0 点起），与滚动 24 小时不是一回事")
	}
}
