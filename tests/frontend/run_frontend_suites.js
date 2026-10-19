// run_frontend_suites.js —— 前端套件的**唯一**入口。
//
// # 为什么需要这个包装脚本
//
// 套件是 generator 驱动的：`gen_X.js` 写出 `X_gen.js`，再跑 `X_gen.js`。
// 这个结构有两个已经真实发生过的失效模式：
//
//  1. **generator 失败 → 陈旧的 _gen.js 假装绿**
//     gen_dashboard.js 因为在 String.raw 模板里写了反引号而解析失败，
//     但 __dirname 里上一次成功生成的 dashboard_gen.js 还在，它照样全绿。
//     我的校验脚本因此上报过"15/15 通过"，而实际上产品代码根本没被测到。
//
//  2. **generator 读错仓库 → 测的是别人的代码**
//     18 个 generator 里 17 个硬编码了 workbuddy2api（另一个仓库）。
//     它们长期全绿，但从未测到本仓库。
//
// 本脚本用两条硬门禁堵住这两类：
//   - generator 必须 exit 0，否则立即失败（不看产物）
//   - 产物必须**比 generator 新**，否则说明它没被重新生成
//
// 用法：
//   node run_frontend_suites.js
// 退出码 0 = 全绿。
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const DIR = __dirname;
const REPO = path.resolve(DIR, '..', '..');

// ---------------------------------------------------------------------------
// 本套件的**默认二进制**：探针构建（可用 WB2API_EXE 覆盖）
//
// # 为什么要在这里定默认值（2026-10-19 的事故）
//
// 本套件里有若干项会**启动 exe**。原先它们各自解析二进制，默认落到
// `bin/wb2api-server.exe` —— 而那个文件通常是**用户正在跑的那份**（见
// `lib_instance.js:32` 的原话），实测它就是一份 **11 小时前的旧构建**：
//
//   bin/wb2api-server.exe        2026/10/19 04:17:47   ← 旧（没有登录门那一代）
//   bin/wb2api-server-probe.exe  2026/10/19 15:01:59   ← 当前构建
//   internal/server/webui.html   2026/10/19 15:01:47   ← 产品（**编译期内嵌进 exe**）
//
// 后果实测：两条 e2e 探针在旧构建上**是绿的**，因为裸 `/ui` 在那个世代直接吐控制台；
// 换成当前构建就红了（登录门之后裸 `/ui` 给登录页）⇒ 那个"绿"是**在已不复存在的
// 产品世代上的绿灯**。⇒ 默认必须指向**当前构建**，否则一次 `node run_frontend_suites.js`
// 的结果不可解释。
//
// ⚠ 这不是"绕过新鲜度判据"：判据（`lib_instance.js` 的 `assertBuildFresh`，以及两条
//   e2e 探针里的同源实现）比的是"**实际要启动的那个 exe** 是否比 webui.html 新" ——
//   本默认值只是让"实际要启动的那个"是当前构建；**若连 probe 构建也旧，照样红**。
// ⚠ 必须在**任何 require 之后、任何子进程之前**设：`lib_instance.js:38-49` 记着这个坑
//   （模块求值时就把 EXE 定下了，require 之后再设 env 会静默回落）。
// ---------------------------------------------------------------------------
if (!process.env.WB2API_EXE) {
  process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
  console.log('前置：未指定 WB2API_EXE，本套件默认用 ' + process.env.WB2API_EXE + '（当前构建）；可用同名环境变量覆盖\n');
}
// 前置条件自证：一次跑必须能说清"我在什么前提上得出的结论"
(function printPreconditions() {
  const exe = path.resolve(REPO, process.env.WB2API_EXE);
  const html = path.join(REPO, 'internal', 'server', 'webui.html');
  // ⚠ 必须打**本地时间**（`sv-SE` 给出 `YYYY-MM-DD HH:MM:SS`）：用 toISOString 会打出 UTC，
  //   于是 15:01 的 exe 显示成 07:01、与文件管理器/探针报错里的时间对不上 ——
  //   一个"自证前提"的打印如果时间对不上，就等于没证。
  const mt = (p) => { try { return fs.statSync(p).mtime.toLocaleString('sv-SE'); } catch (e) { return '（不存在）'; } };
  console.log('前置：exe  = ' + exe);
  console.log('前置：      exe mtime=' + mt(exe) + '   webui.html mtime=' + mt(html));
  console.log('前置：.git  = ' + (fs.existsSync(path.join(REPO, '.git')) ? '有（两项审计按 git ls-files 扫）' : '**没有** ⇒ 依赖它的项会红'));
  console.log('前置：Chrome= ' + (() => { try { return require(path.join(DIR, 'chrome_path.js')).resolveChrome(); } catch (e) { return '**解析失败**：' + e.message; } })());
  console.log('');
})();

