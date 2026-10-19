// verify_console_cookie_auth_e2e.js —— 「控制台鉴权 cookie 化」之后的验收探针。
//
// 主脑这轮改的是一件**看不见**的事：控制台的身份从哪里来。
//
//	改造前：服务端把 `config.api_key` 与 `admin_token` **明文注入页面 HTML**，
//	        前端记住了就写 localStorage（用户报障 ②：轮换后不生效）
//	改造后：服务端下发 **HttpOnly 会话 cookie**（`wb2api_sid`）——
//	        JS 读不到、跨站带不上；`/ui?token=` 那条一次性换手也只落到
//	        HttpOnly 的 `wb2api_admin_token`，URL 里不留令牌
//
// # 为什么必须是这个形状（这类改动最容易"静默退化"）
//
// 五条真实退化形态，**每一条都不会让页面看起来坏掉**：
//
//	① 未登录也吐控制台外壳 —— 门禁从根上没了；页面照常渲染，只有"不该看见
//	   账号池的人"能看见。所以本探针拿**无凭据请求**去量，而不是看代码。
//	② 外壳里没有 `probeSession` —— 页面不再问"我是谁"，于是会话过期后
//	   留一屏点不动的面板（用户报障原话："token 没授权，设置页却还在那里"）。
//	   所以判据是"外壳里到底有没有那个请求"，不是"页面能不能打开"。
//	③ 外壳里又被注入 `admin_token` —— 泄密换了个地方（改造前是 api_key）。
//	   所以判据是**页面正文里有没有已知令牌的值**（lib.secretHits），
//	   以及与"是否还留着旧凭据模型的符号"两条互补的源码级判据。
//	④ `/status` 又放行 `api_key` —— api_key 是发给客户端/机器人的对话凭据，
//	   而 /status 回的是账号池画像（昵称/积分/上游熔断态）。
//	   ⇒ "给某人一把对话钥匙 = 把运营数据也给他看"。所以这条要**单独**量：
//	     一个只出示 api_key、别的凭据都不带的请求，必须 401。
//	⑤ `/status` 不做角色判定 —— 成员也能读管理面数据。037 要一个**真成员**。
//
// # 三个特别容易写错的地方（都实测踩过）
//
//	ⓐ **一次请求只能出示一个凭据**。用"已登录管理员的同一个客户端"去发
//	   `Bearer <api_key>`，客户端会把会话 cookie 一起回放 ⇒ `Identify` 的会话
//	   那条命中 ⇒ 200 ⇒ "api_key 必须 401"这条断言**假绿**。
//	   `lib.makeClient` 的 `statusAnon()` / `statusBearer()` 因此走**裸请求**。
//	ⓑ **逐字节比对必须同一时刻取两次**。登录页的正文按"账号库空不空"分叉
//	   （bootstrap 标志位），注册前取的基线与注册后取的不是同一份 HTML ——
//	   拿这两个比会"红得莫名其妙"，而那不是端点在做 oracle。
//	ⓒ **成员会话必须来自"第二次注册"**。空实例的第一个注册走 CreateBootstrap
//	   （建成**管理员**，回执多 `"bootstrap":true`，见 internal/account/http.go:327）；
//	   拿它当"成员"去测 403 会得到 200，看起来像"实现没做角色判定"，
//	   其实是探针用错了账号。所以本探针**按实测角色挑账号**，
//	   并优先用第二个注册当成员。lib_authapi 的桩照同一口径实现
//	   （空库首个注册建管理员），两边行为一致 ⇒ 自检才有意义。
//
// # 三种用法
//
//	node tests/frontend/verify_console_cookie_auth_e2e.js                 # 打 WB2API_BASE（缺省 http://127.0.0.1:7885）
//	node tests/frontend/verify_console_cookie_auth_e2e.js --start         # 探针自己起隔离空实例（7800 段端口 + D:\temp 下的临时目录）
//	node tests/frontend/verify_console_cookie_auth_e2e.js --self-test     # 打本地桩，验证**探针自身有判别力**
//
// 默认模式要验"api_key 不能进控制台"，所以需要**两把不等的令牌**：
//	WB2API_ADMIN_TOKEN / WB2API_API_KEY（--start 时探针自己配好，不用设）
//
// ⚠ 段落 ⓪ 会把"这次验的是哪个二进制"打出来（路径 + mtime + 大小）。
//   改动没生效时第一个该怀疑的就是"验了旧构建"，所以它必须显式可见。
'use strict';
const fs = require('fs');
const lib = require('./lib_authapi.js');
// 只为打印"本次验的是哪个二进制"（lib_instance 是起实例的唯一入口，见其文件头）。
const instLib = require('./lib_instance.js');

