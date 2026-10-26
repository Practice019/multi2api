// run_http_suites.js —— 把「HTTP 层验收探针」跑成一条命令。
//
// # 为什么需要它（这四条探针原先各跑各的）
//
// `tests/frontend/` 下有**两族**探针，形状不同、跑法也不同：
//
//	CDP 族（run_e2e.js 那批）  需要跑着的实例 + Chrome + 每条一个 *_URL 环境变量
//	                          ⇒ 验的是"界面点下去有没有反应"
//	HTTP 族（本脚本这四条）     自带 `--start`（自己起隔离实例）与 `--self-test`
//	                          ⇒ 验的是"服务端按不按契约应答"
//
// 第二族原先**没有运行器**：改完一处要记得手敲四条长命令，且顺序/环境变量
// 全靠脑子记。漏跑一条的后果不是"少验了一点"，而是"以为全绿了"。
// 本脚本就是那份清单 —— 加新探针时**只改这里一处**。
//
// # 每条跑两遍（这是刻意的）
//
//	--self-test  打本地桩：好桩必须全绿、坏桩必须变红
//	             ⇒ 证明**探针本身有判别力**（不做这步，"全绿"可能只是它什么
//	               都没判 —— 一个恒真的尺子量什么都是合格的）
//	--start      打真实例：必须全绿
//
// 缺了第一遍，"全绿"这件事本身不可信。所以本脚本把两遍都跑。
//
// # 用法
//
//	node tests/frontend/run_http_suites.js            # 全部（含 --self-test）
//	node tests/frontend/run_http_suites.js --quick    # 只跑 --self-test（秒级，不需要构建）
//	node tests/frontend/run_http_suites.js --only role_split
//	node tests/frontend/run_http_suites.js --keep     # 即使某条失败也继续跑完（默认就是继续）
//
// # ⚠ 前置：探针二进制必须是**本次构建**
//
// 本脚本会用 `WB2API_EXE`（缺省 `bin/wb2api-server-probe.exe`）。若它比
// `internal/` 或 `webui.html` 旧，验的就是上一轮的代码 —— 那种"全绿"毫无意义。
// 所以下面会比对源码 mtime 并**显式警告**（不判红：边改边跑是常态，
// 但警告必须显眼到不能忽略）。
'use strict';

const { spawnSync } = require('child_process');
const fs = require('fs');
const path = require('path');

const REPO = path.resolve(__dirname, '..', '..');
const PROBES = [
  // 每条的默认端口必须互不相同：它们会**同时**存在（本脚本串行跑，但
  // 上一轮的实例可能还没退干净）。
  { key: 'login_flow', file: 'verify_login_flow_e2e.js', desc: '登录流程正路（注册/登录/会话/登出）' },
  { key: 'abuse', file: 'verify_register_abuse_e2e.js', desc: '反批量（限速 429 / 请求体 413 / 人机校验）' },
  { key: 'console_auth', file: 'verify_console_cookie_auth_e2e.js', desc: '控制台 cookie 鉴权（/ui 三条路 + /status 四种身份）' },
  { key: 'role_split', file: 'verify_role_split_e2e.js', desc: 'API key 归属 + /admin/* 角色分流' },
];

// ── 探针二进制：**必须**在这里定死（2026-10-26 加）─────────────────────────
//
// 本脚本自己 spawn 子探针，而子探针走 lib_instance.js —— 那个模块在 WB2API_EXE
// 缺省时回落到 `bin/wb2api-server.exe`，也就是**用户正在跑的那份**（Windows 上
// 还被进程锁着）。于是「没显式设 WB2API_EXE 就跑本脚本」会去起用户那个构建：
// 探针断言失败看起来像产品缺陷，实际是验错了对象（本仓记过这个亏）。
//
// 所以与 run_frontend_suites.js / run_e2e_family3.js 保持同一口径：
// 未指定 ⇒ 默认 `bin/wb2api-server-probe.exe`（当前构建）。
// ⚠ 显式把 WB2API_EXE 指到 bin/wb2api-server.exe 仍然允许（对照实验要用），
//   下面会为此打一条显眼警告 —— 但**默认**绝不落到用户的二进制上。
if (!process.env.WB2API_EXE) {
  process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
  console.log('前置：未指定 WB2API_EXE，本脚本默认用 ' + process.env.WB2API_EXE +
    '（当前构建；不默认 bin/wb2api-server.exe —— 那是用户在跑的实例）\n');
} else if (/wb2api-server\.exe$/i.test(process.env.WB2API_EXE)) {
  console.log('⚠⚠ WB2API_EXE 指向 bin/wb2api-server.exe —— 那通常是**用户正在跑**的二进制。');
  console.log('   只在做「新旧构建对照」时才该这样；常规验收请用 bin/wb2api-server-probe.exe。\n');
}

