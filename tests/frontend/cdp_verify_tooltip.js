// tests/frontend/cdp_verify_tooltip.js —— 在真浏览器里验证 24h 趋势图的 hover 读数
//
// # 为什么要跑真浏览器
//
// 2026-10-20 用户报「鼠标在缓存命中率那个表格上悬浮时，没有东西显示」。
// 根因是代码里 `rd.textContent = ...` 的赋值行丢了 —— 语法合法、语义错。
// 语法门禁拦不住，我新增的语义门禁能拦，但两者都是**静态**的：
// 它们能证明"赋值语句存在"，不能证明"DOM 上真的显示了文字"。
//
// 这个脚本补上最后一段：起 Chrome headless → 连 CDP → 打开 /ui →
// 在 SVG 上派发 mousemove → 读 #ctRead 的 textContent。
//
// # 用法
//   node tests/frontend/cdp_verify_tooltip.js http://127.0.0.1:17864/ui
//
// ⚠ 无第三方依赖：Node 22+ 自带全局 WebSocket，Chrome 用 --remote-debugging-port。
'use strict';

const { spawn } = require('child_process');
const http = require('http');
const os = require('os');
const path = require('path');
const fs = require('fs');

// ⚠ 身份（2026-11-01 补，被 verify_probe_console_loaded.js 点名）：T5 之后 /ui 是两态 ——
//   带身份 ⇒ 控制台；不带 ⇒ 登录页（~17-22KB）。裸 /ui 会让本探针**静默跑在登录页上**，
//   而本探针的断言是「悬浮后 #ctRead 里有读数」这类**正向**断言，会红得莫名其妙。
//   所以身份从 env 取（WB2API_PROBE_TOKEN），拿不到就**当场失败**，不再默默跑。
const RAW_TARGET = process.argv[2] || process.env.WB2API_PROBE_URL || 'http://127.0.0.1:17864/ui';
const PROBE_TOKEN = process.env.WB2API_PROBE_TOKEN || '';
const TARGET = (() => {
  if (!/\/ui(\?|$)/.test(RAW_TARGET)) return RAW_TARGET;
  if (/[?&]token=/.test(RAW_TARGET)) return RAW_TARGET;
  return RAW_TARGET + '?token=' + encodeURIComponent(PROBE_TOKEN);
})();
const PORT = 9333;
const CHROME = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((p) => fs.existsSync(p));

if (!CHROME) { console.log('FAIL 找不到 Chrome/Edge'); process.exit(1); }

const userDir = path.join(os.tmpdir(), 'cdp-tooltip-' + Date.now());
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function httpGet(url) {
  return new Promise((res, rej) => {
    http.get(url, (r) => {
      let d = '';
      r.on('data', (c) => (d += c));
      r.on('end', () => res(d));
    }).on('error', rej);
  });
}

