// invite_panel_render_test.go 守「JS 语句被写进模板字符串」这一类错（2026-11-12 真事故）。
//
// # 事故形态（用户报障「刷新失败：hintText is not defined」）
//
//	邀请面板那段提示文案的**计算**（const hintHas / const hints / hints.push(...)）
//	被整段写进了 `const head = \`<div …` **模板字符串内部** ⇒ 它们只是 HTML 文本，不是语句；
//	而模板里 `${hintText}` 在插值时该变量不存在 ⇒ **ReferenceError**，整个面板刷新失败。
//
// ⚠ `node --check` **抓不到它**：模板里的 `${}` 插值在语法上是合法的 —— 这正是这轮漏掉的原因。
//
// # 本文件的两条判据
//
//	① **执行式（主判据）**：把「提示文案 + head 模板」那一段**真的在 node 里跑一遍**。
//	   没声明的标识符（hintText 那种）会当场抛 ReferenceError ⇒ 这条能抓住本次事故。
//	② **结构式（兜底，node 不在时也跑）**：head 模板体内不得出现语句行；
//	   模板插值里的标识符必须在文件里被声明过。
//
// ⚠ 诚实交代：② 是**权宜之计** —— 它按文本判定，理论上会被"字符串里恰好长得像声明"骗过；
//
//	真正的判据是 ①（真执行）。保留 ② 只是因为执行式需要 node（CI/本机都有，但不保证任何环境都有）。
package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// appJSInviteHintWindow 取出「提示文案 + head 模板」那一段（含两端的语句）。
func appJSInviteHintWindow(t *testing.T, js string) string {
	t.Helper()
	start := strings.Index(js, "const hintHas")
	if start < 0 {
		t.Fatal("没在 app.js 里找到 `const hintHas`（提示文案那段被改名/搬走了？这条守卫要跟着改，别删）")
	}
	headStart := strings.Index(js[start:], "const head = `")
	if headStart < 0 {
		t.Fatal("`const hintHas` 之后没找到 `const head = ` 那一行 —— 顺序变了？")
	}
	absHead := start + headStart
	end := strings.Index(js[absHead:], "`;")
	if end < 0 {
		t.Fatal("没找到 head 模板的收尾 反引号+分号 —— 模板结构变了？")
	}
	return js[start : absHead+end+2]
}

// TestInvitePanelHintBlockIsRealCode ① 执行式：那段代码必须能真的跑起来。
func TestInvitePanelHintBlockIsRealCode(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	snippet := appJSInviteHintWindow(t, string(src))

	node, lerr := exec.LookPath("node")
	if lerr != nil {
		t.Skip("没有 node，跳过执行式判据（结构式判据仍然生效）")
	}
	// 桩：只给这段代码真正依赖的自由变量（其余任何未声明名字都会 ReferenceError）。
	harness := strings.Join([]string{
		"const accts = [{",
		"  uid: 'u-1', nickname: 'n', ok: true,",
		"  codes: [{ code: 'AAAA-BBBB-CCCC', status: 'active' }],",
		"  my_code: 'AAAA-BBBB-CCCC', my_link: 'https://example.invalid/r/x',",
		"  bind_supported: false, bind_page: undefined, warning: undefined, balance_partial: undefined,",
		"}];",
		"const cols = { activated: false, invited: false, codes: true, myCode: true, wechat: false };",
		"const esc = (s) => String(s);",
		"const providerId = 'phanthywork';",
		"let totalN = 1;",
		"try {",
		"\t" + snippet,
		"\tif (typeof hintText !== 'string' || hintText.length === 0) { console.error('hintText 不是非空字符串'); process.exit(3); }",
		"\tif (!hintText.includes('我生成的码')) { console.error('hintText 缺「我生成的码」那句（cols.codes=true）'); process.exit(4); }",
		"\tif (hintText.includes('微信')) { console.error('cols.wechat=false 时不该出现「微信」那句'); process.exit(5); }",
		"\tif (typeof head !== 'string' || !head.includes(hintText)) { console.error('head 里没插进 hintText'); process.exit(6); }",
		"\tconsole.log('OK');",
		"} catch (e) { console.error('抛出: ' + (e && e.message ? e.message : e)); process.exit(1); }",
	}, "\n")

	cmd := exec.Command(node, "-e", harness)
	out, runErr := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	if runErr != nil || !strings.Contains(outStr, "OK") {
		t.Errorf("邀请面板那段提示文案在 node 里跑不通（**这正是用户报障的形态**）：%v\n输出: %s\n———— 被执行的片段 ————\n%s",
			runErr, outStr, snippet)
	}
}

