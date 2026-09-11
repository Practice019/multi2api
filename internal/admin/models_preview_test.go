package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/upstream"
)

// GET /admin/models/preview 返回「模型 id → 成本倍率」，供前端在模型列表上标注倍率。
//
// 为什么不改 /v1/models：那是**对外**的 OpenAI 兼容端点，客户端按规范解析 data[]，
// 塞自定义字段属于污染公共契约（且会被部分客户端当作未知字段告警）。
// 倍率是网关自己的观测信息，走 admin 面。
//
// 与 /admin/stats 里 model_multipliers 的区别：
//   - stats 那份只含**窗口内被调用过**的模型（报表口径：钱花在哪）
//   - 这份是**全部目录模型**（选择口径：这个模型多贵），前端要在下拉框里标注所有选项
func TestModelsPreviewReturnsAllCatalogMultipliers(t *testing.T) {
	h, _ := newStatsHandler(t, nil)
	h.cfg.ModelCatalog = func() *upstream.ModelCatalog {
		return catalogWith(
			upstream.ModelCatalogEntry{ID: "deepseek-v4-pro", Multiplier: 0.51},
			upstream.ModelCatalogEntry{ID: "hy4-preview-f", Multiplier: 0}, // 免费：x0.00
			upstream.ModelCatalogEntry{ID: "kimi-k3-1", Multiplier: 1.62},
		)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/models/preview", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	list, ok := out["models"].([]any)
	if !ok {
		t.Fatalf("缺少 models 数组: %v", out)
	}
	if len(list) != 3 {
		t.Fatalf("应返回全部 3 个模型（不只被调用过的），得到 %d", len(list))
	}

	// 关键：**0 倍率也要保留**（x0.00 = 免费，是有意义的信息，不能当成"没有"）
	got := map[string]float64{}
	for _, e := range list {
		m, _ := e.(map[string]any)
		// 字段名是 "model"（沿用 ModelMultiplier 的既有 JSON 契约，
		// 与 /admin/stats 的 model_multipliers 保持同一形状）
		id, _ := m["model"].(string)
		v, _ := m["multiplier"].(float64)
		got[id] = v
	}
	if got["deepseek-v4-pro"] != 0.51 {
		t.Errorf("deepseek-v4-pro=%v want 0.51", got["deepseek-v4-pro"])
	}
	if v, exists := got["hy4-preview-f"]; !exists {
		t.Error("0 倍率的模型必须保留（免费是有意义的信息，不是缺失）")
	} else if v != 0 {
		t.Errorf("免费模型应为 0，得到 %v", v)
	}
	if got["kimi-k3-1"] != 1.62 {
		t.Errorf("kimi-k3-1=%v want 1.62", got["kimi-k3-1"])
	}
}

// 未接线时返回空表而不是报错 —— 前端拿不到倍率就只显示模型名，不该整块失败。
func TestModelsPreviewNilCatalogIsEmptyNotError(t *testing.T) {
	h, _ := newStatsHandler(t, nil)
	// ModelCatalog 保持 nil（未接线）

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/models/preview", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("未接线应返回 200 + 空表，得到 HTTP %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	if list, ok := out["models"].([]any); !ok || len(list) != 0 {
		t.Errorf("未接线时应为空数组，得到 %v", out["models"])
	}
}

// 目录拿不到（返回 nil）时同样退化为空表。
func TestModelsPreviewFetchFailureIsEmpty(t *testing.T) {
	h, _ := newStatsHandler(t, nil)
	h.cfg.ModelCatalog = func() *upstream.ModelCatalog { return nil }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/models/preview", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	if list, _ := out["models"].([]any); len(list) != 0 {
		t.Errorf("拿不到目录时应为空，得到 %v", list)
	}
}
