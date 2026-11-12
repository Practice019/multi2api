package logx

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFlushAggregates 汇总行必须给出"人看的四个关键数字"：请求数/成功失败、输出 token、
// tok/s、p50 延迟；并在有失败时点名**是哪个上游**。
func TestFlushAggregates(t *testing.T) {
	base := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	resetWindowForTest()

	Observe(Outcome{Provider: "loomy", Status: 200, OutToks: 100, TTFB: time.Second, Total: 3 * time.Second})
	cur = base.Add(30 * time.Second)
	Observe(Outcome{Provider: "loomy", Status: 200, OutToks: 200, TTFB: 2 * time.Second, Total: 5 * time.Second})
	cur = base.Add(60 * time.Second)
	Observe(Outcome{Provider: "loomy", Status: 502, OutToks: 0, TTFB: time.Second, Total: 2 * time.Second})
	cur = base.Add(90 * time.Second)

	got := Flush(func() PoolCounts { return PoolCounts{Total: 44, Healthy: 41, Cooling: 2, Disabled: 1} })
	for _, want := range []string{
		"近 1 分钟",
		"请求 3（成功 2 / 失败 1）",
		"输出 300 tok",
		"号 44（可用 41 / 冷却 2 / 禁用 1）",
		"⚠ 失败来自 loomy×1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("汇总行缺少 %q：\n%s", want, got)
		}
	}
	// 清窗之后必须**什么都没有**（否则静默期会打一条"0 请求"的噪声）。
	if again := Flush(nil); again != "" {
		t.Errorf("清窗后不该再有汇总行，实际：%q", again)
	}
}

// TestFlushP50 中位数必须是**排序后取中**，而不是"随手取最后一个"。
func TestFlushP50(t *testing.T) {
	base := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	resetWindowForTest()
	for _, ttfb := range []time.Duration{4 * time.Second, time.Second, 2 * time.Second} {
		Observe(Outcome{Provider: "p", Status: 200, TTFB: ttfb, Total: ttfb + time.Second})
		cur = cur.Add(time.Second)
	}
	got := Flush(nil)
	if !strings.Contains(got, "TTFB p50 2s") {
		t.Errorf("TTFB p50 应为 2s（排序后取中），实际：%s", got)
	}
}

// TestFailProvidersAreNamedRanked 失败必须点名上游、且按次数排序（只报"1 失败"等于没说）。
func TestFailProvidersAreNamedRanked(t *testing.T) {
	base := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	resetWindowForTest()
	for _, p := range []string{"raccoon", "qoder", "raccoon", "cline"} {
		Observe(Outcome{Provider: p, Status: 500})
		cur = cur.Add(time.Second)
	}
	got := Flush(nil)
	if !strings.Contains(got, "raccoon×2、cline×1、qoder×1") {
		t.Errorf("失败上游应按次数排序且全部列出（≤3 个）：%s", got)
	}
}

// ExampleFlush 把"人看到的那一行长什么样"钉成可断言的契约。
func ExampleFlush() {
	base := time.Date(2026, 11, 12, 13, 28, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	resetWindowForTest()
	Observe(Outcome{Provider: "loomy", Status: 200, OutToks: 200, TTFB: time.Second, Total: 3 * time.Second})
	cur = base.Add(2 * time.Minute)
	Observe(Outcome{Provider: "loomy", Status: 200, OutToks: 200, TTFB: time.Second, Total: 3 * time.Second})
	fmt.Println(Flush(func() PoolCounts { return PoolCounts{Total: 44, Healthy: 41, Cooling: 2, Disabled: 1} }))
	// Output:
	// 近 2 分钟 · 请求 2（成功 2 / 失败 0） · 输出 400 tok · 100 tok/s · TTFB p50 1s · 时长 p50 3s · 号 44（可用 41 / 冷却 2 / 禁用 1）
}

// resetWindowForTest 清掉窗口（包级状态；测试之间必须隔离）。
func resetWindowForTest() {
	statsMu.Lock()
	w = &window{}
	statsMu.Unlock()
}

var _ = bytes.MinRead
