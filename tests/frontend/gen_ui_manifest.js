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
// ⚠ 扫描必须跑在**剥过注释**的文本上 —— 这是本套件踩过的一个真坑。
//
// 注释里出现 `<table>` 这种字面量是完全正常的（本项目注释写得很长，
// 会拿 `<table>`、`<tr>` 当例子讲结构）。而 `/<table[^>]*>([\s\S]*?)<\/table>/`
// 会**从注释里的那个 `<table>` 开始**匹配，一路吃到后文真正的 `</table>`，
// 于是把一整段注释当成了"一张表" —— 实测（2026-09-14）：
// 注释里那句「一个 <table>，不能嵌套 div」被当成一张 0 列的表，
// 而它后面恰好跟着真正的动态表，两者被合并，得出一张 0 列的幽灵表。
//
// CRLF 也必须先归一化：行尾是 `\r` 时 `\/\/.*$` 匹配不上，剥注释会变成空操作
//（这条是 audit_dead_buttons.js 已经吃过一次的教训，这里照抄它的做法）。
function stripComments(s) {
  const lf = s.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  return lf
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .split('\n')
    .map(l => l.replace(/\/\/.*$/, ''))
    .join('\n');
}

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
// 表格扫描用剥注释后的文本；而 `<th>`/`colspan` 的字面量统计仍用原文
//（下面那些 matchAll 是在数**真正的字面量**，注释里的不算 —— 见各自的注释）。
const codeHtml = stripComments(html);
const tables = tablesWithHeaders(codeHtml);
ok(tables.length > 0, '至少解析到一个表格（' + tables.length + ' 个）');

// 表头列数必须是已知的合理值（防止解析失败被当成"没问题"）
//
// ⚠ 账号池那张表是**动态生成**的（`<thead>` 由 `cols.map(acctThHTML)` 拼出来），
// 所以它的静态 `<th>` 数量是 **0** —— 那不是"解析失败"，是"列数由运行期决定"。
// 把它一并按 >=3 判会得到一个**永远红**的假警报，而假警报的代价是
// 下一个人干脆把这段删掉 —— 那才是真正的守卫丢失。
//
// 所以：静态列数 >0 的表按区间判；0 列的表必须能证明它"由列集生成"。
let zeroColSeen = 0;
for (const t of tables) {
  if (t.cols === 0) {
    zeroColSeen++;
    // ⚠ 必须切 `codeHtml`，不能切 `html` —— t.start 是**剥注释后**的偏移，
    // 拿去索引原文会切到完全不相干的区域（原文比剥后长 11 万字符）。
    // 第一版就是切错了对象，导致断言永远红。
    const head = codeHtml.slice(t.start, t.start + 120);
    // ⚠ 判据别写死成 class="accttable"（2026-10-19 修假红）：
    //   动态表现在的开头是
    //     <table class="accttable accttable--aligned" data-acctpid="${esc(g.provider)}" …
    //   · `accttable--aligned`：1b05af3「所有表格改为自适应列宽」加的；
    //   · `data-acctpid`：b8859e9「每上游一张表 + 横向滚动容器」加的。
    //   旧写法要求 class 属性**恰好等于** accttable ⇒ 产品只要再挂一个修饰类就假红，
    //   而"这张表是谁"其实由两个特征共同确定，比单看一个类名更准：
    //     · class 以 accttable 起头（账号池家族）
    //     · 带 data-acctpid=…（只有 JS 生成的那张每上游表才有；静态表没有）
    ok(/^<table class="accttable[ "][^>]*data-acctpid=/.test(head),
      '0 列的表只能是账号池那张动态表（accttable 前缀 + data-acctpid 两者都要有；实际头部 '
      + JSON.stringify(head.slice(0, 80)) + '）');
    continue;
  }
  ok(t.cols >= 3 && t.cols <= 20, `表格列数在合理区间（实际 ${t.cols}）`);
}
ok(zeroColSeen <= 1, '最多只有一张动态表（实际 ' + zeroColSeen + '）');
// 防空跑锚：上面那条"0 列的表只能是账号池动态表"只在**真扫到 0 列表**时才执行。
// 哪天动态表被改成静态表（或扫描口径再变），zeroColSeen 会变成 0，
// 那半段判据就成了一句空话 —— 而套件照样报绿。所以要求它必须 >= 1。
ok(zeroColSeen >= 1,
  '动态表（静态 0 列、列数由列集在运行期决定）仍被扫到 —— 否则上一条是空跑（实际 ' + zeroColSeen + '）');

