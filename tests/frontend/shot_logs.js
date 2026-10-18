// shot_logs.js —— 给「请求日志」页拍图 + 取运行时诊断（2026-10-19）。
//
// # 为什么单开一个
//
// 这页从 `<table>` 换成了 div + grid（照参考项目 Agent2API 的 page-requests.css 移植）。
// 换形态后有个**必须实测才知道成没成**的点：`position:sticky` 的表头。
// 本项目的 `section` 带 `overflow:hidden`（为圆角），sticky 在它里面**根本不会生效**——
// 所以专门给日志区建了 `.logwrap` 当滚动容器。但"我以为它会生效"和"它真的生效了"
// 是两件事，这里滚动容器再量表头的实际位置来验。
//
// 其余量的是换形态的四条修好没有：数值列右对齐、无值占位淡一档、空态显式画出来、
// 以及列宽是不是真的按 grid 轨道走（`table-layout:auto` 时代长模型名会撑破列）。
//
// ⚠ 调试端口用 9336（shot_accounts 9329 / shot_heatmap 9331 / shot_jobs 9335）。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9336;
const TARGET = process.env.LOGS_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-logs-' + Date.now());
const OUT = path.resolve(__dirname, '..', '..', 'shots');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, (r) => {
      const c = [];
      r.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res(Buffer.concat(c).toString('utf8')));
    }).on('error', rej);
  });
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run',
    '--no-default-browser-check', '--remote-debugging-port=' + PORT,
    '--user-data-dir=' + PROFILE, '--window-size=1600,1200', 'about:blank'],
    { stdio: 'ignore' });

  let ws = null;
  let exitCode = 0;
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');
    ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0;
    const pend = new Map();
    const errs = [];
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails || {};
        errs.push('[异常] ' + (d.exception && d.exception.description ? d.exception.description : d.text));
      } else if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
        errs.push('[console.error] ' + (m.params.args || []).map((a) => a.value || a.description).join(' '));
      }
    };
    const send = (mm, p) => new Promise((r) => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    await send('Runtime.enable');
    await send('Page.enable');
    await send('Page.navigate', { url: TARGET });

    // 等骨架（轮询，不死等）
    let ready = -1, waited = 0;
    for (let i = 0; i < 60; i++) {
      ready = await ev(`document.querySelectorAll('.log-head,.logwrap').length`) || -1;
      if (ready > 0) break;
      await sleep(500); waited += 500;
    }
    console.log('  骨架就绪: ' + ready + ' 个节点（等待 ' + waited + 'ms）');

    // 切到「请求日志」面板（隐藏面板里量尺寸恒为 0 —— 本项目踩过）
    await ev(`window.__wb2api__ && window.__wb2api__.showPanelByKey("logs")`);
    // 日志走 /admin/logs/history 异步取数 —— 等出行来
    let rows = 0;
    for (let i = 0; i < 40; i++) {
      rows = await ev(`document.querySelectorAll('#logrows .log-row').length`) || 0;
      if (rows > 0) break;
      await sleep(500);
    }
    console.log('  数据行: ' + rows + ' 行');
    await sleep(400);

    const D = JSON.parse(await ev(`(function(){
      var wrap = document.querySelector('.logwrap');
      var head = document.querySelector('.log-head');
      var items = document.querySelectorAll('#logrows .log-row');
      if (!wrap || !head) return JSON.stringify({err:'.logwrap / .log-head 不存在'});
      var hs = head.children;
      var first = items[0];
      var cells = first ? Array.prototype.map.call(first.children, function(c){
        var r = c.getBoundingClientRect();
        return { t: Math.round(r.left), w: Math.round(r.width),
                 align: getComputedStyle(c).textAlign };
      }) : null;
      return JSON.stringify({
        headCells: hs.length,
        rows: items.length,
        failedRows: document.querySelectorAll('#logrows .log-row.failed').length,
        gridCols: getComputedStyle(first || head).gridTemplateColumns,
        headGridCols: getComputedStyle(head).gridTemplateColumns,
        cellGeom: cells,
        右对齐格子数: cells ? cells.filter(function(c){ return c.align === 'right'; }).length : -1,
        noValueMarks: document.querySelectorAll('#logrows .log-none').length,
        oldTableGone: !document.querySelector('#logrows tr, .logwrap table'),
        wrapOverflow: getComputedStyle(wrap).overflow,
        wrapMaxH: getComputedStyle(wrap).maxHeight,
        emptyMarkup: !!document.querySelector('#logrows .log-empty')
      });
    })()`));
    if (D.err) { console.log('  ⚠ ' + D.err); exitCode = 2; }
    else {
      console.log('  表头格 ' + D.headCells + ' · 数据行 ' + D.rows + ' · 失败行 ' + D.failedRows);
      console.log('  **表头与行轨道一致 = ' + (D.gridCols === D.headGridCols) + '**（不一致就会错位）');
      console.log('  轨道 = ' + D.gridCols);
      console.log('  **数值列右对齐 = ' + D.右对齐格子数 + '/4**（TTFB/tok/tok\u002Fs/耗时）');
      console.log('  无值占位 .log-none = ' + D.noValueMarks + ' 处 · 旧表格已移除 = ' + D.oldTableGone);
      console.log('  滚动容器 overflow=' + D.wrapOverflow + ' max-height=' + D.wrapMaxH);
      if (D.gridCols !== D.headGridCols) { console.log('  ⚠⚠ 表头与数据轨道不一致'); exitCode = 2; }
      if (!D.oldTableGone) { console.log('  ⚠⚠ 旧表格还在'); exitCode = 2; }
      if (D.headCells !== 10) { console.log('  ⚠⚠ 表头格不是 10 个'); exitCode = 2; }
    }

    console.log('=== **sticky 表头实测**（本项目 section 带 overflow:hidden，须单独验）===');
    // ⚠ 先把窗口压矮再测。
    //   容器高是 `max-height: calc(100vh - 260px)` —— 1200px 高的窗口下它有 844px，
    //   而 15 行日志才 400 来像素，**根本滚不动**，滚动量恒为 0 ⇒ sticky 测了个寂寞
    //   （第一次跑就栽在这：容器里 `scrollTop = 400` 被夹回 0，
    //    两张截图字节数一模一样才发现）。压到 460px 高 ⇒ 容器剩 200px ⇒ 必然溢出。
    await send('Emulation.setDeviceMetricsOverride', { width: 1600, height: 460, deviceScaleFactor: 1, mobile: false });
    await sleep(600);
    const S = JSON.parse(await ev(`(function(){
      var wrap = document.querySelector('.logwrap');
      var head = document.querySelector('.log-head');
      wrap.scrollTop = 400;
      return JSON.stringify({ wrapH: Math.round(wrap.getBoundingClientRect().height),
        scrollable: wrap.scrollHeight - wrap.clientHeight,
        headTopBefore: Math.round(head.getBoundingClientRect().top) });
    })()`));
    await sleep(500);
    const S2 = JSON.parse(await ev(`(function(){
      var wrap = document.querySelector('.logwrap');
      var head = document.querySelector('.log-head');
      var wt = wrap.getBoundingClientRect().top, ht = head.getBoundingClientRect().top;
      return JSON.stringify({ wrapTop: Math.round(wt), headTop: Math.round(ht),
        delta: Math.round(ht - wt), scrolled: wrap.scrollTop });
    })()`));
    console.log('  容器高 ' + S.wrapH + 'px · 可滚量 ' + S.scrollable + 'px');
    console.log('  滚动 ' + S2.scrolled + 'px 后：容器顶 ' + S2.wrapTop + ' · 表头顶 ' + S2.headTop + ' · 差 ' + S2.delta + 'px');
    if (S2.scrolled < 100) {
      console.log('  ⚠⚠ **还是没滚起来**（可滚量只有 ' + S.scrollable + 'px）—— sticky 仍未实测');
      exitCode = 2;
    } else if (Math.abs(S2.delta) <= 2) {
      console.log('  ✓ **sticky 生效**：表头钉在容器顶（滚动 ' + S2.scrolled + 'px 而表头没跟着走）');
    } else {
      console.log('  ⚠⚠ **sticky 没生效**（差 ' + S2.delta + 'px）—— 查祖先的 overflow');
      exitCode = 2;
    }
    await send('Emulation.clearDeviceMetricsOverride');
    await sleep(400);

    async function shot(name) {
      const r = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
      const b = Buffer.from(r.result.data, 'base64');
      fs.writeFileSync(path.join(OUT, name), b);
      console.log('  已保存 ' + path.join(OUT, name) + '（' + b.length + ' 字节）');
    }
    await shot('logs-1-wide.png');
    // 滚动后那张：同样先把窗口压矮，否则滚不动、这张图和上一张一模一样
    await send('Emulation.setDeviceMetricsOverride', { width: 1600, height: 700, deviceScaleFactor: 1, mobile: false });
    await sleep(400);
    await ev(`document.querySelector('.logwrap').scrollTop = 400`);
    await sleep(400);
    await shot('logs-2-scrolled.png');
    await send('Emulation.clearDeviceMetricsOverride');

    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) exitCode = 2;
    console.log('=== 结论：' + (exitCode === 0 ? '通过' : '有问题（见上面 ⚠⚠）') + ' ===');
  } catch (e) {
    console.error('失败: ' + (e && e.message ? e.message : e));
    exitCode = 1;
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { ch.kill(); } catch { }
    await sleep(300);
  }
  process.exit(exitCode);
})();
