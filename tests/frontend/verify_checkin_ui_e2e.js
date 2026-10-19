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
// # 它是本目录**唯一**自己起网关实例的套件
//
// 其余套件要么纯静态、要么需要外部已跑着的实例（ACC_URL）。本套件
// 自己起一个**隔离实例**（临时目录 + 空闲端口），因为：
//
//   - 要拦 fetch 记录真实请求，页面**首先得能正常引导**（boot() 会真打
//     /healthz、/admin/ui/manifest、/admin/settings…）。用假后端复刻这些
//     形状比起真实例更不真实、也更脆。
//   - 只把 manifest 换成本文件手造的那份（假上游名 alpha/beta/gamma），
//     于是"前端不认识任何上游名"这条判据也被真正测到。
//
// # 前置判据（新增）：要启动的 exe 必须是**当前构建**
//
// 本套件验的是**页面行为**，而页面来自 exe 内嵌的 webui.html
// （`internal/server/webui.go:15` 的 `//go:embed`，编译期内嵌）。
// exe 比磁盘上的 html 旧 ⇒ 测的是**旧产品**，绿灯也是假绿。
// 判据与理由见下面 assertBuildFresh 的注释（同一条判据另一个副本在
// verify_all_buttons_e2e.js）。
//
// # ⚠ 端口与目录都是临时的
//
// 仓库里已有的 config.json（7863）/ config-test-port7864.json（7864）
// 可能是**用户正在用**的，绝不碰。端口用 E2E_PORT 覆盖（默认 7871），
// 目录在 os.tmpdir() 下按 pid 唯一。收尾**按 PID 杀**，不用
// `Get-Process <name> | Stop-Process`（那会误杀用户自己的实例）。
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn, execFileSync } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
// 控制台身份：withToken 是共享层里"给 /ui 挂令牌"的**唯一**实现
//（本文件自带 CDP 引导段，不走 withChrome，所以得自己调一次 —— 见 UI_URL 的注释）。
const { withToken } = require('./lib_cdp.js');
const REPO = path.resolve(__dirname, '..', '..');
// EXE 可用环境变量覆盖：WB2API_EXE=bin/wb2api-server-probe.exe
//   ⚠ 存在的理由：**不能覆盖 bin/wb2api-server.exe** —— 那通常是用户正在跑的
//     实例（Windows 上还被进程锁着）。改了代码要验证时，编一个别的名字再指过来。
//
// 这三行是 `lib_instance.js:31-36` 的**照抄**，不是另创一套。本探针是自包含的
// （自己 spawn + 自己开 CDP），不 require lib_instance.js；但"要启动哪个二进制"
// 这条规则仓库里只能有**一份**：env 优先、相对仓库根解析。
//
// 改之前它是写死的 `path.join(REPO, 'bin', 'wb2api-server.exe')`（旧第 44 行），
// 后果就是下面 assertBuildFresh 注释里那次事故：这条探针从来没有读过 WB2API_EXE。
const EXE = process.env.WB2API_EXE
  ? path.resolve(REPO, process.env.WB2API_EXE)
  : path.join(REPO, 'bin', 'wb2api-server.exe');

// EXE_AT_LOAD 记下"本文件求值那一刻"的取值，用来抓 `lib_instance.js:38-49` 那个**静默**坑：
//
//	process.env.WB2API_EXE = 'bin/xxx-probe.exe';   // ← 写在本文件被 require/import 之后
//	const mod = require('./verify_checkin_ui_e2e.js');
//
// 上面的顺序里，本模块早就求值完了，EXE 恒为缺省的 `bin/wb2api-server.exe`
// —— 而那常常是**用户正在跑的旧构建**。症状极具误导性：探针会去起那个旧版本，
// 于是"上一世代"的行为被当成当前代码的 bug 来查（实测有人整轮断言作废）。
//
// 判据只能是"求值时的值 vs 用的时候的值"，所以两处都记下来比较（照抄同一对策）。
const EXE_AT_LOAD = EXE;

const fmtTime = (ms) => new Date(ms).toLocaleString('zh-CN', { hour12: false });

