
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
// 模块级常量：renderStats 用到 NODATA（"无数据"占位）。从源码取，避免两处漂移。
const NODATA = /const NODATA = '([^']*)'/.exec(html)[1];
const els = {};
// style 必须存在：新的 renderStatsModels 会设 wrap.style.display
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, style: {} }; }
// 假 DOM 的元素集合**直接取自页面里出现过的 id**（不再手写一份清单：手写清单会在页面删/加
// id 时静默漂移，而漂移的方向恰好是"夹具替产品把已删的 id 造出来"，判据就空转了）。
for (const m of html.matchAll(/id="([^"]+)"/g)) els[m[1]] = mkEl(m[1]);
// ⚠ $ 与产品保持一致：webui.html:1795 是 "const $ = id => document.getElementById(id);"
//   ⇒ **缺失的 id 返回 null**。本仓库其它 generator 习惯"自动建元素"，本文件**不**这么做：
//   renderStatsReport 的守卫是 "if (!donutEl || !legendEl) return;"，而 statsTopUid 这个
//   id **已被产品删除**（webui.html:1214 的注释）—— 自动建元素会让"产品里根本走不到"的
//   那段代码在夹具里被跑到；缺元素时夹具应当**红着**提醒我们同步页面结构。
const $ = id => els[id] || null;
// grabBlock 从整页 HTML 里抠出一个函数本体（按大括号配平）—— 与 generator 侧**同一个实现**。
// [4] 的静态判据要的是"只有 renderStats 本体"这个精确区间：原来用
// html.slice(indexOf('function renderStats('), indexOf('function fmtBytes(')) 取，
// 这会随函数的**排列顺序**漂移（本轮实测：fmtBytes 排到了 renderStats 之后 ⇒ 区间从 77 行
// 膨胀到 1200+ 行，判据变成"从 renderStats 到文件另一头"）。所以把 grabBlock 也发到夹具里。
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


