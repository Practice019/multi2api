// verify_login_visual_e2e.js —— 「登录/账号/控制台」的**视觉端到端**验收（无头浏览器 + 截图）。
//
// 和其它探针的区别：别的探针量 HTTP 契约，这一份量**人眼看到的东西**。
// 目标是把"视觉正常"变成可判据的事实，而不是留一堆截图给人看。
//
// # 判据为什么必须是"在视口里、有尺寸、不遮挡"
//
// 「前端没生效」这一类故障有五种形态，**没有一种会让请求失败**，所以 HTTP 探针
// 全绿也照样可能是坏的：
//
//	① 元素存在但 `display:none`（CSS 选择器写错 / 成员模式样式漏了一档）
//	   ⇒ DOM 里有、`querySelector` 找得到，用户看不见。
//	② 元素存在但宽高为 0（被绝对定位挤掉、父容器高度塌陷、flex 里被压扁）
//	   ⇒ 截图上是"一片空白"，DOM 断言却全过。
//	③ 元素可见但在**视口之外**（顶栏被横幅顶下去、面板被挤到屏幕外）
//	   ⇒ 尺寸正常，但用户打开页面看不到。
//	④ 面板可点开但**内容是空的**（接口 500、懒加载没触发、渲染函数早退）
//	   ⇒ 面板"打开了"，截图里是白屏 —— 最像"没坏"的一种坏。
//	⑤ 元素互相**遮挡**（横幅压住顶栏按钮）
//	   ⇒ 看得见但点不到，用户只能反复点、以为按钮坏了。
//
// 所以每一步都同时量：**存在** + **visible** + **尺寸 > 0** + **在视口内** +
// **内容非空** + **关键元素不相交**，并且存一张截图作为可复核的现场。
//
// # 三条硬判据（全程）
//
//	· `console.error` 数组为空
//	· 未捕获异常（`Runtime.exceptionThrown`）数组为空
//	· `Network.loadingFailed` 数组为空（**排除 canceled / ERR_ABORTED**）
//
// ⚠ 第三条为什么要排除：切页/导航会**故意取消**在途请求，那会正常地产生
//   `loadingFailed(canceled=true)`。不排除的话，本探针的每一次 `Page.navigate`
//   都会凭空多一条"加载失败"，判据会变成"永远红"——那不是产品坏了，是尺子错了。
//   被排除掉的那几条会**打印出来**（不藏），真失败（超时/断连/协议错）一律留红。
//
// # 执行顺序与任务编号不同（刻意的）
//
// 身份是**全局状态**（一个 cookie jar、一个标签页），场景之间会互相干扰：
// `?token=` 会种下一把 HttpOnly 的管理令牌 cookie，之后任何"未认证裸开 /ui"
// 都会变成"已认证" ⇒ ②③ 永远看不到登录页。所以真实执行顺序按**身份阶段**排：
//
//	身份阶段               场景（任务编号）
//	无 cookie              ① 首开创建管理员 → ② 登录页 → ③ 注册视图/注册
//	成员会话               ④ 成员视图 → ⑤ 我的 API Key → ⑥ 改密码 → ⑦ 登出
//	（用新口令再登）        ⑨a 成员只看自己的 key
//	清 cookie + 令牌路径   ⑩ /ui?token= → 顶栏「管理令牌」、没有「改密码」
//	管理员会话             ⑧ 逐个面板 + ⑨b 管理员看全部 key
//	另一个实例（开人机校验） ⑪ 题目在页面上真的画出来了
//
// # --self-test：用**注入**证明判据有判别力
//
// 本探针的"桩"不是 HTTP 桩，而是**往真实页面里注入缺陷**（不改产品代码）：
//
//	坏桩 A  `#memberBanner{display:none!important}` → "可见性"判据必须红
//	坏桩 B  让 `/admin/apikeys` 的 GET 回 500      → "非空白屏"判据必须红
//	坏桩 C  页面里抛一次未捕获异常                  → "console 无异常"判据必须红
//
// 三个坏桩各自只要**一条**红就算判别力成立 —— 生产这个故障时，那条断言就是
// 唯一会亮的东西。
//
// # 用法
//
//	node tests/frontend/verify_login_visual_e2e.js                # 起隔离实例 + 无头浏览器，跑全流程
//	node tests/frontend/verify_login_visual_e2e.js --self-test    # 1 好 + 3 坏（注入式）
//	node tests/frontend/verify_login_visual_e2e.js --shot-dir=/tmp/shots   (或任意目录)
//
// ⚠ `WB2API_EXE` 必须在 `require('./lib_instance.js')` **之前**设
//   （lib_instance 在模块求值那一刻就把 EXE 定下来了；设晚了会静默回落
//   `bin/wb2api-server.exe` —— 那通常是用户正在跑的旧构建，症状极具误导性）。
//   本文件第一行就设，并允许外部覆盖。
'use strict';
// ⚠ 必须在任何 require 之前（lib_instance / lib_authapi 都会在加载时定下 EXE）。
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';

const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawn } = require('child_process');

const lib = require('./lib_authapi.js');           // 只用 makeReporter/pickPort（统一输出格式）
const { until, sleep } = require('./lib_cdp.js');  // 复用两个**纯函数**（不碰它的 withChrome，理由见文件头）
const instLib = require('./lib_instance.js');      // startInstance（隔离实例的唯一入口）

// 截图输出目录：**默认不写机器绝对路径**（用系统临时目录）。
//
// 为什么不用 `<tmp>\...` 那种字面量：仓库的内容审计（audit_tracked_content.js）
// 专门抓"被跟踪文件里的机器绝对路径 / 内网地址"这类东西 —— 那是把作者本机布局
// 泄进仓库、也让别人 clone 下来跑不通。`os.tmpdir()` 在本机就是同一个地方，
// 在别的机器上是它自己的临时目录，两边都对。要固定位置仍然可以 `--shot-dir=`。
const DEFAULT_SHOT_DIR = path.join(os.tmpdir(), 'wb2api-visual', 'shots');

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

function httpGet(u) {
  return new Promise((res, rej) => {
    http.get(u, (r) => {
      const c = [];
      r.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res(Buffer.concat(c).toString('utf8')));
    }).on('error', rej);
  });
}

// portFree 探一个端口是否空闲（CDP 端口用；服务端端口走 lib.pickPort）。
function portFree(port) {
  return new Promise((resolve) => {
    const net = require('net');
    const s = net.createServer();
    s.once('error', () => resolve(false));
    s.once('listening', () => s.close(() => resolve(true)));
    s.listen(port, '127.0.0.1');
  });
}

async function freeCdpPort(preferred) {
  for (const p of preferred) if (await portFree(p)) return p;
  for (let p = 9201; p <= 9299; p += 1) if (await portFree(p)) return p;
  throw new Error('找不到空闲的 CDP 端口（92xx 段）');
}

// 视口/尺寸判据（在页面里求值）。返回一个可序列化对象，便于把"实际"打进失败信息。
const BOX_JS = `(sel) => {
  const el = document.querySelector(sel);
  if (!el) return { found: false };
  const cs = getComputedStyle(el);
  const r = el.getBoundingClientRect();
  return {
    found: true, display: cs.display, visibility: cs.visibility, opacity: cs.opacity,
    hiddenAttr: !!el.hidden, w: Math.round(r.width), h: Math.round(r.height),
    x: Math.round(r.left), y: Math.round(r.top),
    inView: r.bottom > 0 && r.top < innerHeight && r.right > 0 && r.left < innerWidth,
    text: (el.textContent || '').replace(/\\s+/g, ' ').trim().slice(0, 90),
  };
}`;

// visible() 本探针的可见性判据：存在 + 非 display:none + 非 visibility:hidden +
// 不透明 + 宽高都 > 0。（`hidden` 属性单独判 —— 它是 HTML 的布尔属性，
// 但被 CSS 覆盖时可以"hidden 却可见"，那种情况要按 CSS 判。）
function visible(b) {
  return !!b && b.found === true && b.display !== 'none' && b.visibility !== 'hidden'
    && Number(b.opacity) > 0 && b.w > 0 && b.h > 0;
}

// img 判据一句话描述，失败信息里统一用它（避免每条断言各写一套措辞）。
const VDESC = '存在 + display≠none + visibility≠hidden + 宽高>0';

// 面板内容判据（在页面里求值）。
//
// # 为什么不是"innerText 非空"
//
// 面板里**永远**有文字：h2 标题、说明、按钮。innerText 非空对"白屏"毫无判别力
// （实测：一个只渲染出标题的面板 innerText 有几十个字）。所以这里数的是
// **结构化内容**：真数据行（apikey 行/表格行/日志行）、卡片、表单控件，
// 并且把"加载中/加载失败/HTTP 5xx"这类**失败文案**单独认出来 → 直接判红。
const PANEL_JS = `(key) => {
  const sec = document.querySelector('#content > section[data-key="' + key + '"]');
  if (!sec) return { found: false };
  const cs = getComputedStyle(sec);
  const r = sec.getBoundingClientRect();
  const t = (sec.innerText || '').replace(/\\s+/g, ' ').trim();
  const ids = new Set();
  sec.querySelectorAll('[data-apikey]').forEach(e => ids.add(e.getAttribute('data-apikey')));
  const rows = sec.querySelectorAll('table tbody tr, .logrow, .jobrow, tbody tr').length;
  const cards = sec.querySelectorAll('.card, .metric, .kv-row, .acctcard, .grow, details').length;
  const forms = sec.querySelectorAll('input, select, textarea').length;
  const failText = /读取失败|加载失败|请求失败|接口失败|连不上服务端|加载不出来|HTTP [45]\\d\\d|Failed to fetch/.test(t);
  // ⚠⚠ 「加载中」必须与「失败」**分开**：它是**瞬时**状态。
  //   第一版把它并进 failText，结果再也等不到面板画完 —— 点击导航那一瞬间快照到的
  //   就是「加载中…」，判据直接把"还没画完"读成"坏了"（请求日志面板就这样被误判成红，
  //   而截图里它明明有表头 + 「落盘日志为空」的空态）。分开之后：加载中 → 继续等；
  //   失败 → 停等并按失败判。
  const loadingText = /加载中|正在读取|载入中|读取中|请稍候/.test(t);
  const emptyText = /还没有|暂无|没有数据|无数据|无记录|未配置|为空|空/.test(t);
  const h2 = sec.querySelector('h2');
  return {
    found: true, display: cs.display, hiddenAttr: !!sec.hidden,
    w: Math.round(r.width), h: Math.round(r.height),
    keys: ids.size, rows: rows, cards: cards, forms: forms,
    failText: failText, loadingText: loadingText, emptyText: emptyText,
    head: h2 ? h2.innerText.replace(/\\s+/g, ' ').trim().slice(0, 30) : '',
    textLen: t.length, text: t.slice(0, 150),
  };
}`;

