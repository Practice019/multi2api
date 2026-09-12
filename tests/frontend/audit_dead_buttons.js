// audit_dead_buttons.js —— **静态**找出"没有任何事件处理"的按钮。
//
// # 为什么需要它（与 audit_buttons.js 互补）
//
// `audit_buttons.js` 靠**行为证据**（点击后 DOM 变没变、有没有请求）判断死按钮。
// 那有两个弱点：
//   1. 慢（每个按钮要等 1.5s）
//   2. 会**误报** —— 剪贴板写入、弹窗、下载都不会改 DOM 也不发请求，
//      但它们是有效按钮（实测「复制 curl 示例」就是这样被误报的）
//
// 本脚本改用**静态判据**：按钮的 id 在源码里是否出现在事件绑定位置。
// 这能精确回答"这个按钮有没有被接上"，且瞬间完成。
//
// 判据：对每个 `id="btnXxx"`，在源码里查找 `'btnXxx'.onclick` /
// `'btnXxx'.addEventListener` / `getElementById('btnXxx')` 后接绑定。
// 注意排除：纯导航按钮（data-nav 由委托处理）、submit 表单按钮等。
'use strict';
const fs = require('fs');
const path = require('path');

const REPO = path.resolve(__dirname, '..', '..');
// 可用 AUDIT_WEBUI 指向别处的 webui.html —— 变异测试需要它指向一次性副本。
// （我第一版漏了这行，于是"验证扫描器能抓出缺陷"时一直在扫真实仓库，
//   得到的"未命中"是假结论。测工具本身时，先确认工具读的是你以为的那个文件。）
const WEBUI = process.env.AUDIT_WEBUI || path.join(REPO, 'internal/server/webui.html');
const src = fs.readFileSync(WEBUI, 'utf8');

// 排除清单：这些按钮由**事件委托**或页面机制处理，不在按钮自己身上绑
const DELEGATED = new Set([
  'themeSwitch',       // 容器，内部按钮由委托处理
  'accts', 'jobrows', 'logrows', 'histrows', 'travelRows', 'growthRows', // 表格容器
]);

console.log('=== 静态死按钮扫描 ===\n');

// 1) 收集 HTML 里所有 `<button id="...">`
//
// ⚠ 必须排除**模板里生成的行按钮**：它们的 id 形如 id="${esc(U)}"，
// 由事件委托（#content 上的一次性监听）处理，不在按钮自己身上绑。
// 第一版没排除，报了 19 个假死按钮（签到/积分/派猫/领奖…全是表格行里的）。
const isTemplateId = (id) => /\$\{/.test(id);

const buttons = [];
for (const m of src.matchAll(/<button\s[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/button>/g)) {
  const id = m[1];
  if (isTemplateId(id)) continue;
  const text = m[2].replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
  buttons.push({ id, text });
}
console.log('HTML 里的静态按钮（排除模板行按钮）: ' + buttons.length + '\n');

// 2) 对每个 id，看源码里是否有绑定
//
// ⚠ 必须**先剥掉注释**。我第一版直接在全文上匹配，结果把注释里提到的
// `$('btnJobRefresh').onclick = ...` 也算成了绑定 —— 变异测试证实它
// 抓不出"绑定被删掉"的情况（删了绑定后注释仍在，扫描器照样报"有绑定"）。
// 这与本项目反复出现的"验证书写形式而非本体"是同一类错误。
function stripComments(s) {
  // 去 // 行注释与 /* */ 块注释。
  // 简化处理：本项目没有把 // 放进字符串里的情况（有的话宁可多报也不漏报）。
  return s
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .split('\n')
    .map(l => l.replace(/\/\/.*$/, ''))
    .join('\n');
}
const code = stripComments(src);

const dead = [];
const alive = [];
for (const b of buttons) {
  if (DELEGATED.has(b.id)) { alive.push({ ...b, how: '委托/容器' }); continue; }

  const id = b.id;
  const patterns = [
    new RegExp(`\\$\\('${id}'\\)\\.onclick`),                    // $('x').onclick
    new RegExp(`getElementById\\('${id}'\\)\\.onclick`),         // getElementById('x').onclick
    new RegExp(`\\$\\('${id}'\\)\\.addEventListener`),           // addEventListener
    new RegExp(`getElementById\\('${id}'\\)\\.addEventListener`),
  ];
  let how = null;
  for (const p of patterns) { if (p.test(code)) { how = '直接绑定'; break; } }

  // 批量绑定：id 出现在**数组字面量**里，且该数组附近有 onclick/addEventListener。
  //
  // ⚠ 两个坑，都靠变异测试才发现：
  //   1. `['"]id['"]` 无边界 → `btnJobRefresh` 是 `btnGrowthRefresh` 的子串，
  //      匹配到后者的绑定，把死按钮判成"有绑定"
  //   2. 加边界后仍命中 —— 因为 HTML 里 `id="btnJobRefresh"` **本身就是**引号
  //      包裹的 id，被当成了"数组里的 id"
  //
  // 所以必须：先剥掉 HTML（只看 JS），再要求 id 是**单引号**包裹
  //（JS 数组用单引号；HTML 属性用双引号，天然区分）。
  if (!how) {
    const jsOnly = code.replace(/<[^>]*>/g, '\n');   // 去掉所有 HTML 标签
    const re = new RegExp(`'${id}'`);
    const m = re.exec(jsOnly);
    if (m) {
      const ctx = jsOnly.slice(Math.max(0, m.index - 400), m.index + 600);
      if (/onclick|addEventListener/.test(ctx)) how = '批量绑定';
    }
  }

  if (how) alive.push({ ...b, how });
  else dead.push(b);
}

console.log('=== 有绑定的按钮 ===');
alive.forEach(b => console.log('  ✓ #' + b.id.padEnd(22) + ' "' + b.text.slice(0, 16) + '"  ' + b.how));
console.log('');
console.log('=== ⚠ 没有任何绑定的按钮（点了没反应）===');
if (!dead.length) console.log('  （无）');
dead.forEach(b => console.log('  ✗ #' + b.id.padEnd(22) + ' "' + b.text.slice(0, 24) + '"'));

console.log('');
console.log('合计: ' + alive.length + ' 个有绑定, ' + dead.length + ' 个疑似死按钮');
process.exit(dead.length ? 1 : 0);
