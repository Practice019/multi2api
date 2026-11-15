// excluded_detail_test.go 被排除模型的**逐条原因**判据。
//
// # 改造前的问题
//
// 只有白名单那一种原因会被记录，其余三种都是**静默消失**：
// 模型既不在 `models` 也不在 `excluded_models` 里。运维看到"这个模型用不了"，
// 但拿不到任何原因 —— 只能猜。
//
// 而"猜"在这个系统里代价很高：分组、白名单、端点能力、渠道状态四层叠加，
// 猜错一次就会去改一个本来没问题的地方。
package channelstore

import (
	"path/filepath"
	"reflect"
	"testing"

	"workbuddy2api/internal/groupstore"
)

// exclFixture 一条在组内的渠道 + 一条不在组内的渠道。
func exclFixture(t *testing.T) (*groupstore.Store, []*Channel) {
	t.Helper()
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Upsert(&groupstore.Group{
		ID: "g-a", Name: "route", PolicyVersion: 2, ChannelIDs: []string{"ch-a"},
	}); err != nil {
		t.Fatal(err)
	}
	channels := []*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real-a"}},
		{ID: "ch-b", Name: "b", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real-b"}},
	}
	return gs, channels
}

// detailOf 找某个公开名的原因（找不到返回零值）。
func detailOf(res PolicyResult, public string) (ExcludedModel, bool) {
	for _, d := range res.ExcludedDetail {
		if d.Public == public {
			return d, true
		}
	}
	return ExcludedModel{}, false
}

// ★ ① 白名单未包含 ⇒ reason=whitelist。
func TestExcludedReasonWhitelist(t *testing.T) {
	gs, channels := exclFixture(t)
	res := ResolvePolicy(gs, channels, RoutingPolicy{
		PolicyVersion: 2, Groups: []string{"g-a"},
		ModelLimitsEnabled: true, ModelLimits: []string{"something-else"},
	})
	d, ok := detailOf(res, "real-a")
	if !ok {
		t.Fatalf("★ 白名单排除的模型必须带原因，实际 %+v", res.ExcludedDetail)
	}
	if d.Reason != ExcludedWhitelist {
		t.Fatalf("reason = %q，want %q", d.Reason, ExcludedWhitelist)
	}
	// 与旧的 ExcludedModels 保持一致（那不是新的真相，是同一件事的旧形状）。
	if !reflect.DeepEqual(res.ExcludedModels, []string{"real-a"}) {
		t.Fatalf("ExcludedModels = %v，want [real-a]", res.ExcludedModels)
	}
}

// ★ ② 渠道停用 ⇒ reason=channel_disabled。
func TestExcludedReasonChannelDisabled(t *testing.T) {
	gs, channels := exclFixture(t)
	channels[0].Status = 0 // ch-a 停用（仍在 g-a 里）
	res := ResolvePolicy(gs, channels, RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}})

	d, ok := detailOf(res, "real-a")
	if !ok {
		t.Fatalf("★ 渠道停用导致的消失必须带原因，实际 %+v", res.ExcludedDetail)
	}
	if d.Reason != ExcludedChannelDisabled {
		t.Fatalf("reason = %q，want %q", d.Reason, ExcludedChannelDisabled)
	}
	if !reflect.DeepEqual(d.ChannelIDs, []string{"ch-a"}) {
		t.Fatalf("证据渠道 = %v，want [ch-a]", d.ChannelIDs)
	}
}

// ★ ③ 端点不支持 ⇒ reason=endpoint_unsupported。
func TestExcludedReasonEndpoint(t *testing.T) {
	gs, channels := exclFixture(t)
	// ch-a 只支持 chat（默认），请求 images 就没人能服务。
	res := ResolvePolicy(gs, channels, RoutingPolicy{
		PolicyVersion: 2, Groups: []string{"g-a"}, Endpoint: "images",
	})
	d, ok := detailOf(res, "real-a")
	if !ok {
		t.Fatalf("★ 端点不支持导致的消失必须带原因，实际 %+v", res.ExcludedDetail)
	}
	if d.Reason != ExcludedEndpoint {
		t.Fatalf("reason = %q，want %q", d.Reason, ExcludedEndpoint)
	}
}

