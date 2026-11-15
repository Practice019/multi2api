// dryrun_test.go U-02 存量权限 dry-run 比较器的判据。
//
// # 这组判据防的是"切完才知道"的三种形态
//
//	变宽  请求成功，只是这个 token 突然能用它本来不该用的模型 —— **没有任何信号**
//	变窄  请求 403，用户以为模型下线了
//	全失  切完这个 token 一个模型都调不了
//
// # 旧语义是从代码读出来的，不是猜的
//
// `internal/server/apikey_models.go` 的 allowedModelsForRequest 逐字写着：
//
//	「分组作用域非空 ⇒ 允许范围 = 这些分组的模型并集」
//	「分组非空时白名单**不参与判定**」
//	「受限且为空 ⇒ 一个都不给」
//
// ⇒ 旧是**覆盖**，新（ResolvePolicy）是**交集**。因为覆盖的集合 ⊇ 交集的集合，
//
//	**v2 只会变窄**；出现 widened 一定是比较器或新解释算错了 —— 所以必须阻塞。
package channelstore

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/groupstore"
)

// dryRunFixture 造一个带 v2 分组的隔离夹具。
//
// 渠道 ch-a 声明真实模型 real，对外公开名 public。
func dryRunFixture(t *testing.T) (*groupstore.Store, []*Channel) {
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
	channels := []*Channel{{
		ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled,
		Models:   []string{"real"},
		ModelMap: map[string]string{"public": "real"},
	}}
	return gs, channels
}

// testCatalog 目录：只有 public 一个公开模型。
var testCatalog = []string{"public"}

// ── 旧解释的三条规则（覆盖语义）──────────────────────────────────────────

// TestDryRunOldGroupsWinOverWhitelist 旧解释：分组非空 ⇒ 白名单**不参与**。
//
// 这是 U-02 最容易被写错的一格：把旧解释也写成交集，于是"旧集合"被算窄，
// 之后再怎么比都不会报 widened —— 变宽的迁移就漏掉了。
func TestDryRunOldGroupsWinOverWhitelist(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name: "app",
		// 旧：分组作用域给出 public；白名单里写着别的（会被忽略）
		OldGroupsResolved: true,
		OldGroupsModels:   []string{"public"},
		OldLimitEnabled:   true,
		OldLimits:         []string{"unused-model"},
		// 新：交集 ⇒ public ∩ unused-model = 空
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"},
			ModelLimitsEnabled: true, ModelLimits: []string{"unused-model"}},
	}})

	item := report.Items[0]
	if item.Old.Kind != PermModels || !reflect.DeepEqual(item.Old.Models, []string{"public"}) {
		t.Fatalf("★ 旧解释必须是分组并集（白名单不参与），实际 %+v —— "+
			"若这里是空集，说明把旧解释也写成交集了", item.Old)
	}
	if !item.NarrowedAll {
		t.Fatal("新交集为空 ⇒ 这个令牌切过去完全失效，NarrowedAll 必须为 true")
	}
}

// TestDryRunOldWhitelistWhenGroupsEmpty 旧解释：分组为空 ⇒ 落到白名单。
func TestDryRunOldWhitelistWhenGroupsEmpty(t *testing.T) {
	gs, channels := dryRunFixture(t)
	cases := []struct {
		name     string
		enabled  bool
		limits   []string
		wantKind string
	}{
		{"未启用白名单", false, nil, PermUnrestricted},
		{"启用且非空", true, []string{"public"}, PermModels},
		{"启用且空", true, nil, PermRestrictedEmpty}, // ←「受限且为空 ⇒ 一个都不给」
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
				Name:              "app",
				OldGroupsResolved: true,
				OldGroupsModels:   nil, // 分组作用域为空
				OldLimitEnabled:   c.enabled,
				OldLimits:         c.limits,
				Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
			}})
			if got := report.Items[0].Old.Kind; got != c.wantKind {
				t.Fatalf("旧解释 = %q，want %q", got, c.wantKind)
			}
		})
	}
}

// TestDryRunUnresolvedOldGroupsBlocks 旧解释的分组作用域解析不了 ⇒ 阻塞。
//
// ⚠ 「无法证明旧权限」比「算出个大概」重要得多 —— 算窄了就再也发现不了变宽。
func TestDryRunUnresolvedOldGroupsBlocks(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: false, // ← 调用方没拿到分组事实
		OldLimitEnabled:   false,
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})

	item := report.Items[0]
	if item.Blocked == "" {
		t.Fatal("★ 旧权限无法证明时必须阻塞（默认值就是阻塞，忘记传不会静默算窄）")
	}
	if report.Ready {
		t.Fatal("有阻塞时不该 Ready")
	}
}

// ── 变宽 / 变窄 / 全失 ────────────────────────────────────────────────────

