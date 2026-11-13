// armor_ui_test.go —— 破甲三态设置那一块的**前端**判据。
//
// # 为什么要有
//
// 本仓在设置页上踩过两个形态：
//
//	① 控件在、绑定漏了（改了保存不上去 / 保存了读不回来）；
//	② 控件在、绑定到**别的**键（两个控件写同一个配置，改一个丢一个）。
//
// 这条守卫按"**控件 id ↔ 配置键**"逐个钉住，并跑 node --check 兜语法。
package server

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// armorUIKeys 本次新增（含既有两条）的「控件 id → 配置键」对应表。
//
// ⚠ 加控件时**只改这张表**，别在断言里各写一遍：写两遍必然漂，
//
//	而漂的表现是"某个开关静默失效"。
var armorUIKeys = []struct{ id, key string }{
	{"setArmorMode", "prompt_armor_mode"},
	{"setArmorRetry", "prompt_armor_retry"},
	{"setArmorInjectScope", "prompt_armor_inject_scope"},
	{"setArmorInjectTargets", "prompt_armor_inject_targets"},
	{"setArmorBlockScope", "prompt_armor_block_scope"},
	{"setArmorBlockTargets", "prompt_armor_block_targets"},
	{"setArmorObserve", "prompt_armor_observe"},
	{"setArmorPatterns", "prompt_armor_patterns"},
	{"setArmorPatternsOnly", "prompt_armor_patterns_only"},
}

// TestArmorSettingsControlsAreBound 每个控件都要**存在**且**绑到自己的键**。
func TestArmorSettingsControlsAreBound(t *testing.T) {
	html, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	page := string(html)
	js, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读 ui/app.js 失败: %v", err)
	}
	app := string(js)
	seenKey := map[string]string{} // 键 → 绑它的控件 id（抓"两个控件写同一个键"）
	for _, c := range armorUIKeys {
		if !strings.Contains(page, "id=\""+c.id+"\"") {
			t.Errorf("★ webui.html 里没有控件 %q —— 界面上少一个开关", c.id)
			continue
		}
		binding := "['" + c.id + "', '" + c.key + "',"
		if !strings.Contains(app, binding) {
			t.Errorf("★ app.js 的 SET_FIELDS 里没有绑定 %s → %s（控件在、绑定漏了 ⇒ 改了保存不上）", c.id, c.key)
		}
		if prev, dup := seenKey[c.key]; dup {
			t.Errorf("★ 两个控件绑到了同一个键 %s：%s 与 %s（改一个丢一个）", c.key, prev, c.id)
		}
		seenKey[c.key] = c.id
	}
}

// TestArmorModeSelectHasThreeStates 下拉**恰好三档**：关闭 / 追加（启用破甲）/ 禁用（拦截）。
//
// # T-902 的收窄（用户看图后要求）
//
// 不再提供 replace（替换）：它会把整段 system 覆盖掉，实测代价（无视格式要求、
// JSON 静默失败、每请求多烧 ~6000 token）远大于收益。旧配置里的 replace
// 由后端**读得到并归一成 append**（见 cmd/server 的 normalize 与这条文件里的
// TestArmorReplaceCompatibility 说明）—— 所以这里要断言它**不再出现在界面**。
func TestArmorModeSelectHasThreeStates(t *testing.T) {
	html, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	start := strings.Index(page, "id=\"setArmorMode\"")
	if start < 0 {
		t.Fatal("找不到 setArmorMode 下拉")
	}
	end := strings.Index(page[start:], "</select>")
	if end < 0 {
		t.Fatal("setArmorMode 下拉没有收尾 —— 守卫切不出边界（fail-open）")
	}
	sel := page[start : start+end]
	// 恰好三档：既要有这三档，也不许多出别的（"多出 replace"就是这次要收掉的东西）。
	var got []string
	for _, m := range optionValueRe.FindAllStringSubmatch(sel, -1) {
		got = append(got, m[1])
	}
	want := []string{"off", "append", "block"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("★ 模式下拉的档位 = %v want %v —— 必须恰好是 关闭/追加(启用)/禁用(拦截) 三档", got, want)
	}
	if strings.Contains(sel, "\"replace\"") {
		t.Error("★ 下拉里仍有 replace —— T-902 已收窄成三档（replace 只做旧配置读兼容，不进界面）")
	}
}

// optionValueRe 抠 <option value="x"> 的 value（判档位集合用）。
var optionValueRe = regexp.MustCompile(`value="([^"]+)"`)

