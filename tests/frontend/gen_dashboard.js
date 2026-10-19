// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：验证「网关状态 / 调用统计 / API 接入信息」已合并为单个「仪表盘」面板。
//
// 断言策略（2026-10-19 复核：已与产品当前契约对齐；每处改动都留了历史口径，见各段注释）：
//  A. 静态结构 —— 三个 h2 面板只剩一个「仪表盘」；三段内容都在同一个 section 内，
//     且仪表盘的子段落集合与顺序被钉住。
//  B. 导航标签 —— 抽出真实的 buildNav 逻辑里取 label 的表达式，确认它取到「仪表盘」
//     而不是把 h2 里那些 span/button 的文本也拼进去。
//  C. 唯一性 —— 各承载容器 / 控制元素在整页只出现一次（合并后最容易出的错是
//     留了一个孤立的旧 id 或重复绑定）；产品已整体移除的旧 id 断言为 0 次。
//  D. renderCards 产出的卡片集合（7 张，含每账号画像聚合）与 #gwHint 三种分支文案都对。
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

const renderCardsSrc = grabBlock('function renderCards(');

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '' }; }
for (const id of ['cards','gwHint']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
// window 是浏览器全局：renderCards 会读 window._statsCache（renderStats 在 webui.html:9558 写入）。
// Node 里没有这个全局 —— 不补就是 "ReferenceError: window is not defined"，那是 stub 缺口，
// 不是产品缺陷。形状照真实的来：平时 null，拿到 /admin/stats 后才是一个 {total, ok, ...} 对象。
const window = { _statsCache: null };
function resetEls() { for (const k of Object.keys(els)) { const e = els[k]; e.hidden = true; e.className=''; e.textContent=''; e.innerHTML=''; } window._statsCache = null; }
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// markupOnly：只留下「浏览器初次解析时会变成 DOM 节点」的那部分文本。
//
// # 哪些文本不是节点（不剥掉，扫描器看到的就是假结构）
//
//   1. HTML 注释      —— T6 踩过：注释里举例写了标签名，扫描器把它当真实节点。
//   2. script 正文    —— script data state 下里面的标签字样只是 JS 注释/字符串，
//                        **不会**生成任何元素。
//   3. style 正文     —— CSS 注释/选择器里同样可能出现标签名。
//
// # 第 2 条是本次的真实根因（不是 DOM stub 少元素/少属性）
//
// webui.html:3118 有一行 JS 注释，举例写了一个 section 开标签（带 class="acctgroup"）。
// 它在 script 正文里，浏览器只当注释文本；本函数旧版只剥 HTML 注释，
// 于是把它当成一个真实开标签 ⇒ 配平深度归不了零 ⇒ 内层 while 结束时 t 为 null
// ⇒ 崩在 t.index 上，报出 "Cannot read properties of null (reading 'index')"。
// 症状看起来像产品代码读了某个元素的 .index，其实是**测量工具自己**崩了 ——
// 产品代码里唯一的 .index 读点在 webui.html:5672，读的是数组元素 x.index，与此无关。
//
// 反向也查过：4334 / 6112 是模板字面量里拼出来的 section 开标签（配 4380 / 6121
// 的闭合标签），它们成对，只贡献"数量"不破坏"结构"；
// 真正打破配平的只有 3118 那个孤立的开标签。
//
// 剥完之后的静态 section 应为 10 个（仪表盘/API 管理/账号池/…/设置），与真实页面一致。
// 三种剥除顺序（注释先/脚本先/两轮）都实测过，结论相同。
function markupOnly(src) {
  // 两轮：注释里可能提到 <script>（被整段注释掉的脚本），
  // 脚本里也可能出现 <!-- 字样。单轮剥除对这两种写法会留残渣。
  // 顺序上脚本/样式先剥：在 script data state 里 HTML 注释规则不生效。
  for (let pass = 0; pass < 2; pass++) {
    src = src.replace(/<script[\s\S]*?<\/script>/gi, '');
    src = src.replace(/<style[\s\S]*?<\/style>/gi, '');
    src = src.replace(/<!--[\s\S]*?-->/g, '');
  }
  return src;
}

function sectionsOf(src) {
  src = markupOnly(src);

  // 测量工具的前置自检：剥完之后 <section ...> 与 </section> 必须成对。
  // 不成对说明"剥得不够"（又有新的非节点文本混进来了）—— 这里显式报错，
  // 而不是让 t 变成 null 后崩在 t.index 上、把原因伪装成产品缺陷。
  const opens = (src.match(/<section(?:\s[^>]*)?>/g) || []).length;
  const closes = (src.match(/<\/section>/g) || []).length;
  if (opens !== closes) {
    throw new Error('扫描器口径失效：<section> 开标签 ' + opens + ' 个、闭合 ' + closes
      + ' 个（不成对）。多半是 markupOnly 少剥了一类"不是节点"的文本；'
      + '别去改产品代码。');
  }

  const out = [];
  // 匹配带属性的 <section ...>，而不只是裸 <section>。
  //
  // # 为什么必须放宽（T4 踩到的真实回归）
  //
  // T4 给通用面板加了标识（形如 section 上多了 data-key="dashboard"）。
  // 本函数原先只认字面量裸标签，于是**一个 section 都匹配不到** ——
  // 后面所有断言拿到空数组后集体变红，看起来像产品坏了，
  // 其实是测试的解析器太脆。
  //
  // 教训：解析 HTML 时不要假设标签上没有属性。属性随时会加。
  // 另注：本文件是 String.raw 模板，注释里**不能出现反引号** ——
  // 反引号会提前终止模板（这个坑刚踩过，generator 静默产出陈旧文件）。
  const re = /<section(?:\s[^>]*)?>/g;
  let m;
  while ((m = re.exec(src))) {
    const openTag = m[0];
    // 手工配平 <section ...>...</section>
    let i = m.index + openTag.length, depth = 1;
    const tag = /<\/?section(?:\s[^>]*)?>/g;
    tag.lastIndex = i;
    let t;
    while ((t = tag.exec(src))) {
      // 用 <\/ 开头判定闭合，避免把带属性的开标签误判成闭合
      depth += t[0].startsWith('</') ? -1 : 1;
      if (depth === 0) break;
    }
    // 兜底：上面的成对自检已经拦住了这种情况，这里再留一句可读报错，
    // 免得将来有人改动剥除逻辑后，报错又变回一条看不懂的 TypeError。
    if (!t) throw new Error('section 未闭合，起始于第 ' + (src.slice(0, m.index).split('\n').length) + ' 行');
    out.push(src.slice(m.index, t.index + t[0].length));
  }
  return out;
}
function navLabelOf(sec) {
  const h2 = /<h2>([\s\S]*?)<\/h2>/.exec(sec);
  if (!h2) return null;
  // 模拟 childNodes[0].textContent：h2 开标签后直到第一个 '<' 的那段文本
  const inner = h2[1];
  const cut = inner.indexOf('<');
  return (cut < 0 ? inner : inner.slice(0, cut)).trim();
}

// ---------- A. 结构：三个面板合并成一个 ----------
console.log('\n[A] 面板块数与标题');
const sections = sectionsOf(html);
const labels = sections.map(navLabelOf);
ok(labels.includes('仪表盘'), '存在「仪表盘」面板');
ok(!labels.includes('网关状态'), '已无独立「网关状态」面板');
ok(!labels.includes('调用统计'), '已无独立「调用统计」面板');
ok(!labels.includes('API 接入信息'), '已无独立「API 接入信息」面板');
ok(labels.filter(l => l === '仪表盘').length === 1, '「仪表盘」只出现一次');

// 三段内容必须在同一个 section 内 —— 用「同一 section 里同时含三个 h3」来判
const dash = sections.find(s => navLabelOf(s) === '仪表盘');
ok(!!dash, '能定位到仪表盘 section');
// 定位不到就直接停在这里并说清原因：否则下一行 dash.includes 会抛 TypeError，
// 看起来又像"产品读了 null 的属性"，其实只是面板被改名/删掉了（先看上面那条 FAIL）。
if (!dash) throw new Error('定位不到「仪表盘」section：看上面那条 FAIL，面板或它的 h2 被改名/删除了。');
for (const t of ['网关状态', '调用统计', 'API 接入信息']) {
  ok(dash.includes('<h3>' + t), '仪表盘内以 h3 呈现「' + t + '」');
}
ok((dash.match(/class="boardsec"/g) || []).length === 7, '仪表盘内恰好 7 个子段落 boardsec');
// 子段落**集合与顺序**也钉住：只数个数的话，换掉一个子段落的名字（等于换了一块内容）
// 依然是绿的 —— 那条判据就没有判别力。
//
// ⚠ 为什么是 7 而不是最初合并时的 3（历史口径，别当成"数字被改大了"）：
//   f1d4326 把三段合并成 1 个面板 / 3 个子段落；
//   df3693e、957a846、b7f4fe0、18e9278（融合 agent2api 报表页那批）陆续加到 7 个。
//   再动这里 = 动了仪表盘的结构，应当复核导航标签与下面的容器断言。
const titles = (dash.match(/<h3>([^<]*)/g) || []).map(x => x.slice(4).trim());
ok(titles.join('/') === '网关状态/调用统计/活跃热力图/缓存命中率/近 24 小时缓存命中率趋势/按天 Token 趋势/API 接入信息',
  '子段落标题与顺序符合当前契约（实际：' + titles.join('/') + '）');

// 子段落的 h3 不能污染导航标签：h3 必须排在 h2 之后，且 h2 首个文本节点是「仪表盘」
ok(dash.indexOf('<h2>') < dash.indexOf('<h3>'), 'h2 在 h3 之前');
ok(navLabelOf(dash) === '仪表盘', '导航标签解析结果为「仪表盘」（不含子标题与按钮文案）');

// ---------- B. 三个宿主容器都还在且各只有一个 ----------
console.log('\n[B] 承载容器唯一性（合并最易留下的错：id 重复或被误删）');
// ⚠ statsModels 已从本列表移出：2026-10-18 用户要求删掉「模型明细」chips
//    （webui.html:1195-1200，renderStatsModels 函数体保留但不再被调用）
//    ⇒ 它现在是"应为 0 次"的旧 id，见下面那组。替它占位的是缓存四格 #cacheCards（957a846）。
for (const id of ['cards','statsCards','cacheCards','apiCards','epRows','curlSample','rawstatus','gwHint','statsWindow']) {
  const n = (html.match(new RegExp('id="' + id + '"', 'g')) || []).length;
  ok(n === 1, '#' + id + ' 恰好出现一次（实际 ' + n + '）');
}
// 控制器只保留一份：合并后不应再有第二套刷新开关。
// #auto 是旧的「每 5s 刷新」勾选框，已由 aa2fc7f 整体移除（webui.html:12254 有说明）
// ⇒ 断言 0 次：防的是"HTML 回来了但接线没回来"这种半截改动。
for (const id of ['refresh']) {
  const n = (html.match(new RegExp('id="' + id + '"', 'g')) || []).length;
  ok(n === 1, '#' + id + ' 恰好出现一次（刷新入口已收敛为手动一处，实际 ' + n + '）');
}
for (const id of ['auto','statsModels','statsModelsWrap']) {
  const n = (html.match(new RegExp('id="' + id + '"', 'g')) || []).length;
  ok(n === 0, '#' + id + ' 已整体移除（实际 ' + n + ' 次）');
}

// 网格容器：仪表盘内 4 处 .cards（状态 / 统计 / 缓存四格 / 接入），且没有残留的旧 id
ok((dash.match(/class="cards"/g) || []).length === 4, '仪表盘内 4 处 .cards（状态/统计/缓存四格/接入）');

// ---------- C. renderCards 的卡片集合 + gwHint 三态 ----------
console.log('\n[C] renderCards 输出与网关判读文案');
// 作者侧辅助：把卡片渲染结果解析成 { 标签: {cls, v} } 与"标签顺序"，
// 这样断言的既不是一坨 HTML 字符串，也不只是个数。
function cardMap(html) {
  const out = {};
  const re = /<div class="k">([^<]*)<\/div><div class="v ?([^"]*)">([^<]*)<\/div>/g;
  let m;
  while ((m = re.exec(html))) out[m[1]] = { cls: m[2].trim(), v: m[3] };
  return out;
}
function cardLabels(html) { return (html.match(/<div class="k">[^<]*<\/div>/g) || []).map(x => x.replace(/<[^>]*>/g, '')); }
function cardsOf(h, s) { resetEls(); renderCards(h, s); return $('cards').innerHTML; }

