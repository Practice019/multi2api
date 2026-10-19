// series_by_model_test.go series 的 by_model 聚合守卫（2026-10-20 加）。
//
// # 为什么要这条测试
//
// `by_model` 是给前端「按模型堆叠的消耗分布」用的原料，它的正确性直接决定柱状图对不对。
// 三条容易错的地方各钉一下：
//
//	① Tokens = -1 是"上游没给 usage"的哨兵 ⇒ 不能进 by_model（与主 Tokens 同一条守卫）
//	② model 名为空的行 ⇒ 计入桶的 tokens，但**不**在 by_model 里伪造一个 "" 键
//	   （否则图例会多出一个无名色块，用户以为是未知模型在偷偷扣量）
//	③ by_model 各模型之和 ≤ 该桶 tokens（空模型名那部分差额是存在且合理的）
package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

func TestSeriesByModelAggregation(t *testing.T) {
	now := time.Now()
	items := []logbuf.Entry{
		{At: now.Add(-10 * time.Minute), Model: "m-a", Provider: "p", Status: 200, Tokens: 100},
		{At: now.Add(-9 * time.Minute), Model: "m-a", Provider: "p", Status: 200, Tokens: 200},
		{At: now.Add(-8 * time.Minute), Model: "m-b", Provider: "p", Status: 200, Tokens: 50},
		// ② 哨兵：绝不能进 by_model
		{At: now.Add(-7 * time.Minute), Model: "m-b", Provider: "p", Status: 200, Tokens: -1},
		// ③ 没有模型名的行：计入 tokens，不进 by_model
		{At: now.Add(-6 * time.Minute), Model: "", Provider: "p", Status: 200, Tokens: 77},
	}
	h, _ := newStatsHandler(t, items)

	code, body := doJSON(t, h, http.MethodGet, "/admin/stats/series?range=24h", "")
	if code != http.StatusOK {
		t.Fatalf("/admin/stats/series 应 200，实际 %d: %s", code, body)
	}
	var resp struct {
		Series []struct {
			TS      string           `json:"ts"`
			Total   int64            `json:"total"`
			Tokens  int64            `json:"tokens"`
			ByModel map[string]int64 `json:"by_model"`
		} `json:"series"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	var bucket *struct {
		TS      string           `json:"ts"`
		Total   int64            `json:"total"`
		Tokens  int64            `json:"tokens"`
		ByModel map[string]int64 `json:"by_model"`
	}
	for i := range resp.Series {
		if resp.Series[i].Total > 0 {
			bucket = &resp.Series[i]
			break
		}
	}
	if bucket == nil {
		t.Fatalf("24h 内应有非空桶，实际全是空桶")
	}
	// ① 桶总量：100+200+50+77 = 427（-1 被排除）
	if bucket.Tokens != 427 {
		t.Fatalf("★ 桶 tokens 应为 427（-1 被排除），实际 %d", bucket.Tokens)
	}
	if got := bucket.ByModel["m-a"]; got != 300 {
		t.Fatalf("m-a 应为 300，实际 %d（by_model: %v）", got, bucket.ByModel)
	}
	if got := bucket.ByModel["m-b"]; got != 50 {
		t.Fatalf("m-b 应为 50（-1 不计），实际 %d（by_model: %v）", got, bucket.ByModel)
	}
	// ② 不能出现空键
	if _, ok := bucket.ByModel[""]; ok {
		t.Fatalf("★ by_model 不能有空模型名键，实际 %v", bucket.ByModel)
	}
	// ③ 各模型之和不应超过桶总量
	var sum int64
	for _, v := range bucket.ByModel {
		sum += v
	}
	if sum > bucket.Tokens {
		t.Fatalf("★ by_model 之和 %d 不应超过桶 tokens %d", sum, bucket.Tokens)
	}
	// 差额必须正好是"没有模型名"的那条（77）—— 这条断言把"差额是合理的"钉住，
	// 否则未来有人会以为 350≠427 是漏统计。
	if bucket.Tokens-sum != 77 {
		t.Fatalf("差额应为无模型名那条的 77，实际 %d（by_model 合计 %d，桶 %d）",
			bucket.Tokens-sum, sum, bucket.Tokens)
	}
}