// ⚠ 账号池的表**不在静态 HTML 里**了（拆成"每上游一张表"后由 JS 生成），
// 所以上面的静态扫描**看不到它** —— 这正是"守卫随重构静默变弱"的形态：
// 旧版这里靠静态那张表的 11 列兜底，现在那条兜底消失了，而扫描**照样绿**。
//
// 补一条针对动态表的判据：账号池的表头必须由**列集**生成，
// 不许写死列数或列名。判据落在源码文本上（这是静态套件，跑不了浏览器）。
{
  const dyn = html.match(/<table class="accttable">[\s\S]{0,200}/);
  ok(!!dyn, '账号池的动态表仍在源码里（找不到说明表结构又变了，本节的判据需要同步）');
  // 表头必须由 cols.map(...) 生成 —— 写死一串 <th> 会让"每上游不同列集"失效
  ok(/cols\.map\([^)]*acctThHTML/.test(html),
    '账号池表头由列集 `cols.map(acctThHTML)` 生成（写死 <th> 会让每上游不同列集失效）');
  // 且不许出现写死的列数
  ok(!/ACCT_COLS\s*=\s*11\b/.test(html),
    'ACCT_COLS 不再写死 11（它已收窄为"默认列集长度"，改回常量会让 codearts 的占位行跨错列数）');
}

// 全局 colspan 与列数的匹配检查：每个 colspan 值都必须能在**某个**表格的列数里找到。
//
// ⚠ 必须在 codeHtml（剥过注释）上数，不能在原文上数（2026-10-19 修假红）：
//   本节上面的注释早就写明"注释里的不算"，但这一行原先是在**原文**上 matchAll。
//   实测：webui.html:8461 有一句说明文字「原先写死 colspan="11"，而那行只有 1 个
//   前置 <td> + 11 = 12 格…」—— 那是文档，原文扫出来就变成一个凭空的可疑值 11。
//   把文档当代码读 = 假红；本项目已经因为这类事吃过不止一次亏。
const colspans = [...codeHtml.matchAll(/colspan="(\d+)"/g)].map(x => Number(x[1]));
const tableCols = new Set(tables.map(t => t.cols));

// 「占满整行」哨兵：colspan="99" 这类**故意超出列数**的写法。
//
// # 为什么它不是 B1/B2 那类缺陷（webui.html:6920-6925）
//   panelRowErr 一行要在**两个列数不同的面板**里通用（查失败的行，见该函数注释
//   "列数用 colspan 吃掉，两个面板通用"），于是作者用 colspan=99 一次吃掉剩余格数。
//   它**不依赖列数**，所以不会随列数漂移 —— 而 B1/B2 的形态恰恰相反：写死的旧列数
//   （11 / 8）在列数变化后跨错列，露底色或挤掉一列。
//   ⇒ 必须豁免，但豁免要**显式 + 可自检**，不能靠"值很大就当没事"糊过去：
//     ① 每个哨兵都必须真的还在源码里用着（否则这条豁免该删掉，而不是永不过期）；
//     ② 哨兵必须大到不可能被误当成真实列数（列数区间是 3..20，见上面那条断言）。
const COLSPAN_ALL_SENTINELS = [
  { value: 99, why: '「占满整行」哨兵（panelRowErr，两个列数不同的面板通用 —— webui.html:6925）' },
];
const sentinelValues = new Set(COLSPAN_ALL_SENTINELS.map(s => s.value));
const orphan = colspans.filter(n => !sentinelValues.has(n) && !tableCols.has(n) && !tableCols.has(n + 1));
// colspan = 列数 或 列数-1（首列已占一格）都是合法的
ok(orphan.length === 0,
  '每个 colspan 都对应某个表格的列数（或是已登记的行哨兵）' +
  (orphan.length ? ` —— 可疑值 ${JSON.stringify([...new Set(orphan)])}，已知列数 ${JSON.stringify([...tableCols])}，已登记哨兵 ${JSON.stringify([...sentinelValues])}` : ''));
for (const s of COLSPAN_ALL_SENTINELS) {
  ok(colspans.includes(s.value), `行哨兵 colspan="${s.value}" 仍在使用（${s.why}）`);
  ok(s.value > 20, `行哨兵 colspan="${s.value}" 大于列数上限 20 —— 它只表示"占满"，不能被当成真实列数`);
}
// 防空跑锚：colspans 为空时上面那条判据恒真
ok(colspans.length > 0, '确实扫到了 colspan 字面量（否则上一条是空跑）—— 实际 ' + colspans.length + ' 处');

