// run_e2e_family3.js —— **第三族（浏览器 E2E，25 条）的可复现入口**。
//
// # 为什么需要它（这是第三族"数字不可复现"的根治）
//
// 之前跑第三族要人工凑四件事：实例配置、端口、前置数据、带令牌的 BASE。
// 谁凑错一样，"通过数"就变 —— 同一份代码同一天能跑出 18/25 和 20/25。
//
// 这个入口把这四件事**全部**交给 `lib_family3.js`（那才是唯一事实来源），
// 并额外产出**可机器比对的红集合**：
//
//	RUNS=1  PASS=20  TOTAL=25  REDS_COUNT=5
//	REDS=accounts_e2e,user_session_e2e,verify_dynamic_actions,verify_t3_t4,verify_account_buttons
//
// ⇒ "可复现"的验收就变成一条命令：**连跑两次，`REDS=` 那一行逐字相同**
//   （红可以留着，但不许飘）。脚本里再包一层就能自动判：
//
//	node tests/frontend/run_e2e_family3.js --repeat=2   # 同一端口连跑两次并自动比对
//
// 用法:
//	node tests/frontend/run_e2e_family3.js               # 自选空闲端口
//	node tests/frontend/run_e2e_family3.js --port=18100  # 指定端口
//	node tests/frontend/run_e2e_family3.js --repeat=2    # 连跑两次并比对 REDS（可复现性验收）
//	node tests/frontend/run_e2e_family3.js --keep        # 跑完不删临时目录（排障用）
//
// ⚠ `WB2API_EXE` 必须在 require(lib_instance) **之前**设
//   （lib_instance 在模块求值那一刻就把 EXE 定下来了；设晚了会静默回落
//   `bin/wb2api-server.exe` —— 那通常是用户正在跑的旧构建）。
'use strict';
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';

const path = require('path');
const net = require('net');
const { spawn } = require('child_process');

const fam = require('./lib_family3.js');

// 自选空闲端口：从 18100 往上找一个能 bind 的。
// ⚠ 显式避开用户实例 7863、DSH 3080、new-api 3000，以及主脑在用的 18095/18080。
const RESERVED = new Set([7863, 3080, 3000, 18095, 18080]);
function freePort(from) {
  return new Promise((resolve, reject) => {
    let p = from || 18100;
    const tryOne = () => {
      if (p > 18299) return reject(new Error('18100..18299 全被占用'));
      if (RESERVED.has(p)) { p++; return tryOne(); }
      const s = net.createServer();
      s.once('error', () => { p++; tryOne(); });
      s.once('listening', () => s.close(() => resolve(p)));
      s.listen(p, '127.0.0.1');
    };
    tryOne();
  });
}

// 从 run_e2e 的输出里抽"红集合"（机器可比的规范形式）。
//
// 认两种形态（run_e2e 的汇总行 + 失败明细块），两种都出现时取并集去重。
function parseReds(text) {
  const reds = new Set();
  const envReds = new Set();
  let pass = null, total = null;
  // 汇总行: "=== 18/25 通过 ==="
  let m = text.match(/===\s*(\d+)\s*\/\s*(\d+)\s*通过\s*===/);
  if (m) { pass = Number(m[1]); total = Number(m[2]); }
  // 明细行: "  FAIL  cp3_f1_null_elements.js"
  for (const line of text.split(/\r?\n/)) {
    const f = line.match(/^\s*FAIL\s+(\S+\.js)\b/);
    if (f) reds.add(f[1]);
    // 失败明细块: "  cp3_f1_null_elements.js（断言失败）"
    const d = line.match(/^\s{2}(\S+\.js)（[^）]*）\s*$/);
    if (d) reds.add(d[1]);
    // run_e2e 的 "启动/环境失败"子集：这些红**不是产品红**，不参与稳定性判据。
    const e = line.match(/^\s*REDS_ENV=(.*)$/);
    if (e && e[1].trim()) e[1].trim().split(',').forEach((x) => x && envReds.add(x.trim()));
  }
  return { pass, total, reds: [...reds].sort(), envReds: [...envReds].sort() };
}

// ── 已知 flake（**不参与**「红集合稳定」判据）────────────────────────────
//
// verify_login_local_hint.js：**探针自身的启动/连接类失败**，不是产品缺陷。
//
//   · 实测复现率 **1/22 ≈ 4.5%**（批次A 1/10、批次B 0/12；同一隔离实例、同一 HINT_URL、
//     同一探针二进制，端口为现场探测的空闲端口）——2026-10-26 由 verifier-wave2 测量。
//   · 失败形态：**exit=1 且没有任何 FAIL 行**（断言根本没跑到）⇒ 归因在探针侧，
//     与「产品行为错了」是两件事。
//   · 它出现时会让「连跑两次 REDS 逐字相同」误判；而那条判据要守的是**产品红集合**，
//     不是探针自身的抖动。
//
// 处置：从稳定性判据里**剔除**，但**原文照打**（REDS 原样保留，另加 REDS_FLAKY 一行），
// 不隐藏、不静默。⚠ 若复现率升高（>20%）或失败形态变成「有 FAIL 行」，必须重新归因，
// 不许继续挂 flake。
const KNOWN_FLAKY = {
  'verify_login_local_hint.js': '探针自身启动类失败（exit=1 且无 FAIL 行），实测 1/22≈4.5%',
};
function splitFlaky(reds) {
  return {
    stable: reds.filter((f) => !KNOWN_FLAKY[f]),
    flaky: reds.filter((f) => KNOWN_FLAKY[f]),
  };
}

