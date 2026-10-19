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
  // ⚠ maskNick 必须抽进来 —— renderGrowth 现在用它渲染昵称
  //   （webui.html 的那一行：const name = esc(maskNick(g.nickname)) + stale;）。
  //   不抽的后果不是"少测一条"，而是**生成物一跑就 ReferenceError**
  //   （maskNick is not defined），整个夹具在第一条断言之前就死了。
  //
  // # 这个坑的形状（第三次踩，所以写在抽取列表旁边）
  //
  // grabBlock 的抽取列表是**手工维护**的：产品里某个被抽函数新调了一个帮手，
  // 帮手不会自动跟着来。产品侧改名/新增一次，夹具就整片红 —— 而且红的位置
  // （ReferenceError）离真正的原因（抽取列表没跟上）很远，很容易误判成
  // "产品坏了"。判定顺序永远是：先在**基线工作树**上跑同一条，看是不是同样红；
  // 同样红 ⇒ 测量工具的问题，不是产品缺陷。
  grabBlock('function maskNick('),
  grabBlock('function creditsOf('),
  // ⚠ 这两个是**静态自检当场挖出来的真缺口**（不是为了让检查闭嘴才加的）：
  //   renderGrowth 的空池分支写的是 rowPlaceholder(COLS.growth, '账号池为空')，
  //   而原来的抽取列表 + 桩都没有它们 ⇒ 只要这个夹具渲染一次**空列表**，
  //   生成物就 ReferenceError。原来没人踩到，只是因为测试体从没喂过空列表
  //   （一条永远不跑的分支 = 一个永远不响的雷）。
  //   现在连它们一起抽出来，并且在测试体里**真的跑一次空池**（见 [7]）。
  grabBlock('const COLS ='),
  grabBlock('function rowPlaceholder('),
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
// ⚠ 形状变了、意图没变（2026-10 订正）。
//
// 产品把「待完成任务」+「完成后可获得的额度」两张卡**合并**成一张
// 「待办合计 17 任务 · +2150」（webui.html:8424-8425，commit f32624a
// 「成长计划精简 —— 12 列/8 卡合并为 6 列/6 卡」，用户要求少空白）。
// 所以这里断言的不再是两张卡的名字，而是**合并后那张卡的两个数**：
// 任务数来自 pending_count、额度来自 pending_credit（口径还是"没做完的"）。
ok(cards.includes('待办合计'), '含「待办合计」（待完成 + 完成后可得的合并卡）');
ok(cards.includes('17 任务 · +2150'), '待办合计 = 「17 任务 · +2150」（任务数 + 完成后可得的额度）');
ok(!cards.includes('待接单任务'), '不再有「待接单任务」');
ok(!cards.includes('接单后可得积分'), '不再有「接单后可得积分」');
// ⚠ T4 订正：这条原来断言「统一用**积分**」。T4 把界面用词统一到了
// **「额度」**（决策 5，用户原话"不要叫积分叫额度都可以"），
// 所以"统一"这个**意图**没变，变的只是统一到哪个词。
// 断言跟着改成"不再出现任何游离的『积分』说法" —— 它守的东西反而更强了：
// 改造前是"两个词里挑一个"，现在是"页面上不该再有第二个词"。
ok(!cards.includes('信用分'), '不再出现「信用分」（已废弃的第三种叫法）');
ok(!cards.includes('现在可领积分'), '不再有「现在可领积分」（T4 已统一为「现在可领额度」）');
// 0 值的「合计」卡不生成（webui.html:8430-8439）：claimable 全 0 时它是
// 「0 项 · +0」，占一格却没有任何信息量。本夹具这次把它一起钉住 ——
// 它是合并折腾之后最容易被下一次改动带回来的那张卡。
ok(!cards.includes('可领合计'), '可领合计为 0 时不生成该卡（少空白）');
ok(cards.includes('账号数'), '「账号数」卡不受 0 值过滤影响');
// ⚠ 过滤规则本身也要钉住**作用域**：只过滤名字含「合计」的卡。
// 「账号数 0」是"配了上游却没账号"的最重要信号，一刀切会掩盖真故障。
// 这条读的是产品源码里的判据（精确到一处表达式），不是对整篇做全文匹配 ——
// 全文匹配正是本文件上一次假红的成因（见 [2] 的表头断言）。
ok(/indexOf\('合计'\) >= 0/.test(html), '0 值过滤只作用于名字含「合计」的卡（账号数 0 必须留着）');

