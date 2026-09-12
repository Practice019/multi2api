// mutation_sweep.js —— 统一变异扫描器：验证"守卫是否真的能抓住它针对的缺陷"。
//
// # 为什么需要这个工具（而不是再加测试）
//
// 本项目多次出现"守卫能存活变异"：
//   - 徽章底色：断言匹配 `rgba(255,255,255,.22)` 字符串，而 Chrome 序列化成
//     `color(srgb ...)` → 断言恒真
//   - 导航徽章：把整块删掉，21 个套件全绿
//   - codearts 错误分类：把分类逻辑固定成 500，测试仍绿（因为错误文本里
//     还带着原文）
//   - 面板渲染：把 renderModels/loadSettings 等整个短路，13 个 E2E 全绿
//
// 共同点：**验证了书写形式，而不是本体**。这个工具用来系统性地找出这类守卫。
//
// # 三种"存活"，必须分开（踩过的坑）
//
//   A. 测试真的有洞     → 补测试
//   B. 变异无效         → 换注入（曾把 `if(...)x=..` 换成 `if(true){}else{}`，
//                          对结果毫无影响，却报"存活"）
//   C. 环境不可观测     → 不能算缺陷（曾在一个"日志为空"的新实例上
//                          断言"日志面板要有内容"）
//
// 本工具对每一类都有显式处理，而不是把它们混成一个数字。
//
// # 用法
//
//   node tests/frontend/mutation_sweep.js              # 跑全部
//   node tests/frontend/mutation_sweep.js e2e          # 只跑浏览器 E2E 那组
//   node tests/frontend/mutation_sweep.js static       # 只跑静态套件那组
//   node tests/frontend/mutation_sweep.js --self-test  # 只验证扫描器自身
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn, spawnSync, execFileSync } = require('child_process');

const HERE = __dirname;
const REPO = path.resolve(HERE, '..', '..');
const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';

// 工作目录放系统临时目录（可移植）；可用 MUT_WORK 覆盖
const WORK = process.env.MUT_WORK || path.join(os.tmpdir(), 'wb2api-mutsweep');