// ============================================================================
// 沙箱 shim：window（L1 的修法）
// ============================================================================
// 【为什么是夹具的问题，不是产品的问题】
// 被测的 renderStats 第一行就是 window._statsCache = s;（webui.html:9558），
// 读它的是 webui.html:2226 的网关卡 const st = window._statsCache;。
// webui.html 是 //go:embed 进二进制的整页 <script>（internal/server/webui.go:15），
// 全程在浏览器里跑 ⇒ window 是宿主提供的全局对象，那行**在浏览器里合法且必要**。
// Node 沙箱里没有 window ⇒ 缺件在**夹具**。判定依据就是这个：同一段代码在真实
// 宿主（浏览器）里合法 ⇒ 该补的是沙箱，不是产品。
//
// 【为什么不能写成 window = {}】
// 浏览器里 window **就是**全局对象本身，自带 localStorage / matchMedia / location /
// document / addEventListener…。补成 {} 的后果不是"现在能不能跑"，而是**以后**：
// 产品一旦读到 window.localStorage，夹具会以 TypeError 红出来，而真原因是沙箱缺件
// ⇒ 所有断言的真实失败原因被"沙箱缺件"盖住，判别力丢失。
// 所以这里按浏览器形状补：值域合法、行为真实（localStorage 真能存取，matchMedia
// 返回 MediaQueryList 形状），并且补完**立刻自检**（见测试体 [0] 节）。
//
// 【故意不补的东西（同样是判别力问题）】
//   · window.__WB2API_KEY__ / __WB2API_ADMIN_TOKEN__ —— 这两个是**服务端注入**的
//     （webui.html:9 的 <script> 在服务时才被替换成真值）。夹具里没有它们，
//     将来被测代码开始读它，夹具应当**红着**提醒我们把注入面一起搬进来，
//     而不是让一个假值把断言蒙过去。
//   · 真正的 DOM 树 —— 夹具用 mkEl 造的假元素（见 header）；两者别混用。
//
// ⚠ 顶层标识符叫 shimWindow 而**不是** win：产品 renderStats 内部有一个局部
//   let win;（webui.html:9576，窗口文案的变量），同名会被遮蔽/误导；
//   叫 shimWindow 就不会被将来的一次复制粘贴撞成重复声明（SyntaxError）。
const shimWindow = {
  // 真实形状的 Storage：length / key(i) / getItem / setItem / removeItem / clear
  localStorage: (function () {
    const m = Object.create(null);
    return {
      getItem: k => (String(k) in m ? m[String(k)] : null),
      setItem: (k, v) => { m[String(k)] = String(v); },
      removeItem: k => { delete m[String(k)]; },
      clear: () => { for (const k of Object.keys(m)) delete m[k]; },
      key: i => Object.keys(m)[i] === undefined ? null : Object.keys(m)[i],
      get length() { return Object.keys(m).length; },
    };
  })(),
  // MediaQueryList 形状：matches=false（浅色档）、有 addEventListener/removeEventListener
  // （产品在 webui.html:2623 就是 addEventListener('change', …)，只给 matches 会漏）。
  matchMedia: q => ({
    matches: false, media: String(q), onchange: null,
    addEventListener() {}, removeEventListener() {},
    addListener() {}, removeListener() {},
    dispatchEvent() { return false; },
  }),
  location: { href: 'http://127.0.0.1/', protocol: 'http:', host: '127.0.0.1',
    hostname: '127.0.0.1', port: '', pathname: '/', search: '', hash: '' },
  innerWidth: 1280, innerHeight: 800,
  addEventListener() {}, removeEventListener() {}, dispatchEvent() { return false; },
  getComputedStyle: () => ({ getPropertyValue: () => '' }),
  requestAnimationFrame: cb => setTimeout(() => cb(Date.now()), 0),
  cancelAnimationFrame: id => clearTimeout(id),
};
// document.getElementById 走**产品同一条路**（缺失返回 null，与产品 $ 一致）；
// 元素由 header 从页面 id 里造好，所以这里能查到的是"页面里真有这个 id"的那些。
shimWindow.document = {
  getElementById: id => els[id] || null,
  querySelector: () => null, querySelectorAll: () => [],
  addEventListener() {}, removeEventListener() {},
  documentElement: { classList: { add() {}, remove() {}, contains: () => false } },
  body: { classList: { add() {}, remove() {}, contains: () => false }, appendChild() {}, style: {} },
  createElement: tag => ({ tagName: String(tag).toUpperCase(), style: {}, appendChild() {}, setAttribute() {} }),
};
// 浏览器里 window === self，且两者都指向全局对象本身。Node 里显式挂上去。
shimWindow.window = shimWindow;
shimWindow.self = shimWindow;
global.window = shimWindow;
global.document = shimWindow.document;

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