// 直接类探针（不支持 --self-test/--start，自带实例 + Chrome）：跑法与上面四条不同，
// 不并入下面的 phases 循环（否则本脚本会变成依赖 Chrome）。但必须在这里**点名**，
// 否则「本脚本全绿」会被误读成「渠道页保存路径也验过了」。
const DIRECT_PROBES = [
  {
    file: 'verify_channels_save.js',
    cmd: 'node tests/frontend/verify_channels_save.js',
    note: '渠道页「新增 → 保存」真点一次的回归判据（v1.9.15 的 ReferenceError: key is not defined）。' +
      '自带隔离实例（CH_PORT，默认 7899）+ Chrome；已登记在 run_frontend_suites.js 的独立探针清单里（那边会跑它）。',
  },
  {
    file: 'verify_wave2_pages.js',
    cmd: 'node tests/frontend/verify_wave2_pages.js',
    note: '**Wave 2 三页面的固定门禁**（状态/渠道/模型：302/401、window_seconds=300、阈值 75/90、' +
      'page_size 夹到 100、翻页与搜索 0 次请求、双 tab、合成行不可勾选、无定价字样）。' +
      '自带隔离实例（**空闲端口现场探测**）+ Chrome。',
    why: '不放进 run_frontend_suites.js：那条 runner 的定位是「静态套件、无需浏览器」（见其文件头），' +
      '而本探针必须起实例 + Chrome ⇒ 按 direct 类登记在这里，并在 run_frontend_suites.js 尾部给出单跑命令。',
  },
];

const argv = process.argv.slice(2);
const quick = argv.includes('--quick');
const onlyArg = argv.find((a) => a.startsWith('--only'));
const only = onlyArg ? (onlyArg.split('=')[1] || argv[argv.indexOf(onlyArg) + 1] || '').trim() : '';

function main() {
  const list = only ? PROBES.filter((p) => p.key === only || p.file.includes(only)) : PROBES;
  if (!list.length) {
    console.error('没有匹配的探针。可用的 key：' + PROBES.map((p) => p.key).join(', '));
    return 2;
  }

  warnIfStaleBinary();

  const rows = [];
  let failed = 0;
  for (const p of list) {
    const file = path.join(__dirname, p.file);
    if (!fs.existsSync(file)) {
      rows.push({ probe: p.key, phase: 'missing', code: '—', note: '文件不存在' });
      failed++;
      continue;
    }
    const phases = quick ? [['--self-test', 'self-test']] : [['--self-test', 'self-test'], ['--start', 'live']];
    for (const [flag, label] of phases) {
      const t0 = Date.now();
      const r = spawnSync(process.execPath, [file, flag], {
        cwd: REPO,
        env: process.env,
        stdio: ['ignore', 'pipe', 'pipe'],
        encoding: 'utf8',
      });
      const ms = Date.now() - t0;
      const out = (r.stdout || '') + (r.stderr || '');
      const code = r.status === null ? 'killed' : String(r.status);
      const ok = r.status === 0;
      if (!ok) failed++;
      rows.push({ probe: p.key, phase: label, code, note: summarize(out), ms, ok });
      // 失败时把尾部的判据行打出来 —— 摘要不够定位（"结果: 1 条 FAIL"）。
      if (!ok) {
        console.log('\n--- ' + p.key + ' ' + label + ' 失败，末尾输出 ---');
        console.log(out.split('\n').slice(-25).join('\n'));
      }
    }
  }

  console.log('\n===== HTTP 探针汇总 =====');
  for (const r of rows) {
    const mark = r.ok === undefined ? '?' : (r.ok ? 'PASS' : 'FAIL');
    console.log(
      pad(r.probe, 14) + pad(r.phase, 10) + pad(mark, 6) + pad('exit=' + r.code, 10) +
      pad(r.ms ? (r.ms / 1000).toFixed(1) + 's' : '', 8) + r.note
    );
  }
  const total = rows.filter((r) => r.ok !== undefined).length;
  console.log('\n' + (failed === 0 ? '全绿' : failed + ' 条失败') + '（共 ' + total + ' 次运行）');
  if (quick) console.log('（--quick：只跑了 --self-test，未验真实例）');

  // 直接类探针：**不跑**，但要让人看见它们存在与怎么跑（见 DIRECT_PROBES 的注释）。
  if (!only) {
    console.log('\n--- 直接类探针（本脚本不跑，需单跑）---');
    for (const d of DIRECT_PROBES) {
      console.log('  ' + d.file);
      console.log('      跑法: ' + d.cmd);
      console.log('      说明: ' + d.note);
      if (d.why) console.log('      登记位置理由: ' + d.why);
    }
  }
  return failed === 0 ? 0 : 1;
}

