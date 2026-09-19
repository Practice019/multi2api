// models_multiprovider_test.go T1 的回归 + 新增断言：/v1/models 必须**同时**给出
// 两个上游的模型，且每个上游**只列一条带前缀的 id**（目录收窄，见下）。
//
// # 为什么这个文件先于实现存在（硬要求）
//
// `/v1/models` 是**公共端点**：所有客户端都调它。改错会让**所有上游**的模型消失，
// 而不只是新接的那个。所以顺序必须是：
//
//  1. 先让"workbuddy 的模型一条不少"在**改动前**是绿的（本文件的 baseline 用例）
//  2. 再动实现
//  3. 改完再跑：workbuddy 的模型仍在 + codearts 的新增
//
// 跳过第 1 步就无法区分"我改对了"与"本来就坏"。
//
// # 目录收窄：基线从 32 条变 16 条（本文件的口径随之改变）
//
// 收窄前默认上游（workbuddy）是"裸名 + 带前缀"两份：16 个模型 → 32 条，
// `glm-5.2` 与 `workbuddy/glm-5.2` 同时在列 —— 用户报的就是这个重复
// （原话"可用模型重复了，很多"）。收窄后每个上游只列一条 `provider/model`：
// workbuddy 16 条 + codearts 7 条 = 23 条。
//
// ⚠ 变少**不是**模型丢失：少的是同一个模型的第二种写法。
// 单上游模式（Provider 未注入）仍然只有裸名 16 条、一个字没动，
// 见 TestV1ModelsBaselineSingleProvider。
//
// # 基线值从哪来
//
// `.task/ui-convergence/shared/baselines/models-before-t1.txt`：在真实实例 18080
// 上实测得到的 32 条 id（16 个裸名 + 16 个 workbuddy/ 前缀）。
// 本文件只保留其中的**裸名 16 个**作为模型集合的来源；期望值改成它们的
// `workbuddy/` 前缀投影（收窄后不再展开裸名）。
// 裸名与代码里的 staticModels 不同（静态表只有 10 条且含 hy3-preview 等），
// 因为真实实例走的是**动态列表**路径（fetchDynamicModels 命中上游）。
// 本文件用桩上游复刻那条动态路径，因此模型集合可与真实实例逐字对齐。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// baselineWorkbuddyIDs 是改动前实测基线里的**裸名 16 个**（见源文件）。
//
// 它按**裸名 id** 给出；带 workbuddy/ 前缀的那 16 条是前缀投影，
// 由下面的期望函数生成，避免手抄时漏一条。
//
// ⚠ 收窄后目录里**没有**这些裸名 —— 本变量只用来生成前缀投影，
// 不能当成"期望输出"（期望输出是 baselineWorkbuddyExpected）。
// 多上游模式下它们必须**全部不出现**，见 TestV1ModelsIDsUniqueAndPrefixed。
var baselineWorkbuddyIDs = []string{
	"auto",
	"deepseek-v4-pro",
	"deepseek-v4.1-flash",
	"glm-5.1",
	"glm-5.2",
	"glm-5.3",
	"glm-5.3-flash",
	"glm-5v-turbo",
	"hy3",
	"hy3-x",
	"hy4-preview",
	"kimi-k2.6",
	"kimi-k2.7",
	"kimi-k2.8-preview",
	"kimi-k3-1",
	"minimax-m3",
}

// baselineWorkbuddyExpected 是收窄后默认上游在 /v1/models 里的期望值：
// **只有** 16 条 `workbuddy/` 前缀 id，没有任何裸名。
//
// 为什么从 32 变 16：收窄前每个模型列两份（裸名 + 前缀版），
// 现在只列一份带前缀的（用户要求"统一、不要重复"）。
// 模型集合本身一个都没少 —— 少的是同一个模型的第二种写法。
func baselineWorkbuddyExpected() []string {
	out := make([]string, 0, len(baselineWorkbuddyIDs))
	for _, id := range baselineWorkbuddyIDs {
		out = append(out, "workbuddy/"+id)
	}
	sort.Strings(out)
	return out
}