function fmtTokens(n) {
    if (typeof n !== 'number' || !isFinite(n)) return '—';
    const abs = Math.abs(n);
    const units = [[1e9, 'B'], [1e6, 'M'], [1e3, 'K']];
    for (const [div, suf] of units) {
      if (abs >= div) {
        const v = n / div;
        // 保留一位小数，但整数就别写成 "10.0M"。
        const t = Math.round(v * 10) / 10;
        return (Number.isInteger(t) ? String(t) : t.toFixed(1)) + suf;
      }
    }
    return String(n);
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
    // 网关状态卡要读累计请求/成功率：存一份供 renderCards 使用
    //（renderCards 在 refresh 里先于 renderStats 调用）。
    window._statsCache = s;
    if (!s) return;
    // ⚠⚠ **报表区在这里就画**（不能放在函数末尾 —— 我踩过）。
    // 函数体里有一处 `return [`（提前返回卡片数组），我最初把调用插在它之后
    // ⇒ **成了死代码，永远执行不到**：页面上"Top 账号 / 上游占比"两个面板一直空着，
    // 而 `pageerror` 干干净净，让人误以为没事。放在这里最稳：拿到数据就画得到。
    renderStatsReport(s);
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

    // 精简后的核心统计（用户要求"不用很复杂，就是简单的统计"，参考 new-api 那类
    // 仪表盘的呈现：请求数 / 成功失败 / 成功率 / token / 耗时）。
    // 砍掉：缓存命中率 / 缓存 token / 推理 token / 缓冲占用 —— 排障细节，
    // 堆在这里只会让"一眼看网关用没用起来"这件事变难。
    //
    // 「累计消耗」改成 token（用户本轮要求）：不同上游的 credit 单位不同
    //（积分/额度/点），加起来没有可比性；而 token 是跨上游统一的口径。
    //
    // ⚠⚠ **这里原来写的是「消耗 token = 输出 token + 推理 token（usage 里记录的只有这两个）」——
    //   那个假设是错的，已于 2026-10-18 修掉（F1，由 R1 复核提出、主脑复验）：**
    //   日志里的 `tokens` 就是 `completion_tokens`，而它**已经包含推理 token** ⇒ 再加一遍
    //   `think_tokens` 等于把推理算两遍，会把「消耗 token」虚高约 **47%**。
    //   复验方法（可重跑）：统计 `think_tokens > tokens` 的行数 —— **零反例**；
    //   量级上 sum(think)/sum(tokens) ≈ 0.474。另有三处代码旁证：wire/usage.go 从
    //   `completion_tokens_details.reasoning_tokens` 取、usage_test.go 的 fixture、
    //   cline.go 的「思考 token 计入 completion_tokens」注释。
    // ⇒ 现在展示为两张**各表一件事**的卡：「输出 token」= tokens（含推理的真值）、
    //   「推理 token」= think_tokens（其中有多少是推理）。原先那处重复计算已删除。
    //
    // 计算抽成 statsCardsEntries（第 1 组）+ statsCardsEntries2（第 2 组）：
    // renderStats（整页渲染）与无感轮询 pollStatsCards（只改数值）共用同一份判据，
    // 杜绝两处漂移 —— 卡片清单**只有这两个函数**，别的地方不许再列一遍。

    // card 统一渲染：v 为 null 表示缺数据 → 显示占位符 + dim 色。
    // data-stat-key 供无感轮询定位（见 pollStatsCards）。
    const card = (k, v, cls) =>
      `<div class="card" data-stat-key="${esc(k)}"><div class="k">${esc(k)}</div><div class="v ${v === null ? 'dim' : (cls || '')}">${esc(v === null ? NODATA : v)}</div></div>`;

    // ⚠ 两个容器各自渲染一组（分组理由见 HTML 里 `#statsCards2` 那段注释）：
    //   第 1 组 = 流量与耗时，第 2 组 = 模型归属与活跃度。
    //   ⚠ 两组**共用同一个 card()**（同一个 `data-stat-key` 约定），否则回填逻辑要认两种形状。
    $('statsCards').innerHTML = statsCardsEntries(s).map(([k, v, c]) => card(k, v, c)).join('');
    $('statsCards2').innerHTML = statsCardsEntries2(s).map(([k, v, c]) => card(k, v, c)).join('');
    renderHealthBar(s);   // 与卡片同源（同一份 s），不会出现"卡对条错"
    renderModelRank(s);   // 次数排行同源，同样只读这一份响应

    // ⚠ 「模型明细」chips 已按用户要求删除（2026-10-18）⇒ 这里**不再调** renderStatsModels。
    // ⚠ 函数本体保留未删：它里面还带着 `statsCatalogHint` 等挂载逻辑，
    //   删函数的风险大于收益；而只要容器不存在，它本来就是空转（开头有 `if (!wrap) return`）。
    // renderStatsModels(s);
  }

