// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：每页条数持久化到 localStorage。
//
// 背景（用户报告）：分页条「每页 [30] 条」改完之后刷新页面又回到 30 —— 不持久化。
//
// 断言策略：抽出真实的 loadPageSize / savePageSize / clampPageSize / renderPager，
// 配一个**真实的 localStorage 假实现**（内存 Map + setItem/getItem/removeItem），
// 而不是打桩返回常量 —— 否则测不出"存进去又读回来"这条路径。
//
// 关键覆盖：
//  1. 从未设置过 → 默认 30
//  2. 改值 → 写入 localStorage；重新 loadPageSize() 读回同一个值（持久化的本质）
//  3. 两个列表（请求日志 / 任务历史）共用同一个键
//  4. 存了非法值（0 / 负数 / 999 / 'abc' / 空串）→ 读回时被夹紧，不炸
//  5. localStorage 抛异常（隐私模式）→ 回落默认值，不影响功能
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(path, 'utf8');

function grabBlock(startMarker) {
  const start = html.indexOf(startMarker);
  if (start < 0) throw new Error('找不到: ' + startMarker);
  let i = html.indexOf('{', start), depth = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}') { depth--; if (depth === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + startMarker);
}

const pieces = [
  grabBlock('function loadPageSize('),
  grabBlock('function savePageSize('),
  grabBlock('function clampPageSize('),
  grabBlock('function pageCount('),
  grabBlock('function pageIndex('),
  grabBlock('function renderPager('),
];

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false }; }
const $ = id => els[id] || (els[id] = mkEl(id));

// ⚠ 这三个常量**从 webui.html 实读**，不硬编码。
//
// 原先这里把三个常量抄进了测试装置（PAGE_SIZE_MIN/MAX 与 DefaultPageSize
// 各写死一个数）。后果：源码里把它改成 15，这套测试**照样全绿**
//（它验的是自己抄的那份），而断言文案还写着"与后端一致"，
// 会误导人以为源码也是 30。
//
// 这与本项目反复出现的"测量方式测不到那一份"是同一类错误：
// **测试装置抄了被测对象的常量，就等于没有测它。**
const pickConst = (name) => {
  const m = new RegExp('const\\\\s+' + name + '\\\\s*=\\\\s*(\\\\d+)').exec(html);
  if (!m) throw new Error('源码里找不到常量 ' + name + ' —— 抽取失败，测试装置失效');
  return Number(m[1]);
};
const PAGE_SIZE_MIN = pickConst('PAGE_SIZE_MIN');
const PAGE_SIZE_MAX = pickConst('PAGE_SIZE_MAX');
const DefaultPageSize = pickConst('DefaultPageSize');
const LS_PAGESIZE = 'wb2api.pagesize';

// 真实的 localStorage 假实现（内存 Map），不是打桩常量 —— 要能验证读写往返。
function makeStorage(opts) {
  const map = new Map();
  return {
    _map: map,
    getItem(k) { if (opts && opts.throwOnRead) throw new Error('denied'); return map.has(k) ? map.get(k) : null; },
    setItem(k, v) { if (opts && opts.throwOnWrite) throw new Error('denied'); map.set(k, String(v)); },
    removeItem(k) { map.delete(k); },
  };
}
let localStorage = makeStorage();
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// ---------- 1. 默认值 ----------
console.log('\n[1] 从未设置过时用默认 ' + DefaultPageSize);
localStorage = makeStorage();
ok(loadPageSize() === DefaultPageSize,
  'localStorage 为空 → 默认 ' + DefaultPageSize + '（实际 ' + loadPageSize() + '）');
// ⚠ 这条断言的含义已修正。
//
// 原先断言的是"DefaultPageSize 常量恰好等于 30"，文案是"（与后端一致）" ——
// 两句都过时了：
//   · 数值：前端默认已按用户要求改成 15
//   · 文案："与后端一致"不再是要求（后端 DefaultPageSize 是**服务端单页上限**，
//     前端这个是**界面首屏偏好**，两者语义不同，不该绑定）
//
// 现在改为断言"抽取成功且在合法区间" —— 真正的判据是上面那条
// loadPageSize() === DefaultPageSize（行为一致），
// 而不是"某个魔数等于多少"。数值本身不该被测试钉死：
// 它是产品决策，改它不该让测试红。
ok(DefaultPageSize > 0 && DefaultPageSize <= PAGE_SIZE_MAX,
  'DefaultPageSize 在合法区间内（实读自源码，实际 ' + DefaultPageSize + '）');

// ---------- 2. 持久化往返（本次修复的核心） ----------
console.log('\n[2] 改值 → 写入 → 读回（持久化的本质）');
localStorage = makeStorage();
savePageSize(100);
ok(localStorage.getItem('wb2api.pagesize') === '100', 'savePageSize 写入了 localStorage');
ok(loadPageSize() === 100, 'loadPageSize 读回 100 —— 模拟刷新页面后仍是 100');
// 再模拟一次「新会话」：只保留 storage，重新读
ok(loadPageSize() === 100, '连续两次读取都是 100（不是一次性副作用）');
savePageSize(7);
ok(loadPageSize() === 7, '改成 7 后读回 7（可反复改）');

