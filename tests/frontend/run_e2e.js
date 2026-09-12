// run_e2e.js —— 浏览器 E2E 的唯一入口，串行执行并隔离 Chrome 端口。
//
// # 为什么需要这个包装脚本
//
// 各 E2E 脚本各自硬编码一个 CDP 调试端口（9222/9224…9240）。单独跑都没问题，
// 但**连续跑**时上一个 Chrome 还没释放端口，下一个就启动了 ——
// 表现为"这次 A 失败、下次 B 失败"，看起来像产品有 bug，其实是测试抢端口。
//
// 本脚本：
//   1. 每次运行前后清理残留 Chrome（按 user-data-dir 精确清理，不误杀用户浏览器）
//   2. 轮询确认调试端口真的空闲了再启动下一个
//   3. 用唯一端口传入（各脚本支持 XXX_TEST_URL，端口由这里统一分配）
//   4. 失败时打印该脚本的完整错误尾部，便于区分"真失败"与"启动失败"
//
// 用法：node D:\tmp\run_e2e.js [base-url]
// 默认 base-url = http://127.0.0.1:18080/ui
const fs = require('fs');
const path = require('path');
const net = require('net');
const { execFileSync, execSync } = require('child_process');

const DIR = __dirname;
const BASE = process.argv[2] || 'http://127.0.0.1:18080/ui';

// 每个脚本 + 它的 URL 环境变量名 + 专属 Chrome profile
const TESTS = [
  ['panels_e2e.js',     'PANEL_TEST_URL'],
  ['nav_e2e.js',        'NAV_TEST_URL'],
  ['accounts_e2e.js',   'ACC_TEST_URL'],
  ['topstats_e2e.js',   'T7_TEST_URL'],
  ['jobs_e2e.js',       'T9_TEST_URL'],
  ['theme_e2e.js',      'T10_TEST_URL'],
  ['flash_test.js',     'FLASH_URL'],     // 抗闪白：按加载阶段采样，验结果而非验脚本位置
  ['p4_badge_check.js', 'P4_URL'],        // 侧栏徽章底色跟随主题（P4 发现的残留写死颜色）
  ['cp3_f1_null_elements.js', 'F1N_URL'], // 畸形 manifest 元素不得让 refresh 抛异常（CP3 F1）
  ['contrast_e2e.js',   'CONTRAST_URL'],  // 两套主题下组件对比度达 WCAG AA
  ['panel_content_e2e.js', 'PANEL_URL'],
  ['user_session_e2e.js',  'USER_URL'],   // 真的按按钮：刷新/清屏/翻页/放弃修改/切主题
  ['verify_clearscreen.js','CLEAR_URL'],  // 清屏行为契约：含轮询与切面板
  ['verify_dynamic_actions.js','DYN_URL'], // 动态行按钮/下拉/视图切换真的有效
  ['verify_info_modal.js','INFO_URL'],   // 只读信息弹窗（替代 alert）
  ['verify_t3_t4.js','T34_URL'],       // T3+T4：面板归位与删除的用户可见结果
  ['verify_account_buttons.js','ACC_URL'], // 账号池按钮清单（防增防删）
  ['verify_models_grouped.js','MODELS_URL'], // T5+T6：模型去重/分组/可折叠/倍率
  ['verify_cp2_f1.js',  'F1_TEST_URL'],
  ['verify_cp2_f2.js',  'F2_TEST_URL'],
  ['final_visual.js',   'VISUAL_URL'],
];

// 各脚本内部使用的调试端口（与脚本里的常量对应）。
// 这里只用于"等它释放"，不修改脚本本身。
const DEBUG_PORTS = [9222, 9224, 9225, 9226, 9227, 9228, 9229, 9230, 9231, 9232, 9240, 9241, 9251, 9252, 9258, 9260, 9290, 9293, 9295, 9296, 9302, 9304, 9306];

const sleep = ms => new Promise(r => setTimeout(r, ms));

// portFree 探测端口是否真的空闲（比"端口存在"更准：能连上说明还占着）
function portFree(port) {
  return new Promise(resolve => {
    const s = net.connect({ host: '127.0.0.1', port }, () => { s.destroy(); resolve(false); });
    s.on('error', () => resolve(true));
    s.setTimeout(500, () => { s.destroy(); resolve(true); });
  });
}

async function waitAllPortsFree(timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    let allFree = true;
    for (const p of DEBUG_PORTS) {
      if (!(await portFree(p))) { allFree = false; break; }
    }
    if (allFree) return true;
    await sleep(300);
  }
  return false;
}

// killStaleChrome 只杀**本套件启动的** Chrome（按 user-data-dir 前缀识别），
// 绝不误杀用户自己开的浏览器。
function killStaleChrome() {
  try {
    // PowerShell 精确匹配命令行里带 chrome-* / *profile 的 headless 实例
    execSync(
      "Get-CimInstance Win32_Process -Filter \"Name='chrome.exe'\" | " +
      "Where-Object { $_.CommandLine -match '--user-data-dir=D:\\\\tmp\\\\chrome-' } | " +
      "ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }",
      { stdio: 'ignore', shell: 'pwsh' }
    );
  } catch { /* 没有残留时命令会"无事可做"，忽略 */ }
}

function run(file, env) {
  try {
    const out = execFileSync(process.execPath, [path.join(DIR, file)], {
      cwd: DIR, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
      timeout: 240000, env: Object.assign({}, process.env, env),
    });
    return { ok: true, out };
  } catch (e) {
    return { ok: false, out: (e.stdout || '') + (e.stderr || '') };
  }
}

(async () => {
  console.log('=== 浏览器 E2E（串行 + 端口隔离）===');
  console.log('目标: ' + BASE + '\n');

  // 起始先清一次
  killStaleChrome();
  await sleep(800);

  let pass = 0;
  const failures = [];

  for (const [file, envVar] of TESTS) {
    // 跑之前确认端口都空了
    if (!(await waitAllPortsFree(15000))) {
      killStaleChrome();
      await waitAllPortsFree(10000);
    }

    const env = {};
    env[envVar] = BASE;
    const r = run(file, env);

    if (r.ok) {
      pass++;
      const last = (r.out || '').trim().split('\n').pop() || '';
      console.log(`  ok    ${file.padEnd(20)} ${last.trim()}`);
    } else {
      const tail = (r.out || '').trim().split('\n').slice(-6).join('\n         ');
      // 区分"启动失败"与"断言失败"：前者是基础设施问题，后者才是产品问题
      const isStartup = /EXCEPTION: Chrome 未启动|Cannot find module|EADDRINUSE/.test(r.out || '');
      failures.push({ file, isStartup, tail });
      console.log(`  ${isStartup ? 'ENV   ' : 'FAIL  '}${file}`);
    }

    // 跑完立刻收尾，给下一个腾地方
    killStaleChrome();
    await sleep(600);
  }

  console.log(`\n=== ${pass}/${TESTS.length} 通过 ===`);
  if (failures.length) {
    console.log('\n失败明细：');
    for (const f of failures) {
      console.log(`\n  ${f.file}（${f.isStartup ? '环境/启动问题' : '断言失败'}）`);
      console.log('         ' + f.tail);
    }
  }
  process.exit(failures.length ? 1 : 0);
})();