function statsCardsEntries(s) {
    if (!s) return [];
    const total = num(s.total);
    const ok = num(s.ok);
    const rate = total && ok !== null ? Math.round((ok / total) * 100) : null;
    // ⚠⚠ 平均 RPM / TPM 的窗口（2026-10-20 加，对标 new-api 的指标卡）：
    //   必须用**真实窗口跨度**（后端给的 window_from/window_to）去除，不能用固定的 24h ——
    //   用户切 1h / 24h / 7d / 30d 档位时窗口差几十倍，写死就把 7 天摊成了 1 天。
    //   ⚠ 跨度拿不到（老响应 / 只有一个点）⇒ 返回 null，卡片显示占位而不是编个数。
    let winMin = null;
    if (s.window_from && s.window_to) {
      const a = new Date(s.window_from).getTime();
      const b = new Date(s.window_to).getTime();
      if (isFinite(a) && isFinite(b) && b > a) { winMin = (b - a) / 60000; }
    }
    const perMin = (v) => {
      const n = num(v);
      return (winMin && n !== null) ? n / winMin : null;
    };
    const rpm = perMin(s.total);
    const tpm = perMin(s.tokens);
    return [
      ['总请求', fmtCount(s.total), ''],
      ['成功', fmtCount(s.ok), ok ? 'ok' : 'dim'],
      ['失败', fmtCount(s.fail), num(s.fail) ? 'err' : 'dim'],
      ['成功率', rate === null ? null : rate + '%', rate === null ? 'dim' : rate >= 95 ? 'ok' : rate >= 80 ? 'warn' : 'err'],
      // ⚠ F1 修复：不再 `tokens + think_tokens`（那会把推理算两遍）。两张卡各表一件事。
      // ⚠ 2026-10-20 加回「总处理量」（2026-10-20 用户授权推翻"只显示输出"的做法）。
    //
    //   出处：DSH 的 token-ledger-panel.js
    //     total = inputTokens + cacheReadTokens + outputTokens   // 缓存写入不重复计入
    //     lines.push('总处理量 = 输入 + 输出；缓存写入不重复计入')
    //
    //   我们的 s.tokens 就是上游 usage 的 total_tokens，而 total_tokens = prompt + completion
    //   （prompt 已含 cached tokens）⇒ 与 DSH 那个式子**等价**，直接复用不必新算。
    //
    // ⚠ 2026-10-20 用户要求：三张卡合并成**一张「总 Token」**，与 new-api 一致。
    //
    //   改前：总处理量 / 输出 token / 推理 token 三张并列
    //   改后：只有「总 Token」
    //
    // # 为什么是总 Token
    //
    //   new-api 的「Total Tokens」= Σ token_used = Σ(prompt_tokens + completion_tokens)
    //   （model/log.go:395），它整个面板只有这一个 token 口径。
    //   用户原话：「前端展示的全部都是总Token，不要输出Token了」。
    //
    // # ⚠ 哪些地方**不能**改成总 Token
    //
    //   tok/s（生成速度）必须继续用**输出** token —— 那是物理定义
    //   （"生成 N 个字用了 T 秒"）。改用总 Token 会得出"每秒 40 万 token"这种荒谬读数，
    //   因为输入里含几万到几十万的缓存上下文。
    //   所以 fmtTps / tokPerSec 那一族仍然吃 out_tokens，不在本次改动范围。
    ['总 Token', num(s.tokens) ? fmtTokens(s.tokens) : null, ''],
      ['平均 TTFB', fmtCount(s.avg_ttfb_ms) === null ? null : s.avg_ttfb_ms + 'ms', ''],
      ['平均总耗时', fmtCount(s.avg_total_ms) === null ? null : s.avg_total_ms + 'ms', ''],
      // 对标 new-api 的两张：每分钟请求数 / 每分钟 token 数（口径见上面的 winMin 注释）
      ['平均 RPM', rpm === null ? null : (rpm >= 10 ? fmtCount(Math.round(rpm)) : rpm.toFixed(1)), 'dim'],
      ['平均 TPM', tpm === null ? null : fmtTokens(Math.round(tpm)), 'dim'],
    ];
  }

