// jobs_e2e.js —— T9 验证：任务调度面板。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9230;
const URL_UNDER_TEST = process.env.T9_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-t9profile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

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

    console.log('\n[T9] 任务调度面板');
    //
    // ⚠⚠ 2026-10-19 形态变更：任务面板从**6 列表格**换成**卡片网格**
    //    （webui.html:3236-3338 的 renderJobs，理由：错误原文动辄上百字符，
    //     表格里只能截断 + title 悬停，等于把排障信息藏在鼠标后面）。
    //
    // 所以下面每一条判据都**重新表达**在当前 DOM 形态上，判据的**意图**一条没减：
    //   · 老「表头 6 列」        → 新「卡片数 == manifest 注册任务数」（反而更强：
    //                              老的那条对"少渲染几条任务"完全无感）
    //   · 老「每行占满 6 列」    → 新「每张卡都有名称 + 状态行」（表格 colspan
    //                              这个概念在卡片形态下不存在了，等价的意图是
    //                              "每一条的结构都完整"）
    //   · 老「上次执行列不留 —」 → 新「状态行里必须有『上次 …』或『还没有执行记录』」
    //                              （同一个意图：不许用一个会骗人的字面量顶替事实）
    const t = JSON.parse(await evalJs(`(() => {
      const host = document.getElementById('jobrows');
      if (!host) return JSON.stringify({ missing: true });
      const W = window.__wb2api__;
      const m = W.manifest();
      const cards = Array.from(host.querySelectorAll('.job-item')).map(c => ({
        cls: c.className,
        name: ((c.querySelector('.job-name') || {}).textContent || '').replace(/\\s+/g, ' ').trim(),
        provider: ((c.querySelector('.pill') || {}).textContent || '').trim(),
        state: ((c.querySelector('.job-state:not(.err)') || {}).textContent || '').replace(/\\s+/g, ' ').trim(),
        hasDescTip: !!c.querySelector('.job-q'),
        hasError: !!c.querySelector('.job-state.err'),
        btns: c.querySelectorAll('button').length,
      }));
      return JSON.stringify({
        cols: cards.length,
        meta: (document.getElementById('jobMeta') || {}).textContent || '',
        empty: !!host.querySelector('.job-empty'),
        manifestJobs: (m.jobs || []).filter(j => j && typeof j === 'object').map(j => j.name),
        cards,
      });
    })()`));

    if (t.missing) throw new Error('找不到 #jobrows');
    console.log('    卡片数=' + t.cols + '  manifest 注册任务=' + t.manifestJobs.length +
      '  子标题=' + t.meta);
    t.cards.forEach(r => console.log('      ' + r.name + '  [' + r.provider + ']  ' + r.state +
      (r.hasError ? '  [错误卡]' : '')));

    ok(!t.empty, '面板不在加载态/空态（没有 .job-empty 占位）—— 实例确实注册了定时任务');

    // 判据的核心：**卡片数 == manifest 里注册的任务数**。
    // 老那条「表头恰好 6 列」只证明"表还在"，这条证明"任务一条不少地渲染出来了"。
    ok(t.manifestJobs.length > 0, '前置：manifest 里有已注册任务（' + t.manifestJobs.length + ' 个）');
    ok(t.cols === t.manifestJobs.length,
      '每个注册任务都渲染成一张卡（' + t.cols + ' == manifest ' + t.manifestJobs.length + '）');

    ok(t.cards.every(c => c.name.length > 0), '每张卡都有非空的任务名');
    ok(t.cards.every(c => c.state.length > 0), '每张卡都有非空的状态行（间隔/上次/下次）');
    ok(t.cards.every(c => c.btns === 0), '卡片内部没有任何按钮（本面板刻意只读，见 webui.html:1550）');
    ok(/个已注册任务/.test(t.meta), '子标题显示任务数（实际：' + t.meta + '）');
    ok(t.meta.indexOf(String(t.manifestJobs.length)) >= 0,
      '子标题里的任务数 == manifest 的条数（' + t.meta + ' 里应有 ' + t.manifestJobs.length + '）');

    // 三个已知任务都应出现（老断言：按名字列出）
    //
    // ⚠ 英文名现在显示在中文标签后面的括号里（「WorkBuddy 成长计划守卫 (workbuddy-growth-watch)」，
    //   见 renderJobs 的 nameHtml），所以这里按**整串包含**判，与渲染形态解耦。
    for (const n of ['workbuddy-travel-watch', 'workbuddy-growth-watch', 'codearts-refresh']) {
      ok(t.cards.some(c => c.name.indexOf(n) >= 0), '列出任务 ' + n);
    }

    // 「上次执行」不许留空/不许用一个会骗人的字面量顶替。
    //
    // 老形态是「上次执行」列里出现 `—`；新形态是状态行里多一段。
    // 从未执行时必须明说 —— growth-watch 一直没跑过，显示「—」会被当成
    // "正常但无关的信息"（见 renderJobs 里 `seg.push('还没有执行记录')` 那段注释）。
    const badState = t.cards.filter(c => !/上次|还没有执行记录/.test(c.state));
    ok(badState.length === 0,
      '每张卡都讲清了"上次执行"（要么「上次 …」要么「还没有执行记录」）' +
      (badState.length ? ' —— 含糊的: ' + JSON.stringify(badState.map(c => c.name + ': ' + c.state)) : ''));
    const neverRun = t.cards.filter(c => /还没有执行记录/.test(c.state));
    console.log('    未执行的任务数: ' + neverRun.length);
    ok(t.cards.every(c => !/(^|[\s·])—([\s·]|$)/.test(c.state.replace(/每\s*—/, ''))),
      '状态行不靠「—」表达"没有值"（间隔除外）');

    const badItv = t.cards.filter(c => !/每\s*(\d+(\.\d+)?\s*(秒|分|小时|天))/.test(c.state));
    ok(badItv.length === 0,
      '每张卡的「间隔」都是可读时长' +
      (badItv.length ? ' —— 不认识的: ' + JSON.stringify(badItv.map(c => c.name + ': ' + c.state)) : ''));

    // 说明挂在问号上（抄参考项目的 .tip-q）：有 desc 的必须有问号，
    // 没有 desc 的不许留一个点了没反应的圆点。判据取自 manifest，不硬编码。
    const withDesc = JSON.parse(await evalJs(`JSON.stringify(
      (window.__wb2api__.manifest().jobs || []).filter(function(j){ return j && j.desc; }).map(function(j){ return j.name; }))`));
    console.log('    manifest 里带 desc 的任务: ' + withDesc.length + ' 个');
    if (withDesc.length) {
      ok(t.cards.some(c => c.hasDescTip), '有 desc 的任务画出了问号提示（' + withDesc.length + ' 个有 desc）');
    }
    const namesWithTip = t.cards.filter(c => c.hasDescTip).map(c => c.name);
    const tipNotDeclared = namesWithTip.filter(n => !withDesc.some(d => n.indexOf(d) >= 0));
    ok(tipNotDeclared.length === 0,
      '问号只出现在 manifest 声明了 desc 的任务上（不留点了没反应的圆点）' +
      (tipNotDeclared.length ? ' —— 多出来的: ' + JSON.stringify(tipNotDeclared) : ''));

    console.log('\n[T9] 面板在导航里可达');
    const nav = JSON.parse(await evalJs(`JSON.stringify(
      Array.from(document.querySelectorAll('#nav button[data-nav]')).map(b => b.textContent.trim()))`));
    ok(nav.some(x => x.indexOf('任务调度') >= 0), '导航里有「任务调度」入口（实际：' + nav.join(' / ') + '）');

    console.log('\n[T9] 只读：没有手动触发按钮');
    const triggers = await evalJs(`document.querySelectorAll('#content > section[data-key="jobs"] button[data-jobrun], #content > section[data-key="jobs"] button[data-trigger]').length`);
    ok(triggers === 0, '没有任何"手动触发"按钮（重入风险，刻意不做）');
    // ⚠ 补一条更严的：卡片形态下，"手动触发"可能被做成**卡片内的按钮**
    //   （老判据只按 data-jobrun / data-trigger 两个属性找，换实现就漏）。
    //   范围限定在 #jobrows 之内 —— 面板 h2 上那个「刷新」（btnJobRefresh）
    //   是**读**操作的重取，不是"手动跑一次任务"，不在本判据的意图里。
    const anyBtn = await evalJs(`document.querySelectorAll('#jobrows button').length`);
    ok(anyBtn === 0, '任务卡片里**一个按钮都没有**（只读面板；实际 ' + anyBtn + ' 个）');

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

  console.log(fail === 0 ? '\n=== T9 端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