// ---------------------------------------------------------------- 5. 零外部依赖 & 无 emoji
console.log('\n[5] 零外部依赖与无 emoji');
const extRe = /(?:src|href)\s*=\s*["'](?:https?:)?\/\//gi;
const extHits = [...html.matchAll(extRe)].length;
ok(extHits === 0, '没有外部 http(s) 资源引用（实际 ' + extHits + ' 个）');

// ============================== emoji：两层强度 ==============================
//
// # 规则原文与它的范围（出处不止原位注释那两行）
//   webui.html:124   /* 选中指示条：纯 CSS 画，不引入任何图标字体或 emoji */
//   webui.html:5709  // 纯文字 + CSS 选中指示条，不使用任何图标字体或 emoji。
//   .task/frontend-decouple/protocol.md:27、两份 SOUL.md、docs/progress-log.md:606
//   mutation-sweep-3.md:10 的 S2 变异（注入 ✅）就是照这条写的
// ⇒ "无 emoji" 在本仓库是**全仓库**规则；既有三条分区级守卫
//   （dashboard_gen / gen_settings_layout / nav_e2e）只是**覆盖面**没铺满，不是口径更窄。
//
// # 两层强度，在同一份源码上同时跑
//   第 1 层【图标位，无条件红】<nav> / <h1>-<h3> / <th> / <button> 的**可见文本**：
//     出现 emoji 一律红，**不进任何登记表** —— 这些位置正是规则原文说的"图标位"。
//     顺手把 nav_e2e 只扫导航的覆盖面补齐到 <th> / <h2>；静态标记与 JS 拼出来的标记都扫
//     （本产品多数表头/按钮是 JS 拼的，只扫静态 HTML 等于没扫）。
//   第 2 层【散文位，必须来自登记表】提示 / 说明 / toast / tooltip / 属性文本。
//
// # 为什么登记表**故意留空**（不是"还没填"）
// 2026-10-19 产品侧已按这条规则清理干净（webui.html 18 处替换、语义不丢：
//   '已复制 ✓'→'已复制'、'完成 ✅ 稍候…'→'完成，稍候…'、'⚠ 别填错'→'注意…'、
//   '⚡${g.energy}'→'能量${g.energy}'、列头 title「⚡后是该账号的能量」→「「能量N」…」…）。
// 此时开白名单**只会把规则放松**：一旦登记了 ⚡/✅，以后新写的散文位 emoji 就有了先例可援。
// 空表 = 现在谁都进不来；而将来真有人登记，那条登记自己还要过"仍在使用"自检
// （登记了却没人用 = 防线腐烂，同样红）⇒ 下一个人看到的是"规则 + 违规已清"，不是"规则 + 白名单"。
const RENDER_MARKER_EXEMPT = {
  // 空。要往里加，请写清"这个记号表达什么语义 / 为什么纯文字代替不了"，并保证源码里仍在使用。
};

// ---- 等长挖空工具：保留每个字符的位置，行号才可信（见下面两条坑）----
const blankOut = s => s.replace(/[^\n]/g, ' ');
const maskRules = (src, rules) => rules.reduce((o, re) => o.replace(re, mm => blankOut(mm)), src);
const blankRanges = (src, ranges) => ranges.reduce((o, r) => o.slice(0, r[0]) + blankOut(o.slice(r[0], r[1])) + o.slice(r[1]), src);

// 脚本正文（注释按原长度挖空）
//
// 坑 1：不能"剥掉"注释再算行号 —— 长度一变行号整体前移（📱 在 5368 会被报成 3694）。
//       行号是给人去改产品的，错行号比没有行号更坏。
// 坑 2：块注释的 `/*` 前不能是标识符字符。否则**注释散文里的路径**会被当成注释开始：
//       注释里写着「（/admin/* 要鉴权）」⇒ `/admin/*` 的 `/*` 一路吃到几百行后的 `*/`，
//       实测**吞掉 30KB 真代码**（html 8074..8811），连带把当时 8520 那行的 ⚡ 也吞了
//       ⇒ 产品有 emoji 而套件**假绿**。这是判据自身的洞，比那十几处 emoji 严重得多。
//       真代码里 `/*` 前几乎不可能是标识符字符（`a/*x*/b` 这种写法本项目没有），
//       所以这条约束挡住路径、不误伤真注释（真注释要么在行首、要么前面是空白/括号/分号）。
function maskComments(s) {
  // 块注释：整段换成等长空格（保留其中的换行）；`/*` 前不得是 [A-Za-z0-9_$]
  let out = s.replace(/(^|[^\w$])\/\*[\s\S]*?\*\//g,
    (mm, p1) => p1 + mm.slice(p1.length).replace(/[^\n]/g, ' '));
  // 行注释：逐行挖空（避开 http:// 这类带冒号的写法，与 stripComments 同一约定）
  out = out.split('\n').map(l => {
    const m = /(^|[^:])\/\//.exec(l);
    if (!m) return l;
    const at = m.index + (m[0].startsWith('/') ? 0 : 1);
    return l.slice(0, at) + ' '.repeat(l.length - at);
  }).join('\n');
  return out;
}
const codeMasked = maskComments(js);
ok(codeMasked.length === js.length,
  '挖空注释不改变脚本长度（长度变了行号就会漂）—— ' + codeMasked.length + ' vs ' + js.length);

// ---- 仪器自检：给上面"坑 2"那个假绿洞上锁 ----
// 坏桩：注释散文里的 `/admin/*`，**它后面**是真代码里的 emoji。
//   用朴素 /\*[\s\S]*?\*\// 跑这条桩，那个 `/*` 会被当成注释开始、一路吃到行尾的 `*/`，
//   把中间的真代码连同 emoji 一起吞掉 ⇒ 桩里的 emoji 消失 = 假绿。
//   ⚠ 桩里**必须存在一个更靠后的 `*/`**：否则朴素正则配不上对、根本吞不了东西，这条桩就废了。
const STUB_BAD = 'const a = 1; // 见（/admin/* 要鉴权）\nconst b = `🔥`;\nconst c = 2; /* 后文某处的真注释 */';
ok(/\u{1F525}/u.test(maskComments(STUB_BAD)),
  '坏桩：注释里的 /admin/* 之后，真代码里的 emoji 必须留下来（否则就是那个吞 30KB 真代码的假绿洞）');
const STUB_OK = '/* 真注释里的 🔥 不算数 */\nconst c = 1;';
ok(!/\u{1F525}/u.test(maskComments(STUB_OK)),
  '正桩：真块注释里的 emoji 必须被挖掉（注释不渲染，扫出来是假阳性）');

const symGlobal = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}]/gu;
// 记号 → 出现位置（带 webui.html 的真实行号）
function symbolSites(text, baseOffset, origin) {
  const out = [];
  const re = new RegExp(symGlobal.source, 'gu');
  let m;
  while ((m = re.exec(text))) {
    out.push({
      cp: 'U+' + m[0].codePointAt(0).toString(16).toUpperCase(),
      ch: m[0],
      at: (baseOffset || 0) + m.index,
      line: html.slice(0, (baseOffset || 0) + m.index).split('\n').length,
      origin,
    });
  }
  return out;
}
const describeHits = list => list.map(h => 'webui.html:' + h.line + ' ' + h.cp + ' ' + h.ch + ' [' + h.origin + ']').join('；');