(async () => {
  const chrome = spawn(CHROME, [
    '--headless=new',
    '--remote-debugging-port=' + PORT,
    '--user-data-dir=' + userDir,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-gpu',
    'about:blank',
  ], { stdio: 'ignore' });

  let fails = 0;
  const fail = (m) => { console.log('FAIL ' + m); fails++; };
  const pass = (m) => console.log('PASS ' + m);

  try {
    // 等 CDP 端口
    let wsUrl = null;
    for (let i = 0; i < 40; i++) {
      await sleep(300);
      try {
        const j = JSON.parse(await httpGet('http://127.0.0.1:' + PORT + '/json/version'));
        if (j.webSocketDebuggerUrl) { wsUrl = j.webSocketDebuggerUrl; break; }
      } catch {}
    }
    if (!wsUrl) { fail('CDP 端口没起来'); throw new Error('no cdp'); }

    // 连 browser 端点，建一个新 page
    const bws = new WebSocket(wsUrl);
    await new Promise((r, j) => { bws.onopen = r; bws.onerror = j; });
    let id = 0;
    const bpend = new Map();
    bws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && bpend.has(m.id)) { bpend.get(m.id)(m); bpend.delete(m.id); }
    };
    const bcmd = (method, params) => new Promise((r) => {
      const i = ++id; bpend.set(i, r); bws.send(JSON.stringify({ id: i, method, params }));
    });

    const tgt = await bcmd('Target.createTarget', { url: 'about:blank' });
    const targetId = tgt.result.targetId;
    const att = await bcmd('Target.attachToTarget', { targetId, flatten: true });
    const sessionId = att.result.sessionId;

    // 在 page session 上发命令
    let pid = 0;
    const ppend = new Map();
    bws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && ppend.has(m.id)) { ppend.get(m.id)(m); ppend.delete(m.id); }
    };
    const pcmd = (method, params) => new Promise((r) => {
      const i = ++pid; ppend.set(i, r);
      bws.send(JSON.stringify({ id: i, sessionId, method, params }));
    });

    await pcmd('Page.enable');
    await pcmd('Runtime.enable');
    if (!PROBE_TOKEN && /\/ui(\?|$)/.test(RAW_TARGET)) {
      fail('没有身份：给 WB2API_PROBE_TOKEN（或 argv[2] 里直接带 ?token=）—— 否则进的是登录页，后面的断言毫无意义');
      throw new Error('no identity');
    }
    await pcmd('Page.navigate', { url: TARGET });
    await sleep(3500);   // 等首屏与异步取数

    // ── ⓪ 正向自证：进的是**控制台**（不是登录页、也不是空壳）──
    //   这条必须先于所有断言：否则「找不到 #cacheTrend」会被读成产品坏了，
    //   而真相是我们根本没进控制台（T5 之后这是最常见的一种假红/假绿）。
    const loaded = await ev(`(() => {
      const body = document.body ? document.body.innerHTML.length : 0;
      const hasShell = !!document.querySelector('#content');
      const hasApi = !!window.__wb2api__;
      const onLogin = !!document.querySelector('#loginForm') || body < 40000;
      return { body: body, hasShell: hasShell, hasApi: hasApi, onLogin: onLogin };
    })()`);
    if (!loaded || loaded.onLogin || !loaded.hasShell) {
      fail('进的不是控制台（body=' + (loaded && loaded.body) + ' hasShell=' + (loaded && loaded.hasShell)
        + ' hasApi=' + (loaded && loaded.hasApi) + '）—— 先查身份/实例，再查产品');
      throw new Error('not console');
    }
    pass('进的是控制台（body=' + loaded.body + '，__wb2api__=' + loaded.hasApi + '）');

    const ev = async (expr) => {
      const r = await pcmd('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return { err: JSON.stringify(r.result.exceptionDetails).slice(0, 300) };
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // ── ① 页面里有没有那个 SVG 容器 ──
    const hasChart = await ev(`(() => {
      const el = document.querySelector('#cacheTrend');
      if (!el) return { ok:false, why:'找不到 #cacheTrend' };
      const svg = el.querySelector('svg');
      return { ok: !!svg, hasCursor: !!el.querySelector('#ctCursor'), hasRead: !!el.querySelector('#ctRead'),
               data: !!el._ctData, bound: !!el._ctBound, rows: el._ctData ? el._ctData.rows.length : -1 };
    })()`);
    console.log('  页面状态: ' + JSON.stringify(hasChart));
    if (!hasChart || !hasChart.ok) { fail('趋势图 SVG 没渲染出来（可能没登录/没数据）'); throw new Error('no chart'); }
    if (!hasChart.hasRead) fail('#ctRead 元素不存在');
    if (!hasChart.data) fail('host._ctData 没挂上（重绘数据绑定缺失）');

    // ── ② 在 SVG 上派发 mousemove ──
    const hover = await ev(`(() => {
      const host = document.querySelector('#cacheTrend');
      const svg = host.querySelector('svg');
      const rect = svg.getBoundingClientRect();
      const evt = new MouseEvent('mousemove', {
        bubbles: true, cancelable: true,
        clientX: rect.left + rect.width * 0.55,
        clientY: rect.top + rect.height * 0.5,
      });
      host.dispatchEvent(evt);
      const rd = host.querySelector('#ctRead');
      const cur = host.querySelector('#ctCursor');
      return { text: rd ? (rd.textContent || '') : '(no el)',
               display: rd ? rd.style.display : '',
               curDisplay: cur ? cur.style.display : '',
               curX: cur ? cur.getAttribute('x1') : '' };
    })()`);
    console.log('  hover 结果: ' + JSON.stringify(hover));

    if (!hover) { fail('派发 mousemove 后拿不到结果'); }
    else {
      if (hover.display !== 'block') fail('读数元素没被显示（display=' + hover.display + '）');
      else pass('① 读数元素被显示');
      if (!hover.text || hover.text.trim() === '') {
        fail('★ 读数元素显示了，但 **textContent 是空的** —— 这正是用户报的现象');
      } else {
        pass('② 读数有内容：' + hover.text);
        if (!/累计/.test(hover.text)) fail('读数里没有「累计」字段');
        if (!/Total|总 Token/.test(hover.text)) fail('读数里没有「总 Token」字段');
        else pass('③ 字段齐全');
      }
      if (hover.curDisplay !== 'block') fail('参考竖线没显示');
      else pass('④ 参考竖线显示，x=' + hover.curX);
    }

    // ── ③ mouseleave 后应隐藏 ──
    const leave = await ev(`(() => {
      const host = document.querySelector('#cacheTrend');
      host.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }));
      const rd = host.querySelector('#ctRead');
      return rd ? rd.style.display : '(no el)';
    })()`);
    if (leave === 'none') pass('⑤ mouseleave 后读数隐藏');
    else fail('mouseleave 后读数没隐藏（display=' + leave + '）');

  } catch (e) {
    console.log('  （中断：' + e.message + '）');
  } finally {
    try { chrome.kill(); } catch {}
    try { fs.rmSync(userDir, { recursive: true, force: true }); } catch {}
  }

  if (fails > 0) { console.log('\n=== 失败 ' + fails + ' 项 ==='); process.exit(1); }
  console.log('\n=== 全部通过 ===');
})();
