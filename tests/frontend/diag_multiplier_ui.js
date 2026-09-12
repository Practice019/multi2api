// diag_multiplier_ui.js —— 界面上到底显示了几个倍率？（不靠读代码推断）
//
// 我上一份诊断是从 API 数据推的（"32 个里只有 15 个能查表"）。
// 但**界面实际渲染成什么样**是另一回事 —— 必须真的打开看。
//
// 这符合本项目反复的教训：读代码推断 ≠ 实测渲染结果。
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9301;
const TARGET = process.env.INFO_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-mult');
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

    // 切到可用模型面板
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('可用模型') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2500);

    const info = JSON.parse(await ev(`JSON.stringify((function(){
      var chips = Array.from(document.querySelectorAll('#models .chip'));
      var withMult = [], withoutMult = [];
      chips.forEach(function(c){
        var txt = (c.textContent || '').trim();
        // ⚠ 倍率实际渲染成**小写 x**（形如 hy4-previewx0.29），不是全角 ×，
        // 也没有分隔空格 —— multTag 返回的 span 紧跟在模型名后面。
        // 我第一版查全角 ×，于是把 32 个**全判成"没有倍率"** —— 那是个假结论，
        // 差点让我去"修"一个本来正常的功能。
        // 判据改为：chip 里有 .mult 子元素（结构判据，不猜字符）。
        if (c.querySelector('.mult')) withMult.push(txt); else withoutMult.push(txt);
      });
      return {
        total: chips.length,
        withMultCount: withMult.length,
        withoutMultCount: withoutMult.length,
        withSample: withMult.slice(0, 8),
        withoutSample: withoutMult.slice(0, 8),
        mcount: (document.getElementById('mcount')||{}).textContent || '',
        // 倍率表加载状态
        multLoaded: (function(){
          try { return Object.keys(window.__wb2api__.modelMultipliers ? window.__wb2api__.modelMultipliers() : {}).length; }
          catch(e){ return 'n/a'; }
        })(),
      };
    })())`));

    console.log('=== 可用模型面板的**实际渲染** ===');
    console.log('  chip 总数        : ' + info.total);
    console.log('  带倍率的         : ' + info.withMultCount);
    console.log('  **不带倍率的**   : ' + info.withoutMultCount);
    console.log('  mcount 文案      : ' + JSON.stringify(info.mcount));
    console.log('');
    console.log('  带倍率示例: ' + JSON.stringify(info.withSample));
    console.log('  无倍率示例: ' + JSON.stringify(info.withoutSample));

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); }
  finally { try { ch.kill(); } catch { } }
  process.exit(0);
})();
