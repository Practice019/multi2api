// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：成长计划卡片的措辞 + 接单按钮 tooltip。
//
// 背景（用户要求）：「待接单任务 / 接单后可得信用分」两张卡片改成
//   「待完成任务 / 完成后可获得的信用分」
// 理由：这些任务要先接单、再去 WorkBuddy 里把动作做掉，最后还要领奖才到账。
// 原措辞只说到"接单"，容易让人以为接完就加分（实际接单不发任何奖励）。
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

const pieces = [
  grabBlock('function creditsOf('),
  grabBlock('function renderGrowth('),
];

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '' }; }
for (const id of ['growthCards','growthRows','growthGroups','growthAccount','growthViews','growthTaskMeta']) els[id] = mkEl(id);
els.growthViews.querySelectorAll = () => [];
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let share = [], growthSnaps = [];
function renderGrowthGroups() {}
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

function renderWith(claim, credit) {
  for (const k in els) { els[k].innerHTML = ''; els[k].textContent = ''; }
  share = [];
  growthSnaps = [{
    uid: 'u1', nickname: '账号 A', tasks_total: 18, tasks: [],
    claimable_count: 0, claimable_credit: 0,
    acceptable_count: claim, acceptable_credit: credit,
    streak_days: 1, energy: 5,
  }];
  renderGrowth(growthSnaps);
  return $('growthCards').innerHTML;
}

// ---------- 1. 卡片文案 ----------
console.log('\n[1] 卡片标签');
const cards = renderWith(17, 2150);
ok(cards.includes('待完成任务'), '含「待完成任务」');
ok(cards.includes('完成后可获得的额度'), '含「完成后可获得的额度」');
ok(!cards.includes('待接单任务'), '不再有「待接单任务」');
ok(!cards.includes('接单后可得积分'), '不再有「接单后可得积分」');
// ⚠ T4 订正：这条原来断言「统一用**积分**」。T4 把界面用词统一到了
// **「额度」**（决策 5，用户原话"不要叫积分叫额度都可以"），
// 所以"统一"这个**意图**没变，变的只是统一到哪个词。
// 断言跟着改成"不再出现任何游离的『积分』说法" —— 它守的东西反而更强了：
// 改造前是"两个词里挑一个"，现在是"页面上不该再有第二个词"。
ok(!cards.includes('信用分'), '不再出现「信用分」（已废弃的第三种叫法）');
ok(!cards.includes('现在可领积分'), '不再有「现在可领积分」（T4 已统一为「现在可领额度」）');
// 数值仍然正确渲染
ok(cards.includes('>17<'), '待完成数量 17 正常渲染');
ok(cards.includes('>2150<'), '完成后可得 2150 正常渲染');

// ---------- 2. 静态检查：HTML 里也不该残留旧措辞 ----------
console.log('\n[2] 全局无残留旧措辞');
ok(!html.includes('待接单任务'), '页面无「待接单任务」');
ok(!html.includes('接单后可得积分'), '页面无「接单后可得积分」');
ok(!html.includes('<th>待接单</th>'), '表头不再是「待接单」');
ok(!html.includes('<th>接单后可得</th>'), '表头不再是「接单后可得」');
// T4：用户可见区域不该再出现「积分」。见 tests/frontend/verify_t4_t5.js
// 那条更精确的判据（它剥注释后按**渲染结果**判，不在源码上做全文匹配）。
ok(!html.includes('<th>积分</th>'), '表头不再是「积分」（T4 已统一为「额度」）');
// 表格新表头
ok(html.includes('<th>待完成</th>') && html.includes('<th>完成后可得</th>'),
   '表头已改为「待完成 / 完成后可得」');

// ---------- 3. 「接单」作为动作词仍保留（不是要删掉它） ----------
console.log('\n[3] 动作词「接单」保留');
ok(html.includes('>接单</button>'), '行内按钮仍叫「接单」（那是实际动作）');
ok(html.includes('全部接单'), '「全部接单」按钮保留');
ok(html.includes("accept: '接单'"), 'GROWTH_LABEL 里 accept 映射仍是「接单」');

// ---------- 4. 接单按钮带 tooltip 说明不发奖励 ----------
console.log('\n[4] 接单按钮 tooltip');
const m = /data-gact="accept"[^>]*title="([^"]*)"/.exec(html);
ok(!!m, '接单按钮有 title 属性');
if (m) {
  ok(m[1].includes('不发奖励'), 'tooltip 说明「不发奖励」（实际：' + m[1] + '）');
  ok(m[1].includes('领取'), 'tooltip 指引去看「领取」');
}

// ---------- 5. 渲染出的行内按钮也带 tooltip ----------
console.log('\n[5] 渲染结果里的 tooltip');
for (const k in els) { els[k].innerHTML = ''; els[k].textContent = ''; }
share = [{ uid: 'u1', nickname: '账号 A', credits: 100 }];
growthSnaps = [{
  uid: 'u1', nickname: '账号 A', tasks_total: 18, tasks: [],
  claimable_count: 0, claimable_credit: 0,
  acceptable_count: 3, acceptable_credit: 300,
  streak_days: 1, energy: 5,
}];
renderGrowth(growthSnaps);
const rows = $('growthRows').innerHTML;
ok(rows.includes('data-gact="accept"'), '行内渲染出接单按钮');
ok(/data-gact="accept"[^>]*title="[^"]*不发奖励/.test(rows), '行内接单按钮带「不发奖励」tooltip');

// ---------- 6. 错误提示的中性/红色判定要和后端文案对齐 ----------
console.log('\n[6] 前端 toast 的良/恶性判定');
// 从页面里抽出真实的 benign 正则，避免测试自说自话
const benignRe = /const benign = (\/[^/]+\/)\.test/.exec(html);
ok(!!benignRe, '页面里能抽出 benign 判定正则');
if (benignRe) {
  const re = eval(benignRe[1]);
  // 这些是"现在不该做"，应显示为中性（不打红）
  const benignMsgs = [
    '接单 0 个（跳过 17）；被前置任务 first_buddy 挡住',
    '接单 0 个（跳过 2）',
    '没有待接单的任务',
    '任务「x」当前不可接单',
    '成长次数不足',
    '该任务已领取',
  ];
  for (const m of benignMsgs) ok(re.test(m), '中性：' + m);

  // 这些是真故障，必须打红
  const failMsgs = [
    '接单请求失败: dial tcp timeout',
    '拉取任务失败: 502 Bad Gateway',
    '接单 0 个（失败 1）；最后错误 x: internal server error',
  ];
  for (const m of failMsgs) ok(!re.test(m), '打红：' + m);
}

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('growth_labels_gen.js', out);
console.log('生成: growth_labels_gen.js (' + out.length + ' 字节)');
