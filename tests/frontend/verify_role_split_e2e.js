// verify_role_split_e2e.js —— 「API key 归属 + /admin/* 角色分流」的验收探针（T8）。
//
// 主脑这轮修的是一个**真洞**：成员可以轮换管理令牌。
//
//	改造前：成员白名单是**前缀**匹配，里面只写了 `/admin/apikeys`
//	        ⇒ `POST /admin/apikeys/rotate-admin` 也命中前缀
//	        ⇒ 而那个端点的回执里带**完整的新 admin_token**
//	        ⇒ 成员一次调用 = 拿到管理员那条路（提权）
//	改造后：白名单是**精确的 (方法, 路径) 对**（admin.go:709 的 memberRules），
//	        并且 key 的读/改都带**归属**判据（apikey/owner.go）
//
// # 为什么必须是这个形状（这类改动全是"看不见的退化"）
//
// 五条真实退化形态，**每一条都不会让界面看起来坏掉**：
//
//	① 列表不过滤归属 —— 成员在"我的 API Key"页里看见别人的 key（含管理员的）。
//	   页面照常渲染、行数还更多，只有不该看见的人在看见。所以判据是
//	   "成员那把钥匙的**列表内容**"，不是"接口能不能调通"。
//	② 改 key 不判归属 —— 成员能改/删**别人**的 key（改额度、直接删掉）。
//	   最危险的形态是 delete：一次调用让对方凭据失效。判据必须**逐操作**量
//	   （toggle/delete/reset/update 四条各自都能被漏判）。
//	③ 成员能轮换管理凭据 —— 回执带完整新 admin_token ⇒ 一次调用即提权。
//	   这条正好可以独立量：一个只出成员会话的请求打 rotate-*，必须 403，
//	   且**回执里不得出现任何凭据字段**（否则"403 但顺手把新令牌吐出来"）。
//	④ 成员能读 /admin/stats —— 它回 by_uid / by_provider 这类**管理面**聚合
//	   （按上游账号与上游排行）⇒ 等于把上游账号的昵称与用量端出去。
//	⑤ 归属判据的**边界写反** —— 空 owner 在实现里是"管理员所有"的标记
//	   （老数据都没有 OwnerID）。所以 `k.owner === owner` 这种"比相等"的写法
//	   在两边都是空串时成立 ⇒ 任何让身份落到空串的情况（未登录、会话失效、
//	   身份没接上）都会把管理员的 key 整列表端出去。`apikey.ListFor` 因此是
//	   "先判身份再看归属"。本探针用 **admin_token 脚本路径**造一把空 owner 的
//	   key，专测这条边界（成员既看不见也管不了）。
//
// # 四个特别容易写错的地方（都实测踩过）
//
//	ⓐ **owner = `account.Principal.UserID`，不是"管理员建的为空"**。
//	   实测（真实服务端，2026-10-19）：成员建的 ⇒ 自己的 UserID；
//	   **管理员会话**建的 ⇒ **管理员自己的 UserID（非空）**；
//	   `admin_token` 建的 ⇒ 无账号 ⇒ 空串 = 管理员所有（存量语义）。
//	   所以"空 owner"只出现在**机器令牌**那条路上。照"管理员建的一定为空"写
//	   会得到一条永远红的断言（而它红的原因是实现正确）。
//	ⓑ **成员必须是第二/第三次注册**。隔离实例空库的首个注册走 CreateBootstrap
//	   ⇒ 建成**管理员**（internal/account/http.go:327）。拿它当成员测 403
//	   会得到 200，看起来像"实现没做角色判定"，其实是探针用错了账号。
//	ⓒ **每个账号只签一条会话**。会话签发有上限（1 小时内次数过多 ⇒ 429，
//	   见 account.SessionStore），本探针每个账号登录一次、此后复用同一条会话
//	   （Go 侧 apikey_role_split_test.go 的 sidOf 用的是同一口径）。
//	ⓓ **管理员那一侧的 rotate 会真的轮换凭据并写回 config.json**。
//	   所以它只在**探针自己起的隔离实例**（--start / --self-test）上做；
//	   `--base` 打外部实例时跳过（那会改掉你的部署）。成员那一侧是纯 403，
//	   不改任何东西，任何时候都跑。
//
// # 三种用法
//
//	node tests/frontend/verify_role_split_e2e.js                 # 打 WB2API_BASE（缺省 http://127.0.0.1:7885）
//	node tests/frontend/verify_role_split_e2e.js --start         # 探针自己起隔离空实例（7800 段端口 + D:\temp 下的临时目录）
//	node tests/frontend/verify_role_split_e2e.js --self-test     # 打本地桩，验证**探针自身有判别力**
//
// ⚠ 段落 ⓪ 会把"这次验的是哪个二进制"打出来（路径 + mtime + 大小）。
//   改动没生效时第一个该怀疑的就是"验了旧构建"，所以它必须显式可见。
'use strict';
const fs = require('fs');
const path = require('path');
const lib = require('./lib_authapi.js');
// 只为打印"本次验的是哪个二进制"（lib_instance 是起实例的唯一入口，见其文件头）。
const instLib = require('./lib_instance.js');

