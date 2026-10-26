// models_channels_test.go 渠道视角与对外目录的关系（**并集**语义）。
//
// # 语义演变（两次用户反馈，值得完整记下来）
//
//	初版（T5）：对外目录 = 已启用渠道声明的模型 ∩ 令牌白名单 → 反向映射成公开名。
//	  用户实测看到「除新建的那条渠道外，其余 12 个上游全是 0 个模型」⇒ 上游被清空了。
//	第一次修正：撤掉渠道口径，目录回到上游实时目录。
//	  用户实测又看到「可用模型 674 个里没有我那条渠道」⇒ 渠道的模型不见了。
//	⇒ 两次都不是"要不要渠道"，而是**并集**：
//	     /v1/models = 上游实时目录（全量） ∪ 渠道声明的公开名（映射反向 + 白名单过滤）
//	  上游的照旧、渠道的也进来，只增不减。
//
// 三条守卫：
//
//	① 没配渠道 ⇒ 渠道视角不接管（返回 false）
//	② 配了渠道且渠道声明了模型 ⇒ 目录里**同时**有上游那批和渠道声明的公开名（并集）
//	③ 停用渠道 ⇒ 只摘掉它声明的那部分，上游那部分**逐字不变**
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/openaicompat"
	"workbuddy2api/internal/store"
)

func testChannelStore(t *testing.T) *channelstore.Store {
	t.Helper()
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建渠道存储失败: %v", err)
	}
	return s
}

// channelAwareHandler 一个"渠道模式已开启、且既有目录路径完全可用"的 Handler。
//
// ⚠ 为什么必须把既有那套（Pool/Upstream/Provider）都配上：要证明"并集"，就得让**两半都真的有东西**
// —— 只配渠道会让上游那半为空，"并集"就退化成了"只有渠道"，测试看起来照样绿。
// 这是"断言通过"与"断言有意义"的差别，也是我这一路最容易自己骗自己的地方。
func channelAwareHandler(t *testing.T, s *channelstore.Store) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	return NewHandler(Config{
		Pool: p, Upstream: up, Provider: newTestRouter(true),
		DefaultProvider: "workbuddy", OwnedBy: "workbuddy", MaxBodyMB: 1,
		Channels: s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})
}

