// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// TDZ 回归测试：验证 webui.html 的真实脚本按**文件中的顺序**执行时不抛
// "Cannot access 'X' before initialization"。
//
// 为什么必须这样做：既有的生成式测试都是「按标记抽出函数块 → 拼进新作用域」，
// 拼接顺序由生成器决定，与文件里的真实顺序无关 —— 因此**看不见 TDZ 类错误**。
// 实测教训：loadPageSize 曾声明在 logPage 之后，const DefaultPageSize 处于
// 暂时性死区，浏览器报 ReferenceError，而全部 9 套抽取式测试都是绿的。
//
// 本测试改为：把真实 <script> 整段放进浏览器执行，观察是否抛错。
// 用最小 DOM 桩让脚本能跑到底（脚本是 IIFE，出错会冒到 window.onerror）。
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');
const cp = require('child_process');

const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]);
const main = scripts.reduce((a, b) => (a.length > b.length ? a : b), '');

// 逐个标识符做静态顺序检查（快速、精确指出问题在哪一行）
const src = main;
function declLine(re) {
  const m = re.exec(src);
  if (!m) return null;
  return src.slice(0, m.index).split('\n').length;
}
const checks = [
  ['const PAGE_SIZE_MIN', /const PAGE_SIZE_MIN\b/],
  ['const PAGE_SIZE_MAX', /const PAGE_SIZE_MAX\b/],
  ['const DefaultPageSize', /const DefaultPageSize\b/],
  ['const LS_PAGESIZE', /const LS_PAGESIZE\b/],
  ['const logPage', /const logPage\s*=/],
  ['const histPage', /const histPage\s*=/],
];
const lines = {};
for (const [name, re] of checks) lines[name] = declLine(re);
console.log('脚本内声明行号（相对 <script> 起点）:');
for (const [name, ln] of Object.entries(lines)) console.log('  ' + name.padEnd(24) + (ln == null ? '缺失' : ln));

// 找出所有 loadPageSize() 的调用行，且排除函数声明自身与注释
const callLines = [];
src.split('\n').forEach((L, i) => {
  if (/loadPageSize\s*\(\s*\)/.test(L) && !/function loadPageSize/.test(L) && !/^\s*\/\//.test(L)) {
    callLines.push({ line: i + 1, text: L.trim().slice(0, 90) });
  }
});
console.log('\nloadPageSize() 的调用点:');
callLines.forEach(c => console.log('  line ' + c.line + ': ' + c.text));

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

console.log('\n断言:');
// 核心：loadPageSize 读取的每个 const 都必须在最早调用点之前声明
const firstCall = Math.min(...callLines.map(c => c.line));
ok(Number.isFinite(firstCall), '存在 loadPageSize() 调用点');
for (const name of ['const PAGE_SIZE_MIN', 'const PAGE_SIZE_MAX', 'const DefaultPageSize', 'const LS_PAGESIZE']) {
  const ln = lines[name];
  ok(ln != null && ln < firstCall,
    `${name} (line ${ln}) 声明在最早的 loadPageSize() 调用 (line ${firstCall}) 之前`);
}
ok(lines['const logPage'] > lines['const DefaultPageSize'], 'logPage 声明在 DefaultPageSize 之后（供其读取）');
ok(lines['const histPage'] > lines['const DefaultPageSize'], 'histPage 声明在 DefaultPageSize 之后');

// 每个标识符只应声明一次（重复 const 会直接 SyntaxError）
console.log('\n重复声明检查:');
for (const [name, re] of checks) {
  const g = new RegExp(re.source, 'g');
  const n = (src.match(g) || []).length;
  ok(n === 1, `${name} 声明 ${n} 次（应为 1）`);
}

