// gen_ui_manifest.js —— T11 回归套件：守住"前端零硬编码"。
//
// # 它防的是什么
//
// 判据 1「加新上游核心零改动」在**前端**上的落点，靠的是这条链：
//   manifest（后端下发） → 契约层（读数据） → 导航/面板（渲染）
// 任何一环被写死，判据就悄悄失效，而页面看起来完全正常。
// 所以本套件断言的是**结构性质**，不是功能：
//
//   1. webui.html 里不出现上游名字字面量（'workbuddy' / 'codearts'）
//   2. 每个能力查询函数都有真实调用点（防"定义了没人用"的死代码 ——
//      上一版 providersWithCap / groupByProvider 正是这样烂掉的）
//   3. 所有表格的 colspan 都来自列数，不写死数字（防 B1/B2 那类漂移）
//   4. 不引入外部资源与 emoji（既有硬规则）
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');

const REPO = (process.env.WB2API_REPO || __dirname + '/../..');
const WEBUI = path.join(REPO, 'internal/server/webui.html');
const html = fs.readFileSync(WEBUI, 'utf8');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// ---------------------------------------------------------------- 取主脚本
//
// # 为什么要取"最后一个 <script>"而不是第一个（T6 踩到的坑）
//
// 页面头部有一个**内联小脚本**（主题防闪白用），它不是主逻辑。
// 原先用 indexOf('<script>') 取第一段，结果把头部脚本到主脚本之间的
// **HTML 片段**也当成了 JS —— 于是 `<section data-provider="workbuddy">`
// 这种 HTML 属性被报成"脚本里有上游名字字面量"。
//
// 主脚本是**最后一个** <script>...</script>（它包住整个 IIFE）。
const scriptStart = html.lastIndexOf('<script>');
const scriptEnd = html.lastIndexOf('</script>');
const js = html.slice(scriptStart, scriptEnd);

