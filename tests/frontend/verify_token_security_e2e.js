// verify_token_security_e2e.js —— 令牌相关三条：错误令牌能不能进 / 轮换后会不会把人锁在外面。
//
// # 用户报的三条（2026-10-19，附 API 管理页截图）
//
//	① config.example.json **没有写出 admin_token** —— 用户不知道有这个字段
//	② 点「轮换」（管理令牌 admin_token / 对话密钥 api_key）之后网页
//	   **admin token 加载失败、必须重启**，他希望轮换完页面照常可用
//	③ **admin token 输入错误居然可以进入 UI 页面** ⇒ api_key 泄露
//
// 本文件先量**后端**（HTTP）：谁在什么令牌下能拿到什么。第 ③ 条还有一半在
// 前端（页面拿本地缓存渲染密钥）—— 那部分由 verify_token_gate_ui_e2e.js 量。
//
// # 全合成：临时目录起实例，令牌是当场编的
'use strict';
const { startInstance, httpJson, sleep } = require('./lib_instance.js');

const PORT = Number(process.env.TOKEN_PORT || 7883);
const T1 = 'probe-admin-token-1111111111';
const K1 = 'probe-api-key-1111111111';
const WRONG = 'definitely-not-the-token';

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
const info = (m) => console.log('  · ' + m);
const short = (s, n) => (s === undefined || s === null ? '' : String(s).slice(0, n || 60));

// 探针要看的端点清单（`secret: true` = 回执里含密钥或密钥线索，未授权看到即泄露）
const ENDPOINTS = [
  { p: '/healthz', secret: false, what: '健康检查（设计上公开）' },
  { p: '/status', secret: false, what: '状态（账号视图）' },
  { p: '/admin/settings', secret: true, what: '设置（能改上游配置，管理员专属）' },
  { p: '/admin/apikeys', secret: true, what: 'API Key 列表（回执带 admin_key / admin_token 的掩码）' },
  { p: '/admin/accounts', secret: false, what: '账号池' },
  { p: '/v1/models', secret: false, what: '模型列表（/v1 凭据）' },
];

// prob 打一遍端点清单，返回 [ {p, status, leaked} ]
async function prob(base, label, token) {
  const rows = [];
  for (const e of ENDPOINTS) {
    const r = await httpJson(base, e.p, token ? { token } : {});
    // 泄露判据：回执里**真的出现**了这两把合成令牌的任何一把
    const leaked = [T1, K1].some((s) => r.text && r.text.includes(s));
    rows.push({ p: e.p, what: e.what, status: r.status, leaked, secret: e.secret, body: r.text });
  }
  info('---- ' + label + ' ----');
  rows.forEach((r) => info('   ' + String(r.status).padEnd(4) + r.p.padEnd(18) + (r.leaked ? '  ⚠ 回执里含令牌' : '') + (r.status === 200 ? '' : '  ' + short(r.body, 70))));
  return rows;
}