// ---------- 2. 静态检查：HTML 里也不该残留旧措辞 ----------
console.log('\n[2] 全局无残留旧措辞');
ok(!html.includes('待接单任务'), '页面无「待接单任务」');
ok(!html.includes('接单后可得积分'), '页面无「接单后可得积分」');
ok(!html.includes('<th>待接单</th>'), '表头不再是「待接单」');
ok(!html.includes('<th>接单后可得</th>'), '表头不再是「接单后可得」');
// T4：用户可见区域不该再出现「积分」。见 tests/frontend/verify_t4_t5.js
// 那条更精确的判据（它剥注释后按**渲染结果**判，不在源码上做全文匹配）。
ok(!html.includes('<th>积分</th>'), '表头不再是「积分」（T4 已统一为「额度」）');
// 表格新表头：合并成 6 列后是「待完成 / 可领」，口径解释搬进了 title
// （webui.html:1506-1507）。
//
// ⚠ 这里踩过一次假红：原来断言写的是 '<th>待完成</th>' —— 表头**加了 title
//   属性**之后就匹配不上了，看起来像"产品把表头改回旧词了"，其实一个字符都
//   没丢。字面量断言必须跟着**实际标记**走，或者只用得上名字的部分。
//   用 title 里的口径解释当锚点反而更强：它同时钉住"表头叫什么"和
//   "界面必须解释 +N 是什么意思"。
ok(/<th title="[^"]*\+N 是完成后可获得的额度">待完成<\/th>/.test(html),
   '表头「待完成」带口径说明（+N = 完成后可获得的额度）');
ok(/<th title="[^"]*">可领<\/th>/.test(html), '表头「可领」带口径说明');

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

// ---------- 7. 空池路径（静态自检逼出来的一条真分支） ----------
//
// 这条分支原来**一次都没跑过**：抽取列表里没有 rowPlaceholder / COLS，
// 测试体也从没喂过空列表 ⇒ "渲染空池"这条路从生成到现在没有任何覆盖，
// 一跑就是 ReferenceError。lib_extract_check.js 在生成阶段把它点了出来。
// 现在"两条都补"：抽真函数 + 真的跑一次。
console.log('\n[7] 空池占位');
for (const k in els) { els[k].innerHTML = ''; els[k].textContent = ''; }
share = [];
renderGrowth([]);
const emptyCards = $('growthCards').innerHTML;
const emptyRows = $('growthRows').innerHTML;
ok(emptyCards.includes('账号数'), '空池时仍画「账号数」卡');
ok(!emptyCards.includes('可领合计'), '空池时不画「可领合计」（0 值合计卡不生成）');
ok(emptyRows.includes('账号池为空'), '空池时表格给出占位文案');
// colspan 必须跟着 COLS.growth 走，不能写死 —— 成长表精简过一次列数（12 → 6），
// 写死的数字不会跟着变（本条与 gen_ui_manifest 的 colspan 漂移守卫同类，只是就地）。
ok(emptyRows.includes('colspan="' + COLS.growth + '"'), '空池占位的 colspan == COLS.growth（' + COLS.growth + '，不写死）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

// ---- 抽取列表的静态自检（本轮新加）----
//
// 这里用的是 tests/frontend/lib_extract_check.js：静态扫一遍"抽出来的块引用了哪些
// 名字，谁提供"，缺了就**当场非零退出**并点名说清怎么补。
//
// ⚠ 为什么值得多这一行：maskNick 那次四个夹具一起红，报错是
//   「ReferenceError: maskNick is not defined」—— 位置离原因（抽取列表）很远，
//   而且**第一条断言都没跑到**，很容易被误判成"产品坏了"。这条检查让它在
//   **生成阶段**就用一句人话报出来（缺谁、被哪个块引用、该加什么）。
//
// ⚠ 它只接了 4 个 growth generator（父代理批的窄版本），**其余 12 个没接** ——
//   别以为全仓都接上了，见 lib_extract_check.js 文件头那一节。
const { checkExtract } = require('./lib_extract_check.js');

checkExtract({
  pieces,
  stub: header,
  productFile: path,
  label: 'gen_growth_labels.js',
  // extra：夹具自己提供的名字（**每个都要有理由**，不接受整类豁免）。
  // 目前为空 —— 报出来的每一个都要么补桩、要么补抽取，没有"算了"这一档。
  extra: [],
});

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('growth_labels_gen.js', out);
console.log('生成: growth_labels_gen.js (' + out.length + ' 字节)');
