// diag_grouprow_ui.js —— 看分组行的**实际渲染**（T10 的视觉核验）。
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9305;
const TARGET = 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-grp');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
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
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(5000);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

    const out = await ev(`JSON.stringify(Array.from(document.querySelectorAll('#accts tr.grouprow')).map(function(tr){
      var td = tr.querySelector('td');
      return {
        text: (td.textContent || '').replace(/\\s+/g, ' ').trim(),
        buttons: Array.from(td.querySelectorAll('button')).map(function(b){
          return { txt: (b.textContent||'').trim(), title: b.title || '' };
        }),
        // 右对齐是否生效（动作区应贴在右侧）
        actRight: (function(){
          var a = td.querySelector('.gacts');
          if (!a) return null;
          var r = a.getBoundingClientRect(), tr2 = td.getBoundingClientRect();
          return Math.round(tr2.right - r.right);
        })(),
      };
    }))`);
    console.log('=== 分组行实际渲染 ===');
    JSON.parse(out).forEach(g => {
      console.log('  ' + JSON.stringify(g.text));
      g.buttons.forEach(b => console.log('      [btn] ' + b.txt + '   title=' + JSON.stringify(b.title.slice(0, 60))));
      console.log('      （动作区距单元格右缘 ' + g.actRight + 'px）');
    });
    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); }
  finally { try { ch.kill(); } catch { } }
  process.exit(0);
})();
