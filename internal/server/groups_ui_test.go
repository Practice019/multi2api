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
	"regexp"
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

// ---- 上游筛选（共享组件 model-picker.js）----
//
// # 为什么需要它（用户实测的痛点）
//
// 候选池是**全站目录**：699 个模型、15 个上游，其中 cline 一家占 497 个。
// 不按上游筛，其余 14 个上游全被压在滚动区深处 —— 选模型等于大海捞针。
//
// # 为什么要守「共享」这件事
//
// 这段逻辑（切上游 / 归组 / 过滤）如果两个页面各写一遍，必然漂移成
// 「分组页能筛、渠道页不能」这种无从解释的差异。所以它被抽成
// internal/server/model-picker.js，由 tools/model_picker_sync.js 注入。
// 注入产物**必须与源文件一致**，否则就是"改了源码但页面没更新"。
func TestGroupsUIModelPickerIsInjected(t *testing.T) {
	ui := readGroupsUI(t)
	for _, n := range []struct{ what, needle string }{
		{"上游解析（带 ownedBy 兜底）", "function mpOwnerOf(id, ownedBy)"},
		{"上游清单", "function mpOwners("},
		{"两级过滤", "function mpFilter("},
		{"上游下拉填充", "function mpFillProviderSelect("},
		{"上游下拉容器（select，与 API 页同形态）", `id="gProvider"`},
		{"全部上游 选项", "全部上游"},
	} {
		if !strings.Contains(ui, n.needle) {
			t.Errorf("★ 分组页缺少%s（%q）—— 上游筛选不可用，699 个模型要靠滚动翻", n.what, n.needle)
		}
	}
}

// 注入产物必须与源文件逐字节一致（改了源码忘了跑同步脚本 ⇒ 这里红）。
func TestGroupsUIModelPickerIsInSync(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("model-picker.js"))
	if err != nil {
		t.Fatalf("读 model-picker.js: %v", err)
	}
	want := strings.TrimSpace(strings.ReplaceAll(string(src), "\r\n", "\n"))
	ui := readGroupsUI(t)
	start := strings.Index(ui, "// ==== WB2API:MODEL-PICKER:START")
	end := strings.Index(ui, "// ==== WB2API:MODEL-PICKER:END ====")
	if start < 0 || end < start {
		t.Fatalf("页面里找不到注入标记（start=%d end=%d）", start, end)
	}
	// 去掉 START 那行本身，取中间正文。
	body := ui[start:end]
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	}
	got := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if got != want {
		t.Error("★ 页面里的 model-picker 与源文件不一致 —— 跑 node tools/model_picker_sync.js 同步")
		t.Logf("源文件 %d 字节，页面内 %d 字节", len(want), len(got))
	}
}

// 「全选当前」必须与列表用**同一套筛选**。
//
// 破了会怎样：用户筛了 cline 看到 497 个，点「全选当前」却把 699 个全选上 ——
// 他会静默选进两百个自己没看见的模型。
func TestGroupsUIPickAllRespectsProviderFilter(t *testing.T) {
	src := sliceFuncBody(t, readGroupsUI(t), "function pickAll(")
	// 用标识符而不是完整调用串：Go 源码里嵌 JS 的引号/括号转义容易让
	// Contains 判据本身出错（我在这里踩过一次），只认标识符更稳。
	if !strings.Contains(src, "gProvider") {
		t.Error("★ pickAll 没读上游下拉 —— 「全选当前」会选进用户看不见的模型")
	}
	if !strings.Contains(src, "mpFilter(") {
		t.Error("★ pickAll 必须用与列表相同的共享过滤（否则两处范围会漂移）")
	}
	if !strings.Contains(src, ".map(function (e) { return e.id; })") {
		t.Error("★ pickAll 必须把 catalog 条目映射成 id（catalog 现在是 {id,owner} 结构）")
	}
}

// 分组页的上游规则必须与 webui.html **同构**（用户指出过形态不一致）。
//
// 用户原话：「这个实现和api管理里面的实现不一样啊 直接复刻啊抽共享」
//
// ⚠ 2026-10-24 更新：API Key 弹窗里的模型选择器已被移除（key 只选分组），
// 所以那边的 #akProvider 不在了。但**对话测试**仍有 #modelProvider，
// 它用的还是同一条上游推断规则 —— 基准换成它。
func TestGroupsUIMatchesConsoleProviderRule(t *testing.T) {
	console, err := os.ReadFile(filepath.Join("webui.html"))
	if err != nil {
		t.Fatalf("读 webui.html: %v", err)
	}
	cs := string(console)
	// 基准：对话测试的上游下拉还在。
	if !strings.Contains(cs, `id="modelProvider"`) {
		t.Fatal("基准变了：webui.html 里找不到 #modelProvider（对话测试的上游筛选）")
	}
	// 分组页的上游筛选同样是 select 下拉。
	ui := readGroupsUI(t)
	if !strings.Contains(ui, `id="gProvider"`) {
		t.Error("★ 分组页的上游筛选不是 select 下拉 —— 与对话测试形态不一致")
	}
	// 两边都必须有 owned_by 兜底。
	for _, f := range []struct{ name, src string }{
		{"webui.html", cs},
		{"分组页", ui},
	} {
		src := sliceFuncBody(t, f.src, "function modelOwnerOf(")
		if src == "" {
			src = sliceFuncBody(t, f.src, "function mpOwnerOf(")
		}
		if !strings.Contains(src, "ownedBy") {
			t.Errorf("★ %s 的上游推断没有 owned_by 兜底（无前缀的模型会丢掉上游）", f.name)
		}
	}
}

