// apikey_groups_test.go P2：分组成为「这把 key 能用哪些模型」的主判据。
//
// # 要守住的三条
//
//	① 分组优先：key 有分组时，白名单**不参与**判定（PLAN Q3 采纳方案 B）
//	② 存量兼容：没分组的 key 仍旧走白名单，一条都不能坏
//	③ 受限且为空 ⇒ 拒绝，**不是**"不限制"
//
// 第 ③ 条是本文件最要紧的一条。空列表在**白名单**语义里是"没设限"，
// 但在**分组**语义里还有第二种来源：这把 key 指着的分组里一个模型都没有
// （分组被清空 / 全被删 / 悬空）。照直沿用"空 = 不限制"就是把权限**放大**，
// 而这种放大在界面上完全看不出来。
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// groupFixture 造一个带分组解析的 Handler。
//
// 用固定表而不是真实 groupstore：本文件测的是**判定链**，
// 分组里有什么是 groupstore 的事（那边有自己的测试）。
func groupFixture(t *testing.T, groups map[string][]string) *Handler {
	t.Helper()
	keys := newKeyStore(t)
	keys.SetGroupModels(func(id string) []string { return groups[id] })
	return &Handler{cfg: Config{APIKeys: keys}}
}

// ① 分组优先：分组里的模型放行、分组外的一律拒绝（即使白名单里有它）。
func TestGroupTakesPrecedenceOverWhitelist(t *testing.T) {
	keys := newKeyStore(t)
	keys.SetGroupModels(func(id string) []string {
		if id == "g-vip" {
			return []string{"vip-model"}
		}
		return nil
	})
	h := &Handler{cfg: Config{APIKeys: keys}}
	k, err := keys.Create("app", 0, 0, []string{"legacy-model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-vip"}); err != nil {
		t.Fatal(err)
	}

	r := reqAs(t, k.ID)
	w := httptest.NewRecorder()
	if !h.modelAllowedForRequest(w, r, "vip-model") {
		t.Error("分组里的模型必须放行")
	}

	// 白名单里有 legacy-model，但分组优先 ⇒ 必须拒绝
	w2 := httptest.NewRecorder()
	if h.modelAllowedForRequest(w2, r, "legacy-model") {
		t.Error("★ 分组非空时白名单不参与判定（方案 B）—— " +
			"放行它就是两处配置同时生效，而那正是 Q3 否决方案 A 的理由")
	}
	if w2.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d，want 403", w2.Code)
	}
}

// ② 存量兼容：没配分组的 key 仍旧走白名单（升级后行为一个字节都不变）。
func TestLegacyWhitelistStillWorksWithoutGroups(t *testing.T) {
	h := groupFixture(t, map[string][]string{"g-vip": {"vip-model"}})
	k, err := h.cfg.APIKeys.Create("legacy", 0, 0, []string{"legacy-model"})
	if err != nil {
		t.Fatal(err)
	}
	r := reqAs(t, k.ID)

	w := httptest.NewRecorder()
	if !h.modelAllowedForRequest(w, r, "legacy-model") {
		t.Error("没有分组的 key 必须继续走白名单 —— 否则升级把存量 key 全打死")
	}
	w2 := httptest.NewRecorder()
	if h.modelAllowedForRequest(w2, r, "vip-model") {
		t.Error("白名单外的模型仍要拒绝")
	}
}

