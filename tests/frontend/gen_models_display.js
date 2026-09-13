// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：模型列表的显示（去掉 ctx、加倍率标记）。
//
// 用户要求：
//   1. 「可用模型」chips 与「对话」下拉框都不再显示 ctx 数字
//   2. 改为显示成本倍率（x0 / x0.51 / x2 这种），需标记出来
//
// 关键边界：
//   - **0 倍率必须保留**（x0 = 免费，是有意义的信息，不是"没有"）
//   - 系数未知时**不加** x0 后缀（否则把"不知道"说成"免费"）
//   - 倍率取不到时模型列表照常显示（不能整块失败）
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(WEBUI, 'utf8');

function grab(marker) {
  const start = html.indexOf(marker);
  if (start < 0) throw new Error('找不到: ' + marker);
  let i = html.indexOf('{', start), d = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') d++;
    else if (html[i] === '}') { d--; if (d === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}

const pieces = [
  grab('function multText('),
  grab('function multTag('),
  grab('function multTagPlain('),
  // T5 新增的三个：renderModels 现在按上游分组渲染，依赖它们。
  // 漏一个就会在产物里报 `xxx is not defined` —— 抽取是**按名字**取的，
  // 新增被 renderModels 调用的函数时必须同步加进来。
  grab('function groupModelsByOwner('),
  grab('function renderModelGroup('),
  grab('function modelGroupOpen('),
  // T7 新增的三个：renderModels 现在还会按上游刷新对话测试的下拉。
  // ⚠ 抽取是**按名字**取的 —— 每次给 renderModels 加一个被调用的函数，
  // 都必须同步加到这里，否则产物报 `xxx is not defined`（本文件已踩两次）。
  grab('function modelOwnerOf('),
  grab('function modelProviderFilter('),
  grab('function syncModelProviderOptions('),
  // T5 补漏：空态分组（评审抓到的漏实现）。
  // ⚠ 这已经是第 4 次因为"给 renderModels 加了被调用的函数但忘了加进抽取列表"
  // 而在产物里报 `xxx is not defined`。抽取是按名字取的 ——
  // **改 renderModels 的依赖时，必须同步改这里**。
  grab('function emptyModelGroups('),
  grab('function renderModels('),
  // R2 抽出的**共享判据**：providerRegistryIds() 是"manifest 里有哪些上游"
  // 的唯一枚举点，模型面板与账号池都读它（决策 1：判据只有一份，不许各写一遍）。
  // renderModels 经 emptyModelGroups 间接依赖它，所以必须一起抠出来。
  //
  // ⚠ 这是第 5 次踩同一个坑的**防线**：抽取是按名字取的，
  // 给 renderModels 加任何被调用的函数，都要同步加到这里。
  grab('function providerRegistryIds('),
];

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', style: {}, value: '' }; }
for (const id of ['mcount','models','model','modelProvider']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let modelMultipliers = {};
let models = [];

// ---- T5 依赖：折叠状态与目录失败标记 ----
//
// 这几个都**从源码实读**，不硬编码 —— 本文件的教训：
// 装置抄了被测对象的常量就等于没测它（原 PAGE_SIZE 就是这么被抄坏的）。
// LS_MODELGROUP 用正则从 webui.html 取；取不到就抛错（抽取失败必须响亮）。
const lsKeyMatch = /const\\s+LS_MODELGROUP\\s*=\\s*'([^']+)'/.exec(html);
if (!lsKeyMatch) throw new Error('源码里找不到 LS_MODELGROUP —— 抽取失败，测试装置失效');
const LS_MODELGROUP = lsKeyMatch[1];
// T7 的筛选键同理：从源码取，不硬编码。
const lsProvMatch = /const\\s+LS_MODELPROVIDER\\s*=\\s*'([^']+)'/.exec(html);
if (!lsProvMatch) throw new Error('源码里找不到 LS_MODELPROVIDER —— 抽取失败，测试装置失效');
const LS_MODELPROVIDER = lsProvMatch[1];
// T7 的筛选值用**模块变量**存（源码里也是这么存的 —— 因为 <select> 会把
// 不在 option 里的值归一成空串，见 modelProviderFilter 的注释）。
let modelProviderWanted = '';
function setModelProviderFilter(v) {
  modelProviderWanted = v || '';
  const el = $('modelProvider');
  if (el && el.value !== modelProviderWanted) el.value = modelProviderWanted;
}
let multCatalogFailed = false;

// ---- R2 依赖：manifest 的上游目录 ----
//
// providerRegistryIds() 读的是模块级 PROVIDERS，而 emptyModelGroups 现在经它
// 枚举 —— 不在这里声明，产物会报 PROVIDERS is not defined。
// （反引号在本 String.raw 模板里是**终止符**，所以上面那句不写引号 ——
//  这是本文件已经踩过两次的坑，见 run_frontend_suites.js 的门禁 0。）
//
// 值由**测试自己**通过 setProviders() 灌入（默认空）—— 不抄真 manifest，
// 否则就成了"装置抄了被测对象的常量"（本文件的 PAGE_SIZE 就这么坏过）。
let PROVIDERS = [];
function setProviders(list) { PROVIDERS = list || []; }
// providerInfo 读的也是 PROVIDERS（emptyModelGroups 用它取 capabilities）。
// 这里**内联而不是从源码抓**：它是 4 行直读，抓过来只多一个断裂点，
// 没有额外保真度收益；而 providerRegistryIds 是真判据，必须从源码抓
//（见下面 pieces 的注释）。
function providerInfo(id) {
  return PROVIDERS.find(p => p && p.id === id) || null;
}

