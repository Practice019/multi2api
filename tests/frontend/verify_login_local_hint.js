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

// 控制台身份令牌。取值顺序：URL 上的 `?token=`（run_e2e 传的就是它）→ 进程环境变量。
//
// # ⚠ 2026-10-19：这条探针此前**打错了页面**（诊断纠正）
//
// 原版把场景 URL 拼成裸的 `http://<host>:<port>/ui`，以为 `#loginLocalHint`
// 在**登录页**上。实测证否：
//
//	未认证 /ui      = 17864 字节，`loginLocalHint` **0** 处（那是登录页，只有 id="form"）
//	带 token 换手的 /ui = 539097 字节，`loginLocalHint` **2** 处
//
// 它其实在**控制台**的「添加账号」弹窗里（webui.html:1723 的
// `<p id="loginLocalHint">`，由 renderLoginLocalHint() 按 location.hostname 显隐，
// 见 5225-5252 与 renderLoginIntro 那一段）。浏览器停在登录页时那个元素**根本不存在**
// ⇒ 三条场景全报"页面上没有 #loginLocalHint 元素"（HEAD/基线同样红）。
//
// 所以正确做法是**两个条件同时满足**：
//   ① 带 `?token=` 进控制台（元素才在 DOM 里）
//   ② 用非本机 host 打开（location.hostname 才不是 127.0.0.1）
// 而 host 仍然只能靠 `--host-resolver-rules` 真解析（location.hostname 覆写不了，
// 那段实测记录见下面 scenarios 的注释）。
//
// 令牌与 lib_cdp.probeApiKey 同源：探针实例里 api_key == admin_token，
// 而 run_e2e 传进来的 URL 上就带着 `?token=`。
function resolveToken() {
  const u = process.env.HINT_URL;
  if (u) { const m = u.match(/[?&]token=([^&#]+)/); if (m) return decodeURIComponent(m[1]); }
  return process.env.WB2API_PROBE_TOKEN || process.env.WB2API_PROBE_KEY || '';
}
const TOKEN = resolveToken();
// withToken 给控制台 URL 挂上身份（与 lib_cdp.withToken 同一语义，本文件自带 CDP 引导段故就地实现）
const withToken = (u) => (TOKEN ? u + (u.indexOf('?') >= 0 ? '&' : '?') + 'token=' + encodeURIComponent(TOKEN) : u);
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
      // ⚠ 三个场景都必须带 ?token=（见文件顶部 resolveToken 那段）：
      //   `#loginLocalHint` 在**控制台**的添加账号弹窗里，不在登录页上。
      { host: '127.0.0.1', url: withToken('http://127.0.0.1:' + PORT + '/ui'),
        expectShown: false, why: '本机访问：没有回调问题' },
      { host: 'localhost', url: withToken('http://localhost:' + PORT + '/ui'),
        expectShown: false, why: '本机访问（等价写法）' },
      { host: 'gw.example.test', url: withToken('http://gw.example.test:' + PORT + '/ui'),
        resolve: true, expectShown: true, why: '域名：回调会打到用户自己那台' },
    ];
    if (!TOKEN) {
      console.log('  ⚠ 没有拿到令牌（HINT_URL 上没有 ?token=、也没有 WB2API_PROBE_TOKEN）' +
        ' ⇒ 浏览器会停在登录页，三条场景都会报"没有 #loginLocalHint 元素"');
    }

    for (const sc of scenarios) {
      // 每个场景一个独立浏览器（resolver 规则只能启动时给）
      ws.close();
      try { ch.kill(); } catch { }
      await sleep(600);

      const prof = PROFILE + '-' + sc.host.replace(/[^a-z0-9]/gi, '_');
      fs.rmSync(prof, { recursive: true, force: true });
      const args = ['--headless=new', '--disable-gpu', '--no-first-run',
        '--no-default-browser-check', '--remote-debugging-port=' + CDP,
        '--user-data-dir=' + prof, '--window-size=1440,1100',
        // ⚠ `--no-proxy-server` 是**必需**的，不是保险。
        //
        // 实测（诊断字段抓到的）：用 `--host-resolver-rules` 造非本机 host 时，
        // Chrome 报的是
        //    url=chrome-error://chromewebdata/  title="gw.example.test"
        //    正文 "该网页无法正常运作 … HTTP ERROR 503"
        // 而**同一时刻**用 curl 打同一个 Host（带不带端口、带不带 cookie）都是 302/200
        // —— 也就是说 503 **不是本服务返回的**。
        // 唯一解释：本机配了 HTTP 代理，Chrome 对"非本机 host"走代理，代理回 503；
        // 而 127.0.0.1 / localhost 走 bypass 列表所以前两条场景正常。
        // 加上 `--no-proxy-server` 后，假域名同样直连到 127.0.0.1（resolver 规则生效）。
        '--no-proxy-server'];
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
        if (!el) return {
          missing: true,
          // ⚠ 诊断字段：这一条红了必须能一眼看出"停在登录页"还是"压根没打开"。
          // 本探针吃过一次亏 —— 原版把三条场景都拼成裸 /ui，全都停在登录页，
          // 报出来只有一句"没有 #loginLocalHint 元素"，看不出真因。
          url: String(location.href), hostSeen: String(location.hostname),
          title: (document.title || ''),
          bytes: document.documentElement.outerHTML.length,
          isLoginPage: !!document.getElementById('form'),
          isConsole: !!document.getElementById('nav'),
          raw: (document.body ? document.body.textContent || '' : '').replace(/\\s+/g, ' ').slice(0, 120),
        };
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

      if (state.missing) {
        console.log('  ' + sc.host + ' 诊断: url=' + state.url + ' host=' + state.hostSeen +
          ' title=' + JSON.stringify(state.title) + ' bytes=' + state.bytes +
          ' 登录页=' + state.isLoginPage + ' 控制台=' + state.isConsole);
        console.log('    正文片段: ' + JSON.stringify(state.raw));
        ok(false, '页面上没有 #loginLocalHint 元素（host=' + sc.host + '，' +
          (state.isLoginPage ? '停在**登录页** —— 令牌没生效' :
            (state.isConsole ? '是控制台但元素缺失 —— 产品侧真的删掉了 #loginLocalHint' : '页面不是控制台也不是登录页')) + '）');
        ws2.close();
        try { br.kill(); } catch { }
        continue;
      }
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
