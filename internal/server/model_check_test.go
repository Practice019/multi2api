package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// spyRouter 假路由：记录被问过哪些前缀、以及**有没有真的发过对话请求**。
//
// 用"Chat 调用次数"当断言是本次的关键：用户报的 bug 不是"报了错"，
// 而是"报错之前白烧了 6 次上游调用"（6 个账号各挨一次 400）。
// ⚠ 实现方式：**嵌入**既有的 testRouter（models_multiprovider_test.go），只覆盖我需要的两处。
//
// 我第一版手写了 ProviderRouter 的全部方法，结果被接口逐个打回（Classify → Credential →
// RefreshSkew …）—— 每加一次都要重编译一轮。嵌入既有替身之后，接口以后再长方法也不用改这里。
type spyRouter struct {
	testRouter
	known    map[string]bool
	chatCall int
}

func newSpyRouter(ids ...string) *spyRouter {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return &spyRouter{testRouter: newTestRouter(false), known: m}
}

// Has 只认我显式给的前缀集合（这样"未知前缀"用例才有确定答案）。
func (s *spyRouter) Has(id string) bool { return s.known[id] }

// Default 固定 workbuddy：裸名会走到它。
func (s *spyRouter) Default() string { return "workbuddy" }

// Chat 是本组用例的关键计数器：守卫若失效，这里就会被调用。
func (s *spyRouter) Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	s.chatCall++
	return gateway.ChatStream{}, true, nil
}

// guardChatReq 造一次最小可用的对话请求。
func guardChatReq(t *testing.T, model string) *http.Request {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// guardHandler 装一个最小 Handler：有 Pool/Upstream/Provider，但没有渠道。
func guardHandler(t *testing.T, sp *spyRouter) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up, Provider: sp,
		DefaultProvider: "workbuddy", OwnedBy: "workbuddy", MaxBodyMB: 1,
	})
	return h
}

// errorCodeOf 取响应体里的 error.code（不是这个形状时返回 ""）。
func errorCodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var obj struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	return obj.Error.Code
}

// TestUnknownModelRejectedBeforeAnyUpstreamCall 是用户那个 bug 的回归钉。
//
// 判据三条：① 400（不是池子的 503）；② code=unknown_model；③ **Provider.Chat 一次都没被调用**。
func TestUnknownModelRejectedBeforeAnyUpstreamCall(t *testing.T) {
	for _, model := range []string{"definitely-not-a-model-xyz", "fake-refusal-test"} {
		resetDynamicCache()
		sp := newSpyRouter("workbuddy", "codearts")
		h := guardHandler(t, sp)
		// ⚠ 先把默认上游目录**灌热**，这条断言才成立。
		//
		// 守卫在"目录不可知"（冷缓存 / 拉取失败负缓存期）时是**放行**的（fail-open，
		// 见 model_check.go 里的取舍说明）—— 不灌热的话，裸名会一路走到池子，
		// 断言拿到的就是那句误导性的 503，测出来的不是守卫行为。
		// 真实部署里目录是热的（实测 /v1/models 635 条），这才是用户遇到的那条路径。
		if got := h.fetchDynamicModels(); len(got) == 0 {
			t.Fatal("前置条件不成立：默认上游目录为空，这条断言无从谈起")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, guardChatReq(t, model))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("model=%q 应回 400，实际 %d（body=%s）", model, rec.Code, rec.Body.String())
		}
		if code := errorCodeOf(t, rec.Body.Bytes()); code != "unknown_model" {
			t.Errorf("model=%q 的 error.code 应为 unknown_model，实际 %q", model, code)
		}
		if sp.chatCall != 0 {
			t.Errorf("model=%q 不该发任何上游请求，实际发了 %d 次（这正是被修掉的「白烧配额」）", model, sp.chatCall)
		}
	}
}

// TestUnknownPrefixRejected 未知前缀 → 400 unknown_provider（沿用既有 code）。
func TestUnknownPrefixRejected(t *testing.T) {
	resetDynamicCache()
	sp := newSpyRouter("workbuddy")
	h := guardHandler(t, sp)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, guardChatReq(t, "nosuchprovider/x"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应回 400，实际 %d", rec.Code)
	}
	if code := errorCodeOf(t, rec.Body.Bytes()); code != "unknown_provider" {
		t.Errorf("error.code = %q，want unknown_provider", code)
	}
	if sp.chatCall != 0 {
		t.Errorf("不该发上游请求，实际 %d 次", sp.chatCall)
	}
}

// TestKnownPrefixPassesGuard 合法前缀不得被拦（守卫不能把正常请求打死）。
func TestKnownPrefixPassesGuard(t *testing.T) {
	resetDynamicCache()
	sp := newSpyRouter("workbuddy")
	h := guardHandler(t, sp)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, guardChatReq(t, "workbuddy/auto"))
	if rec.Code == http.StatusBadRequest && errorCodeOf(t, rec.Body.Bytes()) == "unknown_model" {
		t.Fatalf("合法前缀被守卫拦下了: %s", rec.Body.String())
	}
}

// TestBareNameInDefaultCatalogPassesGuard 裸名只要在默认上游目录里就放行（legacy 路径）。
//
// 这条防的是"守卫写太狠"：裸名 `auto` 是用户实际在用的写法（请求日志里就有），
// 目录里有它 ⇒ 必须放行。
func TestBareNameInDefaultCatalogPassesGuard(t *testing.T) {
	resetDynamicCache()
	// 先把动态目录灌进缓存（新请求会读它判裸名）
	seedHandler := guardHandler(t, newSpyRouter("workbuddy"))
	if got := seedHandler.fetchDynamicModels(); len(got) == 0 {
		t.Fatal("前置条件不成立：动态目录为空，这条断言就没意义了")
	}
	var bare string
	for _, m := range seedHandler.fetchDynamicModels() {
		if m.ID != "" && !strings.Contains(m.ID, "/") {
			bare = m.ID
			break
		}
	}
	if bare == "" {
		t.Skip("动态目录里没有裸名，跳过（这不影响守卫本身的判定）")
	}
	rec := httptest.NewRecorder()
	seedHandler.modelNameUsable(rec, guardChatReq(t, bare), bare)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("目录里的裸名 %q 被拦了，body=%s", bare, rec.Body.String())
	}
}

// TestCatalogIDsAllHavePrefix 回归钉：/v1/models 的名字**全部**带前缀。
//
// 用户的原话是「我的模型目录里面只能显示出有上游前缀的模型」。
func TestCatalogIDsAllHavePrefix(t *testing.T) {
	resetDynamicCache()
	h := guardHandler(t, newSpyRouter("workbuddy", "codearts"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("目录不是合法 JSON: %v", err)
	}
	if len(out.Data) == 0 {
		t.Fatal("目录为空，断言就没有意义")
	}
	var bare []string
	for _, m := range out.Data {
		if !strings.Contains(m.ID, "/") {
			bare = append(bare, m.ID)
		}
	}
	if len(bare) > 0 {
		t.Errorf("目录里出现了没有前缀的模型（用户报的正是这个）: %v", bare)
	}
}