// 共享层必须**多页同一份**：webui.html（对话测试）与分组页。
//
// # 这是用户连问两次才做对的事
//
//   「这个实现和api管理里面的实现不一样啊 直接复刻啊抽共享」
//   「不是api管理里面已经实现了1吗直接共享层使用啊」
//
// 第一版我把代码"复刻"到分组页 —— 那是**第二个副本**，不是共享。
// 真正的判据是：webui.html 自己也调用这里，而不是各留一份。
//
// ⚠ 2026-10-24：API Key 弹窗的模型选择器移除后，webui.html 里
// 仍在用共享层的是 **对话测试**（modelOwnerOf + syncModelProviderOptions）。
// 所以断言它俩，而不是已删除的 syncAKProviders。
func TestModelPickerIsSharedAcrossPages(t *testing.T) {
	console, err := os.ReadFile(filepath.Join("webui.html"))
	if err != nil {
		t.Fatalf("读 webui.html: %v", err)
	}
	ui := readGroupsUI(t)

	// ① 两个页面都必须有同一份注入块。
	const startMark = "// ==== WB2API:MODEL-PICKER:START"
	const endMark = "// ==== WB2API:MODEL-PICKER:END ===="
	blockOf := func(src string) string {
		a := strings.Index(src, startMark)
		b := strings.Index(src, endMark)
		if a < 0 || b < a {
			return ""
		}
		body := src[a:b]
		if i := strings.Index(body, "\n"); i >= 0 {
			body = body[i+1:]
		}
		return strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	}
	cb, gb := blockOf(string(console)), blockOf(ui)
	if cb == "" {
		t.Fatal("★ webui.html 里没有共享块 —— 它仍在用自己的私有实现（那就不是共享）")
	}
	if gb == "" {
		t.Fatal("★ webui-groups.html 里没有共享块")
	}
	if cb != gb {
		t.Errorf("★ 两页的共享块不一致（%d vs %d 字节）—— 跑 node tools/model_picker_sync.js", len(cb), len(gb))
	}

	// ② webui.html 的 modelOwnerOf 必须是**委托**，不能自己再实现一遍。
	src := sliceFuncBody(t, string(console), "function modelOwnerOf(")
	if src == "" {
		t.Fatal("webui.html 里找不到 modelOwnerOf")
	}
	if !strings.Contains(src, "mpOwnerOf(") {
		t.Error("★ webui.html 的 modelOwnerOf 没有委托到共享层 —— 判据会有两份，早晚漂移")
	}

	// ③ 仍在用的填充逻辑必须走共享的 mpFillProviderSelect。
	f := sliceFuncBody(t, string(console), "function syncModelProviderOptions(")
	if f == "" {
		t.Fatal("webui.html 里找不到 syncModelProviderOptions（对话测试的上游下拉）")
	}
	if !strings.Contains(f, "mpFillProviderSelect(") {
		t.Error("★ syncModelProviderOptions 没有用共享的 mpFillProviderSelect —— 各写一遍必然有一处写反")
	}
}

