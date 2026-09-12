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
  grabBlock('function creditsOf('),
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
ok(cards.includes('>5<'), '待完成用 pending_count=5（不是 acceptable 的 17）');
ok(cards.includes('>600<'), '完成后可得用 pending_credit=600（不是 2150）');
ok(!cards.includes('>17<'), '不显示 acceptable_count=17');
ok(!cards.includes('>2150<'), '不显示 acceptable_credit=2150');
ok(cards.includes('待完成任务') && cards.includes('完成后可获得的积分'), '标签文案正确');

// 兼容旧快照：没有 pending_* 时回落到 acceptable_*
console.log('\n[1b] 旧快照回落');
reset();
growthSnaps = [snap({ acceptable_count: 7, acceptable_credit: 700 })];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('>7<') && cards.includes('>700<'), '缺 pending_* 时回落到 acceptable_*（不显示 0）');

// 多账号求和
console.log('\n[1c] 多账号求和');
reset();
growthSnaps = [
  snap({ uid: 'u1', pending_count: 3, pending_credit: 300 }),
  snap({ uid: 'u2', pending_count: 2, pending_credit: 200 }),
];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('>5<'), '两个账号 pending 求和 = 5');
ok(cards.includes('>500<'), '两个账号 credit 求和 = 500');

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
ok(!/全部\$\{label\}失败：/.test(html), '「全部X失败：」旧写法已移除（改走 growthError）');
ok(html.includes('id="notice"'), '页面里有 #notice 元素');
ok(/\.notice\.warn\{/.test(html), '.notice.warn 样式已定义');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('growth_pending_ui_gen.js', out);
console.log('生成: growth_pending_ui_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
