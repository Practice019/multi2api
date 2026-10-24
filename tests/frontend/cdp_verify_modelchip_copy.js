// tests/frontend/cdp_verify_modelchip_copy.js —— 在真浏览器里验证
// 「点一下可用模型的卡片 → 完整模型名进了剪贴板」。
//
// # 为什么必须跑真浏览器
//
// 静态门禁（ui_wiring_modelchip_test.go）只能证明「源码里有 copyText(id, ...)」
// 与「生成处带 data-id」这两件事**同时存在**。它证明不了：
//
//   · 事件委托真的挂上了（监听器绑在 #models 上，chip 是后渲染的子节点）
//   · dataset.id 里真的是完整 id（而不是被 esc/截断过的显示文字）
//   · 剪贴板真的拿到了值（clipboard API 在非安全上下文会静默不可用）
//
// 这三件都只在真 DOM 上成立或不成立。
//
// # 用法
//   node tests/frontend/cdp_verify_modelchip_copy.js [http://127.0.0.1:7863/ui]
//
// ⚠ 无第三方依赖：Node 22+ 自带全局 WebSocket，Chrome 用 --remote-debugging-port。
'use strict';

const { spawn } = require('child_process');
const http = require('http');
const os = require('os');
const path = require('path');
const fs = require('fs');

const TARGET = process.argv[2] || 'http://127.0.0.1:7863/ui';
// 控制台要身份：优先环境变量，其次命令行第 3 个参数。
// 用 admin_token 那条路（Authorization: Bearer），不必走登录表单 + 人机校验。
const ADMIN_TOKEN = process.env.WB2API_ADMIN_TOKEN || process.argv[3] || '';
// 模型目录（/v1/models）要的是**对话密钥**（api_key），不是 admin_token。
// 两者是不同身份：admin_token 进控制台，api_key 调 /v1/*。
// 不给它，「可用模型」面板会是空的（HTTP 401 invalid_api_key），
// 本脚本就没有卡片可点 —— 那不是功能坏了，是没喂密钥。
const API_KEY = process.env.WB2API_API_KEY || process.argv[4] || '';
const PORT = 9341;
const CHROME = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((p) => fs.existsSync(p));

if (!CHROME) { console.log('FAIL 找不到 Chrome/Edge'); process.exit(1); }

