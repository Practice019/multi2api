// groups_test.go P4：渠道可以属于多个分组，能力索引对 (models × groups) 做笛卡尔积。
//
// # 三条必须钉住的
//
//	① 存量兼容：只写了 Group="default" 的渠道，行为**一个字节都不变**
//	② 笛卡尔积：一条渠道进两个组、声明两个模型 ⇒ 四条能力记录
//	③ 归一化：Groups / Group 双向补齐、去空白去重，且 EffectiveGroups 恒非空
//
// 第 ① 条是本文件的重点：本仓现网三条渠道全是 Group="default"，
// 升级后只要有一条行为变了，就是一次事故。
package channelstore

import (
	"testing"
)

// 两分组渠道（同一批模型进 default 与 vip）。
func chGroups(id string, groups []string, models ...string) *Channel {
	c := ch(id, models...)
	c.Groups = groups
	return c
}

// ★ ① 存量兼容：只有 Group="default" 的渠道，能力与对外目录与改造前完全一致。
func TestLegacySingleGroupUnchanged(t *testing.T) {
	s, _ := newTestStore(t)
	// 完全按存量形态建：只填 Group，不填 Groups
	if err := s.Upsert(&Channel{
		ID: "legacy", Name: "legacy", BaseURL: "https://x.invalid/v1",
		Group: "default", Models: []string{"m-a", "m-b"},
		Keys: []ChannelKey{{Key: "sk-1", Status: StatusEnabled}},
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := s.Get("legacy")
	if !ok {
		t.Fatal("渠道没存进去")
	}
	if got.Group != DefaultGroup {
		t.Errorf("Group 应保持 default，实际 %q", got.Group)
	}
	if g := got.EffectiveGroups(); len(g) != 1 || g[0] != DefaultGroup {
		t.Errorf("★ 存量渠道的 EffectiveGroups 必须是 [default]，实际 %v", g)
	}
	// 能力记录：2 条（2 模型 × 1 组），与改造前一致
	abs := s.Abilities()
	if len(abs) != 2 {
		t.Fatalf("★ 存量渠道的能力记录数必须仍是 2（不是 4），实际 %d: %+v", len(abs), abs)
	}
	for _, ab := range abs {
		if ab.Group != DefaultGroup {
			t.Errorf("能力记录的 group 应为 default，实际 %q", ab.Group)
		}
	}
	if models := s.EnabledModels(DefaultGroup); len(models) != 2 {
		t.Errorf("default 组对外模型应为 2 个，实际 %v", models)
	}
}

// Group 为空（更老的形态）⇒ 补 default，行为同上。
func TestEmptyGroupDefaultsToDefault(t *testing.T) {
	s, _ := newTestStore(t)
	c := ch("bare", "m")
	c.Group = ""
	c.Groups = nil
	if err := s.Upsert(c); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("bare")
	if got.Group != DefaultGroup {
		t.Errorf("空 Group 应补成 default，实际 %q", got.Group)
	}
	if g := got.EffectiveGroups(); len(g) != 1 || g[0] != DefaultGroup {
		t.Errorf("EffectiveGroups 应为 [default]，实际 %v", g)
	}
}

// ★ ② 笛卡尔积：两个组 × 两个模型 ⇒ 4 条能力。
func TestAbilitiesCartesianProduct(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(chGroups("c1", []string{"default", "vip"}, "m-a", "m-b")); err != nil {
		t.Fatal(err)
	}

	abs := s.Abilities()
	if len(abs) != 4 {
		t.Fatalf("★ 2 组 × 2 模型 = 4 条能力记录，实际 %d: %+v", len(abs), abs)
	}
	// 每个 (group, model) 组合都在
	seen := map[string]bool{}
	for _, ab := range abs {
		seen[ab.Group+"|"+ab.Model] = true
		if ab.ChannelID != "c1" {
			t.Errorf("能力记录的渠道 ID 不对: %q", ab.ChannelID)
		}
	}
	for _, g := range []string{"default", "vip"} {
		for _, m := range []string{"m-a", "m-b"} {
			if !seen[g+"|"+m] {
				t.Errorf("缺能力记录 %s|%s（笛卡尔积漏了）", g, m)
			}
		}
	}

	// 两个组都能查到这两个模型
	for _, g := range []string{"default", "vip"} {
		if models := s.EnabledModels(g); len(models) != 2 {
			t.Errorf("%s 组应有 2 个模型，实际 %v", g, models)
		}
	}
}

// 改 Groups 后旧的组能力必须消失（不能残留成"幽灵组"）。
func TestChangingGroupsRemovesOldAbilities(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(chGroups("c1", []string{"default", "vip"}, "m-a")); err != nil {
		t.Fatal(err)
	}
	if len(s.EnabledModels("vip")) != 1 {
		t.Fatal("前置：vip 组应有 m-a")
	}
	// 从 vip 摘出去
	c, _ := s.Get("c1")
	c.Groups = []string{"default"}
	if err := s.Upsert(c); err != nil {
		t.Fatal(err)
	}
	if got := s.EnabledModels("vip"); len(got) != 0 {
		t.Errorf("★ 移出 vip 后该组不该再有模型（残留 = 幽灵能力），实际 %v", got)
	}
	if got := s.EnabledModels(DefaultGroup); len(got) != 1 {
		t.Errorf("default 组应仍有 m-a，实际 %v", got)
	}
}

// ★ ③ 归一化：去空白、去空项、去重；Group 与 Groups 双向同步。
func TestGroupsNormalization(t *testing.T) {
	cases := []struct {
		name   string
		groups []string
		group  string
		want   []string
	}{
		{"Groups 为空 + Group=vip", nil, "vip", []string{"vip"}},
		{"两者都空", nil, "", []string{DefaultGroup}},
		{"去空白与空项", []string{" vip ", "", "  ", "svip"}, "", []string{"vip", "svip"}},
		{"去重", []string{"vip", "vip ", "vip"}, "", []string{"vip"}},
		{"Group 追加在 Groups 之后（去重后不重复）", []string{"vip"}, "default", []string{"vip", "default"}},
		{"Group 已在 Groups 里则不重复", []string{"vip", "default"}, "default", []string{"vip", "default"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ch("x", "m")
			c.Groups = tc.groups
			c.Group = tc.group
			got := c.EffectiveGroups()
			if len(got) != len(tc.want) {
				t.Fatalf("期望 %v，实际 %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("期望 %v，实际 %v", tc.want, got)
				}
			}
		})
	}
}

// Upsert 之后 Group 必须同步成 Groups[0]（旧版 exe 回退时读得懂）。
func TestGroupSyncedFromGroupsOnUpsert(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(chGroups("c1", []string{"vip", "svip"}, "m")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("c1")
	if got.Group != "vip" {
		t.Errorf("★ Group 应同步成 Groups[0]=vip（旧版 exe 只读这个字段），实际 %q", got.Group)
	}
	if len(got.Groups) != 2 {
		t.Errorf("Groups 应保留两个，实际 %v", got.Groups)
	}
}

// 落盘重载：Groups 要读回来，能力索引自愈后仍是笛卡尔积。
func TestGroupsPersistAndRebuild(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.Upsert(chGroups("c1", []string{"default", "vip"}, "m-a")); err != nil {
		t.Fatal(err)
	}
	s2, err := New(path)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	got, ok := s2.Get("c1")
	if !ok {
		t.Fatal("重载后渠道丢失")
	}
	if len(got.Groups) != 2 {
		t.Errorf("重载后 Groups 应为 2 个，实际 %v", got.Groups)
	}
	if models := s2.EnabledModels("vip"); len(models) != 1 {
		t.Errorf("重载后 vip 组应仍能查到模型，实际 %v", models)
	}
}

// 深拷贝：改返回值的 Groups 不能污染存储（与 Models 的同一条守卫）。
func TestGetClonesGroups(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(chGroups("c1", []string{"default", "vip"}, "m")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("c1")
	got.Groups[0] = "被改掉了"
	again, _ := s.Get("c1")
	if again.Groups[0] != "default" {
		t.Errorf("Get 返回的 Groups 必须深拷贝，实际存储被改成了 %q", again.Groups[0])
	}
}

// EffectiveGroups 每次返回新切片（调用方 append 不该影响渠道对象）。
func TestEffectiveGroupsReturnsFreshSlice(t *testing.T) {
	c := chGroups("c1", []string{"default"}, "m")
	first := c.EffectiveGroups()
	first[0] = "被改掉了"
	second := c.EffectiveGroups()
	if second[0] != "default" {
		t.Errorf("EffectiveGroups 必须返回新切片，实际 %q", second[0])
	}
}
