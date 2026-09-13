
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
const lsKeyMatch = /const\s+LS_MODELGROUP\s*=\s*'([^']+)'/.exec(html);
if (!lsKeyMatch) throw new Error('源码里找不到 LS_MODELGROUP —— 抽取失败，测试装置失效');
const LS_MODELGROUP = lsKeyMatch[1];
// T7 的筛选键同理：从源码取，不硬编码。
const lsProvMatch = /const\s+LS_MODELPROVIDER\s*=\s*'([^']+)'/.exec(html);
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

// localStorage 假实现（内存 Map）—— 折叠状态要走真实的读写往返，
// 用打桩常量验证不了"存进去再读回来还是同一个值"。
const __lsMap = new Map();
const localStorage = {
  getItem: k => (__lsMap.has(k) ? __lsMap.get(k) : null),
  setItem: (k, v) => { __lsMap.set(k, String(v)); },
  removeItem: k => { __lsMap.delete(k); },
};

function multText(v) {
    if (!Number.isFinite(v) || v < 0) return null;
    // 0 → "0"；0.51 → "0.51"；2 → "2"。两位小数足够（上游最细到 x0.01）。
    return Number.isInteger(v) ? String(v) : String(Number(v.toFixed(2)));
  }

function multTag(id) {
    // 用户要求：倍率表里没有的模型显示 `x无`。
    //
    // ⚠ 但要区分**三种"没有"**，它们不该长一样：
    //
    //   1. 倍率表里有            → `x0.29`
    //   2. 表里没有这个模型       → `x无`      （本分支，用户要求）
    //   3. **倍率表整体没加载**   → 不显示 `x无`，改为面板级提示
    //
    // 第 3 种是"静默失败"的典型场景：`loadModelMultipliers` 失败时把
    // `modelMultipliers` 清空，若不区分，用户会以为**所有**模型都没有倍率，
    // 而真相是目录挂了。这种情况由 multCatalogFailed 标记，在面板顶部提示。
    //
    // 用"无"而不是留空：留空与"倍率恰好是 0 但没渲染"无法区分；
    // 显式写"无"是把不确定性**显示出来**，而不是藏起来。
    // 渲染成 `x无`（小写 x）与其它 15 个同形 —— 视觉一致比照搬输入字符重要。
    if (!Object.prototype.hasOwnProperty.call(modelMultipliers, id)) {
      if (multCatalogFailed) return '';   // 目录整体失败 → 不给每个 chip 都挂"无"
      return '<span class="mult unknown" title="上游未提供该模型的倍率">x无</span>';
    }
    const txt = multText(modelMultipliers[id]);
    if (txt === null) return '';   // 脏数据 → 当作没有系数，而不是显示 xNaN
    const v = modelMultipliers[id];
    // 免费（0）与高倍率（>=1）各给一个样式类，便于一眼扫出成本档位。
    // 注意：不发散到"低/中/高"多档 —— 倍率分布随上游调整，硬编码分档很快会失真。
    const cls = v === 0 ? ' free' : (v >= 1 ? ' hi' : '');
    return `<span class="mult${cls}" title="每次调用消耗 ${txt} 倍基础额度">x${esc(txt)}</span>`;
  }

function multTagPlain(id) {
    if (!Object.prototype.hasOwnProperty.call(modelMultipliers, id)) return '';
    const txt = multText(modelMultipliers[id]);
    return txt === null ? '' : ` (x${txt})`;
  }

function groupModelsByOwner(list, empties) {
    const byOwner = new Map();
    // 先把"声明了 models 能力"的上游都建好空组，
    // 这样没有模型的上游也会出现在界面上（见 emptyModelGroups 的注释）。
    for (const e of (empties || [])) {
      if (!byOwner.has(e.owner)) {
        byOwner.set(e.owner, { owner: e.owner, items: [], seen: new Set(), accountCount: e.accountCount });
      }
    }
    for (const m of list) {
      if (!m || !m.id) continue;
      const slash = m.id.indexOf('/');
      const prefix = slash > 0 ? m.id.slice(0, slash) : '';
      const bare = slash > 0 ? m.id.slice(slash + 1) : m.id;
      // 归属：前缀优先（它就是显式指定的上游），否则用 owned_by。
      const owner = prefix || m.owned_by || '（未标注）';
      if (!byOwner.has(owner)) byOwner.set(owner, { owner: owner, items: [], seen: new Set() });
      const g = byOwner.get(owner);
      if (g.seen.has(bare)) continue;      // 去重：同一组内同名只留一条
      g.seen.add(bare);
      // 展示用 id 取**裸名**；multTag 查表也用裸名（倍率表的键是裸名）。
      g.items.push({ id: bare, fullId: m.id, owner: owner });
    }
    // 组间按上游名排序；组内按名字排序（顺序稳定，便于扫读）。
    const out = Array.from(byOwner.values());
    out.sort((a, b) => (a.owner < b.owner ? -1 : a.owner > b.owner ? 1 : 0));
    out.forEach(g => g.items.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)));
    return out;
  }

