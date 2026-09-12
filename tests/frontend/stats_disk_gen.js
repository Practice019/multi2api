
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
  const m = /<div class="v[^"]*">([^<]*)<\/div>/.exec(h.slice(i));
  return m ? m[1] : null;
}

function num(v) {
    if (typeof v === 'number') return Number.isFinite(v) ? v : null;
    if (typeof v === 'string' && v.trim() !== '') {
      const n = Number(v);
      return Number.isFinite(n) ? n : null;
    }
    return null;
  }

function numNonNeg(v) {
    const n = num(v);
    return n === null || n < 0 ? null : n;
  }

function fmtCount(v) {
    const n = numNonNeg(v);
    return n === null ? null : String(Math.round(n));
  }

function fmtCredit(v) {
    const n = numNonNeg(v);
    if (n === null) return null;
    if (n === 0) return '0';
    return String(Number(n.toFixed(4)));
  }

function fmtTokens(v) {
    const n = numNonNeg(v);
    if (n === null) return null;
    if (n < 1000) return String(Math.round(n));
    if (n < 1000000) return (n / 1000).toFixed(n < 10000 ? 1 : 0) + 'k';
    return (n / 1000000).toFixed(1) + 'M';
  }

function fmtRate(v) {
    const n = num(v);
    if (n === null || n < 0 || n > 1) return null;
    return (n * 100).toFixed(1) + '%';
  }

function fmtBytes(n) {
    if (!n || n < 0) return '0 B';
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1024 / 1024).toFixed(1) + ' MiB';
  }

function renderStatsModels(s) {
    const wrap = $('statsModelsWrap');
    if (!wrap) return;
    // 非数值 / 负数计数一律当 0（不能让 NaN 参与排序，那会让顺序随机；
    // 负数排序更会让「最多的排最后」，与用户预期相反）。
    const bm = Object.entries(s.by_model || {})
      .map(([k, v]) => { const n = numNonNeg(v); return [k, n === null ? 0 : Math.round(n)]; })
      .sort((a, b) => b[1] - a[1]);
    if (!bm.length) { wrap.style.display = 'none'; return; }
    wrap.style.display = '';

    // 系数索引：后端只回「窗口里调用过且有系数」的模型，这里做成 map 方便按名字取。
    const mult = {};
    (Array.isArray(s.model_multipliers) ? s.model_multipliers : []).forEach(m => {
      if (!m) return;
      const v = num(m.multiplier);
      if (v !== null && v > 0) mult[m.model] = v;
    });

    // 目录状态提示：把「有 / 已过期 / 拿不到」讲清楚，
    // 因为用户看到一排没有系数的 chip 时唯一能做的就是去点「刷新模型」。
    const cat = (s.model_catalog && typeof s.model_catalog === 'object') ? s.model_catalog : {};
    const catModels = fmtCount(cat.models);
    let hint;
    if (cat.state === 'ok') {
      hint = '· 成本系数来自 /v3/config' + (catModels === null ? '' : `（缓存 ${catModels} 个模型）`);
    } else if (cat.state === 'stale') {
      hint = '· 成本系数为过期缓存，可信度降低（点「刷新模型」重新拉取）';
    } else {
      hint = '· 成本系数不可用（上游 /v3/config 未取到），仅显示调用次数';
    }
    $('statsCatalogHint').textContent = hint;

    $('statsModels').innerHTML = bm.map(([name, calls]) => {
      const m = mult[name];
      // 系数后缀：有才加；没有就只显示次数，不显示「x0 倍」这种会被读成免费的说法。
      const suffix = m === undefined
        ? `<span title="未取到该模型的成本系数">${calls} 次</span>`
        : `<span title="每次调用消耗 ${m} 倍基础额度">${calls} 次 · x${m}</span>`;
      return `<span class="chip" data-id="${esc(name)}">${esc(name)}${suffix}</span>`;
    }).join('');
  }

