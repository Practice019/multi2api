// panels_e2e.js —— T6 端到端：上游面板按能力位生成与显隐。
//
// # 验证什么
//
//  1. codearts 的 welfare 面板**自动出现**（前端此前 0 入口，用户看不见）
//  2. workbuddy 的成长/旅行面板出现在 workbuddy 组下
//  3. 导航分成「通用 / workbuddy / codearts」三组
//  4. 篡改 manifest 注入一个假上游 → 导航**自动多一组**（判据 1 的现场证明）
//  5. 把 workbuddy 的能力位抹掉 → 它的面板从导航消失（B7 的另一半）
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
const PORT = 9226;
const URL_UNDER_TEST = process.env.PANEL_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-panelprofile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

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
    '--window-size=1400,1000', 'about:blank',
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

    // ---------------------------------------------------------------- 分组
    console.log('\n[1] 导航分组（通用 + 每上游一组）');
    const groups = JSON.parse(await evalJs(`JSON.stringify(
      Array.from(document.querySelectorAll('#nav .navgroup')).map(g => ({
        pid: g.dataset.pid || '(通用)',
        hd: (g.querySelector('.navgrouphd')||{}).textContent || '',
        items: Array.from(g.querySelectorAll('button[data-nav]')).map(b => b.textContent.replace(/\\s+/g,' ').trim()),
      })))`));
    groups.forEach(g => console.log('    ' + g.pid + ' [' + g.hd.trim() + ']: ' + g.items.join(' | ')));

    const pids = groups.map(g => g.pid);
    ok(pids.indexOf('(通用)') >= 0, '有「通用」组');
    ok(pids.indexOf('workbuddy') >= 0, '有 workbuddy 组（实际 ' + JSON.stringify(pids) + '）');
    ok(pids.indexOf('codearts') >= 0, '有 codearts 组');

    const wb = groups.find(g => g.pid === 'workbuddy');
    const ca = groups.find(g => g.pid === 'codearts');
    ok(wb && wb.items.some(i => i.indexOf('成长计划') >= 0), 'workbuddy 组下有「成长计划」');
    ok(wb && wb.items.some(i => i.indexOf('猫猫旅行') >= 0), 'workbuddy 组下有「猫猫旅行」');
    ok(ca && ca.items.some(i => i.indexOf('福利') >= 0), 'codearts 组下有「福利中心」（此前前端 0 入口）');

    // ---------------------------------------------------------------- 组头徽章（CP2 F4）
    //
    // # 为什么必须断言**渲染结果**而不只是"组存在"
    //
    // 评审 CP2 F4 证明了一个刺眼的事实：把组头的账号数徽章整块删掉
    // （`const cnt = ''`），18 个前端套件 + 3 个浏览器 E2E **全部照样全绿**。
    // 原因就是这个 [1] 段只断言"有某某组"，不检查组头里到底有什么 ——
    // 它甚至把被破坏后的组头文字打印出来了，然后照样 PASS。
    //
    // 那条"全绿"因此**不能**作为"T4 交付了账号数徽章"的证据。
    // 现在补上：用 manifest 里的 account_count 做对照（不硬编码数字，
    // 否则数据一变测试就假红）。
    console.log('\n[1b] 组头账号数徽章必须与 manifest.account_count 一致');
    const badge = JSON.parse(await evalJs(`(() => {
      const W = window.__wb2api__;
      const m = W.manifest();
      const out = [];
      for (const p of m.providers) {
        const g = document.querySelector('#nav .navgroup[data-pid="' + p.id + '"]');
        if (!g) continue;   // 该上游没有可见面板时不占导航位，跳过
        const el = g.querySelector('.navgcount');
        out.push({ id: p.id, expect: p.account_count, actual: el ? Number(el.textContent) : null });
      }
      return JSON.stringify(out);
    })()`));
    badge.forEach(b => console.log('      ' + b.id + ' account_count=' + b.expect + ' 徽章=' + b.actual));
    ok(badge.length > 0, '至少检查到一个上游组头');
    ok(badge.every(b => b.actual === b.expect),
      '每个组头的徽章值 == manifest.account_count（徽章被删会变 null → 红）');
    ok(badge.every(b => b.actual !== null),
      '徽章元素真实存在于 DOM（F4 的注入形态就是它消失）');

    // ---------------------------------------------------------------- 面板内容
    console.log('\n[2] codearts 福利面板真的渲染出条目');
    const welfare = await evalJs(`(() => {
      const sec = document.querySelector('#content > section[data-provider="codearts"][data-cap="welfare"]');
      if (!sec) return 'NOSEC';
      return sec.textContent.replace(/\\s+/g,' ').trim().slice(0, 220);
    })()`);
    console.log('    内容: ' + welfare);
    ok(welfare !== 'NOSEC', 'codearts welfare section 存在');
    ok(/积分|体验版|CreditTotal|package/.test(welfare), '渲染出真实福利/套餐内容（不是空面板）');

    // ---------------------------------------------------------------- 假上游注入（判据 1）
    console.log('\n[3] 注入假上游 → 导航自动多一组（判据 1 现场证明）');
    const injected = await evalJs(`(async () => {
      // 用页面自己的契约层注入一个"前端从未见过"的上游
      const W = window.__wb2api__; const original = W.manifest();
      const fake = W.manifest();
      fake.providers.push({ id:'ghostnet', capabilities:['chat','welfare'], default:false, account_count:0 });
      fake.admin_routes.push({ provider:'ghostnet', method:'GET', path:'/admin/welfare', capability:'welfare', title:'幽灵福利' });
      W.applyManifest(fake);
      W.syncProviderPanels();
      W.buildNav();
      const groups = Array.from(document.querySelectorAll('#nav .navgroup')).map(g => g.dataset.pid || '(通用)');
      const hasPanel = !!document.querySelector('#content > section[data-provider="ghostnet"]');
      // 还原
      W.applyManifest(original);
      W.syncProviderPanels();
      W.buildNav();
      const after = Array.from(document.querySelectorAll('#nav .navgroup')).map(g => g.dataset.pid || '(通用)');
      return JSON.stringify({ groups, hasPanel, after });
    })()`);
    const inj = JSON.parse(injected);
    console.log('    注入后分组: ' + JSON.stringify(inj.groups));
    console.log('    还原后分组: ' + JSON.stringify(inj.after));
    ok(inj.groups.indexOf('ghostnet') >= 0, '注入假上游后导航自动出现 ghostnet 组（前端 0 改动）');
    ok(inj.hasPanel === true, '假上游的面板 section 被自动生成');
    ok(inj.after.indexOf('ghostnet') < 0, '还原后 ghostnet 组消失（无残留）');

    // ---------------------------------------------------------------- B7：抹掉能力位
    console.log('\n[4] 抹掉 workbuddy 的 travel 能力位 → 该面板消失（B7）');
    const b7 = await evalJs(`(async () => {
      const W = window.__wb2api__; const original = W.manifest();
      const stripped = W.manifest();
      const wb = stripped.providers.find(p => p.id === 'workbuddy');
      wb.capabilities = wb.capabilities.filter(c => c !== 'travel');
      stripped.admin_routes = stripped.admin_routes.filter(r => !(r.provider==='workbuddy' && r.capability==='travel'));
      W.applyManifest(stripped);
      W.syncProviderPanels();
      W.buildNav();
      const items = Array.from(document.querySelectorAll('#nav button[data-nav]')).map(b => b.textContent);
      const sec = document.querySelector('#content > section[data-provider="workbuddy"][data-cap="travel"]');
      const out = { hasTravelEntry: items.some(t => t.indexOf('猫猫旅行') >= 0), secHidden: sec ? sec.hidden : null };
      W.applyManifest(original);
      W.syncProviderPanels();
      W.buildNav();
      return JSON.stringify(out);
    })()`);
    const b = JSON.parse(b7);
    console.log('    travel 入口存在=' + b.hasTravelEntry + '  section.hidden=' + b.secHidden);
    ok(b.hasTravelEntry === false, '没有 travel 能力位时，「猫猫旅行」入口从导航消失');
    ok(b.secHidden === true, '对应 section 被标记 hidden（不只是不加载数据）');

    // ---------------------------------------------------------------- 健康
    console.log('\n[5] 页面健康');
    const errs = await evalJs('JSON.stringify(window.__errs || [])');
    ok(errs === '[]' || errs === undefined, '无未捕获脚本错误');

    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== 上游面板端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
