// lib_instance.js —— 「起一个**隔离**实例」的共享层（2026-10-19 抽）。
//
// # 为什么抽（AGENTS 第 6 条）
//
// `verify_all_buttons_e2e.js` 里那段"临时目录 + 空闲端口 + 只改 listen 的 config"
// 是**安全前提**，不是可选的样板：仓库根目录的 config.json 是用户正在用的
// 7863 实例，auths/ 与 data/ 里是**真实账号**。任何探针只要 cwd 落在仓库根、
// 或者 config 里的相对路径没被替换，就会读到、写到真实数据。
//
// 所以这段逻辑抽取成一份，并把它**做成唯一入口**：
//   · cwd = 临时目录 ⇒ `./auths`、`./data`、`./data/state.json` 全部落在临时目录
//   · 只写探针自己的 config.json ⇒ 不碰 config.json / config-test-port7864.json
//   · 调用方只给"要哪些合成凭证"，拿不到仓库根的任何路径
//
// # 三条硬约束（每条都对应一次踩坑）
//
//   ① **端口必须由调用方显式给**：7863 是用户的、3080 是 DSH、3000 是 new-api。
//      探针只用 7800 段的自选空闲口，且不覆盖已有监听。
//   ② **auths 内容由调用方逐文件给**：不读仓库里的 auths/，也不读 auths-test/
//      （那个目录虽然 gitignore，但内容是别人的真实 token）。
//   ③ **stdout/stderr 收进文件**：`stdio:'ignore'` 会让失败无从查起（实例起不来
//      时只剩一句"未起来"）。收进 <tmp>/server.log，报错时把它打出来。
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn } = require('child_process');

const REPO = path.resolve(__dirname, '..', '..');
// EXE 可用环境变量覆盖：WB2API_EXE=bin/wb2api-server-probe.exe
//   ⚠ 存在的理由：**不能覆盖 bin/wb2api-server.exe** —— 那通常是用户正在跑的
//     实例（Windows 上还被进程锁着）。改了代码要验证时，编一个别的名字再指过来。
const EXE = process.env.WB2API_EXE
  ? path.resolve(REPO, process.env.WB2API_EXE)
  : path.join(REPO, 'bin', 'wb2api-server.exe');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// httpJson 发一次请求并返回**状态码 + 原文**（不抛异常非 2xx）。
//
// ⚠ 不用 r.json()：本仓的 admin() 是 `r.text()` + `JSON.parse`，
//   而这里要能看见 4xx/5xx 的**响应体**（错误原因通常就在那里）。
function httpJson(base, p, opt) {
  const o = opt || {};
  const u = new URL(p, base);
  const body = o.body === undefined ? null : (typeof o.body === 'string' ? o.body : JSON.stringify(o.body));
  return new Promise((resolve) => {
    const r = http.request({
      hostname: u.hostname, port: u.port, path: u.pathname + u.search,
      method: o.method || 'GET',
      headers: Object.assign(
        body === null ? {} : { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
        o.token ? { Authorization: 'Bearer ' + o.token } : {},
      ),
    }, (res) => {
      const c = [];
      res.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      res.on('end', () => {
        const text = Buffer.concat(c).toString('utf8');
        let json = null;
        try { json = JSON.parse(text); } catch { }
        resolve({ status: res.statusCode, text, json });
      });
    });
    r.on('error', (e) => resolve({ status: 0, text: String(e && e.message), json: null }));
    if (body !== null) r.write(body);
    r.end();
  });
}

function httpGetText(u) {
  return new Promise((res, rej) => {
    http.get(u, (r) => {
      const c = [];
      r.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res({ status: r.statusCode, text: Buffer.concat(c).toString('utf8') }));
    }).on('error', rej);
  });
}

