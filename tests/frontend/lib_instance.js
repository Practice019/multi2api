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

// EXE_AT_LOAD 记下"模块求值那一刻"的取值，用来抓一个**静默**的坑：
//
//	process.env.WB2API_EXE = 'bin/xxx-probe.exe';   // ← 写在 require 之后
//	const { startInstance } = require('./lib_instance.js');
//
// 上面的顺序里，本模块早就求值完了，EXE 恒为缺省的 `bin/wb2api-server.exe`
// —— 而那常常是**用户正在跑的旧构建**。症状极具误导性：探针会去起那个旧版本，
// 于是"裸 /ui 吐控制台外壳 + 页面里有明文 __WB2API_KEY__"这类**上一世代**的
// 行为被当成当前代码的 bug 来查（实测有人整轮断言作废）。
//
// 判据只能是"加载时的值 vs 用的时候的值"，所以两处都记下来比较。
const EXE_AT_LOAD = EXE;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ─────────────────────────────────────────────────────────────────────────────
// describeBuild / assertBuildFresh —— "本次起的是哪个构建"（2026-10-19 加）
//
// # 为什么要成为 startInstance 的**内置**步骤
//
// `internal/server/*.html` 走 //go:embed：**HTML 改了不重建，浏览器拿到的就是
// 内嵌的旧页面**。而这里的失败形态极其误导 —— 它看起来永远像"前端没生效"，
// 于是整轮排查都花在前端代码上（本仓吃过这个亏；`verify_login_visual_e2e.js`
// 的 :419-431 就是为了这个场景写的"⓪ 本次验的是哪个构建"）。
//
// 那个探针是**只报不判**（它要能验旧构建做对照）。这里是**判**：`startInstance`
// 是"起实例"的唯一入口，从这里起来的一定是"要拿来做断言"的那个 ——
// 所以它必须是新的，否则后面所有断言的归因都是错的。
//
// # 三条刻意不做的
//
//   ① **不兜底到 `-probe.exe`**：EXE 缺省就是 `bin/wb2api-server.exe`，
//      静默换成别的会让"验的是哪个二进制"变成猜的（正是上面那个坑）。
//   ② **不做入口自动 `go build`**：构建是有副作用的动作（还会和正在跑的实例
//      抢文件锁），不该藏在"起实例"里。报清楚 + 给命令行，让人自己决定。
//   ③ **不在模块加载时判**：加载时还没有"这次要起什么"的上下文，
//      而且会让 require 本身抛错（一堆只 require 不做事的工具会集体炸）。
// ─────────────────────────────────────────────────────────────────────────────

// 参与新鲜度比较的**源**文件（改了它们就必须重建二进制）。
// 只列会进 //go:embed 或影响页面内容的；列太多会把"无关的小改"也判成红。
const BUILD_SOURCES = ['internal/server/webui.html', 'internal/server/webui-login.html'];

/**
 * 报"本次要起的是哪个构建"：exe 路径 + mtime + 最新源文件 mtime + 是否新鲜。
 * 只收集事实，不打印、不抛。
 * @returns {{exe: string, exeMtime: Date|null, newestRel: string, newestMtime: Date|null, fresh: boolean|null}}
 */
function describeBuild() {
  let exeMtime = null;
  try { exeMtime = fs.statSync(EXE).mtime; } catch { /* 不存在，交给 assert 分支 */ }
  let newest = null;
  for (const rel of BUILD_SOURCES) {
    try {
      const m = fs.statSync(path.join(REPO, rel)).mtime;
      if (!newest || m > newest.mtime) newest = { rel, mtime: m };
    } catch { /* 该源文件不在这份工作树里：跳过 */ }
  }
  return {
    exe: EXE,
    exeMtime,
    newestRel: newest ? newest.rel : '(无源文件可比)',
    newestMtime: newest ? newest.mtime : null,
    // fresh === null 表示"无从判断"（取不到 mtime），此时调用方**不许**当成绿。
    fresh: (exeMtime && newest) ? (exeMtime >= newest.mtime) : null,
  };
}

