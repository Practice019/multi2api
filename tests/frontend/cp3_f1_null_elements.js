// cp3_f1_null_elements.js —— 验证 CP3 评审 F1：null 元素不再让 refresh 抛异常。
//
// # F1 的形态
//
// applyManifest 原先只做 `Array.isArray(x) ? x : []` —— 归一化到**数组层**，
// 没到**元素层**。于是 `providers:[null]` 一路流到渲染层：
//   upstreamGroups 的 `!!x.default`  → TypeError
//   renderJobs 的 `a.provider`       → TypeError
//
// 后果不是崩溃，而是"页面莫名停止刷新"（refresh 的 promise 抛出后不再继续），
// 且 window.__errs 实测为 0 —— 测试的错误捕获也看不到。极难定位。
//
// # 怎么测
//
// 在真实浏览器里注入含 null / 非对象元素的 manifest，
// 断言：① 不抛异常 ② 页面仍能正常渲染 ③ 后续 refresh 不受影响。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9252;
const URL_UT = process.env.F1N_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = require('os').tmpdir() + '/chrome-f1n';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,900', 'about:blank'], { stdio: 'ignore' });

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { /* 等 */ }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');

    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0;
    const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: URL_UT });
    await sleep(3500);

    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      // 注意：这里**不**把 exceptionDetails 当致命错误 —— 被测的正是"会不会抛"
      return r.result || {};
    };

    console.log('\n[CP3 F1] 含 null / 非对象元素的 manifest 不得让渲染抛异常');

    const shapes = [
      ['providers:[null]', '{ providers: [null] }'],
      ['providers:[null,合法]', '{ providers: [null, {id:"ok",capabilities:["chat"],account_count:1}] }'],
      ['jobs:[null]', '{ jobs: [null] }'],
      ['capabilities:[null]', '{ capabilities: [null] }'],
      ['admin_routes:[null]', '{ admin_routes: [null] }'],
      ['providers:[数字]', '{ providers: [42] }'],
      ['providers:[字符串]', '{ providers: ["x"] }'],
      ['providers:[true]', '{ providers: [true] }'],
      ['全部数组含 null', '{ providers:[null], jobs:[null], capabilities:[null], admin_routes:[null] }'],
    ];

    for (const [name, patch] of shapes) {
      const r = await ev(`(() => {
        const W = window.__wb2api__;
        const original = W.manifest();
        const out = { name: ${JSON.stringify(name)}, threw: null, groups: null, jobs: null, restored: null };
        try {
          const m = W.manifest();
          const p = ${patch};
          Object.assign(m, p);
          W.applyManifest(m);
          W.syncProviderPanels();
          W.buildNav();
          out.groups = document.querySelectorAll('#nav .navgroup').length;
          out.jobs = document.querySelectorAll('#jobrows tr').length;
        } catch (e) {
          out.threw = String(e && e.message || e);
        }
        // 还原并确认页面仍可用
        W.applyManifest(original); W.syncProviderPanels(); W.buildNav();
        out.restored = document.querySelectorAll('#nav .navgroup').length;
        return JSON.stringify(out);
      })()`);
      const v = JSON.parse(r.result.value);
      ok(v.threw === null, name + ' 不抛异常' + (v.threw ? '（实际抛出：' + v.threw + '）' : ''));
      ok(v.groups > 0, name + ' 仍渲染出导航分组（实际 ' + v.groups + '）');
      ok(v.restored > 0, name + ' 还原后页面仍可用（实际 ' + v.restored + ' 组）');
    }

    console.log('\n[回归] 合法 manifest 不受影响');
    const sane = await ev(`(() => {
      const W = window.__wb2api__;
      const m = W.manifest();
      return JSON.stringify({
        providers: m.providers.length,
        jobs: m.jobs.length,
        caps: m.capabilities.length,
        routes: m.admin_routes.length,
        groups: document.querySelectorAll('#nav .navgroup').length,
      });
    })()`);
    const s = JSON.parse(sane.result.value);
    console.log('    ' + JSON.stringify(s));
    ok(s.providers === 2, '合法 providers 数仍为 2');
    ok(s.jobs === 3, '合法 jobs 数仍为 3');
    ok(s.groups >= 3, '分组仍正常（通用 + 2 上游）');

    console.log('\n[健康]');
    const errs = await ev('JSON.stringify(window.__errs || [])');
    ok(errs.result.value === '[]' || errs.result.value === undefined, '无未捕获脚本错误');

    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { /* 已退出 */ } }

  console.log(fail === 0 ? '\n=== CP3 F1 修复验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
