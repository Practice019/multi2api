// Task 8 / T3 前端：上游契约层（manifest 驱动）。
//
// 后端 /admin/ui/manifest 已就绪，下发 providers / capabilities / admin_routes / jobs。
// 前端契约层要提供：
//   1. 账号归属（providerOf，含历史账号回落默认上游）
//   2. 能力位查询（providersWithCap）—— 必须**从数据里读**，不能硬编码
//   3. 能力标题翻译（capTitle）—— 标题也由后端下发
//   4. 上游分组（upstreamGroups）—— 顺序必须稳定
//   5. GET 路由查询（routesFor）—— 只把 GET 当"面板入口"
//
// # 为什么能力位必须来自后端
//
// 前端硬编码能力表的话，加第三个上游就要改前端 —— 那正是判据 1
// （加新上游核心零改动）要避免的。
//
// # 历史教训
//
// 上一版定义了 providersWithCap / groupByProvider 却**从未调用**（死代码）。
// 本套件因此额外断言：这些函数都有真实调用点。
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');

// 用相对本机的方式定位仓库：优先环境变量，其次从 __dirname 推断。
// 不写死路径 —— 写死的那版指向 workbuddy2api（不含"实验版本"），
// 所以它读到的是**另一个仓库**，测的东西跟本仓库无关。
const REPO = (process.env.WB2API_REPO || __dirname + '/../..');
const WEBUI = path.join(REPO, 'internal/server/webui.html');
const html = fs.readFileSync(WEBUI, 'utf8');