// API Key 弹窗只能选**分组**，不能再选模型。
//
// # 用户要求（2026-10-24）
//
//   「这里API 不要设置模型了只能设置分组 因为分组已经设置了模型」
//
// # 为什么这条必须守
//
// 模型范围在分组里定义一次就够了。key 再选一遍模型 = 同一件事两个真相来源：
// 改分组里的模型，key 上那份不会跟着变 —— 那正是我们反复踩的悬空问题
// （见 PLAN-groups.md Q3：两者语义重复，白名单并入分组）。
//
// 判据分两半：**旧控件必须消失** + **新控件必须存在且在提交体里**。
func TestAPIKeyModalSelectsGroupsNotModels(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("webui.html"))
	if err != nil {
		t.Fatalf("读 webui.html: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, `<div id="akModal"`)
	if start < 0 {
		t.Fatal("找不到 API Key 弹窗")
	}
	modal := src[start:]

	// ① 旧模型选择控件必须都不在弹窗里了。
	for _, gone := range []string{
		`id="akAll"`,        // 「全部允许」勾选框
		`id="akModelList"`,  // 模型多选列表
		`id="akProvider"`,   // 上游下拉
		`id="akSearch"`,     // 模型搜索
		`id="akSelectedChips"`,
		`id="akPicker"`,
	} {
		if strings.Contains(modal, gone) {
			t.Errorf("★ API Key 弹窗里还有 %s —— 模型应该在分组里选，不该在这里再选一遍", gone)
		}
	}

	// ② 新分组控件必须在。
	for _, want := range []string{
		`id="akGroupList"`,
		`id="akGroupChips"`,
		`id="akGroupEmpty"`,
	} {
		if !strings.Contains(modal, want) {
			t.Errorf("★ API Key 弹窗缺少分组选择控件 %s", want)
		}
	}

	// ③ 提交体必须带 groups（而不是 models）。
	//
	// ⚠ 2026-10-24：model_map 已移到分组，提交体不再带它。
	if !strings.Contains(src, "limit, rpm, groups }") {
		t.Error("★ 提交体没有带 groups —— 界面上选了也不生效")
	}
	if regexp.MustCompile(`groups, model_map`).MatchString(src) {
		t.Error("★ 提交体还在发 model_map —— 别名已移到分组，key 上配的永远不会生效")
	}
	if regexp.MustCompile(`rpm, models, model_map`).MatchString(src) {
		t.Error("★ 提交体还在发 models —— 后端虽然有兼容，但界面不该再产生它")
	}
}

// 页面里**被调用但没有定义**的项目内函数必须为 0。
//
// # 这条测试是因为一个真实事故加的（2026-10-24）
//
// 我替换「模型选择器」那一整段 JS 时，把三个不相干的函数一起删掉了
// （akAddMapRow / akRenderMap / akCollectMap —— 别名映射用的），
// 但调用点还在。后果不是"映射坏了"，而是 **ReferenceError 打断整个 IIFE**：
// 之后所有 addEventListener 都没执行 ⇒ 界面上每个按钮都点不动。
//
// 用户报的是「按钮没用」，而语法门禁全绿 —— 因为语法确实合法，
// 未定义符号要到运行时才炸。所以需要这条**静态**检查。
//
// 判据只查项目内命名习惯的函数（驼峰、ak/render/sync/update/ensure/close/open/save/mp 前缀），
// 不查浏览器内置（setTimeout/parseInt）与方法调用（.closest/.slice）。
func TestWebUINoUndefinedFunctionCalls(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("webui.html"))
	if err != nil {
		t.Fatalf("读 webui.html: %v", err)
	}
	src := string(raw)

	// 收集全部 <script> 块，按出现顺序累积"已定义"集合
	// （前面的 script 定义的全局函数对后面的可见）。
	type blk struct{ body string }
	var blocks []blk
	idx := 0
	for {
		a := strings.Index(src[idx:], "<script>")
		if a < 0 {
			break
		}
		a += idx
		b := strings.Index(src[a:], "</script>")
		if b < 0 {
			break
		}
		b += a
		blocks = append(blocks, blk{body: src[a+8 : b]})
		idx = b + 9
	}
	if len(blocks) == 0 {
		t.Fatal("webui.html 里没有 <script> 块")
	}

	// 浏览器内置 / DOM 方法：不是项目内函数，跳过。
	skip := map[string]bool{
		"setTimeout": true, "setInterval": true, "clearTimeout": true, "clearInterval": true,
		"requestAnimationFrame": true, "parseInt": true, "parseFloat": true,
		"isNaN": true, "isFinite": true, "encodeURIComponent": true, "decodeURIComponent": true,
		"querySelector": true, "querySelectorAll": true, "closest": true, "actionPath": true,
		"fetchChunk": true, "onChunk": true, "onIdle": true, "dailyBalance": true,
		"pendingCount": true, "reloadProvider": true, "toFixed": true, "slice": true,
	}

	defined := map[string]bool{}
	for i, b := range blocks {
		local := map[string]bool{}
		for _, m := range regexp.MustCompile(`function\s+([A-Za-z_$][\w$]*)\s*\(`).FindAllStringSubmatch(b.body, -1) {
			local[m[1]] = true
		}
		for _, m := range regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=`).FindAllStringSubmatch(b.body, -1) {
			local[m[1]] = true
		}
		// 只查项目内命名习惯：ak*/render*/sync*/update*/ensure*/close*/open*/save*/mp*
		//
		// ⚠ Go 的 regexp 是 RE2，**不支持 lookbehind**（(?<!...) 会 panic）。
		// 所以把「前一个字符」显式吃进捕获组，用第 2 组拿函数名。
		// 前缀 (^|[^.\w$]) 保证不会把 obj.method( 里的 method 算进来。
		callRe := regexp.MustCompile(`(^|[^.\w$])((?:ak|render|sync|update|ensure|close|open|save|mp)[A-Za-z0-9]*)\s*\(`)
		var missing []string
		seen := map[string]bool{}
		for _, m := range callRe.FindAllStringSubmatch(b.body, -1) {
			n := m[2]
			if seen[n] || skip[n] || local[n] || defined[n] {
				continue
			}
			seen[n] = true
			missing = append(missing, n)
		}
		if len(missing) > 0 {
			t.Errorf("★ script[%d] 调用了未定义的函数 %v —— 运行时 ReferenceError 会打断整个 IIFE，界面上所有按钮都会失效", i, missing)
		}
		for d := range local {
			defined[d] = true
		}
	}
}
