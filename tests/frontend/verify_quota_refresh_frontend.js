// verify_quota_refresh_frontend.js —— 浏览器实测：额度按钮真的打到 core 通用端点，
// 且 toast 显示的是**回执的真实计数**（不是"已提交"那句空话）。
//
// # 本 bug 的两个形态，各自需要什么判据
//
// 形态一（作用域错）：前端把 workbuddy 的私有端点 `/admin/credits/refresh`
//   当成全局额度刷新 → codearts 那一整片账号从来没被刷过，界面沉默。
//   判据：**按钮发出的请求 URL** 必须是 `/admin/accounts/quota/refresh`。
//
// 形态二（分支选错 → 静默走 else）：core 的回执没有 `results`，
//   而 startAll/accountAct 当时只认 `{result:{…}}`、`{claimed,…}`、`{results}`，
//   于是新回执落进兜底 else，toast 说"全部额度已提交" ——
//   按钮点了、有 toast、用户**看不到任何真实结果**。
//   判据：**toast 文案必须含回执里的真实计数**（已更新 N / providers 名），
//   且**不得**是兜底那句"已提交"。
//
// ⚠ 形态二的判据不能只看"有没有 toast" —— 旧代码**也有** toast（兜底那句）。
//   所以必须断言文案内容与后端回执**逐字段对齐**：
//   把后端同一端点直接跑一遍拿到 updated/providers，再要求 toast 里出现它们。
//   这是先例 verify_reload_frontend_provider.js 判据 5 的同款手法
//   （用后端真实回执当期望值，而不是前端自己猜的数字）。
//
// # 为什么要真浏览器（后端套件看不出来）
//
// 后端能证明端点对、回执对；前端完全可能仍然发旧 URL、或者拿到新回执
// 却走错分支。两者后端套件都绿。所以这里 hook fetch 抓**真实请求**。
//
// # 判据还要证明"两个上游都被刷到"
//
// 只断言 URL 对不够 —— 万一有人把端点改对了但后端只刷了一个上游呢？
// 所以判据 5 直接读浏览器拿到的**表格里的额度值**：
// 至少要有 codearts 的一个号显示出非 `—` 的额度，
// 且 providers 里同时含 codearts 与 workbuddy。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const CDP_PORT = 9321;
const PORT = Number(process.env.QUOTA_PORT || 18080);
const TARGET = process.env.QUOTA_URL || ('http://127.0.0.1:' + PORT + '/ui');
const PROFILE = path.join(os.tmpdir(), 'chrome-quota-refresh');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

// apiKey 从运行实例的配置里读（与 verify_reload_frontend_provider.js 同一手法：
// 期望值来自**后端**，不是前端猜的）。
function apiKey() {
  try { return JSON.parse(fs.readFileSync('D:\\tmp\\run-' + PORT + '.json', 'utf8')).api_key; }
  catch { return ''; }
}

