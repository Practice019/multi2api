package admin

import (
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// Task 7：按上游分组（by_provider）。
//
// # 为什么需要它（one-api 社区的实证教训）
//
// "先把观测建好" —— 多上游之后，若统计不带 provider 维度，
// 两个上游的调用次数与消耗会混在一个数里，面板**无法归因**：
// 看不出钱花在哪个上游、哪个上游在报错。
//
// # 向后兼容
//
// 历史日志行没有 provider 键，反序列化得空串。这类行归入"未标注"桶，
// **不丢弃、也不假装属于某个上游**。

func entryWithProvider(provider, model string, status int, credit float64) logbuf.Entry {
	return logbuf.Entry{
		At:       time.Now(),
		Provider: provider,
		Model:    model,
		Status:   status,
		Credit:   credit,
		Tokens:   10,
	}
}

// TestAggregateByProvider 两个上游的调用要分开计数。
func TestAggregateByProvider(t *testing.T) {
	items := []logbuf.Entry{
		entryWithProvider("workbuddy", "auto", 200, 1.5),
		entryWithProvider("workbuddy", "auto", 200, 2.5),
		entryWithProvider("codearts", "GLM-5.2", 200, 10),
		entryWithProvider("codearts", "GLM-5.2", 503, 0),
	}
	resp := aggregateChatLog(items)

	bp, ok := resp["by_provider"].(map[string]providerStat)
	if !ok {
		// 允许实现用别的具体类型，退化为 map[string]any 检查
		raw, ok2 := resp["by_provider"].(map[string]any)
		if !ok2 {
			t.Fatalf("by_provider 缺失或类型不对: %T", resp["by_provider"])
		}
		checkProviderAny(t, raw, "workbuddy", 2, 4.0, 2)
		checkProviderAny(t, raw, "codearts", 2, 10.0, 1)
		return
	}
	checkProvider(t, bp, "workbuddy", 2, 4.0, 2)
	checkProvider(t, bp, "codearts", 2, 10.0, 1)
}

func checkProvider(t *testing.T, m map[string]providerStat, name string, calls int64, credit float64, ok int64) {
	t.Helper()
	s, exists := m[name]
	if !exists {
		t.Fatalf("by_provider 缺 %q（有: %v）", name, keysOfStat(m))
	}
	if s.Calls != calls {
		t.Errorf("%s.Calls=%d want %d", name, s.Calls, calls)
	}
	if s.Credit != credit {
		t.Errorf("%s.Credit=%v want %v", name, s.Credit, credit)
	}
	if s.OK != ok {
		t.Errorf("%s.OK=%d want %d", name, s.OK, ok)
	}
}

func checkProviderAny(t *testing.T, raw map[string]any, name string, calls int64, credit float64, ok int64) {
	t.Helper()
	v, exists := raw[name]
	if !exists {
		t.Fatalf("by_provider 缺 %q", name)
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		t.Fatalf("by_provider[%q] 类型 %T", name, v)
	}
	// 数值可能是 int / int64 / float64，统一转
	num := func(k string) float64 {
		switch x := m[k].(type) {
		case int:
			return float64(x)
		case int64:
			return float64(x)
		case float64:
			return x
		}
		return -999
	}
	if num("calls") != float64(calls) {
		t.Errorf("%s.calls=%v want %d", name, m["calls"], calls)
	}
	if num("credit") != credit {
		t.Errorf("%s.credit=%v want %v", name, m["credit"], credit)
	}
	if num("ok") != float64(ok) {
		t.Errorf("%s.ok=%v want %d", name, m["ok"], ok)
	}
}

func keysOfStat(m map[string]providerStat) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAggregateByProviderUnlabeled 历史行（无 provider）归入"未标注"，不丢弃。
//
// 这条守住向后兼容：旧日志文件里的行没有 provider 键，
// 反序列化得空串。它们必须仍然被计数，只是归到一个明确的桶里。
func TestAggregateByProviderUnlabeled(t *testing.T) {
	items := []logbuf.Entry{
		{At: time.Now(), Model: "auto", Status: 200, Tokens: 5}, // 历史行，Provider 为空
		entryWithProvider("workbuddy", "auto", 200, 1),
	}
	resp := aggregateChatLog(items)

	if resp["total"] != 2 {
		t.Fatalf("total=%v want 2（历史行不该被丢弃）", resp["total"])
	}

	// 无论实现用哪种类型，都要能查到"未标注"这一桶
	switch bp := resp["by_provider"].(type) {
	case map[string]providerStat:
		if _, ok := bp[unlabeledProvider]; !ok {
			t.Errorf("by_provider 缺 %q 桶（有: %v）", unlabeledProvider, keysOfStat(bp))
		}
	case map[string]any:
		if _, ok := bp[unlabeledProvider]; !ok {
			t.Errorf("by_provider 缺 %q 桶", unlabeledProvider)
		}
	default:
		t.Fatalf("by_provider 类型 %T", resp["by_provider"])
	}
}

// TestAggregateByProviderStatusBreakdown 每个上游要有自己的成功/失败计数。
//
// 用途：多上游之后，"哪个上游在报错"必须一眼可见 ——
// 只看全局 fail 数无法归因。
func TestAggregateByProviderStatusBreakdown(t *testing.T) {
	items := []logbuf.Entry{
		entryWithProvider("workbuddy", "auto", 200, 1),
		entryWithProvider("workbuddy", "auto", 500, 0),
		entryWithProvider("workbuddy", "auto", 502, 0),
		entryWithProvider("codearts", "m", 200, 2),
	}
	resp := aggregateChatLog(items)

	switch bp := resp["by_provider"].(type) {
	case map[string]providerStat:
		if got := bp["workbuddy"].Fail; got != 2 {
			t.Errorf("workbuddy.Fail=%d want 2", got)
		}
		if got := bp["codearts"].Fail; got != 0 {
			t.Errorf("codearts.Fail=%d want 0", got)
		}
	case map[string]any:
		m := bp["workbuddy"].(map[string]any)
		if f, _ := m["fail"].(float64); f != 2 {
			t.Errorf("workbuddy.fail=%v want 2", m["fail"])
		}
	default:
		t.Fatalf("by_provider 类型 %T", resp["by_provider"])
	}
}

// TestAggregateByProviderEmpty 空集不该产出空的 by_provider 噪音。
//
// 与 cache_hit_rate 的处理一致：没数据时**不输出该键**，
// 让前端显示「—」而不是一个误导性的空对象。
func TestAggregateByProviderEmpty(t *testing.T) {
	resp := aggregateChatLog(nil)
	if v, exists := resp["by_provider"]; exists {
		t.Errorf("空集不该输出 by_provider，得到 %v", v)
	}
}

// TestAggregateByProviderSingleUpstream 单上游部署下 by_provider 仍应存在
// （只有一个桶），这样前端逻辑不必分两种形态。
func TestAggregateByProviderSingleUpstream(t *testing.T) {
	items := []logbuf.Entry{entryWithProvider("workbuddy", "auto", 200, 1)}
	resp := aggregateChatLog(items)
	if _, exists := resp["by_provider"]; !exists {
		t.Error("单上游时 by_provider 仍应存在（一个桶），便于前端统一渲染")
	}
}
