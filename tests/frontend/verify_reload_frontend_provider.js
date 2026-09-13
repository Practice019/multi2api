// verify_reload_frontend_provider.js —— T3 浏览器验证：前端真的把 provider 传进请求体。
//
// # 为什么必须浏览器实测（脚本 4/5 正是 verify_reload_by_upstream.js 缺的）
//
// 后端脚本只能证明"**后端**按 provider 分派了"。前端完全可能：
//   · 仍然发 `{}`            → 后端走回落，功能没通（本 T3 修的就是它）
//   · 发了 provider 但文案用自己猜的 pid → 界面继续撒谎
// 两者后端套件都看不出来，因为后端收到的请求一切都对。
//
// 所以这里 hook `fetch`，抓**真实请求体**（先例：verify_model_provider_filter.js）。
//
// # 判据（都是"错误实现必然失败"的，不是"看起来对"）
//
// 4. 点 codearts 行的「重载 auths」→ 捕获的请求体含 provider:"codearts"
//    （改动前这里是 `{}` → 必然 FAIL）
// 5. toast 文案里的上游 == **回执**的 provider
// 6. **鉴别力判据**：codearts 的 `before/after` 变化与 workbuddy 不同
//    （见下，用来补上"两目录文件数相同"导致的判据 1 蒙对问题）
// 7. ghost（不存在的上游）→ 404 能**报出来**，不被吞掉
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const CDP_PORT = 9311;
const TARGET = process.env.RELOAD_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-t3-reload');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + CDP_PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + CDP_PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');

    // 先装 hook 再进页面是做不到的（页面自带的 fetch 立刻可用），
    // 所以用 evaluateOnNewDocument 式的做法：先 navigate 到 about:blank 装钩子不行
    // —— 钩子要装在**目标页的 window** 上。这里退一步：navigate 后立刻注入，
    // 注入点远早于任何用户点击（点击由本脚本在注入之后发起）。
    await send('Page.navigate', { url: TARGET });
    await sleep(5000);

    // ---- 装 fetch hook：记录 path + 原始 body ----
    await send('Runtime.evaluate', { expression: `(function(){
      window.__cap = [];
      var of = window.fetch;
      window.fetch = function(u, o){
        try {
          window.__cap.push({ url: String(u), body: (o && o.body) ? String(o.body) : null,
                              method: (o && o.method) || 'GET' });
        } catch(e){ window.__cap.push({ url: String(u), body: 'CAP_ERR' }); }
        return of.apply(this, arguments);
      };
    })()` });

    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails));
      return r.result && r.result.result ? r.result.result.value : undefined; };

    // 切到账号池
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('账号池') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2000);

    const groups = JSON.parse(await ev(`JSON.stringify(Array.from(
      document.querySelectorAll('button[data-greload]')).map(function(b){
        return { provider: b.dataset.greload,
                 text: b.textContent.trim() }; }))`));
    console.log('  页面上的「重载 auths」按钮: ' + JSON.stringify(groups));
    ok(groups.length > 0, '账号池面板渲染出了 data-greload 按钮（' + groups.length + ' 个）');

    // ---------------------------------------------------------------- 帮手
    // clickReload 点某个上游的重载按钮，返回捕获到的请求与 toast 文案。
    async function clickReload(pid) {
      await ev(`(function(){ window.__cap = []; })()`);
      await ev(`(function(){
        var b = Array.from(document.querySelectorAll('button[data-greload]'))
          .filter(function(x){ return x.dataset.greload === ${JSON.stringify(pid)}; })[0];
        if (!b) throw new Error('no button for ' + ${JSON.stringify(pid)});
        b.click();
      })()`);
      await sleep(3500);
      const cap = JSON.parse(await ev('JSON.stringify(window.__cap)'));
      const toastTxt = await ev(`(function(){ var t=document.getElementById('toast');
        return (t && !t.hidden) ? t.textContent : ''; })()`);
      return { cap, toastTxt };
    }

    // ---------------------------------------------------------------- 判据 4
    const A = await clickReload('codearts');
    const reloadReqs = A.cap.filter(c => c.url.indexOf('/admin/accounts/reload') >= 0);
    console.log('  捕获到的 /admin/accounts/reload 请求: ' + JSON.stringify(reloadReqs));
    ok(reloadReqs.length === 1, '点一次「重载 auths」只发一次 reload 请求（实际 ' + reloadReqs.length + '）');

    const rq = reloadReqs[0];
    let bodyObj = null;
    try { bodyObj = JSON.parse(rq.body); } catch { }
    ok(!!bodyObj && bodyObj.provider === 'codearts',
      '【判据 4】请求体含 provider:"codearts"（实际 ' + JSON.stringify(rq.body) + '）');

    // ---------------------------------------------------------------- 判据 5
    console.log('  toast: ' + JSON.stringify(A.toastTxt));
    // 回执的 provider 由后端决定；前端若继续用自己猜的 pid，在"传对了"的场景下
    // 两者相同 → 看不出差别。所以判据 5 必须**绕过前端变量**：
    // 直接比较 toast 里出现的上游名与后端该次请求真实回执的 provider。
    const backend = await new Promise(res => {
      const key = JSON.parse(fs.readFileSync('D:\\tmp\\run-18080.json', 'utf8')).api_key;
      const b = JSON.stringify({ provider: 'codearts' });
      const q = http.request({ host: '127.0.0.1', port: 18080, path: '/admin/accounts/reload', method: 'POST',
        headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json',
                   'Content-Length': Buffer.byteLength(b) } },
        r => { let d = ''; r.on('data', c => d += c); r.on('end', () => { try { res(JSON.parse(d)); } catch { res(null); } }); });
      q.on('error', () => res(null)); q.write(b); q.end();
    });
    ok(!!backend && A.toastTxt.indexOf('已重载 ' + backend.provider + ' 的 auths') >= 0,
      '【判据 5】toast 用的是回执的 provider（期望含 "已重载 ' + (backend && backend.provider) + ' 的 auths"）');

    // 文案里不该出现"对齐 …"那套旧措辞（说明确实改了，不是恰好碰对）
    ok(A.toastTxt.indexOf('对齐') < 0, 'toast 已不含旧的"（对齐 X）"措辞');

    // ---------------------------------------------------------------- 判据 4b
    const W = await clickReload('workbuddy');
    const wr = W.cap.filter(c => c.url.indexOf('/admin/accounts/reload') >= 0)[0];
    let wBody = null; try { wBody = JSON.parse(wr.body); } catch { }
    ok(!!wBody && wBody.provider === 'workbuddy',
      '【判据 4b】点 workbuddy 行时请求体含 provider:"workbuddy"（实际 ' + JSON.stringify(wr.body) + '）');
    console.log('  workbuddy toast: ' + JSON.stringify(W.toastTxt));

    // ---------------------------------------------------------------- 判据 6（鉴别力）
    // ⚠ 后端脚本的前提检查已报出：两目录文件数**相同（3 vs 3）**，
    // 所以"scanned 不同"那条判据没有鉴别力 —— 恒用一个目录的实现会蒙对。
    //
    // 我第一版判据 6 想拿"两个上游重载后的池大小不同"来补鉴别力，
    // 理由是"codearts 的账号数为 0"。**实测这条 FAIL 了**，而且是**我的假设错**，
    // 不是功能坏 —— 隔离复现（probe_synctodirfor_control.js）证明：
    //   · provider=codearts → dir=auths\codearts   scanned=3  after=3
    //   · 不传 provider     → dir=auths\workbuddy  scanned=3  after=3
    //   · provider=workbuddy→ dir=auths\workbuddy  scanned=3  after=3
    // 即 **两个上游各自都有 3 个凭证**，对齐后池大小天然相同。
    // 顺带纠正我先前的另一个误读：data/state.json 只落了 3 条、且没有 provider 字段，
    // 但内存池确实有 codearts 域（SyncToDirFor 按 providerOf(e) 分域，
    // 剔除范围严格限制在本上游内 —— 见 pool.go 的方法注释）。
    // **不要为了让这条变绿去改测试**：判据本身没有鉴别力就该换成有鉴别力的。
    //
    // 真正有鉴别力的判据是 `dir`：它是后端**实际扫描的目录**的回执，
    // 不受"两目录内容恰好相同"影响。再叠一条请求体 ↔ 回执目录的**交叉核对**，
    // 它把"前端传的 provider"与"后端扫的目录"直接拴在一起 ——
    // 前端发 `{}` 或发错上游都会让它 FAIL。
    const dirRe = pid => new Promise(res => {
      const key = JSON.parse(fs.readFileSync('D:\\tmp\\run-18080.json', 'utf8')).api_key;
      const b = JSON.stringify({ provider: pid });
      const q = http.request({ host: '127.0.0.1', port: 18080, path: '/admin/accounts/reload', method: 'POST',
        headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json',
                   'Content-Length': Buffer.byteLength(b) } },
        r => { let d = ''; r.on('data', c => d += c); r.on('end', () => { try { res(JSON.parse(d)); } catch { res(null); } }); });
      q.on('error', () => res(null)); q.write(b); q.end();
    });
    const dC = await dirRe('codearts'), dW = await dirRe('workbuddy');
    ok(!!dC && dC.dir !== dW.dir,
      '【判据 6a · 鉴别力】两个上游扫描的**目录**不同（' + (dC && dC.dir) + ' vs ' + (dW && dW.dir) +
      '）—— 这条不受"两目录文件数相同"影响，恒用一个目录的实现必然 FAIL');

    // 交叉核对：**前端捕获的请求体**里的 provider → 后端应为它扫出对应的目录
    const capPid = bodyObj && bodyObj.provider;
    const crossed = await dirRe(capPid);
    ok(!!crossed && new RegExp(capPid).test(crossed.dir),
      '【判据 6b · 交叉核对】前端发的 provider("' + capPid + '")→ 后端扫的目录 = "' +
      (crossed && crossed.dir) + '"（两者必须对应）');

    // ---------------------------------------------------------------- 判据 7
    // ghost：页面上没有这个按钮，所以直接改一个真实按钮的 dataset 再点，
    // 模拟"上游不存在"（等价于后端收到的 provider 查不到）。
    await ev(`(function(){ window.__cap = []; })()`);
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('button[data-greload]'))[0];
      b.dataset.greload = 'ghost-upstream';
      b.click();
    })()`);
    await sleep(3000);
    const ghostToast = await ev(`(function(){ var t=document.getElementById('toast');
      return (t && !t.hidden) ? t.textContent : ''; })()`);
    const ghostCap = JSON.parse(await ev('JSON.stringify(window.__cap)'))
      .filter(c => c.url.indexOf('/admin/accounts/reload') >= 0);
    console.log('  ghost 请求体: ' + JSON.stringify(ghostCap[0] && ghostCap[0].body));
    console.log('  ghost toast : ' + JSON.stringify(ghostToast));
    ok(ghostToast.indexOf('失败') >= 0 && /404|上游不存在/.test(ghostToast),
      '【判据 7】不存在的上游：404 **被报出来**，没被吞掉（toast=' + JSON.stringify(ghostToast) + '）');

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + (e && e.message)); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== T3 前端传参验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