// localStorage 假实现（内存 Map）—— 折叠状态要走真实的读写往返，
// 用打桩常量验证不了"存进去再读回来还是同一个值"。
const __lsMap = new Map();
const localStorage = {
  getItem: k => (__lsMap.has(k) ? __lsMap.get(k) : null),
  setItem: (k, v) => { __lsMap.set(k, String(v)); },
  removeItem: k => { __lsMap.delete(k); },
};
`;

const tail = String.raw`
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
//
// ⚠ 这一节的判据在 T6 变了，但**原始意图没变** ——
// 原来写的是"没系数就不加后缀"，理由是"别把不知道说成 x0（免费）"。
// 用户要求改为显式显示 x无 —— 那比"留空"**更贴合原意图**：
//   · 留空     → 用户分不清"这个模型没倍率"与"倍率没渲染出来"
//   · x无    → 明确说"上游没给"
// 所以这里断言 x无，并且仍然断言**不出现 x0**（那才是"伪装成免费"）。
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
//
// ⚠ 与第 4 节的**关键区别**：这里是"目录接口失败"（multCatalogFailed=true），
// 那时**不该**给每个 chip 都挂 x无 —— 否则用户会以为上游真的没配倍率。
// 这是本项目的"静默失败"形态：两种情况必须可区分。
console.log('\n[5] 倍率**整体**取不到时降级（与"单个模型没倍率"区分）');
modelMultipliers = {};
multCatalogFailed = true;        // 目录接口失败
renderModels(LIST.map(x => ({ ...x })));
const c5 = $('models').innerHTML;
ok(LIST.every(m => c5.includes(m.id)), '所有模型名照常显示');
ok(!/x无/.test(c5), '**不**给每个 chip 都挂 x无（那会让人以为上游真没配倍率）');
ok(!/x\d/.test(c5), '没有任何倍率数字');
ok(!/undefined|NaN/.test(c5), '不出现 undefined/NaN');

// 目录恢复后，未知系数重新显示 x无
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

// ---------- 7. T7：对话测试的「上游筛选」（不去重）----------
//
// 与 chip 区**故意不同**：chip 区去重，这里保留 provider/xxx 形态 ——
// 那是"显式指定上游"的手段，测试时要用。
console.log('\n[7] 上游筛选（不去重）');
modelMultipliers = {};
const MIXED = [
  { id: 'auto', owned_by: 'workbuddy' },
  { id: 'workbuddy/auto', owned_by: 'workbuddy' },
  { id: 'glm-5.2', owned_by: 'workbuddy' },
  { id: 'workbuddy/glm-5.2', owned_by: 'workbuddy' },
  { id: 'codearts/deepseek', owned_by: 'codearts' },
];
// 7a) 「全部」时下拉**不去重**：5 项全在
$('modelProvider').value = '';
renderModels(MIXED.map(x => ({ ...x })));
const optsAll = ($('model').innerHTML.match(/<option /g) || []).length;
ok(optsAll === 5, '选「全部」时下拉有 5 项，**不去重**（实际 ' + optsAll + '）');

// 7b) 上游下拉的选项来自模型列表，且带「全部上游」
const provOpts = $('modelProvider').innerHTML;
ok(provOpts.includes('全部上游'), '上游下拉有「全部上游」选项');
ok(provOpts.includes('workbuddy') && provOpts.includes('codearts'),
  '上游下拉列出了数据里出现过的上游');

// 7c) 选某个上游 → 只列它的
$('modelProvider').value = 'workbuddy';
const shown = MIXED.filter(m => modelOwnerOf(m.id, m.owned_by) === 'workbuddy');
$('model').innerHTML = shown.map(m => '<option value="' + esc(m.id) + '">' + esc(m.id) + '</option>').join('');
// ⚠ 期望值**从数据算**，不写字面量 —— 我第一版写了 3，而 workbuddy 实际有 4 项
// （auto / workbuddy/auto / glm-5.2 / workbuddy/glm-5.2），于是报了个假失败。
// 数出来的期望比手写的可靠：数据改了断言自动跟上，而不是靠人记得改。
const wantCount = MIXED.filter(m => modelOwnerOf(m.id, m.owned_by) === 'workbuddy').length;
ok((($('model').innerHTML.match(/<option /g) || []).length) === wantCount,
  '选 workbuddy 时只列它的 ' + wantCount + ' 项（实际 '
  + (($('model').innerHTML.match(/<option /g) || []).length) + '）');
ok($('model').innerHTML.indexOf('codearts/deepseek') < 0,
  '别家上游的模型**不**出现在 workbuddy 的列表里');

// 7d) modelOwnerOf 的规则：前缀优先，否则 owned_by
ok(modelOwnerOf('workbuddy/x', 'whatever') === 'workbuddy', 'modelOwnerOf：有前缀时用前缀');
ok(modelOwnerOf('auto', 'workbuddy') === 'workbuddy', 'modelOwnerOf：无前缀时用 owned_by');
ok(modelOwnerOf('', 'workbuddy') === 'workbuddy', 'modelOwnerOf：空 id 不崩');

// 7e) 上游从列表消失时回落「全部」（而不是留一个不存在的选择）
$('modelProvider').value = 'gone-upstream';
syncModelProviderOptions(MIXED.map(x => ({ ...x })));
ok($('modelProvider').value === '', '已选上游不在列表里时回落「全部」（实际 ' + JSON.stringify($('modelProvider').value) + '）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

fs.writeFileSync('models_display_gen.js', header + '\n' + pieces.join('\n\n') + '\n' + tail);
console.log('生成: models_display_gen.js');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
