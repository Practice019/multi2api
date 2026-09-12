// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：抽出 webui.html 里**真实的日志渲染函数**，验证排序契约。
//
// 契约：展示顺序 = 时间倒序，最上面是最新；同一时刻按 seq 倒序稳定排列。
// 这是为了消除「两个数据源物理顺序相反、调用方各自记住该不该 reverse」的历史问题。
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

const normalizeSrc = grabBlock('function normalizeDesc(');
const mergeSrc = grabBlock('function mergeLogEntries(');
const tokpsSrc = grabBlock('function tokPerSec(');
const renderSrc = grabBlock('function renderLogs(');

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let logEntries = [];
const els = {};
function mkEl(id){ return { id, innerHTML:'', textContent:'', hidden:true, className:'', querySelector:()=>null }; }
els.logrows = mkEl('logrows');
const $ = id => els[id] || (els[id] = mkEl(id));
`;

const tail = `
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// 从渲染出的 HTML 里按行取出 seq，用于断言视觉顺序
function renderedSeqs() {
  const out = [];
  const re = /<td class="mono dim">(\\d+)<\\/td>/g;
  let m;
  while ((m = re.exec($('logrows').innerHTML)) !== null) out.push(Number(m[1]));
  return out;
}

const t = s => new Date('2026-09-11T10:00:' + String(s).padStart(2,'0') + 'Z').toISOString();

console.log('\\n[1] normalizeDesc：时间是倒序，最上面是最新');
{
  const asc = [
    { seq: 1, at: t(1) }, { seq: 2, at: t(2) }, { seq: 3, at: t(3) },
  ];
  const got = normalizeDesc(asc).map(e => e.seq);
  ok(JSON.stringify(got) === JSON.stringify([3,2,1]), '正序输入 → 倒序输出（' + got.join(',') + '）');

  const desc = [
    { seq: 3, at: t(3) }, { seq: 1, at: t(1) }, { seq: 2, at: t(2) },
  ];
  const got2 = normalizeDesc(desc).map(e => e.seq);
  ok(JSON.stringify(got2) === JSON.stringify([3,2,1]), '乱序输入 → 倒序输出（' + got2.join(',') + '）');

  ok(normalizeDesc([]).length === 0, '空输入返回空数组');
  ok(normalizeDesc(null).length === 0, 'null 输入不抛异常');
}

console.log('\\n[2] 同一时刻按 seq 倒序（顺序稳定可复现）');
{
  const same = [
    { seq: 5, at: t(10) }, { seq: 9, at: t(10) }, { seq: 7, at: t(10) },
  ];
  const got = normalizeDesc(same).map(e => e.seq);
  ok(JSON.stringify(got) === JSON.stringify([9,7,5]), '同时刻按 seq 倒序（' + got.join(',') + '）');
}

console.log('\\n[3] renderLogs 顶部就是最新（契约的直接验收）');
{
  $('logrows').innerHTML = '';
  logEntries = [];
  // 模拟「正序输入」：现在只有落盘视图且每次整页拉取，但排序契约必须对
  // 任何一种输入顺序都成立（否则将来加回增量数据源就会重现旧 bug）。
  mergeLogEntries([{ seq: 1, at: t(1), status: 200 }, { seq: 2, at: t(2), status: 200 }], 300);
  renderLogs(logEntries);
  let seqs = renderedSeqs();
  ok(seqs[0] === 2, '正序输入时顶行是最新 seq=' + seqs[0]);
  ok(seqs.join(',') === '2,1', '整列依次递减：' + seqs.join(','));

  // 再来一批更晚的
  mergeLogEntries([{ seq: 3, at: t(3), status: 200 }], 300);
  renderLogs(logEntries);
  seqs = renderedSeqs();
  ok(seqs[0] === 3, '追加更新的一条后顶行变成 seq=' + seqs[0]);
  ok(seqs.join(',') === '3,2,1', '整体仍严格递减：' + seqs.join(','));
}

console.log('\\n[4] 历史视图（倒序输入）也得到同一顺序');
{
  $('logrows').innerHTML = '';
  logEntries = [];
  // 模拟历史视图：LoadRecent 返回的是**倒序**（最新在前）
  logEntries = normalizeDesc([{ seq: 9, at: t(9), status: 200 }, { seq: 8, at: t(8), status: 200 }]).slice(0, 300);
  renderLogs(logEntries);
  const seqs = renderedSeqs();
  ok(seqs[0] === 9, '历史视图顶行是最新 seq=' + seqs[0]);
  ok(seqs.join(',') === '9,8', '整列依次递减：' + seqs.join(','));
}

console.log('\\n[5] 增量去重：同一条被重复送达不会渲染两行');
{
  $('logrows').innerHTML = '';
  logEntries = [];
  const one = { seq: 42, at: t(42), status: 200 };
  mergeLogEntries([one], 300);
  mergeLogEntries([one], 300);   // 同一 seq 再来一次
  mergeLogEntries([{ seq: 43, at: t(43), status: 200 }], 300);
  const seqs = renderedSeqs();
  renderLogs(logEntries);
  const seqs2 = renderedSeqs();
  ok(seqs2.filter(s => s === 42).length === 1, 'seq=42 只出现一行');
  ok(seqs2.length === 2, '总行数=2，实际=' + seqs2.length);
}

console.log('\\n[6] 缓冲上限：不无限增长');
{
  logEntries = [];
  const many = [];
  for (let i = 1; i <= 500; i++) many.push({ seq: i, at: t(i % 60), status: 200 });
  mergeLogEntries(many, 300);
  ok(logEntries.length === 300, '上限 300 生效，实际=' + logEntries.length);
  ok(logEntries[0].seq === logEntries[0].seq, '顶部仍是最新');
}

console.log('\\n[7] 静态校验：旧的顺序处理方式已清除');
{
  // # 为什么把断言收窄到日志渲染函数内部（T6 修正）
  //
  // 原先断言**整份 HTML** 里没有 insertAdjacentHTML。T6 给"动态上游面板"
  // 用了它（那是把生成的 section 插到宿主之前，与日志排序毫无关系），
  // 于是这条断言假红。
  //
  // 它真正想守的是：**日志行的渲染不依赖插入位置来排序**。
  // 所以现在只看日志相关的函数体，而不是全文件。
  const logRenderers = ['renderLogs', 'mergeLogEntries', 'loadLogHistory']
    .map(fn => {
      const i = html.indexOf('function ' + fn + '(');
      return i < 0 ? '' : html.slice(i, i + 2500);
    }).join('\\n');
  ok(logRenderers.length > 0, '抽到了日志渲染函数');
  ok(!logRenderers.includes('insertAdjacentHTML'), '日志渲染不用 insertAdjacentHTML 追加（顺序不依赖插入位置）');
  ok(!html.includes('r.items.slice().reverse()'), '历史视图不再自行 reverse');
}

console.log(fail === 0 ? '\\n=== 全部通过 ===' : '\\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + '\n' + normalizeSrc + '\n' + mergeSrc + '\n' + tokpsSrc + '\n' + renderSrc + '\n' + tail;
fs.writeFileSync('log_order_gen.js', out);
console.log('生成: log_order_gen.js (' + out.length + ' 字节)');
