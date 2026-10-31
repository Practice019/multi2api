// ref_test.go 守「分组引用」解析的四条语义。
//
//  1. ID 命中 —— 既有（迁移写的就是 ID）必须一字不变
//  2. 名字命中 —— 界面按名字存下来的那些引用必须能被认出来，否则那把令牌的
//     并集为空 = 一个模型都不许用（本次修的就是这条）
//  3. ID 优先 —— 名字可以被伪装，ID 不行
//  4. 未命中不报错、不 panic —— 悬空引用交给 P5，不打断判定链
package groupstore

import (
	"path/filepath"
	"testing"
)

func TestResolveRefPrefersIDThenName(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, err := s.Create("alpha", "", []string{"m1"})
	if err != nil {
		t.Fatalf("Create alpha: %v", err)
	}
	// 故意把 beta 的**名字**取成 alpha 的 ID —— 用来钉住"ID 优先"。
	b, err := s.Create(a.ID, "", []string{"m2"})
	if err != nil {
		t.Fatalf("Create beta: %v", err)
	}

	list := s.List()

	for _, ref := range []string{a.ID, a.Name} {
		got, ok := ResolveRef(list, ref)
		if !ok || got != a.ID {
			t.Errorf("ResolveRef(%q) = (%q,%v)，期望 (%q,true)", ref, got, ok, a.ID)
		}
	}
	// 名字与另一个分组的 ID 撞车时，按 ID 解 —— 否则 alpha 会突然指向 beta。
	if got, ok := ResolveRef(list, b.Name); !ok || got != a.ID {
		t.Errorf("ID 与名字撞车时应按 ID 解：ResolveRef(%q) = (%q,%v)，期望 (%q,true)", b.Name, got, ok, a.ID)
	}
	// 首尾空白要容忍（表单里很容易带进来）。
	if got, ok := ResolveRef(list, "  alpha  "); !ok || got != a.ID {
		t.Errorf("应容忍首尾空白：ResolveRef(%q) = (%q,%v)", "  alpha  ", got, ok)
	}
	for _, ref := range []string{"", "   ", "不存在的分组", "g-deadbeef"} {
		if got, ok := ResolveRef(list, ref); ok {
			t.Errorf("ResolveRef(%q) 不该命中，实际 = %q", ref, got)
		}
	}
}

func TestGetRefByIDAndName(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g, err := s.Create("666 的模型", "备注", []string{"workbuddy/hy3"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, ref := range []string{g.ID, "666 的模型", " 666 的模型 "} {
		got, ok := s.GetRef(ref)
		if !ok || got == nil || got.ID != g.ID {
			t.Fatalf("GetRef(%q) = (%+v,%v)，期望 id=%s", ref, got, ok, g.ID)
		}
	}
	if _, ok := s.GetRef("没有这个分组"); ok {
		t.Error("未知引用不该命中")
	}
	if _, ok := s.GetRef(""); ok {
		t.Error("空引用不该命中")
	}
	if _, ok := (*Store)(nil).GetRef(g.ID); ok {
		t.Error("nil Store 不该 panic，应返回未命中")
	}

	// 深拷贝：改返回值不能污染库里的那条。
	got, _ := s.GetRef(g.ID)
	got.Models[0] = "被改了"
	if s.groups[g.ID].Models[0] == "被改了" {
		t.Error("GetRef 必须返回深拷贝（Models 被共享了）")
	}
}
