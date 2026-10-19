// mapping_test.go 守卫映射层的五条语义：
//
//	① 未命中 ⇒ 原样透传（不映射，也不是错误）
//	② 链条 A→B→C 走到不动点 C
//	③ 环 A→B→A 被拒绝（Validate 与 Resolve 都要拦）
//	④ 自指 A→A 是空操作，**不是环**（否则合理配置会被打死）
//	⑤ 反向表在一对多时给冲突清单，不静默挑一个
package modelmap

import (
	"errors"
	"testing"
)

func TestResolvePassthroughWhenUnmapped(t *testing.T) {
	m := Mapping{"pub": "real"}
	got, changed, err := m.Resolve("不在表里")
	if err != nil || changed || got != "不在表里" {
		t.Fatalf("未命中应原样透传，实际 %q changed=%v err=%v", got, changed, err)
	}
	// 空表（nil）同样透传
	var nilMap Mapping
	if got, changed, err := nilMap.Resolve("x"); err != nil || changed || got != "x" {
		t.Fatalf("nil 表应原样透传，实际 %q changed=%v err=%v", got, changed, err)
	}
	if !nilMap.Identity() {
		t.Fatal("nil 表应被判为 Identity")
	}
}

func TestResolveChain(t *testing.T) {
	m := Mapping{"a": "b", "b": "c"}
	got, changed, err := m.Resolve("a")
	if err != nil {
		t.Fatalf("链条不该报错: %v", err)
	}
	if got != "c" || !changed {
		t.Fatalf("a 应解析到 c，实际 %q changed=%v", got, changed)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("链条是合法配置，Validate 不该报错: %v", err)
	}
}

func TestCycleIsRejected(t *testing.T) {
	m := Mapping{"a": "b", "b": "a"}
	if _, _, err := m.Resolve("a"); !errors.Is(err, ErrCycle) {
		t.Fatalf("应返回 ErrCycle，实际 %v", err)
	}
	if err := m.Validate(); !errors.Is(err, ErrCycle) {
		t.Fatalf("Validate 也应拦下环，实际 %v", err)
	}
	// 更长的环也要拦
	long := Mapping{"a": "b", "b": "c", "c": "a"}
	if err := long.Validate(); !errors.Is(err, ErrCycle) {
		t.Fatalf("三节点环也应被拦下，实际 %v", err)
	}
}

func TestSelfMapIsNoOpNotCycle(t *testing.T) {
	m := Mapping{"pub": "pub"}
	got, changed, err := m.Resolve("pub")
	if err != nil {
		t.Fatalf("自指不该判成环: %v", err)
	}
	if got != "pub" || changed {
		t.Fatalf("自指应是空操作，实际 %q changed=%v", got, changed)
	}
	if !m.Identity() {
		t.Fatal("只含自指的映射应判为 Identity")
	}
}

func TestReverseReportsConflicts(t *testing.T) {
	m := Mapping{"pub1": "real", "pub2": "real", "pub3": "other"}
	rev, conflicts := m.Reverse()
	if rev["other"] != "pub3" {
		t.Fatalf("一对一的项应正常反向，实际 %v", rev)
	}
	// 两个公开名指向同一上游名 ⇒ 保留字典序在前的那个，另一个进冲突清单
	if rev["real"] != "pub1" {
		t.Fatalf("冲突时应保留 pub1，实际 %q", rev["real"])
	}
	if len(conflicts) != 1 || conflicts[0].Upstream != "real" || conflicts[0].Dropped != "pub2" {
		t.Fatalf("应报 1 条冲突（real/pub2），实际 %+v", conflicts)
	}
}

// UpstreamFor 是选路阶段的便捷入口：改写与否要如实告诉调用方。
func TestUpstreamForReportsChange(t *testing.T) {
	m := Mapping{"pub": "real"}
	if got, ch := m.UpstreamFor("pub"); got != "real" || !ch {
		t.Fatalf("命中应报改写，实际 %q changed=%v", got, ch)
	}
	if got, ch := m.UpstreamFor("wild"); got != "wild" || ch {
		t.Fatalf("未命中应报未改写，实际 %q changed=%v", got, ch)
	}
	// 环的情况下不能悄悄把请求发出去（返回原名 + false，由调用方拒绝）
	bad := Mapping{"a": "b", "b": "a"}
	if got, ch := bad.UpstreamFor("a"); ch {
		t.Fatalf("成环时不该声称改写完成，实际 %q", got)
	}
}
