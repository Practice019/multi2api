// verify_register_abuse_e2e.js —— 登录流程**反滥用**侧的验收探针。
//
// # 为什么单独立一个（而不是塞进 verify_login_flow_e2e）
//
// 正路探针验证"该通的通"，本探针验证"该拦的拦"。两者的失败模式完全不同：
// 正路漏了会静默放行，反滥用漏了会**没有任何可见症状** —— 注册接口不限流，
// 页面照常工作，日志照常写，只有被人拿去刷号时才暴露。所以限流必须有一条
// 明确的尺子，而且这条尺子最容易写成"无论服务端怎样都通过"（比如把断言
// 写成"没有 429 也算过"），于是它必须带 `--self-test` 自证判别力。
//
// # 冻结的契约（本探针只量，不改）
//
//	注册/登录连打到第 N 次 → 429 {"error":{"code":"rate_limited"}}（N ≥ 2：
//	    限流不能把第一个正常请求就挡掉，那等于谁都登不进来）
//	超长 body → 413
//	PRD 里的"人机校验"：若回执出现 captcha_required:true，则**不带答案**的注册必须被拒
//
// # 三种用法
//
//	node tests/frontend/verify_register_abuse_e2e.js              # 打 WB2API_BASE（缺省 http://127.0.0.1:7885）
//	node tests/frontend/verify_register_abuse_e2e.js --start      # 探针自己起隔离空实例（限流桶干净 ⇒ "第 N 次"才确定）
//	node tests/frontend/verify_register_abuse_e2e.js --self-test  # 打本地桩，验证探针自身的判别力
//
// 两个环境变量：
//	WB2API_ABUSE_CAP   连打次数上限（默认 24）
//	WB2API_MAX_BODY    超长 body 的字节数（默认 256KB；契约没写阈值，
//	                   主脑定下 413 的阈值后把这里设成"阈值 + 1KB"）
'use strict';
const lib = require('./lib_authapi.js');

const SELFTEST_CASES = [
  { desc: '好桩 ①：注册限流 8 / 登录限流 6，其余按契约 ⇒ 探针必须全绿', expect: 'pass', stub: {} },
  { desc: '好桩 ②：注册开人机校验且真的拦住 ⇒ 探针必须全绿（注册侧那几项按实测跳过）', expect: 'pass', stub: { captcha: true } },
  { desc: '坏桩 ①：完全没有限流（注册/登录永不 429）⇒ 探针必须变红', expect: 'fail', stub: { defects: { no_rate_limit: true } } },
  { desc: '坏桩 ②：第一次请求就 429（限流把正常登录一起挡了）⇒ 探针必须变红', expect: 'fail', stub: { defects: { rate_limit_from_first: true } } },
  { desc: '坏桩 ③：超长 body 不拒（照常处理）⇒ 探针必须变红', expect: 'fail', stub: { defects: { no_413: true } } },
  { desc: '坏桩 ④：429 但 code 不是 rate_limited（前端无法据此提示）⇒ 探针必须变红', expect: 'fail', stub: { defects: { rate_limited_wrong_code: true } } },
  { desc: '坏桩 ⑤：开了人机校验却不拦无答案注册 ⇒ 探针必须变红', expect: 'fail', stub: { captcha: true, defects: { captcha_ignored: true } } },
];