// baselineDynamicBody 是 workbuddy 动态模型接口的最小真实形状，
// 内容与基线文件里的 16 个 id 一一对应。
//
// ⚠ 必须带 `agents` 里名为 **cli** 的那一项：FetchModels 在 cliIDs 为空时
// 直接返回错误（"no cli agent models found"），于是整个动态路径失败、
// 回落到 staticModels。那种情况下测试会**假绿** —— 断言到的是静态表，
// 而不是真实实例上的动态列表。
func baselineDynamicBody() string {
	var ms, ids string
	for i, id := range baselineWorkbuddyIDs {
		if i > 0 {
			ms += ","
			ids += ","
		}
		ms += fmt.Sprintf(`{"id":%q,"maxInputTokens":131072,"maxOutputTokens":32768}`, id)
		ids += fmt.Sprintf("%q", id)
	}
	return `{"code":0,"data":{"models":[` + ms +
		`],"agents":[{"name":"cli","models":[` + ids + `]}]}}`
}

// testRouter 是本包内实现 ProviderRouter 的桩，用于模拟"多上游装配层"。
//
// 它刻意实现 IDs()（可选能力）—— modelList 靠接口断言发现它，
// 不实现的话"其余上游"永远是空的，测试就会**假绿**。
type testRouter struct {
	def    string
	ids    []string
	models map[string][]gateway.ModelInfo
	// fail 里的上游返回 ok=false（模拟"该上游现在给不出目录"）。
	fail map[string]bool
}

// 编译期断言：桩必须与出口层声明的接口**逐字一致**。
// 签名差一点会静默失配（见 tasks/plan.md 决策 E 的实测教训）。
var _ ProviderRouter = testRouter{}

func (r testRouter) Has(id string) bool {
	for _, x := range r.ids {
		if x == id {
			return true
		}
	}
	return false
}

func (r testRouter) Default() string { return r.def }

// IDs 出口层的**可选**枚举能力（providerIDs 用接口断言发现它）。
func (r testRouter) IDs() []string { return r.ids }

func (r testRouter) Models(_ context.Context, id string) ([]gateway.ModelInfo, bool) {
	if r.fail[id] {
		return nil, false
	}
	ms, ok := r.models[id]
	if !ok || len(ms) == 0 {
		return nil, false
	}
	return ms, true
}

// ModelMultipliers 测试桩：未实现官方倍率扩展点。
func (r testRouter) ModelMultipliers(_ context.Context, _ string) (map[string]float64, bool) {
	return nil, false
}

// Chat / Credential / RefreshCredential 是**出站**能力，本文件（目录合并）不涉及。
//
// 但它们必须存在：接口断言要求方法集精确匹配，少一个就编译不过
// （见 tasks/plan.md 决策 E 的实测教训 —— 签名差一点是静默失配，
// 少一个方法是编译期失败，后者反而是好事）。
//
// ⚠ Chat 返回 ok=false 而不是"假装成功"：本文件的用例都只调 /v1/models，
// 一旦哪天有人拿这个桩去测 chat，会立刻拿到一个明确的"上游没接上"，
// 而不是一个看起来正常的空流 —— 后者会让测试**假绿**。
func (r testRouter) Chat(_ context.Context, _ string, _ gateway.Credential, _ []byte) (gateway.ChatStream, bool, error) {
	return gateway.ChatStream{}, false, nil
}

func (r testRouter) Credential(_, _ string) (gateway.Credential, bool) {
	return gateway.Credential{}, false
}

func (r testRouter) RefreshCredential(_ context.Context, _ string, _ gateway.Credential) (bool, error) {
	return false, nil
}

// RefreshSkew 报告"距过期不足多久就该提前续期"。
//
// ⚠ 返回 `ok=false` = **本桩没有意见**，不是"不用刷"。
// 出口层据此回落到核心的通用兜底窗口（与改造前一致）——
// 所以既有测试的语义一字未变。
//
// 返回 `0, true` 含义完全不同：那是"本上游**明确声明**不需要提前刷"，
// 出口层**必须尊重**（不刷）。两者不可混用，见 provider_router.go 的注释。
func (r testRouter) RefreshSkew(_ string, _ gateway.Credential) (time.Duration, bool) {
	return 0, false
}

// ResetAt 报告"额度耗尽的号什么时候能再用"（gateway.ResetPolicyExt）。
//
// ⚠ 同样返回 `ok=false` = **本桩没有这个信息**，出口层回落核心的通用保守值。
// 本文件（目录合并）不涉及冷却路径，但接口断言要求方法集精确匹配。
// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
func (r testRouter) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

func (r testRouter) ResetAt(_ string, _ gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}

