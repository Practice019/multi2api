// lib_cdp.js —— 无头 Chrome 截图/度量的共享层（2026-10-19 抽）。
//
// # 为什么抽
//
// `shot_heatmap.js` / `shot_jobs.js` / `shot_logs.js` 三个脚本里有大段逐字重复的
// 代码：起 Chrome、等调试口、连 WebSocket、`send`/`ev` 两个闭包、收集
// `Runtime.exceptionThrown` 与 `console.error`、轮询等待条件、截图写盘。
// 每加一个页面就要再抄一遍（第四遍就是这一份）——AGENTS 第 6 条说的正是这个。
//
// ⚠ 只抽**已验证过**的部分。三个老脚本**不动**：它们各自跑通并留下了证据，
//   为了统一而重写它们属于"没坏就别修"，收益是零、风险是有的。
//   新脚本用这一份，老的按原样留着。
//
// # 三条踩过的坑（都固化在这里，不用每次重新踩）
//
//   ① `Runtime.enable` **必须在 `Page.navigate` 之前** —— 否则加载期抛的异常
//      一条都收不到，你会得到"零错误"的假绿。
//   ② **轮询等待，不要固定 sleep** —— 冷启动时 `series` / `logs` 这类请求可能
//      要好几秒，固定等 9 秒曾让我拍到一张空白热力图，并误判"代码坏了"。
//   ③ 隐藏面板里量任何尺寸**恒为 0** —— 切面板之后要么等一帧，要么显式 sleep
//      一下再量（本项目踩过：在仪表盘上量热力图，全是 0）。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// withToken —— 给控制台 URL 带上身份令牌（T7 加登录之后，所有开 /ui 的探针都需要）。
//
// # 为什么放在**共享层**而不是逐个探针改
//
// T7 起 `/ui` 需要登录（未登录只渲染一张登录页）。改动前，探针导航到
// `http://127.0.0.1:PORT/ui` 就能看到控制台 —— 那靠的是"回环地址即凭据"那条
// 已删除的判据。于是 `tests/frontend/` 下**60 多个**开 /ui 的探针会一起红，
// 而它们绝大多数与登录无关（它们验的是表格列、按钮、面板切换）。
//
// 逐个改 60 个文件有两个坏处：改动面大到无法审阅；以后每加一个探针都要记得
// 这件事（漏了就是"点了没反应"的假红）。所以在唯一的浏览器入口这里统一处理。
//
// # 令牌从哪来
//
// `lib_instance.js` 起实例时把它的 `admin_token` 写进 `WB2API_PROBE_TOKEN`
// （探针起的实例本就是临时凭据，写在进程环境里不会外泄到别的地方）。
// 所以"用 startInstance 起的实例 + withChrome 打开的浏览器"这条链路自动带上身份。
//
// # 服务端怎么处理 `?token=`
//
// `internal/server/webui.go`：校验通过 ⇒ `Set-Cookie`（12 小时）+ **302 到干净的
// `/ui`**（令牌不留URL里，避免顺着历史记录 / Referer / 代理日志漏出去）。
// 所以浏览器最终停在无令牌的地址上，cookie 已生效。
//
// # 三条刻意的例外
//
//	opts.token === false  明确不要令牌 —— 要验**登录页本身**的探针必须这样写
//	                         （否则浏览器被自动登入，永远看不到登录页）
//	opts.token === 'xxx'  用指定令牌 —— 验"错误令牌进不去"这类断言
//	没有 WB2API_PROBE_TOKEN 环境变量  原样返回，行为与改动前完全一致
//	                         （例如探针要打**外部的、已经登录好的**实例）
function withToken(url, token) {
  if (typeof url !== 'string' || !url) return url;
  let tok;
  if (token === false) return url;
  if (typeof token === 'string' && token) tok = token;
  else tok = process.env.WB2API_PROBE_TOKEN || '';
  if (!tok) return url;
  // 只给控制台页面加：探针人造的其它 URL（静态资源、/admin/* 直连）不该被改。
  if (!/\/ui(\?|#|$)/.test(url)) return url;
  if (url.indexOf('token=') >= 0) return url; // 调用方自己带了，别覆盖
  return url + (url.indexOf('?') >= 0 ? '&' : '?') + 'token=' + encodeURIComponent(tok);
}

// probeApiKey —— 探针**自己**的 api_key（页内 fetch 用），不是产品注入的凭据。
//
// # 为什么需要它
//
// 改造前控制台由服务端注入 `window.__WB2API_KEY__`，几个探针直接拿它在页内
// `fetch('/v1/models')`。T5/T7 起服务端**从不**注入凭据（本次安全改造的目标之一）
// ⇒ 那个变量恒为 undefined ⇒ 探针带着空 Bearer 打后端 ⇒ 401 ⇒ 0 条 ⇒ 报
// "至少有一个上游分组（0）"。
//
// ⚠ 实测确认那是**探针前提过期**而非产品回归：同一批端点在改造前后回执逐字相同
//   （/admin/ui/manifest、/admin/stats、/admin/accounts 全一致）。
//
// 取值顺序：
//   ① opts.probeKey 显式给的字符串
//   ② process.env.WB2API_PROBE_KEY   —— 探针脚本自己设的
//   ③ process.env.WB2API_PROBE_TOKEN —— lib_instance 起实例时写的就是它，
//      而探针实例里 `api_key` 与 `admin_token` 同值（见 lib_instance 的合成 config）
//   ④ 空串（不注入；调用方自己想办法）
//
// ⚠ 变量名刻意**不复用** `__WB2API_KEY__`：产品一旦重新注入凭据就是安全事故，
//   两个名字分开，代码审阅时"产品又注入了"一眼可见。
// injectProbeKey(send) —— 给**自带 CDP 实现**的探针用的注入器。
//
// # 为什么需要（与 probeApiKey 同源，但是给另一族探针）
//
// 探针分两族：走 `withChrome` 的（共享层自动注入）与自带 CDP 引导段的
// （`verify_models_grouped` / `verify_info_modal` / `verify_model_provider_filter`
// / `panel_content_e2e` …）。后者在页内 `fetch('/v1/models')` 时读
// `window.__WB2API_KEY__`，而 T5/T7 起产品**不再**注入它 ⇒ 空 Bearer ⇒ 401 ⇒
// 0 条模型 ⇒ 报"至少有一个上游分组（0）"（实测：同一批端点回执逐字相同，
// 所以那不是产品回归，是探针前提过期）。
//
// 这个函数把**探针自己的**夹具变量写进页面（名字刻意不同，见 probeApiKey 的注释）。
// 用法（在 `Page.navigate` + 等待骨架之后、任何读模型的步骤之前）：
//
//	await injectProbeKey(send);          // 自带 send 的探针
//
// 返回 true 表示注入了（环境里没给密钥时返回 false，探针应把它当成硬失败，
// 否则会静默地验一个空目录 —— 那正是本仓库反复出现过的"假绿"）。
// pasteConsoleKey(ev, key) —— 像人一样把对话密钥粘进顶栏并点「应用」。
//
// # 为什么需要（T5/T7 之后控制台自己没有密钥了）
//
// 改造前控制台由服务端注入 api_key，`/v1/models` 随页面加载就自动有数据；
// 现在按设计**必须有人粘一次**（顶部那个「对话密钥」框，刻意不写 localStorage）。
// 于是"模型目录 / 对话测试 / 倍率"这些依赖 `/v1` 的面板，在新控制台上默认是
// **空**的 —— 这不是坏，是契约变了：探针要么走人的路径（粘一次），要么就
// 别断言"面板里有数据"。
//
// 本函数走人的路径：写 `#key.value` + 点 `#conn`（那正是 `wireTopBar` 接的
// 那个按钮：它设内部 KEY、清 `/v1/models` 缓存、然后 refresh()）。
//
// ev 是探针自己的求值函数（`await ev(expr)`）。返回 'ok' / 'no-input' / 'no-btn'，
// 探针应把非 ok 当硬失败（否则又是一次"量到空面板还以为绿了"的假绿）。
// consoleSource(ev) —— 取**浏览器里那份**控制台源码（已认证，含内联脚本）。
//
// # 为什么不能用裸 `http.get('/ui')`
//
// T5 起未认证的 `/ui` 回 302 到登录页；而很多探针的静态检查是
//
//	const html = await get(BASE);        // get() 是 http.get，**不跟 302**
//	ok(/function showInfo/.test(html), 'showInfo 已定义');
//
// 于是它们拿到的要么是空的 302 body、要么是登录页 —— 静态断言全红；更糟的是
// 反向断言（`!/alert\(/`）会**空洞通过**：空字符串里当然没有 alert。这是本仓库
// 反复吃过的那类亏（判据在空输入上恒真），所以这里既给正确来源，也顺手让调用方
// 能自检来源不是空的（见下面的 assertConsole 用法）。
//
// 走浏览器取的好处：那份 HTML 是**实际渲染的**（cookie/重定向/注入都已完成），
// 不需要探针自己重放鉴权。
async function consoleSource(ev) {
  const html = await ev('document.documentElement.outerHTML');
  return typeof html === 'string' ? html : '';
}
async function pasteConsoleKey(ev, key, url) {
  const k = key || probeApiKey(undefined, url);
  if (!k) return 'no-key';
  const r = await ev(`(function(){
    var el = document.getElementById('key');
    if (!el) return 'no-input';
    el.value = ${JSON.stringify(k)};
    var b = document.getElementById('conn');
    if (!b) return 'no-btn';
    b.click();
    return 'ok';
  })()`);
  return r;
}
async function injectProbeKey(send, explicit, url) {
  const pk = probeApiKey(explicit, url);
  if (!pk) return false;
  await send('Runtime.evaluate', {
    expression: 'window.__PROBE_API_KEY__ = ' + JSON.stringify(pk),
    returnByValue: true,
  });
  return true;
}
// consoleHtmlAuthed(url, token) —— 取控制台源码，**自己完成 token 换手**。
//
// # 为什么需要它（静态检查也过登录这关）
//
// 探针的"静态检查"段（读源码文本断 showInfo 在不在、有没有 alert(）跑在起浏览器
// **之前**，所以拿不到浏览器里那份已认证的 HTML。它原先直接 `http.get('/ui')`，
// 而 T5 起未认证的 /ui 回 **302 到登录页** ⇒ 静态断言全红；更糟的是反向断言
// （`!/alert\(/`）会**空洞通过** —— 空字符串里当然没有 alert。
//
// 本函数按产品的真实路径走：带 `?token=` 请求 → 拿 `Set-Cookie` → 用该 Cookie
// 再取干净的 /ui。⚠ 只跟**一次**，不递归（跟产品的 302 语义一致）。
//
// 返回 body 字符串；调用方**必须**自检"这真是控制台"（例如含 id="nav"），
// 否则结论在空输入上恒真 —— 那是本仓库反复吃过的那类亏。
async function consoleHtmlAuthed(url, token) {
  const tok = token || probeApiKey(undefined, url);
  let cookie = '';
  const hasQ = url.indexOf('token=') >= 0;
  if (hasQ || tok) {
    const first = hasQ ? url
      : url + (url.indexOf('?') >= 0 ? '&' : '?') + 'token=' + encodeURIComponent(tok);
    const r1 = await httpGetOnce(first, {});
    for (const c of (r1.headers['set-cookie'] || [])) {
      const m = String(c).match(/^(wb2api_admin_token=[^;]+)/);
      if (m) cookie = m[1];
    }
  }
  const clean = url.replace(/[?&]?token=[^&#]*/, '').replace(/\?$/, '');
  const r2 = await httpGetOnce(clean, cookie ? { Cookie: cookie } : {});
  return r2.body;
}

// httpGetOnce(u, headers) —— 单次 GET，**不跟重定向**，返回 {status, headers, body}。
// （Node 的 http.get 默认就不跟；这里显式写出来是为了让"为什么能拿到 302 的
//   Set-Cookie"这件事在代码里可读。）
function httpGetOnce(u, headers) {
  return new Promise((res, rej) => {
    const req = http.get(u, { headers: headers || {} }, (r) => {
      const c = [];
      r.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res({
        status: r.statusCode,
        headers: r.headers,
        body: Buffer.concat(c).toString('utf8'),
      }));
    });
    req.on('error', rej);
  });
}

function probeApiKey(explicit, url) {
  if (typeof explicit === 'string' && explicit) return explicit;
  const env = process.env.WB2API_PROBE_KEY || process.env.WB2API_PROBE_TOKEN;
  if (env) return env;
  // ④ 从探针自己的 URL 里取 `token=`。
  //
  // 为什么这一步是安全的：探针实例（lib_instance 起的那种）里 `api_key` 与
  // `admin_token` 写成**同一个值**，而 URL 上的 `?token=` 正是那个值 ⇒ 拿它当
  // /v1 的 Bearer 是自洽的。⚠ 只用于探针夹具，产品代码里绝不允许这样互相顶替。
  //
  // 没有它的话，那些"自带 CDP、只认一个 *_URL"的探针在套件里（没人设环境变量时）
  // 会退化成空 Bearer ⇒ 静默量到空目录 ⇒ 假红/假绿 —— 正是本仓库反复吃过的那类亏。
  if (typeof url === 'string' && url) {
    const m = url.match(/[?&]token=([^&#]+)/);
    if (m) return decodeURIComponent(m[1]);
  }
  return '';
}
function httpGet(u) {
  return new Promise((res, rej) => {
    http.get(u, (r) => {
      const c = [];
      r.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res(Buffer.concat(c).toString('utf8')));
    }).on('error', rej);
  });
}

// 起 Chrome、连上、执行 fn(ctx)，无论如何都收尾（关连接 + 杀进程）。
//
// ctx = { send, ev, errs, shot, sleep }
//   send(method, params)  → CDP 原始调用，返回整个消息
//   ev(expression)        → 求值并返回 value（returnByValue + awaitPromise）
//   errs                  → 收集到的异常/console.error 文本数组
//   shot(name)            → 截全页图写进 <repo>/shots/<name>，返回字节数
//
// opts = { port, url, width, height, readyExpr, outDir, token }
//   readyExpr  轮询表达式，返回真值即认为骨架就绪（默认等 document.body）
//   token      控制台身份令牌（见下方 withToken 的长注释）。
//              true/省略 ⇒ 自动取 process.env.WB2API_PROBE_TOKEN；
//              false      ⇒ 明确不带令牌（要验登录页本身的探针用这个）；
//              字符串      ⇒ 用这一把。
async function withChrome(opts, fn) {
  const CHROME = require('./chrome_path.js').resolveChrome();
  const port = opts.port;
  const width = opts.width || 1600;
  const height = opts.height || 1200;
  const navURL = withToken(opts.url, opts.token);
  if (navURL !== opts.url) {
    console.log('  身份：URL 带 ?token=（探针实例的 admin_token，换手后 302 到干净的 /ui）');
  }
  const outDir = opts.outDir || path.resolve(__dirname, '..', '..', 'shots');
  const profile = path.join(os.tmpdir(), 'chrome-cdp-' + port + '-' + Date.now());
  fs.mkdirSync(outDir, { recursive: true });
  try { fs.rmSync(profile, { recursive: true, force: true }); } catch { }

  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run',
    '--no-default-browser-check', '--remote-debugging-port=' + port,
    '--user-data-dir=' + profile, '--window-size=' + width + ',' + height, 'about:blank'],
    { stdio: 'ignore' });

  let ws = null;
  try {
    let target = null;
    for (let i = 0; i < 40; i++) {
      try {
        const list = JSON.parse(await httpGet('http://127.0.0.1:' + port + '/json/list'));
        target = list.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
        if (target) break;
      } catch { }
      await sleep(250);
    }
    if (!target) throw new Error('Chrome 没起来（调试口 ' + port + ' 连不上）');

    ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });

    let id = 0;
    const pend = new Map();
    const errs = [];
    // dialogs 收集页面上弹出的原生对话框（alert/confirm/prompt）。
    //
    // # 为什么必须有（2026-10-19 加）
    //
    // 测**销毁性按钮**必然撞上 `confirm()`（本仓「移除账号」「删除 API Key」
    // 都先弹一次确认）。headless 里没人处理这个对话框时它有两条坏结局：
    //
    //	· 页面**卡住**在 confirm 上 ⇒ 后面的请求一个都不发，看起来像"按钮坏了"
    //	· 浏览器自动取消 ⇒ 请求同样不发，**同样是假象**
    //
    // 两种都会把"测量"变成"误判"——而按钮到底有没有坏，只有接受确认之后
    // 看请求与回执才知道。所以这里默认**自动接受**，并把文案收集下来，
    // 让调用方能直接断言「弹的是哪句确认」。
    const dialogs = [];
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Page.javascriptDialogOpening') {
        dialogs.push({ type: m.params.type, message: m.params.message || '' });
        send('Page.handleJavaScriptDialog', { accept: true });
      } else if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails || {};
        errs.push('[异常] ' + (d.exception && d.exception.description ? d.exception.description : d.text));
      } else if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
        errs.push('[console.error] ' + (m.params.args || []).map((a) => a.value || a.description).join(' '));
      }
    };

    const send = (method, params) => new Promise((r) => {
      const i = ++id; pend.set(i, r);
      ws.send(JSON.stringify({ id: i, method: method, params: params || {} }));
    });
    const ev = async (expr) => {
      const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      return r.result && r.result.result ? r.result.result.value : undefined;
    };
    const shot = async (name) => {
      const r = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
      const b = Buffer.from(r.result.data, 'base64');
      const p = path.join(outDir, name);
      fs.writeFileSync(p, b);
      console.log('  已保存 ' + p + '（' + b.length + ' 字节）');
      return b.length;
    };

    // ① enable 先于 navigate
    await send('Runtime.enable');
    await send('Page.enable');
    await send('Page.navigate', { url: navURL });

    // ② 轮询等骨架
    const readyExpr = opts.readyExpr || 'document.body ? 1 : 0';
    let waited = 0;
    for (let i = 0; i < 60; i++) {
      if (await ev(readyExpr)) break;
      await sleep(500); waited += 500;
    }
    console.log('  骨架就绪（等待 ' + waited + 'ms）');

    // ③ 给页面注入**探针自己的** api_key 夹具。
    //
    // # 为什么需要（T5/T7 之后探针的旧前提失效）
    //
    // 改造前控制台由服务端注入 `window.__WB2API_KEY__`，于是好几个探针直接用它
    // 在页内 `fetch('/v1/models')`（`Authorization: Bearer <那个变量>`）。
    // T5/T7 起服务端**从不**注入凭据（这正是本次安全改造的目标之一）⇒ 那个变量
    // 恒为 undefined ⇒ 探针带着空 Bearer 打后端 ⇒ 401 ⇒ 拿到 0 条 ⇒ 报
    // "至少有一个上游分组（0）" 这类红。
    //
    // ⚠ 那是**探针的前提过期**，不是产品回归（实测：两边同一批端点回执逐字相同，
    //   见 docs/newapi-parity.md）。修法不是把密钥塞回产品页，而是探针自己带夹具：
    //   变量名刻意叫 `__PROBE_API_KEY__`（而不是复用旧名），这样"产品又注入凭据了"
    //   与"探针自带了夹具"在代码里一眼可辨 —— 旧名一旦重新出现就是安全事故。
    //
    // ⚠ 只对 /ui 注入（与 withToken 同一范围）；取值顺序见 probeApiKey()。
    if (opts.probeKey !== false) {
      const pk = probeApiKey(opts.probeKey, navURL);
      if (pk && /\/ui(\?|#|$)/.test(new URL(navURL).pathname + (new URL(navURL).search || ''))) {
        await ev('window.__PROBE_API_KEY__ = ' + JSON.stringify(pk));
      }
    }

    return await fn({ send, ev, errs, dialogs, shot, sleep, outDir });
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { ch.kill(); } catch { }
    await sleep(300);
  }
}

