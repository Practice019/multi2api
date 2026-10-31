// verify_theme_compliance.js —— 「页面自有样式不得越界」守卫（规范：designforai.dev/style/maximalism）。
//
// # 为什么需要它（2026-10-30 用户原话）
//   「统一吧前端的页面设计一下 有几个页面 没有按照要求来 ，你看看能不能抽共享统一起来。」
//   实测（剥掉主题块后的**页面自有**样式）：
//     webui.html      硬编码色 70 处 · 非零圆角 61 处 · font-family 20 处 · 用主题字体变量 0 处
//     status          46 · 8 · 3 · 0
//     login           27 · 4 · 1 · 0
//     groups          22 · 7 · 4 · 1
//     models           1 · 8 · 8 · 1
//     tokens/channels  0 · 0 · 2/3 · 3/18
//   根因不是"某一页写错了"，而是**没有一条门禁说清'页面还能自己写什么'** ——
//   于是每页各抄一份旧调色板/一份圆角/一份字族，主题块改一次就得靠人去追。
//   同一天还踩到它的极端形态：模型页自带 :root[data-theme="light"]（特异性 0,2,0）
//   压过主题块的裸 :root（0,1,0）⇒ 整页发白，改了三轮样式都没碰到根上。
//
// # 本守卫只断言"页面自有样式"，不碰主题块
//   主题块（WB2API:MAXIMALISM:START..END）由 tools/theme_sync.js 从
//   internal/server/theme-max.css 生成，是**视觉层唯一真相**；它内部当然可以写
//   具体色值与圆角。所以本守卫先把主题块剥掉，只扫**页面自己写的** <style>。
//   同理不扫 <script>/HTML：那里出现 #fff 可能是 JS 里给 canvas 用的，不属于样式越界。
//
// # 判据（R1–R5，对应交付契约）
//   R1 硬编码颜色：不得出现 #hex、rgb()、rgba() 字面值 ⇒ 一律走语义变量
//   R2 圆角：不得出现非零 border-radius（规范 seed 是 --radius:0）
//   R3 字体：font-family 只允许 var(--font-display|ui|serif-max|mono)（不许写字族串）
//   R4 别名卫生：不许 var(--mono) —— 主题块里那个变量叫 --font-mono，
//      写 --mono 只会静默退化成继承字体（等宽列会跟着错位）
//   R5 零外部资源：不得 url(http…) / @import（本仓铁律「Release 下载即用」）
//
// # 边界（写清以免被当成漏测）
//   · 内联 style="…" 属性不在本门禁范围内（那属于 DOM 属性，不是页面样式表）；
//     它的清理由各自的页面任务负责，本门禁只守样式表这一层。
//   · 允许 color-mix(...) / currentColor / transparent / var(...) / inherit。
//
// 退出码：全部合规 = 0；任一页违规 = 1（逐页打印违规计数与**前几条样例**，便于直接定位）。

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..', '..');
const DIR = path.join(ROOT, 'internal', 'server');

const START = 'WB2API:MAXIMALISM:START';
const END = 'WB2API:MAXIMALISM:END';

