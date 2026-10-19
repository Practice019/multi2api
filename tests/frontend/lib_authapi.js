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
  'ui_leaks_key',             // /ui 的 HTML 里混进 mk- 密钥
  'no_rate_limit',            // 注册/登录永不 429
  'rate_limit_from_first',    // 第一次请求就 429
  'no_413',                   // 超长 body 照常处理，不回 413
  'captcha_ignored',          // 开着人机校验却不拦无答案注册
  'rate_limited_wrong_code',  // 429 但 code 不是 rate_limited
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
//
// 返回 { base, port, stop, requests, secrets, counts }
async function startStub(opts) {
  const o = Object.assign({
    port: 0, limits: { register: 8, login: 6 }, captcha: false, maxBody: 4096, defects: {}, users: [],
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
    const sendHtml = (code, html) => {
      const buf = Buffer.from(html, 'utf8');
      rec(code);
      res.writeHead(code, { 'Content-Type': 'text/html; charset=utf-8', 'Content-Length': buf.length });
      res.end(buf);
    };

    if (p === '/healthz') return send(200, { healthy: 0 });
    if (p === '/' || p === '/ui' || p === '/ui/') {
      if (d.ui_leaks_key) leaked = 'mk-' + randToken(64);
      const leak = leaked ? '\n<script>window.__LEAK__ = "' + leaked + '";</script>\n' : '';
      return sendHtml(200, '<!doctype html><html><head><meta charset="utf-8"><title>stub ui</title></head>'
        + '<body><div id="app">stub</div>' + leak + '</body></html>');
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
      users.set(u.toLowerCase(), { id: 'u-' + randToken(16), username: u, password: pw, role: 1, enabled: true });
      // captcha 开着（ignored 那条缺陷）时如实报告自己在校验 —— 探针正是靠
      // 这个字段发现"开了却没拦住"。
      return send(200, o.captcha ? { ok: true, captcha_required: true } : { ok: true });
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
// cfg: { title, runProbe, cases, apiKey }
// runProbe 收到 ctx = { base, reporter, secrets, fresh, captchaParam, stub }
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
    if (start) {
      const port = portFromBase(base);
      // 复用 lib_instance 的隔离实例：cwd=临时目录、只写探针自己的 config。
      inst = await startInstance({ port, auths: {}, apiKey: cfg.apiKey || ('probe-key-' + port) });
      base = inst.base;
      console.log('已起隔离实例（空账号库）: ' + base + '  pid=' + inst.pid);
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
  COOKIE, SID_RE, SESSION_401_CODES, DEFAULT_BASE, DEFECTS,
  // 工具
  randToken, freshUsername, short, hist, errCode, isRateLimited, hugeBody, missingReason, missingHint, secretHits, secretsFromEnv,
  // HTTP 客户端
  request, parseSetCookie, findCookie, makeClient,
  // 报告
  makeReporter,
  // 桩与自检
  startStub, pickPort, runSelfTest,
  // 入口
  baseFromArgv, portFromBase, runCli,
};
