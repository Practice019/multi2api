// groups_prune_plaintext_test.go 权限放大反馈**不得披露明文凭据**，且必须禁缓存。
//
// # 为什么要单独一个文件，而不是并进 groups_prune_test.go
//
// 因为这条缺陷的形态是**"只改了一条路径"**：
//
//	/admin/groups/delete   早就掩码了（groups.go 的 masked）
//	/admin/groups/prune    直接把 res.KeysUnrestricted 原文塞进响应和 warning
//
// 两者走的是**同一个** apikey.PruneResult.KeysUnrestricted，而那个字段里装的是
// apikey.ID —— 它就是**明文密钥本身**。所以判据必须同时跑两条路径：
// 只断言其中一条，另一条漏了照样全绿。
//
// # 顺带钉住 no-store
//
// 掩码不等于可以缓存：掩码带可辨认前缀（"我现在用的是哪一把"），
// 让它留在浏览器 bfcache / 中间代理里没有任何好处。
// 与 tokens.go 的明文端点用**同一个**常量 credentialCacheControl。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pruneCallRaw 发一次管理端请求并返回完整 recorder（要读响应头，不能只拿 body）。
func pruneCallRaw(t *testing.T, f *pruneFixture, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, withTestAuth(req))
	return rec
}

// assertNoPlaintext 断言响应体与 warning 文案里都没有那把 key 的原文。
func assertNoPlaintext(t *testing.T, label, rawBody, warning, keyID string) {
	t.Helper()
	if strings.Contains(rawBody, keyID) {
		t.Fatalf("%s：响应体里出现了完整凭据 %q —— 权限放大的反馈只许回掩码\nbody=%s",
			label, keyID, rawBody)
	}
	if warning != "" && strings.Contains(warning, keyID) {
		t.Fatalf("%s：warning 文案里出现了完整凭据 %q —— 那句话是要被贴进工单/聊天窗口的\nwarning=%s",
			label, keyID, warning)
	}
}

// TestGroupPruneNeverLeaksPlaintextKey ★ 一键清理不得披露明文凭据。
//
// 这条是本次修的缺陷本身：改造前 `/admin/groups/prune` 把 KeysUnrestricted
// 原样放进 `keys_unrestricted` 与 `warning`。
func TestGroupPruneNeverLeaksPlaintextKey(t *testing.T) {
	f := newPruneFixture(t)
	// ⚠ key 必须**只**绑这一个分组：KeysUnrestricted 的定义是
	// "清理之后这把 key 的分组列表变成空" —— 若还绑着别的分组就不算不限制，
	// 响应里不会出现 keys_unrestricted，判据会误以为"没泄漏"。
	// （第一版就是这么写错的：绑了 gone+keep，清完 still restricted。）
	gone, err := f.groups.Create("gone", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := f.keys.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{gone.ID}); err != nil {
		t.Fatal(err)
	}
	// 造一个悬空引用：分组表里删掉 gone，但 key 还指着它。
	if err := f.groups.Delete(gone.ID); err != nil {
		t.Fatal(err)
	}

	rec := pruneCallRaw(t, f, http.MethodPost, "/admin/groups/prune", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("prune 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	warning, _ := out["warning"].(string)
	assertNoPlaintext(t, "/admin/groups/prune", body, warning, k.ID)

	// 掩码必须真的在（否则"没泄漏"可能只是"什么都没报"）。
	list, ok := out["keys_unrestricted"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("★ 必须报告变空的 key（否则用户在不知情的情况下放开了权限），实际 %v", out)
	}
	masked, _ := list[0].(string)
	// ⚠ 掩码形态取决于**用哪个掩码函数**，不要凭印象断言：
	//   tokend 的 store.MaskTokenKey 用 `*`（实测 "sk-e**********028a"）
	//   channelstore 的渠道 key 掩码用省略号 U+2026
	// 本端点走的是前者。所以判据只断言"它不等于原文、比原文短、带掩码字符"。
	if masked == k.ID {
		t.Fatalf("keys_unrestricted 回的是原文而不是掩码：%q", masked)
	}
	if len(masked) >= len(k.ID) {
		t.Fatalf("掩码应当比原文短：%q（len %d）vs 原文 len %d", masked, len(masked), len(k.ID))
	}
	if !strings.ContainsAny(masked, "*…") {
		t.Fatalf("掩码里应有屏蔽字符（* 或 …），实际 %q", masked)
	}

	// 禁缓存。
	if got := rec.Header().Get("Cache-Control"); got != credentialCacheControl {
		t.Fatalf("prune 响应必须带 no-store（%q），实际 %q ——\n"+
			"掩码带可辨认前缀，留在 bfcache/代理里没有好处", credentialCacheControl, got)
	}
}

// TestGroupDeleteNeverLeaksPlaintextKey 同样的断言跑在**另一条**路径上。
//
// ⚠ 这条不是重复：两组端点是分别实现的，历史上只有这一条掩了码、prune 那条没掩。
// 判据必须两条都跑，否则"改了一条"的形态会一直是绿的。
func TestGroupDeleteNeverLeaksPlaintextKey(t *testing.T) {
	f := newPruneFixture(t)
	gone, err := f.groups.Create("gone", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := f.keys.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{gone.ID}); err != nil {
		t.Fatal(err)
	}

	rec := pruneCallRaw(t, f, http.MethodPost, "/admin/groups/delete", `{"id":"`+gone.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	warning, _ := out["warning"].(string)
	assertNoPlaintext(t, "/admin/groups/delete", body, warning, k.ID)
}

// TestGroupsConsistencyReportNeverLeaksPlaintext 一致性报告同样只回掩码。
//
// 报告的用途就是"贴到工单里给人看"，明文出现在这里等于主动外带。
func TestGroupsConsistencyReportNeverLeaksPlaintext(t *testing.T) {
	f := newPruneFixture(t)
	gone, err := f.groups.Create("gone", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := f.keys.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{gone.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.groups.Delete(gone.ID); err != nil {
		t.Fatal(err)
	}

	rec := pruneCallRaw(t, f, http.MethodGet, "/admin/groups/consistency", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("consistency 应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	assertNoPlaintext(t, "/admin/groups/consistency", rec.Body.String(), "", k.ID)
}
