// verify_login_flow_e2e.js —— 登录流程**正路**的验收探针（给主脑"改完必须全绿"用的尺子）。
//
// # 为什么是这个形状
//
// 主脑正在实现 `/admin/auth/*`（注册 / 登录 / 会话 / 登出）与"未登录不许看
// 控制台"的门禁。这类改动最容易出的不是崩溃，而是**静默放行**：
//
//	· 错口令也发了会话（判定分支写反，页面照样进得去）
//	· `Set-Cookie` 少了 HttpOnly（浏览器照常工作，只有看见响应头才知道）
//	· 登出只清了客户端 cookie，服务端那条会话还在（旧 cookie 依旧可用）
//	· 未登录时 `/ui` 的 HTML 里带着密钥（打开页面即泄露）
//
// 这些都不会让任何"看起来像测试"的东西变红，所以本探针只认**响应字节**：
// HttpOnly 是解析 `Set-Cookie` 属性得到的，会话有效是"带 cookie 拿到 200"。
//
// # 三种用法
//
//	node tests/frontend/verify_login_flow_e2e.js                 # 打 WB2API_BASE（缺省 http://127.0.0.1:7885）
//	node tests/frontend/verify_login_flow_e2e.js --start         # 探针自己起一个隔离空实例（7800 段端口）
//	node tests/frontend/verify_login_flow_e2e.js --self-test     # 打本地桩，验证**探针自身有判别力**
//
// 端点还没实现时，默认模式会**清晰报红**并说明"端点未实现" —— 那是刻意的，
// 别把它读成"探针坏了"，也别为了让它变绿去改 Go 代码。
'use strict';
const lib = require('./lib_authapi.js');

// 自检用例：好桩必须让探针全绿，坏桩必须让探针变红。
//
// 五个坏桩不是随便挑的，每个都对应上面"最容易静默放行"的一条：
const SELFTEST_CASES = [
  { desc: '好桩 ①：完全按契约实现（注册不限验证码）⇒ 探针必须全绿', expect: 'pass', stub: {} },
  { desc: '好桩 ②：注册开了人机校验且真的拦住（探针带答案注册）⇒ 探针必须全绿', expect: 'pass', stub: { captcha: true } },
  { desc: '坏桩 ①：错口令也回 200 + 发会话（判定写反）⇒ 探针必须变红', expect: 'fail', stub: { defects: { login_wrong_password_200: true } } },
  { desc: '坏桩 ②：Set-Cookie 少了 HttpOnly ⇒ 探针必须变红', expect: 'fail', stub: { defects: { cookie_missing_httponly: true } } },
  { desc: '坏桩 ③：未登录也回 200（会话与 /admin/accounts 都不设防）⇒ 探针必须变红', expect: 'fail', stub: { defects: { anon_open: true } } },
  { desc: '坏桩 ④：登出只清客户端，服务端会话没撤（旧 cookie 仍可用）⇒ 探针必须变红', expect: 'fail', stub: { defects: { logout_no_revoke: true } } },
  { desc: '坏桩 ⑤：/ui 的 HTML 里混进了 mk- 密钥 ⇒ 探针必须变红', expect: 'fail', stub: { defects: { ui_leaks_key: true } } },
];

