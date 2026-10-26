// verify_checkin_ui_e2e.js —— 「全部签到」分级布局的**真前端 e2e**。
//
// # 为什么必须存在（它抓到过一个静态断言抓不到的缺陷）
//
// 本仓的教训（见 verify_account_buttons.js 的文件头）：断言只检查
// "有没有某个元素"时，把整块删掉也照样绿。本探针要证明的是**行为**：
//
//	① 卡片头那个按钮点下去，POST 的 URL 是**该上游自己的** all_url
//	② 顶部那个按钮点下去，POST 的 URL 是核心端点 /admin/accounts/checkin/all
//	③ **刷新之后**（renderAccounts 重建整块 innerHTML）按钮数量不增不减
//	④ 刷新 N 次后，点一次按钮只发**一个**请求（监听器不累积）
//
// ③ ④ 是"看起来对"与"真的对"的分界：DOM 长得一样，但委托若被绑了
// 多次，点一下会发 N 个请求。**变异验证已证明**：把
// `installAllDailyActions` 的"只绑一次"守卫去掉、并改成每次重绘都调它，
// 刷新 5 次后点一次会发 **7 个**请求 —— 而所有只看 DOM 的断言全绿。
//
// # 实例启动：走共享层 lib_instance（T83 统一）
//
// 本套件要起一个**隔离实例**（临时目录 + 空闲端口），因为：
//
//   - 要拦 fetch 记录真实请求，页面**首先得能正常引导**（boot() 会真打
//     /healthz、/admin/ui/manifest、/admin/settings…）。用假后端复刻这些
//     形状比起真实例更不真实、也更脆。
//   - 只把 manifest 换成本文件手造的那份（假上游名 alpha/beta/gamma），
//     于是「前端不认识任何上游名」这条判据也被真正测到。
//
// ⚠ **T83 之前**本探针自己解析 EXE、自己 spawn：EXE 在模块求值那一刻定死，
//   缺省是 `bin/wb2api-server.exe`（**用户正在跑的旧构建**）⇒ 未设 WB2API_EXE
//   时它会去起用户的实例，然后被「构建陈旧」判据拦下。T79 实测：3 次里红 1 次，
//   而**产品没有任何问题**（探针侧 flake）。
//
// 现在与其它实例探针同构（范式见 verify_wave2_pages.js）：
//
//   ① 缺省 WB2API_EXE = probe 构建（**在 require(lib_instance) 之前**设）
//   ② startInstance 现起实例：现场探测的空闲端口 + os.tmpdir() 下的临时目录
//   ③ 构建新鲜度由 lib_instance.assertBuildFresh **硬门禁**（过期直接抛，
//      没有 bypass 开关）—— 本文件不再持有那份实现的同源副本
//   ④ 收尾按 PID/句柄收（inst.stop()），端口空出会被确认
//
// 断言与语义**一个字没改**：本文件要证的行为（卡片头按钮打自己的 all_url、
// 顶部打核心端点、刷新后按钮不累积、点一次只发一个请求）逐条照旧。
//
// # ⚠ 端口与目录都是临时的
//
// 仓库里已有的 config.json（7863）/ config-test-port7864.json（7864）
// 可能是**用户正在用**的，绝不碰。端口用 E2E_PORT 覆盖（默认 7871），
// 目录在 os.tmpdir() 下按 pid 唯一。收尾**按 PID 杀**，不用
// `Get-Process <name> | Stop-Process`（那会误杀用户自己的实例）。
'use strict';
// ⚠⚠ 必须在 require(lib_instance) **之前**设：lib_instance 在**模块求值那一刻**
// 就把 EXE 定下来（lib_instance.js:38-49），设晚了会静默回落到
// bin/wb2api-server.exe —— 那通常是用户正在跑的旧构建（本仓吃过这个亏）。
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn, execFileSync } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
// 控制台身份：withToken 是共享层里"给 /ui 挂令牌"的**唯一**实现
//（本文件自带 CDP 引导段，不走 withChrome，所以得自己调一次 —— 见 UI_URL 的注释）。
const { withToken } = require('./lib_cdp.js');
// 实例启动统一走共享层（T83）：EXE 缺省、端口预检、构建新鲜度、临时目录、
// 收尾确认端口空出 —— 全都在它里面，探针不再各写一份。
const { startInstance } = require('./lib_instance.js');
const REPO = path.resolve(__dirname, '..', '..');
// ── EXE 与构建新鲜度：**都交给 lib_instance**（T83 统一，不再各写一份）──
//
// 这里原先有两块本地实现：① 自己解析 EXE（缺省 bin/wb2api-server.exe = 用户二进制）；
// ② assertBuildFresh 的同源副本。它们的缺省值就是 T79 实测那条 1/3 间歇红的根因：
// 未设 WB2API_EXE ⇒ 起用户的旧构建 ⇒ 被新鲜度判据拦下 ⇒ 看起来像产品缺陷。
//
// 现在 EXE 只由 lib_instance 决定（缺省 probe 构建；显式指向用户二进制时会打印
// 醒目告警），新鲜度由 lib_instance.assertBuildFresh **硬门禁**（不新鲜直接抛，
// 没有 bypass 开关，理由见那函数的长注释）。
//
// ⚠ 仍有同源副本的是 `verify_all_buttons_e2e.js`（T83 只允许改本文件，
//   那条留给后续统一；它同样自解析 EXE + 自建新鲜度判据）。

