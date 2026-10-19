// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：设置面板「分组结构 + 组内复选框与输入框分离 + 字段零漂移」。
//
// 与既有 7 套测试同一套路：从 webui.html 里**正则提取真实结构**做断言，
// 不复制副本 —— 否则实现改了测试仍会绿（假绿）。
//
// 覆盖 6 类断言：
//  A. 分离性：每个 fieldset 内 .field 的类型不交叉（本次需求的核心可测条件）
//  B. 容器完整性：每个 .field 都落在容器（.fieldgroup / .formgrid）内，且容器内类型单一
//  B2. 行级复核：不依赖区间推断的第二口径，两口径的字段总数必须一致
//  C. 零漂移：字段 id 集合与钉住的清单逐字一致，且全部来自旧基线（只删、不改名）
//  D. 结构保真：legend 基础文案 + 生效时机徽章 + min/max + 关键说明原文未变
//  E. 无 emoji
//  F. 收敛契约：被有意删除的 id 不得复活，只读项必须以 *RO 形态存在
//
// ═══════════════════════════════════════════════════════════════════════════
// 2026-10-19：整轮重定期望口径（判定 = (a) 锚点过期，**不是** (b) 产品缺陷）
//
// 四条红的根因是**有意的界面收敛**，不是产品把分组搅乱。证据链：
//   · 0e762cb 设置页重设计：5 个平铺 fieldset → 6 组按主题切分 + 三色徽章标生效时机
//   · c1cb20f 删减收敛：删 11 项 / 3 项转只读 / 2 项隐藏（提交信息原文
//     "用户要求：删掉前端不需要的设置；必要但不该改的只读展示或直接隐藏"）
//   · e95b814 再隐藏 3 个纯收益开关；be285f1 移除「启用每日签到」开关
//   · webui.html:1619-1622 / 1631-1632 的内联注释逐条写明删减理由
//   · 同一项在基线工作树（865ae96）跑出**同一批数字** ⇒ 不是"这一轮改坏的"，
//     而是这份夹具自 2026-09-13 写出后没跟上这三次收敛
//   · 真实浏览器（隔离实例 7881 + CDP 9251）实测与静态解析逐字一致
//
// 因此本文件只改**期望值口径**：不删断言、不弱化。能换成更强形式的都换掉了
// （典型：「未包裹的 .field」从"只认 .fieldgroup"改成"认两种容器"，并补上
//  "每个容器内类型单一" —— 那才是"不交叉"的结构性本质）。
// ═══════════════════════════════════════════════════════════════════════════
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');
// 基线文件**随测试一起入库**（tests/frontend/settings-baseline-ids.txt）。
// 此前它指向 D:/project_GIT/workbuddy2api/tasks/... —— **另一个仓库**的
// 绝对路径，且无兜底：换机器立刻抛 ENOENT。这正是"测试只在开发机可跑"
// 的典型形态，与上一轮修掉的 7863 依赖同类。
//
// 2026-10-19 起它的角色变了：**不再是"当前应有的 id 清单"**（那 27 项里有 20 项
// 已被有意收敛掉），而是"重设计前的真快照"，用来钉住一条硬性质：
// 现在的字段只能从这 27 项里**剩下来**，不能改名、不能凭空新增（见 [C]）。
const BASELINE = (process.env.WB2API_REPO || __dirname + '/../..') +
  '/tests/frontend/settings-baseline-ids.txt';
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';

