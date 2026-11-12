
const fs = require('fs');
const html = require('./lib_frontend_source').FRONTEND;
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
// logStatus / logPage.total：状态码卡片条要读的两个绑定（本套件不筛、只渲染）。
let logStatus = null;
const logPage = { total: 0 };
const els = {};
const $ = id => els[id] || (els[id] = { id, innerHTML: '', textContent: '', style: {} });

function normalizeDesc(items) {
    return (items || []).slice().sort((a, b) => {
      const ta = new Date(a.at).getTime(), tb = new Date(b.at).getTime();
      if (ta !== tb) return tb - ta;
      return (b.seq || 0) - (a.seq || 0);
    });
  }

function fmtTps(v) {
    const c = Math.max(0, v);
    return c >= 10 ? String(Math.round(c)) : String(Math.round(c * 10) / 10);
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

// ttfb_ms 默认 0：tok/s 的分母是**纯生成阶段**（total_ms − ttfb_ms），
// 下面几节写的都是"整轮耗时当分母"的算术，默认 0 才让两边对得上。
// 首字等待要不要扣掉，单独在第 [6] 节用真实数字验。
function renderOne(entry) {
  $('logrows').innerHTML = '';
  renderLogs([Object.assign({ seq: 1, at: new Date().toISOString(), model: 'm', mode: 'stream', status: 200, uid: 'u', ttfb_ms: 0 }, entry)]);
  return $('logrows').innerHTML;
}

// 取出行的**顶层 span**（状态列是 span>span 的嵌套，必须按深度切，
// 用 <span>...</span> 的非贪婪匹配会把状态列切碎、并留下半个闭合标签）。
function topSpans(inner) {
  const out = [];
  const re = /<span\b[^>]*>|<\/span>/g;
  let depth = 0, start = -1, m;
  while ((m = re.exec(inner)) !== null) {
    if (m[0].charAt(1) === '/') {
      depth--;
      if (depth < 0) throw new Error('span 多于开标签，行结构损坏');
      if (depth === 0) { out.push(inner.slice(start, m.index + m[0].length)); start = -1; }
    } else {
      if (depth === 0) start = m.index;
      depth++;
    }
  }
  if (depth !== 0) throw new Error('span 不闭合，行结构损坏');
  return out;
}

// 取出最后一行的所有单元格文本，便于按下标断言列顺序。
//
// ⚠ 2026-10-19 产品把日志行从 <tr><td> 换成 <div class="log-row"> + 10 个
//   <span>（webui.html:9350；列宽由 .log-head/.log-row 共用的
//   grid-template-columns 定义 ⇒ DOM 顺序就是视觉列顺序）。
//   所以"单元格"现在是行的**顶层 span**。判据一个字没动：第 9 格必须是
//   tok/s 的数值、第 8 格是原始 token 数、第 10 格是耗时。
function cells(h) {
  const row = /<div class="log-row[^"]*">([\s\S]*)<\/div>\s*$/.exec(h);
  if (!row) throw new Error('没有渲染出 <div class="log-row">');
  return topSpans(row[1]).map(s => s.replace(/<[^>]+>/g, '').trim());
}

// 把 grid-template-columns 的值切成轨道：按**顶层**空白切，所以 minmax(0,1.3fr)
// 内部的逗号/空格不会被当成列分隔符。
// ⚠ 若将来轨道里出现 repeat(...)，本函数会把整段 repeat 算作一条轨道（少算），
//   这条断言会假红。届时应当展开 repeat，而不是删掉断言。
function gridTracks(spec) {
  const out = [];
  let depth = 0, cur = '';
  for (const ch of spec) {
    if (ch === '(') depth++;
    else if (ch === ')') depth--;
    if (depth === 0 && /\s/.test(ch)) { if (cur) { out.push(cur); cur = ''; } continue; }
    cur += ch;
  }
  if (cur) out.push(cur);
  return out;
}

console.log('\n[1] tok/s 基本计算');
{
  const c = cells(renderOne({ tokens: 100, out_tokens: 100, total_ms: 1000 }));
  // 列顺序：# 时间 模型 模式 状态 UID TTFB tok tok/s 耗时
  ok(c.length === 10, '渲染出 10 列（新增 tok/s），实际 ' + c.length);
  // ⚠ 显示精度在 2026-10-20 照 DSH 官方的 formatTokensPerSecond 改过：
  //   **≥10 取整、<10 保留一位小数**（两套精度，不是统一 toFixed(1)）。
  //   这里原来是 '100.0'，那是旧 toFixed(1) 的期望值。
  ok(c[8] === '100', '100 tok / 1.0s → 100（≥10 取整），实际 ' + c[8]);
  ok(c[7] === '100', 'tok 列仍是原始 token 数');
  ok(c[9] === '1000ms', '耗时列未被挤掉');
}