// runProbe 一段代码同时服务两种模式：--self-test 时 base 是本地桩，
// 默认模式时 base 是真实实例。所以这里**不能**有任何"桩专用"的分支。
async function runProbe(ctx) {
  const rep = ctx.reporter;
  const cli = lib.makeClient(ctx.base);
  const secrets = ctx.secrets || [];
  // 每跑一次都换名字：同一个实例上反复跑不该因为"用户名已占"而红。
  const uname = lib.freshUsername('p-login');
  const pass = 'probe-pass-' + lib.randToken(8);
  const post = (username, password) => {
    const body = { username, password };
    if (ctx.captchaParam) body.captcha_param = ctx.captchaParam; // 桩/实例开了人机校验时的答案
    return cli.post('/admin/auth/register', body);
  };

  rep.section('① 未登录基线');
  const anon = await cli.session();
  const why = lib.missingReason(anon);
  if (why) {
    rep.ok(false, '端点可用：GET /admin/auth/session 必须按契约应答', `status=${anon.status} ${lib.short(anon.text, 120)}`);
    for (const line of lib.missingHint(anon, ctx.base)) rep.info(line);
    return;
  }
  rep.ok(anon.status === 401, 'GET /admin/auth/session 未登录 → 401', `status=${anon.status} body=${lib.short(anon.text, 90)}`);
  rep.ok(lib.SESSION_401_CODES.includes(lib.errCode(anon)),
    '401 的 code 落在契约集合内（' + lib.SESSION_401_CODES.join('/') + '）', lib.errCode(anon));

  const anonAcc = await cli.accounts();
  rep.ok(anonAcc.status === 401, 'GET /admin/accounts 未登录 → 401', `status=${anonAcc.status} body=${lib.short(anonAcc.text, 90)}`);

  const ui = await cli.ui();
  rep.ok(ui.status === 200, 'GET /ui 未登录 → 200（静态外壳本身可加载）', `status=${ui.status}`);
  const hits = lib.secretHits(ui.text, secrets);
  rep.ok(hits.length === 0, '★ /ui 的 HTML 里不含任何密钥（mk-/sk- 形状与已知令牌）', hits.join(' | '));

  rep.section('② 注册');
  const reg = await post(uname, pass);
  if (lib.errCode(reg) === 'captcha_required') {
    rep.ok(false, '注册可用：本次注册被人机校验拦住', `status=${reg.status} ${lib.short(reg.text, 100)}`);
    rep.info('⇒ 拿不到登录路径的证据。请对**关闭注册人机校验**的实例跑本探针，');
    rep.info('   或把答案放进 WB2API_CAPTCHA_PARAM（--start 起的隔离实例默认不需要）。');
    return;
  }
  const regOK = rep.ok(reg.status === 200 && reg.json && reg.json.ok === true,
    'POST /admin/auth/register 新用户名 → 200 {"ok":true}', `status=${reg.status} body=${lib.short(reg.text, 120)}`);
  if (!regOK) {
    rep.info('注册不成立 ⇒ 后面的登录步骤没有前置条件，就此收尾（本条的失败原因见上）');
    return;
  }

  const dup = await post(uname, pass);
  rep.ok(dup.status === 409 && lib.errCode(dup) === 'username_taken',
    '同名重复注册 → 409 username_taken', `status=${dup.status} code=${lib.errCode(dup)}`);
  const dupCase = await post(uname.toUpperCase(), pass);
  rep.ok(dupCase.status === 409,
    '大小写变体同名 → 409（比较口径是 normalizeUsername，见 internal/account/account.go:165）',
    `status=${dupCase.status} code=${lib.errCode(dupCase)}`);

  rep.section('③ 登录');
  const wrong = await cli.login(uname, 'wrong-' + pass);
  rep.ok(wrong.status === 401 && lib.errCode(wrong) === 'bad_credentials',
    '错口令 → 401 bad_credentials', `status=${wrong.status} code=${lib.errCode(wrong)}`);

  const login = await cli.login(uname, pass);
  const lj = login.json || {};
  const u = lj.user || {};
  rep.ok(login.status === 200 && lj.ok === true, '正确口令 → 200 {"ok":true}', `status=${login.status} body=${lib.short(login.text, 120)}`);
  rep.ok(Boolean(u.id) && u.username === uname && typeof u.role === 'number' && Boolean(u.role_name),
    '回执 user 含 id/username/role/role_name（role_name 非空）', lib.short(u, 120));

  const sc = lib.findCookie(login.setCookie, lib.COOKIE);
  rep.ok(Boolean(sc), 'Set-Cookie 里确实设了 ' + lib.COOKIE, login.setCookie.join(' | ') || '(没有 Set-Cookie)');
  if (sc) {
    rep.ok(lib.SID_RE.test(sc.value), lib.COOKIE + ' 的值是 64 位十六进制（32 字节随机）', lib.short(sc.value, 24));
    // ★ 这三条必须解析真实属性，不能用"回执文本里出现过 HttpOnly"这种写法
    rep.ok(sc.attrs.httponly === true, '★ Set-Cookie 真的带 HttpOnly', sc.raw);
    rep.ok(String(sc.attrs.samesite || '').toLowerCase() === 'lax', 'SameSite=Lax', sc.raw);
    rep.ok(sc.attrs.path === '/', 'Path=/', sc.raw);
  }

  rep.section('④ 带 cookie 的会话与受保护端点');
  const sess = await cli.session();
  rep.ok(sess.status === 200 && sess.json && sess.json.ok === true && sess.json.user,
    '带 cookie 查会话 → 200 {"ok":true,"user":{…}}', `status=${sess.status} body=${lib.short(sess.text, 120)}`);
  rep.ok(sess.json && sess.json.user && sess.json.user.id === u.id,
    '会话回执的 user.id 与登录回执一致', sess.json && sess.json.user && sess.json.user.id);

  const acc = await cli.accounts();
  rep.ok(acc.status === 200 || acc.status === 403,
    '带 cookie 访问 /admin/accounts → 200（管理员）或 403（成员）', `status=${acc.status} body=${lib.short(acc.text, 90)}`);
  // 角色与结果必须自洽：403 只能给"成员"，200 只能给"管理员"。
  // 不一致说明"回执里的角色"和"真正判权的角色"是两套口径。
  const isAdmin = u.role === 10; // 10 = RoleAdmin，见 internal/account/account.go:43
  rep.ok((acc.status === 200) === isAdmin,
    '角色与结果自洽（role=' + u.role + ' ⇒ 期望 ' + (isAdmin ? '200' : '403') + '）', `status=${acc.status}`);

  rep.section('⑤ 登出与会话失效');
  const oldSid = sc ? sc.value : '';
  const out = await cli.logout();
  rep.ok(out.status === 200 && out.json && out.json.ok === true,
    'POST /admin/auth/logout → 200 {"ok":true}', `status=${out.status} body=${lib.short(out.text, 90)}`);
  const del = lib.findCookie(out.setCookie, lib.COOKIE);
  rep.ok(Boolean(del) && del.value === '' && Number(del.attrs['max-age']) <= 0,
    '登出回执清 cookie（' + lib.COOKIE + '=; Max-Age=0）', out.setCookie.join(' | ') || '(没有 Set-Cookie)');
  rep.ok(!cli.jar.has(lib.COOKIE), '客户端 cookie 罐按 Max-Age=0 把它丢掉了（回放语义与浏览器一致）', cli.cookieHeader() || '(空)');

  const after = await cli.session();
  rep.ok(after.status === 401, '登出后再查会话 → 401', `status=${after.status} body=${lib.short(after.text, 90)}`);

  if (oldSid) cli.setCookie(lib.COOKIE, oldSid); // 手动回放"登出前那条" cookie
  const replay = await cli.session();
  rep.ok(replay.status === 401,
    '★ 拿**登出前的旧 cookie** 再打会话 → 401（服务端真的撤销了，不只是清了客户端）',
    `status=${replay.status} body=${lib.short(replay.text, 90)}`);
  const replayAcc = await cli.accounts();
  rep.ok(replayAcc.status === 401, '旧 cookie 访问 /admin/accounts → 401', `status=${replayAcc.status}`);
  cli.clearCookie();
}

lib.runCli({
  title: '登录流程验收（正路 E2E）',
  runProbe,
  cases: SELFTEST_CASES,
  apiKey: 'probe-key-login',
}).then((code) => process.exit(code));
