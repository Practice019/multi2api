// verify_codearts_login.js —— R1 的**端到端**守卫。
//
// # 为什么必须有这个文件（它抓到过一个单测抓不到的 bug）
//
// R1-c 接线后我跑端到端，发现两个**互相矛盾**的观察：
//
//	GET /admin/login/poll  → "授权会话不存在或已结束"
//	直打回调地址           → 200 <h2>授权成功</h2>   ← 回调服务器明明活着
//
// 唯一解释：**poll 拿到的不是 start 用的那个 flow 实例**。
//
// 根因：`LoginFlow()` 每次调用都新建实例，于是 `sessions` map 各是一份空表。
// **后果：页内添加账号永远不可能成功，且不报错。**
//
// 而我当时已有的 9 条 Go 单测**全绿** —— 因为它们都用 `newTestFlow`
// 直接构造一个 loginFlow 再连续调方法，**全程同一个实例**，
// "每次调用换实例"这件事在那些测试里根本不会发生。
//
// > 单测覆盖单元内部逻辑，覆盖不了"对象的生命周期"。
//
// 所以这个文件测的是**跨 HTTP 请求**的状态连续性 —— 那是单测的盲区。
//
// # 判据全部走真实 HTTP，不读源码
'use strict';
const http = require('http');
const fs = require('fs');

// 端口来源（按优先级）：
//   1. LOGIN_URL   —— run_e2e.js 的约定（它给每个套件传一个 URL 环境变量）
//   2. LOGIN_PORT  —— 单独跑时更直接
//   3. 18080       —— 默认
//
// ⚠ 两种都要支持：run_e2e 传 URL、手动跑更习惯传端口。
// 只认一种会导致"单独跑绿、纳入套件后红"，而那种失败最难查
//（看起来像套件坏了，实际是参数没接上）。
function resolvePort() {
  const u = process.env.LOGIN_URL;
  if (u) {
    try { return Number(new URL(u).port || 80); } catch { /* 落到下一档 */ }
  }
  return Number(process.env.LOGIN_PORT || 18080);
}
const PORT = resolvePort();

// api_key 的来源：**从部署配置里读**，不硬编码。
//
// ⚠ 本项目因为这个栽过三次（真实 key 进了源码与 git 历史）。
// 所以这里只认两条路径：环境变量，或配置文件的 api_key 字段。
// 找不到就**直接失败并说明**，而不是回落到一个写死的值。
function loadKey() {
  if (process.env.WB2API_KEY) return process.env.WB2API_KEY;
  const cfgPath = process.env.WB2API_CONFIG || 'D:\\tmp\\run-' + PORT + '.json';
  try {
    const cfg = JSON.parse(fs.readFileSync(cfgPath, 'utf8'));
    if (cfg.api_key) return cfg.api_key;
  } catch (e) {
    // 落到下面的统一报错
  }
  console.log('  无法获得 api_key：');
  console.log('    · 设 WB2API_KEY 环境变量，或');
  console.log('    · 设 WB2API_CONFIG 指向部署配置（默认 D:\\tmp\\run-' + PORT + '.json）');
  process.exit(2);
}
const KEY = loadKey();
const BASE = 'http://127.0.0.1:' + PORT;

function post(path, body) {
  return new Promise((res, rej) => {
    const b = JSON.stringify(body);
    const q = http.request({
      host: '127.0.0.1', port: PORT, path, method: 'POST',
      headers: { Authorization: 'Bearer ' + KEY, 'Content-Type': 'application/json',
                 'Content-Length': Buffer.byteLength(b) },
    }, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => {
      let j = null; try { j = JSON.parse(d); } catch { }
      res({ code: r.statusCode, body: d, json: j });
    }); });
    q.on('error', rej);
    q.write(b); q.end();
  });
}

