'use strict';
// lib_extract_check.js —— 「抽取列表缺依赖」的静态守卫（**纯静态，零依赖，不需要实例/Chrome**）
//
// # 它解决的问题（这条坑已经踩过三次）
//
// 一批前端夹具（gen_X.js → X_gen.js）的构造方式是：从 `internal/server/webui.html`
// 里用 `grabBlock('function foo(')` **把真实产品函数抠出来**，配一个最小 DOM 桩跑断言。
// 抽取列表是**手工维护**的，而产品侧会演进：被抠出来的函数**新调了一个帮手**，
// 帮手不会自动跟来 ⇒ 生成物一跑就 `ReferenceError: xxx is not defined`。
//
// 真实案例：
//   · fmtTaskExpiry  （两次，抽取列表里没跟）
//   · maskNick + autoCodes（一次，四个 growth 夹具**一起**红）
//
// # 为什么"红得很难查"
//
// 报错是 `ReferenceError`，抛在**第一条断言跑到那一段渲染**的时候。于是：
//   · 位置离原因很远（真正的原因在 gen 文件的抽取列表里，不在这里）
//   · 夹具**后面的断言一条都没跑**，看起来像"产品坏了"，实际是"量具坏了"
//     （判 a/b 的口径见 tests/frontend/README.md：先在基线工作树跑同一条）
//
// 所以本工具要做的就是把这件事**提前**、**点名**说清楚：在 generator 里、在写产物
// 之前，静态扫一遍"抽出来的块引用了哪些名字，这些名字谁提供"。
//
// # 设计上的三条硬约束
//
//  1. **不许用万能 Proxy / try-catch 兜底**。
//     用一个 Proxy 当全局对象去"跑一遍看缺谁"，或者 catch 住 ReferenceError 继续跑，
//     都会产出**假绿**：前者把"环境碰巧有这个名字"当成"依赖齐了"，后者把
//     "这 30 条断言全都没跑"当成"没失败"。本仓库反复吃过这类亏，明令禁止。
//     这里的判据是**静态的**：token 扫出来、集合减出来，谁都不许吞。
//
//  2. **宁可误报，不可漏报**。
//     漏报 = 生成物在别人机器上炸；误报 = 加一行豁免并写清理由（成本低且可见）。
//     所以启发式一律朝"多报"偏：拿不准就当**引用**。相应地，`extra` 豁免名单
//     必须**逐个名字写明理由**，不接受"整类豁免"。
//
//  3. **它只是守卫，不是编译器**。
//     手写词法器（本仓库零三方依赖，没有 acorn/esprima 可用）在少数语法形态上
//     会判错 —— 已知盲区写在下面 `KNOWN BLIND SPOTS` 一节，遇到就补规则/加豁免，
//     别指望它替代 `node --check`。
//
// # 已知盲区（都是"假阴性"方向，即可能漏报；遇到就补）
//
//   · 作用域被**粗化**成"整个块一坨"：内层箭头函数里声明的名字，在外层也被当成已声明。
//     这只会漏报（少报一个真缺的），不会误报。
//   · `i++ / 2` 这类"除法紧跟在自增后面"会被当成正则开始（极少见，靠 `+`/`-` 连写
//     的那条小规则挡掉大部分）。
//   · 对象方法的简写 `{ foo() {} }` 靠"`(` 前一个 token 是 `{`/`,`"识别 ——
//     如果写成 `{ async foo() {} }` 也能识别（`async` 是关键字，规则看的是再前一个）。
//   · with / eval 动态取值一律看不见（本来也不该出现在这个仓库）。
//
// # 这条检查**只接了 4 个 growth generator**
//
// 全仓 16 个 generator 对（见 run_frontend_suites.js 的 SUITES），本次只接了：
//   gen_growth_labels.js / gen_growth_pending_ui.js / gen_growth_select.js /
//   gen_growth_taskview.js
//
// 其余 12 个**没接**（父代理明确批的窄版本：一次性改 16 个文件的风险面太大，
// 而且它们各自的抽取列表/桩形态不同，需要逐个验证"现状是不是真的干净"——
// 万一某个 gen 现状就有**真缺**，接线会让它当场变红，那是另一件事）。
// 所以：**不要以为全仓都接上了**。接线的时候照 growth 这 4 个的样子抄一遍即可，
// 但每接一个都要先跑一次看它报什么（报出来的可能是**真缺**，不是误报）。
//
// # 接线注意（两条都是本次实际踩到的）
//
//  1. **凡是"只复制几个 gen 文件"的脚本，必须把这个 lib 一起复制。**
//     否则 gen 里那句 `require('./lib_extract_check.js')` 直接 MODULE_NOT_FOUND，
//     每一个变异都变成 GENFAIL —— 判别力被自己掩盖掉（我的一次性变异脚本就这么
//     空转过一轮，六个变异全报 GENFAIL，看起来"全红"，其实什么都没测到）。
//     仓库里的 `mutation_sweep.js` 用 robocopy 复制**整个仓库**，不受影响。
//
//  2. **本文件是普通 JS（不是 generator 模板）**，所以门禁 0（`String.raw` 模板里
//     反引号必须成对）**不适用于它** —— 这里写反引号是安全的。
//     但**gen 文件里的注释**不能写反引号（含注释也算模板内容），那条坑见
//     gen_growth_taskview.js 的 header 注释。
//
// # 它怎么用（generator 侧三行）
//
//     const { checkExtract } = require('./lib_extract_check.js');
//     checkExtract({ pieces, stub: header, productFile: WEBUI, label: 'gen_X.js', extra: [] });
//
// 放在 `writeFileSync` **之前** —— 缺依赖时要在写产物之前就退出，否则会留下
// 一个"生成成功但一跑就炸"的半成品，掩盖住真正的失败点。
const fs = require('fs');