// startInstance 起一个完全隔离的实例。
//
// opts:
//   port      （必填）监听端口
//   auths     { '<相对路径>': '<文件内容>' } —— 合成凭证，逐文件写进 <tmp>/auths/
//   apiKey    给 config 的 api_key（同时用作 admin_token，省一次配）
//   config    额外合并进 config.json 的键
//   readyMs   等待 /healthz 的毫秒上限（默认 12s）
//
// 返回 { base, tmp, key, logPath, stop(), readLog() }
async function startInstance(opts) {
  const port = opts.port;
  if (!port) throw new Error('必须显式给端口（别用 7863/3080/3000）');
  const tmp = path.join(os.tmpdir(), 'wb2api-inst-' + port + '-' + process.pid);
  fs.rmSync(tmp, { recursive: true, force: true });
  fs.mkdirSync(path.join(tmp, 'auths'), { recursive: true });
  fs.mkdirSync(path.join(tmp, 'data'), { recursive: true });

  for (const [rel, content] of Object.entries(opts.auths || {})) {
    const f = path.join(tmp, 'auths', rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, content);
  }

  const key = opts.apiKey || ('probe-key-' + port);
  const cfg = Object.assign({
    listen: '127.0.0.1:' + port,
    api_key: key,
    admin_token: key,
    auth_dir: './auths',
    state_file: './data/state.json',
    admin: { checkin_log_path: './data/checkin-log.json', client_login_enabled: false },
    login: { open_browser: false, isolated: true },
    // ⚠ 探针起的实例一律**关掉人机校验**（T10 起它默认开）。
    //
    // 理由：算术题是给**人**看的（本地 SVG，答案不落在响应里），自动脚本答不了。
    // 开着它，所有走"注册 → 登录"的探针都会停在第一条 400 captcha_required 上
    // —— 那不是"实现坏了"，而是"尺子量错了对象"。
    //
    // 这不等于"人机校验没被验"：
    //   · 它被拦住/放行的**端点契约**由 internal/account/captcha_test.go 的 6 条覆盖
    //   · 它"默认真的骑着"由下面这条断言覆盖 —— 探针用**不关它**的实例验一次
    //     400 captcha_required（见 verify_captcha_e2e.js 的 --captcha-on）
    //   · 其余三层（限速/请求体/会话上限）在关了它的实例上照常生效，正是
    //     计划里的验收口径："不开验证码时其余三层仍拦得住"
    //
    // 想验开着的那条路：`opts.config = { console: { disable_captcha: false } }`。
    console: { disable_captcha: true },
  }, opts.config || {});
  // 深一层地被 opts.config 覆盖：Object.assign 只做浅合并，调用方给
  // `{console:{disable_captcha:false}}` 会把上面这行整个换掉 —— 这是想要的
  // （调用方明确表态时以它为准），但**必须**知道：给 `console` 只写一半
  // （例如只想设 cookie_secure）会顺手把"关掉人机校验"这半边丢掉。
  // 所以合并显式做一次，两边的键都保住。
  if (opts.config && opts.config.console) {
    cfg.console = Object.assign({ disable_captcha: true }, opts.config.console);
  }
  fs.writeFileSync(path.join(tmp, 'config.json'), JSON.stringify(cfg, null, 2));

  const logPath = path.join(tmp, 'server.log');
  const out = fs.openSync(logPath, 'a');
  // ⚠ cwd=tmp：config 里的相对路径（./auths、./data）**全部**落在临时目录。
  const srv = spawn(EXE, ['-config', 'config.json'], { cwd: tmp, stdio: ['ignore', out, out] });

  const base = 'http://127.0.0.1:' + port;
  const deadline = Date.now() + (opts.readyMs || 12000);
  let up = false;
  while (Date.now() < deadline) {
    try {
      const r = await httpGetText(base + '/healthz');
      // ⚠⚠ `/healthz` 在**没有健康账号**时回 **503**（body 是 {"healthy":0,…}）。
      //
      // 那不是"没起来"，而是"起来但池子是空的"。就绪判据若只认 200，
      // 一个**故意不带凭证**的探针（例如只验令牌与鉴权的那个）会永远等下去，
      // 报错说"实例未起来"，把探针自己的前提问题伪装成产品故障
      //（实测踩到：token 安全探针起不来，日志里其实已经"multi2api 已启动"）。
      //
      // 所以判据是**服务开始应答**（200 或 503 都是应答），不是"healthy > 0"。
      if (r.status === 200 || r.status === 503) { up = true; break; }
    } catch { }
    await sleep(200);
  }
  const readLog = () => { try { return fs.readFileSync(logPath, 'utf8'); } catch { return ''; } };
  if (!up) {
    try { srv.kill(); } catch { }
    throw new Error('实例未在 ' + base + ' 起来（pid=' + srv.pid + '）\n--- server.log ---\n' + readLog().slice(-2000));
  }
  const stop = () => { try { srv.kill(); } catch { } };
  return { base, tmp, key, logPath, stop, readLog, pid: srv.pid };
}

module.exports = { startInstance, httpJson, httpGetText, sleep, REPO, EXE };
