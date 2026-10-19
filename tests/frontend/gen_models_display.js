// gen_models_display.js —— 生成 tests/frontend/models_display_gen.js。
//
// # 本生成器只产出一样东西：**运行期实读形态**的套件
//
// 它**不再**在生成时 grab() webui.html 的函数体并内联进产物 —— 那正是被修掉的东西：
// 内联快照会假绿（源码变了、产物没重新生成，断言照样全绿；实测证据在产物的头部注释里）。
// 现在产物自带读源码装置：运行时 readFileSync + grab() + eval，永远测当前源码。
// 所以下面 READER / STUBS / ASSERTS 三段是**逐字**写进产物的，它们里面**不允许**出现
// 任何 webui.html 函数体的副本 —— 一出现假绿就跟着回来（文件末尾的结构守卫会拦）。
//
// ⚠ 本文件用了 String.raw 模板：**任何一处裸反引号都会提前终止模板**，
//   而 run_frontend_suites.js 的门禁 0 会在跑之前把它报出来（反引号奇数个 = 模板中止）。
//   需要反引号时写 \u0060（在模板里保持字面，到产物里才被 JS 解析成反引号）——
//   产物里 grabStatement 的引号状态机就是这么写的。本文件的反引号只有 4 对 String.raw
//   定界符（共 8 个，偶数），过门禁 0。
//
// ⚠ 产物写到 __dirname，不写 cwd：旧写法 fs.writeFileSync(产物名, ...) 在从仓库根跑
//   node tests/frontend/gen_models_display.js 时会把产物写到**仓库根**，而 tests/frontend
//   里的产物根本没被更新 —— 看起来"生成了"，实际测的还是旧的。同类坑的既有先例见
//   gen_tdz_guard.js 121-126 行的注释（探针写到 cwd，Chrome 永远打不开，断言恒失败）。
//
// 产物新鲜度由 run_frontend_suites.js 的门禁 1/2 保证：生成器必须 exit 0，且产物 mtime
// 必须被更新（本生成器每次都重写）。随后产物自己会跑一遍实读 —— 源码抽不到就红。
//
// ⚠ 生成期**也**查一遍清单（文件末尾守卫 2）：SRC_FUNCS 里的每个名字必须真的存在于
//   webui.html。没有这一步时，清单漏一个名字的后果只在**产物运行期**炸
//   （ReferenceError: multKeyOf is not defined），而生成器 exit 0 —— 门禁 1 白设。
//   ⚠ 它只在生成期查名字是否存在，不复制函数体：产物仍然是运行期实读。
//
// ⚠ 守卫 2 查的是「**清单里的名字在不在源码里**」，它**查不出**「源码里的函数**多了一个
//   依赖**」—— 后者是反向的漂移：被抽取的函数体改了（新调了一个私有助手），清单没跟着加，
//   于是实读进产物的函数一执行就 ReferenceError。
//   实测形态（本轮修的那条）：commit b8693bb 给 groupModelsByOwner /
//   syncModelProviderOptions 的排序换成 providerRank（组间按 manifest 顺序，替代旧的
//   字典序 ids.sort()），本夹具清单没加 providerRank ⇒ 产物运行期
//   「ReferenceError: providerRank is not defined」。
//   ⚠ 而它**只在比较器真的被调用**时才炸：Array#sort 对 0/1 个元素根本不调比较器 ——
//     上面 1~6 节只有一个 owner，全绿；到第 7 节（两个 owner）才红。
//     「测试跑到一半才红」+「错误指向源码里明明存在的函数」正是这条缝的特征。
//   所以清单的维护判据是：**被抽取的函数体里出现的每个自由标识符，都要在清单里**。
//   第 [11] 节就是为它写的回归：造出两个 owner，真的把 providerRank 的比较器跑起来。

const fs = require('fs');
const path = require('path');

const OUT = path.join(__dirname, 'models_display_gen.js');

// [0] 产物头部注释。
const BANNER = String.raw`// ============================================================================
// models_display_gen.js —— 模型列表显示（ctx / 倍率 / 分组 / 上游筛选）的纯 JS 套件。
//
// ⚠ 本文件是 **tests/frontend/gen_models_display.js 的产物**，形态是「**运行期实读**」：
//   运行时 readFileSync(webui.html) → grab() 逐字抠出函数体 → eval 进本模块作用域，
//   断言直接打在当前源码上。**被抽取的函数体一个字节都不在本文件里**（清单见 SRC_FUNCS）。
//
// # 为什么必须这样（两种形态都实测过）
//
//   · 旧形态是「生成时内联快照」：generator 在生成那一刻 grab() 出函数体，再**字面写死**
//     进本文件。于是运行期测的是"生成那一刻的源码快照"：源码改了、产物没重新生成，
//     断言照样全绿。实测（在 %TEMP% 里做，没碰仓库）：用旧 generator 对**改造前**的源码
//     副本生成一份内联产物，它 "全部通过"；随后把那份源码逐字换成现在的新口径
//     （id: m.id, fullId: m.id；真源码里 id: bare 已 0 次），**不重新生成**、直接再跑
//     同一个产物 —— 仍然 "全部通过"。它看的是自己那份快照。**这就是假绿。**
//   · 现在这条失效模式**在结构上不存在**：产物里没有源码副本，只有读源码的装置。
//     抽不到（函数被改名/删掉）就直接抛错 —— 响亮失败，不静默跳过。
//   · generator 只产出这个结构（见 gen_models_display.js 的 READER / STUBS / ASSERTS
//     三段），所以「重新生成」与「实读」不再矛盾：重新生成只是把同一个实读装置再写一遍。
//     gen_models_display.js 里还带一条**结构守卫**，明着拒绝退回内联快照形态。
//
// ⚠ 本文件是生成物：改断言请改 gen_models_display.js（run_frontend_suites.js 每次都会
//   重新生成并覆盖本文件；只改本文件会被下一次运行冲掉）。
//
// 口径现状（用户要求「统一成 workbuddy/xxx 或 codearts/xxx，不要重复」）：
//   · 后端 /v1/models 多上游模式只发 provider/model（39 条 → 23 条）；
//   · chip 与下拉都显示**完整 id**；倍率表的键是 **provider/裸名 组合键**
//     （后端 /admin/models/preview 每条带 provider，前端 loadModelMultipliers 拼键），
//     查表统一走 multTableKey：带前缀的展示 id **原样**当组合键查（不去前缀），
//     裸名原样查（单上游口径）。同名模型因此天然互不覆盖。
//     （真 Chrome 实测过旧反例：codearts/glm-5.3-flash 曾无条件去前缀、
//     继承 workbuddy 的 x0.06 —— 显示一个错的数字比 x无 有害得多。）
//   · 倍率查表**不再依赖 DEFAULT_PROVIDER**：归属在键里，manifest 什么时候到都不影响。
//   · 组间排序（chips 分组 + 上游下拉）走 providerRank：上游在 manifest 里的下标，
//     manifest 外的排末尾。**不是字典序** —— 服务端排的展示顺序（海外版在前）优先。
//
// ⚠ SRC_FUNCS 必须覆盖被抽取函数的**全部依赖**（含新加的私有助手）：漏一个的表现是
//   产物运行期 ReferenceError，而且只在那个助手**真的被调用到**时才炸
//   （实测先例：Array#sort 的比较器在 0/1 个元素时根本不调用）。见第 [11] 节。
// ============================================================================
`;

