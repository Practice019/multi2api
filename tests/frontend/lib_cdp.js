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
// opts = { port, url, width, height, readyExpr, outDir }
//   readyExpr  轮询表达式，返回真值即认为骨架就绪（默认等 document.body）
async function withChrome(opts, fn) {
  const CHROME = require('./chrome_path.js').resolveChrome();
  const port = opts.port;
  const width = opts.width || 1600;
  const height = opts.height || 1200;
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
    await send('Page.navigate', { url: opts.url });

    // ② 轮询等骨架
    const readyExpr = opts.readyExpr || 'document.body ? 1 : 0';
    let waited = 0;
    for (let i = 0; i < 60; i++) {
      if (await ev(readyExpr)) break;
      await sleep(500); waited += 500;
    }
    console.log('  骨架就绪（等待 ' + waited + 'ms）');

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

module.exports = { withChrome, until, showPanel, sleep, httpGet };
