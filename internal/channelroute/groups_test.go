// groups_test.go P4：一条渠道属于多个分组时，Plan 在**每个**组里都能看见它。
//
// # 要守的
//
//	① 多分组渠道在它所属的每一个组里都是候选
//	② 不属于该组的渠道仍然被跳过（多分组不是"对所有组开放"）
//	③ 存量单组渠道行为不变
//
// ② 是 ① 的反面，但必须单独测：把判定写成"Groups 非空就放行"是最容易犯的错，
// 而那条错会让**所有**多分组渠道对所有分组可见 —— 权限被静默放大。
package channelroute

import (
	"errors"
	"testing"

	"workbuddy2api/internal/channelstore"
)

// addGroups 建一条属于多个分组的渠道。
func addGroups(t *testing.T, s *channelstore.Store, id string, prio int, groups []string, models []string) {
	t.Helper()
	c := &channelstore.Channel{
		ID: id, Name: id, BaseURL: "https://" + id + ".example.com/v1",
		Status: channelstore.StatusEnabled, Priority: prio, Models: models,
		Groups: groups,
	}
	if err := s.Upsert(c); err != nil {
		t.Fatalf("建渠道 %s 失败: %v", id, err)
	}
}

// ★ ① 多分组渠道在每个所属组里都是候选。
func TestPlanSeesChannelInEveryGroup(t *testing.T) {
	s := store(t)
	addGroups(t, s, "ch-multi", 10, []string{"default", "vip"}, []string{"m"})

	for _, g := range []string{"default", "vip"} {
		got, err := Plan(s, g, "m")
		if err != nil {
			t.Fatalf("组 %s 应能看到这条渠道: %v", g, err)
		}
		if len(got) != 1 || got[0].Channel.ID != "ch-multi" {
			t.Fatalf("组 %s 的候选不对: %v", g, ids(got))
		}
	}
}

// ★ ② 不属于该组的渠道仍被跳过（多分组不是"对所有组开放"）。
func TestPlanHidesChannelFromForeignGroups(t *testing.T) {
	s := store(t)
	addGroups(t, s, "ch-vip", 10, []string{"vip"}, []string{"m"})

	if _, err := Plan(s, "svip", "m"); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("★ svip 组不该看见只属于 vip 的渠道（放行 = 权限被静默放大），实际 %v", err)
	}
	if got, err := Plan(s, "vip", "m"); err != nil || len(got) != 1 {
		t.Fatalf("vip 组应看见它，实际 %v err=%v", ids(got), err)
	}
}

// ★ ③ 存量单组渠道行为不变（Plan 拿到的 group 为空时按 default）。
func TestPlanLegacySingleGroupUnchanged(t *testing.T) {
	s := store(t)
	// 完全按存量形态：只填 Group
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-legacy", Name: "legacy", BaseURL: "https://x.invalid/v1",
		Status: channelstore.StatusEnabled, Group: "default", Priority: 5,
		Models: []string{"m"},
	}); err != nil {
		t.Fatal(err)
	}
	// group="" ⇒ 按 default
	if got, err := Plan(s, "", "m"); err != nil || len(got) != 1 {
		t.Fatalf("★ 存量渠道在默认组里必须可见，实际 %v err=%v", ids(got), err)
	}
	// 显式 default 同样
	if got, err := Plan(s, "default", "m"); err != nil || len(got) != 1 {
		t.Fatalf("显式 default 组应可见，实际 %v err=%v", ids(got), err)
	}
	// 别的组看不见
	if _, err := Plan(s, "vip", "m"); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("存量渠道不该出现在 vip 组，实际 %v", err)
	}
}

// 多分组 + 多候选：同一模型在两组里各有渠道 ⇒ 每组各自的候选集合正确。
func TestPlanPerGroupCandidateSets(t *testing.T) {
	s := store(t)
	addGroups(t, s, "ch-both", 100, []string{"default", "vip"}, []string{"m"})
	addGroups(t, s, "ch-default-only", 10, []string{"default"}, []string{"m"})
	addGroups(t, s, "ch-vip-only", 10, []string{"vip"}, []string{"m"})

	def, err := Plan(s, "default", "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 2 {
		t.Fatalf("default 组应有 2 条候选，实际 %v", ids(def))
	}
	// 优先级高的在前
	if def[0].Channel.ID != "ch-both" {
		t.Errorf("default 组首条应是优先级 100 的 ch-both，实际 %s", def[0].Channel.ID)
	}

	vip, err := Plan(s, "vip", "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(vip) != 2 {
		t.Fatalf("vip 组应有 2 条候选，实际 %v", ids(vip))
	}
	for _, c := range vip {
		if c.Channel.ID == "ch-default-only" {
			t.Error("★ 只属于 default 的渠道不该出现在 vip 组")
		}
	}
}