function renderStatsReport(s) {
    const topEl = $('statsTopUid'), donutEl = $('statsDonut'), legendEl = $('statsDonutLegend');
    // ⚠ 守卫**不再要求 `topEl`**（2026-10-18）：「Top 账号」那块已按用户要求删掉，
    //   `$('statsTopUid')` 恒为 null —— 若还把它算进守卫，本函数会**每次早返回** ⇒
    //   两个环一个都不画。⚠ 这类"删了容器忘了改守卫"是静默失败，只能靠看图发现。
    if (!donutEl || !legendEl) return;
    if (!s) return;
    // ⚠ 取值要**同时认两种形状**：
    //   · 老形状：`by_provider = { "loomy": 26012 }`（裸数字）
    //   · 新形状（A 代理刚补的富化版）：`{ "loomy": { calls, ok, fail, credit } }`
    // ⚠ 只认一种就会静默出 0 —— 实测：环形图当时整块不画（因为总和为 0 被防零除挡住），
    //   而 `pageerror` 干干净净，不看图根本发现不了。
    const calls = (v) => {
      const raw = (v && typeof v === 'object') ? v.calls : v;
      const n = numNonNeg(raw);
      return n === null ? 0 : Math.round(n);
    };
    const pad = calls;
    // ⚠ 每个环比色都走主题变量（切换深浅色时不能有"局部还是旧配色"的花脸）。
    // ⚠ `COLORS` 已提到**模块级**（见 `DONUT_COLORS`）—— 2026-10-18 的教训：
    //   它原来定义在本函数内部，而抽出去的 `renderDonutInto` 也要用它
    //   ⇒ 跨函数作用域直接是 `ReferenceError`，**而异常被空 catch 吞掉、console 干净**
    //   ⇒ 我在那个盲区里盲改了四次。⚠ 共享函数要用的常量，必须放在共享函数够得着的地方。

    // ⚠⚠ digest 守卫：本函数由 `pollStatsCards` 每 30 秒调一次（让榜/环与卡片同步）。
    // 不判"值变没变"⇒ 榜与环每 30 秒整块重绘 ⇒ 肉眼可见的**闪**（尤其 `conic-gradient` 的环）。
    // ⚠ digest 只取本函数真正消费的两个字段：`by_uid` 与 `by_provider`。
    // ⚠⚠ 2026-10-18 踩坑记录：我一度以为这份守卫被早先的 edit 挤掉了、于是又加了一遍
    //   ⇒ **变成重复声明**（`Identifier 'dg' has already been declared`）
    //   ⇒ 整页内联 JS **一段都不执行**（`cards = 0`），而我当时**已经把坏二进制替换进去了**。
    //   ⇒ 教训：**改完必须先跑门禁、红了就不许替换二进制**（我那次编了就直接换了，顺序反了）。

    // ⚠ 「Top 账号」那块已删（2026-10-18 用户要求"直接删除不需要这个"）⇒ 它那两行一并去掉：
    //   原来这里设 `#statsTopHint`（"共 N 个账号有请求"），而那个容器已不存在。
    //   ⚠ 保留这段注释是为了说明：**顶部提示那条约束仍然成立** —— 若将来又加回某个顶部提示，
    //     仍必须放在**所有 return 之前**（R2 抓到的陈旧文案问题）。

    // ⚠ 「Top 模型」卡**不在这里填**（2026-10-18 实测）：
    //   本函数会在多处被调，其中有的入参不是 `/admin/stats` 的完整响应
    //   ⇒ 在这里填完会被后面的调用覆盖掉（实测：数据/选择器/算法全对，卡片仍是 `—`）。
    //   ⇒ 已挪到 `fillTopModel(s)`，只在 `refresh` 那个确定是 `/admin/stats` 响应的地方调。

    // ⚠⚠ digest 守卫。为什么必须有：`pollStatsCards` 每 30 秒会调一次本函数
    //（为了让榜/环与卡片同步 —— 否则同一屏出现两个时刻）。若不判"值变没变"，
    // 榜与环每 30 秒就整块重绘一次 ⇒ ⚠ 肉眼可见的**闪**（尤其是 `conic-gradient` 的环）。
    // ⚠ digest 只取本函数真正消费的两个字段：`by_uid` 与 `by_provider`。
    //   别的字段变了但这两个没变 ⇒ **不该**重绘（本函数画的只有这两样）。
    const dg = JSON.stringify(s.by_uid) + '|' + JSON.stringify(s.by_provider);
    if (dg === renderStatsReport._last) return;
    renderStatsReport._last = dg;

    // ---- 左栏：Top 账号（按请求数降序取前 8）----
    const uids = Object.entries(s.by_uid || {}).map(([k, v]) => [k, pad(v)])
      .sort((a, b) => b[1] - a[1]).slice(0, 8);
    const uidMax = uids.length ? uids[0][1] : 0;
    let uidSum = 0;
    for (const v of Object.values(s.by_uid || {})) uidSum += pad(v);
    // ⚠ 这块的容器已删（见上面注释）⇒ 加 `if (topEl)` 保住这段代码不死；
    //   等哪天又要 Top 榜，把容器加回来它就自动生效（⚠ 而不是留着一段必崩的语句）。
    if (topEl) topEl.innerHTML = uids.length
      ? uids.map(([uid, n]) => {
          const w = uidMax > 0 ? Math.max(2, (n / uidMax) * 100) : 0;         // 最小 2% 才看得见
          const pct = uidSum > 0 ? ((n / uidSum) * 100).toFixed(1) : '—';      // ⚠ 防零除
          // uid 形如 UUID 或纯数字，都很长 ⇒ 截断显示，全文放 title（与表格列同一套做法）。
          const label = uid.length > 12 ? uid.slice(0, 12) + '…' : uid;
          return `<div class="toprow" title="${esc(uid)}">
            <span class="tname">${esc(label)}</span>
            <span class="tbar"><i style="width:${w}%"></i></span>
            <span class="tval">${n}</span>
            <span class="tpct">${pct}%</span>
          </div>`;
        }).join('')
      : '<span class="dim" style="font-size:12px">暂无数据</span>';

    // ⚠⚠ 两个环**不在这里画**（2026-10-18 实测踩到）。
    //   根因与「Top 模型」那一处**同一个**：本函数会在多处被调，其中有的入参
    //   不是 `/admin/stats` 的完整响应 ⇒ `by_provider_detail` / `by_model_detail` 取不到
    //   ⇒ 环整块空；⚠ 而抛错被 try/catch 吞掉、`pageerror` 干净，只有看图/量 DOM 才发现。
    //   ⇒ 已挪到 `fillDonuts(s)`，只在 `refresh` 那个确定是 `/admin/stats` 响应的地方调。
    //
    // ⚠ 同理：上面那个 **Top 账号榜**也读 `s.by_uid` —— 它留在本函数里，
    //   所以它也有同一风险（只是实测里那次入参恰好完整）。⚠ 见 fillDonuts 的注释。

    // ⚠ 顶部提示已移到函数开头（见那里注释：原来放这儿会被空数据分支的 return 跳过）。
  }

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const base = {
  total: 743, ok: 700, fail: 43, avg_ttfb_ms: 120, avg_total_ms: 900, tokens: 12345,
  source: 'file', window_from: '2026-09-11T06:59:39+08:00',
  file: { size: 262144, max_bytes: 8388608 },
  capacity: 500, held: 120, by_model: { 'deepseek-v4-flash': 12 },
};

