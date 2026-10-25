// probe_o2.js 验热力图与按天趋势是否按「纯输出」着色（2026-10-20, Round 11）
// mock series：两天数据，输出量差 20 倍，total 差 2 倍。
// 若图仍按 total 着色 ⇒ 两格颜色接近；若按输出 ⇒ 两格明显不同（深浅两档）。
const http = require('http');
const get = u => new Promise((s, j) => http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => s(d)); }).on('error', j));
const sleep = ms => new Promise(r => setTimeout(r, ms));

const d1 = new Date(Date.now() - 24 * 3600 * 1000).toISOString();
const d2 = new Date().toISOString();
const SERIES = JSON.stringify({
  range: '365d', bucket: '24h', source: 'log',
  series: [
    // 第 1 天：total 大但输出极小（典型"长上下文转发"）
    { ts: d1, total: 10, ok: 10, fail: 0, tokens: 2000000, by_model: { a: 2000000 }, by_model_out: { a: 100 } },
    // 第 2 天：total 小一半，但输出是 20 倍
    { ts: d2, total: 10, ok: 10, fail: 0, tokens: 1000000, by_model: { a: 1000000 }, by_model_out: { a: 2000 } },
  ],
});
const STATS = JSON.stringify({
  total: 20, ok: 20, fail: 0, tokens: 3000000, out_tokens: 2100, think_tokens: 0,
  avg_ttfb_ms: 1000, avg_total_ms: 2000,
  window_from: d1, window_to: d2,
  by_model_detail: { a: { req: 20, ok: 20, fail: 0, tokens: 3000000, out_tokens: 2100 } },
  by_provider_detail: {}, by_model: {}, by_provider: {}, by_uid: {},
});

(async () => {
  const base = process.argv[2], token = process.argv[3];
  const list = JSON.parse(await get('http://127.0.0.1:9235/json/list'));
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
    const out = {};
    // 热力图：取所有格子的背景色，看有几种不同色阶
    const cells = [...document.querySelectorAll('[class*="heat"]')].filter(e => e.offsetWidth > 0 && e.offsetWidth < 30);
    const colors = {};
    cells.forEach(c => { const bg = getComputedStyle(c).backgroundColor; if (bg && bg !== 'rgba(0, 0, 0, 0)') colors[bg] = (colors[bg] || 0) + 1; });
    out.heatCells = cells.length;
    out.heatColorVariants = Object.keys(colors).length;
    out.heatColors = Object.entries(colors).sort((a,b)=>b[1]-a[1]).slice(0,6).map(([k,v]) => k + '×' + v);
    // Token 趋势：柱子高度分布
    // ⚠ 这里原来看的是 #dailyTrend，那张图已于 2026-10-26 删除
    //   （它与 #statsTrend 同数据同范围、是后者的严格子集）。
    //   改看保留下来的 #statsTrend —— 它现在就是"那张 token 趋势图"。
    const trend = document.querySelector('#statsTrend') || [...document.querySelectorAll('[id*="rend"]')][0];
    if (trend) {
      const rects = [...trend.querySelectorAll('rect')].map(r => Math.round(parseFloat(r.getAttribute('height') || '0')));
      out.trendId = trend.id;
      out.trendHeights = rects.filter(h => h > 0).slice(0, 8);
      out.trendMaxH = rects.length ? Math.max(...rects) : 0;
    } else { out.trendId = '<未找到按天趋势容器>'; }
    return out;
  })()`;
  const r = await send('Runtime.evaluate', { expression: probe, returnByValue: true });
  console.log(JSON.stringify(r.result.result.value, null, 1));
  ws.close();
})().catch(e => { console.error('PROBE FAIL: ' + e.message); process.exit(3); });
