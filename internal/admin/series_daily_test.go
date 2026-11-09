package admin

// series_daily_test.go —— T-386：`/admin/stats/series` 的天桶档改读 log_daily。
//
// 三条 seam（全部只用公共边界：HTTP handler + sqlstore 的公开函数）：
//  ① 365d ⇒ source=="log_daily"、桶数 ≥365、**最早那天有数**（原来它永远是空的）
//  ② 明细被裁光 ⇒ 365d 仍有数、source 仍是 log_daily（"裁剪不影响聚合"）
//  ③ log_daily 为空 ⇒ seriesFromDaily 返回 ok=false（调用方回落到 logfile）

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlstore "workbuddy2api/internal/store"
)

// openDailyAdminDB 开临时库 + 建表（系列测试与 store 测试共用同一套公开 API）。
func openDailyAdminDB(t *testing.T) {
	t.Helper()
	prev := sqlstore.DB
	if err := sqlstore.Open(filepath.Join(t.TempDir(), "series.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sqlstore.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlstore.DB != nil {
			if s, err := sqlstore.DB.DB(); err == nil {
				_ = s.Close()
			}
		}
		sqlstore.DB = prev
	})
}

// seed400Days 造 400 天的明细（每天 2 条：1 成功 1 失败，total_tokens 3/0）。
func seed400Days(t *testing.T) time.Time {
	t.Helper()
	oldest := time.Now().AddDate(0, 0, -400)
	for i := 0; i < 400; i++ {
		at := oldest.AddDate(0, 0, i).Add(12 * time.Hour)
		if err := sqlstore.AppendLog(sqlstore.Log{
			CreatedAt: at.Unix(), ModelName: "m-day", TotalTokens: 3, PromptTokens: 3,
			Success: true, CostQuota: 1,
		}); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
		if err := sqlstore.AppendLog(sqlstore.Log{
			CreatedAt: at.Unix(), ModelName: "m-day", Success: false, ErrKind: "upstream",
		}); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}
	return oldest
}

// TestSeries365FromDaily ① 365 天能画满（含"最早那天有数"）。
func TestSeries365FromDaily(t *testing.T) {
	openDailyAdminDB(t)
	oldest := seed400Days(t)
	days, err := sqlstore.BackfillLogDaily()
	if err != nil || days < 400 {
		t.Fatalf("回填 = %d 天 err=%v，want ≥400", days, err)
	}

	h := &Handler{}
	w := httptest.NewRecorder()
	// ⚠ range=365d 时 handler 在读 log_daily 后就返回了（不会碰 h.cfg 的明细路径）
	h.statsSeries(w, httptest.NewRequest(http.MethodGet, "/admin/stats/series?range=365d", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP = %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Range  string            `json:"range"`
		Bucket string            `json:"bucket"`
		Source string            `json:"source"`
		Series []json.RawMessage `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%s", w.Body.String())
	}
	if resp.Source != "log_daily" {
		t.Errorf("source = %q，want log_daily", resp.Source)
	}
	if len(resp.Series) < 365 {
		t.Errorf("series 桶数 = %d，want ≥365（这就是「365 天热力图」的数据基础）", len(resp.Series))
	}
	// 最早那天的桶必须有数（不是全 0 空桶）
	var firstBucket struct {
		TS     string `json:"ts"`
		Total  int64  `json:"total"`
		Tokens int64  `json:"tokens"`
		Fail   int64  `json:"fail"`
	}
	if err := json.Unmarshal(resp.Series[0], &firstBucket); err != nil {
		t.Fatal(err)
	}
	if firstBucket.Total != 2 || firstBucket.Tokens != 3 || firstBucket.Fail != 1 {
		t.Errorf("最早桶 = %+v，want total=2 tokens=3 fail=1", firstBucket)
	}
	t.Logf("首桶 %s", string(resp.Series[0]))
	t.Logf("尾桶 %s", string(resp.Series[len(resp.Series)-1]))
	t.Logf("桶数 %d，source=%s，range=%s，bucket=%s", len(resp.Series), resp.Source, resp.Range, resp.Bucket)
	t.Logf("（最早的明细日期是 %s）", sqlstore.DayOf(oldest))
}

// TestSeries365SurvivesPrune ② 明细被裁光 ⇒ 365d 仍有数（裁剪不影响聚合）。
func TestSeries365SurvivesPrune(t *testing.T) {
	openDailyAdminDB(t)
	seed400Days(t)
	if _, err := sqlstore.BackfillLogDaily(); err != nil {
		t.Fatal(err)
	}
	// 把所有明细删掉（等价于保留期之外的都裁掉了）
	if err := sqlstore.DB.Exec(`DELETE FROM logs`).Error; err != nil {
		t.Fatal(err)
	}
	var left int64
	sqlstore.DB.Raw(`SELECT COUNT(*) FROM logs`).Scan(&left)
	if left != 0 {
		t.Fatalf("logs 仍有 %d 行（模拟裁剪失败）", left)
	}

	h := &Handler{}
	w := httptest.NewRecorder()
	h.statsSeries(w, httptest.NewRequest(http.MethodGet, "/admin/stats/series?range=365d", nil))
	var resp struct {
		Source string            `json:"source"`
		Series []json.RawMessage `json:"series"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON：%s", w.Body.String())
	}
	if resp.Source != "log_daily" {
		t.Errorf("source = %q，want log_daily", resp.Source)
	}
	if len(resp.Series) < 365 {
		t.Errorf("明细删光后 series 只剩 %d 桶 ⇒ 日聚合没起作用", len(resp.Series))
	}
	if !strings.Contains(string(resp.Series[0]), `"total":2`) {
		t.Errorf("最早桶丢数了：%s", string(resp.Series[0]))
	}
	t.Logf("明细已删光，series 仍有 %d 桶，首桶 %s", len(resp.Series), string(resp.Series[0]))
}

// TestSeriesFallsBackWhenDailyEmpty ③ log_daily 为空 ⇒ 回落到明细路径（不报错）。
func TestSeriesFallsBackWhenDailyEmpty(t *testing.T) {
	openDailyAdminDB(t)
	if _, ok := (&Handler{}).seriesFromDaily(365*24*time.Hour, 24*time.Hour); ok {
		t.Error("log_daily 为空时 seriesFromDaily 应返回 ok=false（让调用方回落）")
	}
}