// panelNonBlank 把 PANEL_JS 的结果变成"非空白屏"判据。
//
// 成立的条件（缺一不可）：面板真的显示出来了、有尺寸、**没有**失败/加载中的文案，
// 并且有结构化内容（数据行/卡片/表单控件）**或**一句明确的空态文案。
// 「有一行字」不算 —— 面板里的 h2 标题与说明文字永远存在，对"白屏"零判别力。
function panelNonBlank(p) {
  if (!p || !p.found || p.hiddenAttr || p.display === 'none' || p.h <= 0) return false;
  if (p.failText || p.loadingText) return false;
  return (p.keys + p.rows + p.cards + p.forms) > 0 || p.emptyText;
}

// panelSettled 面板"画完了"（或明确失败了）—— 等这个，而不是等"非空白"。
//   还在加载 → 不是答案，继续等；
//   明确失败 → 是答案（按失败判）；
//   非空白   → 是答案（按通过判）。
function panelSettled(p) {
  return panelNonBlank(p) || !!p && p.found && !p.loadingText && p.failText;
}

// 矩形相交（遮挡判据）。边界相接不算相交。
function intersect(a, b) {
  if (!a || !b || !a.found || !b.found) return false;
  return !(a.x + a.w <= b.x || b.x + b.w <= a.x || a.y + a.h <= b.y || b.y + b.h <= a.y);
}

// ---------------------------------------------------------------------------
// CDP 驱动：起 Chrome、连上、给调用方一套"能判视觉"的原语
// ---------------------------------------------------------------------------
//
// # 为什么不用 lib_cdp 的 withChrome
//
// 三个本探针**必须**有、而它没有的能力：
//	① 收 `Network.loadingFailed`（它的 onmessage 只路由异常与 console.error）；
//	② **多次导航**（withChrome 只 navigate 一次，而本探针要在登录页/控制台之间来回）；
//	③ 清 cookie（身份阶段切换要靠它）。
// lib_cdp 是共享层且**本轮不许改**，所以这里自带一份最小驱动；两个纯函数
// （until / sleep）仍然复用，不重复实现。
//
// 与 withChrome 一致的两条约定照抄：`Runtime.enable` 必须在 `Page.navigate` **之前**
// （否则加载期的异常一条都收不到 ⇒ 假绿）；等待一律**轮询**，不固定 sleep。
async function withBrowser(opts, fn) {
  const CHROME = require('./chrome_path.js').resolveChrome();
  const port = opts.port;
  const width = opts.width || 1600;
  const height = opts.height || 1000;
  const outDir = opts.outDir;
  fs.mkdirSync(outDir, { recursive: true });
  const profile = path.join(os.tmpdir(), 'chrome-visual-' + port + '-' + Date.now());
  fs.rmSync(profile, { recursive: true, force: true });

  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run',
    '--no-default-browser-check', '--remote-debugging-port=' + port,
    '--user-data-dir=' + profile, '--window-size=' + width + ',' + height,
    '--hide-scrollbars', 'about:blank'], { stdio: 'ignore' });

  let ws = null;
  try {
    let target = null;
    for (let i = 0; i < 60; i += 1) {
      try {
        const list = JSON.parse(await httpGet('http://127.0.0.1:' + port + '/json/list'));
        target = list.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
        if (target) break;
      } catch { /* 还没起来 */ }
      await sleep(250);
    }
    if (!target) throw new Error('Chrome 没起来（调试口 ' + port + ' 连不上）');
    ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });

    let id = 0;
    const pend = new Map();
    const consoleErrs = [];
    const exceptions = [];
    const loadFailed = [];
    const logs = [];
    const dialogs = [];
    const httpErrors = [];
    // prompt 的答案队列：改密码那一步连着弹三次 prompt（当前/新/再输）。
    // 真实路径照样走（不是"把 window.prompt 换掉"），只是把答案喂给它 ——
    // 队列空了就用默认值（confirm 不读它）。
    const promptAnswers = [];
    let lastPrompt = '';

    const send = (method, params) => new Promise((r) => {
      const i = ++id; pend.set(i, r);
      ws.send(JSON.stringify({ id: i, method: method, params: params || {} }));
    });

    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      // ⚠ 每条错误记录都带 **时间戳**：finish() 用它把"哪条异常发生在哪一段场景"
      //   还原出来（对照 runProbe 里给 rep.section 打的时间点）。没有它，
      //   报告只能说"全程有 1 条异常"，而用户真正要问的是"什么时候会踩到"。
      const now = Date.now();
      if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
        consoleErrs.push({ t: now, text: (m.params.args || []).map((a) => a.value || a.description || '').join(' ').slice(0, 200) });
      } else if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails || {};
        exceptions.push({ t: now, text: String((d.exception && d.exception.description) || d.text).slice(0, 200) });
      } else if (m.method === 'Network.loadingFailed') {
        loadFailed.push({
          t: now, errorText: String(m.params.errorText || ''), canceled: !!m.params.canceled,
          type: String(m.params.type || ''), requestId: m.params.requestId,
        });
      } else if (m.method === 'Network.responseReceived') {
        // 只收 4xx/5xx：把"哪些请求失败了、什么状态码"变成可读的一张表 ——
        // 光看 Log 域那句「Failed to load resource: 404」是没法判断到底是
        // "按设计被拒"还是"接口没了"的（本实例两种都有）。
        const r = m.params.response || {};
        if (Number(r.status) >= 400) {
          httpErrors.push({ t: now, status: Number(r.status), url: String(r.url || ''), type: String(m.params.type || '') });
        }
      } else if (m.method === 'Log.entryAdded') {
        const en = m.params.entry || {};
        logs.push({ t: now, level: en.level, source: en.source, url: String(en.url || ''), text: String(en.text || '').slice(0, 160) });
      } else if (m.method === 'Page.javascriptDialogOpening') {
        // ⚠ 不处理对话框会把页面**卡死**（confirm/prompt 是同步阻塞的）：
        //   随后所有 Runtime.evaluate 都不返回，表现为"按钮点了没反应"。
        dialogs.push({ type: m.params.type, message: String(m.params.message || '') });
        const params = { accept: true };
        if (m.params.type === 'prompt') {
          params.promptText = promptAnswers.length ? String(promptAnswers.shift()) : '';
          lastPrompt = params.promptText;
        }
        send('Page.handleJavaScriptDialog', params);
      }
    };

    const ev = async (expr) => {
      const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return undefined;
      return r.result && r.result.result ? r.result.result.value : undefined;
    };
    const shot = async (name) => {
      const r = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
      const b = Buffer.from(r.result.data, 'base64');
      fs.writeFileSync(path.join(outDir, name), b);
      return b.length;
    };
    const nav = async (url, readyExpr, ms) => {
      await send('Page.navigate', { url });
      return until(ev, readyExpr, (v) => !!v, ms || 20000);
    };
    const click = (sel) => ev(`(() => { const e = document.querySelector(${JSON.stringify(sel)});`
      + ' if (!e) return "no-el"; e.click(); return "ok"; })()');
    const box = (sel) => ev(`(${BOX_JS})(${JSON.stringify(sel)})`);
    // ensureBox 量一次；不在视口内就先 scrollIntoView 再量（"滚动后可见"也算可见）。
    const ensureBox = async (sel) => {
      let b = await box(sel);
      if (b && b.found && !b.inView) {
        await ev(`(() => { const e = document.querySelector(${JSON.stringify(sel)});`
          + ' if (e) e.scrollIntoView({block:"center"}); return 1; })()');
        await sleep(250);
        b = await box(sel);
      }
      return b;
    };
    const setValue = (sel, v) => ev(`(() => { const e = document.querySelector(${JSON.stringify(sel)});`
      + ' if (!e) return "no-el"; e.value = ' + JSON.stringify(v)
      + '; e.dispatchEvent(new Event("input", {bubbles:true})); return "ok"; })()');
    const clearCookies = () => send('Network.clearBrowserCookies');

    // ① enable 全部先于任何 navigate
    await send('Runtime.enable');
    await send('Page.enable');
    await send('Network.enable');
    await send('Log.enable');
    // 剪贴板权限（尽力而为）：新建 key 后页面会 copyText()，无头环境默认拒绝，
    // 会在截图里留下一条「复制失败」的提示条 —— 那是环境产物，不是产品行为。
    await send('Browser.grantPermissions', {
      origin: opts.url ? String(opts.url).replace(/\/ui.*$/, '') : undefined,
      permissions: ['clipboardReadWrite', 'clipboardSanitizedWrite'],
    });

    return await fn({
      send, ev, shot, nav, click, box, ensureBox, setValue, clearCookies, until, sleep,
      consoleErrs, exceptions, loadFailed, logs, dialogs, httpErrors, promptAnswers,
      lastPrompt: () => lastPrompt,
    });
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { ch.kill(); } catch { }
    await sleep(250);
    try { fs.rmSync(profile, { recursive: true, force: true }); } catch { }
  }
}

// ---------------------------------------------------------------------------
// 场景执行器
// ---------------------------------------------------------------------------

// 登录页/控制台的判别标记（两个页面各有一组**互斥**的 id，见 webui.html 与
// webui-login.html：控制台有 #appMain 与 #toast，登录页有 #captchaRefresh 与
// #submit，两边没有重名 —— 实测 id="title" 在控制台里出现 0 次）。
const IS_LOGIN_JS = '(() => !!document.getElementById("captchaRefresh") && !document.getElementById("appMain"))()';
const IS_CONSOLE_JS = '(() => !!document.getElementById("appMain") && !document.getElementById("captchaRefresh"))()';


// ---------------------------------------------------------------------------
// 主流程
// ---------------------------------------------------------------------------

