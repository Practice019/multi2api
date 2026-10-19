// verify_pagesize_default.js —— T9 浏览器实测：新会话默认每页 15 条。
//
// # 判据必须是"实际请求了几条"，不是"读某个常量"
//
// 静态套件已验过常量值（且已改成实读源码）。这里验的是**端到端行为**：
// 清空 localStorage → 打开请求日志 → 拦截网络请求 → 看 limit 是不是 15。
//
// 这与本项目教训一致：读代码/读常量 ≠ 用户看到的行为。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9303;
const RAW_TARGET = process.env.PS_URL || 'http://127.0.0.1:18080/ui';

// ⚠ 入口必须带令牌，否则本探针量到的是**登录页**，而它的断言全是"正向"的
//   （"下拉里有 15"这种），于是会红在"值不对"这种迷惑的地方 ——
//   归因指向分页默认值，真因却是"页面根本不是控制台"。
//   这条口径与 lib_cdp 的探针密钥一致：优先 URL 上的 `?token=`，
//   其次 WB2API_PROBE_TOKEN，再次 WB2API_API_KEY。
const TARGET = (function resolveConsoleUrl(u) {
  if (/[?&]token=/.test(u)) return u;
  const tok = process.env.WB2API_PROBE_TOKEN || process.env.WB2API_API_KEY || '';
  if (!tok) return u;
  return u + (u.indexOf('?') >= 0 ? '&' : '?') + 'token=' + encodeURIComponent(tok);
})(RAW_TARGET);
const PROFILE = path.join(os.tmpdir(), 'chrome-pagesize');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    const reqs = [];
    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Network.requestWillBeSent') reqs.push(m.params.request.url);
    };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable'); await send('Network.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(5000);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

    // ⚠ 先**正向自证**"打开的是控制台外壳"，再谈分页。
    //
    // 缺了这一步，入口错（登录页 / 空体）时下面的断言会红在
    // "新会话请求 limit=15（实际 null）"这种地方 —— 看着像"默认值坏了"，
    // 其实是页面不对。把前提说清楚，红的归因才是准的。
    const shell = JSON.parse(await ev(`JSON.stringify({
      hasApi: typeof window.__wb2api__ === 'object' && !!window.__wb2api__,
      hasNav: !!document.getElementById('nav'),
      hasContent: !!document.getElementById('content'),
      len: (document.body ? document.body.innerHTML.length : 0),
    })`));
    console.log('  控制台外壳: ' + JSON.stringify(shell));
    ok(shell.hasApi && shell.hasNav && shell.hasContent,
      '打开的是控制台外壳（__wb2api__ + #nav + #content 都在）—— ' +
      '入口: ' + TARGET.replace(/token=[^&]*/, 'token=***'));

    // 全新 profile = localStorage 为空（新会话）
    const lsEmpty = await ev(`(function(){ try { return localStorage.getItem('wb2api.pagesize'); } catch(e){ return 'ERR'; } })()`);
    console.log('  localStorage 里的 pagesize: ' + JSON.stringify(lsEmpty));

    // 切到请求日志，看它发出的请求
    const before = reqs.length;
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('请求日志') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2500);

    const logReq = reqs.slice(before).filter(u => u.indexOf('/admin/logs/history') >= 0);
    console.log('  切面板后发的日志请求: ' + JSON.stringify(logReq.slice(0, 3)));
    const m = logReq.length ? /limit=(\d+)/.exec(logReq[0]) : null;
    const gotLimit = m ? m[1] : null;
    console.log('  实际 limit = ' + gotLimit);
    ok(gotLimit === '15', '新会话请求 limit=15（实际 ' + gotLimit + '）');

    // ⚠ 「任务历史」标签页**已被用户要求删除**（`#histrows` / `#histPager` 随之下线，
    //   见 webui.html 里 `loadHistory` 开头那段说明），所以原来那一段
    //   "任务历史同样 limit=15" 无从量起。
    //
    //   判据的意图是"**默认每页 15** 这件事在新会话里对所有走 pager 的面板都成立"。
    //   重新表达成两条更强的：
    //     ① 反向守卫：`#histPager` 不存在（被删的标签页没复活）；
    //     ② 正向：默认值**真的落到了渲染上** —— 日志面板第一页渲染 15 行，
    //        且分页器里的"每页"控件显示 15。
    //   ② 比原来强：原来只读了**请求 URL** 里的 limit（"发对了"），
    //   现在多验一层"画出来的行数也确实按它切"（"用对了"）。
    const histGone = await ev(`!document.getElementById('histPager') && !document.getElementById('histrows')`);
    ok(histGone === true, '#histPager / #histrows 不存在（被删掉的「任务历史」标签页没有复活）');

    // 回到请求日志，量"默认值落到渲染上"
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('请求日志') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2500);
    const rendered = await ev(`document.querySelectorAll('#logrows .log-row').length`);
    console.log('  日志面板第一页渲染行数: ' + rendered);
    ok(rendered === 15,
      '新会话日志面板第一页渲染 ' + rendered + ' 行 == 默认每页 15（默认值落到了渲染上，不只是发对了请求）');

    // 面板上的每页控件应显示 15
    // ⚠ 原来这里只算了一个 `shown` 却**从不断言**（死读）。改成三条真断言：
    //   控件必须存在 + 必须显示 15 + 它的值必须与接口请求里的 limit 一致。
    const shownInfo = JSON.parse(await ev(`JSON.stringify((function(){
      var inp = document.querySelector('#logPageSize') ||
                Array.from(document.querySelectorAll('#logPager input, #logPager select')).filter(function(e){
                  return e.type === 'number' || e.tagName === 'SELECT';
                })[0];
      return { found: !!inp, value: inp ? String(inp.value) : null,
               tag: inp ? inp.tagName.toLowerCase() : null };
    })())`));
    console.log('  每页控件: ' + JSON.stringify(shownInfo));
    ok(shownInfo.found, '日志面板有"每页"控件（#logPageSize 或 #logPager 里的数字/下拉）');
    ok(shownInfo.found && shownInfo.value === gotLimit,
      '每页控件显示的值（' + shownInfo.value + '）== 实际请求的 limit（' + gotLimit + '）' +
      ' —— 界面显示与真实行为一致，不是"显示 15 实际发别的"');

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== T9 验证通过（默认每页 15）===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