// assertBuildFresh 是**硬门禁**：要启动的 exe 必须不比 internal/server/webui.html 旧。
//
// # 为什么这条判据存在（2026-10-19 实测）
//
// `internal/server/webui.go:15` 用 `//go:embed webui.html` **编译期内嵌**整页前端，
// 所以 **exe 里嵌的是"构建那一刻"的 HTML**。exe 比 html 旧 ⇒ 它内嵌的是**旧产品**：
// 浏览器拿到的 /ui 根本不是磁盘上那份，本探针数的那套按钮/委托也不是当前代码的
// —— 于是它报的绿灯是"**旧产品上的绿灯**"，也就是假绿。
//
// 实测时间戳：`bin/wb2api-server.exe` = 04:17:47（旧构建），
// `bin/wb2api-server-probe.exe` = 15:01:59，`internal/server/webui.html` = 15:01:47。
// 这两条探针原先**写死** bin/wb2api-server.exe 且完全不读 WB2API_EXE，于是它们
// 启动的是 11 小时前的旧构建：盘上改了前端、探针验的还是旧前端。
//
// 比较对象是**实际要启动的那个 exe**（EXE，可能被 WB2API_EXE 指到探针构建），
// 不是写死的那条路径 —— 否则换成 probe 二进制就判不出陈旧来。
// exe 不存在也判红：不存在的 exe 不可能"比 html 新"，那同样是没测到当前产品。
//
// # 为什么**不给**"允许旧的"绕过开关
//
// 唯一正当的场景是"我就是要拿旧二进制做对照实验"，而那属于**一次性另跑**：
// 把探针显式指到另一个文件（WB2API_EXE=bin/wb2api-server-old.exe）就够。
// 一旦有 `WB2API_ALLOW_STALE=1` 这类开关，它迟早会被用来把红灯变绿，
// 而本仓库的纪律是"不许为了变绿而弱化判据"（`run_frontend_suites.js` 的门禁 1/2 同款理由）。
// 换句话说这条门禁回答的问题是"你现在验的是哪一份产品"，那个问题没有"其实也可以"的答案。
//
// ⚠ 同源副本：`verify_all_buttons_e2e.js` 里有一份逐字相同的实现
//   （本轮纪律是只改这两个文件、不新建共享层）。改一处必须同步另一处，
//   否则两条探针的新鲜度判据会悄悄漂成两套。
function assertBuildFresh(exe) {
  const html = path.join(REPO, 'internal', 'server', 'webui.html');
  if (!fs.existsSync(exe)) {
    console.log('  FAIL 构建新鲜度：要启动的 exe 不存在 ⇒ 不可能是当前构建');
    console.log('       使用的 exe : ' + exe);
    console.log('       下一步     : go build -o bin/wb2api-server-probe.exe ./cmd/server');
    console.log('                    并设 WB2API_EXE=bin/wb2api-server-probe.exe 再跑本探针');
    return false;
  }
  const exeMs = fs.statSync(exe).mtimeMs;
  const htmlMs = fs.statSync(html).mtimeMs;
  if (exeMs >= htmlMs) {
    console.log('  PASS 构建新鲜度：exe（' + fmtTime(exeMs) + '）不比 webui.html（' +
      fmtTime(htmlMs) + '）旧');
    return true;
  }
  console.log('  FAIL 构建新鲜度：要启动的 exe 比 internal/server/webui.html 旧 ⇒ 验的是**旧产品**' +
    '（exe 内嵌的是旧 HTML）');
  console.log('       使用的 exe : ' + exe);
  console.log('       exe  mtime  : ' + fmtTime(exeMs));
  console.log('       html mtime  : ' + fmtTime(htmlMs) + '   (' + path.relative(REPO, html) + ')');
  console.log('       exe 旧了    : ' + Math.round((htmlMs - exeMs) / 1000) + ' 秒');
  console.log('       下一步     : go build -o bin/wb2api-server-probe.exe ./cmd/server');
  console.log('                    并设 WB2API_EXE=bin/wb2api-server-probe.exe 再跑本探针');
  return false;
}

// ⚠ 端口与目录都必须是一次性的：仓库里已有的 config.json（7863）/
// config-test-port7864.json（7864）可能是用户在用的，绝不碰。
const PORT = Number(process.env.E2E_PORT || 7871);
const TMP = path.join(os.tmpdir(), 'wb2api-e2e-' + PORT + '-' + process.pid);
const PROFILE = path.join(os.tmpdir(), 'chrome-checkinui-e2e-' + Date.now() + '-' + process.pid);
const CDP = 9361;
const KEY = 'e2e-only-key-' + PORT;
const BASE = 'http://127.0.0.1:' + PORT;

