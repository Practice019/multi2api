// shot_jobs.js —— 给「任务调度」页拍图 + 取运行时诊断（2026-10-19）。
//
// # 为什么单开一个
//
// 这页从**6 列表格**换成了**卡片网格**（照参考项目 Agent2API 的 page-tasks.css 移植），
// 而两件事只有肉眼 + 度量合起来才能判对错：
//   · 「2 栏」是不是真的两栏 —— 断言只能查 `grid-template-columns` 的**声明值**，
//     而"实际渲染出来几列"要看 `getBoundingClientRect` 的分组；
//   · 错误文本换行后卡片会不会被撑破、同行两张卡是否等高 —— 这是布局反馈，
//     文字断言看不见。
// 所以既拍图，也把卡片数与几何打出来（与 shot_heatmap.js 同一套理由）。
//
// ⚠ 调试端口用 9335（避开 shot_accounts 的 9329、shot_heatmap 的 9331），
//   三个脚本可以同时跑而不互踢。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9335;
const TARGET = process.env.JOBS_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-jobs-' + Date.now());
const OUT = path.resolve(__dirname, '..', '..', 'shots');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function get(u, h) {
  return new Promise((res, rej) => {
    http.get(u, { headers: h || {} }, (r) => {
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

    // ⚠ 先 enable 再 navigate：加载期异常才收得到（heat_console_probe 的教训）
    await send('Runtime.enable');
    await send('Page.enable');
    await send('Page.navigate', { url: TARGET });

    // 等面板骨架 + manifest 都就绪（轮询，不死等）
    let ready = 0, waited = 0;
    for (let i = 0; i < 60; i++) {
      ready = await ev(`(function(){var h=document.getElementById('jobrows');
        return h ? h.querySelectorAll('.job-item,.job-empty').length : -1;})()`) || -1;
      if (ready >= 0) break;
      await sleep(500); waited += 500;
    }
    console.log('  面板就绪: ' + ready + ' 项（等待 ' + waited + 'ms）');

    // 切到任务调度面板（默认是仪表盘；隐藏面板里量任何尺寸恒为 0 —— 本项目踩过）
    await ev(`window.__wb2api__ && window.__wb2api__.showPanelByKey("jobs")`);
    await sleep(1200);

    async function shot(name) {
      const r = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
      const b = Buffer.from(r.result.data, 'base64');
      fs.writeFileSync(path.join(OUT, name), b);
      console.log('  已保存 ' + path.join(OUT, name) + '（' + b.length + ' 字节）');
    }

    console.log('=== 诊断：任务调度卡片 ===');
    const D = JSON.parse(await ev(`(function(){
      var host = document.getElementById('jobrows');
      if (!host) return JSON.stringify({err:'#jobrows 不存在'});
      var items = host.querySelectorAll('.job-item');
      var empties = host.querySelectorAll('.job-empty');
      var cols = getComputedStyle(host).gridTemplateColumns;
      var errsN = host.querySelectorAll('.job-item.errrow').length;
      // 按"卡片左边缘的 x"分组 ⇒ 实际渲染成几列（断言声明值看不见这个）
      var xs = {};
      for (var i = 0; i < items.length; i++) {
        var x = Math.round(items[i].getBoundingClientRect().left);
        xs[x] = (xs[x] || 0) + 1;
      }
      var first = items[0];
      return JSON.stringify({
        items: items.length, empties: empties.length, errCards: errsN,
        gridCols: cols,
        actualColumns: Object.keys(xs).length,
        perColumn: Object.values(xs),
        firstCard: first ? {
          cls: first.className,
          name: (first.querySelector('.job-name')||{}).textContent,
          hasQ: !!first.querySelector('.job-q'),
          state: (first.querySelector('.job-state')||{}).textContent,
          provider: (first.querySelector('.pill')||{}).textContent,
          height: Math.round(first.getBoundingClientRect().height)
        } : null,
        qTitles: Array.prototype.map.call(host.querySelectorAll('.job-q'), function(e){ return e.getAttribute('title'); }).slice(0,3),
        blankRemoved: !document.querySelector('#jobrows table'),
        bodyOverflowAnchor: getComputedStyle(document.body).overflowAnchor
      });
    })()`));
    if (D.err) { console.log('  ⚠ ' + D.err); exitCode = 2; }
    else {
      console.log('  卡片 ' + D.items + ' 张 · 空态 ' + D.empties + ' 条 · 错误卡 ' + D.errCards + ' 张');
      console.log('  **实际列数 = ' + D.actualColumns + '**（每列张数 ' + JSON.stringify(D.perColumn) + '）· 声明 ' + D.gridCols);
      console.log('  首卡: ' + JSON.stringify(D.firstCard));
      console.log('  问号 title = ' + JSON.stringify(D.qTitles));
      console.log('  **旧表格已移除 = ' + D.blankRemoved + ' · body overflow-anchor = ' + D.bodyOverflowAnchor + '**');
      if (D.tablesRemoved === false) { console.log('  ⚠⚠ 表格还在'); exitCode = 2; }
    }
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) exitCode = 2;

    await shot('jobs-1-wide.png');

    // 窄窗口：验证 1120px 断点回落单栏
    await send('Emulation.setDeviceMetricsOverride', { width: 1000, height: 1200, deviceScaleFactor: 1, mobile: false });
    await sleep(700);
    const narrow = JSON.parse(await ev(`(function(){
      var host = document.getElementById('jobrows');
      var items = host.querySelectorAll('.job-item');
      var xs = {};
      for (var i = 0; i < items.length; i++) { var x = Math.round(items[i].getBoundingClientRect().left); xs[x] = 1; }
      return JSON.stringify({ cols: Object.keys(xs).length, decl: getComputedStyle(host).gridTemplateColumns });
    })()`));
    console.log('=== 窄窗口（1000px）===');
    console.log('  **实际列数 = ' + narrow.cols + '**（应为 1）· 声明 ' + narrow.decl);
    if (narrow.cols !== 1) { console.log('  ⚠⚠ 断点没生效'); exitCode = 2; }
    await shot('jobs-2-narrow.png');
    await send('Emulation.clearDeviceMetricsOverride');

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
