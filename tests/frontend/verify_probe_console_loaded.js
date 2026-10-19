// verify_probe_console_loaded.js —— 静态守卫：**开 /ui 的探针，必须能证明它进的是控制台，而不是登录页**。
//
// # 为什么需要（一次实测到的假绿）
//
// T5 之后 `/ui` 变成两态：**带身份 ⇒ 控制台**；**不带 ⇒ 登录页**（~17–22KB）。于是有一类探针会
// **静默跑在登录页上**：它们用裸 CDP `Page.navigate('/ui')` 开页（绕过了 `lib_cdp.withChrome` 里
// 那行 `withToken(...)`），然后在**空页面**上做断言。
//   · 断言是**正向**的（"有这个按钮"）⇒ 会红（症状吓人但至少不是假绿）；
//   · 断言是**负向**的（"没有死按钮"/"没有异常"/"不出现 X"）⇒ **静默通过 —— 假绿**。
//
// 实测（2026-10-19）：`verify_all_buttons_e2e.js` / `verify_checkin_ui_e2e.js` 就是前者，
// 它们在**加登录之前**的那份 exe 上一直"绿"（裸 /ui 直接吐控制台），换到当前构建后
// 第一条断言 `真页面加载成功（暴露 applyManifest）` 立刻红，后面 24 条全是级联。
// 也就是说：**这类假绿之前没人发现，是因为被测的二进制本身是旧的。**
//
// # 判据（对每个"会打开 /ui"的探针文件）
//
//   (A) 带身份：`?token=` / `withToken(` / `withChrome(` / `WB2API_PROBE_TOKEN` / `Cookie` /
//       `session` / 由**显式 env 覆盖**传入 URL（如 `CLEAR_URL`）；**或**
//   (B) 有正向"控制台已加载"断言：`applyManifest` / `__wb2api__` / `consolePageMarker` /
//       页面长度阈值（`body.length > N`）/ 明显的控制台容器选择器。
//   二者有一个 ⇒ 过；都没有 ⇒ 红（并点名文件 + 给出改法）。
//
// ⚠ 这条判据是**静态**的：它判"有没有自证能力"，不判"事实进没进控制台"。后者要真跑（`run_e2e.js`）。
//   两条互补：静态这条便宜、能一次扫全仓；动态那条贵、只扫族。
//
// 用法：
//   node verify_probe_console_loaded.js            # 扫 tests/frontend
//   node verify_probe_console_loaded.js --self-test # 判别力自检（好桩绿 / 裸开+纯负向红）
const fs = require('fs');
const path = require('path');

const DIR = path.join(path.resolve(__dirname), '.');
const SELF_TEST = process.argv.includes('--self-test');

// 派生文件（生成器产物）不在此判据范围：它们是被生成的夹具，不是探针。
const DERIVED = /(_gen\.js|^tdz_probe\.html$)/;
// 共享层自己：它们**提供**身份机制，不受此判据约束。
const SHARED = /^(verify_probe_console_loaded\.js|lib_cdp\.js|lib_instance\.js|lib_authapi\.js|lib_fixtures\.js|lib_extract_check\.js)$/;
// 带理由的豁免（每一项都必须写明"为什么它**不需要**身份"，而且要过下面的自检：
// 豁免文件里**不许**出现对控制台内容的断言，否则豁免无效 ⇒ 判红）。
const ALLOW = {
  'instance_freshness.js': '只把 /ui 的**字节数**与磁盘 HTML 比，用来判"跑着的实例是不是当前源码编的"；'
    + '它刻意不带身份（两态都能量），且不对页面内容下任何断言 ⇒ 不受本判据约束。',
};
// 非探针（工具/入口/截图脚本）：仍扫，但只报警不判红（它们可能故意用别的方式取页面）
const TOOLISH = /^(run_e2e\.js|run_frontend_suites\.js|run_http_suites\.js|mutation_sweep\.js|shot_.*\.js|acceptance\.js|audit_.*\.js|diag_.*\.js|heat_.*\.js|final_visual\.js|flash_test\.js)$/;

