// panels_e2e.js —— T6 端到端：上游面板按能力位生成与显隐。
//
// # 验证什么
//
//  1. codearts 的 welfare 面板**不再出现** —— 用户要求删掉「福利中心」标签页。
//     做法是后端把它声明成 hidden（"路由存在但不是面板入口"），
//     前端按标志位过滤；能力位与端点都保留。
//     直接后果：codearts 组下没有任何可见面板 → **整组从左侧导航消失**
//    （该分组原本只有 welfare 这一个面板）—— 这是预期行为。
//  2. workbuddy 的成长/旅行面板出现在 workbuddy 组下
//  3. 导航分成「通用 / workbuddy」两组（codearts 无面板，不占位置）
//  4. 篡改 manifest 注入一个假上游 → 导航**自动多一组**（判据 1 的现场证明）
//  5. 把 workbuddy 的能力位抹掉 → 它的面板从导航消失（B7 的另一半）
//  6. hidden 是**数据驱动**的：只给假上游一条 hidden 的 GET 路由 → 它同样不出现
//     （这条是"隐藏由标志位决定、不是前端写死上游名"的现场证明）
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
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
    // ⚠ 本轮改动翻转的断言：codearts 组**必须不存在**。
    //
    // 用户要求删除 codearts 的「福利中心」标签页。它的 welfare 两条 GET 路由
    // 被后端声明成 hidden（路由存在、但不是面板入口）→ panelCapsOf 为空
    // → 不生成任何 section → buildNav 的 `if (!secs.length) continue`
    // 让**整个 codearts 分组**不占导航位置。这是预期行为，不是漏了分组头。
    ok(pids.indexOf('codearts') < 0,
      'codearts 组不存在（它唯一的面板 welfare 已被后端标成 hidden）—— 实际 ' + JSON.stringify(pids));

    const wb = groups.find(g => g.pid === 'workbuddy');
    const ca = groups.find(g => g.pid === 'codearts');
    ok(wb && wb.items.some(i => i.indexOf('成长计划') >= 0), 'workbuddy 组下有「成长计划」');
    ok(wb && wb.items.some(i => i.indexOf('猫猫旅行') >= 0), 'workbuddy 组下有「猫猫旅行」');
    ok(!ca, 'codearts 组下没有「福利中心」标签页（该组整个消失）');

    // ---------------------------------------------------------------- 隐藏是数据驱动的
    //
    // # 为什么必须单独断言这一层（而不是只看"面板不在"）
    //
    // "某个上游的面板不在"也可能是因为前端把它写死了 —— 那样加第四个上游时
    // 又要改前端（判据 1 失效）。真正要守的是：
    //   · 路由**仍在** manifest 里（能力位/端点没被删）
    //   · 面板入口判据（panelRoutesFor）把它滤掉了
    //   · 两个函数的差恰好是 hidden 那几条
    // 这条也是本轮的**变异验证**落点：把 panelCapsOf 换回用 routesFor，
    // 下面第一条断言立刻变红。
    console.log('\n[1c] hidden 标志位驱动面板消失（不是前端写死上游名）');
    const hiddenProbe = JSON.parse(await evalJs(`(() => {
      const W = window.__wb2api__;
      const m = W.manifest();
      const caGetRoutes = (m.admin_routes || []).filter(r =>
        r.provider === 'codearts' && r.capability === 'welfare' &&
        String(r.method).toUpperCase() === 'GET');
      return JSON.stringify({
        panelCaps: W.panelCapsOf('codearts'),
        allGet: W.routesFor('codearts', 'welfare').length,
        entryGet: W.panelRoutesFor('codearts', 'welfare').length,
        routePaths: caGetRoutes.map(r => r.path),
        hiddenFlags: caGetRoutes.map(r => !!r.hidden),
        caps: (m.providers.find(p => p.id === 'codearts') || {}).capabilities || [],
      });
    })()`));
    console.log('    panelCapsOf(codearts) = ' + JSON.stringify(hiddenProbe.panelCaps));
    console.log('    GET 路由: 全部 ' + hiddenProbe.allGet + ' / 面板入口 ' + hiddenProbe.entryGet
      + '  hidden=' + JSON.stringify(hiddenProbe.hiddenFlags) + '  ' + JSON.stringify(hiddenProbe.routePaths));
    ok(hiddenProbe.panelCaps.indexOf('welfare') < 0,
      'panelCapsOf(codearts) 不含 welfare（换回 routesFor 会让这条变红）—— 实际 '
      + JSON.stringify(hiddenProbe.panelCaps));
    ok(hiddenProbe.allGet === 2, 'routesFor 的语义**不变**：两条 welfare GET 仍在（实际 ' + hiddenProbe.allGet + '）');
    ok(hiddenProbe.entryGet === 0, 'panelRoutesFor 把这 2 条都滤掉了（实际 ' + hiddenProbe.entryGet + '）');
    ok(hiddenProbe.hiddenFlags.length === 2 && hiddenProbe.hiddenFlags.every(Boolean),
      '两条 welfare GET 都带 hidden:true（实际 ' + JSON.stringify(hiddenProbe.hiddenFlags) + '）');
    ok(hiddenProbe.caps.indexOf('welfare') >= 0,
      'codearts 的 welfare 能力位仍然存在（隐藏面板 ≠ 抹掉能力位）—— 实际 '
      + JSON.stringify(hiddenProbe.caps));

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

    // ---------------------------------------------------------------- 面板不存在
    //
    // ⚠ 本轮改动翻转的断言：原先这里断言 codearts 福利面板**存在**并渲染出条目。
    // 用户要求删除该标签页，所以现在断言的是它**不存在**。
    // "内容仍然可读"这件事由 [1c] 的 routesFor（2 条 GET 仍在）保证 —— 端点没删。
    console.log('\n[2] codearts 的 welfare 面板**不存在**（用户要求删除「福利中心」标签页）');
    const welfare = await evalJs(`(() => {
      const sec = document.querySelector('#content > section[data-provider="codearts"][data-cap="welfare"]');
      if (!sec) return 'NOSEC';
      return sec.textContent.replace(/\\s+/g,' ').trim().slice(0, 220);
    })()`);
    console.log('    section 查询结果: ' + (welfare === 'NOSEC' ? '(不存在)' : welfare));
    ok(welfare === 'NOSEC', 'codearts welfare section 不存在（出现了说明 hidden 没生效）');

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

    // ---------------------------------------------------------------- 假上游 + hidden
    //
    // # 为什么这一条不能只靠 codearts 现场证明
    //
    // [1c]/[2] 证明的是"codearts 的面板不在"。若有人把这事实现成前端写死上游名，
    // 那两条**照样绿** —— 而那正是判据 1 被打破的形态。
    // 所以这里注入一个前端**从未见过**的上游，它唯一的 welfare GET 路由带 hidden：
    // 它必须和 codearts 一样不出现。这条只有"隐藏由标志位驱动"才能通过。
    console.log('\n[3b] 假上游的唯一 GET 路由带 hidden → 它同样不占导航位置');
    const hiddenInjected = await evalJs(`(async () => {
      const W = window.__wb2api__; const original = W.manifest();
      const fake = W.manifest();
      fake.providers.push({ id:'hiddennet', capabilities:['chat','welfare'], default:false, account_count:0 });
      fake.admin_routes.push({ provider:'hiddennet', method:'GET', path:'/admin/welfare', capability:'welfare', title:'隐形福利', hidden:true });
      W.applyManifest(fake);
      W.syncProviderPanels();
      W.buildNav();
      const out = {
        groups: Array.from(document.querySelectorAll('#nav .navgroup')).map(g => g.dataset.pid || '(通用)'),
        hasPanel: !!document.querySelector('#content > section[data-provider="hiddennet"]'),
        panelCaps: W.panelCapsOf('hiddennet'),
        // hasAnyPanel 走 routesFor（不滤 hidden）—— 所以它是 true 而分组仍不出现，
        // 证明"分组消失"来自"没有可见 section"，不是上游被丢弃。
        hasAnyPanel: W.hasAnyPanel('hiddennet'),
      };
      W.applyManifest(original);
      W.syncProviderPanels();
      W.buildNav();
      out.after = Array.from(document.querySelectorAll('#nav .navgroup')).map(g => g.dataset.pid || '(通用)');
      return JSON.stringify(out);
    })()`);
    const hi = JSON.parse(hiddenInjected);
    console.log('    注入后分组: ' + JSON.stringify(hi.groups) + '  panelCaps=' + JSON.stringify(hi.panelCaps)
      + '  hasAnyPanel=' + hi.hasAnyPanel);
    ok(hi.groups.indexOf('hiddennet') < 0, '唯一的 GET 路由带 hidden → 该上游不占导航位置（前端 0 改动）');
    ok(hi.hasPanel === false, '也没有为它生成面板 section');
    ok(hi.hasAnyPanel === true, 'hasAnyPanel 仍为 true（分组消失来自"无可见 section"，不是上游被丢弃）');
    ok(hi.after.indexOf('hiddennet') < 0, '还原后无残留');

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