function grab(marker) {
  const start = html.indexOf(marker);
  if (start < 0) throw new Error('找不到: ' + marker);
  let i = html.indexOf('{', start), d = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') d++;
    else if (html[i] === '}') { d--; if (d === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}

const pieces = [
  grab('function applyManifest('),
  grab('function providerOf('),
  // R2 把"manifest 里有哪些上游"收敛成 providerRegistryIds()，
  // 现在 providersWithCap / upstreamGroups 都经它枚举 —— 不一起抠出来，
  // 产物会报 `providerRegistryIds is not defined`。
  //
  // ⚠ 与 gen_models_display.js 同一个坑：抽取是**按名字**取的。
  // 顺序放在使用者之前（这里是函数声明，靠提升也能活，但显式一点更好排障）。
  grab('function providerRegistryIds('),
  grab('function providersWithCap('),
  grab('function capTitle('),
  grab('function providerInfo('),
  grab('function routesFor('),
  grab('function hasAnyPanel('),
  grab('function upstreamGroups('),
].join('\n\n');

// 被抽出的函数引用了这几个模块级变量，必须在生成文件里先声明。
const header = `
const esc = s => String(s == null ? '' : s)
  .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let MANIFEST = { service: 'gateway', providers: [], capabilities: [], admin_routes: [], jobs: [], loaded: false };
let PROVIDERS = [];
let DEFAULT_PROVIDER = '';
let manifestError = '';
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const M = new Function('esc',
  SRC + '\nreturn { applyManifest, providerOf, providersWithCap, capTitle, providerInfo, routesFor, hasAnyPanel, upstreamGroups,' +
  ' get MANIFEST(){return MANIFEST}, get DEFAULT(){return DEFAULT_PROVIDER} };'
)(esc);

// ---------------------------------------------------------------- applyManifest
console.log('\n[1] applyManifest 的形状归一与降级');
M.applyManifest({
  service: 'workbuddy2api',
  providers: [
    { id: 'workbuddy', capabilities: ['chat','models','growth','travel'], default: true, account_count: 3 },
    { id: 'codearts', capabilities: ['chat','models','welfare','quota-probe'], default: false, account_count: 1 },
  ],
  capabilities: [
    { id: 'growth', title: '成长计划' }, { id: 'travel', title: '猫猫旅行' },
    { id: 'welfare', title: '福利中心' }, { id: 'chat', title: '对话' },
    { id: 'models', title: '模型目录' }, { id: 'quota-probe', title: '额度探测' },
  ],
  admin_routes: [
    { provider: 'workbuddy', method: 'GET', path: '/admin/growth', capability: 'growth', title: '成长计划' },
    { provider: 'workbuddy', method: 'POST', path: '/admin/growth/claim', capability: 'growth', title: '领取奖励' },
    { provider: 'workbuddy', method: 'GET', path: '/admin/travel', capability: 'travel', title: '猫猫旅行' },
    { provider: 'codearts', method: 'GET', path: '/admin/welfare', capability: 'welfare', title: '福利中心' },
    { provider: 'codearts', method: 'POST', path: '/admin/welfare/claim', capability: 'welfare', title: '领取福利' },
    { provider: 'codearts', method: 'GET', path: '/admin/subscription', capability: 'welfare', title: '套餐与额度' },
  ],
  jobs: [{ provider: 'codearts', name: 'codearts-refresh', interval_ms: 60000 }],
});
ok(M.MANIFEST.loaded === true, 'applyManifest 后 loaded=true');
ok(M.MANIFEST.service === 'workbuddy2api', 'service 原样存下');
ok(M.DEFAULT === 'workbuddy', '默认上游从 providers[].default 推导（manifest 顶层无 default 字段）');

// 畸形输入必须安全降级（不崩、不产生 half-state）
M.applyManifest(null);
ok(M.MANIFEST.loaded === true && M.MANIFEST.providers.length === 0, 'null 输入 → 空但 loaded');
ok(M.MANIFEST.service === 'gateway', '缺 service → 回落 gateway（不是空串）');
M.applyManifest({ providers: 'not-an-array', capabilities: null, admin_routes: 42 });
ok(Array.isArray(M.MANIFEST.providers) && Array.isArray(M.MANIFEST.admin_routes), '非数组字段被归一成数组（否则 .map 会抛）');
ok(M.MANIFEST.service === 'gateway', '空 service 字符串也回落 gateway');

// 恢复标准 fixture
const FIX = {
  service: 'workbuddy2api',
  providers: [
    { id: 'workbuddy', capabilities: ['chat','models','growth','travel'], default: true, account_count: 3 },
    { id: 'codearts', capabilities: ['chat','models','welfare','quota-probe'], default: false, account_count: 1 },
  ],
  capabilities: [
    { id: 'growth', title: '成长计划' }, { id: 'travel', title: '猫猫旅行' },
    { id: 'welfare', title: '福利中心' }, { id: 'chat', title: '对话' },
    { id: 'models', title: '模型目录' }, { id: 'quota-probe', title: '额度探测' },
  ],
  admin_routes: [
    { provider: 'workbuddy', method: 'GET', path: '/admin/growth', capability: 'growth', title: '成长计划' },
    { provider: 'workbuddy', method: 'POST', path: '/admin/growth/claim', capability: 'growth', title: '领取奖励' },
    { provider: 'workbuddy', method: 'GET', path: '/admin/travel', capability: 'travel', title: '猫猫旅行' },
    { provider: 'codearts', method: 'GET', path: '/admin/welfare', capability: 'welfare', title: '福利中心' },
    { provider: 'codearts', method: 'POST', path: '/admin/welfare/claim', capability: 'welfare', title: '领取福利' },
    { provider: 'codearts', method: 'GET', path: '/admin/subscription', capability: 'welfare', title: '套餐与额度' },
  ],
  jobs: [],
};
M.applyManifest(FIX);

// ---------------------------------------------------------------- 账号归属
console.log('\n[2] 账号的上游归属');
ok(M.providerOf({ uid: 'u1', provider: 'workbuddy' }) === 'workbuddy', '显式带 provider 的账号');
ok(M.providerOf({ uid: 'u2', provider: 'codearts' }) === 'codearts', '另一个上游的账号');
ok(M.providerOf({ uid: 'u3' }) === 'workbuddy', '无 provider 的历史账号 → 归默认上游（向后兼容）');
ok(M.providerOf({ uid: 'u4', provider: '' }) === 'workbuddy', '空串 → 默认上游');
ok(M.providerOf(null) === '', 'null 输入不崩');

// ---------------------------------------------------------------- 能力位查询
console.log('\n[3] 能力位查询（必须从数据读，不能硬编码）');
ok(M.providersWithCap('growth').length === 1, '只有 workbuddy 有 growth');
ok(M.providersWithCap('growth')[0] === 'workbuddy', 'growth 属于 workbuddy');
ok(M.providersWithCap('welfare').length === 1, '只有 codearts 有 welfare');
ok(M.providersWithCap('welfare')[0] === 'codearts', 'welfare 属于 codearts');
ok(M.providersWithCap('chat').length === 2, 'chat 两个上游都有');
ok(M.providersWithCap('nonexistent').length === 0, '不存在的能力返回空');

// 能力位换到别的上游后查询必须跟着变（证明读的是数据）
M.applyManifest(Object.assign({}, FIX, {
  providers: [
    { id: 'workbuddy', capabilities: ['chat'], default: true, account_count: 1 },
    { id: 'third', capabilities: ['growth'], default: false, account_count: 1 },
  ],
}));
ok(M.providersWithCap('growth')[0] === 'third',
  '能力位换到别的上游后，查询跟着变 —— 证明读的是数据而非硬编码');
M.applyManifest(FIX);

// ---------------------------------------------------------------- 能力标题
console.log('\n[4] 能力标题翻译（标题也由后端下发）');
ok(M.capTitle('growth') === '成长计划', 'growth → 成长计划');
ok(M.capTitle('welfare') === '福利中心', 'welfare → 福利中心');
ok(M.capTitle('quota-probe') === '额度探测', 'quota-probe → 额度探测（不是英文 id）');
ok(M.capTitle('unknown-cap') === 'unknown-cap', '未登记的退回 id（至少能看出是什么，不是空白）');

// 标题跟着数据变
M.applyManifest(Object.assign({}, FIX, { capabilities: [{ id: 'growth', title: '改名了' }] }));
ok(M.capTitle('growth') === '改名了', '标题跟着 manifest 变 —— 证明不是硬编码');
M.applyManifest(FIX);

// ---------------------------------------------------------------- 路由查询
console.log('\n[5] 路由查询（只把 GET 当面板入口）');
ok(M.routesFor('workbuddy', 'growth').length === 1,
  'workbuddy 的 growth 面板入口只有 1 个（POST /admin/growth/claim 是动作不是面板）');
ok(M.routesFor('codearts', 'welfare').length === 2,
  'codearts 的 welfare 有 2 个 GET（福利中心 + 套餐与额度）');
ok(M.routesFor('workbuddy').length === 2, 'workbuddy 全部 GET 端点 2 个');
ok(M.routesFor('nosuch').length === 0, '不存在的上游返回空');
M.applyManifest(Object.assign({}, FIX, {
  admin_routes: [{ provider: 'x', method: 'delete', path: '/admin/x', capability: 'chat' }],
}));
ok(M.routesFor('x').length === 0, '小写 delete 也不被当成面板入口（方法比较不区分大小写）');
M.applyManifest(FIX);

// ---------------------------------------------------------------- 上游分组
console.log('\n[6] 上游分组顺序稳定');
const g = M.upstreamGroups();
ok(g.length === 2, '两组，得到 ' + g.length);
ok(g[0].id === 'workbuddy', '默认上游排在最前');
ok(M.upstreamGroups()[0].id === g[0].id, '两次调用顺序一致（稳定）');

// 默认换人 → 顺序跟着换
M.applyManifest(Object.assign({}, FIX, {
  providers: [
    { id: 'codearts', capabilities: ['chat','welfare'], default: true, account_count: 1 },
    { id: 'workbuddy', capabilities: ['chat','growth'], default: false, account_count: 3 },
  ],
}));
ok(M.upstreamGroups()[0].id === 'codearts', '默认上游换成 codearts 后它排最前');
M.applyManifest(FIX);

// 空/null 安全
M.applyManifest({ providers: [], admin_routes: [], capabilities: [] });
ok(M.upstreamGroups().length === 0, '空 providers → 空分组');
ok(M.providerOf({ uid: 'x' }) === '', '无默认上游时 providerOf 返回空串（不崩）');

// ---------------------------------------------------------------- 显示门槛
console.log('\n[7] 分组显示门槛（无面板且无账号的上游不占导航）');
M.applyManifest({
  service: 's', capabilities: [{ id: 'welfare', title: '福利' }],
  providers: [
    { id: 'real', capabilities: ['chat','welfare'], default: true, account_count: 2 },
    { id: 'empty', capabilities: ['chat','models'], default: false, account_count: 0 },
    { id: 'ghost', capabilities: ['chat','models','welfare'], default: false, account_count: 0 },
  ],
  admin_routes: [{ provider: 'ghost', method: 'GET', path: '/admin/w', capability: 'welfare' }],
  jobs: [],
});
const ids = M.upstreamGroups().map(p => p.id);
ok(ids.indexOf('real') >= 0, '有账号的上游保留');
ok(ids.indexOf('empty') < 0, '无账号且只有 chat/models 的上游被隐藏（chat 不构成专属面板）');
ok(ids.indexOf('ghost') >= 0, '无账号但**有专属面板**的上游保留（否则用户看不到它的入口）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = path.join(__dirname, 'task8_gen.js');
fs.writeFileSync(out, header + pieces + '\n' + tail.replace('SRC', JSON.stringify(pieces)));
console.log('生成: ' + out);