// [1] 实读装置：grab() 抠函数体 + eval + typeof 校验（旧形态的函数体内联在这里，现在没有）。
const READER = String.raw`const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');

// ---- 源码实读装置：按名字抠出函数体（花括号配对），eval 进本模块作用域 ----
//
// 非严格模式下 direct eval 里的 function 声明会落进调用者的变量环境，
// 因此下面这些函数在 eval 之后可以直接按名字调用。抽不到就抛错。
function grab(marker) {
  const start = html.indexOf(marker);
  if (start < 0) throw new Error('源码里找不到 ' + marker + ' —— 抽取失败，测试装置失效');
  let i = html.indexOf('{', start), d = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') d++;
    else if (html[i] === '}') { d--; if (d === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}

const SRC_FUNCS = [
  'function multText(',
  // 展示 id（「provider/xxx」）→ **裸名** 的唯一归一函数。
  // ⚠ 倍率表的键**不再是裸名**（见下一项）：它的消费点现在是 groupModelsByOwner
  //   （组内按裸名去重）与 multTableKey 的**无前缀**分支。
  // 漏了它，groupModelsByOwner 与 multTableKey 会一起炸。
  'function multKeyOf(',
  // 倍率查表的**唯一入口**：带前缀 → 原 id 就是 provider/裸名 组合键；无前缀 → 裸名。
  // 漏了它，multTag / multTagPlain 会一起炸（它们已经不再直接调 multKeyOf）。
  'function multTableKey(',
  'function multTag(',
  'function multTagPlain(',
  'function groupModelsByOwner(',
  'function renderModelGroup(',
  'function modelGroupOpen(',
  'function modelOwnerOf(',
  'function modelProviderFilter(',
  'function setModelProviderFilter(',
  'function syncModelProviderOptions(',
  'function providerInfo(',
  'function providerRegistryIds(',
  // 组间排序的**唯一判据**：上游在 manifest 里的下标（旧的 ids.sort() 字典序实现已被
  // b8693bb 换成它，见 providerRegistryIds 的注释）。
  // groupModelsByOwner 与 syncModelProviderOptions 都直接调它 —— 漏了它，
  // 产物运行期 ReferenceError（见本文件头部那条「反向漂移」的说明）。
  'function providerRank(',
  'function emptyModelGroups(',
  'function renderModels(',
];

// esc 也是被测渲染的一部分（chip 的 data-id/title 走它），所以一起从源码取。
// 它是 「const esc = ...」 箭头函数，且**跨两行**、正文里还有带分号的字符串
// （'&amp;'）—— 所以不能用 「/...;/」 非贪婪正则（会在字符串里的分号处截断）。
// 这里用一个带引号状态机的小扫描器取到「深度 0 且引号外的第一个分号」。
function grabStatement(marker) {
  const start = html.indexOf(marker);
  if (start < 0) throw new Error('源码里找不到 ' + marker + ' —— 抽取失败，测试装置失效');
  let depth = 0, q = null;
  for (let i = start; i < html.length; i++) {
    const c = html[i];
    if (q) { if (c === q) q = null; continue; }
    if (c === "'" || c === '"' || c === '\u0060') { q = c; continue; }
    if (c === '(' || c === '[' || c === '{') depth++;
    else if (c === ')' || c === ']' || c === '}') depth--;
    else if (c === ';' && depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('语句未闭合: ' + marker);
}
let esc;
eval(grabStatement('const esc =').replace(/^\s*const\s+esc\s*=/, 'esc ='));
if (typeof esc !== 'function') throw new Error('源码实读失败：esc 未生效');

eval(SRC_FUNCS.map(grab).join('\n\n'));
// 实读必须响亮：eval 没生效（例如被包进严格模式）时不要静默降级成"没测到"。
// 用 typeof（对未声明标识符也不抛）而不是 eval(n)，后者会抛 ReferenceError 盖掉原因。
for (const n of ['multText', 'multKeyOf', 'multTableKey', 'multTag', 'multTagPlain', 'groupModelsByOwner',
  'renderModelGroup', 'modelGroupOpen', 'modelOwnerOf', 'modelProviderFilter',
  'setModelProviderFilter', 'syncModelProviderOptions', 'providerInfo', 'providerRegistryIds',
  'providerRank', 'emptyModelGroups', 'renderModels']) {
  if (eval('typeof ' + n) !== 'function') {
    throw new Error('源码实读失败：' + n + ' 未生效（eval 没把声明泄漏出来？被包进严格模式了？）');
  }
}
`;

