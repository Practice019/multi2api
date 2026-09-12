// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：调用统计的「落盘占用」与窗口文案。
//
// 覆盖本轮两点要求：
//  1. 落盘占用只显示占用量，不显示上限（旧实现渲染 "X / 8.0 MiB"）
//  2. 窗口文案不再回显「从某时刻起」，只报总条数
//
// 顺带锁死两条不能退化的边界：
//  - 内存来源仍保留 "held/capacity" 的分母语义（那里的分母是真实缓冲占用率，有意义）
//  - 落盘来源但 file 缺失时不能崩、也不能渲染出 undefined/NaN
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(path, 'utf8');

function grabBlock(startMarker) {
  const start = html.indexOf(startMarker);
  if (start < 0) throw new Error('找不到: ' + startMarker);
  let i = html.indexOf('{', start), depth = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}') { depth--; if (depth === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + startMarker);
}

// renderStats 现在依赖一组数字格式化辅助函数（Task B1 引入的防御式格式化）。
// 必须把它们一并抽出来，否则生成物在 renderStats 里查不到符号 —— 这是真实发生过的
// 失败：新增 num() 后本测试报 `ReferenceError: num is not defined`。
// 教训：从真实源码抽函数时，要连**它调用的其它函数**一起抽，不能只抽入口。
const pieces = [
  grabBlock('function num('),
  grabBlock('function numNonNeg('),
  grabBlock('function fmtCount('),
  grabBlock('function fmtCredit('),
  grabBlock('function fmtTokens('),
  grabBlock('function fmtRate('),
  grabBlock('function fmtBytes('),
  grabBlock('function renderStatsModels('),
  grabBlock('function renderStats('),
];

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
// 模块级常量：renderStats 用到 NODATA（"无数据"占位）。从源码取，避免两处漂移。
const NODATA = /const NODATA = '([^']*)'/.exec(html)[1];
const els = {};
// style 必须存在：新的 renderStatsModels 会设 wrap.style.display
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, style: {} }; }
for (const id of ['statsWindow','statsCards','statsModels','statsModelsWrap','statsCatalogHint']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
// esc 在源码里是 const 箭头函数（不是 function 声明），grabBlock 抽不到，
// 这里照抄实现（它只影响转义，与本次要验证的渲染逻辑无关）。
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
function resetEls() { for (const k of Object.keys(els)) { const e = els[k]; e.textContent=''; e.innerHTML=''; } }
// 从渲染出的卡片 HTML 里取某个 key 对应的值（卡片结构：k 一行、v 一行）
function cardValue(key) {
  const h = $('statsCards').innerHTML;
  const i = h.indexOf('>' + key + '<');
  if (i < 0) return null;
  const m = /<div class="v[^"]*">([^<]*)<\\/div>/.exec(h.slice(i));
  return m ? m[1] : null;
}
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const base = {
  total: 743, ok: 700, fail: 43, avg_ttfb_ms: 120, avg_total_ms: 900, tokens: 12345,
  source: 'file', window_from: '2026-09-11T06:59:39+08:00',
  file: { size: 262144, max_bytes: 8388608 },
  capacity: 500, held: 120, by_model: { 'deepseek-v4-flash': 12 },
};

// ---------- 1. 落盘占用只显示占用量 ----------
console.log('\n[1] 落盘占用只显示占用量');
resetEls(); renderStats({ ...base });
const used = cardValue('落盘占用');
ok(used === '256.0 KB', '只渲染占用量 256.0 KB（实际：' + used + '）');
ok(!String(used).includes('/'), '不含分隔斜杠（不再出现「X / Y」）');
ok(!String(used).includes('MiB'), '不出现上限 8.0 MiB');
ok(!$('statsCards').innerHTML.includes('8388608'), '未把 max_bytes 的字节数渲染出来');

// 换一个上限值，输出必须完全不变 —— 证明上限真的不参与渲染
resetEls(); renderStats({ ...base, file: { size: 262144, max_bytes: 1 } });
ok(cardValue('落盘占用') === '256.0 KB', '改动 max_bytes 不影响输出（上限确已脱离渲染）');

