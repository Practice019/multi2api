// channels_seed_test.go —— T08 的计数判据 + 三条"只有正确实现才会通过"的反例。
//
// # 判据（PLAN-hyxapi-alignment §4 T08）
//
//	① go test ./internal/admin/ -run TestChannelSeed -count=1 必须**命中 >0 条测试**
//	   （改前该 -run 是 "[no tests to run]" 的空过，exit=0 但什么都没验）；
//	② channels 行数 = 13 —— main.go 的 13 处 registry.Register，
//	   **不得按 provider ID / 上游实现去重**。
//
// # 为什么每条判据都配反例
//
// "13 行"这个数字可以被很多错实现凑出来（比如手写 13 条 INSERT，或者按上游
// 包名去重后恰好也是 13）。所以这里让每条判据都有一处**只有正确实现才通过**的
// 形态：
//
//	TestChannelSeed                      —— 13 行 + 逐行 provider/id 对照 + 幂等
//	TestChannelSeedDoesNotDedupByProviderID —— workbuddy 两个实例的形态（同名不同端点）
//	TestChannelSeedExpandsAbilities      —— 能力行展开 + 目录联查展开 + 重复不翻倍
//	TestChannelSeedGapsAgainstRegistry   —— 清单与真实注册表对得上
//
// # 数据库隔离
//
// store.DB 是包级单例（db.go 的注释写明了代价），所以本文件的每个用例各开一个
// t.TempDir() 下的库，并在 Cleanup 里 Close。本包测试**不加 t.Parallel()**
// （全包现状如此），因此不存在"两个用例同用一个库"的串扰。
package admin

import (
	"fmt"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/store"
)

// openSeedDB 开一个临时库并建表，Cleanup 时关掉。
func openSeedDB(t *testing.T) {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
}

// seedHandler 造一个只带注册表的管理台 Handler（其余依赖留空 —— seed 不需要它们）。
func seedHandler(reg *gateway.Registry) *Handler {
	return New(Config{AdminToken: testAdminToken, ListensLoopback: true, Registry: reg})
}

// TestChannelSeed 是验收②的正面判据：13 个注册点 ⇒ 13 行，逐行可对照。
func TestChannelSeed(t *testing.T) {
	openSeedDB(t)
	h := seedHandler(nil)

	seeds := RegistryChannelSeeds()
	if len(seeds) != 13 {
		t.Fatalf("注册点目录 = %d 条，want 13（main.go 的 13 处 registry.Register）", len(seeds))
	}

	// 反例：渠道工厂不是注册点。
	//
	// internal/openaicompat 是第 12 个上游包，但它不进 registry ——
	// 它是作为**工厂**注入 main.go:1535-1537 与 :1666-1668（用户新建 Key 型渠道时
	// 按 (id, base_url) 现造 Provider）。它若出现在清单里就会凭空多一行。
	for _, s := range seeds {
		if s.Provider == "openaicompat" {
			t.Fatalf("openaicompat 是渠道工厂（main.go:1535-1537/:1666-1668 注入），不是注册点，不该进 seed 清单")
		}
	}

	// 反例：13 个注册点的身份 (provider, base_url) 必须两两不同 ——
	// 否则落库时会少行，而"13 行"的判据会静默失真。
	seen := make(map[string]string, len(seeds))
	for _, s := range seeds {
		key := s.Provider + "|" + s.BaseURL
		if prev, dup := seen[key]; dup {
			t.Fatalf("seed 身份重复 %s：%s 与 %s", key, prev, s.Source)
		}
		seen[key] = s.Source
	}

	// ① 首启 seed：新建 13 行。
	st, err := h.SeedSQLChannels()
	if err != nil {
		t.Fatalf("SeedSQLChannels: %v", err)
	}
	if st.Created != 13 {
		t.Fatalf("首启应新建 13 行渠道，实际 Created=%d Matched=%d", st.Created, st.Matched)
	}

	// ② 行数判据。
	n, err := store.CountChannels()
	if err != nil {
		t.Fatalf("CountChannels: %v", err)
	}
	if n != 13 {
		t.Fatalf("channels 行数 = %d，want 13（不得按 provider ID 去重）", n)
	}

	// ③ 逐行 provider/id 对照。
	//
	// 顺序由 store.ListChannels 保证（id 升序 = 插入顺序 = 注册顺序），
	// 所以第 i 行必然对应清单第 i 条 —— 对不上就是插入顺序或身份键错了。
	chans, err := store.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(chans) != 13 {
		t.Fatalf("ListChannels 回 %d 行，want 13", len(chans))
	}
	fmt.Printf("T08 channels seed 对照表（%d 行）:\n", len(chans))
	for i, s := range seeds {
		ch := chans[i]
		if ch.Name != s.Provider || ch.BaseURL != s.BaseURL {
			t.Fatalf("第 %d 行对不上：库里 name=%q base_url=%q，清单 provider=%q base_url=%q",
				i+1, ch.Name, ch.BaseURL, s.Provider, s.BaseURL)
		}
		if ch.Status != store.ChannelStatusEnabled {
			t.Fatalf("第 %d 行 status=%d，want %d（启用）", i+1, ch.Status, store.ChannelStatusEnabled)
		}
		line := fmt.Sprintf("  [%2d] id=%-2d provider=%-16s base_url=%-36s source=%s",
			i+1, ch.ID, ch.Name, ch.BaseURL, s.Source)
		fmt.Println(line)
		t.Log(line)
	}

	// ④ 幂等：再 seed 一次必须全部命中已有行（Created=0），行数不变。
	st2, err := h.SeedSQLChannels()
	if err != nil {
		t.Fatalf("二次 SeedSQLChannels: %v", err)
	}
	if st2.Created != 0 || st2.Matched != 13 {
		t.Fatalf("二次 seed 应全部命中：Created=%d Matched=%d，want 0/13", st2.Created, st2.Matched)
	}
	if n2, err := store.CountChannels(); err != nil || n2 != 13 {
		t.Fatalf("二次 seed 后行数 = %d (err=%v)，want 13", n2, err)
	}
}

