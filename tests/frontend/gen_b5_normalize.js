// gen_b5_normalize.js —— B5 归一化逻辑的单测（抽出真实产品函数）。
//
// # B5 的**修正后**描述
//
// 计划里我写的 B5 是「/status 没有 provider 字段 → 账号全被归到默认上游」。
// 实测发现那条**不准确**：/status 的响应里**确实有** provider 字段
// （见实测输出：uid/nickname/provider/quota/credits/...）。
//
// 所以真正的风险不是"当前一定错"，而是**形状契约没有保障**：
// 两个端点的账号形状不同（/admin/accounts 带 today_checkin、token_expire_sec；
// /status 带 quota、err_total、last_err），回落路径静默依赖"两边都有 provider"。
// 哪天 /status 少一个字段，界面会静默把号归错上游，且不会报任何错。
//
// 本测试钉住归一化行为本身：缺 provider 时要**显式标记为推断**，
// 并且不能污染调用方传入的对象。
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');

const REPO = (process.env.WB2API_REPO || __dirname + '/../..');
const html = fs.readFileSync(path.join(REPO, 'internal/server/webui.html'), 'utf8');

function grab(marker) {
  const s = html.indexOf(marker);
  if (s < 0) throw new Error('找不到: ' + marker);
  let i = html.indexOf('{', s), d = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') d++;
    else if (html[i] === '}') { d--; if (d === 0) return html.slice(s, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}

const src = [
  grab('function normalizeAccount('),
  grab('function providerOf('),
].join('\n');

const header = `
let DEFAULT_PROVIDER = '';
let PROVIDERS = [];
`;

const tail = String.raw`
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
`;

fs.writeFileSync(path.join(__dirname, 'b5_normalize_gen.js'), header + 'const SRC = ' + JSON.stringify(src) + ';\n' + tail);
console.log('生成: b5_normalize_gen.js');
