package admin

// logs_history_status_test.go —— T-393：`/admin/logs/history` 的 `status` 服务端筛选。
//
// # 为什么要服务端筛
//
// 前端原来只能在内存里过滤**当前页 30 条** —— 用户翻到第 2 页点「200」，
// 看到的只是第 2 页里的 200，chip 上的数字也是"本页数量"。日志有 5 万条，
// 前端不可能全拉下来筛。
//
// 两条 seam（只测这两个）：
//  ① status 过滤 + offset 分页的交互（total 反映**筛选后**总数，翻页在筛选后的集合上走）
//  ② 不筛时零回归（与改动前逐条一致）

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// logsHistoryReq 走真 HTTP 边界（复用 stats_test.go 的 newStatsHandler + withTestAuth）。
func logsHistoryReq(t *testing.T, h *Handler, query string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/logs/history?"+query, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withTestAuth(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s ⇒ HTTP %d：%s", query, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	return out
}

// statusSeeds 造 120 条：200 的 80 条、429 的 40 条（时间递增，最新在后 ⇒ sink 里最新在前）。
func statusSeeds() []logbuf.Entry {
	base := time.Now().Add(-2 * time.Hour)
	out := make([]logbuf.Entry, 0, 120)
	for i := 0; i < 120; i++ {
		st := 200
		if i%3 == 2 { // 每 3 条里 1 条 429 ⇒ 恰好 40 条
			st = 429
		}
		out = append(out, logbuf.Entry{
			At: base.Add(time.Duration(i) * time.Minute), Status: st, Model: "m-f",
			Channel: "ch", UID: "u", Tokens: 5, OutTokens: 2, Credit: 1, TotalMS: 10,
		})
	}
	return out
}

func itemsOf(t *testing.T, resp map[string]any) []map[string]any {
	t.Helper()
	raw, ok := resp["items"].([]any)
	if !ok {
		t.Fatalf("items 不是数组：%#v", resp["items"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("item 不是对象：%#v", v)
		}
		out = append(out, m)
	}
	return out
}

// TestLogsHistoryStatusFilter seam ①：筛选 + 分页。
func TestLogsHistoryStatusFilter(t *testing.T) {
	h, _ := newStatsHandler(t, statusSeeds())

	// 第 1 页：total 是**筛选后**总数（80），不是全库（120）
	r1 := logsHistoryReq(t, h, "limit=30&offset=0&status=200")
	if r1["total"] != float64(80) {
		t.Errorf("total = %v，want 80（筛选后的总数，不是本页数）", r1["total"])
	}
	if r1["total_all"] != float64(120) {
		t.Errorf("total_all = %v，want 120（未筛选的全库总数）", r1["total_all"])
	}
	if r1["filtered"] != true {
		t.Errorf("filtered = %v，want true", r1["filtered"])
	}
	it1 := itemsOf(t, r1)
	if len(it1) != 30 {
		t.Errorf("首页 items = %d，want 30", len(it1))
	}
	for i, m := range it1 {
		if m["status"] != float64(200) {
			t.Fatalf("第 %d 条 status = %v，want 200（筛选必须服务端生效）", i, m["status"])
		}
	}
	t.Logf("① ?limit=30&offset=0&status=200 ⇒ total=%v total_all=%v filtered=%v items=%d 首条=%v",
		r1["total"], r1["total_all"], r1["filtered"], len(it1), it1[0])

	// 第 3 页（offset=60）⇒ 80 条里的最后 20 条
	r2 := logsHistoryReq(t, h, "limit=30&offset=60&status=200")
	if r2["total"] != float64(80) || len(itemsOf(t, r2)) != 20 {
		t.Errorf("offset=60 ⇒ total=%v items=%d，want 80 / 20", r2["total"], len(itemsOf(t, r2)))
	}
	// 越界页 ⇒ 空数组但 total 不变（前端据此显示"没有更多"，而不是"没数据"）
	r3 := logsHistoryReq(t, h, "limit=30&offset=90&status=200")
	if r3["total"] != float64(80) {
		t.Errorf("越界页 total = %v，want 80", r3["total"])
	}
	if n := len(itemsOf(t, r3)); n != 0 {
		t.Errorf("越界页 items = %d，want 0", n)
	}
	t.Logf("② offset=60 ⇒ items=%d；offset=90 ⇒ items=%d（total 仍 %v）",
		len(itemsOf(t, r2)), len(itemsOf(t, r3)), r3["total"])

	// status=429 也必须真的筛（80/40 两边都验，防止"只对 200 生效"的假实现）
	r4 := logsHistoryReq(t, h, "limit=30&offset=0&status=429")
	if r4["total"] != float64(40) || r4["total_all"] != float64(120) {
		t.Errorf("status=429 ⇒ total=%v total_all=%v，want 40 / 120", r4["total"], r4["total_all"])
	}
	for i, m := range itemsOf(t, r4) {
		if m["status"] != float64(429) {
			t.Fatalf("429 档第 %d 条 status = %v，want 429", i, m["status"])
		}
	}
	t.Logf("   status=429 ⇒ total=%v items=%d", r4["total"], len(itemsOf(t, r4)))
}

// TestLogsHistoryNoFilterRegression seam ②：不筛（缺省 / 空串 / all）零回归。
func TestLogsHistoryNoFilterRegression(t *testing.T) {
	h, _ := newStatsHandler(t, statusSeeds())

	base := logsHistoryReq(t, h, "limit=30&offset=0")
	if base["total"] != float64(120) || base["total_all"] != float64(120) {
		t.Errorf("不筛 ⇒ total=%v total_all=%v，want 120 / 120", base["total"], base["total_all"])
	}
	if base["filtered"] != false {
		t.Errorf("不筛 ⇒ filtered=%v，want false", base["filtered"])
	}
	if n := len(itemsOf(t, base)); n != 30 {
		t.Errorf("不筛首页 items = %d，want 30", n)
	}
	// 与改动前逐条一致：三种"不筛"写法必须得到完全一样的结果
	for _, q := range []string{"limit=30&offset=0&status=", "limit=30&offset=0&status=all", "limit=30&offset=0&status=abc", "limit=30&offset=0&status=-5"} {
		got := logsHistoryReq(t, h, q)
		if got["filtered"] != false {
			t.Errorf("%s ⇒ filtered=%v，want false（非法值当作不筛，不要 400）", q, got["filtered"])
		}
		if got["total"] != base["total"] {
			t.Errorf("%s ⇒ total=%v，want %v（与不筛一致）", q, got["total"], base["total"])
		}
		a, _ := json.Marshal(itemsOf(t, got))
		b, _ := json.Marshal(itemsOf(t, base))
		if string(a) != string(b) {
			t.Errorf("%s ⇒ items 与不筛不一致（会有脏值把面板变成错误条或换了数据）", q)
		}
	}
	// 分页仍可用：第 2 页 30 条、第 4 页 30 条（总共 120）
	r2 := logsHistoryReq(t, h, "limit=30&offset=30")
	if len(itemsOf(t, r2)) != 30 || r2["total"] != float64(120) {
		t.Errorf("不筛 offset=30 ⇒ items=%d total=%v，want 30 / 120", len(itemsOf(t, r2)), r2["total"])
	}
	t.Logf("③ 不筛 ⇒ total=%v total_all=%v filtered=%v items=%d（status= 空/all/abc/-5 四种写法结果一致）",
		base["total"], base["total_all"], base["filtered"], len(itemsOf(t, base)))
}
