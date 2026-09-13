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
// # 判据（v2 —— 改掉了会报**假绿**的第一版）
//
// 1. **后端**：`{"provider":"codearts"}` 与 `{"provider":"workbuddy"}` 的
//    **`dir` 必须不同** —— 目录名不同是**结构性**的，与目录里几个文件无关
// 2. **后端**：`{}`（不带 provider）行为与改前一致
// 3. **后端**：不存在的上游 → 404
// 4. **前端**：点 codearts 行，请求体含 `provider":"codearts"`（见另一个脚本）
//
// ## ⚠ 第一版为什么是坏的（Reviewer 抓出来的）
//
// 第一版靠 `scanned 不同` 判"真的选了不同目录"。**两目录文件数相同时它恒真**：
// 恒用 workbuddy 的缺陷实现两次都返回 `scanned: 3`，
// 而 `3 === counts.workbuddy` 与 `3 === counts.codearts` **同时成立**。
//
// 真实环境恰好就是 workbuddy=3 / codearts=3。实测缺陷实现下：
//
//   请求 codearts → {"provider":"workbuddy","scanned":3}
//   三条 scanned 断言全部 → true → 报「验证通过」exit=0        ← **假绿**
//
// 第一版**确实**会打印一条"前提不满足"的警告，但它**报完警照样打 PASS、
// 照样 exit=0、照样输出"验证通过"** —— 所以那句警告救不了任何人。
//
// 现在主力判据换成 **`dir`**，`scanned` 退为辅助并**明确标注它在同数时恒真**。
// 回执若没有 `dir` 字段，脚本**直接 FAIL** 而不是静默降级成弱判据。
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

    // ⚠ 判据主力：回执的 **dir**，不是 scanned。
    //
    // # 为什么（Reviewer 抓出来的，我原来的写法会报**假绿**）
    //
    // 我原来靠 `scanned 不同` 判"真的选了不同目录"。**两目录文件数相同时它恒真**：
    // 恒用 workbuddy 的缺陷实现，两次都返回 `scanned: 3`，
    // 而 `3 === counts.workbuddy` 与 `3 === counts.codearts` **同时成立** → 照样 PASS。
    //
    // 实测（真实环境 workbuddy=3、codearts=3）：
    //   缺陷实现回执：请求 codearts → {"provider":"workbuddy","scanned":3}
    //   脚本三条 scanned 断言全部 → true → 报「验证通过」exit=0
    //
    // **我前两版说"脚本会自报前提所以不会骗人" —— 那句话是错的。**
    // 它报了警，然后**照样打 PASS、照样 exit=0、照样输出"验证通过"**。
    //
    // 现在改用 **dir**：目录名不同是**结构性**的，不依赖文件数量。
    // scanned 退为**辅助**判据（它仍能验"扫的是不是那个目录的条数"）。
    const dirs = { workbuddy: 'auths\\workbuddy', codearts: 'auths\\codearts' };
    if (rW.json.dir && rC.json.dir) {
      ok(rW.json.dir !== rC.json.dir,
        '两个上游扫的**目录不同**（' + rW.json.dir + ' vs ' + rC.json.dir +
        '）—— 这是结构性判据，与目录里有几个文件无关');
      ok(rW.json.dir === dirs.workbuddy,
        'workbuddy 扫的是它自己的目录（实际 ' + rW.json.dir + '，期望 ' + dirs.workbuddy + '）');
      ok(rC.json.dir === dirs.codearts,
        'codearts 扫的是它自己的目录（实际 ' + rC.json.dir + '，期望 ' + dirs.codearts + '）');
    } else {
      // 回执没带 dir（老版本后端）→ **明确说出来**，不要静默降级成弱判据
      console.log('  ⚠ 回执没有 dir 字段 —— 结构性判据无法执行，' +
        '本轮**只能**靠 provider 字段判断（scanned 在同数目录下恒真，不可信）');
      ok(false, '回执缺少 dir 字段（结构性判据依赖它）');
    }

    // scanned 作为辅助：对上目录数
    ok(rW.json.scanned === counts.workbuddy,
      '（辅助）workbuddy 的 scanned（' + rW.json.scanned + '）== 它目录的文件数（' + counts.workbuddy + '）');
    ok(rC.json.scanned === counts.codearts,
      '（辅助）codearts 的 scanned（' + rC.json.scanned + '）== 它目录的文件数（' + counts.codearts + '）');
    if (counts.workbuddy === counts.codearts) {
      console.log('  ℹ 两目录文件数相同（' + counts.workbuddy + '）—— ' +
        '上面两条 scanned 断言**在本环境下恒真**，不作为鉴别力来源（主力是 dir）');
    }
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