// [2] 桩与状态：DOM / localStorage / PROVIDERS / DEFAULT_PROVIDER / chip 解析器。
const STUBS = String.raw`
// ---- DOM / 状态桩 ----
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', style: {}, value: '' }; }
for (const id of ['mcount', 'models', 'model', 'modelProvider']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
let modelMultipliers = {};
let models = [];

// LS_MODELGROUP / LS_MODELPROVIDER 从源码取，不硬编码 —— 抄常量等于没测它。
const lsKeyMatch = /const\s+LS_MODELGROUP\s*=\s*'([^']+)'/.exec(html);
if (!lsKeyMatch) throw new Error('源码里找不到 LS_MODELGROUP —— 抽取失败，测试装置失效');
const LS_MODELGROUP = lsKeyMatch[1];
const lsProvMatch = /const\s+LS_MODELPROVIDER\s*=\s*'([^']+)'/.exec(html);
if (!lsProvMatch) throw new Error('源码里找不到 LS_MODELPROVIDER —— 抽取失败，测试装置失效');
const LS_MODELPROVIDER = lsProvMatch[1];

// 筛选值用模块变量存（源码里也是这么存的 —— <select> 会把不在 option 里的
// 值归一成空串，见 modelProviderFilter 的注释）。
let modelProviderWanted = '';
let multCatalogFailed = false;

// PROVIDERS 由测试自己通过 setProviders() 灌入（默认空）—— 不抄真 manifest。
let PROVIDERS = [];
function setProviders(list) { PROVIDERS = list || []; }

// DEFAULT_PROVIDER 是**模块级变量**（webui.html 里 「let DEFAULT_PROVIDER = '';」，
// 只在 applyManifest 里、manifest 到达后才被赋值）。这里给一个桩。
// ⚠ 它**不再参与倍率查表**：多上游改造后倍率表的键是 provider/裸名 组合键，
//   归属在键里，不再靠"是不是默认上游"判断（旧口径那样判会把海外版整组打成 x无）。
//   现在它的消费点是裸模型名走谁、以及倍率里那条**已不可达**的降级 title。
// 桩的初值刻意选 workbuddy（真部署里的默认上游）；第 [10] 节末会临时改成 ''，
// 用来证明倍率查表与 manifest 时机无关。
let DEFAULT_PROVIDER = 'workbuddy';

// localStorage 假实现（内存 Map）：折叠状态要走真实读写往返。
const __lsMap = new Map();
const localStorage = {
  getItem: k => (__lsMap.has(k) ? __lsMap.get(k) : null),
  setItem: (k, v) => { __lsMap.set(k, String(v)); },
  removeItem: k => { __lsMap.delete(k); },
};

// ---- chips 解析（渲染结果 → 判据）----
// chip 结构：<span class="chip" data-id="ID" title="ID">ID<span class="mult…">…</span></span>
// 可见名字取到第一个 '<' 为止（后面跟着的 multTag 片段都是标签）。
function chipTexts(htmlStr) {
  const out = [];
  const re = /<span class="chip"[^>]*>([^<]*)/g;
  let m;
  while ((m = re.exec(htmlStr))) out.push(m[1]);
  return out;
}
function chipDataIds(htmlStr) {
  const out = [];
  const re = /<span class="chip" data-id="([^"]*)"/g;
  let m;
  while ((m = re.exec(htmlStr))) out.push(m[1]);
  return out;
}
function optionValues(htmlStr) {
  const out = [];
  const re = /<option value="([^"]*)"/g;
  let m;
  while ((m = re.exec(htmlStr))) out.push(m[1]);
  return out;
}
`;

// [3] 断言本体。
const ASSERTS = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const LIST = [
  { id: 'auto', context_length: 200000 },
  { id: 'deepseek-v4-pro', context_length: 1000000 },
  { id: 'hy4-preview-f', context_length: 1000000 },
  { id: 'kimi-k3-1', context_length: 1000000 },
];

// ---------- 1. 不再显示 ctx ----------
console.log('\n[1] 不再显示 ctx 数字');
modelMultipliers = {};
renderModels(LIST.map(x => ({ ...x })));
const chips = $('models').innerHTML;
const opts = $('model').innerHTML;
ok(!/ctx/.test(chips), 'chips 里没有 "ctx" 字样');
ok(!/ctx/.test(opts), '下拉框里没有 "ctx" 字样');
ok(!/1000000|200000/.test(chips), 'chips 里没有上下文字数（1000000/200000）');
ok(!/1000000|200000/.test(opts), '下拉框里没有上下文字数');
ok(!html.includes("esc(m.context_length"), '源码里已无 context_length 插值');

// ---------- 2. 倍率标记 ----------
console.log('\n[2] 倍率标记');
modelMultipliers = { 'deepseek-v4-pro': 0.51, 'hy4-preview-f': 0, 'kimi-k3-1': 1.62 };
renderModels(LIST.map(x => ({ ...x })));
const c2 = $('models').innerHTML;
const o2 = $('model').innerHTML;
ok(c2.includes('x0.51'), '0.51 显示为 x0.51（实际片段：' + (c2.match(/x0\.51/) || ['无'])[0] + '）');
ok(c2.includes('x0') && c2.includes('hy4-preview-f'),
  '**0 倍率保留**为 x0（免费是有效信息，不能丢）');
ok(c2.includes('x1.62'), '1.62 显示为 x1.62');
ok(o2.includes('(x0.51)') && o2.includes('(x0)') && o2.includes('(x1.62)'),
  '下拉框里倍率用括号包起（option 不能放 HTML 标签）');
ok(!/<span[^>]*mult[^>]*>/.test(o2) || !o2.match(/<option[^>]*>[^<]*<span/),
  'option 内不含 HTML 标签');

// ---------- 3. 免费与高倍率各有样式类 ----------
console.log('\n[3] 成本档位的视觉标记');
ok(/class="mult free"[^>]*>x0</.test(c2), '免费（x0）带 free 类');
ok(/class="mult hi"[^>]*>x1\.62</.test(c2), '高倍率（>=1）带 hi 类');
ok(/class="mult"[^>]*>x0\.51</.test(c2), '中间倍率用中性样式');
ok(c2.includes('每次调用消耗'), '带 title 说明，不靠颜色单独表意');

// ---------- 4. 未知系数显示「无」，而不是伪装成免费 ----------
console.log('\n[4] 系数未知时显示「无」（不伪装成免费）');
modelMultipliers = { 'deepseek-v4-pro': 0.51 };   // 其余没有系数
renderModels(LIST.map(x => ({ ...x })));
const c4 = $('models').innerHTML;
ok(!/\bauto\b[^<]*x0(?!\.)|\bauto[^<]*>x0</.test(c4), 'auto 无系数时不该显示 x0（会把"不知道"说成"免费"）');
ok(c4.includes('deepseek-v4-pro') && c4.includes('x0.51'), '有系数的仍显示真实倍率');
ok(/x无/.test(c4), '没有系数的显示 x无（用户要求：显式说出"未知"）');
ok(/class="mult unknown"/.test(c4), 'x无 用 unknown 样式类（虚线边框，与 free 的实底可区分）');
ok((c4.match(/x无/g) || []).length > 1, '不止一个模型拿到 x无（说明是普遍回落，不是特例）');

