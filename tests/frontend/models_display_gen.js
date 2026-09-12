
const fs = require('fs');
const html = fs.readFileSync("D:\\project_GIT\\workbuddy2api实验版本\\tests\\frontend/../../internal/server/webui.html", 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', style: {}, value: '' }; }
for (const id of ['mcount','models','model']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let modelMultipliers = {};
let models = [];

function multText(v) {
    if (!Number.isFinite(v) || v < 0) return null;
    // 0 → "0"；0.51 → "0.51"；2 → "2"。两位小数足够（上游最细到 x0.01）。
    return Number.isInteger(v) ? String(v) : String(Number(v.toFixed(2)));
  }

function multTag(id) {
    if (!Object.prototype.hasOwnProperty.call(modelMultipliers, id)) return '';
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

function renderModels(list) {
    models = list || [];
    $('mcount').textContent = models.length ? '（' + models.length + ' 个，上游实时目录）' : '';
    if (!models.length) { $('models').innerHTML = '<span class="dim">无可用模型</span>'; return; }
    // 只显示模型名 + 倍率，不再显示 context_length ——
    // ctx 数字对"选哪个模型"没有帮助，反而把名字挤得难读；倍率才是决策依据。
    $('models').innerHTML = models.map(m =>
      `<span class="chip" data-id="${esc(m.id)}">${esc(m.id)}${multTag(m.id)}</span>`).join('');
    const sel = $('model'), keep = sel.value;
    sel.innerHTML = models.map(m =>
      `<option value="${esc(m.id)}">${esc(m.id)}${multTagPlain(m.id)}</option>`).join('');
    sel.value = models.some(m => m.id === keep) ? keep : (models.find(m => m.id === 'auto') || models[0]).id;
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

// ---------- 4. 未知系数不伪装成免费 ----------
console.log('\n[4] 系数未知时不加后缀');
modelMultipliers = { 'deepseek-v4-pro': 0.51 };   // 其余没有系数
renderModels(LIST.map(x => ({ ...x })));
const c4 = $('models').innerHTML;
const chips4 = c4.split('</span>').filter(x => x.includes('chip'));
ok(!/\bauto\b[^<]*x0\b/.test(c4), 'auto 无系数时不该显示 x0（会把"不知道"说成"免费"）');
ok(c4.includes('deepseek-v4-pro') && c4.includes('x0.51'), '有系数的仍显示');
ok((c4.match(/class="mult/g) || []).length === 1, '只有 1 个模型带倍率标记');

// ---------- 5. 倍率整体取不到时列表照常渲染 ----------
console.log('\n[5] 倍率取不到时降级');
modelMultipliers = {};
renderModels(LIST.map(x => ({ ...x })));
const c5 = $('models').innerHTML;
ok(LIST.every(m => c5.includes('esc')===false && c5.includes(m.id)), '所有模型名照常显示');
ok(!/x\d/.test(c5), '没有任何倍率标记');
ok(!/undefined|NaN/.test(c5), '不出现 undefined/NaN');

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

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
