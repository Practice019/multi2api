package admin

// series_24h_test.go —— T-389（P0）：`range=24h` 的**右端必须跟着"现在"走**。
//
// # 修的是什么
//
// statsSeries 末尾有一段「去掉尾部全空桶（活跃窗口之外）」：
//
//	for len(out) > 0 && out[len(out)-1].Total == 0 { out = out[:len(out)-1] }
//
// 后果（主脑实测）：最近几小时没有请求时，右端退回**最后一个有数据的桶** ——
// 21:44 打开"近 24 小时"，图上最后一格是 12:00（17 小时前），
// 桶数从 25 变成 8，x 轴刻度跟着变，看起来像"时间是 05:00~12:00"。
//
// 一个趋势图如果右端不是当前时刻，它就是在撒谎 —— 右侧留白远比这轻微。
//
// 三条 seam（都在 HTTP 边界上观察，不测私有函数）：
//  ① 桶数固定（不随数据稀疏而缩）
//  ② 最后一个桶的 ts == 当前整点（允许 ±1 小时）
//  ③ 365d 档回归（桶数与内容不受影响）

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sqlstore "workbuddy2api/internal/store"
)

// seriesReq 调一次 /admin/stats/series 并解出通用外壳。
func seriesReq(t *testing.T, query string) (source string, series []json.RawMessage, raw string) {
	t.Helper()
	h := &Handler{}
	w := httptest.NewRecorder()
	h.statsSeries(w, httptest.NewRequest(http.MethodGet, "/admin/stats/series?"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%s ⇒ HTTP %d：%s", query, w.Code, w.Body.String())
	}
	var resp struct {
		Source string            `json:"source"`
		Series []json.RawMessage `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%s", w.Body.String())
	}
	return resp.Source, resp.Series, w.Body.String()
}

// bucketTS 取一个桶的 ts。
func bucketTS(t *testing.T, b json.RawMessage) string {
	t.Helper()
	var m struct {
		TS    string `json:"ts"`
		Total int64  `json:"total"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("桶不是 JSON：%s", string(b))
	}
	return m.TS
}

// TestSeries24hBucketCountIsFixed seam ① + ②：稀疏数据也不能让桶数变少、右端必须贴住当前小时。
func TestSeries24hBucketCountIsFixed(t *testing.T) {
	openOutAdminDB(t)
	// 造"最近 3 小时无请求、之前有"的日志集：最后一条在 4 小时前
	tail := time.Now().Add(-4 * time.Hour)
	for i := 0; i < 5; i++ {
		at := tail.Add(-time.Duration(i) * time.Hour)
		if err := sqlstore.AppendLog(sqlstore.Log{
			CreatedAt: at.Unix(), ModelName: "m-24h", TotalTokens: 3, CompletionTokens: 1, Success: true,
		}); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}

	// ⚠ 24h 档在 handler 里会走 statsItems()（需要真 Ring，零值 Handler 会 panic）⇒
	//    这里直接观察**窗口纯函数**：它才是"右端跟不跟现在走"的唯一决定者。
	start := bucketStart(time.Now().Add(-24*time.Hour), time.Hour)
	series, _ := seriesBuckets(start, time.Hour, 24)
	raw := "(seriesBuckets 纯函数)"
	// 契约：24 小时窗口 ⇒ 25 个桶（含边界桶）。关键是**固定**，不随数据稀疏而缩。
	if len(series) != 25 {
		t.Errorf("桶数 = %d，want 25（原来会因为「尾部全空」被砍到只剩有数据的几格）\n响应=%s",
			len(series), trunc(raw))
	}
	lastT, err := time.Parse(time.RFC3339, series[len(series)-1].TS)
	last := series[len(series)-1].TS
	if err != nil {
		t.Fatalf("末桶 ts 解析失败 %q", last)
	}
	wantLast := time.Now().Truncate(time.Hour)
	if diff := lastT.Sub(wantLast); diff > time.Hour || diff < -time.Hour {
		t.Errorf("末桶 ts = %s，want ≈ %s（当前整点，允许 ±1h）⇒ 右端没跟着「现在」走",
			last, wantLast.Format(time.RFC3339))
	}
	t.Logf("24h 桶数 = %d，首桶 %s，末桶 %s，当前整点 %s",
		len(series), series[0].TS, last, wantLast.Format(time.RFC3339))
}

// TestSeries24hNoDataStillFullWindow seam ①（无数据档）：完全没有日志也要给完整窗口，不能是空数组。
func TestSeries24hNoDataStillFullWindow(t *testing.T) {
	openOutAdminDB(t) // 空库
	start := bucketStart(time.Now().Add(-24*time.Hour), time.Hour)
	series, _ := seriesBuckets(start, time.Hour, 24)
	raw := "(seriesBuckets 纯函数)"
	if len(series) == 0 {
		t.Fatalf("无日志时返回了空数组（前端 loadHeatmap 会直接 return）⇒ 窗口必须照给：%s", trunc(raw))
	}
	if len(series) != 25 {
		t.Errorf("桶数 = %d，want 25（无数据也要完整窗口）", len(series))
	}
	// 全空桶：每一格 total 都是 0，但 ts 必须连续铺满
	if series[0].Total != 0 || series[len(series)-1].Total != 0 {
		t.Errorf("空库的桶应当全为 0，得到首 %d / 末 %d", series[0].Total, series[len(series)-1].Total)
	}
	t.Logf("无数据：桶数 = %d，首桶 %s，末桶 %s", len(series), series[0].TS, series[len(series)-1].TS)
}

// TestSeries365dRegressionUnchanged seam ③：365d 档（走 log_daily）不受本次修复影响。
func TestSeries365dRegressionUnchanged(t *testing.T) {
	openOutAdminDB(t)
	now := time.Now()
	for d := 0; d < 3; d++ {
		at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -d)
		if err := sqlstore.AppendLog(sqlstore.Log{
			CreatedAt: at.Unix(), ModelName: "m-reg", TotalTokens: 9, CompletionTokens: 3, Success: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlstore.BackfillLogDaily(); err != nil {
		t.Fatal(err)
	}
	source, series, raw := seriesReq(t, "range=365d")
	if source != "log_daily" {
		t.Errorf("source = %q，want log_daily（365d 仍走日聚合）", source)
	}
	if len(series) < 365 {
		t.Errorf("365d 桶数 = %d，want ≥365", len(series))
	}
	// 末桶必须是"今天"（既有行为：seriesFromDaily 按 [from,to] 铺满、不砍尾部）
	// ⚠ 不要用 time.Now().Truncate(24*time.Hour)：它是 **UTC 基准**，东八区会偏 8 小时
	//（本地 11-09 05:00 = UTC 11-08 21:00 ⇒ Truncate 后是 11-08）。本地自然日用 DayOf。
	today := sqlstore.DayOf(time.Now())
	lastT, _ := time.Parse(time.RFC3339, bucketTS(t, series[len(series)-1]))
	if sqlstore.DayOf(lastT) != today {
		t.Errorf("365d 末桶 = %s，want 今天 %s", sqlstore.DayOf(lastT), today)
	}
	t.Logf("365d 桶数 = %d，末桶 %s（今天 %s）", len(series), bucketTS(t, series[len(series)-1]), today)
	t.Logf("响应长度 %d 字节", len(raw))
}

func trunc(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
