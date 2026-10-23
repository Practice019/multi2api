// groups_prune_test.go P5：删分组时的级联清理、一致性检查、一键清理。
//
// # 三条端到端断言（走真实 HTTP 路由，不只测存储层）
//
//	① 删分组 ⇒ 所有 key 里该分组的引用消失
//	② 因清理变空的 key 会被**明确告知**（权限放大不能静默）
//	③ 一致性检查能分别报出"悬空引用"与"陈旧模型"，且**区分"没查到"与"查了是空的"**
//
// 第 ③ 条的后半截是本文件最容易被漏的：目录没接线时 stale_models 也是 []
// （空切片），若前端把 [] 读成"一切正常"，用户会以为配得好好的。
// 所以报告里有 stale_checkable 这个位。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/groupstore"
)

// pruneFixture 造一个带分组存储 + apikey 存储的管理端。
type pruneFixture struct {
	h      *Handler
	groups *groupstore.Store
	keys   *apikey.Store
}

func newPruneFixture(t *testing.T) *pruneFixture {
	t.Helper()
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{AdminToken: testAdminToken, Groups: gs, APIKeys: ks})
	return &pruneFixture{h: h, groups: gs, keys: ks}
}

func (f *pruneFixture) call(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, withTestAuth(req))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ★ ① 删分组 ⇒ key 里的引用被清掉。
func TestDeleteGroupPrunesKeyRefs(t *testing.T) {
	f := newPruneFixture(t)
	g, err := f.groups.Create("vip", "", []string{"m-a"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := f.keys.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{g.ID}); err != nil {
		t.Fatal(err)
	}

	code, out := f.call(t, http.MethodPost, "/admin/groups/delete", `{"id":"`+g.ID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d: %v", code, out)
	}
	if out["pruned_keys"] == nil {
		t.Errorf("响应应报告清理了几把 key，实际 %v", out)
	}
	groups, _ := f.keys.GroupsOf(k.ID)
	if len(groups) != 0 {
		t.Errorf("★ 删分组后 key 里不该还留着该引用（悬空 ⇒ 该 key 调用全 403），实际 %v", groups)
	}
	// 分组本身确实没了
	if _, ok := f.groups.Get(g.ID); ok {
		t.Error("分组应已被删除")
	}
}

// ★ ② 变空的 key 必须被告知（权限放大不能静默）。
func TestDeleteGroupWarnsAboutUnrestrictedKeys(t *testing.T) {
	f := newPruneFixture(t)
	g, _ := f.groups.Create("only-group", "", []string{"m-a"})
	k, _ := f.keys.Create("app", 0, 0, nil)
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{g.ID}); err != nil {
		t.Fatal(err)
	}

	code, out := f.call(t, http.MethodPost, "/admin/groups/delete", `{"id":"`+g.ID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", code)
	}
	raw, ok := out["keys_unrestricted"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("★ 必须报告变空的 key（否则用户在不知情的情况下放开了权限），实际 %v", out)
	}
	if raw[0] != k.ID {
		t.Errorf("报告里应是 key %s，实际 %v", k.ID, raw[0])
	}
	// warning 是给人看的那句话
	if w, _ := out["warning"].(string); !strings.Contains(w, "不受模型限制") {
		t.Errorf("应有一句明确的 warning，实际 %q", w)
	}
}

// 删分组时，别的分组的引用不能被误清。
func TestDeleteGroupKeepsOtherRefs(t *testing.T) {
	f := newPruneFixture(t)
	gone, _ := f.groups.Create("gone", "", nil)
	keep, _ := f.groups.Create("keep", "", nil)
	k, _ := f.keys.Create("app", 0, 0, nil)
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{gone.ID, keep.ID}); err != nil {
		t.Fatal(err)
	}

	if code, out := f.call(t, http.MethodPost, "/admin/groups/delete", `{"id":"`+gone.ID+`"}`); code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d: %v", code, out)
	}
	groups, _ := f.keys.GroupsOf(k.ID)
	if len(groups) != 1 || groups[0] != keep.ID {
		t.Errorf("★ 只该清掉被删的那个分组，实际 %v", groups)
	}
	// 没变空 ⇒ 不该有 warning
	code, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	if code != http.StatusOK {
		t.Fatalf("一致性端点应可用，实际 %d: %v", code, out)
	}
	if refs, _ := out["dangling_refs"].([]any); len(refs) != 0 {
		t.Errorf("没有悬空引用，实际 %v", refs)
	}
}

