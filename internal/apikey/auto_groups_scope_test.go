// auto_groups_scope_test.go 守「令牌级自动分组快照」进入判定链的两件事。
//
// # 为什么值得单独一个文件
//
// 2026-11-01 用户报「我选择分组优先级，然后选择自定义，添加了我的自定义的那个
// 666，但我模型检测出来是所有全局的模型」。实测（demo 7860）：
//
//	令牌 id=4：group="[]" + auto_groups=["666 的模型"] → /v1/models 回 346 个（全部）
//	令牌 id=3：group="666 的模型"（有序分组，选定了一个分组）→ /v1/models 回空列表
//
// 两个方向相反、根因各一：
//
//	① 作用域只认 Key.Groups，不认 AutoGroups ⇒ 自动分组·自定义被当成"不限制"
//	② 引用可能是分组名，而解析只按 ID 查 ⇒ 并集为空 ⇒ 一个模型都不许用
//
// 本文件钉 ①（②解在 groupstore/ref_test.go 与 admin/grouprefs_test.go）。
package apikey

import (
	"testing"

	"workbuddy2api/internal/store"
)

// setAutoGroupsForTest 直接改内存态（生产路径是"admin 写库 → SyncTokenByID 回读"，
// 单测里不需要绕一圈；本包测试同 package，可以动内部 map）。
func setAutoGroupsForTest(t *testing.T, s *Store, id string, groups []string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		t.Fatalf("测试夹具里没有这把 key: %s", id)
	}
	k.AutoGroups = normalizeGroups(groups)
}

// TestGroupRefsOfScopePrecedence 作用域的三种状态与优先级。
//
// 这三条必须互相区分：「没分组 = 不限制」「自动分组·自定义 = 用它」「有序分组 = 用它」。
// 混掉任意两条都会产生一个看不见的权限变化（放大或封死）。
func TestGroupRefsOfScopePrecedence(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	// ① 两者都空 ⇒ 不限制（ok 仍为 true：调用方靠它区分「没限制」与「没这把 key」）
	refs, ok := s.GroupRefsOf(k.ID)
	if !ok || len(refs) != 0 {
		t.Errorf("两者都空应为「不限制」，实际 refs=%v ok=%v", refs, ok)
	}
	if _, ok := s.GroupRefsOf("sk-nope"); ok {
		t.Error("不存在的 key 必须 ok=false")
	}

	// ② 只有自动分组快照（自动分组·自定义）⇒ 用它
	setAutoGroupsForTest(t, s, k.ID, []string{"g-auto"})
	refs, _ = s.GroupRefsOf(k.ID)
	if !equalStrings(refs, []string{"g-auto"}) {
		t.Errorf("Groups 空时应取自动分组快照，实际 %v", refs)
	}

	// ③ 有序分组非空 ⇒ **以它为准**（AutoGroups 是自动分组模式下的候选集，
	//    两种模式不会同时生效；同时存在时按 hyxapi 口径以 Group 为准）
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-manual"}); err != nil {
		t.Fatal(err)
	}
	refs, _ = s.GroupRefsOf(k.ID)
	if !equalStrings(refs, []string{"g-manual"}) {
		t.Errorf("有序分组非空时以 Groups 为准，实际 %v", refs)
	}

	// ④ 返回副本：改它不能污染内存态
	refs[0] = "被改了"
	if again, _ := s.GroupRefsOf(k.ID); again[0] != "g-manual" {
		t.Error("GroupRefsOf 必须返回副本")
	}
}