/**
 * 判"实际要启动的那个 exe 的 mtime ≥ internal/server/webui.html 的 mtime"。
 *
 * # 为什么**没有** bypass 开关（2026-10-19 定：删掉了原先的 `opt.fatal`）
 *
 * 原先的签名是 `assertBuildFresh(warn, { fatal: false })`，而 `startInstance` 把它接成
 * `opts.allowStaleBuild`。当时全仓没有任何调用方传它 —— 也就是说它是一个**潜伏的绕过口**：
 * 将来谁想让红变绿，加一个布尔就够。而"要放行只能换一个新鲜 exe"是本仓的红线。
 *
 * 那"故意对**旧构建**做对照"怎么办？——**不要在 startInstance 里做**：
 * 那种对照只能放在**不经过本库**的独立脚本里，并且结论必须写明产品世代
 * （"这是在 04:17 那份构建上量的"）。本仓吃过这个亏：一整套探针在**已不复存在的产品世代**上取绿，
 * 直到换到当前构建才暴露（见 `verify_probe_console_loaded.js` 的文件头）。
 * 想让旧构建的页面能被量到，正确做法是**重建一个新的对照 exe**，而不是让判据闭嘴。
 * （视觉探针 `verify_login_visual_e2e.js:419-431` 的"只报不判"是它**自己**的两值比较，
 *   不受本判据约束；它同样要求当前构建，跑旧构建时它会明说。）
 *
 * @param {(msg: string) => void} [warn] 打印器；不给就 console.log
 * @returns {ReturnType<typeof describeBuild>}
 * @throws 构建过期 / 无法判断（取不到 mtime）时**一律抛**，没有第二条路
 */
function assertBuildFresh(warn) {
  const say = warn || ((m) => console.log(m));
  const b = describeBuild();
  const rebuild = 'go build -o ' + path.relative(REPO, b.exe).replace(/\\/g, '/') + ' ./cmd/server';
  say('  本次起的构建: ' + b.exe);
  say('    exe mtime      : ' + (b.exeMtime ? String(b.exeMtime) : '(文件不存在)'));
  say('    ' + b.newestRel.replace(/^/, '').padEnd(15, ' ') + ' mtime: ' +
    (b.newestMtime ? String(b.newestMtime) : '(取不到)'));
  if (b.fresh === null) {
    say('  ⚠ 无法判断构建是否新鲜（exe 不存在或源文件 mtime 取不到）—— ' +
      '此时**不当作绿**：后面所有断言的归因都不成立，先修构建。');
    throw new Error('无法判断构建新鲜度：exe=' + b.exe);
    return b;
  }
  if (!b.fresh) {
    say('  ✗✗ 构建**过期**：' + b.exe);
    say('       exe       mtime = ' + String(b.exeMtime));
    say('       ' + b.newestRel + ' mtime = ' + String(b.newestMtime) +
      '  （比 exe 新 ' + Math.round((b.newestMtime - b.exeMtime) / 1000) + ' 秒）');
    say('       ⇒ 浏览器拿到的是 //go:embed 进去的**旧**页面，' +
      '症状看起来永远像"前端没生效"（本仓吃过这个亏）');
    say('       重建命令: ' + rebuild);
    say('       （口径与 tests/frontend/verify_login_visual_e2e.js:419-431 那段一致；' +
      '那边只报不判，因为要能验旧构建做对照）');
    throw new Error('构建过期，拒绝在旧页面上断言：' + b.exe + ' < ' + b.newestRel);
  } else {
    say('    ⇒ 新鲜（exe 不早于 ' + b.newestRel + '）✓');
  }
  return b;
}

