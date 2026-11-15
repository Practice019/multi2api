// catalog_u04_test.go U-04「公开模型映射与版本化能力目录」的判据。
//
// # 三条判据对应计划 §5 U-04 的验收
//
//	判据 1  同名多渠道**显式**聚合 —— 运维写了 ModelMap，就按他写的聚合
//	判据 2  隐式撞名**拒绝发布**  —— 同名但是两个不同真实模型，宁可少一个模型
//	判据 3  无效映射**记录不静默** —— 映射到不存在的真实模型必须看得见
//
// # 为什么判据 2 是这组里最重要的
//
// 它防的是一次**静默的错误路由**：`pub` 在 A 渠道被映射到 `gpt-4`，在 B 渠道又是
// 一个真实存在的模型名。合并之后用户请求 `pub`，一半流量会打到 `gpt-4`、
// 另一半打到 `pub` 自己 —— 而且**两边都成功**，日志里毫无异常。
// 这种错只能靠"拒绝发布"来防，没有任何运行期信号能救。
package channelstore

import (
	"errors"
	"testing"
	"time"
)

// catCh 造一条渠道的最小形态（名字带 cat 前缀：本包里 `ch` 已被 store_test.go 占用）。
func catCh(id string, models []string, mapping map[string]string) *Channel {
	return &Channel{
		ID:       id,
		Name:     "ch-" + id,
		Kind:     "standard",
		Status:   StatusEnabled,
		Models:   models,
		ModelMap: mapping,
	}
}

// ── 判据 1：显式聚合 ────────────────────────────────────────────────────────

// TestCatalogExplicitAggregationMerges 两条渠道把同一公开名显式映射到不同真实模型
// ⇒ **允许**聚合（这是运维的意图表达，不是歧义）。
//
// 它与判据 2 的分界线只有一条：有没有"以自己为真实模型"的供给。
func TestCatalogExplicitAggregationMerges(t *testing.T) {
	a := catCh("a", []string{"real-a"}, map[string]string{"public": "real-a"})
	b := catCh("b", []string{"real-b"}, map[string]string{"public": "real-b"})

	got := BuildCatalog([]*Channel{a, b})

	if len(got.Conflicts) != 0 {
		t.Fatalf("两条都是显式映射，不该判为歧义；实际 conflicts=%+v", got.Conflicts)
	}
	if len(got.Models) != 1 {
		t.Fatalf("应有 1 个公开模型，实际 %d 个：%+v", len(got.Models), got.Models)
	}
	m := got.Models[0]
	if m.ID != "public" || len(m.Providers) != 2 {
		t.Fatalf("公开模型应为 public 且有 2 条供给，实际 ID=%q providers=%d", m.ID, len(m.Providers))
	}
	// 真实模型必须逐条可见 —— 聚合不等于抹掉它到底打给谁。
	reals := map[string]bool{}
	for _, p := range m.Providers {
		reals[p.RealModel] = true
	}
	if !reals["real-a"] || !reals["real-b"] {
		t.Fatalf("聚合后每条供给的真实模型必须保留，实际 %v", reals)
	}
}

// TestCatalogIdentityAggregationMerges 两条渠道都直接声明同一个模型名（无映射）
// ⇒ 也聚合（它们指向同一个真实模型）。
func TestCatalogIdentityAggregationMerges(t *testing.T) {
	got := BuildCatalog([]*Channel{catCh("a", []string{"m1"}, nil), catCh("b", []string{"m1"}, nil)})
	if len(got.Conflicts) != 0 {
		t.Fatalf("恒等声明同名不算歧义，实际 conflicts=%+v", got.Conflicts)
	}
	if len(got.Models) != 1 || len(got.Models[0].Providers) != 2 {
		t.Fatalf("应聚合为 1 模型 2 供给，实际 %+v", got.Models)
	}
}

// ── 判据 2：隐式撞名拒绝发布 ────────────────────────────────────────────────

