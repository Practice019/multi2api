package admin

import (
	"testing"
	"time"
)

// ⚠ 本文件是 `?range=` 时间档的**单测**（2026-10-18 新建）。
//
// # 为什么先有它、后有实现
//
// 上一轮我把"档名归一 + 取边界"写在一个函数里、**没有单测就接线上线**，
// 结果出现"同一个 range 字符串时好时坏"的怪象，我只能靠反复试探去查 —— 查了四轮。
// ⚠ 而这几条断言，5 行就能把当时的问题抓住：
//
//	· 同一档必须**稳定**地得到同一个缓存键（第 1 组）
//	· 每档的起点必须**含今天**（第 2 组 —— 与 agent2api 的 range_bounds 口径一致）
//	· 未知档必须**不过滤**（第 3 组 —— 我们与它不同：它兜 "7"，我们保持"全部"）
func TestStatsRangeKeyIsStableAndNormalized(t *testing.T) {
	cases := []struct{ in, want string }{
		{"today", "today"},
		{"TODAY", "today"},     // ⚠ 大小写必须归一（上一轮的怪象之一）
		{"  Today  ", "today"}, // ⚠ 空白也要归一
		{"7", "7"},
		{" 7 ", "7"},
		{"30", "30"},
		{"month", "month"},
		{"", ""},
		{"all", "all"},
		{"bogus", "bogus"}, // ⚠ 未知值**不兜底**：它要参与缓存键，不同未知值不该互撞
	}
	for _, c := range cases {
		if got := statsRangeKey(c.in); got != c.want {
			t.Errorf("statsRangeKey(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

func TestStatsRangeFromBoundaries(t *testing.T) {
	now := time.Now()
	y, m, d := now.Date()
	todayMidnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())

	cases := []struct {
		key    string
		want   time.Time
		wantOK bool
	}{
		{"today", todayMidnight, true},
		// ⚠ **含今天**：7 档 = 今天往前 6 天（不是 7 天前）—— 与 agent2api 的口径一致
		{"7", todayMidnight.AddDate(0, 0, -6), true},
		{"30", todayMidnight.AddDate(0, 0, -29), true},
		{"month", time.Date(y, m, 1, 0, 0, 0, 0, now.Location()), true},
		// ⚠ 不过滤的四类：空 / all / 未知 / 未归一的（调用方应先用 statsRangeKey）
		{"", time.Time{}, false},
		{"all", time.Time{}, false},
		{"bogus", time.Time{}, false},
		{"TODAY", time.Time{}, false}, // ⚠ 未归一 ⇒ 不过滤（**这是刻意的**：归一在 statsRangeKey）
	}
	for _, c := range cases {
		got, ok := statsRangeFrom(c.key)
		if ok != c.wantOK {
			t.Errorf("statsRangeFrom(%q) ok = %v，想要 %v", c.key, ok, c.wantOK)
			continue
		}
		if ok && !got.Equal(c.want) {
			t.Errorf("statsRangeFrom(%q) = %v，想要 %v", c.key, got, c.want)
		}
	}
}

// TestStatsRange7IncludesToday 把"含今天"这条口径**单独钉一个断言**。
//
// ⚠ 为什么值得单独一条：这是最容易写错、也最难从界面上看出来的一处 ——
// 写成"7 天前"只差一天，但用户看到的数字会少一整天的量。
func TestStatsRange7IncludesToday(t *testing.T) {
	from, ok := statsRangeFrom("7")
	if !ok {
		t.Fatal("7 档应当过滤")
	}
	now := time.Now()
	// 从起点到现在，必须**覆盖满 7 个自然日**（今天往前 6 天到今天的 24 小时）
	if days := now.Sub(from).Hours() / 24; days < 6 || days > 7.01 {
		t.Errorf("7 档跨度 = %.2f 天，应当在 [6, 7.01] 之间（含今天 ⇒ 往前 6 天）", days)
	}
}
