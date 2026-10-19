// verify_derived_freshness.js —— 内容级门禁：**派生物必须等于"由当前源码重生成的产物"**。
//
// # 为什么需要它（这条判据是一次真事故换来的）
//
// `run_frontend_suites.js` 的门禁 2 只查 **mtime**（产物比生成器新）。它挡得住
// "生成器改了、产物没重生成"，但**挡不住"产物内容被别人改过"**：
//
//   2026-10-19 事故（`262b796`）：变异扫描套件用 `spawnSync(node, run_frontend_suites.js,
//   { cwd: 真仓库 })` + `WB2API_REPO=副本` 跑变异 ⇒ **html 读副本、产物写真仓库**
//   （生成器里写的是 `fs.writeFileSync('x_gen.js', out)` —— **相对 cwd**）。
//   于是注入值（`growth: 99` / `（变异：列数漂移）`）落进了真仓库的派生物；而派生物
//   与索引"一致"，`git status` 显示干净 ⇒ 被当成正常改动提交了。
//   ⇒ **mtime 没问题、内容是被注入的**。mtime 检查对此完全无感。
//
// # 两条纪律（本文件自己必须遵守，否则它会变成事故源）
//
//   1. **`cwd` 与 `WB2API_REPO` 必须一起对齐到副本**：只对齐一个就会出现"读 A 写 B"。
//      本脚本用 `execFileSync(..., { cwd: 副本/tests/frontend, env: { WB2API_REPO: 副本 } })`。
//   2. **只在副本里删产物**，真工作树的产物一个都不动（它只是被"读"来比对）。
//
// # 它同时覆盖两种形态
//
//   · 产物**过期**（源码改了没重生成）⇒ 重生成结果 ≠ 工作树 ⇒ 红
//   · 产物**被注入**（内容被换过）    ⇒ 同上 ⇒ 红
//   · 产物**缺失**                    ⇒ 红（并在报错里点名）
//   ⇒ 与门禁 2 的 mtime 检查**互补，不是替代**（那条仍能提前拦住"忘了重生成"）。
//
// 用法：
//   node verify_derived_freshness.js              # 正常校验（默认基准=真工作树）
//   node verify_derived_freshness.js --self-test  # 判别力自检：好桩必须绿、坏桩必须红且点名文件
// 环境变量：
//   WB2API_FRESH_REF=<dir>   比对基准目录（默认=真仓库；自检时用来指向"被注入的基准"）
//   WB2API_REPO=<dir>        真仓库位置（默认=本文件的上两级）
//   WB2API_FRESH_COPY=<dir>  复用的一次性副本目录（默认 D:\temp\wb2api-freshness-copy）
//
// 性能：副本**复用**（robocopy 增量，只有变化的文件会被复制）⇒ 常态化运行是十几秒级。
const fs = require('fs');
const path = require('path');
const os = require('os');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

const REPO = (process.env.WB2API_REPO || path.resolve(__dirname, '..', '..')).replace(/[\\/]+$/, '');
const REF = (process.env.WB2API_FRESH_REF || REPO).replace(/[\\/]+$/, '');
// ⚠ 副本目录选在 `D:\temp`（不是系统 tmp）：本仓库的约定是临时物件都放这里，
//   而且这个副本是**复用**的（不是每轮新建）—— 只有 1 个目录，不会像早先那些
//   `chrome-*` 那样堆出上百个残留（那条教训见 `lib_cdp.js` 那个跨文件发现）。
//
// ⚠⚠ 但"复用 = 固定名"带来一个**并发缺陷**（2026-10-19 实测踩到，我自己修的）：
//   本探针会被**嵌套**跑 —— `mutation_sweep.js` 的静态腿在副本里跑整套套件，
//   那一轮里本探针也会被跑一次，而它的**比对基准**变成了外层副本、**副本目录**却仍是这个
//   固定名 ⇒ 两个实例同时删产物、同时跑生成器、同时哈希 ⇒ 结果随机（我实测到一次
//   "原生栈 + 生成器非零退出"的红，而真因是**两个我在抢同一个目录**，不是产品/源码问题）。
//   ⇒ 修法不是"别嵌套跑"，而是**加所有权戳**：目录里放 `owner.json`（pid + 启动时间）；
//     若发现**另一个活着的进程**正持有它 ⇒ 报 ENV 并 exit 2（**不冒充**绿也不冒充红），
//     由人决定是等它跑完还是用 `WB2API_FRESH_COPY=<另开目录>` 错开。
//     想要并行/隔离跑的调用方，直接给 `WB2API_FRESH_COPY` 指一个自己的目录即可。
const DEFAULT_COPY = fs.existsSync('D:\\temp') ? 'D:\\temp\\wb2api-freshness-copy' : path.join(os.tmpdir(), 'wb2api-freshness-copy');
const COPY = (process.env.WB2API_FRESH_COPY || DEFAULT_COPY).replace(/[\\/]+$/, '');
const OWNER_FILE = path.join(COPY, 'owner.json');

