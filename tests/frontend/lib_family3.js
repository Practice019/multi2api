// lib_family3.js —— **第三族（浏览器 E2E，25 条）的夹具实例共享层**（2026-10-19 抽）。
//
// # 为什么必须抽（这是本轮最重要的一条，比任何单条红都重要）
//
// 第三族的"通过数"曾经取决于**用哪个实例配置跑**：
//
//	同一份代码、同一天，我一个人跑 18/25、另一个人跑 20/25
//	差异来自三件事：实例配置（有没有 codearts/workbuddy 账号）、
//	                时序（模型目录什么时候回来）、并发（同端口串台）
//
// 于是那个数字**不可复现** —— 换个人跑就变。而用户要的是
// 「保证前后端全量测试通过」，一个换个人跑就变的数**背书不了**。
//
// 所以把"族的前置"全部固化到这一份，调用方不再自己猜：
//
//	(1) 实例配置 config / auths —— 见 FAMILY3_CONFIG / FAMILY3_AUTHS
//	(2) 端口 —— 由调用方给（lib_instance 的硬约束：不许用 7863/3080/3000）
//	(3) 前置数据 —— seedTraffic（真实 /v1 调用灌日志）+ seedCheckinLog（合成历史）
//	(4) 认证 —— baseUrl() 拼带令牌的 BASE（T7 起 /ui 要登录）
//	(5) 收尾 —— stopFamily3：实例 + 临时目录一起清（**finally 里调**）
//	(6) 打印 —— describeFamily3：一行"族前置"，像规范入口那 5 行一样
//
// # 谁依赖前置数据（写清楚，别让下一个人再猜）
//
//	seedTraffic    ⇒ verify_clearscreen.js（清屏前要有行）/ verify_t3_t4.js（渲染出行）
//	                 / panel_content_e2e.js（请求日志"接口 N 条"）
//	seedCheckinLog ⇒ verify_t3_t4.js（"total 是过滤后的数" —— 没数据时它只能停在 B）
//
// # 硬约束（与 lib_instance 同源，一条都不放松）
//
//	· 只写临时目录：`auths/`、`data/` 全部落在 <tmp>，**绝不碰用户仓库根的 data/**
//	· 不放真凭据：下面每个 auths 条目都是**合成/假值**（fake JWT、假 uid）
'use strict';
const fs = require('fs');
const path = require('path');

const instLib = require('./lib_instance.js');

// ── (1) 实例配置 ─────────────────────────────────────────────────────────────
//
// 为什么要**显式列出**启用了哪些上游：`startInstance` 的缺省 config 里
// 只有 `auth_dir: './auths'`，一个上游都不 enabled ⇒ 账号池是空的 ⇒
// 所有"面板里有数据"的断言都量不到东西。此前调用方（stage.js 之类）
// 各自就地写一小段 config，于是"跑出来的数"跟着那段 config 漂。

// 合成账号库。**每一个值都是假的**，只为让账号池非空、面板有东西可渲染。
//
// ⚠ loomy 的凭据形态（`{session, userid}`）是从本仓已有探针抄的
//   （见 verify_accts_delegation.js:200、verify_delete_flow_e2e.js:60）——
//   那是**实测可用**的形态，不是我猜的。
// ⚠ workbuddy 用的是核心 `auth.Auth` 的嵌套落盘形态
//   （`internal/auth/auth.go:94-105`：`{auth:{accessToken,…}, account:{uid,…}}`）——
//   它是**合成**的，上游调用会失败（假 JWT），但那不影响第三族的判据：
//   本族量的是**前端渲染**，不是上游协议（上游协议由 internal/workbuddy/*_test.go 覆盖）。
//
// ⚠ `expiresAt` **给未来时间**（不是 0）：本族的夹具态要的是"账号池里有**有效**账号"。
//   给 0 会让它们在 UI 上渲染成**已过期**态 —— 而本族多条断言隐含"账号可用"
//   （徽章、状态条、倍率气泡），那会造出**夹具自己的红**（最难查的一类）。
//   过期/刷新逻辑不归本族验（那条由 internal/auth 的测试覆盖）。
const FAMILY3_EXPIRES_AT = Math.floor(Date.now() / 1000) + 30 * 24 * 3600;
const FAMILY3_AUTHS = {
  'loomy/loomy-synth1.json': JSON.stringify({ session: '00000000-0000-4000-8000-000000000001', userid: 'probe-loomy-1' }),
  'loomy/loomy-synth2.json': JSON.stringify({ session: '00000000-0000-4000-8000-000000000002', userid: 'probe-loomy-2' }),
  'workbuddy/workbuddy-synth1.json': JSON.stringify({
    auth: { accessToken: 'probe.fake.jwt.1', refreshToken: '', expiresAt: FAMILY3_EXPIRES_AT, domain: '', channel: '' },
    account: { uid: 'probe-workbuddy-1', nickname: '探针合成号1', deviceToken: '' },
  }),
  'workbuddy/workbuddy-synth2.json': JSON.stringify({
    auth: { accessToken: 'probe.fake.jwt.2', refreshToken: '', expiresAt: FAMILY3_EXPIRES_AT, domain: '', channel: '' },
    account: { uid: 'probe-workbuddy-2', nickname: '探针合成号2', deviceToken: '' },
  }),
};

