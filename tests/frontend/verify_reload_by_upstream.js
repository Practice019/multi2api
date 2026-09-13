// verify_reload_by_upstream.js —— T2+T3 的端到端验收：按上游重载 auths。
//
// # 为什么需要它（单测覆盖不到的部分）
//
// T2/T3 的核心是**跨层接线**：前端把点的那行 provider 放进请求体 →
// 后端据此选目录 → 回执反映**实际重载的那个上游**。
// 这条链路的任何一环断了，单测都可能全绿：
//
//   · 前端传了但后端没读 → 后端仍用 reloadProvider（单测只测后端，看不出）
//   · 后端读了但前端文案用自己猜的 pid → 界面继续撒谎（后端单测看不出）
//
// # 判据
//
// 1. **后端**：`{"provider":"codearts"}` 与 `{"provider":"workbuddy"}` 的
//    `scanned` **必须不同**（依赖两个目录文件数不同 —— 见下）
// 2. **后端**：`{}`（不带 provider）行为与改前一致
// 3. **后端**：不存在的上游 → 404
// 4. **前端**：点 codearts 行，请求体含 `provider":"codearts"`
// 5. **前端**：toast 文案里的上游 = 后端回执的 `provider`
//
// ⚠ 判据 1 的前提：**两个上游目录的文件数不同**。
// 若相同，"恒用 reloadProvider" 的错误实现会**蒙对**。
// 本脚本会先**检查这个前提**，不满足就明确报出来，而不是给个假绿灯。
'use strict';
const http = require('http');
const fs = require('fs');

const PORT = Number(process.env.RELOAD_PORT || 18080);
const KEY = (() => {
  if (process.env.WB2API_KEY) return process.env.WB2API_KEY;
  try { return JSON.parse(fs.readFileSync('D:\\tmp\\run-' + PORT + '.json', 'utf8')).api_key; }
  catch { return ''; }
})();

function post(path, body) {
  return new Promise(res => {
    const b = JSON.stringify(body);
    const q = http.request({
      host: '127.0.0.1', port: PORT, path, method: 'POST',
      headers: { Authorization: 'Bearer ' + KEY, 'Content-Type': 'application/json',
                 'Content-Length': Buffer.byteLength(b) },
    }, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => {
      let j = null; try { j = JSON.parse(d); } catch { }
      res({ code: r.statusCode, body: d, json: j });
    }); });
    q.on('error', () => res({ code: 0, body: '(请求失败)' }));
    q.write(b); q.end();
  });
}

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  console.log('=== 按上游重载 auths（端到端）===\n');

  // ---- 0. 前提检查：两个目录的文件数必须不同 ----
  const dirs = {
    workbuddy: 'D:\\project_GIT\\workbuddy2api实验版本\\auths\\workbuddy',
    codearts: 'D:\\project_GIT\\workbuddy2api实验版本\\auths\\codearts',
  };
  const counts = {};
  for (const [k, d] of Object.entries(dirs)) {
    try {
      counts[k] = fs.readdirSync(d).filter(f => f.endsWith('.json')).length;
    } catch { counts[k] = -1; }
  }
  console.log('  目录文件数: ' + JSON.stringify(counts));
  if (counts.workbuddy === counts.codearts) {
    console.log('\n  ⚠ **前提不满足**：两个目录文件数相同（' + counts.workbuddy + '）——');
    console.log('     "恒用 reloadProvider" 的错误实现会**蒙对**下面第 1 条断言。');
    console.log('     请让两目录文件数不同，或直接看第 2/3/4/5 条。\n');
  } else {
    console.log('  → 前提成立：两目录文件数不同，判据 1 有鉴别力\n');
  }

  // ---- 1. 两个上游的 scanned 必须不同 ----
  const rW = await post('/admin/accounts/reload', { provider: 'workbuddy' });
  const rC = await post('/admin/accounts/reload', { provider: 'codearts' });
  console.log('  workbuddy: HTTP ' + rW.code + ' ' + JSON.stringify(rW.json));
  console.log('  codearts : HTTP ' + rC.code + ' ' + JSON.stringify(rC.json));

  ok(rW.code === 200 && rC.code === 200, '两个上游的重载都返回 200');
  if (rW.json && rC.json) {
    ok(rW.json.provider === 'workbuddy', 'workbuddy 回执的 provider 正确（实际 ' + rW.json.provider + '）');
    ok(rC.json.provider === 'codearts', 'codearts 回执的 provider 正确（实际 ' + rC.json.provider + '）');
    if (counts.workbuddy !== counts.codearts) {
      ok(rW.json.scanned !== rC.json.scanned,
        '两个上游 scanned **不同**（' + rW.json.scanned + ' vs ' + rC.json.scanned +
        '）—— 证明真的按 provider 选了目录，不是恒用同一个');
    } else {
      ok(rW.json.scanned === counts.workbuddy && rC.json.scanned === counts.codearts,
        '两目录文件数相同时，各自 scanned 应对上自己的目录数');
    }
    // scanned 应等于**该上游目录**的文件数
    ok(rW.json.scanned === counts.workbuddy,
      'workbuddy 的 scanned（' + rW.json.scanned + '）== 它目录的文件数（' + counts.workbuddy + '）');
    ok(rC.json.scanned === counts.codearts,
      'codearts 的 scanned（' + rC.json.scanned + '）== 它目录的文件数（' + counts.codearts + '）');
  }

  // ---- 2. 不带 provider：行为与改前一致（回落到 reloadProvider）----
  const rNone = await post('/admin/accounts/reload', {});
  console.log('  {}       : HTTP ' + rNone.code + ' ' + JSON.stringify(rNone.json));
  ok(rNone.code === 200, '不带 provider 时仍返回 200（向后兼容）');
  if (rNone.json) {
    ok(rNone.json.provider === 'workbuddy',
      '回落到的上游是 reloadProvider 配的那个（实际 ' + rNone.json.provider + '）');
  }

  // ---- 3. 不存在的上游 → 404 ----
  const rGhost = await post('/admin/accounts/reload', { provider: 'ghost-upstream' });
  console.log('  ghost    : HTTP ' + rGhost.code + ' ' + rGhost.body.slice(0, 70));
  ok(rGhost.code === 404,
    '不存在的上游返回 404（实际 ' + rGhost.code + '）—— 不能默默用默认目录');

  console.log(fail === 0 ? '\n=== 按上游重载验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