// ---------------------------------------------------------------- 词法器

// 关键字与"值关键字"：它们不是标识符引用，直接不算候选。
const KEYWORDS = new Set([
  'if', 'else', 'for', 'while', 'do', 'switch', 'case', 'default', 'break', 'continue',
  'return', 'function', 'class', 'extends', 'new', 'delete', 'typeof', 'instanceof', 'in',
  'of', 'void', 'yield', 'await', 'async', 'static', 'get', 'set', 'let', 'const', 'var',
  'try', 'catch', 'finally', 'throw', 'import', 'export', 'from', 'as', 'debugger', 'with',
]);

// 值关键字 / 内建字面量：不是"谁该提供"的名字。
const VALUE_KEYWORDS = new Set(['this', 'super', 'true', 'false', 'null', 'undefined', 'arguments', 'NaN', 'Infinity']);

// 运行环境自带的全局。⚠ 加进这里的每一个名字都等于"放弃守卫它" —— 只放真的全局。
const BUILTINS = new Set([
  // JS 语言内建
  'Object', 'Array', 'Function', 'String', 'Number', 'Boolean', 'Symbol', 'BigInt', 'Math',
  'JSON', 'Date', 'RegExp', 'Error', 'TypeError', 'RangeError', 'SyntaxError', 'EvalError',
  'ReferenceError', 'URIError', 'AggregateError', 'Map', 'Set', 'WeakMap', 'WeakSet', 'Promise',
  'Proxy', 'Reflect', 'Intl', 'ArrayBuffer', 'DataView', 'Uint8Array', 'Int8Array', 'Uint16Array',
  'Int16Array', 'Uint32Array', 'Int32Array', 'Float32Array', 'Float64Array', 'BigInt64Array',
  'BigUint64Array', 'SharedArrayBuffer', 'Atomics', 'FinalizationRegistry', 'WeakRef',
  'parseInt', 'parseFloat', 'isNaN', 'isFinite', 'encodeURIComponent', 'decodeURIComponent',
  'encodeURI', 'decodeURI', 'escape', 'unescape', 'eval', 'globalThis', 'structuredClone',
  'queueMicrotask', 'atob', 'btoa', 'TextEncoder', 'TextDecoder', 'AbortController', 'AbortSignal',
  'URL', 'URLSearchParams', 'FormData', 'Blob', 'File', 'FileReader', 'Headers', 'Request',
  'Response', 'WebSocket', 'EventSource', 'IntersectionObserver', 'MutationObserver',
  'ResizeObserver', 'requestAnimationFrame', 'cancelAnimationFrame', 'getComputedStyle',
  'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'fetch', 'crypto', 'performance',
  // 浏览器侧（被抽出来的产品函数真的会用）
  'window', 'document', 'location', 'navigator', 'history', 'screen', 'localStorage',
  'sessionStorage', 'alert', 'confirm', 'prompt', 'console', 'CustomEvent', 'Event', 'Node',
  'HTMLElement', 'Image', 'XMLHttpRequest', 'matchMedia', 'scrollTo', 'open', 'close', 'top',
  // Node 侧（夹具的运行环境）
  'require', 'module', 'exports', 'process', 'Buffer', '__dirname', '__filename', 'global',
]);