function get(host, port, path) {
  return new Promise(res => {
    http.get({ host, port, path }, r => { let d = ''; r.on('data', c => d += c);
      r.on('end', () => res({ code: r.statusCode, body: d })); })
      .on('error', e => res({ err: e.message }));
  });
}

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  // ---- 1. 两个上游都必须声明 login（不给假按钮，但有的要给） ----
  const man = await new Promise(res => {
    http.get({ host: '127.0.0.1', port: PORT, path: '/admin/ui/manifest',
      headers: { Authorization: 'Bearer ' + KEY } }, r => {
      let d = ''; r.on('data', c => d += c); r.on('end', () => res(JSON.parse(d)));
    });
  });
  const provs = man.providers || [];
  console.log('  manifest 的 login: ' + JSON.stringify(provs.map(p => p.id + '=' + (p.login ? p.login.kind : 'null'))));

  for (const p of provs) {
    ok(p.login && p.login.label, '上游 ' + p.id + ' 声明了 login（有 label）');
  }

  // ---- 2. start 必须给出**该上游自己的**授权页 ----
  const s = await post('/admin/login/start', { provider: 'codearts' });
  ok(s.code === 200, 'provider=codearts 的 start 返回 200（实际 ' + s.code + '）');
  if (s.code !== 200) { console.log('    body=' + s.body.slice(0, 160)); }
  if (s.json && s.json.auth_url) {
    console.log('  codearts auth_url: ' + s.json.auth_url.slice(0, 90));
    ok(s.json.auth_url.includes('codearts.huaweicloud.com'),
      'codearts 的授权页指向 CodeArts 站点（不是 workbuddy 的）');
    ok(s.json.auth_url.includes('code_challenge='),
      'codearts 用 PKCE（URL 含 code_challenge）—— 与 workbuddy 的设备码流程不同');
    ok(s.json.provider === 'codearts', '响应回带 provider=codearts');
  }

  const w = await post('/admin/login/start', { provider: 'workbuddy' });
  ok(w.code === 200, 'provider=workbuddy 的 start 返回 200（实际 ' + w.code + '）');
  if (w.json && w.json.auth_url) {
    console.log('  workbuddy auth_url: ' + w.json.auth_url.slice(0, 90));
    ok(!w.json.auth_url.includes('huaweicloud'),
      'workbuddy 的授权页**不**指向 CodeArts 站点（两个上游真的分开了）');
  }

  // ---- 3. 关键：跨请求的状态连续性（那个 bug 的直接判据） ----
  //
  // 这是本文件存在的理由。start 存下的会话，**下一次 HTTP 请求**的 poll
  // 必须能看见。
  const state = s.json && s.json.state;
  ok(!!state, 'start 返回了 state');

  if (state) {
    const t0 = Date.now();
    const p1 = await post('/admin/login/poll', { state, provider: 'codearts' });
    const dt = Date.now() - t0;

    console.log('  首次 poll: HTTP ' + p1.code + ' body=' + p1.body.slice(0, 60) + ' 耗时 ' + dt + 'ms');

    ok(p1.code === 202 && p1.body.includes('pending'),
      '未授权时 poll 返回 202 pending（实际 ' + p1.code + ' ' + p1.body.slice(0, 60) + '）');

    ok(!p1.body.includes('会话不存在'),
      '**跨请求能看见 start 存下的会话** —— 这是那个 bug 的直接判据。' +
      '若这里失败，说明 start 与 poll 拿到了不同的 flow 实例（sessions 各一份空表）');

    ok(dt < 2000,
      'poll 秒回（' + dt + 'ms）—— 等待下沉到了后台，没被 15 分钟阻塞');

    // ---- 4. 回调服务器真的在监听（证明流程活着，而不是只存了个 state） ----
    const m = /auth_callback_url=([^&]+)/.exec(s.json.auth_url);
    if (m) {
      const cb = decodeURIComponent(m[1]);
      const u = new URL(cb);
      const hit = await get(u.hostname, u.port, u.pathname + '?code=PROBE');
      console.log('  直打回调 ' + cb + ' → ' + JSON.stringify(hit));
      ok(hit.code === 200,
        '回调服务器在真监听（HTTP ' + hit.code + '）—— 说明 start 真的起了监听，不是只存了 state');
    } else {
      ok(false, 'auth_url 里没有 auth_callback_url —— 无法验证回调服务器');
    }
  }

  // ---- 5. 未知 state 必须明确失败，不能静默 pending ----
  const ghost = await post('/admin/login/poll', { state: 'GHOST-STATE-NOT-EXIST', provider: 'codearts' });
  console.log('  幽灵 state: HTTP ' + ghost.code + ' body=' + ghost.body.slice(0, 70));
  ok(ghost.code !== 202,
    '未知 state **不能**返回 pending —— 那会让前端永远转圈' +
    '（实际 ' + ghost.code + '）');

  console.log(fail === 0 ? '\n=== R1 端到端验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