// TestDryRunWideningIsBlocked 旧"启用但白名单空"→ 新"放出来"⇒ 变宽 ⇒ 阻塞。
func TestDryRunWideningIsBlocked(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		// 旧：分组为空 + 启用白名单但列表空 ⇒ 一个都不能用
		OldLimitEnabled: true,
		OldLimits:       nil,
		// 新：v2 + 未启用白名单 ⇒ 用组候选（public）
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})

	item := report.Items[0]
	if item.Old.Kind != PermRestrictedEmpty {
		t.Fatalf("旧解释应为 restricted_empty，实际 %q", item.Old.Kind)
	}
	if item.New.Kind != PermModels {
		t.Fatalf("新解释应为 models，实际 %q", item.New.Kind)
	}
	if !reflect.DeepEqual(item.Widened, []string{"public"}) {
		t.Fatalf("应报出变宽的 public，实际 %v", item.Widened)
	}
	if item.Blocked == "" {
		t.Fatal("★ 变宽必须阻塞迁移（不扩大权限）")
	}
	if report.Blocked != 1 || report.Ready {
		t.Fatalf("报告应被阻塞：blocked=%d ready=%v", report.Blocked, report.Ready)
	}
}

// TestDryRunNarrowingIsNotBlocked 旧"不限"→ 新确定集合 ⇒ 变窄，不阻塞但要看得见。
func TestDryRunNarrowingIsNotBlocked(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		OldLimitEnabled:   false, // 旧：没启用白名单 = 不限
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})

	item := report.Items[0]
	if item.Old.Kind != PermUnrestricted {
		t.Fatalf("旧解释应为 unrestricted，实际 %q", item.Old.Kind)
	}
	if !reflect.DeepEqual(item.Narrowed, []string{"public"}) {
		t.Fatalf("应报出变窄的 public，实际 %v", item.Narrowed)
	}
	if item.Blocked != "" {
		t.Fatalf("变窄不阻塞，实际 %q", item.Blocked)
	}
	if !report.Ready {
		t.Fatalf("无阻塞时应 Ready，实际 blocked=%d reason=%q", report.Blocked, report.BlockReason)
	}
}

// TestDryRunTotalLossIsFlagged 旧给过、新一个都不给 ⇒ NarrowedAll。
//
// ⚠ 按名字相减看不出来（旧的没有名字可减），必须单独一个布尔。
func TestDryRunTotalLossIsFlagged(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		// 新：v2 + **启用**白名单但列表空 ⇒ 一个都不给
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}, ModelLimitsEnabled: true},
	}})

	item := report.Items[0]
	if item.New.Kind != PermRestrictedEmpty {
		t.Fatalf("★ 启用且空白名单必须是 restricted_empty（全拒），实际 %q", item.New.Kind)
	}
	if !item.NarrowedAll {
		t.Fatal("★ 旧给过、新一个都不给 ⇒ NarrowedAll 必须为 true")
	}
	if report.Narrowed != 1 {
		t.Fatalf("narrowed 计数应为 1，实际 %d", report.Narrowed)
	}
}

// TestDryRunNoChangeIsClean 两套解释一致 ⇒ 无差异、Ready。
func TestDryRunNoChangeIsClean(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		OldGroupsModels:   []string{"public"},
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"},
			ModelLimitsEnabled: true, ModelLimits: []string{"public"}},
	}})

	item := report.Items[0]
	if !reflect.DeepEqual(item.Old, item.New) {
		t.Fatalf("两边应一致：old=%+v new=%+v", item.Old, item.New)
	}
	if len(item.Widened) != 0 || len(item.Narrowed) != 0 || item.NarrowedAll {
		t.Fatalf("不该有差异")
	}
	if !report.Ready || report.Blocked != 0 {
		t.Fatal("应 Ready 且无阻塞")
	}
}

// ── 阻塞面 ───────────────────────────────────────────────────────────────

// TestDryRunUnresolvedGroupBlocks 新解释的组不存在 ⇒ 阻塞。
func TestDryRunUnresolvedGroupBlocks(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-missing"}},
	}})

	if report.Items[0].Blocked == "" {
		t.Fatal("★ 组解析不了必须阻塞迁移")
	}
	if report.Ready {
		t.Fatal("有阻塞时不该 Ready")
	}
}

// TestDryRunEmptyCatalogBlocksWholeReport 目录为空 ⇒ 整份报告不可信。
//
// 空目录不是"没有任何模型"，是"不知道有什么模型"。
func TestDryRunEmptyCatalogBlocksWholeReport(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, nil, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})

	if report.CatalogKnown || report.BlockReason == "" || report.Ready {
		t.Fatalf("空目录必须让整份报告不可信：known=%v reason=%q ready=%v",
			report.CatalogKnown, report.BlockReason, report.Ready)
	}
}

