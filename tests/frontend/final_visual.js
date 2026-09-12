// final_visual.js —— 最终目视核验：两套主题下取**渲染后的实际颜色**。
//
// # 为什么需要它
//
// theme_e2e.js 只算了 body 的对比度。但 T10 真正容易出错的地方是
// "某个局部还写死着深色专属颜色" —— 那种花脸**不会**体现在 body 上：
// <pre> 仍是 #0a0d13 的深块、侧栏仍是深底、.pill 仍是深色主题的 rgba。
//
// 所以这里逐元素取 computed style 的**实际亮度**，按主题方向断言：
// 浅色主题下这些块必须是浅的，深色主题下必须是深的。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9240;
const URL_UT = process.env.VISUAL_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = require('os').tmpdir() + '/chrome-finalvisual';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const chrome = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
  try {
    let target = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get(`http://127.0.0.1:${PORT}/json/list`));
        target = l.find(t => t.type === 'page' && t.webSocketDebuggerUrl);
        if (target) break;
      } catch { /* 等 */ }
      await sleep(250);
    }
    if (!target) throw new Error('Chrome 未启动');
    const ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (m, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: m, params: p || {} })); });
    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: URL_UT });
    await sleep(3500);
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails).slice(0, 250));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    const lum = (c) => {
      const m = String(c).match(/\d+/g);
      if (!m) return null;
      const v = m.slice(0, 3).map(x => { x = x / 255; return x <= 0.03928 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4); });
      return 0.2126 * v[0] + 0.7152 * v[1] + 0.0722 * v[2];
    };

    for (const theme of ['dark', 'light']) {
      const r = JSON.parse(await ev(`(() => {
        window.__wb2api__.setThemePref(${JSON.stringify(theme)});
        const px = (s, prop) => { const e = document.querySelector(s); return e ? getComputedStyle(e)[prop] : null; };
        return JSON.stringify({
          applied: document.documentElement.getAttribute('data-theme'),
          bodyBg: px('body','backgroundColor'),
          bodyFg: px('body','color'),
          cardBg: px('.card','backgroundColor'),
          preBg: px('pre','backgroundColor'),
          preFg: px('pre','color'),
          navBg: px('.sidebar','backgroundColor'),
          thFg: px('thead th','color'),
          tdFg: px('tbody td','color'),
          grpHdFg: px('.navgrouphd','color'),
        });
      })()`));
      const wantLight = theme === 'light';
      console.log('    [' + theme + '] body=' + r.bodyFg + ' on ' + r.bodyBg + '  card=' + r.cardBg + '  pre=' + r.preBg + '  nav=' + r.navBg);
      ok(r.applied === theme, theme + ' 主题生效');

      // 逐块断言底色亮度方向
      for (const [name, val] of [['body', r.bodyBg], ['卡片', r.cardBg], ['代码块', r.preBg], ['侧栏', r.navBg]]) {
        if (!val) { ok(false, theme + ': ' + name + ' 取不到颜色'); continue; }
        const L = lum(val);
        if (wantLight) ok(L > 0.5, theme + ': ' + name + ' 底色是浅色（亮度 ' + L.toFixed(3) + '）');
        else ok(L < 0.5, theme + ': ' + name + ' 底色是深色（亮度 ' + L.toFixed(3) + '）');
      }
      // 前景必须与背景形成对比（不能同色）
      for (const [name, fg, bg] of [['正文', r.bodyFg, r.bodyBg], ['代码块文字', r.preFg, r.preBg]]) {
        if (!fg || !bg) continue;
        const L1 = lum(fg), L2 = lum(bg);
        const hi = Math.max(L1, L2), lo = Math.min(L1, L2);
        const ratio = (hi + 0.05) / (lo + 0.05);
        ok(ratio >= 4.5, theme + ': ' + name + ' 对比度 ' + ratio.toFixed(2) + ':1 (>=4.5)');
      }
    }
    await ev(`window.__wb2api__.setThemePref('system')`);
    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { chrome.kill(); } catch { /* 已退出 */ } }
  console.log(fail === 0 ? '\n=== 最终目视核验通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
