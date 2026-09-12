// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成一个自包含的测试脚本：把 webui.html 里**真实的产品函数**抽出来，
// 配最小 DOM 桩 + 真实 API 数据跑断言。抽出的是产品代码本身，不是副本。
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
  grabBlock('const GROWTH_VIEWS = {'),
  grabBlock('const GROWTH_STATUS_TEXT = {'),
  // fmtTaskExpiry 被 renderGrowthGroups 调用（任务到期列）。
  // 它原先不在抽取列表里 —— 那是因为本脚本以前读的是**另一个仓库**的
  // webui.html（见 gen_backup_wrongrepo），那份文件当时还没有这一列。
  grabBlock('function fmtTaskExpiry('),
  grabBlock('function syncGrowthViewButtons('),
  grabBlock('function growthCounts('),
  grabBlock('function buildGrowthAccountSelect('),
  grabBlock('function clientLoginHTML('),
  grabBlock('function renderGrowthGroups('),
];

const header = `
const fs = require('fs');
const KEY = '__REDACTED_LEAKED_KEY__';
let growthSnaps = [], growthUID = '', growthView = 'acceptable', growthViewPicked = false;
let clientStatus = null;
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
// 本机登录状态由测试按需注入（见各断言），这里不真连后端。
async function fetchClientStatus() {
  const r = await fetch('http://127.0.0.1:7863/admin/client-login', { headers: { Authorization: 'Bearer ' + KEY } });
  return r.json();
}
`;

const tail = `
(async () => {
  const r = await fetch('http://127.0.0.1:7863/admin/growth', { headers: { Authorization: 'Bearer ' + KEY } });
  const data = await r.json();
  growthSnaps = data.accounts;

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  console.log('真实账号数: ' + growthSnaps.length + '  (' + growthSnaps.map(g => g.nickname).join(', ') + ')');

  console.log('\\n[1] 下拉框选项');
  buildGrowthAccountSelect();
  const optHtml = elements.growthAccount.innerHTML;
  optHtml.split('</option>').filter(s => s.includes('<option')).forEach(s => console.log('   ' + s.replace(/^.*?<option /, '<option ') + '</option>'));
  ok((optHtml.match(/<option /g) || []).length === growthSnaps.length, '选项数 == 账号数 (' + growthSnaps.length + ')');
  ok(optHtml.includes('待接单'), '选项文字带待接单计数');

  console.log('\\n[2] 默认选择');
  ok(growthUID === growthSnaps[0].uid, '默认选中第一个账号 (' + growthSnaps[0].nickname + ')');
  ok(elements.growthAccount.value === growthUID, 'select.value 与 growthUID 一致');

  console.log('\\n[3] 刷新保持用户选择');
  const second = (growthSnaps[1] || growthSnaps[0]).uid;
  growthUID = second;
  buildGrowthAccountSelect();
  ok(growthUID === second, '刷新后仍是用户选的账号');

  console.log('\\n[4] 选中账号消失时回落');
  growthUID = 'gone-0000-0000-0000-000000000000';
  buildGrowthAccountSelect();
  ok(growthUID === growthSnaps[0].uid, '回落到第一个账号');

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
  ok(elements.growthAccount.innerHTML === '', '空池时下拉框无选项');

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

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('growth_select_gen.js', out);
console.log('生成: growth_select_gen.js  (' + out.length + ' 字节)');
console.log('抽出的产品代码:');
pieces.forEach((p, i) => console.log('  [' + i + '] ' + p.split('\n')[0].trim()));
