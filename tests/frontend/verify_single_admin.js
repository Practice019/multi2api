// verify_single_admin.js —— 「单账号管理员」口径的**接受判据**（2026-10-19 19:17 用户新口径）。
//
// # 用户原话与由此确定的目标态
//
//	「我们那个用户和管理员，我觉得可以只要一个管理员就行了，只要一个账号。
//	  用户其实可以不用。」
//
// ⇒ 只有管理员、只有一个账号、**用户/成员那一套不再存在**。
//   注册只在"首次"（bootstrap，账号库为空时创建**那一个**管理员）；
//   库非空后注册关闭；盘上的历史成员记录不许当管理员放进来；成员面全部消失。
//
// # 为什么是这个形状（与 `verify_login_flow_e2e.js` 同一套骨架）
//
// 六个判据**全部是"响应字节级"的判据**，不看"看起来对不对"：
//   1. 空库 + `POST /admin/auth/register` ⇒ 建出来的**必须是管理员**（回执 `bootstrap:true`）
//   2. 库非空后再注册 ⇒ **被拒**（状态码 + 文案）
//      ⚠ 这条是**判别力来源**：改前这里会 200 并建出成员
//   3. 盘上预置一条非管理员历史记录 ⇒ 登录**被拒**且服务**不崩**（后续请求仍通）
//   4. 未认证 `/ui` = 登录页，且**没有**注册入口（"注册成员"/模式切换那一段）
//   5. 管理员会话 `/ui` = 控制台外壳，且**没有**成员模式残留
//   6. `/admin/apikeys` 载荷**没有** `member` 字段，且列表可用（不许用"载荷为空"冒充通过）
//
// ## 三条纪律（本仓反复付过学费的）
//
// ① **静态判据先剥注释**：判据 4/5 要在 HTML 里找"没有某个词"。
//    本仓有前车之鉴（`2e1aecb`）：剥注释顺序错 ⇒ 判据在那段空转 ⇒ 假绿。
//    所以 `stripComments()` 先剥块注释再剥行注释（反了会把 `// 注释里的 /*`
//    与远处的 `*/` 配对，吃掉一大段），且判据只在**剥完之后**的文本上做。
//    ⚠ 特别地：`/admin/auth/register` 这个**路径**必须留着（它是 bootstrap 的受理端点，
//      判据 1/2 正要打它）⇒ 判据是"没有**注册入口**"，不是"没有 register 字符串"。
// ② **token 与字段缺席都要能区分**：判据 6 用 `/"member"\s*:/` 找字段，
//    而不是找 `member` 这个词（响应里可能有别处提到它）。
// ③ **每条都有坏桩**：`--self-test` 逐条把桩弄坏，断言"探针必须变红"。
//    没有这一步，"它是不是永远绿"没人知道。
//
// # 三种用法
//
//	node verify_single_admin.js                      # 打 WB2API_BASE（默认 127.0.0.1:7885）
//	node verify_single_admin.js --start              # 自起一个**空账号库**的隔离实例
//	node verify_single_admin.js --base http://...    # 指定 base
//	node verify_single_admin.js --self-test          # 打本地桩，验探针自身有判别力（好桩绿/坏桩红）
//
// ⚠ `--self-test` 打的是**本地桩**，不依赖 Go 编译；`--start` 需要能跑的 exe。
'use strict';
const lib = require('./lib_authapi.js');

