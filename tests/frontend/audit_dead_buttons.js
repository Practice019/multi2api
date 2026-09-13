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
const cpSelf = require('child_process');   // 自检用：把自己当子进程跑，配合 AUDIT_WEBUI 换输入

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

// ⚠⚠ 必须用**剥过注释的** code，而不是原始 src。
//
// 这是本扫描器长期误报的根因：按钮收集在第 1 步用 src，绑定判定在第 2 步用 code，
// 两处输入不一致。于是**被注释掉的按钮**（例如 T3 移入注释说明的
// `<button id="btnAllCheckin">`）会被当成"页面上的真实按钮"收进来，
// 然后在剥注释后的 code 里当然找不到绑定 → 报成死按钮。
//
// 实测：`btnAllCheckin` 只出现在 webui.html 的 L526–544 注释块里
// （T3 已把它从 DOM 移除，改为 manifest 驱动的 `<span id="allDailyActs">`）。
// 剥注释后全文 0 次出现，它根本不是页面上的按钮。
//
// 这与文件末尾 stripComments 注释里记的那类错误是同一个：
// "验证书写形式而非本体" —— 这里连"本体是否存在"都没先确认。
//
// 判据修正：**注释不是 DOM**。注释里的 `<button>` 不渲染、不可点，
// 既不该被算作死按钮，也不该被算作活按钮 —— 它不存在。
const stripCommentsEarly = (s) => {
  const lf = s.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  return lf
    .replace(/<!--[\s\S]*?-->/g, '')          // HTML 注释
    .replace(/\/\*[\s\S]*?\*\//g, '')          // 块注释
    .split('\n')
    .map(l => l.replace(/\/\/.*$/, ''))        // 行注释
    .join('\n');
};
const markup = stripCommentsEarly(src);

const buttons = [];
const commentedOut = [];
for (const m of markup.matchAll(/<button\s[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/button>/g)) {
  const id = m[1];
  if (isTemplateId(id)) continue;
  const text = m[2].replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
  buttons.push({ id, text });
}

// 透明化：被注释掉的按钮单独列出来，**不判死**。
// 不列的话，"这个按钮怎么没被检查"就会变成下一个需要考古的谜题。
for (const m of src.matchAll(/<button\s[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/button>/g)) {
  const id = m[1];
  if (isTemplateId(id)) continue;
  if (markup.includes(`id="${id}"`)) continue;
  const text = m[2].replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
  commentedOut.push({ id, text });
}
console.log('HTML 里的静态按钮（排除模板行按钮 + 注释块）: ' + buttons.length + '\n');
if (commentedOut.length) {
  console.log('（以下按钮只存在于注释中，不渲染、不可点，故不参与判定）');
  commentedOut.forEach(b => console.log('  · #' + b.id + ' "' + b.text + '"'));
  console.log('');
}

// 2) 对每个 id，看源码里是否有绑定
//
// ⚠ 必须**先剥掉注释**。我第一版直接在全文上匹配，结果把注释里提到的
// `$('btnJobRefresh').onclick = ...` 也算成了绑定 —— 变异测试证实它
// 抓不出"绑定被删掉"的情况（删了绑定后注释仍在，扫描器照样报"有绑定"）。
// 这与本项目反复出现的"验证书写形式而非本体"是同一类错误。
function stripComments(s) {
  // ⚠ 先把 CRLF 归一化成 LF —— 否则 `\/\/.*$` **完全失效**。
  //
  // 行尾是 `\r` 时，正则里的 `$`（无 m 标志）匹配不上，于是整行原样保留，
  // "剥注释"变成空操作。我最初没归一化，导致注释里提到的绑定被当成真绑定，
  // 变异测试（删掉绑定）抓不出来 —— 工具本身骗了我。
  const lf = s.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  return lf
    .replace(/<!--[\s\S]*?-->/g, '')          // HTML 注释也剥（页面里有）
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

// ---------------------------------------------------------------------------
// 自检：这个扫描器必须**同时**满足两条相反的性质，否则它是没用的。
//
//   性质 A（不误报）：注释块里的 `<button>` 不能被判成死按钮。
//   性质 B（不漏报）：真正没有绑定的 `<button>` 必须被判成死按钮。
//
// 只修 A 的最省事写法是把"找不到绑定"一律放过 —— 那样性质 B 就死了，
// 工具从此永远绿。所以两条都要在本文件里构造出来实测，
// 用 AUDIT_WEBUI 指向临时副本（不动原仓库，也就不会污染别人的工作树）。
const os = require('os');
function scan(target) {
  const env = { ...process.env, AUDIT_WEBUI: target };
  const r = cpSelf.spawnSync(process.execPath, [__filename], { encoding: 'utf8', env });
  return { code: r.status, out: (r.stdout || '') + (r.stderr || '') };
}

const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'deadbtn-'));
const results = [];
function selfCheck(name, mutate, wantDead) {
  const f = path.join(tmpDir, name + '.html');
  fs.writeFileSync(f, mutate(src));
  const r = scan(f);
  const gotDead = r.code !== 0;
  const pass = gotDead === wantDead;
  results.push(pass);
  console.log((pass ? '  PASS ' : '  FAIL ') + `自检 ${name}：期望${wantDead ? '判死(exit 1)' : '不判死(exit 0)'}，实际 exit ${r.code}`);
  if (!pass) console.log(r.out.split('\n').filter(l => /✗|合计/.test(l)).map(l => '      ' + l).join('\n'));
}

// 性质 A：现有仓库（btnAllCheckin 在注释里）必须 exit 0
selfCheck('A-当前仓库-注释里的按钮不判死', s => s, false);

// 性质 B：把那个注释块**取消注释**（模拟"按钮真的回到 DOM 且没绑事件"）→ 必须判死
selfCheck('B-取消注释成真按钮-必须判死', s => s.replace(
  /<button id="btnAllCheckin">全部签到<\/button>/,
  '<span></span></button></section><section><button id="btnAllCheckin">全部签到</button>'),
  true);

// 性质 B'：造一个全新的、确实没有绑定的按钮 → 必须判死（防止"只看 btnAllCheckin"的特判）
selfCheck('B2-全新无绑定按钮-必须判死', s => s.replace(
  '<span id="allDailyActs"></span>',
  '<span id="allDailyActs"></span><button id="btnTotallyUnbound">没人绑我</button>'),
  true);

fs.rmSync(tmpDir, { recursive: true, force: true });

console.log('');
const selfFail = results.filter(r => !r).length;
console.log('自检: ' + (results.length - selfFail) + '/' + results.length + ' 通过');
process.exit(dead.length || selfFail ? 1 : 0);
