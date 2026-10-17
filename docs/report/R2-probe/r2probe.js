// R2 对抗性探针 v2（Playwright）：只在浏览器侧 stub 两个接口，不改源码、不写服务端数据。
// 每条 payload 用一次 reload 落定（避免 busyRun 连点吞掉导致的假结论）。
const { chromium } = require('playwright');

const BASE = 'http://127.0.0.1:7864';
const OUT = [];
function say(...a) { const s = a.map(x => typeof x === 'string' ? x : JSON.stringify(x)).join(' '); OUT.push(s); console.log(s); }
const sleep = ms => new Promise(r => setTimeout(r, ms));

let statsStub = null, statsCalls = 0, seriesStub = null;

function hourly() {
  const out = [], p = n => String(n).padStart(2, '0');
  for (let i = 0; i < 25; i++) {
    const d = new Date(Date.now() - (24 - i) * 3600e3);
    out.push({ ts: `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:00:00+08:00`, total: i, ok: i, fail: 0, tokens: 1 });
  }
  out[out.length - 1].total = 7; out[out.length - 1].ok = 7;
  return out;
}
function daily(lastTotal) {
  const out = [], p = n => String(n).padStart(2, '0');
  for (let i = 0; i < 8; i++) {
    const d = new Date(Date.now() - (7 - i) * 86400e3);
    out.push({ ts: `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T00:00:00+08:00`, total: 1, ok: 1, fail: 0, tokens: 2 });
  }
  out[out.length - 1].total = lastTotal; out[out.length - 1].ok = lastTotal;
  return out;
}