const userDir = path.join(os.tmpdir(), 'cdp-mchip-' + Date.now());
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
    let wsUrl = null;
    for (let i = 0; i < 40; i++) {
      await sleep(300);
      try {
        const j = JSON.parse(await httpGet('http://127.0.0.1:' + PORT + '/json/version'));
        if (j.webSocketDebuggerUrl) { wsUrl = j.webSocketDebuggerUrl; break; }
      } catch {}
    }
    if (!wsUrl) { fail('CDP 端口没起来'); throw new Error('no cdp'); }

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

    // ev0 早于 ev 定义：注入密钥那段要用它，而那时 ev 还没声明。
    const ev0 = async (expr) => {
      const r = await pcmd('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    await pcmd('Page.enable');
    await pcmd('Runtime.enable');
    await pcmd('Network.enable');
    if (ADMIN_TOKEN) {
      // 页面本身也要带这个头才拿得到 /ui 的 HTML（否则被重定向到登录页）。
      await pcmd('Network.setExtraHTTPHeaders', {
        headers: { Authorization: 'Bearer ' + ADMIN_TOKEN },
      });
      console.log('  已注入 admin_token（前 8 位 ' + ADMIN_TOKEN.slice(0, 8) + '…）');
    } else {
      console.log('  ⚠ 未提供 admin_token —— 若页面跳登录页，本脚本必然失败');
    }
    await pcmd('Page.navigate', { url: TARGET });
    await sleep(5000);

    // ★ 必须清掉 CDP 注入的 Authorization 头。
    //
    // 它的作用只是「让 /ui 那次导航拿到 HTML 而不是登录页」——
    // 但 setExtraHTTPHeaders 对**这个 target 之后的所有请求**都生效，
    // 包括页面自己发的 /v1/models。而 /v1/models 认的是 api_key，
    // admin_token 在它那里是无效的 ⇒ 覆盖后稳定 401。
    //
    // 实测踩过：不清头时页面内手动 fetch('/v1/models', {Authorization: Bearer <api_key>})
    // 也是 401，而同样的请求在 PowerShell 里 200 —— 因为浏览器实际发出去的是被
    // 注入头覆盖后的值。这个形态极易误判成「后端坏了」。
    if (ADMIN_TOKEN) {
      await pcmd('Network.setExtraHTTPHeaders', { headers: {} });
      console.log('  已清掉注入的 Authorization 头（避免覆盖页面的 api_key）');
    }

    // 切到「可用模型」面板并按需重拉目录。
    //
    // 两个都必须做，缺一个都拿不到卡片：
    //   ① 那个 section 默认 display:none —— 不切过去，里面的元素即使渲染了也
    //      查得到但不可见（本脚本要点击，必须可见）；
    //   ② 首屏那次 renderModels 可能已经失败过（比如当时 KEY 还没取到），
    //      留下一条错误文本；点「强制刷新目录」才会重跑。
    const switchPanel = async () => {
      return await ev0(`(() => {
        const secs = Array.from(document.querySelectorAll('section[data-key]'));
        const idx = secs.findIndex(s => s.dataset.key === 'models');
        if (idx < 0) return 'no models section; keys=' + secs.map(s => s.dataset.key).join(',');
        const btn = document.querySelector('button[data-nav="' + idx + '"]');
        if (!btn) return 'no nav button for idx=' + idx;
        btn.click();
        return 'ok idx=' + idx;
      })()`);
    };
    console.log('  切到可用模型面板: ' + await switchPanel());
    await sleep(2500);

    let chipCount = await ev0('document.querySelectorAll(".chips .chip").length');
    if (!chipCount) {
      console.log('  面板里还没有卡片，点「强制刷新目录」…');
      await ev0('(() => { const b = document.getElementById("btnModelRefresh"); if (b) b.click(); return !!b; })()');
      await sleep(12000);
      chipCount = await ev0('document.querySelectorAll(".chips .chip").length');
    }
    console.log('  卡片数: ' + chipCount);

    const ev = async (expr) => {
      const r = await pcmd('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return { __err: JSON.stringify(r.result.exceptionDetails).slice(0, 300) };
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // ── ① 卡片存在，且 data-id 是完整 id（含 /） ──
    const st = await ev(`(() => {
      const chips = Array.from(document.querySelectorAll('.chips .chip'));
      if (!chips.length) return { ok:false, why:'一个 .chips .chip 都没有' };
      const c0 = chips[0];
      return {
        ok: true, total: chips.length,
        id0: c0.dataset.id || '',
        text0: (c0.textContent || '').trim().slice(0, 60),
        cursor: getComputedStyle(c0).cursor,
        hasSlash: (c0.dataset.id || '').includes('/'),
      };
    })()`);
    if (!st || st.__err) { fail('读页面状态失败: ' + JSON.stringify(st)); throw new Error('bad page'); }
    if (!st.ok) {
      // 最常见的原因就是没带 token 被重定向到登录页 —— 直接说清楚，
      // 别让人以为是「卡片功能坏了」。
      const where = await ev('location.href');
      const title = await ev('document.title');
      fail(st.why + '（当前页: ' + where + ' / ' + title + '）');
      if (/login/i.test(String(title))) {
        console.log('  ⇒ 这是登录页。请用 admin_token 跑：');
        console.log('    node tests/frontend/cdp_verify_modelchip_copy.js http://127.0.0.1:7863/ui <admin_token>');
      }
      throw new Error('no chips');
    }
    console.log('  卡片总数: ' + st.total);
    console.log('  首个 data-id: ' + st.id0);
    console.log('  首个可见文字: ' + st.text0);
    console.log('  cursor: ' + st.cursor);

    if (st.hasSlash) pass('data-id 是完整模型名（含 /）');
    else fail('data-id 不含 / —— 不是完整模型名: ' + st.id0);

    if (st.cursor === 'pointer') pass('卡片显示为可点击（cursor:pointer）');
    else fail('cursor 是 ' + st.cursor + '，卡片看起来不可点');

    // ── ② 点一下，看剪贴板 ──
    await ev(`(() => {
      // 先清空剪贴板，避免"上次就有这个值"造成假通过
      try { navigator.clipboard.writeText('__EMPTY__'); } catch {}
      const c = document.querySelector('.chips .chip');
      c.click();
      return true;
    })()`);
    await sleep(900);

    const got = await ev(`(async () => {
      try { return await navigator.clipboard.readText(); } catch (e) { return '__ERR__' + e.message; }
    })()`);

    console.log('');
    console.log('  点击后剪贴板: ' + got);
    if (got === st.id0) pass('点击后剪贴板 == data-id（复制成功）');
    else if (got === '__EMPTY__') fail('剪贴板没变 —— 点击没有触发复制');
    else fail('剪贴板内容与 data-id 不一致：' + got);

    // ── ③ toast 必须给出反馈 ──
    const toastTxt = await ev(`(() => {
      const els = Array.from(document.querySelectorAll('*'))
        .filter(e => e.children.length === 0 && /已复制|复制失败/.test(e.textContent || ''));
      return els.length ? els[els.length - 1].textContent.trim() : '';
    })()`);
    console.log('  toast: ' + (toastTxt || '(无)'));
    if (/已复制/.test(toastTxt)) pass('toast 提示已复制');
    else fail('没有「已复制」提示 —— 复制是无声操作，用户无法确认');

  } catch (e) {
    if (!/no cdp|bad page|no chips/.test(String(e && e.message))) {
      fail('异常: ' + (e && e.message));
    }
  } finally {
    try { chrome.kill(); } catch {}
  }

  console.log('');
  console.log(fails === 0 ? 'RESULT OK（全部通过）' : 'RESULT FAIL（' + fails + ' 项失败）');
  process.exit(fails === 0 ? 0 : 1);
})();