// INJECT 自检用的注入（不改产品代码，只往真实页面里塞缺陷）。
const INJECT = {
  // 坏桩 A：横幅被 CSS 藏掉 —— "看得见"判据必须红。
  banner_hidden: `(() => { const s = document.createElement('style');
    s.textContent = '#memberBanner{display:none!important}'; document.head.appendChild(s); return 1; })()`,
  // 坏桩 B：让读列表的接口回 500 —— "非空白屏"判据必须红。
  panel_api_500: `(() => { const of = window.fetch;
    window.fetch = function (u, opt) {
      const method = String((opt && opt.method) || 'GET').toUpperCase();
      if (method === 'GET' && String(u).indexOf('/admin/apikeys') >= 0) {
        return Promise.resolve(new Response('{"error":{"code":"injected","message":"注入的 500"}}',
          { status: 500, headers: { 'Content-Type': 'application/json' } }));
      }
      return of.apply(this, arguments);
    }; return 1; })()`,
  // 坏桩 C：抛一次未捕获异常 —— "console/异常为空"判据必须红。
  page_throw: `setTimeout(() => { throw new Error('注入的未捕获异常（自检用）'); }, 0)`,
};

async function runProbe(opt) {
  const rep = opt.rep;
  const defects = opt.defects || {};
  const shotDir = opt.shotDir;
  const stopAt = opt.stopAt || '';
  const S = { shots: [] };
  const idx = String(opt.seed || 0);

  rep.section('⓪ 本次验的是哪个构建（视觉判据最容易"验了旧页面"）');
  rep.info('二进制: ' + instLib.EXE);
  try {
    const st = fs.statSync(instLib.EXE);
    rep.info('  mtime: ' + String(st.mtime) + '　大小: ' + st.size + ' 字节');
    // ⚠ internal/server/*.html 走 //go:embed：HTML 改了不重建，浏览器拿到的是
    //   内嵌的**旧**页面 —— 最容易误判成"前端没生效"。只报不判。
    const newest = ['internal/server/webui.html', 'internal/server/webui-login.html']
      .map((rel) => { try { return { rel, m: fs.statSync(path.join(instLib.REPO, rel)).mtime }; } catch { return null; } })
      .filter(Boolean).sort((x, y) => y.m - x.m)[0];
    if (newest && newest.m > st.mtime) {
      rep.info('  ⚠ 二进制比 ' + newest.rel + '（' + String(newest.m) + '）旧 —— 改了 HTML 必须 go build 再跑。');
    }
  } catch (e) { rep.info('  取不到该文件: ' + lib.short(e && e.message, 80)); }
  rep.info('截图目录: ' + shotDir);
  if (Object.keys(defects).length) rep.info('⚠ 本次是**注入自检**：' + Object.keys(defects).join(', '));

  // 起实例（主实例：默认关人机校验，见 lib_instance 的长注释）
  const port = await lib.pickPort([7890 + Number(idx) * 2, 7892, 7894, 7896, 7898]);
  const inst = await instLib.startInstance({ port, apiKey: 'probe-visual-key-' + port, auths: {} });
  const cdpPort = await freeCdpPort([9201 + Number(idx)]);
  rep.info('隔离实例: ' + inst.base + '（pid=' + inst.pid + '）　CDP 端口: ' + cdpPort);

  let fatal = null;
  try {
    await withBrowser({ port: cdpPort, url: inst.base + '/ui', outDir: shotDir, width: 1600, height: 1000 }, async (pg) => {
      const rep2 = rep;
      // 把主流程塞进这里（一次浏览器会话贯穿所有身份阶段，见文件头"执行顺序"）。
      await flowAll({ pg, rep: rep2, S, defects, shotDir, stopAt, inst, idx });
    });
  } catch (e) {
    fatal = e;
  } finally {
    try { inst.stop(); } catch { }
    try { fs.rmSync(inst.tmp, { recursive: true, force: true }); } catch { }
  }
  if (fatal) {
    rep.ok(false, '流程跑完（浏览器/实例异常）', String((fatal && fatal.stack) || fatal).slice(0, 300));
  }
  // 收尾：临时目录清掉（探针起的实例都写在系统临时目录下的独立子目录里）。
  rep.info('收尾：已停止隔离实例并清理 ' + inst.tmp);
  return rep;
}

