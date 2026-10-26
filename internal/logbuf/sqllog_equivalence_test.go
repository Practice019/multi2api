// sqllog_equivalence_test.go 双写的**跨数据源一致性**门禁（PLAN §4 T06 的集成判据）。
//
// # 它验什么
//
// 同一批请求日志，两个数据源必须给出**同一组数字**：
//
//	jsonl（data/request-log.jsonl，手工口径：直接遍历条目）
//	  == SQL（logs 表，store.TodayAggregate / store.WindowAggregate）
//
// 允许 ±0：请求数、Token 总数、消费额、窗口请求数、窗口成功数逐位相等；
// 平均延迟是浮点商，留 1e-6 ms 的容差（SQLite 的 AVG 与 Go 的求和顺序可能差最后一位）。
//
// # 为什么用 package logbuf_test（外部测试包）
//
// 这样它**可以**同时 import logbuf 与 store，而生产代码里 logbuf 不必 import store
// （见 sqlsink.go 文件头）。下面的 entryToLog 就是装配层（§4 T11）要用的
// **参考映射**：T11 把它挪进 cmd/server 即可。
//
// # 为什么不调 logbuf.OpenSink 读文件
//
// OpenSink 会 Prune（重写整份文件）。这个测试是只读验收，**绝不能**碰真实数据文件，
// 所以这里用 os.ReadFile + encoding/json 直接解析。
package logbuf_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/store"
)

const logPath = "../../data/request-log.jsonl"

// readJSONL 只读解析真实日志文件（不存在或为空 ⇒ 调用方 skip）。
func readJSONL(t *testing.T) []logbuf.Entry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "data", "request-log.jsonl"))
	if err != nil {
		t.Skipf("跳过：读不到真实日志 %s（%v）", logPath, err)
	}
	var out []logbuf.Entry
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e logbuf.Entry
		if json.Unmarshal([]byte(line), &e) == nil && !e.At.IsZero() {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		t.Skip("跳过：真实日志里没有可用条目")
	}
	return out
}

// entryToLog 参考映射：logbuf.Entry → store.Log（T11 照抄这一份）。
//
//	At       → CreatedAt（Unix 秒，聚合的唯一时间轴）
//	Status   → Success（2xx/3xx 记成功；网关对客户端返回的状态码）
//	TotalMS  → LatencyMs（hyxapi 的 use_time 同口径：整请求耗时）
//	Tokens   → PromptTokens + CompletionTokens（Tokens 是 total，OutTokens 是 completion）
//	Credit   → CostQuota（本仓无计费，credit 就是"消耗了多少额度"；四舍五入到整数）
func entryToLog(e logbuf.Entry) store.Log {
	success := e.Status >= 200 && e.Status < 400
	typ := "error"
	if success {
		typ = "consume"
	}
	var prompt, completion, total int
	if e.Tokens >= 0 {
		total = e.Tokens
		completion = e.OutTokens
		if completion < 0 {
			completion = 0
		}
		if completion > total {
			completion = total // out > total 的脏数据不能让 prompt 变负
		}
		prompt = total - completion
	}
	return store.Log{
		CreatedAt:        e.At.Unix(),
		ModelName:        e.Model,
		ChannelName:      e.Channel,
		AccountUID:       e.UID,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		CostQuota:        int64(math.Round(e.Credit)),
		LatencyMs:        e.TotalMS,
		Success:          success,
		Type:             typ,
	}
}