// 端口预检：**占用就大声红，不静默继续、不自动换端口**（2026-10-19 加）
//
// # 为什么"自动换端口"是错的
//
// 换端口看似友好，实则把一次**真实的碰撞**藏起来：探针 A 起了实例占着 P、
// 探针 B 以为自己在 P 上跑，实际量到的是 A 的实例 ⇒ 两条探针的结论互相污染，
// 而现场没有任何提示。本仓已记过一次这个亏（同端口并发 ⇒ 探针串台，
// 症状完全像产品缺陷）。
//
// 判据：能不能 bind。用 node 自己试一次，比解析 netstat 可靠（跨平台、无解析歧义）。
function assertPortFree(port) {
  const net = require('net');
  return new Promise((resolve) => {
    const srv = net.createServer();
    srv.once('error', (e) => {
      const busy = e && (e.code === 'EADDRINUSE' || e.code === 'EACCES');
      resolve({ free: false, reason: (e && e.code) || String(e && e.message) });
    });
    srv.once('listening', () => { srv.close(() => resolve({ free: true, reason: '' })); });
    srv.listen(port, '127.0.0.1');
  });
}

// 占用端口的进程（Windows 上点名 pid；非 Windows 尽力而为）。
// 只是为了报错信息能直接指向"谁占着"，拿不到就跳过。
function whoOwnsPort(port) {
  try {
    const { execFileSync } = require('child_process');
    if (process.platform === 'win32') {
      const out = execFileSync('powershell.exe', ['-NoProfile', '-Command',
        '(Get-NetTCPConnection -LocalPort ' + port + ' -State Listen -ErrorAction SilentlyContinue ' +
        '| Select-Object -First 1 -ExpandProperty OwningProcess)'], { encoding: 'utf8', timeout: 8000 });
      const pid = String(out || '').trim();
      return pid ? pid : '';
    }
    const out = execFileSync('sh', ['-c', 'lsof -nP -iTCP:' + port + ' -sTCP:LISTEN 2>/dev/null | awk \'NR==2{print $2}\''], { encoding: 'utf8', timeout: 8000 });
    return String(out || '').trim();
  } catch { return ''; }
}

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
  // 抓"require 之后才设 WB2API_EXE"那个静默坑（见 EXE_AT_LOAD 的注释）。
  // ⚠ 只警告**不抛**：探针可能就是故意要打那个旧二进制（对照实验）。
  const nowEXE = process.env.WB2API_EXE
    ? path.resolve(REPO, process.env.WB2API_EXE)
    : path.join(REPO, 'bin', 'wb2api-server.exe');
  if (nowEXE !== EXE_AT_LOAD) {
    console.log('⚠⚠ WB2API_EXE 是在 require(lib_instance) **之后**才设的 —— 本次仍用加载时那份：');
    console.log('     ' + EXE_AT_LOAD);
    console.log('     你想要的：' + nowEXE);
    console.log('     改法：把 process.env.WB2API_EXE = ... 挪到 require 之前。');
    console.log('     （否则验的是另一个二进制，现场表现极具误导性）');
  }
  if (!fs.existsSync(EXE)) {
    console.log('⚠⚠ 探针二进制不存在：' + EXE);
    console.log('     先构建：go build -o bin/wb2api-server-probe.exe ./cmd/server');
    console.log('     ⚠ 改了 internal/server/*.html 也必须重建：那些页面走 //go:embed，');
    console.log('       不重建的话浏览器拿到的是内嵌的**旧**页面（最容易误判成前端 bug）。');
  }
  // ① 构建新鲜度：**在起实例之前**判，因为它是后面所有断言的前提。
  //    ⚠ 没有 bypass：opts.allowStaleBuild 已删（2026-10-19）。要对旧构建做对照，
  //      只能走不经过本库的独立脚本并写明产品世代 —— 见 assertBuildFresh 的长注释。
  assertBuildFresh(console.log);

  // ② 端口预检：占用 ⇒ 大声红 + 点名 pid。**不静默继续、不自动换端口**。
  //    理由见 assertPortFree 上面的长注释（同端口并发 ⇒ 探针串台）。
  const pf = await assertPortFree(port);
  if (!pf.free) {
    const owner = whoOwnsPort(port);
    console.error('');
    console.error('  ✗✗✗ 端口 ' + port + ' 已被占用（' + pf.reason + '）' +
      (owner ? '，占用者 pid=' + owner : ''));
    console.error('       拒绝继续：同端口并发会让两条探针互相读到对方实例的数据，');
    console.error('       现场表现完全是"产品缺陷"，但排查方向全错（本仓记过这个亏）。');
    console.error('       ⚠ 本层**不会**替你换端口 —— 换掉就把这次真实碰撞藏起来了。');
    console.error('       处理：等下一条跑完再跑，或给一个空闲端口。');
    console.error('');
    throw new Error('端口 ' + port + ' 被占用' + (owner ? '（pid ' + owner + '）' : '（' + pf.reason + '）'));
  }

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
  // 让浏览器侧的共享层（lib_cdp.js 的 withToken）知道这把实例的身份令牌：
  // T7 起 /ui 需要登录，探针若是"用 startInstance 起的实例 + 浏览器打开 /ui"，
  // 就靠这一条环境变量自动带上 ?token=，不必逐个探针改（60 多个）。
  //
  // ⚠ 只覆盖，不清理：同一个进程里连起两个实例时以**最后**一个为准
  // （探针的常规形态是"起一个实例 → 用浏览器验它"，符合这个假设）。
  // 要明确不带：`withChrome({ token: false })`。
  process.env.WB2API_PROBE_TOKEN = key;
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

  // ③ `prepareTmp(tmp)`：**进程起来之前**的最后一次落盘机会。
  //
  // # 为什么必须有这个钩子（2026-10-19 实测踩到）
  //
  // 有些状态是**启动时一次性读进内存**的：`checkinlog.New(path, keepDays)`
  // 在 `cmd/server/main.go:156` 被调一次，把 `data/checkin-log.json` 读进
  // `Log.records`，之后 `PageAll()` 只读内存（写回才会重写文件）。
  //
  // 于是"实例起来之后再写那个文件"**完全没有效果**：内存里还是空的，
  // 而端点回的是内存。夹具实测：文件里 30 条，`/admin/checkin/history`
  // 回 `total=4` —— 那 4 条是**产品自己的**被动扫描刚写进去的
  // （2 个账号 × checkin/keepalive）。这个数字看起来像"过滤把 30 条砍成 4 条"，
  // 极容易被当成产品缺陷；真因是"灌晚了"。
  //
  // 所以给一个钩子，让调用方在 spawn 之前把这类"启动即读"的数据放好。
  if (typeof opts.prepareTmp === 'function') {
    try {
      opts.prepareTmp(tmp);
    } catch (e) {
      // 前置数据放不进去必须出声：后面所有断言的归因都会错。
      console.error('  ⚠ prepareTmp 抛了：' + (e && e.message ? e.message : e));
      throw e;
    }
  }

  const logPath = path.join(tmp, 'server.log');
  const out = fs.openSync(logPath, 'a');
  const readLog = () => { try { return fs.readFileSync(logPath, 'utf8'); } catch { return ''; } };

  // ③ 子进程收尾一律走 finally：起不来 / 就绪超时 / 中途抛异常，
  //    **三种情况都必须 kill**。漏一种就留下一个占着端口的孤儿进程，
  //    下一次运行的第一条断言就会是"端口被占用"—— 而那正是为了防串台才加的检查，
  //    于是"泄漏"会被误读成"环境脏"。
  let srv = null;
  try {
    // ⚠ cwd=tmp：config 里的相对路径（./auths、./data）**全部**落在临时目录。
    srv = spawn(EXE, ['-config', 'config.json'], { cwd: tmp, stdio: ['ignore', out, out] });

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
    if (!up) {
      throw new Error('实例未在 ' + base + ' 起来（pid=' + srv.pid + '）\n--- server.log ---\n' + readLog().slice(-2000));
    }

    // ④ stop() **必须等端口真的空出来**再返回（2026-10-19 加，C 段的收口）。
    //
    // # 为什么"kill 完就返回"是错的（决定性实验）
    //
    //	起实例前        绑定=free
    //	起实例后        绑定=busy:EADDRINUSE   监听者=pid 31444
    //	stop() 返回耗时 1 ms                    ← 只 1 毫秒就返回了
    //	  +  0 ms       绑定=busy:EADDRINUSE   监听者=(无监听者)  ← 进程已死，socket 还没让出
    //	  +200 ms       绑定=free              监听者=(无监听者)  ← 真正空出来
    //
    // ⇒ "stop() 完立刻同端口再起"会撞上本层**自己的**端口预检，报红说
    //   "端口被占用" —— 而那一刻**根本没有监听者**（连 pid 都点不出来）。
    //   真泄漏其实没有（200ms 内必然让出），是**时序**。
    //
    // ⚠ 两种"看起来能过"的绕过法都被本仓明确否掉：
    //   · 让调用方自己 sleep 200ms ⇒ 把时序竞态散进 60 多个调用点，
    //     漏一个就是随机红，且没人知道为什么。
    //   · 自动换端口 ⇒ 把一次真实碰撞藏起来（见 assertPortFree 的注释）。
    //   所以修在**这一处**：stop 等到"这个端口真能 bind"或子进程 exit 为止，
    //   有界（默认 5s）；**超时大声红**（进程没退干净），绝不静默放过。
    const stop = async () => {
      try { srv.kill(); } catch { /* 已退出 */ }

      const killDeadline = Date.now() + (opts.stopMs || 5000);
      // 先等进程真的退出（exit 事件比轮询端口更直接；已退出则立刻返回）
      await new Promise((resolve) => {
        if (srv.exitCode !== null || srv.signalCode !== null) return resolve();
        let done = false;
        const fin = () => { if (!done) { done = true; resolve(); } };
        srv.once('exit', fin);
        setTimeout(fin, Math.max(0, killDeadline - Date.now()));
      });

      // 再等"端口真能 bind" —— 进程死了但 socket 让出来还要一点点时间。
      let freed = false;
      while (Date.now() < killDeadline) {
        const pf2 = await assertPortFree(port);
        if (pf2.free) { freed = true; break; }
        await sleep(50);
      }
      if (!freed) {
        // 最后一搏：强杀一次，再多给一小段（Windows 上 SIGTERM 不一定立刻生效）
        try { srv.kill('SIGKILL'); } catch { /* 已退出 */ }
        const hard = Date.now() + 2000;
        while (Date.now() < hard) {
          const pf3 = await assertPortFree(port);
          if (pf3.free) { freed = true; break; }
          await sleep(50);
        }
      }
      if (!freed) {
        const owner = whoOwnsPort(port);
        console.error('');
        console.error('  ✗✗✗ stop() 之后端口 ' + port + ' **没能空出来**（等了 ' +
          ((opts.stopMs || 5000) + 2000) + 'ms）' + (owner ? '，占用者 pid=' + owner : ''));
        console.error('       实例进程没退干净 ⇒ 下一条探针会在自己的端口预检上撞红，');
        console.error('       而那看起来像"环境脏"，真因是这里泄漏了。');
        console.error('       这不静默放过：请查 pid ' + (srv.pid || '?') + ' 的 server.log。');
        console.error('');
        throw new Error('stop() 未能释放端口 ' + port + (owner ? '（pid ' + owner + '）' : ''));
      }
      if (opts.quietStop !== true) {
        // 只报事实，不报时间（避免变成"看起来像在等"的伪信号）
        console.log('  stop(): 端口 ' + port + ' 已确认空出（pid=' + srv.pid + '）');
      }
    };

    return { base, tmp, key, logPath, stop, readLog, pid: srv.pid };
  } catch (e) {
    // 到这里说明异常已经发生：**必须**把子进程收掉再往上抛。
    try { if (srv) srv.kill(); } catch { }
    throw e;
  } finally {
    // 无论走哪条分支都关掉日志句柄（Windows 上不关会锁着文件，
    // 下一次 rmSync(tmp) 会失败）。
    try { fs.closeSync(out); } catch { }
  }
}

module.exports = {
  startInstance, httpJson, httpGetText, sleep, REPO, EXE,
  // B 段：构建新鲜度（口径与 verify_login_visual_e2e.js:419-431 一致，那边只报不判）
  describeBuild, assertBuildFresh, BUILD_SOURCES,
  // C 段：端口预检（占用即红，不换端口）
  assertPortFree, whoOwnsPort,
};