const SNAP = () => {
  const t = id => document.getElementById(id);
  const rows = [...document.querySelectorAll('#statsTopUid .toprow')].map(r => ({
    name: r.querySelector('.tname')?.textContent, bar: r.querySelector('.tbar i')?.style.width,
    val: r.querySelector('.tval')?.textContent, pct: r.querySelector('.tpct')?.textContent,
    title: r.getAttribute('title'),
    attrs: [...r.attributes].map(a => a.name).filter(n => n !== 'title' && n !== 'class'),
  }));
  const lg = [...document.querySelectorAll('#statsDonutLegend .lgrow')].map(r => ({
    name: r.querySelector('.lgname')?.textContent, val: r.querySelector('.tval')?.textContent,
    pct: r.querySelector('.tpct')?.textContent,
    attrs: [...r.attributes].map(a => a.name).filter(n => n !== 'title' && n !== 'class'),
  }));
  const donut = t('statsDonut'), trend = t('statsTrend');
  const h3 = [...document.querySelectorAll('.boardsec')].map(s => s.querySelector('h3')?.textContent?.replace(/\s+/g, ' ').trim()).find(x => x && x.includes('调用统计'));
  const path = trend?.querySelector('path[stroke="#3fb950"]')?.getAttribute('d') || '';
  return {
    rows, topRaw: t('statsTopUid')?.textContent.trim(),
    hint: t('statsTopHint')?.textContent,
    legend: lg,
    donutCenter: donut?.textContent.trim(), donutBg: (donut?.style.background || '').slice(0, 120),
    trendCaption: trend?.querySelector('div')?.textContent?.replace(/\s+/g, ' ').trim(),
    trendL: (path.match(/L/g) || []).length,
    h3,
    cardTotal: [...document.querySelectorAll('#statsCards .card')].map(c => c.querySelector('.k').textContent + '=' + c.querySelector('.v').textContent).join(' | '),
    rangeBtns: [...document.querySelectorAll('#trendRange button')].map(b => b.dataset.range + (b.classList.contains('on') ? '*' : '')),
  };
};

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: 'C:\\Users\\21877\\AppData\\Local\\ms-playwright\\chromium-1200\\chrome-win64\\chrome.exe',
  });
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
  const errs = [];
  page.on('pageerror', e => errs.push('PAGEERROR: ' + e.message));
  page.on('console', m => { if (m.type() === 'error') errs.push('CONSOLE: ' + m.text().slice(0, 160)); });

  await page.route('**/*', async route => {
    let u; try { u = new URL(route.request().url()); } catch { return route.fallback(); }
    if (u.pathname === '/admin/stats') {
      statsCalls++;
      if (!statsStub) return route.fallback();
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(statsStub) });
    }
    if (u.pathname === '/admin/stats/series') {
      if (!seriesStub) return route.fallback();
      const r = await seriesStub(u.searchParams.get('range') || '24h');
      if (r && r.delay) await sleep(r.delay);
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(r.body) });
    }
    return route.fallback();
  });

  // 一条 payload = 一次 reload（确定性）
  const load = async (stub, label) => {
    statsStub = stub;
    const before = statsCalls;
    await page.reload({ waitUntil: 'domcontentloaded' });
    for (let i = 0; i < 60; i++) { await sleep(250); if (statsCalls > before) break; }
    await sleep(1500);
    return page.evaluate(SNAP);
  };
  const waitIdle = async () => {
    try { await page.waitForFunction(() => { const b = document.getElementById('refresh'); return b && !b.disabled; }, null, { timeout: 60000 }); return true; }
    catch { return false; }
  };
  // 页内重渲染：等 #refresh 可用再点，然后等条件成立
  const reApply = async (stub, cond, ms = 30000) => {
    const ok = await waitIdle();
    statsStub = stub;
    await page.click('#refresh');
    const t0 = Date.now();
    let s = null;
    while (Date.now() - t0 < ms) {
      await sleep(500);
      s = await page.evaluate(SNAP);
      if (cond(s)) break;
    }
    return { s, waited: Date.now() - t0, idleBefore: ok };
  };

  await page.goto(BASE + '/ui', { waitUntil: 'domcontentloaded' });
  await sleep(3000);
  say('### 初始（真接口，本实例日志可能为空）');
  say(JSON.stringify(await page.evaluate(SNAP), null, 1).slice(0, 900));

  const A1 = await load({ total: 0, ok: 0, fail: 0, tokens: 0, think_tokens: 0, by_uid: {}, by_provider: {} });
  say('\n### A1 by_uid={} by_provider={} → 零除面');
  say('topRaw=', JSON.stringify(A1.topRaw), '| donutCenter=', JSON.stringify(A1.donutCenter), '| donutBg=', JSON.stringify(A1.donutBg), '| hint=', JSON.stringify(A1.hint));

  const A2 = await load({ total: 10, ok: 9, fail: 1, by_uid: { a1: 5, b2: 0 }, by_provider: { loomy: 26012 } });
  say('\n### A2 by_provider 老形状裸数字 / by_uid {a1:5,b2:0}');
  say('rows=', A2.rows, '\nhint=', JSON.stringify(A2.hint), 'donutCenter=', A2.donutCenter, 'legend=', A2.legend, 'donutBg=', A2.donutBg);

  const A3 = await load({ total: 0, ok: 0, fail: 0, by_uid: { a1: 0, b2: 0 }, by_provider: { p1: 0 } });
  say('\n### A3 全 0');
  say('rows=', A3.rows, '\nhint=', JSON.stringify(A3.hint), '| donutCenter=', JSON.stringify(A3.donutCenter), '| legend=', A3.legend);
  await page.screenshot({ path: 'docs/report/shots/R2-zero-state.png' });

  const A4 = await load({ total: 0, ok: 0, fail: 0, by_uid: { neg: -5, good: 3 }, by_provider: { neg: -3, good: 4 } });
  say('\n### A4 负数');
  say('rows=', A4.rows, '\nlegend=', A4.legend, '| donutCenter=', A4.donutCenter);

  const A5 = await load({ total: 0, ok: 0, fail: 0, by_uid: { u1: { calls: 9 }, u2: { req: 7 } }, by_provider: { p_req: { req: 7, ok: 7, fail: 0, tokens: 10 }, p_calls: { calls: 4 } } });
  say('\n### A5 形状兼容：by_provider {req:7} vs {calls:4}；by_uid {calls:9} vs {req:7}');
  say('rows=', A5.rows, '\nlegend=', A5.legend, '| donutCenter=', A5.donutCenter);

  const A6 = await load({ total: 0, ok: 0, fail: 0, by_uid: { u1: { calls: { n: 7 } } }, by_provider: { p1: { calls: { n: 7 } } } });
  say('\n### A6 更深嵌套 {calls:{n:7}}');
  say('rows=', A6.rows, '\nlegend=', A6.legend, '| donutCenter=', JSON.stringify(A6.donutCenter), '| topRaw=', JSON.stringify(A6.topRaw));

  const A7a = await load({ total: 0, ok: 0, fail: 0, by_uid: { a1: 5 }, by_provider: { p1: 4 } });
  say('\n### A7a 基线（by_provider 有数据 ⇒ hint 会落值）');
  say('rows=', A7a.rows.length, 'hint=', JSON.stringify(A7a.hint));
  const A7b = await reApply({ total: 0, ok: 0, fail: 0, by_uid: { a1: 5, b2: 5 }, by_provider: {} }, s => s.rows.length === 2);
  say('### A7b 页内重渲染：by_uid 2 键 + by_provider={}（等 ' + A7b.waited + 'ms，idleBefore=' + A7b.idleBefore + '）');
  say('rows=', A7b.s.rows.length, '行名=', A7b.s.rows.map(r => r.name), '| hint=', JSON.stringify(A7b.s.hint), '（若 rows=2 而 hint 仍是「共 1 个账号有请求」= 陈旧）');

  const B = await load({
    total: 0, ok: 0, fail: 0,
    by_uid: { 'x" onmouseover="window.__XSS1=1': 3, '<img src=x onerror="window.__XSS2=1">': 2 },
    by_provider: { 'y" onclick="window.__XSS3=1': 4 },
  });
  say('\n### B esc() 属性上下文（uid/provider 带双引号与标签）');
  say('rows=', B.rows, '\nlegend=', B.legend);
  say('XSS 探针=', JSON.stringify(await page.evaluate(() => ({
    xss1: window.__XSS1, xss2: window.__XSS2, xss3: window.__XSS3,
    onmouseoverEls: document.querySelectorAll('[onmouseover]').length,
    onclickEls: document.querySelectorAll('[onclick]').length,
    imgEls: document.querySelectorAll('#statsTopUid img, #statsDonutLegend img').length,
    uidRowHTML: document.querySelector('#statsTopUid .toprow')?.outerHTML.replace(/\s+/g, ' ').slice(0, 300),
  }))));

  // ---------- C: range 竞态 ----------
  say('\n### C range 竞态：7d 延迟 2500ms，中途点回 24h');
  seriesStub = async (range) => range === '7d'
    ? { delay: 2500, body: { range: '168h0m0s', bucket: '24h0m0s', source: 'log', series: daily(999999) } }
    : { delay: 0, body: { range: '24h0m0s', bucket: '1h0m0s', source: 'log', series: hourly() } };
  await load({ total: 3, ok: 3, fail: 0, by_uid: { a1: 3 }, by_provider: { p1: 3 } });
  await sleep(1000);
  const c1 = await page.evaluate(SNAP);
  say('reload 后(24h):', c1.trendCaption, '| L=', c1.trendL, '| 按钮=', c1.rangeBtns);
  const card0 = c1.cardTotal, rows0 = c1.rows.length;
  await page.click('#trendRange button[data-range="7d"]');
  await sleep(200);
  await page.click('#trendRange button[data-range="24h"]');
  await sleep(400);
  const c2 = await page.evaluate(SNAP);
  say('点回 24h 后 0.4s（7d 未回）:', c2.trendCaption, '| L=', c2.trendL);
  await sleep(3200);
  const c3 = await page.evaluate(SNAP);
  say('7d 迟到落地后:', c3.trendCaption, '| L=', c3.trendL, '| 按钮=', c3.rangeBtns);
  say('切 range 前后卡片/榜是否变了:', '卡片同=', card0 === c3.cardTotal, '| 榜行数', rows0, '→', c3.rows.length, '| h3=', JSON.stringify(c3.h3));
  await page.screenshot({ path: 'docs/report/shots/R2-range-race.png' });

  // ---------- D: 30s 轮询 ----------
  say('\n### D 30s 无感轮询：卡片 vs 报表区');
  seriesStub = null;
  const d1 = await load({ total: 11, ok: 11, fail: 0, by_uid: { a1: 5 }, by_provider: { p1: 5 } });
  say('D1 卡片=', d1.cardTotal, '| 榜行=', d1.rows.length, '| hint=', JSON.stringify(d1.hint));
  statsStub = { total: 99, ok: 99, fail: 0, by_uid: { b1: 9, b2: 9 }, by_provider: { pX: 1, pY: 1 } };
  const t0 = Date.now();
  let d2 = d1, hit = false;
  for (let i = 0; i < 50; i++) {
    await sleep(1000);
    d2 = await page.evaluate(SNAP);
    if (d2.cardTotal.includes('总请求=99')) { hit = true; break; }
  }
  say('轮询命中卡片更新=', hit, '（等 ' + Math.round((Date.now() - t0) / 1000) + 's）卡片=', d2.cardTotal);
  say('同一刻 榜行=', d2.rows.map(r => r.name), '| hint=', JSON.stringify(d2.hint), '| 环=', JSON.stringify(d2.donutCenter));

  // ---------- E: DOM 缺失 ----------
  say('\n### E 老版本 DOM 缺失');
  const e0 = errs.length;
  await page.evaluate(() => ['statsTopUid', 'statsDonut', 'statsDonutLegend', 'statsTopHint', 'trendRange', 'statsTrend']
    .forEach(id => { const el = document.getElementById(id); if (el) el.remove(); }));
  const e1 = errs.length;
  await waitIdle();
  await page.click('#refresh');
  await sleep(2500);
  say('删元素后新增 pageerror/console.error 数=', errs.length - e1, '| 删元素前基线=', e1 - e0);
  say('明细=', errs.slice(-4));

  say('\n### 全程 pageerror/console.error（去重）');
  say([...new Set(errs)].slice(0, 10));

  await browser.close();
  require('fs').writeFileSync('.r2probe.out.txt', OUT.join('\n'), 'utf8');
  console.log('\n[written] .r2probe.out.txt');
})().catch(e => { console.log('FATAL: ' + (e && e.stack || e)); process.exit(3); });