// TestJSONLAndSQLAggregatesAgree 是本任务"两个数据源的数字对照"的可复现实现。
func TestJSONLAndSQLAggregatesAgree(t *testing.T) {
	entries := readJSONL(t)

	// 锚点 = 最新一条日志；"今日"= 它所在本地日的 0 点；窗口 = [anchor-5min, anchor]。
	anchor := entries[0].At
	for _, e := range entries {
		if e.At.After(anchor) {
			anchor = e.At
		}
	}
	local := anchor.Local()
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).Unix()
	windowTo := anchor.Unix()
	windowFrom := windowTo - 300

	// ── 手工口径：直接遍历 jsonl（独立实现，不用任何 SQL）────────────────
	var todayReqs, todayTokens, todayCost int64
	var winReqs, winSucc int64
	var winLatencySum int64
	var winLatencyN int64
	for _, e := range entries {
		at := e.At.Unix()
		if at >= dayStart {
			todayReqs++
			if e.Tokens > 0 {
				todayTokens += int64(e.Tokens)
			}
			todayCost += int64(math.Round(e.Credit))
		}
		if at >= windowFrom && at <= windowTo {
			winReqs++
			if e.Status >= 200 && e.Status < 400 {
				winSucc++
			}
			if e.TotalMS > 0 {
				winLatencySum += e.TotalMS
				winLatencyN++
			}
		}
	}
	var winAvg float64
	if winLatencyN > 0 {
		winAvg = float64(winLatencySum) / float64(winLatencyN)
	}

	// ── 同一批条目落进临时库（真实生产写入路径 store.AppendLog）─────────
	dir := t.TempDir()
	if err := store.Open(filepath.Join(dir, "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 只搬"最近 24h + 紧邻的 50 条更早记录"：两个聚合区间都在最近 24h 内，
	// 那 50 条用来验证"边界外的行确实被排除"。
	cut := anchor.Add(-24 * time.Hour).Unix()
	sorted := make([]logbuf.Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	inserted := 0
	older := 0
	for _, e := range sorted {
		if e.At.Unix() < cut {
			if older >= 50 {
				continue
			}
			older++
		}
		if err := store.AppendLog(entryToLog(e)); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
		inserted++
	}

	// ── SQL 侧 ─────────────────────────────────────────────────────────
	sqlReqs, sqlTokens, sqlCost, err := store.TodayAggregate(dayStart)
	if err != nil {
		t.Fatalf("TodayAggregate: %v", err)
	}
	sqlWinReqs, sqlWinSucc, sqlWinAvg, err := store.WindowAggregate(windowFrom, windowTo)
	if err != nil {
		t.Fatalf("WindowAggregate: %v", err)
	}

	t.Logf("数据源 1（jsonl 手工计数）: 今日 requests=%d tokens=%d cost=%d | 窗口 requests=%d success=%d avg=%.3fms",
		todayReqs, todayTokens, todayCost, winReqs, winSucc, winAvg)
	t.Logf("数据源 2（SQL 聚合）    : 今日 requests=%d tokens=%d cost=%d | 窗口 requests=%d success=%d avg=%.3fms",
		sqlReqs, sqlTokens, sqlCost, sqlWinReqs, sqlWinSucc, sqlWinAvg)
	t.Logf("落库：解析 %d 条，今日 %d 条，插入 %d 条（含 %d 条 24h 之前的边界样本）", len(entries), todayReqs, inserted, older)

	if todayReqs == 0 || winReqs == 0 {
		t.Fatalf("真实日志在测试区间内没有流量（today=%d window=%d）—— 样本不足以证明一致性", todayReqs, winReqs)
	}
	if sqlReqs != todayReqs || sqlTokens != todayTokens || sqlCost != todayCost {
		t.Errorf("今日聚合不一致（允许 ±0）：jsonl requests=%d tokens=%d cost=%d，SQL requests=%d tokens=%d cost=%d",
			todayReqs, todayTokens, todayCost, sqlReqs, sqlTokens, sqlCost)
	}
	if sqlWinReqs != winReqs || sqlWinSucc != winSucc {
		t.Errorf("窗口聚合不一致（允许 ±0）：jsonl requests=%d success=%d，SQL requests=%d success=%d",
			winReqs, winSucc, sqlWinReqs, sqlWinSucc)
	}
	if math.Abs(sqlWinAvg-winAvg) > 1e-6 {
		t.Errorf("窗口平均延迟不一致：jsonl=%.6fms，SQL=%.6fms", winAvg, sqlWinAvg)
	}
}
