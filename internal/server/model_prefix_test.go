package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// strictGuardHandler 与 guardHandler 相同，只多一个严格前缀策略（可传 nil = 不接线）。
func strictGuardHandler(t *testing.T, sp *spyRouter, policy *ModelPrefixPolicy) *Handler {
	t.Helper()
	h := guardHandler(t, sp)
	h.cfg.ModelPrefix = policy
	return h
}

// warmCatalog 把默认上游目录灌热（裸名的判定依赖它，见 model_check.go 的 fail-open 取舍）。
func warmCatalog(t *testing.T, h *Handler) {
	t.Helper()
	if got := h.fetchDynamicModels(); len(got) == 0 {
		t.Fatal("前置条件不成立：默认上游目录为空")
	}
}

// TestStrictPrefixRejectsBareNameInCatalog 开关开 ⇒ 目录里的裸名也要 400（这正是这个开关的意义）。
//
// ⚠ 判据三条与 model_check_test.go 里那条一致：400 / code=unknown_model / **Provider.Chat 一次都没调**。
//
//	第一条断言"开关开时能拦住"，第三条断言"拦住发生在试号之前"（否则白烧配额的老问题会回来）。
func TestStrictPrefixRejectsBareNameInCatalog(t *testing.T) {
	resetDynamicCache()
	sp := newSpyRouter("workbuddy")
	h := strictGuardHandler(t, sp, NewModelPrefixPolicy(true))
	warmCatalog(t, h)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, guardChatReq(t, "auto")) // auto 在目录里 ⇒ 非严格模式会放行

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("严格模式下裸名应回 400，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec.Body.Bytes()); code != "unknown_model" {
		t.Errorf("error.code = %q，want unknown_model", code)
	}
	if sp.chatCall != 0 {
		t.Errorf("不该发任何上游请求，实际 %d 次", sp.chatCall)
	}
}

// TestStrictPrefixOffKeepsBareNameWorking 开关关（默认）⇒ 现状不变：目录里的裸名照旧放行。
//
// 这条是"不能把用户的线上调用打死"的保险：默认必须与改造前逐字一致。
func TestStrictPrefixOffKeepsBareNameWorking(t *testing.T) {
	resetDynamicCache()
	sp := newSpyRouter("workbuddy")
	h := strictGuardHandler(t, sp, NewModelPrefixPolicy(false))
	warmCatalog(t, h)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, guardChatReq(t, "auto"))

	if rec.Code == http.StatusBadRequest && errorCodeOf(t, rec.Body.Bytes()) == "unknown_model" {
		t.Fatalf("默认（关）不该拦住目录里的裸名：%s", rec.Body.String())
	}
}

// TestStrictPrefixKeepsPrefixedNames 开关开 ⇒ 带前缀的合法名不受影响。
func TestStrictPrefixKeepsPrefixedNames(t *testing.T) {
	resetDynamicCache()
	sp := newSpyRouter("workbuddy")
	h := strictGuardHandler(t, sp, NewModelPrefixPolicy(true))
	warmCatalog(t, h)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, guardChatReq(t, "workbuddy/auto"))

	if rec.Code == http.StatusBadRequest && errorCodeOf(t, rec.Body.Bytes()) == "unknown_model" {
		t.Fatalf("带前缀的合法名被严格模式拦下了：%s", rec.Body.String())
	}
}

// TestStrictPrefixHotToggle 热生效：同一个 Handler 上开关一变，下一次请求行为就变。
//
// 这条钉住的是**接线纪律**：请求路径必须每次去读共享对象（ModelPrefixPolicy），
// 而不是读启动时复制进 Config 的 bool —— 后者会让设置页的开关变成假的。
// （本仓在这一族上踩过两次：破甲模式、MonkeyCode 出口。）
func TestStrictPrefixHotToggle(t *testing.T) {
	resetDynamicCache()
	sp := newSpyRouter("workbuddy")
	policy := NewModelPrefixPolicy(false)
	h := strictGuardHandler(t, sp, policy)
	warmCatalog(t, h)

	// 关着：放行
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, guardChatReq(t, "auto"))
	if rec.Code == http.StatusBadRequest && errorCodeOf(t, rec.Body.Bytes()) == "unknown_model" {
		t.Fatalf("开关关着时不该拦：%s", rec.Body.String())
	}

	// 热打开：同一个 Handler，不重建、不重启
	policy.SetStrict(true)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, guardChatReq(t, "auto"))
	if rec2.Code != http.StatusBadRequest || errorCodeOf(t, rec2.Body.Bytes()) != "unknown_model" {
		t.Fatalf("热打开后应立刻 400 unknown_model，实际 %d / %s", rec2.Code, rec2.Body.String())
	}

	// 再热关掉：恢复放行
	policy.SetStrict(false)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, guardChatReq(t, "auto"))
	if rec3.Code == http.StatusBadRequest && errorCodeOf(t, rec3.Body.Bytes()) == "unknown_model" {
		t.Fatalf("热关掉后应恢复放行：%s", rec3.Body.String())
	}
}

// TestStrictPrefixKeepsConfiguredAlias 配置过的别名仍可用（用户要求的那条例外）。
//
// 机制上它靠的是**别名解析发生在守卫之前**（handler.go 里那段 aliasCandidates）：
// 对外名到这里已经变成带前缀的内部名，严格模式天然放行。
// 这条用真实的 key/group 存储把整条链路跑一遍，而不是只断言"带前缀能过"。
func TestStrictPrefixKeepsConfiguredAlias(t *testing.T) {
	resetDynamicCache()
	keys := newKeyStore(t)
	gs := newGroupStore(t)
	// 与 aliasFixtureMulti 同形：对外名 006/deepseek-v4.1-flash → 内部名 workbuddy/deepseek-v4.1-flash
	mm := map[string][]string{"006/deepseek-v4.1-flash": {"workbuddy/deepseek-v4.1-flash"}}
	g, err := gs.Create("别名组", "", []string{"workbuddy/deepseek-v4.1-flash"}, mm)
	if err != nil {
		t.Fatalf("建分组: %v", err)
	}
	k, err := keys.CreateOwnedWithGroups("", "666", 0, 0, nil, []string{g.ID})
	if err != nil {
		t.Fatalf("建 key: %v", err)
	}

	sp := newSpyRouter("workbuddy")
	h := strictGuardHandler(t, sp, NewModelPrefixPolicy(true))
	warmCatalog(t, h)
	h.cfg.APIKeys = keys
	h.cfg.Groups = gs

	base := guardChatReq(t, "006/deepseek-v4.1-flash")
	req := base.WithContext(context.WithValue(base.Context(), apikeyCtxKey, k.ID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if errorCodeOf(t, rec.Body.Bytes()) == "unknown_model" {
		t.Fatalf("配置过的别名被严格模式当成未知模型拦下了：%s", rec.Body.String())
	}
}
