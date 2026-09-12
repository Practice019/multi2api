package codearts

import (
	"testing"
)

// TestMultiplierIsUpstreamFixedValue 锁定倍率来自上游下发值而非本地估算。
//
// 数据源：IDE 模型配置块的 credit.ratio_display（如 "0.7x"）。
// 实测三个模型为 0.7x，其余四个上游未下发。
//
// 这些是**固定值** —— 同一模型对所有账号一致，不随调用量变化。
// 因此不该用"从 token 消耗反推"的经验统计（那会随时间漂移）。
func TestMultiplierIsUpstreamFixedValue(t *testing.T) {
	want := map[string]struct {
		mult  float64
		known bool
	}{
		"GLM-5.2":             {0.7, true},
		"glm-5.2-sft-harmony": {0.7, true},
		"openpangu-2.0-pro":   {0.7, true},
		"openpangu-2.0-flash": {0.32, true},
		// 上游未下发（benefit 通道）—— 必须是 unknown 而不是 0（免费的语义）
		"deepseek-v4-flash-0731": {0, false},
		"deepseek-v4-pro-0813":   {0, false},
		"glm-5.3-flash":          {0, false},
	}

	all := AllModels()
	if len(all) != len(want) {
		t.Fatalf("模型数 = %d, 期望 %d", len(all), len(want))
	}
	for _, m := range all {
		w, ok := want[m.ID]
		if !ok {
			t.Errorf("多出未预期的模型 %q", m.ID)
			continue
		}
		if m.Multiplier != w.mult {
			t.Errorf("%s 倍率 = %v, 期望 %v", m.ID, m.Multiplier, w.mult)
		}
		if m.MultiplierKnown != w.known {
			t.Errorf("%s MultiplierKnown = %v, 期望 %v（未知与免费必须区分）",
				m.ID, m.MultiplierKnown, w.known)
		}
	}
}

// TestMultiplierForDistinguishesUnknownFromFree 锁定"未知 vs 免费"的语义区分。
//
// 这是展示正确性的关键：把"上游没给"显示成 "x0" 会被用户读成免费。
func TestMultiplierForDistinguishesUnknownFromFree(t *testing.T) {
	// 已知值
	if v, ok := MultiplierFor("GLM-5.2"); !ok || v != 0.7 {
		t.Errorf("GLM-5.2 -> (%v, %v), 期望 (0.7, true)", v, ok)
	}
	// 未知值：ok 必须是 false
	if v, ok := MultiplierFor("glm-5.3-flash"); ok {
		t.Errorf("glm-5.3-flash 的倍率应标记为未知，实际 (%v, %v)", v, ok)
	}
	// 完全未知的模型
	if _, ok := MultiplierFor("no-such-model"); ok {
		t.Error("未知模型应返回 ok=false")
	}
}

// TestBuildModelCatalogOnlyIncludesKnownMultipliers 确认未下发倍率的模型不进目录。
//
// 理由：ModelCatalog.MultiplierTable() 只收 Multiplier > 0 的条目，
// 若把未知写成 0，等于对外宣称"这个模型免费"。
// 宁可不显示，也不给一个会被读错的数字。
func TestBuildModelCatalogOnlyIncludesKnownMultipliers(t *testing.T) {
	cat := BuildModelCatalog()
	if cat == nil {
		t.Fatal("BuildModelCatalog 返回 nil")
	}

	ids := map[string]float64{}
	for _, e := range cat.Models {
		ids[e.ID] = e.Multiplier
	}

	// 三个有倍率的必须在
	for _, id := range []string{"GLM-5.2", "glm-5.2-sft-harmony", "openpangu-2.0-pro", "openpangu-2.0-flash"} {
		v, ok := ids[id]
		if !ok {
			t.Errorf("有倍率的 %s 未进目录", id)
			continue
		}
		wantMult := map[string]float64{
			"GLM-5.2": 0.7, "glm-5.2-sft-harmony": 0.7,
			"openpangu-2.0-pro": 0.7, "openpangu-2.0-flash": 0.32,
		}[id]
		if v != wantMult {
			t.Errorf("%s 目录里的倍率 = %v, 期望 %v", id, v, wantMult)
		}
	}

	// 四个未知的不能在
	for _, id := range []string{"deepseek-v4-flash-0731", "deepseek-v4-pro-0813", "glm-5.3-flash"} {
		if _, ok := ids[id]; ok {
			t.Errorf("倍率未知的 %s 不应进目录（会被误读为免费）", id)
		}
	}

	// MultiplierTable 应能查到且有值
	tbl := cat.MultiplierTable()
	if tbl == nil {
		t.Fatal("MultiplierTable 返回 nil")
	}
	if v := tbl["GLM-5.2"]; v != 0.7 {
		t.Errorf("MultiplierTable[GLM-5.2] = %v, 期望 0.7", v)
	}
}
