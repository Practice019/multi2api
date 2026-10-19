// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 本轮前端改动测试：
//   1. accepted 显示为「待完成」而非「进行中」（in_progress 才叫进行中）
//   2. 任务进度条渲染（target>0 画条，target=0 不画）
//   3. accepted/in_progress 不给按钮，改显示「去客户端做」
//   4. 接单按钮是次要样式（ghost），且在 not_accepted 时仍可用
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

// 以下状态与常量**从页面真实源码里抽出**，不手抄 —— 手抄会随实现漂移。
// grabBlock 从标记处起找到第一个 '{' 再配平到对应的 '}'，所以标记可以是
// "const GROWTH_VIEWS =" 或 "function growthCounts"。
const pieces = [
  grabBlock('const GROWTH_VIEWS ='),
  grabBlock('const GROWTH_STATUS_TEXT ='),
  grabBlock('function growthCounts('),
  // renderGrowthGroups 现在依赖 fmtTaskExpiry（「到期」列）。
  // 教训（第二次踩）：从真实源码抽函数时，必须连**它调用的函数**一起抽，
  // 否则生成物一跑就 ReferenceError。
  grabBlock('function fmtTaskExpiry('),
  // maskNick：renderGrowthGroups 渲染账号名时调它（第三次同款踩坑）。
  // 抽取列表是手工维护的，产品里新加一层调用，夹具这边不会自动跟上。
  grabBlock('function maskNick('),
  grabBlock('function renderGrowthGroups('),
];