// ---------- 0. 沙箱自检（给"测量工具"本身做体检） ----------
//
// ⚠ 为什么要单独有这一节：本轮红的第一个异常
//   （ReferenceError: window is not defined）**看起来像产品缺陷**，实际是夹具沙箱缺件。
//   先证明沙箱像浏览器，后面的断言才有意义 —— 否则每一节的失败原因都会被"沙箱缺件"盖住。
console.log('\n[0] 沙箱自检 + window 契约');
// 先证明"判据面还在"：夹具驱动的这两个元素必须真的出现在整页里。
// ⚠ 缺了要**提前退**而不是继续跑：继续跑只会在 $('statsCards').innerHTML 上抛
//   "Cannot read properties of null" —— 那条堆栈会把人往"产品缺陷"的方向带，
//   而真实原因是 webui.html 的结构变了（夹具要跟着改）。
const missingEls = ['statsCards', 'statsWindow'].filter(id => !els[id]);
if (missingEls.length) {
  console.log('  FAIL 整页里缺少夹具驱动的元素：' + missingEls.join(' / ') + '（webui.html 结构变了，夹具要跟着改）');
  console.log('\n=== 1 项失败 ===');
  process.exit(1);
}
ok(true, '夹具驱动的元素都在页面里（statsCards / statsWindow）');
ok(typeof window === 'object' && window !== null, 'window 是全局对象（浏览器等价）');
ok(typeof window.localStorage.getItem === 'function' && typeof window.localStorage.setItem === 'function'
  && typeof window.localStorage.removeItem === 'function' && window.localStorage.length === 0,
  'localStorage 形状真实（getItem/setItem/removeItem/length 齐全）');
