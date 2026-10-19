
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let logEntries = [];
const els = {};
function mkEl(id){ return { id, innerHTML:'', textContent:'', hidden:true, className:'', querySelector:()=>null }; }
els.logrows = mkEl('logrows');
const $ = id => els[id] || (els[id] = mkEl(id));

function normalizeDesc(items) {
    return (items || []).slice().sort((a, b) => {
      const ta = new Date(a.at).getTime(), tb = new Date(b.at).getTime();
      if (ta !== tb) return tb - ta;
      return (b.seq || 0) - (a.seq || 0);
    });
  }
function mergeLogEntries(incoming, cap) {
    const bySeq = new Map();
    for (const e of logEntries) bySeq.set(e.seq, e);
    for (const e of (incoming || [])) bySeq.set(e.seq, e);
    logEntries = normalizeDesc(Array.from(bySeq.values())).slice(0, cap);
  }
function tokPerSec(tokens, totalMs) {
    if (tokens < 0) return '-';
    if (!totalMs || totalMs <= 0) return '0.0';
    return (tokens / (totalMs / 1000)).toFixed(1);
  }
function logPlaceholder(text, cls) {
    return `<div class="log-empty ${cls || ''}">${esc(text)}</div>`;
  }
function renderLogs(items) {
    const host = $('logrows');
    if (!host) return;
    const list = normalizeDesc(items).slice(0, 300);
    // ④ 空态必须显式画出来，不能 return 留着上一屏的内容
    if (!list.length) { host.innerHTML = logPlaceholder('暂无请求'); return; }
    host.innerHTML = list.map(e => {
      const st = e.status >= 200 && e.status < 300 ? 'ok' : e.status >= 400 ? 'err' : 'warn';
      // ② 数值列右对齐（.r）
      // ③ 无值用 `<span class="log-none">—</span>`：占位符画出来但比正文淡一档 ——
      //    参考项目的原话"空着会被当成渲染缺失"。破折号用全角「—」与项目其它面板一致。
      const dash = '<span class="log-none">—</span>';
      const secs = new Date(e.at).toLocaleTimeString('zh-CN', { hour12: false });
      return `<div class="log-row${e.status >= 400 ? ' failed' : ''}">
        <span class="mono log-none">${e.seq}</span>
        <span class="log-none">${esc(secs)}</span>
        <span class="mono log-tr" title="${esc(e.model || '')}">${esc(e.model || '—')}</span>
        <span class="log-tr" title="${esc(e.mode || '')}">${esc(e.mode || '—')}</span>
        <span><span class="pill ${st}">${e.status}</span></span>
        <span class="mono log-none log-tr" title="${esc(e.uid || '')}">${esc(e.uid || '—')}</span>
        <span class="mono r">${e.ttfb_ms ? e.ttfb_ms + 'ms' : dash}</span>
        <span class="mono r">${e.tokens >= 0 ? e.tokens : dash}</span>
        <span class="mono r">${tokPerSec(e.tokens, e.total_ms)}</span>
        <span class="mono r">${e.total_ms}ms</span>
      </div>`;
    }).join('');
  }

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// 从渲染出的 HTML 里按行取出 seq，用于断言视觉顺序。
//
// ⚠ 2026-10-19 产品把日志行从 <tr><td> 换成 <div class="log-row"> 的 grid 行
//   （webui.html:9350；列宽由 .log-head/.log-row 共用的 grid-template-columns
//   定义，DOM 顺序即视觉列顺序）。所以"第几列"现在由**行内顶层 span 的顺序**
//   决定，本函数取每行的**第一个单元格**（seq 列）来还原视觉顺序。
//
// 判据一个字没动：顶行必须是最新、整列必须严格递减。改的只是"按什么形状解析"。
//
// ⚠ 解析不出来时记 NaN，而不是把这一行悄悄丢掉：上一版按 <td class="mono dim">
//   解析，换成 div 后一条都匹配不上却返回空数组 —— 失败信息只剩 "seq=undefined"，
//   看不出"渲染了几行、少了哪行"。NaN 让行数如实暴露出来。
function renderedSeqs() {
  const rows = $('logrows').innerHTML.split('<div class="log-row').slice(1);
  const out = [];
  for (const r of rows) {
    // 行的开标签之后第一个 span 就是 seq 列（.log-row 的 DOM 顺序 = 列顺序）
    const m = /^[^>]*>\s*<span[^>]*>([\s\S]*?)<\/span>/.exec(r);
    const txt = m ? m[1].replace(/<[^>]+>/g, '').trim() : '';
    out.push(/^\d+$/.test(txt) ? Number(txt) : NaN);
  }
  return out;
}

const t = s => new Date('2026-09-11T10:00:' + String(s).padStart(2,'0') + 'Z').toISOString();

console.log('\n[1] normalizeDesc：时间是倒序，最上面是最新');
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

console.log('\n[2] 同一时刻按 seq 倒序（顺序稳定可复现）');
{
  const same = [
    { seq: 5, at: t(10) }, { seq: 9, at: t(10) }, { seq: 7, at: t(10) },
  ];
  const got = normalizeDesc(same).map(e => e.seq);
  ok(JSON.stringify(got) === JSON.stringify([9,7,5]), '同时刻按 seq 倒序（' + got.join(',') + '）');
}

console.log('\n[3] renderLogs 顶部就是最新（契约的直接验收）');
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

console.log('\n[4] 历史视图（倒序输入）也得到同一顺序');
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

console.log('\n[5] 增量去重：同一条被重复送达不会渲染两行');
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

console.log('\n[6] 缓冲上限：不无限增长');
{
  logEntries = [];
  const many = [];
  for (let i = 1; i <= 500; i++) many.push({ seq: i, at: t(i % 60), status: 200 });
  mergeLogEntries(many, 300);
  ok(logEntries.length === 300, '上限 300 生效，实际=' + logEntries.length);
  // ⚠ 这里原先是 logEntries[0].seq === logEntries[0].seq —— 自比恒真，是一条
  //   **假绿**：产品里删掉任何一行它都照样 PASS，等于没有判据。改成真正要守的
  //   性质：截断必须发生在排序**之后** —— 留下的 300 条都不早于被丢掉的任何一条
  //   （去掉 normalizeDesc 或把 slice 挪到排序之前，这条立刻红）。
  const keptSeqs = new Set(logEntries.map(e => e.seq));
  const dropped = many.filter(e => !keptSeqs.has(e.seq));
  const whenOf = e => new Date(e.at).getTime();
  ok(dropped.length === 200, '被截断的条数=200，实际=' + dropped.length);
  ok(dropped.every(d => logEntries.every(k =>
    whenOf(k) > whenOf(d) || (whenOf(k) === whenOf(d) && k.seq >= d.seq)
  )), '留下的 300 条都不早于被丢弃的 200 条（截断在排序之后，顶部仍是最新）');
}

console.log('\n[7] 静态校验：旧的顺序处理方式已清除');
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
    }).join('\n');
  ok(logRenderers.length > 0, '抽到了日志渲染函数');
  ok(!logRenderers.includes('insertAdjacentHTML'), '日志渲染不用 insertAdjacentHTML 追加（顺序不依赖插入位置）');
  ok(!html.includes('r.items.slice().reverse()'), '历史视图不再自行 reverse');
}

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