// TestDryRunUnknownModelIsAmbiguous 新解释给出的模型不在目录里 ⇒ 阻塞（歧义）。
func TestDryRunUnknownModelIsAmbiguous(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, []string{"something-else"}, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})

	if report.Items[0].Blocked == "" {
		t.Fatal("★ 模型不在目录里必须阻塞（歧义）")
	}
}

// TestDryRunSkipsNonV2Tokens policy_version != 2 的令牌不参与迁移。
func TestDryRunSkipsNonV2Tokens(t *testing.T) {
	gs, channels := dryRunFixture(t)

	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{
		{Name: "legacy", OldGroupsResolved: true, Policy: RoutingPolicy{PolicyVersion: 1, Groups: []string{"g-a"}}},
		{Name: "native", OldGroupsResolved: true, Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}}},
	})

	if report.Items[0].Applicable {
		t.Fatal("policy_version=1 的令牌不该参与迁移比较")
	}
	if !report.Items[1].Applicable {
		t.Fatal("policy_version=2 的令牌必须参与")
	}
	if report.Applicable != 1 {
		t.Fatalf("Applicable 计数应为 1，实际 %d", report.Applicable)
	}
}

// TestDryRunReportNeverCarriesSecret 报告里不许出现凭据（它要贴进工单）。
func TestDryRunReportNeverCarriesSecret(t *testing.T) {
	gs, channels := dryRunFixture(t)
	report := ComputeDryRun(gs, channels, testCatalog, []DryRunInput{{
		Name: "app", Owner: "alice", OldGroupsResolved: true,
		Policy: RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})
	item := report.Items[0]
	if item.Name != "app" || item.Owner != "alice" {
		t.Fatalf("应原样带上 Name/Owner，实际 %+v", item)
	}
	if _, ok := reflect.TypeOf(item).FieldByName("KeyID"); ok {
		t.Fatal("DryRunItem 不该有 KeyID 字段（它是 Bearer 值本身）")
	}
}

// ── 别名歧义让整份报告阻塞 ──────────────────────────────────────────────────

// TestDryRunBlockedByAliasAmbiguity 模型名一语两解时，dry-run 不能给出结论。
//
// # 为什么这是**整份**阻塞而不是逐项阻塞
//
// 别名歧义是**目录级**问题：一个名字有两个解时，"这把令牌能用哪些模型"对
// **所有**令牌都不再有确定答案。逐项阻塞会让人以为"其他令牌还能迁" ——
// 但它们的模型名同样可能是那个有歧义的名字。
func TestDryRunBlockedByAliasAmbiguity(t *testing.T) {
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Upsert(&groupstore.Group{
		ID: "g-a", Name: "route", PolicyVersion: 2, ChannelIDs: []string{"ch-a", "ch-b"},
	}); err != nil {
		t.Fatal(err)
	}
	// a: pub → real；b: real → real2 ⇒ `real` 一语两解。
	channels := []*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real"}, ModelMap: map[string]string{"pub": "real"}},
		{ID: "ch-b", Name: "b", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real2"}, ModelMap: map[string]string{"real": "real2"}},
	}
	catalog := []string{"pub", "real"}

	report := ComputeDryRun(gs, channels, catalog, []DryRunInput{{
		Name:              "app",
		OldGroupsResolved: true,
		Policy:            RoutingPolicy{PolicyVersion: 2, Groups: []string{"g-a"}},
	}})

	if report.BlockReason == "" {
		t.Fatalf("★ 模型名有歧义时整份报告必须阻塞，实际 block_reason 为空（ready=%v）", report.Ready)
	}
	if !strings.Contains(report.BlockReason, "歧义") {
		t.Fatalf("阻塞原因应说清是歧义，实际 %q", report.BlockReason)
	}
	if report.Ready {
		t.Fatal("★ 有别名歧义时报告不能 Ready —— 它会让运维以为可以开始迁移")
	}
}

// TestDryRunTwoBlockReasonsAccumulate 两个阻塞原因同时成立时都要报出来。
//
// ⚠ 只报一个会让人以为"修完那个就好了"，而实际还有另一个。
func TestDryRunTwoBlockReasonsAccumulate(t *testing.T) {
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	channels := []*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real"}, ModelMap: map[string]string{"pub": "real"}},
		{ID: "ch-b", Name: "b", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real2"}, ModelMap: map[string]string{"real": "real2"}},
	}

	// 目录为空（原因 1）+ 有别名歧义（原因 2）。
	report := ComputeDryRun(gs, channels, nil, nil)

	if !strings.Contains(report.BlockReason, "无法验证旧权限") {
		t.Fatalf("应报出「目录为空」这个原因，实际 %q", report.BlockReason)
	}
	if !strings.Contains(report.BlockReason, "歧义") {
		t.Fatalf("应同时报出「歧义」这个原因，实际 %q", report.BlockReason)
	}
}
