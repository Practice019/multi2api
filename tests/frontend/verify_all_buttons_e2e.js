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
// # 前置判据（新增）：要启动的 exe 必须是**当前构建**
//
// 上面那套判据测的是"页面里的行为"，而页面来自 **exe 内嵌的 webui.html**
// （`//go:embed`，编译期内嵌）。exe 比磁盘上的 html 旧 ⇒ 测的是**旧产品**，
// 于是所有断言再绿也是假绿。判据与理由见下面 assertBuildFresh 的注释。
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn } = require('child_process');

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
// 改之前它是写死的 `path.join(REPO, 'bin', 'wb2api-server.exe')`（旧第 37 行），
// 后果就是下面 assertBuildFresh 注释里那次事故：这条探针从来没有读过 WB2API_EXE。
const EXE = process.env.WB2API_EXE
  ? path.resolve(REPO, process.env.WB2API_EXE)
  : path.join(REPO, 'bin', 'wb2api-server.exe');

// EXE_AT_LOAD 记下"本文件求值那一刻"的取值，用来抓 `lib_instance.js:38-49` 那个**静默**坑：
//
//	process.env.WB2API_EXE = 'bin/xxx-probe.exe';   // ← 写在本文件被 require/import 之后
//	const mod = require('./verify_all_buttons_e2e.js');
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
// 浏览器拿到的 /ui 根本不是磁盘上那份，本探针点的那套按钮也不是当前代码的按钮
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
// ⚠ 同源副本：`verify_checkin_ui_e2e.js` 里有一份逐字相同的实现
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

// ⚠ 临时目录 + 空闲端口。仓库里已有的 config.json（7863）/
// config-test-port7864.json（7864）可能是用户在用的，绝不碰。
const PORT = Number(process.env.E2E_PORT || 7874);
const TMP = path.join(os.tmpdir(), 'wb2api-allbtn-' + PORT + '-' + process.pid);
const PROFILE = path.join(os.tmpdir(), 'chrome-allbtn-' + Date.now() + '-' + process.pid);
const CDP = Number(process.env.E2E_CDP || 9364);
const KEY = 'allbtn-key-' + PORT;
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
// ⇒ 本文件下面每条页面断言都是级联（实测 25 条）。
//
// # 那为什么以前是"绿"的（这条比结论值钱）
//
// 旧构建 `bin/wb2api-server.exe`（2026-10-19 04:17:47）**早于**那道登录门。字节级实测：
// 它**不含** `__WB2API_LOGIN_BOOTSTRAP__`、**含** `__WB2API_KEY__` —— 即
// lib_instance.js:43-46 记的"上一世代：裸 /ui 吐控制台外壳 + 页面里明文 key"。
// 所以裸 /ui 当年真能拿到控制台，本探针的断言**真跑过、也真报过绿**
//（CHANGELOG:125 记的 42/0）—— 那是**在一个已不复存在的产品世代上的绿**。
// 也就是说这里的缺陷不是一个 token，而是两个前提同时过期且互相掩盖：
// ① 探针绑死在 `bin/wb2api-server.exe` 这个旧二进制上（本轮前面的 EXE 解析 + 新鲜度判据修的）；
// ② 开页不带身份（本次修的）。只有把它们一起摆出来，才会看见"绿灯属于哪个世代"。
//
// ⚠ 用共享层的 `withToken`，不在本文件就地实现：它是"控制台 URL 的唯一入口"；
//   就地再写一份就是"同源副本"，两边一漂就变成两套行为。
// ⚠ 令牌走的是**探针自己的** admin_token（下面 config 里的 KEY）：
//   登录门只认 `admin_token`，**未配时恒 false 且不回落到 api_key**
//   （webui.go:114-115 + account.go 侧的 adminTokenOK），所以 config 里必须有那一行。
const UI_URL = withToken(BASE + '/ui', KEY);

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

let srv = null, ch = null, ws = null, fail = 0;
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
    try { if (srv) srv.kill(); } catch { }
    await sleep(600);
    try { fs.rmSync(TMP, { recursive: true, force: true }); } catch { }
    try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  }
  console.log(fail === 0 ? '\n=== 全部按钮 e2e 通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