// 全部套件：[generator, 产物]。
// 顺序保持与历史一致（数字稳定便于对照旧输出）。
const SUITES = [
  ['gen_b5_normalize.js',     'b5_normalize_gen.js'],
  ['gen_busy_feedback.js',    'busy_feedback_gen.js'],
  ['gen_dashboard.js',        'dashboard_gen.js'],
  ['gen_expiry.js',           'expiry_gen.js'],
  ['gen_growth_labels.js',    'growth_labels_gen.js'],
  ['gen_growth_pending_ui.js','growth_pending_ui_gen.js'],
  ['gen_growth_select.js',    'growth_select_gen.js'],
  ['gen_growth_taskview.js',  'growth_taskview_gen.js'],
  ['gen_log_order.js',        'log_order_gen.js'],
  ['gen_models_display.js',   'models_display_gen.js'],
  ['gen_pagesize.js',         'pagesize_gen.js'],
  ['gen_pagesize_persist.js', 'pagesize_persist_gen.js'],
  ['gen_settings_layout.js',  'settings_layout_gen.js'],
  ['gen_stats_disk.js',       'stats_disk_gen.js'],
  ['gen_task8.js',            'task8_gen.js'],
  ['gen_tokps.js',            'tokps_gen.js'],
];

// 独立套件：不需要 generator 配对（自己直接读 webui.html 做断言）。
const PROBES = [
  'gen_tdz_guard.js',      // 真实 Chrome 里跑 TDZ 探针
  'gen_ui_manifest.js',    // 零硬编码 / 死代码 / colspan 漂移 守卫
  'test_chrome_resolver.js', // 浏览器解析器：覆盖生效 + 找不到时明确报错
  'verify_sign_against_spec.js', // 华为云签名实现逐条对照官方文档
  'test_shape_scan.js',        // 密钥形状扫描器：不漏报、不误报
  'audit_tracked_content.js',  // 被跟踪内容审计：密钥/绝对路径/内网/手机/邮箱
  'audit_dead_buttons.js',      // 静态死按钮扫描：id 是否有事件绑定
  'audit_crlf_sensitivity.js',   // CRLF 敏感写法：只报真危险，附安全理由
  'verify_t3_t4_static.js',      // T3+T4 的静态守卫：面板归属属性 / 能力位未被抹掉
  // T4+T5 的静态守卫：额度列的 has_data 判据 / Token 统一渲染器 / 后端指针化。
  // 自带变异验证（在临时副本上做，不动原仓库），所以它会**自己证明**
  // "删掉那一行会红"，而不只是声称如此。
  'verify_t4_t5_static.js',
  // 截图工具（**不是断言套件**：它只拍图，不做判据）。
  //
  // 为什么不放进本脚本：它需要跑着的实例 + Chrome，且产物是 PNG ——
  // 放进"全绿/全红"的列表里会让它看起来像一条守卫，而它证明不了行为。
  // 手动跑：node shot_accounts.js（产物在 .task/ui-convergence/shared/shots/）。
  // R3：账号池的委托是否随刷新累积监听器。
  //
  // ⚠ 它**需要**一个跑着的实例（默认 18080，用 ACC_URL 覆盖），
  // 与上面那些纯静态探针不同 —— 但它的判据（DOMDebugger 数监听器）
  // 没有静态替代品：`renderAccounts` 每次重建 tbody，
  // "绑了几次"只有运行期数得出来。
  // 实例不在时会明确失败而不是静默跳过 —— 静默跳过等于没有守卫。
  'verify_accts_delegation.js',
  // 「全部签到」分级布局的**真前端 e2e**（本轮新增）。
  //
  // ⚠ 它**自己起一个隔离实例**（临时目录 + 空闲端口 7871，可用 E2E_PORT
  // 覆盖），不依赖外部实例，也**不碰**仓库里已有的 config.json（7863）/
  // config-test-port7864.json（7864）—— 那两个可能是用户正在用的。
  //
  // 为什么值得占一个套件位：它抓的是**行为**（点一下发几个请求、
  // 发的是哪个 URL、刷新后按钮与委托是否累积），而 DOM 静态断言
  // 对这几条**完全没有判别力**。变异验证：去掉 installAllDailyActions
  // 的"只绑一次"守卫并每次重绘都调它 → 刷新 5 次后点一次发 7 个请求，
  // 而所有只看 DOM 的断言全绿。
  'verify_checkin_ui_e2e.js',
  // 逐个上游点「刷新本上游额度」+ 点「全部签到」的真前端 e2e（本轮新增）。
  //
  // ⚠ 它抓的是**运行时错误**，静态断言对此**完全没有判别力**：
  // `refreshAccounts is not defined` 那个缺陷，文本里确实有这个名字
  //（grep 得到、正则也匹配得上），只有**真点一次**才会抛 ReferenceError。
  //
  // 变异验证过两条（都已还原）：
  //   · 去掉 refreshAccounts 的定义 → 三个上游都报 "is not defined"
  //   · 把 quotaScopeToast 的对象直接传给 toast → 显示 "[object Object]"
  //
  // 同样自己起隔离实例（临时目录 + 空闲端口 7874，可用 E2E_PORT 覆盖）。
  'verify_all_buttons_e2e.js',
  // 派生物**内容级**新鲜度：派生物必须等于"由当前源码重生成"的产物（本轮新增）。
  //
  // # 为什么门禁 2 挡不住这类事故（这条判据是一次真事故换来的）
  //
  // 上面的门禁 2 只查 **mtime**（产物比生成器新）⇒ 它挡得住"改了生成器忘了重生成"，
  // 但**挡不住"产物内容被别人换过"**：2026-10-19 的 `262b796` 就是 —— 变异扫描套件
  // 用 `spawnSync(..., { cwd: 真仓库 })` + `WB2API_REPO=副本` 跑变异 ⇒ **html 读副本、
  // 产物写真仓库**（生成器里写的是 `fs.writeFileSync('x_gen.js', out)`，**相对 cwd**），
  // 于是注入值（`growth: 99` / `（变异：列数漂移）`）落进真仓库的派生物；而派生物与索引
  // "一致"，`git status` 显示干净 ⇒ 被当正常改动提交了。**mtime 没问题、内容是注入的。**
  //
  // # 它自己必须遵守的两条纪律（否则它会变成事故源）
  //   1. `cwd` 与 `WB2API_REPO` **一起**对齐到副本（只对齐一个就会"读 A 写 B"）；
  //   2. **只在副本里删产物**，真工作树的产物只读不写。
  //
  // # 覆盖两种形态
  //   · 产物过期（源码改了没重生成）· 产物被注入 · 产物缺失 ⇒ 都红
  //   ⇒ 与门禁 2 的 mtime 检查**互补，不是替代**（那条仍能更早拦住"忘了重生成"）。
  //
  // 判别力：`node verify_derived_freshness.js --self-test` ⇒ 好桩绿 / 注入红且**点名文件**
  // 并给出期望与实际哈希 + 重生成命令 / 缺失形态也红（7/7 通过，我已实测）。
  // 速度：副本复用 + robocopy 增量 ⇒ 约 4~7 秒（不是每轮整仓复制）。
  //
  // ⚠ 并发前提：本探针的副本目录是**固定名**（`D:\temp\wb2api-freshness-copy`，以便复用加速），
  //   所以它带一个**所有权戳**：若发现另一个活着的进程正持有它 ⇒ 报 ENV 并 exit 2
  //   （**不冒充** PASS/红）。这条是 2026-10-19 实测踩出来的：`mutation_sweep.js` 的静态腿
  //   会在**它自己的副本里**再跑一遍本套件 ⇒ 两个本探针实例抢同一个副本目录 ⇒ 删产物/跑生成器/
  //   哈希互相踩 ⇒ 出现"原生栈 + 生成器非零退出"的假红。并行跑的调用方请各自指定
  //   `WB2API_FRESH_COPY=<自己的目录>`。
  'verify_derived_freshness.js',
  // 类级静态守卫：**凡会打开 /ui 的探针，必须能自证"进的是控制台而不是登录页"**（本轮新增）。
  //
  // # 为什么需要它（这条判据是一次真事故换来的，与 `verify_derived_freshness.js` 同族）
  //
  // 登录门落地后（`internal/server/webui.go:180-212`，commit `34777b6`），**裸 `/ui` 给登录页**。
  // 而两个 e2e 探针（`verify_checkin_ui_e2e.js` / `verify_all_buttons_e2e.js`）原先导航裸 `/ui`
  // ⇒ `window.__wb2api__` 不存在 ⇒ 页面断言**级联全垮**（25 项），而它们的"绿"其实是
  // **在 04:17 旧构建（没有登录门那一代）上的绿** —— 典型的"在已不复存在的产品世代上通过"。
  // 这类缺陷的共同点：**负向断言（"没有死按钮""没有异常"）在登录页上会静默通过** ⇒ 假绿；
  // 正向断言会红，但归因迷惑（要花一次排查才发现"根本没进页面"）。
  //
  // 判据分两层（`verify_probe_console_loaded.js`）：
  //   (A) **带身份**：`consoleHtmlAuthed(` / `withToken(` / `withChrome(` / `?token=` /
  //       `WB2API_PROBE_TOKEN` / `Cookie` / 由 env 覆盖传入 URL —— 或
  //   (B) **有正向自证**：`applyManifest` / `__wb2api__` / `consolePageMarker` / 页面长度阈值。
  //   两者都没有 ⇒ **硬红**（exit 1）。"身份只来自 env 覆盖、默认值是裸 /ui" ⇒ **警告级**（不影响退出码）。
  //
  // ⚠ 它只在**硬红**时 exit 1；警告只是信息。接线时实测：扫 112 个文件、**硬红 0 / 警告 20
  //   （工具类另计 5）** ⇒ 接进来后入口仍全绿。
  //   那 20 条警告是**已知默认值风险清单**（跑法给对 BASE 时是对的，直接 `node xxx_e2e.js`
  //   才会落到登录页）⇒ 本轮**只记录、不改那批文件**（改动会牵动一批独立探针的调用约定，属另一轮）。
  //
  // ⚠ 与 `run_e2e.js` 的**动态**判据**互补，不许互替**：本判据证明"它**有能力**进控制台"，
  //   动态判据证明"它**真的**进了控制台"。两条都留。
  'verify_probe_console_loaded.js',
  // 内嵌 HTML 的内联 <script> **语法门禁**（本轮新增）。
  //
  // # 为什么是一条真空白（不是"多测一点"）
  //
  // `webui.html` / `webui-login.html` 是 `//go:embed` 的（`internal/server/webui.go:15,18`）
  // ⇒ **Go 编译不校验里面的 JS 语法**：一段语法错的 JS 会被安静编进二进制，
  //   `go build` / 单测 / `go vet` 全绿，直到浏览器打开页面 ——
  //   表现成"页面少了一半功能"，而报错（`renderXxx is not defined`）也要等真点一次。
  // `webui.html` 有 **11000+ 行**内联 JS，改一行注释里的括号就可能把整块打断。
  // 本仓已有同族事故：generator 的 `String.raw` 模板里一个反引号 ⇒ 模板提前终止
  // ⇒ 陈旧的产物照旧全绿（见上面"门禁 0"）。这次防的是同一件事的另一个容器。
  //
  // 判据：`new vm.Script` 逐段**解析**（不执行）⇒ 毫秒级、不需要实例/浏览器。
  // 覆盖清单**从 `//go:embed` 现取**，不写死文件名（写死的清单在新增页面时会静默漏掉）。
  // ⚠ 它只证明"能被解析"，**证明不了行为** —— 行为在 `run_e2e.js` 的 25 条里，两条互补。
  //
  // 判别力自带验证：`node verify_inline_script_syntax.js --self-test` ⇒
  // 好样本绿 / 注入 `function broken( {` 必红且**给出行号**（实测 webui.html:12937）/
  // 覆盖度非零（"全绿"不能是因为"什么都没查"）。
  'verify_inline_script_syntax.js',
];