// TestChannelSeedDoesNotDedupByProviderID 是"不得按 provider ID 去重"的直接反例。
//
// 形态取自 main.go:275 的 wb 与 :347 的 wbIntl：**同一个上游**的两个渠道实例，
// 只有端点不同。任何按 provider（上游实现 / 上游品牌 / ProviderID 常量）去重的
// 实现都会把它们并成一行 —— 这个用例会红。
func TestChannelSeedDoesNotDedupByProviderID(t *testing.T) {
	openSeedDB(t)
	h := seedHandler(nil)

	if _, err := h.SeedSQLChannels(); err != nil {
		t.Fatalf("SeedSQLChannels: %v", err)
	}

	// 同名 provider、不同 BaseURL 的一条新 seed ⇒ 必须新建一行，而不是命中已有行。
	st, err := store.SeedChannels([]store.ChannelSeed{{
		Provider: "workbuddy",
		BaseURL:  "https://wb-intl.example.invalid",
		Source:   "channels_seed_test.go（反例：同 provider 的第二个实例）",
	}})
	if err != nil {
		t.Fatalf("SeedChannels(第二实例): %v", err)
	}
	if st.Created != 1 || st.Matched != 0 {
		t.Fatalf("同 provider 不同 base_url 必须落成新行：Created=%d Matched=%d，want 1/0", st.Created, st.Matched)
	}

	n, err := store.CountChannels()
	if err != nil {
		t.Fatalf("CountChannels: %v", err)
	}
	if n != 14 {
		t.Fatalf("两个 workbuddy 实例应共存 ⇒ 14 行，实际 %d（按 provider 去重会把这里压回 13）", n)
	}

	// 两条 workbuddy 行的 base_url 必须不同（这就是它们没被并成一行的原因）。
	chans, err := store.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	var wb []store.Channel
	for _, ch := range chans {
		if ch.Name == "workbuddy" {
			wb = append(wb, ch)
		}
	}
	if len(wb) != 2 {
		t.Fatalf("名为 workbuddy 的渠道 = %d 行，want 2", len(wb))
	}
	if wb[0].BaseURL == wb[1].BaseURL {
		t.Fatalf("两个 workbuddy 实例的 base_url 不该相同: %q", wb[0].BaseURL)
	}
}

