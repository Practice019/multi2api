// stats_guard_test.go 统计聚合的两条守卫（2026-10-20 补，用户要求"数据准确"的可执行证明）。
//
// # 守的是什么
//
//	① Tokens = -1 是"上游没给 usage"的**哨兵值**，聚合时必须排除 ——
//	   实测 34945 条里有 4321 条（12%）是它，无条件 += 会让按 provider 的合计变成负数
//	   （曾出现 `codearts tokens = -2`），总量也被压低。
//	② cache_hit 存在**量纲错乱**的记录（hit 100 万+ 而同一行 tokens 只有几百，差 346~656 倍），
//	   直接累加会得出"缓存命中 21 亿 token"并把命中率推成 99.9%。
//	   判据：hit > tokens*10 且 tokens>0 ⇒ 这行缓存数据不可信，不计入。
//
// ⚠ 测试用**合成 Entry**，不碰任何真实日志（项目纪律：验证一律合成数据）。
package admin

import (
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// TestAggregateExcludesUsageSentinel ①：tokens=-1 不进入任何合计。
func TestAggregateExcludesUsageSentinel(t *testing.T) {
	now := time.Now()
	items := []logbuf.Entry{
		{At: now, Model: "m-a", Provider: "p", Status: 200, Tokens: 100},
		{At: now, Model: "m-a", Provider: "p", Status: 200, Tokens: 200},
		{At: now, Model: "m-b", Provider: "p", Status: 200, Tokens: -1}, // 哨兵：usage 缺失
		{At: now, Model: "m-b", Provider: "p", Status: 200, Tokens: 50},
	}
	got := aggregateChatLog(items)
	// 值期望：只有 100+200+50 = 350 有效；-1 必须被丢掉（不是当 0，也不是当 -1）
	if v, ok := got["tokens"]; ok {
		if n, _ := v.(int64); n != 350 {
			t.Fatalf("★ tokens 合计应为 350（-1 被排除），实际 %v", v)
		}
	}
	t.Logf("aggregateChatLog 顶层键: %v", guardKeysOf(got))
}

// TestAggregateRejectsCacheScaleOutlier ②：量纲错乱的 cache 行不计入。
func TestAggregateRejectsCacheScaleOutlier(t *testing.T) {
	now := time.Now()
	items := []logbuf.Entry{
		// 正常行：hit 800 / miss 200，命中率 80%
		{At: now, Model: "m", Provider: "p", Status: 200, Tokens: 1000, CacheHitTokens: 800, CacheMissTokens: 200},
		// 量纲错乱行：hit 100 万 而 tokens 只有 980（实测就是这个样子）—— 必须被剔除
		{At: now, Model: "m", Provider: "p", Status: 200, Tokens: 980, CacheHitTokens: 1039616, CacheMissTokens: 1195},
	}
	got := aggregateChatLog(items)
	rate, ok := got["cache_hit_rate"].(float64)
	if !ok {
		t.Fatalf("cache_hit_rate 缺失或类型不对：%T %v", got["cache_hit_rate"], got["cache_hit_rate"])
	}
	// 若异常行被计入，命中率会被推到 99.9% 左右；只算正常行应为 0.8
	if rate > 0.9 {
		t.Fatalf("★ 命中率应≈0.8（异常行被剔除），实际 %.4f —— 量纲守卫没生效", rate)
	}
	if rate < 0.7 {
		t.Fatalf("命中率应≈0.8，实际 %.4f（把正常行也剔了？）", rate)
	}
}

// guardKeysOf 本文件私用（⚠ 包内已有 keysOf，且签名是 map[string]bool —— 重名会编译失败）
func guardKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