// ---------------------------------------------------------------------------
// 变异清单
//
// 每条都对应"用户能看出差别"的破坏。字段说明：
//   kind   'e2e' 需要起服务 + 浏览器；'static' 只需副本 + 静态套件
//   find   锚点（必须在源码里**唯一**命中，否则中止）
//   repl   替换文本
//   probe  用来确认"服务端跑的确实是变异版"的特征串（默认取 repl 前 24 字符）
// ---------------------------------------------------------------------------
const MUTATIONS = [
  // ---- 导航 / 结构 ----
  { id: 'E1-topstats', kind: 'e2e', port: 18101,
    desc: '顶栏状态条不再渲染',
    find: '  function renderTopStats(h, s) {',
    repl: '  function renderTopStats(h, s) { return; // MUT' },
  { id: 'E2-nav-groups', kind: 'e2e', port: 18102,
    desc: '导航不再按上游分组',
    find: '  function upstreamGroups() {',
    repl: '  function upstreamGroups() { return []; // MUT' },
  { id: 'E3-acct-group', kind: 'e2e', port: 18103,
    desc: '账号表不再按上游分组',
    find: '  function groupAccountsByProvider(',
    repl: '  function groupAccountsByProviderUnused(' },

  // ---- 面板内容（第二轮扫描发现的盲区）----
  { id: 'E4-models', kind: 'e2e', port: 18201,
    desc: '可用模型面板不再渲染',
    find: '  function renderModels(list) {',
    repl: '  function renderModels(list) { return; // MUT' },
  { id: 'E5-settings', kind: 'e2e', port: 18202,
    desc: '设置面板不再回填配置',
    find: '  async function loadSettings() {',
    repl: '  async function loadSettings() { return; // MUT' },
  { id: 'E6-growth', kind: 'e2e', port: 18203,
    desc: '成长计划分组不再渲染',
    find: '  function renderGrowthGroups() {',
    repl: '  function renderGrowthGroups() { return; // MUT' },
  { id: 'E7-travel', kind: 'e2e', port: 18204,
    desc: '猫猫旅行面板不再渲染',
    find: '  function renderTravel(list, autoClaim, autoDepart, checkinHours) {',
    repl: '  function renderTravel(list, autoClaim, autoDepart, checkinHours) { return; // MUT' },
  { id: 'E8-history', kind: 'e2e', port: 18205,
    desc: '任务历史不再加载',
    find: '  async function loadHistory() {',
    repl: '  async function loadHistory() { return; // MUT' },
  { id: 'E9-logfileinfo', kind: 'e2e', port: 18206,
    desc: '设置面板的落盘日志信息行不再渲染',
    find: '  function renderLogFileInfo(f) {',
    repl: '  function renderLogFileInfo(f) { return; // MUT' },

  // ---- 主题 ----
  { id: 'E10-theme-persist', kind: 'e2e', port: 18104,
    desc: '主题选择不再持久化',
    find: 'try { localStorage.setItem(LS_THEME, pref); } catch { /* 隐私模式 */ }',
    repl: 'try { void 0; } catch { /* MUT: 不持久化 */ }' },

  // ---- 静态契约 ----
  { id: 'S1-colspan-drift', kind: 'static',
    desc: 'JS 的 COLS.logs 与 Go 替换表漂移',
    find: '    logs: 10,    // 请求日志',
    repl: '    logs: 99,    // 请求日志（变异：漂移）' },
  { id: 'S2-emoji', kind: 'static',
    desc: '设置面板塞入 emoji（项目硬规则：无 emoji）',
    find: '<legend>调度（保存后立即生效）</legend>',
    repl: '<legend>调度（保存后立即生效）✅</legend>' },
  { id: 'S3-pagesize', kind: 'static',
    desc: '分页大小写死（不再从 localStorage 读）',
    find: '  function loadPageSize() {',
    repl: '  function loadPageSize() { return 30; // MUT' },
  { id: 'S4-busy', kind: 'static',
    desc: 'busy 反馈失效（busyRun 被改名）',
    find: '  async function busyRun(btn, fn, opts) {',
    repl: '  async function busyRunUnused(btn, fn, opts) {' },
  { id: 'S5-normalize', kind: 'static',
    desc: '账号归一化失效',
    find: '  function normalizeAccount(',
    repl: '  function normalizeAccountUnused(' },
];

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------
function rmrf(p) { try { fs.rmSync(p, { recursive: true, force: true }); } catch { /* ignore */ } }
function get(url) {
  return new Promise((res, rej) => {
    http.get(url, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res({ status: r.statusCode, body: d })); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

// 复制仓库。robocopy 的退出码 <8 都算成功（1=已复制，2=有额外文件）。
function copyRepo(dest) {
  rmrf(dest);
  const rc = spawnSync('robocopy', [REPO, dest, '/E', '/XD', '.git', 'bin',
    '/NFL', '/NDL', '/NJH', '/NJS', '/NC', '/NS'], { stdio: 'ignore' });
  if (rc.status !== null && rc.status >= 8) throw new Error('robocopy 退出码 ' + rc.status);
  if (!fs.existsSync(path.join(dest, 'go.mod'))) throw new Error('复制失败: ' + dest);
}

// 锚点自检：每个 find 必须在源码里**唯一**命中。
// 不做这一步的话，"锚点找不到"会静默变成"变异存活"。
function selfCheck(muts) {
  console.log('=== 锚点自检 ===');
  let bad = 0;
  const cache = {};
  for (const m of muts) {
    const rel = 'internal/server/webui.html';
    if (!cache[rel]) cache[rel] = fs.readFileSync(path.join(REPO, rel), 'utf8');
    const n = cache[rel].split(m.find).length - 1;
    if (n !== 1) { console.log('  ✗ ' + m.id.padEnd(20) + ' 命中 ' + n + ' 次（应唯一）'); bad++; }
    else console.log('  ✓ ' + m.id.padEnd(20) + ' 唯一');
  }
  if (bad) { console.log('\n' + bad + ' 个锚点无效 —— 中止（否则结果无意义）'); return false; }
  console.log('全部有效\n');
  return true;
}

// 静态套件：跑在副本上（generator 读 WB2API_REPO）
function runStaticSuites(dir) {
  const env = Object.assign({}, process.env, { WB2API_REPO: dir });
  const r = spawnSync('node', [path.join(HERE, 'run_frontend_suites.js')],
    { cwd: HERE, encoding: 'utf8', env });
  const out = (r.stdout || '') + (r.stderr || '');
  const m = out.match(/=== (\d+)\/(\d+) 通过 ===/);
  // 失败明细有两种后缀：「X 断言失败」与「X 失败」（后者是 generator 本身挂了）。
  // 只匹配前者会把生成器级失败记成"无失败" —— 这正是第一版扫描器的 bug。
  const failed = [...out.matchAll(/^\s+-\s+(\S+\.js)\s+.*失败/gm)].map(x => x[1]);
  return { passed: m ? +m[1] : null, total: m ? +m[2] : null, failed };
}

// 浏览器 E2E
function runE2E(port) {
  const env = Object.assign({}, process.env);
  const r = spawnSync('node', [path.join(HERE, 'run_e2e.js'), 'http://127.0.0.1:' + port + '/ui'],
    { cwd: HERE, encoding: 'utf8', env, timeout: 20 * 60 * 1000 });
  const out = (r.stdout || '') + (r.stderr || '');
  const m = out.match(/=== (\d+)\/(\d+) 通过 ===/);
  const failed = [...out.matchAll(/FAIL\s+(\S+\.js)/g)].map(x => x[1]);
  return { passed: m ? +m[1] : null, total: m ? +m[2] : null, failed };
}

// 起一个变异版实例，并**校验服务端内容确实是变异版**。
async function buildAndStart(mut, dir) {
  const f = path.join(dir, 'internal/server/webui.html');
  let src = fs.readFileSync(f, 'utf8');
  if (!src.includes(mut.find)) throw new Error('副本里锚点未找到');
  fs.writeFileSync(f, src.replace(mut.find, mut.repl));

  const exe = path.join(WORK, mut.id + '.exe');
  rmrf(exe);
  execFileSync('go', ['build', '-a', '-o', exe, '.\\cmd\\server'],
    { cwd: dir, env: Object.assign({}, process.env, { CGO_ENABLED: '0' }), stdio: 'pipe' });
  if (!fs.existsSync(exe)) throw new Error('构建产物不存在');

  const cfgPath = path.join(WORK, mut.id + '.json');
  const cfg = JSON.parse(fs.readFileSync(path.join(REPO, 'tests', 'frontend', 'run-config.example.json'), 'utf8'));
  cfg.listen = '127.0.0.1:' + mut.port;
  cfg.state_file = path.join(dir, 'data', 'state.json');
  cfg.auth_dir = path.join(REPO, 'auths');
  fs.mkdirSync(path.dirname(cfg.state_file), { recursive: true });
  fs.writeFileSync(cfgPath, JSON.stringify(cfg, null, 2));

  const log = fs.openSync(path.join(WORK, mut.id + '.log'), 'w');
  const child = spawn(exe, ['-config', cfgPath], { stdio: ['ignore', log, log] });

  for (let i = 0; i < 50; i++) {
    await sleep(300);
    try { const r = await get('http://127.0.0.1:' + mut.port + '/healthz'); if (r.status === 200) break; } catch { /* 等 */ }
  }
  // 关键校验：服务端下发的页面必须含变异特征
  const ui = await get('http://127.0.0.1:' + mut.port + '/ui');
  const probe = (mut.probe || mut.repl).replace('// MUT', '').trim().slice(0, 24);
  return { child, verified: ui.body.includes(probe), probe };
}

// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------
(async () => {
  const mode = process.argv[2] || 'all';
  if (mode === '--self-test') {
    console.log('=== 扫描器自检 ===');
    const okA = selfCheck(MUTATIONS);
    console.log('锚点自检: ' + (okA ? '通过' : '失败'));
    console.log('run_frontend_suites.js 存在: ' + fs.existsSync(path.join(HERE, 'run_frontend_suites.js')));
    console.log('run_e2e.js 存在: ' + fs.existsSync(path.join(HERE, 'run_e2e.js')));
    process.exit(okA ? 0 : 1);
  }

  fs.mkdirSync(WORK, { recursive: true });
  const list = MUTATIONS.filter(m => mode === 'all' || m.kind === mode);
  if (!selfCheck(list)) process.exit(2);

  console.log('=== 变异扫描（' + list.length + ' 个，kind=' + mode + '）===');
  console.log('对每个变异：注入 → 全量构建 → 跑套件 → 校验被测版本\n');

  const results = [];
  for (const mut of list) {
    process.stdout.write('[' + mut.id + '] ' + mut.desc + '\n');
    const dir = path.join(WORK, mut.id);
    let child = null;
    try {
      if (mut.kind === 'static') {
        copyRepo(dir);
        const f = path.join(dir, 'internal/server/webui.html');
        const src = fs.readFileSync(f, 'utf8');
        if (!src.includes(mut.find)) throw new Error('锚点未找到');
        fs.writeFileSync(f, src.replace(mut.find, mut.repl));
        // 跑两次以排除产物陈旧带来的非确定性（第三轮踩过）
        const r1 = runStaticSuites(dir);
        const r2 = runStaticSuites(dir);
        if (r1.passed !== r2.passed || JSON.stringify(r1.failed) !== JSON.stringify(r2.failed)) {
          console.log('   ⚠ 两次结论不同（' + r1.passed + ' vs ' + r2.passed + '）—— 产物陈旧，不给结论');
          results.push({ id: mut.id, desc: mut.desc, skipped: '两次结论不一致' });
          console.log('');
          continue;
        }
        results.push({ id: mut.id, desc: mut.desc, ...r1 });
        console.log('   静态 ' + r1.passed + '/' + r1.total +
          '；抓住它的: ' + (r1.failed.length ? r1.failed.slice(0, 4).join(', ') : '（无 —— 存活）'));
        fs.writeFileSync(path.join(WORK, 'results.json'), JSON.stringify(results, null, 2));
      } else {
        copyRepo(dir);
        const r = await buildAndStart(mut, dir);
        child = r.child;
        if (!r.verified) {
          // 校验失败时必须中止：在测一份没被改动的代码时，"存活"毫无意义
          console.log('   ✗ 服务端内容未含变异标记 —— 中止，本变异不给结论');
          results.push({ id: mut.id, desc: mut.desc, skipped: '无法确认服务端是变异版' });
          console.log('');
          continue;
        }
        const res = runE2E(mut.port);
        results.push({ id: mut.id, desc: mut.desc, ...res });
        console.log('   E2E ' + res.passed + '/' + res.total +
          '；抓住它的: ' + (res.failed.length ? res.failed.join(', ') : '（无 —— 存活）'));
        fs.writeFileSync(path.join(WORK, 'results.json'), JSON.stringify(results, null, 2));
      }
    } catch (e) {
      console.log('   EXC: ' + String(e.message).slice(0, 150));
      results.push({ id: mut.id, desc: mut.desc, error: String(e.message).slice(0, 200) });
    } finally {
      if (child) { try { child.kill(); } catch { /* 已退出 */ } }
      await sleep(400);
    }
    console.log('');
  }

  console.log('=== 汇总 ===');
  const caught = results.filter(r => r.failed && r.failed.length);
  const survived = results.filter(r => r.failed && !r.failed.length);
  const skipped = results.filter(r => r.skipped || r.error);
  console.log('  被抓住: ' + caught.length);
  caught.forEach(r => console.log('    ' + r.id + ' ← ' + r.failed.slice(0, 4).join(', ')));
  console.log('  存活: ' + survived.length);
  survived.forEach(r => console.log('    ' + r.id + ' —— ' + r.desc));
  if (skipped.length) {
    console.log('  跳过（不给结论）: ' + skipped.length);
    skipped.forEach(r => console.log('    ' + r.id + ': ' + (r.skipped || r.error)));
  }
  console.log('');
  console.log('⚠ "存活"有三种含义，需人工区分：');
  console.log('   A. 测试有洞   → 补断言（本轮 E9-logfileinfo 就是这种）');
  console.log('   B. 变异无效   → 换一个真正影响行为的注入');
  console.log('   C. 环境不可观测 → 不能算缺陷（如空日志实例）');
  console.log('结果: ' + path.join(WORK, 'results.json'));
  process.exit(0);
})();