// 轮询到条件成立；超时返回最后一次的值。
// ⚠ 用它代替固定 sleep：冷启动耗时不可预测，固定等待不是拍空就是白等。
async function until(ev, expr, want, timeoutMs) {
  const deadline = Date.now() + (timeoutMs || 20000);
  let last;
  while (Date.now() < deadline) {
    last = await ev(expr);
    if (typeof want === 'function' ? want(last) : last === want) return last;
    await sleep(400);
  }
  return last;
}

// 切面板 + 等一帧。
// ③ 隐藏面板里量尺寸恒为 0 —— 这个 sleep 不是偷懒，是必须的。
async function showPanel(ev, key, ms) {
  await ev(`window.__wb2api__ && window.__wb2api__.showPanelByKey(${JSON.stringify(key)})`);
  await sleep(ms || 900);
}

module.exports = {
  probeApiKey,
  consoleHtmlAuthed,
  consoleSource,
  pasteConsoleKey,
  // withToken —— 为什么把它也导出去（原先只在模块内部被 withChrome 用）
  //
  // 有一族探针**自带 CDP 引导段**（自己 `spawn(Chrome)` + `send('Page.navigate')`），
  // 它们不走 `withChrome` ⇒ 漏掉了 `withChrome` 里那一行
  // `const navURL = withToken(opts.url, opts.token)`（本文件 :269）⇒ **裸 /ui**
  // ⇒ 拿到登录页 ⇒ `window.__wb2api__` 不存在 ⇒ 后面每条页面断言都是级联。
  //
  // 实测（2026-10-19）：`verify_all_buttons_e2e.js` / `verify_checkin_ui_e2e.js`
  // 正是这样在**登录页**上做断言的（6 / 25 条级联红）。
  // 而它们以前"绿"是靠被测二进制**早于登录门**（见那两个文件里 UI_URL 的长注释）。
  //
  // ⚠ 导出的是**同一份**实现：调用方别再就地写一份等价的（本仓已因"同源副本会漂"
  //   交过一次学费 —— 判据/实现复制两份之后，改一处忘一处就变成两套行为）。
  withToken,
  injectProbeKey, withChrome, until, showPanel, sleep, httpGet,
  // assertConsoleShell / consoleEntry —— 见下面那段的说明（自带 CDP 的探针共用）。
  assertConsoleShell,
  consoleEntry,
};