// postJSON 直接打后端端点，拿"期望回执"。
function postJSON(p, body) {
  return new Promise(res => {
    const b = JSON.stringify(body || {});
    const q = http.request({ host: '127.0.0.1', port: PORT, path: p, method: 'POST',
      headers: { Authorization: 'Bearer ' + apiKey(), 'Content-Type': 'application/json',
                 'Content-Length': Buffer.byteLength(b) } },
      r => { let d = ''; r.on('data', c => d += c); r.on('end', () => { try { res(JSON.parse(d)); } catch { res(null); } }); });
    q.on('error', () => res(null)); q.write(b); q.end();
  });
}

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  // ⚠ 删 profile 目录在 Windows 上可能 EPERM：上一次运行若没杀干净 Chrome，
  // 它仍持有该目录里的文件（实测踩过：脚本直接崩在 rmSync 上，
  // 报错看起来像"环境问题"而不是"上次没清理"）。
  // 所以这里退一步：删不掉就**换一个全新的目录名**，并在最后 try 删。
  // 判据不受影响 —— profile 只是个一次性沙箱，不复用反而更干净。
  let profile = PROFILE;
  try {
    fs.rmSync(profile, { recursive: true, force: true });
  } catch (e) {
    profile = PROFILE + '-' + Date.now();
    console.log('  （旧 profile 被占用，改用新目录 ' + path.basename(profile) + '）');
  }

  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + CDP_PORT, '--user-data-dir=' + profile, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + CDP_PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    if (!t) { console.log('  FAIL 没能连上 CDP'); process.exit(1); }
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');

    await send('Page.navigate', { url: TARGET });
    await sleep(6000);

    // ---- fetch hook：抓真实请求的 URL + body ----
    await send('Runtime.evaluate', { expression: `(function(){
      window.__cap = [];
      var of = window.fetch;
      window.fetch = function(u, o){
        try {
          var s = (typeof u === 'string') ? u : ((u && u.url) || '');
          window.__cap.push({ url: s, body: (o && o.body) ? String(o.body) : null,
                              method: (o && o.method) || 'GET' });
        } catch(e){ window.__cap.push({ url: 'CAP_ERR' }); }
        return of.apply(this, arguments);
      };
    })()` });

    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails));
      return r.result && r.result.result ? r.result.result.value : undefined; };

    // 切到账号池面板
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('账号池') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2000);

    // ---------------------------------------------------------------- 页面前提
    // 顶部按钮与行内按钮都必须存在。不存在就别继续（否则后面是"没找到"的假红）。
    const hasTop = await ev(`!!document.getElementById('btnAllCredits')`);
    ok(hasTop, '页面上有顶部按钮 #btnAllCredits');
    const quotaBtns = JSON.parse(await ev(`JSON.stringify(Array.from(
      document.querySelectorAll('button[data-act="credits"]')).map(function(b){
        return { uid: b.dataset.uid, dayurl: b.dataset.dayurl, title: b.title, text: b.textContent.trim() }; }))`));
    console.log('  行内「额度」按钮: ' + JSON.stringify(quotaBtns, null, 0));
    ok(quotaBtns.length > 0, '账号表渲染出了行内「额度」按钮（' + quotaBtns.length + ' 个）');
    if (quotaBtns.length === 0 || !hasTop) { console.log('  前提不成立，终止'); process.exit(1); }

    // 从后端拿"期望回执"（用真后端当期望值来源，不用前端猜）
    const expect = await postJSON('/admin/accounts/quota/refresh', {});
    console.log('  后端 /admin/accounts/quota/refresh 回执: ' + JSON.stringify(
      expect && { updated: expect.updated, unknown: expect.unknown, failed: expect.failed,
                  skipped: expect.skipped, providers: expect.providers }));
    if (!expect || typeof expect.updated !== 'number') { console.log('  FAIL 后端回执不含 updated'); process.exit(1); }

    ok(quotaBtns[0].dayurl === '/admin/accounts/quota/refresh',
      '【判据 1】行内按钮的 data-dayurl 是 core 通用端点（实际 ' + JSON.stringify(quotaBtns[0].dayurl) + '）');
    ok(quotaBtns[0].dayurl.indexOf('/admin/credits/refresh') < 0,
      '【判据 1b】行内按钮**没有**引用 workbuddy 私有端点');
    // 语义变化必须写在 title 里（不许让用户自己发现"点了本行却整表刷新"）
    ok(/全部账号/.test(quotaBtns[0].title || ''),
      '【判据 1c】行内按钮 title 说明了作用域是全部账号（实际 ' + JSON.stringify(quotaBtns[0].title) + '）');

    // ---------------------------------------------------------------- 帮手
    async function clickAndCapture(expr) {
      await ev(`(function(){ window.__cap = []; })()`);
      await ev(expr);
      await sleep(4500);
      const cap = JSON.parse(await ev('JSON.stringify(window.__cap)'));
      const toastTxt = await ev(`(function(){ var t=document.getElementById('toast');
        return (t && !t.hidden) ? t.textContent : ''; })()`);
      return { cap, toastTxt };
    }

    // ---------------------------------------------------------------- 判据 2：顶部按钮
    const A = await clickAndCapture(`document.getElementById('btnAllCredits').click()`);
    const topReqs = A.cap.filter(c => c.url.indexOf('/quota/refresh') >= 0 || c.url.indexOf('/credits/refresh') >= 0);
    console.log('  顶部按钮发出的额度请求: ' + JSON.stringify(topReqs));
    ok(topReqs.length === 1, '【判据 2】点一次顶部按钮只发一次额度刷新请求（实际 ' + topReqs.length + '）');
    if (topReqs.length) {
      ok(topReqs[0].url === '/admin/accounts/quota/refresh',
        '【判据 2b】顶部按钮请求打到 core 通用端点（实际 ' + JSON.stringify(topReqs[0].url) + '）');
      ok(topReqs[0].method === 'POST', '【判据 2c】用的是 POST（实际 ' + topReqs[0].method + '）');
    }
    console.log('  顶部按钮 toast: ' + JSON.stringify(A.toastTxt));

    // 判据 3：toast 必须是**新渲染**，不能是兜底那句"已提交"。
    ok(A.toastTxt.indexOf('已更新 ' + expect.updated) >= 0,
      '【判据 3】toast 含后端回执的真实 updated=' + expect.updated + '（实际 ' + JSON.stringify(A.toastTxt) + '）');
    ok(A.toastTxt.indexOf('已提交') < 0,
      '【判据 3b】toast **不是**兜底那句"已提交" —— 那正是形态二（有 toast 但看不到真实结果）');
    // 两个上游的名字都要出现在 toast 里（proveders 来自回执）
    for (const p of (expect.providers || [])) {
      ok(A.toastTxt.indexOf(p) >= 0, '【判据 3c】toast 里列出了上游 ' + p);
    }
    ok((expect.providers || []).length >= 2,
      '【判据 4】后端本次刷新覆盖 ≥2 个上游（实际 ' + JSON.stringify(expect.providers) +
      '）—— 私有端点时代只有 workbuddy 一个');

    // ---------------------------------------------------------------- 判据 5：行内按钮
    const B = await clickAndCapture(`(function(){
      var b = document.querySelector('button[data-act="credits"]');
      if (b) b.click();
    })()`);
    const inReqs = B.cap.filter(c => c.url.indexOf('/quota/refresh') >= 0 || c.url.indexOf('/credits/refresh') >= 0);
    console.log('  行内按钮发出的额度请求: ' + JSON.stringify(inReqs));
    ok(inReqs.length === 1, '【判据 5】点一次行内按钮只发一次额度刷新请求（实际 ' + inReqs.length + '）');
    if (inReqs.length) {
      ok(inReqs[0].url === '/admin/accounts/quota/refresh',
        '【判据 5b】行内按钮请求打到 core 通用端点（实际 ' + JSON.stringify(inReqs[0].url) + '）');
    }
    console.log('  行内按钮 toast: ' + JSON.stringify(B.toastTxt));
    ok(B.toastTxt.indexOf('已更新 ' + expect.updated) >= 0,
      '【判据 6】行内按钮的 toast 也显示真实 updated（实际 ' + JSON.stringify(B.toastTxt) + '）');
    ok(B.toastTxt.indexOf('已提交') < 0, '【判据 6b】行内按钮的 toast 也不是兜底那句"已提交"');

    // ---------------------------------------------------------------- 判据 7：两个上游真的都被刷到
    // 读**界面上的额度单元格**：至少一个 codearts 的号必须显示非 `—` 的额度。
    // 这是"作用域真的修好了"的最终证据 —— 光看 URL 只能证明意图。
    await ev(`(function(){ if (document.getElementById('btnAllCredits')) document.getElementById('btnAllCredits').click(); })()`);
    await sleep(5000);
    const cells = JSON.parse(await ev(`(function(){
      var out = [];
      Array.from(document.querySelectorAll('tr[data-acctof]')).forEach(function(tr){
        var tds = tr.querySelectorAll('td');
        out.push({ provider: tr.dataset.acctof, uid: (tds[2] ? tds[2].textContent.trim() : ''),
                   quota: (tds[3] ? tds[3].textContent.trim() : '') });
      });
      return JSON.stringify(out);
    })()`));
    console.log('  账号表额度单元格: ' + JSON.stringify(cells));
    const ca = cells.filter(c => c.provider === 'codearts');
    const wb = cells.filter(c => c.provider === 'workbuddy');
    ok(ca.length > 0, '【判据 7】表里有 codearts 的账号（' + ca.length + ' 个）');
    ok(wb.length > 0, '【判据 7b】表里有 workbuddy 的账号（' + wb.length + ' 个）');
    ok(ca.some(c => c.quota && c.quota !== '—'),
      '【判据 7c】codearts 的额度**被刷新到了**（非 —）—— 私有端点时代它恒为 —。实际 ' +
      JSON.stringify(ca.map(c => c.quota)));
    ok(wb.some(c => c.quota && c.quota !== '—'),
      '【判据 7d】workbuddy 的额度也被刷新到了（非 —）。实际 ' + JSON.stringify(wb.map(c => c.quota)));

    // 归零还原：把 fetch 还回去，保证脚本可重复运行不影响页面
    await ev(`(function(){ window.__cap = []; })()`);
  } catch (e) {
    console.log('  FAIL 脚本异常: ' + (e && e.stack || e));
    fail++;
  } finally {
    try { ch.kill(); } catch { }
    // Chrome 的子进程可能在主进程退出后仍持有 profile 里的文件 ——
    // 稍等一下再删，删不掉就留着（不影响正确性，下次会换个目录名）。
    await sleep(800);
    try { fs.rmSync(profile, { recursive: true, force: true }); } catch { /* 被占用，留着 */ }
  }
  console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== 失败 ' + fail + ' 条 ===');
  process.exit(fail === 0 ? 0 : 1);
})();