// Classify 用**该上游自己的**分类器判定错误类别（gateway.ErrorClassifier）。
//
// ⚠ 返回 `ok=false` = **本桩的这条上游没有分类器**，出口层回落到
// `upstream.Classify`（默认上游 workbuddy 的判据，也正是改造前的行为）。
//
// 刻意的取舍：本桩的上游都是虚构的（"p1"/"p2"），没有任何真实错误码表，
// 假装给一个分类反而会让测试看起来覆盖了分类分派而其实没有。
// 真正的分派覆盖在 chat_multiprovider_test.go 的 multiRouter（它转发到
// 具体上游的 recordingProvider，见那边的 Classify）。
func (r testRouter) Classify(_ string, _ int, _ string) (gateway.ErrorKind, bool) {
	return gateway.ErrKindNone, false
}

// resetDynamicCache 清动态模型缓存（与 handler_test.go 的写法同款）。
func resetDynamicCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// modelIDs 取 /v1/models 的 id 列表（已排序）。
func modelIDs(t *testing.T, h *Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object=%q want list", resp.Object)
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		s, _ := m["id"].(string)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// missing 返回 want 里缺的项（按 want 顺序）。
func missing(have []string, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	return out
}

// present 返回 want 里**确实出现**在 have 中的项（missing 的反向）。
//
// 用途：钉住"不该出现的东西出现了" —— 收窄后裸名不得再出现在多上游目录里，
// 而 missing 只能表达"该有的丢了"，表达不了"多出来了"。
func present(have []string, want []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if set[w] {
			out = append(out, w)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 回归基线：改动前就应当是绿的
// ---------------------------------------------------------------------------

// TestV1ModelsBaselineWorkbuddy16 锁定**收窄后**的 16 条基线。
//
// 这是目录收窄之后的**回归闸门**：无论实现怎么改，这 16 个模型必须一条不少，
// 且每条都必须是 workbuddy/ 前缀形态。
//
// # 为什么函数名从 ...Workbuddy32 改成了 ...Workbuddy16
//
// 32 是"每个模型列两份"时代的数字（16 个裸名 + 16 个前缀版）。收窄后
// 每个模型只列一条带前缀的 → 16。名字不改就是名不副实：一个叫 32 的用例
// 断言 16，后面的人只会以为它坏了。
//
// 装配层给了 Provider（多上游模式）。这里刻意只注册 workbuddy 一个上游，
// 让"总数"成为一条**有意义的**断言（否则 codearts 的 7 条会让总数变 23）。
func TestV1ModelsBaselineWorkbuddy16(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		// 多上游模式，但只有一个上游：目录里不该有别人的东西。
		Provider:        testRouter{def: "workbuddy", ids: []string{"workbuddy"}},
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	got := modelIDs(t, h)
	want := baselineWorkbuddyExpected()

	if len(want) != 16 {
		t.Fatalf("基线自检失败：期望表有 %d 条，应为 16（16 个模型 × 1 条）", len(want))
	}
	if miss := missing(got, want); len(miss) > 0 {
		t.Fatalf("★ 回归：收窄后 workbuddy 原有模型丢失 %d 条: %v\n实际共 %d 条: %v",
			len(miss), miss, len(got), got)
	}
	// 收窄的核心断言：16 条，不是 32 条。
	// 破了会怎样：裸名那一份又回来了 → 客户端下拉框里每个模型显示两次
	//（用户原话"可用模型重复了，很多"）。
	if len(got) != 16 {
		t.Fatalf("★ 回归：多上游模式下共 %d 条 want 16 —— 多出来的就是同一个模型的第二种写法（裸名 + provider/model 并存再次出现）: %v",
			len(got), got)
	}
	// 每一条都必须带 workbuddy/ 前缀：裸名重现 = 重复回归。
	for _, id := range got {
		if !strings.HasPrefix(id, "workbuddy/") {
			t.Errorf("★ 回归：出现了裸名 %q —— 收窄后多上游模式的每一条都必须是 provider/model", id)
		}
	}
}

// TestV1ModelsBaselineSingleProvider 单上游模式（Provider 未注入）同样锁死：
// 这条路径是既有部署与既有测试的路径，行为必须与改造前逐字节一致。
func TestV1ModelsBaselineSingleProvider(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, OwnedBy: "workbuddy"})

	got := modelIDs(t, h)
	// 单上游模式：只有裸名，没有任何前缀记录（改造前行为）。
	if miss := missing(got, baselineWorkbuddyIDs); len(miss) > 0 {
		t.Fatalf("★ 回归：单上游模式丢失 %v", miss)
	}
	if len(got) != 16 {
		t.Fatalf("单上游模式应有 16 条裸名，实际 %d 条: %v", len(got), got)
	}
	for _, id := range got {
		if len(id) > 10 && id[:10] == "workbuddy/" {
			t.Errorf("单上游模式不该有前缀记录: %s", id)
		}
	}
}

// ---------------------------------------------------------------------------
// 新增：codearts 必须出现在 /v1/models
// ---------------------------------------------------------------------------

// newTestRouter 造一个模拟"workbuddy + codearts"的装配层路由。
//
// codearts 那侧返回**真实模型表**（从 internal/codearts 的 KnownModels 抄来的
// 7 条，见 probe 实测输出），而不是随手编的名字 —— 这样断言失败时
// 读到的是真实差异，不是桩与现实的差异。
func newTestRouter(codeartsFails bool) testRouter {
	return testRouter{
		def: "workbuddy",
		ids: []string{"codearts", "workbuddy"},
		models: map[string][]gateway.ModelInfo{
			"codearts": {
				{ID: "GLM-5.2", ContextWindow: 196608, MaxOutputTokens: 131072},
				{ID: "glm-5.2-sft-harmony", ContextWindow: 196608, MaxOutputTokens: 131072},
				{ID: "openpangu-2.0-pro", ContextWindow: 512000, MaxOutputTokens: 131072},
				{ID: "openpangu-2.0-flash", ContextWindow: 512000, MaxOutputTokens: 131072},
				{ID: "deepseek-v4-flash-0731", ContextWindow: 1048576, MaxOutputTokens: 65536},
				{ID: "deepseek-v4-pro-0813", ContextWindow: 1048576, MaxOutputTokens: 65536},
				{ID: "glm-5.3-flash", ContextWindow: 202752, MaxOutputTokens: 131072},
			},
		},
		fail: map[string]bool{"codearts": codeartsFails},
	}
}

// codeartsExpectedIDs 是 codearts 侧应当出现在 /v1/models 里的 id（带前缀）。
//
// 与 internal/codearts 的 KnownModels() 一致 —— 那 7 条已由 cmd/t1probe
// 在真实凭证上实测确认（Models() 是静态表，不发网络请求）。
func codeartsExpectedIDs() []string {
	ms := newTestRouter(false).models["codearts"]
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, "codearts/"+m.ID)
	}
	sort.Strings(out)
	return out
}