// ★ ③ 一致性检查：悬空引用被报出来。
func TestConsistencyReportsDanglingRefs(t *testing.T) {
	f := newPruneFixture(t)
	keep, _ := f.groups.Create("keep", "", []string{"m-a"})
	k, _ := f.keys.Create("app", 0, 0, nil)
	// 直接写一个不存在的分组 ID（模拟 P5 之前删掉的分组留下的残留）
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{keep.ID, "g-ghost"}); err != nil {
		t.Fatal(err)
	}

	code, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if out["enabled"] != true {
		t.Errorf("enabled 应为 true，实际 %v", out["enabled"])
	}
	refs, _ := out["dangling_refs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("应报 1 条悬空引用，实际 %v", out["dangling_refs"])
	}
	first, _ := refs[0].(map[string]any)
	if first["group_id"] != "g-ghost" {
		t.Errorf("悬空的应是 g-ghost，实际 %v", first["group_id"])
	}
	// 只有 1/2 个分组悬空 ⇒ 不是"全悬空"，risky 应为 false
	if first["risky"] != false {
		t.Errorf("部分悬空时 risky 应为 false（那把 key 还能用 keep 组的模型），实际 %v", first["risky"])
	}
	if out["ok"] != false {
		t.Errorf("有悬空引用时 ok 应为 false，实际 %v", out["ok"])
	}
	// 报告里不该出现完整 key（会被缓存/截图/贴工单）
	if s, _ := first["key_id"].(string); s == "" {
		t.Error("应带 key_id")
	}
	if m, _ := first["masked"].(string); !strings.Contains(m, "…") {
		t.Errorf("masked 应是掩码形态，实际 %q", m)
	}
}

// 全部组都悬空 ⇒ risky=true（这把 key 当前一个模型都用不了）。
func TestConsistencyMarksRiskyWhenAllDangling(t *testing.T) {
	f := newPruneFixture(t)
	k, _ := f.keys.Create("app", 0, 0, nil)
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-ghost-1", "g-ghost-2"}); err != nil {
		t.Fatal(err)
	}
	_, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	refs, _ := out["dangling_refs"].([]any)
	if len(refs) != 2 {
		t.Fatalf("应报 2 条，实际 %v", out["dangling_refs"])
	}
	for _, r := range refs {
		m, _ := r.(map[string]any)
		if m["risky"] != true {
			t.Errorf("★ 全悬空时 risky 必须为 true（意味着该 key 当前完全不可用），实际 %v", m)
		}
	}
}

// ★③ 后半截：目录没接线时 stale_checkable=false（"没查" ≠ "全干净"）。
func TestConsistencyStaleCheckableDistinguishesNotChecked(t *testing.T) {
	f := newPruneFixture(t)
	_, _ = f.groups.Create("g", "", []string{"m-a"})
	_, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")

	if out["stale_checkable"] != false {
		t.Errorf("★ 目录未接线时 stale_checkable 必须为 false（否则前端把「没查」显示成「全干净」），实际 %v", out["stale_checkable"])
	}
	if out["catalog_size"] != float64(0) {
		t.Errorf("未接线时 catalog_size 应为 0，实际 %v", out["catalog_size"])
	}
	// 未检查时不能因为"陈旧模型为空"就说 ok（悬空为 0 所以这里 ok=true 是允许的，
	// 但语义上"没查目录"不该被当成"内容新鲜"——见代码里 ok 的计算口径）
	if stale, _ := out["stale_models"].([]any); len(stale) != 0 {
		t.Errorf("未接线时不该报陈旧模型，实际 %v", stale)
	}
}

