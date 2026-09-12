// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：验证「网关状态 / 调用统计 / API 接入信息」已合并为单个「仪表盘」面板。
//
// 断言策略：
//  A. 静态结构 —— 三个 h2 面板只剩一个「仪表盘」；三段内容都在同一个 section 内。
//  B. 导航标签 —— 抽出真实的 buildNav 逻辑里取 label 的表达式，确认它取到「仪表盘」
//     而不是把 h2 里那些 span/button 的文本也拼进去。
//  C. 唯一样例 —— #auto / #refresh 等控制元素在整页只出现一次（合并后最容易出的错是
//     留了一个孤立的旧 id 或重复绑定）。
//  D. renderCards 仍正常产出 8 张卡，并且新写的 #gwHint 三种分支文案都对。
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
function resetEls() { for (const k of Object.keys(els)) { const e = els[k]; e.hidden = true; e.className=''; e.textContent=''; e.innerHTML=''; } }
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// 取出 .content 里每一个顶层 <section>，再抽出各自的 h2 首个文本节点。
// 这正是 buildNav() 用的口径，所以这里断言的就是导航会显示什么。
function sectionsOf(src) {
  // 先剥掉 HTML 注释 —— 注释里出现的标签不算 DOM 结构。
  //
  // # 为什么必须剥（T6 踩到的真实崩溃）
  //
  // T6 在设置面板之后加了一段说明性注释，里面举例写了标签名。
  // 扫描器把注释里的那个开标签当成真实节点，配平深度永远归不了零，
  // 于是内层 while 结束时 t 仍是 null → "Cannot read properties of null"。
  //
  // 这不只是这一处的问题：任何注释里写标签都会踩到。
  // 所以在入口统一剥注释，而不是在扫描逻辑里打补丁。
  src = src.replace(/<!--[\s\S]*?-->/g, '');

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
for (const t of ['网关状态', '调用统计', 'API 接入信息']) {
  ok(dash.includes('<h3>' + t), '仪表盘内以 h3 呈现「' + t + '」');
}
ok((dash.match(/class="boardsec"/g) || []).length === 3, '仪表盘内恰好 3 个子段落 boardsec');

// 子段落的 h3 不能污染导航标签：h3 必须排在 h2 之后，且 h2 首个文本节点是「仪表盘」
ok(dash.indexOf('<h2>') < dash.indexOf('<h3>'), 'h2 在 h3 之前');
ok(navLabelOf(dash) === '仪表盘', '导航标签解析结果为「仪表盘」（不含子标题与按钮文案）');

// ---------- B. 三个宿主容器都还在且各只有一个 ----------
console.log('\n[B] 承载容器唯一性（合并最易留下的错：id 重复或被误删）');
for (const id of ['cards','statsCards','statsModels','apiCards','epRows','curlSample','rawstatus','gwHint','statsWindow']) {
  const n = (html.match(new RegExp('id="' + id + '"', 'g')) || []).length;
  ok(n === 1, '#' + id + ' 恰好出现一次（实际 ' + n + '）');
}
// 控制器只保留一份：合并后不应再有第二套刷新开关
for (const id of ['auto','refresh']) {
  const n = (html.match(new RegExp('id="' + id + '"', 'g')) || []).length;
  ok(n === 1, '#' + id + ' 恰好出现一次（刷新开关已收敛为一处，实际 ' + n + '）');
}

// 网格容器：三段各有一处 .cards，且没有残留的旧 id
ok((dash.match(/class="cards"/g) || []).length === 3, '仪表盘内 3 处 .cards（状态/统计/接入）');

// ---------- C. renderCards 仍产出 8 张卡 + gwHint 三态 ----------
console.log('\n[C] renderCards 输出与网关判读文案');
function cardCount(h, s) { resetEls(); renderCards(h, s); return ($('cards').innerHTML.match(/class="card"/g) || []).length; }
ok(cardCount({ healthy: 2 }, { total: 2, healthy: 2, cooling: 0, disabled: 0, in_flight_full: 0, sticky_sessions: 0, redis_mode: 'noop' }) === 8,
  '健康时有 8 张卡');
// /healthz 卡：healthy>0 才是 200
resetEls();
renderCards({ healthy: 1 }, { total: 1, healthy: 1, redis_mode: 'upstash' });
ok($('cards').innerHTML.includes('>200<') && $('cards').innerHTML.includes('upstash'), '健康时 /healthz=200 且 Redis 模式透出');

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