const header = `
// ⚠⚠ 这个模板里（**含注释**）绝不能出现裸反引号：本次修夹具时我在下面的注释里
//    写了一个，generator 立刻 SyntaxError（Unexpected identifier）——
//    而 run_frontend_suites.js 的**门禁 0**（findRawTemplateBacktick）就是为这个坑
//    设的：generator 挂了但上一次的产物还在，直接跑产物照样全绿，
//    于是"以为产品坏了，其实是测量工具坏了"。要引用代码片段就用「」括起来。
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '' }; }
for (const id of ['growthGroups','growthAccount','growthViews','growthTaskMeta','growthCards','growthRows']) els[id] = mkEl(id);
els.growthViews.querySelectorAll = () => [];
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let growthSnaps = [], growthView = 'all', growthViewPicked = true, growthUID = '';
// autoCodes：可自动化任务码集合，产品里是 "let autoCodes = new Set();"。
// 它是**声明语句**不是函数块，grabBlock 按花括号配平，抓不了它 ⇒ 在桩里给空集合。
// ⚠ 空集合是有语义的：canAuto 因此恒为 false，行内按钮走「去客户端做」那一支 ——
//   正是本夹具要判定的分支。想测「一键完成」按钮得先把集合填上（另一个夹具的事）。
// ⚠ autoCodesLoaded 也一起给：产品里它跟着 autoCodes 成对出现，缺了会再次 ReferenceError。
let autoCodes = new Set(), autoCodesLoaded = false;
function creditsOf() { return null; }
function buildGrowthAccountSelect() {}
function syncGrowthViewButtons() {}
// renderGrowthGroups 里会调它渲染「本机登录」行；本测试不关心那部分，给个空实现。
function clientLoginHTML() { return ''; }
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

function render(tasks) {
  growthSnaps = [{
    uid: 'u1', nickname: '账号 A', tasks_total: tasks.length, tasks,
    claimable_count: 0, claimable_credit: 0, acceptable_count: 0, acceptable_credit: 0,
    streak_days: 1, energy: 5,
  }];
  growthView = 'all'; growthUID = 'u1';
  renderGrowthGroups();
  return $('growthGroups').innerHTML;
}

// ---------- 1. accepted 的文案 ----------
console.log('\n[1] accepted = 待完成（不是进行中）');
let h = render([
  { task_code: 'a', status: 'accepted', title: 'A', reward_credit: 100, target: 5, current: 0 },
]);
ok(h.includes('待完成'), 'accepted 显示「待完成」');
ok(!/>进行中</.test(h), 'accepted 不显示「进行中」');
h = render([
  { task_code: 'b', status: 'in_progress', title: 'B', reward_credit: 100, target: 5, current: 3 },
]);
ok(h.includes('进行中'), 'in_progress 显示「进行中」');

// ---------- 2. 进度条 ----------
console.log('\n[2] 进度条');
h = render([
  { task_code: 'c', status: 'in_progress', title: 'C', reward_credit: 100, target: 5, current: 3 },
]);
ok(h.includes('3/5'), '显示 3/5');
ok(h.includes('class="pbar"'), '有进度条容器');
ok(/class="pbar"[^>]*>\s*<i style="width:60%">/.test(h.replace(/\s+/g, ' ')), '宽度 60%（3/5）');
// target=0 不画条
h = render([
  { task_code: 'd', status: 'accepted', title: 'D', reward_credit: 0, target: 0, current: 0 },
]);
ok(!h.includes('class="pbar"'), 'target=0 时不画进度条（避免 0/0 假进度）');
// 边界：current 超过 target 时封顶 100%
h = render([
  { task_code: 'e', status: 'in_progress', title: 'E', reward_credit: 100, target: 3, current: 99 },
]);
ok(/width:100%/.test(h), 'current>target 时封顶 100%（不画出界条）');

// ---------- 3. 按钮策略 ----------
console.log('\n[3] 操作列');
h = render([
  { task_code: 'f', status: 'completed', title: 'F', reward_credit: 300, target: 1, current: 1, claimable: true },
]);
ok(h.includes('data-gclaimreward="f"'), 'completed 给「领取」按钮');
ok(h.includes('领取 300 分'), '按钮带金额');

h = render([
  { task_code: 'g', status: 'accepted', title: 'G', reward_credit: 100, target: 5, current: 0 },
]);
ok(!h.includes('data-gaccept="g"'), 'accepted 不给接单按钮（已经接过了）');
ok(h.includes('去客户端做'), 'accepted 提示「去客户端做」');

h = render([
  { task_code: 'h', status: 'in_progress', title: 'H', reward_credit: 100, target: 5, current: 2 },
]);
ok(!h.includes('data-gaccept="h"'), 'in_progress 不给接单按钮');
ok(h.includes('去客户端做'), 'in_progress 也提示「去客户端做」');

h = render([
  { task_code: 'i', status: 'not_accepted', title: 'I', reward_credit: 100, target: 5, current: 0 },
]);
ok(h.includes('data-gaccept="i"'), 'not_accepted 给「接单」按钮');
ok(/data-gaccept="i"[^>]*class="ghost"/.test(h), '接单按钮是次要样式 ghost');

h = render([
  { task_code: 'j', status: 'claimed', title: 'J', reward_credit: 100, target: 1, current: 1 },
]);
ok(!h.includes('data-gaccept="j"') && !h.includes('data-gclaimreward="j"'), 'claimed 无按钮');

// L 锁定任务不给接单
h = render([
  { task_code: 'k', status: 'not_accepted', title: 'K', reward_credit: 100, locked: true, target: 5 },
]);
ok(!h.includes('data-gaccept="k"'), '未解锁的任务不给接单按钮');
ok(h.includes('未解锁'), '显示「未解锁」标记');

// ---------- 4. 静态：设置项说明与 CSS ----------
console.log('\n[4] 设置项与样式');
// ⚠ 这条断言原来是 /setGrowthAccept[\s\S]{0,200}官方前端没有接单按钮/ ——
//   那个设置项**已经不在了**：commit e95b814「设置页再隐藏 3 个纯收益开关」
//   把「自动领取奖励」「自动接单」从设置页拿掉了（用户决定；理由写在
//   webui.html:1631-1632 的注释里，后端 key growth_auto_accept_tasks 仍在
//   internal/workbuddy/settings.go:37，只是不再有 UI）。
//   ⇒ 断言的**作用域**错了（守着一个被有意移除的 UI 元素），所以改成守**现在的决定**：
//     纯收益开关不展示，而「接单」这个**动作**必须还在 —— 那才是本面板的能力。
ok(!/id="setGrowthAccept"/.test(html), '设置页不再有「自动接单」开关（纯收益项不展示）');
ok(!/id="setGrowthClaim"/.test(html), '设置页不再有「自动领取奖励」开关（同一批隐藏）');
ok(/id="setGrowthMakeup"/.test(html) && /id="setGrowthRedeem"/.test(html)
   && /id="setGrowthOpen"/.test(html) && /id="setGrowthDraw"/.test(html),
   '四个「消耗资源」开关仍在（补签 / 连登兑换 / 开盲盒 / 抽奖）');
ok(html.includes('全部接单'), '「全部接单」按钮仍在（隐藏的是开关，不是动作）');
// 接单按钮为什么必须有：官方前端里根本没有这个状态（webui.html:8562）。
// 这句注释是"当初为什么做它"的唯一记录，钉住它 —— 否则下次有人会把整个接单掰掉。
ok(html.includes('官方前端没有这个状态'), '页面里保留了「官方前端没有接单状态」的理由');
ok(/\.pbar\{/.test(html) && /\.pbar i\{/.test(html), '.pbar 样式已定义');
ok(/\.ghost\{/.test(html), '.ghost 样式已定义');

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
  label: 'gen_growth_taskview.js',
  extra: [],   // 夹具自有的名字才进这里，且每个都要写理由；不接受整类豁免
});

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('growth_taskview_gen.js', out);
console.log('生成: growth_taskview_gen.js (' + out.length + ' 字节)');
