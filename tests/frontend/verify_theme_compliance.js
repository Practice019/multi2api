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

const files = fs.readdirSync(DIR).filter(f => /^webui.*\.html$/.test(f)).sort();
let bad = 0;
const rows = [];

for (const f of files) {
  const html = fs.readFileSync(path.join(DIR, f), 'utf8');
  const css = styleBlocks(html);
  const hits = [];
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
