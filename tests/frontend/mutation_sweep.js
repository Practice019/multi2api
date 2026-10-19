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
// # "存活"的四种含义，必须分开（踩过的坑）
//
//   A. 测试真的有洞     → 补测试
//   B. 变异无效         → 换注入（曾把 `if(...)x=..` 换成 `if(true){}else{}`，
//                          对结果毫无影响，却报"存活"）
//   C. 环境不可观测     → 不能算缺陷（曾在一个"日志为空"的新实例上
//                          断言"日志面板要有内容"）
//   D. 判据不在本扫描器跑的集合里 → 要么把它接进套件集，要么把这条变异换到另一组
//                          （S1 就是这种：它真正的守卫是 Go 的 webui_cols_test.go）
//
// 本工具对每一类都有显式处理，而不是把它们混成一个数字。
//
// # 判定口径：减基线（2026-10-19 新增）
//
// 有些套件**在基线上就是红的**（本轮实测：真仓库 25/29、副本 22/29 —— 副本少了 bin/ 与
// .git，三个探针另有红法）。不减基线的话，任何静态变异都会被那几条基线红"抓住"：
// S1..S5 全部显示"被抓住"，实际一条也没被区分出来 —— 与"锚点失效静默变存活"是同一类假信号。
// 所以每轮先在未变异副本/实例上量一次基线，每条变异报"新增失败"，判定用新增口径。
// 跳过基线：MUT_NO_BASELINE=1（省一轮，但结论回到旧口径，可能假阳）。
//
// # 锚点失效时怎么办
//
// 锚点必须在源码里**唯一**命中，否则中止。自检会为每条失效锚点打印：它锚的是哪个行为 /
// 本该被哪个判据打死 / 最长的"还活着的前缀"及源码里命中它的行 / 该串最后一次变更的提交。
// 照着重写锚点，**别删变异** —— 删掉就是丢一条覆盖。
//
// # 端口与目录（别和用户的活实例抢）
//
// 变异实例用 18101–18206、E2E 基线用 18100 —— 离用户的 7863 很远；
// 工作目录 MUT_WORK（默认系统临时目录下的 wb2api-mutsweep）。
// ⚠ E2E 组需要 `go` 在 PATH 上（构建实例）；Chrome 走 chrome_path.js 解析。
// ⚠ E2E 组的实例必须自己配 admin_token 并在 URL 上带 `?token=<admin_token>` —— /ui 现在
//   只认它（**回环地址不再是凭据**）。这是 2026-10-19 修掉的一个静默失效：此前拿到的
//   永远是登录页，E1..E10 十条变异全部"无法确认服务端是变异版"而中止。
//
// # 用法
//
//   node tests/frontend/mutation_sweep.js              # 跑全部（先量静态 + E2E 两组基线）
//   node tests/frontend/mutation_sweep.js e2e          # 只跑浏览器 E2E 那组
//   node tests/frontend/mutation_sweep.js static       # 只跑静态套件那组
//   node tests/frontend/mutation_sweep.js E9-logfileinfo  # 只跑一条（id 或 id 前缀，单点复现用）
//   node tests/frontend/mutation_sweep.js --self-test  # 只验证扫描器自身（锚点唯一性 + 依赖存在）
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn, spawnSync, execFileSync } = require('child_process');

const HERE = __dirname;
const REPO = path.resolve(HERE, '..', '..');
const CHROME = require('./chrome_path.js').resolveChrome();

// 工作目录放系统临时目录（可移植）；可用 MUT_WORK 覆盖
const WORK = process.env.MUT_WORK || path.join(os.tmpdir(), 'wb2api-mutsweep');

// /ui 的换手令牌。只给本扫描器起的**隔离实例**用（端口 181xx / 18100，配置是一次性生成的文件）。
//
// ⚠ 为什么必须有它（2026-10-19 修的一个静默失效）：
//   产品把 /ui 的门改了 —— **回环地址不再是凭据**，只有 `?token=<admin_token>` 换手的 cookie
//   或带 cookie 的请求才给页面本体，否则一律给登录页（约 17KB，带 #loginLocalHint）。
//   而 run-config.example.json 里**没有** admin_token 字段 ⇒ 旧的 buildAndStart 拿到的一直是
//   登录页 ⇒ "服务端内容未含变异标记" ⇒ E1..E10 十条变异全部中止、不给结论（扫描器看着正常，
//   其实 E2E 那一组是废的）。
const ADMIN_TOKEN = 'mutation-sweep-admin-0001';