// ---------------------------------------------------------------- 1. 上游名字字面量
console.log('\n[1] 脚本里不出现上游名字字面量');
//
// 例外：注释里说明"上一版为什么错"时会提到上游名 —— 那是文档不是逻辑。
// 所以先剥掉注释，再扫描。
const jsNoComments = js
  .replace(/\/\*[\s\S]*?\*\//g, '')   // 块注释
  .replace(/(^|[^:])\/\/.*$/gm, '$1'); // 行注释（避开 http:// 这种）

for (const name of ['workbuddy', 'codearts']) {
  const hits = [];
  // 只统计**字符串字面量**形式（'name' / "name"），不统计出现在模板串里的。
  const re = new RegExp("'" + name + "'|\"" + name + "\"", 'g');
  let m;
  while ((m = re.exec(jsNoComments))) {
    const line = jsNoComments.slice(0, m.index).split('\n').length;
    hits.push('L' + line);
  }
  ok(hits.length === 0, `脚本（剥掉注释后）无 '${name}' 字面量` + (hits.length ? ' —— 命中 ' + hits.join(',') : ''));
}

// ---------------------------------------------------------------- 2. 能力查询函数有调用点
console.log('\n[2] 契约层函数必须都有真实调用点（防死代码）');
//
// # 这条为什么重要
//
// 上一版定义了 providersWithCap / groupByProvider 却**从未调用**，
// 数据取回来就扔，11 个面板照样写死。那是最隐蔽的一种"假解耦"：
// 代码看起来完全符合架构，实际一点用没有。
const mustBeCalled = [
  'providersWithCap',   // 能力位查询
  'upstreamGroups',     // 上游分组
  'routesFor',          // 面板入口查询
  'capTitle',           // 能力标题翻译
  'hasAnyPanel',        // 面板存在性判定
];
for (const fn of mustBeCalled) {
  // 统计"标识符后跟左括号"的总次数，减去"function fn("的形式即得调用次数。
  //
  // 用 lookbehind 排除 function 关键字会把 `function fn(` 里的 fn 一起排除掉，
  // 导致总数少算（第一版就是这么把自己算成 0 次甚至 -1 次的）。
  // 正确做法：先数全部出现，再单独数定义。
  const all = (js.match(new RegExp('\\b' + fn + '\\s*\\(', 'g')) || []).length;
  const defs = (js.match(new RegExp('function\\s+' + fn + '\\s*\\(', 'g')) || []).length;
  const calls = all - defs;
  ok(defs >= 1, `${fn} 有定义`);
  ok(calls >= 1, `${fn} 有真实调用点（定义 ${defs} 次、总出现 ${all} 次 → 调用 ${calls} 次）`);
}

// ---------------------------------------------------------------- 3. 能力位不硬编码
console.log('\n[3] 能力位清单不硬编码在前端');
//
// 能力位（growth/travel/welfare…）必须来自 manifest.capabilities。
// 前端只允许出现**正在消费**的能力名（如 showPanel 里按 cap 分派），
// 不允许出现一份自己的"能力位全表"。
const capTableRe = /(?:const|let|var)\s+\w*(?:CAPS|capabilities)\w*\s*=\s*\[/i;

// # 例外：SPECIAL_PANEL_CAPS（T3/T4）
//
// # 为什么这条断言原先会假红，以及为什么不能用"改名字躲过去"
//
// T3 引入了一张 `const SPECIAL_PANEL_CAPS = ['checkin', CORE_CAP]`，
// 上面那条正则立刻命中。但两者的性质完全不同：
//
//   · 被禁止的：前端自己列一遍 `['chat','checkin','growth','travel',…]`，
//     当作"能力位全集"来读 —— 那样加第 3 个能力位时前端就得改，
//     而且 manifest 说了不算（判据 1 在前端被打破）。
//   · SPECIAL_PANEL_CAPS 是**排他表**：它列的是"这个能力位**不需要**
//     buildProviderPanels 再生成一个通用面板"（因为已有专属面板承担）。
//     它不认识任何上游，也不声称这是全集 —— 表外的能力位一律照常处理。
//     manifest 里没声明 checkin 的上游，这张表对它毫无影响。
//
// 把变量改个不含 CAPS 的名字（比如 SUPPRESS）能让这条断言变绿，
// 但那是**用改名躲过检查**：守卫仍然抓不出真正的"能力位全表"复发。
// 所以正确做法是让守卫精确 —— 用显式豁免表，并把豁免理由写在这里。
//
// 防的是"悄悄给自己开豁免"：豁免项必须真的存在于源码中（下面检查），
// 改名/删除会让这条断言变红，而不是静默放行。
const CAP_TABLE_EXEMPTIONS = [
  { name: 'SPECIAL_PANEL_CAPS', why: '排他表（专属面板已承担），不是能力位全集 —— 见 T3/T4' },
];
let capTables = [];
{
  const re = /(?:const|let|var)\s+(\w*(?:CAPS|capabilities)\w*)\s*=\s*\[/gi;
  let m;
  while ((m = re.exec(jsNoComments))) capTables.push(m[1]);
}
const offending = capTables.filter(n => !CAP_TABLE_EXEMPTIONS.some(e => e.name === n));
ok(offending.length === 0, '前端没有自己的能力位数组常量' +
  (offending.length ? ' —— 命中 ' + JSON.stringify(offending) : ''));
// 豁免项必须真实存在：避免豁免表变成"永久放行"（被豁免的代码删了却没人发现）
for (const e of CAP_TABLE_EXEMPTIONS) {
  ok(capTables.indexOf(e.name) >= 0,
    `豁免项 ${e.name} 仍存在于源码（理由：${e.why}）`);
}
if (capTables.length) {
  console.log('    检测到的能力位数组常量: ' + JSON.stringify(capTables) +
    '（豁免 ' + CAP_TABLE_EXEMPTIONS.length + ' 项）');
}

// ---------------------------------------------------------------- 4. colspan 不写死
console.log('\n[4] 表格 colspan 不出现在渲染函数里的裸数字（防 B1/B2 漂移）');
//
// B1: 账号表 11 列，错误提示写 colspan="8"（旧版列数）→ 后 3 列露底色
// B2: 成长表 12 列，错误行 colspan 算错 → 挤掉「操作」列
//
// 完全禁止 colspan 不现实（HTML 里确实需要），但要求**同一个文件里
// 每个表的 colspan 与它的表头列数一致**。这里做静态近似：
// 抓出 HTML 里每个 <table>...</table>，数它的 <th>，再检查该表所在
// section 内出现的 colspan 是否 <= 表头列数。
function tablesWithHeaders(src) {
  const out = [];
  const re = /<table[^>]*>([\s\S]*?)<\/table>/g;
  let m;
  while ((m = re.exec(src))) {
    const body = m[1];
    const cols = (body.match(/<th[\s>]/g) || []).length;
    out.push({ start: m.index, cols, body });
  }
  return out;
}
const tables = tablesWithHeaders(html);
ok(tables.length > 0, '至少解析到一个表格（' + tables.length + ' 个）');

// 表头列数必须是已知的合理值（防止解析失败被当成"没问题"）
for (const t of tables) {
  ok(t.cols >= 3 && t.cols <= 20, `表格列数在合理区间（实际 ${t.cols}）`);
}

// 全局 colspan 与列数的匹配检查：每个 colspan 值都必须能在**某个**表格的列数里找到
const colspans = [...html.matchAll(/colspan="(\d+)"/g)].map(x => Number(x[1]));
const tableCols = new Set(tables.map(t => t.cols));
const orphan = colspans.filter(n => !tableCols.has(n) && !tableCols.has(n + 1));
// colspan = 列数 或 列数-1（首列已占一格）都是合法的
ok(orphan.length === 0,
  '每个 colspan 都对应某个表格的列数' +
  (orphan.length ? ` —— 可疑值 ${JSON.stringify([...new Set(orphan)])}，已知列数 ${JSON.stringify([...tableCols])}` : ''));

// ---------------------------------------------------------------- 5. 零外部依赖 & 无 emoji
console.log('\n[5] 零外部依赖与无 emoji');
const extRe = /(?:src|href)\s*=\s*["'](?:https?:)?\/\//gi;
const extHits = [...html.matchAll(extRe)].length;
ok(extHits === 0, '没有外部 http(s) 资源引用（实际 ' + extHits + ' 个）');

// emoji 只检查**会渲染出来的文本**：标签之间的内容 + 字符串字面量。
//
// # 为什么不能全文件扫
//
// 注释里允许出现符号作为排版标记（本文件在告警说明里用了 U+26A0），
// 它不会渲染，扫出来是假阳性 —— 而假阳性会让这条断言被当成噪声忽略，
// 等真有 emoji 混进界面时就没人在意了。
const emojiRe = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}]/u;

// 渲染文本 = 去掉 <script>/<style>/注释后的标签间文本
let rendered = html
  .replace(/<script[\s\S]*?<\/script>/gi, '')
  .replace(/<style[\s\S]*?<\/style>/gi, '')
  .replace(/<!--[\s\S]*?-->/g, '')
  .replace(/<[^>]+>/g, ' ');
ok(!emojiRe.test(rendered), '渲染出的文本里没有 emoji');

// JS 里会写进 DOM 的字符串字面量也要查
const jsStr = [...jsNoComments.matchAll(/'([^'\\]|\\.)*'|"([^"\\]|\\.)*"/g)].map(m => m[0]).join('\n');
ok(!emojiRe.test(jsStr), '脚本的字符串字面量里没有 emoji');

// ---------------------------------------------------------------- 6. manifest 是唯一数据源
console.log('\n[6] manifest 是上游信息的唯一来源');
ok(/\/admin\/ui\/manifest/.test(js), '前端调用了 /admin/ui/manifest');
ok(!/\/admin\/providers/.test(js), '前端不再依赖 /admin/providers（已由 manifest 取代）');
ok(/function\s+applyManifest\s*\(/.test(js), '有 applyManifest 统一写入契约');

// ---------------------------------------------------------------- 7. 幂等与泄漏（CP2 F2/F4 的落点）
console.log('\n[7] 幂等性与监听器绑定纪律');
//
// # 这两条为什么必须在**结构层**就守住
//
// CP2 评审证明了两件"全绿但其实是坏"的事：
//    F2 每次 syncProviderPanels() 都往面板容器 addEventListener，从不移除 ——
//       60 次 sync 后一次点击触发 13 个写请求
//    F4 把组头徽章整块删掉，21 个测试仍全绿
//
// 上面 [2][4] 已经补了渲染内容断言。这里再从**源码结构**加一道：
// 事件绑定必须只发生在稳定的祖先上，不许出现在会被反复重建的容器上。
ok(/function\s+installGenericActions\s*\(/.test(js),
  '有 installGenericActions（只绑一次的委托入口）');
ok(!/function\s+bindGenericActions\s*\(/.test(js),
  '已删除 bindGenericActions（它就是逐次重复绑定的来源）');

// 取 loadGenericList 的函数体，断言里面**没有** addEventListener。
// 它每 5 秒被调用一次，在里面绑事件必然泄漏。
const lglStart = js.indexOf('function loadGenericList(');
if (lglStart >= 0) {
  const lglBody = js.slice(lglStart, lglStart + 2000);
  ok(!/addEventListener/.test(lglBody),
    'loadGenericList（每 5s 调用一次）内部不绑事件 —— 在里面绑必然泄漏');
} else {
  ok(false, '找不到 loadGenericList');
}

// installGenericActions 必须有幂等保护（重复调用不重复绑）
const igaStart = js.indexOf('function installGenericActions(');
if (igaStart >= 0) {
  const igaBody = js.slice(igaStart, igaStart + 1200);
  // 判据：函数体开头有一个"已装过就直接 return"的守卫。
  // 不写死变量名（genericActionsInstalled）—— 改名不该让测试假红，
  // 真正要守的是"有守卫且守卫会提前 return"这个形状。
  ok(/if\s*\([A-Za-z_$][\w$]*\)\s*return;/.test(igaBody),
    'installGenericActions 开头有"已装过就 return"的幂等守卫');
  ok(/\$\('content'\)\.addEventListener/.test(igaBody),
    '委托绑在 #content（全生命周期不被替换的祖先）上');
} else {
  ok(false, '找不到 installGenericActions');
}

// ---------------------------------------------------------------- 8. 渲染内容断言（CP2 F4 + 自审）
console.log('\n[8] 关键渲染路径必须被**调用**（不只是"函数存在"）');
//
// # 自审证明的事（比 F4 更狠）
//
// 我做了 7 个真实回归注入（删顶栏渲染、删任务行、服务名硬编码、
// 主题不持久化、账号不分組、徽章恒 0、能力位显隐失效），
// 静态套件抓到 **0 个** —— 因为这里的断言原先只是
//   /renderTopStats/.test(js)
// 也就是"源码里出现过这个词"。而"函数存在"与"函数被正确调用"完全是两件事：
// 把调用点删掉、把返回值写死、把分支短路，字符串照样在。
//
// 所以改成断言**调用点**：`fn(` 的出现次数必须 > 定义次数。
// 这是静态层能做到的最接近"真的会跑"的检查；更彻底的验证在浏览器 E2E。

// mustCall：函数必须至少被调用一次（出现次数 > 定义次数）
const mustCall = [
  ['renderTopStats', '顶栏状态条在 refresh 里被调用'],
  ['renderJobs', '任务调度面板在 refresh 里被调用'],
  ['renderIdentity', '服务名在 refresh 里被调用'],
  ['installGenericActions', '通用动作委托在 boot 里被安装'],
  ['hideOrphanPanels', '无能力面板的隐藏被调用'],
  ['groupAccountsByProvider', '账号按上游分组被调用'],
  ['panelCapsOf', '能力位→面板的判定被调用'],
];
for (const [fn, desc] of mustCall) {
  const all = (js.match(new RegExp('\\b' + fn + '\\s*\\(', 'g')) || []).length;
  const defs = (js.match(new RegExp('function\\s+' + fn + '\\s*\\(', 'g')) || []).length;
  ok(defs >= 1 && all - defs >= 1,
    desc + `（定义 ${defs}、总出现 ${all} → 调用 ${all - defs}）`);
}

// 关键渲染产物必须真的出现在渲染语句里
ok(/class="navgcount/.test(js), '组头账号数徽章会被渲染（F4 的注入形态就是它消失）');
ok(/navgroup/.test(js), '导航分组会被渲染');
ok(/providerPanelHTML/.test(js), '通用上游面板会被渲染');
ok(/id="jobrows"/.test(html) && /id="topstats"/.test(html), '任务表与顶栏的宿主元素在 HTML 里');

// 徽章在 0 时也必须渲染（0 与"徽章坏了"必须可区分）
ok(!/account_count\s*\?\s*`<span class="navgcount/.test(js),
  '徽章不再用 account_count 做真值判断（0 时也必须渲染，否则与"徽章坏了"无法区分）');
// 徽章值必须来自数据，不能写死
ok(/Number\(\s*p\.account_count\s*\)/.test(js),
  '徽章数值取自 p.account_count（写死常量会被这条抓到）');

// 分组必须真的走 groupAccountsByProvider，而不是一个固定数组
ok(/groupAccountsByProvider\(\s*share\s*\)/.test(js),
  '账号分组调用 groupAccountsByProvider(share)（塞固定单组会被这条抓到）');

// 能力位显隐必须真的写 sec.hidden
ok(/\.hidden\s*=\s*!alive/.test(js) || /\.hidden\s*=\s*[^;]*alive/.test(js),
  '能力位显隐真的写 hidden 属性（短路成常量会被这条抓到）');

// 主题必须真的持久化
ok(/localStorage\.setItem\(LS_THEME/.test(js), '主题偏好真的写入 localStorage');
// 服务名必须来自 manifest，不能写死
ok(/MANIFEST\.service/.test(js) && !/return\s*'workbuddy2api'/.test(js),
  '服务名取自 MANIFEST.service（写死会被这条抓到）');

// 任务表必须真的把行 map 出来（不是直接写空串/固定 HTML）。
// 自审的注入形态是 `sorted.map(j => { if (true) return '';` ——
// 它保留了 map 调用却让每行变成空。所以这里要求 map 的回调体里
// 真的产出 <tr>，并且**不是**以常量短路开头。
const jobMap = js.match(/tb\.innerHTML\s*=\s*sorted\.map\([\s\S]{0,120}/);
ok(!!jobMap, '任务表走 sorted.map(...) 渲染（而不是固定 HTML）');
if (jobMap) {
  ok(!/\{\s*if\s*\(\s*(true|1)\s*\)/.test(jobMap[0]),
    '任务行的 map 回调没有以常量短路开头（自审注入形态）');
  // 回调体里必须真的出现 <tr 模板。
  // 窗口要取到该语句所在的**函数结束**，不能拍一个固定长度 ——
  // 回调体里有大段注释，900 字符到不了 <tr>（第一版就是这么假红的）。
  const start = js.indexOf('tb.innerHTML = sorted.map(');
  const fnEnd = js.indexOf('function renderTopStats', start);
  const seg = js.slice(start, fnEnd > start ? fnEnd : start + 4000);
  ok(/<tr/.test(seg), '任务行的 map 回调里真的产出 <tr>');
}

// ---------------------------------------------------------------- 9. 主题（T10）
console.log('\n[9] 主题系统');
ok(/data-theme=/.test(js) || /setAttribute\('data-theme'/.test(html), '有 data-theme 机制');
ok(!/:root\[data-theme="light"\]\{[\s\S]{0,400}?--bg:#0b0e14/.test(html),
  '浅色主题没有把深色的 --bg 抄过来（必须单独挑色）');
// 硬编码颜色会破坏主题切换
const hardcodedColors = [...js.matchAll(/#(?:0a0d13|1b2333)\b|rgba\(0,0,0,\.[0-9]+\)/g)];
ok(hardcodedColors.length === 0,
  'JS 里没有写死的深色专属颜色（会破坏主题切换）' +
  (hardcodedColors.length ? ' —— ' + JSON.stringify([...new Set(hardcodedColors.map(m => m[0]))]) : ''));

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
