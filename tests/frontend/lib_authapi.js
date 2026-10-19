// lib_authapi.js —— 「登录流程」验收探针的**共享层**（2026-10-19 抽）。
//
// # 为什么单独一层（AGENTS 第 6 条）
//
// 两个探针（verify_login_flow_e2e / verify_register_abuse_e2e）要做同一批动作：
// 注册、登录、带 Cookie 打后续请求、登出、查会话。其中最容易写错、也最该
// 只有一处实现的是三件东西：
//
//	① **Cookie 罐**：登录拿到的 `wb2api_sid` 要能被后续请求原样回放，
//	   并且登出回执里的 `Max-Age=0` 必须让罐里那条**消失**。第一版探针
//	   常见写法是"把 Set-Cookie 字符串存起来、后面手动拼 Cookie 头" ——
//	   那样"登出后旧 cookie 必须 401"永远测不出真假（客户端自己没清）。
//	② **桩服务器（stub）**：`--self-test` 要能起一个完全按契约实现的假服务端，
//	   还要能**按开关故意坏掉**。判别力靠它，所以它必须是同一份实现，
//	   不能两个探针各写一套（否则"探针有判别力"这件事本身没有可复现的证据）。
//	③ **三态入口**：默认打 `WB2API_BASE`、`--start` 起隔离实例、`--self-test`
//	   打本地桩。三个探针共用同一段，避免"某个探针漏了 --start"这类静默差异。
//
// # 冻结的 HTTP 契约（主脑实现，本层只**照着量**，不参与实现）
//
//	POST /admin/auth/register  {username,password}
//	    200 {"ok":true} / 400 bad_username|bad_password / 409 username_taken / 429 rate_limited
//	POST /admin/auth/login     {username,password}
//	    200 {"ok":true,"user":{id,username,role,role_name}}
//	        + `Set-Cookie: wb2api_sid=<64hex>; HttpOnly; SameSite=Lax; Path=/`
//	    401 bad_credentials / 403 disabled / 429 rate_limited
//	GET  /admin/auth/session   200 {"ok":true,"user":{…}}
//	    401 auth_required|session_expired|session_revoked|auth_invalid
//	POST /admin/auth/logout    200 {"ok":true} 且 `Set-Cookie: wb2api_sid=; Max-Age=0`
//	GET  /admin/accounts       401 未登录 / 403 成员 / 200 管理员
//	GET  /ui                   未登录时 HTML 里**不含**任何密钥（mk- 前缀）
//
// # 2026-10-19 T7 增量：控制台鉴权改成「HttpOnly 会话 cookie」后的新契约
//
//	GET  /ui                   三条路（internal/server/webui.go:180 的 ui()）
//	    ?token=<admin_token>   合法 ⇒ 302 Location: /ui + `Set-Cookie: wb2api_admin_token=…`
//	                           带 HttpOnly / SameSite=Lax / Path=/
//	    ?token=<错误>           ⇒ 200 **登录页**，且与"压根没带令牌"逐字节相同
//	                           （真实实现刻意的：单独报错会把这个端点变成
//	                             "令牌对不对"的判定 oracle，见 webui.go:197）
//	    已认证（会话/令牌 cookie）⇒ 200 **控制台外壳**（含 `async function probeSession()`）
//	    未认证                   ⇒ 200 **登录页**（含 `id="form"` 与 `/admin/auth/login`）
//	GET  /status               控制台身份 + **必须管理员**（handler.go:540 consoleAdminAuth）
//	    管理员会话 → 200 / 成员会话 → 403 forbidden_role
//	    Bearer admin_token → 200 / Bearer api_key → 401 auth_required
//	POST /admin/auth/register  库空 ⇒ 建**管理员**（bootstrap，回执多 `"bootstrap":true`）；
//	                           库非空 ⇒ 只建**成员**（http.go:327）
//
// ⚠ `/status` 的错误信封是**扁平**的：`{"error":"…","code":"auth_required"}`（writeJSON
//   直接接 map），而 /admin/* 是嵌套的 `{"error":{"code":…}}`。所以本层给探针加了
//   `resCode()` 两种都认 —— 判据是 code 本身，不是信封形状。
//
// # 一条老规矩（README 第 2 条：断言要与数据对照）
//
// 本层所有断言都对着**真实字节**：HttpOnly 是解析 `Set-Cookie` 属性得到的，
// 不是"回执文本里出现过 HttpOnly 这个词"；会话有效是"带 cookie 拿到 200"，
// 不是"cookie 非空"。
'use strict';
const http = require('http');
const net = require('net');
const crypto = require('crypto');
const { httpJson, startInstance, sleep } = require('./lib_instance.js');

// ---------------------------------------------------------------------------
// 常量
// ---------------------------------------------------------------------------

// CookieName 与 `internal/account/session.go:22` 的 CookieName 必须一致。
const COOKIE = 'wb2api_sid';
// SID 是 32 字节随机数的十六进制（`internal/account/session.go:71`）。
const SID_RE = /^[0-9a-f]{64}$/;
// 会话端点的 401 code 集合（契约里列了四个，测试只认这四个）。
const SESSION_401_CODES = ['auth_required', 'session_expired', 'session_revoked', 'auth_invalid'];
const DEFAULT_BASE = 'http://127.0.0.1:7885';

// ADMIN_COOKIE 是 `internal/account/identify.go:43` 的 AdminTokenCookie。
//
// 与会话 cookie（wb2api_sid）**刻意两个名字**：前者是机器令牌的浏览器副本
// （`/ui?token=` 换手来的，无账号、等价管理员），后者是人登录出来的会话。
// 探针要分别验证"哪一条能进控制台"，所以名字必须各自成立。
const ADMIN_COOKIE = 'wb2api_admin_token';

