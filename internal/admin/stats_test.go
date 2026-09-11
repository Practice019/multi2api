package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// 调用统计必须覆盖**全部历史**，而不是「进程启动至今」。
//
// 历史实现只读内存环形缓冲（上限 2000 条），于是：
//   - 重启后立刻显示「暂无请求」，即使落盘文件里有几千条；
//   - 窗口被限制在进程生命周期内，与用户以为的「全部历史」不符。
//
// 这组测试用「落盘文件里有数据 + 内存缓冲为空」来模拟重启后的场景 ——
// 这正是原实现失效的场景。

func newStatsHandler(t *testing.T, persisted []logbuf.Entry) (*Handler, *logbuf.Ring) {
	t.Helper()
	sink, err := logbuf.OpenSink(filepath.Join(t.TempDir(), "r.jsonl"), 7, 0)
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	t.Cleanup(func() { sink.Close() })
	for _, e := range persisted {
		sink.Append(e)
	}
	ring := logbuf.New(4) // 故意很小的内存缓冲
	ring.SetSink(sink)
	return New(Config{Ring: ring}), ring
}

func doStats(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return out
}

func persistedEntries(n int) []logbuf.Entry {
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	out := make([]logbuf.Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, logbuf.Entry{
			Seq:     int64(i + 1),
			At:      base.Add(time.Duration(i) * time.Minute),
			Model:   "deepseek-v4",
			Mode:    "stream",
			Status:  200,
			UID:     "u1",
			TTFBMS:  100,
			Tokens:  10,
			TotalMS: 1000,
		})
	}
	return out
}

// 核心场景：内存缓冲为空但落盘有 50 条 → 统计应为 50，而不是 0/「暂无请求」。
func TestStatsCoversPersistedHistory(t *testing.T) {
	h, _ := newStatsHandler(t, persistedEntries(50))

	out := doStats(t, h)
	if got := out["total"].(float64); got != 50 {
		t.Errorf("total=%v，期望 50（落盘历史应被统计，不能因内存为空就报 0）", got)
	}
	if got := out["ok"].(float64); got != 50 {
		t.Errorf("ok=%v，期望 50", got)
	}
	if got := out["source"].(string); got != "file" {
		t.Errorf("source=%v，期望 file（说明数据来自落盘）", got)
	}
	if got := out["tokens"].(float64); got != 500 {
		t.Errorf("tokens=%v，期望 500（50×10）", got)
	}
}

// 超出内存缓冲上限的历史同样要被统计 —— 这是原实现最明显的缺口。
func TestStatsNotLimitedByRingCapacity(t *testing.T) {
	h, ring := newStatsHandler(t, persistedEntries(300))
	if ring.Cap() >= 300 {
		t.Fatalf("测试前提不成立：期望内存容量(%d)小于数据量(300)", ring.Cap())
	}
	out := doStats(t, h)
	if got := out["total"].(float64); got != 300 {
		t.Errorf("total=%v，期望 300（不受内存容量 %d 限制）", got, ring.Cap())
	}
}

// 窗口时间应覆盖落盘首末条，而不是「进程启动至今」。
func TestStatsWindowFromPersistedRange(t *testing.T) {
	entries := persistedEntries(3)
	h, _ := newStatsHandler(t, entries)
	out := doStats(t, h)

	from, ok := out["window_from"].(string)
	if !ok {
		t.Fatal("缺少 window_from")
	}
	got, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		t.Fatalf("解析 window_from: %v", err)
	}
	if !got.Equal(entries[0].At) {
		t.Errorf("window_from=%v，期望落盘首条时间 %v", got, entries[0].At)
	}
}