function isIdStart(c) {
  return /[A-Za-z_$]/.test(c) || (c.charCodeAt(0) > 0x7f && !/\s/.test(c));
}
function isIdPart(c) {
  return /[A-Za-z0-9_$]/.test(c) || (c.charCodeAt(0) > 0x7f && !/\s/.test(c));
}

// tokenize 返回 [{type:'id'|'num'|'punct'|'str'|'regex'|'template', value, pos}]
// ⚠ 字符串/正则/模板的**字面量内容不产出 token**，但模板里的 `${...}` **要**递归进去
//   —— 产品代码大量用模板串拼 HTML（`${esc(x)}`），漏了这一段就等于不检查。
function tokenize(src, opts) {
  const stopAtBrace = !!(opts && opts.stopAtBrace);
  const toks = [];
  let i = 0;
  let depth = 0;   // ()/[]/{} 的合计深度，仅用于 stopAtBrace
  const prev = () => toks[toks.length - 1];

  const regexAllowed = () => {
    const p = prev();
    if (!p) return true;
    if (p.type === 'id') return KEYWORDS.has(p.value) || VALUE_KEYWORDS.has(p.value);
    if (p.type === 'num' || p.type === 'str' || p.type === 'regex' || p.type === 'template') return false;
    if (p.type === 'punct') {
      if (p.value === ')' || p.value === ']') return false;
      // `i++ / 2`：自增/自减后面那个 `/` 是除法，不是正则开始。
      if ((p.value === '+' || p.value === '-') && toks.length >= 2
          && toks[toks.length - 2].value === p.value) return false;
      return true;
    }
    return true;
  };

  while (i < src.length) {
    const c = src[i];
    if (c === ' ' || c === '\t' || c === '\r' || c === '\n' || c === '\u00a0' || c === '\ufeff') { i++; continue; }
    if (c === '/' && src[i + 1] === '/') { while (i < src.length && src[i] !== '\n') i++; continue; }
    if (c === '/' && src[i + 1] === '*') {
      i += 2;
      while (i < src.length && !(src[i] === '*' && src[i + 1] === '/')) i++;
      i += 2;
      continue;
    }
    if (c === '"' || c === "'") {
      const start = i;
      i++;
      while (i < src.length && src[i] !== c) { if (src[i] === '\\') i++; i++; }
      i++;
      toks.push({ type: 'str', value: src.slice(start, i), pos: start });
      continue;
    }
    if (c === '`') {
      const r = scanTemplate(src, i);
      toks.push({ type: 'template', value: '`…`', pos: i });
      for (const t of r.tokens) toks.push(t);
      i = r.next;
      continue;
    }
    if (c === '/' && regexAllowed()) {
      const start = i;
      i++;
      let inClass = false;
      while (i < src.length) {
        const d = src[i];
        if (d === '\\') { i += 2; continue; }
        if (d === '[') inClass = true;
        else if (d === ']') inClass = false;
        else if (d === '/' && !inClass) { i++; break; }
        else if (d === '\n') break;   // 没闭合：当除法处理，别再吞
        i++;
      }
      while (i < src.length && /[a-z]/i.test(src[i])) i++;   // 标志位 gimsuy
      toks.push({ type: 'regex', value: src.slice(start, i), pos: start });
      continue;
    }
    if (/[0-9]/.test(c)) {
      const start = i;
      while (i < src.length && /[0-9a-fA-FxXoObB._eE+\-]/.test(src[i])) {
        // 别把 `1-2` 的减号吃掉：只有紧跟 e/E 时才允许符号
        if ((src[i] === '+' || src[i] === '-') && !/[eE]/.test(src[i - 1])) break;
        i++;
      }
      toks.push({ type: 'num', value: src.slice(start, i), pos: start });
      continue;
    }
    if (isIdStart(c)) {
      const start = i;
      while (i < src.length && isIdPart(src[i])) i++;
      toks.push({ type: 'id', value: src.slice(start, i), pos: start });
      continue;
    }
    // 括号深度（stopAtBrace 用）
    if (c === '(' || c === '[' || c === '{') depth++;
    else if (c === ')' || c === ']') depth--;
    else if (c === '}') {
      if (stopAtBrace && depth === 0) return { tokens: toks, next: i + 1, closed: true };
      depth--;
    }
    toks.push({ type: 'punct', value: c, pos: i });
    i++;
  }
  return { tokens: toks, next: i, closed: false };
}

