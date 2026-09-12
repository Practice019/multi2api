// verify_history_provenance.js —— 历史记录里有没有**非 workbuddy** 账号？
//
// 这是 T4 的前置验证。发现：/admin/checkin/history 的条目只有 uid、没有 provider，
// 且 total=5000（全量）。而本轮已知 codearts 账号 01a08fe0 曾出现在
// travel 面板里 —— 如果历史里也有它，T4 就必须做过滤；否则只需"归位"。
//
// **不能假设。** 拉全量统计 uid 分布，与账号池对照。
'use strict';
const http = require('http');

const KEY = process.env.WB2API_KEY || 'test-key-not-real';
const BASE = { host: '127.0.0.1', port: 18080 };

function get(path) {
  return new Promise((res, rej) => {
    http.get({ host: BASE.host, port: BASE.port, path, headers: { Authorization: 'Bearer ' + KEY } }, r => {
      const chunks = [];
      r.on('data', c => chunks.push(c));
      r.on('end', () => res(JSON.parse(Buffer.concat(chunks).toString('utf8'))));
    }).on('error', rej);
  });
}

(async () => {
  // 1) 账号池：uid → provider
  const acc = await get('/admin/accounts');
  const poolUids = new Set((acc.accounts || []).map(a => a.uid));
  const byProvider = {};
  (acc.accounts || []).forEach(a => {
    byProvider[a.provider] = byProvider[a.provider] || [];
    byProvider[a.provider].push(a.uid);
  });
  console.log('=== 账号池 ===');
  Object.keys(byProvider).forEach(p => console.log('  ' + p.padEnd(12) + byProvider[p].length + ' 个'));
  console.log('  池内 uid 总数: ' + poolUids.size);

  // 2) 全量历史：统计 uid 分布（分页拉，避免一次太多）
  console.log('\n=== 拉全量历史 ===');
  const uidCount = {};
  let offset = 0;
  const limit = 300;
  let total = null;
  let fetched = 0;
  for (let page = 0; page < 40; page++) {
    const r = await get(`/admin/checkin/history?limit=${limit}&offset=${offset}`);
    if (total === null) total = r.total;
    const items = r.items || [];
    if (!items.length) break;
    for (const it of items) uidCount[it.uid] = (uidCount[it.uid] || 0) + 1;
    fetched += items.length;
    offset += limit;
    if (offset >= total) break;
  }
  console.log('  total(服务端)= ' + total + '，实际取到 ' + fetched + ' 条');

  // 3) 对照
  console.log('\n=== uid 分布 vs 账号池 ===');
  const rows = Object.entries(uidCount).sort((a, b) => b[1] - a[1]);
  let outsiders = 0;
  for (const [uid, n] of rows) {
    const inPool = poolUids.has(uid);
    const prov = inPool
      ? (acc.accounts.find(a => a.uid === uid) || {}).provider
      : '(不在池里)';
    if (!inPool) outsiders++;
    console.log('  ' + uid.slice(0, 8) + '  ' + String(n).padStart(5) + ' 条  ' + prov +
      (inPool ? '' : '   ← 不在账号池'));
  }

  console.log('\n=== 结论 ===');
  console.log('  历史里出现的 uid 种类: ' + rows.length);
  console.log('  不在账号池里的 uid: ' + outsiders);
  if (outsiders === 0) {
    console.log('  → 历史**只含池内账号**。T4 只需把 section 归位，');
    console.log('    不必加后端过滤（但建议加一条断言锁住这个性质）。');
  } else {
    console.log('  → 历史含**池外账号**。T4 必须做过滤，否则面板会显示别人的记录。');
  }
  process.exit(0);
})();
