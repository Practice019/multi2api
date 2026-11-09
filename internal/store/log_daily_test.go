package store

// log_daily_test.go —— T-386 的 seam 测试（包内，只碰公共边界：DB + 三个函数）。
//
// 四条 seam：
//  ① 增量累加：一条日志 ⇒ log_daily 的计数/缓存/ByModel 都对
//  ② 回填幂等：连跑两次 ⇒ 行数不变、数值不翻倍
//  ③ 裁剪只删 logs、不删 log_daily
//  ④ 400 天历史 ⇒ 回填后能按天区间读出（365d 热力图的数据基础）

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// openDailyTestDB 开一个临时库并建表（用公开 API，不碰内部辅助）。
func openDailyTestDB(t *testing.T) {
	t.Helper()
	prev := DB
	if err := Open(filepath.Join(t.TempDir(), "daily.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if DB != nil {
			if s, err := DB.DB(); err == nil {
				_ = s.Close() // ⚠ Windows 上不关连接，t.TempDir 的清理会因文件被占用而失败
			}
		}
		DB = prev
	})
}

// TestAppendLogDailyAccumulates 一条日志 ⇒ 当天那行的每一列都对。
func TestAppendLogDailyAccumulates(t *testing.T) {
	openDailyTestDB(t)
	day := "2026-11-09"

	// 成功一条：10 token、命中 4、未命中 6、成本 1.5、模型 m-a
	if err := AppendLogDaily(day, true, 10, 0, 4, 6, 1.5, "m-a"); err != nil {
		t.Fatalf("AppendLogDaily: %v", err)
	}
	// 失败一条：0 token（无 usage）、模型 m-b
	if err := AppendLogDaily(day, false, 0, 0, 0, 0, 0, "m-b"); err != nil {
		t.Fatalf("AppendLogDaily: %v", err)
	}
	// 再来一条同模型，验证 ByModel 是**累加**而不是覆盖
	if err := AppendLogDaily(day, true, 5, 0, 0, 0, 0.5, "m-a"); err != nil {
		t.Fatalf("AppendLogDaily: %v", err)
	}

	rows, err := LoadLogDaily(day, day)
	if err != nil || len(rows) != 1 {
		t.Fatalf("LoadLogDaily = %d 行 err=%v，want 1 行", len(rows), err)
	}
	r := rows[0]
	if r.Total != 3 || r.OK != 2 || r.Fail != 1 {
		t.Errorf("计数 = total %d / ok %d / fail %d，want 3/2/1", r.Total, r.OK, r.Fail)
	}
	if r.Tokens != 15 {
		t.Errorf("tokens = %d，want 15（10+0+5）", r.Tokens)
	}
	if r.Cost != 2.0 {
		t.Errorf("cost = %v，want 2.0（1.5+0+0.5）", r.Cost)
	}
	if r.CacheHit != 4 || r.CacheMiss != 6 {
		t.Errorf("cache = %d/%d，want 4/6", r.CacheHit, r.CacheMiss)
	}
	// ByModel：m-a 累计 15、m-b 0（token 为 0 的模型不入表 —— 它不贡献用量）
	if want := `{"m-a":15}`; r.ByModel != want {
		t.Errorf("by_model = %s，want %s", r.ByModel, want)
	}
}

// TestBackfillLogDailyIsIdempotent 回填幂等：连跑两次 ⇒ 行数不变、数值不翻倍。
func TestBackfillLogDailyIsIdempotent(t *testing.T) {
	openDailyTestDB(t)
	// 锚到**当天零点**再往前推 —— 直接用 time.Now() 会让 base..base+4h 在晚间跨过
	// 本地午夜，5 条日志落进两个自然日，"want 1 天"就随运行时刻随机变红。
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -3)
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		if err := AppendLog(Log{CreatedAt: at.Unix(), ModelName: "m-x", TotalTokens: 7, Success: true, CostQuota: 2}); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}

	first, err := BackfillLogDaily()
	if err != nil || first != 1 {
		t.Fatalf("第一次回填 = %d 天 err=%v，want 1 天", first, err)
	}
	day := DayOf(base)
	a, _ := LoadLogDaily(day, day)
	second, err := BackfillLogDaily()
	if err != nil || second != 1 {
		t.Fatalf("第二次回填 = %d 天 err=%v（行数不该变）", second, err)
	}
	b, _ := LoadLogDaily(day, day)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("行数 = %d → %d，want 1 → 1", len(a), len(b))
	}
	if a[0].Total != 5 || b[0].Total != 5 {
		t.Errorf("total = %d → %d，want 5 → 5（回填是覆盖式取真相，不是相加）", a[0].Total, b[0].Total)
	}
	if b[0].Tokens != 35 {
		t.Errorf("tokens = %d，want 35（5 条 × 7）", b[0].Tokens)
	}
}

