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
