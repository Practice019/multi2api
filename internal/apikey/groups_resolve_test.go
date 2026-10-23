// groups_resolve_test.go 守「分组 → 模型并集」这一层的三条边界。
//
// # 为什么这一层要有自己的测试
//
// server 侧那唯一一处判定（allowedModelsForRequest）会信任这里返回的并集。
// 三条最容易写歪、且**后果最严重**的边界：
//
//	① 未接线（没注入 groupstore 的解析函数）必须是"降级"而不是"拒绝"
//	   否则运维改一个配置，用户的调用全 403，而错误信息只会说"模型不允许"
//
//	② 悬空分组（key 指向一个已被删的分组）不能 panic、不能报错
//	   悬空是 P5 引用完整性的问题。在这里炸掉等于"删一个分组把调用全打断"
//
//	③ 空 groups（没配分组）= 不限制，**不是**一个都不许
//	   存量 key 全都没有这个字段；读成"一个都不许"会一次升级打死所有调用方
package apikey

import (
	"strings"
	"testing"
)

// fakeGroups 造一个"分组 ID → 模型清单"的固定表（含一个悬空 ID 的行为：
// 不在表里的 ID 返回 nil，与真实实现"分组已被删"一致）。
func fakeGroups(table map[string][]string) GroupModelsFunc {
	return func(id string) []string { return table[id] }
}

// TestGroupsThreeStateAndDefaults 存储层：Groups 的默认值、规整、三态。
func TestGroupsThreeStateAndDefaults(t *testing.T) {
	s := newTestStore(t)

	// ① 不传 groups ⇒ 不限制（既有 Create 签名没有 groups 参数，等价于空）
	k, err := s.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := s.GroupsOf(k.ID); !ok || len(g) != 0 {
		t.Errorf("新建的 key 应当没有分组（不限制），实际 %v ok=%v", g, ok)
	}
	if _, ok := s.GroupsOf("sk-nope"); ok {
		t.Error("不存在的 key 应报 ok=false —— 调用方要靠它区分「不限制」与「没这把 key」")
	}

	// ② 规整：去空白、丢空项、去重、**保序**（不排序）
	g1 := []string{" g-b ", "", "g-a", "g-b", "  "}
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, g1); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GroupsOf(k.ID)
	if len(got) != 2 || got[0] != "g-b" || got[1] != "g-a" {
		t.Fatalf("应规整成 [g-b g-a]（去重且**保持用户勾选顺序**），实际 %v", got)
	}

	// ③ 只改额度：groups 必须原样不动（与 models 的三态同一条理由）
	l := int64(100)
	if err := s.UpdateWithGroups(k.ID, &l, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GroupsOf(k.ID); len(got) != 2 {
		t.Errorf("nil groups 表示「不改」，实际被改成了 %v —— "+
			"那会让「改一次额度」顺手把权限放大成不限制", got)
	}

	// ④ 显式 [] ⇒ 清空 = 恢复不限制
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GroupsOf(k.ID); len(got) != 0 {
		t.Errorf("显式 [] 应当清空分组，实际 %v", got)
	}

	// ⑤ 老方法 Update 不碰 groups（既有调用方零改动，行为不变）
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(k.ID, nil, nil, []string{"glm-5.2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GroupsOf(k.ID); len(got) != 1 || got[0] != "g-x" {
		t.Errorf("老方法 Update 不该动分组（接线前 admin 还在用它），实际 %v", got)
	}
}

// TestGroupsOfReturnsCopy GroupsOf 必须返回副本。
func TestGroupsOfReturnsCopy(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, nil)
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GroupsOf(k.ID)
	got[0] = "被改掉了"
	again, _ := s.GroupsOf(k.ID)
	if again[0] != "g-1" {
		t.Errorf("GroupsOf 必须返回副本，实际存储被改成了 %q", again[0])
	}
}

// TestGroupsPersistAndListView 分组要落盘、要出现在列表视图里。
//
// "出现在 List" 单独断言：本文件已经踩过一次同类坑（ModelMap 曾在 List 里漏掉，
// 用户报"设置了但编辑时是空的"，而数据其实好好地在盘上）。
func TestGroupsPersistAndListView(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/apikeys.json"
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := s.Create("app", 0, 0, nil)
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-1", "g-2"}); err != nil {
		t.Fatal(err)
	}

	// 列表视图要带出来
	views := s.List()
	if len(views) != 1 || len(views[0].Groups) != 2 {
		t.Errorf("View 必须带出 Groups（漏掉它前端就永远回填不出来），实际 %+v", views)
	}

	// 重启后还在
	again, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := again.GroupsOf(k.ID); !ok || len(got) != 2 {
		t.Errorf("重启后分组应还在，实际 %v ok=%v", got, ok)
	}
}

