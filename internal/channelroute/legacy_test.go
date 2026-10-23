// legacy_test.go 显式的存量兼容守卫：只设 Group="default"（**不设 Groups**）的渠道，
// 选路结果与 P4 改造前**完全一致**。
//
// # 为什么要单独一条（而不是并进 groups_test.go）
//
// 本仓现网三条渠道全是这个形态。P4 改了 Channel 的字段、能力索引的展开方式、
// Plan 的组过滤判据 —— 三处里任何一处写错，都会让存量渠道静默消失。
// 而"静默消失"的表现是"模型列表里少了几个"，不是报错 —— 用户不会立刻报障。
//
// 所以这里用**改造前的代码路径**做对照：不设 Groups、只设 Group，
// 断言它能被默认组看见、能出现在对外模型里、能力记录数正确。
package channelroute

import (
	"errors"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
)

// TestLegacySingleGroupChannelUnchanged 存量单组渠道的选路行为不变。
func TestLegacySingleGroupChannelUnchanged(t *testing.T) {
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建 store: %v", err)
	}
	// ⚠ 刻意写成存量形态：只有 Group，没有 Groups。
	if err := s.Upsert(&channelstore.Channel{
		ID:       "ch-legacy",
		Name:     "存量渠道",
		BaseURL:  "https://legacy.example.com/v1",
		Status:   channelstore.StatusEnabled,
		Group:    "default",
		Priority: 10,
		Models:   []string{"m-a", "m-b"},
		Keys:     []channelstore.ChannelKey{{Key: "sk-legacy", Status: channelstore.StatusEnabled}},
	}); err != nil {
		t.Fatalf("建渠道: %v", err)
	}

	// ① 渠道本身：Group 仍是 default，Groups 被补齐成 [default]（不是空）
	ch, ok := s.Get("ch-legacy")
	if !ok {
		t.Fatal("渠道丢了")
	}
	if ch.Group != channelstore.DefaultGroup {
		t.Errorf("① Group 应仍是 default，实际 %q", ch.Group)
	}
	if g := ch.EffectiveGroups(); len(g) != 1 || g[0] != channelstore.DefaultGroup {
		t.Errorf("① EffectiveGroups 应为 [default]，实际 %v", g)
	}

	// ② 能力索引：2 模型 × 1 组 = 2 条（**不是** 4 条 —— 那是多分组才有的）
	if abs := s.Abilities(); len(abs) != 2 {
		t.Errorf("② 存量渠道应产生 2 条能力记录，实际 %d: %+v", len(abs), abs)
	}

	// ③ 对外模型：默认组里两个都在，且顺序稳定
	models := s.EnabledModels(channelstore.DefaultGroup)
	if len(models) != 2 || models[0] != "m-a" || models[1] != "m-b" {
		t.Errorf("③ 默认组对外模型应为 [m-a m-b]，实际 %v", models)
	}

	// ④ 选路：默认组（显式与空串两种写法）都能规划出它；非默认组看不到
	for _, g := range []string{"", channelstore.DefaultGroup} {
		got, err := Plan(s, g, "m-a")
		if err != nil {
			t.Fatalf("④ 组 %q 应能规划出候选: %v", g, err)
		}
		if len(got) != 1 || got[0].Channel.ID != "ch-legacy" {
			t.Errorf("④ 组 %q 的候选不对: %v", g, ids(got))
		}
		if got[0].UpstreamModel != "m-a" {
			t.Errorf("④ 无映射时上游名应原样透传，实际 %q", got[0].UpstreamModel)
		}
	}
	if _, err := Plan(s, "vip", "m-a"); !errors.Is(err, ErrNoCandidate) {
		t.Errorf("④ 存量渠道不该出现在 vip 组，实际 %v", err)
	}

	// ⑤ 停用后行为不变：不参与
	if err := s.SetStatus("ch-legacy", channelstore.StatusManuallyDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(s, "", "m-a"); !errors.Is(err, ErrNoCandidate) {
		t.Errorf("⑤ 停用后应无候选，实际 %v", err)
	}
}
