// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：本轮三项改动
//   1. 「待完成 / 完成后可得」必须用 pending_*（含已接单与进行中）
//   2. 被前置任务挡住时弹**持久面板**（不只是 toast）
//   3. toast 的良/恶性判定覆盖新文案
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(WEBUI, 'utf8');
// 抽取列表的静态自检：node lib_extract_check.js（不带参数）就能一键复现它的判别力
const { checkExtract } = require('./lib_extract_check.js');

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

const pieces = [
  grabBlock('function toast('),
  grabBlock('function notice('),
  grabBlock('function growthError('),
  // ⚠ maskNick 必须抽进来：renderGrowth 渲染昵称时调它
  //   （webui.html 的那一行：const name = esc(maskNick(g.nickname)) + stale;）。
  //   漏抽的后果是生成物直接 ReferenceError，整片夹具在第一条断言前就死。
  //   ⚠ 顺带一条**行为**变化：昵称现在会被打码（长昵称中间变 *），
  //   所以断言里凡是用昵称做 includes 的，名字**必须短于 5 字符**
  //   （maskNick 对 ≤4 字符原样保留；5 字符刚好也是恒等：留 3 尾 2、中间补 0 个 *）。
  grabBlock('function maskNick('),
  grabBlock('function creditsOf('),
  // ⚠ 静态自检当场挖出来的真缺口（不是为了让检查闭嘴）：
  //   renderGrowth 的**空池分支**写的是 rowPlaceholder(COLS.growth, '账号池为空')，
  //   原来两者都没提供 ⇒ 只要这个夹具渲染一次空列表，生成物就 ReferenceError。
  //   本夹具的测试体不跑空池（那条路归 gen_growth_labels.js 的 [7] 覆盖），
  //   但**依赖必须齐**：静态检查看的是"引用了谁"，不是"这次跑没跑到"。
  grabBlock('const COLS ='),
  grabBlock('function rowPlaceholder('),
  grabBlock('function renderGrowth('),
];

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '', onclick: null }; }
for (const id of ['toast','notice','noticeClose','gtask','growthCards','growthRows','growthGroups','growthAccount','growthViews','growthTaskMeta']) els[id] = mkEl(id);
els.growthViews.querySelectorAll = () => [];
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let share = [], growthSnaps = [];
function renderGrowthGroups() {}
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
function reset() { for (const k in els) { const e = els[k]; e.hidden = true; e.className = ''; e.textContent = ''; e.innerHTML = ''; e.onclick = null; } }

function snap(o) {
  return Object.assign({
    uid: 'u1', nickname: '账号 A', tasks_total: 18, tasks: [],
    claimable_count: 0, claimable_credit: 0,
    acceptable_count: 0, acceptable_credit: 0,
    streak_days: 1, energy: 5,
  }, o);
}

// ---------- 1. 「待完成」用 pending_* ----------
console.log('\n[1] 待完成 / 完成后可得 用 pending_*');
reset(); share = [];
growthSnaps = [snap({ pending_count: 5, pending_credit: 600, acceptable_count: 17, acceptable_credit: 2150 })];
renderGrowth(growthSnaps);
let cards = $('growthCards').innerHTML;
// ⚠ 形状变了、意图没变：两张卡（待完成任务 / 完成后可获得的额度）在产品里被
//   **合并**成一张「待办合计 5 任务 · +600」（webui.html:8424-8425，
//   commit f32624a「成长计划精简 —— 12 列/8 卡合并为 6 列/6 卡」）。
//   断言跟着改成"合并卡里那两个数"，但仍然守着本夹具的核心判据：
//   数必须来自 pending_*，不能是 acceptable_*（后者只数未接单的，
//   已接单/进行中的任务会从统计里消失 —— 那才是这个夹具要防的缺陷）。
ok(cards.includes('待办合计'), '含「待办合计」合并卡');
ok(cards.includes('5 任务 · +600'), '待办合计 = 「5 任务 · +600」（pending_count / pending_credit）');
ok(!cards.includes('17') && !cards.includes('2150'), '不显示 acceptable_* 的 17 / 2150');
ok(!cards.includes('待接单任务'), '旧卡名「待接单任务」已合并掉');

// 兼容旧快照：没有 pending_* 时回落到 acceptable_*
console.log('\n[1b] 旧快照回落');
reset();
growthSnaps = [snap({ acceptable_count: 7, acceptable_credit: 700 })];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('7 任务 · +700'), '缺 pending_* 时回落到 acceptable_*（不显示 0）');

// 多账号求和
console.log('\n[1c] 多账号求和');
reset();
growthSnaps = [
  snap({ uid: 'u1', pending_count: 3, pending_credit: 300 }),
  snap({ uid: 'u2', pending_count: 2, pending_credit: 200 }),
];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('5 任务 · +500'), '两个账号 pending 求和 = 5 任务 / 500 额度');

