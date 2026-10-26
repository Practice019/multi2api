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
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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

// channelBaseURL 取渠道行的 base_url 值（契约上该列是 *string，A1 按 hyxapi 对等改的）。
//
// # 为什么必须走一个 helper 而不是到处写 *ch.BaseURL
//
//  1. **nil 解引用会 panic** —— 一次"契约或 seed 出错"会表现成测试崩溃（假红），
//     而不是一条能读懂原因的断言失败；
//  2. **断言消息里要打的是值，不是地址**：%q 打 *string 输出的是 0xc000… 这样的指针值，
//     排障时等于没打（这一条最容易漏，因为它编译得过）。
//
// nil 统一映射成空串：与列上 not null default ” 的语义一致，于是比较变成纯粹的内容比较。
func channelBaseURL(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// TestChannelSeed —— ⚠ **T82 改了这条测试的语义**：清单仍然完整，但**不再写入渠道行**。
//
// # 这次语义变更的原因（写在这里，免得下一个人当成回归）
//
// 用户指令：「默认上游不要放入渠道里面」。13 个注册点是**账号池上游**（流量走
// auths/ 的凭证池），而渠道路径只服务 base_url + api_key 型上游。原来的判据
// 「首启必须新建 13 行渠道」**正是被否掉的那个行为**，所以它必须反过来：
// 一根渠道行都不该被创建；清单（RegistryChannelSeeds）本身不变，它仍是
// ① 覆盖度判据 ② 清理历史 seed 行时的身份来源 ③ 缺口比对。
//
// # 判据（每条都配"只有正确实现才通过"的形态）
//
//	① 空库调用 ⇒ Created/Matched/Removed 全 0，channels 0 行   ← 新语义的核心
//	② 造 13 行历史 seed（当年 SeedChannels 写的）+ 1 条**同名同址但真实**的渠道
//	③ 显式标记只落在历史行上，真实行不沾
//	④ 清理删掉 13 行、**留下真实那条**（同名同址也误删不了，因为它有 external_id）
//	⑤ 幂等：再调用 Removed=0、行数不变
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
	// 清理历史行用的就是这一对键，重复会让"该删的行"指向两条不同的东西。
	seen := make(map[string]string, len(seeds))
	for _, s := range seeds {
		key := s.Provider + "|" + s.BaseURL
		if prev, dup := seen[key]; dup {
			t.Fatalf("seed 身份重复 %s：%s 与 %s", key, prev, s.Source)
		}
		seen[key] = s.Source
	}
	// 清单覆盖度表（仍然要能一条条核对：这是"清单完整"这个判据的可读证据）
	fmt.Printf("T82 注册点清单覆盖度（%d 条，**只作清单/校验，不再写库**）:\n", len(seeds))
	for i, s := range seeds {
		line := fmt.Sprintf("  [%2d] provider=%-16s base_url=%-36s source=%s", i+1, s.Provider, s.BaseURL, s.Source)
		fmt.Println(line)
		t.Log(line)
	}

	// ① 新语义：空库上调用 —— **不写任何渠道行**。
	st0, err := h.SeedSQLChannels()
	if err != nil {
		t.Fatalf("SeedSQLChannels: %v", err)
	}
	if st0.Created != 0 || st0.Matched != 0 || st0.Removed != 0 {
		t.Fatalf("默认上游不该再 seed 成渠道行：Created=%d Matched=%d Removed=%d，want 0/0/0",
			st0.Created, st0.Matched, st0.Removed)
	}
	if n, err := store.CountChannels(); err != nil || n != 0 {
		t.Fatalf("空库调用后 channels 行数 = %d (err=%v)，want 0", n, err)
	}

	// ② 造历史遗留：13 行是当年 store.SeedChannels 写下的（这一步就是模拟 demo 库）。
	legacy, err := store.SeedChannels(seeds)
	if err != nil {
		t.Fatalf("构造历史 seed 行失败: %v", err)
	}
	if legacy.Created != 13 {
		t.Fatalf("历史行应建 13 条，实际 %d", legacy.Created)
	}
	// 再放一条**真实的**渠道：名字与 base_url 故意与清单第 1 条**完全一样**，
	// 唯一区别是它有 ch-* 外部 id —— 这条必须活下来（误删用户渠道是这条清理最坏的后果）。
	wbBase := seeds[0].BaseURL
	realID, err := store.CreateChannel(&store.Channel{
		Name: seeds[0].Provider, BaseURL: &wbBase,
		Status: store.ChannelStatusEnabled, ExternalID: "ch-realt82",
	})
	if err != nil {
		t.Fatalf("建真实渠道失败: %v", err)
	}
	if n, _ := store.CountChannels(); n != 14 {
		t.Fatalf("清理前应有 14 行（13 历史 + 1 真实），实际 %d", n)
	}

	// ③ 显式标记的证据：标记落在 setting 的 JSON 里，且只落在历史行上。
	marked, names, err := store.MarkRegistrySeedChannels(seeds)
	if err != nil {
		t.Fatalf("MarkRegistrySeedChannels: %v", err)
	}
	if marked != 13 {
		t.Fatalf("应标记 13 条历史行，实际 %d（names=%v）", marked, names)
	}
	rows, err := store.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	var markSample string
	sawReal := false
	for i := range rows {
		ch := &rows[i]
		if ch.ID == realID {
			sawReal = true
			if ch.Setting != nil && strings.Contains(*ch.Setting, store.RegistrySeedSettingKey) {
				t.Fatalf("真实渠道 %d 被误标记: %q", ch.ID, *ch.Setting)
			}
			continue
		}
		if ch.Setting == nil || !strings.Contains(*ch.Setting, store.RegistrySeedSettingKey) {
			t.Fatalf("历史行 %d（%s）没有被标记: %v", ch.ID, ch.Name, ch.Setting)
		}
		if markSample == "" {
			markSample = *ch.Setting
		}
	}
	if !sawReal {
		t.Fatalf("真实渠道 %d 不见了", realID)
	}
	var mark map[string]any
	if err := json.Unmarshal([]byte(markSample), &mark); err != nil {
		t.Fatalf("标记不是合法 JSON: %q（%v）", markSample, err)
	}
	if v, _ := mark[store.RegistrySeedSettingKey].(bool); !v {
		t.Fatalf("标记键 %s 应为 true，实际 %v", store.RegistrySeedSettingKey, mark)
	}
	if mark[store.RegistrySeedSettingKey+"_provider"] == nil {
		t.Fatalf("标记应带上 provider 便于排障，实际 %v", mark)
	}
	t.Logf("标记样例: %s", markSample)

	// ④ 清理：第二次调用删掉 13 行，真实那条（同名同址）留下。
	st2, err := h.SeedSQLChannels()
	if err != nil {
		t.Fatalf("清理调用失败: %v", err)
	}
	if st2.Marked != 0 {
		t.Errorf("已标记过的行不该重复标记，实际 Marked=%d", st2.Marked)
	}
	if st2.Removed != 13 {
		t.Fatalf("应清理 13 条历史行，实际 Removed=%d", st2.Removed)
	}
	left, err := store.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(left) != 1 || left[0].ID != realID || left[0].ExternalID != "ch-realt82" {
		t.Fatalf("清理后应只剩真实渠道 %d，实际 %+v", realID, left)
	}
	// 能力行也不能留悬空引用
	if n, err := store.CountAbilities(); err != nil || n != 0 {
		t.Fatalf("历史行本来没有能力行 ⇒ 清理后应仍为 0，实际 %d (err=%v)", n, err)
	}

	// ⑤ 幂等：第三次调用什么都不做。
	st3, err := h.SeedSQLChannels()
	if err != nil {
		t.Fatalf("三次调用失败: %v", err)
	}
	if st3.Marked != 0 || st3.Removed != 0 {
		t.Fatalf("第三次调用应无操作：Marked=%d Removed=%d，want 0/0", st3.Marked, st3.Removed)
	}
	if n, _ := store.CountChannels(); n != 1 {
		t.Fatalf("幂等调用后行数应仍为 1，实际 %d", n)
	}
}