const FAMILY3_CONFIG = {
  loomy: { enabled: true, auth_dir: './auths/loomy' },
  workbuddy: { enabled: true, auth_dir: './auths/workbuddy' },
};

// ── (3) 前置数据 ─────────────────────────────────────────────────────────────

const FAMILY3_SEED_DEFAULTS = { traffic: 80, checkinLog: 30 };

/**
 * 灌**真实**请求日志：打 N 次 /v1/chat/completions。
 *
 * 为什么必须是真的（不能用 stub）：`verify_clearscreen` / `verify_t3_t4` /
 * `panel_content_e2e` 量的是"落盘日志有几条、渲染出几行"——
 * 那是**产品自己的落盘 + 读取 + 渲染**链路。stub 掉就等于把被测的东西换掉了。
 * 上游会回 503（合成凭证），但那正是**真实代码路径**：失败也要记一条日志。
 */
async function seedTraffic(base, key, n) {
  const want = Number(n) || 0;
  let ok = 0, fail = 0;
  for (let i = 0; i < want; i++) {
    const r = await instLib.httpJson(base, '/v1/chat/completions', {
      method: 'POST', token: key,
      body: { model: 'loomy/deepseek-v4-flash-0731', messages: [{ role: 'user', content: '家族夹具 seed ' + i }] },
    });
    if (r.status >= 200 && r.status < 300) ok++; else fail++;
  }
  return { want, ok, fail };
}

/**
 * 灌**合成**签到历史到 <tmp>/data/checkin-log.json。
 *
 * 格式逐字来自 `internal/checkinlog/checkinlog.go:66-76` 的 Record：
 *   {at, uid, nickname?, kind, status, detail?, credits?, trigger?}
 *   kind   ∈ checkin|keepalive|travel|credits|growth|welfare|activity
 *   status ∈ ok|already|fail|skip
 *
 * ⚠ 只写**临时实例自己的** data/ —— 用户仓库根的 data/ 一律不碰。
 * ⚠ uid 一律用合成的 `probe-*`：这条记录会出现在 /admin/checkin/history 里，
 *   真 uid 会把探针的输出泄露成"看起来像真账号"。
 */
function seedCheckinLog(tmp, n, uids) {
  const want = Number(n) || 0;
  const pool = (uids && uids.length) ? uids : ['probe-workbuddy-1', 'probe-workbuddy-2'];
  const kinds = ['checkin', 'keepalive', 'travel', 'growth'];
  const recs = [];
  const t0 = Date.now() - want * 3600 * 1000;
  for (let i = 0; i < want; i++) {
    recs.push({
      at: new Date(t0 + i * 3600 * 1000).toISOString(),
      uid: pool[i % pool.length],
      nickname: '探针合成号' + (((i % pool.length) + 1)),
      kind: kinds[i % kinds.length],
      status: (i % 7 === 0) ? 'fail' : ((i % 5 === 0) ? 'already' : 'ok'),
      detail: '家族夹具合成记录',
      credits: (i % 6) * 5,
      trigger: (i % 3 === 0) ? 'schedule' : 'manual',
    });
  }
  const p = path.join(tmp, 'data', 'checkin-log.json');
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, JSON.stringify(recs, null, 2));
  return { path: p, count: recs.length };
}

