// diag_accts_listener.js —— R3 的委托是否会随刷新累积（本项目改过的那个 bug）。
//
// # 为什么必须单独测这一条
//
// `renderAccounts` 每次刷新都重建整个 <tbody>。如果折叠的事件是**逐行**绑的，
// 监听器会随刷新次数累积 —— 这正是本项目 F2 的形态。
// 我的实现是委托绑在 #accts 上（一次），但"我这么写了"不等于"它没泄漏"，
// 所以这里用 CDP 的 DOMDebugger 实际数一遍。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9323;
const TARGET = process.env.ACC_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-acctlistener');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
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
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable'); await send('DOM.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(7000);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); if (r.result && r.result.exceptionDetails) return 'EXC'; return r.result && r.result.result ? r.result.result.value : undefined; };

    // 取 #accts 的 objectId，再用 DOMDebugger.getEventListeners 数监听器。
    const root = await send('Runtime.evaluate', { expression: `document.getElementById('accts')` });
    const objId = root.result.result.objectId;
    const count = async () => {
      const r = await send('DOMDebugger.getEventListeners', { objectId: objId });
      return (r.result && r.result.listeners ? r.result.listeners.length : -1);
    };

    const n0 = await count();
    console.log('  #accts 初始监听器数: ' + n0);
    ok(n0 > 0, '#accts 上有委托监听器（折叠与账号动作靠它）');
    ok(n0 <= 4, '#accts 上的监听器数是常数级（' + n0 + ' 个：账号动作 + 分组动作 + 折叠 + 可能的其它），不是每行一个');

    // 反复触发重绘（renderAccounts 每次都重建整个 tbody）
    for (let i = 0; i < 8; i++) {
      await ev(`window.__wb2api__.renderAccounts(window.__wb2api__.manifest().providers.map(function(){}).length ? null : null)`);
      await ev(`(function(){ var b=document.getElementById('refresh'); if(b) b.click(); return 1 })()`);
      await sleep(700);
    }
    await sleep(2000);
    const n1 = await count();
    console.log('  8 轮刷新重绘后监听器数: ' + n1);
    ok(n1 === n0, '#accts 监听器数**不随刷新增长**（' + n0 + ' → ' + n1 + '）—— 逐行绑会在这里红');

    // 折叠仍然工作（委托没被刷新破坏）
    await ev(`window.__wb2api__.showPanelByKey('accounts')`);
    await sleep(600);
    const works = await ev(`(function(){
      var tr = document.querySelector('#accts tr.grouprow');
      if (!tr) return 'no-group';
      var btn = tr.querySelector('button[data-atoggle]');
      if (!btn) return 'no-btn';
      btn.click();
      var rows = [];
      for (var e = tr.nextElementSibling; e; e = e.nextElementSibling) {
        if (e.classList.contains('grouprow')) break;
        if (e.classList.contains('acctrow')) rows.push(getComputedStyle(e).display);
      }
      var n = rows.length;
      return JSON.stringify({ collapsed: tr.classList.contains('collapsed'), n: n, hidden: rows.filter(function(d){return d==='none'}).length });
    })()`);
    console.log('  第 8 轮重绘后点折叠: ' + works);
    const w = JSON.parse(works);
    ok(w.collapsed === true && (w.n === 0 || w.hidden === w.n),
      '刷新多轮之后折叠**仍然生效**（委托活在 #accts 上，不在被替换的行上）');

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 账号池委托监听器纪律通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