async function runProbe(ctx) {
  const rep = ctx.reporter;
  const cli = lib.makeClient(ctx.base);
  const CAP = Number(process.env.WB2API_ABUSE_CAP || 24);
  const HUGE = Number(process.env.WB2API_MAX_BODY || 256 * 1024);
  const reg = (username, password) => cli.post('/admin/auth/register', { username, password });
  let captchaEnforced = false; // 人机校验开着且真的拦住 ⇒ 另外三项测不到，如实跳过
  let saw429 = false;          // 跑 ③ 之前就见过程 429 ⇒ 桶已耗尽，"第 N 次"不作判

  // ⓪ 必须最先跑：它的结论决定 ① ② ③ 能不能测。
  rep.section('⓪ 端点存在性 + 人机校验探测（必须先跑：它的结论决定后面三项能不能测）');
  const capRes = await reg(lib.freshUsername('p-abuse-cap'), 'probe-pass-' + lib.randToken(8));
  const why = lib.missingReason(capRes);
  if (why) {
    rep.ok(false, '端点可用：POST /admin/auth/register 必须按契约应答', `status=${capRes.status} ${lib.short(capRes.text, 120)}`);
    for (const line of lib.missingHint(capRes, ctx.base)) rep.info(line);
    return;
  }
  const capOn = Boolean(capRes.json && capRes.json.captcha_required === true) || /captcha/i.test(lib.errCode(capRes) || '');
  if (lib.isRateLimited(capRes)) {
    saw429 = true;
    if (ctx.fresh) {
      rep.ok(false, '★ 本次跑的第一个注册请求不该是 429（限流不能把第一个正常请求就挡掉）',
        `status=${capRes.status} body=${lib.short(capRes.text, 90)}`);
    } else {
      rep.skip('首个注册即 429：共享实例的注册桶可能已被之前的请求消耗，本项不判（--start 可得确定结论）');
    }
  } else if (!capOn) {
    rep.skip('本实例未开启注册人机校验（回执里没有 captcha_required）—— 契约允许，只记不判');
  } else {
    const rejected = capRes.status >= 400 && capRes.status < 500;
    rep.ok(rejected, '★ 人机校验已开启（captcha_required:true）时，不带答案的注册必须被拒',
      `status=${capRes.status} body=${lib.short(capRes.text, 110)}`);
    captchaEnforced = rejected;
    if (!rejected) rep.info('⇒ 回执自称在做人机校验，请求却成功了：校验只写在响应里，没落在判定上');
  }

  rep.section('① 超长 body → 413（本次用 ' + HUGE + ' 字节合法 JSON）');
  if (captchaEnforced) {
    rep.skip('注册开了人机校验：无答案的超长请求会先被校验拦下，413 本次测不到');
  } else {
    const huge = await cli.post('/admin/auth/register', lib.hugeBody(HUGE));
    if (lib.isRateLimited(huge)) {
      saw429 = true;
      rep.skip('超长 body 请求先被限流（429）—— 用 --start 起隔离空实例，或稍后复测 413');
    } else {
      rep.ok(huge.status === 413, '超长 body（' + HUGE + ' 字节）→ 413', `status=${huge.status} body=${lib.short(huge.text, 90)}`);
      if (huge.status !== 413 && huge.status !== 0) {
        rep.info('提示：413 的阈值是主脑定的，契约没写数字。把 WB2API_MAX_BODY 设成"阈值 + 1KB"再跑本项。');
      }
    }
  }

  rep.section('② 非法输入 → 400 + 精确 code');
  if (captchaEnforced) {
    rep.skip('注册开了人机校验：非法输入会先被校验拦下，400 的两条本次测不到');
  } else {
    const badU = await reg('ab', 'probe-pass-' + lib.randToken(8));
    if (lib.isRateLimited(badU)) {
      saw429 = true;
      rep.skip('注册桶已空（429）：非法输入的两条本次未测得');
    } else {
      rep.ok(badU.status === 400 && lib.errCode(badU) === 'bad_username',
        '用户名过短（2 字符）→ 400 bad_username', `status=${badU.status} code=${lib.errCode(badU)}`);
      const badP = await reg(lib.freshUsername('p-abuse-np'), 'short');
      rep.ok(badP.status === 400 && lib.errCode(badP) === 'bad_password',
        '口令过短（5 字符）→ 400 bad_password', `status=${badP.status} code=${lib.errCode(badP)}`);
    }
  }

  rep.section('③ 注册连打 ' + CAP + ' 次内必须出现 429（code=rate_limited）');
  if (captchaEnforced) {
    rep.skip('注册开了人机校验：无答案请求一律被拒，注册侧限流本次测不到（登录侧那条仍会作证）');
  } else {
    const hammer = lib.freshUsername('p-abuse-reg');
    const codes = {};
    let first429 = 0;
    for (let i = 1; i <= CAP; i += 1) {
      // 同一个名字反复注册：第 1 次建号、其后是 409 —— 都是**合法**请求，
      // 正是限流该数的对象。换成每次新名字也行，但那样会真的建出几十个账号。
      const r = await reg(hammer, 'probe-pass-' + lib.randToken(8));
      codes[r.status] = (codes[r.status] || 0) + 1;
      if (r.status === 429) {
        first429 = i;
        rep.ok(lib.errCode(r) === 'rate_limited', '★ 第 ' + i + ' 次注册请求被 429 拒绝，且响应体 code=rate_limited',
          `code=${lib.errCode(r)} body=${lib.short(r.text, 80)}`);
        break;
      }
    }
    rep.ok(first429 > 0, '注册连打 ' + CAP + ' 次内出现 429（实际状态分布 ' + lib.hist(codes) + '）',
      first429 ? '' : 'CAP 次内一次 429 都没有 —— 注册侧没有任何限流');
    if (first429 === 0) {
      rep.info('提示：本探针用"同一用户名反复注册"打，按 IP 或按用户名的限流都能命中；');
      rep.info('      一次都没 429 ⇒ 要么确实没限流，要么阈值高于 ' + CAP + '（用 WB2API_ABUSE_CAP 调大复测）。');
    }
    if (first429 > 0) {
      rep.ok(first429 >= 2 || !ctx.fresh || saw429,
        '★ 不是"第一次就拒"（第 1 次请求不该是 429' + (saw429 ? '；本次跑在 ③ 之前已把桶耗尽，故不作判' : '') + '）',
        `first429=${first429}`);
    }
  }

  rep.section('④ 登录连打 ' + CAP + ' 次内必须出现 429（code=rate_limited）');
  const lp = lib.freshUsername('p-abuse-login');
  const lcodes = {};
  let l429 = 0;
  for (let i = 1; i <= CAP; i += 1) {
    const r = await cli.login(lp, 'wrong-pass-' + lib.randToken(8));
    lcodes[r.status] = (lcodes[r.status] || 0) + 1;
    if (i === 1) {
      rep.ok(r.status === 401 && lib.errCode(r) === 'bad_credentials',
        '第一次登录（不存在的用户 + 错口令）→ 401 bad_credentials（不是 429：限流不该把正常登录一起挡掉）',
        `status=${r.status} code=${lib.errCode(r)}`
          + (r.status === 429 ? ' —— 若本实例的登录桶已被之前的请求消耗，请用 --start 起隔离空实例复测' : ''));
    }
    if (r.status === 429) {
      l429 = i;
      rep.ok(lib.errCode(r) === 'rate_limited', '★ 第 ' + i + ' 次登录请求被 429 拒绝，且响应体 code=rate_limited',
        `code=${lib.errCode(r)} body=${lib.short(r.text, 80)}`);
      break;
    }
  }
  rep.ok(l429 > 0, '登录连打 ' + CAP + ' 次内出现 429（实际状态分布 ' + lib.hist(lcodes) + '）',
    l429 ? '' : 'CAP 次内一次 429 都没有 —— 登录侧没有限流，口令可以无限喷洒');
}

lib.runCli({
  title: '注册/登录反滥用验收（E2E）',
  runProbe,
  cases: SELFTEST_CASES,
  apiKey: 'probe-key-abuse',
}).then((code) => process.exit(code));