// TestCatalogRefusesImplicitCollision 同一个公开名：
//
//	A 渠道显式映射 pub → gpt-4
//	B 渠道直接声明模型 pub（恒等，pub → pub）
//
// ⇒ 两个不同的真实模型共用一个公开名 ⇒ **必须拒绝发布 pub**。
//
// ⚠ 这是本组最重要的一条。合并的后果是"一半流量打到 gpt-4、一半打到 pub 自己，
// 两边都成功"，运行期完全没有信号。
func TestCatalogRefusesImplicitCollision(t *testing.T) {
	a := catCh("a", []string{"gpt-4"}, map[string]string{"pub": "gpt-4"})
	b := catCh("b", []string{"pub"}, nil)

	got := BuildCatalog([]*Channel{a, b})

	for _, m := range got.Models {
		if m.ID == "pub" {
			t.Fatalf("歧义公开名 pub 不该被发布，实际 Models=%+v", got.Models)
		}
	}
	if len(got.Conflicts) != 1 {
		t.Fatalf("应报告 1 条冲突，实际 %d 条：%+v", len(got.Conflicts), got.Conflicts)
	}
	c := got.Conflicts[0]
	if c.Public != "pub" || c.Reason != ConflictImplicitCollision {
		t.Fatalf("冲突内容不对：%+v", c)
	}
	if len(c.Reals) != 2 {
		t.Fatalf("冲突应同时报出两个真实模型，实际 %v", c.Reals)
	}
	if len(c.Channels) != 2 {
		t.Fatalf("冲突应报出涉及的两条渠道，实际 %v", c.Channels)
	}
	// ⚠ 这里**恰好什么都不会发布**，而且这是对的 —— 探清这条语义值得单独写下来：
	//
	//   A 渠道 Models=[gpt-4]、ModelMap={pub: gpt-4}
	//     ⇒ 它的公开名**只有** pub（gpt-4 是 pub 背后的真实模型，不是公开名）
	//   B 渠道 Models=[pub]（恒等）
	//     ⇒ 它的公开名也是 pub
	//
	// 于是 pub 是这两条渠道**唯一**的公开名，被拒之后一条都不剩。
	// gpt-4 此刻确实"存在但取不到" —— 宁可如此，也不要静默挑一个（见上面那段）。
	// 运维修好映射（给 gpt-4 一个自己的公开名，或去掉 B 的冲突声明）后立刻恢复。
	if len(got.Models) != 0 {
		t.Fatalf("pub 是这两条渠道唯一的公开名，拒绝后不该有任何模型；实际 %+v", got.Models)
	}

	// 拒绝必须**精确到公开名**：另加一条互不相干的渠道，它不受牵连。
	withOther := BuildCatalog([]*Channel{a, b, catCh("c", []string{"unrelated"}, nil)})
	if len(withOther.Models) != 1 || withOther.Models[0].ID != "unrelated" {
		t.Fatalf("无关模型被牵连了；实际 %+v", withOther.Models)
	}
	if len(withOther.Conflicts) != 1 || withOther.Conflicts[0].Public != "pub" {
		t.Fatalf("冲突报告不该因无关渠道而改变；实际 %+v", withOther.Conflicts)
	}
}