// ⚠⚠⚠ 2026-10-19 用户新口径：本探针**测的那个面已经不存在了（已退役）**。
//
// 用户原话：「我们那个用户和管理员，我觉得可以只要一个管理员就行了，只要一个账号。
// 用户其实可以不用。」⇒ 只有管理员、只有一个账号，**成员/角色分流这一套不再存在**。
//
// # 退役说明（按本仓规矩：删掉/停用一条探针必须说明"它测的那个面已经不存在"）
//
// 本探针原来的五条判据，全部以"**存在成员会话**"为前提：
//
//	① 列表按归属过滤        —— 前提是"别人的 key"存在（成员 vs 管理员两个 owner）
//	② 改 key 判归属         —— 同上
//	③ 成员不能轮换管理凭据  —— 前提是"成员会话"存在（`memberRules` 白名单）
//	④ 成员不能读 /admin/stats—— 同上
//	⑤ 列表回 member 标志    —— 前提是角色有两种（前端靠它分"我的/全部"）
//
// 新口径下：**没有任何代码路径能创建非管理员账号**（唯一建号路径 `CreateBootstrap`
// 写死管理员），`memberRules` / `memberPaths` / `"member": !p.IsAdmin()` 同时被删。
// ⇒ 上面五条的**触发条件**都不存在了：留着它们会变成"永远为假/永远为真"的判据，
//   后来人会以为还有成员分流要测 —— 那比没有探针更坏。
//
// # 谁来接它的班（**不是删了了事**）
//
//	· **角色面**：`tests/frontend/verify_single_admin.js`（本轮的接受判据，六条 +
//	  自检 9/9 达标）。它接的正是"只有管理员"这条线：注册只在库空时受理一次、
//	  退役成员记录被拒且服务不崩、登录页无注册入口、外壳无成员面、载荷无 member 字段。
//	· **归属面**（机器令牌建的 key owner 为空 vs 会话建的 key、rotate 的管理员硬门禁）：
//	  这一半**仍有实体**，是本次退役留下的**待迁移项** —— 见文件末尾的 §待迁移，
//	  已列出要保留的判据形状与对应缺陷开关，避免"随成员面一起被删掉"。
//
// # 为什么用 exit 2 而不是 exit 0/1
//
//	本仓约定（见 `run_frontend_suites.js` 里 `verify_derived_freshness.js` 的用法）：
//	**2 = ENV/不适用**，1 = 断言失败，0 = 通过。
//	这里必须是 2：它不是"失败"（产品没坏），也不是"通过"（什么都没验）。
//	若返回 0，它就会变成一个**永远绿的占位**；若返回 1，它会被当成产品缺陷。
//
// ⚠ 原实现（495 行）与它的桩侧缺陷开关保留在 git 历史里（本轮之前的提交），
//   要做 §待迁移 的改造时可以照抄那段结构，不必从零写。
if (!process.argv.slice(2).includes('--force-legacy')) {
  console.log('=== API key 归属 + /admin/* 角色分流（T8）· **已退役** ===');
  console.log('');
  console.log('本探针测的"成员/管理员角色分流"这一面，在 2026-10-19 的单账号管理员口径下');
  console.log('**已经不存在**：唯一建号路径 CreateBootstrap 写死管理员，注册只在库空时受理一次。');
  console.log('它原来的五条判据全部以"存在成员会话"为前提 ⇒ 已无触发条件。');
  console.log('');
  console.log('接班的判据：');
  console.log('  角色面 ⇒ node tests/frontend/verify_single_admin.js');
  console.log('  归属面 ⇒ 见本文件末尾 §待迁移（仍有实体，待改造）');
  console.log('');
  console.log('本次退出码 2（ENV/不适用）：不是通过、也不是失败。');
  console.log('想强行跑旧实现（只对老构建有意义）：--force-legacy');
  process.exit(2);
}

// ---------------------------------------------------------------------------
// ↓↓↓ 以下为**旧实现**，仅 `--force-legacy` 时执行（只对"登录门之前的构建"有意义）↓↓↓
// ---------------------------------------------------------------------------

// --start 时给隔离实例配的管理令牌（必须与 apiKey 不等，见 lib 的 runCli）。
const PROBE_ADMIN_TOKEN = 'probe-admin-token-role-split';
const PROBE_API_KEY = 'probe-key-role-split';

