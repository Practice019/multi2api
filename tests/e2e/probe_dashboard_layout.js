// probe_layout.js 验证仪表盘元素在浏览器里的**实际渲染顺序**（2026-10-20）
//
// # 为什么不能只 grep 行号
//
// HTML 里的先后不等于渲染后的先后：CSS 的 flex/order、grid 自动排布、
// 甚至是 display:none 的节点，都会让"文件顺序"与"用户看到的顺序"不一致。
// 用户的要求是**看到的**顺序（「图表统一放到最下面」），所以必须读渲染坐标。
//
// 判据：比较各区块的 getBoundingClientRect().top —— 谁在上面一目了然。
const http = require('http');
const get = u => new Promise((s, j) => http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => s(d)); }).on('error', j));
const sleep = ms => new Promise(r => setTimeout(r, ms));

const STATS = JSON.stringify({
  total: 3, ok: 3, fail: 0, tokens: 62938, out_tokens: 1500, think_tokens: 32, think_tokens_with_out: 20,
  avg_ttfb_ms: 1000, avg_total_ms: 2000,
  window_from: '2026-10-20T07:00:00Z', window_to: '2026-10-20T08:00:00Z',
  by_model_detail: { 'm-a': { req: 2, ok: 2, fail: 0, tokens: 57938, out_tokens: 1000 }, 'm-b': { req: 1, ok: 1, fail: 0, tokens: 5000, out_tokens: 500 } },
  by_provider_detail: { p1: { req: 2, ok: 2, fail: 0, tokens: 57938, out_tokens: 1000 } },
  by_model: {}, by_provider: {}, by_uid: {},
});
const SERIES = JSON.stringify({
  range: '24h', bucket: '1h', source: 'log',
  series: [{ ts: '2026-10-20T07:00:00Z', total: 3, ok: 3, fail: 0, tokens: 62938,
             by_model: { 'm-a': 57938 }, by_model_out: { 'm-a': 1000 } }],
});

(async () => {
  const base = process.argv[2], token = process.argv[3];
  const list = JSON.parse(await get('http://127.0.0.1:9236/json/list'));
  const t = list.find(x => x.type === 'page');
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  let seq = 0; const pend = new Map();
  const fulfill = (rid, body) => ws.send(JSON.stringify({
    id: ++seq, method: 'Fetch.fulfillRequest', params: {
      requestId: rid, responseCode: 200,
      responseHeaders: [{ name: 'Content-Type', value: 'application/json' }],
      body: Buffer.from(body).toString('base64'),
    },
  }));
  ws.onmessage = e => {
    const m = JSON.parse(e.data);
    if (m.method === 'Fetch.requestPaused') {
      const u = m.params.request.url;
      if (u.includes('/admin/stats/series')) return fulfill(m.params.requestId, SERIES);
      if (u.includes('/admin/stats')) return fulfill(m.params.requestId, STATS);
      if (u.includes('/admin/logs')) return fulfill(m.params.requestId, JSON.stringify({ items: [], total: 0, enabled: true, path: './x', max_bytes: 1, keep_days: 1 }));
      if (u.includes('/status')) return fulfill(m.params.requestId, JSON.stringify({ healthy: 1, total: 1, accounts: [{ uid: 'u1', healthy: true, in_flight: 0 }] }));
      return fulfill(m.params.requestId, '{}');
    }
    const f = pend.get(m.id); if (f) { pend.delete(m.id); f(m); }
  };
  const send = (m, p) => new Promise(r => { const i = ++seq; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: m, params: p })); });
  await new Promise(r => { ws.onopen = r; });
  await send('Page.enable');
  await send('Fetch.enable', { patterns: [{ urlPattern: '*/admin/*' }, { urlPattern: '*/status*' }, { urlPattern: '*/healthz*' }] });
  await send('Page.navigate', { url: `${base}/ui?token=${encodeURIComponent(token)}` });
  await sleep(7000);

  const probe = `(() => {
    const ids = ['cards','statsCards','statsCards2','trendView','statsTrend','modelRank','healthBar','statsReport','statsHeat','cacheCards','dailyTrend'];
    const rows = ids.map(id => {
      const el = document.getElementById(id);
      if (!el) return { id, ok: false };
      const r = el.getBoundingClientRect();
      return { id, ok: true, top: Math.round(r.top + window.scrollY), h: Math.round(r.height), w: Math.round(r.width) };
    }).filter(x => x.ok);
    rows.sort((a, b) => a.top - b.top);
    // 关键判据
    const pos = {};
    rows.forEach((r, i) => pos[r.id] = i + 1);
    const out = {
      order: rows.map((r, i) => (i+1) + '. ' + r.id + '  top=' + r.top + 'px  h=' + r.h),
      cardsBeforeTrend: pos['statsCards'] < pos['statsTrend'],
      cards2BeforeTrend: pos['statsCards2'] < pos['statsTrend'],
      trendBeforeRank: pos['statsTrend'] < pos['modelRank'],
      chartsAfterCards: pos['statsTrend'] > pos['statsCards2'],
      thinkCard: (() => { const el = document.querySelector('[data-stat-key="推理 token"] .v'); return el ? el.textContent.trim() : '<缺>'; })(),
      outCard: (() => { const el = document.querySelector('[data-stat-key="输出 token"] .v'); return el ? el.textContent.trim() : '<缺>'; })(),
    };
    return out;
  })()`;
  const r = await send('Runtime.evaluate', { expression: probe, returnByValue: true });
  const v = r.result.result.value;
  console.log('--- 渲染顺序（按 top 坐标升序）---');
  (v.order || []).forEach(l => console.log('  ' + l));
  console.log('--- 判据 ---');
  console.log('  指标卡在趋势图之上: ' + v.cardsBeforeTrend);
  console.log('  Top模型在趋势图之上: ' + v.cards2BeforeTrend);
  console.log('  趋势图在次数排行之上: ' + v.trendBeforeRank);
  console.log('  ★ 图表统一在信息卡之下: ' + v.chartsAfterCards + (v.chartsAfterCards ? '  ✓' : '  ⚠ 不满足'));
  console.log('--- 两张 token 卡的值 ---');
  console.log('  输出 token = ' + v.outCard + '   推理 token = ' + v.thinkCard);
  ws.close();
})().catch(e => { console.error('PROBE FAIL: ' + e.message); process.exit(3); });