// --start 时给隔离实例配的**管理令牌**（必须与 apiKey 不相等，理由见 lib 的 runCli）。
const PROBE_ADMIN_TOKEN = 'probe-admin-token-console-cookie';
const PROBE_API_KEY = 'probe-key-console-cookie';

// 与控制台"不再持有凭据"有关的历史符号。判据一律落在**可执行部分**（见 liveCode）。
//
//	ADMIN_INJECTED   旧模型的两个哨兵（服务端不再替换 ⇒ 旧判据恒假）
//	ADMIN_TOKEN      旧模型里存令牌的变量；⚠ 它是 `__WB2API_ADMIN_TOKEN__` 的子串，
//	                 而那两行占位符自赋值**要保留**（webui_cols_test.go 守着它）
//	                 ⇒ 必须用词边界匹配，否则那个占位符会被误判成"凭据回来了"
//	wb2api.key       旧 api_key 的 localStorage 键名（用户报障 ② 的根因）
const WORD_ADMIN_TOKEN = /(?<![A-Za-z0-9_])ADMIN_TOKEN(?![A-Za-z0-9_])/;
const WORD_ADMIN_INJECTED = /(?<![A-Za-z0-9_])ADMIN_INJECTED(?![A-Za-z0-9_])/;
const WB2API_KEY_KEY = /wb2api\.key(?![A-Za-z0-9_])/;
const OLD_STORAGE_KEY = "localStorage.setItem('wb2api.key'";

// 自检用例：好桩必须让探针全绿，坏桩必须让探针变红。
//
// 每个坏桩都对应上面五条退化形态里的一条（外加一条回归），不是随便挑的：
const SELFTEST_CASES = [
  { desc: '好桩 ①：/ui 三条路 + /status 新口径全按契约实现 ⇒ 探针必须全绿', expect: 'pass', stub: {} },
  { desc: '坏桩 ①：未登录也吐控制台外壳（门禁从根上没了）⇒ 探针必须变红', expect: 'fail', stub: { defects: { console_serves_shell_when_anon: true } } },
  { desc: '坏桩 ②：外壳里没有 probeSession（页面不再问"我是谁"）⇒ 探针必须变红', expect: 'fail', stub: { defects: { console_no_probe: true } } },
  { desc: '坏桩 ③：外壳 HTML 里注入 admin_token（泄密换了个地方）⇒ 探针必须变红', expect: 'fail', stub: { defects: { ui_console_leaks_token: true } } },
  { desc: '坏桩 ④：/status 放行 api_key（账号池画像又对客户端开放）⇒ 探针必须变红', expect: 'fail', stub: { defects: { status_api_key_ok: true } } },
  { desc: '坏桩 ⑤：/status 不做角色判定（成员也能读管理面数据）⇒ 探针必须变红', expect: 'fail', stub: { defects: { status_no_role_check: true } } },
  { desc: '坏桩 ⑥：登录页里混进 mk- 密钥（回归：ui_leaks_key 的注入点搬到了登录页）⇒ 探针必须变红', expect: 'fail', stub: { defects: { ui_leaks_key: true } } },
];

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// liveCode 去掉 HTML 注释块与 `//` 行注释，只留"浏览器真的会执行的部分"。
//
// # 为什么不能直接在原文里找那几个词（与 Go 侧同一口径）
//
// internal/server/webui_console_auth_test.go:30 早就踩过这两个坑，本函数照抄它的结论：
//
//	① 页面里**保留着历史说明**（"旧实现把凭据明文注入页面 / 记住到
//	   localStorage 'wb2api.key'"）。那些词在注释里 —— 直接 grep 原文必然命中，
//	   而那不是"凭据回来了"，是文档。
//	② `ADMIN_TOKEN` 是 `__WB2API_ADMIN_TOKEN__` 的子串 ⇒ 必须词边界匹配。
//
// ⚠ 这个判据的方向是**宽松**的：`//` 会把 `https://…` 后面一并截断，
//   所以它只能证明"活代码里没有"，证明不了"注释里没有"（后者本来就不要求）。
//   "页面里到底有没有真凭据"由 secretHits（按**值**判）负责，两条互补。
function liveCode(html) {
  const noHtmlComments = String(html || '').replace(/<!--[\s\S]*?-->/g, '');
  const out = [];
  for (const line of noHtmlComments.split('\n')) {
    const t = line.trim();
    if (t === '' || t.startsWith('//') || t.startsWith('/*') || t.startsWith('*')) continue;
    const i = line.indexOf('//');
    out.push(i >= 0 ? line.slice(0, i) : line);
  }
  return out.join('\n');
}