// flowAll 是整个视觉验收的主体：一次浏览器会话里跑完所有身份阶段。
async function flowAll(o) {
  const { pg, rep, S, defects, stopAt, shotDir, inst, idx } = o;
  S.base = inst.base;
  S.pass = null;
  S.shots = [];
  const seedTag = 'p-visual-' + (idx || 0);
  S.admin = { user: lib.freshUsername(seedTag + '-adm'), pass: 'Probe-pass-adm-' + lib.randToken(6) };
  S.member = { user: lib.freshUsername(seedTag + '-mem'), pass: 'Probe-pass-mem-' + lib.randToken(6) };
  S.memberNext = 'Probe-pass-mem2-' + lib.randToken(6);

  const shot = async (name) => {
    const n = await pg.shot(name);
    S.shots.push({ name, bytes: n });
    rep.info('截图 ' + name + '（' + n + ' 字节）');
    return n;
  };
  // 给每个 rep.section 打一个时间点：finish() 靠它把"哪条异常发生在哪一段"还原出来。
  // 不改报告器本身的格式，只在外面套一层记录。
  S.sections = [];
  const origSection = rep.section.bind(rep);
  rep.section = (title) => { S.sections.push({ t: Date.now(), title: title }); return origSection(title); };
  const P = (cond, msg, actual) => rep.ok(cond, msg, actual);

  // 继承 runFlow 的前三段（① ② ③）—— 这里直接内联，保持一个会话一条时间线。
  await (async function phase1() {
    rep.section('① 全新实例首开 /ui → 「创建第一个管理员」视图（不是登录表单）');
    await pg.clearCookies();
    await pg.nav(S.base + '/ui', '(() => !!document.getElementById("title"))()');
    const t01 = await pg.box('#title');
    P(t01.found && t01.text.includes('创建第一个管理员'),
      '首开 /ui 的标题是「创建第一个管理员」', `title="${t01 && t01.text}"`);
    const sub01 = await pg.box('#sub');
    P(sub01.found && sub01.text.includes('账号库为空'), '副标题点明"账号库为空、这个入口只出现一次"', sub01.text);
    const btn01 = await pg.box('#submit');
    P(btn01.found && btn01.text.includes('创建第一个管理员'), '提交按钮文案 = 「创建第一个管理员」', btn01.text);
    const sw01 = await pg.box('#switchRow');
    P(sw01.found && (sw01.hiddenAttr === true || sw01.display === 'none'),
      '空库首启**不出现**「注册/登录」切换行（没有账号可登录，给个来回切的入口只会误导）',
      `hidden=${sw01.hiddenAttr} display=${sw01.display}`);
    const f01 = await pg.ensureBox('#form');
    P(visible(f01), '登录表单可见且尺寸正常（' + VDESC + '）', JSON.stringify(f01));
    await shot('s01-bootstrap-create-admin.png');
    // 用**真实表单**建第一个管理员（顺带验"创建后立即登录"）
    await pg.setValue('#username', S.admin.user);
    await pg.setValue('#password', S.admin.pass);
    await pg.click('#submit');
    const ok01 = await pg.until(pg.ev, IS_CONSOLE_JS, (v) => !!v, 20000);
    P(!!ok01, '提交后自动登录并进入控制台（契约：创建后立即以该账号登录）', '控制台可见=' + ok01);
    const a01 = await pg.ensureBox('#appMain');
    P(visible(a01), '控制台主区可见（' + VDESC + '）', JSON.stringify(a01));
    await shot('s01b-admin-logged-in.png');
  })();
  if (stopAt === 's01') return finish(pg, rep, S, defects);

  await (async function phase2() {
    rep.section('② 未认证裸开 /ui → 登录页；页面里没有明文凭据');
    await pg.clearCookies();
    await pg.nav(S.base + '/ui', '(() => !!document.getElementById("title"))()');
    const t = await pg.box('#title');
    P(t.found && t.text.includes('登录控制台'), '裸开 /ui 是**登录页**（库已非空 ⇒ 不再是首启入口）', `title="${t && t.text}"`);
    const f = await pg.ensureBox('#form');
    P(visible(f), '登录表单可见（' + VDESC + '）', JSON.stringify(f));
    const pwType = await pg.ev('document.getElementById("password").type');
    P(pwType === 'password', '口令框类型 = password（不回显）', 'type=' + pwType);
    const sw = await pg.ensureBox('#switchLink');
    P(sw.found && !sw.hiddenAttr && visible(sw),
      '登录页有「没有账号，注册成员」入口（该实例开放注册）', `text="${sw && sw.text}"`);
    const keyType = await pg.ev('typeof window.__WB2API_KEY__');
    P(keyType === 'undefined',
      '★ 登录页里 `typeof window.__WB2API_KEY__ === "undefined"`（改造前服务端把明文钥匙注入页面；现在连这个变量都不存在）',
      'typeof = ' + keyType);
    await shot('s02-login-anon.png');
  })();
  if (stopAt === 's02') {
    // 坏桩 C 的注入点：种下一个未捕获异常。留一拍让 Runtime.exceptionThrown
    // 真的到达驱动 —— 否则 finish() 里那条判据可能在事件之前就取数（假绿）。
    await injectIf(pg, defects, 'page_throw', rep);
    await pg.sleep(800);
    return finish(pg, rep, S, defects);
  }

  await (async function phase3() {
    rep.section('③ 登录页切到注册视图 → 注册成员 → 回登录页');
    await pg.click('#switchLink');
    await pg.until(pg.ev, '(() => { const t = document.getElementById("title"); return !!t && t.textContent.indexOf("注册成员") >= 0; })()', (v) => !!v, 8000);
    const t = await pg.box('#title');
    P(t.found && t.text.includes('注册成员账号'), '切到注册视图后标题 = 「注册成员账号」', `title="${t && t.text}"`);
    const b = await pg.box('#submit');
    P(b.found && b.text === '注册', '按钮文案变成「注册」', `text="${b && b.text}"`);
    const sw = await pg.box('#switchLink');
    P(sw.found && sw.text.includes('去登录'), '切换链接反向变成「已有账号，去登录」', `text="${sw && sw.text}"`);
    await shot('s03-register-view.png');
    await pg.setValue('#username', S.member.user);
    await pg.setValue('#password', S.member.pass);
    await pg.click('#submit');
    const regOk = await pg.until(pg.ev, IS_CONSOLE_JS, (v) => !!v, 20000);
    // ⚠ 与任务书的差异（按实测报）：注册成功会**自动登录**并 replace 到 /ui；
    //   "回登录页"的真实路径是紧接着的一次「登出」，见下一条。
    P(!!regOk, '注册成功后**自动登录**进入控制台（真实契约：注册 OK 后再打一次 login → location.replace("/ui")）；'
      + '任务书写的"注册→回登录页"与实现不符，按实测报', '控制台可见=' + regOk);
    await shot('s03b-member-after-register.png');
    await pg.click('#btnLogout');
    const back = await pg.until(pg.ev, IS_LOGIN_JS, (v) => !!v, 15000);
    P(!!back, '③b 真实「登出」后回登录页（这才是"回登录页"的路径）',
      '登录页=' + back + ' 最近对话框=' + JSON.stringify(pg.dialogs.slice(-1)));
    await shot('s03c-back-to-login.png');
    await pg.clearCookies();
  })();
  if (stopAt === 's03') return finish(pg, rep, S, defects);

  // ---- ④ 成员登录 → 控制台 + 成员视图 -------------------------------------
  await (async function phase4() {
    rep.section('④ 成员登录 → 控制台首屏 + 成员视图（#memberBanner 可见、顶栏有「改密码」「登出」）');
    await pg.nav(S.base + '/ui', '(() => !!document.getElementById("title"))()');
    await pg.setValue('#username', S.member.user);
    await pg.setValue('#password', S.member.pass);
    await pg.click('#submit');
    const ok = await pg.until(pg.ev, IS_CONSOLE_JS, (v) => !!v, 20000);
    P(!!ok, '成员登录成功并进入控制台（不是停在登录页报错）', '控制台可见=' + ok);
    // 等成员模式生效：横幅出现（它由 applyMemberMode() 在 /status 403 之后打开）
    const bannerShown = await pg.until(pg.ev,
      '(() => { const b = document.getElementById("memberBanner"); return !!b && getComputedStyle(b).display !== "none" && b.getBoundingClientRect().height > 0; })()',
      (v) => !!v, 15000);
    if (defects.banner_hidden) await injectIf(pg, defects, 'banner_hidden', rep);
    const banner = await pg.ensureBox('#memberBanner');
    P(!!bannerShown && visible(banner),
      '★ `#memberBanner` 可见且尺寸 > 0（成员模式生效：/status 对成员 403 ⇒ 横幅打开）　' + VDESC,
      JSON.stringify(banner));
    P(banner.found && banner.text.indexOf('成员') >= 0 && banner.text.indexOf('管理员') >= 0,
      '横幅文案说清了"哪些属于管理员"（避免成员以为面板点不动是坏了）', banner.text);
    const who = await pg.ensureBox('#whoami');
    P(visible(who), '顶栏 `#whoami` 可见（显示当前登录者）', JSON.stringify(who));
    const bc = await pg.ensureBox('#btnChpass');
    const bl = await pg.ensureBox('#btnLogout');
    P(visible(bc) && bc.text.includes('改密码'), '顶栏有「改密码」按钮且可见', JSON.stringify(bc));
    P(visible(bl) && bl.text.includes('登出'), '顶栏有「登出」按钮且可见', JSON.stringify(bl));
    P(!intersect(banner, bc) && !intersect(banner, bl),
      '★ 横幅与顶栏两个按钮**不相交**（横幅不会压住按钮 —— 看得见但点不到是最难受的一种坏）',
      `banner=${JSON.stringify({ x: banner.x, y: banner.y, w: banner.w, h: banner.h })}`
      + ` 改密码=${JSON.stringify({ x: bc.x, y: bc.y, w: bc.w, h: bc.h })}`
      + ` 登出=${JSON.stringify({ x: bl.x, y: bl.y, w: bl.w, h: bl.h })}`);
    const nameTxt = await pg.ev('document.getElementById("whoami").textContent');
    P(String(nameTxt).indexOf(S.member.user.slice(-6)) >= 0 || String(nameTxt).indexOf('成员') >= 0,
      '顶栏显示了当前成员的身份（用户名或「成员」标记）', 'whoami="' + nameTxt + '"');
    // 控制台页面里也不能有明文凭据（占位符不算凭据）
    const keyVal = await pg.ev('String(window.__WB2API_KEY__)');
    P(keyVal === '__WB2API_KEY__' || keyVal === 'undefined' || keyVal === '',
      '★ 控制台页面里的 `window.__WB2API_KEY__` **不是真凭据**（是未注入的占位符/空）',
      'value="' + keyVal + '"（真钥匙是 ' + lib.short(inst.key, 10) + '…，不允许出现在页面里）');
    P(keyVal.indexOf(inst.key) < 0, '★ 页面里没有实例的那把真钥匙', 'value="' + keyVal + '"');
    // 成员不该看到管理员面板（视觉上）
    const navInfo = await pg.ev(`(() => {
      const secs = Array.from(document.querySelectorAll('#content > section'));
      return Array.from(document.querySelectorAll('#nav button[data-nav]')).map(b => {
        const s = secs[+b.dataset.nav];
        return { key: s ? s.dataset.key : '', label: b.textContent.trim(), hidden: !!(s && s.hidden),
                 secDisplay: s ? getComputedStyle(s).display : '' };
      });
    })()`);
    S.memberNav = navInfo || [];
    const acctNav = S.memberNav.filter((n) => n.key === 'accounts');
    const acctHidden = acctNav.length === 0 || acctNav.every((n) => n.hidden || n.secDisplay === 'none');
    P(acctHidden, '★ 成员视图里「账号池」面板**不显示**（member-mode 样式把它藏了）',
      JSON.stringify(acctNav));
    rep.info('成员可见导航: ' + JSON.stringify(S.memberNav.map((n) => n.label)));
    const dotTxt = await pg.ev('document.getElementById("htxt").textContent');
    P(String(dotTxt).indexOf('失败') < 0 && String(dotTxt).indexOf('失效') < 0,
      '顶栏状态点不是失败/失效（成员模式下也正常）', 'htxt="' + dotTxt + '"');
    await shot('s04-member-console.png');
  })();
  if (stopAt === 's04') return finish(pg, rep, S, defects);

  // ---- ⑤ 成员「我的 API Key」：新建 → 出现 → 删除 → 消失 -------------------
  await (async function phase5() {
    rep.section('⑤ 成员「我的 API Key」：新建一把 → 列表出现 → 删除 → 消失');
    // 坏桩 B 的注入点：**第一次**打开这个面板之前（`loadAPIKeys()` 走 30s TTL 的
    // sharedGet，所以必须是"这个会话里第一次拉列表"才不会被缓存挡住）。
    // 注入的是页面里的 fetch（同一次导航内有效），不是改产品代码。
    if (defects.panel_api_500) await injectIf(pg, defects, 'panel_api_500', rep);
    await openPanel(pg, 'apikeys');
    let p = await pg.ev(`(${PANEL_JS})('apikeys')`);
    P(panelNonBlank(p), '「API 管理」面板打开且非空白屏（新的成员账号名下 0 把 key）', JSON.stringify(p));
    P(p.keys === 0 && p.emptyText === true,
      '空态：0 把 key，且给出**明确的空态文案**（不是白屏）', JSON.stringify({ keys: p.keys, emptyText: p.emptyText, text: p.text }));
    P(p.head.indexOf('API') >= 0 || p.head.indexOf('Key') >= 0 || p.head.indexOf('key') >= 0,
      '面板标题是「API 管理」（成员看到的是自己名下的 key）', 'h2="' + p.head + '"');
    await shot('s05a-member-keys-empty.png');

    const btnNew = await pg.ensureBox('#btnApiKeyNew');
    P(visible(btnNew), '「新建 API Key」按钮可见（' + VDESC + '）', JSON.stringify(btnNew));
    await pg.click('#btnApiKeyNew');
    const modal = await pg.ensureBox('#akModal');
    P(visible(modal), '点「新建」后弹窗可见（不是点了没反应）', JSON.stringify(modal));
    const nameBox = await pg.ensureBox('#akName');
    P(visible(nameBox), '弹窗里的名称输入框可见', JSON.stringify(nameBox));
    await pg.setValue('#akName', 'probe-visual-key');
    await pg.click('#akSave');
    // ⚠ 这一段是本次探针**最有价值**的地方，所以判据写成三条互相独立的陈述，
    //   而不是一句"列表里有没有"：
    //     ① 保存请求成功了吗（提示条不是失败文案）
    //     ② 列表**立即**（≤3s）出现吗　← 项目自己的标准：删除路径专门为此加了
    //        writeInvalidate，注释里写着"不丢的话列表会把刚删掉的那把 key 原样画
    //        回来（实测 20s 不消失）"，那种现象用户读到的就是"点了没反应"。
    //     ③ 若没立即出现：它是**永远**不出现，还是靠轮询/重开面板才出现
    //        （③ 才是"成员会话下 refresh() 抛异常 ⇒ 30s 轮询根本没注册"的证据）
    const t0 = Date.now();
    const grew3s = await pg.until(pg.ev, `(() => { const p = (${PANEL_JS})('apikeys'); return p.keys; })()`, (v) => v === 1, 3000);
    const dt3 = Date.now() - t0;
    const toast0 = await pg.ev('(() => { const t = document.getElementById("toast"); return t && !t.hidden ? t.textContent : ""; })()');
    P(!/保存失败|创建失败|名称不能为空/.test(String(toast0)),
      '保存请求本身成功（提示条不是失败文案 —— 无头环境里剪贴板被拒会显示「复制失败」，'
      + '那恰好证明服务端回了完整 key）', 'toast="' + String(toast0).slice(0, 60) + '"');
    P(grew3s === 1,
      '★★ 保存后列表**立即**出现这把 key（判据 ≤3000ms）—— 与删除那条（毫秒级）同标准',
      '3000ms 内没出现（' + dt3 + 'ms 时 DOM 里的 key 数=' + (grew3s === undefined ? '0' : grew3s) + '）'
      + '　⚠ 真因：新建成功后的刷新路径**没有** writeInvalidate([EP_APIKEYS])，'
      + '`loadAPIKeys()` 命中 sharedGet 的 30s TTL 旧快照（删除 8279 / 重置 8265 / 切换都有，新建漏了）');

    // ③ 它会不会靠自己出现？把面板**保持开着**等 35 秒（>30s TTL + 一个 30s 轮询周期）。
    //    管理员的控制台有 30s 无感轮询（pollAPIKeys）会把它拉出来；成员会话里
    //    refresh() 在 11803 抛异常 ⇒ 后面那段 setInterval **根本没注册** ⇒ 永远不出现。
    const polled = await pg.until(pg.ev, `(() => { const p = (${PANEL_JS})('apikeys'); return p.keys; })()`, (v) => v === 1, 35000);
    P(polled === 1, '（对照 F1）成员会话里，面板**保持开着**时列表会自己刷新出来（30s 轮询应当把它拉出来）',
      polled === 1 ? '出现了' : '等了 35s 仍未出现 ⇒ 成员会话下这段 30s 轮询没在跑（对应末尾那条 TypeError 的归属）');

    // 恢复现场（不掩盖问题，只是让后续步骤有行可点）：等过 TTL 后重开面板 —— 这就是
    // 用户唯一可行的绕法（等 30 秒 / 重开面板 / 整页刷新；点「立即刷新」对成员无效）。
    const rec = await waitKeys(pg, (n) => n === 1, 20000);
    rep.info('绕法实测：重开面板 ' + rec.ms + 'ms 内看到那把 key（key 数=' + rec.keys + '）');
    P(rec.ok, '★ 重开面板后能看到刚建的那把 key（不是根本建不上，是界面不刷新）',
      'key 数=' + rec.keys + '（耗时 ' + rec.ms + 'ms）');
    const afterRe = rec.keys;
    p = await pg.ev(`(${PANEL_JS})('apikeys')`);
    P(!p.failText, '列表没有"加载失败/请求失败"文案', 'failText=' + p.failText + ' text=' + p.text);
    S.memberKeyId = await pg.ev(`(() => { const e = document.querySelector('#apiKeyRows [data-apikey]'); return e ? e.getAttribute('data-apikey') : ''; })()`);
    P(!!S.memberKeyId, '成员这把 key 的 id 能从 DOM 行里读出来（供 ⑨ 与管理员视图对比）', 'id=' + S.memberKeyId);
    await shot('s05b-member-key-created.png');

    // ⚠ 删除路径**有** writeInvalidate（8279），所以这一条应当是毫秒级 —— 与上一条
    //   形成对照：同一张表、同一个缓存，两条写操作的刷新行为不一致。
    const dialogsBefore = pg.dialogs.length;
    const tDel = Date.now();
    await pg.click('#apiKeyRows button[data-act="delete"]');
    const gone = await pg.until(pg.ev, `(() => { const p = (${PANEL_JS})('apikeys'); return p.keys; })()`, (v) => v === 0, 20000);
    const dtDel = Date.now() - tDel;
    P(gone === 0, '★ 删除后列表里**消失**（回到 0 把）', 'DOM 行里的 key 数=' + gone + '，耗时 ' + dtDel + 'ms');
    const asked = pg.dialogs.slice(dialogsBefore);
    P(asked.some((d) => d.type === 'confirm' && d.message.indexOf('删除这把 API key') >= 0),
      '删除前弹了一次确认（销毁性动作要有确认；无头环境里由驱动自动接受）', JSON.stringify(asked));
    rep.info('对照（同一张表、同一个缓存）：删除丢缓存 ⇒ ' + dtDel + 'ms 内消失；新建不丢 ⇒ 立即出现判据失败');
    await shot('s05c-member-key-deleted.png');

    // 再建一把留着给 ⑨（对比"管理员看全部 vs 成员看自己"）—— 单独的断言，别混淆 ⑤ 的结论。
    // ⚠ 这里**不**再验"立即出现/最终出现"：那已经由上面三条独立陈述报过（同一个缺陷
    //   重复报三次只会淹没新信息）。这里只验"POST 成功"，它的可见性交给 ⑨a 在
    //   **过了 TTL、重开面板**之后按"成员只看到自己那把"的角色隔离判据一并验。
    await pg.click('#btnApiKeyNew');
    await pg.setValue('#akName', 'probe-visual-keep');
    await pg.click('#akSave');
    const toast2 = await pg.ev('(() => { const t = document.getElementById("toast"); return t && !t.hidden ? t.textContent : ""; })()');
    P(!/保存失败|创建失败|名称不能为空/.test(String(toast2)),
      '⑤b 再建一把（留给 ⑨ 与管理员视图对比）—— 保存请求成功', 'toast="' + String(toast2).slice(0, 50) + '"');
    await shot('s05d-member-second-created.png');
  })();
  if (stopAt === 's05') return finish(pg, rep, S, defects);

  // ---- ⑥ 改密码：成功提示 + 会话仍有效 -------------------------------------
  await (async function phase6() {
    rep.section('⑥ 顶栏「改密码」：改成功 → 提示出现 → **会话仍有效**（不跳登录页）');
    // 真实 prompt 路径：驱动替用户把三次输入喂进去（不是把 window.prompt 换掉）。
    pg.promptAnswers.push(S.member.pass, S.memberNext, S.memberNext);
    await pg.click('#btnChpass');
    const toastOk = await pg.until(pg.ev,
      '(() => { const t = document.getElementById("toast"); return !!t && !t.hidden ? t.textContent : ""; })()',
      (v) => typeof v === 'string' && v.indexOf('口令已修改') >= 0, 20000);
    P(typeof toastOk === 'string' && toastOk.indexOf('口令已修改') >= 0,
      '★ 提示条出现且文案是「口令已修改…当前登录保持有效」（用户报障 ② 的正面证据）', 'toast="' + toastOk + '"');
    const dotOk = await pg.until(pg.ev, 'document.getElementById("htxt").textContent',
      (v) => typeof v === 'string' && v.indexOf('当前会话有效') >= 0, 10000);
    P(typeof dotOk === 'string' && dotOk.indexOf('当前会话有效') >= 0,
      '★ 改完页面自己又打了一次 /admin/auth/session 并显示「当前会话有效（无需重启）」',
      'htxt="' + dotOk + '"');
    const stillConsole = await pg.ev(IS_CONSOLE_JS);
    P(!!stillConsole, '★ 会话仍有效：**没有**跳登录页（改口令不该把人踢出去）', '控制台可见=' + stillConsole);
    const bc = await pg.ensureBox('#btnChpass');
    P(visible(bc), '顶栏「改密码」按钮还在（页面没被重置）', JSON.stringify(bc));
    const st = await pg.box('#toast');
    P(st.found && st.w > 0 && st.h > 0, '提示条本身有尺寸（不是零高的一条隐形提示）', JSON.stringify(st));
    await shot('s06-password-changed.png');
  })();
  if (stopAt === 's06') return finish(pg, rep, S, defects);

  // ---- ⑦ 登出 → 回登录页；再开 /ui 仍是登录页 -------------------------------
  await (async function phase7() {
    rep.section('⑦ 「登出」→ 回登录页；再直接开 /ui → 仍是登录页（cookie 真失效）');
    await pg.click('#btnLogout');
    const back = await pg.until(pg.ev, IS_LOGIN_JS, (v) => !!v, 15000);
    P(!!back, '点「登出」后回登录页', '登录页=' + back);
    await shot('s07a-after-logout.png');
    await pg.nav(S.base + '/ui', '(() => !!document.getElementById("title"))()');
    const t = await pg.box('#title');
    P(t.found && t.text.includes('登录控制台'),
      '★ 登出后再开 /ui **仍是登录页**（服务端会话真被撤销了，不只是清了客户端 cookie）', `title="${t && t.text}"`);
    const isConsole = await pg.ev(IS_CONSOLE_JS);
    P(!isConsole, '★ 拿不到控制台外壳（旧 cookie 不再放行）', '控制台可见=' + isConsole);
    await shot('s07b-reload-still-login.png');
  })();
  if (stopAt === 's07') return finish(pg, rep, S, defects);

  // ---- ⑨a 成员用**新口令**再登：只看到自己的 key ---------------------------
  await (async function phase9a() {
    rep.section('⑨a 成员以**新口令**登录 → 只看到自己名下的 key（同时证明⑥真的改了口令）');
    await pg.setValue('#username', S.member.user);
    await pg.setValue('#password', S.memberNext);
    await pg.click('#submit');
    const ok = await pg.until(pg.ev, IS_CONSOLE_JS, (v) => !!v, 20000);
    P(!!ok, '★ 用**新口令**登录成功（改口令不是"提示成功但没生效"）', '控制台可见=' + ok);
    // 等"成员名下只有 1 把"这一刻：反复重开面板覆盖 30s TTL 窗口（判据是**角色隔离**，
    // 不是"立即出现" —— 刷新时机那个缺陷已经在 ⑤ 里量过了，这里不复述）。
    const r9 = await waitKeys(pg, (n) => n === 1, 45000);
    const p = await pg.ev(`(${PANEL_JS})('apikeys')`);
    P(r9.ok, '★ 成员名下 1 把 key（刚建的那把）', 'key 数=' + r9.keys + '（' + r9.ms + 'ms 后）');
    S.memberKeys = await pg.ev('Array.from(new Set(Array.from(document.querySelectorAll("#apiKeyRows [data-apikey]")).map(e => e.getAttribute("data-apikey"))))');
    P(Array.isArray(S.memberKeys) && S.memberKeys.indexOf(S.memberKeyId === undefined ? '' : S.memberKeyId) < 0 && S.memberKeys.length === 1,
      '成员只看到自己那把（而他自己刚删掉的那把已经不在列表里）', JSON.stringify(S.memberKeys));
    const banner = await pg.ensureBox('#memberBanner');
    P(visible(banner), '成员横幅再次可见（重登后成员模式照常生效）', JSON.stringify(banner));
    await shot('s09b-member-keys-only.png');
  })();
  if (stopAt === 's09a') return finish(pg, rep, S, defects);

  // ---- ⑩ 令牌路径：/ui?token=<admin_token> --------------------------------
  await (async function phase10() {
    rep.section('⑩ 令牌路径：/ui?token=<admin_token> → 控制台 + 顶栏「管理令牌」+ **没有**「改密码」');
    await pg.clearCookies();
    await pg.nav(S.base + '/ui?token=' + encodeURIComponent(inst.key),
      '(() => !!document.getElementById("appMain"))()');
    const ok = await pg.until(pg.ev, IS_CONSOLE_JS, (v) => !!v, 20000);
    P(!!ok, '带 ?token= 打开 /ui 进控制台', '控制台可见=' + ok);
    const loc = await pg.ev('location.pathname + location.search');
    P(loc === '/ui', '★ 换手后地址栏是**干净的 /ui**（令牌不留 URL / 历史记录 / Referer）', 'location=' + loc);
    const who = await pg.until(pg.ev, 'document.getElementById("whoami").textContent',
      (v) => typeof v === 'string' && v.indexOf('管理令牌') >= 0, 15000);
    P(typeof who === 'string' && who.indexOf('管理令牌') >= 0,
      '★ 顶栏显示「管理令牌」（令牌身份没有用户名与角色，显示用户名才是错的）', 'whoami="' + who + '"');
    const chpass = await pg.box('#btnChpass');
    P(chpass.found === false, '★ 令牌身份**没有**「改密码」按钮（它没有口令可改，点只会 401）',
      JSON.stringify(chpass));
    const lo = await pg.ensureBox('#btnLogout');
    P(visible(lo), '★ 但「登出」要留（能清掉浏览器上这把钥匙）', JSON.stringify(lo));
    await shot('s10-token-console.png');

    // ---- ★ 这个身份的面板必须显示**这个身份**的数据 --------------------------
    //
    // 为什么必须单独验：`sharedGet` 的缓存键**只有 URL**（'ep:/admin/apikeys'），
    // 又落 localStorage、TTL 30 秒 —— 键里**没有身份**。于是"上一个身份在同一浏览器
    // 里拉过同一端点"时，新身份的面板会**直接命中那份旧快照**，一个字都不刷新。
    // 这一条就是它的判据：把 DOM 里渲染出的东西，和"当场另发一次请求"的真值对比。
    // 方向不重要（这里恰好是令牌身份看到了成员的数据；反过来就是成员看到管理员的
    // 列表 —— 同一个缺陷的两种形态）。
    await openPanel(pg, 'apikeys');
    const domN = await pg.ev(`(() => { const p = (${PANEL_JS})('apikeys'); return p.keys; })()`);
    const akText = String(await pg.ev('(document.getElementById("apiAdminKey")||{}).textContent||""'));
    const fresh = await pg.ev(`(async () => {
      const r = await fetch('/admin/apikeys', { headers: { 'Accept': 'application/json' } });
      const j = await r.json().catch(() => ({}));
      return { status: r.status, member: j.member, keys: Array.isArray(j.keys) ? j.keys.length : -1,
               admin_key: !!j.admin_key, admin_token: !!j.admin_token };
    })()`);
    P(fresh && fresh.status === 200 && fresh.member === false,
      '★ 令牌身份在服务端就是管理员（/admin/apikeys 回 member:false）—— 界面文案不该说反',
      JSON.stringify(fresh));
    P(akText.indexOf('属于管理员：当前账号看不到') < 0,
      '★★ 「管理钥匙」区块没有对令牌身份说"你是成员、看不到"（同一屏顶栏写着「管理令牌／管理员」，文案自相矛盾）',
      'apiAdminKey 文案="' + akText.slice(0, 90) + '"');
    P(fresh && domN === fresh.keys,
      '★★ 面板渲染的行数 = 当场接口回执的行数（同一身份、同一时刻，DOM 不许来自别的身份）',
      'DOM 里 ' + domN + ' 行 / 接口回执 ' + (fresh && fresh.keys) + ' 行'
      + '　⚠ 真因：sharedGet 缓存键只有 URL（没有身份）且落 localStorage、TTL 30s ⇒ '
      + '命中上一个身份留下的快照。只在 30 秒窗口内可见，所以人工很难撞见');
    await shot('s10b-token-apikeys-panel.png');
  })();
  if (stopAt === 's10') return finish(pg, rep, S, defects);

  // ---- ⑧ 管理员登录 → 逐个面板 --------------------------------------------
  let sweep = [];
  await (async function phase8() {
    rep.section('⑧ 管理员登录 → 逐个面板（截图 + 非空白屏判据）');
    await pg.clearCookies();
    await pg.nav(S.base + '/ui', '(() => !!document.getElementById("title"))()');
    await pg.setValue('#username', S.admin.user);
    await pg.setValue('#password', S.admin.pass);
    await pg.click('#submit');
    const ok = await pg.until(pg.ev, IS_CONSOLE_JS, (v) => !!v, 20000);
    P(!!ok, '管理员登录成功并进入控制台', '控制台可见=' + ok);
    const who = await pg.ev('document.getElementById("whoami").textContent');
    P(String(who).indexOf('管理员') >= 0, '顶栏显示「管理员」标记（管理员视图与成员视图的差别之一）', 'whoami="' + who + '"');
    const bc = await pg.ensureBox('#btnChpass');
    P(visible(bc), '管理员也有「改密码」', JSON.stringify(bc));
    const bannerHidden = await pg.ev('(() => { const b = document.getElementById("memberBanner"); return !b || getComputedStyle(b).display === "none"; })()');
    P(!!bannerHidden, '★ 管理员视图里**没有**成员横幅（横幅只对成员出现 —— 与⑨的成员视图形成对照）',
      'banner 隐藏=' + bannerHidden);
    const nav = await pg.ev(`(() => {
      const secs = Array.from(document.querySelectorAll('#content > section'));
      return Array.from(document.querySelectorAll('#nav button[data-nav]')).map((b, i) => {
        const s = secs[+b.dataset.nav];
        return { idx: +b.dataset.nav, label: b.textContent.trim(), key: s ? s.dataset.key : '' };
      });
    })()`);
    S.adminNav = nav || [];
    rep.info('管理员导航项: ' + JSON.stringify(S.adminNav.map((n) => n.label + '(' + n.key + ')')));
    P(S.adminNav.length >= 4, '导航渲染出多个面板（manifest 驱动，不是空导航）', '项数=' + S.adminNav.length);

    // 先把管理员自己的一把 key 建出来（供 ⑨ 与成员对比）
    await openPanel(pg, 'apikeys');
    await pg.click('#btnApiKeyNew');
    await pg.setValue('#akName', 'probe-visual-admin-key');
    await pg.click('#akSave');
    // 这里只判"最终会出现"，把延迟当 info 报 —— 同一个缺陷（新建不丢缓存）
    // 已经在 ⑤ 里以可量化的红条报过一次，重复红只会淹没真正的新信息。
    // ⚠ 管理员侧有 30s 无感轮询（refresh() 没抛异常 ⇒ setInterval 注册成功），
    //   所以这一条**会**自己出现；成员侧不会（见 ⑤ 那条对照）。
    const nA = await waitKeys(pg, (n) => n >= 2, 45000);
    P(nA.ok, '管理员在「API 管理」里建了 1 把 → 列表里 ≥2 行（含成员那把）',
      'DOM 行数=' + nA.keys + '（' + nA.ms + 'ms 后；延迟同 ⑤ 那条：新建后的刷新命中 30s TTL 旧快照）');
    S.adminKeys = await pg.ev('Array.from(new Set(Array.from(document.querySelectorAll("#apiKeyRows [data-apikey]")).map(e => e.getAttribute("data-apikey"))))');
    await shot('s08-apikeys-all.png');

    // 逐个面板：点导航、等渲染、判非空白屏、截图
    const REQUIRED = ['apikeys', 'accounts', 'logs', 'settings', 'models', 'chat'];
    const seen = {};
    for (const item of S.adminNav) {
      if (!item.key) continue;
      await openPanelByIndex(pg, item.idx);
      const p = await waitPanelContent(pg, item.key);
      seen[item.key] = p;
      const blank = panelNonBlank(p);
      // 判红时把"为什么"写清楚：三种可能的红长得完全不同（失败文案 / 一直加载中 /
      // 渲染出来了但什么都没有），三者的修法也完全不同。
      const why = blank ? '' : (p.failText ? '面板里是**失败文案**'
        : (p.loadingText ? '面板**一直停在加载中**（20s 未画完）' : '面板既没有结构化内容也没有空态文案 ⇒ 用户看到的是白屏'));
      P(blank, `面板「${item.label}」(${item.key}) 渲染出非空内容（行/卡片/表单 > 0 或明确的空态文案，且无失败/加载中文案）`
        + '　—— 判据 = ' + VDESC + ' + 结构化内容 > 0', blank ? JSON.stringify(p) : why + '　' + JSON.stringify(p));
      await shot('s08-panel-' + String(item.key).replace(/[:/]/g, '_') + '.png');
    }
    sweep = Object.keys(seen);
    S.panels = seen;
    // 任务书点名的面板必须在场（不在场要说清是"真的没有"还是"没渲染出来"）
    for (const key of REQUIRED) {
      if (seen[key]) continue;
      rep.skip(`点名要验的面板 ${key} 不在本次导航里（该实例/该 manifest 没有它 —— 例如「任务历史」本轮已被删除），`
        + '不是"渲染失败"；已渲染的面板见上面逐条');
    }
    S.sweep = sweep;
  })();
  if (stopAt === 's08') return finish(pg, rep, S, defects);

  // ---- ⑨b 管理员看全部 vs 成员只看自己 -------------------------------------
  await (async function phase9b() {
    rep.section('⑨b 管理员看见**全部** key，成员只看**自己**的（同一实例、两账号对比）');
    const adm = S.adminKeys || [];
    const mem = S.memberKeys || [];
    P(adm.length >= 2 && mem.length === 1,
      '★ 同一个实例里：管理员列表 ' + adm.length + ' 行，成员列表 ' + mem.length + ' 行',
      '管理员=' + JSON.stringify(adm.map((x) => lib.short(x, 12))) + ' 成员=' + JSON.stringify(mem.map((x) => lib.short(x, 12))));
    const memberKeyStillThere = mem.every((id) => adm.indexOf(id) >= 0);
    P(memberKeyStillThere && adm.length > mem.length,
      '★★ 管理员那把列表里**包含**成员那把 key（成员看不到管理员的，管理员看得到成员的）',
      '管理员集合=' + JSON.stringify(adm.map((x) => lib.short(x, 12))));
    const crossVisible = mem.some((id) => adm.indexOf(id) < 0);
    P(!crossVisible, '成员列表里的每一把都在管理员列表里（没有"成员能看到管理员看不到的"这种怪事）',
      JSON.stringify(mem.map((x) => lib.short(x, 12))));
    rep.info('⑨ 结论：管理员 ' + adm.length + ' 行 / 成员 ' + mem.length + ' 行，'
      + '成员的 key 对管理员可见 ⇒ 归属过滤在**界面**上也是对的（不只看 HTTP 回执）');
    rep.info('对照（同实例两个角色的界面差异）：成员=成员横幅可见+账号池面板隐藏+只看自己的 key；'
      + '管理员=无横幅+账号池/日志/设置/模型/对话面板都在+看全部 key');
  })();

  // ---- ⑪ 人机校验开着：题目真的画在页面上 -----------------------------------
  await (async function phase11() {
    rep.section('⑪ 另起一个「人机校验开着」的实例：题面与题目图片真的渲染出来');
    // ⚠ 人机校验在 lib_instance 的默认配置里是**关**的（`console.disable_captcha: true`），
    //   要验"开着的时候页面长什么样"必须显式覆写成 false —— 而且必须**另起实例**：
    //   配置是启动时读的，同一个进程里改不了。这也正是"只跑一遍主实例就能验完"的
    //   想法的破口：那个默认值会让 ⑪ 永远看不见题面，还看起来像功能坏了。
    const port2 = await lib.pickPort([7900, 7902, 7904, 7896]);
    const inst2 = await instLib.startInstance({
      port: port2, apiKey: 'probe-visual-cap-' + port2, auths: {},
      config: { console: { disable_captcha: false } },
    });
    rep.info('第二个实例（人机校验**开**）: ' + inst2.base + '（pid=' + inst2.pid + '）');
    try {
      const cdp2 = await freeCdpPort([9250]);
      await withBrowser({ port: cdp2, url: inst2.base + '/ui', outDir: shotDir, width: 1600, height: 1000 }, async (pg2) => {
        await pg2.clearCookies();
        await pg2.nav(inst2.base + '/ui', '(() => !!document.getElementById("title"))()');
        // 首次探测是**异步**的（页面 load 完才打 /admin/auth/captcha）：
        // 这里等的是"那一行从 hidden 变成有高度"，不是固定 sleep。
        const shown = await pg2.until(pg2.ev,
          '(() => { const r = document.getElementById("captchaRow"); if (!r || r.hidden) return 0;'
          + ' const b = r.getBoundingClientRect(); return b.height > 0 ? 1 : 0; })()',
          (v) => v === 1, 20000);
        P(shown === 1, '★ 开人机校验时 `#captchaRow` 由 hidden 变为可见（探测拿到 200 才显示）', 'visible=' + shown);
        const row = await pg2.ensureBox('#captchaRow');
        P(visible(row), '题面那一行可见且尺寸正常（' + VDESC + '）', JSON.stringify(row));
        const svg = await pg2.ev(`(() => {
          const host = document.getElementById('captchaSvg');
          if (!host) return { found: false, why: '没有 #captchaSvg' };
          const s = host.querySelector('svg');
          if (!s) return { found: false, why: 'span 里没有 svg 元素', html: host.innerHTML.slice(0, 60) };
          const r = s.getBoundingClientRect();
          return { found: true, w: Math.round(r.width), h: Math.round(r.height),
                   display: getComputedStyle(s).display,
                   shapes: s.querySelectorAll('path, rect, text, line, polyline, polygon, circle').length };
        })()`);
        P(!!svg.found && svg.w > 0 && svg.h > 0,
          '★★ 题目图片真的画出来了（`#captchaSvg` 里有 svg 元素，宽高都 > 0 —— 不是一块空占位）',
          JSON.stringify(svg));
        P(!!svg.found && svg.shapes > 0, '题目 SVG 里有绘图图元（不是空 `<svg></svg>`）', '图元数=' + (svg && svg.shapes));
        const inp = await pg2.ensureBox('#captcha');
        P(visible(inp), '答案输入框可见（题面旁边有地方填答案）', JSON.stringify(inp));
        const rf = await pg2.ensureBox('#captchaRefresh');
        P(visible(rf) && String(rf.text).indexOf('换一题') >= 0, '「换一题」按钮可见（题目可换）', JSON.stringify(rf));
        const hint = await pg2.ensureBox('#captchaHint');
        P(visible(hint), '题面下的填写提示可见（告诉用户只填数字）', JSON.stringify(hint));
        await shot('s11-captcha-on.png');
      });
    } finally {
      try { inst2.stop(); } catch { }
      try { fs.rmSync(inst2.tmp, { recursive: true, force: true }); } catch { }
    }
    rep.info('已停止并清理第二个实例: ' + inst2.tmp);
  })();

  return finish(pg, rep, S, defects);
}

