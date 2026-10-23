// prune_groups_test.go 守 P5 的引用完整性：删分组 ⇒ key 里的引用一起消失。
//
// # 三条硬语义
//
//	① 清理后 key.Groups 里**没有**那个 ID（删除动作的一半，漏了就是悬空）
//	② 因清理变空的 key 必须被**报告出来**（权限放大，不能静默）
//	③ 只清给定的 ID，别的一律不动（那把别组的 key 扫平了是灾难）
//
// 第 ② 条最容易做错：把 Groups 清空在数据上等于"不限制"，
// 也就是从"只能用这几个分组的模型"变成"全部模型都能用"。
// 用户的操作是"删掉一个分组"，完全不会预期某把 key 的权限被放开。
package apikey

import (
	"testing"
)

// TestPruneGroupsRemovesRefs 基本语义：引用被移除、其它分组保留。
func TestPruneGroupsRemovesRefs(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-keep", "g-gone"}); err != nil {
		t.Fatal(err)
	}

	res, err := s.PruneGroups([]string{"g-gone"})
	if err != nil {
		t.Fatalf("PruneGroups: %v", err)
	}
	if res.KeysTouched != 1 {
		t.Errorf("应影响 1 把 key，实际 %d", res.KeysTouched)
	}
	if !equalStrings(res.GroupsGone, []string{"g-gone"}) {
		t.Errorf("GroupsGone 应为 [g-gone]，实际 %v", res.GroupsGone)
	}
	groups, _ := s.GroupsOf(k.ID)
	if !equalStrings(groups, []string{"g-keep"}) {
		t.Errorf("★ 引用必须被清掉（否则悬空 ⇒ 该 key 调用全 403），实际 %v", groups)
	}
	// 没变空 ⇒ 不该报"变不限制"
	if len(res.KeysUnrestricted) != 0 {
		t.Errorf("没变空的 key 不该被报成不限制，实际 %v", res.KeysUnrestricted)
	}
}

// ★ ② 清理后变空的 key 必须被报告（权限放大，不能静默）。
func TestPruneGroupsReportsUnrestricted(t *testing.T) {
	s := newTestStore(t)
	k1, _ := s.Create("only-gone", 0, 0, nil)
	k2, _ := s.Create("has-other", 0, 0, nil)
	for _, k := range []*Key{k1, k2} {
		if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-gone"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateWithGroups(k2.ID, nil, nil, nil, []string{"g-gone", "g-other"}); err != nil {
		t.Fatal(err)
	}

	res, err := s.PruneGroups([]string{"g-gone"})
	if err != nil {
		t.Fatal(err)
	}
	if res.KeysTouched != 2 {
		t.Errorf("两把 key 都被改过，实际 %d", res.KeysTouched)
	}
	// 只有 k1 变空
	if !equalStrings(res.KeysUnrestricted, []string{k1.ID}) {
		t.Fatalf("★ 只有 k1 会变空，必须且只能报告它；实际 %v（key1=%s key2=%s）",
			res.KeysUnrestricted, k1.ID, k2.ID)
	}
	// 语义确认：k1 现在是"不限制"
	if g, _ := s.GroupsOf(k1.ID); len(g) != 0 {
		t.Errorf("k1 的分组应为空，实际 %v", g)
	}
	if g, _ := s.GroupsOf(k2.ID); !equalStrings(g, []string{"g-other"}) {
		t.Errorf("k2 应只剩 g-other，实际 %v", g)
	}
}

// ★ ③ 只清给定的 ID，别的引用一律不动（扫平别组是灾难）。
func TestPruneGroupsOnlyTouchesGivenIDs(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, nil)
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-a", "g-b", "g-c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneGroups([]string{"g-b"}); err != nil {
		t.Fatal(err)
	}
	groups, _ := s.GroupsOf(k.ID)
	if !equalStrings(groups, []string{"g-a", "g-c"}) {
		t.Errorf("★ 只该移除 g-b，实际 %v", groups)
	}
}

// 没命中任何 key ⇒ 什么都不动、不写盘、报告 KeysTouched=0。
func TestPruneGroupsNoMatchIsNoop(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, nil)
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-a"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.PruneGroups([]string{"g-nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.KeysTouched != 0 || len(res.GroupsGone) != 0 || len(res.KeysUnrestricted) != 0 {
		t.Errorf("没命中时不该报告任何影响，实际 %+v", res)
	}
	if groups, _ := s.GroupsOf(k.ID); !equalStrings(groups, []string{"g-a"}) {
		t.Errorf("不该动任何引用，实际 %v", groups)
	}
}

// ★ 空输入绝不能把所有 key 的分组清空（最危险的形态）。
func TestPruneGroupsEmptyInputNoop(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		s := newTestStore(t)
		k, _ := s.Create("app", 0, 0, nil)
		if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-a"}); err != nil {
			t.Fatal(err)
		}
		res, err := s.PruneGroups(in)
		if err != nil {
			t.Fatal(err)
		}
		if res.KeysTouched != 0 {
			t.Errorf("空输入不该影响任何 key（%#v），实际 %d", in, res.KeysTouched)
		}
		if groups, _ := s.GroupsOf(k.ID); len(groups) != 1 {
			t.Errorf("★ 空输入把分组清空了 —— 那会让所有 key 变成不限制（%#v），实际 %v", in, groups)
		}
	}
}

// 空串 ID 要被忽略（前端传 ["", "g-a"] 时不该把空串当分组处理）。
func TestPruneGroupsIgnoresEmptyID(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, nil)
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-a"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.PruneGroups([]string{"", "  ", "g-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(res.GroupsGone, []string{"g-a"}) {
		t.Errorf("只该报告 g-a（空串被忽略），实际 %v", res.GroupsGone)
	}
}

// 多把 key 都受影响时全部被清（不只第一把）。
func TestPruneGroupsAffectsAllKeys(t *testing.T) {
	s := newTestStore(t)
	var ids []string
	for i := 0; i < 5; i++ {
		k, _ := s.Create("k", 0, 0, nil)
		if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-x", "g-keep"}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, k.ID)
	}
	res, err := s.PruneGroups([]string{"g-x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.KeysTouched != 5 {
		t.Errorf("应影响 5 把 key，实际 %d", res.KeysTouched)
	}
	for _, id := range ids {
		if groups, _ := s.GroupsOf(id); !equalStrings(groups, []string{"g-keep"}) {
			t.Errorf("key %s 应只剩 g-keep，实际 %v", id, groups)
		}
	}
}

// 清理结果要落盘（重启后不会又回到悬空）。
func TestPruneGroupsPersists(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/apikeys.json"
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := s.Create("app", 0, 0, nil)
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-gone", "g-keep"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PruneGroups([]string{"g-gone"}); err != nil {
		t.Fatal(err)
	}

	again, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	groups, _ := again.GroupsOf(k.ID)
	if !equalStrings(groups, []string{"g-keep"}) {
		t.Errorf("重启后应只剩 g-keep（清理要落盘），实际 %v", groups)
	}
}