function renderStats(s) {
    if (!s) return;
    // 窗口文案必须如实说明数据来源：统计改为聚合落盘历史后，
    // 不再是「进程内窗口」，而落盘不可用时会回落到内存 —— 两种情况不能混为一谈。
    //
    // 三个数字都先过 fmtCount 再插值：直接写 `${s.total}` 时，若上游/后端给了
    // 非数值（对象、数组、垃圾字符串），非空值会被模板原样串成 `[object Object]`
    // 显示给用户。这里与卡片区用同一套格式化，保证「同一个数在哪都长一样」。
    //
    // 判空用 `!totalText || totalText === '0'`：既要挡住 null（无数据），
    // 也要保留「0 条 = 日志为空」这个既有文案 —— 光判 null 会把
    // 「（落盘日志为空）」变成「（全部落盘历史，共 0 条）」，是措辞退化。
    const totalText = fmtCount(s.total);
    let win;
    if (s.source === 'file') {
      // 只报总数，不再回显「从某时刻起」—— 那个起点是文件里最早一条的时间，
      // 随轮转/清理不断变化，写在这里既不稳定也没人据此判断什么。
      win = (!totalText || totalText === '0')
        ? '（落盘日志为空）'
        : `（全部落盘历史，共 ${totalText} 条）`;
    } else if (s.source === 'memory') {
      const capText = fmtCount(s.capacity);
      win = s.window_from
        ? `（仅内存缓冲，进程启动至今，上限 ${capText === null ? NODATA : capText} 条）`
        : '（暂无请求）';
    } else {
      win = '（未启用日志）';
    }
    if (s.error) win += ' · ' + esc(String(s.error));
    $('statsWindow').textContent = win;

    // 成功率：total 缺失或为 0 时不给数字（会得到 0/0 → NaN）。
    const total = num(s.total);
    const ok = num(s.ok);
    const rate = total && ok !== null ? Math.round((ok / total) * 100) : null;

    // 新维度：累计消耗 / 缓存命中率 / 推理 token。
    // 每个都先算出「字符串或 null」，再由 card() 统一决定颜色与占位符。
    const credit = fmtCredit(s.credit_total);
    const hitRate = fmtRate(s.cache_hit_rate);
    const think = fmtTokens(s.think_tokens);
    // 缓存 token 明细：命中 + 未命中都要有数才显示「x / y」，
    // 只有一边有数时显示那一边（避免出现 "undefined / 120"）。
    const ch = fmtTokens(s.cache_hit_tokens);
    const cm = fmtTokens(s.cache_miss_tokens);
    // 初值用 null 而不是 NODATA：card() 只在 `v === null` 时把值换成占位符
    // **并加 dim 类**。直接赋字符串会让它走"有值"分支 —— 显示同样是「—」，
    // 但少了 dim 样式，与其它"无数据"卡片看起来不一致。
    let cacheDetail = null;
    if (ch !== null && cm !== null) cacheDetail = ch + ' / ' + cm;
    else if (ch !== null) cacheDetail = ch;
    else if (cm !== null) cacheDetail = cm;

    // card 统一渲染：v 为 null 表示缺数据 → 显示占位符 + dim 色。
    // 颜色判据也用数值（不是真值），否则「命中率 0%」会因为 0 是 falsy 而被涂成 dim。
    const card = (k, v, cls) =>
      `<div class="card"><div class="k">${esc(k)}</div><div class="v ${v === null ? 'dim' : (cls || '')}">${esc(v === null ? NODATA : v)}</div></div>`;

    const hitRateNum = num(s.cache_hit_rate);
    const hitRateCls = hitRateNum === null ? 'dim' : hitRateNum >= 0.5 ? 'ok' : hitRateNum >= 0.2 ? 'warn' : 'err';
    const creditNum = num(s.credit_total);
    const thinkNum = num(s.think_tokens);

    $('statsCards').innerHTML = [
      ['总请求', fmtCount(s.total), ''],
      ['成功', fmtCount(s.ok), ok ? 'ok' : 'dim'],
      ['失败', fmtCount(s.fail), num(s.fail) ? 'err' : 'dim'],
      ['成功率', rate === null ? null : rate + '%', rate === null ? 'dim' : rate >= 95 ? 'ok' : rate >= 80 ? 'warn' : 'err'],
      // 累计消耗：上游 usage 里的 credit 求和。旧格式日志行该字段恒为 0，
      // 此时显示 0 是**正确**的（确实没有额度数据被记录），所以不特殊处理。
      ['累计消耗', credit, creditNum ? '' : 'dim'],
      ['缓存命中率', hitRate, hitRateCls],
      ['缓存 token', cacheDetail, ch !== null || cm !== null ? '' : 'dim'],
      ['推理 token', think, thinkNum ? '' : 'dim'],
      ['平均 TTFB', fmtCount(s.avg_ttfb_ms) === null ? null : s.avg_ttfb_ms + 'ms', ''],
      ['平均总耗时', fmtCount(s.avg_total_ms) === null ? null : s.avg_total_ms + 'ms', ''],
      ['输出 token', fmtTokens(s.tokens), ''],
      // 缓冲占用只在内存来源时才有意义；落盘来源改写为文件占用，避免两个概念混淆。
      // 落盘只显示当前占用量：上限是配置里的固定值（默认 8 MiB），放在卡片里
      // 既不随数据变化、又让人误以为是一根进度条，去掉更清楚。
      [s.source === 'file' ? '落盘占用' : '缓冲占用',
        s.source === 'file' && s.file
          ? fmtBytes(s.file.size)
          : ((s.held || 0) + '/' + (s.capacity || 0)),
        'dim'],
    ].map(([k, v, c]) => card(k, v, c)).join('');

    renderStatsModels(s);
  }

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
