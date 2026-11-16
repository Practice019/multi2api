// sqlcollapse.go 把 GORM 的多行 SQL 日志压成**一行结构化事件**。
//
// # 它解决什么（用户 2026-11-16 的实测启动日志）
//
// GORM 的慢查询日志长这样 —— 一次 **12 行**：
//
//	10:00:45 WARN  sql  workbuddy2api/internal/store/log_daily.go:168 SLOW SQL >= 200ms
//	[276.461ms] [rows:9] INSERT INTO log_daily
//	                        (day, total, ok, fail, tokens, cost, cache_hit, cache_miss, by_model)
//	                SELECT date(created_at,'unixepoch','localtime') AS day,
//	                       COUNT(*),
//	                       …（还有 8 行）
//
// 而这些信息压成一行完全够用：
//
//	10:00:45 WARN  sql  sql.slow threshold_ms=200 elapsed_ms=276 rows=9 sql="INSERT INTO log_daily (day, total, …) SELECT date(created_at,'unixepoch','localtime') AS day, COUNT(*), …"
//
// 为什么可以压：
//
//   - `SLOW SQL >= 200ms` 是**种类标记**，不是数据 ⇒ 变成事件名 `sql.slow` + `threshold_ms=`
//   - `[276.461ms]` 是耗时 ⇒ `elapsed_ms=276`
//   - `[rows:9]` 是影响行数 ⇒ `rows=9`
//   - 剩下的 SQL 本来就有换行与缩进（GORM 排版用）⇒ 压平空白 + 限长
//
// ⚠ 限长必须**按 rune**（`ShortString` 已经是），否则会把多字节字符切开。
package logx

import (
	"regexp"
	"strings"
)

var (
	// sqlElapsed GORM 的耗时标记 `[276.461ms]`。
	sqlElapsed = regexp.MustCompile(`\[\s*([0-9.]+)\s*ms\s*\]`)
	// sqlRows GORM 的影响行数 `[rows:9]`。
	sqlRows = regexp.MustCompile(`\[\s*rows:\s*(\d+)\s*\]`)
	// sqlThreshold `SLOW SQL >= 200ms` 里的阈值。
	sqlThreshold = regexp.MustCompile(`>=\s*(\d+)\s*ms`)
	// sqlMaxRunes SQL 文本保留多少字符。
	sqlMaxRunes = 160
)

// collapseSQL 把 GORM 的多行消息压成 `事件名 + 字段` 的单行正文。
//
// 输入是 `logger.Writer.Printf` 收到的**完整消息**（可能 12 行）。
// 输出是**不含**时间/级别/作用域三段的正文。
func collapseSQL(msg string) string {
	s := strings.ReplaceAll(msg, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return "sql.log"
	}

	// 第一行通常是种类标记；也可能直接就是 `[…] [rows:…] SQL…`。
	first := strings.TrimSpace(lines[0])
	rest := strings.Join(lines[1:], " ")

	event := "sql.log"
	kv := make([]any, 0, 8)

	switch {
	case strings.HasPrefix(first, "SLOW SQL"):
		event = "sql.slow"
		if m := sqlThreshold.FindStringSubmatch(first); m != nil {
			kv = append(kv, "threshold_ms", m[1])
		}
	case strings.HasPrefix(first, "[error]"):
		event = "sql.error"
		rest = strings.TrimSpace(strings.TrimPrefix(first, "[error]")) + " " + rest
	case strings.HasPrefix(first, "["):
		// 没有种类行：首行就是 `[276.461ms] [rows:9] INSERT …`
		event = "sql.slow"
		rest = first + " " + rest
	default:
		// 未知形态：把首行也当正文的一部分，不丢信息。
		rest = first + " " + rest
	}

	// 从正文里抽出耗时与行数，其余是 SQL 本身。
	if m := sqlElapsed.FindStringSubmatch(rest); m != nil {
		kv = append(kv, "elapsed_ms", m[1])
	}
	if m := sqlRows.FindStringSubmatch(rest); m != nil {
		kv = append(kv, "rows", m[1])
	}
	body := sqlElapsed.ReplaceAllString(rest, "")
	body = sqlRows.ReplaceAllString(body, "")
	// 压平所有空白（GORM 用大量缩进排版，压平后 SQL 仍然可读）。
	body = strings.Join(strings.Fields(body), " ")
	// 去掉 GORM 塞进来的调用点前缀（`path/file.go:168`）—— 它已经在 scope 之外
	// 没有额外价值，而且会让每条 SQL 行都以一长串路径开头。
	if at := strings.Index(body, ".go:"); at >= 0 && at < 80 {
		if sp := strings.IndexByte(body[at:], ' '); sp > 0 {
			body = strings.TrimSpace(body[at+sp:])
		}
	}
	if body != "" {
		kv = append(kv, "sql", ShortString(body, sqlMaxRunes))
	}
	return eventLine(event, kv)
}