// 取所有权：活着的其它进程正持有 ⇒ 返回它（调用方报 ENV、exit 2）。
function claimCopy() {
  try {
    if (fs.existsSync(OWNER_FILE)) {
      const o = JSON.parse(fs.readFileSync(OWNER_FILE, 'utf8'));
      if (o && o.pid && o.pid !== process.pid) {
        let alive = false;
        try { process.kill(o.pid, 0); alive = true; } catch (e) { alive = e.code === 'EPERM'; }
        if (alive) return { busy: true, owner: o };
      }
    }
  } catch (e) { /* 戳文件损坏/不可读 ⇒ 当作可接管 */ }
  try {
    fs.mkdirSync(COPY, { recursive: true });
    fs.writeFileSync(OWNER_FILE, JSON.stringify({ pid: process.pid, since: new Date().toISOString(), argv: process.argv.slice(1) }));
  } catch (e) { /* 写不了戳不影响校验（老环境/只读盘），下一段 robocopy 会自己报错 */ }
  return { busy: false };
}
function releaseCopy() {
  try { if (fs.existsSync(OWNER_FILE)) fs.rmSync(OWNER_FILE, { force: true }); } catch (e) { /* 收尾失败不改变判定 */ }
}
const SELF_TEST = process.argv.includes('--self-test');

// 生成器 → 产物 的配对表（与 run_frontend_suites.js 的配对表一致；加新对时**两边都要改**）
const PAIRS = [
  ['gen_b5_normalize.js', 'b5_normalize_gen.js'],
  ['gen_busy_feedback.js', 'busy_feedback_gen.js'],
  ['gen_dashboard.js', 'dashboard_gen.js'],
  ['gen_expiry.js', 'expiry_gen.js'],
  ['gen_growth_labels.js', 'growth_labels_gen.js'],
  ['gen_growth_pending_ui.js', 'growth_pending_ui_gen.js'],
  ['gen_growth_select.js', 'growth_select_gen.js'],
  ['gen_growth_taskview.js', 'growth_taskview_gen.js'],
  ['gen_log_order.js', 'log_order_gen.js'],
  ['gen_models_display.js', 'models_display_gen.js'],
  ['gen_pagesize.js', 'pagesize_gen.js'],
  ['gen_pagesize_persist.js', 'pagesize_persist_gen.js'],
  ['gen_settings_layout.js', 'settings_layout_gen.js'],
  ['gen_stats_disk.js', 'stats_disk_gen.js'],
  ['gen_task8.js', 'task8_gen.js'],
  ['gen_tokps.js', 'tokps_gen.js'],
  ['gen_tdz_guard.js', 'tdz_probe.html'],   // 非 js 产物，但同属"派生物"
];
// 副本时排除的大目录：只留"生成器需要读的"与"要重生成的"
const SKIP_DIRS = ['.git', 'node_modules', 'docs', '.task', 'bin', 'auths', 'logs', '.dsh', 'workflow-v2'];

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
const sha = (p) => crypto.createHash('sha256').update(fs.readFileSync(p)).digest('hex');

