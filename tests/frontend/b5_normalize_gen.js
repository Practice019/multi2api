
let DEFAULT_PROVIDER = '';
let PROVIDERS = [];
const SRC = "function normalizeAccount(a) {\r\n    if (!a || typeof a !== 'object') return null;\r\n    const out = Object.assign({}, a);\r\n    // 缺 provider：交给 providerOf 走默认上游，但记下\"这是推断来的\"，\r\n    // 供表格显示「上游?」并在分组标题里说明。\r\n    if (!out.provider) out.provider_inferred = true;\r\n    return out;\r\n  }\nfunction providerOf(a) {\r\n    if (!a || typeof a !== 'object') return '';\r\n    return a.provider || DEFAULT_PROVIDER;\r\n  }";

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const M = new Function('DEFAULT_PROVIDER', 'PROVIDERS',
  'return (function(){' + SRC + '; return { normalizeAccount, providerOf };})()')('', []);

console.log('[1] 缺 provider 的账号（回落路径的可能形状）');
const a = M.normalizeAccount({ uid: 'u1', nickname: 'x', credits: 5 });
ok(a.provider_inferred === true, '被标为 provider_inferred（界面据此显示「?」提示）');
ok(M.providerOf(a) === '', '无默认上游时归属为空串（不崩）');

console.log('\n[2] 有 provider 的账号不受影响');
const b = M.normalizeAccount({ uid: 'u2', provider: 'codearts', credits: 1 });
ok(!b.provider_inferred, '不被标为推断');
ok(M.providerOf(b) === 'codearts', '归属保持 codearts');

console.log('\n[3] 畸形输入');
ok(M.normalizeAccount(null) === null, 'null 被过滤（返回 null，由 renderAccounts 滤掉）');
ok(M.normalizeAccount(undefined) === null, 'undefined 被过滤');
ok(M.normalizeAccount('x') === null, '字符串被过滤');
ok(M.normalizeAccount(42) === null, '数字被过滤');

console.log('\n[4] 不污染调用方数据');
const orig = { uid: 'u3' };
M.normalizeAccount(orig);
ok(orig.provider_inferred === undefined, '不修改传入对象（renderAccounts 的入参可能被别处复用）');

console.log('\n[5] 有默认上游时的归属');
const M2 = new Function('DEFAULT_PROVIDER', 'PROVIDERS',
  'return (function(){' + SRC + '; return { normalizeAccount, providerOf };})()')('workbuddy', []);
const c = M2.normalizeAccount({ uid: 'u4' });
ok(c.provider_inferred === true, '仍然标记为推断');
ok(M2.providerOf(c) === 'workbuddy', '归属回落到默认上游（与后端 pool 对未打标签账号的解释一致）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