const fmtTime = (ms) => new Date(ms).toLocaleString('zh-CN', { hour12: false });
const sleep = ms => new Promise(r => setTimeout(r, ms));

// 现场探测一个空闲端口（0 = 让内核分配，拿到就立刻释放）。
// ⚠ 为什么不用固定端口 7871：固定端口在并发/别的探针残留时会撞（lib_instance 的
// 端口预检会因此红），而"现场探测的端口"才是本仓其它实例探针的统一口径。
// E2E_PORT 仍可显式指定（用于复现/对照），指定时按它来。
function freePort() {
  return new Promise((res, rej) => {
    const s = require('net').createServer();
    s.on('error', rej);
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); });
  });
}

const PROFILE = path.join(os.tmpdir(), 'chrome-checkinui-e2e-' + Date.now() + '-' + process.pid);
const KEY = 'e2e-only-key-checkin';
// BASE / PORT / UI_URL 都要等实例起来才知道（端口是现场探测的）⇒ 在 main 里赋值。
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
// ⇒ 下面每条页面断言都是级联（实测 6 条：引导 2 条 + 布局 4 条 + 一条 EXCEPTION）。
//
// ⚠ 用共享层的 `withToken`，不在本文件就地实现：它是「控制台 URL 的唯一入口」。
// ⚠ 令牌走探针自己的 admin_token（KEY）；登录门只认 `admin_token`，未配时恒 false
//   且不回落到 api_key（webui.go:114-115）—— lib_instance 起实例时两者同值。
//
// ⚠ UI_URL 现在在 main 里赋值：端口是**现场探测**的，模块求值期还不知道 BASE。
const get = u => new Promise((res, rej) => http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej));

// 手造 manifest：两个**仓库里不存在**的上游名。
// 用真名（workbuddy/codearts）会让"前端其实是硬编码的"这种缺陷照样绿。
const MANIFEST = {
  service: 'e2e',
  providers: [
    { id: 'alpha', default: true, account_count: 2, capabilities: ['chat'],
      accounts_columns: ['provider', 'nickname', 'uid', 'ops'],
      login: { kind: 'oauth', label: '添加账号' } },
    { id: 'beta', account_count: 1, capabilities: ['chat'],
      accounts_columns: ['provider', 'nickname', 'uid', 'ops'],
      login: { kind: 'oauth', label: '添加账号' } },
    { id: 'gamma', account_count: 0, capabilities: ['chat'],
      accounts_columns: ['provider', 'nickname', 'uid', 'ops'] },
  ],
  capabilities: [{ id: 'chat', title: '对话' }],
  admin_routes: [],
  jobs: [],
  daily_actions: [
    { provider: 'alpha', id: 'alpha-checkin', label: '签到', title: 'alpha 签到',
      one_url: '/admin/alpha/checkin', all_url: '/admin/alpha/checkin/all', batch: true },
    { provider: 'beta', id: 'beta-checkin', label: '签到', title: 'beta 签到',
      one_url: '/admin/beta/checkin', all_url: '/admin/beta/checkin/all', batch: true },
    // 有全量端点但 Batch=false：产品决定不给入口（workbuddy 保活同形）
    { provider: 'beta', id: 'beta-bulk-noentry', label: '派猫', title: 'beta 派猫',
      one_url: '/admin/beta/travel', all_url: '/admin/beta/travel/all', batch: false },
    // 没有全量端点
    { provider: 'beta', id: 'beta-keepalive', label: '保活', title: 'beta 保活',
      one_url: '/admin/beta/keepalive', all_url: '', batch: false },
    // gamma 一个动作都没有（0 账号的实例）
  ],
};

