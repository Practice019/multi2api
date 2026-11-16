// sqlcollapse_test.go GORM 多行 SQL 日志压平的判据。
//
// # 用户实测（2026-11-16 启动日志）
//
// 一次慢查询打了 **12 行**，其中 11 行是 GORM 用缩进排版的 SQL：
//
//	10:00:45 WARN  sql  … SLOW SQL >= 200ms
//	[276.461ms] [rows:9] INSERT INTO log_daily
//	                        (day, total, ok, fail, tokens, cost, cache_hit, cache_miss, by_model)
//	                SELECT date(created_at,'unixepoch','localtime') AS day,
//	                       …（还有 8 行）
//
// 压成一行后信息一个不少：
//
//	sql.slow threshold_ms=200 elapsed_ms=276.461 rows=9 sql="INSERT INTO log_daily …"
package logx

import (
	"bytes"
	"strings"
	"testing"
)

// captureSQL 把一段 GORM 消息过一遍 PrintfWriter，返回日志输出。
func captureSQL(msg string) string {
	var buf bytes.Buffer
	restore := CaptureTo(&buf)
	defer restore()
	SQLWriter(LevelWarn).Printf("%s", msg)
	return buf.String()
}

// ★ ① 12 行的慢查询日志必须只剩 1 行。
func TestCollapseSQLIsOneLine(t *testing.T) {
	out := captureSQL("SLOW SQL >= 200ms\n" +
		"[276.461ms] [rows:9] INSERT INTO log_daily\n" +
		"                        (day, total, ok, fail, tokens, cost, cache_hit, cache_miss, by_model)\n" +
		"                SELECT date(created_at,'unixepoch','localtime') AS day,\n" +
		"                       COUNT(*),\n" +
		"                       SUM(CASE WHEN success THEN 1 ELSE 0 END),\n" +
		"                       COALESCE(SUM(total_tokens), 0),\n" +
		"                       ''\n" +
		"                FROM logs\n" +
		"                GROUP BY day\n" +
		"                ON CONFLICT(day) DO UPDATE SET\n" +
		"                        total      = excluded.total")
	body := strings.TrimRight(out, "\n")
	if n := strings.Count(body, "\n"); n != 0 {
		t.Fatalf("★ 压平后仍打出 %d 行（应 1 行）：\n%s", n+1, out)
	}
}

// ★ ② 关键数据一个不许丢：种类、阈值、耗时、行数、SQL 本体。
func TestCollapseSQLKeepsFacts(t *testing.T) {
	out := captureSQL("SLOW SQL >= 200ms\n[276.461ms] [rows:9] INSERT INTO log_daily (day) SELECT 1")
	for _, want := range []string{"sql.slow", "threshold_ms=200", "elapsed_ms=276.461", "rows=9", "INSERT INTO log_daily"} {
		if !strings.Contains(out, want) {
			t.Fatalf("★ 压平丢了 %q：\n%s", want, out)
		}
	}
}

// ③ SQL 文本限长，且限长要按 rune（不能把多字节汉字切开）。
func TestCollapseSQLTruncatesByRune(t *testing.T) {
	long := strings.Repeat("删", 400)
	out := captureSQL("SLOW SQL >= 200ms\n[1ms] [rows:1] DELETE FROM t WHERE x = '" + long + "'")
	if strings.Contains(out, "\ufffd") {
		t.Fatalf("★ 出现替换字符（按字节截断了多字节字符）：\n%s", out)
	}
	if n := len([]rune(out)); n > 320 {
		t.Fatalf("压平后 %d 字符，太长（SQL 应限长 %d）：\n%s", n, sqlMaxRunes, out)
	}
}

// ④ 错误类消息也必须能压平（GORM 的 [error] 形态）。
func TestCollapseSQLErrorShape(t *testing.T) {
	out := captureSQL("[error] UNIQUE constraint failed: logs.id\n[1.2ms] [rows:0] INSERT INTO logs (id) VALUES (1)")
	if !strings.Contains(out, "sql.error") {
		t.Fatalf("错误类应识别成 sql.error：\n%s", out)
	}
	if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
		t.Fatalf("错误类也必须是 1 行：\n%s", out)
	}
}

// ⑤ 没有种类首行的形态（少见）也不能丢信息。
func TestCollapseSQLBareShape(t *testing.T) {
	out := captureSQL("[12.5ms] [rows:3] SELECT * FROM logs")
	if !strings.Contains(out, "elapsed_ms=12.5") || !strings.Contains(out, "rows=3") {
		t.Fatalf("裸形态应仍能抽出耗时与行数：\n%s", out)
	}
}
