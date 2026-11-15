// plaza_handler_test.go 模型广场端点的判据。
//
// # 端点层要钉的四条
//
//	① 管理员限定（广场暴露的是整个对外模型承诺面）
//	② 禁缓存
//	③ 只读（连查多次，分组/渠道数量不变）
//	④ 空配置时**不 panic、不 500**，而是 200 + 空广场 + 一句说明
//
// 第④条是这里最容易做错的地方：`cfg.Groups` 或 `cfg.Channels` 为 nil 时
// 直接调 `List()` 会 panic（nil 接口）。那在真实部署里表现为"点一下广场就白屏"。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// plazaFixture 一个分组（含映射）+ 一条能服务它的渠道。
func plazaFixture(t *testing.T) *Handler {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Upsert(&groupstore.Group{
		ID: "g-a", Name: "主线", PolicyVersion: 2,
		ChannelIDs: []string{"ch-a"},
		Models:     []string{"real-x", "ghost"},
		ModelMap:   map[string][]string{"public-x": {"real-x"}},
	}); err != nil {
		t.Fatal(err)
	}
	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Upsert(&channelstore.Channel{
		ID: "ch-a", Name: "a", Kind: "standard", Status: 1,
		Models: []string{"real-x"}, ModelMap: map[string]string{"public-x": "real-x"},
	}); err != nil {
		t.Fatal(err)
	}
	return New(Config{AdminToken: testAdminToken, ListensLoopback: true,
		Groups: gs, Channels: cs})
}

func callPlaza(t *testing.T, h *Handler, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/channels/plaza", nil)
	if admin {
		req = withTestAuth(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ★ ① 正常路径：对外名、分组、孤儿都在。
func TestPlazaEndpointReportsPublicModels(t *testing.T) {
	h := plazaFixture(t)
	rec := callPlaza(t, h, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out channelstore.ModelPlaza
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, rec.Body.String())
	}
	// public-x（有映射）+ ghost（无映射、无渠道）= 2 个。
	if out.Total != 2 {
		t.Fatalf("应有 2 个对外模型，实际 %d：%+v", out.Total, out.Models)
	}
	if out.Orphaned != 1 || len(out.OrphanedList) != 1 || out.OrphanedList[0] != "ghost" {
		t.Fatalf("★ 应报 1 个孤儿 ghost，实际 orphaned=%d list=%v", out.Orphaned, out.OrphanedList)
	}
	for _, m := range out.Models {
		if m.Public == "public-x" && !m.InCatalog {
			t.Fatalf("public-x 有渠道能服务，in_catalog 应为 true")
		}
		if m.Public == "public-x" && len(m.GroupNames) == 0 {
			t.Fatalf("应带分组名（供人读）")
		}
	}
}

// ★ ② 空配置 ⇒ 200 + 空广场 + 说明（**不是 panic / 500**）。
func TestPlazaEndpointToleratesEmptyConfig(t *testing.T) {
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	rec := callPlaza(t, h, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("★ 空配置应 200（实际 %d）—— 直接对 nil 调 List() 会 panic，界面会白屏：%s",
			rec.Code, rec.Body.String())
	}
	var out channelstore.ModelPlaza
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Total != 0 {
		t.Fatalf("空配置应 0 个模型，实际 %d", out.Total)
	}
	if out.Note == "" {
		t.Fatal("空广场必须给一句说明（否则界面一片空白）")
	}
}

// ③ 管理员限定。
func TestPlazaEndpointRequiresAdmin(t *testing.T) {
	h := plazaFixture(t)
	rec := callPlaza(t, h, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("★ 非管理员拿到了广场：%s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("应 401/403，实际 %d", rec.Code)
	}
}

// ★ ④ 只读 + 禁缓存。
func TestPlazaEndpointReadOnlyNoStore(t *testing.T) {
	h := plazaFixture(t)
	before := len(h.cfg.Groups.List())

	rec := callPlaza(t, h, true)
	if got := rec.Header().Get("Cache-Control"); got != credentialCacheControl {
		t.Fatalf("必须带 no-store（%q），实际 %q", credentialCacheControl, got)
	}
	for i := 0; i < 3; i++ {
		_ = callPlaza(t, h, true)
	}
	if after := len(h.cfg.Groups.List()); after != before {
		t.Fatalf("★ 广场改了分组数量：%d → %d（它必须只读）", before, after)
	}
}