// 模板字面量：扫到收尾反引号，`${...}` 里的表达式**递归** tokenize
// （⚠ 本文件自己的注释里也不能写裸反引号 —— 见文件末尾"反引号"那节）。
function scanTemplate(src, i) {
  const out = { tokens: [], next: i + 1 };
  let j = i + 1;
  while (j < src.length) {
    const c = src[j];
    if (c === '\\') { j += 2; continue; }
    if (c === '`') { out.next = j + 1; return out; }
    if (c === '$' && src[j + 1] === '{') {
      const r = tokenize(src.slice(j + 2), { stopAtBrace: true });
      for (const t of r.tokens) out.tokens.push(t);
      if (!r.closed) { out.next = src.length; return out; }
      j = j + 2 + r.next;
      continue;
    }
    j++;
  }
  out.next = src.length;
  return out;
}

// ---------------------------------------------------------------- 声明的名字

// declaredNames：一段源码里**声明**了哪些名字（最佳努力）。
// 覆盖：function / class 声明、let/const/var 的绑定模式、catch 形参、
//       函数形参、箭头形参。不求精确到作用域（见文件头"已知盲区"）。
function declaredNames(code) {
  const toks = tokenize(code).tokens;
  const out = new Set();
  const at = (k) => toks[k];
  const sig = (k, dir) => {   // 第 k 个 token 之后(dir=1)/之前(dir=-1)第一个 token
    let j = k + dir;
    while (j >= 0 && j < toks.length) { if (toks[j].type !== 'comment') return toks[j]; j += dir; }
    return null;
  };
  // 收集一段"绑定区"里的名字：从 k 开始，遇到 stop 位置前收集；
  // 跳过 `.` 后的属性名、`: ` 前的键名；`=` 的初始化式整段跳过。
  function collectBindings(k, stopAt) {
    let depth = 0;
    let j = k;
    let names = 0;
    while (j < toks.length) {
      const t = toks[j];
      if (t.type === 'punct') {
        if (t.value === '(' || t.value === '[' || t.value === '{') depth++;
        else if (t.value === ')' || t.value === ']' || t.value === '}') { if (depth === 0 && stopAt.indexOf(t.value) >= 0) return j; depth--; }
        if (depth === 0 && t.value === '=') {
          // 跳过初始化式：到深度 0 的 `,` / `;` / 停止符为止
          let d2 = 0;
          j++;
          while (j < toks.length) {
            const u = toks[j];
            if (u.type === 'punct') {
              if (u.value === '(' || u.value === '[' || u.value === '{') d2++;
              else if (u.value === ')' || u.value === ']' || u.value === '}') { if (d2 === 0) break; d2--; }
              else if (d2 === 0 && (u.value === ',' || u.value === ';')) break;
            }
            j++;
          }
          continue;
        }
        if (depth === 0 && stopAt.indexOf(t.value) >= 0) return j;
        if (depth < 0) return j;
      }
      if (t.type === 'id' && !KEYWORDS.has(t.value) && !VALUE_KEYWORDS.has(t.value)) {
        const p = at(j - 1);
        const n = at(j + 1);
        const isProp = p && p.type === 'punct' && p.value === '.';
        const isKey = n && n.type === 'punct' && n.value === ':';
        if (!isProp && !isKey) { out.add(t.value); names++; }
      }
      j++;
    }
    return j;
  }

  for (let k = 0; k < toks.length; k++) {
    const t = toks[k];
    if (t.type !== 'id') continue;
    if (t.value === 'function') {
      let m = k + 1;
      if (at(m) && at(m).value === '*') m++;
      const nm = at(m);
      if (nm && nm.type === 'id') out.add(nm.value);
      // 形参
      let p = m + 1;
      if (at(p) && at(p).value === '(') collectBindings(p + 1, [')']);
    } else if (t.value === 'class') {
      const nm = at(k + 1);
      if (nm && nm.type === 'id') out.add(nm.value);
    } else if (t.value === 'let' || t.value === 'const' || t.value === 'var') {
      collectBindings(k + 1, [';']);
    } else if (t.value === 'catch') {
      const p = at(k + 1);
      if (p && p.value === '(') collectBindings(k + 2, [')']);
    }
  }
  // 箭头函数形参：`x =>` 与 `(a, b) =>`
  for (let k = 0; k < toks.length; k++) {
    const t = toks[k];
    const n1 = at(k + 1);
    if (t.type === 'id' && n1 && n1.type === 'punct' && n1.value === '=' && at(k + 2) && at(k + 2).value === '>') {
      out.add(t.value);
    }
    if (t.type === 'punct' && t.value === '=' && n1 && n1.type === 'punct' && n1.value === '>') {
      const p = at(k - 1);
      if (p && p.type === 'punct' && p.value === ')') {
        // 往回找配对的 `(`
        let d = 0;
        for (let j = k - 1; j >= 0; j--) {
          const u = toks[j];
          if (u.type !== 'punct') continue;
          if (u.value === ')') d++;
          else if (u.value === '(') { d--; if (d === 0) { collectBindings(j + 1, [')']); break; } }
        }
      } else if (p && p.type === 'id' && !KEYWORDS.has(p.value)) {
        out.add(p.value);
      }
    }
  }
  return out;
}

