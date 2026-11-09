package store

// log_daily_out_test.go —— T-387：`by_model_out`（纯输出 token）与 logs 的 cache 两列。
//
// # 为什么 by_model_out 必须单独一列
//
// ByModel 存的是 total（输入+输出），而用户看"消耗分布"时以为是"模型生成了多少字"。
// 实测差距极大：某条请求 total=37938 而输出仅 176（其余是缓存读取的上下文）。
// 两个语义混在一起等于撒谎 —— 所以按模型也要分 total / out 两份。
//
// # 为什么 logs 要补 cache 两列
//
// log_daily 有 cache_hit/cache_miss，但**回填段恒为 0**，因为 logs 表根本没存它们。
// 后果：升级前那 365 天的趋势里缓存命中率永远显示 0（用户会以为"以前完全没命中过"）。
// 补列后：历史行是 0 ⇒ 语义正确地表示"当时没记录"，而不是假装"命中率为 0"。

import (
	"path/filepath"
	"testing"
	"time"
)

func openOutTestDB(t *testing.T) {
	t.Helper()
	prev := DB
	if err := Open(filepath.Join(t.TempDir(), "out.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if DB != nil {
			if s, err := DB.DB(); err == nil {
				_ = s.Close()
			}
		}
		DB = prev
	})
}

// TestAppendLogDailyAccumulatesByModelOut seam 1：by_model_out 增量累加正确。
func TestAppendLogDailyAccumulatesByModelOut(t *testing.T) {
	openOutTestDB(t)
	day := "2026-11-10"
	// 两条同模型：total 各 30/10，输出各 20/10 ⇒ by_model_out 应为 30、by_model 为 40
	if err := AppendLogDaily(day, true, 30, 20, 0, 0, 0, "m"); err != nil {
		t.Fatalf("AppendLogDaily: %v", err)
	}
	if err := AppendLogDaily(day, true, 10, 10, 0, 0, 0, "m"); err != nil {
		t.Fatalf("AppendLogDaily: %v", err)
	}
	rows, err := LoadLogDaily(day, day)
	if err != nil || len(rows) != 1 {
		t.Fatalf("LoadLogDaily = %d 行 err=%v", len(rows), err)
	}
	if rows[0].ByModel != `{"m":40}` {
		t.Errorf("by_model = %s，want {\"m\":40}", rows[0].ByModel)
	}
	if rows[0].ByModelOut != `{"m":30}` {
		t.Errorf("by_model_out = %s，want {\"m\":30}", rows[0].ByModelOut)
	}
}

// TestBackfillAggregatesByModelOut seam 2：回填按模型聚合纯输出 token。
func TestBackfillAggregatesByModelOut(t *testing.T) {
	openOutTestDB(t)
	at := time.Now().AddDate(0, 0, -2)
	for _, out := range []int{50, 20} {
		if err := AppendLog(Log{
			CreatedAt: at.Unix(), ModelName: "m-bf",
			TotalTokens: 100, CompletionTokens: out, Success: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := BackfillLogDaily(); err != nil {
		t.Fatalf("BackfillLogDaily: %v", err)
	}
	day := DayOf(at)
	rows, _ := LoadLogDaily(day, day)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，want 1", len(rows))
	}
	if rows[0].ByModelOut != `{"m-bf":70}` {
		t.Errorf("by_model_out = %s，want {\"m-bf\":70}（50+20）", rows[0].ByModelOut)
	}
	if rows[0].ByModel != `{"m-bf":200}` {
		t.Errorf("by_model = %s，want {\"m-bf\":200}（100+100）", rows[0].ByModel)
	}
}

// TestBackfillReadsCacheColumns seam 4：logs 带 cache 值 ⇒ 回填后 log_daily 的 cache 也是它。
func TestBackfillReadsCacheColumns(t *testing.T) {
	openOutTestDB(t)
	at := time.Now().AddDate(0, 0, -1)
	if err := AppendLog(Log{
		CreatedAt: at.Unix(), ModelName: "m-c", TotalTokens: 10, CompletionTokens: 4,
		CacheHitTokens: 4, CacheMissTokens: 6, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := BackfillLogDaily(); err != nil {
		t.Fatalf("BackfillLogDaily: %v", err)
	}
	day := DayOf(at)
	rows, _ := LoadLogDaily(day, day)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d，want 1", len(rows))
	}
	if rows[0].CacheHit != 4 || rows[0].CacheMiss != 6 {
		t.Errorf("cache = %d/%d，want 4/6（logs 的 cache 列必须被回填读到）", rows[0].CacheHit, rows[0].CacheMiss)
	}
}

// TestMigrateAddsCacheColumnsToOldTable AutoMigrate 在**旧库**上补列（零迁移风险实测）。
func TestMigrateAddsCacheColumnsToOldTable(t *testing.T) {
	prev := DB
	path := filepath.Join(t.TempDir(), "old.db")
	if err := Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if DB != nil {
			if s, err := DB.DB(); err == nil {
				_ = s.Close()
			}
		}
		DB = prev
	})
	// 造一个**没有** cache 列的旧 logs 表（模拟 T-387 之前的库）
	if err := DB.Exec(`CREATE TABLE logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, created_at INTEGER NOT NULL DEFAULT 0,
		model_name TEXT NOT NULL DEFAULT '', channel_name TEXT NOT NULL DEFAULT '',
		account_uid TEXT NOT NULL DEFAULT '', prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
		cost_quota INTEGER NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0,
		success NUMERIC NOT NULL DEFAULT 0, err_kind TEXT NOT NULL DEFAULT '',
		type TEXT NOT NULL DEFAULT '')`).Error; err != nil {
		t.Fatalf("造旧表: %v", err)
	}
	if err := DB.Exec(`INSERT INTO logs (created_at, model_name, total_tokens, success) VALUES (?, 'old', 5, 1)`,
		time.Now().AddDate(0, 0, -3).Unix()).Error; err != nil {
		t.Fatalf("插旧行: %v", err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("Migrate（旧库上加列）: %v", err)
	}
	var cols []string
	DB.Raw(`SELECT name FROM pragma_table_info('logs')`).Scan(&cols)
	has := map[string]bool{}
	for _, c := range cols {
		has[c] = true
	}
	if !has["cache_hit_tokens"] || !has["cache_miss_tokens"] {
		t.Fatalf("Migrate 没给旧 logs 表补上 cache 列：%v", cols)
	}
	var hit, miss int
	DB.Raw(`SELECT cache_hit_tokens, cache_miss_tokens FROM logs LIMIT 1`).Row().Scan(&hit, &miss)
	if hit != 0 || miss != 0 {
		t.Errorf("旧行的 cache = %d/%d，want 0/0（老数据没有这个概念，不能凭空造值）", hit, miss)
	}
}