function run(file) {
  try {
    const out = execFileSync(process.execPath, [path.join(DIR, file)], {
      cwd: DIR, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 180000,
    });
    return { ok: true, out };
  } catch (e) {
    return { ok: false, out: (e.stdout || '') + (e.stderr || ''), err: e.message };
  }
}

let pass = 0;
const failures = [];

console.log('=== 前端套件（generator 门禁 + 产物新鲜度门禁 + 模板反引号门禁）===\n');

// ---- 门禁 0：generator 的 String.raw 模板里不能有裸反引号 ----
//
// # 为什么单独加这一条
//
// 多数 generator 用 String.raw`...` 包住测试体。若在**注释/字符串**里写一个
// 反引号，模板会提前终止 —— generator 抛语法错，但**上一次成功生成的产物还在**，
// 直接跑产物就是全绿。这个坑已经踩过两次（gen_dashboard、gen_expiry），
// 两次都是"以为产品坏了，其实是测量工具坏了"。
//
// 静态检查一下就能在 generator 跑之前把原因说清楚，
// 比事后对着 "Cannot read properties of undefined" 猜要省事得多。
function findRawTemplateBacktick(file) {
  const src = fs.readFileSync(path.join(DIR, file), 'utf8');
  // 统计反引号总数：String.raw 模板要求成对，奇数说明有未闭合的
  const ticks = (src.match(/`/g) || []).length;
  return ticks % 2 === 1 ? ticks : 0;
}

for (const [gen, artifact] of SUITES) {
  const odd = findRawTemplateBacktick(gen);
  if (odd) {
    failures.push(`${gen} 的反引号数量为奇数（${odd}）—— String.raw 模板被提前终止`);
    console.log(`  TICK    ${gen} 反引号不成对，模板被提前终止（注释里写了反引号？）`);
    continue;
  }
  const aPath = path.join(DIR, artifact);
  const before = fs.existsSync(aPath) ? fs.statSync(aPath).mtimeMs : 0;

  // ---- 门禁 1：generator 必须成功 ----
  const g = run(gen);
  if (!g.ok) {
    failures.push(`${gen} 生成失败（产物 ${artifact} 是否陈旧未验证）`);
    console.log(`  GENFAIL ${gen}`);
    console.log('    ' + (g.out || g.err || '').split('\n').slice(0, 3).join('\n    '));
    continue;
  }

  // ---- 门禁 2：产物必须被重新写过 ----
  const after = fs.existsSync(aPath) ? fs.statSync(aPath).mtimeMs : 0;
  if (after <= before) {
    failures.push(`${gen} 没有重写 ${artifact}（产物可能是陈旧的）`);
    console.log(`  STALE   ${artifact} 未被 ${gen} 更新`);
    continue;
  }

  // ---- 跑产物 ----
  const r = run(artifact);
  if (r.ok) {
    pass++;
    console.log(`  ok      ${artifact}`);
  } else {
    failures.push(`${artifact} 断言失败`);
    console.log(`  FAIL    ${artifact}`);
    console.log('    ' + (r.out || '').split('\n').filter(l => /FAIL|Error/.test(l)).slice(0, 4).join('\n    '));
  }
}

console.log('\n=== 独立探针 ===');
for (const p of PROBES) {
  const r = run(p);
  if (r.ok) {
    pass++;
    console.log(`  ok      ${p}`);
  } else {
    failures.push(`${p} 失败`);
    console.log(`  FAIL    ${p}`);
    console.log('    ' + (r.out || '').split('\n').slice(-3).join('\n    '));
  }
}

const total = SUITES.length + PROBES.length;
console.log(`\n=== ${pass}/${total} 通过 ===`);
console.log(`（${SUITES.length} 个 generator 对 + ${PROBES.length} 个独立探针）`);
console.log('\n浏览器 E2E 不在本脚本内（它们需要真实网关 + Chrome）：');
console.log('  node panels_e2e.js   上游面板按能力位生成');
console.log('  node nav_e2e.js      导航分组 + 组头徽章（以 manifest 为对照）');
console.log('  node accounts_e2e.js 账号池分组 + 列数一致');
console.log('  node topstats_e2e.js 顶栏状态条 + 表格三态');
console.log('  node jobs_e2e.js     任务调度面板');
console.log('  node theme_e2e.js    深色/浅色三态主题 + 对比度实算');
console.log('  node verify_cp2_f1.js 组头 title 不自相矛盾');
console.log('  node verify_cp2_f2.js 监听器不泄漏 / 一次点击一个请求');
if (failures.length) {
  console.log('\n失败明细：');
  failures.forEach(f => console.log('  - ' + f));
}
process.exit(failures.length ? 1 : 0);