// 一键清理：只清悬空，清完报告权限放大的 key。
func TestGroupsPruneEndpoint(t *testing.T) {
	f := newPruneFixture(t)
	k, _ := f.keys.Create("app", 0, 0, nil)
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-ghost"}); err != nil {
		t.Fatal(err)
	}

	code, out := f.call(t, http.MethodPost, "/admin/groups/prune", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %v", code, out)
	}
	if out["pruned"] != true {
		t.Errorf("应报告 pruned=true，实际 %v", out)
	}
	if !strings.Contains(fmtStr(out["groups_gone"]), "g-ghost") {
		t.Errorf("应报告清掉了 g-ghost，实际 %v", out["groups_gone"])
	}
	// ⚠ 权限放大必须告知
	if u, _ := out["keys_unrestricted"].([]any); len(u) != 1 {
		t.Errorf("★ 变空的 key 必须被告知，实际 %v", out)
	}
	if w, _ := out["warning"].(string); !strings.Contains(w, "不受模型限制") {
		t.Errorf("应有一句明确的 warning，实际 %q", w)
	}
	// 清完之后再查一致性 ⇒ 干净
	_, after := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	if refs, _ := after["dangling_refs"].([]any); len(refs) != 0 {
		t.Errorf("清理后不该再有悬空引用，实际 %v", refs)
	}
	if groups, _ := f.keys.GroupsOf(k.ID); len(groups) != 0 {
		t.Errorf("key 的分组应已清空，实际 %v", groups)
	}
}

