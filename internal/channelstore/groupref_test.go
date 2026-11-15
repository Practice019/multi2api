// groupref_test.go 分组引用兼容解析的判据（U-09）。
//
// # 核心判据是第 ③ 条
//
// 「冲突不得任选」—— 名字匹配到多个分组时**不许挑一个**。
// 挑错了就是"用户以为 A 组、实际是 B 组"，两个组的模型集合不同 ⇒
// 权限判错且**毫无信号**（不报错，只是多给或少给了模型）。
package channelstore

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/groupstore"
)

func grp(id, name string) *groupstore.Group {
	return &groupstore.Group{ID: id, Name: name, PolicyVersion: 2}
}

// ① 精确 ID 命中。
func TestResolveGroupRefsByID(t *testing.T) {
	groups := []*groupstore.Group{grp("g-a", "alpha"), grp("g-b", "beta")}
	res := ResolveGroupRefs([]string{"g-a"}, groups)
	if !reflect.DeepEqual(res.IDs, []string{"g-a"}) {
		t.Fatalf("IDs = %v，want [g-a]", res.IDs)
	}
	if !res.OK() {
		t.Fatalf("不该有未解析/歧义：%+v", res)
	}
}

// ② 旧形态：按名字解析（唯一时）。
func TestResolveGroupRefsByName(t *testing.T) {
	groups := []*groupstore.Group{grp("g-a", "alpha"), grp("g-b", "beta")}
	res := ResolveGroupRefs([]string{"alpha", "beta"}, groups)
	if !reflect.DeepEqual(res.IDs, []string{"g-a", "g-b"}) {
		t.Fatalf("IDs = %v，want [g-a g-b]（且必须保输入顺序）", res.IDs)
	}
	if !res.OK() {
		t.Fatalf("不该有未解析/歧义：%+v", res)
	}
}

// ★ ③ 名字撞车 ⇒ 歧义，**不许任选**。
func TestResolveGroupRefsDuplicateNameIsAmbiguous(t *testing.T) {
	// 两个分组同名（早期数据可以，后来才加唯一约束）。
	groups := []*groupstore.Group{grp("g-a", "dup"), grp("g-b", "dup")}
	res := ResolveGroupRefs([]string{"dup"}, groups)

	if len(res.IDs) != 0 {
		t.Fatalf("★ 同名匹配到 %d 个分组却解析出了 ID %v —— 这就是「任选」，"+
			"挑错了等于用户以为 A 组、实际是 B 组，而两个组的模型集合不同", 2, res.IDs)
	}
	if !reflect.DeepEqual(res.Ambiguous, []string{"dup"}) {
		t.Fatalf("★ 必须记进 Ambiguous，实际 %+v", res)
	}
	if res.OK() {
		t.Fatal("★ 有歧义时 OK() 必须为 false")
	}
}

// ④ 未知引用 ⇒ Unresolved（不是静默丢弃）。
func TestResolveGroupRefsUnknownIsUnresolved(t *testing.T) {
	groups := []*groupstore.Group{grp("g-a", "alpha")}
	res := ResolveGroupRefs([]string{"nope"}, groups)
	if len(res.IDs) != 0 || !reflect.DeepEqual(res.Unresolved, []string{"nope"}) {
		t.Fatalf("未知引用应进 Unresolved，实际 %+v", res)
	}
}

// ⑤ 空白项丢弃（否则空串会让令牌进"受限"分支 ⇒ 一个模型都不许用）。
func TestResolveGroupRefsDropsBlank(t *testing.T) {
	groups := []*groupstore.Group{grp("g-a", "alpha")}
	res := ResolveGroupRefs([]string{"", "  ", "g-a"}, groups)
	if !reflect.DeepEqual(res.IDs, []string{"g-a"}) {
		t.Fatalf("IDs = %v，want [g-a]", res.IDs)
	}
	if len(res.Unresolved) != 0 {
		t.Fatalf("空串不该进 Unresolved，实际 %v", res.Unresolved)
	}
}

// ⑥ 去重（按名字与按 ID 指向同一个分组时只出现一次）。
func TestResolveGroupRefsDedupes(t *testing.T) {
	groups := []*groupstore.Group{grp("g-a", "alpha")}
	res := ResolveGroupRefs([]string{"g-a", "alpha", "g-a"}, groups)
	if !reflect.DeepEqual(res.IDs, []string{"g-a"}) {
		t.Fatalf("IDs = %v，want [g-a]（按 ID 与按名字指向同一组，只算一次）", res.IDs)
	}
}

// ★ ⑦ ID 优先于名字：即使有分组名字恰好等于另一个分组的 ID。
func TestResolveGroupRefsIDWinsOverName(t *testing.T) {
	// 分组 A 的 ID 是 "X"，分组 B 的名字也叫 "X"。
	groups := []*groupstore.Group{grp("X", "alpha"), grp("g-b", "X")}
	res := ResolveGroupRefs([]string{"X"}, groups)

	if !reflect.DeepEqual(res.IDs, []string{"X"}) {
		t.Fatalf("★ 引用 \"X\" 应命中**分组 ID** X，实际 %v —— "+
			"ID 永远不歧义，所以它必须优先于名字", res.IDs)
	}
	if len(res.Ambiguous) != 0 {
		t.Fatalf("ID 命中之后不该再报歧义，实际 %v", res.Ambiguous)
	}
}

// ⑧ ResolvePolicyGroups：有歧义时**不降级成部分解析**。
//
// ⚠ 这条防的是"任选"的另一种形态：明明有一项歧义，却用"能解析的那部分"
// 去判定权限 —— 它不报错，只是少给了几个模型，比报错更难发现。
func TestResolvePolicyGroupsDoesNotPartiallyApply(t *testing.T) {
	groups := []*groupstore.Group{grp("g-a", "dup"), grp("g-b", "dup")}
	policy := RoutingPolicy{PolicyVersion: 2, Groups: []string{"dup", "g-a"}}

	got, res := ResolvePolicyGroups(policy, groups)

	if len(res.Ambiguous) == 0 {
		t.Fatal("前置条件不成立：应有歧义")
	}
	if !reflect.DeepEqual(got.Groups, policy.Groups) {
		t.Fatalf("★ 有歧义时 Groups 不该被改写（实际 %v）—— "+
			"用「能解析的那部分」去判定权限是「任选」的另一种形态", got.Groups)
	}

	// 无歧义时才真的替换。
	clean := RoutingPolicy{PolicyVersion: 2, Groups: []string{"alpha"}}
	groups2 := []*groupstore.Group{grp("g-a", "alpha")}
	got2, res2 := ResolvePolicyGroups(clean, groups2)
	if !res2.OK() || !reflect.DeepEqual(got2.Groups, []string{"g-a"}) {
		t.Fatalf("无歧义时应替换成 ID，实际 %v (%+v)", got2.Groups, res2)
	}
}