window.localStorage.setItem('wb2api_probe', '1');
const lsWrite = window.localStorage.getItem('wb2api_probe') === '1' && window.localStorage.length === 1;
window.localStorage.removeItem('wb2api_probe');
ok(lsWrite && window.localStorage.getItem('wb2api_probe') === null && window.localStorage.length === 0,
  'localStorage 真能存取与删除（不是空壳 {}）');
const mq = window.matchMedia('(prefers-color-scheme: dark)');
ok(mq && mq.matches === false && typeof mq.addEventListener === 'function' && typeof mq.removeEventListener === 'function',
  'matchMedia 返回 MediaQueryList 形状（浅色档 matches=false，且带 addEventListener）');
ok(window.self === window && window.window === window, 'window 自引用（浏览器里 window === self）');
ok(window.document.getElementById('statsCards') === $('statsCards'),
  'document.getElementById 与 $ 查同一套元素（产品里 $ 就是它，webui.html:1795）');
ok($('__fixture_absent_id__') === null, '缺失 id 返回 null（不自动造元素 —— 免得跑到产品走不到的路径）');

// renderStats 的 window 契约：webui.html:9558 写、:2226 读（网关卡据此填累计请求/成功率）。
// 这条断言是 shim 的"判别力担保"：它证明暖的 window 就是产品读的那个窗口对象。
const payload = { ...base };
resetEls(); renderStats(payload);
ok(window._statsCache === payload, 'renderStats 把入参挂到 window._statsCache（同一份对象，供网关卡读）');

// ---------- 1. 落盘上限不进入统计卡区 ----------
//
// ⚠ 判据面的变迁（L3）：本判据原本盯「落盘占用」卡（要求只显示占用量，不显示 "X / 8.0 MiB"）。
//   那张卡**已被产品删除**（v1.5.0 控制台精简；现存卡片列表见 webui.html:9640，同时 fmtBytes
//   成了零调用的死代码）。删卡是产品侧的**有意**动作：文件里逐项写了删掉哪些（:9599-:9605），
//   落盘信息挪到设置/日志页 renderLogFileInfo（:7868），那里**故意**把上限显示出来，
//   理由写在 :7862-:7864。⇒ 判定 (a)：不是回归，是夹具的断言面过期。
//   修法：重定向到仍然存在、且**更强**的形式 —— 「上限不许以任何形式进入统计卡区」。
//   比原判据更不易被绕过：卡片回来但格式错了、字节数漏出去，都会红。
console.log('\n[1] 落盘上限不进入统计卡区');
resetEls(); renderStats({ ...base });
const cardsA = $('statsCards').innerHTML;
const cardKeysA = (cardsA.match(/data-stat-key=/g) || []).length;
ok(cardKeysA > 0, '卡区确实渲染出了卡片（' + cardKeysA + ' 张）—— 否则下面两条否定断言是空转的真');
ok(!cardsA.includes('8388608'), 'max_bytes 的字节数没有进入卡区');
ok(!cardsA.includes('MiB'), '卡区不出现 MiB 量级的上限写法');

// 换一个上限值，卡区必须逐字节不变 —— 证明上限真的不参与渲染
resetEls(); renderStats({ ...base, file: { size: 262144, max_bytes: 1 } });
ok($('statsCards').innerHTML === cardsA, '换一个上限值，卡区逐字节不变（上限确已脱离渲染）');

