// jobs_e2e.js —— T9 验证：任务调度面板。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9230;
const URL_UNDER_TEST = process.env.T9_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-t9profile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const chrome = spawn(CHROME, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`,
    '--window-size=1440,1000', 'about:blank',
  ], { stdio: 'ignore' });

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let target = null;
    for (let i = 0; i < 40; i++) {
      try {
        const list = JSON.parse(await get(`http://127.0.0.1:${PORT}/json/list`));
        target = list.find(t => t.type === 'page' && t.webSocketDebuggerUrl);
        if (target) break;
      } catch { /* 等 */ }
      await sleep(250);
    }
    if (!target) throw new Error('Chrome 未启动');

    const ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let id = 0; const pending = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
    const send = (m, p) => new Promise(r => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method: m, params: p || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: URL_UNDER_TEST });
    await sleep(3500);

    const evalJs = async (expr) => {
      const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 300));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    console.log('\n[T9] 任务调度面板');
    const t = JSON.parse(await evalJs(`(() => {
      const tb = document.getElementById('jobrows');
      if (!tb) return JSON.stringify({ missing: true });
      const table = tb.closest('table');
      const cols = table.querySelectorAll('thead th').length;
      const rows = Array.from(tb.querySelectorAll('tr')).map(r => ({
        cells: Array.from(r.children).map(td => td.textContent.trim()),
        span: Array.from(r.children).reduce((n, td) => n + Number(td.getAttribute('colspan') || 1), 0),
        err: r.classList.contains('errrow'),
      }));
      return JSON.stringify({
        cols,
        meta: (document.getElementById('jobMeta') || {}).textContent || '',
        rows,
      });
    })()`));

    if (t.missing) throw new Error('找不到 #jobrows');
    console.log('    表头列数=' + t.cols + '  子标题=' + t.meta);
    t.rows.forEach(r => console.log('      ' + r.cells.join(' | ') + (r.err ? '  [错误行]' : '')));

    ok(t.cols === 6, '任务表 6 列（实际 ' + t.cols + '）');
    ok(t.rows.length >= 1, '至少渲染出 1 个任务（实际 ' + t.rows.length + '）');
    ok(t.rows.every(r => r.span === t.cols), '每行都恰好占满 ' + t.cols + ' 列');
    ok(/个已注册任务/.test(t.meta), '子标题显示任务数（实际：' + t.meta + '）');

    // 三个已知任务都应出现
    const names = t.rows.map(r => r.cells[0]).join(',');
    for (const n of ['workbuddy-travel-watch', 'workbuddy-growth-watch', 'codearts-refresh']) {
      ok(names.indexOf(n) >= 0, '列出任务 ' + n);
    }
    // 从未执行的必须明确显示「未执行」而不是 —
    const neverRun = t.rows.filter(r => r.cells[3] === '未执行');
    console.log('    未执行的任务数: ' + neverRun.length);
    ok(t.rows.every(r => r.cells[3] !== '—' && r.cells[3] !== ''),
      '「上次执行」列不留空/不留 —（未执行时显示「未执行」）');
    ok(t.rows.every(r => /秒|分|—/.test(r.cells[2])), '「间隔」列是可读时长');

    console.log('\n[T9] 面板在导航里可达');
    const nav = JSON.parse(await evalJs(`JSON.stringify(
      Array.from(document.querySelectorAll('#nav button[data-nav]')).map(b => b.textContent.trim()))`));
    ok(nav.some(x => x.indexOf('任务调度') >= 0), '导航里有「任务调度」入口（实际：' + nav.join(' / ') + '）');

    console.log('\n[T9] 只读：没有手动触发按钮');
    const triggers = await evalJs(`document.querySelectorAll('#content > section[data-key="jobs"] button[data-jobrun], #content > section[data-key="jobs"] button[data-trigger]').length`);
    ok(triggers === 0, '没有任何"手动触发"按钮（重入风险，刻意不做）');

    console.log('\n[健康]');
    const errs = await evalJs('JSON.stringify(window.__errs || [])');
    ok(errs === '[]' || errs === undefined, '无未捕获脚本错误');

    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== T9 端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