// ---------------------------------------------------------------- 自由标识符

// freeIdentifiers：块里"引用了"的名字（已扣掉它自己声明的、属性名、对象键名）。
function freeIdentifiers(code) {
  const toks = tokenize(code).tokens;
  const declared = declaredNames(code);
  const seen = new Set();
  for (let k = 0; k < toks.length; k++) {
    const t = toks[k];
    if (t.type !== 'id') continue;
    if (KEYWORDS.has(t.value) || VALUE_KEYWORDS.has(t.value)) continue;
    const p = toks[k - 1];
    const n = toks[k + 1];
    // 属性访问 a.foo / a?.foo（`?.` 会被切成 `?` + `.`，所以看 `.` 就够）
    if (p && p.type === 'punct' && p.value === '.') continue;
    // 对象键 / 解构键：{ k: v }，`k` 前面只能是 `{` 或 `,`
    if (n && n.type === 'punct' && n.value === ':' && p && p.type === 'punct' && (p.value === '{' || p.value === ',')) continue;
    // 对象方法简写 { foo() {} } / { async foo() {} }
    if (n && n.type === 'punct' && n.value === '(') {
      const pp = p && p.type === 'id' ? (p.value === 'async' || p.value === 'get' || p.value === 'set' ? toks[k - 2] : p) : p;
      if (pp && pp.type === 'punct' && (pp.value === '{' || pp.value === ',')) continue;
    }
    if (declared.has(t.value)) continue;
    seen.add(t.value);
  }
  return seen;
}

// ---------------------------------------------------------------- 主判据

// analyze(opts) → { missing:[{name, block, hint}], blocks:[{head, free}], provided:Set }
//
// opts:
//   pieces       被抽出来的源码块（字符串数组 / 单个字符串 / [{name,code}]）
//   stub         桩的源码（通常就是 gen 里那个 header 模板）—— 从里面抠出它提供的名字
//   extra        额外豁免的名字（**每个都要有理由**，写在调用处注释里）
//   productFile  产品源码路径（只用来生成"加什么"的建议，读不到就退化）
//   label        报错前缀（通常传 gen 文件名）
function analyze(opts) {
  const o = opts || {};
  const raw = Array.isArray(o.pieces) ? o.pieces : [o.pieces];
  const blocks = raw.filter((x) => x != null).map((x) => {
    const code = typeof x === 'string' ? x : String(x.code || '');
    const head = (typeof x === 'object' && x.name) ? String(x.name) : (code.split('\n')[0] || '').trim().slice(0, 60);
    return { head, code };
  });

  const provided = new Set();
  for (const b of blocks) for (const n of declaredNames(b.code)) provided.add(n);
  if (o.stub) for (const n of declaredNames(String(o.stub))) provided.add(n);
  for (const n of (o.extra || [])) provided.add(n);
  for (const n of BUILTINS) provided.add(n);

  const product = o.productFile ? readMaybe(o.productFile) : '';
  const missing = [];
  for (const b of blocks) {
    for (const name of freeIdentifiers(b.code)) {
      if (provided.has(name)) continue;
      missing.push({ name, block: b.head, hint: suggest(name, product) });
    }
  }
  return { missing, blocks, provided };
}

