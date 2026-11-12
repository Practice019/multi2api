
const fs = require('fs');
const html = require('./lib_frontend_source').FRONTEND;
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let logEntries = [];
// logStatus：状态码筛选（null = 全部）。真实产品里它是 webui.html 的模块级 let，
// 这里按同一语义复刻一份 —— 抽出的函数读的就是这个绑定。
let logStatus = null;
// logPage.total：卡片条尾的「共 M 条」读它。本套件只关心排序契约，给 0 即可。
const logPage = { total: 0 };
const els = {};
function mkEl(id){ return { id, innerHTML:'', textContent:'', hidden:true, className:'', style:{}, querySelector:()=>null }; }
els.logrows = mkEl('logrows');
els.logStatusBar = mkEl('logStatusBar');
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
function tokPerSec(outTokens, totalMs, ttfbMs, mode) {
    if (mode && mode !== 'stream') return '-';
    if (!outTokens || outTokens <= 0) return '-';
    if (!totalMs || totalMs <= 0) return '0.0';
    const genMs = totalMs - (ttfbMs > 0 ? ttfbMs : 0); // 纯生成阶段 = DSH 的 decodeMs
    if (genMs <= 0) return '-';
    return fmtTps(outTokens / (genMs / 1000));
  }
function logPlaceholder(text, cls) {
    return `<div class="log-empty ${cls || ''}">${esc(text)}</div>`;
  }
function logStatusOf(e) {
    const n = Number(e && e.status);
    return isFinite(n) && n > 0 ? n : null;
  }
function renderLogStatusBar(serverCounts, serverTotalAll, serverTotalFiltered, pageLen) {
    const box = $('logStatusBar');
    if (!box) return;
    const cnt = (k) => {
      const v = (serverCounts || {})[String(k)];
      const n = Number(v);
      return isFinite(n) && n > 0 ? n : 0;
    };
    const codes = Object.keys(serverCounts || {}).map(Number).filter(n => isFinite(n) && n > 0).sort((a, b) => a - b);
    if (!codes.length && !serverTotalAll) { hideLogStatusBar(); return; }
    const chip = (val, label, num, on, hint) =>
      `<button type="button" class="chip${on ? ' on' : ''}" data-logst="${val}"
               aria-pressed="${on ? 'true' : 'false'}" title="${esc(hint)}">${label}<span>${num}</span></button>`;
    const cards = [
      chip('', '全部', serverTotalAll, logStatus === null, '不筛选，全库 ' + serverTotalAll + ' 条'),
    ].concat(codes.map(c => chip(String(c), String(c), cnt(c), String(logStatus) === String(c),
      '只看全库状态码 ' + c + ' 的 ' + cnt(c) + ' 条')));
    box.innerHTML = cards.join('') +
      `<span class="dim" style="font-size:11px;margin-left:4px">`
      + `本页 ${pageLen} 条 · 当前筛选 ${serverTotalFiltered} 条 · 全库 ${serverTotalAll} 条</span>`;
    box.style.display = 'flex';
  }
function hideLogStatusBar() {
    const box = $('logStatusBar');
    if (!box) return;
    box.innerHTML = '';
    box.style.display = 'none';
  }
