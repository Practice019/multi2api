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
// 有些套件**在基线上就是红的**（2026-10-19 本轮开始时实测：真仓库 25/29、副本 22/29，4 个套件
// 基线就红；收工时同一批套件已被别的修复带到 29/29 —— 所以基线**每轮现量**，别照抄这里的数字）。
// 不减基线的话，任何静态变异都会被那几条基线红"抓住"：S1..S5 全部显示"被抓住"，
// 实际一条也没被区分出来 —— 与"锚点失效静默变存活"是同一类假信号。
// 所以每轮先在未变异副本/实例上量一次基线（静态组跑两次取**并集**，宁可少报"抓住"）。
//
// ⚠ 一致性判据也受同一件事影响：套件里混着依赖实例/Chrome/端口的探针，它们自己就会抖，
//   所以"两次通过数全等"这种口径会把**无关抖动**判成"不给结论"（S2/S4 就这样被丢过）。
//   现在用"**两次都新增的失败**"（交集）作判定，抖动的套件单独列出来。
//
// 跳过基线：MUT_NO_BASELINE=1（省一轮，但结论回到旧口径，可能假阳）。
//
// # 锚点失效时怎么办
//
// 锚点必须在源码里**唯一**命中，否则中止。自检会为每条失效锚点打印：它锚的是哪个行为 /
// 本该被哪个判据打死 / 最长的"还活着的前缀"及源码里命中它的行 / 该串最后一次变更的提交。
// 照着重写锚点，**别删变异** —— 删掉就是丢一条覆盖。
//
// # 两条硬纪律（2026-10-19 的 262b796 事故换来的，别删）
//
//  ⚠ **跑别人的生成器/夹具时，`cwd` 与 `WB2API_REPO` 必须一起对齐 —— 只对齐一个就会"读 A 写 B"。**
//    旧版本正是这样：`WB2API_REPO=副本` 但 `cwd=真仓库`，于是 html 读副本、**产物写进真仓库**。
//    现在静态组连 cwd 一起放进副本（见 runStaticSuites）。
//  ⚠ **变异跑在真仓库期间，`git status` 干净 ≠ 工作树没有变异值 —— 别提交。**
//    产物既没被删也没换路径，`git status` 会显示"和索引一致"，它证明不了任何事：那段时间里
//    父代理的一次 `git add` 就把注入值 `growth: 99` 提交进了 262b796。要判断有没有被注入，
//    靠的是**内容级**检查（本文件的 `--restore-only` 变异标记物扫描 / 派生物与源码的逐字节比对），
//    不是 `git status`。
//
// # 端口与目录（别和用户的活实例抢）
//
// 变异实例用 18101–18206、E2E 基线用 18100 —— 离用户的 7863 很远；
// 工作目录 MUT_WORK（默认系统临时目录下的 wb2api-mutsweep）。
// ⚠ **真仓库 cwd 零写入**：所有变异都在副本里注入、构建、跑套件 —— 所以本工具跑完，
//   真仓库的 `git status` 不应该多出任何文件。
// ⚠ **副本里的基线比真仓库低几项**（副本没有 `.git` ⇒ test_shape_scan 的 `git ls-files` 必红，
//   真仓库是 29/29，副本约 27/29）。这不是谁坏了，是减基线口径要吸收的东西；
//   两边的基线数**不能互相比较**。
// ⚠ E2E 组需要 `go` 在 PATH 上（构建实例）；静态组里声明了 goTest 的变异也要（见 runGoTests）。
//   Chrome 走 chrome_path.js 解析。
// ⚠ E2E 组的实例必须自己配 admin_token 并在 URL 上带 `?token=<admin_token>` —— /ui 现在
//   只认它（**回环地址不再是凭据**）。这是 2026-10-19 修掉的一个静默失效：此前拿到的
//   永远是登录页，E1..E10 十条变异全部"无法确认服务端是变异版"而中止。
//   实证：修前 E2E 基线脚本 panels_e2e.js FAIL；修后 PASS，E9 单点走完全程 exit 0。
//
// # 用法
//
//   node tests/frontend/mutation_sweep.js              # 跑全部（先量静态 + E2E 两组基线）
//   node tests/frontend/mutation_sweep.js e2e          # 只跑浏览器 E2E 那组
//   node tests/frontend/mutation_sweep.js static       # 只跑静态套件那组
//   node tests/frontend/mutation_sweep.js E9-logfileinfo  # 只跑一条（id 或 id 前缀，单点复现用）
//   node tests/frontend/mutation_sweep.js --self-test  # 只验证扫描器自身（锚点唯一性 + 依赖存在）
//   node tests/frontend/mutation_sweep.js --restore-only  # 只做复原：扫派生产物里的变异标记物，
//                                                        # 有就按未变异源码重生成（怀疑工作树被注入时用）
//   环境变量：MUT_WORK=工作目录｜MUT_NO_BASELINE=1 跳过基线（结论回落旧口径）｜
//            WB2API_GO=<go 的绝对路径>（go 不在 PATH 时；本机一般是 D:\go-path\go\bin\go.exe）
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const crypto = require('crypto');
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
    // 锚的是哪个行为：`const COLS = { travel: 8, growth: 6 }`（webui.html:3038-3044 的列数常量）。
    //   动态空态/错误行用它推导 colspan（rowPlaceholder / 成长表错误行），静态空态行用
    //   __COLS_<区>__ 占位符（由 Go 的 colPlaceholders 替换）。同一个列数存在**两份** ⇒
    //   漂移就是 B1/B2 那一类：静态行跨 6 列、动态行跨 98 列，页面错位且不报任何错。
    // 被哪个判据打死：**Go 的 internal/server/webui_cols_test.go**（TestColPlaceholdersMatchJSCols）
    //   —— 已用 `goTest` 把它接进本扫描器（2026-10-19，见 runGoTests 的 ENV 纪律）。
    //   · 为什么要接：静态集里**已经没有任何覆盖**了。原先是 tokps 套件里
    //     `COLS.logs === 10` 那条断言在抓它；2026-10-19 产品把 logs / jobs 从 COLS 与占位符里
    //     删掉，那条断言随之删除（tokps_gen.js:155-160）⇒ 实测 growth:99 与 travel:99 两个候选
    //     都不产生任何新增失败套件。不接 Go 判据时，本变异只能报"存活"，而读者会误读成
    //     "测试有洞"（其实是 D 类：判据不在集合里）。
    //   · go 不可用时（不在 PATH / WB2API_GO 指不到）：报 ENV，本变异**未计入** ——
    //     既不算被抓住、也不算存活。别把 ENV 读成"存活"。
    // 锚点历史：原锚点 'logs: 10,    // 请求日志' 随 2026-10-19 的 COLS 收敛失效（字段已删，
    //   同一处注释 webui.html:3043 写着 `__COLS_JOBS__` / `__COLS_LOGS__` 占位符也一并删了）。
    find: '    growth: 6,   // 成长计划（精简：额度/能量、连登/补签、待完成/可得、可领 合并）',
    repl: '    growth: 99,  // 成长计划（变异：列数漂移）',
    // 静态组额外跑的 Go 判据（正主）。ENV 纪律见 runGoTests：不看退出码，只认 `--- PASS: <名字>`。
    goTest: ['TestColPlaceholdersMatchJSCols'],
    caughtBy: 'webui_cols_test.go 的 TestColPlaceholdersMatchJSCols（经 goTest 接进本扫描器，2026-10-19）' },
  { id: 'S2-emoji', kind: 'static',
    desc: '设置面板塞入 emoji（项目硬规则：无 emoji）',
    // 锚的是哪个行为：设置面板里那条 <legend>（分组标题 + 生效时机徽章）。它落在
    //   gen_settings_layout.js 按 DOM 结构切片出的设置面板 <section> 里。
    // 被哪个判据打死：**两条**（实测：static 扫描从基线 29/29 掉到 27/29，两次一致，
    //   两条都是"新增失败"）
    //   · settings_layout_gen.js 的 [E] 节 `!emoji.test(sec)`（gen_settings_layout.js:406-409）
    //   · gen_ui_manifest.js 的 [5] 节「零外部依赖与无 emoji」（gen_ui_manifest.js:284+）
    //   ⚠ 后者在本轮开始时**基线就红**（脚本字符串字面量里已经有 emoji）⇒ 那时它抓不住这条
    //     变异、不能当证据；收工时它已被别的修复带到绿，实测两次都新增 ⇒ 现在才是有效判据。
    //     教训：判据算不算"有效证据"要现量 —— 基线红名单会随别人的修复变动。
    // 锚点历史：原锚点 '<legend>调度（保存后立即生效）</legend>' 随设置页收敛消失
    //   （现在 legend 都是「基础文案 + 生效时机徽章」两段式：webui.html:1637 日常调度/自动执行、
    //    1643 成长中心自动化/立即生效、1656 历史与日志、1667 网络与容错、1676 会话粘性、
    //    1682 运行信息；`调度` 那组已改名「日常调度」）。
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
//
// 排除项只有 `.git`：4 个变异 × 每次 200MB+ 的 .git 不划算，而且副本里带一份"会自己动的索引"
// 只会让"我以为在副本里"更难判断。`bin` 照拷（有探针按 REPO 相对路径找 bin\wb2api-server.exe）。
// ⚠ **别指望副本基线等于真仓库基线**：副本没有 .git，`test_shape_scan` / `audit_tracked_content`
//   这类"按 git 走"的判据在副本里必红，另有几条依赖真实检出/exe 的探针在副本里另有红法。
//   两边的基线数**不能互相比较**；每轮跑出来的基线数都打印在"基线"那一行，减基线口径自动吸收。
function copyRepo(dest) {
  rmrf(dest);
  const rc = spawnSync('robocopy', [REPO, dest, '/E', '/XD', '.git',
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

// ---------------------------------------------------------------------------
// 静态组的 Go 判据（2026-10-19 新增）
//
// 为什么需要它：有些契约的守卫**不在 tests/frontend 里**，而在 Go 测试里 —— 最典型的是
// internal/server/webui_cols_test.go 的 TestColPlaceholdersMatchJSCols：它守"JS 的 COLS 常量
// 与 Go 的 colPlaceholders 替换表逐项相等"，也就是 S1-colspan-drift 的**正主判据**。
// 不把它接进来，S1 只能报"存活"，而那是 D 类（判据不在集合里），下一个人很容易误读成"测试有洞"。
//
// # ENV 纪律（抄 verify_sign_against_spec.js 那套）
//   · go 不在默认 PATH 上是本机的常态（它在 D:\go-path\go\bin\go.exe，1.22.5）。
//     用 WB2API_GO 覆盖，或把那个目录加进 PATH。
//   · **找不到 go ⇒ 大声报 ENV、不并入任何计数**（既不算"被抓住"、也不算"存活"）：
//     判据根本没跑成，静默当"存活"就是假绿 —— 比少一条判据严重得多。
//   · 判据**不看退出码**：`go test -run <不存在的名字>` 也是 exit 0 ⇒ 必须 `-count=1 -v`
//     并要求输出里有 `--- PASS: <名字>`。（这条踩坑记录在 verify_sign_against_spec.js 里。）
// ---------------------------------------------------------------------------
function goBin() { return process.env.WB2API_GO || 'go'; }
function runGoTests(mut, dir) {
  const names = mut.goTest || [];
  if (!names.length) return { failed: [], env: null };
  const failed = [];
  for (const name of names) {
    const r = spawnSync(goBin(), ['test', '-count=1', '-v', '-run', '^' + name + '$', './internal/server'],
      { cwd: dir, encoding: 'utf8', env: Object.assign({}, process.env, { CGO_ENABLED: '0' }) });
    if (r.error && r.error.code === 'ENOENT') {
      return { failed: [], env: '找不到 go（试过 "' + goBin() + '"）。本机一般在 ' +
        'D:\\go-path\\go\\bin\\go.exe（go1.22.5，不在默认 PATH）—— 用 WB2API_GO 指定它，' +
        '或把那个目录加进 PATH' };
    }
    const out = String(r.stdout || '') + String(r.stderr || '');
    if (!new RegExp('^\\s*--- PASS: ' + name + '\\b', 'm').test(out)) failed.push('go:' + name);
  }
  return { failed, env: null };
}

// 静态套件：**在副本里**跑（2026-10-19 改，这条是踩出来的，别改回去）
//
// ⚠ 为什么 cwd 也必须在副本里：
//   旧版是 `spawnSync(node, HERE/run_frontend_suites.js, { cwd: HERE, env: WB2API_REPO=副本 })`——
//   html 读副本，但 generator **按 cwd 写产物** ⇒ 注入值落进**真仓库**的 *_gen.js 与
//   tdz_probe.html。更糟的是 `git status` 在那期间**会自己一致**（产物既没被删也没改路径），
//   它证明不了任何事：父代理的一次 `git add` 就把 `growth: 99` 提交进了 262b796。
//   一句话：**变异只要在真仓库的 cwd 上跑，工作树就不再是"人的工作树"。**
//   现在 cwd = 副本的 tests/frontend ⇒ html 与产物都在副本，真仓库零写入。
//
// 代价（已实测并吸收）：副本没有 .git，于是 test_shape_scan / audit_tracked_content 这类
//   "按 git 走"的判据在副本里另有红法 —— 副本基线因此比真仓库低几项（**两边的基线数不能互相
//   比较**，每轮的基线数都打印在输出里）。但这些红**在基线里也有**，减基线口径正好抵消；
//   真正抓变异的判据（settings_layout_gen / pagesize_persist_gen / gen_busy_feedback /
//   gen_b5_normalize）在副本里照样能抓住。
function runStaticSuites(dir) {
  const env = Object.assign({}, process.env, { WB2API_REPO: dir });
  const runner = path.join(dir, 'tests', 'frontend', 'run_frontend_suites.js');
  const r = spawnSync('node', [runner], { cwd: path.dirname(runner), encoding: 'utf8', env });
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

// ---------------------------------------------------------------------------
// 派生产物簿记 / 复原（2026-10-19，覆盖"被中断"这条路）
//
// 为什么需要：`_gen.js` / `tdz_probe.html` 都是 **generator 的派生物**（不许手改）。
// 只要变异在真仓库的 cwd 上跑过一轮，产物里就可能带着**变异内容**；更阴的是
// `git status` 在那期间**会自己一致** —— 产物既没被删也没换路径，于是它显示"和索引一致"，
// 而内容已经被换掉了。父代理就是这么把 `growth: 99` 一起提交进 262b796 的。
// 一句话：**git status 干净 ≠ 工作树没有变异值。**
//
// 所以：
//   · 主防线：静态组已经改成"在副本 cwd 里跑"（见 runStaticSuites）—— 真仓库零写入，从源头掐掉。
//   · 兜底（这里）：启动时记下派生产物的哈希 + 扫一遍变异标记物；收尾比对，不一致就按
//     **未变异源码**重生成再比对；真复原不了，就把"哪几个文件 + 一条复原命令"打到最显眼处
//     （只说一句 warning 不够 —— 下一个人会在假状态上做结论）。
//   · 整轮包 try/finally + 挂 SIGINT/SIGTERM：一轮 all 要 90 分钟，被 Ctrl-C / kill 掉是常态。
//   · 单跑 `--restore-only`：只做这件事并打印两轮状态（任何人怀疑树被污染时都可以当体检用）。
// ---------------------------------------------------------------------------
const DERIVED_EXTRA = ['tdz_probe.html'];   // 不在 *_gen.js 里，但同样是派生物
function derivedFiles() {
  try {
    return fs.readdirSync(HERE).filter(f => /_gen\.js$/.test(f)).concat(DERIVED_EXTRA)
      .filter(f => fs.existsSync(path.join(HERE, f)));
  } catch { return []; }
}
function hashFile(f) {
  try { return crypto.createHash('sha256').update(fs.readFileSync(path.join(HERE, f))).digest('hex').slice(0, 12); }
  catch { return null; }
}
function hashDerived() {
  const out = {};
  for (const f of derivedFiles()) out[f] = hashFile(f);
  return out;
}
// 变异"注入特征物"：取 find→repl 的**差集**（公共前后缀之外的那段），再加一条通用的 `// MUT`。
// 例：S1 的差集是 '99,  // 成长计划（变异：列数漂移）'、S4/S5/E3 是 'Unused'、E1 是 ' return; // MUT'。
// 用它扫产物：命中就是"这里被注入过"。（比"只看 git status"强得多，见上面那段。）
function mutationMarkers() {
  const out = [{ id: '(通用 // MUT)', s: '// MUT' }];
  for (const m of MUTATIONS) {
    const a = m.find || '', b = m.repl || '';
    let i = 0; while (i < a.length && i < b.length && a[i] === b[i]) i++;
    let j = 0; while (j < a.length - i && j < b.length - i &&
      a[a.length - 1 - j] === b[b.length - 1 - j]) j++;
    const delta = b.slice(i, b.length - j);
    if (delta.trim().length >= 4) out.push({ id: m.id, s: delta });
  }
  return out;
}
// 打印派生物状态；返回"带变异标记"的文件名数组。
function derivedReport(label) {
  const files = derivedFiles();
  const markers = mutationMarkers();
  const dirty = [];
  console.log('  ' + label + '：' + files.length + ' 个派生产物');
  for (const f of files) {
    let txt = '';
    try { txt = fs.readFileSync(path.join(HERE, f), 'utf8'); } catch { /* 读不到就当没标记 */ }
    const hit = markers.filter(m => txt.includes(m.s)).map(m => m.id);
    if (hit.length) dirty.push(f);
    console.log('    ' + (hit.length ? '✗' : '✓') + ' ' + f.padEnd(30) + ' ' + (hashFile(f) || '(读不到)') +
      (hit.length ? '   ← 含变异标记: ' + hit.join(', ') : ''));
  }
  return dirty;
}
let derivedBefore = null;
let restoreDone = false;
function finishAndRestore() {
  if (restoreDone) return;
  restoreDone = true;
  if (!derivedBefore) return;
  const before = derivedBefore;
  const dirty = Object.keys(before).filter(k => hashFile(k) !== before[k]);
  if (!dirty.length) {
    console.log('\n收尾：' + Object.keys(before).length + ' 个派生产物与扫描前**逐字一致**（无需复原）');
    return;
  }
  console.log('\n收尾：发现 ' + dirty.length + ' 个派生产物被改过 —— ' + dirty.join(', '));
  console.log('      按未变异源码重生成一次…');
  let r = null;
  try { r = runStaticSuites(REPO); } catch (e) { console.log('      重生成失败: ' + String(e.message).slice(0, 120)); }
  const still = dirty.filter(k => hashFile(k) !== before[k]);
  if (!still.length) {
    console.log('      ✓ 已复原（' + dirty.length + ' 个文件回到扫描前' +
      (r ? '；重生成后 ' + r.passed + '/' + r.total + ' 通过' : '') + '）');
    return;
  }
  // 最坏情况：别只说一句 warning —— 下一个人会在假状态上做结论
  const marked = derivedReport('复原后状态');
  console.log('');
  console.log('  ✗✗✗ 复原失败：这些派生产物**还带着变异内容**（' + still.join(', ') + '）');
  if (marked.length) console.log('      其中带变异标记的: ' + marked.join(', '));
  console.log('      后果：下一个人跑全套时看到的是"变异后"的产物 ⇒ 假绿/假红。');
  console.log('      手工复原（任一即可）：');
  console.log('        node tests/frontend/run_frontend_suites.js          # 全套跑一遍，产物全部按源码重生成');
  console.log('        node tests/frontend/<对应的 gen_*.js>               # 只重生成那一个');
  console.log('        node tests/frontend/mutation_sweep.js --restore-only # 本工具只做复原');
  console.log('');
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
function baselineStatic(muts) {
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
  // 声明了 goTest 的变异：基线也量一次 Go 判据。否则"基线就红的 Go 测试"会让每条 goTest
  // 变异都变成**假抓住**（它红的不是变异，是本来就红）。
  const withGo = (muts || []).filter(m => m.kind === 'static' && (m.goTest || []).length);
  let goFailed = [], goEnv = null;
  if (withGo.length) {
    const g = runGoTests(withGo[0], dir);
    goEnv = g.env;
    goFailed = g.failed;
    if (goEnv) {
      console.log('  ⚠ ENV: ' + goEnv);
      console.log('    ⇒ 声明了 goTest 的变异本轮记"**未计入**"（既不算被抓住、也不算存活）');
    } else {
      console.log('  Go 判据（基线）: ' + (goFailed.length
        ? '红 ' + goFailed.join(', ') + ' —— 先修它，否则 goTest 变异给不出结论'
        : '全绿 ' + withGo[0].goTest.join(', ')));
    }
  }
  console.log('  ⇒ 下面每条变异的"抓住它的"是**减掉这份基线**之后的新增失败\n');
  rmrf(dir);
  return { passed: Math.min(a.passed, b.passed), total: a.total, failed: red, goFailed, goEnv };
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
  // 只做复原：怀疑"工作树里有没有变异值"时当体检用（变异标记物扫描 + 按源码重生成）。
  // ⚠ 注意它能证明什么：只证明"没有我们的注入标记"；要证明"派生物 == 由当前源码重生成"，
  //   那是逐字节的检查（跑 run_frontend_suites.js，或看 verify_derived_freshness）。
  if (mode === '--restore-only') {
    console.log('=== 只做复原：派生产物体检 ===');
    const dirty = derivedReport('当前状态');
    if (!dirty.length) {
      console.log('  ⇒ 没有任何变异标记残留\n');
      process.exit(0);
    }
    console.log('  ⇒ 有文件带变异标记，按未变异源码重生成一次…');
    const r = runStaticSuites(REPO);
    const after = derivedReport('重生成后');
    console.log('  重生成 ' + r.passed + '/' + r.total + ' 通过；仍带标记 ' + after.length + ' 个' +
      (after.length ? '（手工：node tests/frontend/run_frontend_suites.js）' : '') + '\n');
    process.exit(after.length ? 1 : 0);
  }

  fs.mkdirSync(WORK, { recursive: true });
  // 派生产物簿记 + 中断兜底：**先记哈希，再选变异**，整轮包 try/finally，另外挂 SIGINT/SIGTERM。
  // 一轮 all 要 90 分钟，被 Ctrl-C / kill 掉是常态，收尾绝不能只写在"正常跑完"那条路径上；
  // 边界要尽量靠前 —— 只要有可能写过产物，就必须已经在 try 里面（这条是被一个"插在 try 之外
  // 的模拟中断"试出来的：那次 finally 没跑到，产物留在盘上）。
  derivedBefore = hashDerived();
  const onSignal = sig => {
    console.log('\n⚠ 收到 ' + sig + '：先把派生产物复原再退出' +
      '（否则仓库里留的是变异版产物，而 `git status` 还看不出来）');
    finishAndRestore();
    process.exit(sig === 'SIGINT' ? 130 : 143);
  };
  process.on('SIGINT', () => onSignal('SIGINT'));
  process.on('SIGTERM', () => onSignal('SIGTERM'));

  try {
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
      if (list.some(m => m.kind === 'static')) baseStatic = baselineStatic(list);
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

        // Go 判据（该变异声明了 goTest 才跑）。ENV 缺失 / 基线就红都是**硬条件**：不给结论。
        const hasGo = (mut.goTest || []).length > 0;
        const g = runGoTests(mut, dir);
        let goBlocked = null;
        if (hasGo) {
          if (g.env) goBlocked = 'ENV 缺失（go 不可用）—— Go 判据未计入';
          else if (baseStatic && baseStatic.goFailed && baseStatic.goFailed.length) {
            goBlocked = '基线就红的 Go 判据（' + baseStatic.goFailed.join(', ') + '）—— 先修它';
          } else {
            // 把 Go 判据并入失败清单：后面的"新增失败"口径自动把它算进去
            for (const f of g.failed) {
              if (!r1.failed.includes(f)) r1.failed.push(f);
              if (!r2.failed.includes(f)) r2.failed.push(f);
            }
            console.log('   Go 判据 ' + mut.goTest.join(', ') + ': ' +
              (g.failed.length ? 'FAIL → 并入失败清单 ' + g.failed.join(', ') : 'PASS（基线也 PASS）'));
          }
          if (g.env) console.log('   ⚠ ENV: ' + g.env);
        }

        const nf1 = newFailures(r1, baseStatic) || [];
        const nf2 = newFailures(r2, baseStatic) || [];
        const both = nf1.filter(x => nf2.includes(x));
        const flaky = r1.failed.filter(x => !r2.failed.includes(x))
          .concat(r2.failed.filter(x => !r1.failed.includes(x)));
        const rec = { id: mut.id, desc: mut.desc, ...r1, passedRuns: [r1.passed, r2.passed],
          newFailed: both, newFailedRuns: [nf1, nf2],
          go: hasGo ? (g.env ? 'ENV' : (goBlocked ? 'BLOCKED' : (g.failed.length ? 'FAIL' : 'PASS'))) : undefined };
        if (goBlocked) {
          // 判据没跑成 ⇒ 既不算被抓住、也不算存活。别让读者把 ENV 读成"存活"。
          rec.newFailed = [];
          rec.skipped = goBlocked;
          console.log('   ⚠ ' + goBlocked);
          console.log('      ⇒ 本变异**未计入**（既不算被抓住、也不算存活）—— 判据没跑成，别读成"存活"');
        } else if (both.length) {
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

  // 静态组跑完必须收尾（清掉真仓库里的变异内容）—— 现在统一由下面的 finally 里的
  // finishAndRestore() 负责：先比对哈希，不一致才重生成（见那段长注释）。
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
  console.log('   D. 判据不在本扫描器跑的集合里 → 接进套件集（S1 的 Go 守卫就是这么接的：' +
    '见 goTest / runGoTests）');
  console.log('结果: ' + path.join(WORK, 'results.json'));
  } finally {
    // 中断兜底：正常跑完 / 抛异常 / 被 kill（SIGINT/SIGTERM）都走这里
    finishAndRestore();
  }
})().then(() => process.exit(0))
  .catch(e => {
    // 兜底之上再兜一层：给一句人话，而不是把栈丢给读者（栈里看不出"产物可能被注入"）
    console.log('\n✗ 扫描器异常终止: ' + String(e && e.message || e).slice(0, 200));
    console.log('  派生产物状态见上面的"收尾"那几行；怀疑有残留就用 ' +
      '`node tests/frontend/mutation_sweep.js --restore-only` 体检一次。');
    process.exit(1);
  });