// suggest：把"缺谁"变成"照做什么"。
//   · 产品里有 `function NAME(`  → 在 pieces 里加 grabBlock
//   · 产品里有 `const NAME = {…}` → 对象字面量，grabBlock 按花括号配平**能**抓
//     （它找标记后的第一个 `{` 再配平到收尾 `}` —— COLS 就是这么抓的）
//   · 产品里是别的声明语句（`let autoCodes = new Set();`）→ **抓不了**（没有配平的块）
//     ⇒ 只能写进桩里，并写清它的取值语义（autoCodes 踩过这个）
//   · 产品里根本没有 → 这个名字是夹具自己的东西（桩漏了 / 该写进 extra 豁免）
function suggest(name, product) {
  if (product) {
    const fn = new RegExp('function\\s+' + name + '\\s*\\(').exec(product);
    if (fn) {
      const line = product.slice(0, fn.index).split('\n').length;
      return '产品里有 function ' + name + '(（webui.html:' + line + '）⇒ 在 pieces 里加：'
        + "grabBlock('function " + name + "(')";
    }
    const decl = new RegExp('(?:let|const|var)\\s+' + name + '\\b').exec(product);
    if (decl) {
      const line = product.slice(0, decl.index).split('\n').length;
      const objRe = new RegExp('(?:let|const|var)\\s+' + name + '\\s*=\\s*\\{');
      if (objRe.test(product.slice(decl.index, decl.index + 60))) {
        return '产品里是 "const ' + name + ' = {…}"（对象字面量，webui.html:' + line + '）⇒ '
          + 'grabBlock 按花括号配平**能**抓 ⇒ 加 grabBlock(\'const ' + name + ' =\')';
      }
      return '产品里是 "let/const/var ' + name + '"（webui.html:' + line + '）—— 它是**声明语句**、'
        + '不是能配平的花括号块 ⇒ grabBlock 抓不了 ⇒ 在桩（header）里给它一份，并写清它的取值语义';
    }
  }
  return '产品里找不到这个名字 ⇒ 它是夹具自己的桩：补进 header，'
    + '或（确实是别处提供时）加进 checkExtract 的 extra 并写明理由';
}

function readMaybe(p) {
  try { return fs.readFileSync(p, 'utf8'); } catch { return ''; }
}

// checkExtract：不干净就**抛异常**（generator 因此非零退出；绝不静默、不 catch 掉）。
function checkExtract(opts) {
  const r = analyze(opts);
  if (!r.missing.length) return r;
  const label = (opts && opts.label) || '生成器';
  const lines = [
    '抽取列表缺依赖 —— ' + label + ' 抽出来的产品代码引用了没人提供的名字。',
    '',
    '这类缺口的后果：生成物一跑就 ReferenceError，**夹具在第一条断言之前就死**，',
    '看起来像产品坏了，其实是量具（抽取列表）没跟上产品演进。',
    '',
  ];
  const byName = new Map();
  for (const m of r.missing) {
    if (!byName.has(m.name)) byName.set(m.name, { ...m, blocks: [] });
    byName.get(m.name).blocks.push(m.block);
  }
  for (const [name, m] of byName) {
    lines.push('  缺 ' + name + '（被这些块引用：' + m.blocks.join(' / ') + '）');
    lines.push('    → ' + m.hint);
  }
  lines.push('');
  lines.push('（判据是静态的：token 扫出来的自由标识符 − 抽取集 − 桩 − 内建 − extra 豁免。');
  lines.push('  真·夹具自有的名字请加进 extra，并在调用处写明理由；不要整类豁免。）');
  const err = new Error(lines.join('\n'));
  err.missing = r.missing;
  throw err;
}

// ---------------------------------------------------------------- 自检

