package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestTasksTableBalanceColumnHidesWhenNoMeaningfulValue T-908：任务表（ltask）的余额列同样处理。
//
// # 背景（2026-11-13 用户要求「余额 0 列也不要」，主脑决定两张表统一）
//
// 任务表的余额格原来是**无条件**渲染的。而 phanthywork 的**任务视图**按契约：
// balance_known **恒为 false**、且**不许给** balance —— 见 internal/phanthywork/invite_test.go:333-347
// （原话：「不该给 balance —— 会让前端长出多余的列」）。
// 那一列在任务表上永远是「—」，占一整列宽度却零信息量 ⇒ 整列不显示。
//
// # 判据与邀请码面板**同一份**
//
// hasMeaningfulBalance(accts)：至少一行 balance_known !== false 且 Number(balance) > 0。
// 两处共用同一个函数（colSet 的 balance 键 / 任务表的 showBal）—— 判据不许各写一份。
//
// # 判据为什么是执行式
//
// 表头 th、行内格子、占位行列数（colspan）三处必须**同步**，否则表格歪一列而静态看不出来。
// 这里把 app.js 里的真代码抠出来跑：判据函数、任务表的余额格渲染点、表头门控表达式、行模板、
// panelRowLoading —— 不是抄第二份实现。
func TestTasksTableBalanceColumnHidesWhenNoMeaningfulValue(t *testing.T) {
	src, err := os.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	for _, need := range []string{"function hasMeaningfulBalance(accts)", "const balanceTd = ", "showBal = hasMeaningfulBalance(accts)", "colN = 6 + (showBal ? 1 : 0)"} {
		if !strings.Contains(js, need) {
			t.Fatalf("app.js 里找不到 %q —— 任务表的余额门控被拿掉/改名了？（守卫要跟着改，别删）", need)
		}
	}
	node, lerr := exec.LookPath("node")
	if lerr != nil {
		t.Skip("没有 node，跳过执行式判据")
	}
	harness := strings.Join([]string{
		"const fs = require('fs');",
		"const js = fs.readFileSync('ui/app.js', 'utf8');",
		"const BT = String.fromCharCode(96);",
		"const NL = String.fromCharCode(10);",
		"const bad = [];",
		"const ok = (c, m) => { if (!c) bad.push(m); };",
		"const esc = (s) => String(s);",
		"function win(from, to) {",
		"  const i = js.indexOf(from);",
		"  if (i < 0) { console.error('抠不到起点 ' + from); process.exit(9); }",
		"  const j = js.indexOf(to, i);",
		"  if (j < 0) { console.error('抠不到终点 ' + to); process.exit(9); }",
		"  return js.slice(i, j);",
		"}",
		"eval(win('function hasMeaningfulBalance(accts) {', 'function colSet(accts) {'));",
		"function panelRowId(cap, uid) { return cap + '-' + uid; }",
		"eval(win('function panelRowLoading(', 'function panelRowErr('));",
		"if (typeof hasMeaningfulBalance !== 'function' || typeof panelRowLoading !== 'function') { console.error('抠出来的代码没定义出函数'); process.exit(9); }",
		"const lines = js.split(NL);",
		"const balLine = lines.find((l) => l.indexOf('const balanceTd = ') >= 0);",
		"if (!balLine) { console.error('找不到 balanceTd'); process.exit(9); }",
		"const arrow = balLine.slice(balLine.indexOf('= ') + 2).replace(/;\\s*$/, '');",
		"const mkCell = (sb) => new Function('showBal', 'esc', 'return (' + arrow + ');')(sb, esc);",
		"const hdrLine = lines.find((l) => l.indexOf('<th>UID</th>') >= 0 && l.indexOf('showBal') >= 0);",
		"if (!hdrLine) { console.error('找不到任务表表头里的余额门控'); process.exit(9); }",
		"function gateExpr(line, needle) {",
		"  const k = line.indexOf(needle);",
		"  if (k < 0) return null;",
		"  const start = line.lastIndexOf('$' + '{', k);",
		"  if (start < 0) return null;",
		"  let depth = 0;",
		"  for (let i = start + 1; i < line.length; i++) {",
		"    const c = line[i];",
		"    if (c === '{') depth++;",
		"    else if (c === '}') { depth--; if (depth === 0) return line.slice(start + 2, i); }",
		"  }",
		"  return null;",
		"}",
		"const thExpr = gateExpr(hdrLine, 'showBal ?');",
		"if (!thExpr) { console.error('表头里的余额门控表达式抠不出来'); process.exit(9); }",
		"const mkHdr = (sb) => new Function('showBal', 'return ' + BT + hdrLine.trim() + BT + ';')(sb);",
		"const rowStart = js.indexOf('return ' + BT + '<tr>', js.indexOf('const rowHTML'));",
		"const rowEnd = js.indexOf(BT + ';', rowStart);",
		"if (rowStart < 0 || rowEnd < 0) { console.error('行模板抠不出来'); process.exit(9); }",
		"const rowTpl = js.slice(rowStart, rowEnd);",
		"const rowLiteralTd = (rowTpl.match(/<td/g) || []).length;",
		"const cases = [",
		"  ['① 任务表全列无有意义取值（任务视图：balance_known 恒 false、不给 balance）', [{ uid: 'u1', balance_known: false }, { uid: 'u2', balance_known: false }], false],",
		"  ['①b 完全没有余额字段的行', [{ uid: 'u1' }, { uid: 'u2' }], false],",
		"  ['② 一行有真值 5', [{ uid: 'u1', balance_known: false }, { uid: 'u2', balance_known: true, balance: 5 }], true],",
		"];",
		"for (const [label, rows, want] of cases) {",
		"  const sb = hasMeaningfulBalance(rows);",
		"  ok(sb === want, label + ': hasMeaningfulBalance = ' + sb + ' want ' + want);",
		"  const colN = 6 + (sb ? 1 : 0);",
		"  const hdr = mkHdr(sb);",
		"  const thCount = (hdr.match(/<th/g) || []).length;",
		"  ok(sb ? hdr.indexOf('余额') >= 0 : hdr.indexOf('余额') < 0, label + ': 表头 = ' + JSON.stringify(hdr.trim().slice(0, 110)));",
		"  const cell = mkCell(sb)(rows[rows.length - 1]);",
		"  ok(sb ? cell.indexOf('<td') >= 0 : cell === '', label + ': 余额格 = ' + JSON.stringify(cell));",
		"  if (sb) ok(cell.indexOf('5') >= 0, label + ': 有真值时格子该显示 5（实际 ' + JSON.stringify(cell) + '）');",
		"  ok(thCount === colN, label + ': 表头列数 ' + thCount + ' ≠ 代码里的列数 ' + colN);",
		"  const rowTd = rowLiteralTd + (cell !== '' ? 1 : 0);",
		"  ok(rowTd === thCount, label + ': 行内 td 数 ' + rowTd + ' ≠ 表头列数 ' + thCount + '（表格会歪一列）');",
		"  const lr = (panelRowLoading('tasks', 'u1', 'nn', colN).match(/<td/g) || []).length;",
		"  ok(lr === colN, label + ': 占位行 td 数 ' + lr + ' ≠ colN ' + colN + '（占位行会歪）');",
		"}",
		"ok(js.indexOf('balance: hasMeaningfulBalance(accts)') >= 0, '④ colSet 的 balance 键必须复用 hasMeaningfulBalance（判据只有一份）');",
		"if (bad.length) { console.error(bad.join(' | ')); process.exit(1); }",
		"console.log('OK rowLiteralTd=' + rowLiteralTd);",
	}, "\n")
	out, runErr := exec.Command(node, "-e", harness).CombinedOutput()
	if runErr != nil || !strings.Contains(string(out), "OK") {
		t.Errorf("★ 任务表余额列不合格（%v）:\n%s", runErr, out)
	}
}
