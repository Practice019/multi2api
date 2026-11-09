package main

// logwriter_daily_test.go —— T-387 seam 3：`sqlLogWriter.WriteLog` 的两条落库路径都带 cache 值。
//
// 为什么要单独测这一层：cache 两列是**新加**的，而 writer 是唯一的生产写入点。
// 只测 store 的 AppendLog 不能证明"生产路径真的把值传下去了"—— 中间那一跳最容易漏。

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
	sqlstore "workbuddy2api/internal/store"
)

// TestSQLWriterPersistsCacheAndDailyOut 写完一次 ⇒ logs 的 cache 两列有值、log_daily 的 by_model_out 有值。
func TestSQLWriterPersistsCacheAndDailyOut(t *testing.T) {
	prev := sqlstore.DB
	if err := sqlstore.Open(filepath.Join(t.TempDir(), "writer.db")); err != nil {
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

	e := logbuf.Entry{
		At: time.Now(), Status: 200, Model: "m-w", Channel: "ch", UID: "u1",
		Tokens: 10, OutTokens: 4, CacheHitTokens: 4, CacheMissTokens: 6, Credit: 1,
	}
	if err := (sqlLogWriter{}).WriteLog(e); err != nil {
		t.Fatalf("WriteLog: %v", err)
	}

	var row struct {
		CacheHitTokens   int
		CacheMissTokens  int
		CompletionTokens int
	}
	if err := sqlstore.DB.Raw(`SELECT cache_hit_tokens, cache_miss_tokens, completion_tokens
		FROM logs ORDER BY id DESC LIMIT 1`).Scan(&row).Error; err != nil {
		t.Fatalf("读 logs: %v", err)
	}
	if row.CacheHitTokens != 4 || row.CacheMissTokens != 6 {
		t.Errorf("logs.cache = %d/%d，want 4/6（seam 3：writer 必须把 cache 值落库）",
			row.CacheHitTokens, row.CacheMissTokens)
	}
	if row.CompletionTokens != 4 {
		t.Errorf("logs.completion = %d，want 4", row.CompletionTokens)
	}

	// 同一次写入也要顺手累加当天那一行（T-386 的增量路径）
	day := sqlstore.DayOf(e.At)
	rows, err := sqlstore.LoadLogDaily(day, day)
	if err != nil || len(rows) != 1 {
		t.Fatalf("log_daily = %d 行 err=%v，want 1（writer 应顺手累加）", len(rows), err)
	}
	if rows[0].ByModelOut != `{"m-w":4}` {
		t.Errorf("log_daily.by_model_out = %s，want {\"m-w\":4}", rows[0].ByModelOut)
	}
	if rows[0].CacheHit != 4 || rows[0].CacheMiss != 6 {
		t.Errorf("log_daily.cache = %d/%d，want 4/6", rows[0].CacheHit, rows[0].CacheMiss)
	}
}

// TestSQLWriterNegativeCacheIsZero 负数的 cache 值按 0 落库（上游异常值不能进聚合）。
func TestSQLWriterNegativeCacheIsZero(t *testing.T) {
	prev := sqlstore.DB
	if err := sqlstore.Open(filepath.Join(t.TempDir(), "writer2.db")); err != nil {
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
	e := logbuf.Entry{At: time.Now(), Status: 200, Model: "m-neg", Tokens: 5, OutTokens: 2,
		CacheHitTokens: -1, CacheMissTokens: -3, Credit: 0}
	if err := (sqlLogWriter{}).WriteLog(e); err != nil {
		t.Fatalf("WriteLog: %v", err)
	}
	var row struct{ CacheHitTokens, CacheMissTokens int }
	sqlstore.DB.Raw(`SELECT cache_hit_tokens, cache_miss_tokens FROM logs ORDER BY id DESC LIMIT 1`).Scan(&row)
	if row.CacheHitTokens != 0 || row.CacheMissTokens != 0 {
		t.Errorf("logs.cache = %d/%d，want 0/0（负数是上游异常，归 0）", row.CacheHitTokens, row.CacheMissTokens)
	}
}