// TestChannelSeedExpandsAbilities 覆盖"展开模型↔渠道绑定"的两条路：
//
//	seed 期   —— 渠道显式声明的模型（ChannelSeed.Models）
//	目录就绪后 —— T09 的两级目录（vendor = provider id ⇒ 渠道名）
//
// 默认清单的 Models 一律为空（模型目录是运行时事实，启动期不能打上游），
// 所以这里用一份显式声明了模型的 seed 来验展开本身。
func TestChannelSeedExpandsAbilities(t *testing.T) {
	openSeedDB(t)

	st, err := store.SeedChannels([]store.ChannelSeed{
		{
			Provider: "codearts", BaseURL: "https://codearts.huaweicloud.com", Source: "test",
			// 含重复与空白项：展开必须先去重/去空，否则能力行会出现同 (group,model,channel) 的重复。
			Models: []string{"glm-5.2", "glm-5.2", "  "},
		},
		{
			Provider: "loomy", BaseURL: "https://loomyad.xunfei.cn/api/v1", Source: "test",
			Models: []string{"loomy-chat"},
		},
	})
	if err != nil {
		t.Fatalf("SeedChannels: %v", err)
	}
	if st.Abilities != 2 {
		t.Fatalf("能力行 = %d，want 2（codearts 去重后 1 条 + loomy 1 条）", st.Abilities)
	}
	if n, err := store.CountAbilities(); err != nil || n != 2 {
		t.Fatalf("abilities 行数 = %d (err=%v)，want 2", n, err)
	}

	chans, err := store.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(chans) != 2 {
		t.Fatalf("channels 行数 = %d，want 2", len(chans))
	}

	// 重复展开不翻倍（幂等的第二个面）。
	if n, err := store.ExpandAbilities(chans[0].ID, "", []string{"glm-5.2", "glm-5.2"}); err != nil || n != 0 {
		t.Fatalf("重复展开应新建 0 行，实际 %d (err=%v)", n, err)
	}

	// 目录联查展开：T09 的一级行 vendor 与本文件的渠道名同键（都是 provider id）。
	if _, err := store.SeedModelCatalog([]store.CatalogEntry{
		{Vendor: "codearts", Name: "glm-5.2"},     // 已有绑定 ⇒ 不该新建
		{Vendor: "codearts", Name: "glm-5.2-air"}, // 新模型 ⇒ 该新建 1 条绑定
		{Vendor: "loomy", Name: "loomy-chat"},     // 已有绑定 ⇒ 不该新建
		{Vendor: "nowhere", Name: "orphan-model"}, // 没有对应渠道 ⇒ 不该建绑定
	}); err != nil {
		t.Fatalf("SeedModelCatalog: %v", err)
	}
	n, err := store.ExpandAbilitiesFromCatalog("")
	if err != nil {
		t.Fatalf("ExpandAbilitiesFromCatalog: %v", err)
	}
	if n != 1 {
		t.Fatalf("目录联查应新建 1 条绑定（glm-5.2-air → codearts），实际 %d", n)
	}
	if total, err := store.CountAbilities(); err != nil || total != 3 {
		t.Fatalf("abilities 总行数 = %d (err=%v)，want 3", total, err)
	}

	// 能力行必须挂在**正确的**渠道上（不能只看总数）。
	abs, err := store.ListAbilities()
	if err != nil {
		t.Fatalf("ListAbilities: %v", err)
	}
	want := map[string]int64{"glm-5.2": chans[0].ID, "glm-5.2-air": chans[0].ID, "loomy-chat": chans[1].ID}
	for _, ab := range abs {
		wantID, ok := want[ab.Model]
		if !ok {
			t.Fatalf("能力行出现了不该有的 model=%q（group=%q channel_id=%d）", ab.Model, ab.Group, ab.ChannelID)
		}
		if ab.ChannelID != wantID {
			t.Fatalf("model %q 挂到了 channel_id=%d，want %d", ab.Model, ab.ChannelID, wantID)
		}
		if !ab.Enabled {
			t.Fatalf("model %q 的能力行该是 enabled", ab.Model)
		}
	}
}

// TestChannelSeedGapsAgainstRegistry 钉住"清单 vs 真实注册表"这条守卫：
// 清单是手写的，registry.Register 是真发生的，两者漂移的症状是
// 「某个上游在跑、渠道表里却没有它」，且没有任何报错。
func TestChannelSeedGapsAgainstRegistry(t *testing.T) {
	reg := gateway.NewRegistry()
	for _, s := range RegistryChannelSeeds() {
		if err := reg.Register(&stubProvider{id: s.Provider, caps: gateway.CapChat}); err != nil {
			t.Fatalf("注册桩 %s: %v", s.Provider, err)
		}
	}
	h := seedHandler(reg)
	if gaps := h.ChannelSeedGaps(); len(gaps) != 0 {
		t.Fatalf("注册表与 seed 清单必须一一对应，多出的 provider: %v", gaps)
	}

	if err := reg.Register(&stubProvider{id: "brand-new-upstream", caps: gateway.CapChat}); err != nil {
		t.Fatalf("注册桩 brand-new-upstream: %v", err)
	}
	gaps := h.ChannelSeedGaps()
	if len(gaps) != 1 || gaps[0] != "brand-new-upstream" {
		t.Fatalf("新增注册点应被报为缺口，实际 %v", gaps)
	}

	// Registry 为 nil（测试/未接线路径）时不该 panic，也不该报缺口。
	if got := seedHandler(nil).ChannelSeedGaps(); len(got) != 0 {
		t.Fatalf("无注册表时应回空缺口，实际 %v", got)
	}
}