function tryRobocopy(src, dst) {
  try { execFileSync('robocopy', [src, dst, '/E', '/NFL', '/NDL', '/NJH', '/NJS', '/NP', '/XD'].concat(SKIP_DIRS), { stdio: 'ignore' }); return { ok: true }; }
  catch (e) {
    // ⚠ robocopy 的退出码不是普通命令的语义：0=没有需要复制的、1=已复制、2=有额外文件… **≥8 才是失败**。
    //   第一版我直接 execFileSync 就被 exit=1 抛异常骗过一次。
    //
    // ⚠⚠ 第二版修的是另一件事：**本机没有 robocopy** 时（macOS/Linux/精简 Windows），
    //   execFileSync 抛的是 `ENOENT`，`e.status === undefined` ⇒ 旧代码算出 `code=-1`，
    //   而 `-1 >= 8` 为假 ⇒ **异常被吞** ⇒ 副本空着 ⇒ 后面 17 个生成器全非零退出 ⇒
    //   判据会报"生成器非零退出"—— **结论是红，但归因错**（读的人去查生成器，真因是缺工具）。
    //   这违反本仓库自己给 `S1` 定下的那条纪律（找不到 go 就大声报 ENV、不计数、不冒充 PASS）
    //   ⇒ 同一条纪律必须适用到这里：先认 ENOENT，再走 Node 复制兜底，最后才报 ENV。
    if (e.code === 'ENOENT') return { ok: false, enoent: true };
    const code = e.status === undefined ? -1 : e.status;
    if (code >= 8) return { ok: false, why: 'robocopy exit=' + code };
    return { ok: true };   // 0..7 都是正常
  }
}

// 准备副本。返回 {ok, how} 或 {ok:false, why}（调用方负责报 ENV 并退出）。
function prepareCopy(src, dst) {
  const rc = tryRobocopy(src, dst);
  if (rc.ok) return { ok: true, how: 'robocopy（复用 + 增量，只有变化的文件会复制）' };
  if (!rc.enoent) return { ok: false, why: rc.why };
  // 无 robocopy ⇒ Node 复制兜底（慢一点没关系，它是本地门禁）
  try {
    fs.mkdirSync(dst, { recursive: true });
    fs.cpSync(src, dst, {
      recursive: true, force: true,
      filter: (s) => {
        const rel = path.relative(src, s);
        if (!rel) return true;
        // 任一路径段命中排除名单就跳过（与 robocopy /XD 的按名匹配一致）
        return !rel.split(/[\\/]/).some((seg) => SKIP_DIRS.includes(seg));
      },
    });
    return { ok: true, how: 'fs.cpSync（本机无 robocopy 的兜底路径）' };
  } catch (e) {
    return { ok: false, why: 'robocopy 不存在，且 fs.cpSync 也失败：' + e.message };
  }
}