// 管理员模式（s 有值）的卡片集合与顺序。
//
// ⚠ 历史口径：旧版是 8 张（账号总数/健康/冷却中/已禁用/在途占满/粘性会话/Redis//healthz），
//   判据原文写的是"健康时有 8 张卡"。07d3f93 之后卡片换成了
//   运营/排障口径的 7 张，且冷却/禁用/在途不再是 s 上的预聚合字段，
//   而是从 s.accounts 的**每账号画像**现算（webui.html:2222-2225）。
//   所以这里断言"7 张 + 标签顺序固定"，比原来的"数 8 张"更能抓住"卡片被换掉/漏掉"。
const adminS = {
  total: 5, healthy: 4,
  accounts: [{ cooling: true }, { cooling: true }, { disabled: true }, { in_flight: 1 }, { in_flight: 2 }],
};
const cards = cardsOf({ healthy: 4, total: 5 }, adminS);
ok(cardLabels(cards).join('/') === '账号总数/健康/冷却中/已禁用/在途请求/累计请求/成功率',
  '管理员模式渲染 7 张卡且标签顺序固定（实际：' + cardLabels(cards).join('/') + '）');
const cm = cardMap(cards);
// 每账号画像聚合：冷却 2 / 禁用 1 / 在途 1+2=3，沿用各自的配色档位
ok(cm['冷却中'].v === '2' && cm['冷却中'].cls === 'warn', '冷却中按每账号画像聚合为 2 且标 warn（实际：' + cm['冷却中'].v + '/' + cm['冷却中'].cls + '）');
ok(cm['已禁用'].v === '1' && cm['已禁用'].cls === 'err', '已禁用按每账号画像聚合为 1 且标 err（实际：' + cm['已禁用'].v + '/' + cm['已禁用'].cls + '）');
ok(cm['在途请求'].v === '3' && cm['在途请求'].cls === 'warn', '在途请求是各账号 in_flight 之和 3 且标 warn（实际：' + cm['在途请求'].v + '/' + cm['在途请求'].cls + '）');
// 健康档位：healthy>0 ⇒ 绿。旧版这条由 /healthz 卡（200/503）承担，那张卡已删。
ok(cm['健康'].v === '4' && cm['健康'].cls === 'ok', '健康>0 ⇒ 健康卡标 ok（实际：' + cm['健康'].v + '/' + cm['健康'].cls + '）');