// ========================= 第 1 层：图标位（无条件红） =========================
const ICON_TAGS = ['nav', 'h1', 'h2', 'h3', 'th', 'button'];
function iconZones(src, base, origin) {
  const out = [];
  for (const tag of ICON_TAGS) {
    const re = new RegExp('<' + tag + '\\b[^>]*>[\\s\\S]*?<\\/' + tag + '>', 'gi');
    let m;
    while ((m = re.exec(src))) {
      out.push({
        tag, origin, base,
        srcStart: m.index,
        srcEnd: m.index + m[0].length,
        // 只留可见文本（把标签等长挖成空格 ⇒ 偏移仍与源对齐，行号照样准）
        textMasked: m[0].replace(/<[^>]+>/g, mm => blankOut(mm)),
      });
    }
  }
  return out;
}
// 静态标记：先把 <script>/<style>/HTML 注释按长度挖空（保住偏移）
const htmlStripped = maskRules(html, [
  /<script[\s\S]*?<\/script>/gi,
  /<style[\s\S]*?<\/style>/gi,
  /<!--[\s\S]*?-->/g,
]);
const zones = [
  ...iconZones(htmlStripped, 0, '静态标记'),
  ...iconZones(codeMasked, scriptStart, 'JS 拼出来的标记'),
];
const zoneCount = {};
for (const z of zones) zoneCount[z.tag] = (zoneCount[z.tag] || 0) + 1;
// 防空跑锚：容器要真的扫到（扫不到 = 第 1 层是一句空话）
ok(zoneCount.th > 0 && zoneCount.h2 > 0 && zoneCount.nav > 0 && zoneCount.button > 0,
  '图标位容器确实扫到了（否则第 1 层是空跑）—— ' + JSON.stringify(zoneCount));
