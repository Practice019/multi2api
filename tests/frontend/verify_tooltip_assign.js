// tests/frontend/verify_tooltip_assign.js —— 守住「hover 读数必须真的被赋值」
//
// # 为什么需要这个文件（2026-10-20）
//
// 用户报：「鼠标在缓存命中率那个表格上悬浮时，没有东西显示。」
//
// 根因是我改 tooltip 时做了一次**整块行替换**，把
//
//	rd.textContent = r.ts.slice(11, 16) + '  命中率 ' + ...
//
// 的**第一行**（也就是那个赋值本身）吃掉了，只留下续行的 `+ '  累计 ' + ...`：
//
//	rd.setAttribute('x', ...);
//	+ '  累计 ' + (r.has ? ...)          ← 悬空的一元加表达式，求值后丢弃
//	+ (r.hasHour ? ... : '')
//	rd.style.display = 'block';         ← 把元素显示出来，但内容从没被写
//
// 结果：竖线出来了、读数框出来了，**里面是空的**。
//
// # ⚠ 为什么已有的门禁拦不住它
//
// `verify_inline_script_syntax.js` 只做**语法**检查（把每段包进函数体试解析）。
// 而 `+ '  累计 ' + x;` 是**完全合法的 JS**（一元加 + 字符串拼接），
// 语法上毫无问题 —— 只是毫无意义。语法门禁永远不会报这种错。
//
// ⇒ 需要一条**语义**守卫：读数字段必须有 `xxx.textContent =` 赋值头，
//   不能是一串孤立的 `+ ...` 续行。
'use strict';

const fs = require('fs');
const path = require('path');

const htmlPath = path.join(__dirname, '..', '..', 'internal', 'server', 'webui.html');
const src = fs.readFileSync(htmlPath, 'utf8');

let fails = 0;
const fail = (msg) => { console.log('FAIL ' + msg); fails++; };
const pass = (msg) => console.log('PASS ' + msg);

// ── ① 24h 缓存命中率趋势的读数（#ctRead）必须有 textContent 赋值 ──

const anchor = "host.querySelector('#ctRead')";
const start = src.indexOf(anchor);
if (start < 0) {
  fail('找不到 ' + anchor + '（读数元素的 querySelector 被删了？）');
} else {
  let end = src.indexOf("host.addEventListener('mouseleave'", start);
  if (end < 0) end = Math.min(start + 4000, src.length);
  const seg = src.slice(start, end);

  if (!/rd\.textContent\s*=/.test(seg)) {
    fail('★ 24h 趋势的读数段落里没有 `rd.textContent =` 赋值');
    console.log('     ⇒ hover 时元素会被 display:block 显示，但内容是空的（用户看不到任何东西）');
  } else {
    pass('① 24h 趋势读数有 `rd.textContent =` 赋值');
  }

  // ── ③ 正向确认：读数里该有的四个字段都在 ──
  const need = ['累计', '本小时', '请求', '总 Token'];
  const missing = need.filter((w) => !seg.includes(w));
  if (missing.length) {
    fail('★ 读数段落缺少字段：' + missing.join(' / '));
  } else {
    pass('③ 读数字段齐全（' + need.join(' / ') + '）');
  }
}

// ── ② 全局扫描：孤立的 `+ '...'` 续行 ──
//
// ⚠ 这条我写错过两次，记下来免得再犯：
//
//   第一版判据「上一行必须以 = + ( [ , ` 结尾」⇒ **误报 44 处**。
//   因为正常的跨行拼接里，上一行可能是任意表达式片段：
//       + esc(x) + ' …</b></span>');          ← 上一行以引号结尾
//   第二版补上「以引号结尾也算」⇒ 又**误报 18 处**：
//       return gDailyAll
//       + addBtn
//       + noLoginNote                          ← 上一行以**标识符**结尾
//   只要上一行是任何标识符收尾，它就是合法的 —— 所以「上一行长什么样」
//   **根本不是一个能用的判据**。
//
// ⇒ 精确判据：把这一串 `+` 续行往**上**追到"头部行"（第一条不以 + 开头的行）。
//   若头部行以 `;` 结尾，说明**那条语句已经结束**了，它后面再出现 `+` 续行
//   就必然是悬空表达式（2026-10-20 那次的实际形态）：
//
//       rd.setAttribute('x', ...);            ← 以 ; 结束
//       + '  累计 ' + (r.has ? … : '—')       ← 悬空，求值后丢弃
//
//   而 `return gDailyAll` 不以 `;` 结尾 ⇒ 后面的 `+` 是它的一部分 ⇒ 合法。

const lines = src.split('\n');
const dangling = [];
for (let i = 0; i < lines.length; i++) {
  const t = lines[i].trim();
  if (!t.startsWith('+ ')) continue;

  // 往上追到头部行：第一条不以 '+' 开头、非空、非注释的行
  let j = i - 1;
  while (j >= 0) {
    const p = lines[j].trim();
    if (p === '' || p.startsWith('//')) { j--; continue; }
    if (p.startsWith('+ ') || p === '+' || p.endsWith('+')) { j--; continue; }  // 还在续行链里
    break;
  }
  if (j < 0) continue;

  const head = lines[j].trim();
  if (head.endsWith(';')) {
    dangling.push({ line: i + 1, head, cur: t });
  }
}

if (dangling.length) {
  fail('★ 共 ' + dangling.length + ' 处孤立的 `+` 续行（语句已用 ; 结束，续行求值后丢弃）');
  for (const d of dangling.slice(0, 5)) {
    console.log('     L' + d.line + ' 头部行（已结束）：' + d.head);
    console.log('            悬空续行：' + d.cur);
  }
} else {
  pass('② 没有孤立的 `+` 续行（头部行以 ; 结束却还有续行的情形为零）');
}

// ── ④ hover 监听器必须能拿到**当次**渲染的数据（不能是第一次的闭包快照）──
//
// 2026-10-20 同一轮里发现的第二个隐患：监听器用 `host._ctBound` 只绑一次，
// 所以它捕获的 rows/n/xAt 永远是第一次渲染那份 —— 数据刷新后 hover 读到旧值。
// 修法是把最新数据挂到 host 上，监听器通过 host 读。

if (!src.includes('host._ctData')) {
  fail('★ 没有 `host._ctData` —— hover 监听器只绑一次，会一直读到**第一次渲染**的数据（刷新后读数不更新）');
} else {
  const hasRead = /const\s+d\s*=\s*host\._ctData/.test(src);
  if (!hasRead) {
    fail('★ 有 host._ctData 但监听器没有从它读（仍然在用闭包里的旧 rows）');
  } else {
    pass('④ hover 读数从 host._ctData 取当次数据（不是闭包快照）');
  }
}

if (fails > 0) {
  console.log('\n=== 失败 ' + fails + ' 项 ===');
  process.exit(1);
}
console.log('\n=== 全部通过 ===');
