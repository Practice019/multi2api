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
// 探针自带夹具密钥：T5/T7 起产品不再把凭据注入页面（那是安全改造的目标之一），
// 页内 fetch('/v1/models') 必须由探针自己带 key —— 见 lib_cdp.injectProbeKey。
const { injectProbeKey: injectKey, pasteConsoleKey, consoleHtmlAuthed } = require('./lib_cdp.js');
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
  // ⚠ 必须取**已认证**的控制台源码：T5 起未认证的 /ui 回 302 到登录页，而裸
  // `http.get` 不跟重定向 ⇒ 静态断言会因拿到登录页而全红，反向断言（`!/alert\(/`）
  // 还会**空洞通过**（空字符串里当然没有 alert）。见 lib_cdp.consoleHtmlAuthed。
  const html = await consoleHtmlAuthed(BASE);
  const code = stripComments(html);
  // 来源自检：拿到的必须真是控制台（而不是登录页/空体），否则后面的结论都不算数。
  ok(/id="infoModal"/.test(code) || /function showInfo/.test(code),
    '静态检查的来源是控制台本体（不是登录页/空体，长度 ' + code.length + '）');
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
    // ⚠ 注入必须在新文档建立**之后**（紧跟 navigate 的赋值会被随后的导航冲掉）。
    // 页内 fetch 用的密钥是**探针自己的夹具**：T5/T7 起产品不再注入凭据，见 lib_cdp.injectProbeKey。
    if (!(await injectKey(send, undefined, BASE))) {
      console.error('  ⚠ 环境没给探针密钥（WB2API_PROBE_KEY / WB2API_PROBE_TOKEN）⇒ 页内 /v1/models 是空目录，断言会假红');
    }

    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return 'EXC';
      return r.result && r.result.result ? r.result.result.value : undefined;
    };
    // 像人一样把密钥粘进顶栏并点「应用」。
    // ⚠ 必须做：T5/T7 起控制台不再由服务端注入 api_key（安全改造的目标之一），
    // 于是依赖 /v1 的面板（模型目录 / 对话测试 / 倍率预览）默认是空的 —— 不粘就
    // 断「面板有内容」会假红，而那个红是**契约变了**，不是产品坏了。
    // 见 lib_cdp.pasteConsoleKey。
    const pasted = await pasteConsoleKey(ev, undefined, BASE);
    if (pasted !== 'ok') console.error('  ⚠ 粘密钥失败（' + pasted + '）⇒ 下面依赖 /v1 的断言会假红');

    // ⚠ 装旅行夹具**必须在第一次打开「猫猫旅行」之前**。
    //
    // 为什么需要夹具：本探针要的是"点详情 → 弹窗里出现可读字段"这条**渲染**
    // 判据，而 18080 合成实例里 `/admin/travel` 回 `{"accounts":[]}`（没有 workbuddy 凭证）
    // ⇒ 连一个「详情」按钮都渲染不出来。
    // 夹具只替换 `/admin/travel` 与 `/admin/travel/status` 两条回执，
    // renderTravel / travelAct / showInfo / 事件委托 **全部真跑**（见 lib_fixtures 顶部）。
    const fixtures = require('./lib_fixtures.js');
    const fr = await fixtures.fixTravel(ev);
    console.log('  (' + await fixtures.fixGrowth(ev) + ' / ' + fr + ')');
    if (!String(fr).startsWith('installed') && !String(fr).startsWith('already')) {
      console.error('  ⚠ 旅行夹具没装上（' + fr + '）⇒ 下面会报"没有详情按钮"，那是夹具问题');
    }

    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('猫猫旅行') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(1500);
    // ⚠ 夹具装上后还要**点一次面板自己的「刷新」**：loadTravel 的非刷新路径走
    //   `sharedGet('ep:/admin/travel')`（30 秒 TTL），而夹具是页面加载**之后**才装的
    //   —— 首次回源可能已把空结果写进缓存，于是"装了夹具面板还是空"。
    //   「刷新」走 `?refresh=1`（直连、绕开缓存），正是用户看到空面板时会做的操作。
    console.log('      夹具+刷新: ' + await fixtures.prepare(ev, 'travel',
      `document.querySelectorAll('#travelRows tr').length`, 1));
    await sleep(800);

    console.log('\n[2] 点「详情」应弹出 infoModal');
    const hasBtn = await ev(`document.querySelectorAll('#travelRows button[data-tact="detail"]').length`);
    ok(hasBtn > 0, '存在「详情」按钮（' + hasBtn + ' 个）');

    // ⚠ 这条原来叫"至少有一个账号能返回旅行数据"，做法是**逐个 uid 打真后端**
    //   （同步 XHR 探 200）。夹具装上以后那条探测本身就是错的：它打的是真后端，
    //   而真后端对这个实例没有旅行数据（404）—— 于是"夹具装了却探测说没有"。
    //   判据的意图是"待会儿要点的那个账号，它的 status 端点会返回可渲染的数据"；
    //   在夹具模式下这由夹具自己保证，所以改成断言**夹具行渲染出来了**（结构），
    //   并且把"端点真的返回了数据"交给下面那条弹窗内容断言去证（那是端到端的）。
    const rendered = JSON.parse(await ev(`JSON.stringify((function(){
      return Array.from(document.querySelectorAll('#travelRows tr')).map(function(tr){
        var b = tr.querySelector('button[data-tact="detail"]');
        return { uid: b ? b.dataset.uid : null, cells: tr.children.length };
      });
    })())`));
    console.log('      旅行面板渲染出的行: ' + JSON.stringify(rendered.map(r => r.uid)));
    ok(rendered.length > 0 && rendered.every(r => r.uid),
      '旅行面板渲染出带 uid 的夹具行（' + rendered.length + ' 行）—— 夹具生效');

    // 点第一行的详情按钮
    const clicked = await ev(`(function(){
      var b = document.querySelector('#travelRows button[data-tact="detail"]');
      if (!b) return false;
      b.click();
      return b.dataset.uid;
    })()`);
    ok(clicked !== false, '点击了「详情」（uid=' + String(clicked) + '）');
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