// finish：全程三条硬判据 + 汇总（放在最后，注入类缺陷一定落在窗口内）。
async function finish(pg, rep, S, defects) {
  rep.section('全程错误通道（三条硬判据）');
  const canceled = pg.loadFailed.filter((f) => f.canceled || f.errorText === 'net::ERR_ABORTED');
  const realFail = pg.loadFailed.filter((f) => !(f.canceled || f.errorText === 'net::ERR_ABORTED'));
  rep.ok(pg.consoleErrs.length === 0, 'console.error 数组为空（前端没有报错）',
    pg.consoleErrs.length ? pg.consoleErrs.map((x) => x.text).join(' | ') : '');
  rep.ok(pg.exceptions.length === 0, '未捕获异常数组为空（Runtime.exceptionThrown）',
    pg.exceptions.length ? pg.exceptions.map((x) => x.text).join(' | ') : '');
  rep.ok(realFail.length === 0, 'Network.loadingFailed 数组为空（已排除 ' + canceled.length
    + ' 条 canceled/ERR_ABORTED：切页会正常取消在途请求，那是导航产物不是产品故障）',
    realFail.length ? JSON.stringify(realFail.slice(0, 3)) : '');
  // 把每条异常归到"当时正在跑哪一段"（靠 section 时间戳），报告才好据以复现。
  const where = (t) => {
    let cur = '(页面加载期)';
    for (const s of S.sections || []) { if (s.t <= t) cur = s.title; else break; }
    return cur;
  };
  for (const x of pg.exceptions) rep.info('异常归属: ' + where(x.t) + ' ⇒ ' + x.text);
  for (const x of pg.consoleErrs) rep.info('console.error 归属: ' + where(x.t) + ' ⇒ ' + x.text);
  if (canceled.length) {
    rep.info('（已排除的 ' + canceled.length + ' 条 canceled：' + JSON.stringify(canceled.slice(0, 3).map((c) => c.errorText)) + '）');
  }
  // HTTP 4xx/5xx 清单（不判红，但必须列出来：本实例里"按设计被拒"与"真坏了"混在一起，
  // 只有把状态码 + URL 摆出来，别人才分得清哪条需要修）。
  if (pg.httpErrors.length) {
    const agg = {};
    for (const h of pg.httpErrors) {
      const k = h.status + ' ' + h.url.replace(/^https?:\/\/127\.0\.0\.1:\d+/, '');
      agg[k] = (agg[k] || 0) + 1;
    }
    const rows = Object.entries(agg).sort((a, b) => b[1] - a[1]).slice(0, 8);
    rep.info('HTTP 4xx/5xx 汇总（' + pg.httpErrors.length + ' 条，最多 8 组）：' + JSON.stringify(rows));
    rep.info('  ↑ 本实例的构成：403 /status 与 /admin/stats(series)=成员按设计被拒（T8 收窄）；'
      + '404 /admin/auth/captcha=本实例关着人机校验（探测按设计回 404）；'
      + '401 /v1/models=控制台会话不带 api_key（模型目录空白的根因）；'
      + '503 /healthz=无上游账号时的就绪语义');
  }
  if (pg.logs.length) {
    const errs = pg.logs.filter((l) => l.level === 'error');
    // 按 URL 归类：404 到底是资源还是接口（只报数，不判红 —— 网络层的 404/503
    // 不进三条硬判据，但"哪个 URL"是排障要的第一条信息，不收集就只能靠猜）。
    const byUrl = {};
    for (const l of errs) { const k = l.url || '(无 url)'; byUrl[k] = (byUrl[k] || 0) + 1; }
    const top = Object.entries(byUrl).sort((a, b) => b[1] - a[1]).slice(0, 6);
    rep.info('（Log 域补充信息：' + pg.logs.length + ' 条，其中 error ' + errs.length + ' 条，按 URL：'
      + JSON.stringify(top) + ' —— 本实例无上游账号，/healthz 按设计就回 503；'
      + '这些**不计入**上面三条硬判据）');
  }
  rep.info('截图共 ' + S.shots.length + ' 张（含逐面板），目录见 ⓪ 段');
  return S;
}