// 外部读数透出：旧版由「Redis 模式」卡（upstash/noop）与「/healthz=200」卡证明。
// 两张卡都随 07d3f93 之后的改版删了（Redis 模式按 c1cb20f 隐藏），
// 现在同一职责由「累计请求 / 成功率」承担 —— 它们读 window._statsCache
// （renderStats 写入，见 webui.html:9558 与 2226-2228）。判据的**意图**不变：
// "renderCards 真的把外部读数渲染进卡片，而不是恒渲染 —"。
resetEls();
window._statsCache = { total: 200, ok: 190 };
renderCards({ healthy: 4, total: 5 }, adminS);
const withStats = cardMap($('cards').innerHTML);
ok(withStats['累计请求'].v === '200' && withStats['成功率'].v === '95%',
  'stats 缓存透出：累计请求 200 / 成功率 95%（实际：' + withStats['累计请求'].v + ' / ' + withStats['成功率'].v + '）');
ok($('cards').innerHTML.includes('>200<'), '累计请求以 >200< 的数值形状渲染（不是 undefined）');

// 没有 stats 缓存时必须是 —，不是 undefined / NaN
resetEls();
renderCards({ healthy: 4, total: 5 }, adminS);
const noStats = cardMap($('cards').innerHTML);
ok(noStats['累计请求'].v === '—' && noStats['成功率'].v === '—',
  '没有 stats 缓存时两项显示 —（实际：' + noStats['累计请求'].v + ' / ' + noStats['成功率'].v + '）');