// TestCatalogAllowsExplicitMappingToOwnName 显式把 pub 映射到 pub 自己
// ⇒ 指向同一个模型，**不是**歧义。
func TestCatalogAllowsExplicitMappingToOwnName(t *testing.T) {
	a := catCh("a", []string{"pub"}, map[string]string{"pub": "pub"})
	b := catCh("b", []string{"pub"}, nil)

	got := BuildCatalog([]*Channel{a, b})

	if len(got.Conflicts) != 0 {
		t.Fatalf("显式映射到自己不该判歧义，实际 %+v", got.Conflicts)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "pub" {
		t.Fatalf("pub 应被发布，实际 %+v", got.Models)
	}
}

// ── 判据 3：无效映射记录不静默 ──────────────────────────────────────────────

// TestCatalogReportsUndeclaredMappingTarget ModelMap 指向未声明的真实模型
// ⇒ 记录冲突，且**不发布**那个公开名（发布出去也调不通）。
//
// 改造前这里是 `if declared[real]` 静默跳过 —— 运维配了个错字，界面上什么都没有。
func TestCatalogReportsUndeclaredMappingTarget(t *testing.T) {
	a := catCh("a", []string{"real"}, map[string]string{"ghost": "typo-model"})

	got := BuildCatalog([]*Channel{a})

	if len(got.Conflicts) != 1 || got.Conflicts[0].Reason != ConflictMappingUndeclared {
		t.Fatalf("应报告一条 mapping_target_undeclared，实际 %+v", got.Conflicts)
	}
	if got.Conflicts[0].Public != "ghost" {
		t.Fatalf("冲突的公开名应为 ghost，实际 %q", got.Conflicts[0].Public)
	}
	for _, m := range got.Models {
		if m.ID == "ghost" {
			t.Fatalf("指向不存在真实模型的公开名不该被发布：%+v", got.Models)
		}
	}
	// 同一条渠道里合法的声明不受影响。
	if len(got.Models) != 1 || got.Models[0].ID != "real" {
		t.Fatalf("real 应仍然发布，实际 %+v", got.Models)
	}
}

// ── Version 语义 ────────────────────────────────────────────────────────────

// TestCatalogVersionIsContentAddressed 同样输入必得同样 Version；
// 配置或冲突变化必须改变 Version。
func TestCatalogVersionIsContentAddressed(t *testing.T) {
	a := catCh("a", []string{"gpt-4"}, map[string]string{"pub": "gpt-4"})
	b := catCh("b", []string{"pub"}, nil)

	first := BuildCatalog([]*Channel{a, b})
	// ReadAt 每次都变，但它**不得**进入 Version —— 否则版本每次请求都不同。
	for i := 0; i < 5; i++ {
		if again := BuildCatalog([]*Channel{a, b}); again.Version != first.Version {
			t.Fatalf("同样输入第 %d 次得到不同 Version：%q vs %q —— "+
				"map 遍历顺序或 ReadAt 混进了哈希", i+2, again.Version, first.Version)
		}
	}

	// 把 b 的声明去掉 ⇒ 歧义消失，pub 被发布 ⇒ Version 必变。
	noConflict := BuildCatalog([]*Channel{a, catCh("b", []string{"other"}, nil)})
	if noConflict.Version == first.Version {
		t.Fatal("冲突消失后 Version 未变化 —— 版本没有覆盖 Conflicts")
	}
}

// TestCatalogConflictFieldsDoNotLeakCredentials 冲突报告里也不许出现凭据。
func TestCatalogConflictFieldsDoNotLeakCredentials(t *testing.T) {
	a := &Channel{
		ID: "a", Name: "a", Kind: "standard", Status: StatusEnabled,
		Models:   []string{"real"},
		ModelMap: map[string]string{"ghost": "typo"},
		Keys:     []ChannelKey{{Key: "SECRET-MUST-NOT-LEAK", Status: 1}},
	}
	got := BuildCatalog([]*Channel{a})
	for _, c := range got.Conflicts {
		if c.Public == "SECRET-MUST-NOT-LEAK" {
			t.Fatal("凭据出现在冲突报告里")
		}
		for _, s := range append(append([]string{}, c.Reals...), c.Channels...) {
			if s == "SECRET-MUST-NOT-LEAK" {
				t.Fatal("凭据出现在冲突明细里")
			}
		}
	}
}

// ── 快照：获取与读取分离 ────────────────────────────────────────────────────

// TestSnapshotFailKeepsOldCatalog 「刷新失败保留旧快照」。
//
// ⚠ 反向验证的关键：如果 Fail 顺手把 Catalog 清空（或覆盖成空快照），
// 客户端会看到一个**空目录**，而"刷新失败"本该是**看不见的**退化。
func TestSnapshotFailKeepsOldCatalog(t *testing.T) {
	s := NewCatalogSnapshotStore()
	base := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	if s.Read().Ready() {
		t.Fatal("一份都没发布过时不该 Ready")
	}

	published := BuildCatalog([]*Channel{catCh("a", []string{"m1"}, nil)})
	after := s.Publish(published)
	if after.Sequence != 1 || after.Attempts != 1 || !after.Ready() {
		t.Fatalf("首次发布后应为 seq=1 attempts=1 ready；实际 %+v", after)
	}

	failed := s.Fail(errors.New("upstream 502"))
	if failed.Sequence != 1 {
		t.Fatalf("失败不得推进 Sequence，实际 %d", failed.Sequence)
	}
	if failed.Attempts != 2 {
		t.Fatalf("失败必须推进 Attempts，实际 %d", failed.Attempts)
	}
	if failed.LastError != "upstream 502" {
		t.Fatalf("失败原因必须留痕，实际 %q", failed.LastError)
	}
	// ★ 三条必须原样保留
	if failed.Catalog.Version != published.Version {
		t.Fatalf("失败换掉了快照版本：%q → %q", published.Version, failed.Catalog.Version)
	}
	if len(failed.Catalog.Models) != len(published.Models) {
		t.Fatalf("失败清空了快照内容：%d → %d 个模型", len(published.Models), len(failed.Catalog.Models))
	}
	if !failed.PublishedAt.Equal(base) {
		t.Fatalf("失败改了发布时间：%v → %v", base, failed.PublishedAt)
	}
	if failed.FailedAttempts() != 1 {
		t.Fatalf("FailedAttempts 应为 1，实际 %d", failed.FailedAttempts())
	}

	// 再失败一次：仍然保留，且计数继续累加。
	again := s.Fail(nil) // nil 也要留痕
	if again.Sequence != 1 || again.Attempts != 3 || again.FailedAttempts() != 2 {
		t.Fatalf("第二次失败后应为 seq=1 attempts=3 failed=2；实际 %+v", again)
	}
	if again.LastError == "" {
		t.Fatal("Fail(nil) 也必须留下痕迹 —— 否则「失败过」看不出来")
	}
}

// TestSnapshotPublishClearsErrorAndAdvances 成功发布清空错误并推进两把尺子。
func TestSnapshotPublishClearsErrorAndAdvances(t *testing.T) {
	s := NewCatalogSnapshotStore()
	s.Publish(BuildCatalog([]*Channel{catCh("a", []string{"m1"}, nil)}))
	s.Fail(errors.New("boom"))
	if s.Read().LastError == "" {
		t.Fatal("前置条件不成立：应先有 LastError")
	}

	// 内容不变也再发布一次 —— Sequence 前进了，Version 不变。
	before := s.Read()
	same := s.Publish(before.Catalog)
	if same.LastError != "" {
		t.Fatalf("成功发布应清空 LastError，实际 %q", same.LastError)
	}
	if same.Sequence != 2 {
		t.Fatalf("再发布一次 Sequence 应为 2（发布动作发生了），实际 %d", same.Sequence)
	}
	if same.Catalog.Version != before.Catalog.Version {
		t.Fatalf("内容相同的两次发布 Version 不该变：%q → %q", before.Catalog.Version, same.Catalog.Version)
	}
	// ⚠ FailedAttempts 是**累计**失败次数（Attempts − Sequence），成功**不**清零。
	//
	// 曾想把断言写成 0（"成功后清零"）—— 那是把两件事混了：
	//   · "现在这份快照是不是好的" → LastError 为空即说明最近一次尝试成功
	//   · "总共失败过几次"         → 累计值，只增不减
	// 清零会让"这台机器历史上抖过 40 次"彻底不可见。
	//   publish(1) → att=1 seq=1
	//   fail       → att=2 seq=1
	//   publish(2) → att=3 seq=2   ⇒ 累计失败 = 1
	if same.Attempts != 3 || same.Sequence != 2 {
		t.Fatalf("应为 att=3 seq=2，实际 att=%d seq=%d", same.Attempts, same.Sequence)
	}
	if got := same.FailedAttempts(); got != 1 {
		t.Fatalf("累计失败次数应为 1，实际 %d", got)
	}
	// 而"最近一次是好的"由 LastError 空表达 —— 两件事各由一个字段说清。
	if same.LastError != "" {
		t.Fatalf("最近一次成功后 LastError 应为空，实际 %q", same.LastError)
	}
}

// TestSnapshotStaleAge 快照年龄用于"这份有多旧"的展示。
func TestSnapshotStaleAge(t *testing.T) {
	s := NewCatalogSnapshotStore()
	base := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	s.Publish(BuildCatalog(nil))

	snap := s.Read()
	if got := snap.Stale(base.Add(90 * time.Second)); got != 90*time.Second {
		t.Fatalf("Stale 应为 90s，实际 %v", got)
	}
	// 时钟回拨不得给出负数（否则前端会显示"-3 秒前"）。
	if got := snap.Stale(base.Add(-time.Minute)); got != 0 {
		t.Fatalf("时钟回拨时 Stale 应为 0，实际 %v", got)
	}
	// 从未发布过 ⇒ 没有时间基准 ⇒ 0。
	if got := (CatalogSnapshot{}).Stale(base); got != 0 {
		t.Fatalf("未发布过时 Stale 应为 0，实际 %v", got)
	}
}

// ── 端点名的对外映射（计划 U-04 验收「image_generation 保留」）──────────────

// TestCatalogEmitsPublicEndpointNames 目录对外只发 `image_generation`，绝不发内部名 `images`。
//
// # 为什么这条必须单独钉
//
//	内部（SupportsEndpoint / 路由 / 渠道能力表）  "images"
//	对外（/v1/models 的 capabilities）            "image_generation"
//
// 现有 `/v1/models` 发的是后者（handler.go 的 capabilities）。统一目录若照抄
// 内部名发 "images"，同一个网关就对同一个能力给出两个名字 —— 客户端按名字
// 匹配能力，两边不一致就会出现"列表里有、调不了"。
func TestCatalogEmitsPublicEndpointNames(t *testing.T) {
	// ch-a 只支持 images（ModelEndpoints 显式声明）。
	a := &Channel{
		ID: "a", Name: "a", Kind: "standard", Status: StatusEnabled,
		Models:         []string{"paint"},
		ModelEndpoints: map[string][]string{"paint": {"images"}},
	}
	got := BuildCatalog([]*Channel{a})

	if len(got.Models) != 1 {
		t.Fatalf("应有 1 个公开模型，实际 %d", len(got.Models))
	}
	m := got.Models[0]
	if !containsString(m.Endpoints, "image_generation") {
		t.Fatalf("★ 模型端点应含 image_generation（对外名），实际 %v", m.Endpoints)
	}
	for _, ep := range m.Endpoints {
		if ep == "images" {
			t.Fatalf("★ 目录对外不能出现内部名 \"images\"，实际 %v", m.Endpoints)
		}
	}
	// 每条供给也要映射过。
	for _, p := range m.Providers {
		for _, ep := range p.Endpoints {
			if ep == "images" {
				t.Fatalf("供给 %s 的端点里出现了内部名：%v", p.ChannelID, p.Endpoints)
			}
		}
	}

	// 反向：只支持 chat 的模型只发 chat，不多发 image_generation。
	b := &Channel{ID: "b", Name: "b", Kind: "standard", Status: StatusEnabled,
		Models: []string{"talk"}}
	got = BuildCatalog([]*Channel{b})
	for _, ep := range got.Models[0].Endpoints {
		if ep == "image_generation" {
			t.Fatalf("只支持 chat 的模型不该有 image_generation，实际 %v", got.Models[0].Endpoints)
		}
	}
}

// TestPublicEndpointMapping 映射函数本身：只映射 images，其余原样。
func TestPublicEndpointMapping(t *testing.T) {
	cases := map[string]string{
		"images": "image_generation",
		"chat":   "chat",
		"":       "",
		"other":  "other",
	}
	for in, want := range cases {
		if got := PublicEndpoint(in); got != want {
			t.Fatalf("PublicEndpoint(%q) = %q，want %q", in, got, want)
		}
	}
	// 幂等：映射过的不再变。
	if got := PublicEndpoint(PublicEndpoint("images")); got != "image_generation" {
		t.Fatalf("映射应幂等，二次得到 %q", got)
	}
}

// ── 判据 4：别名歧义（U-09 别名兼容段）────────────────────────────────────

// TestCatalogReportsAliasAmbiguity ★ 一语两解必须报出来，不能默默选一个。
//
//	渠道 A：pub → real   （real 是 pub 背后的真实模型）
//	渠道 B：real → real2 （real 自己又是一个对外别名）
//
// 请求 `real` 有两个答案；现有解析链会让"是键"那一解胜出（静默）。
// 而 `/v1/models` 会把 pub 与 real 都列出来 —— 用户复制 real 得到的是 real2。
func TestCatalogReportsAliasAmbiguity(t *testing.T) {
	a := catCh("a", []string{"real"}, map[string]string{"pub": "real"})
	b := catCh("b", []string{"real2"}, map[string]string{"real": "real2"})

	got := BuildCatalog([]*Channel{a, b})

	var found *CatalogConflict
	for i := range got.Conflicts {
		if got.Conflicts[i].Public == "real" && got.Conflicts[i].Reason == ConflictAliasAmbiguous {
			found = &got.Conflicts[i]
		}
	}
	if found == nil {
		t.Fatalf("★ `real` 既是对外别名又是真实模型 ⇒ 必须报 alias_ambiguous，实际 %+v", got.Conflicts)
	}
	if len(found.Channels) != 2 {
		t.Fatalf("应报出涉及的两条渠道，实际 %v", found.Channels)
	}
}

// TestCatalogSelfMappingIsNotAmbiguous 映射到自己不算歧义（两种解同一目标）。
func TestCatalogSelfMappingIsNotAmbiguous(t *testing.T) {
	a := catCh("a", []string{"pub"}, map[string]string{"pub": "pub"})
	got := BuildCatalog([]*Channel{a})
	for _, c := range got.Conflicts {
		if c.Reason == ConflictAliasAmbiguous {
			t.Fatalf("自映射不该报歧义，实际 %+v", c)
		}
	}
}

// TestCatalogPureAliasIsNotAmbiguous 只有一种解时不算歧义。
func TestCatalogPureAliasIsNotAmbiguous(t *testing.T) {
	// pub 是别名，real 只作为目标出现（没有任何渠道把它当别名键）。
	a := catCh("a", []string{"real"}, map[string]string{"pub": "real"})
	got := BuildCatalog([]*Channel{a})
	for _, c := range got.Conflicts {
		if c.Reason == ConflictAliasAmbiguous {
			t.Fatalf("单一解不该报歧义，实际 %+v", c)
		}
	}
}

// TestCatalogAliasAmbiguityChangesVersion 歧义变化必须改变版本（否则快照比较失效）。
func TestCatalogAliasAmbiguityChangesVersion(t *testing.T) {
	a := catCh("a", []string{"real"}, map[string]string{"pub": "real"})
	clean := BuildCatalog([]*Channel{a})

	// 再加一条把 real 变成别名的渠道 ⇒ 出现歧义 ⇒ 版本必变。
	b := catCh("b", []string{"real2"}, map[string]string{"real": "real2"})
	dirty := BuildCatalog([]*Channel{a, b})

	if clean.Version == dirty.Version {
		t.Fatal("出现别名歧义后 Version 未变 —— 版本没有覆盖 Conflicts")
	}
}