// injectIf 按缺陷名注入（自检用）。缺一个都不许静默跳过 —— 注入失败等于自检失去意义。
async function injectIf(pg, defects, name, rep) {
  if (!defects[name]) return;
  const r = await pg.ev(INJECT[name]);
  rep.info('已注入缺陷 ' + name + '（evaluate 返回 ' + JSON.stringify(r) + '）');
}

// openPanel 用应用自己的 showPanelByKey 切面板（导航项索引不可用时用）。
async function openPanel(pg, key) {
  await pg.ev(`window.__wb2api__ && window.__wb2api__.showPanelByKey(${JSON.stringify(key)})`);
  return waitPanelContent(pg, key);
}

// openPanelByIndex 点真实的**导航按钮**（比直接调 showPanelByKey 更接近用户操作）。
async function openPanelByIndex(pg, idx) {
  await pg.click('#nav button[data-nav="' + idx + '"]');
}

// waitKeys 反复**重开面板**直到列表里的 key 数满足 ok(n)（封顶 ms）。
//
// 为什么要"反复重开"而不是"等一次"：`loadAPIKeys()` 走 sharedGet 的 30s TTL 缓存，
// 缓存没到期时**再多次重开都拿到同一份旧快照**；到期之后第一次重开才会真拉。
// 所以这里用"每 2.5s 重开一次"的方式覆盖整个 TTL 窗口 —— 一旦过期立刻成功，
// 既不用拍一个固定 31s 的 sleep，也不会因为拍短了而假红。
// 返回值里带 ms，报告可以写"实测 T+xx s 才出现"。
async function waitKeys(pg, ok, ms) {
  const t0 = Date.now();
  for (;;) {
    await openPanel(pg, 'apikeys');
    const v = await pg.ev(`(() => { const p = (${PANEL_JS})('apikeys'); return p.keys; })()`);
    if (ok(v)) return { ok: true, ms: Date.now() - t0, keys: v };
    if (Date.now() - t0 > ms) return { ok: false, ms: Date.now() - t0, keys: v };
    await pg.sleep(2500);
  }
}

