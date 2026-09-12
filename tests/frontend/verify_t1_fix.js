// verify_t1_fix.js —— 在**新鲜实例**上验证 T1 修复真的生效。
//
// # 前置（必须）
//
// 先跑 instance_freshness.js 确认实例是当前源码编的。
// 我上一轮就是没做这一步，拿旧二进制测了半天、结论全错。
//
// # 验什么
//
// T1 修的是 `accountList()` 返回整个池。修好后：
//   /admin/travel 应只列 workbuddy 账号（不含 01a08fe0）
//   /admin/growth 同理
// 而"01a08fe0"是 codearts 账号，通过 codearts.auth_dir 注册进同一个池。
//
// 判据用**数据对照**，不硬编码条数：从 /admin/accounts 读出池内各上游账号，
// 再看面板返回的 uid 集合是不是它的子集。
'use strict';
const http = require('http');

const PORT = Number(process.env.FRESH_PORT || 18200);
const KEY = process.env.WB2API_KEY || 'test-key-not-real';

function get(path) {
  return new Promise((res, rej) => {
    http.get({ host: '127.0.0.1', port: PORT, path, headers: { Authorization: 'Bearer ' + KEY } }, r => {
      const chunks = [];
      r.on('data', c => chunks.push(c));
      r.on('end', () => {
        const body = Buffer.concat(chunks).toString('utf8');
        try { res({ status: r.statusCode, json: JSON.parse(body) }); }
        catch { res({ status: r.statusCode, json: null, body: body.slice(0, 200) }); }
      });
    }).on('error', rej);
  });
}

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  console.log('=== T1 修复验证（端口 ' + PORT + '）===\n');

  // 1) 账号池：uid → provider
  const acc = await get('/admin/accounts');
  const accounts = (acc.json || {}).accounts || [];
  const byUid = {};
  accounts.forEach(a => byUid[a.uid] = a.provider);
  const providers = {};
  accounts.forEach(a => { providers[a.provider] = (providers[a.provider] || 0) + 1; });
  console.log('账号池: ' + JSON.stringify(providers) + '（共 ' + accounts.length + '）');

  ok(accounts.length > 0, '账号池非空（' + accounts.length + ' 个）');

  // 2) travel
  const tr = await get('/admin/travel');
  const trAcc = (tr.json || {}).accounts || [];
  const trUids = trAcc.map(a => a.uid);
  console.log('\n/admin/travel: ' + trAcc.length + ' 条  ' + JSON.stringify(trUids.map(u => u.slice(0, 8))));

  const trOutside = trUids.filter(u => !byUid[u]);
  ok(trOutside.length === 0,
    'travel 不含池外账号' + (trOutside.length ? '（越界: ' + trOutside.map(u => u.slice(0, 8)).join(',') + '）' : ''));

  const trWrongProvider = trUids.filter(u => byUid[u] && byUid[u] !== 'workbuddy');
  ok(trWrongProvider.length === 0,
    'travel 不含别家上游账号' + (trWrongProvider.length ? '（越界: ' + trWrongProvider.map(u => u.slice(0, 8) + '=' + byUid[u]).join(',') + '）' : ''));

  // 3) growth
  const gr = await get('/admin/growth');
  const grAcc = (gr.json || {}).accounts || [];
  const grUids = grAcc.map(a => a.uid);
  console.log('\n/admin/growth: ' + grAcc.length + ' 条  ' + JSON.stringify(grUids.map(u => u.slice(0, 8))));

  const grOutside = grUids.filter(u => !byUid[u]);
  ok(grOutside.length === 0, 'growth 不含池外账号' +
    (grOutside.length ? '（越界: ' + grOutside.map(u => u.slice(0, 8)).join(',') + '）' : ''));

  // 4) 池里到底有没有别家账号？（决定这个测试是否有意义）
  console.log('');
  const otherProviders = Object.keys(providers).filter(p => p !== 'workbuddy');
  if (otherProviders.length === 0) {
    console.log('  ⚠ 池里**没有**别家上游账号 —— 这个测试当前**证明不了**过滤生效');
    console.log('    （没有可被错误包含的对象）。需要往池里塞一个 codearts 账号才有意义。');
    fail++;
  } else {
    console.log('  池里有别家上游: ' + otherProviders.join(', ') +
      ' —— 过滤有实际对象，测试有意义');
  }

  console.log('');
  console.log(fail === 0 ? '=== T1 修复验证通过 ===' : '=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})().catch(e => { console.log('EXCEPTION: ' + e.message); process.exit(1); });