// 真机执行：整段脚本在浏览器里跑，任何 TDZ / ReferenceError 都会冒到 onerror
const probe = `<!doctype html><html><head><meta charset="utf-8"></head><body>
<div id="host"></div>
<script>
window.__err = null;
window.addEventListener('error', e => { window.__err = String(e.message || e.error); });
</script>
<script>
// 最小 DOM 桩：脚本会 querySelector / getElementById 一批元素
(function(){
  const mk = (id) => {
    const el = { id, hidden:false, className:'', textContent:'', innerHTML:'', value:'',
                 checked:false, disabled:false, style:{}, dataset:{}, childNodes:[{textContent:'面板'}],
                 classList:{ add(){}, remove(){}, toggle(){}, contains(){return false} },
                 addEventListener(){}, removeEventListener(){}, appendChild(){}, removeChild(){},
                 querySelector(){return null}, querySelectorAll(){return []},
                 closest(){return null}, getBoundingClientRect(){return {x:0,y:0,width:0,height:0,right:0,bottom:0}},
                 setAttribute(){}, getAttribute(){return null}, focus(){}, blur(){}, select(){} };
    return el;
  };
  const cache = new Map();
  document.getElementById = id => { if(!cache.has(id)) cache.set(id, mk(id)); return cache.get(id); };
  document.querySelector = () => null;
  document.querySelectorAll = () => [];
  document.createElement = () => mk('tmp');
  document.addEventListener = () => {};
  window.addEventListener = () => {};
  window.__WB2API_KEY__ = '__WB2API_KEY__';
  try { Object.defineProperty(localStorage, 'clear', { value(){}, configurable:true }); } catch(e){}
})();
</script>
<!--REAL-->
<script>
try {
  __REAL_SCRIPT__
  document.title = 'SCRIPT_OK';
} catch (e) {
  document.title = 'SCRIPT_ERR:' + (e && e.message ? e.message : String(e));
}
</script>
</body></html>`;

const page = probe.replace('__REAL_SCRIPT__', main);
// ⚠ 探针文件必须写在**本脚本所在目录**（而不是 cwd），并且 Chrome 要打开
// 它的**绝对路径**。旧写法是 `fs.writeFileSync('tdz_probe.html', ...)`
// 配 `file:///tdz_probe.html` —— 后者是根目录下的文件，永远打不开，
// 于是"真实脚本执行无异常"这条断言恒失败（而它是本套件的核心断言）。
const PROBE = path.join(__dirname, 'tdz_probe.html');
fs.writeFileSync(PROBE, page);

// ---------------------------------------------------------------------------
// 第二层：把整段真实脚本**按文件里的原始顺序**在真实 JS 引擎里跑一遍。
//
// 历史做法是用 headless Chrome 执行整页。但本机 Chrome 已无法产出输出
// （连 data: URL 都返回空，属环境问题而非被测代码问题），因此改为两条腿：
//   A. 优先用 Chrome（环境正常时它最接近真实浏览器）
//   B. Chrome 不可用时用 Node 执行同一段脚本 —— 同样是 V8，
//      同样按原顺序求值，TDZ / ReferenceError 一样会抛出来。
// 两者都不可用才算失败，并把原因写清楚，避免"没跑却报通过"的假绿。
// ---------------------------------------------------------------------------
const CHROME = require('./chrome_path.js').resolveChrome();