// waitPanelContent 轮询到面板"画完"为止（不是固定 sleep —— 冷启动耗时不可预测）。
//
// ⚠ 判据是"画完了"，不是"非空白"：等待条件里必须有 loadingText 这一支，
//   否则会把「加载中…」当成失败立刻收工（见 PANEL_JS 里那段注释）。
//   超时（20s 还停在加载中）时返回最后一帧 —— 那一刻它就是坏的面板，
//   由调用方的断言带着"卡在加载中"这一事实判红。
async function waitPanelContent(pg, key, ms) {
  const v = await until(pg.ev, `(${PANEL_JS})(${JSON.stringify(key)})`,
    (p) => panelSettled(p), ms || 20000);
  if (v) return v;
  const last = await pg.ev(`(${PANEL_JS})(${JSON.stringify(key)})`);
  if (last) last.timedOut = true;
  return last;
}

// ---------------------------------------------------------------------------
// 自检用例
// ---------------------------------------------------------------------------

// # 自检用例表：为什么不是"好桩必须全绿"
//
// 本构建上探针**本来就是红的**（成员登录时的未捕获 TypeError、新建 key 后列表
// 靠 30s TTL 才刷新、模型目录面板是一段原始 HTTP 401 JSON —— 见本轮报告）。
// 如果自检要求"好桩全绿"，那三条既有红会让自检永远失败，判别力反而无法证明。
// 所以自检改成**基线 + 增量**：
//
//	① 好桩 = 基线（无注入）：记录本构建既有的红条集合 R0；
//	② 每个坏桩必须让**指定的那条判据**变红，且这条红**不在 R0 里**
//	   —— 即"这条红是这次注入造成的"，而不是既有问题顺带亮起。
//
// 这样既证明判据有判别力（注入什么就红什么），又如实保留了既有红条。
// redLines 取出本次跑出来的**完整红条**（断言文字 + "—— 实际: 证据"）。
//
// ⚠⚠ 为什么不用 `rep.fails`：它只 push **断言文字**，而"实际"那一截（证据）只在
//    打印行里。于是同一条断言在两次跑里字符串**完全相同** —— 坏桩 ③ 第一次就
//    栽在这里：注入的异常确实进了页面（异常归属行写得清清楚楚），可比较用的是
//    `rep.fails`，"未捕获异常数组为空"两边一模一样 ⇒ 被判成"没有新增红"。
//    判据必须比较**带证据的整行**：同一断言、证据不同 ⇒ 是两件事。
function redLines(rep) {
  return rep.lines.filter((l) => l.indexOf('FAIL ') === 0 || /^\s+FAIL /.test(l)).map((l) => l.trim());
}