// TestPruneLogsKeepsDaily 裁剪只删 logs 明细，**绝不碰** log_daily。
func TestPruneLogsKeepsDaily(t *testing.T) {
	openDailyTestDB(t)
	old := time.Now().AddDate(0, 0, -100)
	day := DayOf(old)
	if err := AppendLog(Log{CreatedAt: old.Unix(), ModelName: "m-old", TotalTokens: 3, Success: true}); err != nil {
		t.Fatal(err)
	}
	if err := AppendLogDaily(day, true, 3, 0, 0, 0, 0, "m-old"); err != nil {
		t.Fatal(err)
	}

	n, err := PruneLogs(30)
	if err != nil {
		t.Fatalf("PruneLogs: %v", err)
	}
	if n != 1 {
		t.Errorf("删除行数 = %d，want 1（100 天前的那条）", n)
	}
	var logsLeft int64
	DB.Raw(`SELECT COUNT(*) FROM logs`).Scan(&logsLeft)
	if logsLeft != 0 {
		t.Errorf("logs 剩余 %d 行，want 0", logsLeft)
	}
	rows, err := LoadLogDaily(day, day)
	if err != nil || len(rows) != 1 {
		t.Fatalf("log_daily 被裁掉了 ✗（rows=%d err=%v）—— 日聚合是历史不变量，必须永久保留", len(rows), err)
	}
	if rows[0].Tokens != 3 {
		t.Errorf("log_daily.tokens = %d，want 3", rows[0].Tokens)
	}
	// 保留期下限：传 1 天也要按 7 天处理（排障至少一周）⇒ 6 天前的行不该被删
	keep := time.Now().AddDate(0, 0, -6)
	if err := AppendLog(Log{CreatedAt: keep.Unix(), Success: true}); err != nil {
		t.Fatal(err)
	}
	if n2, _ := PruneLogs(1); n2 != 0 {
		t.Errorf("PruneLogs(1) 删了 %d 行，want 0（下限 7 天）", n2)
	}
}

// TestBackfill400DaysReadable 400 天历史 ⇒ 回填后区间查询能读到最早与最晚那天。
func TestBackfill400DaysReadable(t *testing.T) {
	openDailyTestDB(t)
	oldest := time.Now().AddDate(0, 0, -400)
	for i := 0; i < 400; i++ {
		at := oldest.AddDate(0, 0, i)
		if err := AppendLog(Log{CreatedAt: at.Unix(), ModelName: "m", TotalTokens: 1, Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	days, err := BackfillLogDaily()
	if err != nil {
		t.Fatalf("BackfillLogDaily: %v", err)
	}
	if days < 400 {
		t.Errorf("回填 %d 天，want ≥400", days)
	}
	rows, err := LoadLogDaily(DayOf(oldest), DayOf(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 400 {
		t.Fatalf("区间读出 %d 天，want ≥400", len(rows))
	}
	if rows[0].Total != 1 || rows[len(rows)-1].Total != 1 {
		t.Errorf("首/尾桶 total = %d / %d，want 1 / 1（每一天都要有数）", rows[0].Total, rows[len(rows)-1].Total)
	}
	t.Logf("回填 %d 天；首桶 %s(total=%d) … 尾桶 %s(total=%d)",
		days, rows[0].Day, rows[0].Total, rows[len(rows)-1].Day, rows[len(rows)-1].Total)
	_ = fmt.Sprint()
}