// 一键清理：没有悬空时不做任何事（pruned=false）。
func TestGroupsPruneNoopWhenClean(t *testing.T) {
	f := newPruneFixture(t)
	g, _ := f.groups.Create("g", "", nil)
	k, _ := f.keys.Create("app", 0, 0, nil)
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	code, out := f.call(t, http.MethodPost, "/admin/groups/prune", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if out["pruned"] != false {
		t.Errorf("没有悬空时 pruned 应为 false，实际 %v", out)
	}
	if groups, _ := f.keys.GroupsOf(k.ID); len(groups) != 1 {
		t.Errorf("不该动任何引用，实际 %v", groups)
	}
}

// 未启用分组存储 ⇒ 两个端点都不存在/降级，不是 panic。
func TestConsistencyWithoutGroupStore(t *testing.T) {
	ks, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	// 没注入 Groups ⇒ registerGroups 不会被调用，路由不存在 ⇒ 404
	h := New(Config{AdminToken: testAdminToken, APIKeys: ks})
	req := httptest.NewRequest(http.MethodGet, "/admin/groups/consistency", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withTestAuth(req))
	if rec.Code != http.StatusNotFound {
		t.Errorf("未启用分组时应 404（路由不注册），实际 %d", rec.Code)
	}
}

// fmtStr 把 any 转成可比较的字符串（测试里只为查子串）。
func fmtStr(v any) string {
	switch t := v.(type) {
	case []any:
		var sb strings.Builder
		for _, x := range t {
			sb.WriteString(fmtStr(x))
			sb.WriteString(",")
		}
		return sb.String()
	case string:
		return t
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// 陈旧模型：目录里没有的模型被报出来；目录里有的不报。
//
// 注入 ModelNames 而不是让端点自己去取 —— 这也验证了注入点是可用的。
func TestConsistencyReportsStaleModels(t *testing.T) {
	f := newPruneFixture(t)
	f.h.cfg.ModelNames = func() ModelCatalogSnapshot {
		return ModelCatalogSnapshot{Names: []string{"m-live"}} // 只有 m-live 还活着
	}
	g, err := f.groups.Create("g", "", []string{"m-live", "m-retired"})
	if err != nil {
		t.Fatal(err)
	}

	code, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if out["stale_checkable"] != true {
		t.Fatalf("接了目录之后 stale_checkable 必须为 true，实际 %v", out["stale_checkable"])
	}
	if out["catalog_size"] != float64(1) {
		t.Errorf("catalog_size 应为 1，实际 %v", out["catalog_size"])
	}
	stale, _ := out["stale_models"].([]any)
	if len(stale) != 1 {
		t.Fatalf("应报 1 条陈旧模型，实际 %v", out["stale_models"])
	}
	m, _ := stale[0].(map[string]any)
	if m["model"] != "m-retired" {
		t.Errorf("陈旧的应是 m-retired，实际 %v", m["model"])
	}
	if m["group_id"] != g.ID || m["group_name"] != "g" {
		t.Errorf("应带上分组信息，实际 %v", m)
	}
	if out["ok"] != false {
		t.Errorf("有陈旧模型时 ok 应为 false，实际 %v", out["ok"])
	}
}

// 目录里全都在 ⇒ 没有陈旧模型，ok=true。
func TestConsistencyCleanWhenCatalogCoversGroups(t *testing.T) {
	f := newPruneFixture(t)
	f.h.cfg.ModelNames = func() ModelCatalogSnapshot {
		return ModelCatalogSnapshot{Names: []string{"m-a", "m-b"}}
	}
	_, _ = f.groups.Create("g", "", []string{"m-a"})
	_, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	if stale, _ := out["stale_models"].([]any); len(stale) != 0 {
		t.Errorf("不该有陈旧模型，实际 %v", stale)
	}
	if out["ok"] != true {
		t.Errorf("干净时 ok 应为 true，实际 %v", out["ok"])
	}
}

// ★ 目录**真的为空**（查了但一条都没有）⇒ **不能**判定任何模型陈旧。
//
// # 这条是我改过一个 bug 后重写的
//
// 原语义是「空目录 ⇒ 分组里每个模型都陈旧」。实测后果：CachedModelNames
// 第一版只读默认上游的缓存，把 csg/loomy/qiniuyun/senseaudio 这些渠道模型
// 全报成陈旧（702 个里报 9 个中的 8 个）—— 而界面会把结论呈现成
// 「你分组里全是废名字，清空吧」。那是个会毁配置的建议。
//
// 正确口径：**查不到 ≠ 不存在**。目录为空时宁可说「未就绪」。
func TestConsistencyEmptyCatalogIsNotTreatedAsAllStale(t *testing.T) {
	f := newPruneFixture(t)
	f.h.cfg.ModelNames = func() ModelCatalogSnapshot {
		return ModelCatalogSnapshot{Names: []string{}} // 查了，但目录是空的
	}
	_, _ = f.groups.Create("g", "", []string{"m-a"})

	_, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	if out["stale_checkable"] != false {
		t.Errorf("★ 空目录时 stale_checkable 必须是 false（把「没查到」当成「不存在」会建议用户清空分组），实际 %v", out["stale_checkable"])
	}
	if stale, _ := out["stale_models"].([]any); len(stale) != 0 {
		t.Errorf("★ 空目录不该报任何陈旧模型，实际 %v", stale)
	}
	if out["catalog_size"] != float64(0) {
		t.Errorf("catalog_size 应如实报 0，实际 %v", out["catalog_size"])
	}
}

// ★ 部分来源未就绪时：已查的部分照常判陈旧，但要把没查成的来源报出来。
//
// 破了会怎样：用户看到「无陈旧模型」以为全干净，而那个模型恰好来自
// 没查成的上游 —— 于是问题被一个「全绿」的报告盖住。
func TestConsistencyReportsIncompleteCatalog(t *testing.T) {
	f := newPruneFixture(t)
	f.h.cfg.ModelNames = func() ModelCatalogSnapshot {
		return ModelCatalogSnapshot{
			Names:       []string{"m-live"},
			Unavailable: []string{"minimax"},
		}
	}
	_, _ = f.groups.Create("g", "", []string{"m-live", "m-gone"})

	_, out := f.call(t, http.MethodGet, "/admin/groups/consistency", "")
	inc, _ := out["catalog_incomplete"].([]any)
	if len(inc) != 1 || inc[0] != "minimax" {
		t.Errorf("★ 必须如实报出没查成的来源，实际 %v", out["catalog_incomplete"])
	}
	// 已查的部分仍然可信：m-gone 确实不在 m-live 那一份里。
	if stale, _ := out["stale_models"].([]any); len(stale) != 1 {
		t.Errorf("已查部分应照常判陈旧，实际 %v", stale)
	}
}

// 一键清理**不碰**陈旧模型（只清悬空）—— 目录会抖动，按目录清会误伤。
func TestGroupsPruneDoesNotTouchStaleModels(t *testing.T) {
	f := newPruneFixture(t)
	// 目录是空的（模拟上游抖动），但分组里有模型
	f.h.cfg.ModelNames = func() ModelCatalogSnapshot {
		return ModelCatalogSnapshot{Names: []string{}}
	}
	g, err := f.groups.Create("g", "", []string{"m-a"})
	if err != nil {
		t.Fatal(err)
	}

	code, out := f.call(t, http.MethodPost, "/admin/groups/prune", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	// 没有悬空 ⇒ 什么都不做（尽管陈旧模型有一堆）
	if out["pruned"] != false {
		t.Errorf("没有悬空引用时不该做事，实际 %v", out)
	}
	after, ok := f.groups.Get(g.ID)
	if !ok {
		t.Fatal("分组不该被删")
	}
	if len(after.Models) != 1 || after.Models[0] != "m-a" {
		t.Errorf("★ 一键清理不得按目录删模型（目录抖动时会误伤全部配置），实际 %v", after.Models)
	}
}