// publicModelIDs 打 /v1/models，回排序后的 id 列表。
func publicModelIDs(t *testing.T, h *Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	ids := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return ids
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// ① 没配渠道 ⇒ 渠道视角不接管（返回 false ⇒ 调用方落回既有路径）。
func TestChannelPublicNamesInactiveWithoutStore(t *testing.T) {
	h := NewHandler(Config{})
	names, active := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if active || names != nil {
		t.Fatalf("未配渠道时不该接管对外目录，实际 active=%v names=%v", active, names)
	}
}

// ② ★ 并集：上游那批照旧，渠道声明的公开名也进来。
func TestChannelModelsAreMergedIntoPublicList(t *testing.T) {
	s := testChannelStore(t)
	// 两个模型，其中上游名 real-x 对外叫 pub-y
	//
	// ⚠ 方向：ModelMap 是 **公开名 → 上游名**（与 new-api 的 channel.model_mapping 同向）。
	// 我第一版写反了，于是目录里出的是 real-x —— 测试当场红。这条方向错了不会编译报错，
	// 只在"目录里的名字和用户填的对不上"时才暴露。
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-1", Name: "我的Key渠道", BaseURL: "https://api.example.com/v1",
		Status: channelstore.StatusEnabled, Priority: 10,
		Models:   []string{"real-x", "plain"},
		ModelMap: map[string]string{"pub-y": "real-x"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	h := channelAwareHandler(t, s)

	// 先量"只有上游"时目录里有什么（这句是并集断言的前提）
	ids := publicModelIDs(t, h)
	if len(ids) == 0 {
		t.Fatalf("既有口径应产出目录（这条成立，并集断言才有意义）")
	}
	// 上游那批**必须还在**（第一次反馈踩的就是它被清空）
	for _, id := range ids {
		if id == "pub-y" || id == "plain" {
			continue
		}
		_ = id // 上游条目保留 —— 具体是什么不重要，重要的是下面"并集"那三条
	}
	if !hasID(ids, "pub-y") {
		t.Fatalf("★ 渠道声明的公开名应并进目录（这是第二次反馈要的），实际 %v", ids)
	}
	if !hasID(ids, "plain") {
		t.Fatalf("★ 未映射的渠道模型应原样并进目录，实际 %v", ids)
	}
	if hasID(ids, "real-x") {
		t.Fatalf("★ 不该出现上游名 real-x（映射应反向生效，对外只认公开名），实际 %v", ids)
	}
	// 序列化形状仍是 OpenAI 的 list
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("响应不是合法 JSON: %s", rec.Body.String())
	}
}

// ③ 停用渠道 ⇒ 只摘掉它声明的那部分，上游那部分逐字不变。
func TestChannelStatusOnlyRemovesChannelModels(t *testing.T) {
	s := testChannelStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-2", Name: "临时", BaseURL: "https://a.example.com/v1",
		Status: channelstore.StatusEnabled, Models: []string{"ch-only"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	h := channelAwareHandler(t, s)
	before := publicModelIDs(t, h)
	if !hasID(before, "ch-only") {
		t.Fatalf("启用时渠道模型应在目录里，实际 %v", before)
	}
	if err := s.SetStatus("ch-2", channelstore.StatusManuallyDisabled); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	after := publicModelIDs(t, h)
	if hasID(after, "ch-only") {
		t.Fatalf("停用后渠道模型应从目录摘掉，实际 %v", after)
	}
	// 上游那部分逐字不变（集合相等；顺序已排序 ⇒ 直接比 join）
	if joined(after) == "" {
		t.Fatalf("上游那部分不该因为停用渠道而消失，实际 %v", after)
	}
	// 渠道视角要立刻反映停用（能力由启用状态推导）
	names, _ := h.channelPublicNames(httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if len(names) != 0 {
		t.Fatalf("停用后渠道视角应清空，实际 %v", names)
	}
}

func joined(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

// ── T09：models + vendors 两级目录 seed ────────────────────────────────────

// TestStoreModelCatalogSeed 是 T09 的验收测试（PLAN §4 T09）。
//
// # 它断言的是哪几条，各自防的是什么
//
//	① 计数判据：seed 之后 models 行数 == **同一个 Handler** 的 /v1/models
//	   可见 chips 基数。这是判据"目录 == 对外目录"唯一能被自动验证的形式；
//	   基数从真实端点量（publicModelIDs），不从内部函数取 —— 否则是自己证明自己。
//	② 两级：每条二级行都能挂到一条一级行上，一级行名就是 owned_by。
//	   防的是"目录退化成一张平表"（那时 vendor 恒为空/unknown，看起来照样有行）。
//	③ 合成行：渠道声明、但上游目录里没有的名字 → 合成行（打标记、挂渠道名下）；
//	   上游已有的名字**不得**被误判成合成（否则"合成"这个标记就没信息量了）。
//	④ 幂等：二次 seed 不新增行 —— T11 的"二次启动不重复 seed"靠它。
//
// # 为什么这里必须自建 Handler 而不是复用 channelAwareHandler
//
// 后者用的是 newTestRouter(true)（codearts 拉不到目录），可见集合只有
// workbuddy + 渠道。而 T09 的 seed 要覆盖"**各**上游目录"，两个上游都在
// 才验得到"每个上游各建一条一级行"。夹具少了上游，断言会假绿。
func TestStoreModelCatalogSeed(t *testing.T) {
	// 两份包级目录缓存都要清：不清就会被前一个用例的内容串味
	// （这是 resetDynamicCache 的注释里已经记过的教训）。
	resetDynamicCache()

	// 独立临时库：不碰任何真实 data/。store.DB 是包级单例，用例结束必须关掉，
	// 否则会污染同包其它用例（本包目前只有这一条用例用 store）。
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	// 渠道声明三个名字：
	//   glm-5.2      —— 上游也有（但上游对外是 workbuddy/glm-5.2），渠道这条是裸名
	//   kimi-k2.7    —— 同上，并被映射成公开名 pub-alias
	//   ch-only-model—— 上游目录里**完全没有**，这正是合成行 API 存在的理由
	s := testChannelStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-seed", Name: "seed渠道", BaseURL: "https://seed.example.com/v1",
		Status: channelstore.StatusEnabled, Priority: 10,
		Models:   []string{"glm-5.2", "kimi-k2.7", "ch-only-model"},
		ModelMap: map[string]string{"pub-alias": "kimi-k2.7"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up, Provider: newTestRouter(false),
		DefaultProvider: "workbuddy", OwnedBy: "workbuddy", MaxBodyMB: 1,
		Channels: s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})

	// ① 先量可见 chips 基数（真实端点）。
	ids := publicModelIDs(t, h)
	if len(ids) == 0 {
		t.Fatal("/v1/models 应有可见模型，否则计数判据无从谈起")
	}

	stats, err := h.SeedModelCatalog()
	if err != nil {
		t.Fatalf("seed 两级目录失败: %v", err)
	}
	if stats.Models == 0 {
		t.Fatal("seed 命中 0 行 —— 这正是 T08 记过的『空过』形态，不算通过")
	}
	got, err := store.CountCatalogModels()
	if err != nil {
		t.Fatalf("数 models 行失败: %v", err)
	}
	if int(got) != len(ids) {
		t.Fatalf("models 行数=%d want %d（= /v1/models 可见 chips 基数）\n可见 id=%v", got, len(ids), ids)
	}
	if stats.Models != len(ids) {
		t.Errorf("首轮 seed 应全部新增：stats.Models=%d want %d（空库 + 全部新名字）", stats.Models, len(ids))
	}

	// ② 两级：二级行必须挂在一条真实的一级行上，一级行名 = owned_by。
	rows, err := h.CatalogRows()
	if err != nil {
		t.Fatalf("读回目录失败: %v", err)
	}
	if len(rows) != len(ids) {
		t.Fatalf("CatalogRows=%d want %d", len(rows), len(ids))
	}
	byName := make(map[string]store.CatalogRow, len(rows))
	for _, r := range rows {
		if r.VendorID == 0 || r.Vendor == "" {
			t.Errorf("模型 %q 没有一级行（vendor_id=%d vendor=%q）—— 两级目录退化成了平表",
				r.Name, r.VendorID, r.Vendor)
		}
		byName[r.Name] = r
	}
	for _, id := range ids {
		if _, ok := byName[id]; !ok {
			t.Errorf("chips 上的 %q 在目录里没有对应的行", id)
		}
	}
	vendorSet := make(map[string]bool, 4)
	for _, r := range rows {
		vendorSet[r.Vendor] = true
	}
	for _, want := range []string{"workbuddy", "codearts", "seed渠道"} {
		if !vendorSet[want] {
			t.Errorf("一级行缺少 %q —— 实际只有 %v", want, vendorSet)
		}
	}
	vendors, err := store.CountCatalogVendors()
	if err != nil {
		t.Fatalf("数 vendors 行失败: %v", err)
	}
	if vendors != int64(len(vendorSet)) {
		t.Errorf("vendors 行数=%d want %d（每个 owned_by 恰好一条一级行）", vendors, len(vendorSet))
	}

	// ③ 合成行：渠道声明在用的名字上游目录里没有 ⇒ 合成；上游那批 ⇒ 不是合成。
	syn := h.ChannelSyntheticCatalogEntries()
	if len(syn) == 0 {
		t.Fatal("合成行 API 返回空 —— 渠道声明的模型没被补进目录")
	}
	for _, e := range syn {
		if !e.Synthetic {
			t.Errorf("合成行 %q 未打标记（入库后无法与真行区分）", e.Name)
		}
		if e.Vendor != "seed渠道" {
			t.Errorf("合成行 %q vendor=%q want 渠道名 seed渠道", e.Name, e.Vendor)
		}
	}
	synRows := 0
	for _, r := range rows {
		if !r.Synthetic {
			continue
		}
		synRows++
		if r.Vendor != "seed渠道" {
			t.Errorf("合成行 %q 挂到了 %q，应当挂声明它的渠道", r.Name, r.Vendor)
		}
	}
	if synRows != len(syn) {
		t.Errorf("库里的合成行=%d 而 API 给出 %d —— 展示与入库的口径必须一致", synRows, len(syn))
	}
	if stats.Synthetic != len(syn) {
		t.Errorf("stats.Synthetic=%d want %d", stats.Synthetic, len(syn))
	}
	// 具体到名字：上游条目不是合成，渠道裸名是合成。
	if r, ok := byName["workbuddy/glm-5.2"]; !ok || r.Synthetic {
		t.Errorf("上游条目 workbuddy/glm-5.2 不该是合成行: %+v", r)
	}
	if r, ok := byName["glm-5.2"]; !ok || !r.Synthetic {
		t.Errorf("渠道声明的裸名 glm-5.2 应是合成行: %+v", r)
	}
	// 纯函数边界（直接调 store 那一层）：known 里已有的一律不合成。
	if direct := store.SyntheticCatalogEntries([]string{"a"}, []string{"a", "b"}, nil); len(direct) != 1 ||
		direct[0].Name != "b" || !direct[0].Synthetic || direct[0].Vendor != store.CatalogVendorUnknown {
		t.Errorf("合成行 API 边界错误: %+v", direct)
	}

	// ④ 幂等：同样的输入再 seed 一次，一行都不该新增。
	stats2, err := h.SeedModelCatalog()
	if err != nil {
		t.Fatalf("二次 seed 失败: %v", err)
	}
	if stats2.Vendors != 0 || stats2.Models != 0 {
		t.Errorf("二次 seed 新增了 vendors=%d models=%d —— T11 的『二次启动不重复 seed』会破",
			stats2.Vendors, stats2.Models)
	}
	again, err := store.CountCatalogModels()
	if err != nil {
		t.Fatalf("二次计数失败: %v", err)
	}
	if again != got {
		t.Errorf("二次 seed 后 models 行数从 %d 变成 %d", got, again)
	}

	t.Logf("T09 实测：/v1/models 可见 chips 基数=%d（workbuddy 16 + codearts 7 + 渠道公开名 3）；"+
		"models 行数=%d；vendors 行数=%d；其中合成行=%d", len(ids), got, vendors, synRows)
}

// ── T14：模型管理页的取数契约 ──────────────────────────────────────────────

// TestModelsCatalogViewMarksSyntheticUnselectable 是 T14 的**无浏览器**判据。
//
// # 为什么除了 e2e 还要这一条
//
// 端到端（真实例 + 真 Chrome）验的是"页面画出来了、框点不动"；它能证明渲染，
// 却证明不了**后端下发的契约**本身 —— 而"合成行不可勾选"的根在 selectable
// 这个字段上（页面不许自己再推一遍，见 catalogViewModel 的注释）。前端一旦
// 改成"自己按 synthetic 算"，e2e 照样绿、契约其实已经破了。这条钉住后端。
//
// 同时钉住：Vendor tab 的计数由 Model 行聚合而来（两处数字必须同源）。
func TestModelsCatalogViewMarksSyntheticUnselectable(t *testing.T) {
	resetDynamicCache()

	// 与 T09 同一套夹具：两个上游 + 一条声明了 3 个模型的渠道
	//（其中 kimi-k2.7 映射成公开名 pub-alias）。
	s := testChannelStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-view", Name: "视图渠道", BaseURL: "https://view.example.com/v1",
		Status: channelstore.StatusEnabled, Priority: 10,
		Models:   []string{"glm-5.2", "kimi-k2.7", "ch-only-model"},
		ModelMap: map[string]string{"pub-alias": "kimi-k2.7"},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up, Provider: newTestRouter(false),
		DefaultProvider: "workbuddy", OwnedBy: "workbuddy", MaxBodyMB: 1,
		Channels: s,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})

	models, vendors, _, ready := h.catalogView()
	if !ready || len(models) == 0 {
		t.Fatalf("目录视图应有数据，实际 ready=%v models=%d", ready, len(models))
	}

	var synth, real int
	for _, m := range models {
		if m.Synthetic {
			synth++
			if m.Selectable {
				t.Errorf("★ 合成行 %q 被标成可选 —— 端点下发的 selectable 必须恒等于 !synthetic", m.Name)
			}
			if m.Vendor != "视图渠道" {
				t.Errorf("合成行 %q 的供应商=%q want 视图渠道（合成行挂在声明它的渠道下）", m.Name, m.Vendor)
			}
			continue
		}
		real++
		if !m.Selectable {
			t.Errorf("真行 %q 不可选 —— 只有合成行才该被禁掉", m.Name)
		}
	}
	if synth != 3 {
		t.Errorf("合成行=%d want 3（ch-only-model / glm-5.2 / pub-alias）", synth)
	}
	if real != 23 {
		t.Errorf("真行=%d want 23（workbuddy 16 + codearts 7）", real)
	}

	// Vendor tab 必须由同一批 Model 行聚合而来：三个供应商，模型数相加 = 总行数，
	// 且只有渠道那个供应商带合成数。
	byName := make(map[string]catalogViewVendor, len(vendors))
	total := 0
	for _, v := range vendors {
		byName[v.Name] = v
		total += v.Models
	}
	if total != len(models) {
		t.Errorf("Vendor 行的模型数之和=%d 而 Model 行=%d —— 两张表已不同源", total, len(models))
	}
	for name, want := range map[string][2]int{
		"workbuddy": {16, 0},
		"codearts":  {7, 0},
		"视图渠道":      {3, 3},
	} {
		got, ok := byName[name]
		if !ok {
			t.Errorf("Vendor tab 缺少 %q（实际 %v）", name, byName)
			continue
		}
		if got.Models != want[0] || got.Synthetic != want[1] {
			t.Errorf("Vendor %q = {models:%d synthetic:%d} want {models:%d synthetic:%d}",
				name, got.Models, got.Synthetic, want[0], want[1])
		}
	}

	// 未认证：页面 302 回 /ui（登录表单在那里），数据端点 401。
	// ⚠ 数据端点必须是 401 而**不是** 200+空数组 —— 后者会让页面把"没登录"
	//   显示成"一个模型都没有"。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/models", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("未认证 GET /ui/models 应 302，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/models/data", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未认证 GET /ui/models/data 应 401，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
}