// appJSWindow 从 app.js 里抠一段（按起止标记），供 node 执行式断言用。
//
// ⚠ 找不到就 Fatal（fail-open 的守卫比没有守卫更坏）：标记被改名时必须跟着改。
func appJSWindow(t *testing.T, startMark, endMark string) string {
	t.Helper()
	js, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatalf("读 ui/app.js 失败: %v", err)
	}
	s := string(js)
	i := strings.Index(s, startMark)
	if i < 0 {
		t.Fatalf("app.js 里找不到起点 %q（改成这样也可以，但守卫必须跟着改）", startMark)
	}
	rest := s[i:]
	j := strings.Index(rest, endMark)
	if j < 0 {
		t.Fatalf("app.js 里找不到终点 %q —— 切不出边界", endMark)
	}
	return rest[:j]
}

// TestArmorSectionsVisibilityByMode ② 显隐按模式（**执行式**：真跑那段判定）。
//
// 判据是字段/模式驱动（只看模式字符串）—— 不涉及任何上游名。
// 三档的期望（用户看图后拍板）：
//
//	off     ⇒ 下面**不显示**任何配置项（只留一行说明）
//	append  ⇒ 只显示「注入范围 + 注入名单」
//	block   ⇒ 显示「拦截范围 + 名单 + 观察模式 + 模式库」
func TestArmorSectionsVisibilityByMode(t *testing.T) {
	snippet := appJSWindow(t, "const ARMOR_MODE_SECTIONS", "function applyArmorModeVisibility(")
	if !strings.Contains(snippet, "function armorSectionsFor(") {
		t.Fatalf("抠出来的片段不含 armorSectionsFor —— 切错位置了（fail-open）:\n%s", snippet)
	}
	node, lerr := exec.LookPath("node")
	if lerr != nil {
		t.Skip("没有 node，跳过执行式判据（下面那条静态判据仍然生效）")
	}
	harness := strings.Join([]string{
		snippet,
		"const cases = [['off', []], ['append', ['append']], ['block', ['block']], ['replace', ['append']], ['', []]];",
		"for (const [m, want] of cases) {",
		"  const got = armorSectionsFor(m);",
		"  if (JSON.stringify(got) !== JSON.stringify(want)) {",
		"    console.error('mode ' + JSON.stringify(m) + ' -> ' + JSON.stringify(got) + ' want ' + JSON.stringify(want));",
		"    process.exit(1);",
		"  }",
		"}",
		"console.log('OK');",
	}, "\n")
	out, runErr := exec.Command(node, "-e", harness).CombinedOutput()
	if runErr != nil || !strings.Contains(string(out), "OK") {
		t.Errorf("显隐判定在 node 里跑不过（%v）:\n%s\n———— 片段 ————\n%s", runErr, out, snippet)
	}

	// 静态兜底 + 非空匹配：函数返回的每个段名，HTML 里都必须真有对应的 data-armorsec。
	// （防"函数返回了 HTML 里不存在的段名" ⇒ 界面永远全部隐藏，而断言全绿。）
	html, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, sec := range []string{"append", "block"} {
		if !strings.Contains(page, "data-armorsec=\""+sec+"\"") {
			t.Errorf("★ webui.html 里没有任何 data-armorsec=%q 的配置段 —— 显隐开关切了个空", sec)
		}
	}
	if !strings.Contains(page, "id=\"armorModeHint\"") {
		t.Error("没有 #armorModeHint —— off 档" + "什么都不显示" + "时用户会以为界面坏了，需要一行说明")
	}
}

