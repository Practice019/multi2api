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
//
// # 实例启动：走共享层 lib_instance（T87 统一，与 verify_checkin_ui_e2e.js 同款）
//
// 上面那套判据测的是"页面里的行为"，而页面来自 **exe 内嵌的 webui.html**
// （`//go:embed`，编译期内嵌）。exe 比磁盘上的 html 旧 ⇒ 测的是**旧产品**，
// 于是所有断言再绿也是假绿。
//
// ⚠ T87 之前本探针自己解析 EXE（缺省 `bin/wb2api-server.exe` = 用户线上二进制）、
//   自带一份 assertBuildFresh、自己写 config 并 spawn（固定端口 7874/9364）。
//   未设 WB2API_EXE 时它会落到用户二进制 ⇒ 被新鲜度门禁判红（T83 那条同类隐患的
//   最后一处，实测 ALL_BUTTONS_NO_ENV_EXIT=1）。
//
// 现在与其它实例探针同构：缺省 probe（**在 require(lib_instance) 之前**设）、
// startInstance 现起实例（现场探测端口 + os.tmpdir() 临时目录）、构建新鲜度由
// lib_instance.assertBuildFresh 硬门禁、收尾 inst.stop() 并确认端口空出。
// 断言与语义**一个字没改**。
'use strict';
// ⚠⚠ 必须在 require(lib_instance) **之前**设（它在模块求值那一刻就定下 EXE）。
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
// 控制台身份：withToken 是共享层里"给 /ui 挂令牌"的**唯一**实现
//（本文件自带 CDP 引导段，不走 withChrome，所以得自己调一次 —— 见 UI_URL 的注释）。
const { withToken } = require('./lib_cdp.js');
// 实例启动统一走共享层（T87）：EXE 缺省、端口预检、构建新鲜度、临时目录、
// 收尾确认端口空出 —— 全都在它里面，探针不再各写一份。
const { startInstance } = require('./lib_instance.js');
const REPO = path.resolve(__dirname, '..', '..');

// ── EXE 与构建新鲜度：**都交给 lib_instance**（T87 统一，不再各写一份）──
//
// 这里原先有两块本地实现：① 自己解析 EXE（缺省 bin/wb2api-server.exe = 用户二进制）；
// ② assertBuildFresh 的同源副本。未设 WB2API_EXE 时前者会让探针去起用户的旧构建、
// 后者的门禁随即把它判红 —— 看起来像产品缺陷，其实是探针在量错对象（已实测）。
//
// 现在 EXE 只由 lib_instance 决定（缺省 probe；显式指向用户二进制时打印醒目告警），
// 新鲜度由 lib_instance.assertBuildFresh **硬门禁**（不新鲜直接抛，无 bypass 开关）。

const fmtTime = (ms) => new Date(ms).toLocaleString('zh-CN', { hour12: false });
const sleep = ms => new Promise(r => setTimeout(r, ms));
const get = u => new Promise((res, rej) => http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej));

// 现场探测一个空闲端口（0 = 内核分配，拿到即释放）。
// ⚠ 不写死 7874/9364：固定端口在并发或别的探针残留时会撞，lib_instance 的端口预检
// 会因此红；"现场探测"才是本仓实例探针的统一口径。实例端口仍可用 E2E_PORT 显式指定。
function freePort() {
  return new Promise((res, rej) => {
    const s = require('net').createServer();
    s.on('error', rej);
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); });
  });
}

const PROFILE = path.join(os.tmpdir(), 'chrome-allbtn-' + Date.now() + '-' + process.pid);
const KEY = 'allbtn-key-checkin';
// PORT / BASE / UI_URL 要等实例起来才知道（端口是现场探测的）⇒ 在 main 里赋值。
let PORT = Number(process.env.E2E_PORT || 0);
let BASE = '';
let UI_URL = '';

// UI_URL —— 开控制台必须**带身份**，不能再裸开 /ui。
//
// # 机制（为什么裸 /ui 会静默跑在**登录页**上）
//
// `internal/server/webui.go:180-213` 的 `/ui` 有三条路：
//   ① `?token=<admin_token>` 校验通过 ⇒ `Set-Cookie`（令牌 cookie，12h）+ **302 回干净的 /ui**
//   ② 已认证（会话 cookie 或令牌 cookie）⇒ 控制台外壳（页里有 `window.__wb2api__`）
//   ③ **其余**（含裸 `/ui`）⇒ 直接渲染**登录页**（webui-login.html，没有 `window.__wb2api__`）
//
// 本探针自带 CDP 引导段（不 `withChrome`），于是**绕过**了共享层里的
// `withToken(navURL)`（lib_cdp.js）⇒ 裸 /ui ⇒ 登录页 ⇒ `window.__wb2api__` 不存在
// ⇒ 下面每条页面断言都是级联（实测 25 条）。
//
// ⚠ 用共享层的 `withToken`，不在本文件就地实现：它是「控制台 URL 的唯一入口」。
// ⚠ 令牌走探针自己的 admin_token（KEY）；登录门只认 admin_token，未配时恒 false
//   且不回落到 api_key（webui.go:114-115）—— lib_instance 起实例时两者同值。
//
// ⚠ UI_URL 现在在 main 里赋值：端口是**现场探测**的，模块求值期还不知道 BASE。

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

// ---- 起隔离实例：全部交给 lib_instance（T87）----
//
// 它写的 config 已含本探针需要的前提（api_key 与 admin_token **同值**、auth_dir、
// admin.checkin_log_path、login.isolated），所以这里不再手写 config —— 手写那份就是
// 「同一个东西两处定义」，lib_instance 改了前提本探针会静默地少一条。
// 端口若未用 E2E_PORT 指定 ⇒ 现场探测一个空闲端口。

let inst = null, ch = null, ws = null, fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  try {
    if (!PORT) PORT = await freePort();
    // 实例启动 + 构建新鲜度门禁 + 端口预检全在 startInstance 里（T87 统一）：
    //   · EXE 缺省 probe 构建（本文件顶部在 require 之前设的）
    //   · assertBuildFresh 硬门禁（过期直接抛；没有 bypass 开关）
    //   · 端口被占 ⇒ 大声红并点名 pid（不静默换端口）
    inst = await startInstance({ port: PORT, apiKey: KEY });
    BASE = inst.base;
    UI_URL = withToken(BASE + '/ui', KEY);
    console.log('  实例已起: ' + BASE + '（pid=' + inst.pid + '，临时目录 ' + inst.tmp + '）');
    // CDP 端口同样现场探测（原先写死 9364：并发时会撞）。
    const CDP = await freePort();


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
    await send('Page.navigate', { url: UI_URL });
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
    // 实例收尾交给 startInstance 的 stop()：按 pid 收 + **确认端口真空出来**。
    try { if (inst) await inst.stop(); } catch (e) { console.log('  stop(): ' + e.message); }
    await sleep(600);
    try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  }
  console.log(fail === 0 ? '\n=== 全部按钮 e2e 通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