(async () => {
  let inst = null;
  try {
    inst = await startInstance({
      port: PORT,
      apiKey: K1,
      config: { admin_token: T1 },   // 与 api_key 故意不同：才能分辨是哪一把漏了
      auths: {},
    });
    info('实例 ' + inst.base + '（pid=' + inst.pid + '）· admin_token=' + short(T1, 12) + '… api_key=' + short(K1, 12) + '…');

    console.log('\n=== ① 静态页与「不带令牌」 ===');
    const ui = await httpJson(inst.base, '/ui', {});
    info('/ui 无令牌 → ' + ui.status + '（' + ui.text.length + ' 字节 HTML）');
    ok(ui.status === 200, '/ui 页面本身可以无令牌加载（它是静态外壳；判据在下面的数据端点）');
    ok(![T1, K1].some((s) => ui.text.includes(s)), '★ /ui 的 HTML 里**不含**任何令牌（否则打开页面就泄露）');

    const noTok = await prob(inst.base, '无令牌', '');
    const noTokSecret = noTok.filter((r) => r.leaked);
    ok(noTokSecret.length === 0, '★ 不带令牌时拿不到任何密钥' + (noTokSecret.length ? '：' + noTokSecret.map((r) => r.p).join(',') : ''));
    const noTok200Secret = noTok.filter((r) => r.secret && r.status === 200);
    ok(noTok200Secret.length === 0, '★ 带密钥的端点（' + ENDPOINTS.filter((e) => e.secret).map((e) => e.p).join(' / ') + '）无令牌必须拒绝' +
      (noTok200Secret.length ? '，实际 200：' + noTok200Secret.map((r) => r.p).join(',') : ''));

    console.log('\n=== ② 错误令牌 ===');
    const bad = await prob(inst.base, '错误令牌 ' + short(WRONG, 18), WRONG);
    const badLeak = bad.filter((r) => r.leaked);
    ok(badLeak.length === 0, '★ 错误令牌**拿不到密钥**（第③条 bug 的核心判据）' +
      (badLeak.length ? '：泄露于 ' + badLeak.map((r) => r.p).join(',') : ''));
    const bad200Secret = bad.filter((r) => r.secret && r.status === 200);
    ok(bad200Secret.length === 0, '★ 错误令牌访问带密钥端点必须 401/403，不能 200' +
      (bad200Secret.length ? '，实际 200：' + bad200Secret.map((r) => r.p).join(',') : ''));

    console.log('\n=== ③ 正确令牌（对照组，必须能拿到自己的密钥线索）===');
    const good = await prob(inst.base, '正确令牌', T1);
    ok(good.find((r) => r.p === '/admin/settings').status === 200, '正确令牌能读 /admin/settings');

    // ⚠ 这条判据改过：原来断言的是 `/admin/settings` 的回执里出现**完整** admin_key。
    //  两处都不成立：① settingsView() 从不含密钥字段（掩码在 /admin/apikeys 的
    //  apiKeysList 里，见 internal/admin/apikeys.go:71-72）；② 那里回的是**掩码**
    //  （mk-1d4e…bb27），`text.includes(完整令牌)` 恒为假 —— 那条断言从来没有
    //  通过过，它红不是因为"权限不对"，而是因为**判据写错了对象**。
    //  现在按实际来源判：管理员读 /admin/apikeys 应 200，且回执里有 admin_key /
    //  admin_token 两个掩码字段（这是控制台的正当能力），而**完整**密钥不出现。
    const keys = await httpJson(inst.base, '/admin/apikeys', { token: T1 });
    ok(keys.status === 200, '正确令牌能读 /admin/apikeys（' + keys.status + '）');
    const kb = keys.text || '';
    ok(/"admin_key"\s*:/.test(kb) && /"admin_token"\s*:/.test(kb),
      '回执里有 admin_key 与 admin_token 两个掩码字段（控制台要靠它显示"当前用的是哪把钥匙"）');
    ok(kb.includes(T1.slice(0, 6)), '掩码保留了可辨认的前缀（' + T1.slice(0, 6) + '…）');
    ok(!kb.includes(T1) && !kb.includes(K1), '★ 回执里没有**完整**密钥（掩码而非回显）');

    console.log('\n=== ④ 轮换管理令牌 admin_token（用户第②条）===');
    const rot = await httpJson(inst.base, '/admin/apikeys/rotate-admin', { method: 'POST', token: T1, body: {} });
    info('rotate-admin → ' + rot.status + '  ' + short(rot.text, 120));
    ok(rot.status === 200, '轮换请求成功');
    const T2 = rot.json && (rot.json.full || rot.json.token || rot.json.admin_token);
    ok(!!T2, '回执里有完整新令牌（前端要靠它就地换掉手里的那份）');
    if (T2) {
      const t2 = await httpJson(inst.base, '/admin/settings', { token: T2 });
      ok(t2.status === 200, '★ 新令牌立即可用（无需重启）');
      const t1 = await httpJson(inst.base, '/admin/settings', { token: T1 });
      ok(t1.status !== 200, '旧令牌立即失效（回 ' + t1.status + '）');
      const none = await httpJson(inst.base, '/admin/settings', {});
      info('轮换后**不带令牌**读 /admin/settings → ' + none.status);
    }

    console.log('\n=== ⑤ 轮换对话密钥 api_key（用户第②条的另一半）===');
    const rot2 = await httpJson(inst.base, '/admin/apikeys/rotate-api-key', { method: 'POST', token: T2 || T1, body: {} });
    info('rotate-api-key → ' + rot2.status + '  ' + short(rot2.text, 120));
    ok(rot2.status === 200, '轮换对话密钥成功');
    const K2 = rot2.json && (rot2.json.full || rot2.json.api_key || rot2.json.key);
    ok(!!K2, '回执里有完整新密钥');
    if (K2) {
      const old = await httpJson(inst.base, '/v1/models', { token: K1 });
      ok(old.status !== 200, '旧对话密钥立即失效（回 ' + old.status + '）—— 所以前端刷新 /v1/models 会 401');
      const nw = await httpJson(inst.base, '/v1/models', { token: K2 });
      ok(nw.status === 200, '新对话密钥可用');
    }

    console.log('\n=== ⑥ config.example.json 是否写出这两个字段（用户第①条）===');
    const fs = require('fs');
    const path = require('path');
    const { REPO } = require('./lib_instance.js');
    const ex = fs.readFileSync(path.join(REPO, 'config.example.json'), 'utf8');
    // ⚠ 不 JSON.parse：config.example.json 是 **JSONC**（带 // 注释，见 internal/…/jsonc.go），
    // 直接 parse 会在第 2 行就抛 SyntaxError —— 那是"解析方式错了"，不是"字段缺失"。
    // 这里按**键行**判存在性（行首即键名，注释里的同名字符串不算）。
    const hasKey = (k) => new RegExp('^\\s*"' + k + '"\\s*:', 'm').test(ex);
    ok(hasKey('admin_token'), 'config.example.json 写出了 admin_token');
    ok(hasKey('api_key'), 'config.example.json 写出了 api_key');
  } catch (e) {
    console.log('  探针异常: ' + (e && e.stack ? e.stack : e));
    fail++;
  } finally {
    if (inst) inst.stop();
  }
  console.log('\n' + (fail ? '结果: ' + fail + ' 条 FAIL' : '结果: 全绿'));
  process.exit(fail ? 1 : 0);
})();
