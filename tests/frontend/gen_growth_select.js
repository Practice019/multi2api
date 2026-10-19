// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成一个自包含的测试脚本：把 webui.html 里**真实的产品函数**抽出来，
// 配最小 DOM 桩 + 真实 API 数据跑断言。抽出的是产品代码本身，不是副本。
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(path, 'utf8');
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
  grabBlock('const GROWTH_VIEWS = {'),
  grabBlock('const GROWTH_STATUS_TEXT = {'),
  // fmtTaskExpiry 被 renderGrowthGroups 调用（任务到期列）。
  // 它原先不在抽取列表里 —— 那是因为本脚本以前读的是**另一个仓库**的
  // webui.html（见 gen_backup_wrongrepo），那份文件当时还没有这一列。
  grabBlock('function fmtTaskExpiry('),
  // maskNick：renderGrowthGroups / buildGrowthAccountSelect 都靠它渲染账号名
  // （const name = esc(maskNick(g.nickname))，产品里 8 处引用）。
  // 抽取列表手工维护 ⇒ 产品新加一层调用，夹具不会自动跟上，表现是
  // 生成物第一行断言之前就 ReferenceError。
  grabBlock('function maskNick('),
  grabBlock('function syncGrowthViewButtons('),
  grabBlock('function growthCounts('),
  grabBlock('function buildGrowthAccountSelect('),
  grabBlock('function clientLoginHTML('),
  grabBlock('function renderGrowthGroups('),
];

const header = `
const fs = require('fs');
// ⚠ 密钥**绝不**写进源码。此前这份文件里直接写了一串真实 api_key，
// 随测试套件一起提交进了版本库（被 acceptance.js 的密钥扫描抓到）。
// 现在从环境变量取；未设置时用一个明显无效的占位值 ——
// 断言不依赖密钥的真实性（它只测渲染逻辑）。
const KEY = process.env.WB2API_KEY || 'test-key-not-real';
let growthSnaps = [], growthUID = '', growthView = 'acceptable', growthViewPicked = false;
let clientStatus = null;
// autoCodes：可自动化任务码集合（产品里是 let autoCodes = new Set();）。
// 它是**声明语句**、不是函数块，grabBlock 按花括号配平 ⇒ 抓不了，只能在桩里给。
// ⚠ 给空集合是有语义的：renderGrowthGroups 里 canAuto 恒 false，
//   行内按钮落到「去客户端做」/「接单」分支 —— 本夹具判定的就是这些分支。
let autoCodes = new Set(), autoCodesLoaded = false;
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
const elements = {};
function resetEls() {
  elements.growthAccount = { innerHTML: '', value: '' };
  elements.growthViews = { querySelectorAll: () => [] };
  elements.growthTaskMeta = { textContent: '' };
  elements.growthGroups = { innerHTML: '' };
}
resetEls();
const $ = id => elements[id];
// 本机登录状态由**合成夹具**提供，不请求任何在跑的实例。
//
// 原实现是 fetch('http://127.0.0.1:7863/admin/client-login') —— 7863 是
// 用户的生产网关：测试会在别人机器上失败，并把真实账号名带进断言输出。
//
// 字段形状按 clientLoginHTML 的真实契约：
//   enabled / current{uid,nickname} / client_running / candidates[]{uid,valid,current}
async function fetchClientStatus() {
  return {
    enabled: true,
    current: { uid: 'test-uid-0002', nickname: '测试账号乙' },
    client_running: false,
    has_backup: false,
    candidates: [
      { uid: 'test-uid-0001', nickname: '测试账号甲', valid: true, current: false },
      { uid: 'test-uid-0002', nickname: '测试账号乙', valid: true, current: true },
      { uid: 'test-uid-0003', nickname: '测试账号丙', valid: true, current: false },
    ],
  };
}
`;