// 仪器健康锚：单区跨度异常大 = 标签配对配错了（比如漏写 </h2> 一路吃到别的区块），
// 那会让"图标位"把散文吸进来 ⇒ 先看配对，别急着改判据。（实测最长 2288，是账号池标题栏本身）
const maxZone = zones.reduce((n, z) => Math.max(n, z.textMasked.length), 0);
ok(maxZone < 4000, '图标位容器的最大跨度正常（异常大 = 标签配对配错了，先查 </h2>/</th> 是否漏写）—— 实际 ' + maxZone);

const seenIcon = new Set();
const iconHits = [];
for (const z of zones) {
  for (const h of symbolSites(z.textMasked, z.base + z.srcStart, '图标位<' + z.tag + '>/' + z.origin)) {
    if (seenIcon.has(h.at)) continue;   // 嵌套容器（<button> 在 <th> 里）会重复命中，去重
    seenIcon.add(h.at);
    iconHits.push(h);
  }
}
ok(iconHits.length === 0,
  '[icon位] 导航/分区标题/列头/按钮文本里没有 emoji（规则原文：不引入图标字体或 emoji）'
  + (iconHits.length ? ' —— 命中 ' + iconHits.length + ' 处：' + describeHits(iconHits) : ''));

// ===================== 第 2 层：散文位（必须来自登记表） =====================
// 三个来源：
//   ① 静态可见文本（图标位容器整段挖掉，避免与第 1 层重复计数）
//   ② JS 正文（注释已挖空；JS 里拼出来的图标位标记也挖掉）
//   ③ 可渲染属性 title / placeholder / aria-label / alt
//      ⚠ 属性归**散文位**：列头的 title 是**工具提示**、不是列头文本本身
//        （1504 那处「…，⚡后是该账号的能量」就按散文位处理）。
const htmlTextMasked = htmlStripped.replace(/<[^>]+>/g, mm => blankOut(mm));
const htmlProseText = blankRanges(htmlTextMasked,
  zones.filter(z => z.origin === '静态标记').map(z => [z.srcStart, z.srcEnd]));
const jsProseText = blankRanges(codeMasked,
  zones.filter(z => z.origin === 'JS 拼出来的标记').map(z => [z.srcStart, z.srcEnd]));

function visibleAttrSites(src) {
  const out = [];
  const re = /\s(?:title|placeholder|aria-label|alt)="([^"]*)"/g;
  let m;
  while ((m = re.exec(src))) out.push(...symbolSites(m[1], m.index + m[0].indexOf(m[1]), '可渲染属性'));
  return out;
}
const proseHits = [
  ...symbolSites(htmlProseText, 0, '静态可见文本'),
  ...symbolSites(jsProseText, scriptStart, 'JS 正文'),
  ...visibleAttrSites(htmlStripped),
];
// 防空跑锚：两个文本来源都要真的扫到东西
ok(htmlProseText.length > 1000 && jsProseText.length > 1000,
  '散文位的两个文本来源确实扫到了（否则本层是空跑）—— 静态 ' + htmlProseText.length + ' 字符 / JS ' + jsProseText.length + ' 字符');

const registeredChars = new Set(Object.keys(RENDER_MARKER_EXEMPT));
const unregistered = proseHits.filter(h => !registeredChars.has(h.ch));
ok(unregistered.length === 0,
  '[散文位] emoji 必须全部来自登记表（每字符带理由 + 仍在使用自检；登记表当前为空 ⇒ 任何 emoji 都红）'
  + (unregistered.length ? ' —— 未登记 ' + unregistered.length + ' 处：' + describeHits(unregistered) : ''));

// 登记表自检：登记了却没人用 = 防线腐烂，同样红（表空时本循环不执行）
for (const [ch, why] of Object.entries(RENDER_MARKER_EXEMPT)) {
  const used = proseHits.some(h => h.ch === ch) || iconHits.some(h => h.ch === ch);
  ok(used, `登记项 ${ch} (U+${ch.codePointAt(0).toString(16).toUpperCase()}) 仍在使用（${why}）—— 登记了却没人用同样是红`);
}

