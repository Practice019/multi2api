// verify_key_alias_models.js —— 一把 key 配了多条**同裸名**的对外名时，/v1/models 必须一条不漏。
//
// # 用户报的现象（2026-10-21）
//   给 key 加了 workbuddy/deepseek-v4.1-flash，对外公布成 006/deepseek-v4.1-flash，
//   但客户端拿到的模型列表里没有 006/…（配了 8 条，实际只回 7 条）
//
// # 为什么用真实例测
//   这个 bug 只在「filter 已改名 → applyPublicNames 又改名」这条**两遍改名**的路径上出现，
//   纯单元测试能证明函数幂等，但证明不了 /v1/models 的组装顺序没再变。所以真起一个实例、
//   真建一把 key、真请求一次。
'use strict';
const path = require('path');
const { startInstance } = require('./lib_instance.js');
const PORT = Number(process.env.ALIAS_PORT || 7901);
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  const inst = await startInstance({ port: PORT, apiKey: 'probe-admin-key' });
  try {
    const H = { 'Content-Type': 'application/json', Authorization: 'Bearer probe-admin-key' };
    const reg = await fetch(inst.base + '/admin/auth/register', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: 'aliasadm', password: 'alias2026' }) });
    ok(reg.status === 200, '建管理员 status=' + reg.status);
    // 三条同裸名的内部模型，各配一个对外名（正是用户的形态）
    const models = ['qiniuyun/deepseek-v4.1-flash', 'senseaudio/deepseek-v4.1-flash', 'workbuddy/deepseek-v4.1-flash'];
    const modelMap = {
      '004/deepseek-v4.1-flash': 'qiniuyun/deepseek-v4.1-flash',
      '005/deepseek-v4.1-flash': 'senseaudio/deepseek-v4.1-flash',
      '006/deepseek-v4.1-flash': 'workbuddy/deepseek-v4.1-flash',
    };
    const cr = await fetch(inst.base + '/admin/apikeys', { method: 'POST', headers: H, body: JSON.stringify({ name: 'alias', models, model_map: modelMap }) });
    const cj = await cr.json();
    // 创建端点把明文放在 full 字段（列表端点只给掩码 —— 明文只在"刚建好"这一刻可见）
    const raw = cj.full || cj.key || cj.token || (cj.data && (cj.data.full || cj.data.key));
    ok(!!raw, '建 key 并拿到明文（status=' + cr.status + '）');
    // 上游目录里没有这些模型 ⇒ 用 channel 声明把它们挂进来（/v1/models 会并入渠道声明的模型）
    await fetch(inst.base + '/admin/channels', { method: 'POST', headers: H, body: JSON.stringify({
      name: 'alias-ch', base_url: 'https://example.invalid/v1', keys: ['sk-alias-1'],
      models, model_mapping: {}, status: 1,
    }) });
    const r = await fetch(inst.base + '/v1/models', { headers: { Authorization: 'Bearer ' + raw } });
    const j = await r.json();
    const ids = (j.data || []).map(m => m.id);
    ok(r.status === 200, '/v1/models status=' + r.status);
    for (const want of ['004/deepseek-v4.1-flash', '005/deepseek-v4.1-flash', '006/deepseek-v4.1-flash']) {
      ok(ids.indexOf(want) >= 0, '列表里有 ' + want + '（实际 ' + JSON.stringify(ids) + '）');
    }
    ok(ids.length === 3, '只剩这 3 条（不该多也不该少，实际 ' + ids.length + '）');
    // 用**对外名**调用必须通过白名单（上游会失败，但不能是 403 model_not_allowed）
    const call = await fetch(inst.base + '/v1/chat/completions', {
      method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + raw },
      body: JSON.stringify({ model: '006/deepseek-v4.1-flash', messages: [{ role: 'user', content: 'hi' }] }),
    });
    const cbody = await call.text();
    ok(!(call.status === 403 && cbody.indexOf('model_not_allowed') >= 0),
      '用对外名 006/… 调用不被白名单拒（status=' + call.status + '）');
  } finally { inst.stop(); }
  console.log(fail === 0 ? '\n=== 对外名模型列表全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exitCode = fail ? 1 : 0;
})().catch(e => { console.error('EX ' + (e && e.message)); process.exitCode = 2; });