const SELFTEST_CASES = [
  {
    desc: '好桩（基线）：真实页面按契约渲染，无注入 ⇒ 记录本构建既有红条',
    expect: 'baseline', defects: {}, stopAt: '',
  },
  {
    desc: '坏桩 ①：把 #memberBanner 用 CSS 藏掉（display:none）⇒ "横幅可见/不遮挡"判据必须红',
    expect: 'fail', defects: { banner_hidden: true }, stopAt: 's04', mustRed: '#memberBanner',
  },
  {
    desc: '坏桩 ②：让读 key 列表的接口回 500（真页面渲染出「读取失败」）⇒ "非空白屏"判据必须红',
    expect: 'fail', defects: { panel_api_500: true }, stopAt: 's05', mustRed: '非空白屏',
  },
  {
    desc: '坏桩 ③：页面里抛一次未捕获异常 ⇒ "console/异常为空"判据必须红',
    expect: 'fail', defects: { page_throw: true }, stopAt: 's02', mustRed: '未捕获异常',
  },
];

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

function parseArgs(argv) {
  const o = { selfTest: false, shotDir: DEFAULT_SHOT_DIR, only: 0 };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === '--self-test') o.selfTest = true;
    else if (a === '--shot-dir') o.shotDir = argv[++i];
    else if (a.indexOf('--shot-dir=') === 0) o.shotDir = a.slice('--shot-dir='.length);
    // --only=N：调试自检用（N 从 1 数）。**总是连基线一起跑** —— 否则没有 R0
    // 可比，坏桩会"因为基线是空的"而假通过。
    else if (a.indexOf('--only=') === 0) o.only = Number(a.slice('--only='.length)) || 0;
  }
  return o;
}

async function main() {
  const o = parseArgs(process.argv.slice(2));
  fs.mkdirSync(o.shotDir, { recursive: true });
  if (!o.selfTest) {
    console.log('=== 登录/账号/控制台 视觉验收（E2E + 截图） ===');
    const rep = lib.makeReporter();
    await runProbe({ rep, shotDir: o.shotDir, seed: 0 });
    return conclude([{ rep, expect: 'pass', desc: '真实例' }], false);
  }
  console.log('=== 自检：往真实页面注入缺陷，验证判据"注入什么就红什么" ===');
  const results = [];
  let baseline = [];             // 本构建既有红条（好桩那一次跑出来的）
  for (let i = 0; i < SELFTEST_CASES.length; i += 1) {
    const c = SELFTEST_CASES[i];
    if (o.only && i !== 0 && i !== o.only - 1) continue;
    // ⚠ 每个用例写**自己的子目录**：自检的坏桩截图（例如被 CSS 藏掉横幅的那张）
    //   与真实例的截图同名，混在一个目录里会让人把"注入后的样子"当成产品现状。
    const dir = path.join(o.shotDir, 'selftest-' + (i + 1) + (c.expect === 'baseline' ? '-baseline' : '-bad'));
    fs.mkdirSync(dir, { recursive: true });
    console.log('\n---- 自检 ' + (i + 1) + '/' + SELFTEST_CASES.length + '：' + c.desc + ' ----');
    console.log('     截图: ' + dir);
    const rep = lib.makeReporter({ quiet: true });
    try {
      await runProbe({ rep, shotDir: dir, defects: c.defects, stopAt: c.stopAt, seed: i + 1 });
    } catch (e) {
      rep.ok(false, '自检用例跑完（异常）', String((e && e.message) || e).slice(0, 200));
    }
    const reds = redLines(rep);
    let good = true;
    let verdict = '';
    if (c.expect === 'baseline') {
      baseline = reds.slice();
      good = true;
      verdict = reds.length === 0 ? '探针全绿（本构建无既有红条）'
        : '探针红 ' + reds.length + ' 条（记为基线 R0，属本构建既有问题）';
      if (reds.length) console.log('    基线红条（不属自检失败）: ' + reds.map((m) => lib.short(m, 80)).join(' ／ '));
    } else {
      // 判据：这条红必须**同时**满足"含目标关键字"且"基线里没有逐字相同的这条"。
      // 后半句是关键 —— 否则"注入根本没生效、红的是既有问题"会被当成判别力成立。
      const newReds = reds.filter((m) => baseline.indexOf(m) < 0);
      const hit = newReds.filter((m) => m.indexOf(c.mustRed) >= 0);
      const baseKeys = baseline.filter((m) => m.indexOf(c.mustRed) >= 0).length;
      good = hit.length >= 1;
      verdict = hit.length >= 1
        ? '目标判据「' + c.mustRed + '」新增红 ' + hit.length + ' 条（基线里同判据 ' + baseKeys + ' 条，'
          + '本次总红 ' + reds.length + ' 条 / 其中新增 ' + newReds.length + ' 条）'
        : (newReds.length ? '新增红 ' + newReds.length + ' 条，但**没有一条**属于「' + c.mustRed + '」（判别力对不上）'
          : '一条都没新增（**没有判别力**：注入没生效，或该判据本来就不会因此变红）');
      if (hit.length) console.log('    新红（注入造成）: ' + hit.map((m) => lib.short(m, 100)).join(' ／ '));
      else {
        console.log('    本次红条: ' + reds.map((m) => lib.short(m, 140)).join(' ／ '));
        // 判红时要能一眼看出"注入到底有没有生效"：把注入记录与异常归属打出来。
        for (const l of rep.lines.filter((x) => x.indexOf('已注入') >= 0 || x.indexOf('异常归属') >= 0).slice(0, 6)) {
          console.log('    ' + l.trim());
        }
      }
    }
    console.log((good ? 'PASS ' : 'FAIL ') + c.desc + '　［期望' + (c.expect === 'baseline' ? '记录基线' : '指定判据变红')
      + '，实际 ' + verdict + '］');
    results.push({ rep, expect: c.expect, desc: c.desc, good });
  }
  const allGood = results.every((r) => r.good);
  console.log('\n自检结论: ' + (allGood
    ? '符合预期：好桩给出基线（' + baseline.length + ' 条既有红条），三个坏桩各自让目标判据变红 ⇒ 探针有判别力'
    : '有不符合预期的用例（见上）'));
  const code = conclude(results, true);
  return allGood ? 0 : (code || 1);
}

function conclude(results, isSelfTest) {
  const totalPass = results.reduce((s, r) => s + (r.rep ? r.rep.passed : 0), 0);
  const totalFail = results.reduce((s, r) => s + (r.rep ? r.rep.fails.length : 0), 0);
  const totalSkip = results.reduce((s, r) => s + (r.rep ? r.rep.skipped : 0), 0);
  const stuck = results.filter((r) => (r.expect === 'pass') && r.rep && r.rep.fails.length);
  if (!isSelfTest) {
    console.log('\n结果: ' + (stuck.length === 0
      ? '全绿'
      : stuck.length + ' 条 FAIL（SKIP ' + totalSkip + ' 项，见上面每条的说明）'));
  }
  console.log('（本次 PASS=' + totalPass + ' FAIL=' + totalFail + ' SKIP=' + totalSkip + '）');
  return (stuck.length === 0) ? 0 : 1;
}

module.exports = { parseArgs, panelNonBlank, panelSettled, visible, intersect, SELFTEST_CASES };

if (require.main === module) {
  main().then((code) => process.exit(code)).catch((e) => {
    console.error('探针异常: ' + ((e && e.stack) || e));
    process.exit(1);
  });
}
