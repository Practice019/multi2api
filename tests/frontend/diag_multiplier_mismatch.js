// diag_multiplier_mismatch.js —— T6 前置：倍率为什么在界面上显示不出来？
//
// 实测线索：32 个模型里只有 15 个能在 /admin/models/preview 里找到倍率。
// 对不上的看着有规律（前缀名、auto vs default）。本脚本把规律查实。
'use strict';
const http = require('http');

const KEY = process.env.WB2API_KEY || 'test-key-not-real';
const PORT = Number(process.env.FRESH_PORT || 18080);

function get(path) {
  return new Promise((res, rej) => {
    http.get({ host: '127.0.0.1', port: PORT, path, headers: { Authorization: 'Bearer ' + KEY } }, r => {
      const chunks = [];
      r.on('data', c => chunks.push(c));
      r.on('end', () => { try { res(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch { res(null); } });
    }).on('error', rej);
  });
}

(async () => {
  const models = (await get('/v1/models')).data || [];
  const prev = (await get('/admin/models/preview')).models || [];
  const mult = {};
  prev.forEach(x => mult[x.model] = x.multiplier);

  console.log('=== 逐个模型看倍率命中情况 ===\n');
  const bare = [], prefixed = [];
  for (const m of models) {
    const id = m.id;
    const slash = id.indexOf('/');
    const rec = {
      id,
      owned: m.owned_by,
      has: Object.prototype.hasOwnProperty.call(mult, id),
      value: mult[id],
      // 若带前缀，去掉前缀再查
      stripped: slash >= 0 ? id.slice(slash + 1) : null,
      strippedHas: slash >= 0 ? Object.prototype.hasOwnProperty.call(mult, slash >= 0 ? id.slice(slash + 1) : '') : null,
      strippedValue: slash >= 0 ? mult[id.slice(slash + 1)] : undefined,
    };
    (slash >= 0 ? prefixed : bare).push(rec);
  }

  console.log('--- 裸名（' + bare.length + ' 个）---');
  bare.forEach(r => console.log('  ' + r.id.padEnd(24) + ' 直接命中=' + r.has + '  倍率=' + JSON.stringify(r.value)));

  console.log('\n--- 带前缀（' + prefixed.length + ' 个）---');
  prefixed.forEach(r => console.log('  ' + r.id.padEnd(30) + ' 直接命中=' + r.has +
    '  去前缀后=' + r.strippedHas + ' (' + r.stripped + ') 倍率=' + JSON.stringify(r.strippedValue)));

  // 统计
  const directHit = models.filter(m => Object.prototype.hasOwnProperty.call(mult, m.id)).length;
  const stripHit = models.filter(m => {
    const s = m.id.indexOf('/');
    return s >= 0 && Object.prototype.hasOwnProperty.call(mult, m.id.slice(s + 1));
  }).length;

  console.log('\n=== 汇总 ===');
  console.log('  直接命中      : ' + directHit + '/' + models.length);
  console.log('  去前缀后命中  : ' + stripHit + '/' + models.length);

  console.log('\n=== 倍率表里 /v1/models 没有的（前 12）===');
  const ids = new Set(models.map(m => m.id));
  const onlyInTable = prev.filter(x => !ids.has(x.model));
  onlyInTable.slice(0, 12).forEach(x => console.log('  ' + x.model.padEnd(26) + ' ×' + x.multiplier));
  console.log('  （共 ' + onlyInTable.length + ' 个）');

  console.log('\n=== 结论 ===');
  if (directHit === 0 && stripHit > 0) {
    console.log('  倍率表用的是**裸名**，而面板显示 **provider/model** 形态 → id 对不上');
  }
  const autoIssue = models.find(m => m.id === 'auto');
  if (autoIssue && !Object.prototype.hasOwnProperty.call(mult, 'auto')) {
    console.log('  另外 "auto" 在倍率表里叫 "default" —— 是**第二个**命名不一致');
  }
  process.exit(0);
})().catch(e => { console.log('EXCEPTION: ' + e.message); process.exit(1); });
