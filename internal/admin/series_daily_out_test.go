package admin

// series_daily_out_test.go —— T-387 seam 5/6：seriesFromDaily 的 by_model_out 与 cache 语义。
//
// seam 6（语义护栏）：cache 两列都是 0 的桶 ⇒ 两个字段也保持 0，**不**把它当成"命中率 0%"。
// 老数据（logs 表还没有 cache 列的那些行）就是这种情况；前端据此判"该桶无缓存数据、不画点"。

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	sqlstore "workbuddy2api/internal/store"
)

func openOutAdminDB(t *testing.T) {
	t.Helper()
	prev := sqlstore.DB
	if err := sqlstore.Open(filepath.Join(t.TempDir(), "seriesout.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sqlstore.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlstore.DB != nil {
			if s, err := sqlstore.DB.DB(); err == nil {
				_ = s.Close()
			}
		}
		sqlstore.DB = prev
	})
}

// TestSeriesDailyByModelOutAndCache seam 5 + 6：三天数据（其中一天带 cache）⇒ 回填 ⇒ 逐天断言。
func TestSeriesDailyByModelOutAndCache(t *testing.T) {
	openOutAdminDB(t)
	now := time.Now()
	// 第 0 天（今天）：m-a total 100/out 30、m-b total 50/out 20，**带 cache** 4/6
	// 第 1 天：m-a total 60/out 10（老数据：cache 两列为 0）
	// 第 2 天：m-b total 20/out 5（同上）
	type seed struct {
		dayAgo     int
		model      string
		total, out int
		hit, miss  int
	}
	seeds := []seed{
		{0, "m-a", 100, 30, 4, 6},
		{0, "m-b", 50, 20, 0, 0},
		{1, "m-a", 60, 10, 0, 0},
		{2, "m-b", 20, 5, 0, 0},
	}
	for _, s := range seeds {
		at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -s.dayAgo)
		if err := sqlstore.AppendLog(sqlstore.Log{
			CreatedAt: at.Unix(), ModelName: s.model, TotalTokens: s.total, CompletionTokens: s.out,
			CacheHitTokens: s.hit, CacheMissTokens: s.miss, Success: true,
		}); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}
	if _, err := sqlstore.BackfillLogDaily(); err != nil {
		t.Fatalf("BackfillLogDaily: %v", err)
	}

	out, ok := (&Handler{}).seriesFromDaily(7*24*time.Hour, 24*time.Hour)
	if !ok {
		t.Fatal("seriesFromDaily 应命中（log_daily 非空）")
	}
	byDay := map[string]map[string]any{}
	for _, it := range out {
		var m map[string]any
		b, _ := json.Marshal(it)
		_ = json.Unmarshal(b, &m)
		byDay[sqlstore.DayOf(mustTime(t, it.TS))] = m
	}
	today := sqlstore.DayOf(now)
	yesterday := sqlstore.DayOf(now.AddDate(0, 0, -1))

	// seam 5：by_model_out 逐天正确
	if got := byDay[today]["by_model_out"]; got == nil {
		t.Fatalf("今天的桶缺 by_model_out（seam 5）")
	} else {
		m, _ := got.(map[string]any)
		if m["m-a"] != float64(30) || m["m-b"] != float64(20) {
			t.Errorf("今天的 by_model_out = %v，want m-a:30 m-b:20", m)
		}
	}
	if m, _ := byDay[today]["by_model"].(map[string]any); m["m-a"] != float64(100) {
		t.Errorf("今天的 by_model(m-a) = %v，want 100（total，与 out 分开）", m["m-a"])
	}
	if m, _ := byDay[yesterday]["by_model_out"].(map[string]any); m["m-a"] != float64(10) {
		t.Errorf("昨天的 by_model_out(m-a) = %v，want 10", m["m-a"])
	}

	// 带 cache 的那天：非 0
	if byDay[today]["cache_hit"] != float64(4) || byDay[today]["cache_miss"] != float64(6) {
		t.Errorf("今天的 cache = %v/%v，want 4/6", byDay[today]["cache_hit"], byDay[today]["cache_miss"])
	}
	// seam 6：老数据那天 cache 必须保持 0（不是被算成"命中率 0%"）
	if byDay[yesterday]["cache_hit"] != float64(0) || byDay[yesterday]["cache_miss"] != float64(0) {
		t.Errorf("昨天的 cache = %v/%v，want 0/0（老数据没有 cache 概念，不能假装算过）",
			byDay[yesterday]["cache_hit"], byDay[yesterday]["cache_miss"])
	}
	// 把三天的实际 JSON 打出来（这就是验收要的实测）
	for _, s := range seeds {
		d := sqlstore.DayOf(time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -s.dayAgo))
		b, _ := json.Marshal(byDay[d])
		t.Logf("%s ⇒ %s", d, string(b))
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("桶 ts 解析失败 %q: %v", s, err)
	}
	return ts
}