// ---------- 5. 倍率整体取不到时列表照常渲染 ----------
console.log('\n[5] 倍率**整体**取不到时降级（与"单个模型没倍率"区分）');
modelMultipliers = {};
multCatalogFailed = true;        // 目录接口失败
renderModels(LIST.map(x => ({ ...x })));
const c5 = $('models').innerHTML;
ok(LIST.every(m => c5.includes(m.id)), '所有模型名照常显示');
ok(!/x无/.test(c5), '**不**给每个 chip 都挂 x无（那会让人以为上游真没配倍率）');
ok(!/x\d/.test(c5), '没有任何倍率数字');
ok(!/undefined|NaN/.test(c5), '不出现 undefined/NaN');

console.log('\n[5b] 目录恢复后回到 x无');
multCatalogFailed = false;
modelMultipliers = {};
renderModels(LIST.map(x => ({ ...x })));
ok(/x无/.test($('models').innerHTML), '目录正常但该模型没系数 → 显示 x无');

// ---------- 6. 边界：空列表 / 缺字段 ----------
console.log('\n[6] 边界');
modelMultipliers = {};
renderModels([]);
ok($('models').innerHTML.includes('无可用模型'), '空列表提示不变');
modelMultipliers = { 'x': NaN };
renderModels([{ id: 'x' }]);
ok(!/NaN/.test($('models').innerHTML), '倍率为 NaN 时不渲染 NaN');
modelMultipliers = { 'y': 0.005 };
renderModels([{ id: 'y' }]);
ok(!/x0\.005/.test($('models').innerHTML), '过小倍率被规整为 x0.01 或 x0，不出现 x0.005');

// ---------- 7. T7：对话测试的「上游筛选」——按 **id** 去重 ----------
//
// 新一轮口径：下拉与 chip 区都显示**完整 id**。去重键两处不同，这是源码里
// 写明的口径（见 renderModels 的注释）：
//   · chip 区：按**去前缀后的裸名**去重（同一个模型只留一条）
//   · 这里：按 **id** 去重（完全相同的 id 只留一条），保留上游返回的顺序
console.log('\n[7] 上游筛选（按 id 去重，报完整 id）');
modelMultipliers = {};
const MIXED = [
  { id: 'workbuddy/auto', owned_by: 'workbuddy' },
  { id: 'workbuddy/glm-5.2', owned_by: 'workbuddy' },
  { id: 'workbuddy/glm-5.2', owned_by: 'workbuddy' },   // 完全重复的 id（后端退化形态）
  { id: 'workbuddy/kimi-k3', owned_by: 'workbuddy' },
  { id: 'codearts/deepseek', owned_by: 'codearts' },
  { id: 'codearts/glm-5.2', owned_by: 'codearts' },
];
const distinctIds = Array.from(new Set(MIXED.map(m => m.id)));

// 7a) 「全部」时下拉按 id 去重：6 条输入 → 5 个不同 id → 5 项
$('modelProvider').value = '';
renderModels(MIXED.map(x => ({ ...x })));
const valsAll = optionValues($('model').innerHTML);
ok(valsAll.length === distinctIds.length,
  '选「全部」时下拉项数（' + valsAll.length + '）== 不同 id 数（' + distinctIds.length + '）'
  + ' —— 6 条输入里的那条完全重复被去掉了');
ok(new Set(valsAll).size === valsAll.length,
  '下拉里每个 id 只出现一次（实际 ' + JSON.stringify(valsAll) + '）');
ok(valsAll.every(v => v.indexOf('/') > 0),
  '下拉里的 id 全部形如 provider/xxx（实际 ' + JSON.stringify(valsAll) + '）');
ok(valsAll.indexOf('workbuddy/glm-5.2') >= 0 && valsAll.indexOf('codearts/glm-5.2') >= 0,
  '不同上游的同名模型是两条不同的 id，都保留（不会被按裸名吃掉）');

// 7b) 上游下拉的选项来自模型列表，且带「全部上游」
const provOpts = $('modelProvider').innerHTML;
ok(provOpts.includes('全部上游'), '上游下拉有「全部上游」选项');
ok(provOpts.includes('workbuddy') && provOpts.includes('codearts'),
  '上游下拉列出了数据里出现过的上游');

// 7c) 选某个上游 → 只列它的
//
// ⚠ 驱动方式必须走 setModelProviderFilter（源码实读的那份）而不是只改 DOM 的
// 「.value」：真实筛选值存在**模块变量** modelProviderWanted 里（「<select>」 会把
// 不在 option 里的值归一成空串，见源码注释）。只改 DOM 的话 renderModels
// 读到的仍是"全部"—— 这正是本文件旧版绕开 renderModels 手工拼 option 的原因，
// 但也因此**没测到**真实筛选路径。
setModelProviderFilter('workbuddy');
renderModels(MIXED.map(x => ({ ...x })));
const wbIds = Array.from(new Set(
  MIXED.filter(m => modelOwnerOf(m.id, m.owned_by) === 'workbuddy').map(m => m.id)));
const valsWb = optionValues($('model').innerHTML);
// ⚠ 期望值**从数据算**，不写字面量 —— 手写数字在数据变化时会变成假失败。
ok(valsWb.length === wbIds.length,
  '选 workbuddy 时只列它的 ' + wbIds.length + ' 项（实际 ' + valsWb.length + '）');
ok(valsWb.every(v => modelOwnerOf(v, '') === 'workbuddy'),
  '筛出来的每一项都属于 workbuddy（实际 ' + JSON.stringify(valsWb) + '）');
ok($('model').innerHTML.indexOf('codearts/deepseek') < 0,
  '别家上游的模型**不**出现在 workbuddy 的列表里');

// 7d) modelOwnerOf 的规则：前缀优先，否则 owned_by
ok(modelOwnerOf('workbuddy/x', 'whatever') === 'workbuddy', 'modelOwnerOf：有前缀时用前缀');
ok(modelOwnerOf('auto', 'workbuddy') === 'workbuddy', 'modelOwnerOf：无前缀时用 owned_by');
ok(modelOwnerOf('', 'workbuddy') === 'workbuddy', 'modelOwnerOf：空 id 不崩');

// 7e) 上游从列表消失时回落「全部」（而不是留一个不存在的选择）
//
// 灌一个**不在列表里的**想要值，再看它是否被清掉 —— 变量与 DOM 都要回落，
// 只清 DOM 会让下一次 renderModels 又把不存在的上游当筛选条件。
setModelProviderFilter('gone-upstream');
syncModelProviderOptions(MIXED.map(x => ({ ...x })));
ok($('modelProvider').value === '' && modelProviderWanted === '',
  '已选上游不在列表里时回落「全部」（DOM=' + JSON.stringify($('modelProvider').value)
  + '，变量=' + JSON.stringify(modelProviderWanted) + '）');