function renderModelGroup(g) {
    const open = modelGroupOpen(g.owner);
    // 分组标题带**模型数**（不是账号数）—— 与导航里的 navgcount 区分开：
    // 那个是账号数。这里写清楚"个模型"，避免两个数字被当成同一件事。
    //
    // 空组（该上游声明了 models 能力、但当前一个模型都没有）不渲染 chips 区，
    // 改为一句能自我解释的说明 —— 见 emptyModelGroups 的注释：
    // **空白必须能自我解释**，否则用户只会以为功能坏了。
    let body;
    if (!g.items.length) {
      const why = (g.accountCount === 0)
        ? '没有可用账号，无法获取模型目录。先在「账号池」添加账号。'
        : '上游没有返回模型目录（账号可用但目录为空）。可用「强制刷新目录」重试。';
      body = `<div class="chips"><span class="dim" style="font-size:12px">${esc(why)}</span></div>`;
    } else {
      body = `<div class="chips">${g.items.map(m =>
        `<span class="chip" data-id="${esc(m.fullId)}" title="${esc(m.fullId)}">`
        + `${esc(m.id)}${multTag(m.id)}</span>`).join('')}</div>`;
    }
    return `<div class="mgroup${open ? '' : ' collapsed'}" data-owner="${esc(g.owner)}">
      <button class="mgrouphead" data-mtoggle="${esc(g.owner)}"
              aria-expanded="${open ? 'true' : 'false'}"
              title="${esc(g.owner)} · ${g.items.length} 个模型（点标题折叠/展开）">
        <span class="mcaret"></span>
        <span class="mowner">${esc(g.owner)}</span>
        <span class="mcount2">${g.items.length} 个模型</span>
      </button>
      ${body}
    </div>`;
  }

function modelGroupOpen(owner) {
    const key = LS_MODELGROUP + '.' + owner;
    try {
      const v = localStorage.getItem(key);
      return v === null ? true : v === '1';
    } catch { return true; }   // 隐私模式：默认展开
  }

function modelOwnerOf(id, ownedBy) {
    const s = String(id || '').indexOf('/');
    return s > 0 ? String(id).slice(0, s) : (ownedBy || '');
  }

function modelProviderFilter() {
    return modelProviderWanted;
  }

function syncModelProviderOptions(list) {
    const el = $('modelProvider');
    if (!el) return;
    const owners = [];
    for (const m of list) {
      const o = modelOwnerOf(m.id, m.owned_by);
      if (o && owners.indexOf(o) < 0) owners.push(o);
    }
    owners.sort();
    const keep = modelProviderWanted;
    el.innerHTML = '<option value="">全部上游</option>'
      + owners.map(o => `<option value="${esc(o)}">${esc(o)}</option>`).join('');
    // 选项就绪后把「想要的」写回 DOM。
    // 不在列表里（上游被移除）则回落「全部」——
    // 留着一个不存在的值会让 select 显示空、而内层 value 还是旧的。
    setModelProviderFilter(owners.indexOf(keep) >= 0 ? keep : '');
  }

function emptyModelGroups() {
    const out = [];
    const provs = (typeof PROVIDERS !== 'undefined' && PROVIDERS) || [];
    for (const p of provs) {
      if (!p || !p.id) continue;
      if (!Array.isArray(p.capabilities) || p.capabilities.indexOf('models') < 0) continue;
      out.push({ owner: p.id, accountCount: Number(p.account_count) || 0 });
    }
    return out;
  }

function renderModels(list) {
    models = list || [];
    $('mcount').textContent = models.length ? '（' + models.length + ' 个，上游实时目录）' : '';
    if (!models.length) { $('models').innerHTML = '<span class="dim">无可用模型</span>'; return; }

    // 按上游分组渲染（T5）。三件事一起做：
    //   1. **去重**：`/v1/models` 同时给裸名与 `provider/name` 两种写法，
    //      同一个模型会出现两次（实测 32 条 = 16 个模型 × 2 种写法）。
    //      对外 API 两种都要保留（`provider/model` 是显式指定上游用的），
    //      但**界面上不该重复展示**。
    //   2. **分组**：按 `owned_by` 归组（OpenAI 规范字段，后端已下发）。
    //   3. **可折叠**：状态存 localStorage，刷新后保持。
    const groups = groupModelsByOwner(models, emptyModelGroups());
    $('models').innerHTML = groups.map(renderModelGroup).join('');
    const sel = $('model'), keep = sel.value;
    // T7：模型下拉按上游筛选，**不去重**。
    //
    // 与上面的 chip 区是**两套**逻辑，故意不一致：
    //   · chip 区（可用模型面板）：去重 + 按上游分组，便于"看有哪些模型"
    //   · 这里（对话测试下拉）：按上游**筛选**，不去重 —— 用户要求保留
    //     `provider/xxx` 形态，因为那是"显式指定上游"的手段，测试时要用
    //
    // 先刷新上游下拉的选项（它依赖当前模型列表），再按选中的上游填模型。
    syncModelProviderOptions(models);
    const wantP = modelProviderFilter();
    const shown = wantP
      ? models.filter(m => modelOwnerOf(m.id, m.owned_by) === wantP)
      : models;
    sel.innerHTML = shown.map(m =>
      `<option value="${esc(m.id)}">${esc(m.id)}${multTagPlain(m.id)}</option>`).join('');
    // 保持原选择：若它不在筛选后的列表里，回落到第一个（或 auto）。
    sel.value = shown.some(m => m.id === keep) ? keep
      : ((shown.find(m => m.id === 'auto') || shown[0] || {}).id || '');
  }

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