// 自检用例：好桩必须让探针全绿，坏桩必须让探针变红。
//
// ⚠ 这五个坏桩对应的缺陷开关（`apikeys_list_unfiltered` 等）已随成员面一起从
//   `lib_authapi.js` 的 `DEFECTS` 里**删除**（理由见那边的注释）⇒ 旧实现现在
//   在桩上会**必然红**。这正是它该退役的又一个证据：它的自检已经无处可依。
//   §待迁移 里写清了改造后要用哪几个新开关。
const SELFTEST_CASES = [
  { desc: '好桩 ①：归属过滤 + 精确白名单 + rotate 仅管理员，全按契约实现 ⇒ 探针必须全绿', expect: 'pass', stub: {} },
  {
    desc: '坏桩 ①：成员列表不过滤归属（成员看得见别人的 key）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { apikeys_list_unfiltered: true } },
  },
  {
    desc: '坏桩 ②：改 key 不判归属（成员能改/删**别人**的 key）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { apikeys_mutation_no_owner_check: true } },
  },
  {
    desc: '坏桩 ③：成员能轮换管理凭据（回执带完整新令牌 ⇒ 一次提权）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { rotate_allowed_for_member: true } },
  },
  {
    desc: '坏桩 ④：成员能读 /admin/stats（管理面聚合外泄）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { member_can_read_stats: true } },
  },
  {
    desc: '坏桩 ⑤：列表不回 member 标志（前端分不清"我的"还是"全部"）⇒ 探针必须变红',
    expect: 'fail', stub: { defects: { apikeys_missing_member_flag: true } },
  },
];

// ---------------------------------------------------------------------------
// §待迁移 —— 归属面（仍有实体，别随成员面一起删掉）
// ---------------------------------------------------------------------------
//
// 1. **机器令牌建的 key，owner 为空；会话建的 key，owner = 自己的 UserID。**
//    判据形状：`adminAsToken('POST','/admin/apikeys', token, {name})` 建一把 ⇒
//    列表里它的 owner 是空串；再拿会话建一把 ⇒ owner 非空且等于该账号的 uid。
//    ⚠ 这条**不能**用"管理员建的为空"来写（实测：管理员会话建的是**非空**，
//      只有 `admin_token` 那条机器路径为空；照前者写会得到一条永远红、而实现正确的断言）。
// 2. **归属过滤的边界**：空 owner 在存量语义里 = 管理员所有 ⇒
//    `k.owner === owner` 这种"比相等"的写法在两边都是空串时成立 ⇒
//    任何让身份落到空串的情况都会把管理员的 key 整列表端出去。
//    改造时用"先判身份再看归属"的形状量它（`apikey.ListFor` 的实现契约）。
// 3. **rotate-* 的管理员硬门禁**：`/admin/apikeys/rotate-admin` 与 `rotate-api-key`
//    必须在**任何**身份下都再判一次管理员（`apikeys.go:344/:392` 的第二层硬判），
//    且**回执里不得出现任何凭据字段**（否则"403 但顺手把新令牌吐出来"）。
//    ⚠ 真实例上会真的轮换凭据并写回 config.json ⇒ 只在 `--start`/`--self-test` 上做。
// 4. **桩侧要新加的缺陷开关**（本轮的 6 条单账号缺陷之外）：
//    `owner_missing_on_session_key`（会话建的 key owner 为空 ⇒ 落到"管理员所有"那条边界）、
//    `rotate_admin_gate_removed`（第二轮硬判被删 ⇒ 非管理员也能轮换）、
//    `rotate_body_leaks_token`（403 但回执里仍带新令牌）。


// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// firstDiff 找两段文本第一处不同的下标（逐字节比对失败时给个可查的锚点）。
function firstDiff(a, b) {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i += 1) if (a[i] !== b[i]) return i;
  return a.length === b.length ? -1 : n;
}

// ---------------------------------------------------------------------------
// 探针主体
// ---------------------------------------------------------------------------