// 成员模式（/status 403 ⇒ s 为 null）：整块换成一句说明，既不抛异常也不渲染一排 —
// （webui.html:2211-2217。这条是改版后新增的分支，旧夹具没有覆盖。）
resetEls();
let memberThrew = '';
try { renderCards({ healthy: 0, total: 0 }, null); } catch (e) { memberThrew = String(e && e.message || e); }
ok(!memberThrew, '成员模式下 renderCards 不抛异常（实际：' + (memberThrew || '未抛') + '）');
ok((($('cards').innerHTML.match(/class="card"/g) || []).length === 1) && $('cards').innerHTML.includes('成员'),
  '成员模式渲染 1 张说明卡并点明「成员」（实际：' + $('cards').innerHTML.slice(0, 80) + '）');

console.log('\n[C2] #gwHint 三种分支');
resetEls(); renderCards({ healthy: 2, total: 3 }, { total: 3, healthy: 2 });
ok($('gwHint').textContent === '正在服务 · 2/3 个账号可用', '有可用账号 → 正在服务 2/3（实际：' + $('gwHint').textContent + '）');

resetEls(); renderCards({ healthy: 0, total: 2 }, { total: 2, healthy: 0 });
ok($('gwHint').textContent === '无可用账号 · 请求会返回 503', '账号在池但全不可用 → 提示 503（实际：' + $('gwHint').textContent + '）');