console.log('\n[2] 边界：usage 缺失（tokens = -1）不能拿去算速率');
{
  const c = cells(renderOne({ tokens: -1, out_tokens: -1, total_ms: 1000 }));
  // ⚠ tok 列的空值占位符从 '-' 换成了全角破折号 '—'（webui.html:9348 的 dash
  //   常量，配 .log-none 弱化一档，与项目其它面板一致）。判据不变：**不能把
  //   -1 当成 token 数显示出来**；把 dash 那一行删掉会渲染出 "undefined"。
  ok(c[7] === '—', 'tok 列显示占位符 —（不是 -1），实际 ' + c[7]);
  ok(c[8] === '-', 'tok/s 列同样显示 -（不出现 -1.0 这种荒谬值），实际 ' + c[8]);
}

console.log('\n[3] 边界：total_ms = 0 不能除零');
{
  const c = cells(renderOne({ tokens: 50, out_tokens: 50, total_ms: 0 }));
  ok(c[8] === '0.0', '耗时 0 时显示 0.0（不是 Inf/NaN），实际 ' + c[8]);
}

console.log('\n[4] 小数位与四舍五入');
{
  // 这一节的**判据**是"精度规则"本身，不是某几个小数位：
  //   ≥10 取整（26.15→26、86.90→87），<10 留一位（0.33→0.3）。
  // 旧的 26.2 / 86.9 是 toFixed(1) 时代的期望值，与产品现在的规则相反。
  ok(cells(renderOne({ tokens: 68, out_tokens: 68, total_ms: 2600 }))[8] === '26', '68 tok / 2.6s = 26.15 → 26（≥10 取整）');
  ok(cells(renderOne({ tokens: 365, out_tokens: 365, total_ms: 4200 }))[8] === '87', '365 tok / 4.2s = 86.90 → 87（≥10 取整）');
  ok(cells(renderOne({ tokens: 1, out_tokens: 1, total_ms: 3000 }))[8] === '0.3', '1 tok / 3.0s = 0.33 → 0.3（<10 留一位）');
  ok(cells(renderOne({ tokens: 9, out_tokens: 9, total_ms: 1000 }))[8] === '9', '9 tok / 1.0s → 9（<10 但整数值，不补 .0）');
}

console.log('\n[5] tokens = 0 是合法值（与 -1 语义不同）');
{
  const c = cells(renderOne({ tokens: 0, total_ms: 2000 }));
  ok(c[7] === '0', 'tok 列显示 0（0 是真实值，不能像 -1 那样画占位符）');
  // ⚠ tok/s 的判据在 2026-10-20 变过：分子改成了 **out_tokens**（纯输出），
  //   而 out_tokens=0 表示上游没给 completion_tokens ⇒ 与 -1 一样是"没有数据"
  //   ⇒ 显示 -。这里原来断言 '0.0'，那是旧公式（拿 total 当分子）的期望值。
  ok(c[8] === '-', 'tok/s 在 out_tokens 缺失时显示 -（不拿 total 冒充输出），实际 ' + c[8]);
}

console.log('\n[6] 分母只算生成阶段（total_ms − ttfb_ms），不是整轮耗时');
{
  // 产品注释里那条实测对照：out=176、total=2256ms、ttfb=1262ms
  //   · 拿整轮耗时算 → 78.0（错；客户端显示的是 176）
  //   · 只算生成阶段 → 177（对）
  const c = cells(renderOne({ tokens: 2256, out_tokens: 176, total_ms: 2256, ttfb_ms: 1262 }));
  ok(c[8] === '177', '首字等待已从分母扣除（176/(2.256−1.262)≈177），实际 ' + c[8]);
  // 首字几乎等于整轮 ⇒ 生成阶段 <= 0，速度无从计算；画 0.0 会被读成"极慢"
  const c2 = cells(renderOne({ tokens: 10, out_tokens: 10, total_ms: 1000, ttfb_ms: 1000 }));
  ok(c2[8] === '-', '生成阶段 <= 0 时显示 -（不是 0.0），实际 ' + c2[8]);
  // 非流式没有"生成速度"这个概念
  const c3 = cells(renderOne({ tokens: 100, out_tokens: 100, total_ms: 1000, mode: 'sync' }));
  ok(c3[8] === '-', '非流式不显示 tok/s，实际 ' + c3[8]);
}

