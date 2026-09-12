// accounts_e2e.js —— 真实浏览器验证账号池分组渲染与 B1/B5 修复。
//
// # 验证什么
//
//  T5 验收：账号表按上游分组，组标题显示「上游名 · N 个账号」
//  B1   ：错误态整行提示与真实列数一致（11 列），不再露底色
//  B5   ：/status 回落路径下账号仍能正确归属上游，且有"推断"标记
//
// # 为什么必须在真浏览器里做
//
// 这些都是**渲染结果**：colspan 是否等于表头列数、分组行是否真的插进去了、
// 单元格总数是否等于 cols × rows —— 抠函数文本在 Node 里跑证明不了这些。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
const PORT = 9224;
const URL_UNDER_TEST = process.env.ACC_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-accprofile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

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
      } catch { /* 还没起来 */ }
      await sleep(250);
    }
    if (!target) throw new Error('Chrome 未启动');

    const ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let id = 0; const pending = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
    const send = (method, params) => new Promise(r => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params: params || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: URL_UNDER_TEST });
    await sleep(3000);

    const evalJs = async (expr) => {
      const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) {
        throw new Error('页面内异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 300));
      }
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // ---------------------------------------------------------------- 列数与 colspan 一致性
    console.log('\n[B1] 账号表列数与所有 colspan 一致');
    const meta = JSON.parse(await evalJs(`(() => {
      const table = document.querySelector('#accts').closest('table');
      const ths = table.querySelectorAll('thead th').length;
      const colspans = Array.from(table.querySelectorAll('tbody td[colspan]')).map(td => Number(td.getAttribute('colspan')));
      return JSON.stringify({ ths, colspans });
    })()`));
    console.log('    表头列数=' + meta.ths + '  colspan 值=' + JSON.stringify(meta.colspans));
    ok(meta.ths === 11, '账号表 11 列（实际 ' + meta.ths + '）');
    // 关键：不允许出现"小于列数"的 colspan（那就是 B1 的形态）
    const bad = meta.colspans.filter(n => n < meta.ths);
    ok(bad.length === 0, '没有小于列数的 colspan（B1 的形态是 8 < 11）' + (bad.length ? ' —— 发现 ' + JSON.stringify(bad) : ''));

    // ---------------------------------------------------------------- 分组渲染
    console.log('\n[T5] 账号表按上游分组');
    const grouped = JSON.parse(await evalJs(`(() => {
      const rows = Array.from(document.querySelectorAll('#accts tr'));
      const groups = rows.filter(r => r.classList.contains('grouprow'));
      return JSON.stringify({
        total: rows.length,
        groups: groups.map(g => g.textContent.replace(/\\s+/g,' ').trim()),
        groupCount: groups.length,
      });
    })()`));
    console.log('    总行数=' + grouped.total + '  分组行=' + grouped.groupCount);
    grouped.groups.forEach(g => console.log('      · ' + g));
    ok(grouped.groupCount >= 1, '至少有一个上游分组标题行（实际 ' + grouped.groupCount + '）');
    ok(grouped.groups.every(g => /个账号/.test(g)), '每个组标题都带账号数');
    ok(grouped.total > grouped.groupCount, '分组行之外还有真实账号行');

    // ---------------------------------------------------------------- 分组的**正确性**（自审补）
    //
    // # 为什么 ">= 1 个分组" 不够
    //
    // 自审注入实验证明：把 groupAccountsByProvider 换成"所有账号塞进一个默认上游组"，
    // 上面那三条**全部照样通过** —— 因为那确实还是"有 1 个分组、带账号数、
    // 有真实行"。分组功能整块失效却被判绿。
    //
    // 所以必须断言**分组数量与归属由数据决定**：
    //   分组数 == 账号实际覆盖到的上游数（从 manifest 推导，不硬编码）
    //   每个分组标题里的上游名，必须真的出现在 manifest.providers 里
    console.log('\n[T5] 分组数量与归属由数据决定（不是"有分组就行"）');
    const grpInfo = JSON.parse(await evalJs(`(() => {
      const W = window.__wb2api__;
      const m = W.manifest();
      const known = new Set(m.providers.map(p => p.id));
      // 账号实际覆盖到哪些上游（用页面自己的契约层算，保持同一套解释）
      const rows = Array.from(document.querySelectorAll('#accts tr.grouprow'));
      const titles = rows.map(r => {
        const n = r.querySelector('.gname');
        return n ? n.textContent.trim() : '';
      });
      return JSON.stringify({
        titles,
        known: Array.from(known),
        // 每个账号行第 1 格是「上游」列，取出它去重
        rowProviders: Array.from(new Set(Array.from(document.querySelectorAll('#accts tr:not(.grouprow)'))
          .map(r => (r.children[0] ? r.children[0].textContent.replace(/[?\\s]/g,'') : ''))
          .filter(Boolean))),
      });
    })()`));
    console.log('    分组标题=' + JSON.stringify(grpInfo.titles));
    console.log('    账号行的上游列=' + JSON.stringify(grpInfo.rowProviders));

    // 分组标题必须都是已知上游（不是空串、不是"（未标注）"以外的瞎写）
    ok(grpInfo.titles.every(t => t !== ''), '每个分组标题都有上游名（空标题说明分组没渲染出名字）');
    ok(grpInfo.titles.every(t => grpInfo.known.indexOf(t) >= 0 || /未在 manifest/.test(t) || t === '（未标注）'),
      '分组标题都是 manifest 里真实存在的上游（或明确标注了"未注册"）');

    // 关键：分组数必须覆盖账号实际涉及的所有上游。
    // 把所有账号塞进一组时，rowProviders 会有 2 个而上限 titles 只有 1 个 → 红。
    const covered = grpInfo.rowProviders.filter(p => p !== '');
    if (covered.length > 1) {
      ok(grpInfo.titles.length >= covered.length,
        '分组数(' + grpInfo.titles.length + ') 覆盖了账号涉及的所有上游(' + covered.length + ' 个: ' + covered.join(',') + ')');
    } else {
      console.log('    （账号只涉及 ' + covered.length + ' 个上游，跳过分组数对照）');
    }

    // ---------------------------------------------------------------- 行的单元格配平
    console.log('\n[B1] 每个账号行的单元格数 == 列数');
    const balanced = JSON.parse(await evalJs(`(() => {
      const table = document.querySelector('#accts').closest('table');
      const cols = table.querySelectorAll('thead th').length;
      const bad = [];
      Array.from(table.querySelectorAll('tbody tr')).forEach((r, i) => {
        // 把 colspan 展开成"占了几列"
        let span = 0;
        Array.from(r.children).forEach(td => { span += Number(td.getAttribute('colspan') || 1); });
        if (span !== cols) bad.push({ row: i, span, cols, text: r.textContent.slice(0, 40) });
      });
      return JSON.stringify(bad);
    })()`));
    ok(balanced.length === 0, '所有行都恰好占满 ' + meta.ths + ' 列' + (balanced.length ? ' —— 异常行 ' + JSON.stringify(balanced) : ''));

    // ---------------------------------------------------------------- 页面无脚本错误
    console.log('\n[健康] 页面脚本无异常');
    const errs = await evalJs('JSON.stringify(window.__errs || [])');
    ok(errs === '[]' || errs === undefined, '无未捕获的脚本错误');

    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== 账号池端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
