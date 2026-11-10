// verify_tokens_expiry_e2e.js —— 「到期即失效」的**真请求**门禁。
//
// # 为什么必须有它（静态断言在这里没有判别力）
//
// 本项目自用：令牌不配额度，**只有到期时间**这一个生命周期控制点。所以
// 「过期了就一定不能用」是这个功能存在的全部意义 —— 而它是一条跨越三层的链路：
//
//   管理端写入 expired_time  →  鉴权索引同步  →  /v1 请求被拒
//
// 任何一层断掉，页面上看起来都完全正常（列表照旧、字段照旧），只有真发一次请求才暴露。
// 本探针就发那一次请求，并且**两个方向都验**：过去 ⇒ 拒；改到未来 ⇒ 立刻恢复。
//
// # 验收口径
//   ① 过期令牌打 /v1/models ⇒ 非 200（401/403）
//   ② 同一把 key 把到期时间改到未来 ⇒ 200（证明拒绝来自到期判定，不是 key 本身失效）
//   ③ 再改回过去 ⇒ 立刻又拒（证明没有把上一次的放行缓存住）
//   ④ 列表/详情下发派生字段 expired:true（前端不必自己算时钟）
//   ⑤ 启用一条已过期令牌 ⇒ 400（界面不给假按钮）
//   ⑥ 更新不存在的 id ⇒ 404（不是 500/panic）
//
// 单跑：node tests/frontend/verify_tokens_expiry_e2e.js（0=全绿 / 1=有红）
'use strict';
// ⚠ 必须在 require(lib_instance) **之前**设 —— 模块求值时就把 EXE 定死了。
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
const net = require('net');
const { startInstance } = require('./lib_instance.js');

function freePort() { return new Promise((res, rej) => { const s = net.createServer(); s.on('error', rej); s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); }); }); }

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
  const PORT = await freePort();
  const ADMIN = 'probe-key-expiry-e2e';
  console.log('实例端口（现场探测）: ' + PORT + '（绝不是 7863）');
  const inst = await startInstance({ port: PORT, apiKey: ADMIN });
  const base = inst.base;
  const hdr = { Authorization: 'Bearer ' + ADMIN, 'Content-Type': 'application/json' };
  const now = Math.floor(Date.now() / 1000);
  // callModels 用**令牌自己的 key** 打 /v1/models（不是管理凭据）—— 这才是用户实际会遇到的路径。
  const callModels = async (tokenKey) => {
    const r = await fetch(base + '/v1/models', { headers: { Authorization: 'Bearer ' + tokenKey } });
    return r.status;
  };
  try {
    // ── 夹具：一条已过期令牌（unlimited_quota 是唯一被支持的额度形态；到期时间在过去）
    const rc = await fetch(base + '/admin/tokens', { method: 'POST', headers: hdr, body: JSON.stringify({
      name: 'probe-expired', count: 1, unlimited_quota: true, automatic: true,
      auto_group_mode: 'inherit', auto_groups: [], groups: [], expired_time: now - 3600,
    }) });
    const cj = await rc.json();
    ok(rc.status === 200 && cj.items && cj.items.length === 1, '夹具：建出一条已过期令牌（HTTP ' + rc.status + '）');
    const id = cj.items[0].id;
    ok(cj.items[0].expired === true, '创建响应里就带派生字段 expired:true（前端不靠自己算时钟）');

    const rd = await fetch(base + '/admin/tokens/' + id, { headers: hdr });
    const dj = await rd.json();
    ok(rd.status === 200 && dj.expired === true, '详情端点同样报 expired:true（列表与详情同源）');

    const rk = await fetch(base + '/admin/tokens/' + id + '/key', { method: 'POST', headers: hdr });
    const kj = await rk.json();
    ok(rk.status === 200 && typeof kj.key === 'string' && kj.key.length > 0, '取到明文 key 用于打 /v1');
    const tokenKey = kj.key;

    // ── ① 过期 ⇒ 必须非 200
    const rejected = await callModels(tokenKey);
    ok(rejected !== 200, '① 过期令牌打 /v1/models 被拒（实际 ' + rejected + '；200 = 到期根本没生效）');
    ok(rejected === 401 || rejected === 403, '① 拒绝码是 401/403（实际 ' + rejected + '；5xx 说明是崩了而不是判过期）');

    // ── ⑤ 启用已过期令牌必须被拒（界面不给假按钮）
    const rEn = await fetch(base + '/admin/tokens/update', { method: 'POST', headers: hdr, body: JSON.stringify({ id: id, status_only: true, status: 1 }) });
    const ej = await rEn.json();
    ok(rEn.status === 400, '⑤ 启用已过期令牌 ⇒ 400（实际 ' + rEn.status + '）');
    ok(rEn.status === 400 && /已过期/.test(JSON.stringify(ej)), '⑤ 错误文案点明「已过期」（实际 ' + JSON.stringify(ej).slice(0, 120) + '）');

    // ── ② 改到未来 ⇒ 立刻恢复
    const rUp = await fetch(base + '/admin/tokens/update', { method: 'POST', headers: hdr, body: JSON.stringify({ id: id, expired_time: now + 3600 }) });
    ok(rUp.status === 200, '② 把到期时间改到未来 ⇒ 200（实际 ' + rUp.status + '）');
    const restored = await callModels(tokenKey);
    ok(restored === 200, '② 同一把 key 立刻可用（实际 ' + restored + '；证明拒绝来自到期判定，不是 key 本身不可用）');

    // ── ③ 再改回过去 ⇒ 立刻又拒（证伪「放行被缓存」）
    const rBack = await fetch(base + '/admin/tokens/update', { method: 'POST', headers: hdr, body: JSON.stringify({ id: id, expired_time: now - 60 }) });
    ok(rBack.status === 200, '③ 再把到期时间推回过去 ⇒ 200（实际 ' + rBack.status + '）');
    const retry = await callModels(tokenKey);
    ok(retry !== 200, '③ 同一把 key 立刻又被拒（实际 ' + retry + '；200 = 上一次的放行被缓存住了）');

    // ── ⑥ 更新不存在的 id ⇒ 404（不得 500/panic）
    const rNF = await fetch(base + '/admin/tokens/update', { method: 'POST', headers: hdr, body: JSON.stringify({ id: 999999, name: '不存在' }) });
    ok(rNF.status === 404, '⑥ 更新不存在的 id ⇒ 404（实际 ' + rNF.status + '；500 = 又回到了空指针那条路）');
  } catch (e) {
    ok(false, '探针自身抛异常：' + (e && e.message ? e.message : String(e)));
  } finally {
    await inst.stop();
  }
  console.log(fail === 0 ? '\n=== 令牌到期语义（真请求）验收通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail === 0 ? 0 : 1);
})();