// 落盘为空时应回落到内存缓冲，并如实标注来源，而不是硬报 0。
//
// 注意构造方式：不能「先 SetSink 再 Push」——Ring.Push 会写穿到文件，
// 那样文件就不空了，source=file 反而是正确行为（这一点最初写错，测试先报红）。
// 这里让 Ring 不接 Sink，模拟「落盘未启用/文件为空」但内存有数据的场景。
func TestStatsFallsBackToMemoryWhenFileEmpty(t *testing.T) {
	ring := logbuf.New(8)
	ring.Push(logbuf.Entry{At: time.Now(), Model: "m", Mode: "stream", Status: 200, TotalMS: 5})
	h := New(Config{Ring: ring})

	out := doStats(t, h)
	if got := out["total"].(float64); got != 1 {
		t.Errorf("total=%v，期望 1（回落内存缓冲）", got)
	}
	if got := out["source"].(string); got != "memory" {
		t.Errorf("source=%v，期望 memory", got)
	}
}

// 没有 Ring 时（配置缺失）应优雅降级，不能 panic。
func TestStatsWithoutRing(t *testing.T) {
	h := New(Config{})
	out := doStats(t, h)
	if got := out["total"].(float64); got != 0 {
		t.Errorf("total=%v，期望 0", got)
	}
	if got := out["source"].(string); got != "none" {
		t.Errorf("source=%v，期望 none", got)
	}
}

// 纯聚合函数的算术验收（不依赖 IO，便于快速定位算术问题）。
func TestAggregateChatLogArithmetic(t *testing.T) {
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	items := []logbuf.Entry{
		{At: base, Model: "a", Status: 200, UID: "u1", TTFBMS: 100, Tokens: 10, TotalMS: 1000},
		{At: base.Add(time.Minute), Model: "a", Status: 200, UID: "u2", TTFBMS: 300, Tokens: 20, TotalMS: 2000},
		{At: base.Add(2 * time.Minute), Model: "b", Status: 500, UID: "u1", TTFBMS: 0, Tokens: -1, TotalMS: 3000},
	}
	out := aggregateChatLog(items)

	if out["total"].(int) != 3 {
		t.Errorf("total=%v", out["total"])
	}
	if out["ok"].(int64) != 2 {
		t.Errorf("ok=%v，期望 2", out["ok"])
	}
	if out["fail"].(int64) != 1 {
		t.Errorf("fail=%v，期望 1", out["fail"])
	}
	// tokens 只累加 >0 的（-1 表示 usage 缺失，不能当 0 累加也别当负数扣）
	if out["tokens"].(int64) != 30 {
		t.Errorf("tokens=%v，期望 30（跳过 -1）", out["tokens"])
	}
	// 平均 TTFB 只按有 TTFB 的样本算：(100+300)/2
	if out["avg_ttfb_ms"].(int64) != 200 {
		t.Errorf("avg_ttfb_ms=%v，期望 200", out["avg_ttfb_ms"])
	}
	// 平均总耗时按全部样本算：(1000+2000+3000)/3
	if out["avg_total_ms"].(int64) != 2000 {
		t.Errorf("avg_total_ms=%v，期望 2000", out["avg_total_ms"])
	}
	byModel := out["by_model"].(map[string]int)
	if byModel["a"] != 2 || byModel["b"] != 1 {
		t.Errorf("by_model=%v", byModel)
	}
	byUID := out["by_uid"].(map[string]int)
	if byUID["u1"] != 2 || byUID["u2"] != 1 {
		t.Errorf("by_uid=%v", byUID)
	}
}

func TestAggregateChatLogEmpty(t *testing.T) {
	out := aggregateChatLog(nil)
	if out["total"].(int) != 0 {
		t.Errorf("total=%v", out["total"])
	}
	// 空集不应产生平均值为 0 的误导性字段
	if _, ok := out["avg_total_ms"]; ok {
		t.Error("空集不应输出 avg_total_ms")
	}
	if _, ok := out["avg_ttfb_ms"]; ok {
		t.Error("空集不应输出 avg_ttfb_ms")
	}
	if _, ok := out["window_from"]; ok {
		t.Error("空集不应输出 window_from")
	}
}