// TestChannelSeedDoesNotDedupByProviderID 是"不得按 provider ID 去重"的直接反例。
//
// ⚠ T82 之后它守的对象变了：生产路径不再写渠道行，但**清理历史行**（以及将来的
// 显式写入）仍然是按 (name, base_url) 这对键判身份的。所以"两个同 provider 实例
// 是两个身份"这条必须继续成立 —— 否则清理会把两条实例当成一条。
//
// 形态取自 main.go:275 的 wb 与 :347 的 wbIntl：**同一个上游**的两个渠道实例，
// 只有端点不同。任何按 provider（上游实现 / 上游品牌 / ProviderID 常量）去重的
// 实现都会把它们并成一行 —— 这个用例会红。
func TestChannelSeedDoesNotDedupByProviderID(t *testing.T) {
	openSeedDB(t)

	// 基线：历史 seed 写下的 13 行（生产路径已不再调用 store.SeedChannels，
	// 这里用它来**构造**那个历史状态 —— 这正是它的保留用途之一）。
	if _, err := store.SeedChannels(RegistryChannelSeeds()); err != nil {
		t.Fatalf("构造历史 seed 行: %v", err)
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
	// ⚠ 必须比**值**而不是比指针：两个 *string 字段各自指向不同的字符串对象，
	// 直接写 wb[0].BaseURL == wb[1].BaseURL 是**指针比较**，恒 false ——
	// 那条"两个实例不该同名同端点"的断言会被静默弱化（不报错、永远绿）。
	if channelBaseURL(wb[0].BaseURL) == channelBaseURL(wb[1].BaseURL) {
		t.Fatalf("两个 workbuddy 实例的 base_url 不该相同: %q",
			channelBaseURL(wb[0].BaseURL))
	}

	// ★ T82 的第二半：清理**只**认清单里的那 13 个身份，绝不扩大口径。
	// 第 14 行同名但 base_url 不同 ⇒ 它不在清单里，必须留下。
	marked, _, err := store.MarkRegistrySeedChannels(RegistryChannelSeeds())
	if err != nil {
		t.Fatalf("MarkRegistrySeedChannels: %v", err)
	}
	if marked != 13 {
		t.Fatalf("只应标记清单里的 13 个身份，实际 %d（多标了就是把'同名不同址'也算进去）", marked)
	}
	removed, names, err := store.DeleteRegistrySeedChannels()
	if err != nil {
		t.Fatalf("DeleteRegistrySeedChannels: %v", err)
	}
	if removed != 13 {
		t.Fatalf("只应删 13 行，实际 %d（names=%v）", removed, names)
	}
	left, err := store.ListChannels()
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	if len(left) != 1 || left[0].Name != "workbuddy" || channelBaseURL(left[0].BaseURL) != "https://wb-intl.example.invalid" {
		t.Fatalf("留下的应是'同名不同址'那条，实际 %+v", left)
	}
}

// TestChannelSeedExpandsAbilities 覆盖"展开模型↔渠道绑定"的两条路：
//
//	seed 期   —— 渠道显式声明的模型（ChannelSeed.Models）
//	目录就绪后 —— T09 的两级目录（vendor = provider id ⇒ 渠道名）
//
// 默认清单的 Models 一律为空（模型目录是运行时事实，启动期不能打上游），
// 所以这里用一份显式声明了模型的 seed 来验展开本身。
//
// ⚠ T82：store.SeedChannels 已不再是生产启动路径（见它的注释），但这条测试仍然
// 有价值 —— 它验的是**能力展开本身**（去重、幂等、目录联查三件事），而那是
// ExpandAbilities / ExpandAbilitiesFromCatalog 的共用实现。
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