const OPENS_UI = [
  /Page\.navigate['"]\s*,\s*\{[^}]*url:[^}]*\/ui/,          // 裸 CDP 导航（URL 字面量在 navigate 行上）
  /consoleHtmlAuthed\s*\(/,                                  // 共享层：自己完成换手
  /consoleSource\s*\(/,                                      // 共享层：取控制台源码
  /(?:http\.get|https\.get|fetch|request)\s*\([^)]*\/ui/,     // 原生 HTTP 取 /ui
];
// ⚠ 上面那几条会**漏掉**"URL 先赋值给变量再导航"的写法（例如
//   `const U = process.env.X || '…/ui'; await send('Page.navigate', { url: U });`）——
//   而漏报正是危险方向（假阴性 ⇒ 那条探针继续静默假绿）。所以补一条**更宽**的相关性判定：
//   文件"在**代码**里"提到 /ui、且确实有导航/取页动作 ⇒ 就算"会打开 /ui"，必须自证。
function opensUiLoose(src) {
  if (!/\/ui/.test(src)) return false;
  const acts = /Page\.navigate|consoleHtmlAuthed\s*\(|consoleSource\s*\(|http\.get|https\.get|fetch\s*\(|\brequest\s*\(/.test(src);
  if (!acts) return false;
  // 只在注释里提到 /ui 的不算（粗略去行注释：`// …` 之后的部分）
  const codeOnly = src.split(/\r?\n/).map((l) => l.replace(/(^|\s)\/\/.*$/, '$1')).join('\n');
  return /\/ui/.test(codeOnly);
}
const HAS_IDENTITY = [
  /consoleHtmlAuthed\s*\(/,   // ⚠ 它**本身就是**带身份的入口（内部完成 token 换手）—— 第一版漏登记，被自检抓好
  /withToken\s*\(/,
  /withChrome\s*\(/,
  /\?token=/,
  /WB2API_PROBE_TOKEN/,
  /Cookie/,
  /wb2api_/,
  /CLEAR_URL/,
  /LOGIN_URL/,
];
// "只有 env 覆盖、默认值裸开" ⇒ 默认跑法会落到登录页（警告级，不判红）
const ENV_ONLY_BARE = /process\.env\.[A-Z_]+\s*\|\|\s*['"][^'"]*\/ui['"]/;
const PROVES_CONSOLE = [
  /applyManifest/,
  /__wb2api__/,
  /consolePageMarker/,
  /body\.length\s*>\s*\d{5,}/,
  /\.length\s*>\s*1[0-9]{5}/,
  /#loginForm|#loginPage|login-panel/,
];
// 豁免自检用：这些出现即说明该文件**确实在断言控制台内容** ⇒ 不许豁免
const ASSERTS_CONSOLE = [/applyManifest/, /__wb2api__/, /querySelector\(['"]#pbody/, /panel_key|setPanel/];

function classify(src) {
  const opensUi = opensUiLoose(src) || OPENS_UI.some((r) => r.test(src));
  if (!opensUi) return { relevant: false };
  const id = HAS_IDENTITY.filter((r) => r.test(src)).length;
  const pf = PROVES_CONSOLE.filter((r) => r.test(src)).length;
  return { relevant: true, identity: id, proof: pf, envOnly: ENV_ONLY_BARE.test(src), ok: id > 0 || pf > 0 };
}
// 豁免是否成立：文件里**不许**有对控制台内容的断言（有 ⇒ 它其实会假绿 ⇒ 豁免无效）
function allowIsValid(src) { return !ASSERTS_CONSOLE.some((r) => r.test(src)); }

// ---------------------------------------------------------------- 自检
if (SELF_TEST) {
  let bad = 0;
  const t = (name, src, wantOk) => {
    const r = classify(src);
    const got = r.relevant && r.ok;
    const pass = got === wantOk;
    console.log((pass ? '  PASS ' : '  FAIL ') + name + '（期望 ' + (wantOk ? '过' : '红') + '，实际 ' + (got ? '过' : '红') + '）');
    if (!pass) bad++;
  };
  // 好桩 1：带令牌导航
  t('好桩①：Page.navigate 带 withToken', "await send('Page.navigate', { url: withToken(BASE + '/ui', key) });\nok(x, '真页面加载成功');", true);
  // 好桩 2：裸导航但**有正向自证**
  t('好桩②：裸导航 + applyManifest 自证', "await send('Page.navigate', { url: BASE + '/ui' });\nconst b = await ev('!!window.__wb2api__.applyManifest');", true);
  // 好桩 3：共享层换手取源码
  t('好桩③：consoleHtmlAuthed 取页', "const h = await lib.consoleHtmlAuthed(BASE + '/ui', key);", true);
  // 坏桩 1：裸导航 + 纯负向断言（= 实测到的那两条探针的形态）
  t('坏桩①：裸导航 + 只做负向断言', "await send('Page.navigate', { url: BASE + '/ui' });\nok(errs.length === 0, '没有未捕获异常');\nok(!hasDeadButton, '没有死按钮');", false);
  // 坏桩 2：原生 http 取 /ui 且无凭据
  t('坏桩②：http.get /ui 无凭据', "http.get(BASE + '/ui', (r) => { ok(r.body.includes('x'), '断言'); });", false);
  // 豁免自检：豁免文件里**不许**出现控制台内容断言
  {
    const exempt = "const r = http.get(BASE + '/ui'); console.log('字节数 ' + r.body.length);";
    const bad2 = "const r = http.get(BASE + '/ui'); ok(!!window.__wb2api__.applyManifest, '控制台在');";
    const ok1 = allowIsValid(exempt), ok2 = allowIsValid(bad2);
    console.log((ok1 && !ok2 ? '  PASS ' : '  FAIL ') + '豁免自检：只比字节数的可豁免（' + ok1 + '）／断言控制台内容的不许豁免（' + !ok2 + '）');
    if (!(ok1 && !ok2)) bad++;
  }
  // "env 覆盖但默认裸开"要能被识别为警告
  {
    const w = "const U = process.env.CLEAR_URL || 'http://127.0.0.1:18080/ui';\n"
      + "await send('Page.navigate', { url: U });";
    console.log((classify(w).envOnly ? '  PASS ' : '  FAIL ') + '警告级：env 覆盖但默认值裸开能被识别');
    if (!classify(w).envOnly) bad++;
  }
  console.log('\n' + (bad === 0 ? '=== 自检通过：好桩绿 / 裸开+纯负向红 ===' : '=== ' + bad + ' 项自检失败 ==='));
  process.exit(bad ? 1 : 0);
}

// ---------------------------------------------------------------- 全仓扫描
const files = fs.readdirSync(DIR).filter((f) => /\.(js|html)$/.test(f) && !DERIVED.test(f) && !SHARED.test(f)).sort();
const hits = [];
const warns = [];
for (const f of files) {
  let src = '';
  try { src = fs.readFileSync(path.join(DIR, f), 'utf8'); } catch (_) { continue; }
  if (ALLOW[f]) {
    if (!allowIsValid(src)) hits.push({ f, identity: 0, proof: 0, tool: false, why: '豁免无效：该文件里出现了对控制台内容的断言' });
    continue;
  }
  const r = classify(src);
  if (!r.relevant) continue;
  if (!r.ok) hits.push({ f, identity: r.identity, proof: r.proof, tool: TOOLISH.test(f), why: '既无身份机制、也无"控制台已加载"的正向自证' });
  else if (r.envOnly) warns.push({ f, why: '身份只来自 env 覆盖，而默认值是**裸 /ui** ⇒ 默认跑法会落到登录页' });
}
const hard = hits.filter((h) => !h.tool);
console.log('=== 开 /ui 的探针必须能自证"进的是控制台" ===');
console.log('  扫描 ' + files.length + ' 个文件；硬红 ' + hard.length + ' 个；警告 ' + warns.length + ' 个'
  + '（工具类另计 ' + (hits.length - hard.length) + ' 个）');
for (const h of hits.sort((a, b) => (a.tool ? 1 : 0) - (b.tool ? 1 : 0) || a.f.localeCompare(b.f))) {
  console.log('  ' + (h.tool ? '⚠ 工具类' : 'FAIL 探针') + '  ' + h.f
    + '  （身份 ' + h.identity + ' / 正向自证 ' + h.proof + '）—— ' + (h.why || ''));
}
for (const w of warns) console.log('  ⚠ 警告    ' + w.f + '  —— ' + w.why);
if (hard.length) {
  console.log('\n  改法（二选一）：');
  console.log('   1) 带身份开页：用 lib_cdp 的 withChrome（它内部就 withToken）或 withToken(BASE + \'/ui\', inst.key)；');
  console.log('   2) 加正向自证：开页后先断言控制台外壳真的在（window.__wb2api__.applyManifest 存在 / 页面长度阈值），');
  console.log('      再往下做负向断言 —— 否则负向断言会在**登录页**上静默通过（假绿）。');
}
console.log('');
console.log(hard.length === 0 ? '=== 通过：开 /ui 的探针都有自证能力 ===' : '=== ' + hard.length + ' 个探针缺少自证能力 ===');
process.exit(hard.length ? 1 : 0);
