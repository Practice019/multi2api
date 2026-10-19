// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：调用统计的卡片区与窗口文案。
//
// 覆盖本轮两点要求：
//  1. 落盘占用只显示占用量，不显示上限（旧实现渲染 "X / 8.0 MiB"）
//  2. 窗口文案不再回显「从某时刻起」，只报总条数
//
// 顺带锁死两条不能退化的边界：
//  - 内存来源仍保留 "held/capacity" 的分母语义（那里的分母是真实缓冲占用率，有意义）
//  - 落盘来源但 file 缺失时不能崩、也不能渲染出 undefined/NaN
//
// ============================================================================
// 2026-10-19 修复记录：本夹具连着**三层漂移**，红的第一个异常只是最表层
// ============================================================================
// 现场签名（修前）：
//   tests/frontend/stats_disk_gen.js:117  ReferenceError: window is not defined
//     at renderStats  ← 源头 webui.html:9558 `window._statsCache = s;`
// 基线工作树（D:\temp\wb2api-base-wt，865ae96）跑同一项：**同一条** ReferenceError、同一行。
//
// 三层各自的判定（都是 (a) 夹具/沙箱过期，不是产品缺陷）：
//  L1 沙箱缺 window —— 产品那行在**浏览器里完全合法**：webui.html 是 `//go:embed`
//     进二进制的整页 <script>（internal/server/webui.go:15），`window` 是宿主提供的全局对象；
//     读它的是 webui.html:2226 的网关卡 `const st = window._statsCache;`。
//     ⇒ 补 shim（见下面的 shim 块），**不要**去改产品那行。
//  L2 抽取清单过期 —— renderStats 现在会调 renderStatsReport（:9564）与
//     statsCardsEntries（:9626），而 pieces 里没有它们 ⇒ 补完 window 之后紧接着就是
//     `ReferenceError: renderStatsReport is not defined`（本轮实测复现过）。
//  L3 断言面的产品形态变了 —— 「落盘占用」「缓冲占用」两张卡**已被产品删除**
//     （v1.5.0 控制台精简；当前卡片列表见 webui.html:9640，fmtBytes 变成**零调用**的死代码）。
//     ⇒ 断言按"保住原意"重定向到仍然存在的面上，并顺手把判据**加强**（见 [1] 注释）。
//
// ⚠ 本文件是 generator：它用 String.raw 包测试体 ⇒ 模板里**不许出现裸反引号**
//   （套件门禁 0，见 run_frontend_suites.js:findRawTemplateBacktick）。
//   反引号总数必须是偶数，否则模板被提前终止、生成失败，而**上一次的产物还在**、照样全绿。
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
  // ⚠ 本轮补的两个（L2）：renderStats 现在会调它们。漏抽的代价是运行期
  // `ReferenceError: xxx is not defined` —— 与产品缺陷长得**一模一样**，
  // 只有回到源码看"这个函数是不是同一个 <script> 里的编程错误"才分得清。
  // grabBlock 找不到标记时会抛错 ⇒ 函数被改名会在**生成期**就 loudly 失败（不是静默全绿）。
  grabBlock('function statsCardsEntries('),
  grabBlock('function renderStatsReport('),
];

const header = `
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
  const m = /<div class="v[^"]*">([^<]*)<\\/div>/.exec(h.slice(i));
  return m ? m[1] : null;
}
`;

// ---------------------------------------------------------------- 沙箱 shim
// String.raw —— 里面**不能出现裸反引号**（门禁 0），也不能出现模板插值。
const shim = String.raw`
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
`;

const out = header + '\n' + shim + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('stats_disk_gen.js', out);
console.log('生成: stats_disk_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
