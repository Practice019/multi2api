// verify_all_buttons_e2e.js —— 逐个上游点「刷新本上游额度」+ 点「全部签到」的真前端 e2e。
//
// # 为什么必须逐个上游点（用户明确要求）
//
// 用户报的缺陷：点分组行的「刷新本上游额度」弹出
//
//	刷新本上游额度失败：refreshAccounts is not defined
//
// 那是"调用点引用了从未定义的函数"。**静态断言抓不到**：
// 文本里确实有 `refreshAccounts` 这个标识符，grep 得到、正则也匹配得上，
// 只有**真点一次**才会在运行时抛 ReferenceError。
//
// 而且它不是"某一个上游坏了"—— 所有上游的该按钮走的是**同一段代码**，
// 所以理论上全都坏。但用户要求"每个上游都测一遍"，理由充分：
// 这条路径依赖 manifest 的 provider 列表、分组渲染、事件委托三处，
// 任一处对某个上游不成立就可能只有它坏（例如 0 账号的上游、
// 未在 manifest 注册的历史账号分组）。
//
// # 判据是"页面有没有报错"，不是"按钮存不存在"
//
// 本探针同时收集：
//   · window.onerror / unhandledrejection（未捕获异常）
//   · console.error（busyRun 的失败提示走 toast，但未捕获异常走 console）
//   · toast 文案（本仓的失败提示是 toast(`${label}失败：${e.message}`)）
//
// 只要有**任何**一条出现 `is not defined` / `ReferenceError` / `失败`，
// 本套件就红。
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const REPO = path.resolve(__dirname, '..', '..');
const EXE = path.join(REPO, 'bin', 'wb2api-server.exe');

// ⚠ 临时目录 + 空闲端口。仓库里已有的 config.json（7863）/
// config-test-port7864.json（7864）可能是用户在用的，绝不碰。
const PORT = Number(process.env.E2E_PORT || 7874);
const TMP = path.join(os.tmpdir(), 'wb2api-allbtn-' + PORT + '-' + process.pid);
const PROFILE = path.join(os.tmpdir(), 'chrome-allbtn-' + Date.now() + '-' + process.pid);
const CDP = Number(process.env.E2E_CDP || 9364);
const KEY = 'allbtn-key-' + PORT;
const BASE = 'http://127.0.0.1:' + PORT;

const sleep = ms => new Promise(r => setTimeout(r, ms));
const get = u => new Promise((res, rej) => http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej));

// 手造 manifest：**每个上游都有额度能力 + 一个签到动作**，
// 另加一个"0 账号"的上游与一个"没有 quota 能力"的上游 —— 三种形态都要覆盖。
const PROVIDERS = [
  { id: 'alpha', account_count: 2, capabilities: ['chat', 'quota-probe'] },
  { id: 'beta', account_count: 1, capabilities: ['chat', 'quota-probe'] },
  { id: 'gamma', account_count: 0, capabilities: ['chat', 'quota-probe'] }, // 0 账号
  { id: 'delta', account_count: 1, capabilities: ['chat'] },                // 无 quota 能力
];
const MANIFEST = {
  service: 'e2e',
  providers: PROVIDERS.map(p => Object.assign({
    default: p.id === 'alpha',
    accounts_columns: ['provider', 'nickname', 'uid', 'quota', 'ops'],
    login: { kind: 'oauth', label: '添加账号' },
  }, p)),
  capabilities: [{ id: 'chat', title: '对话' }, { id: 'quota-probe', title: '额度' }],
  admin_routes: [],
  jobs: [],
  daily_actions: PROVIDERS.map(p => ({
    provider: p.id, id: p.id + '-checkin', label: '签到', title: p.id + ' 签到',
    one_url: '/admin/' + p.id + '/checkin', all_url: '/admin/' + p.id + '/checkin/all', batch: true,
  })),
};