// 登录页与外壳的**稳定标记**：桩与探针共用同一份，避免两边各写一遍后漂移
// （桩按 B 写、探针按 A 判 ⇒ 好桩也变红，而那种红查起来最费时间）。
//
//	LOGIN_FORM_MARKER  `id="form"`            internal/server/webui-login.html:131
//	LOGIN_ENDPOINT     `/admin/auth/login`    同文件 :278（页面上真的会打这个端点）
//	SHELL_MARKER_RE    `async function probeSession()`  internal/server/webui.html
//	                   （webui_unauth_test.go:66 也钉着这个锚点）
const LOGIN_FORM_MARKER = 'id="form"';
const LOGIN_ENDPOINT = '/admin/auth/login';
const SHELL_MARKER_RE = /async\s+function\s+probeSession\s*\(/;

// 桩的两把固定测试令牌：**必须互不相等**。
//
// 理由很硬：`/status` 的新口径就是"api_key 不再是控制台凭据"。若桩把它们设成
// 同一个值，"拿 api_key 读 /status 必须 401"这条断言就永远测不出真假
// —— 那正好是这次改造要防的静默退化。
const STUB_ADMIN_TOKEN = 'stub-admin-token-0001';
const STUB_API_KEY = 'stub-api-key-0001';

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

const sleepMs = sleep;

function randToken(n) {
  return crypto.randomBytes(Math.ceil((n || 16) / 2)).toString('hex').slice(0, n || 16);
}

// freshUsername 造一个**本次进程独有**的合法用户名。
//
// 为什么不用固定名字：探针会在同一个实例上跑很多次（主脑改一次跑一次），
// 固定名字第二次就变成 409，红得莫名其妙。长度必须落在
// `internal/account/account.go:66-71` 的 3–32 且字符集是白名单
// （字母数字与 `_ - . @`）。
function freshUsername(prefix) {
  const head = String(prefix || 'probe').replace(/[^A-Za-z0-9_.-]/g, '').slice(0, 12) || 'probe';
  return (head + '-' + Date.now().toString(36) + randToken(6)).slice(0, 32);
}

function short(v, n) {
  const s = v === null || v === undefined ? '' : (typeof v === 'string' ? v : JSON.stringify(v));
  const lim = n || 90;
  return s.length > lim ? s.slice(0, lim) + '…' : s;
}

// hist 把 {200:3,409:1} 排成 "200×3 409×1"（失败时说清"到底看到了什么"）。
function hist(codes) {
  const keys = Object.keys(codes || {}).sort();
  return keys.length ? keys.map((k) => k + '×' + codes[k]).join(' ') : '(无响应)';
}

function errCode(res) {
  return res && res.json && res.json.error ? res.json.error.code : undefined;
}

// resCode 取"稳定错误码"，**两种信封都认**：
//
//	嵌套  {"error":{"code":"auth_required"}}         /admin/*（admin 门禁）
//	扁平  {"error":"一句人话","code":"auth_required"}  /status（handler.go 的 consoleAdminAuth）
//
// 为什么不直接把 errCode 改成两种都认：那会**改动已有导出的语义**，三个老探针
// 都按"嵌套"在判，改共享函数等于在它们脚下换地板。所以新增一个，老的照旧。
function resCode(res) {
  const nested = errCode(res);
  if (nested) return nested;
  return res && res.json && typeof res.json.code === 'string' ? res.json.code : undefined;
}

const isRateLimited = (res) => !!res && res.status === 429;

// hugeBody 造一段**合法 JSON** 的超长注册请求体。
//
// ⚠ 用 JSON 而不是随机字节：随机字节会被 JSON 解析拒绝，于是"413"与
//   "400 解析失败"就分不开了 —— 那条断言会退化成"随便什么拒绝都算过"。
function hugeBody(bytes) {
  const pad = 'x'.repeat(Math.max(0, bytes - 200));
  return JSON.stringify({ username: 'p-huge-' + Date.now().toString(36), password: pad });
}

// missingReason 报告"这个端点看起来根本没实现"，返回 '' 表示看起来是实现了。
//
// 目的很具体：真实端点现在还没写，默认模式跑起来必须**清晰**说明"端点未实现"，
// 而不是把 404 的 HTML 当成"断言失败"让主脑去猜。
function missingReason(res) {
  if (!res) return '没有响应';
  if (res.status === 0) return '连不上：' + short(res.text, 60);
  if (res.status === 404 || res.status === 405 || res.status === 501) return 'HTTP ' + res.status;
  if (res.status === 200 && /^\s*</.test(res.text || '')) return 'HTTP 200 但回的是 HTML（SPA 回退），不是 JSON';
  return '';
}

// missingHint 把 missingReason 的结论翻译成**可照做**的提示行。
//
// 分开写是因为"连不上"与"没有这个路由"是两件事：前者是探针用错了目标
// （或实例没起），后者才是产品还没实现。混成一句话会让主脑去查错方向。
function missingHint(res, base) {
  if (res && res.status === 0) {
    return [
      '⇒ 连不上 ' + base + '：这个地址上没有实例在听。',
      '   要么先把实例起起来，要么用 --start 让探针自己起一个隔离空实例。',
    ];
  }
  return [
    '⇒ 端点未实现：' + base + ' 上没有 /admin/auth/* 路由（或 --base 指错了目标）。',
    '   本探针在端点未实现时**故意报红**，不要把它读成"探针坏了"。',
  ];
}

// secretHits 在 HTML 里搜"不该出现的密钥"，返回命中的片段。
//
// 判据是**形状**而不是某个具体字符串：`mk-` / `sk-` 后面跟 ≥16 位十六进制
// （`internal/apikey/apikey.go:458` 生成的就是 `mk-` + 64 hex）。加负向后顾
// 是因为页面里本来就有 `ltask-…` 这类 id（实测 `internal/server/webui.html`
// 里 `ltask-sum-…` 会撞上天真写法），那种不是密钥。
function secretHits(html, secrets) {
  const hits = [];
  const text = String(html || '');
  for (const re of [/(?<![A-Za-z0-9_])mk-[0-9a-fA-F]{16,}/g, /(?<![A-Za-z0-9_])sk-[0-9a-fA-F]{16,}/g]) {
    const m = text.match(re);
    if (m) hits.push(...m.slice(0, 3));
  }
  for (const s of secrets || []) {
    if (s && s.length >= 8 && text.includes(s)) hits.push('已知令牌 ' + short(s, 12));
  }
  return hits;
}

// secretsFromEnv 拿"绝不该出现在 /ui 里的已知令牌"。
function secretsFromEnv() {
  return [process.env.WB2API_ADMIN_TOKEN, process.env.WB2API_API_KEY].filter(Boolean);
}

// ---------------------------------------------------------------------------
// HTTP：带 Cookie 罐的客户端
// ---------------------------------------------------------------------------

// request 发一次请求并返回**状态码 + 原文 + 响应头 + Set-Cookie 原始数组**。
//
// 为什么不直接用 `lib_instance.js` 的 httpJson：它有意只回 status/text/json
// （见那里的注释），看不到 `Set-Cookie` —— 而登录流程的核心判据
// （HttpOnly / SameSite / Max-Age=0）**只存在于响应头里**。所以这里另写一个
// 同款契约的兄弟：不抛异常、4xx/5xx 也回原文（理由与 httpJson 完全一致）。
// httpJson 仍被本层原样再导出，供"不需要看响应头"的一次性请求使用。
function request(base, p, opt) {
  const o = opt || {};
  const u = new URL(p, base);
  const body = o.body === undefined || o.body === null ? null
    : (typeof o.body === 'string' ? o.body : JSON.stringify(o.body));
  const headers = Object.assign({}, o.headers || {});
  if (body !== null && headers['Content-Type'] === undefined) headers['Content-Type'] = 'application/json';
  if (body !== null) headers['Content-Length'] = Buffer.byteLength(body);
  return new Promise((resolve) => {
    const r = http.request({
      hostname: u.hostname, port: u.port || 80, path: u.pathname + u.search,
      method: o.method || 'GET', headers,
    }, (res) => {
      const c = [];
      res.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      res.on('end', () => {
        const text = Buffer.concat(c).toString('utf8');
        let json = null;
        try { json = JSON.parse(text); } catch { }
        resolve({ status: res.statusCode, text, json, headers: res.headers, setCookie: res.headers['set-cookie'] || [] });
      });
    });
    r.on('error', (e) => resolve({ status: 0, text: String(e && e.message), json: null, headers: {}, setCookie: [] }));
    if (body !== null) r.write(body);
    r.end();
  });
}

// parseSetCookie 把 `Set-Cookie` 原文拆成 {name,value,attrs,raw}。
// attrs 全小写键名；无 `=` 的属性（HttpOnly / Secure）值为 true。
function parseSetCookie(list) {
  return (list || []).map((raw) => {
    const parts = String(raw).split(';');
    const kv = parts.shift() || '';
    const i = kv.indexOf('=');
    const name = (i < 0 ? kv : kv.slice(0, i)).trim();
    const value = i < 0 ? '' : kv.slice(i + 1).trim();
    const attrs = {};
    for (const p of parts) {
      const j = p.indexOf('=');
      const k = (j < 0 ? p : p.slice(0, j)).trim().toLowerCase();
      if (k) attrs[k] = j < 0 ? true : p.slice(j + 1).trim();
    }
    return { name, value, attrs, raw: String(raw) };
  });
}

function findCookie(list, name) {
  return parseSetCookie(list).find((c) => c.name === name) || null;
}

// makeClient 造一个"像浏览器一样"的客户端：收到的 cookie 会回放，过期的会丢。
function makeClient(base) {
  const jar = new Map();
  const c = {
    base,
    jar,
    last: null, // 最近一次原始响应（探针要看 Set-Cookie 明细）
    setCookie(name, value) {
      if (!value) jar.delete(name); else jar.set(name, String(value));
    },
    clearCookie(name) { jar.delete(name || COOKIE); },
    cookieHeader() { return [...jar].map(([k, v]) => k + '=' + v).join('; '); },
    async req(p, opt) {
      const o = Object.assign({}, opt || {});
      o.headers = Object.assign({}, o.headers || {});
      // 回放：浏览器会自动带上同源 cookie，探针必须显式模拟这一步，
      // 否则"带 cookie 查会话"永远测不到（会一直被 401，看着像产品坏了）。
      if (jar.size) o.headers.Cookie = c.cookieHeader();
      const res = await request(base, p, o);
      c.last = res;
      // 收：只认 wb2api_sid。空值 / Max-Age<=0 是**删 cookie**（登出就这么做），
      // 若只按"有值才收"处理，登出后罐里还留着旧 SID，"登出后必须 401"
      // 这一步就变成在测探针自己的 bug。
      for (const sc of parseSetCookie(res.setCookie)) {
        if (sc.name !== COOKIE) continue;
        const expired = sc.value === '' || (sc.attrs['max-age'] !== undefined && Number(sc.attrs['max-age']) <= 0);
        if (expired) jar.delete(sc.name); else jar.set(sc.name, sc.value);
      }
      return res;
    },
    get(p, opt) { return c.req(p, Object.assign({}, opt, { method: 'GET' })); },
    post(p, body, opt) { return c.req(p, Object.assign({ method: 'POST', body: body === undefined ? {} : body }, opt)); },
    register(username, password) { return c.post('/admin/auth/register', { username, password }); },
    login(username, password) { return c.post('/admin/auth/login', { username, password }); },
    session() { return c.get('/admin/auth/session'); },
    logout() { return c.post('/admin/auth/logout'); },
    accounts() { return c.get('/admin/accounts'); },
    ui() { return c.get('/ui'); },

    // ---- T7 增量：/status 与"带令牌的 /ui"（都是新增方法，不动上面任何签名）----
    //
    // ⚠ 下面三个 /ui 辅助**刻意不读 cookie 罐**（走裸 request，不走 c.req）：
    //
    //   · `uiAnon()` 要的是"**一个 cookie 都不带**"的基线。若走 c.req，罐里
    //     有会话时会自动回放 ⇒ 那条请求已经不是"未登录"了，而 `?token=<错误>`
    //     与它逐字节相同的断言就会退化成"两个已登录请求长得一样"。
    //   · `uiWith()` 要的是"只凭 URL 里那个令牌换手"，有没有别的 cookie 会把
    //     "换手失败"掩盖成"其实靠会话进来了"。
    //   · `uiWithCookie()` 要的是"只有令牌 cookie、没有会话 cookie"，
    //     混进会话就分不清是哪一条在放行。
    status(opt) { return c.get('/status', opt); },
    // statusAnon / statusBearer 走**裸 request**（不带 cookie 罐）—— 必须这样。
    //
    // 实测踩到：拿"已登录管理员"的同一个客户端去发 `Bearer <api_key>`，罐里的
    // 会话 cookie 会同时被回放 ⇒ `Identify` 的会话那条命中 ⇒ 200。于是
    // "拿 api_key 读 /status 必须 401"这条断言会**假绿**（它测的其实是会话）。
    // 这条判据要成立，前提就是"本次只出示 api_key，别的凭据一个都不带"。
    statusAnon() { return request(base, '/status', {}); },
    statusBearer(tok) {
      return request(base, '/status', { headers: tok ? { Authorization: 'Bearer ' + tok } : {} });
    },
    uiAnon() { return request(base, '/ui', {}); },
    uiWith(token) { return request(base, '/ui?token=' + encodeURIComponent(token === undefined || token === null ? '' : token), {}); },
    uiWithCookie(token) {
      return request(base, '/ui', { headers: { Cookie: ADMIN_COOKIE + '=' + token } });
    },

    // ---- T8 增量：/admin/apikeys 系列与管理端点（全部新增，不动上面任何签名）----
    //
    // ⚠ 这一批都走 c.req（读 cookie 罐）：控制台身份就是同源会话 cookie，
    //   角色分流验的正是"带着自己那条会话能做什么"。
    apikeys() { return c.get('/admin/apikeys'); },
    createKey(name, extra) { return c.post('/admin/apikeys', Object.assign({ name }, extra || {})); },
    keyToggle(id, enabled) { return c.post('/admin/apikeys/toggle', { id, enabled: enabled !== false }); },
    keyDelete(id) { return c.post('/admin/apikeys/delete', { id }); },
    keyReset(id) { return c.post('/admin/apikeys/reset', { id }); },
    keyUpdate(id, patch) { return c.post('/admin/apikeys/update', Object.assign({ id }, patch || {})); },
    rotateAdmin() { return c.post('/admin/apikeys/rotate-admin', {}); },
    rotateApiKey() { return c.post('/admin/apikeys/rotate-api-key', {}); },
    adminGet(path) { return c.get(path); },
    adminPost(path, body) { return c.post(path, body === undefined ? {} : body); },
    // adminAsToken 以**脚本令牌**身份打管理端点（裸请求：一个 cookie 都不带）。
    //
    // 用途很具体：验"用 admin_token 建的 key，owner 为空 = 管理员所有"。
    // 那条边界只能由**没有账号身份**的调用产生（会话身份永远带 UserID），
    // 所以它必须走裸请求，否则测出来的其实是"管理员会话建的"。
    adminAsToken(method, path, tok, body) {
      return request(base, path, {
        method: method || 'GET',
        body: body === undefined ? {} : body,
        headers: tok ? { Authorization: 'Bearer ' + tok } : {},
      });
    },
  };
  return c;
}

// ---------------------------------------------------------------------------
// 报告器
// ---------------------------------------------------------------------------

// makeReporter 统一输出格式：`  PASS/FAIL/SKIP/·`。
//
// quiet=true 时只攒行不打印 —— `--self-test` 一个探针要对着 7 个桩跑 7 遍，
// 全打出来会淹没那 7 行结论。
function makeReporter(opts) {
  const quiet = !!(opts && opts.quiet);
  const r = {
    quiet, lines: [], fails: [], passed: 0, skipped: 0,
    echo(s) { r.lines.push(s); if (!quiet) console.log(s); },
    ok(cond, msg, actual) {
      const bad = !cond;
      r.echo((cond ? '  PASS ' : '  FAIL ') + msg + (bad && actual !== undefined ? '　—— 实际: ' + short(actual, 140) : ''));
      if (cond) r.passed += 1; else r.fails.push(msg);
      return !!cond;
    },
    info(m) { r.echo('  · ' + m); },
    skip(m) { r.skipped += 1; r.echo('  SKIP ' + m); },
    section(t) { r.echo('\n' + t); },
  };
  return r;
}

// ---------------------------------------------------------------------------
// 桩服务器：按契约实现，且能被开关**故意弄坏**
// ---------------------------------------------------------------------------

// 全部可用缺陷（每个都对应一条真实断言，见两个探针的 --self-test 用例表）。
const DEFECTS = [
  'login_wrong_password_200', // 错口令也回 200 + 发会话
  'cookie_missing_httponly',  // Set-Cookie 少了 HttpOnly
  'anon_open',                // 未登录也回 200（会话与 /admin/accounts 都不设防）
  'logout_no_revoke',         // 登出只清客户端，服务端会话还在
  'ui_leaks_key',             // /ui 的 HTML 里混进 mk- 密钥（未认证时那条 = 登录页）
  'no_rate_limit',            // 注册/登录永不 429
  'rate_limit_from_first',    // 第一次请求就 429
  'no_413',                   // 超长 body 照常处理，不回 413
  'captcha_ignored',          // 开着人机校验却不拦无答案注册
  'rate_limited_wrong_code',  // 429 但 code 不是 rate_limited
  // ---- T7 增量：控制台 cookie 化的五个新缺陷 ----
  'console_serves_shell_when_anon', // 未登录也吐**控制台外壳**（门禁从根上没了）
  'console_no_probe',               // 外壳里没有 probeSession（页面不再问"我是谁"）
  'ui_console_leaks_token',         // 外壳 HTML 里注入 admin_token（换了个地方泄密）
  'status_api_key_ok',              // /status 放行 api_key（账号池画像对客户端开放）
  'status_no_role_check',           // /status 不做角色判定（成员也能读管理面数据）
  // ---- T8 增量：API key 归属与角色分流的五个新缺陷 ----
  'apikeys_list_unfiltered',          // 成员看到全部 key（归属过滤没了）
  'apikeys_mutation_no_owner_check',  // 成员能改**别人的** key（越权面）
  'rotate_allowed_for_member',        // 成员能轮换管理凭据（回执带完整新令牌 ⇒ 一次提权）
  'member_can_read_stats',            // 成员能读 /admin/stats（管理面聚合外泄）
  'apikeys_missing_member_flag',      // 不回 member 标志（前端分不清"我的"还是"全部"）
];

// pickPort 找一个空闲端口。
//
// 规则来自 `lib_instance.js`：7800 段自选空闲口，绝不碰用户的 7863。
// preferred 里可以给"优先试"的端口；全都占用时退回 0（让内核分配）。
async function pickPort(preferred) {
  const free = (port) => new Promise((resolve) => {
    const s = net.createServer();
    s.once('error', () => resolve(false));
    s.once('listening', () => s.close(() => resolve(true)));
    s.listen(port, '127.0.0.1');
  });
  for (const p of preferred || []) if (p && (await free(p))) return p;
  for (let p = 7881; p <= 7899; p += 1) if (await free(p)) return p;
  return 0;
}

// startStub 起一个本地桩服务器。
//
// opts:
//   port      监听端口（默认自动挑 7800 段的空闲口）
//   limits    { register, login } 各动作的请求上限（超过即 429）
//   captcha   是否开启注册人机校验（开了就必须带 captcha_param 才放行）
//   defects   上面 DEFECTS 里的开关
//   maxBody   body 上限，超过回 413（no_413 时无效）
//   users     预置账号 [{username,password,role,enabled}]
//   adminToken 管理令牌（缺省 STUB_ADMIN_TOKEN）—— `/ui?token=` 换手与 Bearer 用它
//   apiKey     对话密钥（缺省 STUB_API_KEY）—— /status 错误的凭据用它作对照组
//
// 返回 { base, port, stop, requests, secrets, counts, adminToken, apiKey, leakedKey }
async function startStub(opts) {
  const o = Object.assign({
    port: 0, limits: { register: 8, login: 6 }, captcha: false, maxBody: 4096, defects: {}, users: [],
    adminToken: STUB_ADMIN_TOKEN, apiKey: STUB_API_KEY,
  }, opts || {});
  const d = o.defects || {};
  const users = new Map();
  const sessions = new Map();
  const counts = { register: 0, login: 0 };
  const requests = [];
  let leaked = '';

  for (const u of o.users || []) {
    users.set(String(u.username).trim().toLowerCase(), {
      id: 'u-' + randToken(16), username: String(u.username), password: u.password,
      role: u.role === undefined ? 1 : u.role, enabled: u.enabled !== false,
    });
  }

  const viewOf = (u) => ({
    id: u.id, username: u.username, role: u.role,
    role_name: u.role === 10 ? '管理员' : '成员', enabled: u.enabled,
  });

  const cookieOf = (req, name) => {
    for (const part of String(req.headers.cookie || '').split(';')) {
      const i = part.indexOf('=');
      if (i > 0 && part.slice(0, i).trim() === name) return part.slice(i + 1).trim();
    }
    return '';
  };

  // -------------------------------------------------------------------------
  // T8：/admin/* 的角色分流 + API key 归属（对齐 internal/admin 与 internal/apikey）
  // -------------------------------------------------------------------------

  // MEMBER_RULES 是**精确的 (方法, 路径) 对**，不是前缀
  // （`internal/admin/admin.go:709` 的 memberRules）。
  //
  // ⚠ T8 修掉的那个真洞就是前缀版：`/admin/apikeys` 前缀会让
  //   `POST /admin/apikeys/rotate-admin` 也命中，而那个端点回执带**完整新
  //   admin_token** ⇒ 成员一次调用即提权。桩必须照精确对实现，否则
  //   "成员不能轮换令牌"这条断言在自检里没有对照物（坏桩永远绿 = 没判别力）。
  const MEMBER_RULES = [
    ['GET', '/admin/apikeys'],
    ['POST', '/admin/apikeys'],
    ['POST', '/admin/apikeys/toggle'],
    ['POST', '/admin/apikeys/delete'],
    ['POST', '/admin/apikeys/reset'],
    ['POST', '/admin/apikeys/update'],
    ['GET', '/admin/ui/manifest'],
  ];

  // apiKeys 是**进程级**的 key 库（不是每请求一份）。
  //
  // ⚠ 第一版把它写在 handle() 里，于是每个请求都拿到一份全新的空数组：
  //   列表恒为 0 条、归属检查恒 false ⇒ "成员改不了别人的 key"看着是绿的，
  //   其实测的是"库里根本没有这把 key"（假绿）。凡是有状态的桩，状态都必须在
  //   startStub 这一层。
  const apiKeys = [];

  // maskId 复刻 apikey.MaskID 的形状（保留可辨认前缀，用于"我用的哪一把"）。
  const maskId = (id) => (String(id).length > 12 ? String(id).slice(0, 7) + '…' + String(id).slice(-4) : String(id));

  // viewOfKey 复刻 apikey.View：owner_id 是 `omitempty`
  // ⇒ 空 owner（管理员所有）**整条字段不出现**，而不是出现一个空串。
  const viewOfKey = (k) => {
    const v = {
      id: k.id, masked: maskId(k.id), name: k.name, enabled: k.enabled, limit: k.limit,
      used: k.used, remain: k.limit ? Math.max(0, k.limit - k.used) : -1,
      requests: k.requests, fails: k.fails, rpm: k.rpm, created_at: k.created_at,
    };
    if (k.owner) v.owner_id = k.owner;
    if (k.models && k.models.length) v.models = k.models;
    return v;
  };

  // listFor 复刻 apikey.Store.ListFor：**先判身份再看归属**。
  //
  // ⚠ 空 owner + 非管理员 = **空列表**（不是"列出所有 owner 为空的"）。
  //   写成 `k.owner === owner` 的话两边都是空串时成立 ⇒ 任何让身份落到空串的
  //   情况（未登录、会话失效、身份没接上）都会把管理员的 key 整列表端出去。
  //   这是本改造最危险的一处，桩也要照最严那一档实现。
  const listFor = (owner, isAdmin) => {
    if (isAdmin) return apiKeys.slice();
    const o = String(owner || '').trim();
    if (o === '') return [];
    return apiKeys.filter((k) => k.owner === o);
  };

  // canManage 复刻 apikey.Store.CanManage（能看见才谈得上管；不存在也 false）。
  const canManage = (owner, isAdmin, id) => {
    const o = String(owner || '').trim();
    if (!isAdmin && o === '') return false;
    const k = apiKeys.find((x) => x.id === id);
    if (!k) return false;
    return isAdmin ? true : k.owner === o;
  };

  // limited 计数并判定；no_rate_limit / rate_limit_from_first 两个开关在这里生效。
  const limited = (bucket) => {
    if (d.no_rate_limit) return false;
    counts[bucket] += 1;
    if (d.rate_limit_from_first) return true;
    const lim = o.limits[bucket];
    return typeof lim === 'number' && counts[bucket] > lim;
  };
  const rlBody = () => ({ error: { code: d.rate_limited_wrong_code ? 'too_many_requests' : 'rate_limited' } });

  const sidCookie = (sid) => {
    const parts = [COOKIE + '=' + sid];
    if (!d.cookie_missing_httponly) parts.push('HttpOnly');
    parts.push('SameSite=Lax', 'Path=/');
    return parts.join('; ');
  };

  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on('data', (x) => chunks.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
    req.on('error', () => { });
    req.on('end', () => handle(req, res, Buffer.concat(chunks)));
  });

  function handle(req, res, rawBuf) {
    const p = String(req.url || '/').split('?')[0];
    const text = rawBuf.toString('utf8');
    let body = {};
    try { body = text ? JSON.parse(text) : {}; } catch { body = {}; }
    const rec = (code) => { requests.push({ method: req.method, path: p, status: code }); };
    const send = (code, obj, extra) => {
      const buf = Buffer.from(JSON.stringify(obj), 'utf8');
      const headers = Object.assign({ 'Content-Type': 'application/json; charset=utf-8', 'Content-Length': buf.length }, extra || {});
      rec(code);
      res.writeHead(code, headers);
      res.end(buf);
    };
    const sendHtml = (code, html, extra) => {
      const buf = Buffer.from(html, 'utf8');
      rec(code);
      res.writeHead(code, Object.assign({ 'Content-Type': 'text/html; charset=utf-8', 'Content-Length': buf.length }, extra || {}));
      res.end(buf);
    };
    const sendRedirect = (loc, extra) => {
      rec(302);
      res.writeHead(302, Object.assign({ Location: loc, 'Content-Length': 0 }, extra || {}));
      res.end();
    };

    // bearerOf 取 Authorization: Bearer <cred>（无则空串），与 Go 侧 bearerCred 同口径。
    const bearerOf = () => {
      const v = String(req.headers.authorization || '').trim();
      return v.length >= 7 && v.slice(0, 7).toLowerCase() === 'bearer ' ? v.slice(7).trim() : '';
    };
    // principal 解析本轮请求的身份（令牌 → 会话），与 account.Identify 同一顺序。
    const principal = () => {
      const bearer = bearerOf();
      const tokCookie = cookieOf(req, ADMIN_COOKIE);
      const tokenOK = Boolean(o.adminToken)
        && ((bearer && bearer === o.adminToken) || (tokCookie && tokCookie === o.adminToken));
      if (tokenOK) return { kind: 'token', role: 10, user: undefined, bearer };
      const sid = cookieOf(req, COOKIE);
      const key = sid ? sessions.get(sid) : undefined;
      const user = key ? users.get(key) : undefined;
      return { kind: user ? 'session' : '', role: user ? user.role : 0, user, bearer };
    };

    if (p === '/healthz') return send(200, { healthy: 0 });

    // ---- /ui：三条路（对齐 internal/server/webui.go:180 的 ui()）----
    //
    //	① `?token=<admin_token>` 合法 → 302 回 /ui + 下发令牌 cookie（URL 里不再留令牌）
    //	② 已认证（会话 cookie 或令牌 cookie）→ 控制台外壳
    //	③ 未认证 → 登录页（200，不是 401：这是给**人**的入口）
    //
    // ⚠ 登录页必须与外壳**互斥且可判**：探针靠"含不含 probeSession"分辨两者，
    //   所以外壳里必须有 probeSession、登录页里必须没有。
    if (p === '/' || p === '/ui' || p === '/ui/') {
      const q = String(new URL(String(req.url || '/ui'), 'http://stub.local').searchParams.get('token') || '').trim();
      // ui_leaks_key 的注入点**跟着"未认证时到底给哪个页面"走**：
      // 改造后未认证拿到的是登录页，若还往外壳里注入，这条缺陷就静默失效
      // —— verify_login_flow_e2e 的坏桩⑤ 会变成假绿。
      const loginPage = () => {
        if (d.ui_leaks_key && !leaked) leaked = 'mk-' + randToken(64);
        return '<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>登录 · stub</title></head><body>'
          + '<form id="form" novalidate><input id="user" name="username" autocomplete="username">'
          + '<input id="pass" name="password" type="password" autocomplete="current-password">'
          + '<button id="submit" type="submit">登录</button></form>'
          + '<script>fetch("' + LOGIN_ENDPOINT + '", { method: "POST", body: "{}" });</script>'
          + (leaked ? '\n<script>window.__LEAK__ = "' + leaked + '";</script>\n' : '')
          + '</body></html>';
      };
      const shell = () => {
        // ui_console_leaks_token 注入的是**真实令牌值**（不是形状像令牌的假串）：
        // 判据因此落在"页面里有没有已知令牌"上，与 mk- 形状判据互补。
        const injected = d.ui_console_leaks_token && o.adminToken ? o.adminToken : '';
        const probe = d.console_no_probe ? ''
          : '<script>\nasync function probeSession() { const r = await fetch("/admin/auth/session"); return r.status; }\nprobeSession();\n</script>';
        return '<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>网关控制台 · stub</title></head><body>'
          + '<main id="appMain">stub-console</main>'
          + (injected ? '<script>window.__STUB_INJECTED__ = "' + injected + '";</script>' : '')
          + probe + '</body></html>';
      };
      if (q) {
        if (o.adminToken && q === o.adminToken) {
          return sendRedirect('/ui', {
            'Cache-Control': 'no-store',
            'Set-Cookie': ADMIN_COOKIE + '=' + q + '; HttpOnly; SameSite=Lax; Path=/; Max-Age=43200',
          });
        }
        // ⚠ 无效令牌**不单独报错**：那会把这个端点变成"令牌对不对"的判定 oracle。
        // 与"压根没带令牌"走同一条路 ⇒ 响应体**逐字节相同**（探针按字节比）。
      }
      const who = principal();
      if (who.kind) {
        // 已认证就给外壳。console_serves_shell_when_anon 只影响下面那条未认证分支。
        return sendHtml(200, shell(), { 'Cache-Control': 'no-store' });
      }
      if (d.console_serves_shell_when_anon) return sendHtml(200, shell(), { 'Cache-Control': 'no-store' });
      return sendHtml(200, loginPage(), { 'Cache-Control': 'no-store' });
    }
    // 超长 body：契约说 413。放在路由之前（真实实现通常也在 HTTP 层拦）。
    if (rawBuf.length > o.maxBody && !d.no_413) return send(413, { error: { code: 'payload_too_large' } });

    if (p === '/admin/auth/register' && req.method === 'POST') {
      // 人机校验是契约外的扩展点。桩用它驱动"开启了就必须拦住无答案请求"。
      const answered = Boolean(body && body.captcha_param) || Boolean(req.headers['x-captcha-param']);
      if (o.captcha && !answered && !d.captcha_ignored) {
        return send(400, { error: { code: 'captcha_required' }, captcha_required: true });
      }
      if (limited('register')) return send(429, rlBody());
      const u = String(body && body.username != null ? body.username : '').trim();
      const pw = String(body && body.password != null ? body.password : '');
      if (!/^[A-Za-z0-9_.@-]{3,32}$/.test(u)) return send(400, { error: { code: 'bad_username' } });
      if (pw.length < 8 || pw.length > 128) return send(400, { error: { code: 'bad_password' } });
      if (users.has(u.toLowerCase())) return send(409, { error: { code: 'username_taken' } });
      // ⚠ 空库的首个注册 ⇒ **管理员**（对齐真实实例的 CreateBootstrap，http.go:327）。
      //
      // 这不是"桩比真实实现更宽松"，而是刻意对齐：--start 起的就是空库，
      // 首个注册必然建管理员，回执多一个 "bootstrap": true。桩若一律建成员，
      // "管理员会话 → /status 200"这条断言在自检里就没有正例可跑，
      // 好桩会假红 —— 而假红最容易被误读成"实现没做角色判定"。
      const first = users.size === 0;
      const nu = { id: 'u-' + randToken(16), username: u, password: pw, role: first ? 10 : 1, enabled: true };
      users.set(u.toLowerCase(), nu);
      // captcha 开着（ignored 那条缺陷）时如实报告自己在校验 —— 探针正是靠
      // 这个字段发现"开了却没拦住"。
      const capFlag = o.captcha ? { captcha_required: true } : {};
      if (first) {
        return send(200, Object.assign({ ok: true, bootstrap: true, user: viewOf(nu), message: '已创建管理员账号 ' + nu.username + '，请用它与口令登录' }, capFlag));
      }
      return send(200, Object.assign({ ok: true, user: viewOf(nu) }, capFlag));
    }

    if (p === '/admin/auth/login' && req.method === 'POST') {
      if (limited('login')) return send(429, rlBody());
      const key = String(body && body.username != null ? body.username : '').trim().toLowerCase();
      const pw = String(body && body.password != null ? body.password : '');
      const user = users.get(key);
      if (!user) return send(401, { error: { code: 'bad_credentials' } });
      if (user.password !== pw) {
        if (!d.login_wrong_password_200) return send(401, { error: { code: 'bad_credentials' } });
        // 坏桩：错口令照样发会话（探针必须在这里变红）
      }
      if (!user.enabled) return send(403, { error: { code: 'disabled' } });
      const sid = randToken(64);
      sessions.set(sid, key);
      return send(200, { ok: true, user: viewOf(user) }, { 'Set-Cookie': sidCookie(sid) });
    }

    if (p === '/admin/auth/session' && req.method === 'GET') {
      const sid = cookieOf(req, COOKIE);
      const key = sid ? sessions.get(sid) : undefined;
      if (!key) {
        if (d.anon_open) {
          return send(200, { ok: true, user: viewOf({ id: 'u-' + randToken(16), username: 'anon-stub', role: 10, enabled: true }) });
        }
        return send(401, { error: { code: sid ? 'auth_invalid' : 'auth_required' } });
      }
      return send(200, { ok: true, user: viewOf(users.get(key)) });
    }

    if (p === '/admin/auth/logout' && req.method === 'POST') {
      const sid = cookieOf(req, COOKIE);
      if (sid && !d.logout_no_revoke) sessions.delete(sid);
      return send(200, { ok: true }, { 'Set-Cookie': COOKIE + '=; Max-Age=0; Path=/; HttpOnly; SameSite=Lax' });
    }

    // ---- 门禁与 API key 库（判据在 startStub 外层，见那里的注释）----
    const canAccess = (who, method, path) => {
      if (who.role >= 10) return true;
      // 坏桩：把 /admin/stats 放给成员（管理面聚合不该给成员看）
      if (d.member_can_read_stats && (path === '/admin/stats' || path === '/admin/stats/series')) return true;
      // 坏桩：复刻 T8 修掉的那个真洞 —— 门禁把 rotate-* 也放行（前缀白名单时代）。
      // 关掉的不只是门禁那一层：第二个开关由下面的 handler 消费，两层一起坏，
      // 才复刻出"成员一次调用即拿到新管理令牌"的真实形态。
      if (d.rotate_allowed_for_member && path.startsWith('/admin/apikeys/rotate-')) return true;
      return MEMBER_RULES.some((r) => r[0] === method && r[1] === path);
    };
    const ownerOf = (who) => (who && who.user ? who.user.id : '');

    if (p.startsWith('/admin/') && !p.startsWith('/admin/auth/')) {
      let who = principal();
      if (!who.kind) {
        if (d.anon_open) {
          // 旧缺陷 anon_open 的语义是"未登录也放行"（原本 /admin/* 不设防）
          who = { kind: 'anon', role: 10, user: undefined };
        } else {
          return send(401, { error: '管理接口需要授权。', code: 'auth_required' });
        }
      }
      if (!canAccess(who, req.method, p)) {
        return send(403, {
          error: '当前账号（' + (who.user ? who.user.username : 'admin_token') + '，角色 '
            + (who.role >= 10 ? '管理员' : '成员') + '）无权访问 ' + p + '。该功能仅管理员可用。',
          code: 'forbidden_role', role: who.role, scope: 'admin',
        });
      }
      const isAdmin = who.role >= 10;

      // GET /admin/apikeys —— 按身份过滤 + member 标志 + 管理钥匙掩码只给管理员
      if (p === '/admin/apikeys' && req.method === 'GET') {
        const keys = d.apikeys_list_unfiltered ? apiKeys.slice() : listFor(ownerOf(who), isAdmin);
        const body = {
          keys: keys.map(viewOfKey),
          enabled: true,
          admin_key: isAdmin ? maskId(o.apiKey) : '',
          admin_token: isAdmin ? maskId(o.adminToken) : '',
        };
        // apikeys_missing_member_flag：不吐这个字段 ⇒ 前端分不清"我的"还是"全部"
        if (!d.apikeys_missing_member_flag) body.member = !isAdmin;
        return send(200, body);
      }

      // POST /admin/apikeys —— 新建，归属 = 当前身份（owner 为空 = 机器令牌那条路）
      if (p === '/admin/apikeys' && req.method === 'POST') {
        const name = String((body && body.name) || '').trim();
        if (!name) return send(400, { error: '名称不能为空' });
        const k = {
          id: 'sk-' + randToken(16), name, enabled: true,
          limit: Number((body && body.limit) || 0), rpm: Number((body && body.rpm) || 0),
          models: (body && body.models) || null, owner: ownerOf(who),
          created_at: new Date().toISOString(), used: 0, requests: 0, fails: 0,
        };
        apiKeys.push(k);
        return send(200, {
          id: k.id, masked: maskId(k.id), full: k.id, name: k.name, enabled: k.enabled,
          limit: k.limit, rpm: k.rpm, models: k.models, created_at: k.created_at,
        });
      }

      // 四个改 key 的端点：前置归属检查（越权一律 403，**不区分**"不存在"与"不是你的"）
      const MUTS = ['toggle', 'delete', 'reset', 'update'];
      const op = p.startsWith('/admin/apikeys/') ? p.slice('/admin/apikeys/'.length) : '';
      if (req.method === 'POST' && MUTS.indexOf(op) >= 0) {
        const id = String((body && body.id) || '');
        const mine = canManage(ownerOf(who), isAdmin, id);
        // apikeys_mutation_no_owner_check：跳过归属检查 ⇒ 成员能改别人的 key
        if (!mine && !d.apikeys_mutation_no_owner_check) {
          return send(403, {
            error: '这把 API key 不属于当前账号（成员只能管理自己创建的 key）。',
            code: 'forbidden_role', scope: 'apikey',
          });
        }
        const k = apiKeys.find((x) => x.id === id);
        if (!k) return send(404, { error: 'API key 不存在' });
        if (op === 'delete') {
          apiKeys.splice(apiKeys.indexOf(k), 1);
          return send(200, { deleted: true });
        }
        if (op === 'toggle') {
          k.enabled = !(body && body.enabled === false);
          return send(200, { id: maskId(id), enabled: k.enabled });
        }
        if (op === 'reset') {
          k.used = 0; k.fails = 0;
          return send(200, { reset: true });
        }
        if (body && body.limit !== undefined && body.limit !== null) k.limit = Number(body.limit);
        if (body && body.rpm !== undefined && body.rpm !== null) k.rpm = Number(body.rpm);
        if (body && body.models) k.models = body.models;
        return send(200, { updated: true });
      }

      // 轮换管理凭据：**仅管理员**。回执带完整明文新凭据 ⇒ 成员调通一次即提权。
      if (req.method === 'POST' && (op === 'rotate-admin' || op === 'rotate-api-key')) {
        // 第二层硬判（对齐 apikeys.go:344 / :392 的 IsAdmin 再判一次）。
        // rotate_allowed_for_member 会把两层一起关掉 —— 那正是 T8 之前的形状。
        if (!isAdmin && !d.rotate_allowed_for_member) {
          return send(403, { error: '轮换管理凭据仅管理员可用。', code: 'forbidden_role', scope: 'admin' });
        }
        if (!isAdmin) {
          // 坏桩：复刻"成员一次调用即拿到新管理令牌"的真洞
          const full = 'mk-' + randToken(48);
          return send(200, { full, masked: maskId(full), path: 'config.json', note: '旧管理令牌已立即失效。' });
        }
        const full = 'mk-' + randToken(48);
        return send(200, {
          full, masked: maskId(full), path: 'config.json',
          note: op === 'rotate-admin' ? '旧管理令牌已立即失效。' : '这是**对话接口**的凭据。',
        });
      }

      // 成员白名单的正例（否则"成员什么都不能"与"角色分流对了"分不开）
      if (p === '/admin/ui/manifest' && req.method === 'GET') {
        return send(200, { service: 'multi2api', member: !isAdmin });
      }
      // 管理面读数：管理员专属（成员在门禁就被挡下了）
      if (req.method === 'GET' && ['/admin/settings', '/admin/logs', '/admin/providers', '/admin/stats', '/admin/stats/series'].indexOf(p) >= 0) {
        return send(200, { ok: true, path: p });
      }
    }

    if (p === '/admin/accounts' && req.method === 'GET') {
      const sid = cookieOf(req, COOKIE);
      const key = sid ? sessions.get(sid) : undefined;
      const user = key ? users.get(key) : undefined;
      if (!user) {
        if (d.anon_open) return send(200, { ok: true, accounts: [...users.values()].map(viewOf) });
        return send(401, { error: { code: 'auth_required' } });
      }
      if (user.role !== 10) return send(403, { error: { code: 'forbidden' } });
      return send(200, { ok: true, accounts: [...users.values()].map(viewOf) });
    }

    // ---- /status：控制台身份 + **必须管理员**（handler.go:540 consoleAdminAuth）----
    //
    // 改造前它挂在 withAuth 上（认 api_key）⇒ "给某人一把对话钥匙"等于
    // "把账号池画像也给他看"。所以这里钉死四条：
    //
    //	管理员会话 → 200 / 成员会话 → 403 forbidden_role
    //	Bearer admin_token → 200 / Bearer api_key（或别的）→ 401 auth_required
    //
    // ⚠ 错误信封用**扁平**形状（`{"error":"一句人话","code":"…"}`），与真实实现
    //   的 writeJSON(map) 一致 —— 不是 /admin/* 的 {"error":{"code":…}} 嵌套形状。
    if (p === '/status' && req.method === 'GET') {
      const who = principal();
      const pool = { ok: true, accounts: [], total: 0, healthy: 0 };
      if (who.kind === 'token') return send(200, pool);
      if (who.kind === 'session') {
        if (who.role === 10 || d.status_no_role_check) return send(200, pool);
        return send(403, {
          error: '账号池读数仅管理员可见（成员请用 /admin/stats 看自己的用量）。',
          code: 'forbidden_role',
          role: who.role,
        });
      }
      // 坏桩：把 api_key 也当控制台凭据（正是这次改造要禁止的那条回流路径）
      if (d.status_api_key_ok && who.bearer && o.apiKey && who.bearer === o.apiKey) return send(200, pool);
      return send(401, {
        error: '该接口需要控制台身份：请在 /ui 登录，或用 Authorization: Bearer <admin_token>。',
        code: 'auth_required',
      });
    }

    return send(404, { error: { code: 'not_found' } });
  }

  const port = await pickPort([o.port]);
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', resolve);
  });
  const real = server.address().port;
  return {
    port: real,
    base: 'http://127.0.0.1:' + real,
    requests,
    counts,
    // 本桩的两把令牌：探针必须知道它们才能断言"哪一把能进控制台"。
    // 回传给调用方（runSelfTest 也会把它塞进 ctx），**不改**上面的既有字段。
    adminToken: o.adminToken,
    apiKey: o.apiKey,
    // /ui 里**不该出现**的已知令牌：本次没有配置任何真令牌，故为空数组
    // （缺陷 ui_leaks_key 靠形状判据抓，见 secretHits）。
    secrets: [],
    leakedKey: () => leaked,
    stop() {
      try { server.closeAllConnections(); } catch { }
      try { server.close(); } catch { }
    },
  };
}