// ── (2)+(4)+(5)+(6)：起 / 描述 / 收 ──────────────────────────────────────────

/**
 * 起一个**族夹具实例**：隔离实例 + 合成凭证 + 两份前置数据。
 *
 * @param {{port: number, seed?: {traffic?: number, checkinLog?: number}, quiet?: boolean}} opts
 * @returns {Promise<{base, tmp, key, pid, logPath, stop, readLog, baseUrl, seeded, preamble}>}
 */
async function startFamily3(opts) {
  const o = opts || {};
  if (!o.port) throw new Error('startFamily3 必须显式给端口（别用 7863/3080/3000，见 lib_instance）');
  const seed = Object.assign({}, FAMILY3_SEED_DEFAULTS, o.seed || {});

  const inst = await instLib.startInstance({
    port: o.port,
    apiKey: o.apiKey || ('probe-key-' + o.port),
    auths: FAMILY3_AUTHS,
    config: FAMILY3_CONFIG,
  });

  // ⚠ 前置数据的顺序无关，但**必须在跑探针之前**：
  //   verify_clearscreen 的前置断言写着"若这里是 0，先跑 seed_traffic"——
  //   那就是"夹具没灌"被当成产品缺陷的形态。这里一次性做掉。
  let traffic = { want: 0, ok: 0, fail: 0 };
  let checkin = { count: 0, path: '' };
  let accounts = { total: 0, byProvider: {}, uids: [] };
  let verified = null;   // 回读结果（这才是"证明"，见下面 verifySeed 的注释）
  try {
    // 先看账号池 —— 这一步同时**证明**了"4 份合成凭证真的被认下来了"。
    // ⚠ 不证明它，"写进文件就算成功"是个可能为假的前提：auths 目录名/配置
    //   auth_dir 对不上时文件就只是躺在那里，而下游会红在"面板里没有账号"。
    accounts = await readAccounts(inst.base, inst.key);

    traffic = await seedTraffic(inst.base, inst.key, seed.traffic);
    // 从 /admin/accounts 取 workbuddy 的 uid（合成凭证也会被列出来），
    // 让签到历史里的 uid 与账号池**同源** —— 否则 verify_t3_t4 的
    // "历史端点只返回本上游账号的记录"会因为 uid 对不上而红，
    // 而那是夹具自己造出来的红。
    checkin = seedCheckinLog(inst.tmp, seed.checkinLog, accounts.uids);

    // ⚠⚠ 回读一次，把**真值**当判据 —— 这是本层的核心纪律：
    //     "灌了 80 次"（意图）≠"落盘 80 行"（结果）。
    //     某次 seed 全 4xx/连接失败时 ok=0/fail=80，但若前置行只打 want，
    //     照样会说"调用 80 次（真实落盘）" —— 于是下游那三条依赖种子的探针
    //     一红，归因又飘到"产品"。所以要回读后端自己说的数。
    verified = await verifySeed(inst.base, inst.key, seed);
  } catch (e) {
    // 前置数据失败**不吞**：它会让后面的红归因全错，必须说出来。
    console.error('  ⚠ 族前置数据没灌成: ' + (e && e.message ? e.message : e));
    console.error('    ⇒ 下面 verify_clearscreen / verify_t3_t4 / panel_content_e2e 的红');
    console.error('      可能是"夹具没数据"，不是产品缺陷。');
  }

  const fx = Object.assign({}, inst, {
    baseUrl: inst.base + '/ui?token=' + encodeURIComponent(inst.key),
    seeded: { traffic, checkinLog: checkin, accounts, verified },
  });
  fx.preamble = describeFamily3(fx);
  if (o.quiet !== true) fx.preamble.forEach((l) => console.log(l));

  // 前置不一致就**大声红**（但**不抛**：让人还能看到那 25 条的真实结果，
  // 从而自己判断哪些红是夹具造成的）。出口码由入口决定，见 run_e2e_family3.js。
  if (verified && !verified.ok) {
    console.error('');
    console.error('  ✗✗✗ 族前置**没能证明**：种子意图与后端回读不一致');
    verified.problems.forEach((p) => console.error('       · ' + p));
    console.error('       ⇒ 下面依赖种子的探针（verify_clearscreen / verify_t3_t4 /');
    console.error('         panel_content_e2e）的红**不能算产品缺陷**，先修夹具。');
    console.error('       这不静默放过：前置不成立时，"全族通过"这句话没有意义。');
    console.error('');
    fx.seedMismatch = verified.problems;
  }
  return fx;
}