function countOccurrences(text, needle) {
  return String(text || '').split(needle).length - 1;
}

// hitLine 给出命中处的上下文（失败信息里"到底在哪一行"比"有没有"有用得多）。
function hitLine(text, re) {
  const lines = String(text || '').split('\n');
  for (let i = 0; i < lines.length; i += 1) if (re.test(lines[i])) return 'L' + (i + 1) + ': ' + lines[i].trim().slice(0, 110);
  return '(未命中)';
}

// firstDiff 找两段文本第一处不同的下标（逐字节比对失败时给个可查的锚点）。
function firstDiff(a, b) {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i += 1) if (a[i] !== b[i]) return i;
  return a.length === b.length ? -1 : n;
}

// ---------------------------------------------------------------------------
// 探针主体
// ---------------------------------------------------------------------------

async function runProbe(ctx) {
  const rep = ctx.reporter;
  const base = ctx.base;
  const adminToken = ctx.adminToken || '';
  const apiKey = ctx.apiKey || '';
  // 已知令牌清单：正文里**出现即泄露**。admin 与 api 两把都盯 —— 改造前后
  // 泄的正是它们，而"泄露哪一把"决定了这是"旧模型回流"还是"新bug"。
  const secrets = [].concat(ctx.secrets || [], [adminToken, apiKey]).filter(Boolean);

  const anonCli = lib.makeClient(base); // 全程**不登录**：基线 + 换手 + /status 的裸请求
  const cliA = lib.makeClient(base);    // 首个注册（空实例 ⇒ 管理员）
  const cliB = lib.makeClient(base);    // 第二个注册（⇒ 成员）
  const unameA = lib.freshUsername('p-console-a');
  const unameB = lib.freshUsername('p-console-b');
  const pass = 'probe-pass-' + lib.randToken(8);
  const reg = (cli, username) => {
    const body = { username, password: pass };
    if (ctx.captchaParam) body.captcha_param = ctx.captchaParam;
    return cli.post('/admin/auth/register', body);
  };

  rep.section('⓪ 本次验的是哪个构建（"改了没生效"第一嫌疑是验了旧构建）');
  rep.info('二进制: ' + instLib.EXE);
  try {
    const st = fs.statSync(instLib.EXE);
    rep.info('  mtime: ' + String(st.mtime) + '　大小: ' + st.size + ' 字节');
    // 只报不判：主脑可能正在改源码（此刻正是），此时"二进制比源码旧"是**事实**
    // 而不是失败 —— 但它必须可见，否则"改了没生效"会被误读成实现坏了。
    const newest = ['internal/server/webui.go', 'internal/server/webui.html', 'internal/server/webui-login.html',
      'internal/server/handler.go', 'internal/account/identify.go', 'internal/account/http.go']
      .map((rel) => { try { return { rel, m: fs.statSync(require('path').join(instLib.REPO, rel)).mtime }; } catch { return null; } })
      .filter(Boolean)
      .sort((x, y) => y.m - x.m)[0];
    if (newest && newest.m > st.mtime) {
      rep.info('  ⚠ 这个二进制比 ' + newest.rel + '（' + String(newest.m) + '）旧 —— 若刚改过源码，先 go build 再复跑。');
    }
  } catch (e) {
    rep.info('  取不到该文件: ' + lib.short(e && e.message, 80) + '（--self-test 打的是本地桩，不经过二进制）');
  }
  rep.info('目标: ' + base + (ctx.stub ? '（本地桩：lib_authapi 的 startStub）' : ''));
  rep.info('本次的两把令牌: admin_token=' + (adminToken ? lib.short(adminToken, 14) + '…' : '(未提供)')
    + '　api_key=' + (apiKey ? lib.short(apiKey, 14) + '…' : '(未提供)'));

  // ---- ① 未登录基线 -------------------------------------------------------
  rep.section('① 未登录基线：无凭据 GET /ui ⇒ 登录页（不是控制台外壳，也不含密钥）');
  const anon = await anonCli.uiAnon();
  if (anon.status !== 200) {
    rep.ok(false, 'GET /ui 无凭据 → 200（给**人**的入口回登录页，不是 401）',
      `status=${anon.status} body=${lib.short(anon.text, 120)}`);
    rep.info('⇒ 确认目标地址与实例是否起来（--start 可起一个隔离空实例）。');
    return;
  }
  const anonIsLogin = anon.text.includes(lib.LOGIN_FORM_MARKER) && anon.text.includes(lib.LOGIN_ENDPOINT);
  rep.ok(anonIsLogin, '未登录拿到的是**登录页**（含 ' + lib.LOGIN_FORM_MARKER + ' 与 ' + lib.LOGIN_ENDPOINT + '）',
    '含 form=' + anon.text.includes(lib.LOGIN_FORM_MARKER) + ' 含端点=' + anon.text.includes(lib.LOGIN_ENDPOINT) + ' ' + lib.short(anon.text, 100));
  rep.ok(!lib.SHELL_MARKER_RE.test(anon.text),
    '★ 未登录**拿不到控制台外壳**（正文里没有 probeSession）—— 这是门禁存在的直接证据',
    lib.short(anon.text, 140));
  const hitsAnon = lib.secretHits(anon.text, secrets);
  rep.ok(hitsAnon.length === 0,
    '★ 未登录的页面正文里不含任何已知密钥（本次盯 ' + secrets.length + ' 把）也不含 mk-/sk- 形状',
    hitsAnon.join(' | '));
  const anonPagedLive = liveCode(anon.text);
  rep.ok(!WORD_ADMIN_TOKEN.test(anonPagedLive) && !WORD_ADMIN_INJECTED.test(anonPagedLive),
    '登录页的可执行脚本里没有旧的哨兵/令牌变量（ADMIN_INJECTED / ADMIN_TOKEN）',
    hitLine(anonPagedLive, WORD_ADMIN_INJECTED) + ' ' + hitLine(anonPagedLive, WORD_ADMIN_TOKEN));

  // ---- ② 登录后进控制台 ---------------------------------------------------
  rep.section('② 登录后进控制台：注册 → 登录 → 带会话 cookie GET /ui');
  const regA = await reg(cliA, unameA);
  if (regA.status !== 200 || !(regA.json && regA.json.ok === true)) {
    const why = lib.missingReason(regA);
    rep.ok(false, '端点可用：POST /admin/auth/register 必须按契约应答（②~④ 全靠它建号）',
      `status=${regA.status} ${lib.short(regA.text, 120)}`);
    if (why) for (const line of lib.missingHint(regA, base)) rep.info(line);
    else rep.info('⇒ 注册没成功后面的账号就都没有前置条件，本次收尾（失败原因见上）。');
    return;
  }
  // 首启路径的证据：空库的第一个注册走 CreateBootstrap ⇒ 建成**管理员**。
  if (ctx.fresh || ctx.stub) {
    rep.ok(regA.json.bootstrap === true,
      '空库的首个注册走 bootstrap（回执 "bootstrap":true ⇒ 这一把是管理员）', lib.short(regA.json, 140));
  } else {
    rep.skip('共享实例：库可能非空 ⇒ 首个注册未必是 bootstrap，本项不判（--start 可得确定结论）');
  }

  const loginA = await cliA.login(unameA, pass);
  rep.ok(loginA.status === 200 && loginA.json && loginA.json.ok === true,
    '登录成功 → 200 {"ok":true}', `status=${loginA.status} body=${lib.short(loginA.text, 110)}`);
  const roleA = loginA.json && loginA.json.user ? loginA.json.user.role : 0;

  const shell = await cliA.ui();
  rep.ok(shell.status === 200, '带会话 cookie GET /ui → 200', `status=${shell.status}`);
  const shellIsConsole = lib.SHELL_MARKER_RE.test(shell.text);
  rep.ok(shellIsConsole, '★ 拿到的是**控制台外壳**（含 probeSession 这条"问我是谁"的请求）',
    lib.short(shell.text, 160));
  const hitsShell = lib.secretHits(shell.text, secrets);
  rep.ok(hitsShell.length === 0,
    '★ 已认证的控制台外壳正文里**仍不含**任何密钥（凭据在 HttpOnly cookie 里，不在页面里）',
    hitsShell.join(' | '));

  // ---- ③ ?token= 换手 ----------------------------------------------------
  rep.section('③ ?token= 换手：URL 里的令牌 → 302 + HttpOnly 令牌 cookie（URL 不再留令牌）');
  if (!adminToken) {
    rep.skip('本次没有 admin_token（--base 模式请设 WB2API_ADMIN_TOKEN）⇒ 换手与"哪一把能进控制台"几项测不到；--start 会自动带上');
  } else {
    // 裸请求：**一个 cookie 都不带**。带会话的话"换手失败"会被"其实靠会话进来了"掩盖。
    const swap = await anonCli.uiWith(adminToken);
    rep.ok(swap.status === 302, 'GET /ui?token=<admin_token> → 302（不是直接 200 渲染外壳）',
      `status=${swap.status} location=${(swap.headers && swap.headers.location) || '(无)'}`);
    rep.ok(String((swap.headers && swap.headers.location) || '') === '/ui',
      'Location 指向干净的 /ui（令牌不留在地址栏/历史记录里）', (swap.headers && swap.headers.location) || '(无)');
    const tc = lib.findCookie(swap.setCookie, lib.ADMIN_COOKIE);
    rep.ok(Boolean(tc), 'Set-Cookie 里确实设了 ' + lib.ADMIN_COOKIE,
      (swap.setCookie || []).join(' | ') || '(没有 Set-Cookie)');
    if (tc) {
      rep.ok(tc.value === adminToken, lib.ADMIN_COOKIE + ' 的值就是出示的那把令牌', lib.short(tc.value, 20));
      // ★ 这三条必须解析**真实属性**，不能用"回执文本里出现过 HttpOnly"那种写法
      rep.ok(tc.attrs.httponly === true, '★ 令牌 cookie 带 HttpOnly（页面 JS 读不到 ⇒ XSS 偷不走）', tc.raw);
      rep.ok(String(tc.attrs.samesite || '').toLowerCase() === 'lax', 'SameSite=Lax（挡住绝大多数 CSRF 形态）', tc.raw);
      rep.ok(tc.attrs.path === '/', 'Path=/（/ui 与 /admin/* 都带得上）', tc.raw);
    }
    const byCookie = await anonCli.uiWithCookie(adminToken);
    rep.ok(byCookie.status === 200 && lib.SHELL_MARKER_RE.test(byCookie.text),
      '★ 只带令牌 cookie（不带会话）GET /ui → 控制台外壳（换来的那份真的能用）',
      `status=${byCookie.status} probeSession=${lib.SHELL_MARKER_RE.test(byCookie.text)}`);

    // ★ 错误令牌：与"压根没带"逐字节相同 ⇒ 这个端点不能当"令牌对不对"的 oracle。
    // ⚠ 基线必须**现取**：登录页正文按"账号库空不空"分叉（bootstrap 标志位），
    //   与 ① 段（注册前）取的那份天然不同 —— 拿它们比会红得莫名其妙。
    const baseline = await anonCli.uiAnon();
    const wrong = await anonCli.uiWith('definitely-not-the-token-' + lib.randToken(8));
    rep.ok(wrong.status === 200 && wrong.text.includes(lib.LOGIN_FORM_MARKER),
      '错误令牌 → 200 登录页（不是 401/403，更不是控制台外壳）',
      `status=${wrong.status} 含 form=${wrong.text.includes(lib.LOGIN_FORM_MARKER)} ${lib.short(wrong.text, 90)}`);
    const same = wrong.text === baseline.text;
    rep.ok(same,
      '★★ 错误令牌的响应体与同刻"压根没带令牌"**逐字节相同**（否则端点成了令牌判定 oracle）',
      same ? '' : `长度 ${wrong.text.length} vs ${baseline.text.length}，首处差异 @${firstDiff(wrong.text, baseline.text)}`);
  }

  // ---- ④ /status 新口径 ---------------------------------------------------
  rep.section('④ /status 新口径：控制台身份 + **必须管理员**（api_key 一律 401）');
  // 下面几条都走**裸请求**：一次只出示一个凭据，否则会话 cookie 会把结论改掉（见文件头 ⓐ）。
  const noCred = await anonCli.statusAnon();
  rep.ok(noCred.status === 401, '无凭据 GET /status → 401',
    `status=${noCred.status} body=${lib.short(noCred.text, 110)}`);
  rep.ok(lib.resCode(noCred) === 'auth_required',
    '401 的 code = auth_required（前端据此跳登录页，而不是显示"没权限"）', lib.resCode(noCred));

  if (apiKey) {
    const byKey = await anonCli.statusBearer(apiKey);
    rep.ok(byKey.status === 401,
      '★★ 拿 **api_key**（对话凭据）读 /status → 401（账号池画像不再对客户端开放）',
      `status=${byKey.status} body=${lib.short(byKey.text, 110)}`);
    rep.ok(!(byKey.status === 200), '（同一条的反面）api_key **不能**读 /status —— 这是本轮破坏性变更的要点',
      `status=${byKey.status}`);
  } else {
    rep.skip('本次没有 api_key（--base 模式请设 WB2API_API_KEY）⇒ "api_key 必须 401"测不到；--start 会自动带上');
  }

  if (adminToken) {
    const byAdmin = await anonCli.statusBearer(adminToken);
    rep.ok(byAdmin.status === 200,
      '拿 **admin_token** 读 /status → 200（脚本/CI 那条路仍然通）',
      `status=${byAdmin.status} body=${lib.short(byAdmin.text, 110)}`);
  } else {
    rep.skip('本次没有 admin_token ⇒ "管理令牌能读 /status"测不到');
  }

  // 管理员会话：空实例的首个注册就是管理员（见文件头 ⓒ）。
  if (roleA === 10) {
    const adm = await cliA.status();
    rep.ok(adm.status === 200, '带**管理员会话** cookie 读 /status → 200',
      `status=${adm.status} body=${lib.short(adm.text, 110)}`);
  } else {
    rep.skip('首个账号实测 role=' + roleA + '（不是管理员）⇒ 管理员会话那条本次不判；--start 起空实例可得确定结论');
  }

  // 成员会话：必须来自**第二次注册**（第一次是 bootstrap 管理员，拿它测 403 必然错）。
  rep.info('成员会话用**第二个注册**的账号（' + unameB + '）：空实例的首个注册是 bootstrap 管理员，不能当成员用');
  const regB = await reg(cliB, unameB);
  let roleB = 0;
  if (regB.status === 200 && regB.json && regB.json.ok === true) {
    const loginB = await cliB.login(unameB, pass);
    roleB = loginB.json && loginB.json.user ? loginB.json.user.role : 0;
  } else if (ctx.fresh) {
    // 隔离实例上第二个注册必须成功（库非空 ⇒ 只建成员）。它失败等于成员这条没法验。
    rep.ok(false, '第二个注册必须成功（隔离实例、库已非空 ⇒ 建成成员）',
      `status=${regB.status} code=${lib.resCode(regB)} body=${lib.short(regB.text, 110)}`);
  } else {
    rep.skip('本实例不接受第二个注册（status=' + regB.status + ' code=' + lib.resCode(regB) + '）⇒ 成员那条测不到；--start 可得确定结论');
  }

  // 按**实测角色**挑账号：共享实例上"第一个"未必是管理员。
  const member = roleB === 1 ? { cli: cliB, name: unameB } : (roleA === 1 ? { cli: cliA, name: unameA } : null);
  if (member) {
    const mem = await member.cli.status();
    rep.ok(mem.status === 403, '★★ 带**成员会话** cookie 读 /status → 403（账号池读数仅管理员可见）',
      `status=${mem.status} 账号=${member.name} role=${member.role || ''} body=${lib.short(mem.text, 110)}`);
    rep.ok(lib.resCode(mem) === 'forbidden_role',
      '403 的 code = forbidden_role（把"没登录"与"权限不够"分开，前端才能正确提示）', lib.resCode(mem));
  } else {
    rep.skip('本次没有实测到成员账号（roleA=' + roleA + ' roleB=' + roleB + '）⇒ "成员必须 403"测不到');
  }

  // ---- ⑤ 控制台脚本不再持有凭据 -------------------------------------------
  rep.section('⑤ 控制台脚本不再持有凭据（源码级判据，只看可执行部分）');
  if (!shellIsConsole) {
    rep.skip('② 没拿到控制台外壳（拿到的是登录页/别的）⇒ 本组在它上面判定没有意义，先修 ② 那条');
  } else {
    const live = liveCode(shell.text);
    rep.ok(live.indexOf('if (ADMIN_INJECTED') < 0,
      '可执行脚本里没有 `if (ADMIN_INJECTED` —— 服务端不再替换哨兵，旧判据恒假会让已登录的人被告知"未获授权"',
      hitLine(live, /if \(ADMIN_INJECTED/));
    rep.ok(live.indexOf(OLD_STORAGE_KEY) < 0,
      '可执行脚本里没有 ' + OLD_STORAGE_KEY + ' —— 那是「记住 api_key」的旧实现（用户报障 ② 的根因）',
      hitLine(live, WB2API_KEY_KEY));
    rep.ok(!WB2API_KEY_KEY.test(live),
      '可执行脚本里没有任何 wb2api.key 键名（含双引号/变量拼接等变体）', hitLine(live, WB2API_KEY_KEY));
    rep.ok(!WORD_ADMIN_TOKEN.test(live),
      '可执行脚本里没有独立出现的 ADMIN_TOKEN 变量（页面不再持有令牌；占位符 __WB2API_ADMIN_TOKEN__ 不算）',
      hitLine(live, WORD_ADMIN_TOKEN));
    rep.ok(!WORD_ADMIN_INJECTED.test(live),
      '可执行脚本里没有 ADMIN_INJECTED 哨兵（旧凭据模型的遗迹）', hitLine(live, WORD_ADMIN_INJECTED));
    // 参考信息：说明"剥掉的都是注释"，避免上面几条被误读成"页面里从没提过这些词"。
    rep.info('（参考）整页原文里 `if (ADMIN_INJECTED` 出现 ' + countOccurrences(shell.text, 'if (ADMIN_INJECTED')
      + ' 次、ADMIN_TOKEN ' + countOccurrences(shell.text, 'ADMIN_TOKEN')
      + ' 次、wb2api.key ' + countOccurrences(shell.text, 'wb2api.key')
      + ' 次 —— 都落在注释/占位符里（可执行部分为 0 才是判据）');
    rep.ok(lib.secretHits(shell.text, secrets).length === 0,
      '★ 外壳里既没有旧模型的符号，也**没有真令牌的值**（值与符号两条判据互补，见 liveCode 注释）',
      lib.secretHits(shell.text, secrets).join(' | '));
  }
}

lib.runCli({
  title: '控制台鉴权（HttpOnly 会话 cookie）验收（E2E）',
  runProbe,
  cases: SELFTEST_CASES,
  apiKey: PROBE_API_KEY,
  // 隔离实例必须两把令牌不相等：否则"api_key 读 /status 必须 401"是在拿同一把
  // 钥匙比对自身，必然 200 —— 那条断言会变成恒真（见 lib 的 runCli 注释）。
  instanceConfig: { admin_token: PROBE_ADMIN_TOKEN },
}).then((code) => process.exit(code));
