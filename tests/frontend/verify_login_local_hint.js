// verify_login_local_hint.js —— 验证"必须本机浏览器"的提示按实际 hostname 显隐。
//
// # 为什么需要这条
//
// codearts 用**本地回调服务器**（127.0.0.1:<随机端口>）接授权结果。
// 远程用户点完授权页的确认后，浏览器回调到**用户自己的 127.0.0.1**
// （那里没有回调服务器）→ 界面永远停在"等待授权"，**没有任何错误**。
//
// 这类"静默卡住"是本项目反复出现的问题形态：不报错，也不成功，用户只能猜。
//
// # 判据
//
// 提示必须**按地址栏的 hostname** 显隐，而不是恒显或恒隐：
//   localhost / 127.0.0.1 → 不显示（本机访问没有这个问题）
//   其它（域名/IP）       → 显示，且文案里带上实际的 host
//
// ⚠ 恒显也是错的：本机用户会看到一个与自己无关的警告，属于噪声。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const CDP = 9313;

// 端口：从 HINT_URL 推，或直接用 HINT_PORT，默认 18081。
//
// ⚠ 每个场景要用**不同的 host 拼 URL**（这正是被测变量），
// 所以这里只解析出 port，不缓存整个 URL。
function resolvePort() {
  const u = process.env.HINT_URL;
  if (u) { try { return Number(new URL(u).port || 80); } catch { } }
  return Number(process.env.HINT_PORT || 18081);
}
const PORT = resolvePort();
const PROFILE = path.join(os.tmpdir(), 'chrome-hint');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + CDP, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + CDP + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');

    // ---- 怎么造出"从别的 host 访问" ----
    //
    // ⚠ 我第一版想用 CDP 覆写 `location.hostname` —— **那行不通**。
    // 实测：`Object.defineProperty(window.location, 'hostname', ...)`
    // **静默失败**（不抛错，读回来仍是真实值）。Chrome 不允许改 location
    // 的属性。于是四种情形全都读到 `127.0.0.1`，测试报 6 项假失败。
    //
    // 正确做法：让浏览器**真的用别的 host 打开**。
    // 用 `--host-resolver-rules` 把一个假域名解析到 127.0.0.1 ——
    // 这样 `location.hostname` 真的是那个域名，被测代码读到的是真值。
    //
    // 每个场景要**重启浏览器**（resolver 规则是启动参数）。
    const scenarios = [
      { host: '127.0.0.1', url: 'http://127.0.0.1:' + PORT + '/ui',
        expectShown: false, why: '本机访问：没有回调问题' },
      { host: 'localhost', url: 'http://localhost:' + PORT + '/ui',
        expectShown: false, why: '本机访问（等价写法）' },
      { host: 'gw.example.test', url: 'http://gw.example.test:' + PORT + '/ui',
        resolve: true, expectShown: true, why: '域名：回调会打到用户自己那台' },
    ];

    for (const sc of scenarios) {
      // 每个场景一个独立浏览器（resolver 规则只能启动时给）
      ws.close();
      try { ch.kill(); } catch { }
      await sleep(600);

      const prof = PROFILE + '-' + sc.host.replace(/[^a-z0-9]/gi, '_');
      fs.rmSync(prof, { recursive: true, force: true });
      const args = ['--headless=new', '--disable-gpu', '--no-first-run',
        '--no-default-browser-check', '--remote-debugging-port=' + CDP,
        '--user-data-dir=' + prof, '--window-size=1440,1100'];
      if (sc.resolve) args.push('--host-resolver-rules=MAP ' + sc.host + ' 127.0.0.1');
      args.push('about:blank');
      const br = spawn(CHROME, args, { stdio: 'ignore' });

      let t2 = null;
      for (let i = 0; i < 40; i++) {
        try { const l = JSON.parse(await get('http://127.0.0.1:' + CDP + '/json/list'));
          t2 = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t2) break; } catch { }
        await sleep(250);
      }
      if (!t2) { ok(false, sc.host + '：浏览器起不来'); continue; }
      const ws2 = new WebSocket(t2.webSocketDebuggerUrl);
      await new Promise((r, j) => { ws2.onopen = r; ws2.onerror = j; });
      let id2 = 0; const pend2 = new Map();
      ws2.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend2.has(m.id)) { pend2.get(m.id)(m); pend2.delete(m.id); } };
      const send2 = (mm, p) => new Promise(r => { const i = ++id2; pend2.set(i, r); ws2.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
      await send2('Page.enable'); await send2('Runtime.enable');
      await send2('Page.navigate', { url: sc.url });
      await sleep(4500);
      const ev2 = async (x) => { const r = await send2('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

      const state = JSON.parse(await ev2(`JSON.stringify((function(){
        var el = document.getElementById('loginLocalHint');
        if (!el) return { missing: true };
        var seenBefore = String(location.hostname);
        if (typeof window.__wb2api__ !== 'undefined' && window.__wb2api__.renderLoginLocalHint) {
          window.__wb2api__.renderLoginLocalHint();
        }
        return {
          hostSeen: seenBefore,
          display: getComputedStyle(el).display,
          text: (el.textContent || '').slice(0, 80),
          hasHook: !!(window.__wb2api__ && window.__wb2api__.renderLoginLocalHint),
        };
      })())`));

      if (state.missing) { ok(false, '页面上没有 #loginLocalHint 元素'); continue; }
      ok(state.hostSeen === sc.host,
        '浏览器确实用 ' + sc.host + ' 打开了（实际 ' + state.hostSeen + '）—— ' +
        '这条先确认，否则下面的显隐断言测的是同一个 host');
      ok(state.hasHook, 'renderLoginLocalHint 通过 __wb2api__ 暴露（否则这条分支没法人造触发）');

      const shown = state.display !== 'none';
      console.log('  host=' + sc.host.padEnd(18) + ' display=' + state.display.padEnd(6) +
                  ' 文本=' + JSON.stringify(state.text.slice(0, 46)));
      ok(shown === sc.expectShown,
        'host=' + sc.host + ' 时提示' + (sc.expectShown ? '应显示' : '应隐藏') +
        '（实际 ' + (shown ? '显示' : '隐藏') + '）—— ' + sc.why);

      if (sc.expectShown) {
        ok(state.text.indexOf(sc.host) >= 0,
          '提示文案里带上了实际 host（' + sc.host + '）—— 用户才知道问题出在哪台机器');
        ok(/127\.0\.0\.1/.test(state.text),
          '提示说明了回调会打到 127.0.0.1 —— 这是让用户能自己判断的关键信息');
      }
      ws2.close();
      try { br.kill(); } catch { }
      await sleep(400);
    }
    ch.kill();

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 本机提示验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