/** 读账号池：总数 + 按上游分布 + workbuddy 的 uid 列表（供签到历史同源）。 */
async function readAccounts(base, key) {
  const r = await instLib.httpJson(base, '/admin/accounts', { token: key });
  const list = (r.json && r.json.accounts) || [];
  const byProvider = {};
  list.forEach((a) => { const p = (a && a.provider) || '(无)'; byProvider[p] = (byProvider[p] || 0) + 1; });
  return {
    total: list.length,
    byProvider,
    uids: list.filter((a) => a && a.provider === 'workbuddy').map((a) => a.uid).filter(Boolean),
  };
}

/**
 * 回读一次，比较"种子意图"与"后端自己说的数"。
 *
 * 判据（两条独立通道，任一条不成立就是没灌成）：
 *   · `/admin/logs/history` 的 `total`      —— 读取路径（探针会用它）
 *   · `/admin/stats` 的 `file.count`        —— 落盘路径（文件里真的有几行）
 * 两条都对得上，才敢在前置行里写"日志 total=N ✓"。
 */
async function verifySeed(base, key, seed) {
  const wantTraffic = Number(seed.traffic) || 0;
  const wantCheckin = Number(seed.checkinLog) || 0;
  const problems = [];
  const hist = await instLib.httpJson(base, '/admin/logs/history?limit=1&offset=0', { token: key });
  const stats = await instLib.httpJson(base, '/admin/stats', { token: key });
  const logTotal = (hist.json && typeof hist.json.total === 'number') ? hist.json.total : null;
  const fileCount = (stats.json && stats.json.file && typeof stats.json.file.count === 'number')
    ? stats.json.file.count : null;

  if (logTotal === null) problems.push('/admin/logs/history 没给出 number 类型的 total（status=' + hist.status + '）');
  else if (logTotal < wantTraffic) problems.push('日志 total=' + logTotal + ' < 意图 ' + wantTraffic + '（灌了但没落全）');
  if (fileCount === null) problems.push('/admin/stats 没给出 file.count（status=' + stats.status + '）');
  else if (fileCount < wantTraffic) problems.push('落盘 file.count=' + fileCount + ' < 意图 ' + wantTraffic);

  // 签到历史：文件是我们直接写的，回读端点确认它真被读出来了
  const chk = await instLib.httpJson(base, '/admin/checkin/history?limit=1&offset=0', { token: key });
  const chkTotal = (chk.json && typeof chk.json.total === 'number') ? chk.json.total : null;
  if (wantCheckin > 0) {
    if (chkTotal === null) problems.push('/admin/checkin/history 没给出 number 类型的 total（status=' + chk.status + '）');
    else if (chkTotal !== wantCheckin) problems.push('签到历史 total=' + chkTotal + ' ≠ 写的 ' + wantCheckin + ' 条（格式或路径不对）');
  }

  return {
    ok: problems.length === 0,
    problems,
    logTotal, fileCount, checkinTotal: chkTotal,
    wantTraffic, wantCheckin,
  };
}