// TestGroupModelsOfUsesAutoGroups 回归：自动分组·自定义**必须收窄**模型范围。
//
// 修之前这里返回的是"不限制"（nil），于是 /v1/models 把全部模型发给这把令牌。
func TestGroupModelsOfUsesAutoGroups(t *testing.T) {
	s := newTestStore(t)
	s.SetGroupModels(fakeGroups(map[string][]string{
		"g-666": {"m1", "m2"},
	}))
	k, _ := s.Create("app", 0, 0, nil)
	setAutoGroupsForTest(t, s, k.ID, []string{"g-666"})

	groups, models, ok := s.GroupModelsOf(k.ID)
	if !ok || !equalStrings(groups, []string{"g-666"}) {
		t.Fatalf("应把自动分组快照当作作用域，实际 groups=%v ok=%v", groups, ok)
	}
	if !equalStrings(models, []string{"m1", "m2"}) {
		t.Fatalf("★ 自动分组·自定义必须收窄到分组里的模型，实际 %v（空 ⇒ 判定链会当成「不限制」，那正是用户报的 346 个模型）", models)
	}

	// 快照指向一个悬空分组 ⇒ 并集空（受限且为空，调用方据此拒绝），不是"不限制"
	setAutoGroupsForTest(t, s, k.ID, []string{"g-没有这个组"})
	_, models, _ = s.GroupModelsOf(k.ID)
	if len(models) != 0 {
		t.Errorf("悬空分组的并集应为空，实际 %v", models)
	}
}

// TestAutoGroupsOfCopy 取快照返回副本（同 GroupsOf 的约定）。
func TestAutoGroupsOfCopy(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, nil)
	setAutoGroupsForTest(t, s, k.ID, []string{"g-1"})

	got, ok := s.AutoGroupsOf(k.ID)
	if !ok || !equalStrings(got, []string{"g-1"}) {
		t.Fatalf("AutoGroupsOf = %v ok=%v", got, ok)
	}
	got[0] = "改"
	if again, _ := s.AutoGroupsOf(k.ID); again[0] != "g-1" {
		t.Error("AutoGroupsOf 必须返回副本")
	}
	if v, ok := s.AutoGroupsOf("sk-nope"); ok || v != nil {
		t.Errorf("不存在的 key 应 (nil,false)，实际 %v ok=%v", v, ok)
	}
}

// TestAutoGroupsSurviveTokenRoundTrip 行 ↔ 内存态往返无损。
//
// # 这条同时是一条**防数据丢失**的守卫
//
// store.SaveToken 的 Select 清单里有 auto_groups 列，而 apikey 的写回
// （flushSQLLocked → upsertLocked → SaveToken）走的就是它。若映射里漏了快照，
// 任何一次 flush（改名、prune、P3.5 迁移）都会把用户配好的自动分组**刷成空串**
// —— 表现是"我明明配了，过一阵自己变回继承了"，而没有任何报错。
func TestAutoGroupsSurviveTokenRoundTrip(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, nil)
	setAutoGroupsForTest(t, s, k.ID, []string{"g-1", "g-2"})

	row, err := keyToToken(k)
	if err != nil {
		t.Fatalf("keyToToken: %v", err)
	}
	if row.AutoGroups != `["g-1","g-2"]` {
		t.Fatalf("快照没写进 auto_groups 列（SaveToken 会把这一列刷空）：%q", row.AutoGroups)
	}
	back := keyFromToken(row)
	if !equalStrings(back.AutoGroups, []string{"g-1", "g-2"}) {
		t.Fatalf("往返后快照丢了：%v", back.AutoGroups)
	}

	// 没有快照的 key ⇒ 列里是空串（不是 "null" / "[]" 之类的脏值）
	k2, _ := s.Create("app2", 0, 0, nil)
	row2, err := keyToToken(k2)
	if err != nil {
		t.Fatalf("keyToToken: %v", err)
	}
	if row2.AutoGroups != "" {
		t.Errorf("没有快照时应写空串，实际 %q", row2.AutoGroups)
	}

	// 坏 JSON 不当致命：按"没有快照"处理（与 TokenView.Masked 同口径）
	bad := &store.Token{}
	bad.AutoGroups = "{不是数组}"
	if got := keyFromToken(bad); len(got.AutoGroups) != 0 {
		t.Errorf("坏 JSON 应按无快照处理，实际 %v", got.AutoGroups)
	}
}