// 红了要红得能直接动手：把两层的命中逐条打出来
if (iconHits.length || unregistered.length) {
  console.log('    ⚠ emoji 命中明细（第 1 层无条件红；第 2 层必须来自登记表）：');
  for (const h of iconHits) console.log('      [icon位·硬红]      webui.html:' + h.line + '  ' + h.cp + ' ' + h.ch + '  ' + h.origin);
  for (const h of unregistered) console.log('      [散文位·未登记]    webui.html:' + h.line + '  ' + h.cp + ' ' + h.ch + '  ' + h.origin);
}

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

// 任务列表必须真的把项 map 出来（不是直接写空串/固定 HTML）。
//
// ⚠ 锚点别绑在变量名上（2026-10-19 修假红）：
//   旧版找的是 `tb.innerHTML = sorted.map(`，而 d720223（任务调度页从 6 列表格
//   换成卡片网格）把接收者改叫 `host`（`const host = $('jobrows')`）——
//   信号本身没变，名字变了就假红。所以先切出 renderJobs 的函数体，
//   再在体里找"容器.innerHTML = sorted.map("这个形状。
const jobsStart = js.indexOf('function renderJobs(');
const jobsEnd = jobsStart < 0 ? -1 : js.indexOf('\n  function ', jobsStart + 10);
const jobsBody = jobsStart < 0 ? '' : js.slice(jobsStart, jobsEnd > jobsStart ? jobsEnd : jobsStart + 4000);
const jobMap = /\.innerHTML\s*=\s*sorted\.map\(/.exec(jobsBody);
ok(!!jobMap, '任务列表走 sorted.map(...) 渲染（而不是固定 HTML）'
  + (jobMap ? '' : ' —— renderJobs 体里找不到 `.innerHTML = sorted.map(`（起始偏移 ' + jobsStart + '）'));
if (jobMap) {
  const seg = jobsBody.slice(jobMap.index);
  // ⚠ 反向锚必须跑在**剥过注释**的文本上（2026-10-19，我自己第一版就踩了）：
  //   renderJobs 里有两处注释拿 <tr> 当例子讲历史
  //   （webui.html:3330「原来是一行 <tr>」、3332「往里塞 <tr> 会被 HTML 解析器…」）——
  //   直接在原文上找 <tr 会把**文档**当成代码，把一条正确的实现判红。
  //   这与本文件第 152-162 行记的坑是同一类（注释被当成代码）。
  const segCode = stripComments(seg);
  // 反向锚要看**整个函数体**，不能只看 map 语句之后那一段 ——
  // 变异自测抓到的：把 `<tr>` 塞在函数开头（比如 map 之前的一行常量）时，
  // 窗口内找不到它、断言照样绿（等于给"半截改动"留了后门）。
  const bodyCode = stripComments(jobsBody);
  // 注入形态：`sorted.map(j => { if (true) return '';` —— 保留了 map 却让每项变空
  ok(!/\{\s*if\s*\(\s*(true|1)\s*\)/.test(segCode.slice(0, 160)),
    '任务项的 map 回调没有以常量短路开头（自审注入形态）');
  // 回调体里必须真的产出**每一项的元素模板**。
  // ⚠ 形状判据也从 <tr> 换成了卡片：d720223 把 6 列表格换成卡片网格，
  //   理由写在 webui.html:3309（错误文本要整段换行显示，表格里只能截断）。
  //   要守的性质没变："每一项真的产出一个元素"，不是空串也不是固定 HTML。
  //   窗口取 map 语句之后的整段函数体（回调里有大段注释，固定长度会假红）。
  ok(/job-item/.test(segCode), '任务项的 map 回调里真的产出 job-item 卡片（表格形态已换成卡片）');
  ok(/<div class=/.test(segCode), '回调体里真的产出元素模板（不是空串/固定 HTML）');
  ok(/\.join\(''\)/.test(segCode), 'map 的结果真的被 join 成一段 HTML 写进容器（漏 join 是半截改动）');
  // 反向锚：换了形态之后就不该再往 #jobrows 里写 <tr>（写回去说明只换了一半）
  ok(!/<tr[\s>]/.test(bodyCode), '整个 renderJobs 都不再产出 <tr>（卡片形态；半截改动会被这条抓到）');
}

// ---------------------------------------------------------------- 9. 账号池折叠（R3）的结构守卫
//
// # 为什么要在**静态层**再加一条（E2E 已经量了真实几何）
//
// E2E 覆盖的是"行为对不对"。这一条覆盖的是**形状**——两者防的不是同一类回归：
//   · E2E 会红：隐藏规则被删、委托改成逐行绑、折叠不持久化
//   · 静态会红：有人"顺手"给分组行加上 .mgroup 类名（复用模型面板的观感），
//     于是 `.mgroup.collapsed .chips{display:none}` 命中一个**没有 .chips 的**
//     行 —— 折叠变成只转三角、什么都不隐藏的空动作，且不报任何错。
//     那种改动在 E2E 上**也是红的**，但报错信息指向 display 而不是根因；
//     静态这条能直接说出"别把 .mgroup 用在这里"。
console.log('\n[9] 账号池折叠的形状（R3）');

// 折叠的隐藏必须由**整表**规则承担（拆成每上游一张表之后的形态）。
// 它一旦被删，"折叠"就退化成一个空动作。
//
// ⚠ 这两条都跑在 **codeHtml（已剥注释）** 上，不是原始 html：
// 下面那条反向守卫要找的正是这串 CSS，而样式表旁边的警告注释里**逐字引用了它**
// （"这里不能有 tr.acctrow.collapsed{display:none}"）。直接在原文上匹配的话，
// 警告本身会把守卫判红 —— 守卫被自己的说明文字触发，是典型的自伤。
// ⚠ 不能只要求"存在一条 .acctgroup.collapsed … display:none"（2026-10-19 修假红）：
//   现在的规则是**并列两条选择器**（webui.html:252-253）：
//     .acctgroup.collapsed > table,
//     .acctgroup.collapsed > .acctwrap > table{display:none}
//   而 JS 生成的真实结构是 `div.acctwrap > table.accttable`（webui.html:4375 附近），
//   所以**真正生效的只有第二条**；只留第一条会变成死选择器 ——
//   这正是 webui.html:243-248 记着的同类教训（"选择器必须是三层…那条规则从来没匹配过"）。
//   ⇒ 所以这里不只要求规则存在，还要求它**覆盖 JS 实际生成的那层包装**。
const foldRule = /\.acctgroup\.collapsed[^{}]*\{[^}]*display\s*:\s*none[^}]*\}/.exec(codeHtml);
ok(!!foldRule, '有 .acctgroup.collapsed …{display:none} —— 折叠真正隐藏整张表的地方'
  + (foldRule ? '' : '（一条都没有：折叠会退化成只转三角的空动作）'));