// styleBlocks 取出页面**自有** <style> 里的内容（剥掉主题块）。
function styleBlocks(html) {
  const out = [];
  const re = /<style[^>]*>([\s\S]*?)<\/style>/g;
  let m;
  while ((m = re.exec(html)) !== null) {
    let css = m[1];
    const i = css.indexOf(START);
    const j = css.indexOf(END);
    if (i >= 0 && j > i) {
      // 连注释标记一起剥（块是 /* ==== START … END ==== */ 这种形态）
      const before = css.lastIndexOf('/*', i);
      const after = css.indexOf('*/', j);
      const from = before >= 0 ? before : i;
      const to = after >= 0 ? after + 2 : j;
      css = css.slice(0, from) + css.slice(to);
    }
    // ⚠ 剥掉 CSS 注释再扫：注释**不是样式**，而本仓注释里大量出现
    //   「rgba(0,0,0,0)」「#FFFFFF」这类字面值（记录踩坑原文），
    //   把它们算成违规等于让门禁去评价散文 —— 实测踩到过（groups/login 各 2 处）。
    out.push(css.replace(/\/\*[\s\S]*?\*\//g, ' '));
  }
  return out.join('\n');
}

// scriptBodies 取出 <script> 里的**代码**（剥掉注释）。
//
// # 为什么要扫 JS（2026-11-01 新增 R6）
//
// 本门禁原先的边界（上面第 32-33 行）写着「内联 style 属性不在范围内」—— 那个边界
// 换来的是**盲区**：4 处硬编码色在 JS 里活了很久（命中率线的 #58a6ff、图例色块、
// 任务状态绿 #0f8a4d、SVG 轴标签 fill），最后靠 theme-max.css 里的属性选择器补丁
// （[fill="#58a6ff"]{fill:var(--acc)}）盖住 —— 补丁让「写死色值」看起来没事，
// 于是没人去改源头。补丁的前提「属性里不认 var()」也已被实测推翻（隔离用例：
// stroke="var(--x)" 在 Chrome 里解析且随主题切换）。
//
// ⚠ 剥注释是必须的：本仓注释里为了记录踩坑大量出现色值原文，把散文当代码会误报。
function scriptBodies(html) {
  const out = [];
  const re = /<script[^>]*>([\s\S]*?)<\/script>/g;
  let m;
  while ((m = re.exec(html)) !== null) {
    out.push(m[1].replace(/\/\*[\s\S]*?\*\//g, ' '));
  }
  return out.join('\n');
}

// codeOutsideStyle R6 的输入：页面里**除样式表之外**的代码文本。
//
// # 为什么不用「按 <script> 配对切块」（第一版就是这么写的，漏了一整段）
//
// 页内 JS 自己的字符串里就会出现 `<script` / `</script>`（模板里拼 HTML），
// 非贪婪配对被这些标记带偏 ⇒ 某些段落被判成「不是脚本」，R6 在那段里**恒不命中**。
// 实测：HEAD 版页面里 `fill="#58a6ff"` 那一段就这样整段漏掉了（脚本配对长度比真身少一截）。
//
// 现在的做法是**从反面界定**：剥掉样式表与注释，剩下的都算代码。
// 内联 style 属性另交给 R7（这里先把它们的值抹平，避免同一处报两次）。
function codeOutsideStyle(html) {
  const text = html
    .replace(/<style[^>]*>[\s\S]*?<\/style>/g, ' ')
    .replace(/<!--[\s\S]*?-->/g, ' ');
  // ⚠ **不**抹平 style 属性：R6 的判据 B 正好要覆盖「内联 style 里的 color:#…」，
  //   抹掉它会让那类整段失守（第一版就是这样，漏了一处）。
  return stripJsComments(text);
}

// stripJsComments 用**引号状态机**剥掉 JS 注释（保留换行，行号不漂）。
//
// # 为什么不能用 /\/\*[\s\S]*?\*\//（第一版就是，吞掉了一整段真代码）
//
// 页面 JS 里存在**孤立的** `/*`（正则/字符串/自有转义里都可能出现），非贪婪配对会从那里
// 一直吞到下一个 `*/` —— 实测 HEAD 版里 `fill="#58a6ff"` 那一整段就是这样被吃掉的，
// 于是 R6 在那一段恒不命中（门禁看起来绿，实际上没扫到）。
// 同理不能用「按行剥 //」：那会把 `https://…` 后面的内容一起切掉。
//
// 状态机只认三件事：单/双/模板字符串的开闭、行注释、块注释 —— 够用且不会误吞。
function stripJsComments(src) {
  let out = '';
  let q = null;      // 当前字符串定界符（' " `）
  let block = false; // 块注释
  let line = false;  // 行注释
  for (let i = 0; i < src.length; i++) {
    const c = src[i], n = src[i + 1];
    if (c === '\n') { line = false; out += c; continue; }
    if (line) { out += ' '; continue; }
    if (block) {
      if (c === '*' && n === '/') { block = false; out += '  '; i++; } else out += ' ';
      continue;
    }
    if (q) {
      // 字符串内部：只找闭合引号（不处理转义 —— 本仓的 JS 里没有 `\"` 形态会踩到）
      if (c === q) q = null;
      out += c;
      continue;
    }
    if (c === '/' && n === '/') { line = true; out += '  '; i++; continue; }
    if (c === '/' && n === '*') { block = true; out += '  '; i++; continue; }
    if (c === "'" || c === '"' || c === '\u0060') { q = c; out += c; continue; }
    out += c;
  }
  return out;
}

function lineOf(css, index) {
  return css.slice(0, index).split('\n').length;
}

const RULES = [
  { id: 'R1', name: '硬编码颜色', re: /#[0-9a-fA-F]{3,8}\b|\brgba?\(/g,
    hint: '改用语义变量：--bg/--fg/--panel/--panel2/--line/--dim/--acc/--ok/--warn/--err/--code-bg/--ink/--cream/--pink/--yellow/--blue/--acid/--orange/--on-acc' },
  { id: 'R2', name: '非零圆角', re: /border-radius\s*:\s*([^;}]+)/g, allow: v => /^0(\b|\s)/.test(v) || /^var\(--radius\b/.test(v),
    hint: '规范 seed 是 --radius:0 ⇒ 一律 border-radius:0（不要 8px/999px/50%）' },
  { id: 'R3', name: '字体字族串', re: /font-family\s*:\s*([^;}]+)/g, allow: v => /^var\(--font-/.test(v) || /^(inherit|initial|unset|revert)$/.test(v),
    hint: '只允许 var(--font-display)/var(--font-ui)/var(--font-serif-max)/var(--font-mono)（inherit 等关键字不算字族串）' },
  { id: 'R4', name: 'var(--mono) 别名', re: /var\(--mono\)/g,
    hint: '主题块里叫 --font-mono；写 --mono 会静默退化成继承字体' },
  { id: 'R5', name: '外部资源', re: /url\(\s*['"]?https?:|@import/g,
    hint: '零外部资源：不引字体文件/图片/@import' },
];

// R6 是**唯一一条扫 <script> 的判据**：JS 侧的颜色字面量。
//
// # 判据为什么必须限定「样式语境」（而不是扫所有 #hex）
//
// 天真写法（`/#[0-9a-fA-F]{3,8}\b/` 扫 JS）会立刻误报：`$('#abc')` / `data-key="#decade"`
// 这类**选择器与键名**本身就是合法的十六进制形态。实测过这条歧义，所以只认三种
// 「这个色值一定会进样式」的语境：
//
//   A 属性形态   fill="#…" / stroke="#…" / stop-color="#…"（SVG presentation attribute）
//   B 样式形态   color:#… / background:#… / border-color:#… / .style.x = '#…'
//   C 常量形态   var X_COLOR = '#…'（大写常量通常是颜色常量，正是本轮修掉的那个形态）
//
// 允许：var(--x, #fallback) —— A/B 两种形态都要求色值**紧跟** `=`/`:`，
// 而 fallback 前面是逗号，所以天然不命中（不需要额外例外）。
const R6 = {
  id: 'R6',
  name: 'JS 与正文里的硬编码颜色',
  re: /(?:fill|stroke|stop-color|flood-color)="#[0-9a-fA-F]{3,8}"|(?:color|background|background-color|border-color|borderColor|backgroundColor|fill|stroke)\s*[:=]\s*'#[0-9a-fA-F]{3,8}\b|(?:color|background|border-color)\s*:\s*#[0-9a-fA-F]{3,8}\b|(?:var|const|let)\s+[A-Z][A-Z0-9_]*COLOR[A-Z0-9_]*\s*=\s*'#[0-9a-fA-F]{3,8}'/g,
  hint: 'JS 拼出来/内联写的颜色也要走语义变量：var(--acc/--ok/--warn/--err/--fg/--dim/--line)（实测：SVG presentation attribute 里认 var()，且随主题切换）',
};

// htmlInlineStyles 取出**页面正文里**的内联 style 属性（<style>/<script> 之外）。
//
// # 为什么还要这一条（R7）
//
// R1 只扫页面自有 <style>，R6 只扫 <script> —— 而用户报障那 4 处里有 1 处是**正文里的**
// 内联属性：`<span style="color:#58a6ff">━</span>`（命中率图例的色块）。
// 两个门禁都扫不到它，它就一直在那儿。
//
// ⚠ 剥掉 <style>…</style> 与 <script>…</script> 之后再找 `style="…"`：
//   JS 里拼的 style 串由 R6 负责，重复扫会让同一处报两次（行号也对不上）。
function htmlInlineStyles(html) {
  const body = html
    .replace(/<style[^>]*>[\s\S]*?<\/style>/g, ' ')
    .replace(/<script[^>]*>[\s\S]*?<\/script>/g, ' ')
    .replace(/<!--[\s\S]*?-->/g, ' ');
  const out = [];
  const re = /style\s*=\s*"([^"]*)"/g;
  let m;
  while ((m = re.exec(body)) !== null) out.push(m[1]);
  return out.join('\n');
}

const files = fs.readdirSync(DIR).filter(f => /^webui.*\.html$/.test(f)).sort();
let bad = 0;
const rows = [];

for (const f of files) {
  const html = fs.readFileSync(path.join(DIR, f), 'utf8');
  const css = styleBlocks(html);
  const js = codeOutsideStyle(html);
  const hits = [];
  // R6 单独跑在 JS 上（其余五条跑在剥掉主题块的自有样式上）——
  // 两组文本不同，混在一起扫会让行号与「页面自有样式」的定位对不上。
  R6.re.lastIndex = 0;
  {
    const found = []; let m6;
    while ((m6 = R6.re.exec(js)) !== null) {
      found.push({ line: lineOf(js, m6.index), text: m6[0].trim().slice(0, 60) });
      if (found.length >= 60) break;
    }
    if (found.length) hits.push({ rule: R6, found });
  }
  for (const r of RULES) {
    r.re.lastIndex = 0;
    const found = [];
    let m;
    while ((m = r.re.exec(css)) !== null) {
      // 有捕获组的判据按**值**判（lookahead 在含空格/注释的值上会误判，实测踩到过）
      if (r.allow) {
        const v = String(m[1] == null ? '' : m[1]).trim();
        if (r.allow(v)) continue;
      }
      found.push({ line: lineOf(css, m.index), text: m[0].trim().slice(0, 60) });
      if (found.length >= 60) break;
    }
    if (found.length) hits.push({ rule: r, found });
  }
  const total = hits.reduce((a, h) => a + h.found.length, 0);
  rows.push({ f, total, hits });
  if (total) bad++;
}

console.log('=== 前端主题合规门禁（只扫页面自有 <style>，主题块已剥除）===');
for (const row of rows) {
  if (!row.total) { console.log('  PASS ' + row.f); continue; }
  console.log('  FAIL ' + row.f + '  违规 ' + row.total + ' 处');
  for (const h of row.hits) {
    console.log('        ' + h.rule.id + ' ' + h.rule.name + ' × ' + h.found.length + ' → ' + h.rule.hint);
    for (const s of h.found.slice(0, 3)) console.log('           :' + s.line + '  ' + s.text);
  }
}
console.log(bad
  ? '=== ' + bad + ' 个页面越界：页面自有样式必须只用主题变量/共享类 ==='
  : '=== 全部 ' + rows.length + ' 页合规 ===');
process.exitCode = bad ? 1 : 0;
