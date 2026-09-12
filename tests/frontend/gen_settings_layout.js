// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：设置面板「组内复选框与输入框分离」。
//
// 与既有 7 套测试同一套路：从 webui.html 里**正则提取真实结构**做断言，
// 不复制副本 —— 否则实现改了测试仍会绿（假绿）。
//
// 覆盖 4 类断言：
//  A. 分离性：每个 fieldset 内 .field 的类型翻转次数 = 0（本次需求的核心可测条件）
//  B. 分组完整性：不存在未被 .fieldgroup 包裹的 .field
//  C. 零漂移：字段 id 全集与基线逐字一致；label[for] 均指向存在的 id
//  D. 结构保真：legend 文案、placeholder/hint/min/max 原文未变
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');
// 基线文件**随测试一起入库**（tests/frontend/settings-baseline-ids.txt）。
// 此前它指向 D:/project_GIT/workbuddy2api/tasks/... —— **另一个仓库**的
// 绝对路径，且无兜底：换机器立刻抛 ENOENT。这正是"测试只在开发机可跑"
// 的典型形态，与上一轮修掉的 7863 依赖同类。
const BASELINE = (process.env.WB2API_REPO || __dirname + '/../..') +
  '/tests/frontend/settings-baseline-ids.txt';
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';

const header = `
const fs = require('fs');
const BASELINE = (process.env.WB2API_REPO || __dirname + '/../..') + '/tests/frontend/settings-baseline-ids.txt';
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(WEBUI, 'utf8');

// 只截设置面板那一段：从 <h2>设置 到这个 section 的结束标签。
//
// # 为什么终点不能用 <div id="modal">（T6 踩到的坑）
//
// 原先切到模态框为止。T6 在设置 section 之后、模态框之前插入了
// 「动态上游面板宿主」块（含一句带 ⚠ 的 HTML 注释），于是这段切片
// 把**不属于设置面板**的内容也包了进来，emoji 断言因此假红。
//
// 正确做法是按 DOM 结构切：设置面板的 <h2> 属于某个 <section>，
// 切到该 section 的闭合标签为止。
const SET_START = html.indexOf('<h2>设置');
const SET_END = html.indexOf('</section>', SET_START);
if (SET_END < 0) throw new Error('找不到设置面板的 </section>');
const sec = html.slice(SET_START, SET_END);

// 按 DOM 顺序切出每个 fieldset 的内部
function fieldsets(src) {
  const out = [];
  const re = /<fieldset([^>]*)>([\\s\\S]*?)<\\/fieldset>/g;
  let m;
  while ((m = re.exec(src))) {
    const legend = /<legend>([\\s\\S]*?)<\\/legend>/.exec(m[2]);
    out.push({ attrs: m[1] || '', body: m[2], legend: legend ? legend[1].trim() : null });
  }
  return out;
}

// 取一个 fieldset 内 .field 的类型序列（按 DOM 顺序）
// .field 与 .field.chk 都算；用「本 .field 的起始位置」排序后逐个判类
function fieldKinds(body) {
  const kinds = [];
  const re = /<div class="field( chk[^"]*)?">/g;
  let m;
  while ((m = re.exec(body))) kinds.push(m[1] ? 'CHK' : 'IN');
  return kinds;
}
function flips(kinds) {
  let n = 0;
  for (let i = 1; i < kinds.length; i++) if (kinds[i] !== kinds[i - 1]) n++;
  return n;
}

// 「分离」的准确判据：类型序列必须是「同类连续」的 A…AB…B 形式，
// 即**最多一次翻转**，且两类各自成段。
//
// 为什么不是「翻转 = 0」：把复选框与输入框分成两个容器后，两者之间必然有
// 一次类型切换（CHK…CHK → IN…IN）。那一次切换正是「分离」本身，不是交叉。
// 真正的交叉是 A…AB…A 这种来回跳（翻转 ≥ 2，或同类型被另一类型打断）。
// 所以判据取 flips <= 1 且等于「非空类型种类数 - 1」。
function isSeparated(kinds) {
  if (kinds.length === 0) return true;
  const kindsPresent = new Set(kinds).size;
  return flips(kinds) === kindsPresent - 1;
}

// 取 .field 的 id（从 <div class="field..."> 往后到该 field 结束的片段里找第一个 id）
function fieldIds(body) {
  const out = [];
  const re = /<div class="field(?: chk[^"]*)?">([\\s\\S]*?)(?=<div class="field(?: chk[^"]*)?">|<\\/div>\\s*<\\/div>|<div class="dim"|<fieldset|<\\/fieldset>|<div id="setLogFileInfo"|$)/g;
  let m;
  while ((m = re.exec(body))) {
    const id = /id="([^"]+)"/.exec(m[1]);
    if (id) out.push(id[1]);
  }
  return out;
}

// 全部 .field 的 id（全局口径，用于与基线比对）
function allFieldIds(src) {
  const out = [];
  const re = /<div class="field(?: chk[^"]*)?">([\\s\\S]*?)(?=<div class="field(?: chk[^"]*)?">|<\\/fieldset>|<\\/div>\\s*<\\/fieldset>|<div class="dim"|<div id="setLogFileInfo"|$)/g;
  let m;
  while ((m = re.exec(src))) {
    const id = /id="([^"]+)"/.exec(m[1]);
    if (id) out.push(id[1]);
  }
  return out;
}

// .fieldgroup 的边界：统计每个 .field 是否落在某个 .fieldgroup 内
function countUngroupedFields(src) {
  // 把每个 .fieldgroup 的内容抠掉，剩下的 .field 就是未被包裹的
  let stripped = src.replace(/<div class="fieldgroup[^"]*">[\\s\\S]*?<\\/div>\\s*<\\/div>/g, '');
  // 上面的非贪婪可能切不干净；用更稳的办法：数总 field 数 - 各 fieldgroup 内 field 数
  const total = (src.match(/<div class="field(?: chk[^"]*)?">/g) || []).length;
  let grouped = 0;
  const re = /<div class="fieldgroup[^"]*">([\\s\\S]*?)<\\/div>\\s*<\\/div>/g;
  let m;
  while ((m = re.exec(src))) grouped += (m[1].match(/<div class="field(?: chk[^"]*)?">/g) || []).length;
  return { total, grouped, ungrouped: total - grouped };
}

// 基线：<chk|in> <id>
function baselineIds() {
  return fs.readFileSync(BASELINE, 'utf8').split('\\n')
    .map(l => l.trim()).filter(l => l && !l.startsWith('#'))
    .map(l => l.split(/\\s+/)[1]);
}
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

const sets = fieldsets(sec);
const named = sets.filter(s => fieldKinds(s.body).length > 0);

// ---------- A. 分离性：每个组内类型不再交叉 ----------
console.log('\n[A] 每个 fieldset 内复选框与输入框不交叉');
for (const s of named) {
  const k = fieldKinds(s.body);
  const f = flips(k);
  const sep = isSeparated(k);
  ok(sep, '「' + s.legend + '」' + (sep ? '已分离' : '仍交叉') + '（翻转 ' + f + ' 次，顺序 = ' + (k.join(' ') || '(空)') + '）');
}
// 至少要有 4 个含字段的组，否则上面的循环可能因为解析失败而"空跑全过"
ok(named.length === 4, '含字段的 fieldset 数量 = ' + named.length + '，期望 4（防止解析失败导致空跑）');

// 交叉的两组必须真的变成「同类型连续」
const diao = named.find(s => s.legend && s.legend.startsWith('调度'));
const restart = named.find(s => s.legend && s.legend.startsWith('需要重启'));
ok(!!diao && isSeparated(fieldKinds(diao.body)), '「调度」组已分离');
ok(!!restart && isSeparated(fieldKinds(restart.body)), '「需要重启才生效」组已分离');
if (diao) {
  const k = fieldKinds(diao.body);
  ok(k.filter(x => x === 'CHK').length === 3 && k.filter(x => x === 'IN').length === 3,
    '「调度」组仍是 3 复选框 + 3 输入框（实际 ' + k.filter(x=>x==='CHK').length + '/' + k.filter(x=>x==='IN').length + '）');
}
if (restart) {
  const k = fieldKinds(restart.body);
  ok(k.filter(x => x === 'CHK').length === 2 && k.filter(x => x === 'IN').length === 11,
    '「需要重启」组仍是 2 复选框 + 11 输入框（实际 ' + k.filter(x=>x==='CHK').length + '/' + k.filter(x=>x==='IN').length + '）');
}

// ---------- B. 分组完整性 ----------
console.log('\n[B] 不存在未分组的 .field');
const g = countUngroupedFields(sec);
ok(g.total > 0, '解析到 .field 总数 = ' + g.total + '（>0 才说明解析有效）');
ok(g.ungrouped === 0, '未被 .fieldgroup 包裹的 .field = ' + g.ungrouped + '（期望 0）');
ok(g.grouped === g.total, '.fieldgroup 内覆盖 ' + g.grouped + '/' + g.total);
for (const s of named) {
  ok(/class="fieldgroup/.test(s.body), '「' + s.legend + '」含 .fieldgroup 容器');
}

// ---------- B2. 行级复核：每个 .fieldgroup 内部类型必须单一 ----------
// 为什么要有这一节：字符区间法会被 HTML 注释和 class 里的 '>' 干扰，
// 可能把「已正确分组」误判成「未分组」。行级扫描（本文件里每个 .field、
// 每个 fieldgroup 开闭都独占一行）不依赖区间推断，结论更硬。
// 「组内类型单一」正是「不交叉」的结构性证据 —— 它比"翻转次数"更贴近本质：
// 只要每个容器只装一种控件，视觉上就不可能交叉，与视口列数无关。
console.log('\n[B2] 行级复核：各 .fieldgroup 内类型单一');
const lines = html.split(/\r?\n/);
const lstart = lines.findIndex(l => l.includes('<h2>设置'));
const lend = lines.findIndex(l => l.includes('<div id="modal"'));
let lcur = null, lgidx = -1;
const lmembers = [];
const lorphan = [];
for (let i = lstart; i < lend; i++) {
  const L = lines[i];
  const open = /<div class="fieldgroup([^"]*)">/.exec(L);
  if (open) { lcur = open[1].includes('checks') ? 'CHK' : 'IN'; lgidx++; lmembers[lgidx] = { type: lcur, fields: [], lineNo: i + 1 }; continue; }
  if (lcur && /^\s*<\/div>\s*$/.test(L)) {
    const indent = L.match(/^\s*/)[0].length;
    const openIndent = lines[lmembers[lgidx].lineNo - 1].match(/^\s*/)[0].length;
    if (indent === openIndent) lcur = null;
    continue;
  }
  const f = /<div class="field( chk[^"]*)?">/.exec(L);
  if (f) {
    const rec = { kind: f[1] ? 'CHK' : 'IN', line: i + 1 };
    if (lcur) lmembers[lgidx].fields.push(rec); else lorphan.push(rec);
  }
}
ok(lmembers.length === 6, '行级扫描到 ' + lmembers.length + ' 个 .fieldgroup，期望 6');
ok(lorphan.length === 0, '行级扫描：不在任何 group 内的 .field = ' + lorphan.length + '（期望 0）');
let mixed = 0;
for (let i = 0; i < lmembers.length; i++) {
  const kinds = [...new Set(lmembers[i].fields.map(f => f.kind))];
  if (kinds.length > 1) { mixed++; console.log('     混合: [' + i + '] ' + kinds.join('/')); }
}
ok(mixed === 0, '每个 .fieldgroup 内类型单一（混合的 = ' + mixed + '）');
// 行级与区间法必须给出同样的字段总数，否则说明有一种解析是错的
const ltotal = lmembers.reduce((a, m) => a + m.fields.length, 0) + lorphan.length;
ok(ltotal === 27, '行级扫描字段总数 = ' + ltotal + '，期望 27');

// ---------- C. 零漂移 ----------
console.log('\n[C] 字段 id 零漂移');
const base = baselineIds();
const now = allFieldIds(sec);
ok(base.length === 27, '基线 id 数 = ' + base.length + '，期望 27');
ok(now.length === 27, '当前 id 数 = ' + now.length + '，期望 27');
const missing = base.filter(x => !now.includes(x));
const added = now.filter(x => !base.includes(x));
ok(missing.length === 0, '丢失的 id: ' + (missing.length ? missing.join(', ') : '(无)'));
ok(added.length === 0, '新增的 id: ' + (added.length ? added.join(', ') : '(无)'));
// 集合相等即可，**顺序必然会变** —— 分组本身就要把同类排列到一起。
// 因此不复用基线顺序，只比对排序后的集合（顺序变化不构成漂移）。
ok(base.slice().sort().join(',') === now.slice().sort().join(','), 'id 集合与基线一致（顺序变化是分组的必然结果）');
// 反向确认：顺序确实变了，否则说明根本没做分组
ok(base.join(',') !== now.join(','), 'id 顺序已因分组而改变（若未变则说明分组未真正发生）');

// label[for] 指向存在的 id
const allIdsInSec = [...sec.matchAll(/<input[^>]*id="([^"]+)"/g)].map(m => m[1]);
const fors = [...sec.matchAll(/<label for="([^"]+)"/g)].map(m => m[1]);
const badFor = fors.filter(f => !allIdsInSec.includes(f));
ok(badFor.length === 0, 'label[for] 指向不存在的 id: ' + (badFor.length ? badFor.join(', ') : '(无)'));
ok(fors.length === 11, 'label[for] 数量 = ' + fors.length + '，期望 11（复选框数）');

// ---------- D. 结构保真 ----------
console.log('\n[D] 文案与属性保真');
const legends = sets.map(s => s.legend).filter(Boolean);
ok(legends.includes('调度（保存后立即生效）'), 'legend「调度（保存后立即生效）」保留');
ok(legends.includes('成长中心自动动作（保存后立即生效）'), 'legend「成长中心自动动作（保存后立即生效）」保留');
ok(legends.includes('历史与日志（保存后立即生效）'), 'legend「历史与日志（保存后立即生效）」保留');
ok(legends.some(l => l.startsWith('需要重启才生效')), 'legend「需要重启才生效」保留');
ok(legends.length === 5, 'legend 数量 = ' + legends.length + '，期望 5（组边界未增未减）');

// 抽查关键属性原文
ok(sec.includes('placeholder="9,21"'), 'setCheckinHours 的 placeholder 保留');
ok(sec.includes('placeholder="22"'), 'setKeepaliveHours 的 placeholder 保留');
ok(sec.includes('守卫轮 ticker 在启动时创建，改动需重启'), 'setGrowthInterval 的长 hint 保留');
ok(sec.includes('id="setLogFileInfo"'), '#setLogFileInfo 保留');
ok(sec.includes('前两项是纯收益'), '成长组的收益/消耗说明保留');
ok(/id="setTravelInterval"[^>]*min="10"[^>]*max="3600"/.test(sec), 'setTravelInterval 的 min/max 保留');
ok(/id="setGrowthInterval"[^>]*min="60"[^>]*max="86400"/.test(sec), 'setGrowthInterval 的 min/max 保留');

// ---------- E. 无 emoji ----------
console.log('\n[E] 风格约定');
const emoji = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}]/u;
ok(!emoji.test(sec), '设置面板无 emoji');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + tail;
fs.writeFileSync('settings_layout_gen.js', out);
console.log('生成: settings_layout_gen.js (' + out.length + ' 字节)');