const ACCOUNTS = {
  accounts: [
    { uid: 'u-alpha-1', provider: 'alpha', nickname: '甲号一', disabled: false, has_token: true },
    { uid: 'u-alpha-2', provider: 'alpha', nickname: '甲号二', disabled: false, has_token: true },
    { uid: 'u-beta-1', provider: 'beta', nickname: '乙号一', disabled: false, has_token: true },
  ],
};

// ---- 起隔离实例：全部交给 lib_instance（T83）----
//
// 它写的那份 config 已含本探针需要的前提（api_key 与 admin_token **同值**、auth_dir、
// admin.checkin_log_path、login.isolated），所以这里不再手写 config —— 手写那份就是
// 「同一个东西两处定义」，一旦 lib_instance 改了前提，本探针会静默地少一条。

let inst = null, ch = null, fail = 0, ws = null;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  try {
    // 端口：E2E_PORT 显式给了就照它；否则现场探测一个空闲端口（不写死 7871）。
    if (!PORT) PORT = await freePort();
    // 实例启动 + 构建新鲜度门禁 + 端口预检全在 startInstance 里（T83 统一）：
    //   · EXE 缺省 probe 构建（本文件顶部在 require 之前设的）
    //   · assertBuildFresh 硬门禁（过期直接抛；没有 bypass 开关）
    //   · 端口被占 ⇒ 大声红并点名 pid（不静默换端口）
    inst = await startInstance({ port: PORT, apiKey: KEY });
    BASE = inst.base;
    UI_URL = withToken(BASE + '/ui', KEY);
    console.log('  实例已起: ' + BASE + '（pid=' + inst.pid + '，临时目录 ' + inst.tmp + '）');
    // CDP 端口同样现场探测（原先写死 9361：并发时会撞）。
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

    // 导航**之前**注入：
    //  · 记录所有出站请求（证明"点了按钮真的发了对的那个"）
    //  · 把 manifest 与账号换成本探针的手造数据
    const pre = `
      window.__REQS__ = [];
      window.__MANIFEST__ = ${JSON.stringify(MANIFEST)};
      window.__ACCOUNTS__ = ${JSON.stringify(ACCOUNTS)};
      const __realFetch = window.fetch.bind(window);
      window.fetch = function(u, opt){
        const s = String(u);
        window.__REQS__.push({ url: s, method: (opt && opt.method) || 'GET', body: (opt && opt.body) || '' });
        if (s.indexOf('/admin/ui/manifest') >= 0)
          return Promise.resolve({ ok:true, status:200, json:()=>Promise.resolve(window.__MANIFEST__), text:()=>Promise.resolve('') });
        if (s.indexOf('/admin/accounts/checkin/all') >= 0)
          return Promise.resolve({ ok:true, status:202, json:()=>Promise.resolve({started:true,providers:['alpha','beta'],count:2}), text:()=>Promise.resolve('') });
        if (s.indexOf('/admin/accounts') >= 0)
          return Promise.resolve({ ok:true, status:200, json:()=>Promise.resolve(window.__ACCOUNTS__), text:()=>Promise.resolve('') });
        if (s.indexOf('/admin/task') >= 0)
          return Promise.resolve({ ok:true, status:200, json:()=>Promise.resolve({running:false}), text:()=>Promise.resolve('') });
        if (s.indexOf('/admin/') >= 0 || s.indexOf('/v1/') >= 0 || s.indexOf('/healthz') >= 0)
          return Promise.resolve({ ok:true, status:200, json:()=>Promise.resolve({}), text:()=>Promise.resolve('') });
        return __realFetch(u, opt);
      };
    `;
    await send('Page.addScriptToEvaluateOnNewDocument', { source: pre });
    await send('Page.navigate', { url: UI_URL });
    await sleep(4000);

    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) {
        throw new Error('求值异常: ' + JSON.stringify(r.result.exceptionDetails.exception || r.result.exceptionDetails));
      }
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // 引导检查
    const boot = JSON.parse(await ev(`JSON.stringify({
      hasApi: !!(window.__wb2api__ && window.__wb2api__.applyManifest),
      reqs: (window.__REQS__||[]).length
    })`));
    console.log('  引导: ' + JSON.stringify(boot));
    ok(boot.hasApi, '真页面加载成功且暴露了 applyManifest');
    ok(boot.reqs > 0, '页面确实发出了真实请求（不是静态假页）');

    // 用手造 manifest + 账号驱动渲染
    const drove = await ev(`(function(){
      try {
        var W = window.__wb2api__;
        W.applyManifest(window.__MANIFEST__);
        W.renderAccounts(window.__ACCOUNTS__.accounts);
        return 'ok';
      } catch (e) { return 'ERR:' + e.message; }
    })()`);
    ok(drove === 'ok', '能把手造 manifest + 账号喂进渲染管线（' + drove + '）');
    await sleep(500);

    // ================================================================
    // ① 布局：卡片头有按钮、顶部没有
    // ================================================================
    console.log('\n[1] 布局');
    const snap = JSON.parse(await ev(`JSON.stringify((function(){
      var out = { heads:{}, topAllday:-1, topButtons:[] };
      document.querySelectorAll('#accts > section.acctgroup').forEach(function(sec){
        var hd = sec.querySelector('.ghead .gacts');
        out.heads[sec.dataset.acctgroup] = hd
          ? Array.prototype.slice.call(hd.querySelectorAll('button[data-allday]'))
              .map(function(b){ return { t:(b.textContent||'').trim(), a:b.dataset.allday }; })
          : null;
      });
      var h2 = document.querySelector('#content > section[data-key="accounts"] h2');
      if (h2) {
        out.topAllday = h2.querySelectorAll('button[data-allday]').length;
        out.topButtons = Array.prototype.slice.call(h2.querySelectorAll('button'))
          .map(function(b){ return { t:(b.textContent||'').split(' ').join('').trim(), id:b.id||null }; });
      }
      return out;
    })())`));
    console.log('  卡片头: ' + JSON.stringify(snap.heads));
    console.log('  顶部: ' + JSON.stringify(snap.topButtons) + '  data-allday 数=' + snap.topAllday);

    ok(snap.heads.alpha && snap.heads.alpha.length === 1 && snap.heads.alpha[0].a === 'alpha:alpha-checkin',
      'alpha 卡片头有且只有自己的「全部签到」（指向 alpha 的动作）');
    ok(snap.heads.beta && snap.heads.beta.length === 1 && snap.heads.beta[0].a === 'beta:beta-checkin',
      'beta 卡片头有且只有自己的「全部签到」');
    ok(!!snap.heads.beta && !snap.heads.beta.some(x => x.a.indexOf('beta-bulk-noentry') >= 0),
      'Batch=false 的动作（beta-bulk-noentry）**不**长出按钮');
    ok(!!snap.heads.beta && !snap.heads.beta.some(x => x.a.indexOf('beta-keepalive') >= 0),
      '无全量端点的动作（beta-keepalive）**不**长出按钮');
    ok(snap.heads.gamma !== null && snap.heads.gamma.length === 0,
      'gamma（无任何动作）卡片头**没有**按钮，但卡片本身仍在');
    ok(snap.topAllday === 0, '账号池**顶部**没有任何 data-allday 按钮');
    ok(snap.topButtons.some(b => b.id === 'btnAllCheckinAll'), '顶部有全局「全部签到」');
    ok(snap.topButtons.filter(b => b.t === '全部签到').length === 1, '顶部「全部签到」只有一个');

    // ================================================================
    // ② 行为：点卡片头按钮 → 打该上游自己的 all_url
    // ================================================================
    console.log('\n[2] 点卡片头「全部签到」（alpha）');
    await ev(`(function(){ window.__REQS__ = []; return 1 })()`);
    await ev(`document.querySelector('#accts > section.acctgroup[data-acctgroup="alpha"] .ghead .gacts button[data-allday]').click()`);
    await sleep(900);
    const r1 = JSON.parse(await ev(`JSON.stringify(window.__REQS__.filter(function(r){ return r.method === 'POST' }))`));
    console.log('  发出的 POST: ' + JSON.stringify(r1));
    const post1 = r1.filter(r => r.url.indexOf('/admin/') >= 0 && r.url.indexOf('/admin/accounts') < 0);
    ok(post1.length >= 1, '点击后确实发出了 POST 请求（按钮不是死的）');
    ok(post1.every(r => r.url.indexOf('/admin/alpha/checkin/all') >= 0),
      'POST 打的是 **alpha 自己的** all_url（实际: ' + JSON.stringify(post1.map(r => r.url)) + '）');
    ok(!post1.some(r => r.url.indexOf('/admin/accounts/checkin/all') >= 0),
      '卡片头按钮**没有**误打核心端点（那是顶部按钮的语义）');
    ok(!post1.some(r => r.url.indexOf('/admin/beta') >= 0), '没有串到 beta 的端点');

    // ================================================================
    // ③ 行为：点顶部按钮 → 打核心端点
    // ================================================================
    console.log('\n[3] 点顶部「全部签到」');
    await ev(`(function(){ window.__REQS__ = []; return 1 })()`);
    await ev(`document.getElementById('btnAllCheckinAll').click()`);
    await sleep(900);
    const r2 = JSON.parse(await ev(`JSON.stringify(window.__REQS__.filter(function(r){ return r.method === 'POST' }))`));
    console.log('  发出的 POST: ' + JSON.stringify(r2));
    const post2 = r2.filter(r => r.url.indexOf('/admin/accounts/checkin/all') >= 0);
    ok(post2.length === 1,
      '顶部按钮**恰好**发 1 个核心端点请求（实际 ' + post2.length + ' 个）—— 多了说明委托绑了多次');
    ok(!r2.some(r => r.url.indexOf('/admin/alpha/checkin/all') >= 0 || r.url.indexOf('/admin/beta/checkin/all') >= 0),
      '顶部按钮**没有**去逐个打上游的 all_url（那会让一次点击变成 N 个请求）');

    // ================================================================
    // ④ 刷新逻辑：反复重绘后按钮不重复、委托不累积
    // ================================================================
    console.log('\n[4] 刷新逻辑（renderAccounts 反复重建）');
    const before = JSON.parse(await ev(`JSON.stringify({
      alpha: document.querySelectorAll('#accts > section.acctgroup[data-acctgroup="alpha"] .ghead .gacts button[data-allday]').length,
      top: document.querySelector('#content > section[data-key="accounts"] h2').querySelectorAll('button[data-allday]').length,
      groups: document.querySelectorAll('#accts > section.acctgroup').length,
    })`));
    console.log('  刷新前: ' + JSON.stringify(before));

    // 反复刷新 5 次（每次 renderAccounts 都重建整块 innerHTML）
    for (let i = 0; i < 5; i++) {
      await ev(`(function(){
        window.__wb2api__.applyManifest(window.__MANIFEST__);
        window.__wb2api__.renderAccounts(window.__ACCOUNTS__.accounts);
        return 1;
      })()`);
      await sleep(120);
    }
    await sleep(400);

    const after = JSON.parse(await ev(`JSON.stringify({
      alpha: document.querySelectorAll('#accts > section.acctgroup[data-acctgroup="alpha"] .ghead .gacts button[data-allday]').length,
      top: document.querySelector('#content > section[data-key="accounts"] h2').querySelectorAll('button[data-allday]').length,
      groups: document.querySelectorAll('#accts > section.acctgroup').length,
      allAllday: document.querySelectorAll('#accts button[data-allday]').length,
    })`));
    console.log('  刷新 5 次后: ' + JSON.stringify(after));

    ok(after.alpha === 1, '刷新 5 次后 alpha 卡片头**仍然只有 1 个**「全部签到」（实际 ' + after.alpha + '）—— 多了说明按钮被累积渲染');
    ok(after.top === 0, '刷新 5 次后顶部**仍然没有** data-allday 按钮（实际 ' + after.top + '）');
    ok(after.groups === 3, '刷新 5 次后分组数不变（3 个上游，实际 ' + after.groups + '）');
    ok(after.allAllday === 2, '整个账号池只有 2 个卡片头全量按钮（alpha+beta，实际 ' + after.allAllday + '）');

    // 关键：刷新后点一次，只该发**一个**请求（委托若累积会发多个）
    await ev(`(function(){ window.__REQS__ = []; return 1 })()`);
    await ev(`document.querySelector('#accts > section.acctgroup[data-acctgroup="alpha"] .ghead .gacts button[data-allday]').click()`);
    await sleep(900);
    const r3 = JSON.parse(await ev(`JSON.stringify(window.__REQS__.filter(function(r){ return r.method === 'POST' && r.url.indexOf('/admin/alpha/checkin/all') >= 0 }))`));
    console.log('  刷新后点一次发出的 alpha 请求数: ' + r3.length);
    ok(r3.length === 1,
      '刷新 5 次后点一次**只发 1 个**请求（实际 ' + r3.length + '）—— 多了说明 #content 上的委托被重复绑定了');

    // 顶部同理
    await ev(`(function(){ window.__REQS__ = []; return 1 })()`);
    await ev(`document.getElementById('btnAllCheckinAll').click()`);
    await sleep(900);
    const r4 = JSON.parse(await ev(`JSON.stringify(window.__REQS__.filter(function(r){ return r.method === 'POST' && r.url.indexOf('/admin/accounts/checkin/all') >= 0 }))`));
    console.log('  刷新后点顶部发出的核心请求数: ' + r4.length);
    ok(r4.length === 1, '刷新 5 次后点顶部**只发 1 个**请求（实际 ' + r4.length + '）');

    // ================================================================
    // ⑤ 空池刷新：仍然渲染出全部上游（R2 的出口没被改坏）
    // ================================================================
    console.log('\n[5] 空池刷新');
    await ev(`window.__wb2api__.renderAccounts([])`);
    await sleep(300);
    const empty = JSON.parse(await ev(`JSON.stringify({
      groups: document.querySelectorAll('#accts > section.acctgroup').length,
      placeholder: /账号池为空/.test(document.getElementById('accts').innerHTML),
      buttons: document.querySelectorAll('#accts button[data-allday]').length,
      topAllday: document.querySelector('#content > section[data-key="accounts"] h2').querySelectorAll('button[data-allday]').length,
    })`));
    console.log('  空池: ' + JSON.stringify(empty));
    ok(empty.groups === 3, '空池仍渲染 3 个上游分组（实际 ' + empty.groups + '）');
    ok(empty.placeholder === false, '空池**没有**「账号池为空」提前返回');
    ok(empty.buttons === 2, '空池时卡片头按钮仍在（它们是按上游自报渲染的，与有没有号无关）');
    ok(empty.topAllday === 0, '空池时顶部仍然没有 data-allday 按钮');

    // 恢复
    await ev(`(function(){
      window.__wb2api__.applyManifest(window.__MANIFEST__);
      window.__wb2api__.renderAccounts(window.__ACCOUNTS__.accounts);
      return 1;
    })()`);

    // 页面无未捕获异常
    const errs = await ev(`JSON.stringify((window.__errs||[]).slice(0,5))`);
    console.log('  页面未捕获异常: ' + errs);
    ok(errs === '[]', '整个过程页面没有未捕获异常');

  } catch (e) {
    console.log('EXCEPTION: ' + e.message);
    fail++;
  } finally {
    // ⚠ 按 PID 杀，不用 Get-Process <name>（会误杀用户自己的实例）
    try { if (ws) ws.close(); } catch { }
    try { if (ch) ch.kill(); } catch { }
    // 实例收尾交给 startInstance 的 stop()：按 pid 收 + **确认端口真空出来**
    //（它比自己 kill 更强：kill 之后还要等 socket 释放，否则下一条探针会撞端口预检）。
    try { if (inst) await inst.stop(); } catch (e) { console.log('  stop(): ' + e.message); }
    try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  }
  console.log(fail === 0 ? '\n=== 「全部签到」前端 e2e 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