// UI_URL —— 开控制台必须**带身份**，不能再裸开 /ui。
//
// # 机制（为什么裸 /ui 会静默跑在**登录页**上）
//
// `internal/server/webui.go:180-213` 的 `/ui` 有三条路：
//   ① `?token=<admin_token>` 校验通过 ⇒ `Set-Cookie`（令牌 cookie，12h）+ **302 回干净的 /ui**
//   ② 已认证（会话 cookie 或令牌 cookie）⇒ 控制台外壳（页里有 `window.__wb2api__`）
//   ③ **其余**（含裸 `/ui`）⇒ 直接渲染**登录页**（webui-login.html，没有 `window.__wb2api__`）
// 这道登录门是 commit `34777b6`（2026-10-19 08:59:27）落地的（T5/T7 拆掉
// "回环地址即凭据 + 页面明文注入 api_key"）。
//
// 本探针自带 CDP 引导段（不 `withChrome`），于是**绕过**了共享层里的
// `withToken(navURL)`（lib_cdp.js:269）⇒ 裸 /ui ⇒ 登录页 ⇒ `window.__wb2api__` 不存在
// ⇒ 本文件下面每条页面断言都是级联（实测 6 条：引导 2 条 + 布局 4 条 + 一条 EXCEPTION）。
//
// # 那为什么以前是"绿"的（这条比结论值钱）
//
// 旧构建 `bin/wb2api-server.exe`（2026-10-19 04:17:47）**早于**那道登录门。字节级实测：
// 它**不含** `__WB2API_LOGIN_BOOTSTRAP__`、**含** `__WB2API_KEY__` —— 即
// lib_instance.js:43-46 记的"上一世代：裸 /ui 吐控制台外壳 + 页面里明文 key"。
// 所以裸 /ui 当年真能拿到控制台，本探针的断言**真跑过、也真报过绿**
//（CHANGELOG:1415 记的这条 e2e 与 42/0 同批）—— 那是**在一个已不复存在的产品世代上的绿**。
// 也就是说这里的缺陷不是一个 token，而是两个前提同时过期且互相掩盖：
// ① 探针绑死在 `bin/wb2api-server.exe` 这个旧二进制上（本轮前面的 EXE 解析 + 新鲜度判据修的）；
// ② 开页不带身份（本次修的）。只有把它们一起摆出来，才会看见"绿灯属于哪个世代"。
//
// ⚠ 用共享层的 `withToken`，不在本文件就地实现：它是"控制台 URL 的唯一入口"；
//   就地再写一份就是"同源副本"，两边一漂就变成两套行为。
// ⚠ 令牌走的是**探针自己的** admin_token（下面 config 里的 KEY）：
//   登录门只认 `admin_token`，**未配时恒 false 且不回落到 api_key**
//   （webui.go:114-115），所以 config 里必须有那一行。
const UI_URL = withToken(BASE + '/ui', KEY);

const sleep = ms => new Promise(r => setTimeout(r, ms));
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

// ---- 起隔离实例 ----
fs.mkdirSync(TMP, { recursive: true });
fs.mkdirSync(path.join(TMP, 'auths'), { recursive: true });
fs.mkdirSync(path.join(TMP, 'data'), { recursive: true });
// config 里必须给 **admin_token**（不只是 api_key）：
// 登录门只认 admin_token，未配时 `adminTokenOK` 恒 false，**不回落到 api_key**
//（webui.go:114-115）。这就是"探针起的实例没人给它管理令牌 ⇒ 开 /ui 只能看登录页"
// 的那个前提缺口；lib_instance.js:148-149 起实例时也是 `api_key` 与 `admin_token` 同值。
fs.writeFileSync(path.join(TMP, 'config.json'), JSON.stringify({
  listen: '127.0.0.1:' + PORT,
  api_key: KEY,
  admin_token: KEY,
  auth_dir: './auths',
  admin: { checkin_log_path: './data/checkin-log.json', client_login_enabled: false },
  login: { open_browser: false, isolated: true },
}, null, 2));

let srv = null, ch = null, fail = 0, ws = null;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  try {
    // 抓"本文件求值之后才设 WB2API_EXE"那个静默坑（见 EXE_AT_LOAD 的注释）。
    // ⚠ 只警告**不抛**：探针可能就是故意要打另一个二进制（对照实验）。
    const nowEXE = process.env.WB2API_EXE
      ? path.resolve(REPO, process.env.WB2API_EXE)
      : path.join(REPO, 'bin', 'wb2api-server.exe');
    if (nowEXE !== EXE_AT_LOAD) {
      console.log('⚠⚠ WB2API_EXE 是在本文件求值**之后**才设的 —— 本次仍用求值时那份：');
      console.log('     ' + EXE_AT_LOAD);
      console.log('     你想要的：' + nowEXE);
      console.log('     改法：把 WB2API_EXE 设在启动本探针**之前**（命令行 / 包装脚本里设）。');
      console.log('     （否则验的是另一个二进制，现场表现极具误导性）');
    }
    // 硬门禁：先确认要启动的是**当前构建**，再去起它（见 assertBuildFresh 的注释）。
    // ⚠ 位置在 spawn **之前**：旧 exe 一旦起来，页面里全是上一世代的行为，
    //   后面几十条断言会以极具误导性的方式红/绿 —— 不如在这里就停，
    //   而且这时唯一可信的结论正好是"你现在验的是哪一份产品"。
    if (!assertBuildFresh(EXE)) {
      throw new Error('构建陈旧：要启动的 exe 比 internal/server/webui.html 旧 —— 拒绝在旧产品上验前端');
    }
    srv = spawn(EXE, ['-config', 'config.json'], { cwd: TMP, stdio: 'ignore' });
    // 等它监听
    let up = false;
    for (let i = 0; i < 40; i++) {
      try { await get(BASE + '/healthz'); up = true; break; } catch { }
      await sleep(250);
    }
    if (!up) throw new Error('实例未在 ' + BASE + ' 起来');
    console.log('  实例已起: ' + BASE + '（pid=' + srv.pid + '，临时目录 ' + TMP + '）');

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
    try { if (srv) srv.kill(); } catch { }
    await sleep(600);
    try { fs.rmSync(TMP, { recursive: true, force: true }); } catch { }
    try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  }
  console.log(fail === 0 ? '\n=== 「全部签到」前端 e2e 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