const ACCOUNTS = {
  accounts: [
    { uid: 'u-alpha-1', provider: 'alpha', nickname: '甲一', disabled: false, has_token: true, quota: 100 },
    { uid: 'u-alpha-2', provider: 'alpha', nickname: '甲二', disabled: false, has_token: true, quota: 200 },
    { uid: 'u-beta-1', provider: 'beta', nickname: '乙一', disabled: false, has_token: true, quota: 300 },
    { uid: 'u-delta-1', provider: 'delta', nickname: '丁一', disabled: false, has_token: true, quota: 400 },
  ],
};

fs.mkdirSync(TMP, { recursive: true });
fs.mkdirSync(path.join(TMP, 'auths'), { recursive: true });
fs.mkdirSync(path.join(TMP, 'data'), { recursive: true });
fs.writeFileSync(path.join(TMP, 'config.json'), JSON.stringify({
  listen: '127.0.0.1:' + PORT,
  api_key: KEY,
  auth_dir: './auths',
  admin: { checkin_log_path: './data/checkin-log.json', client_login_enabled: false },
  login: { open_browser: false, isolated: true },
}, null, 2));

let srv = null, ch = null, ws = null, fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  try {
    srv = spawn(EXE, ['-config', 'config.json'], { cwd: TMP, stdio: 'ignore' });
    let up = false;
    for (let i = 0; i < 40; i++) {
      try { await get(BASE + '/healthz'); up = true; break; } catch { }
      await sleep(250);
    }
    if (!up) throw new Error('实例未在 ' + BASE + ' 起来');
    console.log('  实例已起: ' + BASE + '（pid=' + srv.pid + '）');

    ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
      '--remote-debugging-port=' + CDP, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'],
      { stdio: 'ignore' });
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + CDP + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');

    ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');

    // ⚠⚠ 两个坑（都是实测踩出来的）：
    //
    //  1. toast **不是全局函数**（它在 webui.html 的 IIFE 里），所以
    //     **不能** patch window.toast —— 我第一版就那么写，结果收集到的
    //     提示恒为空，探针"看起来在测提示"其实什么都没测到。
    //     唯一可靠的判据是读**真实渲染结果**：toast() 把文案写进 #toast
    //     节点（见 webui.html 的 toast 实现），直接读那个节点。
    //     与本仓"断言要落在可观测量上"同一条：DOM 是事实，函数是形式。
    //
    //  2. `admin()` 走的是 **r.text() + JSON.parse**（不是 r.json()）。
    //     mock 只给 json() 而不给 text() 时，解析结果是 null，
    //     界面会报 "Cannot read properties of null" —— 那是**假失败**，
    //     与真实缺陷混在一起就分不清了。所以 text() 必须回真 JSON 文本。
    //
    // （⚠ 注释里不能出现反引号 —— 它在模板串内部，会提前终止字符串。）
    const pre = `
      window.__ERRS__ = [];
      window.addEventListener('error', function(e){ window.__ERRS__.push('error: ' + (e.message||'')); });
      window.addEventListener('unhandledrejection', function(e){
        window.__ERRS__.push('rejection: ' + ((e.reason && e.reason.message) || String(e.reason)));
      });
      var __ce = console.error;
      console.error = function(){ window.__ERRS__.push('console.error: ' + Array.prototype.join.call(arguments,' ')); return __ce.apply(console, arguments); };
      window.__MANIFEST__ = ${JSON.stringify(MANIFEST)};
      window.__ACCOUNTS__ = ${JSON.stringify(ACCOUNTS)};
      window.__REQS__ = [];
      // 读 #toast 的当前文案（hidden 时也算 —— 我们只关心它写过什么）
      window.__TOAST_TEXT__ = function(){
        var el = document.getElementById('toast');
        if (!el) return '';
        var t = (el.textContent || '').trim();
        var cls = el.className || '';
        return t ? (cls.indexOf('bad') >= 0 ? '[bad] ' + t : t) : '';
      };
      window.__CLEAR_TOAST__ = function(){
        var e = document.getElementById('toast');
        if (e) { e.textContent = ''; e.hidden = true; }
      };
      var __rf = window.fetch.bind(window);
      window.fetch = function(u, opt){
        var s = String(u);
        window.__REQS__.push({ url: s, method: (opt && opt.method) || 'GET' });
        function reply(status, obj){
          var txt = JSON.stringify(obj);
          return Promise.resolve({ ok: status < 400, status: status, json: function(){ return Promise.resolve(obj); }, text: function(){ return Promise.resolve(txt); } });
        }
        if (s.indexOf('/admin/ui/manifest') >= 0) return reply(200, window.__MANIFEST__);
        // 额度刷新回执：带上刷新后的 accounts（后端真实形状如此）
        if (s.indexOf('/admin/accounts/quota/refresh') >= 0)
          return reply(200, { updated:3, unknown:0, failed:0, skipped:0, scope:'provider', count:3,
                              providers:['alpha'], accounts: window.__ACCOUNTS__.accounts });
        if (s.indexOf('/admin/accounts/checkin/all') >= 0)
          return reply(202, { started:true, providers:['alpha','beta'], count:2 });
        if (s.indexOf('/admin/accounts') >= 0) return reply(200, window.__ACCOUNTS__);
        if (s.indexOf('/admin/task') >= 0) return reply(200, { running:false });
        if (s.indexOf('/admin/') >= 0 || s.indexOf('/v1/') >= 0 || s.indexOf('/healthz') >= 0) return reply(200, {});
        return __rf(u, opt);
      };
    `;
    await send('Page.addScriptToEvaluateOnNewDocument', { source: pre });
    await send('Page.navigate', { url: BASE + '/ui' });
    await sleep(4000);

    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) {
        return { __evalError: JSON.stringify(r.result.exceptionDetails.exception || r.result.exceptionDetails) };
      }
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    const boot = JSON.parse(await ev(`JSON.stringify({ hasApi: !!(window.__wb2api__ && window.__wb2api__.applyManifest) })`));
    ok(boot.hasApi, '真页面加载成功（暴露 applyManifest）');

    await ev(`(function(){
      window.__wb2api__.applyManifest(window.__MANIFEST__);
      window.__wb2api__.renderAccounts(window.__ACCOUNTS__.accounts);
      return 1;
    })()`);
    await sleep(500);

    // ================================================================
    // A. 逐个上游点「刷新本上游额度」
    // ================================================================
    //
    // # 判据
    //
    //   · 每个**声明了 quota-probe** 的上游，它的分组行必须真有这个按钮
    //     （0 账号的也要有 —— 后端会对它回 404，那正是要暴露的真相）
    //   · 没声明 quota-probe 的上游**不该**有（假按钮守卫）
    //   · 点完之后：页面**没有**未捕获异常、**没有**含 "is not defined" 的提示
    //   · 请求确实发到了 /admin/accounts/quota/refresh，且 body 带该上游
    console.log('\n[A] 逐个上游点「刷新本上游额度」');
    const groups = JSON.parse(await ev(`JSON.stringify(
      Array.prototype.slice.call(document.querySelectorAll('#accts > section.acctgroup')).map(function(sec){
        var b = sec.querySelector('.ghead .gacts button[data-gquota]');
        return { provider: sec.dataset.acctgroup, hasBtn: !!b, gquota: b ? b.dataset.gquota : null };
      })
    )`));
    console.log('  分组与按钮: ' + JSON.stringify(groups));

    const withQuota = ['alpha', 'beta', 'gamma'];   // 声明了 quota-probe（含 0 账号的 gamma）
    const withoutQuota = ['delta'];                 // 没声明
    for (const p of withQuota) {
      const g = groups.find(x => x.provider === p);
      ok(g && g.hasBtn && g.gquota === p,
        p + ' 的分组行有「刷新本上游额度」按钮（data-gquota=' + (g ? g.gquota : '—') + '）');
    }
    for (const p of withoutQuota) {
      const g = groups.find(x => x.provider === p);
      ok(g && !g.hasBtn,
        p + '（未声明 quota-probe）**没有**该按钮 —— 假按钮守卫');
    }

    // 真点每一个
    for (const p of withQuota) {
      await ev(`(function(){ window.__REQS__ = []; window.__ERRS__ = []; window.__CLEAR_TOAST__(); return 1 })()`);
      const clicked = await ev(`(function(){
        var sec = document.querySelector('#accts > section.acctgroup[data-acctgroup="' + ${JSON.stringify(p)} + '"]');
        if (!sec) return 'no-group';
        var b = sec.querySelector('.ghead .gacts button[data-gquota]');
        if (!b) return 'no-button';
        b.click();
        return 'clicked';
      })()`);
      await sleep(1000);

      const st = JSON.parse(await ev(`JSON.stringify({
        errs: window.__ERRS__,
        toasts: (window.__TOAST_TEXT__() ? [window.__TOAST_TEXT__()] : []),
        reqs: window.__REQS__.filter(function(r){ return r.url.indexOf('/admin/accounts/quota/refresh') >= 0; }),
      })`));

      console.log('  ' + p + ': clicked=' + clicked +
        ' reqs=' + JSON.stringify(st.reqs.map(r => r.url)) +
        ' toasts=' + JSON.stringify(st.toasts) +
        ' errs=' + JSON.stringify(st.errs));

      ok(clicked === 'clicked', p + ' 的「刷新本上游额度」按钮能点到（' + clicked + '）');
      ok(st.reqs.length === 1, p + ' 点一次恰好发 1 个额度刷新请求（实际 ' + st.reqs.length + '）');

      // ⚠ 核心判据：不得出现 "is not defined" / ReferenceError / 失败提示
      const badErr = st.errs.filter(x => /is not defined|ReferenceError|not a function/.test(x));
      ok(badErr.length === 0,
        p + ' 点完之后**没有**未捕获异常（本缺陷的形态：refreshAccounts is not defined）' +
        (badErr.length ? '\n      实际: ' + JSON.stringify(badErr) : ''));

      const badToast = st.toasts.filter(x => /失败|is not defined/.test(x));
      ok(badToast.length === 0,
        p + ' 点完之后**没有**失败提示' +
        (badToast.length ? '\n      实际: ' + JSON.stringify(badToast) : ''));

      const okToast = st.toasts.filter(x => /额度/.test(x));
      ok(okToast.length >= 1, p + ' 点完之后给出了额度回执文案（实际 ' + JSON.stringify(st.toasts) + '）');
    }

    // ================================================================
    // B. 点「全部签到」（顶部）
    // ================================================================
    console.log('\n[B] 点顶部「全部签到」');
    await ev(`(function(){ window.__REQS__ = []; window.__ERRS__ = []; window.__CLEAR_TOAST__(); return 1 })()`);
    const clickedAll = await ev(`(function(){
      var b = document.getElementById('btnAllCheckinAll');
      if (!b) return 'no-button';
      b.click();
      return 'clicked';
    })()`);
    await sleep(1200);
    const allSt = JSON.parse(await ev(`JSON.stringify({
      errs: window.__ERRS__,
      toasts: (window.__TOAST_TEXT__() ? [window.__TOAST_TEXT__()] : []),
      reqs: window.__REQS__.filter(function(r){ return r.url.indexOf('/admin/accounts/checkin/all') >= 0; }),
    })`));
    console.log('  clicked=' + clickedAll + ' reqs=' + JSON.stringify(allSt.reqs.map(r => r.url)) +
      ' toasts=' + JSON.stringify(allSt.toasts) + ' errs=' + JSON.stringify(allSt.errs));

    ok(clickedAll === 'clicked', '顶部「全部签到」按钮能点到（' + clickedAll + '）');
    ok(allSt.reqs.length === 1, '点一次恰好发 1 个核心签到请求（实际 ' + allSt.reqs.length + '）');
    ok(allSt.errs.filter(x => /is not defined|ReferenceError/.test(x)).length === 0,
      '顶部「全部签到」点完**没有**未捕获异常' + (allSt.errs.length ? '\n      实际: ' + JSON.stringify(allSt.errs) : ''));
    ok(allSt.toasts.filter(x => /失败/.test(x)).length === 0,
      '顶部「全部签到」点完**没有**失败提示' + (allSt.toasts.length ? '\n      实际: ' + JSON.stringify(allSt.toasts) : ''));
    ok(allSt.toasts.some(x => /已启动|全部签到/.test(x)),
      '顶部「全部签到」给出了启动回执（实际 ' + JSON.stringify(allSt.toasts) + '）');

    // ================================================================
    // C. 逐个上游点卡片头「全部签到」
    // ================================================================
    console.log('\n[C] 逐个上游点卡片头「全部签到」');
    for (const p of PROVIDERS.map(x => x.id)) {
      await ev(`(function(){ window.__REQS__ = []; window.__ERRS__ = []; window.__CLEAR_TOAST__(); return 1 })()`);
      const c = await ev(`(function(){
        var sec = document.querySelector('#accts > section.acctgroup[data-acctgroup="' + ${JSON.stringify(p)} + '"]');
        if (!sec) return 'no-group';
        var b = sec.querySelector('.ghead .gacts button[data-allday]');
        if (!b) return 'no-button';
        b.click();
        return 'clicked';
      })()`);
      await sleep(900);
      const st = JSON.parse(await ev(`JSON.stringify({
        errs: window.__ERRS__,
        toasts: (window.__TOAST_TEXT__() ? [window.__TOAST_TEXT__()] : []),
        reqs: window.__REQS__.filter(function(r){ return r.method === 'POST' && r.url.indexOf('/admin/' + ${JSON.stringify(p)} + '/checkin/all') >= 0; }),
      })`));
      ok(c === 'clicked', p + ' 卡片头「全部签到」能点到');
      ok(st.reqs.length === 1, p + ' 点一次恰好发 1 个该上游的 all_url 请求（实际 ' + st.reqs.length + '）');
      ok(st.errs.filter(x => /is not defined|ReferenceError/.test(x)).length === 0,
        p + ' 卡片头签到点完没有未捕获异常' + (st.errs.length ? '：' + JSON.stringify(st.errs) : ''));
      ok(st.toasts.filter(x => /失败/.test(x)).length === 0,
        p + ' 卡片头签到点完没有失败提示' + (st.toasts.length ? '：' + JSON.stringify(st.toasts) : ''));
    }

    // ================================================================
    // D. 全程汇总
    // ================================================================
    console.log('\n[D] 全程汇总');
    const total = JSON.parse(await ev(`JSON.stringify({ errs: window.__ERRS__, toasts: (window.__TOAST_TEXT__() ? [window.__TOAST_TEXT__()] : []) })`));
    console.log('  累计未捕获异常: ' + JSON.stringify(total.errs));
    console.log('  最后一次 toast: ' + JSON.stringify(total.toasts));
    ok(total.errs.filter(x => /is not defined/.test(x)).length === 0,
      '全程**没有** "is not defined" 类异常（本缺陷的指纹）');

  } catch (e) {
    console.log('EXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { if (ch) ch.kill(); } catch { }
    try { if (srv) srv.kill(); } catch { }
    await sleep(600);
    try { fs.rmSync(TMP, { recursive: true, force: true }); } catch { }
    try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  }
  console.log(fail === 0 ? '\n=== 全部按钮 e2e 通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