// ─────────────────────────────────────────────────────────────────────────────
// assertConsoleShell / consoleEntry —— "自带 CDP 引导段"的探针共用的两个小件
//
// # 为什么要有它们（2026-10-19，第三族"硬红体检"）
//
// 有些探针自己 `spawn(Chrome)` + `send('Page.navigate', {url: TARGET})`，
// 而 `TARGET = process.env.XXX_URL || 'http://127.0.0.1:18080/ui'` —— **裸 /ui**。
//
// 于是当环境里没人设那个变量时，它们量到的是**登录页**（或空体），
// 而它们的断言**全是正向的**（"下拉里显示 15"、"请求体里有 X"）⇒
// 红是红了（不是静默假绿），但**归因指错了地方**：
// 报的是"分页默认值不对"，真因是"页面根本不是控制台"。
// 排查时会先去翻产品代码 —— 这是本仓最贵的一类亏。
//
// 两条修法（都能用，建议都上）：
//   ① `consoleEntry(url)`：给入口补上 `?token=`，让页面**真的**是控制台；
//   ② `assertConsoleShell(ev, ok, url)`：开页后先**正向自证**外壳在，
//      把"页面不对"这件事在第一条断言里就大声说出来。
// ─────────────────────────────────────────────────────────────────────────────

/**
 * 给控制台入口补 `?token=`（已有就不动）。
 *
 * 令牌来源与 `probeApiKey` 同源：环境变量 → URL 上的 `?token=`。
 * 探针实例里 `api_key` 与 `admin_token` 是**同一个值**（见 probeApiKey 的注释），
 * 所以这里用 api_key 环境变量补 admin 门是自洽的。
 *
 * @param {string} url
 * @returns {string}
 */