// selfTest：判别力自检（`node lib_extract_check.js --self-test`）。
//   ① 人为缺标识符的样例 ⇒ **必须红**
//   ② 现状的 4 个 growth generator ⇒ **必须绿**（不绿就是噪声，会被人关掉）
//   ③ 产品侧的"帮手"确实被抽出来时 ⇒ 不报（别把正常情况也报出来）
function selfTest() {
  const path = require('path');
  const { execFileSync } = require('child_process');
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  console.log('\n[1] 坏样例必须变红（否则这工具没有判别力）');
  const bad = analyze({
    pieces: ['function renderThing(list) { return list.map(x => maskNick(x.nickname) + missingHelper(x)); }'],
    stub: 'const esc = s => String(s);',
    label: 'sample',
  });
  const names = bad.missing.map((m) => m.name).sort();
  ok(names.indexOf('maskNick') >= 0, '报出 maskNick（实测：' + names.join(', ') + '）');
  ok(names.indexOf('missingHelper') >= 0, '报出 missingHelper');
  ok(names.indexOf('list') < 0, '不报形参 list（那是块自己声明的）');
  ok(names.indexOf('nickname') < 0, '不报属性名 nickname（x.nickname 不是引用）');
  ok(names.indexOf('String') < 0, '不报内建 String');
  let threw = false;
  try { checkExtract({ pieces: ['function f(){ return nope(); }'], stub: '', label: 'sample' }); } catch (e) { threw = true; }
  ok(threw, 'checkExtract 在缺依赖时**抛异常**（生成器因此非零退出，不静默）');

  console.log('\n[2] 传统启发式形态的假阳性排查');
  const tricky = analyze({
    pieces: [ 'function f(g) { const m = { a: 1, b() { return g; }, c: (z) => z + d }; '
      + 'const [p, q] = [1, 2]; const { k: r } = m; for (const s of [1]) { g(s); } '
      + 'try { g(1); } catch (e) { g(e); } return `${p}${q}${r}${d}`; }' ],
    stub: 'let d = 1; function g() {}',
  });
  const trickyNames = tricky.missing.map((m) => m.name);
  ok(trickyNames.length === 0, '对象键/方法简写/解构/箭头/for-of/catch/模板插值都不误报（实测：'
    + (trickyNames.join(', ') || '空') + '）');

  console.log('\n[3] 现状的 4 个 growth generator 必须绿');
  const gens = ['gen_growth_labels.js', 'gen_growth_pending_ui.js', 'gen_growth_select.js', 'gen_growth_taskview.js'];
  for (const g of gens) {
    let out = '', code = 0;
    try {
      out = execFileSync(process.execPath, [path.join(__dirname, g)], { cwd: __dirname, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
    } catch (e) { code = 1; out = (e.stdout || '') + (e.stderr || ''); }
    ok(code === 0, g + ' 跑起来仍绿' + (code ? '（前几行：' + out.split('\n').slice(0, 4).join(' | ') + '）' : ''));
  }

  console.log('\n[4] 建议文本要区分"能抓"和"抓不了"（这条建议我写错过一次）');
  const objHint = suggest('COLS', 'const COLS = {\n  growth: 6,\n};\n');
  ok(objHint.indexOf('grabBlock') >= 0, '对 const X = {…}（对象字面量）建议 grabBlock 抽取：' + objHint.slice(0, 40) + '…');
  const declHint = suggest('autoCodes', 'let autoCodes = new Set();\nlet autoCodesLoaded = false;\n');
  ok(declHint.indexOf('桩') >= 0 && declHint.indexOf("grabBlock('const autoCodes") < 0,
    '对 let X = new Set()（声明语句）建议写进桩、而不是 grabBlock：' + declHint.slice(0, 40) + '…');
  const noneHint = suggest('someFixtureHelper', 'function other() {}\n');
  ok(noneHint.indexOf('extra') >= 0, '产品里没有的名字 → 指到"补桩 / 写进 extra 并说明理由"');

  console.log(fail === 0 ? '\n=== 自检全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  return fail;
}

module.exports = {
  checkExtract, analyze, declaredNames, freeIdentifiers, tokenize, suggest,
  BUILTINS, KEYWORDS, VALUE_KEYWORDS,
};

// 直接 `node lib_extract_check.js` 就跑自检 —— 这样将来若要把它挂进
// run_frontend_suites.js 的 PROBES，只要加一个文件名（PROBES 里是**不带参数**跑的）。
//
// ⚠ 本轮**没有**去动 run_frontend_suites.js：那是共享门禁文件，父代理明确只批了
//   "接 4 个 generator"这一窄版本。加一行 PROBES 是将来的一件小事，但必须先有人
//   确认"整仓 16 个 gen 都接线之后套件仍然全绿"，否则套件会因为别人的 gen 变红。
if (require.main === module) {
  process.exit(selfTest() ? 1 : 0);
}