// ③ 受限且为空 ⇒ 拒绝。这是本文件的核心断言。
func TestRestrictedEmptyGroupDeniesEverything(t *testing.T) {
	// 分组存在但一个模型都没有
	h := groupFixture(t, map[string][]string{"g-empty": {}})
	k, _ := h.cfg.APIKeys.Create("app", 0, 0, nil)
	if err := h.cfg.APIKeys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-empty"}); err != nil {
		t.Fatal(err)
	}

	allowed, restricted := h.allowedModelsForRequest(reqAs(t, k.ID))
	if !restricted {
		t.Fatal("配了分组就是受限（restricted=true），哪怕并集为空")
	}
	if len(allowed) != 0 {
		t.Fatalf("并集应为空，实际 %v", allowed)
	}

	w := httptest.NewRecorder()
	if h.modelAllowedForRequest(w, reqAs(t, k.ID), "任何模型") {
		t.Error("★ 受限且为空必须拒绝 —— 空并集不是「不限制」，" +
			"照直放行等于把权限放大成全部可用，而界面上看不出任何异常")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d，want 403", w.Code)
	}

	// 目录也必须给空，不能把完整列表发出去（"看得见调不了"是最难解释的形态）
	list := []map[string]any{{"id": "anything"}, {"id": "other"}}
	if got := h.filterModelsForRequest(reqAs(t, k.ID), list); len(got) != 0 {
		t.Errorf("受限且为空时目录必须为空，实际 %v", got)
	}
}

// 悬空分组（key 指向已被删的分组）同样落到"受限且为空"⇒ 拒绝。
//
// 这与 P1 groupstore.Delete 不级联是配套的：删分组之后 key 里会暂时悬空，
// 直到 P5 的引用完整性清理。悬空期间的语义必须是"拒绝"而不是"放开"。
func TestDanglingGroupDeniesInsteadOfOpeningUp(t *testing.T) {
	h := groupFixture(t, map[string][]string{}) // 表是空的 = 所有分组都悬空
	k, _ := h.cfg.APIKeys.Create("app", 0, 0, nil)
	if err := h.cfg.APIKeys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-deleted"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if h.modelAllowedForRequest(w, reqAs(t, k.ID), "任何模型") {
		t.Error("★ 悬空分组必须拒绝。放行等于「删一个分组把权限全部放开」——" +
			"而用户完全看不出自己的 key 被扩大了权限")
	}
}

// 未接线（装配层忘了注入解析函数）⇒ 降级到白名单，**不是**拒绝。
//
// 这两种失败模式的严重性不同：拒绝 = 用户调用全 403 而错误信息只说
// "模型不允许"（排查跑偏）；降级 = 权限按存量口径走（可能偏宽，但可解释）。
// 选后者是因为"忘接线"是运维事故，不该表现为"用户的 key 突然不能用了"。
func TestUnwiredResolverFallsBackToWhitelist(t *testing.T) {
	keys := newKeyStore(t) // 不 SetGroupModels
	h := &Handler{cfg: Config{APIKeys: keys}}
	k, _ := keys.Create("app", 0, 0, []string{"legacy-model"})
	if err := keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-vip"}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	if !h.modelAllowedForRequest(w, reqAs(t, k.ID), "legacy-model") {
		t.Error("未接线时应降级到白名单（存量行为），不该拒绝")
	}
}

// 多分组：并集生效（一个 key 能用多个分组的模型）。
func TestMultipleGroupsUnion(t *testing.T) {
	h := groupFixture(t, map[string][]string{
		"g-a": {"m-a", "m-shared"},
		"g-b": {"m-b", "m-shared"},
	})
	k, _ := h.cfg.APIKeys.Create("app", 0, 0, nil)
	if err := h.cfg.APIKeys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-a", "g-b"}); err != nil {
		t.Fatal(err)
	}
	r := reqAs(t, k.ID)
	for _, m := range []string{"m-a", "m-b", "m-shared"} {
		w := httptest.NewRecorder()
		if !h.modelAllowedForRequest(w, r, m) {
			t.Errorf("多分组的并集应放行 %q", m)
		}
	}
	w := httptest.NewRecorder()
	if h.modelAllowedForRequest(w, r, "m-outside") {
		t.Error("不在任何分组里的模型必须拒绝")
	}

	// 目录同理：只剩并集里的三条
	list := []map[string]any{
		{"id": "m-a"}, {"id": "m-b"}, {"id": "m-outside"},
	}
	if got := h.filterModelsForRequest(r, list); len(got) != 2 {
		t.Errorf("目录应剩 2 条（m-a / m-b），实际 %v", got)
	}
}