// 7f) 两处去重键**故意不同**：退化形态（裸名 + 前缀名并存）下
//     chip 区按裸名去重（1 条，保留**先出现**的那条），下拉按 id 去重（2 条）。
//     这是源码里写明的口径；若哪天被"统一"成一种，下面几条会立刻报红。
//     ⚠ 真实数据不该出现这个形态（后端多上游只发 「provider/model」），
//     这里测的是"后端若退化，界面不会重复展示"这条防御性冗余。
console.log('\n[7f] 退化形态：chip 按裸名去重、下拉按 id 去重');
$('modelProvider').value = '';
setModelProviderFilter('');
const DEGENERATE = [
  { id: 'glm-5.2', owned_by: 'workbuddy' },
  { id: 'workbuddy/glm-5.2', owned_by: 'workbuddy' },
];
renderModels(DEGENERATE.map(x => ({ ...x })));
const degChips = chipTexts($('models').innerHTML);
const degOpts = optionValues($('model').innerHTML);
ok(degChips.length === 1,
  'chip 区按裸名去重 → 「glm-5.2」 与 「workbuddy/glm-5.2」 合成 1 条（实际 ' + JSON.stringify(degChips) + '）');
ok(degChips[0] === 'glm-5.2',
  '保留的是**先出现**的那条（裸名在前 → 显示裸名；顺序反转时显示完整 id，见下一条）');
renderModels(DEGENERATE.slice().reverse().map(x => ({ ...x })));
const degChipsRev = chipTexts($('models').innerHTML);
ok(degChipsRev.length === 1 && degChipsRev[0] === 'workbuddy/glm-5.2',
  '顺序反转（前缀名在前）→ 仍 1 条，显示完整 id（实际 ' + JSON.stringify(degChipsRev) + '）');
ok(degOpts.length === 2,
  '下拉按 id 去重 → 两条 id 不同都保留（实际 ' + JSON.stringify(degOpts) + '）'
  + ' —— 这是"按 id 去重"的口径，不是按裸名');

// ---------- 8. 新口径：chips 显示完整 「provider/xxx」，组内 id 唯一 ----------
console.log('\n[8] 新口径：chip 文本是完整 id，且组内 id 唯一');
setProviders([]);                 // 不引入空态分组，专心看 chips
setModelProviderFilter('');       // 不带上游筛选，看全量
modelMultipliers = {};
const PREFIXED = [
  { id: 'workbuddy/auto', owned_by: 'workbuddy' },
  { id: 'workbuddy/glm-5.2', owned_by: 'workbuddy' },
  { id: 'workbuddy/glm-5.2', owned_by: 'workbuddy' },   // 完全重复的 id
  { id: 'workbuddy/kimi-k3', owned_by: 'workbuddy' },
  { id: 'codearts/glm-5.2', owned_by: 'codearts' },     // 与 workbuddy 同名、不同上游
  { id: 'codearts/deepseek-v4', owned_by: 'codearts' },
];
renderModels(PREFIXED.map(x => ({ ...x })));
const c8 = $('models').innerHTML;
const texts8 = chipTexts(c8);
const dataIds8 = chipDataIds(c8);

// ① chip 文本是**完整 id**（含 「/」）
ok(texts8.length > 0 && texts8.every(t => t.indexOf('/') > 0),
  '① chip 可见文本都是完整 「provider/xxx」（实际 ' + JSON.stringify(texts8) + '）');
ok(texts8.indexOf('workbuddy/glm-5.2') >= 0,
  '① 完整 id 「workbuddy/glm-5.2」 出现在 chip 文本里');
ok(texts8.indexOf('glm-5.2') < 0,
  '① 不再出现裸名 「glm-5.2」（旧口径的形态 —— 它正是"重复"的来源）');
ok(JSON.stringify(dataIds8) === JSON.stringify(texts8),
  '① data-id / title / 可见文本三处同为完整 id（实际 data-id=' + JSON.stringify(dataIds8) + '）');

// ② 组内 id 唯一（按源码的 groupModelsByOwner 取分组，再与渲染结果对照）
const groups8 = groupModelsByOwner(PREFIXED.map(x => ({ ...x })), []);
const wb8 = groups8.find(g => g.owner === 'workbuddy');
const ca8 = groups8.find(g => g.owner === 'codearts');
ok(!!wb8 && !!ca8, '② 建出了 workbuddy / codearts 两个分组');
const wbIds8 = wb8 ? wb8.items.map(i => i.id) : [];
ok(new Set(wbIds8).size === wbIds8.length,
  '② workbuddy 组内 id 唯一（实际 ' + JSON.stringify(wbIds8) + '）');
ok(wbIds8.length === 3,
  '② workbuddy 组内 3 条（auto / glm-5.2 / kimi-k3）—— 那条完全重复的 id 被去掉（实际 ' + wbIds8.length + '）');
ok(wbIds8.every(i => i.indexOf('/') > 0), '② 组内 item.id 是完整 id（渲染文本与之一致）');
ok(!!ca8 && ca8.items.map(i => i.id).indexOf('codearts/glm-5.2') >= 0,
  '② 与 workbuddy 同名的 「glm-5.2」 在 codearts 组里**仍在**（跨组不互相吃掉）');
ok(texts8.length === new Set(texts8).size,
  '② 渲染出的 chip 文本无重复（实际 ' + JSON.stringify(texts8) + '）');
ok(texts8.length === 5,
  '② 6 条输入（含 1 条完全重复）→ 5 个 chip，去重生效（实际 ' + texts8.length + '）');
ok(texts8.filter(t => t === 'workbuddy/glm-5.2').length === 1,
  '② 同一分组内同名模型只渲染一次');