console.log('\n[7] 静态校验：表头 / grid 轨道 / 实际行 三处列数一致');
{
  // # 这一节原来按"日志表是 <table>"写（断言 <th>tok/s</th>、COLS.logs=10、
  //   logPlaceholder 由 rowPlaceholder + COLS.logs 推导 colspan、日志区含
  //   __COLS_LOGS__ 占位符）。2026-10-19 产品把日志区从 <table> 换成 grid：
  //   表头是 .log-head 的 10 个 span（webui.html:1563）、行是 .log-row 的 10 个
  //   span（webui.html:9350）、列宽由两者**共用**的 grid-template-columns 定义
  //   （webui.html:928），COLS.logs / __COLS_LOGS__ 作为死字段被删掉
  //   （webui.html:3028 有记录）。
  //
  //   **判据一个字没变**：表头声明的列数与真实渲染出的列数必须一致，且列数
  //   只有一个来源。只是"列数"的载体从 colspan 换成了 grid 轨道。
  const headStart = html.indexOf('<div class="log-head">');
  const headEnd = headStart < 0 ? -1 : html.indexOf('</div>', headStart);
  const headHtml = headStart < 0 ? '' : html.slice(headStart, headEnd < 0 ? html.length : headEnd);
  ok(headHtml.length > 0, '抽到了日志表头 .log-head');
  const headCells = topSpans(headHtml).map(s => s.replace(/<[^>]+>/g, '').trim());
  ok(headCells.length === 10, '表头 10 列，实际 ' + headCells.length);
  ok(headCells[8] === 'tok/s', '表头第 9 列是 tok/s，实际 ' + headCells[8]);

  // 列数的**唯一来源**：表头与数据行共用同一条 grid-template-columns 规则
  // （webui.html:929 的原话："一处定义两处用，改列宽不会只改一半"）。
  const gridRule = /\.log-head,\.log-row\{[^}]*grid-template-columns:\s*([^;]+);/.exec(html);
  ok(!!gridRule, '表头与数据行共用一套 grid-template-columns（列数只有一处定义）');
  const tracks = gridRule ? gridTracks(gridRule[1]) : [];
  ok(tracks.length === 10, 'grid 轨道 10 列，实际 ' + tracks.length);

  // 运行时那一侧：真实渲染出的一行确实也是 10 格。
  const rowCells = cells(renderOne({ tokens: 1, total_ms: 1000 }));
  ok(rowCells.length === headCells.length && rowCells.length === tracks.length,
    '表头 / grid 轨道 / 实际行 三处列数一致（表头 ' + headCells.length
    + ' / 轨道 ' + tracks.length + ' / 行 ' + rowCells.length + '）');

  // 占位块：日志区不再是表格，所以不能再走 rowPlaceholder 的 <tr colspan>
  // —— <tr> 塞进 div 容器会被 HTML 解析器 foster-parent 掉，页面上就是
  // "占位句不见了"，而那正是最需要它出现的时刻（webui.html:9049 的教训）。
  ok(/function\s+logPlaceholder\s*\(/.test(html), '日志占位行走 logPlaceholder');
  ok(/function\s+logPlaceholder\s*\([^)]*\)\s*\{[\s\S]{0,200}?log-empty/.test(html),
    'logPlaceholder 生成 .log-empty 块（不是 <tr><td colspan>）');
  ok(!/rowPlaceholder\(COLS\.logs/.test(html), '日志区不再借用表格 colspan 占位（COLS.logs 已删）');

  // 反向验证：日志区不该再出现表格标签 / 写死的 colspan 数字 / 死占位符。
  //
  // 注意：这里的 html 变量是**源文件**，不是服务端替换后的输出；"服务端渲染
  // 出来的列数是几"由读 /ui 真实响应的探针负责，本套件只管源码形状。
  //
  // ⚠ 边界必须取自 <section data-key="logs"> 到下一个 <section>。
  //   原来取的是 id="logrows" → id="jobrows"，但 jobrows（1551）在
  //   logrows（1567）**之前**，于是 slice 落回"从这里到文件结尾"——
  //   等于把整个文件尾部当成日志区，判据悄悄变成了另一件事。
  const secStart = html.indexOf('<section data-key="logs">');
  const secEnd = secStart < 0 ? -1 : html.indexOf('<section', secStart + 1);
  const logSection = secStart < 0 ? '' : html.slice(secStart, secEnd < 0 ? html.length : secEnd);
  ok(logSection.length > 0, '抽到了请求日志的 section');
  ok(!/<(table|thead|tbody|tr|td)\b/.test(logSection),
    '日志区不含 table/tr/td（占位句不会再被 HTML 解析器吞掉）');
  // ⚠ 这里原先写的是单反斜杠 \d（见本文件生成器的模板转义说明）。在**非
  //   String.raw 的模板字面量**里，\d 会被吃掉反斜杠变成字面的 d，产物里的
  //   正则就成了 /colspan="d+"/ —— 任何数字 colspan 都匹配不上，这条断言
  //   恒真。改成 \\d 之后它才真的能抓到硬编码的数字 colspan。
  const literalNum = (logSection.match(/colspan="\d+"/g) || []);
  ok(literalNum.length === 0,
    '日志区没有硬编码的数字 colspan（实际 ' + literalNum.length + ' 处）');
  ok(!logSection.includes('__COLS_LOGS__'),
    '日志区不再有死占位符 __COLS_LOGS__（列数已由 grid 轨道定义）');
}

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
