package admin

// logs_history_counts_test.go —— T-394 ②：`/admin/logs/history` 增加**全库状态计数** `counts`。
//
// # 为什么需要它
//
// 前端 chip 上要显示「200: 80 / 429: 40」，而 T-393 的响应里只有 total（当前筛选档）
// 与 total_all（全库）—— chip 拿不到各档数字，用户点「429 : 1200」也验证不了。
//
// # 为什么必须与 `?status=X` 的 total 逐字对齐
//
// 「429 : 1200」点进去必须正好 1200 条。不一致就是必现的信任崩塌 ——
// 所以这条一致性是**测试钉死的契约**，不是"实现细节"。

import "testing"

// TestLogsHistoryCountsMatchFilter seam ②：counts 与筛选结果、与 total_all 三重一致。
func TestLogsHistoryCountsMatchFilter(t *testing.T) {
	h, _ := newStatsHandler(t, statusSeeds()) // 200×80 + 429×40 = 120

	resp := logsHistoryReq(t, h, "limit=30&offset=0")
	rawCounts, ok := resp["counts"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 counts（或形状不对）：%#v", resp["counts"])
	}
	if rawCounts["200"] != float64(80) || rawCounts["429"] != float64(40) {
		t.Errorf("counts = %v，want {\"200\":80,\"429\":40}", rawCounts)
	}
	if len(rawCounts) != 2 {
		t.Errorf("counts 键数 = %d，want 2（没有的状态不该出现）", len(rawCounts))
	}

	// ① 与 ?status=X 的 total 逐字相等（用户点 chip 进去必须正好那么多条）
	sum := 0.0
	for k, v := range rawCounts {
		f, _ := v.(float64)
		sum += f
		one := logsHistoryReq(t, h, "limit=5&offset=0&status="+k)
		if one["total"] != v {
			t.Errorf("counts[%s] = %v，但 ?status=%s 的 total = %v ⇒ chip 数字与列表不一致（信任崩塌）",
				k, v, k, one["total"])
		}
		// 而且那一档真的筛得出来（总数 > 每页时，首页条数应为 limit）
		if f > 5 {
			if n := len(itemsOf(t, one)); n != 5 {
				t.Errorf("?status=%s&limit=5 ⇒ items=%d，want 5", k, n)
			}
		}
	}
	// ② 求和 == total_all
	if sum != resp["total_all"] {
		t.Errorf("sum(counts) = %v，want total_all = %v", sum, resp["total_all"])
	}
	if resp["total_all"] != float64(120) {
		t.Errorf("total_all = %v，want 120", resp["total_all"])
	}
	t.Logf("counts = %v（sum=%v == total_all=%v）；逐档核对：?status=200 total=%v、?status=429 total=%v",
		rawCounts, sum, resp["total_all"],
		logsHistoryReq(t, h, "limit=1&offset=0&status=200")["total"],
		logsHistoryReq(t, h, "limit=1&offset=0&status=429")["total"])
}

// TestLogsHistoryCountsUnderFilter counts 是**全库**计数（不随 status 参数缩水）。
func TestLogsHistoryCountsUnderFilter(t *testing.T) {
	h, _ := newStatsHandler(t, statusSeeds())
	for _, q := range []string{"limit=30&offset=0", "limit=30&offset=0&status=200", "limit=30&offset=0&status=429"} {
		resp := logsHistoryReq(t, h, q)
		c, _ := resp["counts"].(map[string]any)
		if c["200"] != float64(80) || c["429"] != float64(40) {
			t.Errorf("%s ⇒ counts = %v，want 全库计数 {\"200\":80,\"429\":40}（chip 在筛选态下也要显示各档总数）",
				q, c)
		}
	}
	t.Log("counts 在筛选态与不筛态下一致：全库计数 ✓")
}