// ---------------------------------------------------------------------------
// 剥注释：静态判据必须先做这一步
// ---------------------------------------------------------------------------
//
// 顺序**必须**是先块注释再行注释 —— 反了会把 `// 注释里的 /*` 与远处的 `*/` 配对。
// 这是本仓真实事故（`2e1aecb`：吃掉 2801 字符，含绑定函数 ⇒ 判据空转 ⇒ 假绿）。
function stripComments(html) {
  return String(html)
    .replace(/<!--[\s\S]*?-->/g, '')      // HTML 注释
    .replace(/\/\*[\s\S]*?\*\//g, '')     // 块注释
    .split('\n').filter((l) => !/^\s*\/\//.test(l)).join('\n');  // 整行行注释
}

// 登录页的"注册入口"标记。**只列用户看得见的东西**（文案/元素 id），
// 不列 `/admin/auth/register`（那是 bootstrap 的受理端点，必须留着）。
const REGISTER_ENTRY_MARKERS = [
  ['switchRow', '注册⇄登录 的模式切换块'],
  ['switchLink', '注册⇄登录 的切换按钮'],
  ['注册成员', '「没有账号，注册成员」文案'],
  ['注册成员账号', '注册页标题'],
  ['__WB2API_LOGIN_REGISTER__', '服务端下发的 register 标志占位符'],
];

// 控制台外壳的"成员面"残留标记。
const MEMBER_FACE_MARKERS = [
  ['applyMemberMode', '成员模式切换函数'],
  ['member-mode', '成员模式根类（收起面板的 CSS 钩子）'],
  ['memberBanner', '成员模式提示元素'],
  ['成员模式', '成员模式相关文案'],
  ['账号池读数仅管理员可见', '成员降级说明条的原话'],
];

// 自检用的两个预置账号。
//
// ⚠ 为什么需要"空库"与"非空库"**两种基线**：
//   判据 1（bootstrap 建出管理员）只有**库空**时才可能观察；
//   判据 3（退役成员被拒）只有盘上**真有**那条记录时才可能观察。
//   真实世界里一个实例不可能同时是两者（`--start` 给的是空库，
//   用户盘上如果有老记录就是非空库）⇒ 探针必须**按库的状态分支**，
//   并**如实 SKIP**它观察不到的那一条（静默跳过等于没有守卫，所以要说出来）。
const ADMIN_SEED = { username: 'probe-seed-admin', password: 'probe-seed-admin-pw-0001', role: 10 };
const LEGACY_SEED = { username: 'legacy-member', password: 'legacy-pass-0001', role: 1 };

// ---------------------------------------------------------------------------
// 探针主体
// ---------------------------------------------------------------------------
async function runProbe(ctx) {
  const { base, reporter: rep } = ctx;

  // ---- 前置：判断账号库是空还是非空（决定哪几条判据可观察）----
  //
  // 判据用的是**行为**而不是"直接读盘"：能拿预置管理员登录 ⇒ 库非空。
  // 这样同一个探针既能打真实实例（`--start` 空库 / 用户非空库），也能打桩。
  const seedClient = lib.makeClient(base);
  const pre = await seedClient.login(ADMIN_SEED.username, ADMIN_SEED.password);
  const nonEmpty = pre.status === 200;
  rep.info('前置：账号库状态 = ' + (nonEmpty ? '非空（预置管理员可直接登录）' : '空（走 bootstrap 引导）'));

  let adminClient = nonEmpty ? seedClient : null;

  rep.section('判据 1：空库 + register ⇒ 建出来的必须是**管理员**（bootstrap）');
  if (nonEmpty) {
    // ⚠ 不假装测过：库非空时 bootstrap 入口**本来就该关闭**（判据 2 正测这个）。
    rep.skip('本次账号库非空 ⇒ bootstrap 入口按设计不开放。'
      + '判据 1 的可观察形态是"空库实例"（`node verify_single_admin.js --start` 起的就是空库）。');
  } else {
    const c = lib.makeClient(base);
    const u = lib.freshUsername('probe-admin');
    const pw = lib.randToken(20);
    const body = ctx.captchaParam
      ? { username: u, password: pw, captcha_param: ctx.captchaParam }
      : { username: u, password: pw };
    const res = await c.post('/admin/auth/register', body);
    const j = res.json || {};
    const role = j.user && j.user.role;
    rep.ok(res.status === 200, '空库首次注册被受理（status=200）', 'status=' + res.status);
    rep.ok(j.bootstrap === true, '回执带 bootstrap:true —— "这是首次引导"必须能被判出来',
      'bootstrap=' + JSON.stringify(j.bootstrap));
    rep.ok(role === 10, '**建出来的角色是管理员（role=10）** —— 不是"验证一下能不能登录"那种间接判据',
      'role=' + JSON.stringify(role) + ' role_name=' + JSON.stringify(j.user && j.user.role_name));
    rep.ok(!!(j.user && j.user.enabled !== false), '建出来的账号是启用态', JSON.stringify(j.user && j.user.enabled));

    if (res.status === 200) {
      // 拿这**一个**账号登录 —— 顺带证明"建出来的就是能用的管理员"。
      const c2 = lib.makeClient(base);
      const login = await c2.login(u, pw);
      rep.ok(login.status === 200, '用刚建的管理员登录成功（status=200）', 'status=' + login.status);
      rep.ok(lib.findCookie(login.setCookie, lib.COOKIE) !== null,
        '登录回执下发了会话 cookie（' + lib.COOKIE + '）',
        JSON.stringify(lib.parseSetCookie(login.setCookie).map((x) => x.name)));
      if (login.status === 200) adminClient = c2;
    }
  }

  rep.section('判据 2：库非空后再注册 ⇒ **被拒**（这条是判别力来源）');
  {
    const c = lib.makeClient(base);
    const u2 = lib.freshUsername('probe-second');
    const res = await c.post('/admin/auth/register', { username: u2, password: lib.randToken(20) });
    rep.ok(res.status !== 200, '**库非空时注册不被受理**（改前这里会 200 并建出成员）',
      'status=' + res.status + ' code=' + JSON.stringify(lib.errCode(res)));
    rep.ok(res.status === 403 || res.status === 409 || res.status === 400,
      '给的是**明确的**状态码（403/409/400 之一，不是 500）', 'status=' + res.status);
    const code = lib.errCode(res) || '';
    rep.ok(!!code, '回执里有机器可读的 code（前端据此选文案）', 'code=' + JSON.stringify(code));
    // ⚠ 反证：被拒之后**不能**多出账号 —— 状态码对了但账号建出来了也是失败。
    const c2 = lib.makeClient(base);
    const bad = await c2.login(u2, 'wrong-password-0000');
    rep.ok(bad.status !== 200, '被拒的那个用户名**没有**被建出来（拿它登录仍失败）',
      'status=' + bad.status);
  }

  rep.section('判据 3：盘上的**非管理员历史记录** ⇒ 登录被拒且服务不崩');
  {
    // ⚠ 先确认"盘上真的存在那条记录"，否则这条判据会退化成
    //   "拿一个不存在的用户名登录当然失败"——**空洞的绿**。
    //   判据形状：先拿**正确口令**打一次；若账号根本不存在（401 bad_credentials）
    //   且它本应是退役成员（403 member_retired），说明没预置 ⇒ 如实 SKIP。
    const lu = LEGACY_SEED;
    const c = lib.makeClient(base);
    const res = await c.login(lu.username, lu.password);
    const code = lib.errCode(res) || '';
    if (res.status === 401 && code === 'bad_credentials') {
      rep.skip('盘上没有预置的退役成员记录（拿正确口令也回 401 bad_credentials）⇒ '
        + '判据 3 的可观察形态是"预置一条 role=非管理员 的历史记录"，'
        + '`--self-test` 的基线与坏桩③ 已覆盖它。不在这里假装测过。');
    } else {
      rep.ok(res.status !== 200, '**退役成员账号登录被拒**（不许当管理员放进来）',
        'status=' + res.status + ' code=' + JSON.stringify(code));
      rep.ok(lib.findCookie(res.setCookie, lib.COOKIE) === null, '被拒时**没有**下发会话 cookie',
        JSON.stringify(lib.parseSetCookie(res.setCookie).map((x) => x.name)));
      rep.ok(!(res.json && res.json.user), '被拒时**不回账号对象**（连 enabled 都不给）',
        JSON.stringify(res.json && (res.json.user || null)));
      // ⚠ 服务不许崩：拿一个**已知正常**的请求验证服务还活着。
      const alive = await lib.request(base, '/healthz', {});
      rep.ok(alive.status === 200, '被拒之后服务仍然健康（不崩）', 'status=' + alive.status);
      // ⚠ 顺序判据：**先验口令、再看角色** —— 反了就是**用户名枚举口子**。
      //
      // 判据形状要写对，我第一次写错了：我原来断言"（对退役账号）口令错与口令对
      // 回同一个状态码" —— 那是**错的**。口令对但账号已退役，**必须**能与
      // "口令错"分开，否则用户永远得不到"成员账号已取消，请用管理员账号"这句话。
      //
      // 真正的枚举面是**另一个方向**：攻击者在**猜不到口令**的前提下，
      // 不该能区分"这个用户名存在"与"不存在"。所以对照组是
      // **口令错 + 存在的退役账号** vs **口令错 + 不存在的用户名**：
      // 两者的状态码与 code **必须逐字节一致**。
      const wrongExisting = await lib.makeClient(base).login(lu.username, 'definitely-wrong-pw');
      const wrongAbsent = await lib.makeClient(base).login('no-such-user-' + lib.randToken(6), 'definitely-wrong-pw');
      rep.ok(wrongExisting.status === wrongAbsent.status,
        '口令错时：**存在的退役账号**与**不存在的用户名**回同一个状态码 —— 否则可枚举用户名',
        'existing=' + wrongExisting.status + ' absent=' + wrongAbsent.status);
      rep.ok((lib.errCode(wrongExisting) || '') === (lib.errCode(wrongAbsent) || ''),
        '口令错时两条路径的 code 也相同（口径一致）',
        'existing=' + JSON.stringify(lib.errCode(wrongExisting) || '')
        + ' absent=' + JSON.stringify(lib.errCode(wrongAbsent) || ''));
      rep.ok(wrongExisting.text === wrongAbsent.text,
        '口令错时两条路径的**响应体逐字节相同**（连文案都不许泄露用户名是否存在）',
        'len ' + String(wrongExisting.text).length + ' vs ' + String(wrongAbsent.text).length);
      // 反向确认：口令**对**但账号退役 ⇒ 必须能与"口令错"分开（否则用户拿不到指路文案）。
      rep.ok(res.status !== wrongExisting.status || code !== (lib.errCode(wrongExisting) || ''),
        '口令对但账号已退役 ⇒ 与"口令错"**可区分**（否则用户得不到"请用管理员账号"这句话）',
        'retired=' + res.status + '/' + code + ' wrongpw=' + wrongExisting.status + '/' + (lib.errCode(wrongExisting) || ''));
    }
  }

  rep.section('判据 4：未认证 /ui = 登录页，且**没有**注册入口');
  {
    // 裸请求：一个 cookie 都不带（走 lib.request，不走 cookie 罐）
    const anon = await lib.request(base, '/ui', {});
    const html = String(anon.text || '');
    const stripped = stripComments(html);
    rep.ok(anon.status === 200, '未认证 /ui 回 200（给人看的入口，不是 401）', 'status=' + anon.status);
    rep.ok(html.includes(lib.LOGIN_FORM_MARKER), '是**登录页**（含 ' + lib.LOGIN_FORM_MARKER + '）',
      'bytes=' + html.length);
    rep.ok(!lib.SHELL_MARKER_RE.test(html), '**不是**控制台外壳（不含 probeSession）',
      'shell_marker=' + lib.SHELL_MARKER_RE.test(html));
    for (const [mark, why] of REGISTER_ENTRY_MARKERS) {
      rep.ok(!stripped.includes(mark), '剥注释后**没有**注册入口：' + mark + '（' + why + '）',
        stripped.includes(mark) ? '命中' : '未命中');
    }
  }

  rep.section('判据 5：管理员会话 /ui = 控制台外壳，且**没有**成员面残留');
  if (!adminClient) {
    // ⚠ 没有管理员会话就没法观察外壳 —— 如实 SKIP，不静默通过。
    rep.skip('本次没拿到管理员会话（库空且 bootstrap 没成功）⇒ 判据 5 不可观察。');
  } else {
    const c = adminClient;
    // ⚠ 走 cookie 罐（会话身份）。裸请求会拿回登录页 —— 那是驱动错了，不是产品坏了。
    const r = await c.ui();
    const html = String(r.text || '');
    const stripped = stripComments(html);
    rep.ok(r.status === 200, '管理员会话 /ui 回 200', 'status=' + r.status);
    rep.ok(lib.SHELL_MARKER_RE.test(html), '是**控制台外壳**（含 probeSession）',
      'shell_marker=' + lib.SHELL_MARKER_RE.test(html));
    rep.ok(!html.includes(lib.LOGIN_FORM_MARKER), '**不是**登录页（不含 ' + lib.LOGIN_FORM_MARKER + '）',
      'login_marker=' + html.includes(lib.LOGIN_FORM_MARKER));
    for (const [mark, why] of MEMBER_FACE_MARKERS) {
      rep.ok(!stripped.includes(mark), '剥注释后**没有**成员面：' + mark + '（' + why + '）',
        stripped.includes(mark) ? '命中' : '未命中');
    }
  }

  rep.section('判据 6：/admin/apikeys 载荷**没有** member 字段，且列表可用');
  if (!adminClient) {
    rep.skip('本次没拿到管理员会话 ⇒ 判据 6 不可观察。');
  } else {
    const c = adminClient;
    const res = await c.apikeys();
    const text = String(res.text || '');
    rep.ok(res.status === 200, '/admin/apikeys 可用（status=200）', 'status=' + res.status);
    // ⚠ 判据是"字段缺席"，不是"这个词没出现"：响应里别处可能提到 member。
    rep.ok(!/"member"\s*:/.test(text), '载荷里**没有** `"member":` 字段（新口径下角色只有一种）',
      (text.match(/"member"\s*:\s*[^,}]*/) || ['(未命中)'])[0]);
    // ⚠ 不许用"载荷为空"冒充通过：列表必须真的**可用** ——
    //   先建一个 key，再确认它出现在列表里。
    const name = 'probe-single-admin-' + lib.randToken(6);
    const made = await c.createKey(name);
    rep.ok(made.status === 200, '能新建一个 API key（列表可用性的前置）', 'status=' + made.status);
    const after = await c.apikeys();
    const keys = (after.json && (after.json.keys || after.json.items)) || [];
    rep.ok(keys.length > 0, '列表**非空**且真的返回了 key（不许用空载荷冒充通过）',
      'keys=' + keys.length);
    rep.ok(String(after.text || '').includes(name), '刚建的 key **出现在列表里**（不是空壳响应）',
      'name=' + name);
  }
}

// ---------------------------------------------------------------------------
// 自检用例：每条坏桩都必须让探针变红
// ---------------------------------------------------------------------------
const SELFTEST_CASES = [
  {
    // 空库基线：判据 1（bootstrap 建出管理员）在这里可观察；判据 3 会如实 SKIP。
    desc: '好桩 ①（**空库**基线）：bootstrap 应建出管理员 ⇒ 探针必须全绿',
    expect: 'pass', stub: { users: [] },
  },
  {
    // 非空库基线：判据 3（退役成员被拒）在这里可观察；判据 1 会如实 SKIP。
    // ⚠ 两条基线缺一不可 —— 一个实例不可能同时是"空库"与"盘上已有老记录"。
    desc: '好桩 ②（**非空库**基线，含一条退役成员）：应与空库同样全绿',
    expect: 'pass', stub: { users: [ADMIN_SEED, LEGACY_SEED] },
  },
  {
    desc: '坏桩 ①：库非空还能注册（`register_open_after_bootstrap`）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { register_open_after_bootstrap: true }, users: [ADMIN_SEED, LEGACY_SEED] },
  },
  {
    // ⚠ 必须用**空库**：判据 1 只在空库时可观察，否则这条坏桩观察不到。
    desc: '坏桩 ②：bootstrap 建出来的不是管理员（`register_creates_member`）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { register_creates_member: true }, users: [] },
  },
  {
    desc: '坏桩 ③：退役成员历史记录照样登录（`legacy_member_accepted`）⇒ 探针必须变红',
    expect: 'fail',
    stub: { defects: { legacy_member_accepted: true }, users: [ADMIN_SEED, LEGACY_SEED] },
  },
  {
    desc: '坏桩 ④：登录页还带注册入口（`login_shows_register`）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { login_shows_register: true }, users: [ADMIN_SEED, LEGACY_SEED] },
  },
  {
    desc: '坏桩 ⑤：外壳里还带成员模式残留（`console_leaks_member_mode`）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { console_leaks_member_mode: true }, users: [ADMIN_SEED, LEGACY_SEED] },
  },
  {
    desc: '坏桩 ⑥：/admin/apikeys 载荷里还有 member 字段（`apikeys_payload_member_field`）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { apikeys_payload_member_field: true }, users: [ADMIN_SEED, LEGACY_SEED] },
  },
  {
    desc: '坏桩 ⑦：未登录也吐控制台外壳（`console_serves_shell_when_anon`）⇒ 判据 4 必须变红',
    expect: 'fail', stub: { defects: { console_serves_shell_when_anon: true }, users: [ADMIN_SEED, LEGACY_SEED] },
  },
];

lib.runCli({
  title: '单账号管理员口径',
  runProbe,
  cases: SELFTEST_CASES,
  apiKey: 'probe-key-single-admin',
  // ⚠ 自检路径要能用"预置一条退役成员"这条判据：桩的 `users` 由用例给出，
  //   而 `runProbe` 要知道那个用户名 ⇒ 桩模式下用固定名（见 SELFTEST_CASES 里的 users）。
}).then((code) => {
  // runCli 自己处理了 exit code；这里只在 true 模式下补一句结论。
  process.exit(code);
});