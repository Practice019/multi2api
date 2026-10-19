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
  }
  return { pass, total, reds: [...reds].sort() };
}

// 跑一轮：起夹具实例 → run_e2e → 收尾（**走 withFamily3，保证 finally 必收**）。
function runOnce(port, keep) {
  return fam.withFamily3({ port }, async (fx) => {
    const runner = path.join(__dirname, 'run_e2e.js');
    let out = '';
    const code = await new Promise((resolve) => {
      const ch = spawn(process.execPath, [runner, fx.baseUrl], {
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
    console.log('');
    console.log('=== 第三族红集合（这一行就是可复现性判据）===');
    console.log('  REDS=' + r.reds.join(','));
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
    return { code, ...r, seedOk: !fx.seedMismatch };
  }).finally(() => { /* withFamily3 已收尾（keep 分支在上面已手动 stop） */ });
}

(async () => {
  const argv = process.argv.slice(2);
  const keep = argv.indexOf('--keep') >= 0;
  const repeat = Number((argv.find((a) => a.startsWith('--repeat=')) || '--repeat=1').split('=')[1]) || 1;
  const portArg = (argv.find((a) => a.startsWith('--port=')) || '').split('=')[1];
  const port = portArg ? Number(portArg) : await freePort(18100);

  const runs = [];
  for (let i = 1; i <= repeat; i++) {
    console.log('');
    console.log('################ 第 ' + i + ' / ' + repeat + ' 次（端口 ' + port + '）################');
    runs.push(await runOnce(port, keep));
  }

  let bad = runs.some((r) => r.code !== 0 || !r.seedOk) ? 1 : 0;
  if (repeat > 1) {
    const first = runs[0].reds.join(',');
    const same = runs.every((r) => r.reds.join(',') === first);
    console.log('');
    console.log('=== 可复现性（' + repeat + ' 连跑，同一端口）===');
    runs.forEach((r, i) => console.log('  run' + (i + 1) + ' REDS=' + r.reds.join(',') +
      '  (PASS=' + (r.pass === null ? '?' : r.pass) + '/' + (r.total === null ? '?' : r.total) + ')'));
    console.log('  ⇒ 红集合' + (same ? '**逐字相同** ✓' : '**不一致** ✗ —— 这就是"数字会飘"，必须修夹具'));
    if (!same) bad = 1;
  }
  process.exit(bad);
})().catch((e) => {
  console.log('EXCEPTION: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});