resetEls(); renderCards({ healthy: 0, total: 0 }, { total: 0, healthy: 0 });
ok($('gwHint').textContent === '账号池为空', '池为空 → 账号池为空（实际：' + $('gwHint').textContent + '）');

// total 缺失时不应渲染出 undefined
resetEls(); renderCards({ healthy: 1 }, { healthy: 1 });
ok(!$('gwHint').textContent.includes('undefined'), "total 缺失时 gwHint 不出现 undefined（实际：" + $('gwHint').textContent + '）');

// ---------- D. 不引入 emoji，且子标题样式已定义 ----------
console.log('\n[D] 样式与风格约定');
ok(/\.boardsec\s+h3\s*\{/.test(html), '.boardsec h3 样式已定义');
ok(/\.boardsec\s*\+\s*\.boardsec\s*\{[^}]*border-top/.test(html), '子段落之间以 border-top 分隔');
const emoji = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}]/u;
ok(!emoji.test(dash), '仪表盘段落内无 emoji');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + '\n' + renderCardsSrc + '\n' + tail;
fs.writeFileSync('dashboard_gen.js', out);
console.log('生成: dashboard_gen.js (' + out.length + ' 字节)');
console.log('  [renderCards] ' + renderCardsSrc.split('\n')[0].trim());
