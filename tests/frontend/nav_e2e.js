// nav_e2e.js —— 用真实 Chrome（headless + CDP）验证导航分组的**渲染结果**。
//
// # 为什么不只用 generator 单测
//
// generator 套件是把 webui.html 里的函数文本抠出来在 Node 里跑 ——
// 它能验证纯函数逻辑，但验证不了：
//   - buildNav 真的被调用了、真的写进了 #nav
//   - CSS 分组样式真的生效（.navgroup / .collapsed 的 display 行为）
//   - 点击组头真的折叠、且状态真的持久化
//   - showPanel 与 data-key 的配合是否真的切换了 section
//
// 这些都是"页面级"事实，只有真浏览器能证明。CDP 直连（不装 playwright）
// 是因为本机有 Chrome 但没有 node 浏览器库，装一个只为跑几条断言不划算。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
const PORT = 9222;
const URL_UNDER_TEST = process.env.NAV_TEST_URL || 'http://127.0.0.1:18099/ui';
const DEBUG_DIR = require('os').tmpdir() + '/chrome-navprofile';  // 绝对路径（原为相对 cwd）

function get(url) {
  return new Promise((res, rej) => {
    http.get(url, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}

// 极简 CDP 客户端（用 Node 内置 WebSocket，Node 22+ 自带）
async function cdp(wsUrl) {
  const ws = new WebSocket(wsUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  let id = 0;
  const pending = new Map();
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
  };
  const send = (method, params) => new Promise(res => {
    const myId = ++id;
    pending.set(myId, res);
    ws.send(JSON.stringify({ id: myId, method, params: params || {} }));
  });
  return { send, close: () => ws.close() };
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(DEBUG_DIR, { recursive: true, force: true });
  const chrome = spawn(CHROME, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${DEBUG_DIR}`,
    '--window-size=1400,1000', 'about:blank',
  ], { stdio: 'ignore' });

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    // 等 Chrome 起来
    let ver = null;
    for (let i = 0; i < 40; i++) {
      try { ver = JSON.parse(await get(`http://127.0.0.1:${PORT}/json/version`)); break; } catch { await sleep(250); }
    }
    if (!ver) throw new Error('Chrome 未在 40 次重试内启动');
    console.log('Chrome: ' + ver.Browser);

    // 取一个 page 类型的 target。
    //
    // 不用 /json/new：Chrome 111+ 要求 PUT 才允许新建标签，
    // GET 会返回一段纯文本（"Using unsafe HTTP verb GET..."），
    // 对它 JSON.parse 就得到本脚本最初的报错。
    // 用启动时自带的那一个 about:blank 页面即可。
    let target = null;
    for (let i = 0; i < 20; i++) {
      const list = JSON.parse(await get(`http://127.0.0.1:${PORT}/json/list`));
      target = list.find(t => t.type === 'page' && t.webSocketDebuggerUrl);
      if (target) break;
      await sleep(250);
    }
    if (!target) throw new Error('找不到可用的 page target');
    const c = await cdp(target.webSocketDebuggerUrl);
    await c.send('Page.enable');
    await c.send('Runtime.enable');
    // 导航到目标页（/json/new 已带 URL，这里再显式导航一次保证加载完成）
    await c.send('Page.navigate', { url: URL_UNDER_TEST });
    await sleep(2500);

    const evalJs = async (expr) => {
      const r = await c.send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) {
        throw new Error('页面内异常: ' + JSON.stringify(r.result.exceptionDetails));
      }
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // ---------------------------------------------------------------- 页面基本可用
    console.log('\n[1] 页面加载与契约层');
    ok(await evalJs('!!document.getElementById("nav")'), '#nav 存在');
    ok(await evalJs('document.querySelectorAll("#nav button[data-nav]").length') > 0, '导航有面板项');
    const navErr = await evalJs('String(window.__navError||"")');
    ok(navErr === '', '页面无脚本错误（' + (navErr || '无') + '）');

    // ---------------------------------------------------------------- 分组
    console.log('\n[2] 上游分组渲染');
    const groups = await evalJs(`JSON.stringify(Array.from(document.querySelectorAll('#nav .navgroup')).map(g=>({pid:g.dataset.pid||'',hd:(g.querySelector('.navgrouphd')||{}).textContent||''})))`);
    const gl = JSON.parse(groups);
    console.log('    实际分组: ' + JSON.stringify(gl));
    ok(gl.length >= 1, '至少有「通用」组');
    ok(gl[0].pid === '', '第一组是通用组（无 data-pid）');
    // 上游组（T6 之前上游面板还不存在，所以只断言"若存在则正确"）
    const upstream = gl.filter(g => g.pid);
    console.log('    上游组数量: ' + upstream.length + (upstream.length ? ' (' + upstream.map(u => u.pid).join(', ') + ')' : '（T6 后才有）'));

    // ---------------------------------------------------------------- 组头内容（CP2 F4）
    //
    // # 为什么这段必须存在
    //
    // 评审 CP2 F4 证明：把组头的账号数徽章整块删掉，18 套件 + 3 个浏览器 E2E
    // **全部照样全绿** —— 因为上面这段只断言"有某某组"、并把组头文字打印出来，
    // 却从不检查它到底有什么。那让"全绿"无法作为"无用户可见回归"的证据。
    //
    // 这里改为拿 manifest 的 account_count 做对照（不硬编码数字），
    // 并且断言徽章**在 0 时也必须存在**（0 与"徽章坏了"必须可区分）。
    console.log('\n[2b] 组头账号数徽章（以 manifest 为对照，不硬编码）');
    const badge = JSON.parse(await evalJs(`(() => {
      const W = window.__wb2api__;
      const m = W.manifest();
      const out = [];
      for (const p of m.providers) {
        const g = document.querySelector('#nav .navgroup[data-pid="' + p.id + '"]');
        if (!g) continue;
        const el = g.querySelector('.navgcount');
        out.push({ id: p.id, expect: p.account_count, actual: el ? Number(el.textContent) : null });
      }
      return JSON.stringify(out);
    })()`));
    badge.forEach(b => console.log('      ' + b.id + ' account_count=' + b.expect + ' 徽章=' + b.actual));
    ok(badge.length > 0, '至少检查到一个上游组头');
    ok(badge.every(b => b.actual !== null), '徽章元素存在于每个上游组头（F4 的注入形态是它消失）');
    ok(badge.every(b => b.actual === b.expect), '徽章值 == manifest.account_count');

    // 组头 title 不能与"是否有面板"矛盾（CP2 F1）
    const contradiction = await evalJs(`(() => {
      const bad = [];
      document.querySelectorAll('#nav .navgroup[data-pid]').forEach(g => {
        const hd = g.querySelector('.navgrouphd');
        const nPanel = g.querySelectorAll('button[data-nav]').length;
        const t = hd ? (hd.getAttribute('title') || '') : '';
        if (nPanel > 0 && t.indexOf('无可打开的面板') >= 0) bad.push(g.dataset.pid);
      });
      return JSON.stringify(bad);
    })()`);
    const badTitles = JSON.parse(contradiction);
    ok(badTitles.length === 0, '没有"有面板却说无可打开的面板"的矛盾组头' +
      (badTitles.length ? ' —— ' + JSON.stringify(badTitles) : ''));

    // ---------------------------------------------------------------- 通用项都在
    console.log('\n[3] 通用面板入口完整');
    const labels = JSON.parse(await evalJs(`JSON.stringify(Array.from(document.querySelectorAll('#nav button[data-nav]')).map(b=>b.textContent.replace(/\\s+/g,' ').trim()))`));
    console.log('    导航项: ' + labels.join(' | '));
    for (const need of ['仪表盘', '账号池', '请求日志', '任务历史', '可用模型', '对话测试', '设置']) {
      ok(labels.some(l => l.indexOf(need) >= 0), '有「' + need + '」入口');
    }

    // ---------------------------------------------------------------- 切换面板
    console.log('\n[4] 点击切换面板（data-key 生效）');
    const clicked = await evalJs(`(() => {
      const bs = Array.from(document.querySelectorAll('#nav button[data-nav]'));
      const b = bs.find(x => x.textContent.indexOf('账号池') >= 0);
      if (!b) return 'NOBTN';
      b.click();
      const sec = document.querySelector('#content > section.active');
      return sec ? (sec.dataset.key || '(no key)') : '(none)';
    })()`);
    ok(clicked === 'accounts', '点「账号池」后 active section 的 data-key = accounts（实际 ' + clicked + '）');

    // ---------------------------------------------------------------- 折叠
    console.log('\n[5] 分组折叠行为');
    const foldable = gl.filter(g => g.pid);
    if (!foldable.length) {
      console.log('    跳过：T6 之前没有上游组可折叠');
    } else {
      const pid = foldable[0].pid;
      const st = await evalJs(`(() => {
        const g = document.querySelector('#nav .navgroup[data-pid="${pid}"]');
        const hd = g.querySelector('.navgrouphd');
        hd.click();
        const collapsed = g.classList.contains('collapsed');
        const itemsHidden = getComputedStyle(g.querySelector('.navitems')).display === 'none';
        return JSON.stringify({collapsed, itemsHidden, saved: localStorage.getItem('wb2api.navcollapsed')});
      })()`);
      const s = JSON.parse(st);
      ok(s.collapsed === true, '点击组头后该组被标记为 collapsed');
      ok(s.itemsHidden === true, '折叠后 .navitems 真的 display:none（CSS 生效）');
      ok(String(s.saved).indexOf(pid) >= 0, '折叠状态写进了 localStorage');
    }

    // ---------------------------------------------------------------- 无外部资源
    console.log('\n[6] 零外部依赖（无 CDN / 无图标字体）');
    //
    // 只检查页面**自己声明**的资源（link/script/img 的静态属性），
    // 不检查运行时 fetch（那是同源的 /admin/* 调用，本来就该有）。
    const ext = JSON.parse(await evalJs(`JSON.stringify(Array.from(document.querySelectorAll('link,script,img'))
      .map(e => e.getAttribute('href') || e.getAttribute('src') || '')
      .filter(u => /^(https?:)?\\/\\//.test(u)))`));
    if (ext.length) console.log('    外部资源: ' + JSON.stringify(ext));
    ok(ext.length === 0, '页面没有声明任何外部 http(s) 资源（实际 ' + ext.length + ' 个）');
    const emoji = await evalJs(`/\\p{Extended_Pictographic}/u.test(document.getElementById('nav').textContent)`);
    ok(emoji === false, '导航文字里没有 emoji');

    c.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== 导航端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