// summarize 从探针输出里挑出最有用的一行（"结果: …" 或最后一条 FAIL）。
function summarize(out) {
  const lines = out.split('\n').map((l) => l.trim()).filter(Boolean);
  const fail = lines.filter((l) => /^FAIL\b|结果: .*FAIL/.test(l));
  if (fail.length) return fail[0].slice(0, 90);
  const res = lines.filter((l) => /^结果:/.test(l));
  if (res.length) return res[0].slice(0, 90);
  return lines.slice(-1)[0] ? lines.slice(-1)[0].slice(0, 90) : '(无输出)';
}

function pad(s, n) {
  s = String(s);
  // 中文字符按两个宽度算，表格才对得齐（本仓库的既有口味）。
  let w = 0;
  for (const ch of s) w += /[\u4e00-\u9fff\uff00-\uffef]/.test(ch) ? 2 : 1;
  return s + ' '.repeat(Math.max(1, n - w));
}

// warnIfStaleBinary 比对二进制与关键源码的 mtime。
//
// 为什么只警告不判红：边改代码边跑探针是常态（而且本脚本跑一次要几分钟）。
// 但"验了上一轮的构建"这种假绿最难发现 —— 探针自己也会打印警示行，
// 这里再来一道，两处都显眼。
function warnIfStaleBinary() {
  const exeRel = process.env.WB2API_EXE || 'bin/wb2api-server-probe.exe';
  const exe = path.resolve(REPO, exeRel);
  if (!fs.existsSync(exe)) {
    console.log('⚠ 探针二进制不存在：' + exeRel);
    console.log('  先构建：go build -o bin/wb2api-server-probe.exe ./cmd/server');
    return;
  }
  const exeMs = fs.statSync(exe).mtimeMs;
  const watch = [
    'internal/account', 'internal/admin', 'internal/server', 'internal/authlimit',
    'internal/apikey', 'cmd/server',
  ];
  let newest = 0;
  let newestFile = '';
  const walk = (p) => {
    const st = fs.statSync(p);
    if (st.isDirectory()) {
      for (const f of fs.readdirSync(p)) walk(path.join(p, f));
      return;
    }
    if (st.mtimeMs > newest) { newest = st.mtimeMs; newestFile = path.relative(REPO, p); }
  };
  for (const w of watch) {
    const abs = path.join(REPO, w);
    if (fs.existsSync(abs)) walk(abs);
  }
  if (newest > exeMs) {
    console.log('⚠⚠ 探针二进制比源码旧 —— 这次的"全绿"验的是**上一轮**的代码！');
    console.log('     二进制: ' + new Date(exeMs).toTimeString().slice(0, 8) + '  ' + exeRel);
    console.log('     更新的源码: ' + newestFile + '  ' + new Date(newest).toTimeString().slice(0, 8));
    console.log('     请先：go build -o bin/wb2api-server-probe.exe ./cmd/server');
  } else {
    console.log('探针二进制是最新的（' + exeRel + '，' + new Date(exeMs).toTimeString().slice(0, 8) + '）');
  }
}

process.exit(main());
