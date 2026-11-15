// catalog_impact_handler_test.go 删除影响端点的判据（U-11「删除影响明确」）。
//
// # 端点层的三条
//
//	① 只读：不写任何东西，且禁缓存
//	② 管理员限定
//	③ 缺参数时要**明确报错**，不能回一个"零影响"的空结论
//	   —— 那会让调用方以为"删了没事"
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/store"
)

// impactFixture 两条渠道：ch-a 唯一供给 m1，ch-a+ch-b 共同供给 m2。
func impactFixture(t *testing.T) *Handler {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range []*channelstore.Channel{
		{ID: "ch-a", Name: "渠道A", Kind: "standard", Status: 1,
			Models: []string{"m1", "m2"}},
		{ID: "ch-b", Name: "渠道B", Kind: "standard", Status: 1,
			Models: []string{"m2"}},
	} {
		if err := cs.Upsert(ch); err != nil {
			t.Fatal(err)
		}
	}
	return New(Config{AdminToken: testAdminToken, ListensLoopback: true, Channels: cs})
}

// callImpact 发一次影响预览请求。
func callImpact(t *testing.T, h *Handler, admin bool, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/channels/catalog/impact"+query, nil)
	if admin {
		req = withTestAuth(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ★ ① 正常路径：两类影响分开报。
func TestCatalogImpactEndpointReportsBoth(t *testing.T) {
	h := impactFixture(t)
	rec := callImpact(t, h, true, "?channel_id=ch-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out channelstore.CatalogDeletionImpact
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, rec.Body.String())
	}
	if len(out.Lost) != 1 || out.Lost[0] != "m1" {
		t.Fatalf("Lost 应为 [m1]，实际 %v", out.Lost)
	}
	if len(out.Degraded) != 1 || out.Degraded[0] != "m2" {
		t.Fatalf("Degraded 应为 [m2]，实际 %v", out.Degraded)
	}
	if out.ChannelName == "" {
		t.Fatal("应带上渠道名（否则运维分不清自己点的是哪条）")
	}
}

// ★ ② 缺 channel_id ⇒ 400，而不是回一个"零影响"。
//
// 回零影响是最坏的选择：调用方会读成"删了没事"。
func TestCatalogImpactEndpointRequiresChannelID(t *testing.T) {
	h := impactFixture(t)
	for _, q := range []string{"", "?channel_id=", "?channel_id=%20"} {
		rec := callImpact(t, h, true, q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q 应 400，实际 %d：%s", q, rec.Code, rec.Body.String())
		}
	}
}

// 只有管理员能看（影响分析暴露的是整个目录的供给侧）。
func TestCatalogImpactEndpointRequiresAdmin(t *testing.T) {
	h := impactFixture(t)
	rec := callImpact(t, h, false, "?channel_id=ch-a")
	if rec.Code == http.StatusOK {
		t.Fatalf("★ 非管理员拿到了影响分析：%s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("应 401/403，实际 %d", rec.Code)
	}
}

// ★ ③ 只读 + 禁缓存。
func TestCatalogImpactEndpointIsReadOnlyNoStore(t *testing.T) {
	h := impactFixture(t)
	before := len(h.cfg.Channels.List())

	rec := callImpact(t, h, true, "?channel_id=ch-a")
	if got := rec.Header().Get("Cache-Control"); got != credentialCacheControl {
		t.Fatalf("必须带 no-store（%q），实际 %q", credentialCacheControl, got)
	}
	// 连查三次，渠道数量不变（只读）。
	for i := 0; i < 3; i++ {
		_ = callImpact(t, h, true, "?channel_id=ch-a")
	}
	if after := len(h.cfg.Channels.List()); after != before {
		t.Fatalf("★ 影响预览改了渠道数量：%d → %d（它必须只读）", before, after)
	}
}

// 渠道存储没接线 ⇒ **404**（路径不存在），不是 501、不是 panic。
//
// ⚠ 这条判据第一版断言 501，实测回 404 —— 因为本路由注册在 `registerChannels()` 里，
// 没配渠道存储时整个组都不注册。这与仓库既有模式一致：
// 「路由不存在 ⇒ 前端按 manifest 不渲染操作区，而不是渲染一个点了会 501 的按钮」。
//
// 当时处理函数里还有一段 `if h.cfg.Channels == nil { 501 }` —— 那是**不可达的死代码**，
// 已删。留一段假兜底比没有兜底更坏：下一个人会以为它真的会被执行到。
func TestCatalogImpactEndpointWithoutChannelStore(t *testing.T) {
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	rec := callImpact(t, h, true, "?channel_id=ch-a")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404（路由未注册），实际 %d：%s", rec.Code, rec.Body.String())
	}
}