// ★ ④ 分组**外**的模型**不该**被报成"被排除"。
//
// 第一版把"不在分组内"也做成一档原因，结果 `ExcludedModels` 从 [real-a]
// 变成 [real-a real-b] —— 而 real-b 所在的 ch-b 根本不在令牌的分组里。
// 放大到真实部署就是："这把令牌排除了几百个模型"，把真正的三条原因全淹掉。
//
// 分组范围是分组编辑器该回答的问题，不是逐令牌预览该回答的。
func TestExcludedDetailSkipsOutOfScopeChannels(t *testing.T) {
	gs, channels := exclFixture(t)
	res := ResolvePolicy(gs, channels, RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}})

	if _, ok := detailOf(res, "real-b"); ok {
		t.Fatalf("★ 分组外渠道的模型不该出现在逐条原因里：%+v", res.ExcludedDetail)
	}
	// ★★ 更关键的一条：它更不能成为**候选**。
	// 第一版只断言了 ExcludedDetail，于是把"跳过分组外渠道"那行删掉时判据照样绿 ——
	// 而那个反例的实际后果是 real-b **变成可用模型**（越权），比"多报一条原因"严重得多。
	for _, m := range res.Models {
		if m == "real-b" {
			t.Fatalf("★★ 分组外渠道的 real-b 成了可用模型 %v —— 那是越权", res.Models)
		}
	}
	// real-a 在组内且可用 ⇒ 也不该被报。
	if len(res.ExcludedDetail) != 0 {
		t.Fatalf("组内渠道的模型可用，不该有任何排除原因：%+v", res.ExcludedDetail)
	}
}

// ★ ⑤ 优先级：同一个模型同时满足两条时，报**更可操作**的那条。
//
// 报哪条决定了运维去哪里改：报「端点不支持」会让他去查端点能力，
// 而真正的问题是那条渠道停用了 —— 那是个开关，比端点能力常见得多。
//
// ⚠ 第一版这个用例是**空的**：它让"停用"的那条在组内、"端点"的那条在组外，
// 而分组外的渠道现在整段跳过 ⇒ 只剩一种原因 ⇒ 优先级根本没被触发
// （反向验证把优先级改反也照样绿）。
// 改成**两条都在组内**才真的测到。
func TestExcludedReasonPriority(t *testing.T) {
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Upsert(&groupstore.Group{
		ID: "g-a", Name: "route", PolicyVersion: 2,
		ChannelIDs: []string{"ch-off", "ch-noendpoint"},
	}); err != nil {
		t.Fatal(err)
	}
	channels := []*Channel{
		// 停用 —— 优先级高。
		{ID: "ch-off", Name: "off", Kind: "standard", Status: 0,
			Models: []string{"real-a"}},
		// 启用但只支持 chat；请求 images 时它不支持端点 —— 优先级低。
		{ID: "ch-noendpoint", Name: "noep", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real-a"}},
	}
	res := ResolvePolicy(gs, channels, RoutingPolicy{
		PolicyVersion: 2, Groups: []string{"g-a"}, Endpoint: "images",
	})
	d, ok := detailOf(res, "real-a")
	if !ok {
		t.Fatalf("应有原因，实际 %+v", res.ExcludedDetail)
	}
	if d.Reason != ExcludedChannelDisabled {
		t.Fatalf("★ 同时满足「停用」与「端点不支持」时应报更可操作的 %q，实际 %q（证据渠道 %v）",
			ExcludedChannelDisabled, d.Reason, d.ChannelIDs)
	}
}

// ⑥ 有候选的模型不该出现在 ExcludedDetail 里。
func TestExcludedDetailOmitsAvailable(t *testing.T) {
	gs, channels := exclFixture(t)
	res := ResolvePolicy(gs, channels, RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}})

	if len(res.Models) != 1 || res.Models[0] != "real-a" {
		t.Fatalf("前置条件：real-a 应可用，实际 %v", res.Models)
	}
	if _, ok := detailOf(res, "real-a"); ok {
		t.Fatalf("★ real-a 有候选却被报成被排除：%+v", res.ExcludedDetail)
	}
}

// ⑦ 未解析的分组直接返回时不该编造原因。
func TestExcludedDetailEmptyOnUnresolvedGroup(t *testing.T) {
	gs, channels := exclFixture(t)
	res := ResolvePolicy(gs, channels, RoutingPolicy{PolicyVersion: 2, Groups: []string{"nope"}})
	if len(res.ExcludedDetail) != 0 {
		t.Fatalf("★ 分组都没解析成功，不该给出「逐模型原因」（那是猜）：%+v", res.ExcludedDetail)
	}
	if len(res.UnresolvedGroups) != 1 {
		t.Fatalf("应报未解析分组，实际 %v", res.UnresolvedGroups)
	}
}