async function runProbe(ctx) {
  const rep = ctx.reporter;
  const base = ctx.base;
  const adminToken = ctx.adminToken || '';

  const cliAdmin = lib.makeClient(base); // 首个注册（空实例 ⇒ 管理员）
  const cliA = lib.makeClient(base);     // 第二个注册 ⇒ 成员 A
  const cliB = lib.makeClient(base);     // 第三个注册 ⇒ 成员 B
  const anonCli = lib.makeClient(base);  // 全程不登录：401/403 的分野要用它
  const unameAdmin = lib.freshUsername('p-role-adm');
  const unameA = lib.freshUsername('p-role-a');
  const unameB = lib.freshUsername('p-role-b');
  const pass = 'probe-pass-' + lib.randToken(8);
  const reg = (cli, username) => {
    const body = { username, password: pass };
    if (ctx.captchaParam) body.captcha_param = ctx.captchaParam;
    return cli.post('/admin/auth/register', body);
  };

  // 建过的 key 都记下来，最后清掉 —— 探针是**读也写**的，离开时别留垃圾
  // （--start 的临时目录无所谓，但 --base 打的是别人的实例）。
  const created = [];
  const remember = (r) => {
    if (r && r.json && r.json.id) created.push(r.json.id);
    return r;
  };
  const keysOf = (r) => (r && r.json && Array.isArray(r.json.keys) ? r.json.keys : []);
  const has = (views, id) => (views || []).some((k) => k.id === id);
  // ownerIdOf 取某个 view 的 owner_id，并把"字段缺失"与"条目不存在"分开
  // （`omitempty` 之下，空 owner 的表现就是**整个字段不出现**）。
  const ownerIdOf = (views, id) => {
    const v = (views || []).find((k) => k.id === id);
    if (!v) return { found: false, hasField: false, value: '' };
    return { found: true, hasField: Object.prototype.hasOwnProperty.call(v, 'owner_id'), value: v.owner_id };
  };
  const codeOf = (r) => lib.resCode(r);
  const scopeOf = (r) => (r && r.json && r.json.scope) || '';
  const ev = (r) => `status=${r.status} code=${codeOf(r)} scope=${scopeOf(r)} body=${lib.short(r.text, 120)}`;

  rep.section('⓪ 本次验的是哪个构建（"改了没生效"第一嫌疑是验了旧构建）');
  rep.info('二进制: ' + instLib.EXE);
  try {
    const st = fs.statSync(instLib.EXE);
    rep.info('  mtime: ' + String(st.mtime) + '　大小: ' + st.size + ' 字节');
    // 只报不判：主脑可能正在改源码，此时"二进制比源码旧"是**事实**而不是失败。
    const newest = ['internal/admin/admin.go', 'internal/admin/apikeys.go', 'internal/apikey/owner.go',
      'internal/account/http.go', 'internal/account/identify.go', 'internal/server/webui.html']
      .map((rel) => { try { return { rel, m: fs.statSync(path.join(instLib.REPO, rel)).mtime }; } catch { return null; } })
      .filter(Boolean)
      .sort((x, y) => y.m - x.m)[0];
    if (newest && newest.m > st.mtime) {
      rep.info('  ⚠ 这个二进制比 ' + newest.rel + '（' + String(newest.m) + '）旧 —— 若刚改过源码，先 go build 再复跑。');
    }
  } catch (e) {
    rep.info('  取不到该文件: ' + lib.short(e && e.message, 80) + '（--self-test 打的是本地桩，不经过二进制）');
  }
  rep.info('目标: ' + base + (ctx.stub ? '（本地桩：lib_authapi 的 startStub）' : ''));
  rep.info('本次的管理令牌: ' + (adminToken ? lib.short(adminToken, 14) + '…（用来造"空 owner = 管理员所有"那把 key）' : '(未提供)'));
  rep.info('角色分流口径: memberRules 是**精确 (方法,路径) 对**；成员允许集 = GET/POST /admin/apikeys + 四个改动端点 + GET /admin/ui/manifest');

  // ---- 建号：管理员 + 两个成员（见文件头 ⓑ）--------------------------------
  rep.section('① 归属隔离：管理员看全部，成员只看自己名下的');
  const regAdmin = await reg(cliAdmin, unameAdmin);
  if (regAdmin.status !== 200 || !(regAdmin.json && regAdmin.json.ok === true)) {
    const why = lib.missingReason(regAdmin);
    rep.ok(false, '端点可用：POST /admin/auth/register 必须按契约应答（本组到 ⑤ 全靠它建号）',
      `status=${regAdmin.status} ${lib.short(regAdmin.text, 120)}`);
    if (why) for (const line of lib.missingHint(regAdmin, base)) rep.info(line);
    else rep.info('⇒ 注册没成功就没有后续前置条件，本次收尾（失败原因见上）。');
    return;
  }
  if (ctx.fresh || ctx.stub) {
    rep.ok(regAdmin.json.bootstrap === true,
      '空库首个注册走 bootstrap（回执 "bootstrap":true ⇒ 这一把是管理员）', lib.short(regAdmin.json, 140));
  } else {
    rep.skip('共享实例：库可能非空 ⇒ 首个注册未必是 bootstrap，本项不判（--start 可得确定结论）');
  }
  const loginAdmin = await cliAdmin.login(unameAdmin, pass);
  const roleAdmin = loginAdmin.json && loginAdmin.json.user ? loginAdmin.json.user.role : 0;
  const isAdmin = roleAdmin === 10;
  if (!isAdmin) {
    rep.ok(false, '隔离实例上首个账号必须是管理员（role=10）—— 后面 ②/⑤ 依赖"管理员"这个身份',
      `role=${roleAdmin} ${lib.short(loginAdmin.text, 120)}`);
  }

  // 成员必须是**第二/第三次**注册（首个是 bootstrap 管理员，见文件头 ⓑ）。
  const memberRegs = [];
  for (const [cli, uname, label] of [[cliA, unameA, 'A'], [cliB, unameB, 'B']]) {
    const r = await reg(cli, uname);
    if (r.status === 200 && r.json && r.json.ok === true) {
      const l = await cli.login(uname, pass);
      memberRegs.push({ cli, uname, label, role: l.json && l.json.user ? l.json.user.role : 0 });
    } else if (ctx.fresh) {
      rep.ok(false, `第 ${label} 个成员的注册必须成功（隔离实例、库已非空 ⇒ 只建成员）`,
        `status=${r.status} code=${codeOf(r)} body=${lib.short(r.text, 110)}`);
    } else {
      rep.skip(`本实例不接受更多注册（status=${r.status} code=${codeOf(r)}）⇒ 成员 A/B 测不到；--start 可得确定结论`);
    }
  }
  const members = memberRegs.filter((m) => m.role === 1);
  const memA = members[0] || null;
  const memB = members[1] || null;
  rep.info('实测角色: 管理员=' + roleAdmin + ' 成员=' + JSON.stringify(memberRegs.map((m) => m.label + ':' + m.role)));
  if (!memA || !memB) {
    rep.skip('本次没拿到**两个**实测成员（成员隔离与"成员之间也互相看不见"都需要两个）⇒ 成员那几条测不到');
  }

  // 建 key：管理员一把（会话身份）、A 一把、B 一把
  const before = keysOf(await cliAdmin.apikeys());
  const kAdmin = remember(await cliAdmin.createKey('probe-role-admin'));
  rep.ok(kAdmin.status === 200 && Boolean(kAdmin.json && kAdmin.json.id),
    '管理员建 key → 200 且回执带 id（`full` 是仅此一次返回的明文）',
    `status=${kAdmin.status} full=${lib.short((kAdmin.json || {}).full, 18)} body=${lib.short(kAdmin.text, 110)}`);

  const kA = memA ? remember(await memA.cli.createKey('probe-role-member-a')) : null;
  const kB = memB ? remember(await memB.cli.createKey('probe-role-member-b')) : null;
  if (kA && kB) {
    rep.ok(kA.status === 200 && kB.status === 200,
      '两个成员各建一把 key → 200（成员**能**建自己的 key，不是"成员什么都不能"）',
      `A=${kA.status} B=${kB.status}`);
  }

  const listAdmin = await cliAdmin.apikeys();
  rep.ok(listAdmin.status === 200, '管理员 GET /admin/apikeys → 200', `status=${listAdmin.status}`);
  const adminViews = keysOf(listAdmin);
  rep.ok(adminViews.length === before.length + 3,
    '★ 管理员看到**全部**（基线 ' + before.length + ' 把 + 本次 3 把）',
    `实际 ${adminViews.length} 把 ids=${adminViews.map((k) => lib.short(k.id, 12)).join(',')}`);
  rep.ok(listAdmin.json.member === false,
    '管理员的回执带 member=false（前端据此显示"全部 API Key"而不是"我的"）', `member=${listAdmin.json.member}`);

  if (memA && memB) {
    const listA = await memA.cli.apikeys();
    const listB = await memB.cli.apikeys();
    const av = keysOf(listA);
    const bv = keysOf(listB);
    rep.ok(listA.status === 200 && bv.length === 1 && av.length === 1,
      '★★ 每个成员各只看到**自己那 1 把**（新账号名下不可能有别的 key）',
      `A=${av.length} 把 ${av.map((k) => lib.short(k.id, 12)).join(',')} / B=${bv.length} 把 ${bv.map((k) => lib.short(k.id, 12)).join(',')}`);
    rep.ok(av.length === 1 && av[0].id === kA.json.id,
      '★★ 成员 A 列表里那 1 把**就是自己建的那把**', `期望 ${lib.short(kA.json.id, 14)} 实际 ${av.map((k) => lib.short(k.id, 14)).join(',')}`);
    rep.ok(av.length === 1 && bv.length === 1 && bv[0].id === kB.json.id,
      '★ 成员 B 同理（成员之间也互相看不见，不是"只挡管理员"）', `B 看到 ${bv.map((k) => lib.short(k.id, 14)).join(',')}`);
    rep.ok(listA.json.member === true && listB.json.member === true,
      '★ 成员的回执带 member=true（前端据此把标题换成「我的 API Key」——不回这个字段成员会以为"别人的 key 都不见了"）',
      `A.member=${listA.json.member} B.member=${listB.json.member}`);
    // ★ 管理钥匙的掩码：连掩码都不该给成员（它提示"这东西存在、长什么样"）
    rep.ok(listA.json.admin_key === '' && listA.json.admin_token === '',
      '★★ 成员的回执里 admin_key / admin_token 两个掩码**恒为空串**（管理员才有）',
      `admin_key="${lib.short(listA.json.admin_key, 16)}" admin_token="${lib.short(listA.json.admin_token, 16)}"`);
    rep.ok(Boolean(listAdmin.json.admin_key) && Boolean(listAdmin.json.admin_token),
      '管理员那侧两个掩码都非空（否则前端分不清"我在用哪一把、轮换的是哪一把"）',
      `admin_key="${lib.short(listAdmin.json.admin_key, 16)}" admin_token="${lib.short(listAdmin.json.admin_token, 16)}"`);
  }

  // ---- ② 归属合法：谁看不见谁的，以及空 owner 那条边界 ----------------------
  rep.section('② 归属合法：成员看不见别人的 key；空 owner（脚本建的）成员也看不见也管不了');
  if (memA && memB) {
    const listA = await memA.cli.apikeys();
    const av = keysOf(listA);
    rep.ok(!has(av, kAdmin.json.id),
      '★★ 成员 A 的列表里**不含管理员的 key**（管理员那把的 owner 是管理员自己的 UserID）',
      `列表=${av.map((k) => lib.short(k.id, 14)).join(',')} 管理员那把=${lib.short(kAdmin.json.id, 14)}`);
    rep.ok(!has(av, kB.json.id), '★★ 成员 A 的列表里不含成员 B 的 key（横向隔离）',
      `列表=${av.map((k) => lib.short(k.id, 14)).join(',')} B 那把=${lib.short(kB.json.id, 14)}`);
    // owner_id 是"这把归谁"的唯一可见证据（omitempty：空 owner 整条字段不出现）。
    const ownA = ownerIdOf(av, kA.json.id);
    const listB = await memB.cli.apikeys();
    const ownB = ownerIdOf(keysOf(listB), kB.json.id);
    rep.ok(ownA.hasField && ownA.value && ownB.hasField && ownB.value && ownA.value !== ownB.value,
      '★ 两把成员 key 各自带非空的 owner_id，且**互不相同**（归属确实按账号落盘，不是在展示层过滤）',
      `A.owner_id=${lib.short(ownA.value, 12)} B.owner_id=${lib.short(ownB.value, 12)}`);
    const admOwn = ownerIdOf(keysOf(listAdmin), kAdmin.json.id);
    rep.info('（参考）管理员**会话**建的 key owner_id=' + (admOwn.hasField ? lib.short(admOwn.value, 12) : '(字段缺失=空 owner)')
      + ' —— 实测口径：owner = Principal.UserID，管理员会话同样是**非空**；"空 owner"只出现在 admin_token 那条路上（见文件头 ⓐ）');
  }

  // ★ 空 owner 那条边界：只能用**没有账号身份**的调用造（会话身份永远带 UserID）。
  if (adminToken) {
    const scriptKey = await anonCli.adminAsToken('POST', '/admin/apikeys', adminToken, { name: 'probe-role-script' });
    rep.ok(scriptKey.status === 200 && Boolean(scriptKey.json && scriptKey.json.id),
      '拿 **admin_token**（脚本路径，无账号身份）建 key → 200 —— 用来造"空 owner = 管理员所有"的这把',
      ev(scriptKey));
    if (scriptKey.json && scriptKey.json.id) {
      const sid = scriptKey.json.id;
      created.push(sid);
      const admViews = keysOf(await cliAdmin.apikeys());
      const o = ownerIdOf(admViews, sid);
      rep.ok(o.found && !o.hasField,
        '★ 这把 key 的 owner_id **整个字段不出现**（omitempty：空 owner 就是"管理员所有"的标记）',
        `found=${o.found} hasField=${o.hasField} value=${lib.short(o.value, 12)}`);
      if (memA) {
        const av = keysOf(await memA.cli.apikeys());
        rep.ok(!has(av, sid),
          '★★ 成员 A **看不见** owner 为空的那把 key —— 这是本改造最危险的边界（写反了就把管理员的 key 整列表端出去）',
          `列表=${av.map((k) => lib.short(k.id, 14)).join(',')} 空 owner 那把=${lib.short(sid, 14)}`);
        const del = await memA.cli.keyDelete(sid);
        rep.ok(del.status === 403 && codeOf(del) === 'forbidden_role',
          '★★ 成员 A 对 owner 为空的 key 做 delete → 403（成员不得认领"空 owner = 管理员所有"的老数据）', ev(del));
      }
    }
  } else {
    rep.skip('本次没有 admin_token（--base 模式请设 WB2API_ADMIN_TOKEN）⇒ "空 owner = 管理员所有"那条边界测不到；--start 会自动带上');
  }

  // ---- ③ 越权改 key -------------------------------------------------------
  rep.section('③ 越权改 key：动别人的一律 403 forbidden_role，动自己的一律 200');
  const MUTS = [
    ['toggle', (cli, id) => cli.keyToggle(id, false)],
    ['delete', (cli, id) => cli.keyDelete(id)],
    ['reset', (cli, id) => cli.keyReset(id)],
    ['update', (cli, id) => cli.keyUpdate(id, { limit: 999 })],
  ];
  if (memA && memB) {
    for (const [target, id] of [['管理员的 key', kAdmin.json.id], ['成员 B 的 key', kB.json.id]]) {
      const bad = [];
      for (const [name, fn] of MUTS) {
        const r = await fn(memA.cli, id);
        if (!(r.status === 403 && codeOf(r) === 'forbidden_role' && scopeOf(r) === 'apikey')) {
          bad.push(`${name}→${r.status}/${codeOf(r)}/${scopeOf(r)}`);
        }
      }
      rep.ok(bad.length === 0,
        `★★ 成员 A 对**${target}**做 toggle/delete/reset/update 四条全部 403 forbidden_role(scope=apikey)`,
        bad.length ? '不符合的四条: ' + bad.join(' ') : `${target} id=${lib.short(id, 14)}`);
    }

    // ★ 不给"这个 id 存在吗"的探测口：不存在与不是你的，回执必须**逐字节相同**。
    const ghost = 'sk-does-not-exist-' + lib.randToken(8);
    const diffs = [];
    for (const [name, fn] of MUTS) {
      const foreign = await fn(memA.cli, kAdmin.json.id);
      const missing = await fn(memA.cli, kAdmin.json.id === ghost ? ghost : ghost);
      if (foreign.text !== missing.text) diffs.push(`${name}@${firstDiff(foreign.text, missing.text)}`);
    }
    rep.ok(diffs.length === 0,
      '★★ 成员对"别人的 key"与"根本不存在的 id"回执**逐字节相同**（否则这个端点成了 id 存在性 oracle）',
      diffs.length ? '有差异的操作: ' + diffs.join(' ') : '四条改动端点的两个回执均逐字节相同');

    // 自己的 key：四个操作都要 200（否则就是"过滤过头"，成员管不了自己的东西）。
    // ⚠ 每条各用**一把新 key**：delete 会把 key 删掉，删掉之后 reset/update 拿到的是
    //    "key 不存在" ⇒ CanManage 判 false ⇒ 403。那是**正确**行为，但会让
    //    "复用同一把"的写法看起来像过滤过头（Go 侧同一条注释）。
    const ownFails = [];
    for (const [name, fn] of MUTS) {
      const fresh = remember(await memA.cli.createKey('probe-role-own-' + name));
      if (fresh.status !== 200) { ownFails.push(name + ':建 key ' + fresh.status); continue; }
      const r = await fn(memA.cli, fresh.json.id);
      if (r.status !== 200) ownFails.push(`${name}→${r.status}/${codeOf(r)}`);
    }
    rep.ok(ownFails.length === 0,
      '★ 成员 A 对**自己**的 key 做四条操作全部 200（每条各用一把新 key：delete 会删掉，复用会把"正确 403"误读成过滤过头）',
      ownFails.length ? '不符合的: ' + ownFails.join(' ') : '四条均 200');

    // 管理员管全部：连成员的 key 也能删（"能看见才谈得上管"）。
    const spare = remember(await memB.cli.createKey('probe-role-spare'));
    const admDel = await cliAdmin.keyDelete(spare.json.id);
    rep.ok(admDel.status === 200,
      '★ 管理员能删**成员建的** key → 200（管理员管全部，不是"只管 owner 为空的老数据"）', ev(admDel));
    const bv2 = keysOf(await memB.cli.apikeys());
    rep.ok(!has(bv2, spare.json.id), '删掉之后成员 B 的列表里也立刻没有了（同一个 store，不是各自缓存）',
      `B 现在 ${bv2.length} 把`);
  }

  // ---- ④ 管理端点：成员只能进白名单，其余 403 -----------------------------
  rep.section('④ 角色分流：成员进不去管理面（403 forbidden_role），但白名单内的要能进');
  const ADMIN_ONLY = ['/admin/accounts', '/admin/settings', '/admin/logs', '/admin/providers'];
  if (memB) {
    const bad = [];
    for (const p of ADMIN_ONLY) {
      const r = await memB.cli.adminGet(p);
      if (!(r.status === 403 && codeOf(r) === 'forbidden_role')) bad.push(`${p}→${r.status}/${codeOf(r)}`);
    }
    rep.ok(bad.length === 0,
      '★★ 成员访问 ' + ADMIN_ONLY.join(' / ') + ' 全部 403 forbidden_role（有身份但角色不够）',
      bad.length ? '不符合的: ' + bad.join(' ') : '四条均 403');
    const one = await memB.cli.adminGet('/admin/accounts');
    rep.ok(one.status === 403,
      '403 与 401 分开：错误文案里带上"当前账号 + 角色"，前端据此显示「无权」而不是再跳登录页',
      lib.short(one.text, 150));

    // ★ /admin/stats：T8 从成员白名单撤出（by_uid / by_provider 是管理面聚合）
    const statsBad = [];
    for (const p of ['/admin/stats', '/admin/stats/series']) {
      const r = await memB.cli.adminGet(p);
      if (r.status !== 403) statsBad.push(`${p}→${r.status}/${codeOf(r)}`);
    }
    rep.ok(statsBad.length === 0,
      '★★ 成员访问 /admin/stats 与 /admin/stats/series → 403（管理面聚合：by_uid / by_provider 会把上游账号昵称与用量端出去）',
      statsBad.length ? '不符合的: ' + statsBad.join(' ') : '两条均 403');
  }

  // ★★ 提权面：rotate-* 仅管理员；成员那条**连回执都不许带凭据字段**。
  const ROTATES = ['/admin/apikeys/rotate-admin', '/admin/apikeys/rotate-api-key'];
  if (memB) {
    for (const p of ROTATES) {
      const r = await memB.cli.adminPost(p, {});
      rep.ok(r.status === 403 && codeOf(r) === 'forbidden_role' && scopeOf(r) === 'admin',
        '★★ 成员调 ' + p + ' → 403 forbidden_role(scope=admin) —— 它的回执带**完整新凭据**，调通一次即提权', ev(r));
      const leaks = ['admin_token', 'api_key"', '"full"', '"masked"'].filter((k) => r.text.includes(k));
      rep.ok(leaks.length === 0,
        '★★ ' + p + ' 的**拒绝回执里不含任何凭据字段/值**（否则就是"403 但顺手把新令牌吐出来"）',
        leaks.length ? '命中的字段名: ' + leaks.join(', ') + ' body=' + lib.short(r.text, 140) : '无凭据字段');
    }
  }

  // 白名单正例：不验这个，"成员什么都不能"与"角色分流对了"分不开。
  if (memB) {
    const okList = await memB.cli.apikeys();
    const okManifest = await memB.cli.adminGet('/admin/ui/manifest');
    rep.ok(okList.status === 200,
      '★ 成员进得去白名单里的 GET /admin/apikeys → 200（memberRules 是白名单，不是"成员一律 403"）',
      `status=${okList.status}`);
    rep.ok(okManifest.status === 200,
      '★ 成员进得去 GET /admin/ui/manifest → 200（否则成员连界面外壳都渲染不出来）',
      `status=${okManifest.status} body=${lib.short(okManifest.text, 90)}`);
  }

  // 未认证：401（"你没有凭据"）而不是 403（"你权限不够"）。
  const anonHit = await anonCli.adminGet('/admin/accounts');
  rep.ok(anonHit.status === 401 && codeOf(anonHit) === 'auth_required',
    '★ 无凭据访问 /admin/accounts → 401 auth_required（与成员的 403 分开：前端据此决定跳登录页还是显示"无权"）',
    ev(anonHit));

  // ---- ⑤ 管理员不受影响 ---------------------------------------------------
  rep.section('⑤ 管理员不受影响：同一批端点管理员 200；轮换两个端点"不是 403"');
  if (isAdmin) {
    const bad = [];
    const codes = [];
    for (const p of ['/admin/accounts', '/admin/settings', '/admin/logs', '/admin/providers', '/admin/stats', '/admin/stats/series']) {
      const r = await cliAdmin.adminGet(p);
      codes.push(`${p.replace('/admin/', '')}=${r.status}`);
      if (r.status !== 200) bad.push(`${p}→${r.status}`);
    }
    rep.ok(bad.length === 0,
      '★ 管理员访问 accounts/settings/logs/providers/stats/stats-series 全部 200（收窄白名单不能连管理员一起挡掉）',
      bad.length ? '不符合的: ' + bad.join(' ') : codes.join(' '));
  } else {
    rep.skip('本次首账号实测不是管理员 ⇒ 管理员那批 200 测不到');
  }

  // 清理：探针建的 key 全删掉（能删就删，删不掉只报不判 —— 末尾别把验收堵在清理上）。
  if (isAdmin && created.length) {
    let gone = 0;
    for (const id of created) {
      const r = await cliAdmin.keyDelete(id);
      if (r.status === 200 || r.status === 404) gone += 1;
    }
    const after = keysOf(await cliAdmin.apikeys());
    rep.info('清理：本次建的 ' + created.length + ' 把 key 已删 ' + gone + ' 把；管理员列表剩 ' + after.length + ' 把（基线 ' + before.length + ' 把）');
  }

  // ⚠ 最后做：这两个端点会**真的轮换**凭据并写回 config.json（见文件头 ⓓ）。
  if (isAdmin && (ctx.fresh || ctx.stub)) {
    for (const [p, label] of [['/admin/apikeys/rotate-admin', '管理令牌'], ['/admin/apikeys/rotate-api-key', '对话 api_key']]) {
      const r = await cliAdmin.adminPost(p, {});
      rep.ok(r.status !== 403,
        '★ 管理员调 ' + p + ' 不该被角色拦住（隔离实例接线了 ConfigPath ⇒ 实测 200；未接线时是 400，两种都不是 403）',
        `status=${r.status} body=${lib.short(r.text, 140)}`);
      // 回执带完整新凭据 —— 这正是成员必须被拦住的原因，顺手把它量出来。
      if (r.status === 200) {
        rep.ok(Boolean(r.json && r.json.full && r.json.full.length >= 8),
          '★ 轮换成功的回执带**完整新**' + label + '（⇒ 若成员能调，他一次调用就拿到管理员那条路）',
          `full=${lib.short((r.json || {}).full, 16)} masked=${lib.short((r.json || {}).masked, 16)}`);
      }
    }
  } else if (isAdmin) {
    rep.skip('--base 打的是外部实例 ⇒ 跳过管理员的 rotate 调用（它会真的轮换凭据并写回 config.json）；'
      + '成员那一侧的 403 已在 ④ 段验过（纯拒绝，不改任何东西）');
  }
}

lib.runCli({
  title: 'API key 归属 + /admin/* 角色分流验收（E2E）',
  runProbe,
  cases: SELFTEST_CASES,
  apiKey: PROBE_API_KEY,
  // 隔离实例两把令牌必须不相等：否则"成员不能轮换管理令牌"与"管理员能轮换"是在
  // 拿同一把钥匙比对自身（见 lib 的 runCli 注释）。另外 ⓐ 那条边界也要求
  // admin_token 是一把**已知**的值。
  instanceConfig: { admin_token: PROBE_ADMIN_TOKEN },
}).then((code) => process.exit(code));