// ---------- 3. 两个列表共用一个键 ----------
console.log('\n[3] 请求日志与任务历史共用同一个键');
ok(/const logPage = \{[^}]*limit: loadPageSize\(\)/.test(html), 'logPage.limit 由 loadPageSize() 初始化（不再是写死的 30）');
ok(/const histPage = \{[^}]*limit: loadPageSize\(\)/.test(html), 'histPage.limit 由 loadPageSize() 初始化');
localStorage = makeStorage();
savePageSize(50);
ok(loadPageSize() === 50, '一处修改，两处共用（单一键 = 单一偏好）');
// 页面里只应有一个 pagesize 键名定义
const keyDefs = (html.match(/LS_PAGESIZE = '/g) || []).length;
ok(keyDefs === 1, 'LS_PAGESIZE 只定义一次（实际 ' + keyDefs + '）');

// ---------- 4. 存入非法值时的鲁棒性 ----------
console.log('\n[4] 非法持久值被夹紧，不炸');
for (const [stored, want] of [['0', 1], ['-5', 1], ['999', 300], ['abc', 1], ['', 1], ['45.9', 45], ['  77  ', 77]]) {
  localStorage = makeStorage();
  localStorage.setItem('wb2api.pagesize', stored);
  const got = loadPageSize();
  ok(got === want, '存 "' + stored + '" → 读回 ' + got + '（期望 ' + want + '）');
}
localStorage = makeStorage();
localStorage.setItem('wb2api.pagesize', 'NaN');
ok(loadPageSize() === 1, '存 "NaN" → 读回 1（不产生 NaN 请求参数）');

// ---------- 5. localStorage 不可用（隐私模式） ----------
console.log('\n[5] localStorage 抛异常时静默回落');
localStorage = makeStorage({ throwOnRead: true });
ok(loadPageSize() === DefaultPageSize,
  '读抛异常 → 回落默认 ' + DefaultPageSize + '（实际 ' + loadPageSize() + '）');
localStorage = makeStorage({ throwOnWrite: true });
let threw = false;
try { savePageSize(120); } catch { threw = true; }
ok(threw === false, '写入抛异常不向外抛（被 try/catch 吞掉）');

// ---------- 6. renderPager 改值时确实落了盘 ----------
console.log('\n[6] 在分页条上改值会持久化');
function makeStub(containerId) {
  const el = mkEl(containerId);
  const parsed = { inputs: [], btns: [] };
  const parse = () => {
    const h = el.innerHTML;
    const inputs = [];
    let m;
    const re = /<input[^>]*data-page="size"[^>]*>/g;
    while ((m = re.exec(h))) {
      const val = /value="([^"]*)"/.exec(m[0]);
      inputs.push({ value: val ? val[1] : '', onchange: null, onkeydown: null });
    }
    const btns = [];
    const bre = /<button data-page="(prev|next)"([^>]*)>/g;
    while ((m = bre.exec(h))) btns.push({ page: m[1], disabled: /\bdisabled\b/.test(m[2]), onclick: null });
    return { inputs, btns };
  };
  el.querySelectorAll = sel => (/button/.test(sel) ? parsed.btns : []);
  el.querySelector = sel => (/input/.test(sel) ? parsed.inputs[0] || null : null);
  let _h = '';
  Object.defineProperty(el, 'innerHTML', {
    get: () => _h,
    set: v => { _h = v; const p = parse(); parsed.inputs.length = 0; parsed.btns.length = 0;
                p.inputs.forEach(x => parsed.inputs.push(x)); p.btns.forEach(x => parsed.btns.push(x)); },
  });
  return el;
}

localStorage = makeStorage();
els.p1 = makeStub('p1');
const st = { offset: 0, limit: 30, total: 743, container: 'p1', onGo: () => {} };
renderPager(st);
const box = $('p1').querySelector('input');
box.value = '120';
box.onchange();
ok(st.limit === 120, 'state.limit 更新为 120');
ok(localStorage.getItem('wb2api.pagesize') === '120', '同时在分页条上改值也写入 localStorage（实际 ' + localStorage.getItem('wb2api.pagesize') + '）');
ok(loadPageSize() === 120, '刷新后读回 120');

// 未变化时不应写入（避免无谓写盘）
localStorage = makeStorage();
els.p2 = makeStub('p2');
const st2 = { offset: 0, limit: 100, total: 743, container: 'p2', onGo: () => {} };
renderPager(st2);
const box2 = $('p2').querySelector('input');
box2.value = '100';
box2.onchange();
ok(localStorage.getItem('wb2api.pagesize') === null, '值未变化时不写盘');

// 夹紧后的值写盘的是夹紧值而不是原始输入
localStorage = makeStorage();
els.p3 = makeStub('p3');
const st3 = { offset: 0, limit: 30, total: 743, container: 'p3', onGo: () => {} };
renderPager(st3);
const box3 = $('p3').querySelector('input');
box3.value = '999';
box3.onchange();
ok(localStorage.getItem('wb2api.pagesize') === '300', '输入 999 落盘的是夹紧后的 300（不是 999）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('pagesize_persist_gen.js', out);
console.log('生成: pagesize_persist_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