if (foldRule) {
  const rule = foldRule[0].replace(/\s+/g, ' ');
  ok(/\.acctgroup\.collapsed\s*>\s*(\.\w+\s*>\s*)?table\b/.test(rule),
    '折叠规则里有匹配 table 的选择器（实际：' + JSON.stringify(rule.slice(0, 140)) + '）');
  // 真实结构包了一层 .acctwrap ⇒ 规则里必须有 `> .acctwrap > table` 这一支
  ok(/>\s*\.acctwrap\s*>\s*table\b/.test(rule),
    '折叠规则覆盖 .acctwrap > table（JS 生成的真实结构；只写直挂 table 会变成死选择器）');
}
// 防空跑锚：上面那条"覆盖 .acctwrap"只有在 JS 真的生成 .acctwrap 时才有意义；
// 反过来，如果结构不再有这层包装，这条判据就该跟着改，而不是留着一句空话。
ok(/class="acctwrap"/.test(js), '账号池的表确实被 .acctwrap 包着（否则上一条是空条件）');

// ⚠ 反向守卫：**不许**再出现行级的 tr.acctrow.collapsed{display:none}。
//
// 这条曾经是正向断言（附理由"折叠真正隐藏东西的地方就这一条"），拆多表后那句
// 已经不成立 —— 真正隐藏的是上面那条整表规则。而那条行级规则留在样式表里
// 会造成一个真实回归：accountRow 渲染时给行盖 collapsed 类，展开却只切
// section 的类（纯 DOM 切换、不重绘）→ "渲染时处于折叠态 → 点开"只剩表头，
// 直到下一次 5 秒轮询重绘。
//
// 所以这里必须翻成反向：留着它就等于把那个回归锁死（而且下一个人会以为
// 它是折叠的必需机制，于是永远修不掉）。
ok(!/tr\.acctrow\.collapsed\s*\{\s*display\s*:\s*none/.test(codeHtml),
  '没有 tr.acctrow.collapsed{display:none} —— 行级隐藏会把"折叠态渲染后展开"弄成空表');

// 分组行**不能**带 .mgroup：那会让模型面板的 `.mgroup.collapsed .chips` 命中它，
// 而账号表里没有 .chips —— 结果是"看着折叠了、其实什么都没隐藏"。
ok(!/class="grouprow[^"]*\bmgroup\b/.test(js) && !/'mgroup'/.test(js.split('function accountGroupRow')[1] || ''),
  '账号池分组行不借用 .mgroup 类名（借了会命中 .chips 规则 → 假折叠）');

