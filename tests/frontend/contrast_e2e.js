// contrast_e2e.js —— 浅色主题下各组件的对比度必须达到 WCAG AA。
//
// 用独立的 contrast_probe.js（不再内嵌模板字符串，语法可单独校验）。
//
// # 这个测试要抓的缺陷
//
// `.pill.dim` 原先文字用 `--dim`，浅色主题下实测只有 **3.52:1**，
// 低于 AA 要求的 4.5:1。深色主题下够用，所以一直没被发现 ——
// 只有"逐个组件量对比度"才抓得到，看 body 的背景/前景永远看不到。
//
// # 关键：必须切到**含目标元素的面板**再采样
//
// 页面默认停在「仪表盘」，那里没有 .pill.dim。原先直接 querySelector
// 拿到 null，于是"量到的对比度"完全取决于时机 —— 结论不可信。
// 找不到元素现在按**失败**计，不再静默跳过。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');
const probe = require('./contrast_probe.js');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9258;
const URL_UT = process.env.CONTRAST_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = require('os').tmpdir() + '/chrome-contrast';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

// 采样清单：都是页面上真实存在、且用户会读的文本
const PAIRS = [
  ['表格头', 'thead th'],
  ['表格单元格', 'tbody td'],
  ['代码块', 'pre'],
  ['状态胶囊', '.pill'],
  ['状态胶囊(dim)', '.pill.dim'],
];
// 需要切到「账号池」才有的：表格、.pill.dim
const PANEL = '账号池';

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,900', 'about:blank'], { stdio: 'ignore' });

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { /* 等 */ }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');

    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0;
    const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: URL_UT });
    await sleep(3500);

    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 200));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // 注入探针源码 + 切面板
    await ev(probe.IN_PAGE_SOURCE + '; window.__probe = __probeColors;');
    ok(await ev('typeof window.__probe === "function"'), '探针已注入页面');

    for (const theme of ['light', 'dark']) {
      console.log('\n[' + theme + '] 组件对比度（AA 要求 >= 4.5:1）');
      await ev(`window.__wb2api__.setThemePref(${JSON.stringify(theme)})`);
      // 切到含目标元素的面板
      const clicked = await ev(`(() => {
        const b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
          .find(x => x.textContent.indexOf(${JSON.stringify(PANEL)}) >= 0);
        if (b) { b.click(); return true; }
        return false;
      })()`);
      ok(clicked, theme + ': 找到并点击了「' + PANEL + '」入口');
      await sleep(1200);

      const rows = JSON.parse(await ev(`JSON.stringify(window.__probe(${JSON.stringify(PAIRS)}))`));
      for (const r of rows) {
        if (r.missing) { ok(false, theme + ': ' + r.name + ' —— 元素未找到（无法测量，按失败计）'); continue; }
        const a = probe.parseColor(r.fg), b = probe.parseColor(r.bg);
        if (!a || !b) { ok(false, theme + ': ' + r.name + ' 颜色无法解析 ' + r.fg + ' / ' + r.bg); continue; }
        const cr = probe.contrast(a, b);
        ok(cr >= 4.5, theme + ': ' + r.name + ' 对比 ' + cr.toFixed(2) + ':1  ' + r.fg + ' on ' + r.bg +
          (cr >= 4.5 ? '' : '  ← 低于 AA'));
      }
    }

    await ev(`window.__wb2api__.setThemePref('system')`);
    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { /* 已退出 */ } }

  console.log(fail === 0 ? '\n=== 对比度检查通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
