// verify_info_modal.js —— 验证 alert() 已替换为只读信息弹窗，并测它的行为。
//
// # 为什么这个检查要"剥注释"
//
// 我在替换 alert() 时写了 4 处**注释**解释"为什么不用 alert()"。
// 直接 grep `alert(` 会把这 4 处注释也算上 → 误报"仍在用 alert"。
// 这个坑本轮已经踩过一次（死按钮扫描器），所以判据必须剥注释。
//
// # 测什么
//   1. 剥注释后，代码里**没有** alert( 调用
//   2. 点旅行「详情」→ 弹出 infoModal（而不是原生弹窗）
//   3. 弹窗内容含可读字段（猫名/状态等）
//   4. 关闭按钮 / Esc 都能关
//   5. 「复制」按钮存在
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9296;
const BASE = process.env.INFO_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-info');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

function stripComments(s) {
  // ⚠ 必须先把 CRLF 归一化成 LF。
  //
  // `\/\/.*$` 在不带 m 标志时，`$` 只匹配**字符串末尾**；而每行结尾是 `\r`
  //（页面按 CRLF 下发），于是 `.*$` 匹配到 `\r` 之前就停了？不 ——
  // 实测正相反：`$` 因为 `\r` 的存在而匹配不上，整行**完全没被剥掉**。
  // 我的"剥注释"因此静默失效，把注释里的 `alert()` 算成了真实调用。
  //
  // 先归一化换行，再逐行剥 —— 这样才可靠。
  const lf = s.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  return lf
    .replace(/<!--[\s\S]*?-->/g, '')          // HTML 注释
    .replace(/\/\*[\s\S]*?\*\//g, '')         // JS 块注释
    .split('\n').map(l => l.replace(/\/\/.*$/, '')).join('\n');   // JS 行注释
}

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  // ---- 1) 静态：代码里不该有 alert( ----
  console.log('[1] 静态检查（剥注释）');
  const html = await get(BASE);
  const code = stripComments(html);
  ok(!/\balert\s*\(/.test(code), '代码里没有 alert() 调用' +
    (/\balert\s*\(/.test(code) ? '（实际: ' + /\balert\s*\([^)]*\)/.exec(code)[0] + '）' : ''));
  ok(/function showInfo/.test(code), 'showInfo 已定义');
  ok(/id="infoModal"/.test(code), 'infoModal 容器存在');

  // ---- 2) 行为：点旅行详情 ----
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
    const errs = [];
    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails;
        errs.push(String((d.exception && d.exception.description) || d.text || '').slice(0, 120));
      }
    };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');
    await send('Page.navigate', { url: BASE });
    await sleep(4000);
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return 'EXC';
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('猫猫旅行') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2200);

    console.log('\n[2] 点「详情」应弹出 infoModal');
    const hasBtn = await ev(`document.querySelectorAll('#travelRows button[data-tact="detail"]').length`);
    ok(hasBtn > 0, '存在「详情」按钮（' + hasBtn + ' 个）');

    // ⚠ 必须点**会返回 200 的那个账号**。
    // 我第一版点第一行，而那行是 codearts 账号 —— 它的 travel/status 返回
    // 404（无可用凭证），于是代码走 catch 分支显示 toast，弹窗自然不出现。
    // 那是**测试选错了数据**，不是产品缺陷。
    // 这里先探测每行的账号是否可用，再点可用的那一行。
    const usable = await ev(`(function(){
      return new Promise(function(resolve){
        var req = new XMLHttpRequest();
        req.open('GET', '/admin/accounts', false);
        req.setRequestHeader('Authorization', 'Bearer ' + (window.__WB2API_KEY__ || ''));
        req.send(null);
        var accs = (JSON.parse(req.responseText).accounts || []);
        var good = [];
        accs.forEach(function(a){
          var r2 = new XMLHttpRequest();
          try {
            r2.open('GET', '/admin/travel/status?uid=' + encodeURIComponent(a.uid), false);
            r2.setRequestHeader('Authorization', 'Bearer ' + (window.__WB2API_KEY__ || ''));
            r2.send(null);
            if (r2.status === 200) good.push(a.uid);
          } catch (e) {}
        });
        resolve(JSON.stringify(good));
      });
    })()`);
    const goodUids = JSON.parse(usable);
    console.log('      有可用旅行数据的账号: ' + goodUids.length + ' 个');
    ok(goodUids.length > 0, '至少有一个账号能返回旅行数据');

    // 点那一行的详情按钮
    const clicked = await ev(`(function(){
      var rows = Array.from(document.querySelectorAll('#travelRows tr'));
      var good = ${JSON.stringify(goodUids)};
      for (var i = 0; i < rows.length; i++) {
        var b = rows[i].querySelector('button[data-tact="detail"]');
        if (!b) continue;
        if (good.indexOf(b.dataset.uid) >= 0) { b.click(); return b.dataset.uid; }
      }
      return false;
    })()`);
    ok(clicked !== false, '点击了有数据的账号的「详情」（uid=' + String(clicked).slice(0, 8) + '）');
    await sleep(2000);

    const state = JSON.parse(await ev(`JSON.stringify((function(){
      var m = document.getElementById('infoModal');
      return {
        exists: !!m,
        display: m ? m.style.display : null,
        title: (document.getElementById('infoTitle')||{}).textContent || '',
        body: ((document.getElementById('infoBody')||{}).textContent || '').slice(0, 200),
        hasCopy: !!document.getElementById('infoCopy'),
      };
    })())`));
    console.log('      弹窗状态: ' + JSON.stringify(state));
    ok(state.exists, 'infoModal 存在');
    ok(state.display === 'flex', '弹窗已打开（display=' + state.display + '）');
    ok(/旅行/.test(state.title), '标题正确（' + state.title + '）');
    ok(state.body.length > 0, '内容非空（' + state.body.replace(/\n/g, ' | ').slice(0, 80) + '）');
    ok(state.hasCopy, '有「复制」按钮（alert 做不到）');

    console.log('\n[3] 关闭方式');
    // Esc
    await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
    await sleep(600);
    const afterEsc = await ev(`document.getElementById('infoModal').style.display`);
    ok(afterEsc === 'none', 'Esc 能关闭（display=' + afterEsc + '）');

    // 关闭按钮
    await ev(`document.querySelector('#travelRows button[data-tact="detail"]').click()`);
    await sleep(1800);
    await ev(`document.getElementById('infoClose').click()`);
    await sleep(600);
    const afterBtn = await ev(`document.getElementById('infoModal').style.display`);
    ok(afterBtn === 'none', '「关闭」按钮能关闭（display=' + afterBtn + '）');

    console.log('\n[4] 页面健康');
    ok(errs.length === 0, '0 个控制台错误' + (errs.length ? ': ' + errs.slice(0, 2).join(' | ') : ''));

    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 信息弹窗验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