// TestV1ModelsIncludesCodearts 是 T1 的核心验收（目录收窄后的口径）：
// /v1/models 必须**同时**含两个上游，且每个上游只列**一条** provider/model：
// 16 条 workbuddy/ + 7 条 codearts/ = 23 条，一条不多一条不少。
func TestV1ModelsIncludesCodearts(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool:            p,
		Upstream:        up,
		Provider:        newTestRouter(false),
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	got := modelIDs(t, h)

	// 1) 回归：workbuddy 16 条带前缀一条不少
	if miss := missing(got, baselineWorkbuddyExpected()); len(miss) > 0 {
		t.Fatalf("★ 回归：workbuddy 模型丢失 %v", miss)
	}
	// 2) 新增：codearts 7 条带前缀必须在
	if miss := missing(got, codeartsExpectedIDs()); len(miss) > 0 {
		t.Fatalf("codearts 模型缺失 %v\n实际: %v", miss, got)
	}
	// 3) 收窄的核心断言：裸名一条都不许在。
	// 破了会怎样：裸名与 provider/model 并存 = 每个模型在客户端列两遍
	//（用户报的"可用模型重复了，很多"）。
	if bare := present(got, baselineWorkbuddyIDs); len(bare) > 0 {
		t.Fatalf("★ 目录重复：多上游模式下列出了 %d 个裸名 %v —— 它们与 workbuddy/ 前缀版是同一个模型，会重复显示",
			len(bare), bare)
	}
	// 4) 总数 = 16 + 7
	if len(got) != 23 {
		t.Fatalf("总数=%d want 23（16 workbuddy/ + 7 codearts/）；多了说明重复列目录，少了说明有上游被静默跳过: %v",
			len(got), got)
	}
}

// TestV1ModelsOwnedByPerProvider owned_by 必须反映**实际上游**，
// 而不是全局一个值 —— 否则前端/客户端无法区分同名前缀之外的归属。
func TestV1ModelsOwnedByPerProvider(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		Provider:        newTestRouter(false),
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		owned, _ := m["owned_by"].(string)
		switch {
		case len(id) > 9 && id[:9] == "codearts/":
			if owned != "codearts" {
				t.Errorf("%s owned_by=%q want codearts", id, owned)
			}
		default:
			if owned != "workbuddy" {
				t.Errorf("%s owned_by=%q want workbuddy", id, owned)
			}
		}
	}
}

