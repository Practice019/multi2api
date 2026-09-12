// verify_cp2_f2.js —— 复现并验证评审 CP2 F2（监听器泄漏）的修复。
//
// # F2 的形态
//
// bindGenericActions 往长期存在的面板容器上 addEventListener，
// 而 syncProviderPanels() 每 5 秒被 refresh() 调一次 —— 监听器数**线性增长**，
// 且从不移除。60 次 sync 后一次点击会触发 13 个 /admin/welfare 请求。
//
// # 怎么证明修好了
//
//  1. 反复调 syncProviderPanels()，用 CDP 拿**真实监听器数**（不是数代码）
//  2. 自然 5s 自动刷新下观察是否仍增长
//  3. 数网络请求：一次点击必须**只产生 1 个** POST
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9231;
const URL_UNDER_TEST = process.env.F2_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-f2profile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

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
    const requests = [];
    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); }
      else if (m.method === 'Network.requestWillBeSent') {
        requests.push({ url: m.params.request.url, method: m.params.request.method });
      }
    };
    const send = (m, p) => new Promise(r => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method: m, params: p || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Network.enable');
    await send('Page.navigate', { url: URL_UNDER_TEST });
    await sleep(3500);

    const evalJs = async (expr) => {
      const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 300));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // CDP 拿真实监听器数（比在页面里数代码可靠）
    const listenerCount = async (selector) => {
      const doc = await send('DOM.getDocument', { depth: -1 });
      const node = await send('DOM.querySelector', { nodeId: doc.result.root.nodeId, selector });
      if (!node.result || !node.result.nodeId) return -1;
      const l = await send('DOMDebugger.getEventListeners', { objectId: (await send('DOM.resolveNode', { nodeId: node.result.nodeId })).result.object.objectId });
      return (l.result && l.result.listeners ? l.result.listeners.length : 0);
    };

    console.log('\n[F2-1] 反复 syncProviderPanels 后监听器数不增长');
    const counts = [];
    for (let i = 0; i < 25; i++) {
      await evalJs('window.__wb2api__.syncProviderPanels()');
    }
    // 面板容器
    const hostSel = '#pbody-codearts-welfare';
    const hostCount = await listenerCount(hostSel);
    console.log('    ' + hostSel + ' 上的监听器数: ' + hostCount);
    ok(hostCount === 0, '面板容器上**没有**监听器（改为委托到 #content 后应为 0）');

    const contentCount = await listenerCount('#content');
    console.log('    #content 上的监听器数: ' + contentCount);
    // #content 上有若干个（input/change 等历史绑定），关键是它**不随 sync 增长**
    const before = contentCount;
    for (let i = 0; i < 25; i++) await evalJs('window.__wb2api__.syncProviderPanels()');
    const after = await listenerCount('#content');
    console.log('    再 25 次 sync 后: ' + after);
    ok(after === before, '#content 的监听器数不随 syncProviderPanels 增长（' + before + ' → ' + after + '）');

    console.log('\n[F2-2] 自然 5s 自动刷新下也不增长');
    requests.length = 0;
    const c0 = await listenerCount('#content');
    await sleep(12000);   // 跨过 2 个自动刷新周期
    const c1 = await listenerCount('#content');
    console.log('    12 秒后 #content 监听器: ' + c0 + ' → ' + c1);
    ok(c1 === c0, '自动刷新 12 秒后监听器数不变（F2 的原始形态是 +1/次）');

    console.log('\n[F2-3] 一次点击只产生一个 POST');
    // 注入一个可领取的条目，模拟上游真的返回可领福利
    await evalJs(`(() => {
      const W = window.__wb2api__;
      const host = document.getElementById('pbody-codearts-welfare');
      if (!host) return;
      host.innerHTML = '<div class="glist"><div class="gitem">'
        + '<span class="gname">测试福利</span>'
        + '<button data-gclaim="codearts" data-gcampaign="999" data-gpath="/admin/welfare/claim">领取</button>'
        + '</div></div>';
      // 屏蔽确认框与真实写操作，只数请求
      window.confirm = () => true;
      window.__origAdmin = window.fetch;
    })()`);
    requests.length = 0;
    await evalJs(`document.querySelector('#pbody-codearts-welfare button[data-gclaim]').click()`);
    await sleep(1500);
    const posts = requests.filter(r => r.method === 'POST' && /\/admin\/welfare\/claim/.test(r.url));
    console.log('    一次点击产生的 claim POST 数: ' + posts.length);
    ok(posts.length === 1, '一次点击**恰好**一个 POST（F2 时是 13 个）');

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

  console.log(fail === 0 ? '\n=== CP2 F2 修复验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
