package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
)

// 分页契约的 HTTP 层验收：前端分页组件直接依赖这三个字段的语义，
// 所以要在 handler 层钉住，而不只是包内方法。
//   - items 时间倒序（最新在前）；
//   - total 是过滤后总数（用于算总页数），不是本页条数；
//   - offset/limit 原样回显，便于前端核对。

func newHistoryHandler(t *testing.T, n int) *Handler {
	t.Helper()
	log := checkinlog.New(filepath.Join(t.TempDir(), "h.json"), 30)
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		kind := checkinlog.KindCheckin
		if i%2 == 0 {
			kind = checkinlog.KindGrowth
		}
		log.Append(checkinlog.Record{
			At:     base.Add(time.Duration(i) * time.Minute),
			UID:    "u",
			Kind:   kind,
			Status: checkinlog.StatusOK,
		})
	}
	return New(Config{Log: log})
}

func doHistory(t *testing.T, h *Handler, query string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/checkin/history"+query, nil)
	req.RemoteAddr = "127.0.0.1:12345" // 必须 loopback，否则被 ServeHTTP 拦掉
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

func TestHistoryEndpointPaginates(t *testing.T) {
	h := newHistoryHandler(t, 25)

	page1 := doHistory(t, h, "?limit=10&offset=0")
	if got := page1["total"].(float64); got != 25 {
		t.Errorf("total=%v，期望 25（总数而不是本页条数）", got)
	}
	if got := len(page1["items"].([]any)); got != 10 {
		t.Errorf("第 1 页条数=%d，期望 10", got)
	}
	if got := page1["offset"].(float64); got != 0 {
		t.Errorf("offset 回显=%v，期望 0", got)
	}

	page3 := doHistory(t, h, "?limit=10&offset=20")
	if got := len(page3["items"].([]any)); got != 5 {
		t.Errorf("末页条数=%d，期望 5（25 条按 10 条一页）", got)
	}
	if got := page3["total"].(float64); got != 25 {
		t.Errorf("末页 total=%v，期望仍为 25", got)
	}

	// 两页不应有重叠
	seen := map[string]bool{}
	for _, it := range page1["items"].([]any) {
		seen[it.(map[string]any)["at"].(string)] = true
	}
	for _, it := range page3["items"].([]any) {
		if seen[it.(map[string]any)["at"].(string)] {
			t.Error("第 1 页与第 3 页出现重复记录")
		}
	}
}

func TestHistoryEndpointNewestFirst(t *testing.T) {
	h := newHistoryHandler(t, 3)
	out := doHistory(t, h, "?limit=3")
	items := out["items"].([]any)

	var prev time.Time
	for i, it := range items {
		at, err := time.Parse(time.RFC3339Nano, it.(map[string]any)["at"].(string))
		if err != nil {
			t.Fatalf("解析 at: %v", err)
		}
		if i > 0 && at.After(prev) {
			t.Errorf("第 %d 条比第 %d 条更新 —— 不是时间倒序", i, i-1)
		}
		prev = at
	}
}

func TestHistoryEndpointKindFilterAffectsTotal(t *testing.T) {
	// 25 条里 growth 占 i%2==0（i=0,2,...,24）共 13 条
	h := newHistoryHandler(t, 25)
	out := doHistory(t, h, "?limit=30&kind=growth")
	if got := out["total"].(float64); got != 13 {
		t.Errorf("growth 过滤后 total=%v，期望 13", got)
	}
	if got := len(out["items"].([]any)); got != 13 {
		t.Errorf("本页条数=%d，期望 13", got)
	}
	for _, it := range out["items"].([]any) {
		if it.(map[string]any)["kind"] != "growth" {
			t.Errorf("过滤失效：混入 %v", it.(map[string]any)["kind"])
		}
	}
}

func TestHistoryEndpointDefaults(t *testing.T) {
	h := newHistoryHandler(t, 40)
	out := doHistory(t, h, "")
	if got := len(out["items"].([]any)); got != checkinlog.DefaultPageSize {
		t.Errorf("无参数时本页=%d，期望默认 %d", got, checkinlog.DefaultPageSize)
	}
	if got := out["limit"].(float64); got != float64(checkinlog.DefaultPageSize) {
		t.Errorf("limit 回显=%v，期望 %d", got, checkinlog.DefaultPageSize)
	}
}

func TestHistoryEndpointOffsetBeyondEnd(t *testing.T) {
	// 前端算错 offset 时不能 500，也不能返回整页假数据
	h := newHistoryHandler(t, 5)
	out := doHistory(t, h, "?limit=10&offset=999")
	if got := len(out["items"].([]any)); got != 0 {
		t.Errorf("越界 offset 应返回空页，得到 %d 条", got)
	}
	if got := out["total"].(float64); got != 5 {
		t.Errorf("越界时 total=%v，期望仍为 5", got)
	}
}

func TestHistoryEndpointNilLogDegradesGracefully(t *testing.T) {
	// 未注入 Log 时（配置缺失）应返回空结构而不是 panic
	h := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/admin/checkin/history", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，期望 200（优雅降级）", rec.Code)
	}
}
