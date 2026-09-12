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
const TARGET = process.env.PS_URL || 'http://127.0.0.1:18080/ui';
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

    // 任务历史同理
    const before2 = reqs.length;
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('任务历史') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2500);
    const histReq = reqs.slice(before2).filter(u => u.indexOf('/admin/checkin/history') >= 0);
    const m2 = histReq.length ? /limit=(\d+)/.exec(histReq[0]) : null;
    console.log('  任务历史请求: ' + JSON.stringify(histReq.slice(0, 2)));
    ok(m2 && m2[1] === '15', '任务历史同样 limit=15（实际 ' + (m2 ? m2[1] : '—') + '）');

    // 面板上的每页控件应显示 15
    const shown = await ev(`(function(){
      var inp = document.querySelector('#logPageSize, #histPageSize, input[type=number]');
      return inp ? inp.value : '(未找到)';
    })()`);
    console.log('  每页输入框显示: ' + JSON.stringify(shown));

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== T9 验证通过（默认每页 15）===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
