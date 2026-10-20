// model_map_test.go API key 的模型对外名映射（2026-10-21）
//
// # 用户的需求（原话）
//
//	「这些模型，开放的模型，对于这个 API key 开放的模型，我觉得需要有一个
//	 模型映射，就是对外的模型应该叫什么」
//
// # 这个文件钉的是哪些语义
//
//  1. `normalizeModelMap` 的规整：去空白、丢空项、**键值相同就省略**
//  2. 映射是**增量**能力：没配映射的 key 行为与从前完全一致
//  3. 清空语义：传空 map 清空、传 nil 不改（变参三态）
//
// ⚠ 第 3 条最容易写错：Go 的变参让"没传"和"传了 nil"看起来一样，
//
//	而"传了空 map"必须能与前两者区分开 —— 否则前端就没法把映射删干净。
package apikey

import "testing"

// TestNormalizeModelMapTrimsAndDropsEmpty 空白与空项必须丢掉。
//
// 用户在输入框里多打一个回车就会产出 `{"  ": "  "}`，若原样存下，
// `/v1/models` 会拿一个空串对外名去做去重与替换。
func TestNormalizeModelMapTrimsAndDropsEmpty(t *testing.T) {
	in := map[string]string{
		"  pro  ":   " csg/deepseek-v4.1-flash-1017 ",
		"":          "csg/x",
		"blank-val": "   ",
	}
	got := normalizeModelMap(in)
	if len(got) != 1 {
		t.Fatalf("★ 应只剩 1 项（去空白后有效的那条），实际 %d 项: %v", len(got), got)
	}
	if v, ok := got["pro"]; !ok || v != "csg/deepseek-v4.1-flash-1017" {
		t.Fatalf("★ 键值应被 trim：期望 pro → csg/deepseek-v4.1-flash-1017，实际 %v", got)
	}
}

// TestNormalizeModelMapDropsIdentityMapping 键值相同 ⇒ 删掉。
//
// `{"x": "x"}` 与"不映射"完全等价。留着只会让界面上出现一个
// "填了但毫无作用"的别名输入框，用户会以为没生效。
func TestNormalizeModelMapDropsIdentityMapping(t *testing.T) {
	got := normalizeModelMap(map[string]string{"same": "same", "a": "b"})
	if _, ok := got["same"]; ok {
		t.Fatalf("★ 键值相同的项应被省略，实际保留了: %v", got)
	}
	if got["a"] != "b" {
		t.Fatalf("真正的映射应保留，实际 %v", got)
	}
}

// TestNormalizeModelMapEmptyReturnsNil 空表 ⇒ nil（不是空 map）。
//
// 返回 nil 才能让 JSON 里**不出现**这个字段，老 key 写回时不产生 diff。
func TestNormalizeModelMapEmptyReturnsNil(t *testing.T) {
	if got := normalizeModelMap(nil); got != nil {
		t.Fatalf("nil 应返回 nil，实际 %v", got)
	}
	if got := normalizeModelMap(map[string]string{}); got != nil {
		t.Fatalf("空 map 应返回 nil，实际 %v", got)
	}
	// 全是无效项 ⇒ 也返回 nil
	if got := normalizeModelMap(map[string]string{"a": "a", " ": "x"}); got != nil {
		t.Fatalf("全无效项应返回 nil，实际 %v", got)
	}
}

// TestFirstModelMapVariadicNilVsEmpty ★ 变参三态的核心守卫。
//
//	不传        → nil（不改）
//	传 nil      → nil（不改）
//	传空 map    → nil（但对 Update 而言这表示"清空" —— 见下一个测试）
//	传非空 map  → 规整后的表
func TestFirstModelMapVariadicNilVsEmpty(t *testing.T) {
	if got := firstModelMap(); got != nil {
		t.Fatalf("不传应返回 nil，实际 %v", got)
	}
	if got := firstModelMap(nil); got != nil {
		t.Fatalf("传 nil 应返回 nil，实际 %v", got)
	}
	if got := firstModelMap(map[string]string{}); got != nil {
		t.Fatalf("传空 map 应返回 nil（规整后），实际 %v", got)
	}
	got := firstModelMap(map[string]string{"pro": "csg/x"})
	if got["pro"] != "csg/x" {
		t.Fatalf("非空 map 应保留，实际 %v", got)
	}
}

// TestUpdateModelMapThreeStates ★ Update 的三态：不改 / 清空 / 替换。
//
// 这是"用户点了保存却没生效"这类问题的直接来源，必须钉住。
func TestUpdateModelMapThreeStates(t *testing.T) {
	s := newTestStore(t)

	k, err := s.CreateOwned("", "k1", 0, 0, []string{"csg/x", "csg/y"},
		map[string]string{"pro": "csg/x", "lite": "csg/y"})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if k.ModelMap["pro"] != "csg/x" {
		t.Fatalf("创建时应写入映射，实际 %v", k.ModelMap)
	}

	// ① 不传 → 不改
	if err := s.Update(k.ID, nil, nil, nil); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	if m, _ := s.ModelMapOf(k.ID); m["pro"] != "csg/x" {
		t.Fatalf("★ 不传 modelMap 时不该动它，实际 %v", m)
	}

	// ② 传 nil → 不改
	if err := s.Update(k.ID, nil, nil, nil, nil); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	if m, _ := s.ModelMapOf(k.ID); m["pro"] != "csg/x" {
		t.Fatalf("★ 传 nil 时不该动它，实际 %v", m)
	}

	// ③ 传非空 → 整体替换
	if err := s.Update(k.ID, nil, nil, nil, map[string]string{"new": "csg/y"}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	m, _ := s.ModelMapOf(k.ID)
	if len(m) != 1 || m["new"] != "csg/y" {
		t.Fatalf("★ 应整体替换成 {new: csg/y}，实际 %v", m)
	}

	// ④ 传空 map → 清空
	if err := s.Update(k.ID, nil, nil, nil, map[string]string{}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	if m, _ := s.ModelMapOf(k.ID); m != nil {
		t.Fatalf("★ 传空 map 应清空映射，实际 %v", m)
	}
}

// TestModelMapOfReturnsCopy 返回副本，改它不能污染 store。
func TestModelMapOfReturnsCopy(t *testing.T) {
	s := newTestStore(t)
	k, err := s.CreateOwned("", "k1", 0, 0, nil, map[string]string{"pro": "csg/x"})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	m, _ := s.ModelMapOf(k.ID)
	m["pro"] = "被改坏了"
	m["注入"] = "csg/evil"

	again, _ := s.ModelMapOf(k.ID)
	if again["pro"] != "csg/x" {
		t.Fatalf("★ 调用方改副本污染了 store：%v", again)
	}
	if _, ok := again["注入"]; ok {
		t.Fatalf("★ 调用方往副本里加键污染了 store：%v", again)
	}
}

// TestModelMapNilStoreReturnsFalse nil store 不该 panic。
func TestModelMapNilStoreReturnsFalse(t *testing.T) {
	var s *Store
	if m, ok := s.ModelMapOf("whatever"); m != nil || ok {
		t.Fatalf("nil store 应返回 (nil,false)，实际 (%v,%v)", m, ok)
	}
}