// TestArmorTargetsArePickersNotFreeText ③ 名单是**选择器**，不是手打输入框。
//
// 这条要能抓住两类回退：① 又退回 `<input type=text>`；② 控件不来自真实清单
// （例如留一个空 select 让用户自己填）。
func TestArmorTargetsArePickersNotFreeText(t *testing.T) {
	html, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, c := range []struct{ which, id string }{
		{"inject", "setArmorInjectTargets"},
		{"block", "setArmorBlockTargets"},
	} {
		pick := c.id + "Pick"
		if !strings.Contains(page, "id=\""+pick+"\"") || !strings.Contains(page, "data-armorpick=\""+c.which+"\"") {
			t.Errorf("★ 没有 %q 这个选择器（data-armorpick=%q）—— 名单必须是可选可加的控件", pick, c.which)
		}
		if !strings.Contains(page, "id=\""+c.id+"Chips\"") {
			t.Errorf("★ 没有 %q 这个 chip 容器 —— 选中的项要有 × 可删（用户要求）", c.id+"Chips")
		}
		// 旧的手打输入框必须变成 hidden（存盘格式不变），且**不许**再有 text 形态。
		if !strings.Contains(page, "type=\"hidden\" id=\""+c.id+"\"") {
			t.Errorf("★ %q 不再是隐藏输入（存盘仍要它）", c.id)
		}
		if strings.Contains(page, "type=\"text\" id=\""+c.id+"\"") {
			t.Errorf("★ %q 又退回手打输入框了 —— 名单只许从上游/渠道清单里选", c.id)
		}
	}
	// 候选项必须来自真实清单（上游 manifest + 渠道端点），而不是空 select。
	js, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(js)
	if !strings.Contains(app, "providerRegistryIds()") {
		t.Error("★ 名单候选没有取上游清单（providerRegistryIds）")
	}
	if !strings.Contains(app, "/admin/channels") {
		t.Error("★ 名单候选没有取渠道清单（/admin/channels）")
	}
	// 清单为空时的可读提示：**不得**退化成手打（上面那条 text 断言是硬判据，这里是提示存在性）。
	if !strings.Contains(app, "还没有可用的上游或渠道") {
		t.Error("★ 清单为空时没有可读提示（用户会对着一个空选择器发呆）")
	}
}

// TestArmorTargetsStorageFormatUnchanged ④ 存盘格式仍是列表字符串（回归锁）。
//
// 分配格式不改是这次三条里唯一的"硬约束"：后端键、normalize、splitArmorList
// 一行都不用动，所以前端必须照旧往那两个 input 里写"逗号分隔字符串"。
func TestArmorTargetsStorageFormatUnchanged(t *testing.T) {
	js, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(js)
	if !strings.Contains(app, "list.join(', ')") {
		t.Error("★ 前端不再往隐藏输入里写逗号分隔字符串 —— 存盘格式变了，后端/ normalize 会被迫跟着改")
	}
	// SET_FIELDS 里那两条绑定必须仍是 'str'，且 id 没变（改 id 等于换配置键）。
	for _, id := range []string{"setArmorInjectTargets", "setArmorBlockTargets"} {
		if !strings.Contains(app, "['"+id+"', '") {
			t.Errorf("★ %q 的 SET_FIELDS 绑定没了（改名/删了都会让存盘断链）", id)
		}
	}
	if !strings.Contains(app, "'prompt_armor_inject_targets', 'str'") || !strings.Contains(app, "'prompt_armor_block_targets', 'str'") {
		t.Error("★ 名单的绑定类型不再是 'str'（后端键是逗号分隔字符串，类型一改就断）")
	}
	// 执行式：拆分口径必须与后端 splitArmorList 同形（逗号/中文逗号/分号/空白都要认）。
	snippet := appJSWindow(t, "function splitArmorListValue(", "function armorTargetList(")
	node, lerr := exec.LookPath("node")
	if lerr != nil {
		t.Skip("没有 node，跳过执行式判据（静态判据仍然生效）")
	}
	harness := strings.Join([]string{
		snippet,
		"const got = splitArmorListValue('phanthywork, 视图渠道；loomy codearts');",
		"const want = ['phanthywork', '视图渠道', 'loomy', 'codearts'];",
		"if (JSON.stringify(got) !== JSON.stringify(want)) { console.error(JSON.stringify(got)); process.exit(1); }",
		"if (splitArmorListValue('a, a, b').length !== 2) { console.error('去重失效'); process.exit(2); }",
		// 换行也要认（设置页里粘贴多行是常见形态）；用 fromCharCode 避免在 Go 字符串里再套转义。
		"if (splitArmorListValue('a' + String.fromCharCode(10) + 'b').length !== 2) { console.error('换行分隔没认'); process.exit(3); }",
		"console.log('OK');",
	}, "\n")
	out, runErr := exec.Command(node, "-e", harness).CombinedOutput()
	if runErr != nil || !strings.Contains(string(out), "OK") {
		t.Errorf("拆分口径在 node 里跑不过（%v）:\n%s\n———— 片段 ————\n%s", runErr, out, snippet)
	}
}

// TestArmorUIAssetsParse 执行式兜底：脚本语法必须过（node --check）。
func TestArmorUIAssetsParse(t *testing.T) {
	js, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	nodeCheck(t, "armor-app.js", string(js))
}
