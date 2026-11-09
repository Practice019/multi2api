package gateway

import (
	"reflect"
	"testing"
)

// TestPickWinnersByUID 钉住 PickWinnersByUID 的行为契约（T-339）。
//
// # 这是 seam 测试
//
// seam = 本函数本身（公共边界）。它抓：去重保留"更好"的那份、首次出现顺序、按 uid 升序。
// 它**不抓**：各上游 betterAuth 的优劣判据（那是各家私有语义，各自测）、uid 怎么派生。
//
// # 期望值来源（独立真源，不是把实现重算一遍）
//
// 下面全是**手算样例**：输入与"人眼可验证"的输出都写成字面量。
func TestPickWinnersByUID(t *testing.T) {
	type item struct {
		uid     string
		version int
	}
	list := []item{
		{"B", 1},
		{"A", 1},
		{"B", 2}, // 同一 uid 的第二份，version 更大 ⇒ 应当胜出
		{"A", 1}, // 同 uid 同 version ⇒ 保留**先到**的那份（稳定）
	}
	// better：version 大者更好（同 version 时判定为"不更好"⇒ 保留先到的）
	got := PickWinnersByUID(list, func(x item) string { return x.uid }, func(a, b item) bool { return a.version > b.version })

	want := []item{{"A", 1}, {"B", 2}} // 手算：B 取 version 2；顺序按 uid 升序
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PickWinnersByUID = %+v，want %+v", got, want)
	}

	// 空输入 ⇒ 空输出（不是 nil）
	if out := PickWinnersByUID([]item{}, func(x item) string { return x.uid }, func(a, b item) bool { return false }); len(out) != 0 {
		t.Fatalf("空输入应回空切片，实际 %+v", out)
	}

	// 全不同 uid ⇒ 原样（且升序）
	list2 := []item{{"C", 1}, {"A", 1}, {"B", 1}}
	out2 := PickWinnersByUID(list2, func(x item) string { return x.uid }, func(a, b item) bool { return false })
	if want2 := []item{{"A", 1}, {"B", 1}, {"C", 1}}; !reflect.DeepEqual(out2, want2) {
		t.Fatalf("不同 uid 应升序原样，实际 %+v", out2)
	}
}