function consoleEntry(url) {
  if (typeof url !== 'string' || !url) return url;
  // 只处理"控制台入口"这种 URL（路径以 /ui 结尾或含 /ui?），别给 /v1 之类乱加。
  if (!/\/ui(\?|$)/.test(url)) return url;
  if (/[?&]token=/.test(url)) return url;
  const tok = probeApiKey('', url) || process.env.WB2API_API_KEY || '';
  if (!tok) return url;
  return url + (url.indexOf('?') >= 0 ? '&' : '?') + 'token=' + encodeURIComponent(tok);
}

/**
 * 正向自证"打开的是控制台外壳"，并把结论作为一条断言记入。
 *
 * 返回一个对象，调用方可据此决定后续要不要继续（页面不对时继续跑没意义）。
 *
 * @param {(expr: string) => Promise<any>} ev  页内求值器
 * @param {(cond: any, msg: string) => void} ok 断言记录器
 * @param {string} url 入口 URL（仅用于报错时点名，令牌会被打码）
 * @returns {Promise<{ready: boolean, info: object}>}
 */
async function assertConsoleShell(ev, ok, url) {
  const info = JSON.parse(await ev(`JSON.stringify({
    hasApi: typeof window.__wb2api__ === 'object' && !!window.__wb2api__,
    hasNav: !!document.getElementById('nav'),
    hasContent: !!document.getElementById('content'),
    isLoginPage: !!document.getElementById('form') && !document.getElementById('nav'),
    bytes: document.documentElement ? document.documentElement.outerHTML.length : 0,
  })`));
  const shown = String(url || '').replace(/token=[^&]*/, 'token=***');
  ok(info.hasApi && info.hasNav && info.hasContent,
    '打开的是控制台外壳（__wb2api__ + #nav + #content 都在）' +
    ' —— 入口 ' + shown + '，实际 ' + JSON.stringify(info) +
    (info.isLoginPage ? ' ⇒ **停在登录页**：入口缺 ?token= 或令牌不对，下面每条页面断言都会级联' : ''));
  return { ready: !!(info.hasApi && info.hasNav && info.hasContent), info };
}
