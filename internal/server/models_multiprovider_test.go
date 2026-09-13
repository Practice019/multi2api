// models_multiprovider_test.go T1 的回归 + 新增断言：/v1/models 必须**同时**给出
// 两个上游的模型，且默认上游（workbuddy）原有的那 32 条**一条不少**。
//
// # 为什么这个文件先于实现存在（硬要求）
//
// `/v1/models` 是**公共端点**：所有客户端都调它。改错会让**所有上游**的模型消失，
// 而不只是新接的那个。所以顺序必须是：
//
//  1. 先让"workbuddy 32 条一条不少"在**改动前**是绿的（本文件的 baseline 用例）
//  2. 再动实现
//  3. 改完再跑：32 条仍在 + codearts 的新增
//
// 跳过第 1 步就无法区分"我改对了"与"本来就坏"。
//
// # 基线值从哪来
//
// `.task/ui-convergence/shared/baselines/models-before-t1.txt`：在真实实例 18080
// 上实测得到的 32 条 id（16 个裸名 + 16 个 workbuddy/ 前缀）。
// 裸名与代码里的 staticModels 不同（静态表只有 10 条且含 hy3-preview 等），
// 因为真实实例走的是**动态列表**路径（fetchDynamicModels 命中上游）。
// 本文件用桩上游复刻那条动态路径，因此基线可与真实实例逐字对齐。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// baselineWorkbuddyIDs 是改动前的实测基线（32 条，见源文件）。
//
// 它按**裸名 id** 给出；带 workbuddy/ 前缀的那 16 条是前缀投影，
// 由下面的期望函数生成，避免手抄 32 行时漏一条。
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

// baselineWorkbuddyExpected 把裸名展开成真实实例上的 32 条（裸名 + 前缀版）。
func baselineWorkbuddyExpected() []string {
	out := make([]string, 0, len(baselineWorkbuddyIDs)*2)
	out = append(out, baselineWorkbuddyIDs...)
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

// ---------------------------------------------------------------------------
// 回归基线：改动前就应当是绿的
// ---------------------------------------------------------------------------

// TestV1ModelsBaselineWorkbuddy32 锁定改动前的 32 条基线。
//
// 这是 T1 的**回归闸门**：无论实现怎么改，这 32 条必须一条不少。
// 装配层给了 Provider（多上游模式），所以裸名 + workbuddy/ 前缀两份都要在。
func TestV1ModelsBaselineWorkbuddy32(t *testing.T) {
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
	want := baselineWorkbuddyExpected()

	if len(want) != 32 {
		t.Fatalf("基线自检失败：期望表有 %d 条，应为 32", len(want))
	}
	if miss := missing(got, want); len(miss) > 0 {
		t.Fatalf("★ 回归：workbuddy 原有模型丢失 %d 条: %v\n实际共 %d 条: %v",
			len(miss), miss, len(got), got)
	}
	if len(got) != 32 {
		t.Logf("当前 %d 条（基线 32）: %v", len(got), got)
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

// TestV1ModelsIncludesCodearts 是 T1 的核心验收：
// /v1/models 必须**同时**含两个上游，且 workbuddy 那 32 条一条不少。
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

	// 1) 回归：workbuddy 32 条一条不少
	if miss := missing(got, baselineWorkbuddyExpected()); len(miss) > 0 {
		t.Fatalf("★ 回归：workbuddy 模型丢失 %v", miss)
	}
	// 2) 新增：codearts 7 条带前缀必须在
	if miss := missing(got, codeartsExpectedIDs()); len(miss) > 0 {
		t.Fatalf("codearts 模型缺失 %v\n实际: %v", miss, got)
	}
	// 3) 总数 = 32 + 7
	if len(got) != 39 {
		t.Fatalf("总数=%d want 39（32 workbuddy + 7 codearts）: %v", len(got), got)
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
	if len(got) != 32 {
		t.Fatalf("codearts 失败时应只剩 32 条 workbuddy，实际 %d: %v", len(got), got)
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
	if len(got) != 32 {
		t.Fatalf("应只有 32 条，实际 %d", len(got))
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

	if got := modelIDs(t, h); len(got) != 39 {
		t.Fatalf("刷新前 %d 条 want 39", len(got))
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