// ---------------------------------------------------------------- 自检（判别力）
if (SELF_TEST) {
  console.log('=== 判别力自检：好桩必须绿、坏桩必须红并点名文件 ===\n');
  // 好桩：基准 = 真工作树 ⇒ 必须 0 不一致
  const goodRef = path.join(os.tmpdir(), 'fresh-selftest-good');
  fs.rmSync(goodRef, { recursive: true, force: true });
  fs.mkdirSync(path.join(goodRef, 'tests', 'frontend'), { recursive: true });
  for (const [, a] of PAIRS) {
    const src = path.join(REF, 'tests', 'frontend', a);
    if (fs.existsSync(src)) fs.copyFileSync(src, path.join(goodRef, 'tests', 'frontend', a));
  }
  let out1 = '';
  try {
    out1 = execFileSync(process.execPath, [__filename], {
      encoding: 'utf8', env: Object.assign({}, process.env, { WB2API_FRESH_REF: goodRef }),
    });
    ok(true, '好桩（基准 = 工作树）→ 全绿 ✓');
  } catch (e) { ok(false, '好桩居然红了：' + ((e.stdout || '') + (e.stderr || '')).slice(-200)); }

  // 坏桩：把基准里一个派生物换成注入值 ⇒ 必须红且**点名那个文件**
  const badRef = path.join(os.tmpdir(), 'fresh-selftest-bad');
  fs.rmSync(badRef, { recursive: true, force: true });
  fs.cpSync(goodRef, badRef, { recursive: true });
  const victim = 'growth_labels_gen.js';
  const vp = path.join(badRef, 'tests', 'frontend', victim);
  const before = fs.readFileSync(vp, 'utf8');
  const after = before.replace(/growth: 6,\s*\/\/ 成长计划（精简：[^）]*）/, 'growth: 99,  // 成长计划（变异：列数漂移）');
  ok(after !== before, '坏桩真的改到了文件内容（' + victim + ' 里 growth 的列数被改成 99）');
  fs.writeFileSync(vp, after);
  let redOut = '', redExit = 0;
  try {
    redOut = execFileSync(process.execPath, [__filename], {
      encoding: 'utf8', env: Object.assign({}, process.env, { WB2API_FRESH_REF: badRef }),
    });
  } catch (e) { redExit = e.status === undefined ? -1 : e.status; redOut = (e.stdout || '') + (e.stderr || ''); }
  ok(redExit !== 0, '坏桩 → 判据变红（exit=' + redExit + '）');
  ok(redOut.includes(victim), '坏桩 → 报错**点名了**那个文件：' + victim);
  ok(/期望|实际/.test(redOut), '坏桩 → 报错里给出了期望/实际哈希');
  ok(/重生成|node gen_/.test(redOut), '坏桩 → 报错里给出了重生成命令');

  // 坏桩之二：产物"缺失"（另一种形态）也要红
  fs.rmSync(path.join(badRef, 'tests', 'frontend', 'tokps_gen.js'));
  let missOut = '', missExit = 0;
  try {
    missOut = execFileSync(process.execPath, [__filename], {
      encoding: 'utf8', env: Object.assign({}, process.env, { WB2API_FRESH_REF: badRef }),
    });
  } catch (e) { missExit = e.status === undefined ? -1 : e.status; missOut = (e.stdout || '') + (e.stderr || ''); }
  ok(missExit !== 0 && missOut.includes('tokps_gen.js'), '坏桩（缺失形态）→ 红且点名 tokps_gen.js');

  fs.rmSync(goodRef, { recursive: true, force: true });
  fs.rmSync(badRef, { recursive: true, force: true });
  console.log('\n' + (fail === 0 ? '=== 自检通过：这条判据有判别力（好桩绿 / 注入红 / 缺失红）===' : '=== ' + fail + ' 项自检失败 ==='));
  process.exit(fail ? 1 : 0);
}

// ---------------------------------------------------------------- 正常校验
console.log('=== 派生物内容级新鲜度（派生物 == 由当前源码重生成）===');
console.log('  真仓库: ' + REPO);
console.log('  比对基准: ' + REF);
console.log('  副本: ' + COPY + '（复用；robocopy 增量）');