/** (6) 一行"族前置"：exe/mtime + 实例配置摘要 + 种子条数 + BASE 主机/端口。 */
function describeFamily3(fx) {
  const b = instLib.describeBuild();
  const exeRel = path.relative(instLib.REPO, b.exe).replace(/\\/g, '/');
  const providers = Object.keys(FAMILY3_CONFIG);
  const s = (fx && fx.seeded) || {};
  const acc = s.accounts || { total: 0, byProvider: {} };
  const v = s.verified || null;
  const accStr = Object.keys(acc.byProvider).sort()
    .map((k) => k + '=' + acc.byProvider[k]).join(' ') || '(空)';
  return [
    '=== 第三族（浏览器 E2E）夹具前置 ===',
    '  构建  : ' + exeRel + '   mtime=' + (b.exeMtime ? String(b.exeMtime) : '(缺)') +
      '   新鲜=' + (b.fresh === null ? '(无法判断)' : (b.fresh ? '是' : '否')) +
      '  （源: ' + b.newestRel + ' ' + (b.newestMtime ? String(b.newestMtime) : '(缺)') + '）',
    '  实例  : ' + (fx ? fx.base : '(未起)') + '   pid=' + (fx ? fx.pid : '(无)'),
    // ⚠ 这一行**证明**了"凭证真的被账号池认下来了"，而不是"写进文件就算成功"。
    '  账号  : 共 ' + acc.total + ' 个（' + accStr + '）  ← 4 份合成凭证被认下来几个' +
      (acc.uids && acc.uids.length ? '　workbuddy uid=' + acc.uids.join(',') : ''),
    '  种子  : 意图 traffic=' + (s.traffic ? s.traffic.want : '?') + ' / checkin=' +
      (s.checkinLog ? s.checkinLog.count : '?') +
      '　**回读** logTotal=' + (v ? v.logTotal : '?') + ' fileCount=' + (v ? v.fileCount : '?') +
      ' checkinTotal=' + (v ? v.checkinTotal : '?') +
      '　' + (v ? (v.ok ? '一致 ✓' : '✗ 不一致（见下）') : '(未回读)'),
    '  BASE  : ' + (fx ? fx.baseUrl.replace(/token=[^&]*/, 'token=***') : '(无)') +
      '　（探针用带令牌的那份，见 lib_family3.baseUrl）',
    '  依赖  : verify_clearscreen / verify_t3_t4 / panel_content_e2e 依赖上面两份种子',
    '  上游  : ' + providers.join(',') + '　账号为**故意有效**态（expiresAt 给未来 30 天）——' +
      '过期/刷新逻辑不归本族验',
  ];
}

/**
 * (5) 收尾：实例 + 临时目录一起清。**放在 finally 里调，并且必须 await**。
 *
 * ⚠ 必须 await 的两个理由：
 *   ① `lib_instance.stop()` 现在要等到"端口真的能 bind"才返回
 *      （见那边的长注释：kill 完立刻返回会让下一条探针撞上本层自己的端口预检）；
 *   ② 临时目录要等进程真退出才能删干净（Windows 上进程还活着会锁着 data/）。
 *   不 await 就等于把这两件事都退回到"碰运气"。
 *
 * 为什么连临时目录也要清：下一个人的 `startInstance` 用的是
 * `<tmp>/wb2api-inst-<port>-<pid>`，pid 复用时会拿到上一轮的残留
 * （账号池/日志/缓存都不是新的）—— "换个人跑数字就变"的一半来源在这里。
 */
async function stopFamily3(fx) {
  if (!fx) return;
  try { if (typeof fx.stop === 'function') await fx.stop(); } catch (e) {
    // 收尾失败要出声：它会让下一次运行撞在端口预检上，而那看起来像"环境脏"。
    console.error('  ⚠ 族实例收尾失败: ' + (e && e.message ? e.message : e));
  }
  try { fs.rmSync(fx.tmp, { recursive: true, force: true }); } catch { /* 文件锁，下次再说 */ }
}

/** 起 → fn → finally 收。推荐入口（不可能漏收尾）。 */
async function withFamily3(opts, fn) {
  const fx = await startFamily3(opts);
  try {
    return await fn(fx);
  } finally {
    await stopFamily3(fx);
  }
}

module.exports = {
  startFamily3, stopFamily3, withFamily3, describeFamily3,
  seedTraffic, seedCheckinLog,
  FAMILY3_AUTHS, FAMILY3_CONFIG, FAMILY3_SEED_DEFAULTS,
};