// ---------------------------------------------------------------------------
// 自检：证明"探针有判别力"
// ---------------------------------------------------------------------------

// runSelfTest 按用例表跑探针，逐例输出一行 PASS/FAIL。
//
// 判据是**双向**的：
//   expect:'pass' 的桩（完全按契约实现）⇒ 探针必须全绿；
//   expect:'fail' 的桩（故意坏一处）    ⇒ 探针必须变红。
// 只有两个方向都成立，"通过"才代表产品对，而不是探针写成了恒真。
async function runSelfTest(cfg) {
  console.log('=== ' + cfg.title + ' · 探针自检（判别力）===');
  let bad = 0;
  let seq = 0;
  for (const c of cfg.cases) {
    seq += 1;
    const stub = await startStub(Object.assign({ port: 7880 + seq }, c.stub || {}));
    let fails = null;
    let thrown = null;
    let lines = [];
    try {
      const rep = makeReporter({ quiet: true });
      await cfg.runProbe({
        base: stub.base,
        reporter: rep,
        secrets: [],
        fresh: true,          // 桩每次都是全新的桶 ⇒ "第 N 次"的断言必须有确定性
        stub: true,
        captchaParam: 'self-test-captcha-answer',
        // 桩的两把令牌也要进 ctx：探针要判"哪一把能进控制台"，而"哪一把"是
        // 桩才知道的事实（真实模式下这两个字段来自 --start 起的实例 / 环境变量）。
        adminToken: stub.adminToken,
        apiKey: stub.apiKey,
      });
      fails = rep.fails.length;
      lines = rep.lines;
    } catch (e) {
      thrown = e;
    } finally {
      stub.stop();
    }
    const okVerdict = !thrown && (c.expect === 'pass' ? fails === 0 : fails > 0);
    const actual = thrown ? '探针抛异常: ' + short(thrown.message || String(thrown), 80)
      : (fails === 0 ? '探针全绿' : '探针红 ' + fails + ' 条');
    console.log((okVerdict ? 'PASS ' : 'FAIL ') + c.desc
      + '　［期望探针' + (c.expect === 'pass' ? '通过' : '失败') + '，实际 ' + actual + '］');
    if (!okVerdict) {
      // 只有"结论与期望不符"才需要看细节（那说明自检用例本身有问题）
      lines.filter((l) => l.includes('FAIL') || l.includes('SKIP')).forEach((l) => console.log('      ' + l.trim()));
      bad += 1;
    }
  }
  return bad;
}

