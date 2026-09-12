// verify_cp2_f1.js —— 复现并验证评审 CP2 F1 的修复。
//
// # F1 的形态
//
// 一个**只声明 chat** 的上游：它确实会生成「对话」面板（有 chat 的 GET 端点），
// 但组头 title 却写「无专属能力面板」—— 界面与悬停说明直接矛盾。
// 根因：判断"有没有面板"时错误地用了"有没有非 chat/models 能力"。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9229;
const URL_UNDER_TEST = process.env.F1_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-f1profile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

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

    console.log('\n[CP2 F1] 只声明 chat 的上游：有面板，title 不能谎报"无面板"');
    const out = JSON.parse(await evalJs(`(async () => {
      const W = window.__wb2api__;
      const original = W.manifest();
      const m = W.manifest();
      m.providers.push({ id:'chatonly', capabilities:['chat'], default:false, account_count:2 });
      m.admin_routes.push({ provider:'chatonly', method:'GET', path:'/admin/chatx', capability:'chat', title:'对话面板' });
      W.applyManifest(m);
      W.syncProviderPanels();
      W.buildNav();
      const g = document.querySelector('#nav .navgroup[data-pid="chatonly"]');
      const hd = g ? g.querySelector('.navgrouphd') : null;
      const res = {
        groupExists: !!g,
        headerTitle: hd ? hd.getAttribute('title') : '',
        nItems: g ? g.querySelectorAll('button[data-nav]').length : 0,
        itemLabels: g ? Array.from(g.querySelectorAll('button[data-nav]')).map(b => b.textContent.trim()) : [],
        secCount: document.querySelectorAll('#content > section[data-provider="chatonly"]').length,
      };
      W.applyManifest(original); W.syncProviderPanels(); W.buildNav();
      // 还原后也应无残留
      res.afterRestore = document.querySelectorAll('#content > section[data-provider="chatonly"]').length;
      return JSON.stringify(res);
    })()`));
    console.log('    ' + JSON.stringify(out, null, 0));

    ok(out.groupExists === true, 'chat-only 上游出现导航组');
    ok(out.secCount === 1, '它确实生成了一个面板（secCount=1）');
    ok(out.nItems >= 1, '导航里确实有 1 个入口：' + JSON.stringify(out.itemLabels));
    // 核心断言：有面板时，title 不能声称没有面板
    ok(out.headerTitle.indexOf('无可打开的面板') < 0,
      'title 不再谎报「无可打开的面板」（实际：' + out.headerTitle + '）');
    ok(/能力：|基础能力面板/.test(out.headerTitle),
      'title 如实说明它有面板（实际：' + out.headerTitle + '）');
    ok(out.afterRestore === 0, '还原后无残留');

    console.log('\n[对照] 正常上游的 title 仍然列出专属能力');
    const normal = await evalJs(`(() => {
      const g = document.querySelector('#nav .navgroup[data-pid="workbuddy"]');
      return g ? g.querySelector('.navgrouphd').getAttribute('title') : 'MISSING';
    })()`);
    console.log('    workbuddy title: ' + normal);
    ok(/能力：/.test(normal), 'workbuddy 的 title 仍列出能力（实际：' + normal + '）');
    ok(normal.indexOf('chat') < 0, 'title 里不列 chat 这类基础能力（保持原有动机）');

    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== CP2 F1 修复验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