// 未达 1 MiB / 超过 1 MiB 两种量级
resetEls(); renderStats({ ...base, file: { size: 512, max_bytes: 8388608 } });
ok(cardValue('落盘占用') === '512 B', '小于 1KB 时按字节显示');
resetEls(); renderStats({ ...base, file: { size: 5 * 1024 * 1024, max_bytes: 8388608 } });
ok(cardValue('落盘占用') === '5.0 MiB', '大于 1MiB 时按 MiB 显示');

// ---------- 2. 窗口文案不再带起始时刻 ----------
console.log('\n[2] 窗口文案只报条数');
resetEls(); renderStats({ ...base });
const w = $('statsWindow').textContent;
ok(w === '（全部落盘历史，共 743 条）', '文案为「（全部落盘历史，共 743 条）」（实际：' + w + '）');
ok(!w.includes('06:59:39') && !w.includes('2026'), '不再回显起始日期/时刻');
ok(!w.includes('起'), '不再出现「…起」这种以某时刻为边界的措辞');

// window_from 有没有都不影响该分支的文案
resetEls(); renderStats({ ...base, window_from: '' });
ok($('statsWindow').textContent === '（全部落盘历史，共 743 条）', 'window_from 为空时文案不变');

// ---------- 3. 边界：内存来源保留分母，file 缺失不崩 ----------
console.log('\n[3] 边界情况');
resetEls(); renderStats({ ...base, source: 'memory' });
ok(cardValue('缓冲占用') === '120/500', '内存来源仍为 held/capacity（分母有意义，保留）');
ok($('statsWindow').textContent === '（仅内存缓冲，进程启动至今，上限 500 条）', '内存来源窗口文案未受影响');

resetEls(); renderStats({ ...base, file: undefined });
const noFile = cardValue('落盘占用');
ok(noFile !== null && !String(noFile).includes('undefined') && !String(noFile).includes('NaN'),
  'source=file 但 file 缺失时回落为 held/capacity 而非 undefined（实际：' + noFile + '）');

// 空日志
resetEls(); renderStats({ ...base, total: 0, ok: 0, fail: 0, window_from: '' });
ok($('statsWindow').textContent === '（落盘日志为空）', '总数为 0 时提示「落盘日志为空」');

// 窗口文案里的数字也必须防御式格式化。
//
// 回归背景：该行原本是裸插值（模板字符串里直接写 s.total），后端若给出非数值
// （对象/数组/垃圾串），非空值会被串成 [object Object] 直接显示给用户。
// 实测（穷举 15 组病态 payload）抓出：卡片区全部干净，唯独窗口文案漏了。
// 现与卡片区共用 fmtCount。
console.log('\n[3.5] 窗口文案的数字也要防御式');
for (const [name, val] of [
  ['对象', {}], ['数组', [1, 2]], ['垃圾字符串', 'abc'],
  ['NaN', NaN], ['undefined', undefined], ['Infinity', Infinity],
]) {
  resetEls(); renderStats({ ...base, total: val, ok: val });
  const t = $('statsWindow').textContent;
  ok(!/\[object|NaN|undefined|Infinity/.test(t),
    'total=' + name + ' 时窗口文案干净（实际：' + t + '）');
  const cards = $('statsCards').innerHTML;
  ok(!/\[object|NaN|undefined|Infinity/.test(cards),
    'total=' + name + ' 时卡片区干净');
}
// 正常值仍要如实显示
resetEls(); renderStats({ ...base, total: 743 });
ok($('statsWindow').textContent === '（全部落盘历史，共 743 条）', '正常数值仍如实显示');

// 其它卡片不受影响
resetEls(); renderStats({ ...base });
ok(cardValue('总请求') === '743', '总请求仍正常');
ok(cardValue('成功率') === '94%', '成功率仍正常（700/743≈94%）');

// ---------- 4. 静态：不再有引用 max_bytes 的渲染代码 ----------
console.log('\n[4] 静态检查');
const statsFn = (html.slice(html.indexOf('function renderStats('), html.indexOf('function fmtBytes(')) );
ok(!/max_bytes/.test(statsFn), 'renderStats 内已无 max_bytes 引用');
ok(!/toLocaleString/.test(statsFn), 'renderStats 内已无日期格式化（起点回显已移除）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('stats_disk_gen.js', out);
console.log('生成: stats_disk_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