// TestV1ModelsOtherProviderFailureIsIsolated 一个上游给不出目录时**只跳过它**，
// 绝不能把整个 /v1/models 打成失败或让另一个上游的模型消失。
//
// 这条守的正是 T1 的风险面：公共端点不能被单个上游拖垮。
func TestV1ModelsOtherProviderFailureIsIsolated(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		Provider:        newTestRouter(true), // codearts 返回 ok=false
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	got := modelIDs(t, h)
	if miss := missing(got, baselineWorkbuddyExpected()); len(miss) > 0 {
		t.Fatalf("★ 一个上游失败导致默认上游模型丢失: %v", miss)
	}
	if len(got) != 16 {
		t.Fatalf("codearts 失败时应只剩 16 条 workbuddy/（收窄后每个模型一条），实际 %d: %v", len(got), got)
	}
}

// TestV1ModelsNoProviderIDsCapability 路由**不实现** IDs() 时，
// "其余上游"退化成空集，默认上游照常输出 —— 不得 panic、不得变空。
func TestV1ModelsNoProviderIDsCapability(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		Provider:        noIDsRouter{def: "workbuddy"},
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	got := modelIDs(t, h)
	if miss := missing(got, baselineWorkbuddyExpected()); len(miss) > 0 {
		t.Fatalf("无 IDs() 能力时默认上游模型丢失: %v", miss)
	}
	if len(got) != 16 {
		t.Fatalf("应只有 16 条带前缀的 workbuddy/（收窄后每个模型一条），实际 %d: %v", len(got), got)
	}
}

// noIDsRouter 刻意**不**实现 IDs()，用来钉住那条可选能力的退化路径。
type noIDsRouter struct{ def string }

var _ ProviderRouter = noIDsRouter{}

func (r noIDsRouter) Has(id string) bool { return id == r.def }
func (r noIDsRouter) Default() string    { return r.def }
func (r noIDsRouter) Models(_ context.Context, _ string) ([]gateway.ModelInfo, bool) {
	return nil, false
}
func (r noIDsRouter) ModelMultipliers(_ context.Context, _ string) (map[string]float64, bool) {
	return nil, false
}

// 出站能力同样只有声明、没有实现（见 testRouter 上方的注释）。
func (r noIDsRouter) Chat(_ context.Context, _ string, _ gateway.Credential, _ []byte) (gateway.ChatStream, bool, error) {
	return gateway.ChatStream{}, false, nil
}

func (r noIDsRouter) Credential(_, _ string) (gateway.Credential, bool) {
	return gateway.Credential{}, false
}

func (r noIDsRouter) RefreshCredential(_ context.Context, _ string, _ gateway.Credential) (bool, error) {
	return false, nil
}

// RefreshSkew 同 testRouter：`ok=false` 表示本桩没有意见，走核心兜底。
func (r noIDsRouter) RefreshSkew(_ string, _ gateway.Credential) (time.Duration, bool) {
	return 0, false
}

// ResetAt 同 testRouter：`ok=false` = 本桩没有恢复排程，走核心通用保守值。
// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
func (r noIDsRouter) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

func (r noIDsRouter) ResetAt(_ string, _ gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}

// Classify 同 testRouter：`ok=false` = 本桩没有分类器，走 upstream.Classify 回落。
func (r noIDsRouter) Classify(_ string, _ int, _ string) (gateway.ErrorKind, bool) {
	return gateway.ErrKindNone, false
}

// TestResetModelsCacheDoesNotDropOtherProviders 「刷新模型」后 codearts 仍在 ——
// 用户报的就是"点刷新扫不出来"，所以刷新后的状态必须被钉住。
func TestResetModelsCacheDoesNotDropOtherProviders(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up,
		Provider:        newTestRouter(false),
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	if got := modelIDs(t, h); len(got) != 23 {
		t.Fatalf("刷新前 %d 条 want 23（16 workbuddy/ + 7 codearts/）", len(got))
	}
	ResetModelsCache()
	got := modelIDs(t, h)
	if miss := missing(got, codeartsExpectedIDs()); len(miss) > 0 {
		t.Fatalf("★ 点「刷新模型」后 codearts 模型消失: %v", miss)
	}
	if miss := missing(got, baselineWorkbuddyExpected()); len(miss) > 0 {
		t.Fatalf("★ 点「刷新模型」后 workbuddy 模型消失: %v", miss)
	}
}

