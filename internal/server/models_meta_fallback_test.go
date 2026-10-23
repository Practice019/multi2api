// models_meta_fallback_test.go 钉住「缺元数据时的兜底规则」。
//
// # 用户报的缺陷 4
//
//	004/deepseek-v4.1-flash-1017   ctx 与 max_output 都缺
//	001/deepseek-v4.1-flash-1017   连 created 都没有
//
// 实测根因：上游的 data.models（有元数据）与 agents[].models（有能力声明）
// 是**两个数组**。后者里有些名字能调通（实测 200）但不在前者 —— 于是没元数据。
//
// 兜底规则：取该上游已有模型的**众数**。它必然是个真实存在的值。
package server

import "testing"

// TestInferMetaForUsesMode 众数推断。
func TestInferMetaForUsesMode(t *testing.T) {
	existing := map[string][2]int64{
		"a": {200000, 32000},
		"b": {200000, 32000},
		"c": {1000000, 64000},
	}
	ctx, maxOut := inferMetaFor(existing)
	if ctx != 200000 {
		t.Errorf("ctx 众数应为 200000（出现两次），得到 %d", ctx)
	}
	if maxOut != 32000 {
		t.Errorf("maxOut 众数应为 32000，得到 %d", maxOut)
	}
}

// TestInferMetaForEmptyGivesZero 一条元数据都没有时返回 0（保持未知）。
//
// # 为什么这条必须存在
//
// trae 与 lobsterai 实测：所有条目 ctx 都是 0 —— 它们的上游接口
// 压根不返回窗口大小。这种情况下**无众数可推**，必须返回 0
// 让上层不写该字段，而不是编一个值。
//
// 编一个值比留空更糟：客户端会按它分配预算，而那个数字没有任何依据。
func TestInferMetaForEmptyGivesZero(t *testing.T) {
	ctx, maxOut := inferMetaFor(nil)
	if ctx != 0 || maxOut != 0 {
		t.Errorf("无元数据时必须返回 0，得到 %d/%d", ctx, maxOut)
	}
	ctx2, maxOut2 := inferMetaFor(map[string][2]int64{})
	if ctx2 != 0 || maxOut2 != 0 {
		t.Errorf("空表时必须返回 0，得到 %d/%d", ctx2, maxOut2)
	}
}

// TestInferMetaForTiePicksLarger 并列时取较大值。
//
// 并列取大：宁可给宽松预算，也不要让客户端按偏小的值提前截断
//（那种失败看起来像模型质量差，实际是我们报错了窗口）。
func TestInferMetaForTiePicksLarger(t *testing.T) {
	existing := map[string][2]int64{
		"a": {131072, 16384},
		"b": {1000000, 64000},
	}
	ctx, maxOut := inferMetaFor(existing)
	if ctx != 1000000 {
		t.Errorf("并列应取较大值 1000000，得到 %d", ctx)
	}
	if maxOut != 64000 {
		t.Errorf("并列应取较大值 64000，得到 %d", maxOut)
	}
}
