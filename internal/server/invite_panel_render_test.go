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

// TestInviteBalanceColumnHidesWhenNoMeaningfulValue 余额列「整列无有意义取值 ⇒ 整列不显示」。
//
// # 用户原话与背景（2026-11-13）
//
// 截图里两行的「余额」都是 `—`：phanthywork 没有余额口径，后端诚实报 balance_known:false
// （**不许**把它改成 0 或删字段 —— 本仓纪律「未探测 ≠ 0」）。用户的要求是「余额 0 列也不要」。
//
// # 为什么要和别的列分开（这条区分是本轮的语义核心）
//
//	· 状态类列（绑定状态 / 激活状态 / 我生成的码…）：某行是空或「—」= 这一行**当下**的真实状态
//	  ⇒ 一列里有几行「—」很正常，只要有真值就保留（hideEmptyCols 那条通用规则，**不动**）；
//	· 余额列：balance_known === false = **这个上游没有余额口径** ⇒ 全列「—」时这一列没有信息量
//	  ⇒ 整列（含表头）不显示；0 也不算有意义取值（整列皆 0 同样没信息量）。
//
// # 判据为什么必须执行式
//
// map/模板里的东西写对了但"没被执行"过（本仓 2026-11-12 的 hintText 事故）⇒ 这里只跑真代码：
//
//	· colSet / balanceCellHTML 从 ui/app.js 里**抠出来执行**（不是抄第二份）；
//	· 行 td 的门控表达式与表头 th 的门控表达式也从 app.js 里抠出来**真求值**。
//
// 改实现（去掉门控、换个判据）必然改变结果 ⇒ 这组断言会红。
func TestInviteBalanceColumnHidesWhenNoMeaningfulValue(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 app.js: %v", err)
	}
	// 先做一次廉价的存在性检查：门控没了就没必要去 node 里绕一圈（报错也更直白）。
	if !strings.Contains(string(src), "cols.balance") {
		t.Fatalf("app.js 里找不到 cols.balance —— 余额列的门控被拿掉了（本轮要求：整列无有意义取值就不出这一列）")
	}
	node, lerr := exec.LookPath("node")
	if lerr != nil {
		t.Skip("没有 node，跳过执行式判据")
	}
	harness := strings.Join([]string{
		`const fs = require('fs');`,
		`const js = fs.readFileSync('ui/app.js', 'utf8');`,
		`function win(from, to) {`,
		`  const i = js.indexOf(from);`,
		`  if (i < 0) { console.error('抠不到起点: ' + from); process.exit(9); }`,
		`  const j = js.indexOf(to, i);`,
		`  if (j < 0) { console.error('抠不到终点: ' + to); process.exit(9); }`,
		`  return js.slice(i, j);`,
		`}`,
		`const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;');`,
		`// ⚠ 判据已抽成共享函数 hasMeaningfulBalance（T-908 起任务表也用它）⇒ 窗口要含它，`,
		`//   否则 colSet 在 node 里 ReferenceError（这正是它第一次红出来的原因）。`,
		`eval(win('function hasMeaningfulBalance(accts) {', 'function colSet(accts) {'));`,
		`eval(win('function colSet(accts) {', 'function balanceCellHTML(a) {'));`,
		`eval(win('function balanceCellHTML(a) {', '// openPageModal'));`,
		`if (typeof colSet !== 'function' || typeof balanceCellHTML !== 'function') { console.error('抠出来的代码没定义出函数'); process.exit(9); }`,
		`// 门控表达式也从 app.js 里抠（不抄）：行 td 与表头 th 必须各有一条 cols.balance 门控。`,
		`const jsLines = js.split(String.fromCharCode(10));`,
		`const rowLine = jsLines.find(l => l.includes('cols.balance ?') && l.includes('balanceCellHTML'));`,
		`const thLine = jsLines.find(l => l.includes('cols.balance ?') && l.includes('余额'));`,
		`if (!rowLine || !thLine) { console.error('行或表头的余额门控没了（cols.balance ? 找不到）'); process.exit(9); }`,
		`// 抠出 needle 所在的那个插值表达式，**按花括号配平**取界（表头那一行有 7 个插值，`,
		`// 用"第一个 ${ 到最后一个 }"会把半行都吞进来 —— 第一版就这么错的）。`,
		`function gateExpr(line, needle) {`,
		`  const k = line.indexOf(needle);`,
		`  if (k < 0) return null;`,
		`  const start = line.lastIndexOf('${', k);`,
		`  if (start < 0) return null;`,
		`  let depth = 0;`,
		`  for (let i = start + 1; i < line.length; i++) {`,
		`    const c = line[i];`,
		`    if (c === '{') depth++;`,
		`    else if (c === '}') { depth--; if (depth === 0) return line.slice(start + 2, i); }`,
		`  }`,
		`  return null;`,
		`}`,
		`const rowExpr = gateExpr(rowLine, 'cols.balance ?');`,
		`const thExpr = gateExpr(thLine, 'cols.balance ?');`,
		`if (!rowExpr || !thExpr) { console.error('行/表头的余额门控表达式抠不出来'); process.exit(9); }`,
		`const evalRow = (cols, a) => new Function('cols', 'balanceCellHTML', 'a', 'return (' + rowExpr + ');')(cols, balanceCellHTML, a);`,
		`const evalTh = (cols) => new Function('cols', 'return (' + thExpr + ');')(cols);`,
		`const bad = [];`,
		`function check(label, rows, wantCol, wantCellHas) {`,
		`  const cols = colSet(rows);`,
		`  if (cols.balance !== wantCol) bad.push(label + ': colSet.balance = ' + cols.balance + ' want ' + wantCol);`,
		`  const th = evalTh(cols);`,
		`  if (wantCol ? th.indexOf('余额') < 0 : th !== '') bad.push(label + ': 表头 = ' + JSON.stringify(th));`,
		`  const last = rows[rows.length - 1];`,
		`  const td = evalRow(cols, last);`,
		`  if (!wantCol && td !== '') bad.push(label + ': 仍然渲染了余额单元格: ' + JSON.stringify(td));`,
		`  if (wantCellHas && td.indexOf(wantCellHas) < 0) bad.push(label + ': 余额单元格里没有 ' + wantCellHas + '（= ' + JSON.stringify(td) + '）');`,
		`}`,
		`// ① 两行都未知 ⇒ 整列不出现（用户截图那条）`,
		`check('① 两行都未知', [{ uid: 'u1', balance_known: false }, { uid: 'u2', balance_known: false }], false, null);`,
		`// ② 一行有 5 ⇒ 必须出现且显示 5（防"顺手把这列删了"）`,
		`check('② 一行有 5', [{ uid: 'u1', balance_known: false }, { uid: 'u2', balance_known: true, balance: 5 }], true, '5');`,
		`// ③ 一行 0 + 一行未知 ⇒ 0 不算有意义取值 ⇒ 整列不显示`,
		`check('③ 0 与未知', [{ uid: 'u1', balance_known: true, balance: 0 }, { uid: 'u2', balance_known: false }], false, null);`,
		`// ②b 一行有 5 时，未知那一行的格子仍画「—」（不是被顺手填 0）`,
		`{`,
		`  const cols = colSet([{ uid: 'u1', balance_known: false }, { uid: 'u2', balance_known: true, balance: 5 }]);`,
		`  const cell = balanceCellHTML({ balance_known: false });`,
		`  if (cell.indexOf('—') < 0) bad.push('未知行的余额格子必须画「—」，实际 ' + JSON.stringify(cell));`,
		`  if (evalRow(cols, { uid: 'u2', balance_known: true, balance: 5 }) !== '<td class="mono">5</td>') bad.push('有值时格子应为 <td class="mono">5</td>');`,
		`}`,
		`// ④ 回归锁：其余列的判据一个都没变（有「—」/缺值的列仍然按"有真值就留"）`,
		`{`,
		`  const cols = colSet([{ uid: 'u1', invited: true, balance_known: false }, { uid: 'u2', balance_known: false }]);`,
		`  if (cols.invited !== true) bad.push('④ invited 列被误藏（原来有一行 true 就该留）');`,
		`  if (cols.activated !== false || cols.codes !== false || cols.myCode !== false || cols.wechat !== false || cols.nickname !== false) bad.push('④ 其余列的判据被改动了');`,
		`  const nOnly = colSet([{ uid: 'u1', nickname: '  ', balance_known: false }]);`,
		`  if (nOnly.nickname !== false) bad.push('④ nickname 的 hasText 判据被改动了（空白串不算有值）');`,
		`}`,
		`if (bad.length) { console.error(bad.join(' | ')); process.exit(1); }`,
		`console.log('OK');`,
	}, "\n")
	out, runErr := exec.Command(node, "-e", harness).CombinedOutput()
	if runErr != nil || !strings.Contains(string(out), "OK") {
		t.Errorf("★ 余额列整列的显隐判据不对（%v）:\n%s", runErr, out)
	}
}

// TestInvitePanelHintBlockIsRealCode ① 执行式：那段代码必须真的能跑起来。
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
