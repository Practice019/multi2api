// shot_accounts.js —— 给账号池页面拍一张截图，作为 T4/T5 的**视觉证据**。
//
// # 为什么要单开一个
//
// 断言是"某一格等于什么"，而截图能一次说明**整列**的样子：
// 四个账号里 codearts 的额度是 7474、Token 是 `—`，workbuddy 三行是数字 + `59 天`。
// 断言绿而界面难看（比如整列挤在一起）时只有截图看得出来。
//
// 顺带把"注入 has_data=false"那一次的渲染也拍下来 ——
// 那是本次唯一无法在真机上自然出现的状态，图比文字可信。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9329;
const TARGET = process.env.T45_URL || 'http://127.0.0.1:18080/ui';
const API = TARGET.replace(/\/ui$/, '');
const KEY = process.env.WB2API_KEY || '__REDACTED_LEAKED_KEY__';
const PROFILE = path.join(os.tmpdir(), 'chrome-shot-' + Date.now());
const OUT = path.resolve(__dirname, '..', '..', '.task', 'ui-convergence', 'shared', 'shots');
const sleep = ms => new Promise(r => setTimeout(r, ms));

function get(u, h) {
  return new Promise((res, rej) => {
    http.get(u, { headers: h || {} }, r => {
      const c = [];
      r.on('data', x => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res(Buffer.concat(c).toString('utf8')));
    }).on('error', rej);
  });
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1600,1200', 'about:blank'], { stdio: 'ignore' });
  let ws = null;
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');
    ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(6500);
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // 切到账号池面板（隐藏面板里量任何尺寸恒为 0 —— 本项目踩过这个坑）
    await ev('window.__wb2api__.showPanelByKey("accounts")');
    await sleep(1200);

    async function shot(name) {
      const r = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
      const b = Buffer.from(r.result.data, 'base64');
      const p = path.join(OUT, name);
      fs.writeFileSync(p, b);
      console.log('  已保存 ' + p + '（' + b.length + ' 字节）');
    }

    console.log('=== 拍图 ===');
    await shot('t4t5-accounts-real.png');

    // 真实数据下，把两个上游各自的额度/Token 列打出来（供报告引用）
    const table = JSON.parse(await ev(
      'JSON.stringify(Array.prototype.map.call(document.querySelectorAll("#accts tr.acctrow"),'
      + ' function(tr){ var t=tr.querySelectorAll("td");'
      + '   return {provider:tr.dataset.acctof, uid:t[2].textContent.trim(),'
      + '     quota:t[3].textContent.trim(), token:t[5].textContent.trim()}; }))'));
    console.log('  真实渲染:');
    table.forEach(r => console.log('    ' + r.provider.padEnd(11) + ' uid=' + r.uid.padEnd(10)
      + ' 额度=' + String(r.quota).padEnd(8) + ' Token=' + r.token));

    // 注入 has_data=false，再拍一张
    const real = JSON.parse(await get(API + '/admin/accounts', { Authorization: 'Bearer ' + KEY }));
    await ev('(function(){ var W=window.__wb2api__; var real=' + JSON.stringify(real.accounts) + ';'
      + ' var t=JSON.parse(JSON.stringify(real[0]));'
      + ' t.quota=Object.assign({}, t.quota||{}, {has_data:false}); delete t.quota.remaining; t.credits=0;'
      + ' W.renderAccounts([t].concat(JSON.parse(JSON.stringify(real.slice(1)))));'
      + ' W.showPanelByKey("accounts"); })()');
    await sleep(900);
    await shot('t4t5-accounts-hasdata-false.png');

    const injTable = JSON.parse(await ev(
      'JSON.stringify(Array.prototype.map.call(document.querySelectorAll("#accts tr.acctrow"),'
      + ' function(tr){ var t=tr.querySelectorAll("td");'
      + '   return {provider:tr.dataset.acctof, quota:t[3].textContent.trim()}; }))'));
    console.log('  注入 has_data=false 后:');
    injTable.forEach(r => console.log('    ' + r.provider.padEnd(11) + ' 额度=' + r.quota));

    ws.close();
    console.log('\n=== 截图完成 ===');
  } catch (e) {
    console.log('EXCEPTION: ' + e.message);
    process.exitCode = 1;
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { ch.kill(); } catch { }
  }
  process.exit(process.exitCode || 0);
})();