// ---------------------------------------------------------------------------
// 变异清单
//
// 每条都对应"用户能看出差别"的破坏。字段说明：
//   kind   'e2e' 需要起服务 + 浏览器；'static' 只需副本 + 静态套件
//   find   锚点（必须在源码里**唯一**命中，否则中止）
//   repl   替换文本
//   probe  用来确认"服务端跑的确实是变异体"的特征串（默认取 repl 前 24 字符）
//   caughtBy  本该打死它的判据（自检失败时会打出来，提醒别换成一个"能跑但没判别力"的锚点）
//             ⚠ 只写**实证过**的：基线绿 + 变异后真的红。没实证的写"（未实证）"，
//               别把推测当结论。
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
    desc: 'JS 的 COLS.growth 漂移（与 Go 替换表 / 表头列数脱钩）',
    // 锚的是哪个行为：`const COLS = { travel: 8, growth: 6, … }`（webui.html 的列数常量）。
    //   动态空态/错误行用它推导 colspan（rowPlaceholder / 成长表错误行），静态空态行用
    //   __COLS_<区>__ 占位符（由 Go 的 colPlaceholders 替换）。同一个列数存在**两份** ⇒
    //   漂移就是 B1/B2 那一类：静态行跨 6 列、动态行跨 98 列，页面错位且不报任何错。
    // 被哪个判据打死：
    //   · 正主是 Go 的 internal/server/webui_cols_test.go（TestColPlaceholdersMatchJSCols），
    //     但**不在本扫描器跑的套件集里**（这里只跑 tests/frontend/run_frontend_suites.js）。
    //   · 静态集里**已经没有覆盖**了：实测 growth:99 与 travel:99 两个候选都不产生新的失败
    //     套件。原先是 tokps 套件里 `COLS.logs === 10` 那条断言在抓它；2026-10-19 产品把
    //     logs / jobs 从 COLS 与占位符里删掉，那条断言随之删除（见 tokps_gen.js:155-160）。
    //   ⇒ 本变异在当前集合下会记成"存活"，那是 **A 类真缺口**（不是锚点问题、不是产品缺陷）。
    //     恢复信号的两条路：把 `go test ./internal/server -run TestColPlaceholdersMatchJSCols`
    //     接进套件集；或把这条改成 kind:'e2e'（topstats_e2e 逐表核对"每行是否恰好占满列数"）。
    // 锚点历史：原锚点 'logs: 10,    // 请求日志' 随 2026-10-19 的 COLS 收敛失效（字段已删）。
    find: '    growth: 6,   // 成长计划（精简：额度/能量、连登/补签、待完成/可得、可领 合并）',
    repl: '    growth: 99,  // 成长计划（变异：列数漂移）',
    caughtBy: 'webui_cols_test.go（Go 测试，**不在本扫描器的套件集里**）；静态集自 2026-10-19 起无覆盖' },
  { id: 'S2-emoji', kind: 'static',
    desc: '设置面板塞入 emoji（项目硬规则：无 emoji）',
    // 锚的是哪个行为：设置面板里那条 <legend>（分组标题 + 生效时机徽章）。它落在
    //   gen_settings_layout.js 按 DOM 结构切片出的设置面板 <section> 里。
    // 被哪个判据打死：settings_layout_gen.js 的 [E] 节 `!emoji.test(sec)` —— **基线绿**，
    //   实测：本变异把副本的通过数从 22/29 打到 21/29，唯一新增失败就是 settings_layout_gen.js。
    //   ⚠ gen_ui_manifest.js 也有一条 emoji 断言，但它**基线就红**（脚本字符串字面量里已经有
    //     emoji）⇒ 它抓不住这条变异，别把它当证据。
    // 锚点历史：原锚点 '<legend>调度（保存后立即生效）</legend>' 随设置页收敛消失
    //   （现在的 legend 都是「基础文案 + 徽章」两段式，`调度` 那组已改成「日常调度」）。
    // ⚠ repl 用码点拼 ✅（U+2705）而不是写 emoji 字面量：免得工具源码自己带 emoji，
    //   与"无 emoji"这条口径纠缠不清，也免了跨编码往返的风险。等价于
    //   '<legend>成长中心自动化 <span class="badge badge--hot">立即生效</span></legend>✅'。
    find: '<legend>成长中心自动化 <span class="badge badge--hot">立即生效</span></legend>',
    repl: '<legend>成长中心自动化 <span class="badge badge--hot">立即生效</span></legend>' + String.fromCodePoint(0x2705),
    caughtBy: 'settings_layout_gen.js（基线绿，已实证）' },
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
function get(url, headers) {
  return new Promise((res, rej) => {
    http.get(url, { headers: headers || {} }, r => {
      let d = ''; r.on('data', c => d += c);
      // 带 headers：E2E 组要用 admin_token 换手来的 cookie 取干净的 /ui（见 buildAndStart）
      r.on('end', () => res({ status: r.statusCode, body: d, headers: r.headers }));
    }).on('error', rej);
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

// ---------------------------------------------------------------------------
// 锚点自检
//
// 每个 find 必须在源码里**唯一**命中。不做这一步的话，"锚点找不到"会静默变成
// "变异存活" —— 工具废掉，而且废得比红更糟：它还会给出一个看起来正常的数字。
//
// ⚠ 2026-10-19 加固：原版只打印一行"命中 N 次（应唯一）"就中止，而**为什么失效**
//   才是要修的东西。现在每条无效锚点都给四样东西：
//     1. 它锚的是哪个行为（desc）
//     2. 本该由哪个判据打死（caughtBy）—— 提醒别随手换成一个"能跑但没判别力"的锚点
//     3. 可复制的重锚线索：最长的"还活着的前缀" + 源码里命中它的行
//     4. 该串在 webui.html 里最后一次变更的提交（git log -S）—— 大多是产品把它改/删了
//   中止行为不变：锚点无效时结果无意义。
// ---------------------------------------------------------------------------
function occurrences(src, needle) {
  const out = []; let i = src.indexOf(needle);
  while (i >= 0) { out.push(i); i = src.indexOf(needle, i + 1); }
  return out;
}
function lineAt(src, idx) { return src.slice(0, idx).split('\n').length; }
function describeAt(src, idx) {
  const lines = src.split('\n');
  const n = lineAt(src, idx);
  return String(n).padStart(5) + ': ' + (lines[n - 1] || '').trim().slice(0, 100);
}
// 重锚线索：找**最长的还活着的前缀**。例：整条 <legend>调度（保存后立即生效）</legend> 没了，
// 但 '<legend>' 还在 —— 把它的行全列出来，人照着写新锚点。从长到短试，第一个有命中的
// 长度信息量最大。
function reanchorHints(src, find) {
  for (let L = Math.min(find.length - 1, 80); L >= 4; L--) {
    const prefix = find.slice(0, L);
    const hits = occurrences(src, prefix);
    if (hits.length) return { prefix, hits };
  }
  return null;
}
// 该串在 webui.html 里最后一次"增/删"的提交（git 不在、或不是 git 仓库时返回 null）。
function lastCommitTouching(text) {
  try {
    const out = execFileSync('git',
      ['log', '-1', '--format=%h %ad %s', '--date=short', '-S', text, '--', 'internal/server/webui.html'],
      { cwd: REPO, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
    return out || null;
  } catch { return null; }
}
function selfCheck(muts) {
  console.log('=== 锚点自检 ===');
  let bad = 0;
  const cache = {};
  const rel = 'internal/server/webui.html';
  for (const m of muts) {
    if (!cache[rel]) cache[rel] = fs.readFileSync(path.join(REPO, rel), 'utf8');
    const src = cache[rel];
    const n = src.split(m.find).length - 1;
    if (n === 1) { console.log('  ✓ ' + m.id.padEnd(20) + ' 唯一'); continue; }
    bad++;
    console.log('  ✗ ' + m.id.padEnd(20) + ' 命中 ' + n + ' 次（应唯一）');
    console.log('      行为: ' + (m.desc || '(未写)') + '  [kind=' + m.kind + ']');
    console.log('      本该打死它: ' + (m.caughtBy || '(未标注 —— 换锚点前先想清楚这条变异证明什么)'));
    console.log('      锚点(' + rel + '): ' + JSON.stringify(m.find));
    console.log('      该串最后一次变更: ' + (lastCommitTouching(m.find) ||
      '（git 查不到：从没出现过，或不在 git 仓库里）'));
    if (n === 0) {
      const hint = reanchorHints(src, m.find);
      if (!hint) { console.log('      重锚线索: 连 4 个字符的前缀都不在源码里 —— 这段文本可能被整块删了'); continue; }
      console.log('      重锚线索: 前缀 ' + JSON.stringify(hint.prefix) + ' 仍命中 ' + hint.hits.length + ' 处：');
      for (const idx of hint.hits.slice(0, 6)) console.log('        ' + describeAt(src, idx));
      if (hint.hits.length > 6) console.log('        …还有 ' + (hint.hits.length - 6) + ' 处');
      console.log('      ⇒ 先看清产品把它改成了什么，再决定是**改锚点**还是**删变异**；' +
        '删变异会丢掉这条覆盖，别默默删。');
    } else {
      console.log('      命中位置（多处 ⇒ 锚点不够唯一，会改错地方：加长到唯一为止）：');
      for (const idx of occurrences(src, m.find).slice(0, 6)) console.log('        ' + describeAt(src, idx));
    }
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
// ⚠ base-url 必须带 ?token=<admin_token>：/ui 换手一次 cookie 后 302 回干净的 /ui，而各 E2E
//   脚本拿的是同一个 base-url（第一个导航就把 cookie 装进自己的 Chrome profile）。
function runE2E(port) {
  const env = Object.assign({}, process.env);
  const r = spawnSync('node', [path.join(HERE, 'run_e2e.js'),
    'http://127.0.0.1:' + port + '/ui?token=' + encodeURIComponent(ADMIN_TOKEN)],
    { cwd: HERE, encoding: 'utf8', env, timeout: 20 * 60 * 1000 });
  const out = (r.stdout || '') + (r.stderr || '');
  const m = out.match(/=== (\d+)\/(\d+) 通过 ===/);
  const failed = [...out.matchAll(/FAIL\s+(\S+\.js)/g)].map(x => x[1]);
  return { passed: m ? +m[1] : null, total: m ? +m[2] : null, failed };
}

// 收尾：把**真仓库**里的派生产物按未变异源码重生成一次。
//
// 为什么必要：静态组的套件是跑在真仓库的 cwd 上（只把 WB2API_REPO 指向副本）—— 这样那几个
// 依赖真实检出/实例/Chrome 的探针才能正常工作。但 generator 会把产物写到 cwd，于是**变异内容
// 会被写进真仓库的产物**：大到 *_gen.js（夹具里嵌了被变异的片段），小到 tdz_probe.html
// （它是整页 webui.html 的副本）。实测一轮静态扫描会留下 3 个这样的文件（growth_labels_gen.js /
// growth_pending_ui_gen.js / tdz_probe.html，内容是 COLS.growth=99 那版）。
// 产物是派生物、不许手改 ⇒ 唯一正确的收尾就是按**未变异**源码重生成一遍。
function restoreDerived() {
  console.log('=== 收尾：按未变异源码重生成派生产物（清掉扫描残留在真仓库的变异内容）===');
  const r = runStaticSuites(REPO);
  console.log('  重生成后 ' + r.passed + '/' + r.total + ' 通过；失败: ' +
    (r.failed.join(', ') || '（无）') + '\n');
  return r;
}
// 起一个（变异版或基线版）实例，并**校验服务端内容确实是变异版**。
// mut.noop = true 时只构建+启动，不注入 —— 用来量 E2E 组的基线红名单（见下）。
async function buildAndStart(mut, dir) {
  const f = path.join(dir, 'internal/server/webui.html');
  if (!mut.noop) {
    const src = fs.readFileSync(f, 'utf8');
    if (!src.includes(mut.find)) throw new Error('副本里锚点未找到');
    fs.writeFileSync(f, src.replace(mut.find, mut.repl));
  }

  const exe = path.join(WORK, mut.id + '.exe');
  rmrf(exe);
  execFileSync('go', ['build', '-a', '-o', exe, '.\\cmd\\server'],
    { cwd: dir, env: Object.assign({}, process.env, { CGO_ENABLED: '0' }), stdio: 'pipe' });
  if (!fs.existsSync(exe)) throw new Error('构建产物不存在');

  const cfgPath = path.join(WORK, mut.id + '.json');
  const cfg = JSON.parse(fs.readFileSync(path.join(REPO, 'tests', 'frontend', 'run-config.example.json'), 'utf8'));
  cfg.listen = '127.0.0.1:' + mut.port;
  // 必须自己给一个 admin_token（示例配置里没有）：不然 /ui 只给登录页，特征串永远匹配不上
  cfg.admin_token = ADMIN_TOKEN;
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
  // 关键校验：服务端下发的页面必须含变异特征。
  // ⚠ 分两步取，不能直接 get('/ui')：
  //   ① `/ui?token=<admin_token>` → 302 + Set-Cookie（回环地址不是凭据，见 ADMIN_TOKEN 那段）
  //   ② 带上这个 cookie 再取 `/ui` → 才是**页面本体**（不带 cookie 拿到的是登录页）
  const base = 'http://127.0.0.1:' + mut.port;
  const handoff = await get(base + '/ui?token=' + encodeURIComponent(ADMIN_TOKEN));
  const cookie = ((handoff.headers && handoff.headers['set-cookie']) || [])
    .map(c => String(c).split(';')[0]).join('; ');
  const ui = await get(base + '/ui', cookie ? { Cookie: cookie } : {});
  const loginPage = ui.body.includes('loginLocalHint');
  // 基线实例没被改过，没有"特征串"可比 —— 但要照样换手，否则连"页面到手了"都证明不了
  if (mut.noop) {
    return { child, verified: true, probe: '(未变异基线)', status: handoff.status, bodyLen: ui.body.length, loginPage };
  }
  const probe = (mut.probe || mut.repl).replace('// MUT', '').trim().slice(0, 24);
  return { child, verified: ui.body.includes(probe), probe, status: handoff.status, bodyLen: ui.body.length, loginPage };
}

// ---------------------------------------------------------------------------
// 基线对照（2026-10-19 新增）
//
// 为什么必须减基线：本工具把"有套件失败"直接当成"变异被抓住了"，但**有些套件在基线上就是红的**。
// 本轮实测：真仓库 25/29 通过、副本 22/29 —— 副本少了 bin/ 与 .git，有三个探针在副本里另有红法。
// 不减基线的话，任何静态变异都会被那几条基线红"抓住"：S1..S5 全部显示"被抓住"，
// 而实际上一条也没被区分出来 —— 这跟"锚点失效静默变成存活"是同一类错误（假信号）。
//
// 做法：先用**完全相同的路径**在未变异副本上跑一遍，拿到基线红名单；每条变异再报"新增失败"。
//   · 旧字段 res.failed 原样保留（可对照）
//   · 判定"抓住 / 存活"改用新增口径 res.newFailed
// 跳过基线：MUT_NO_BASELINE=1（省时间，但结论回到"没减基线"的旧口径，会假阳）。
// ---------------------------------------------------------------------------
function newFailures(res, base) {
  if (!res || !res.failed) return null;
  const b = new Set((base && base.failed) || []);
  return res.failed.filter(x => !b.has(x));
}
function baselineStatic() {
  const dir = path.join(WORK, '_baseline_static');
  console.log('=== 基线（未变异副本 · 静态组，跑两次取并集）===');
  copyRepo(dir);
  const a = runStaticSuites(dir);
  const b = runStaticSuites(dir);
  const red = Array.from(new Set([...a.failed, ...b.failed]));
  const flaky = red.filter(x => a.failed.includes(x) !== b.failed.includes(x));
  console.log('  静态 ' + a.passed + '/' + a.total + ' 与 ' + b.passed + '/' + b.total +
    '；基线就红（并集）: ' + (red.length ? red.join(', ') : '（无）'));
  if (flaky.length) {
    console.log('  ⚠ 基线自己就抖: ' + flaky.join(', ') +
      '（依赖实例/Chrome/端口的探针）—— 取并集 ⇒ 宁可少报"抓住"，也不假阳');
  }
  console.log('  ⇒ 下面每条变异的"抓住它的"是**减掉这份基线**之后的新增失败\n');
  rmrf(dir);
  return { passed: Math.min(a.passed, b.passed), total: a.total, failed: red };
}
async function baselineE2E(port) {
  const dir = path.join(WORK, '_baseline_e2e');
  console.log('=== 基线（未变异实例 · E2E 组）===');
  let child = null;
  try {
    copyRepo(dir);
    const r = await buildAndStart({ id: '_baseline_e2e', kind: 'e2e', port, find: '', repl: '', noop: true }, dir);
    child = r.child;
    const res = runE2E(port);
    console.log('  E2E ' + res.passed + '/' + res.total + '；基线就红的脚本: ' +
      (res.failed.length ? res.failed.join(', ') : '（无）'));
    console.log('  ⇒ 下面每条变异的"抓住它的"是**减掉这份基线**之后的新增失败\n');
    return res;
  } catch (e) {
    // 基线起不来（没 go / 端口被占）不该让整轮扫描停下 —— 那正是"跑到底"要保证的
    console.log('  ⚠ 基线起不来: ' + String(e.message).slice(0, 120));
    console.log('  ⇒ E2E 组这次不带基线对照，结论按旧口径（可能假阳）\n');
    return null;
  } finally {
    if (child) { try { child.kill(); } catch { /* 已退出 */ } }
    rmrf(dir);
  }
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
  // 选择器：'all' / 'static' / 'e2e' 按组；写成 id（或 id 前缀）则只跑那一条 —— 单点复现用，
  // 免得为了验一条变异把整轮（静态 10 分钟 + E2E 一小时）跑一遍。
  const byId = MUTATIONS.filter(m => m.id === mode || m.id.startsWith(mode));
  const list = mode === 'all' ? MUTATIONS
    : (byId.length ? byId : MUTATIONS.filter(m => m.kind === mode));
  if (!list.length) { console.log('没有匹配的变异: ' + mode); process.exit(2); }
  if (!selfCheck(list)) process.exit(2);

  // 基线（只对本次要跑的组算；MUT_NO_BASELINE=1 跳过 —— 见上面的长注释）
  let baseStatic = null, baseE2E = null;
  if (!process.env.MUT_NO_BASELINE) {
    if (list.some(m => m.kind === 'static')) baseStatic = baselineStatic();
    // E2E 基线端口：18100（变异组用 18101+，留出隔壁一格，互不撞）
    if (list.some(m => m.kind === 'e2e')) baseE2E = await baselineE2E(18100);
  }

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
        // 跑两次以排除产物陈旧带来的非确定性（第三轮踩过）。
        // ⚠ 一致性判据用**新增失败的集合**，不是"通过数全等"：套件里混着几个依赖实例/Chrome/
        //   端口的探针（verify_accts_delegation / verify_checkin_ui_e2e / verify_all_buttons_e2e /
        //   gen_tdz_guard），它们自己就会抖。要求通过数全等 ⇒ 一个无关探针抖一下就把整条结论
        //   丢掉（本轮实测 S2/S4 就是这样被丢的），而"两次都新增的失败"才是这条变异自己的信号。
        const r1 = runStaticSuites(dir);
        const r2 = runStaticSuites(dir);
        const nf1 = newFailures(r1, baseStatic) || [];
        const nf2 = newFailures(r2, baseStatic) || [];
        const both = nf1.filter(x => nf2.includes(x));
        const flaky = r1.failed.filter(x => !r2.failed.includes(x))
          .concat(r2.failed.filter(x => !r1.failed.includes(x)));
        const rec = { id: mut.id, desc: mut.desc, ...r1, passedRuns: [r1.passed, r2.passed],
          newFailed: both, newFailedRuns: [nf1, nf2] };
        if (both.length) {
          console.log('   静态 ' + r1.passed + '/' + r1.total + '（两次 ' + r1.passed + ' / ' + r2.passed + '）' +
            '；抓住它的（两次都新增）: ' + both.join(', '));
        } else if (!nf1.length && !nf2.length) {
          console.log('   静态 ' + r1.passed + '/' + r1.total + '（两次 ' + r1.passed + ' / ' + r2.passed + '）' +
            '；抓住它的（新增）: （无 —— 两次都存活）');
        } else {
          // 只有一次新增：不能用（这次"抓住"可能来自抖动），但也别只说一句"不一致"就完 ——
          // 把两次的清单都摊开，读者才能判断是哪一种。
          console.log('   ⚠ 两次结论不同 —— 第一次新增 ' + JSON.stringify(nf1) +
            '，第二次新增 ' + JSON.stringify(nf2));
          console.log('      第一次失败 ' + JSON.stringify(r1.failed) + ' / 第二次失败 ' + JSON.stringify(r2.failed));
          rec.newFailed = [];
          rec.skipped = '两次结论不一致（单次新增: ' + JSON.stringify(nf1.length ? nf1 : nf2) + '）';
        }
        rec.flaky = flaky;
        if (flaky.length) {
          console.log('      ⚠ 两次失败清单不一致的套件: ' + flaky.join(', ') +
            '（环境依赖探针抖动，与本次变异无关；基线并集口径已尽量吸收）');
        }
        if (baseStatic) {
          console.log('      基线 ' + baseStatic.passed + '/' + baseStatic.total + '；本变异失败套件: ' +
            (r1.failed.length ? r1.failed.join(', ') : '（无）'));
        }
        results.push(rec);
        fs.writeFileSync(path.join(WORK, 'results.json'), JSON.stringify(results, null, 2));
      } else {
        copyRepo(dir);
        const r = await buildAndStart(mut, dir);
        child = r.child;
        if (!r.verified) {
          // 校验失败时必须中止：在测一份没被改动的代码时，"存活"毫无意义。
          // 但要给出足够的线索 —— 只说一句"未含变异标记"没法区分"锚点没生效"和"拿到的是登录页"。
          console.log('   ✗ 服务端内容未含变异标记 —— 中止，本变异不给结论');
          console.log('      /ui 长度 ' + r.bodyLen + '；换手状态 ' + r.status +
            (r.loginPage ? '；下发的是**登录页**（admin_token 换手没生效？）' : '') +
            '；期望含 ' + JSON.stringify(r.probe));
          results.push({ id: mut.id, desc: mut.desc, skipped: '无法确认服务端是变异版' });
          console.log('');
          continue;
        }
        const res = runE2E(mut.port);
        const nf2 = newFailures(res, baseE2E);
        results.push({ id: mut.id, desc: mut.desc, ...res, newFailed: nf2 });
        console.log('   E2E ' + res.passed + '/' + res.total +
          '；抓住它的（新增）: ' + (nf2.length ? nf2.join(', ') : '（无 —— 存活）'));
        if (baseE2E && nf2.length !== res.failed.length) {
          console.log('      本变异失败脚本: ' + (res.failed.length ? res.failed.join(', ') : '（无）'));
        }
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

  // 静态组跑完必须收尾：把真仓库的派生内容按未变异源码重生成（见 restoreDerived 的长注释）
  if (list.some(m => m.kind === 'static')) restoreDerived();

  console.log('=== 汇总 ===');
  // 判定口径：优先用**新增失败**（减掉基线红名单）；没跑基线时回落旧口径 r.failed。
  const failsOf = r => (r.newFailed || r.failed || null);
  const skipped = results.filter(r => r.skipped || r.error);
  const judged = results.filter(r => !r.skipped && !r.error);
  const caught = judged.filter(r => (failsOf(r) || []).length);
  const survived = judged.filter(r => !(failsOf(r) || []).length);
  console.log('  被抓住: ' + caught.length + '（口径：' +
    (baseStatic || baseE2E ? '新增失败 = 本变异失败清单 − 基线红名单' : '失败清单，未减基线') + '）');
  caught.forEach(r => console.log('    ' + r.id + ' ← ' + failsOf(r).slice(0, 4).join(', ')));
  console.log('  存活: ' + survived.length);
  survived.forEach(r => console.log('    ' + r.id + ' —— ' + r.desc));
  if (skipped.length) {
    console.log('  跳过（不给结论）: ' + skipped.length);
    skipped.forEach(r => console.log('    ' + r.id + ': ' + (r.skipped || r.error)));
  }
  if (baseStatic) console.log('  静态基线: ' + baseStatic.passed + '/' + baseStatic.total +
    ' 通过；基线就红: ' + (baseStatic.failed.join(', ') || '（无）'));
  if (baseE2E) console.log('  E2E 基线: ' + baseE2E.passed + '/' + baseE2E.total +
    ' 通过；基线就红: ' + (baseE2E.failed.join(', ') || '（无）'));
  if (!baseStatic && !baseE2E) console.log('  ⚠ 未跑基线（MUT_NO_BASELINE=1）：上面的"抓住"可能来自基线本来就红的套件');
  console.log('');
  console.log('⚠ "存活"有四种含义，需人工区分：');
  console.log('   A. 测试有洞   → 补断言（本轮 E9-logfileinfo 就是这种）');
  console.log('   B. 变异无效   → 换一个真正影响行为的注入');
  console.log('   C. 环境不可观测 → 不能算缺陷（如空日志实例）');
  console.log('   D. 判据不在本扫描器跑的集合里 → 要么把它接进套件集，要么换一组（S1 就是这种：' +
    '守卫在 Go 的 webui_cols_test.go 里）');
  console.log('结果: ' + path.join(WORK, 'results.json'));
  process.exit(0);
})();
