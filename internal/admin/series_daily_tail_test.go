package admin

// series_daily_tail_test.go —— T-394 ①：日聚合路径（365d/30d/7d）**也不再裁尾部空桶**。
//
// # 背景：T-389 只修了一半
//
// 上一轮我删掉了明细路径（statsSeries 主体）的"去掉尾部全空桶"，但 seriesFromDaily 里
// 还留着同样 4 行。后果（主脑实测）：`range=365d` 的尾桶停在 11/08，而"现在"是 11/09
// ⇒ 前端 renderHeatmap 用 list[len-1].ts 当最后一天往前推 364 天 ⇒ **今天永远不显示**。
//
// 钉住两条：
//  ① 365d：尾桶 ts 必须是**今天**（哪怕今天还没产生日志）
//  ② 7d 且最近 2 天无数据：桶数不缩、尾桶仍是今天

import (
	"testing"
	"time"

	sqlstore "workbuddy2api/internal/store"
)

// seedDays 造 daysAgo 天的日志（**故意不含今天**，否则裁尾行为看不出来）。
func seedDaysAgo(t *testing.T, daysAgo ...int) {
	t.Helper()
	now := time.Now()
	for _, d := range daysAgo {
		at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -d)
		if err := sqlstore.AppendLog(sqlstore.Log{
			CreatedAt: at.Unix(), ModelName: "m-tail", TotalTokens: 4, CompletionTokens: 2, Success: true,
		}); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}
	if _, err := sqlstore.BackfillLogDaily(); err != nil {
		t.Fatalf("BackfillLogDaily: %v", err)
	}
}

// TestSeriesDaily365KeepsToday ① 365d 尾桶必须是今天。
func TestSeriesDaily365KeepsToday(t *testing.T) {
	openOutAdminDB(t)
	seedDaysAgo(t, 1, 2, 3) // 今天**没有**日志

	out, ok := (&Handler{}).seriesFromDaily(365*24*time.Hour, 24*time.Hour)
	if !ok {
		t.Fatal("seriesFromDaily 应命中（log_daily 非空）")
	}
	if len(out) < 365 {
		t.Errorf("365d 桶数 = %d，want ≥365", len(out))
	}
	last, err := time.Parse(time.RFC3339, out[len(out)-1].TS)
	if err != nil {
		t.Fatalf("尾桶 ts 解析失败 %q", out[len(out)-1].TS)
	}
	today := sqlstore.DayOf(time.Now())
	if got := sqlstore.DayOf(last); got != today {
		t.Errorf("365d 尾桶 = %s，want **今天** %s ⇒ 今天那一格又被裁掉了（前端热力图最后一列会停在昨天）",
			got, today)
	}
	// 今天没数据 ⇒ 尾桶 total 应为 0（Truth：今天还没有请求），而不是"这一格不存在"
	if out[len(out)-1].Total != 0 {
		t.Errorf("今天无日志 ⇒ 尾桶 total 应为 0，得到 %d", out[len(out)-1].Total)
	}
	t.Logf("365d 桶数 = %d，尾桶 %s（今天 %s，total=%d）",
		len(out), out[len(out)-1].TS, today, out[len(out)-1].Total)
}

// TestSeriesDaily7dKeepsEmptyTail ② 7d 且最近 2 天无数据 ⇒ 桶数不缩、尾桶仍是今天。
func TestSeriesDaily7dKeepsEmptyTail(t *testing.T) {
	openOutAdminDB(t)
	seedDaysAgo(t, 3, 4, 5, 6) // 最近 2 天（今天、昨天）都没有日志

	out, ok := (&Handler{}).seriesFromDaily(7*24*time.Hour, 24*time.Hour)
	if !ok {
		t.Fatal("seriesFromDaily 应命中")
	}
	// 7d 窗口 ⇒ 8 个桶（含边界）；关键是**不被裁成只剩有数据的 3 个**
	if len(out) != 8 {
		t.Errorf("7d 桶数 = %d，want 8（原来会被裁到只剩有数据的几天）", len(out))
	}
	last, _ := time.Parse(time.RFC3339, out[len(out)-1].TS)
	today := sqlstore.DayOf(time.Now())
	if got := sqlstore.DayOf(last); got != today {
		t.Errorf("7d 尾桶 = %s，want 今天 %s", got, today)
	}
	// 尾部两格是空的（昨天 / 今天）—— 这是**正确**的：右侧留白好过右端不是现在
	empty := 0
	for _, it := range out {
		if it.Total == 0 {
			empty++
		}
	}
	// 空桶应当是 4 个：7d 窗口的**边界桶**（now-7d，本来就没数据）+ 第 2 天 + 昨天 + 今天。
	// （我第一版写 2 是漏算了边界桶与第 2 天 —— 造数据只造了第 3~6 天。）
	if empty != 4 {
		t.Errorf("空桶数 = %d，want 4（边界桶 + 第2天 + 昨天 + 今天）", empty)
	}
	t.Logf("7d 桶数 = %d，尾桶 %s（今天 %s），空桶 %d 个", len(out), out[len(out)-1].TS, today, empty)
}