// 先声明（runInNode 会引用它，const 有 TDZ —— 这里自己就不能踩同一个坑）
const DOM_STUB = `
  const __mk = (id) => ({
    id, hidden: false, className: '', textContent: '', innerHTML: '', value: '',
    checked: false, disabled: false, style: {}, dataset: {}, childNodes: [{ textContent: '面板' }],
    classList: { add(){}, remove(){}, toggle(){}, contains(){ return false; } },
    addEventListener(){}, removeEventListener(){}, appendChild(){}, removeChild(){},
    querySelector(){ return null; }, querySelectorAll(){ return []; },
    closest(){ return null; }, setAttribute(){}, getAttribute(){ return null; },
    focus(){}, blur(){}, select(){},
    getBoundingClientRect(){ return { x:0,y:0,width:0,height:0,right:0,bottom:0 }; },
  });
  globalThis.document = {
    getElementById: (id) => __mk(id),
    querySelector: () => null,
    querySelectorAll: () => [],
    createElement: () => __mk('tmp'),
    addEventListener: () => {},
    body: __mk('body'),
  };
  globalThis.window = globalThis;
  globalThis.__WB2API_KEY__ = '__WB2API_KEY__';
  globalThis.localStorage = {
    _m: new Map(),
    getItem(k){ return this._m.has(k) ? this._m.get(k) : null; },
    setItem(k,v){ this._m.set(k, String(v)); },
    removeItem(k){ this._m.delete(k); },
  };
  globalThis.navigator = { clipboard: null };
  globalThis.location = { href: 'file:///ui', origin: 'file://', protocol: 'file:', search: '', hash: '', pathname: '/ui' };
  globalThis.performance = { now: () => 0 };
  globalThis.fetch = () => Promise.resolve({ ok: true, json: async () => ({}), text: async () => '' });
  globalThis.setInterval = () => 0;
  globalThis.clearInterval = () => {};
  globalThis.addEventListener = () => {};
`;

function chromeOk() {
  try {
    const r = cp.spawnSync(CHROME, ['--headless=new', '--disable-gpu', '--dump-dom',
      'data:text/html,<title>PING</title>'], { encoding: 'utf8', timeout: 20000 });
    return r.stdout && r.stdout.includes('PING');
  } catch { return false; }
}

// 用 Node 执行真实脚本：需要最小 DOM 桩。
// 脚本是 IIFE，会在加载时立即执行 —— 与浏览器里的时机一致。
function runInNode(src) {
  const wrapper = `
    ${DOM_STUB}
    try {
      ${src}
      return 'SCRIPT_OK';
    } catch (e) {
      return 'SCRIPT_ERR:' + (e && e.message ? e.message : String(e));
    }
  `;
  try {
    // eslint-disable-next-line no-new-func
    const fn = new Function(wrapper);
    return fn();
  } catch (e) {
    return 'SCRIPT_ERR:' + (e && e.message ? e.message : String(e));
  }
}

let t;
let how = '';
if (chromeOk()) {
  // 用探针文件的**真实绝对路径**（含盘符 → file:///D:/... 形式）。
  // 旧代码写死 file:///tdz_probe.html，那是"根目录下的文件"，从未存在。
  const probeURL = 'file:///' + PROBE.replace(/\\/g, '/');
  const args = ['--headless=new', '--disable-gpu', '--virtual-time-budget=2500',
                '--dump-dom', probeURL];
  const r = cp.spawnSync(CHROME, args, { encoding: 'utf8', timeout: 30000 });
  const title = /<title>([\s\S]*?)<\/title>/.exec(r.stdout);
  t = title ? title[1] : '(no title)';
  how = 'Chrome';
} else {
  t = runInNode(main);
  how = 'Node（Chrome 在本机不可用，已降级到同为 V8 的 Node 执行同一段脚本）';
}
console.log('\n整段脚本执行结果（' + how + '）: ' + t);
ok(t === 'SCRIPT_OK', '真实脚本按原顺序执行无异常（TDZ / ReferenceError 已消除）');

// 自检：这个门禁必须真的能抓到 TDZ。
// 造一段同形态的坏代码（const 声明在使用之后），确认 runInNode 会报错。
// 没有这一步，一个"永远返回 SCRIPT_OK"的实现也能让门禁假装通过。
{
  const bad = `
    function useIt() { return LATE; }
    const r0 = useIt();
    const LATE = 1;
  `;
  const r = runInNode(bad);
  ok(r.startsWith('SCRIPT_ERR') && /before initialization/.test(r),
    '自检：门禁能抓到 TDZ（构造用例返回 ' + r + '）');
}

console.log(fail === 0 ? '\n=== TDZ 回归测试通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
