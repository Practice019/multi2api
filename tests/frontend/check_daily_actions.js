// probe_daily_actions_baseline.js —— 记录**当前**每个上游账号行里显示的按钮清单。
//
// # 为什么先写这个
//
// T3 要把写死的「签到」抽象成"上游自报的每日动作"。改动的最大风险**不是**
// 新功能不做，而是**把 workbuddy 现有的三个动作弄坏**——它们现在能用。
//
// 所以第一步不是改代码，是**把现状钉成基线**：每个上游、每个账号行里
// 到底有哪些按钮、文案是什么、data-act 是什么、点了调哪个端点。
// 改完之后拿同一份探针再跑一遍，逐字比对。
//
// 输出是一份 JSON，写到 stdout，可以直接 diff。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = Number(process.env.PROBE_CDP_PORT || 9341);
const TARGET = process.env.ACC_URL || 'http://127.0.0.1:18080/ui';
// ⚠ 每次运行用**唯一**的 profile 目录，并且**不删旧的**。
//
// 我第一版用固定目录 + 开头 rmSync，实测 EPERM：
// 上一次运行留下的 Chrome 进程还占着那个目录（Windows 上被占用的目录
// 无法删除），于是**第二次运行直接崩**，而不是给出结果。
//
// 固定目录在这里没有任何好处（每次都是全新的浏览器实例），
// 所以改成唯一目录。代价是 Temp 里会留目录 —— 那比"第二次跑就崩"好得多。
const PROFILE = path.join(os.tmpdir(), 'chrome-dailyact-' + Date.now() + '-' + process.pid);
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    if (!t) { console.log(JSON.stringify({ error: 'Chrome 未启动' })); process.exit(2); }
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(6000);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

    // 切到账号池面板（否则 offsetHeight 恒为 0，量不到真实几何）
    await ev(`String(window.__wb2api__.showPanelByKey('accounts'))`);
    await sleep(1500);

    const snap = await ev(`JSON.stringify((function(){
      var out = { providers: {}, topButtons: [], rows: [] };
      var sec = document.querySelector('#content > section[data-key="accounts"]');
      if (sec) {
        var h2 = sec.querySelector('h2');
        if (h2) out.topButtons = Array.prototype.slice.call(h2.querySelectorAll('button'))
          .map(function(b){ return { text: (b.textContent||'').replace(/\\s+/g,' ').trim(), id: b.id || null }; });
      }
      document.querySelectorAll('#accts tr.grouprow').forEach(function(tr){
        out.providers[tr.dataset.acctgroup] = {
          groupActions: Array.prototype.slice.call(tr.querySelectorAll('button'))
            .map(function(b){ return (b.textContent||'').replace(/\\s+/g,' ').trim(); })
        };
      });
      document.querySelectorAll('#accts tr.acctrow').forEach(function(tr){
        var pid = tr.dataset.acctof;
        var acts = Array.prototype.slice.call(tr.querySelectorAll('button[data-act]'))
          .map(function(b){ return { act: b.dataset.act, text: (b.textContent||'').trim(), title: b.title || '' }; });
        out.rows.push({ provider: pid, acts: acts });
        if (out.providers[pid]) { out.providers[pid].rowActs = acts; }
      });
      return out;
    })())`);

    // 行内动作 → 端点映射（从页面里读 ACT_LABEL / paths，不是从源码猜）
    const snap2 = await ev(`JSON.stringify((function(){
      var W = window.__wb2api__;
      return { exports: Object.keys(W || {}).sort() };
    })())`);

    console.log(JSON.stringify({ baseline: JSON.parse(snap), api: JSON.parse(snap2) }, null, 2));
    ws.close();
  } catch (e) { console.log(JSON.stringify({ error: e.message })); }
  finally { try { ch.kill(); } catch { } }
})();