// 折叠状态键必须是独立前缀
const acctKeyMatch = /const\s+LS_ACCTGROUP\s*=\s*'([^']+)'/.exec(js);
ok(!!acctKeyMatch, '源码里有 LS_ACCTGROUP 常量（折叠状态的键前缀）');
if (acctKeyMatch) {
  ok(acctKeyMatch[1] !== 'wb2api.modelgroup',
    '账号池折叠键前缀与模型面板不同（' + acctKeyMatch[1] + ' ≠ wb2api.modelgroup）—— ' +
    '共用会互相覆盖且不报错');
  // 必须与模型面板的键**字面不同**：两边都按上游名做键，共用即冲突
  const modelKeyMatch = /const\s+LS_MODELGROUP\s*=\s*'([^']+)'/.exec(js);
  ok(modelKeyMatch && modelKeyMatch[1] !== acctKeyMatch[1],
    '两个前缀确实不同（modelgroup=' + (modelKeyMatch ? modelKeyMatch[1] : '?') +
    ' / acctgroup=' + acctKeyMatch[1] + '）');
}

// 折叠委托必须绑在 #accts 上，且**只有**一处 —— 逐行绑会随刷新累积。
//
// ⚠ 这里数的是 `$('accts').addEventListener` 的出现次数。第一版写了个
// 带转义的 `\\\$` 正则，在非 String.raw 的普通字符串里匹配不到任何东西，
// 于是假红了一条本来正确的断言 —— 又是一次"测量工具出错"。
const acctsHandlers = (js.match(/\$\('accts'\)\.addEventListener\s*\(/g) || []).length;
ok(acctsHandlers >= 1 && acctsHandlers <= 3,
  '#accts 上有 ' + acctsHandlers + ' 处委托（1 处起步；不随行数/刷新增长 —— 逐行绑会红）');

// R2：枚举点必须唯一 —— providerRegistryIds 是"manifest 里有哪些上游"的收敛处
const regIdsDefs = (js.match(/function\s+providerRegistryIds\s*\(/g) || []).length;
const regIdsAll = (js.match(/\bproviderRegistryIds\s*\(/g) || []).length;
ok(regIdsDefs === 1, 'providerRegistryIds 只定义一次（判据收敛）');
ok(regIdsAll - regIdsDefs >= 2,
  'providerRegistryIds 被**两处以上**消费（账号池 + 模型面板共用同一条判据）—— ' +
  '实际调用 ' + (regIdsAll - regIdsDefs) + ' 次');

// R2：提前返回必须不在 —— 它是"整池空 = 一个上游都不显示"的那个出口
const raStart = js.indexOf('function renderAccounts(');
if (raStart >= 0) {
  // 窗口取到下一个函数定义为止，避免把别处的代码算进来
  const raEnd = js.indexOf('\n  function ', raStart + 10);
  const raBody = js.slice(raStart, raEnd > raStart ? raEnd : raStart + 3000);
  ok(!/if\s*\(\s*!share\.length\s*\)\s*\{?\s*\$\(/.test(raBody),
    'renderAccounts 里没有 "if (!share.length) 显示账号池为空" 的提前返回（R2 删掉的出口）');
  ok(/groupAccountsByProvider\(\s*share\s*\)/.test(raBody),
    'renderAccounts 仍然经 groupAccountsByProvider(share) 分组');
}

// R4：.chip .mult.unknown 不能再有**样式规则**，也不能留空壳。
//
// ⚠ 必须先把 CSS 注释剥掉再找 —— 这段解释"为什么不再用虚线"的注释里
// 必然出现 `.chip .mult.unknown{...}` 这个字样，直接扫原文会假红。
// 扫注释就等于把文档当成代码（本项目踩过：注释被当成代码 → 抓不出真缺陷）。
const htmlNoCssComments = html.replace(/\/\*[\s\S]*?\*\//g, '');
ok(!/\.chip\s+\.mult\.unknown\s*\{/.test(htmlNoCssComments),
  '没有 .chip .mult.unknown{...} 规则（既不能有特例，也不能留空壳）');
// 但类名必须仍然被输出 —— 否则上面那条会因为"根本没这个类"而空过
ok(/class="mult unknown"/.test(js),
  'unknown 类名仍由 multTag 输出（x无 仍是可选取的样式锚点）');

// ---------------------------------------------------------------- 10. 主题（T10）
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