// ---------------------------------------------------------------------------
// 三态入口
// ---------------------------------------------------------------------------

function baseFromArgv() {
  const argv = process.argv.slice(2);
  const i = argv.indexOf('--base');
  if (i >= 0 && argv[i + 1]) return argv[i + 1];
  return process.env.WB2API_BASE || DEFAULT_BASE;
}

function portFromBase(base) {
  try {
    const p = Number(new URL(base).port);
    return p || 7885;
  } catch { return 7885; }
}

// runCli 统一入口：默认打 WB2API_BASE，`--start` 起隔离实例，`--self-test` 打本地桩。
//
// cfg: { title, runProbe, cases, apiKey, instanceConfig }
//
//	instanceConfig  仅 --start 生效：额外合并进隔离实例 config.json 的键。
//	                存在的理由见下面 admin_token 那段（两把钥匙必须不等）。
//
// runProbe 收到 ctx = { base, reporter, secrets, fresh, captchaParam, stub, apiKey, adminToken }
async function runCli(cfg) {
  const argv = process.argv.slice(2);
  const self = argv.includes('--self-test');
  const start = argv.includes('--start');
  let inst = null;
  try {
    if (self) {
      if (start) { console.log('--self-test 与 --start 互斥：自检打的是本地桩，不需要实例'); return 2; }
      const bad = await runSelfTest({ title: cfg.title, cases: cfg.cases, runProbe: cfg.runProbe });
      console.log('\n自检结论: ' + (bad ? bad + ' 例没达到预期 ⇒ 探针自身有问题（先修探针）' : '全部符合预期：好桩绿、坏桩红 ⇒ 探针有判别力'));
      return bad ? 1 : 0;
    }
    let base = baseFromArgv();
    let apiKey = process.env.WB2API_API_KEY || '';
    let adminToken = process.env.WB2API_ADMIN_TOKEN || '';
    if (start) {
      const port = portFromBase(base);
      const extra = cfg.instanceConfig || {};
      // 复用 lib_instance 的隔离实例：cwd=临时目录、只写探针自己的 config。
      inst = await startInstance({ port, auths: {}, apiKey: cfg.apiKey || ('probe-key-' + port), config: extra });
      base = inst.base;
      console.log('已起隔离实例（空账号库）: ' + base + '  pid=' + inst.pid);
      // ⚠ startInstance 缺省把 admin_token 设成 **同一个** api_key。要验
      // "/status 不认 api_key"就必须让两把不相等 —— 否则那条断言是在拿同一把
      // 钥匙比对自身，必然 200，测出来的结论是假的。
      apiKey = inst.key;
      adminToken = extra.admin_token || inst.key;
    }
    const rep = makeReporter({});
    console.log('=== ' + cfg.title + ' ===');
    console.log('目标: ' + base + (start ? '（探针自己起的隔离实例）' : '（来自 WB2API_BASE / --base）'));
    if (inst) rep.info('实例日志: ' + inst.logPath);
    await cfg.runProbe({
      base,
      reporter: rep,
      secrets: secretsFromEnv().concat(inst ? [inst.key] : []),
      fresh: start,
      argv,
      captchaParam: process.env.WB2API_CAPTCHA_PARAM || '',
      apiKey,
      adminToken,
    });
    console.log('\n结果: ' + (rep.fails.length ? rep.fails.length + ' 条 FAIL' : '全绿')
      + (rep.skipped ? '（SKIP ' + rep.skipped + ' 项，见上面每条的说明）' : ''));
    return rep.fails.length ? 1 : 0;
  } catch (e) {
    console.log('探针异常: ' + ((e && e.stack) || e));
    return 1;
  } finally {
    if (inst) { try { inst.stop(); } catch { } }
  }
}

module.exports = {
  // 原样复用 lib_instance 的三件套（本层不重实现"起实例"与"不带头的请求"）
  httpJson, startInstance, sleep: sleepMs,
  // 常量
  COOKIE, ADMIN_COOKIE, SID_RE, SESSION_401_CODES, DEFAULT_BASE, DEFECTS,
  LOGIN_FORM_MARKER, LOGIN_ENDPOINT, SHELL_MARKER_RE, STUB_ADMIN_TOKEN, STUB_API_KEY,
  // 工具
  randToken, freshUsername, short, hist, errCode, resCode, isRateLimited, hugeBody, missingReason, missingHint, secretHits, secretsFromEnv,
  // HTTP 客户端
  request, parseSetCookie, findCookie, makeClient,
  // 报告
  makeReporter,
  // 桩与自检
  startStub, pickPort, runSelfTest,
  // 入口
  baseFromArgv, portFromBase, runCli,
};