// 跑一轮：起夹具实例 → run_e2e → 收尾（**走 withFamily3，保证 finally 必收**）。
function runOnce(port, keep, logdir) {
  return fam.withFamily3({ port }, async (fx) => {
    const runner = path.join(__dirname, 'run_e2e.js');
    // ⚠ `--logdir` 必须**转发给 run_e2e.js**：本入口自己不认识它。
    //   实测踩到：不转发时每条探针的完整日志落到 `os.tmpdir()/wb2api-e2e-logs`，
    //   而调用方以为自己指定的目录里有 —— 找人要日志时会发现"那个目录是空的"。
    const argv = [runner, fx.baseUrl];
    if (logdir) argv.push('--logdir=' + logdir);
    let out = '';
    const code = await new Promise((resolve) => {
      const ch = spawn(process.execPath, argv, {
        cwd: __dirname,
        // 边跑边转发（人看的）+ 同时攒起来（机器比的）
        stdio: ['ignore', 'pipe', 'pipe'],
        env: Object.assign({}, process.env, { WB2API_EXE: process.env.WB2API_EXE }),
      });
      ch.stdout.on('data', (d) => { const s = d.toString('utf8'); out += s; process.stdout.write(s); });
      ch.stderr.on('data', (d) => { const s = d.toString('utf8'); out += s; process.stderr.write(s); });
      ch.on('exit', (c) => resolve(c === null ? 1 : c));
    });

    const r = parseReds(out);
    // 产品稳定集合 = 全部红 − 启动/环境红 − 已测量并挂账的探针 flake。
    const envSet = new Set(r.envReds);
    const sp = splitFlaky(r.reds.filter((f) => !envSet.has(f)));
    console.log('');
    console.log('=== 第三族红集合（这一行就是可复现性判据）===');
    console.log('  REDS=' + r.reds.join(','));
    console.log('  REDS_STABLE=' + sp.stable.join(',') + '   ← 稳定性判据只看这一行');
    if (sp.flaky.length) {
      console.log('  REDS_FLAKY=' + sp.flaky.join(',') + '   ← 已知探针 flake，不参与判据');
      sp.flaky.forEach((f) => console.log('      · ' + f + ' —— ' + KNOWN_FLAKY[f]));
    }
    console.log('  REDS_COUNT=' + r.reds.length + '  PASS=' + (r.pass === null ? '?' : r.pass) +
      '  TOTAL=' + (r.total === null ? '?' : r.total));
    console.log('  exit=' + code + '　端口=' + port + '　时间=' + new Date().toISOString());
    if (fx.seedMismatch) {
      console.log('  ⚠ 本次**前置不成立**（种子回读失败）⇒ 依赖种子的那几条红不算产品缺陷:');
      fx.seedMismatch.forEach((p) => console.log('      · ' + p));
    }

    if (keep) {
      console.log('  --keep: 保留临时目录 ' + fx.tmp + '（只 kill 进程，不删目录）');
      // 这里不能走 withFamily3 的正常收尾（它会删目录），所以手动 kill。
      // ⚠ 仍要 await：stop 要等端口真空出来。
      try { await fx.stop(); } catch (e) { console.error('  ⚠ stop 失败: ' + (e && e.message)); }
      fx.__keptNo = true;
    }
    return { code, ...r, stableReds: sp.stable, seedOk: !fx.seedMismatch };
  }).finally(() => { /* withFamily3 已收尾（keep 分支在上面已手动 stop） */ });
}

(async () => {
  const argv = process.argv.slice(2);
  const keep = argv.indexOf('--keep') >= 0;
  const repeat = Number((argv.find((a) => a.startsWith('--repeat=')) || '--repeat=1').split('=')[1]) || 1;
  const portArg = (argv.find((a) => a.startsWith('--port=')) || '').split('=')[1];
  const logdir = (argv.find((a) => a.startsWith('--logdir=')) || '').split('=')[1] || '';
  const port = portArg ? Number(portArg) : await freePort(18100);

  const runs = [];
  for (let i = 1; i <= repeat; i++) {
    console.log('');
    console.log('################ 第 ' + i + ' / ' + repeat + ' 次（端口 ' + port + '）################');
    runs.push(await runOnce(port, keep, logdir));
  }

  let bad = runs.some((r) => r.code !== 0 || !r.seedOk) ? 1 : 0;
  if (repeat > 1) {
    // ⚠ 判据是「**稳定红集合**逐字相同」（不含 KNOWN_FLAKY）：探针自身的启动抖动
    //   不该让产品红集合的稳定性判据变红，但原始 REDS 仍然照打（不隐藏）。
    const first = runs[0].stableReds.join(',');
    const same = runs.every((r) => r.stableReds.join(',') === first);
    console.log('');
    console.log('=== 可复现性（' + repeat + ' 连跑，同一端口）===');
    runs.forEach((r, i) => console.log('  run' + (i + 1) + ' REDS_STABLE=' + r.stableReds.join(',') +
      '  REDS=' + r.reds.join(',') +
      '  (PASS=' + (r.pass === null ? '?' : r.pass) + '/' + (r.total === null ? '?' : r.total) + ')'));
    console.log('  ⇒ 稳定红集合（已剔除已知 flake）' +
      (same ? '**逐字相同** ✓' : '**不一致** ✗ —— 这是真的"数字会飘"，必须修夹具'));
    const anyFlaky = runs.some((r) => r.reds.length !== r.stableReds.length);
    if (anyFlaky) {
      console.log('  ⚠ 本轮有已知 flake 出现（已从判据剔除，明细见各 run 的 REDS_FLAKY）:');
      Object.keys(KNOWN_FLAKY).forEach((k) => console.log('      · ' + k + ' —— ' + KNOWN_FLAKY[k]));
    }
    if (!same) bad = 1;
  }
  process.exit(bad);
})().catch((e) => {
  console.log('EXCEPTION: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