// TestInvitePanelHeadTemplateHasNoStatements ② 结构式兜底。
//
// 判据一：`const head = \“ 到收尾 `\`;` 之间的**模板体**里不得出现语句行。
// 判据二：该模板里每个 `${…}` 用到的标识符，必须在 app.js 里被声明过（或函数参数）。
func TestInvitePanelHeadTemplateHasNoStatements(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	js := string(src)
	// ⚠ 必须从邀请面板那段（`const hintHas`）**之后**找 `const head` —— 文件里别的面板也有同名变量，
	//   搜第一个会拿到**别的面板**的模板 ⇒ 守卫永远绿（我第一版就是这个错，靠变异验证才发现）。
	anchor := strings.Index(js, "const hintHas")
	if anchor < 0 {
		t.Fatal("没找到 `const hintHas`（提示文案那段）")
	}
	headRel := strings.Index(js[anchor:], "const head = `")
	if headRel < 0 {
		t.Fatal("邀请面板里没找到 `const head = ` 那一行")
	}
	headAt := anchor + headRel
	bodyStart := headAt + len("const head = `")
	bodyEnd := strings.Index(js[bodyStart:], "`;")
	if bodyEnd < 0 {
		t.Fatal("没找到 head 模板的收尾")
	}
	body := js[bodyStart : bodyStart+bodyEnd]

	// 判据一：模板体里出现语句行 ⇒ 那个语句根本没被执行（本事故的形态）。
	stmt := regexp.MustCompile("(?m)^\\s*(const |let |var |function |if \\(|for \\(|while \\(|return |hints\\.push\\()")
	if bad := stmt.FindAllString(body, -1); len(bad) > 0 {
		t.Errorf("head 模板体里出现了 %d 处**语句**（如 %q）—— 它们只是 HTML 文本、不会被求值；"+
			"本仓 2026-11-12 的事故就是它（`hintText is not defined`）⇒ 把计算挪到模板**之前**", len(bad), bad[0])
	}
	// 判据二：插值里的标识符必须被声明过。
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?:const|let|var|function)\s+([A-Za-z_$][\w$]*)`).FindAllStringSubmatch(js, -1) {
		declared[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`function\s+[A-Za-z_$][\w$]*\s*\(([^)]*)\)`).FindAllStringSubmatch(js, -1) {
		for _, p := range strings.Split(m[1], ",") {
			if n := strings.TrimSpace(strings.Split(p, "=")[0]); n != "" {
				declared[n] = true
			}
		}
	}
	globals := map[string]bool{"String": true, "Number": true, "Boolean": true, "Object": true, "Array": true,
		"Math": true, "JSON": true, "Date": true, "RegExp": true, "Promise": true, "undefined": true, "null": true, "true": true, "false": true}
	for _, m := range regexp.MustCompile(`\$\{([^}]*)\}`).FindAllStringSubmatch(body, -1) {
		for _, id := range regexp.MustCompile(`[A-Za-z_$][\w$]*`).FindAllString(m[1], -1) {
			if globals[id] || declared[id] {
				continue
			}
			t.Errorf("head 模板插值里的 %q 在 app.js 里没有被声明过（`${%s}`）—— 运行时会 ReferenceError", id, strings.TrimSpace(m[1]))
		}
	}
}

// TestInviteCodesColumnNotClipped 邀请码那一列不许被"单值截断"规则裁掉（2026-11-13 用户报障）。
//
// # 截断机制（用户截图「只显示 3 个 pill + ...」的真因）
//
//	app.css 的**全局**单元格规则：`th,td{…white-space:nowrap;overflow:hidden;
//	text-overflow:ellipsis;max-width:260px}` —— 它是给"单个值"设计的，
//	而邀请码格子装的是**5 个 pill** ⇒ 超出 260px 就被裁 + 省略号。
//
// # 修法与其边界
//
//	给那一列开修饰类（`td.codewrap`：允许折行、不封顶、不裁），**不动全局 th,td** ——
//	账号池那些表靠 nowrap 保持"一行一账号"。本用例把这条边界也钉住：
//	下面第 2 条断言要求全局规则**原样留着**，谁哪天顺手改了它会当场红。
func TestInviteCodesColumnNotClipped(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("ui", "app.css"))
	if err != nil {
		t.Fatalf("读 app.css: %v", err)
	}
	c := string(css)
	// ① 修饰类存在且真的允许折行/不封顶。
	m := regexp.MustCompile(`(?s)td\.codewrap\{([^}]*)\}`).FindStringSubmatch(c)
	if m == nil {
		t.Fatal("app.css 里没有 `td.codewrap{…}` —— 邀请码列会被全局 nowrap+max-width 裁成 3 个 pill + ...")
	}
	for _, want := range []string{"white-space:normal", "max-width:none", "overflow:visible"} {
		if !strings.Contains(m[1], want) {
			t.Errorf("td.codewrap 规则里缺 `%s`（现值: %s）—— 5 个码展示不全", want, strings.TrimSpace(m[1]))
		}
	}
	if strings.Contains(m[1], "overflow:hidden") {
		t.Errorf("td.codewrap 里还有 overflow:hidden ⇒ 仍会裁: %s", strings.TrimSpace(m[1]))
	}
	// ② 全局 th,td 规则必须**原样保留**（账号池外观不变的结构证据）。
	gm := regexp.MustCompile(`(?s)(^|\n)\s*th,td\{([^}]*)\}`).FindStringSubmatch(c)
	if gm == nil {
		t.Fatal("没找到全局 `th,td{…}` 规则 —— 面板与账号池的单元格样式都靠它")
	}
	for _, want := range []string{"white-space:nowrap", "max-width:260px"} {
		if !strings.Contains(gm[2], want) {
			t.Errorf("全局 `th,td` 少了 `%s` —— 那是**别的表**（账号池）在用的规则，不该动它: %s", want, strings.TrimSpace(gm[2]))
		}
	}
	// ③ 渲染侧：码单元格必须带这个类，且**不许**对数组做截断。
	js, jerr := os.ReadFile(filepath.Join("ui", "app.js"))
	if jerr != nil {
		t.Fatalf("读 app.js: %v", jerr)
	}
	j := string(js)
	if !strings.Contains(j, "`<td class=\"codewrap\">${codes}</td>`") {
		t.Error("邀请码单元格没带 `class=\"codewrap\"` ⇒ 上面的 CSS 不生效")
	}
	if strings.Contains(j, "a.codes.slice(") {
		t.Error("渲染里对 codes 做了 slice —— 码不是全展开（本仓要求 5 个全显示）")
	}
	// ④ 昵称列必须是**条件列**（用户要求：名字看不到就不要这一列）。
	if !strings.Contains(j, "${cols.nickname ? '<th>昵称</th>' : ''}") {
		t.Error("「昵称」表头不是条件式 —— 拿不到展示名的上游会白占一列")
	}
	if !strings.Contains(j, "${cols.nickname ? `<td>${esc(a.nickname)}</td>` : ''}") {
		t.Error("「昵称」单元格不是条件式 —— 表头隐藏而单元格还在会让整行错位")
	}
}