// ---------- 9. 倍率表的键是 provider/裸名 组合键，带前缀的展示 id 直接命中 ----------
//
// 口径（多上游倍率改造后）：
//   · 后端 /admin/models/preview 遍历**全部已注册上游**、每条带 provider
//     （internal/admin/admin.go 的 modelsPreview 逐条填 ModelMultiplier.Provider）
//   · 前端 loadModelMultipliers 把键拼成 provider + '/' + 裸名（webui.html:10966-10969）
//   · 查表点原样把展示 id 当键：**带前缀不去前缀**，裸名原样（multTableKey）
// ⚠ 旧口径（"表只有默认上游一份目录、键是裸名，带前缀要去前缀再查"）已被替换：
//   去前缀正是旧缺陷（codearts/glm-5.3-flash 撞上 workbuddy 同名条目）的来源。
//   本节的期望值必须跟着后端契约走，不能再按"裸名键 + 只查默认上游"写。
// Go 侧的同一判据：internal/server/webui_models_unified_test.go:425-458
//   （② 带前缀分支返回原 id、④ multKeyOf 只出现在无前缀分支、⑥ 表键拼接 provider/裸名）。
console.log('\n[9] 新口径：倍率表的键是 provider/裸名，带前缀的展示 id 直接命中');
modelMultipliers = { 'workbuddy/glm-5.2': 0.51 };    // 键 = 组合键（后端每条带 provider）
renderModels([{ id: 'workbuddy/glm-5.2', owned_by: 'workbuddy' }]);
const c9 = $('models').innerHTML;
const o9 = $('model').innerHTML;
const texts9 = chipTexts(c9);
ok(multKeyOf('workbuddy/glm-5.2') === 'glm-5.2',
  'multKeyOf：完整 id → **裸名**（组内去重用它，见 groupModelsByOwner）');
ok(multKeyOf('glm-5.2') === 'glm-5.2', 'multKeyOf：裸名原样返回（单上游口径不被破坏）');
ok(multTableKey('workbuddy/glm-5.2') === 'workbuddy/glm-5.2',
  'multTableKey：带前缀 → 原 id 就是组合键（**不去前缀**，去前缀会撞别家上游的同名条目）');
ok(multTableKey('glm-5.2') === 'glm-5.2', 'multTableKey：无前缀 → 原样（单上游的表键就是裸名）');
ok(texts9.length === 1 && texts9[0] === 'workbuddy/glm-5.2',
  '③ chip 文本仍是完整 id（「workbuddy/glm-5.2」）');
ok(c9.includes('x0.51'),
  '③ 组合键命中 → 渲染出 x0.51（拿裸名查表只会得到 x无 —— 键口径必须一致）');
ok(!/x无/.test(c9), '③ 表里有该上游的条目时不落回 x无');
ok(o9.includes('(x0.51)'), '③ 下拉（multTagPlain）同样经归一命中');

