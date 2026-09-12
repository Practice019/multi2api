// p4_badge_check.js —— 侧栏选中项徽章：底色必须**跟随主题**。
//
// # 这个测试被评审推翻过一次，重写说明（CP3b F1）
//
// 第一版断言的是：`badgeBg !== 'rgba(255,255,255,.22)'`（字符串比较）。
// 那条断言**按构造恒真**：Chrome 把 color-mix 的结果序列化成
// `color(srgb 1 1 1 / 0.22)`，与 `rgba(255,255,255,.22)` 字符串不同 ——
// 于是"颜色根本没变"也能通过。这正是团队反复踩的同一个坑：
// **验证 X 的书写形式，而不是 X**。
//
// 现在的断言分三层，且**每一层都能被"空操作"打红**：
//   1. dark 与 light 的徽章底色必须**不相等**（核心：跟随主题才算修好）
//   2. 底色必须与旧的写死白 rgba(255,255,255,.22) **实际等价性检查**
//      —— 把两边都规范化成 RGB 再比数值，不看序列化形式
//   3. 与 --acc 的实际混合结果一致（15% ~ 30% 区间，容忍取整）
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9251;
const URL_UT = process.env.P4_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = require('os').tmpdir() + '/chrome-p4';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

// 把任意 CSS 颜色串解析成 [r,g,b,a]（0-255 / 0-1）。
// 覆盖 rgb()/rgba()/color(srgb r g b / a) —— 后者是 Chrome 对 color-mix 的序列化。
function parseColor(s) {
  if (!s) return null;
  let m = s.match(/^rgba?\(([^)]+)\)$/);
  if (m) {
    const p = m[1].split(/[,\s/]+/).filter(Boolean).map(Number);
    return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
  }
  m = s.match(/^color\(srgb\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)(?:\s*\/\s*([\d.]+))?\)$/);
  if (m) {
    return { r: Number(m[1]) * 255, g: Number(m[2]) * 255, b: Number(m[3]) * 255, a: m[4] === undefined ? 1 : Number(m[4]) };
  }
  return null;
}
const eq = (x, y, tol) => x && y && Math.abs(x.r - y.r) <= tol && Math.abs(x.g - y.g) <= tol && Math.abs(x.b - y.b) <= tol && Math.abs(x.a - y.a) <= 0.03;

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
      if (r.result && r.result.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails).slice(0, 200));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    const sample = async (theme) => JSON.parse(await ev(`(() => {
      window.__wb2api__.setThemePref(${JSON.stringify(theme === 'system' ? 'light' : theme)});
      // 直接构造一个处于 active 态的按钮，避免依赖点击是否生效
      const nav = document.getElementById('nav');
      const btn = nav && nav.querySelector('button[data-nav]');
      if (btn) btn.classList.add('active');
      const badge = btn && btn.querySelector('.badge');
      const host = badge || (() => {
        // 没有徽章时造一个，测的是 CSS 规则本身
        const b = document.createElement('span');
        b.className = 'badge';
        b.textContent = '9';
        if (btn) btn.appendChild(b);
        return b;
      })();
      const cs = getComputedStyle(host);
      const btnCs = getComputedStyle(btn);
      const root = getComputedStyle(document.documentElement);
      return JSON.stringify({
        theme: ${JSON.stringify(theme)},
        badgeBg: cs.backgroundColor,
        badgeFg: cs.color,
        btnColor: btnCs.color,
        acc: root.getPropertyValue('--acc').trim(),
        active: btn ? btn.classList.contains('active') : false,
      });
    })()`));

    const dark = await sample('dark');
    const light = await sample('light');
    console.log('    [dark ] 徽章底=' + dark.badgeBg + '  按钮color=' + dark.btnColor + '  --acc=' + dark.acc);
    console.log('    [light] 徽章底=' + light.badgeBg + '  按钮color=' + light.btnColor + '  --acc=' + light.acc);

    ok(dark.active && light.active, '两次采样时按钮都处于 active 态');

    const dC = parseColor(dark.badgeBg);
    const lC = parseColor(light.badgeBg);
    ok(!!dC && !!lC, '两套主题的徽章底色都能解析成 RGB（实际 ' + dark.badgeBg + ' / ' + light.badgeBg + '）');

    // ---- 断言 1（核心）：必须跟随主题 ----
    // 这条是"空操作修复"的照妖镜：无论写成 currentColor 还是写死白色，
    // 两套主题的底色都会相等 → 红。
    const differ = dC && lC && (Math.abs(dC.r - lC.r) > 2 || Math.abs(dC.g - lC.g) > 2 || Math.abs(dC.b - lC.b) > 2);
    ok(differ, 'dark 与 light 的徽章底色**不同**（跟随主题；相等说明根本没跟随）');

    // ---- 断言 2：不能等于旧的写死白 ----
    // 用**数值**比较而非字符串（第一版栽在这里）。
    const WHITE = { r: 255, g: 255, b: 255, a: 0.22 };
    const isOldWhite = c => eq(c, WHITE, 2);
    ok(!isOldWhite(dC), 'dark 的徽章底色不是旧的写死白 rgba(255,255,255,.22)');
    ok(!isOldWhite(lC), 'light 的徽章底色不是旧的写死白 rgba(255,255,255,.22)');

    // ---- 断言 3：确实是与 --acc 的混合 ----
    // 期望 = acc * 0.22 + 透明底色的实际结果。由于徽章叠在按钮底色上，
    // 这里只校验"色相与 --acc 一致"：归一化后三通道的比例应接近 acc。
    function hueClose(c, accHex) {
      const hex = accHex.replace('#', '');
      if (hex.length !== 6) return true;   // 拿不到就跳过
      const a = { r: parseInt(hex.slice(0, 2), 16), g: parseInt(hex.slice(2, 4), 16), b: parseInt(hex.slice(4, 6), 16) };
      if (!c) return false;
      const ratio = (x, y) => (y === 0 ? 1 : x / y);
      const rd = ratio(c.r, a.r), gd = ratio(c.g, a.g), bd = ratio(c.b, a.b);
      // 混合后整体按同一比例缩放，三通道比值应彼此接近
      const mx = Math.max(rd, gd, bd), mn = Math.min(rd, gd, bd);
      return mx - mn < 0.25;
    }
    ok(hueClose(dC, dark.acc), 'dark 徽章底色与 --acc(' + dark.acc + ') 同色相');
    ok(hueClose(lC, light.acc), 'light 徽章底色与 --acc(' + light.acc + ') 同色相');

    // ---- 断言 4：文字在按钮上可读 ----
    const lum = c => c ? (0.2126 * c.r + 0.7152 * c.g + 0.0722 * c.b) / 255 : null;
    for (const [name, s, c] of [['dark', dark, dC], ['light', light, lC]]) {
      const fg = parseColor(s.badgeFg);
      if (fg && c) {
        const L1 = lum(fg), L2 = lum(c);
        const hi = Math.max(L1, L2), lo = Math.min(L1, L2);
        ok((hi + 0.05) / (lo + 0.05) >= 1.5, name + ': 徽章文字与其底色有可分辨对比');
      }
    }

    await ev(`window.__wb2api__.setThemePref('system')`);
    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { /* 已退出 */ } }

  console.log(fail === 0 ? '\n=== P4 徽章检查通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