// ---------- 2. 持久通知面板 ----------
console.log('\n[2] notice() 持久面板');
reset();
notice('标题', '正文内容');
const n = $('notice');
ok(n.hidden === false, '调用后可见');
ok(n.innerHTML.includes('标题'), '含标题');
ok(n.innerHTML.includes('正文内容'), '含正文');
ok(n.innerHTML.includes('知道了'), '含关闭按钮');
ok(typeof $('noticeClose').onclick === 'function', '关闭按钮绑定了 handler');
$('noticeClose').onclick();
ok(n.hidden === true, '点「知道了」后隐藏');
// 与 toast 的区别：不应挂自动消失定时器
ok(typeof notice._t === 'undefined', 'notice 不设自动消失定时器（与 toast 相反）');
// 转义：正文里的 HTML 不能注入
reset();
notice('x', '<img src=x onerror=alert(1)>');
ok(!$('notice').innerHTML.includes('<img'), '正文被 HTML 转义（无注入）');

// ---------- 3. growthError 的分层处理 ----------
console.log('\n[3] growthError 分层');
// 3a 被前置挡住：toast 中性 + 弹面板
reset();
growthError(new Error('接单 0 个（跳过 17）；需要先完成前置任务：first_buddy（「领取一只 Buddy」—— 需在 WorkBuddy 客户端内完成）'), '接单');
ok($('toast').hidden === false, '仍给一条 toast');
ok(!$('toast').className.includes('bad'), 'toast 不打红（预期内）');
ok($('notice').hidden === false, '额外弹出持久面板');
ok($('notice').innerHTML.includes('领取一只 Buddy'), '面板里带出前置任务的中文指引');
ok($('notice').innerHTML.includes('WorkBuddy 客户端'), '面板说明要去哪里做');

// 3b 普通跳过：只 toast，不弹面板
reset();
growthError(new Error('接单 0 个（跳过 3）'), '接单');
ok($('toast').hidden === false && !$('toast').className.includes('bad'), '跳过：中性 toast');
ok($('notice').hidden === true, '跳过：不弹面板');

// 3c 真故障：红色 toast，不弹面板
reset();
growthError(new Error('接单请求失败: dial tcp timeout'), '接单');
ok($('toast').className.includes('bad'), '真故障：红色 toast');
ok($('notice').hidden === true, '真故障：不弹面板（面板只用于"去做什么"）');

// 3d 无前置信息时不弹面板
reset();
growthError(new Error('需要先完成前置任务但格式不同'), '接单');
ok($('notice').hidden === true, '缺少可解析的前置内容时不弹面板');

// ---------- 4. 静态：两条路径共用 growthError ----------
console.log('\n[4] 单一实现（避免两套提示逻辑）');
const callSites = (html.match(/growthError\(/g) || []).length;
ok(callSites >= 3, 'growthError 至少有 1 处定义 + 2 处调用（实际 ' + callSites + ' 处）');
// ⚠ 这条断言原来是对**整篇 HTML** 做全文匹配：!/全部\$\{label\}失败：/.test(html)。
//   它现在必然红，而红的**不是**成长路径 —— 那句字面量还活在 startAll()
//   （webui.html:5064，每日动作：签到 / 刷新额度）里，那是**另一个功能**，
//   本来就该自己拼自己的 toast。全文匹配把一个无关功能算进了成长的账上，
//   判据的**作用域**错了，所以它红得没有道理（"测量工具坏了"的典型形态）。
//   改成把判据钉在**成长批量路径**上：抽 growthAll 那一小段源码看它的 catch。
const gAllAt = html.indexOf('async function growthAll(');
const gAll = gAllAt < 0 ? '' : html.slice(gAllAt, gAllAt + 1200);
ok(gAllAt > 0, '页面里能定位到 growthAll（成长批量动作）');
ok(/growthError\(e,/.test(gAll), 'growthAll 的 catch 走 growthError（单一实现）');
ok(!/全部接单失败|全部领取失败/.test(html), '成长路径不再自己拼「全部X失败：」');
// ⚠ 想在模板里写「全部X失败：」那种**带美元加大括号的字面量**：不能直接写。
//   tail 是 String.raw，但 String.raw 只免转义、**不免插值** —— 美元加大括号
//   一样会被求值，生成器当场 ReferenceError（label 不存在）。
//   原断言是把 $ 与 { 各自转义后才放进正则的；本条干脆换成不含它的形态，
//   既避开这个坑，也不再被"另一个功能"的同形字符串误伤（见上一条注释）。
ok(html.includes('id="notice"'), '页面里有 #notice 元素');
ok(/\.notice\.warn\{/.test(html), '.notice.warn 样式已定义');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

// ---- 抽取列表的静态自检（本轮新加，见 lib_extract_check.js 文件头）----
//
// 判据：抽出来的块里"引用了、但抽取集与桩都没提供、也不是 JS 内建"的名字 ⇒ 当场非零退出。
// ⚠ 它只接了 4 个 growth generator，**其余 12 个没接**（父代理批的窄版本）——别以为全仓都接上了。
checkExtract({
  pieces,
  stub: header,
  productFile: WEBUI,
  label: 'gen_growth_pending_ui.js',
  extra: [],   // 夹具自有的名字才进这里，且每个都要写理由；不接受整类豁免
});

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('growth_pending_ui_gen.js', out);
console.log('生成: growth_pending_ui_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