// 1) 准备副本：**只在副本里**删产物，逼它们只能由生成器重新产出
const claim = claimCopy();
if (claim.busy) {
  console.log('  ENV: 副本目录正被另一个进程使用（pid=' + claim.owner.pid + '，自 ' + claim.owner.since + '）');
  console.log('       ' + COPY);
  console.log('       ⇒ 本判据未运行、不计数、不冒充 PASS/红。');
  console.log('       两个实例抢同一个副本会让结果随机（删产物/跑生成器/哈希互相踩）。');
  console.log('       要么等它跑完，要么换目录： WB2API_FRESH_COPY=D:\\temp\\fresh-<你的名字>');
  process.exit(2);
}
process.on('exit', releaseCopy);
const prep = prepareCopy(REPO, COPY);
if (!prep.ok) {
  // 与 S1 的 ENV 纪律一致：**缺工具 ⇒ 大声报 ENV、不计数、不冒充 PASS**，
  // 绝不能报成"派生物不一致/生成器失败"（那是归因错误，会让人去查错的地方）。
  console.log('  ENV: 无法准备副本（' + prep.why + '）⇒ 本判据未运行，不计数、不冒充 PASS');
  console.log('       期望：Windows 自带 robocopy；无则用 fs.cpSync 兜底（本机两种都失败了）。');
  console.log('       可用 WB2API_FRESH_COPY=<dir> 指定一个可写目录。');
  process.exit(2);
}
console.log('  副本准备: ' + prep.how);
const copyFront = path.join(COPY, 'tests', 'frontend');
// ⚠ 副本是**复用**的（`/E` 不会删掉源里已删的文件）⇒ 副本里可能残留"仓库已删除的文件"。
//   本判据只比那 17 个派生物，且**每轮先把它们删干净再重生成** ⇒ 不受影响。
//   但将来若有人把它改成"整目录比对"，必须先清空副本，否则会看到幻影差异。
for (const [, artifact] of PAIRS) {
  const p = path.join(copyFront, artifact);
  if (fs.existsSync(p)) fs.rmSync(p, { force: true });
}
// 2) 副本里跑生成器。⚠ cwd 与 WB2API_REPO 一起指向副本 —— 这就是那条规则
const env = Object.assign({}, process.env, { WB2API_REPO: COPY });
const bad = [];
for (const [gen, artifact] of PAIRS) {
  let genExit = 0, genErr = '';
  try {
    execFileSync(process.execPath, [path.join(copyFront, gen)], { cwd: copyFront, env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (e) {
    genExit = e.status === undefined ? -1 : e.status;
    genErr = ((e.stdout || '') + (e.stderr || '')).split('\n').filter(Boolean).slice(-1)[0] || '';
  }
  const madeP = path.join(copyFront, artifact);
  const refP = path.join(REF, 'tests', 'frontend', artifact);
  if (genExit !== 0) { bad.push({ artifact, kind: '生成器非零退出', detail: 'exit=' + genExit + ' ' + genErr }); continue; }
  if (!fs.existsSync(madeP)) { bad.push({ artifact, kind: '重生成后产物不存在', detail: '生成器 exit 0 但没产出' }); continue; }
  if (!fs.existsSync(refP)) { bad.push({ artifact, kind: '工作树里没有这个产物', detail: '该产物应被提交' }); continue; }
  const a = sha(madeP), b = sha(refP);
  if (a !== b) bad.push({ artifact, kind: '内容不一致', detail: '期望(重生成)=' + a.slice(0, 16) + ' 实际(工作树)=' + b.slice(0, 16), gen });
}

console.log('\n  逐项：' + (PAIRS.length - bad.length) + '/' + PAIRS.length + ' 与源码一致');
ok(bad.length === 0, PAIRS.length + ' 个派生物全部等于"由当前源码重生成"的产物'
  + (bad.length ? ' —— 有 ' + bad.length + ' 个不一致：' + bad.map((x) => x.artifact).join(', ') : ''));
for (const x of bad) {
  const gen = x.gen || ('gen_' + x.artifact.replace(/_gen\.js$/, '.js'));
  console.log('  ⚠ ' + x.artifact + ' —— ' + x.kind + '：' + x.detail);
  console.log('      重生成命令： cd ' + path.join(REPO, 'tests', 'frontend') + ' && node ' + gen);
}
if (bad.length) {
  console.log('\n  提示：派生物是生成器产物，**不要手改**。修法是改生成器的源码片段后重跑生成器。');
  console.log('  成因排查：若内容是"变异值/注入值"，说明有人把生成器跑在了 `cwd=真仓库` 而 `WB2API_REPO` 指别处');
  console.log('           （生成器写 `fs.writeFileSync` 用的是**相对 cwd** 的路径 ⇒ 读 A 写 B）。');
}
console.log('');
console.log(bad.length === 0 ? '=== 派生物新鲜（内容级）===' : '=== ' + bad.length + ' 个派生物与源码不一致 ===');
process.exit(bad.length ? 1 : 0);
