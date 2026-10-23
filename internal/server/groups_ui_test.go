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
	"path/filepath"
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
	// ⚠ 不能断言"响应体为空"：http.Redirect 会写一小段说明 HTML（约 26 字节）。
	//   要守的是"**没有把页面本身发出去**" —— 用页面标题判定。
	if strings.Contains(rec.Body.String(), "分组管理") {
		t.Error("未授权时泄漏了分组页内容")
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
		{"拉目录（管理端，不是 /v1/models）", "/admin/groups/catalog"},
		{"全选当前", "function pickAll("},
		{"取消已选（事件委托）", "gPickedChips').addEventListener("},
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

// ★ API Key 列表的「可用模型」列必须反映**分组**（P2 之后真正的判据）。
//
// # 这条守的是一个会骗人的显示
//
// 分组非空时判定链用分组并集，`k.models` 完全不参与。而那一列原来只看 `k.models` ——
// 于是 P3.5 迁移之后的 key（只配分组、白名单为空）会显示成「全部」，
// 与事实**恰好相反**：它其实被限制在分组里的少数模型上。
//
// 用户拿这一列回答「这把 key 能用什么」，看反了就等于以为没限制。
func TestAPIKeyModelsColumnShowsGroups(t *testing.T) {
	src := readWebUI(t)
	fn := sliceFuncBody(t, src, "function apiKeyModelsCell(k) {")
	if fn == "" {
		t.Fatal("找不到 apiKeyModelsCell —— 锚点改名了，这条守卫会**静默失效**")
	}
	if !strings.Contains(fn, "k.groups") {
		t.Error("★ 「可用模型」列没读 k.groups —— 配了分组的 key 会显示成「全部」，" +
			"而它实际只被允许用分组里的模型（意思正好相反）")
	}
	if !strings.Contains(fn, "个分组") {
		t.Error("有分组时应显示「N 个分组」而不是空白/全部")
	}
	// 存量白名单那条路不能丢（P3.5 之前建的 key 只有 models）。
	if !strings.Contains(fn, "k.models") {
		t.Error("存量白名单（k.models）那条显示分支不该被删 —— 没配分组的 key 还在用它")
	}
}

// sliceFuncBody 截出某个函数体（从签名到配平的 `}`）。
//
// # 为什么需要它
//
// 直接对整个 webui.html 做 strings.Contains 判"有没有 k.groups"太弱：
// 那个字符串在别处也出现（渲染分组页的逻辑、注释里），于是断言会**假通过**。
// 截到函数体再判，才是"这一列真的读了它"。
func sliceFuncBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : j+1]
			}
		}
	}
	t.Fatalf("%s 的大括号没配平 —— 文件被改坏了", sig)
	return ""
}

// 候选目录必须走**管理端**端点，不能用 /v1/models。
//
// # 这是实测踩到的 bug（用户截图：弹窗里显示「读取模型目录失败：[object Object]」）
//
// /v1/models 是调用方端点，要 API key；管理台页面拿的是 admin token ⇒ 稳定 401。
// 而错误体是 {"error":{"code":..,"message":..}}（对象），前端把它塞进
// new Error() 就渲染成 "[object Object]" —— 真实原因（invalid_api_key）被吞掉。
//
// 两条判据都必须成立，缺一个都会退回那个形态：
//  ① 不调用 /v1/models
//  ② 错误对象要取 message/code，不能整体当字符串
func TestGroupsUICatalogUsesAdminEndpoint(t *testing.T) {
	src := sliceFuncBody(t, readGroupsUI(t), "async function ensureCatalog()")
	if strings.Contains(src, "'/v1/models'") || strings.Contains(src, "\"/v1/models\"") {
		t.Error("★ 候选目录不能用 /v1/models —— 那是调用方端点，管理台会 401")
	}
	if !strings.Contains(src, "/admin/groups/catalog") {
		t.Error("★ 候选目录必须走 /admin/groups/catalog（与一致性检查同一份快照）")
	}
}

// 错误文案不能渲染成 [object Object]。
//
// 后端错误体是 {"error":{"code":..,"message":..}}；前端若写
// new Error(j.error) 就把一个对象转成了字符串 —— 用户只看到
// "[object Object]"，真实原因完全丢失。
func TestGroupsUIExtractsErrorMessageFromObject(t *testing.T) {
	src := sliceFuncBody(t, readGroupsUI(t), "async function api(")
	if !strings.Contains(src, "typeof j.error === 'object'") {
		t.Error("★ api() 必须处理 j.error 是对象的情形，否则显示 [object Object]")
	}
	if !strings.Contains(src, "j.error.message") {
		t.Error("★ 必须从错误对象里取 message")
	}
}

// 同样的错误提取逻辑，渠道页也要有（同一个坑不留两处）。
func TestChannelsUIExtractsErrorMessageFromObject(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("webui-channels.html"))
	if err != nil {
		t.Fatalf("读 webui-channels.html: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "typeof j.error === 'object'") {
		t.Error("★ webui-channels.html 的 api() 也有 [object Object] 的问题，必须一并修")
	}
}
