package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/upstream"
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

// 统计必须覆盖**超过分页上界**的历史，而不是被 Page 的 MaxPageSize 截断。
//
// 为什么单独加这条：上面的 TestStatsNotLimitedByRingCapacity 恰好用了 300 条，
// 而 300 正是后来给「每页条数」加的服务端上界（logbuf.MaxPageSize）。
// 于是它同时满足「> ring.Cap()」和「<= MaxPageSize」，两种情况都通过 ——
// 完全掩盖了「统计被静默截断到 300」这个 bug（线上实测 1855 行只统计了 300）。
//
// 教训：边界测试要**跨过**被怀疑的那个边界值，而不是落在它上面。
func TestStatsCoversHistoryBeyondPageSizeCap(t *testing.T) {
	const n = logbuf.MaxPageSize + 250
	h, _ := newStatsHandler(t, persistedEntries(n))

	out := doStats(t, h)
	if got := out["total"].(float64); got != float64(n) {
		t.Errorf("total=%v，期望 %d —— 说明统计仍被分页上界(%d)截断",
			got, n, logbuf.MaxPageSize)
	}
	// tokens 也必须基于全量（fixture 每条 +10）；截断会让它正比于 300 而非 n
	if got := out["tokens"].(float64); got != float64(n*10) {
		t.Errorf("tokens=%v，期望 %d —— 聚合窗口不完整（被截断到 %d 条时会得到 %d）",
			got, n*10, logbuf.MaxPageSize, logbuf.MaxPageSize*10)
	}
	// 全 200 ⇒ 成功率 100% 且 fail=0；这两条在截断时也会"恰好"成立，
	// 所以不作为截断判据，只是顺带确认口径没被改坏。
	if got := out["fail"].(float64); got != 0 {
		t.Errorf("fail=%v，期望 0（fixture 全为 200）", got)
	}
}

