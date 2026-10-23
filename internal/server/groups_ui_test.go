// groups_ui_test.go 分组管理页的接线守卫。
//
// # 这些守卫守的是什么
//
// 分组页是**嵌在二进制里**的独立 HTML（go:embed），而它与后端之间只靠约定连着：
// 后端返回 groups ←→ 前端读 d.groups；keys_unrestricted ←→ 删除后要弹给用户。
// 任何一处改名，另一处**不会报错** —— 页面上那一块空着、那个提示不出现。
//
// # 三条最要紧的
//
//	① 页面必须真的挂上（路由存在、未授权时 302 回登录页）
//	② 删除分组后必须把「哪几把 key 变成不限制了」**显式弹给用户**
//	   —— 那是权限被放大，不是一条可以静默吞掉的统计
//	③ 一致性检查必须区分「查过是干净的」与「压根没查」
//	   —— 把「没查」显示成「一切正常」是这个检查最坏的失效形态
package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func readGroupsUI(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("webui-groups.html")
	if err != nil {
		t.Fatalf("读 webui-groups.html 失败: %v", err)
	}
	return string(raw)
}

// 页面必须内嵌且路由存在。
func TestGroupsUIPageIsEmbedded(t *testing.T) {
	if len(groupsUIPage) == 0 {
		t.Fatal("groupsUIPage 为空 —— //go:embed webui-groups.html 没生效")
	}
	if !strings.Contains(string(groupsUIPage), "分组管理") {
		t.Error("内嵌内容不像分组页（找不到标题）")
	}
}

// 未授权 ⇒ 302 回 /ui（与 /ui/channels 同一条判据）。
func TestGroupsUIRedirectsWhenUnauthenticated(t *testing.T) {
	h := NewHandler(Config{})
	req := httptest.NewRequest(http.MethodGet, "/ui/groups", nil)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("未授权时应 302 回 /ui，实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui" {
		t.Errorf("应跳 /ui，实际 %q", loc)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("未授权时不该回页面内容（%d 字节）", rec.Body.Len())
	}
}

// ★ ② 删除分组后的「权限放大」必须逐把告知。
func TestGroupsUIReportsUnrestrictedKeys(t *testing.T) {
	src := readGroupsUI(t)

	if !strings.Contains(src, "keys_unrestricted") {
		t.Fatal("★ 页面没有读 keys_unrestricted —— 删分组把某些 key 变成「不限制」时，" +
			"用户完全不会知道自己的权限被放开了")
	}
	if !strings.Contains(src, "不受模型限制") {
		t.Error("★ 必须有一句人能读懂的话说明「这几把 key 现在不受模型限制」，而不是只回显字段名")
	}
	// 必须**打断**用户（alert），而不是页面角落的小字：
	// 这是权限放大，滚过去就等于没告知。
	if !strings.Contains(src, "alert(") {
		t.Error("★ 权限放大必须用 alert 打断用户，不能只在页面上写一行小字")
	}
}

// ★ ③ 「没查」与「查了是干净的」必须分开显示。
func TestGroupsUIDistinguishesNotChecked(t *testing.T) {
	src := readGroupsUI(t)
	if !strings.Contains(src, "stale_checkable") {
		t.Fatal("★ 页面没有读 stale_checkable —— 会把「目录没接入、压根没查」显示成「一切正常」，" +
			"而那是这个检查最坏的失效形态")
	}
	if !strings.Contains(src, "模型目录未检查") {
		t.Error("★ 「没查」必须有一句明确的提示，而不是只留一个空列表")
	}
}

// 一键清理不得按模型目录删模型（目录抖动会误伤整套配置）。
func TestGroupsUIPruneIsExplicitAndScoped(t *testing.T) {
	src := readGroupsUI(t)
	if !strings.Contains(src, "/admin/groups/prune") {
		t.Error("缺少「一键清理悬空引用」的调用")
	}
	if !strings.Contains(src, "confirm(") {
		t.Error("一键清理必须先确认 —— 它会让部分 key 变成不限制")
	}
	// 必须说清"不碰分组里的模型"。
	if !strings.Contains(src, "不会动任何分组里的模型") {
		t.Error("必须说明一键清理只清悬空引用、不按目录删模型 —— " +
			"否则用户会以为它能顺手清掉陈旧模型，而那样做会在目录抖动时误伤配置")
	}
}

// 界面必须覆盖四个 CRUD 端点 + 两个一致性端点。
func TestGroupsUICoversAllEndpoints(t *testing.T) {
	src := readGroupsUI(t)
	for _, ep := range []string{
		"/admin/groups",
		"/admin/groups/update",
		"/admin/groups/delete",
		"/admin/groups/consistency",
		"/admin/groups/prune",
	} {
		if !strings.Contains(src, ep) {
			t.Errorf("页面没有调用 %s —— 那个操作在界面上就是不可用的", ep)
		}
	}
}

// 模型必须**从候选里选**，不能只靠手打。
//
// 手打的失败形态是"分组看起来有 N 个模型，实际那个永远匹配不上"，
// 而保存时完全看不出来（与 API Key 白名单同一个坑）。
func TestGroupsUIPicksModelsFromCatalog(t *testing.T) {
	src := readGroupsUI(t)
	for _, n := range []struct{ what, needle string }{
		{"候选容器", `id="gChoices"`},
		{"已选清单置顶", `id="gPickedChips"`},
		{"搜索框", `id="f-search"`},
		{"拉目录", "/v1/models"},
		{"全选当前", "function pickAll("},
		{"取消已选（事件委托）", "gPickedChips\").addEventListener("},
	} {
		if !strings.Contains(src, n.needle) {
			t.Errorf("分组页缺少%s（%q）", n.what, n.needle)
		}
	}
	// 已选清单必须在候选列表**之前**（置顶，否则仍要滚动才看得见）。
	boxAt := strings.Index(src, `id="gPickedBox"`)
	listAt := strings.Index(src, `id="gChoices"`)
	if boxAt < 0 || listAt < 0 {
		t.Fatalf("锚点缺失: box=%d list=%d", boxAt, listAt)
	}
	if boxAt > listAt {
		t.Error("已选清单在候选列表之后 —— 用户仍要滚动才能核对，与「置顶」的要求相反")
	}
}

// 控制台里必须有一个「分组」入口（iframe 内嵌，与渠道页同款）。
func TestConsoleHasGroupsPanel(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, `data-key="groups"`) {
		t.Fatal("控制台没有「分组」面板 —— 页面存在但没人点得到")
	}
	if !strings.Contains(src, `src="/ui/groups"`) {
		t.Error("分组面板没有内嵌 /ui/groups（用户要求内嵌而不是跳转）")
	}
	// 导航项由 section 的 h2 自动生成，所以标题必须可读。
	if !strings.Contains(src, "<h2>分组") {
		t.Error("分组面板缺少 <h2>分组 —— 侧栏导航项由 h2 文本生成，缺了它入口就没有名字")
	}
}