const header = `
const fs = require('fs');
const BASELINE = (process.env.WB2API_REPO || __dirname + '/../..') + '/tests/frontend/settings-baseline-ids.txt';
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(WEBUI, 'utf8');

// 只截设置面板那一段：从 <section data-key="settings"> 到该 section 的闭合标签。
//
// # 为什么终点不能用 <div id="modal">（T6 踩到的坑）
//
// 原先切到模态框为止。T6 在设置 section 之后、模态框之前插入了
// 「动态上游面板宿主」块（含一句带 ⚠ 的 HTML 注释），于是这段切片
// 把**不属于设置面板**的内容也包了进来，emoji 断言因此假红。
//
// 正确做法是按 DOM 结构切：设置面板的 <section> 有自己的闭合标签，
// 切到它的 </section> 为止（设置面板内**没有**嵌套 section，见 webui.html:1612-1673）。
//
// # 为什么起点从「<h2>设置」换成「<section data-key=」 （2026-10-19）
//
// 旧锚点认的是**文案**：谁把标题从「设置」改成「控制台设置」，这里就 indexOf = -1。
// 而空切片的坏结局是**静默假绿**：下面绝大多数断言在空字符串上都会"全过"。
// data-key 是导航与面包屑共用的结构键，改名风险比文案低得多。
// 仍然保留 <h2>设置 的存在性断言（见 [D]），只是它不再承担"切片起点"这个重任。
const SET_START = html.indexOf('<section data-key="settings">');
if (SET_START < 0) throw new Error('找不到 <section data-key="settings">（锚点失效，别让空切片变成假绿）');
const SET_END = html.indexOf('</section>', SET_START);
if (SET_END < 0) throw new Error('找不到设置面板的 </section>');
const sec = html.slice(SET_START, SET_END);
if (!sec.length) throw new Error('设置面板切片为空');

// 按 DOM 顺序切出每个 fieldset 的内部。
//
// 2026-10-19 新增两个派生字段 —— legend 里现在带**生效时机徽章**：
//   <legend>网络与容错 <span class="badge badge--cold">重启生效</span></legend>
// 于是：
//   base  = 去掉徽章、空白归一后的「主题名」（用于逐字比对与可读的失败消息）
//   badge = 徽章文本（承载"立即生效 / 重启生效 / 只读"这套新设计契约）
// 不这么分的话，断言只能整串比 legend，载入内容的主次就糊在一起了。
function fieldsets(src) {
  const out = [];
  const re = /<fieldset([^>]*)>([\\s\\S]*?)<\\/fieldset>/g;
  let m;
  while ((m = re.exec(src))) {
    const body = m[2];
    const raw = /<legend>([\\s\\S]*?)<\\/legend>/.exec(body);
    const legend = raw ? raw[1].trim() : null;
    const base = legend === null ? null
      : legend.replace(/<span class="badge[^"]*">[\\s\\S]*?<\\/span>/g, '').replace(/\\s+/g, ' ').trim();
    const bm = legend === null ? null : /<span class="badge[^"]*">([\\s\\S]*?)<\\/span>/.exec(legend);
    out.push({ attrs: m[1] || '', body: body, legend: legend, base: base,
      badge: bm ? bm[1].trim() : null });
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
//
// 收敛后当前每个含字段组只装一种控件（flips = 0 也满足），但判据不写死成 0：
// "一组里先复选框后输入框、各自成段"同样是合规的分离形态，不该被误杀。
function isSeparated(kinds) {
  if (kinds.length === 0) return true;
  const kindsPresent = new Set(kinds).size;
  return flips(kinds) === kindsPresent - 1;
}

// 全部 .field 的 id（全局口径）。
//
// 终止前瞻列的是**能出现在 .field 之后、且不该被算进本字段**的那些起始标记：
// 下一个 .field / </fieldset> / 下一个容器 / .subgroup / .info-line。
//
// ⚠ 踩过的坑：旧版前瞻里有 <div id="setLogFileInfo"，而重设计后它变成了
//   <p class="info-line" id="setLogFileInfo">（webui.html:1650）—— 标签从 div 换了 p，
//   那条前瞻从此永不命中。它没造成误判（后面还有 </fieldset> 兜着），
//   但正是"前瞻写死具体元素"这种做法的典型腐烂方式。所以这里按**语义标记**列，
//   不留具体 id；并把 <p class="info-line" 也列上，避免字段体把信息行吞进来。
function allFieldIds(src) {
  const out = [];
  const re = /<div class="field(?: chk[^"]*)?">([\\s\\S]*?)(?=<div class="field(?: chk[^"]*)?">|<\\/fieldset>|<div class="(?:fieldgroup[^"]*|formgrid)">|<div class="subgroup"|<p class="info-line"|<div class="info-line"|$)/g;
  let m;
  while ((m = re.exec(src))) {
    const id = /id="([^"]+)"/.exec(m[1]);
    if (id) out.push(id[1]);
  }
  return out;
}

// 容器完整性统计。
//
// 容器 = .fieldgroup（复选框组）| .formgrid（输入框网格）。
//
// # 为什么口径从「只认 .fieldgroup」改成「两种容器」（2026-10-19）
//
// 旧口径把 .formgrid 里的字段当成"未被包裹的裸字段"，于是在设置页重设计后
// 报「未被 .fieldgroup 包裹的 .field = 3」—— 那 3 个字段其实正躺在 .formgrid 里。
// 这是**解析口径过期**，不是产品把分组搅乱了：重设计把"复选框进 fieldgroup、
// 输入框进 formgrid"做成了两种并列容器（webui.html:1634 checks / 1644、1655 formgrid），
// 浏览器实测 inFieldgroup = 4、inFormgrid = 3、无孤儿字段。
//
// 于是判据改成问**本质问题**：字段有没有落进某个容器、容器里是不是只有一种控件。
// 这比"容器具体叫什么"更贴近原始需求（组内复选框与输入框不交叉），也更难被绕过。
//
// ⚠ 旧实现里还留着一个 'const stripped = src.replace(...)' 却从未使用 ——
//   那是"先按一种容器抠掉、再用另一种口径统计"的半成品残留。一并清掉。
function countContainers(src) {
  const total = (src.match(/<div class="field(?: chk[^"]*)?">/g) || []).length;
  let grouped = 0, containers = 0, mixed = 0;
  const re = /<div class="(?:fieldgroup[^"]*|formgrid)">([\\s\\S]*?)<\\/div>\\s*<\\/div>/g;
  let m;
  while ((m = re.exec(src))) {
    containers++;
    const k = fieldKinds(m[1]);
    grouped += k.length;
    if (new Set(k).size > 1) mixed++;
  }
  return { total: total, grouped: grouped, ungrouped: total - grouped,
    containers: containers, mixed: mixed };
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
  ok(sep, '「' + s.base + '」' + (sep ? '已分离' : '仍交叉') + '（翻转 ' + f + ' 次，顺序 = ' + (k.join(' ') || '(空)') + '）');
}

// 防呆下限：**2026-10-19 按重设计后的结构重定**（旧值 4 对应重设计前的 4 个含字段组）。
//
// 为什么要两条一起断言：单看 named.length 无法区分"结构对"与"解析器只认到一半"。
// 加上 fieldset 总数（6）与下面 [D] 里 legend 逐字比对，才堵得住空跑。
//
// 旧下限为什么会失效（这是本文件本轮要解释的事实，不是"数值调一下"）：
// 重设计后「日常调度」组的字段被删空 —— 签到/保活统一为 30 分钟被动扫描，
// 开关与时点都成了概念残留（be285f1 / c1cb20f），该组只留 .info-line 信息行。
// 于是 4 组含字段 → 3 组。这个数字是**防呆下限**，结构变了就必须重新定，
// 不能因为"改小了看起来像放水"就把它删掉。
const NAMED_EXPECT = 3;
ok(sets.length === 6, '设置面板 fieldset 总数 = ' + sets.length + '，期望 6（重设计后按主题切分 6 组）');
ok(named.length === NAMED_EXPECT, '含字段的 fieldset 数量 = ' + named.length + '，期望 ' + NAMED_EXPECT + '（防呆下限，2026-10-19 重定）');

// 「日常调度」：原「调度」组 —— 旧夹具里它是交叉重灾区（CHK CHK IN IN CHK IN）。
// 收敛后它的字段整体消失，只剩信息行。断言改写成「纯信息组」形态：
// 组还在（信息要有落点）、承载用 .info-line、且 .field 必须为 0。
// 三个方向都有失败力：整组被删 / 字段被塞回来 / 信息行改成别的容器，任一都红。
const diao = sets.find(s => (s.base || '').startsWith('日常调度'));
ok(!!diao, '「日常调度」组存在（字段被收敛掉，但组本身是调度信息的落点）');
if (diao) {
  const k = fieldKinds(diao.body);
  ok(k.length === 0, '「日常调度」已收敛为零字段的纯信息组（实际 .field = ' + k.length + '）');
  ok((diao.body.match(/class="info-line"/g) || []).length >= 2, '「日常调度」用 .info-line 承载签到/保活信息（实际 ' + (diao.body.match(/class="info-line"/g) || []).length + ' 条）');
  ok(/id="schInfo"/.test(diao.body) && /id="setKeepaliveRO"/.test(diao.body), '「日常调度」的信息落点 #schInfo / #setKeepaliveRO 保留');
}

// 「网络与容错」：承接原「需要重启才生效」组（重设计按主题改名，生效时机改由徽章承载）。
// 旧断言要的「已分离 + 2 复选框 + 11 输入框」中，11 个输入框里 8 个已被有意删除
// （c1cb20f：SSE 超时×2 / 429 软冷却 / 在途 / 熔断×3 / 粘性 TTL），2 个转为只读。
// 现在只剩 1 个可编辑输入框 —— 所以断言改成"组在、不交叉、恰好 1 个输入框、
// 徽章是「重启生效」、只读项以 *RO 形态在场"，每一条都有独立失败力。
const restart = sets.find(s => (s.base || '').startsWith('网络与容错'));
ok(!!restart, '「网络与容错」组存在（原「需要重启才生效」组按主题改名）');
if (restart) {
  const k = fieldKinds(restart.body);
  ok(isSeparated(k), '「网络与容错」组内不交叉（顺序 = ' + (k.join(' ') || '(空)') + '）');
  ok(k.filter(x => x === 'CHK').length === 0 && k.filter(x => x === 'IN').length === 1,
    '「网络与容错」只留 1 个可编辑输入框（实际 ' + k.filter(x => x === 'CHK').length + ' 复选框 / ' + k.filter(x => x === 'IN').length + ' 输入框）');
  ok(restart.badge === '重启生效', '「网络与容错」的生效时机由徽章承载（实际 ' + JSON.stringify(restart.badge) + '）');
  ok(/id="setSanitizeRO"/.test(restart.body), '指纹脱敏改为只读展示 #setSanitizeRO（c1cb20f：3 项只读之一）');
}

// 各组字段数（收敛后的真值）：成长中心自动化 4 复选框；历史与日志 2 输入框。
const gz = sets.find(s => (s.base || '').startsWith('成长中心自动化'));
if (gz) {
  const k = fieldKinds(gz.body);
  ok(k.filter(x => x === 'CHK').length === 4 && k.filter(x => x === 'IN').length === 0,
    '「成长中心自动化」仍是 4 复选框 + 0 输入框（实际 ' + k.filter(x => x === 'CHK').length + '/' + k.filter(x => x === 'IN').length + '）');
}
const rz = sets.find(s => (s.base || '').startsWith('历史与日志'));
if (rz) {
  const k = fieldKinds(rz.body);
  ok(k.filter(x => x === 'CHK').length === 0 && k.filter(x => x === 'IN').length === 2,
    '「历史与日志」仍是 0 复选框 + 2 输入框（实际 ' + k.filter(x => x === 'CHK').length + '/' + k.filter(x => x === 'IN').length + '）');
}
const allKinds = fieldKinds(sec);
ok(allKinds.filter(x => x === 'CHK').length === 4 && allKinds.filter(x => x === 'IN').length === 3,
  '全局 .field = 4 复选框 + 3 输入框 = 7（实际 ' + allKinds.length + '）');

// ---------- B. 容器完整性 ----------
console.log('\n[B] 每个 .field 都落在容器内，且容器内类型单一');
const g = countContainers(sec);
ok(g.total > 0, '解析到 .field 总数 = ' + g.total + '（>0 才说明解析有效）');
ok(g.containers === 3, '容器数（.fieldgroup + .formgrid）= ' + g.containers + '，期望 3（1 个 checks + 2 个 formgrid）');
ok(g.ungrouped === 0, '未落在任何容器内的 .field = ' + g.ungrouped + '（期望 0）');
ok(g.grouped === g.total, '容器内覆盖 ' + g.grouped + '/' + g.total);
// 容器内类型单一 = 「不交叉」的结构性证据。它比"翻转次数"更本质：
// 只要每个容器只装一种控件，视觉上就不可能交叉，与视口列数无关。
ok(g.mixed === 0, '类型混合的容器 = ' + g.mixed + '（期望 0）');
// 每个含字段组**恰好 1 个容器**：这同时禁掉两种坏形态 ——
// 字段裸露在 fieldset 下（容器 0 个），或把同类控件又拆成多个容器（>1 个）。
for (const s of named) {
  const n = (s.body.match(/<div class="(?:fieldgroup[^"]*|formgrid)">/g) || []).length;
  ok(n === 1, '「' + s.base + '」恰好 1 个容器（实际 ' + n + '）');
}

// ---------- B2. 行级复核：每个容器内部类型必须单一 ----------
// 为什么要有这一节：字符区间法会被 HTML 注释和 class 里的 '>' 干扰，
// 可能把「已正确分组」误判成「未分组」。行级扫描（本文件里每个 .field、
// 每个容器开闭都独占一行）不依赖区间推断，结论更硬。
// 两口径的字段总数还必须在最后互相对上（见本节的最后一条断言）。
console.log('\n[B2] 行级复核：各容器内类型单一');
const lines = html.split(/\r?\n/);
const lstart = lines.findIndex(l => l.includes('<section data-key="settings"'));
const lend = lines.findIndex(l => l.includes('<div id="modal"'));
let lcur = null, lgidx = -1;
const lmembers = [];
const lorphan = [];
for (let i = lstart; i < lend; i++) {
  const L = lines[i];
  const open = /<div class="(fieldgroup[^"]*|formgrid)">/.exec(L);
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
ok(lstart >= 0 && lend > lstart, '行级扫描区间有效（' + lstart + ' → ' + lend + '，锚点 <section data-key="settings"> → <div id="modal">）');
ok(lmembers.length === 3, '行级扫描到 ' + lmembers.length + ' 个容器（.fieldgroup/.formgrid），期望 3');
ok(lorphan.length === 0, '行级扫描：不在任何容器内的 .field = ' + lorphan.length + '（期望 0）');
let mixed = 0;
for (let i = 0; i < lmembers.length; i++) {
  const kinds = [...new Set(lmembers[i].fields.map(f => f.kind))];
  if (kinds.length > 1) { mixed++; console.log('     混合: [' + i + '] ' + kinds.join('/')); }
}
ok(mixed === 0, '每个容器内类型单一（混合的 = ' + mixed + '）');
// 行级与区间法必须给出同样的字段总数，否则说明有一种解析是错的
const ltotal = lmembers.reduce((a, m) => a + m.fields.length, 0) + lorphan.length;
ok(ltotal === 7, '行级扫描字段总数 = ' + ltotal + '，期望 7');
ok(ltotal === g.total, '行级与区间法字段总数一致（' + ltotal + ' vs ' + g.total + '）');

// ---------- C. 零漂移 ----------
console.log('\n[C] 字段 id 零漂移');
const base = baselineIds();
const now = allFieldIds(sec);
// 当前应有的 7 个 id —— 逐字钉住。
// 依据：2026-10-19 静态解析 + 真实浏览器（隔离实例 7881 / CDP 9251）DOM 实测，两者逐字一致。
// 为什么不沿用旧基线那 27 项：其中 20 项已被**有意的界面收敛**移除或转只读/隐藏
// （c1cb20f 删 11 + 只读 3 + 隐藏 2、e95b814 隐藏 3、be285f1 移除签到开关）。
// 只比**集合**不比顺序：分组本身就要把同类控件排到一起，顺序变化不构成漂移。
const NOW_IDS = ['setGrowthMakeup', 'setGrowthRedeem', 'setGrowthOpen', 'setGrowthDraw',
  'setCheckinLogDays', 'setReqLogDays', 'setUpTimeout'];
ok(base.length === 27, '旧基线 id 数 = ' + base.length + '，期望 27（重设计前快照，仍是文件级契约）');
ok(now.length === NOW_IDS.length, '当前 id 数 = ' + now.length + '，期望 ' + NOW_IDS.length);
const missing = NOW_IDS.filter(x => !now.includes(x));
const added = now.filter(x => !NOW_IDS.includes(x));
ok(missing.length === 0, '丢失的 id: ' + (missing.length ? missing.join(', ') : '(无)'));
ok(added.length === 0, '新增的 id: ' + (added.length ? added.join(', ') : '(无)'));
ok(now.slice().sort().join(',') === NOW_IDS.slice().sort().join(','), 'id 集合与钉住的清单一致');
// ★ 这条是旧基线文件在新形态下的用法：现在的 id 必须**全部来自**那 27 项。
// 收敛只被允许"删"，不允许"改名/凭空造新 id" —— 改名会让它红，纯删除不会。
// 这也是为什么不能把 settings-baseline-ids.txt 删掉：它是"没有换名字"的证明。
const notInOld = now.filter(x => !base.includes(x));
ok(notInOld.length === 0, '当前 id 均由旧基线沿用而非改名/新增（不在旧基线里的: ' + (notInOld.length ? notInOld.join(', ') : '(无)') + '）');
//
// ⚠ 旧版这里还有一条 ok(base.join(',') !== now.join(','), 'id 顺序已因分组而改变')。
//   它的原意是"反向确认分组真的发生过"，但集合已经从 27 缩到 7 之后，它**恒为真**
//   —— 那就成了"永远为真的断言"，只会稀释信号。删掉它，其职责由
//   [B]「每个含字段组恰好 1 个容器」+「容器内类型单一」正面承接（那两条才真的能红）。

// label[for] 指向存在的 id
const allIdsInSec = [...sec.matchAll(/<input[^>]*id="([^"]+)"/g)].map(m => m[1]);
const fors = [...sec.matchAll(/<label for="([^"]+)"/g)].map(m => m[1]);
const badFor = fors.filter(f => !allIdsInSec.includes(f));
ok(badFor.length === 0, 'label[for] 指向不存在的 id: ' + (badFor.length ? badFor.join(', ') : '(无)'));
// 关系式断言（比硬编码数字更耐结构变化）：label[for] 数必须等于复选框数
const chkCount = allKinds.filter(k => k === 'CHK').length;
ok(fors.length === chkCount, 'label[for] 数量 = ' + fors.length + '，期望 = 复选框数 ' + chkCount);
ok(fors.length === 4, 'label[for] 数量 = ' + fors.length + '，期望 4（收敛后的复选框数）');
// 两个解析器交叉验证：input 扫描 与 .field 扫描 必须给出同样多的带 id 输入控件
ok(allIdsInSec.length === now.length, '带 id 的 input = ' + allIdsInSec.length + ' 与 .field 解析出的 id = ' + now.length + ' 一致');

// ---------- D. 结构保真 ----------
console.log('\n[D] 文案与属性保真');
const legends = sets.map(s => s.base).filter(Boolean);
ok(legends.length === 6, 'legend 数量 = ' + legends.length + '，期望 6（组边界：6 组按主题切分）');
ok(legends.join('|') === '日常调度|成长中心自动化|历史与日志|网络与容错|会话粘性|运行信息',
  'legend 基础文案逐字一致: ' + legends.join(' / '));
// 生效时机从"legend 里的括注"（如「需要重启才生效（…）」）改成了**结构化的徽章**，
// 所以这里把六组徽章逐字钉住 —— 它是新设计里承载语义的那一半，不能只看主题名。
ok(sets.map(s => s.badge).join('|') === '自动执行|立即生效|立即生效|重启生效|只读|只读',
  '生效时机徽章逐字一致: ' + sets.map(s => String(s.badge)).join(' / '));
// 切片锚点从文案换成了 data-key，所以另外补一条对标题文案本身的断言（它仍然重要）
ok(sec.includes('<h2>设置'), '面板标题「设置」保留');
// 绑定/保存按钮是设置面板的两个唯一出口，不能随重设计消失
ok(/id="btnSettingsSave"/.test(sec) && /id="btnSettingsReload"/.test(sec), '「保存设置 / 放弃修改」按钮保留');

// 抽查关键属性原文（现存的 3 个数字框）
ok(/id="setCheckinLogDays"[^>]*min="1"[^>]*max="3650"/.test(sec), 'setCheckinLogDays 的 min/max 保留');
ok(/id="setReqLogDays"[^>]*min="1"[^>]*max="3650"/.test(sec), 'setReqLogDays 的 min/max 保留');
ok(/id="setUpTimeout"[^>]*min="1"/.test(sec), 'setUpTimeout 的 min 保留');
// ⚠ 坑：重设计后 #setLogFileInfo 从 <div> 变成了 <p class="info-line">（webui.html:1650）。
//   只断言 id 存在是不够的 —— 标签换了会让上面 allFieldIds 的"写死元素"前瞻失效。
//   所以这里连形态一起断言。
ok(/<p class="info-line" id="setLogFileInfo"/.test(sec), '#setLogFileInfo 保留且为 <p class="info-line">');
ok(sec.includes('消耗资源 · 默认关'), '成长组的收益/消耗说明保留');
ok(sec.includes('超过窗口的旧记录在下次裁剪时清除'), '日志保留天数的 hint 保留');
ok(/id="setGrowthMakeup"[^>]*><label for="setGrowthMakeup">自动补签<\/label>/.test(sec)
  && /id="setGrowthRedeem"[^>]*><label for="setGrowthRedeem">自动连登兑换<\/label>/.test(sec)
  && /id="setGrowthOpen"[^>]*><label for="setGrowthOpen">自动开盲盒<\/label>/.test(sec)
  && /id="setGrowthDraw"[^>]*><label for="setGrowthDraw">自动抽奖<\/label>/.test(sec),
  '成长组 4 个复选框的 id ↔ label 文案逐条对上');

// ---------- E. 无 emoji ----------
console.log('\n[E] 风格约定');
const emoji = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}\u{FE0F}]/u;
ok(!emoji.test(sec), '设置面板无 emoji');

// ---------- F. 收敛契约 ----------
console.log('\n[F] 收敛契约：被有意删掉的 id 不得复活，只读项须以 *RO 形态在场');
// 依据：0e762cb（重设计）/ c1cb20f（删 11 / 只读 3 / 隐藏 2）/ e95b814（再隐藏 3）/
// be285f1（移除「启用每日签到」开关）。这不是"永久禁令"，而是**契约登记**：
// 将来若真要把某项加回来，先改这份夹具并写清依据 —— 这正是"零漂移"在收敛期的形态。
const GONE = ['setCheckinEnabled', 'setKeepaliveEnabled', 'setCheckinHours', 'setKeepaliveHours',
  'setTravelAuto', 'setTravelInterval', 'setGrowthClaim', 'setGrowthAccept', 'setSoftRate',
  'setUpHeaderTimeout', 'setUpIdleTimeout', 'setMaxInFlight', 'setBreakerThreshold',
  'setBreakerCooldown', 'setBreakerCooldownMax', 'setSanitize', 'setUpstashURL',
  'setStickyEnabled', 'setStickyTTL', 'setGrowthInterval'];
const revived = GONE.filter(x => new RegExp('id="' + x + '"').test(sec));
ok(revived.length === 0, '被收敛掉的 ' + GONE.length + ' 个 id 无一复活（复活的: ' + (revived.length ? revived.join(', ') : '(无)') + '）');
const RO = ['setKeepaliveRO', 'setSanitizeRO', 'setStickyRO'];
const missingRO = RO.filter(x => !new RegExp('id="' + x + '"').test(sec));
ok(missingRO.length === 0, '3 项只读展示元素齐备（缺的: ' + (missingRO.length ? missingRO.join(', ') : '(无)') + '）');
// ⚠ 三条 *RO 的**值**是 JS 填充的（renderSettings 里塞 textContent），静态切片里看不到，
//   所以"只读行有值"这件事不在本文件断言范围 —— 它由浏览器侧探针负责
//   （实测：隔离实例 7881 里三行都渲染出「已启用 / 已启用 / 已启用 · TTL 30m」）。
//   这里刻意**不**写一条"看起来在验值"的恒真断言：那只会稀释信号。

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + tail;
fs.writeFileSync('settings_layout_gen.js', out);
console.log('生成: settings_layout_gen.js (' + out.length + ' 字节)');