// aggregated 字段：显式暴露「实际参与聚合的条数」，与 file.count 对不上时报警。
//
// 这是防回归的第二道锁：即便将来又有人把取数路径改成有上界的分页，
// 响应里也会带上 truncated=true 与一句说明，而不是像上次那样静默少统计。
func TestStatsExposesAggregatedAndFlagsTruncation(t *testing.T) {
	const n = logbuf.MaxPageSize + 100
	h, _ := newStatsHandler(t, persistedEntries(n))

	out := doStats(t, h)

	agg, ok := out["aggregated"].(float64)
	if !ok {
		t.Fatal("缺少 aggregated 字段")
	}
	if agg != float64(n) {
		t.Errorf("aggregated=%v，期望 %d", agg, n)
	}
	// 正常情况不该带 truncated
	if v, exists := out["truncated"]; exists && v == true {
		t.Errorf("未截断却标了 truncated=true；error=%v", out["error"])
	}
	// file.count 应与 aggregated 一致（同为全量）
	if f, ok := out["file"].(map[string]any); ok {
		if c, ok := f["count"].(float64); ok && c != agg {
			t.Errorf("file.count=%v 与 aggregated=%v 不一致，应触发 truncated 标注", c, agg)
		}
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

// ---------------------------------------------------------------------------
// B1.1：usage 派生字段（Credit / ThinkTokens / CacheHit / CacheMiss）的聚合
// ---------------------------------------------------------------------------

// 新字段的求和必须正确，且只累加正数。
func TestAggregateChatLogUsageFieldsArithmetic(t *testing.T) {
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	items := []logbuf.Entry{
		{At: base, Model: "a", Status: 200, Tokens: 10, TotalMS: 1000,
			Credit: 0.51, ThinkTokens: 100, CacheHitTokens: 80, CacheMissTokens: 20},
		{At: base.Add(time.Minute), Model: "b", Status: 200, Tokens: 20, TotalMS: 2000,
			Credit: 2.5, ThinkTokens: 300, CacheHitTokens: 120, CacheMissTokens: 30},
	}
	out := aggregateChatLog(items)

	if got := out["credit_total"].(float64); got != 3.01 {
		t.Errorf("credit_total=%v want 3.01", got)
	}
	if got := out["think_tokens"].(int64); got != 400 {
		t.Errorf("think_tokens=%v want 400", got)
	}
	if got := out["cache_hit_tokens"].(int64); got != 200 {
		t.Errorf("cache_hit_tokens=%v want 200", got)
	}
	if got := out["cache_miss_tokens"].(int64); got != 50 {
		t.Errorf("cache_miss_tokens=%v want 50", got)
	}
	// 命中率 = 200 / (200+50) = 0.8
	if got := out["cache_hit_rate"].(float64); got != 0.8 {
		t.Errorf("cache_hit_rate=%v want 0.8", got)
	}
}

// 旧格式行（新字段全为 Go 零值）必须贡献 0，且不破坏既有聚合量。
// 这是本次改动**最需要防止的回归**：真实落盘日志里 ~1900 行都是这种形状。
func TestAggregateChatLogLegacyRowsContributeZero(t *testing.T) {
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	// 刻意构造「旧行 + 新行混排」：旧行不能把新行的聚合冲掉。
	items := []logbuf.Entry{
		{At: base, Model: "old", Status: 200, UID: "u1", TTFBMS: 100, Tokens: 10, TotalMS: 1000},
		{At: base.Add(time.Minute), Model: "new", Status: 200, UID: "u1", TTFBMS: 300,
			Tokens: 20, TotalMS: 2000, Credit: 1.5, ThinkTokens: 7, CacheHitTokens: 3, CacheMissTokens: 1},
		{At: base.Add(2 * time.Minute), Model: "old", Status: 500, UID: "u2", TotalMS: 3000},
	}
	out := aggregateChatLog(items)

	if got := out["credit_total"].(float64); got != 1.5 {
		t.Errorf("credit_total=%v want 1.5（旧行必须贡献 0）", got)
	}
	if got := out["think_tokens"].(int64); got != 7 {
		t.Errorf("think_tokens=%v want 7", got)
	}
	// 命中率只由新行决定：3/(3+1)=0.75（旧行的 0/0 不能稀释分母）
	if got := out["cache_hit_rate"].(float64); got != 0.75 {
		t.Errorf("cache_hit_rate=%v want 0.75（旧行的 0 不应进入分母）", got)
	}
	// 既有聚合量不受影响
	if got := out["total"].(int); got != 3 {
		t.Errorf("total=%v want 3", got)
	}
	if got := out["ok"].(int64); got != 2 {
		t.Errorf("ok=%v want 2", got)
	}
	if got := out["fail"].(int64); got != 1 {
		t.Errorf("fail=%v want 1", got)
	}
	if got := out["tokens"].(int64); got != 30 {
		t.Errorf("tokens=%v want 30", got)
	}
	if got := out["avg_total_ms"].(int64); got != 2000 {
		t.Errorf("avg_total_ms=%v want 2000", got)
	}
	if got := out["avg_ttfb_ms"].(int64); got != 200 {
		t.Errorf("avg_ttfb_ms=%v want 200", got)
	}
}

// 全为旧格式行（真实现状）：四个求和字段为 0，且 cache_hit_rate **不出现**。
//
// 为什么是不出现而不是 0：0% 是一个结论，而真相是「没有缓存数据」。
// 前端据此显示「—」，不会把缺数据读成「缓存完全没命中」。
func TestAggregateChatLogAllLegacyNoHitRate(t *testing.T) {
	items := []logbuf.Entry{
		{At: time.Now(), Model: "m", Status: 200, Tokens: 5, TotalMS: 10},
		{At: time.Now(), Model: "m", Status: 200, Tokens: 5, TotalMS: 10},
	}
	out := aggregateChatLog(items)

	for _, k := range []string{"credit_total", "think_tokens", "cache_hit_tokens", "cache_miss_tokens"} {
		v, ok := out[k]
		if !ok {
			t.Fatalf("缺少字段 %s（数值字段应恒在，便于前端无判空取值）", k)
		}
		switch n := v.(type) {
		case float64:
			if n != 0 {
				t.Errorf("%s=%v want 0", k, n)
			}
		case int64:
			if n != 0 {
				t.Errorf("%s=%v want 0", k, n)
			}
		default:
			t.Errorf("%s 类型意外: %T", k, v)
		}
	}
	if v, ok := out["cache_hit_rate"]; ok {
		t.Errorf("分母为 0 时不应输出 cache_hit_rate（got %v）—— 输出 0 会被读成「命中率 0%%」", v)
	}
}

// 铁律：cache_hit_rate 在任何输入下都不得是 NaN。
//
// 直接钉住 0/0 这个 case：Go 的 float64(0)/float64(0) 是 NaN，
// 而 NaN 经 encoding/json 会变成 `null`（Marshal 报错后 writeJSON 吞掉错误 → 空 body）。
func TestAggregateChatLogCacheHitRateNeverNaN(t *testing.T) {
	cases := map[string][]logbuf.Entry{
		"空集":         nil,
		"只有 hit":     {{CacheHitTokens: 0, CacheMissTokens: 0}},
		"hit 无 miss": {{CacheHitTokens: 100}},
		"miss 无 hit": {{CacheMissTokens: 100}},
		"全 0":        {{}, {}},
		"负数（异常）":     {{CacheHitTokens: -5, CacheMissTokens: -5}},
	}
	for name, items := range cases {
		out := aggregateChatLog(items)
		r, ok := out["cache_hit_rate"]
		if !ok {
			continue // 缺字段是允许的降级形态
		}
		f, isF := r.(float64)
		if !isF {
			t.Errorf("%s: cache_hit_rate 类型 %T 不是 float64", name, r)
			continue
		}
		if f != f {
			t.Errorf("%s: cache_hit_rate 是 NaN", name)
		}
		if f < 0 || f > 1 {
			t.Errorf("%s: cache_hit_rate=%v 越界（应在 [0,1]）", name, f)
		}
	}
}

// cacheHitRate 纯函数的边界表。
func TestCacheHitRate(t *testing.T) {
	cases := []struct {
		hit, miss int64
		want      float64
		wantOK    bool
	}{
		{0, 0, 0, false},
		{10, 0, 1, true},
		{0, 10, 0, true},
		{3, 1, 0.75, true},
		{1, 3, 0.25, true},
		{-1, -1, 0, false},
		// 负数只可能来自异常数据（解析层已保证非负）。这里钉住的是：
		// **不会 NaN、不会 panic**，算术就是普通浮点除法。-5/5 = -1 是正常 IEEE 结果，
		// 而 aggregateChatLog 的 `>0` 过滤意味着这种值根本到不了累加器（见下一条测试）。
		{-5, 10, -1, true},
	}
	for _, c := range cases {
		got, ok := cacheHitRate(c.hit, c.miss)
		if ok != c.wantOK {
			t.Errorf("cacheHitRate(%d,%d) ok=%v want %v", c.hit, c.miss, ok, c.wantOK)
		}
		if ok && got != c.want {
			t.Errorf("cacheHitRate(%d,%d)=%v want %v", c.hit, c.miss, got, c.want)
		}
	}
}

// 四个数值字段恒在响应里（即使是 0），前端才能无判空地取值。
// 这条与「cache_hit_rate 缺席」是一对：数值字段恒在、比率字段按需 — 泾渭分明。
func TestAggregateChatLogNumericFieldsAlwaysPresent(t *testing.T) {
	for _, items := range [][]logbuf.Entry{nil, {{}}, {{Credit: 1}}} {
		out := aggregateChatLog(items)
		for _, k := range []string{"credit_total", "think_tokens", "cache_hit_tokens", "cache_miss_tokens"} {
			if _, ok := out[k]; !ok {
				t.Errorf("items=%d 条时缺少 %s", len(items), k)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// B1.2：成本系数 + 目录状态进 /admin/stats
// ---------------------------------------------------------------------------

// statsModels 从 /admin/stats 的**已解码 JSON** 响应里取出 model_multipliers。
//
// 注意：doStats 走的是真实 HTTP 路径 + json.Unmarshal，所以嵌套值回来是
// []any / map[string]any 而不是 Go 结构体 —— 这里刻意按 JSON 形状读，
// 因为**前端看到的就是这个形状**，用结构体断言等于测了一个前端不存在的形态。
func statsModels(t *testing.T, v any) ([]string, map[string]map[string]any) {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("model_multipliers 类型 = %T，期望 []any", v)
	}
	order := make([]string, 0, len(list))
	out := map[string]map[string]any{}
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("model_multipliers 元素类型 = %T，期望 map[string]any", raw)
		}
		id, _ := m["model"].(string)
		order = append(order, id)
		out[id] = m
	}
	return order, out
}

// statsCatalog 取出已解码的 model_catalog 对象。
func statsCatalog(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("model_catalog 类型 = %T，期望 map[string]any", v)
	}
	return m
}

func catalogWith(entries ...upstream.ModelCatalogEntry) *upstream.ModelCatalog {
	return &upstream.ModelCatalog{Models: entries}
}

// 系数只对**窗口里被调用过**的模型输出，且直接给在 /admin/stats 的同一个响应里。
func TestStatsModelMultipliersFilteredByByModel(t *testing.T) {
	h, _ := newStatsHandler(t, persistedEntries(3))
	h.cfg.ModelCatalog = func() *upstream.ModelCatalog {
		return catalogWith(
			upstream.ModelCatalogEntry{ID: "deepseek-v4", Multiplier: 0.51}, // 窗口里有调用
			upstream.ModelCatalogEntry{ID: "never-called", Multiplier: 9.9}, // 窗口里没有
			upstream.ModelCatalogEntry{ID: "zero-mult", Multiplier: 0},      // 系数 0
		)
	}
	h.cfg.ModelCatalogState = func() ModelCatalogState {
		return ModelCatalogState{State: "ok", Models: 3}
	}

	out := doStats(t, h)
	order, byID := statsModels(t, out["model_multipliers"])
	if len(order) != 1 || order[0] != "deepseek-v4" {
		t.Fatalf("model_multipliers=%v，期望只剩被调用过的 deepseek-v4", order)
	}
	if got := byID["deepseek-v4"]; got["multiplier"] != 0.51 || got["calls"] != float64(3) {
		t.Errorf("deepseek-v4=%v want {multiplier:0.51, calls:3}", got)
	}

	st := statsCatalog(t, out["model_catalog"])
	if st["state"] != "ok" || st["models"] != float64(3) {
		t.Errorf("model_catalog=%v want {state:ok, models:3}", st)
	}
}

// 目录拿不到时：state=unavailable、系数表为空数组、**统计接口本身仍然 200 且字段齐全**。
// 这是硬约束 2（失败不得 break 统计接口）的可测形态。
func TestStatsDegradesWhenCatalogUnavailable(t *testing.T) {
	h, _ := newStatsHandler(t, persistedEntries(5))
	h.cfg.ModelCatalog = func() *upstream.ModelCatalog { return nil }
	h.cfg.ModelCatalogState = func() ModelCatalogState {
		return ModelCatalogState{State: "unavailable", Cooldown: true}
	}

	out := doStats(t, h) // 内部已断言 HTTP 200
	if got := out["total"].(float64); got != 5 {
		t.Errorf("total=%v want 5（目录不可用不得影响其它统计）", got)
	}
	st := statsCatalog(t, out["model_catalog"])
	if st["state"] != "unavailable" || st["cooldown"] != true {
		t.Errorf("model_catalog=%v want unavailable + cooldown", st)
	}
	order, _ := statsModels(t, out["model_multipliers"])
	if len(order) != 0 {
		t.Errorf("不可用时应为空表, got %v", order)
	}
}

// 未接线（两个闭包都是 nil）也不能 panic —— 测试与旧部署路径都会走到。
func TestStatsWithoutCatalogWiring(t *testing.T) {
	h, _ := newStatsHandler(t, persistedEntries(2))
	out := doStats(t, h)
	st := statsCatalog(t, out["model_catalog"])
	if st["state"] != "unavailable" {
		t.Errorf("未接线时 state=%v want unavailable", st["state"])
	}
	order, _ := statsModels(t, out["model_multipliers"])
	if len(order) != 0 {
		t.Errorf("未接线时应为空表, got %v", order)
	}
}

// 目录状态为 unavailable 时**仍然要尝试**取一次 —— 这是自举的必要条件。
//
// 本测试替代了原来的 TestStatsDoesNotFetchCatalogWhenUnavailable。
// 那条测试把「unavailable 就提前返回」当作正确行为来断言，实际上**固化了缺陷**：
//
//	state 在缓存为空时返回 unavailable → 提前返回、从不调 ModelCatalog()
//	→ 缓存永远为空 → 永远 unavailable（自锁死循环）
//
// 正确契约：状态只用于**展示**，不用于决定要不要回源。
// 「要不要回源」由 ModelCatalog 自己按 TTL/负缓存决定（它才掌握那些信息）。
// 冷启动自举的端到端回归见 catalog_bootstrap_test.go。
func TestStatsStillTriesCatalogWhenStateUnavailable(t *testing.T) {
	var calls int
	h, _ := newStatsHandler(t, persistedEntries(2))
	h.cfg.ModelCatalog = func() *upstream.ModelCatalog { calls++; return nil }
	h.cfg.ModelCatalogState = func() ModelCatalogState {
		return ModelCatalogState{State: "unavailable"}
	}
	out := doStats(t, h)

	if calls == 0 {
		t.Fatal("unavailable 时也应尝试取一次，否则目录永远无法自举（冷启动死锁）")
	}
	// 拿不到目录时仍要给出干净的降级结果，不能报错也不能有脏数据
	order, _ := statsModels(t, out["model_multipliers"])
	if len(order) != 0 {
		t.Errorf("拿不到目录时应为空表, got %v", order)
	}
	if st, _ := out["model_catalog"].(map[string]any); st["state"] != "unavailable" {
		t.Errorf("拿不到目录时 state 应如实报 unavailable, got %v", st["state"])
	}
}

// 窗口内没有任何调用时也不该回源 —— 没有 by_model 就没有可归因的模型。
func TestStatsDoesNotFetchCatalogWhenNoCalls(t *testing.T) {
	var calls int
	h, _ := newStatsHandler(t, nil) // 空窗口
	h.cfg.ModelCatalog = func() *upstream.ModelCatalog { calls++; return nil }
	h.cfg.ModelCatalogState = func() ModelCatalogState { return ModelCatalogState{State: "ok"} }
	doStats(t, h)
	if calls != 0 {
		t.Errorf("空窗口仍调用了 ModelCatalog %d 次", calls)
	}
}

// 输出顺序必须稳定（可按名字 diff），不能受 map 遍历随机顺序影响。
func TestStatsModelMultipliersOrderIsStable(t *testing.T) {
	items := []logbuf.Entry{
		{At: time.Now(), Model: "zeta", Status: 200, TotalMS: 1},
		{At: time.Now(), Model: "alpha", Status: 200, TotalMS: 1},
		{At: time.Now(), Model: "mid", Status: 200, TotalMS: 1},
	}
	cat := func() *upstream.ModelCatalog {
		return catalogWith(
			upstream.ModelCatalogEntry{ID: "zeta", Multiplier: 3},
			upstream.ModelCatalogEntry{ID: "alpha", Multiplier: 1},
			upstream.ModelCatalogEntry{ID: "mid", Multiplier: 2},
		)
	}
	state := func() ModelCatalogState { return ModelCatalogState{State: "ok"} }
	want := []string{"alpha", "mid", "zeta"}
	for i := 0; i < 5; i++ {
		// 每次都换新 handler，确保不是「碰巧同一次聚合」
		hh, _ := newStatsHandler(t, items)
		hh.cfg.ModelCatalog = cat
		hh.cfg.ModelCatalogState = state
		order, _ := statsModels(t, doStats(t, hh)["model_multipliers"])
		if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
			t.Fatalf("第 %d 次顺序=%v want %v", i, order, want)
		}
	}
}

// 目录里的模型在窗口里没调用过时，不输出 0 系数条目 ——
// 0 在前端会显示成「免费」，而真相是「不知道」。
func TestStatsOmitsModelsAbsentFromCatalog(t *testing.T) {
	items := []logbuf.Entry{
		{At: time.Now(), Model: "known", Status: 200, TotalMS: 1},
		{At: time.Now(), Model: "unknown", Status: 200, TotalMS: 1},
	}
	h, _ := newStatsHandler(t, items)
	h.cfg.ModelCatalog = func() *upstream.ModelCatalog {
		return catalogWith(upstream.ModelCatalogEntry{ID: "known", Multiplier: 1.5})
	}
	h.cfg.ModelCatalogState = func() ModelCatalogState { return ModelCatalogState{State: "ok"} }

	order, byID := statsModels(t, doStats(t, h)["model_multipliers"])
	if len(order) != 1 || order[0] != "known" {
		t.Fatalf("order=%v，目录里没有的模型不应输出", order)
	}
	if byID["known"]["calls"] != float64(1) {
		t.Errorf("calls=%v want 1", byID["known"]["calls"])
	}
}

// 纯聚合函数不得因为新字段而依赖任何目录状态（保持 doc comment 承诺的纯函数语义）。
func TestAggregateChatLogStaysPure(t *testing.T) {
	items := []logbuf.Entry{{At: time.Now(), Model: "m", Status: 200, Credit: 1, TotalMS: 1}}
	first := aggregateChatLog(items)
	second := aggregateChatLog(items)
	if first["credit_total"] != second["credit_total"] {
		t.Error("同一输入两次聚合结果不同 —— 出现了隐藏状态")
	}
	// 输出里不应混入目录相关键（那是 stats handler 的职责，不是纯聚合的）
	for _, k := range []string{"model_catalog", "model_multipliers"} {
		if _, ok := first[k]; ok {
			t.Errorf("纯聚合函数输出了 %s，应只由 stats handler 附加", k)
		}
	}
}
