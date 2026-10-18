// heat_console_probe.js —— 抓热力图渲染时的运行时异常（2026-10-19）。
//
// # 为什么需要它
//
// `chk_js.js` 只做**语法**检查，它绿了不代表跑得起来。这次改完后门禁是绿的，
// 但页面里热力图整块是空的、图例停在占位符 `—` —— 典型的"函数中途抛异常"。
// 语法门禁看不见这类错，只有浏览器 console 能说清是哪一行、哪个标识符。
//
// ⚠ 必须在 `Page.navigate` **之前** `Runtime.enable`，否则加载期的异常收不到
//   （本文件第一次就是这么漏掉的：enable 晚了一步，日志里什么都没有）。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9333;
const TARGET = process.env.HEAT_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-cons-' + Date.now());
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, (r) => {
      const c = [];
      r.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res(Buffer.concat(c).toString('utf8')));
    }).on('error', rej);
  });
}

(async () => {
  try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run',
    '--no-default-browser-check', '--remote-debugging-port=' + PORT,
    '--user-data-dir=' + PROFILE, '--window-size=1600,1200', 'about:blank'],
    { stdio: 'ignore' });

  let ws = null;
  const events = [];
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');
    ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0;
    const pend = new Map();
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      // ⚠ 三个来源都要收：未捕获异常、console.error、以及被 try/catch 吞掉后
      //   又打出来的日志。热力图那条调用链外面正好包着 try（见 loadHeatmap 的调用点）。
      if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails || {};
        events.push('[异常] ' + (d.exception && d.exception.description ? d.exception.description : d.text)
          + '  @' + ((d.url || '') + ':' + (d.lineNumber != null ? d.lineNumber + 1 : '?') + ':' + (d.columnNumber != null ? d.columnNumber + 1 : '?')));
      } else if (m.method === 'Runtime.consoleAPICalled') {
        const txt = (m.params.args || []).map((a) => (a.value !== undefined ? a.value : a.description)).join(' ');
        events.push('[console.' + m.params.type + '] ' + txt);
      } else if (m.method === 'Log.entryAdded') {
        const en = m.params.entry || {};
        if (en.level === 'error') events.push('[log.error] ' + en.text + ' @' + (en.url || ''));
      }
    };
    const send = (mm, p) => new Promise((r) => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // ⚠ 顺序：先把三个域都 enable，再 navigate。
    await send('Runtime.enable');
    await send('Log.enable');
    await send('Page.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(9000);

    console.log('=== 浏览器侧事件（' + events.length + ' 条）===');
    if (!events.length) console.log('  （一条都没有 —— 那说明异常被无声吞了）');
    for (const e of events) console.log('  ' + e);

    console.log('=== 热力图 DOM 现状 ===');
    const st = await ev(`(function(){
      var h = document.getElementById('statsHeat');
      var m = document.getElementById('statsHeatMonths');
      var s = document.getElementById('heatStats');
      var tip = document.getElementById('hoverTip');
      return JSON.stringify({
        hasGrid: !!h, gridChildren: h ? h.children.length : -1,
        hasMonths: !!m, monthChildren: m ? m.children.length : -1,
        hasStats: !!s, statsChildren: s ? s.children.length : -1,
        hasHoverTip: !!tip,
        gridClass: h ? h.className : null,
        legend: ['h1','h2','h3','h4'].map(function(i){ var e=document.getElementById(i); return e?e.textContent:'(无)'; }).join(',')
      });
    })()`);
    console.log('  ' + st);

    // ⚠ 手工重跑一次 renderHeatmap，看它自己抛什么（比猜强）。
    console.log('=== 手工复现（直接调 window.__wb2api__ 里的公开函数）===');
    const again = await ev(`(function(){
      try {
        var W = window.__wb2api__ || {};
        var keys = Object.keys(W).filter(function(k){ return /[Hh]eat/.test(k); });
        return JSON.stringify({exposedHeat: keys, hasRenderHeatmap: typeof W.renderHeatmap});
      } catch (e) { return 'ERR ' + e.message; }
    })()`);
    console.log('  ' + again);
  } catch (e) {
    console.error('失败: ' + (e && e.message ? e.message : e));
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { ch.kill(); } catch { }
    await sleep(300);
  }
  process.exit(0);
})();