// 归一与键规则只有一处：静态确认两个查表点都经 multTableKey，
// 且**带前缀分支不许去前缀**（去掉前缀 = 重新引入"借别家的数字"）。
const multTagSrc = grab('function multTag(');
const plainSrc = grab('function multTagPlain(');
const resolverSrc = grab('function multTableKey(');
ok(/multTableKey\(\s*id\s*\)/.test(multTagSrc), '③ multTag 经 multTableKey 取键（源码实读的正文里就有它）');
ok(/multTableKey\(\s*id\s*\)/.test(plainSrc), '③ multTagPlain 经 multTableKey 取键（判据没有第二份实现）');
ok((resolverSrc.match(/multKeyOf\(/g) || []).length === 1,
  '③ multTableKey 只在**无前缀**分支调 multKeyOf（实际 '
  + (resolverSrc.match(/multKeyOf\(/g) || []).length + ' 次）—— 带前缀分支不许去前缀');
ok(/return\s+String\(id\)\s*;/.test(resolverSrc),
  '③ 带前缀分支 return String(id)：原 id 就是组合键');
ok(!/indexOf\('\/'\)/.test(multTagSrc + plainSrc),
  '③ 两个查表点都没有自己写去前缀（判据只在 multTableKey 一处）');
ok(!/hasOwnProperty\.call\(modelMultipliers,\s*id\)/.test(multTagSrc + plainSrc),
  '③ 两处都没有"直接拿展示 id 查表"的旧写法');

// 带前缀与裸名查的是**不同的键**，因此输出不该逐字相同（旧口径要求"必须相同"，
// 那正是"别家上游继承默认上游系数"的形态）：
// 表里只有组合键时带前缀命中、裸名落空；反过来只有裸名键时裸名命中、带前缀落空。
ok(multTag('workbuddy/glm-5.2').indexOf('x0.51') >= 0 && multTag('glm-5.2').indexOf('x0.51') < 0,
  '③ 表里只有 provider/裸名 组合键时：带前缀命中、裸名不命中（实际裸名 → '
  + multTag('glm-5.2') + '）');
modelMultipliers = { 'glm-5.2': 0.51 };              // 表里只有裸名键（后端没带 provider 的形态）
ok(multTag('glm-5.2').indexOf('x0.51') >= 0 && multTag('workbuddy/glm-5.2').indexOf('x0.51') < 0,
  '③ 表里只有裸名键时：裸名命中、带前缀**不**命中（旧口径会把 0.51 借给 workbuddy）');

// ---------- 10. 倍率按 (provider, 裸名) 分开：别家上游**不得继承**默认上游的系数 ----------
//
// 真 Chrome 实测过的缺陷形态：倍率表**只由默认上游的目录构建**（键是裸名），
// 而查表点无条件去前缀 —— 于是 「codearts/glm-5.3-flash」 命中 workbuddy 目录里
// 同名模型的 0.06，被当成 codearts 的事实显示出来（同组另外 6 个模型显示 x无）。
// **显示一个错的数字，比显示 x无 有害得多。**
//
// 多上游改造后的修法**不是**"禁止别家上游查表"（那会把海外版整组判成 x无），
// 而是**键本身带上游**：后端每条倍率带 provider、前端表键 = provider/裸名、
// 查表点原样用组合键（见第 [9] 节）。同名模型由此天然互不覆盖 —— 本节守这条。
console.log('\n[10] 倍率按 (provider, 裸名) 分开：同名模型互不继承');
modelMultipliers = { 'workbuddy/glm-5.3-flash': 0.06, 'codearts/glm-5.3-flash': 2.5 };
renderModels([
  { id: 'workbuddy/glm-5.3-flash', owned_by: 'workbuddy' },
  { id: 'codearts/glm-5.3-flash', owned_by: 'codearts' },
]);
const c10 = $('models').innerHTML;
const chipBlocks10 = c10.split('<span class="chip"').slice(1);
const wbBlock10 = chipBlocks10.find(b => b.indexOf('data-id="workbuddy/glm-5.3-flash"') >= 0);
const caBlock10 = chipBlocks10.find(b => b.indexOf('data-id="codearts/glm-5.3-flash"') >= 0);
ok(!!wbBlock10 && /x0\.06/.test(wbBlock10),
  '默认上游那条 chip 拿到**自己的** 0.06（实际 ' + JSON.stringify(wbBlock10 || '') + '）');
ok(!!caBlock10 && /x2\.5/.test(caBlock10),
  '别家上游那条 chip 拿到**自己的** 2.5，不是 workbuddy 的 0.06（实际 '
  + JSON.stringify(caBlock10 || '') + '）');
ok(!!caBlock10 && !/x0\.06/.test(caBlock10),
  '别家上游那条 chip 里**没有** x0.06 —— 它不继承 workbuddy 目录里的系数');
ok((c10.match(/x0\.06/g) || []).length === 1,
  '整块里 x0.06 只出现一次（只在 workbuddy 那条上，实际 '
  + ((c10.match(/x0\.06/g) || []).length) + ' 次）');

// 反例：表里**只有**默认上游的条目（别家目录没取到 / 别家没这个模型）。
// 别家那条必须是 x无，且 title 说"上游未提供"—— 不能借用 0.06。
modelMultipliers = { 'workbuddy/glm-5.3-flash': 0.06 };
const caTag10 = multTag('codearts/glm-5.3-flash');
ok(multTag('workbuddy/glm-5.3-flash').indexOf('x0.06') >= 0,
  'multTag：表里有该上游的条目 → x0.06');
ok(caTag10.indexOf('x无') >= 0, 'multTag：别家上游在表里没有条目 → x无（实际 ' + caTag10 + '）');
ok(caTag10.indexOf('x0.06') < 0, 'multTag：别家上游不出现 0.06');
ok(caTag10.indexOf('上游未提供该模型的倍率') >= 0,
  'multTag：x无 的 title 是"上游未提供该模型的倍率"（与"目录整体失败"那条可区分）');
ok(multTagPlain('workbuddy/glm-5.3-flash') === ' (x0.06)', 'multTagPlain：默认上游 → " (x0.06)"');
ok(multTagPlain('codearts/glm-5.3-flash') === '',
  'multTagPlain：表里没有该上游的条目 → 不加倍率（实际 '
  + JSON.stringify(multTagPlain('codearts/glm-5.3-flash')) + '）');

// 裸名（无前缀）行为**完全不变**（单上游部署、后端没带 provider 的条目靠这一支）。
modelMultipliers = { 'glm-5.3-flash': 0.06 };
ok(multTag('glm-5.3-flash').indexOf('x0.06') >= 0, '裸名（无前缀）→ 原样查表，仍是 x0.06');
ok(multTagPlain('glm-5.3-flash') === ' (x0.06)', '裸名 → 下拉仍带 (x0.06)');
ok(multTag('workbuddy/glm-5.3-flash').indexOf('x0.06') < 0,
  '表里只有裸名键时带前缀的 id **不**命中 —— 无条件去前缀正是"借别家数字"的来源');

// ⚠ 倍率查表**不再依赖 DEFAULT_PROVIDER**（旧口径靠它判"表覆不覆盖这个上游"）。
// 键里已经带了上游，manifest 什么时候到都不影响查表结果 —— 这一条同时是
// "别把归属判据又加回来"的回归：加回来就会把海外版（workbuddy-intl）整组打成 x无。
modelMultipliers = { 'workbuddy/glm-5.3-flash': 0.06 };
DEFAULT_PROVIDER = '';
ok(multTag('workbuddy/glm-5.3-flash').indexOf('x0.06') >= 0,
  'DEFAULT_PROVIDER 为空（manifest 未到达）时，带前缀的 id 仍命中自己的组合键 → x0.06');
ok(multTag('glm-5.3-flash').indexOf('x0.06') < 0,
  '同一时刻裸名查不到（表里只有组合键）—— 证明两侧用的是不同的键，不是"归属未知就一律不查"');
DEFAULT_PROVIDER = 'workbuddy';
modelMultipliers = {};

// ---------- 11. 组间排序走 manifest 顺序（providerRank），不是字典序 ----------
//
// 本节与被修的那条 ReferenceError 是同一件事的两半：
//   · 上半是"清单要带上依赖"—— 不带上，groupModelsByOwner / syncModelProviderOptions
//     一排序就 ReferenceError（比较器只在 >=2 个 owner 时被调用，见文件头说明）；
//   · 下半是"带上之后要真的把它跑起来"—— 塞一个 owner 时比较器根本不被调用，
//     清单里那个名字等于只被 typeof 检查过、从没被用过，假绿照旧。
// 所以这里**刻意反着字典序**给 manifest：manifest 里 workbuddy 在前（字典序是
// codearts < workbuddy），一旦有人把 providerRank 换回 ids.sort()，下面第一条立刻红。
console.log('\n[11] 组间排序走 manifest 顺序（providerRank），未知上游排末尾');
setProviders([{ id: 'workbuddy' }, { id: 'codearts' }]);
const ORDERED = [
  { id: 'codearts/deepseek', owned_by: 'codearts' },
  { id: 'workbuddy/auto', owned_by: 'workbuddy' },
  { id: 'ghost/x', owned_by: 'ghost' },          // 不在 manifest 里的上游 → 排末尾
];
const g11 = groupModelsByOwner(ORDERED.map(x => ({ ...x })), []);
const owners11 = g11.map(g => g.owner);
ok(owners11.join(',') === 'workbuddy,codearts,ghost',
  '组间顺序 = manifest 顺序（workbuddy 在前，字典序会是 codearts 在前），manifest 外的上游排末尾（实际 '
  + JSON.stringify(owners11) + '）');
ok(providerRank('workbuddy') === 0 && providerRank('codearts') === 1,
  'providerRank：命中 manifest 返回它的下标（实际 '
  + JSON.stringify([providerRank('workbuddy'), providerRank('codearts')]) + '）');
ok(providerRank('ghost') === providerRegistryIds().length,
  'providerRank：manifest 外的 id 返回 length（排末尾，实际 ' + providerRank('ghost') + '）');
// 上游下拉是 providerRank 的**第二个**消费点：同一条判据，两个面（chips 分组 + 下拉）。
syncModelProviderOptions(ORDERED.map(x => ({ ...x })));
const provVals11 = optionValues($('modelProvider').innerHTML);
ok(JSON.stringify(provVals11) === JSON.stringify(['', 'workbuddy', 'codearts', 'ghost']),
  '上游下拉的选项顺序同 providerRank（「全部上游」 + manifest 顺序 + 末尾兜底，实际 '
  + JSON.stringify(provVals11) + '）');
// 还原：本节造的 manifest 不能漏给后面的节（PROVIDERS 是桩里的模块变量）。
setProviders([]);
setModelProviderFilter('');
modelMultipliers = {};

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

fs.writeFileSync(OUT, BANNER + READER + STUBS + ASSERTS);

// ---- 结构守卫：明着拒绝退回「生成时内联快照」形态 ----
//
// 上一轮修复的**不可退化标志**：产物里必须有读源码的装置，且**不能**有函数体副本。
// 判据取自"函数体的定义形态" —— 它只可能来自内联快照，实读形态里不可能出现
//（实读形态只有 grab('function xxx(' 这种**名字片段**，跟着的是括号左半边，不是定义）。
//
// ⚠ 判据从清单**推导**，不再硬编码两个名字：清单里每一个函数名都要查一遍。
//   硬编码那两个名字的版本只拦得住 multKeyOf / multTableKey，别的函数体被内联回来照样过。
//
// ⚠ 还加了**生成期**的门禁（守卫 2）：清单里的名字必须真的存在于 webui.html。
//   这条针对的正是「实读形态」引入的新缝：产物既然是运行期实读，清单漏一个名字的
//   后果落在**产物运行期**（ReferenceError: xxx is not defined，或"源码实读失败：xxx 未生效"），
//   而生成器自己照样 exit 0 —— "生成成功"与"能跑"之间出现缝，正是 run_frontend_suites.js
//   门禁 1（generator 必须 exit 0）想堵的东西。把检查放到生成期，失败时机就与其它
//   generator（它们用生成期 grabBlock 读源码）对齐：改名/删函数 → 生成器当场红。
const out = fs.readFileSync(OUT, 'utf8');

// 清单的**唯一定义**在产物里（READER 的 SRC_FUNCS）。这里把它抠回来，
// **不**维护第二份列表 —— 两份清单迟早会漂，而漂的表现就是下面这些守卫全部失效。
const listSrc = /const SRC_FUNCS = \[([\s\S]*?)\];/.exec(out);
if (!listSrc) {
  console.error('产物里找不到 SRC_FUNCS 清单 —— 实读形态被破坏');
  process.exit(1);
}
const MARKERS = Array.from(listSrc[1].matchAll(/'([^']+)'/g)).map(m => m[1]);
if (MARKERS.length < 2) {
  console.error('SRC_FUNCS 清单可疑：只抠出 ' + MARKERS.length + ' 项');
  process.exit(1);
}
const NAMES = MARKERS
  .filter(mk => mk.indexOf('function ') === 0)
  .map(mk => mk.replace(/^function\s+/, '').replace(/\($/, ''));

// ---- 守卫 2：清单 vs 真源码（生成期就红，不留到产物运行期）----
const htmlPath = path.join(process.env.WB2API_REPO || path.join(__dirname, '..', '..'),
  'internal', 'server', 'webui.html');
if (!fs.existsSync(htmlPath)) {
  console.error('找不到被测源码：' + htmlPath + '（用 WB2API_REPO 覆盖仓库根）');
  process.exit(1);
}
const webui = fs.readFileSync(htmlPath, 'utf8');
const missingMarkers = MARKERS.filter(mk => webui.indexOf(mk) < 0);
if (missingMarkers.length) {
  console.error('SRC_FUNCS 清单里的名字在 webui.html 里找不到：' + missingMarkers.join('、'));
  console.error('清单必须与源码同步（源码：' + htmlPath + '）—— 补齐清单后重跑本生成器。');
  process.exit(1);
}

// ---- 守卫 3：产物里不许出现函数体副本（内联快照的唯一形态）----
const inlined = NAMES.filter(nm => new RegExp('(^|\\n)\\s*function\\s+' + nm + '\\s*\\([^)]*\\)\\s*\\{').test(out));
if (inlined.length) {
  console.error('产物里出现了 webui.html 的函数体副本：' + inlined.join('、'));
  console.error('本生成器只允许产出「运行期实读」形态（见文件头）。拒绝写出内联快照。');
  process.exit(1);
}

// ---- 守卫 4：typeof 校验循环必须覆盖清单里的每一个函数名（**双向**）----
// 少一个名字 = 该函数在产物运行期变成裸 ReferenceError（症状难读）；
// typeof 循环是"响亮失败"的那条路，覆盖不全等于把响亮失败又变回难读的失败。
// 反向也要查：typeof 循环里出现了清单没有的名字 = 两份名单已经漂了
//（上一轮就是这个形态：清单漏了 multKeyOf，产物一执行就 ReferenceError）。
const checkSrc = /for \(const n of \[([\s\S]*?)\]\)/.exec(out);
const CHECKED = checkSrc ? Array.from(checkSrc[1].matchAll(/'([^']+)'/g)).map(m => m[1]) : [];
const unguarded = NAMES.filter(nm => CHECKED.indexOf(nm) < 0);
if (unguarded.length) {
  console.error('SRC_FUNCS 里的名字没有进 typeof 校验循环：' + unguarded.join('、'));
  process.exit(1);
}
const onlyChecked = CHECKED.filter(nm => NAMES.indexOf(nm) < 0);
if (onlyChecked.length) {
  console.error('typeof 校验循环里有 SRC_FUNCS 清单没有的名字：' + onlyChecked.join('、'));
  console.error('两份名单必须逐字一致 —— 只在其中一处加名字 = 产物运行期 ReferenceError。');
  process.exit(1);
}

const NEEDS = ['const SRC_FUNCS = [', "'function multKeyOf(',", 'eval(SRC_FUNCS.map(grab)'];
for (const need of NEEDS) {
  if (!out.includes(need)) {
    console.error('产物缺少实读形态的标志：' + need);
    process.exit(1);
  }
}

console.log('生成: ' + OUT);
console.log('  形态: 运行期实读（webui.html 的函数体不内联，运行时 grab + eval）');
console.log('  行数: ' + out.split('\n').length + '；抽取清单 SRC_FUNCS 在产物里');
// 守卫结果必须**被打印出来**：静默通过的守卫与没有守卫在输出上看不出区别，
// 下次有人把清单改坏时会以为"生成器没报错 = 没问题"。这里把核对过的名字数与源码路径写出来。
console.log('  守卫: 清单 ' + MARKERS.length + ' 个标记（' + NAMES.length + ' 个函数名）'
  + '逐个在源码里找到；产物内无函数体副本；typeof 校验覆盖 ' + CHECKED.length + ' 个名字');
console.log('  源码: ' + htmlPath);