// TestUnionGroupModels 并集：多分组、去重、保序、空项。
func TestUnionGroupModels(t *testing.T) {
	s := newTestStore(t)
	s.SetGroupModels(fakeGroups(map[string][]string{
		"g-1": {"a", "b"},
		"g-2": {"b", "c"},
		"g-3": {},
	}))

	cases := []struct {
		name   string
		groups []string
		want   []string
	}{
		{"单分组", []string{"g-1"}, []string{"a", "b"}},
		{"两分组去重", []string{"g-1", "g-2"}, []string{"a", "b", "c"}},
		{"顺序按分组顺序", []string{"g-2", "g-1"}, []string{"b", "c", "a"}},
		{"空分组不影响", []string{"g-3", "g-1"}, []string{"a", "b"}},
		{"悬空分组不炸", []string{"g-gone"}, nil},
		{"空 groups", nil, nil},
		{"全是空串", []string{"", "  "}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.UnionGroupModels(c.groups)
			if !equalStrings(got, c.want) {
				t.Errorf("期望 %v，实际 %v", c.want, got)
			}
		})
	}
}

// TestUnionGroupModelsNotWired 未接线时必须返回空，且 HasGroupResolver=false。
//
// 调用方据此**降级到白名单**，而不是把这把 key 判成"一个模型都不许"。
func TestUnionGroupModelsNotWired(t *testing.T) {
	s := newTestStore(t)
	if s.HasGroupResolver() {
		t.Fatal("没注入时 HasGroupResolver 必须是 false")
	}
	if got := s.UnionGroupModels([]string{"g-1"}); got != nil {
		t.Errorf("未接线时应返回 nil，实际 %v", got)
	}
	// 注入后再撤掉（传 nil）也要回到未接线状态
	s.SetGroupModels(fakeGroups(map[string][]string{"g-1": {"a"}}))
	if !s.HasGroupResolver() {
		t.Fatal("注入后 HasGroupResolver 必须是 true")
	}
	s.SetGroupModels(nil)
	if s.HasGroupResolver() {
		t.Error("传 nil 应当回到未接线状态")
	}
}

// TestUnionGroupModelsNilStore nil Store 不 panic。
//
// 这不是洁癖：Handler 的 cfg.APIKeys 在单 key 部署下就是 nil，
// 而判定链会先判 nil 再调用 —— 但"先判 nil"漏写一处就是生产 panic。
func TestUnionGroupModelsNilStore(t *testing.T) {
	var s *Store
	if s.HasGroupResolver() {
		t.Error("nil Store 应回 false")
	}
	if got := s.UnionGroupModels([]string{"g-1"}); got != nil {
		t.Errorf("nil Store 应回 nil，实际 %v", got)
	}
	s.SetGroupModels(fakeGroups(nil)) // 不该 panic
}

// TestGroupModelsOf 三返回值：区分「没分组」与「分组里没模型」。
func TestGroupModelsOf(t *testing.T) {
	s := newTestStore(t)
	s.SetGroupModels(fakeGroups(map[string][]string{
		"g-1": {"a"},
		"g-e": {},
	}))
	k, _ := s.Create("app", 0, 0, nil)

	// 没分组
	groups, models, ok := s.GroupModelsOf(k.ID)
	if !ok || len(groups) != 0 || models != nil {
		t.Errorf("没分组的 key：groups/models 都应为空，实际 groups=%v models=%v ok=%v", groups, models, ok)
	}

	// 分组里没模型 —— ok=true 但 models 为空，调用方据此可以判"受限且为空"
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-e"}); err != nil {
		t.Fatal(err)
	}
	groups, models, ok = s.GroupModelsOf(k.ID)
	if !ok || len(groups) != 1 || len(models) != 0 {
		t.Errorf("空分组：groups 有 1 个、models 为空，实际 groups=%v models=%v ok=%v", groups, models, ok)
	}

	// 有模型
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-1"}); err != nil {
		t.Fatal(err)
	}
	_, models, _ = s.GroupModelsOf(k.ID)
	if !equalStrings(models, []string{"a"}) {
		t.Errorf("应解析出 [a]，实际 %v", models)
	}

	// 不存在的 key
	if _, _, ok := s.GroupModelsOf("sk-nope"); ok {
		t.Error("不存在的 key 应 ok=false")
	}
}

// TestGroupModelsFuncContract 注入函数的约定：返回切片被复制、空白项被清理。
//
// 清理空白项不是为了好看：分组里若有 "" 混进并集，它匹配不上任何模型，
// 但会让 len>0 —— 若调用方靠 len 判"有没有模型"就会误判。
func TestGroupModelsFuncContract(t *testing.T) {
	s := newTestStore(t)
	inner := []string{" a ", "", "a", "  "}
	s.SetGroupModels(func(string) []string { return inner })

	got := s.UnionGroupModels([]string{"g-1"})
	if !equalStrings(got, []string{"a"}) {
		t.Errorf("应清理空白并去重成 [a]，实际 %v", got)
	}
	// 不能污染注入方返回的切片
	if strings.Join(inner, "|") != " a ||a|  " {
		t.Errorf("UnionGroupModels 不该改写注入函数返回的切片，实际 %q", inner)
	}
}

// equalStrings 比两个字符串切片（nil 与空切片都算"没有"）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