function renderLogs(items) {
    const host = $('logrows');
    if (!host) return;
    const list = normalizeDesc(items).slice(0, 300);

    // 统计当前页的状态码分布（未筛选的全集）。
    const counts = new Map();
    for (const e of list) {
      const code = logStatusOf(e);
      if (code !== null) counts.set(code, (counts.get(code) || 0) + 1);
    }

    // 筛选值若已不在这一页 ⇒ 自动放开并告知。**先判再画卡片**，
    // 否则会出现"卡片显示全部、表格却是空的"这种一闪而过的自相矛盾。
    let autoCleared = null;
    if (logStatus !== null && !counts.has(logStatus)) {
      autoCleared = logStatus;
      logStatus = null;
    }

    const shown = logStatus === null ? list : list.filter(e => logStatusOf(e) === logStatus);

    // ④ 空态必须显式画出来，不能 return 留着上一屏的内容。
    // ⚠ 自动放开筛选这条分支下 list 非空、shown 为空 —— 不是"暂无请求"，
    //   是"这一页没有该状态码"，两者必须用不同的字说，否则等于骗人。
    if (!list.length) {
      // ★ T-396a：清屏路径必须重置签名，否则清屏后画不出来。
      lastLogSig = '';
      host.innerHTML = logPlaceholder('暂无请求');
      hideLogStatusBar();
      return;
    }
    // ★ T-396a 签名短路（"不闪"的底线，比完整行 diff 便宜得多）：
    // 轮询每 30 秒跑一次，而日志往往没变。只要"没变化就一个 DOM 都不碰"，
    // 90% 的闪动就消失了。完整行 diff 留给 396b。
    //   ⚠ 签名用 seq 列表（不是下标）：下标会随新增条目整体位移 ⇒ 每次轮询都"变了"。
    //   ⚠ 换了 logStatus 之后条目集合不同 ⇒ 签名自然变 ⇒ 正常重绘。
    const sig = shown.map(e => String(e.seq)).join(',');
    if (sig === lastLogSig) return;   // ★ 一个 DOM 都不碰
    lastLogSig = sig;
    renderLogStatusBar(serverCounts, serverTotalAll, serverTotalFiltered, list.length);
    if (!shown.length) {
      host.innerHTML = logPlaceholder(
        '本页 ' + list.length + ' 条里没有状态码 ' + autoCleared + '（已取消该筛选；翻页可看其它页）');
      lastLogSig = '';
      return;
    }
    // ★ T-400：行生成收敛到**唯一**的 htmlToRow（本仓踩过"两处模板各写一遍、改一处漏一处"）。
    // ★ T-400：**行级 diff** —— 只增删变化的行，其余 DOM 节点原样保留。
    // 为什么：T-396a 的签名短路只救了"列表完全没变"。只要有新日志进来，
    // 旧实现就整块重建 15 行 × 10 列 ⇒ 重排 + 重绘 + 滚动位置跳（用户原话"每次都会把这个页面进行重构，然后就会卡顿"）。
    // 对齐用 **seq**（单调序号、唯一稳定），不用下标 —— 下标会随"新增一条"整体位移。
    const cur = Array.from(host.querySelectorAll('[data-logseq]'));
    const curSeq = cur.map(el => el.dataset.logseq);
    const wantSeq = shown.map(e => String(e.seq == null ? '' : e.seq));
    const addN = wantSeq.length - curSeq.length;      // 顶部多出来的条数
    // ★ T-402b：判据修正 —— 位移量必须用**锚**算，不能用长度差。
    //
    // # 为什么"长度差"算不出位移（T-401/T-402 两轮实测抓到的第二层缺陷）
    //   日志列表是**固定页大小**（limit=15）：新日志进 1 条 ⇒ 尾部同时挤出 1 条
    //   ⇒ wantSeq.length === curSeq.length ⇒ **addN 恒为 0** ⇒ `addN > 0` 永远为假
    //   ⇒ 情形 A 从来没进过 ⇒ 行 diff 一次都没用上
    //   （实测证据两轮一致：新日志确实进来、行数仍是 15，但旧行 __probe 消失 = 被重建）
    //
    // # 正确做法：拿"旧的第一条"当锚
    //   newN = curSeq[0] 在 wantSeq 里的下标 = **顶部新增的条数**
    //   （旧的第一条在分页窗口里不会消失，除非它被尾部挤出去了 —— 那就走情形 B，见下）
    //   然后要求整段对齐：curSeq[i] === wantSeq[newN + i] 全部成立；
    //   **不要求**尾部相同（尾部被挤出的那些本来就不该再画）。
    let newN = -1;
    if (curSeq.length > 0) newN = wantSeq.indexOf(curSeq[0]);
    // ⚠ T-402 实测第三层：对齐比较必须**按较短的那段**比。
    //   直接 `curSeq.every((s,i) => wantSeq[newN+i] === s)` 时，右端会越界
    //   （wantSeq[15] === undefined）⇒ 在"顶部进 1 条 + 尾部挤 1 条"这个最常见的情形下
    //   **永远为假** ⇒ 情形 A 还是不进（实测：15 行全部 __probe 丢失 = 又被全量重建）。
    //   ⇒ 只比 curSeq 的前 cmpN 项与 wantSeq[newN …] 对齐；被挤出窗口的那几项本来就不该再画。
    const cmpN = Math.min(curSeq.length, Math.max(0, wantSeq.length - newN));
    const aligned = cmpN > 0 && curSeq.slice(0, cmpN).every((s, i) => wantSeq[newN + i] === s);
    if (newN > 0 && aligned) {
      const sc = logScrollEl();
      const nearTop = sc.scrollTop <= 2;     // 贴顶就保持贴顶（否则用户会被"推下去"）
      const rowH = host.firstElementChild ? host.firstElementChild.offsetHeight : 18;
      const frag = document.createDocumentFragment();
      for (let i = newN - 1; i >= 0; i--) {  // 倒序 append ⇒ 最终顺序与 shown 一致
        frag.appendChild(htmlToRow(shown[i]));
      }
      host.insertBefore(frag, host.firstChild);
      // ★ T-402：prepend 之后必须**删掉尾部被挤出窗口的节点**，
      //   否则表格会变成 15+newN 行（比不做 diff 更糟：行数悄悄膨胀）。
      const all = Array.from(host.querySelectorAll('[data-logseq]'));
      for (let i = all.length - 1; i >= wantSeq.length; i--) all[i].remove();
      if (!nearTop) sc.scrollTop += newN * rowH;   // ★ 补偿：正在读的那条停在原地
      return;
    }
    // 情形 B：其余情况（尾部变了 / 筛选变了 / 翻页）⇒ 一次性替换（仍比逐条 append 快）
    host.replaceChildren(logRowsFragment(shown));
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
