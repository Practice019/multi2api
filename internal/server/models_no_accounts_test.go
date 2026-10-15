// models_no_accounts_test.go 「没有可用账号 ⇒ 目录为空」的守卫。
//
// # 用户报障（本次修的第四条）
//
// 原话：*「我一个账号没有，应该所有的[上游]都无法扫描到模型才对」*。
//
// 改造前 `singleProviderModelList` 无条件回落静态表，于是两种完全不同的情形
// 共用一个出口：
//
//	没账号   → fetchDynamicModels 返回 nil → 回落静态表 → 报「10 个模型」
//	拉取失败 → fetchDynamicModels 返回 nil → 回落静态表 → 报「10 个模型」
//
// 前者的真相是**一个模型都调不了**，界面上却写着"有 10 个模型"——
// 用户照着那个列表去调，全部失败，而没有任何地方说明原因。
//
// # 判据为什么用「池子空」而不是「上游报错」
//
// 因为要守的是"账号维度的前提"：没有号就没有目录。至于"有号但上游 502"
// 那种情形，静态兜底是**保留**的（客户端兼容性），不在本组范围内。
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// TestNoAccountsMeansNoModels 空池 ⇒ `/v1/models` 必须**一个模型都不给**。
//
// 这条直接对应报障：账号为 0 时列出模型等于撒谎。
func TestNoAccountsMeansNoModels(t *testing.T) {
	resetDynamicCache()

	// 空池：一个账号都没有。
	empty := pool.New("")
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		// 即使上游**能**返回一份目录，也不该被用上 —— 没有账号去拿它。
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool:            empty,
		Upstream:        up,
		Provider:        testRouter{def: "workbuddy", ids: []string{"workbuddy"}},
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := modelIDs(t, h); len(got) != 0 {
		t.Errorf("★ 池里一个账号都没有，/v1/models 却回了 %d 条模型: %v\n"+
			"\n"+
			"没有账号 ⇒ 动态目录拿不到（要凭证），静态表也与真实可用性无关。\n"+
			"列出它们只会让调用方以为"+
			"「这些模型能用」，而他一个都调不了 —— 这正是用户报的"+
			"「我一个账号没有，应该所有的都无法扫描到模型才对」。",
			len(got), got)
	}
}

// ⚠ 反向：**有**账号时静态兜底仍然生效（不能把兜底一起修没了）。
//
// 这条守的是相反方向：如果只加"空池⇒空"而误把兜底删掉，那么
// "有账号但上游临时失败"的部署会在界面上突然一个模型都没有 ——
// 那是另一个方向的回归，而且更难查（账号是好的，只是目录拉不到）。
func TestAccountsPresentStillFallsBackToStatic(t *testing.T) {
	resetDynamicCache()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, baselineDynamicBody(), false
	})
	h := NewHandler(Config{
		Pool:            p,
		Upstream:        up,
		Provider:        testRouter{def: "workbuddy", ids: []string{"workbuddy"}},
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})

	if got := modelIDs(t, h); len(got) == 0 {
		t.Error("★ 有可用账号时不该返回空目录 ——\n" +
			"静态兜底是为「账号可用、但目录临时拉不到」保留的兼容路径。" +
			"把它一起修没了会让那种部署的界面突然空掉，而账号其实是好的。")
	}
}