const tail = `
// ---------------------------------------------------------------------------
// 合成夹具（**不再请求** 127.0.0.1:7863）。
//
// 原写法是 fetch('http://127.0.0.1:7863/admin/growth') —— 7863 是
// **用户的生产网关**。那让测试依赖"本机恰好在跑那个实例"（别人机器上必然
// 失败），并把真实账号 UID/昵称带进了断言输出。
//
// 本套件要验的是**渲染规则**（选项数、默认选中、刷新后保持、回落到第一个、
// 三视图之和），与数据来源无关；换成合成夹具不影响它证明的东西。
const FIXTURE_ACCOUNTS = [
  {
    uid: 'test-uid-0001', nickname: '测试账号甲',
    tasks_total: 3, tasks_completed: 1, tasks_accepted: 1,
    acceptable_count: 1, acceptable_credit: 10, acceptable_energy: 1,
    pending_count: 1, pending_credit: 20, pending_energy: 1,
    claimable_count: 1, claimable_credit: 30, claimable_energy: 1,
    streak_days: 5, next_tier_remaining: 3, makeup_cards: 1,
    remaining_days: 20, energy: 12,
    blind_box_affordable: true, blind_box_cost: 10, lottery_chances: 2,
    observed_at: '2026-01-01T00:00:00Z', error: '',
    // tasks 供 growthCounts() 统计三视图，并给「说明」列提供 description/how_to。
    // create_canvas 是断言点名的任务码，必须存在。
    tasks: [
      { task_code: 'create_canvas', status: 'not_accepted', locked: false, reward_credit: 10,
        description: '达成条件：完成一次画布创建',
        how_to: '操作指引：在工作台新建画布即可' },
      { task_code: 'test-task-b', status: 'accepted', locked: false, reward_credit: 20,
        description: '达成条件：进行中的任务', how_to: '' },
      { task_code: 'test-task-c', status: 'completed', locked: false, reward_credit: 30,
        description: '达成条件：已完成的任务', how_to: '' },
    ],
  },
  {
    uid: 'test-uid-0002', nickname: '测试账号乙',
    tasks_total: 2, tasks_completed: 0, tasks_accepted: 2,
    acceptable_count: 0, acceptable_credit: 0, acceptable_energy: 0,
    pending_count: 2, pending_credit: 10, pending_energy: 2,
    claimable_count: 0, claimable_credit: 0, claimable_energy: 0,
    streak_days: 1, next_tier_remaining: 7, makeup_cards: 0,
    remaining_days: 30, energy: 3,
    blind_box_affordable: false, blind_box_cost: 10, lottery_chances: 0,
    observed_at: '2026-01-01T00:00:00Z', error: '',
    tasks: [
      { task_code: 'test-task-d', status: 'in_progress', locked: false, reward_credit: 5,
        description: '达成条件：进行中', how_to: '' },
      { task_code: 'test-task-e', status: 'in_progress', locked: false, reward_credit: 5,
        description: '达成条件：进行中', how_to: '' },
    ],
  },
  {
    uid: 'test-uid-0003', nickname: '测试账号丙',
    tasks_total: 0, tasks_completed: 0, tasks_accepted: 0,
    acceptable_count: 0, acceptable_credit: 0, acceptable_energy: 0,
    pending_count: 0, pending_credit: 0, pending_energy: 0,
    claimable_count: 0, claimable_credit: 0, claimable_energy: 0,
    streak_days: 0, next_tier_remaining: 0, makeup_cards: 0,
    remaining_days: 0, energy: 0,
    blind_box_affordable: false, blind_box_cost: 0, lottery_chances: 0,
    observed_at: '2026-01-01T00:00:00Z', error: '',
    tasks: [],
  },
];

(async () => {
  growthSnaps = FIXTURE_ACCOUNTS;

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  console.log('真实账号数: ' + growthSnaps.length + '  (' + growthSnaps.map(g => g.nickname).join(', ') + ')');

  console.log('\\n[1] 下拉框选项');
  buildGrowthAccountSelect();
  const optHtml = elements.growthAccount.innerHTML;
  optHtml.split('</option>').filter(s => s.includes('<option')).forEach(s => console.log('   ' + s.replace(/^.*?<option /, '<option ') + '</option>'));
  // ⚠ 产品在选项最前面多了一项「全部账号」（value=""）——webui.html:8628，
  //   「全部账号」= 空 uid：下拉按钮（一键完成）与顶部「全部xxx」动作将作用于所有账号。
  //   所以**选项数 = 账号数 + 1**。原来断言 === 账号数，产品加这一项之后必红；
  //   这不是产品坏了，是断言把"选项"当成了"账号"（两者从那一刻起不是一个集合）。
  ok((optHtml.match(/<option /g) || []).length === growthSnaps.length + 1,
     '选项数 == 账号数 + 1（含「全部账号」）(' + (growthSnaps.length + 1) + ')');
  ok(optHtml.indexOf('<option value="">全部账号') === 0, '第一项是「全部账号」（value=""）');
  ok(optHtml.includes('待接单'), '选项文字带待接单计数');
  // ⚠ 昵称现在是**打码后**进选项的（renderGrowthGroups/buildGrowthAccountSelect 都走
  //   maskNick，webui.html:8631）。夹具的账号名恰好是 5 个汉字 —— maskNick 留 3 尾 2、
  //   中间补 0 个星，正好恒等，所以下面那些 includes(昵称) 的断言仍然成立。
  //   这里补一条**真正的打码判据**（否则"昵称被打码"这件事在本套件里无人守卫）：
  {
    const longNick = '这是一条很长的测试昵称';   // 11 字符 → 3 + 6 个星 + 2
    growthSnaps = growthSnaps.concat([{ uid: 'test-uid-0009', nickname: longNick, tasks_total: 0, tasks: [] }]);
    buildGrowthAccountSelect();
    const masked = elements.growthAccount.innerHTML;
    ok(!masked.includes(longNick), '长昵称在下拉框里被打码（原文不出现）');
    ok(masked.includes('这是一' + '*'.repeat(6) + '昵称'), '打码形态正确（留首 3 尾 2，中间补 6 个 *）');
    growthSnaps = growthSnaps.filter(g => g.uid !== 'test-uid-0009');
    buildGrowthAccountSelect();
  }

  console.log('\\n[2] 默认选择');
  // ⚠ 默认值变了（产品有意为之）：初始 growthUID 为 '' 时**保持「全部账号」**，
  //   不再自动跳到第一个账号 —— webui.html:8640-8642：
  //   growthUID = (exists || prev === '') ? prev : '';
  //   （"全部账号"能让"点一键完成 = 所有账号都执行"这个作用域一眼可见）。
  ok(growthUID === '', '默认选中「全部账号」（空 uid）');
  ok(elements.growthAccount.value === growthUID, 'select.value 与 growthUID 一致');

  console.log('\\n[3] 刷新保持用户选择');
  const second = (growthSnaps[1] || growthSnaps[0]).uid;
  growthUID = second;
  buildGrowthAccountSelect();
  ok(growthUID === second, '刷新后仍是用户选的账号');

  console.log('\\n[4] 选中账号消失时回落');
  growthUID = 'gone-0000-0000-0000-000000000000';
  buildGrowthAccountSelect();
  // 回落目标是「全部账号」而不是「第一个账号」：账号被删掉时，
  // 界面不能**悄悄替用户选一个别的账号**（那会让下面那块表看起来是"这个账号的数据"）。
  ok(growthUID === '', '账号消失时回落到「全部账号」');

  console.log('\\n[5] 各账号计数（三视图应互斥且加总等于全部）');
  for (const g of growthSnaps) {
    const c = growthCounts(g);
    console.log('   ' + g.nickname + ': 待接单 ' + c.acceptable + ' · 进行中 ' + c.running + ' · 已完成 ' + c.done + ' · 共 ' + c.all);
    ok(c.acceptable + c.running + c.done === c.all, g.nickname + ' 三视图之和 == 全部');
    ok(c.all === (g.tasks || []).length, g.nickname + ' 全部 == tasks 长度');
  }

  console.log('\\n[6] 渲染只画一个账号（不是全部）');
  growthViewPicked = false;
  growthUID = growthSnaps[0].uid;
  growthView = 'all';       // 全部，确保能拿到 create_canvas 这类已完成任务
  growthViewPicked = true;
  renderGrowthGroups();
  const gHtml = elements.growthGroups.innerHTML;
  const heads = (gHtml.match(/class="gname"/g) || []).length;
  ok(heads === 1, '明细区只有 1 个账号节头（实际 ' + heads + '），账号数 ' + growthSnaps.length);
  if (growthSnaps.length > 1) ok(!gHtml.includes(growthSnaps[1].nickname), '未渲染第二个账号');
  ok(elements.growthTaskMeta.textContent.includes('待接单'), 'meta 显示了计数');

  console.log('\\n[6b] 任务详细信息确实输出到前端');
  ok(gHtml.includes('<th>说明</th>'), '表头含「说明」列');
  const first = growthSnaps[0];
  let checkedDesc = 0, checkedHow = 0;
  for (const t of (first.tasks || [])) {
    if (t.description) { ok(gHtml.includes(esc(t.description)), '说明已渲染: ' + t.task_code + ' -> ' + t.description.slice(0, 24)); checkedDesc++; break; }
  }
  for (const t of (first.tasks || [])) {
    if (t.how_to) { ok(gHtml.includes(esc(t.how_to)), '怎么做已渲染: ' + t.task_code); checkedHow++; break; }
  }
  const cc = (first.tasks || []).find(t => t.task_code === 'create_canvas');
  if (cc) {
    console.log('   create_canvas 说明 = ' + cc.description);
    console.log('   create_canvas 怎么做 = ' + (cc.how_to || '(无)'));
    ok(gHtml.includes(esc(cc.description)), '用户点名的 create_canvas 达成条件已渲染');
    if (cc.how_to) ok(gHtml.includes(esc(cc.how_to)), '用户点名的 create_canvas 操作指引已渲染');
  }
  ok(checkedDesc > 0, '说明列有实际内容');

  console.log('\\n[7] 自动落到第一个非空视图');
  console.log('   选中视图 = ' + growthView);
  const c0 = growthCounts(growthSnaps.find(g => g.uid === growthUID));
  ok(c0[growthView] > 0 || growthView === 'all', '自动选中的视图非空 (' + growthView + '=' + c0[growthView] + ')');

  console.log('\\n[8] 切账号会重画');
  if (growthSnaps.length > 1) {
    const target = growthSnaps[1];
    growthUID = target.uid;
    growthViewPicked = true;
    renderGrowthGroups();
    const h2 = elements.growthGroups.innerHTML;
    ok(h2.includes(target.nickname), '切到 ' + target.nickname + ' 后渲染了该账号');
    ok(!h2.includes(growthSnaps[0].nickname), '不再渲染上一个账号');
  }

  console.log('\\n[9] 空账号池');
  const savedSnaps = growthSnaps;
  growthSnaps = [];
  growthUID = '';
  buildGrowthAccountSelect();
  // ⚠ 断言从"无选项"改成"只剩「全部账号」"：那一项不依赖账号列表，
  //   永远存在（webui.html:8628 的 allOpt 是常量前缀）。
  //   空池时它仍然是有意义的 —— 「全部账号」上的动作会对"当前已有的账号"执行。
  ok((elements.growthAccount.innerHTML.match(/<option /g) || []).length === 1,
     '空池时只剩「全部账号」一项');
  ok(elements.growthAccount.innerHTML.indexOf('<option value="">全部账号') === 0,
     '空池时那一项仍是「全部账号」');

  console.log('\\n[10] 明细节头里的「本机登录」控制条');
  growthSnaps = savedSnaps;
  growthUID = growthSnaps[0].uid;
  growthView = 'all';
  growthViewPicked = true;
  const realStatus = await fetchClientStatus();
  console.log('    后端: enabled=' + realStatus.enabled + ' current=' + (realStatus.current && realStatus.current.nickname)
    + ' has_backup=' + realStatus.has_backup);

  // 10a) 读取中（clientStatus 为 null）
  clientStatus = null;
  renderGrowthGroups();
  ok(elements.growthGroups.innerHTML.includes('本机登录：读取中'), '未加载时显示「读取中」');

  // 10b) 未启用
  clientStatus = { enabled: false, error: '测试：未启用' };
  renderGrowthGroups();
  ok(elements.growthGroups.innerHTML.includes('本机登录：不可用'), '未启用时显示「不可用」');

  // 10c) 真实状态
  clientStatus = realStatus;
  renderGrowthGroups();
  const gh = elements.growthGroups.innerHTML;
  const togIdx = gh.indexOf('本机登录：');
  console.log('    控制条片段: ' + (togIdx >= 0
    ? gh.slice(togIdx, togIdx + 160).replace(/<[^>]*>/g, ' ').replace(/\\s+/g, ' ').trim()
    : '(未找到本机登录文字)'));
  ok(gh.includes('本机登录：'), '节头包含本机登录文字');
  if (realStatus.current) {
    ok(gh.includes(esc(realStatus.current.nickname)), '显示当前客户端登录账号名');
  }
  const curCand = (realStatus.candidates || []).find(c => c.uid === growthUID);
  if (curCand && curCand.current) {
    ok(gh.includes('已是本机登录'), '正在看的账号就是本机登录账号 → 显示「已是本机登录」');
  } else if (curCand) {
    ok(gh.includes('data-clswitch'), '正在看的账号不是本机登录账号 → 出现「切换本机登录」按钮');
  }
  ok(realStatus.has_backup ? gh.includes('data-clrestore') : !gh.includes('data-clrestore'),
    '有备份才出现「回滚」按钮（has_backup=' + realStatus.has_backup + '）');

  // 10d) 切到另一个账号，按钮应指向那个账号
  if (growthSnaps.length > 1) {
    const other = growthSnaps[1];
    growthUID = other.uid;
    renderGrowthGroups();
    const gh2 = elements.growthGroups.innerHTML;
    const m = gh2.match(/data-clswitch="([^"]+)"/);
    if (m) ok(m[1] === other.uid, '切换按钮指向当前展示的账号 ' + other.uid.slice(0, 8) + '（看的和切的一致）');
    else ok(gh2.includes('已是本机登录'), '第二账号已是本机登录，无切换按钮');
  }

  // 10e) 旧独立面板确实已移除
  const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
  ok(!html.includes('<h2>本地登录'), '页面里已没有独立的「本地登录」面板');
  ok(!html.includes('clientRows'), '页面里没有旧面板的残留 DOM id');

  console.log(fail === 0 ? '\\n=== 全部通过 ===' : '\\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
`;

// ---- 抽取列表的静态自检（本轮新加，见 lib_extract_check.js 文件头）----
//
// 判据：抽出来的块里"引用了、但抽取集与桩都没提供、也不是 JS 内建"的名字 ⇒ 当场非零退出。
// ⚠ 它只接了 4 个 growth generator，**其余 12 个没接**（父代理批的窄版本）——别以为全仓都接上了。
checkExtract({
  pieces,
  stub: header,
  productFile: path,
  label: 'gen_growth_select.js',
  extra: [],   // 夹具自有的名字才进这里，且每个都要写理由；不接受整类豁免
});

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('growth_select_gen.js', out);
console.log('生成: growth_select_gen.js  (' + out.length + ' 字节)');
console.log('抽出的产品代码:');
pieces.forEach((p, i) => console.log('  [' + i + '] ' + p.split('\n')[0].trim()));
