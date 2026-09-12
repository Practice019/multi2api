// flash_test.js —— 抗闪白的**真实**验证：用 CDP 在页面加载的每个生命周期
// 事件上采样 <html data-theme>，确认浅色偏好下从不出现深色帧。
//
// # 为什么 theme_e2e 的那条断言不够
//
// theme_e2e 验证的是"设 data-theme 的脚本位置在 <style> 之前"。那是**必要**
// 条件，不是充分条件 —— 脚本也可能因为异常而没生效，或者被后续代码覆盖。
// 这里直接看**结果**：整个加载过程中 data-theme 有没有出现过 dark。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9241;
const URL_UT = process.env.FLASH_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = require('os').tmpdir() + '/chrome-flash';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`, '--window-size=1440,900', 'about:blank'], { stdio: 'ignore' });

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get(`http://127.0.0.1:${PORT}/json/list`));
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
    const frames = [];
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });

    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      // 每个加载阶段都立刻采一次当时的 data-theme
      if (['Page.frameNavigated', 'Page.domContentEventFired', 'Page.loadEventFired'].indexOf(m.method) >= 0) {
        send('Runtime.evaluate', { expression: "document.documentElement.getAttribute('data-theme')", returnByValue: true })
          .then(r => { if (r.result && r.result.result) frames.push({ ev: m.method, v: r.result.result.value }); })
          .catch(() => { });
      }
    };

    await send('Page.enable');
    await send('Runtime.enable');

    // 先写偏好为浅色，再导航 —— 模拟"浅色用户打开页面"
    await send('Page.navigate', { url: URL_UT });
    await sleep(2500);
    await send('Runtime.evaluate', { expression: "localStorage.setItem('wb2api.theme','light')", returnByValue: true });

    frames.length = 0;
    await send('Page.navigate', { url: URL_UT });
    await sleep(3500);

    console.log('    加载阶段采样: ' + JSON.stringify(frames));
    const dark = frames.filter(f => f.v === 'dark');
    ok(frames.length > 0, '采到 ' + frames.length + ' 个加载阶段样本');
    ok(dark.length === 0, '预设浅色时加载全程未出现 dark 帧' + (dark.length ? ' —— 命中 ' + dark.length + ' 次' : ''));

    const after = await send('Runtime.evaluate', { expression: "document.documentElement.getAttribute('data-theme')", returnByValue: true });
    ok(after.result.result.value === 'light', '最终主题为 light（实际 ' + after.result.result.value + '）');

    // 反向：预设深色时不该出现 light 帧
    await send('Runtime.evaluate', { expression: "localStorage.setItem('wb2api.theme','dark')", returnByValue: true });
    frames.length = 0;
    await send('Page.navigate', { url: URL_UT });
    await sleep(3000);
    const light = frames.filter(f => f.v === 'light');
    console.log('    深色偏好采样: ' + JSON.stringify(frames));
    ok(light.length === 0, '预设深色时加载全程未出现 light 帧' + (light.length ? ' —— 命中 ' + light.length + ' 次' : ''));

    // 还原
    await send('Runtime.evaluate', { expression: "localStorage.setItem('wb2api.theme','system')", returnByValue: true });
    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { ch.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== 抗闪白验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