// 量级显示契约：fmtBytes 是那条要求留下的唯一实物（现在零调用，属死代码 —— 但卡片回归时
// 量级不能退化）。断言直接打在它身上，保住原判据的判别力，而不是删掉。
ok(fmtBytes(262144) === '256.0 KB', 'fmtBytes 262144 → 256.0 KB（实际：' + fmtBytes(262144) + '）');
ok(fmtBytes(512) === '512 B', '小于 1KB 时按字节显示（实际：' + fmtBytes(512) + '）');
ok(fmtBytes(5 * 1024 * 1024) === '5.0 MiB', '大于 1MiB 时按 MiB 显示（实际：' + fmtBytes(5 * 1024 * 1024) + '）');

// 历史断言的**条件形式**保留：万一「落盘占用」卡回归，值只准是占用量。
const used = cardValue('落盘占用');
ok(used === null || (!used.includes('/') && !used.includes('MiB')),
  '若「落盘占用」卡回归，只准显示占用量（不含 X / Y 与上限；实际：' + used + '）');

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

// ---------- 3. 边界：内存来源的分母语义，file 缺失不崩 ----------
console.log('\n[3] 边界情况');
resetEls(); renderStats({ ...base, source: 'memory' });
// 「held/capacity 分母」那张卡同样已删 ⇒ 分母语义现在只剩窗口文案这一处：
// 上限必须来自 capacity（500），不能是 held（120）、也不能是占位符。
ok($('statsWindow').textContent === '（仅内存缓冲，进程启动至今，上限 500 条）', '内存来源窗口文案未受影响');
ok(!$('statsWindow').textContent.includes('120'), 'held（120）没有被当成上限渲染出去');
resetEls(); renderStats({ ...base, source: 'memory', capacity: undefined });
ok($('statsWindow').textContent === '（仅内存缓冲，进程启动至今，上限 ' + NODATA + ' 条）',
  'capacity 缺失时上限回落为占位符 ' + NODATA + '（实际：' + $('statsWindow').textContent + '）');
// 历史断言的条件形式：缓冲占用卡若回归，分母语义必须是 held/capacity。
const heldCard = cardValue('缓冲占用');
ok(heldCard === null || heldCard === '120/500', '若「缓冲占用」卡回归，值仍是 held/capacity（实际：' + heldCard + '）');

// file 缺失：source=file 但不能崩、不能渲染出 undefined/NaN（原判据的后半句）
let threw = null;
resetEls();
try { renderStats({ ...base, file: undefined }); } catch (e) { threw = e && e.message; }
ok(threw === null, 'source=file 但 file 缺失时不抛（实际：' + threw + '）');
const noFileText = $('statsCards').innerHTML + '|' + $('statsWindow').textContent;
ok(!/undefined|NaN|\[object/.test(noFileText), 'file 缺失时卡区/窗口文案干净（实际：' + noFileText + '）');

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

// ---------- 4. 静态：renderStats 本体内不再有落盘上限 / 日期格式化 ----------
console.log('\n[4] 静态检查');
// ⚠ 原来这里取 html.slice(indexOf('function renderStats('), indexOf('function fmtBytes('))。
//   源码顺序变了 —— fmtBytes 现在排在 renderStats **之后**（webui.html:10777 vs :9555）
//   ⇒ 那个区间会从 77 行膨胀到 1200+ 行，判据从"函数体内"漂成"从 renderStats 到文件另一头"，
//   于是别人的代码里出现 max_bytes/toLocaleString 也会把本判据弄红/弄绿。改用 grabBlock
//   精确取函数本体，判据回到原意（只查 renderStats 自己）。
const statsFn = grabBlock('function renderStats(');
ok(!/max_bytes/.test(statsFn), 'renderStats 内已无 max_bytes 引用');
ok(!/toLocaleString/.test(statsFn), 'renderStats 内已无日期格式化（起点回显已移除）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