// ---------------------------------------------------------------------------
// 新增（目录收窄）：id 唯一 + 无裸名；以及 def == "" 的边界
// ---------------------------------------------------------------------------

// TestV1ModelsIDsUniqueAndPrefixed 直接钉住用户报的那个 bug：
// 目录里**同一个模型只能出现一次**，且多上游模式下**不许有裸名**。
//
// 两条断言各自破了会怎样：
//   - id 重复 → 客户端下拉框里同一个模型显示两次（用户原话"可用模型重复了，很多"）
//   - 出现不带 "/" 的 id → 裸名那一份又回来了，重复必然随之而来
func TestV1ModelsIDsUniqueAndPrefixed(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool:            p,
		Upstream:        up,
		Provider:        newTestRouter(false),
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	got := modelIDs(t, h)

	// 1) id 唯一：len(set) == len(list)
	seen := make(map[string]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	if len(seen) != len(got) {
		dups := make([]string, 0, len(got)-len(seen))
		for id, n := range seen {
			if n > 1 {
				dups = append(dups, fmt.Sprintf("%s×%d", id, n))
			}
		}
		sort.Strings(dups)
		t.Fatalf("★ 目录里有重复 id %v（len(set)=%d < len(list)=%d）—— 破了客户端下拉框会把同一个模型列两遍: %v",
			dups, len(seen), len(got), got)
	}

	// 2) 一条裸名都不许有（多上游模式）
	var bare []string
	for _, id := range got {
		if !strings.Contains(id, "/") {
			bare = append(bare, id)
		}
	}
	if len(bare) > 0 {
		t.Fatalf("★ 多上游模式出现了 %d 个裸名 %v —— 裸名与 provider/model 并存就是重复列目录", len(bare), bare)
	}

	// 3) 前缀必须是**已注册**的上游：否则客户端选了会在选路时吃 400。
	known := map[string]bool{"workbuddy": true, "codearts": true}
	for _, id := range got {
		pid, _, has := gateway.SplitModel(id)
		if !has || !known[pid] {
			t.Errorf("id=%q 的前缀 %q 不是已注册上游（(provider,model) 对不上，选了必 400）", id, pid)
		}
	}
}

// TestV1ModelsEmptyDefaultProviderKeepsBareNames 钉住 def == "" 的边界：
// 多上游模式但默认上游为空时**拼不出** provider/model 前缀 ——
// 硬拼只会得到 "/glm-5.2" 这种没有任何客户端能解析的畸形 id。
//
// 所以此时必须保持裸名（每个模型仍然只有一条），且**零**条以 "/" 开头的 id。
func TestV1ModelsEmptyDefaultProviderKeepsBareNames(t *testing.T) {
	resetDynamicCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	router := newTestRouter(false)
	router.def = "" // 装配层给不出默认上游身份
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		Provider: router,
		// DefaultProvider 显式留空：defaultProvider() 会去问路由，拿到 ""。
		OwnedBy: "workbuddy",
	})

	got := modelIDs(t, h)

	// 1) 默认上游的模型以裸名出现（没有上游身份可拼前缀）。
	if miss := missing(got, baselineWorkbuddyIDs); len(miss) > 0 {
		t.Fatalf("★ def == 空 时模型丢失 %v —— 把空上游名拼进 id 会让目录整个不可用", miss)
	}
	// 2) 其它上游有身份，仍照常带自己的前缀。
	if miss := missing(got, codeartsExpectedIDs()); len(miss) > 0 {
		t.Fatalf("codearts 模型缺失 %v —— 默认上游为空不该拖垮别的上游", miss)
	}
	// 3) 畸形 id：以 "/" 开头 = 拿空上游名拼出来的。
	var malformed []string
	for _, id := range got {
		if len(id) > 0 && id[0] == '/' {
			malformed = append(malformed, id)
		}
	}
	if len(malformed) > 0 {
		t.Fatalf("★ 出现 %d 条以 / 开头的畸形 id %v —— 前缀为空，任何客户端都解析不了", len(malformed), malformed)
	}
	// 4) 总数 = 16 裸名 + 7 codearts/（每个模型仍然只有一条，没有重复）
	if len(got) != 23 {
		t.Fatalf("总数=%d want 23（16 裸名 + 7 codearts/）: %v", len(got), got)
	}
}

