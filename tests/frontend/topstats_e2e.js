// topstats_e2e.js —— T7 验证：顶栏状态条 + 表格三态 + 信息架构。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
const PORT = 9227;
const URL_UNDER_TEST = process.env.T7_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-t7profile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(url) {
  return new Promise((res, rej) => {
    http.get(url, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
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
      if (r.result && r.result.exceptionDetails) throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 250));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    console.log('\n[1] 顶栏状态条渲染');
    const ts = await evalJs(`(() => {
      const el = document.getElementById('topstats');
      if (!el) return 'MISSING';
      return el.textContent.replace(/\\s+/g,' ').trim();
    })()`);
    console.log('    内容: ' + ts);
    ok(ts !== 'MISSING', '#topstats 存在');
    ok(/账号健康\s*\d+\/\d+/.test(ts), '显示「账号健康 N/M」');
    ok(/账号\s*\d+/.test(ts), '显示账号总数');
    ok(/上游\s*\d+/.test(ts), '显示已注册上游数');
    // 字段名从「刷新」改为「加载」（U6：避免暗示"上次成功刷新"）。
    // 断言要同时接受两种字样 —— 否则改一次文案就假红，而文案本身
    // 是描述性的（真正要守的是"时间串被渲染出来"）。
    ok(/(刷新|加载)\s*\d\d:\d\d:\d\d/.test(ts), '显示时间戳 HH:MM:SS');

    console.log('\n[2] 状态条不额外发请求');
    // 统计 /healthz /status /admin/ui/manifest 的请求次数，等 6 秒再数
    const counts = await evalJs(`(async () => {
      window.__reqs = {};
      const orig = window.fetch;
      window.fetch = function(u, o) {
        const k = String(u).split('?')[0];
        window.__reqs[k] = (window.__reqs[k]||0)+1;
        return orig.apply(this, arguments);
      };
      await new Promise(r => setTimeout(r, 6000));
      window.fetch = orig;
      return JSON.stringify(window.__reqs);
    })()`);
    const c = JSON.parse(counts);
    console.log('    6 秒内的请求: ' + JSON.stringify(c));
    // 5s 轮询一次 → 6 秒内大致 1-2 次 refresh。顶栏若自己发请求会多出独立调用。
    const healthz = c['/healthz'] || 0;
    ok(healthz <= 3, '/healthz 调用次数合理（' + healthz + '，顶栏没有自己发请求）');

    console.log('\n[3] 表格三态助手可用且列数正确');
    const states = await evalJs(`(() => {
      const out = {};
      const A = window.__wb2api__;
      // 从行里反推 span
      const span = (html) => {
        const d = document.createElement('tbody');
        d.innerHTML = html;
        let n = 0;
        Array.from(d.querySelectorAll('td')).forEach(td => n += Number(td.getAttribute('colspan') || 1));
        return n;
      };
      return JSON.stringify({
        loadingAcc: span(A.accountPlaceholder('加载中…')),
        errorAcc: span(A.accountPlaceholder('boom','err')),
      });
    })()`);
    const s = JSON.parse(states);
    console.log('    ' + JSON.stringify(s));
    ok(s.loadingAcc === 11, '账号表加载态占满 11 列（实际 ' + s.loadingAcc + '）');
    ok(s.errorAcc === 11, '账号表错误态占满 11 列（实际 ' + s.errorAcc + '）');

    console.log('\n[4] 每张表的三态都与表头列数一致');
    const tableCheck = await evalJs(`(() => {
      const out = [];
      document.querySelectorAll('#content > section table').forEach(t => {
        const cols = t.querySelectorAll('thead th').length;
        const rows = Array.from(t.querySelectorAll('tbody tr'));
        rows.forEach((r, i) => {
          let sp = 0;
          Array.from(r.children).forEach(td => sp += Number(td.getAttribute('colspan') || 1));
          if (sp !== cols) out.push({ cols, span: sp, text: r.textContent.slice(0, 35) });
        });
      });
      return JSON.stringify(out);
    })()`);
    const bad = JSON.parse(tableCheck);
    if (bad.length) console.log('    异常行: ' + JSON.stringify(bad));
    ok(bad.length === 0, '所有可见表格的行都恰好占满各自列数');

    console.log('\n[5] 无外部资源 / 无 emoji / 无横向溢出');
    const ext = JSON.parse(await evalJs(`JSON.stringify(Array.from(document.querySelectorAll('link,script,img'))
      .map(e => e.getAttribute('href') || e.getAttribute('src') || '')
      .filter(u => /^(https?:)?\\/\\//.test(u)))`));
    ok(ext.length === 0, '没有外部 http(s) 资源（实际 ' + ext.length + '）');
    const overflow = await evalJs(`document.documentElement.scrollWidth - document.documentElement.clientWidth`);
    ok(overflow <= 1, '1440 宽下无横向溢出（溢出 ' + overflow + 'px）');

    console.log('\n[6] 页面健康');
    const errs = await evalJs('JSON.stringify(window.__errs || [])');
    ok(errs === '[]' || errs === undefined, '无未捕获脚本错误');

    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== T7 端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
