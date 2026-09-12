// audit_crlf_sensitivity.js —— 找出**真正有害**的 CRLF 敏感写法。
//
// # 为什么要精确到"用法"而不是"有没有出现"
//
// 本会话出现过两次真实的 CRLF 事故（`stripComments` 剥注释静默失效），
// 但如果只按"文件里是否出现 split('\n')"来扫，会报出 13 个 generator
// —— 逐处核对后**全是无害的**（要么只算行号、要么立即 trim、
// 要么用 /\r?\n/）。
//
// 一个报 13 个假阳性的检查等于没有检查：没人会去逐条看。
// 所以判据必须落到**用法**上：只有"拿行内容做字面量比较或行尾 $ 正则"
// 才是真风险。
//
// 判据（对每处逐行处理，看它后续 6 行）：
//   危险 —— `line === '字面量'` / `line.includes('x')` 用于匹配整行
//   危险 —— `/...$/` 匹配行尾（\r 挡在中间）
//   安全 —— `.trim()` 之后使用
//   安全 —— 只用于算行号（slice(0,idx).split('\n').length）
//   安全 —— 用 split(/\r?\n/)
'use strict';
const fs = require('fs');
const path = require('path');

const DIR = __dirname;
const READS_SOURCE = /readFileSync\([^)]*(webui\.html|\.go|_gen\.js)|AUDIT_WEBUI|process\.env\.WB2API_REPO/;
const NORMALIZES = /replace\(\/\\r\\n\/g\s*,\s*['"]\\n['"]\)|replace\(\/\\r\/g/;

const files = fs.readdirSync(DIR).filter(f => f.endsWith('.js') && f !== path.basename(__filename));

console.log('=== CRLF 敏感性扫描（精确到用法）===\n');

const dangerous = [];
const benign = [];

for (const f of files) {
  const src = fs.readFileSync(path.join(DIR, f), 'utf8');
  if (!READS_SOURCE.test(src)) continue;

  const normalizes = NORMALIZES.test(src);
  const lines = src.split('\n');

  lines.forEach((l, i) => {
    // 只关心"逐行处理"的写法
    const isSplit = /\.split\(['"]\\n['"]\)/.test(l) || /\.split\(\/\\r\?\\n\/\)/.test(l);
    const isLineCommentRe = /\.replace\(\/\\\/\\\/\.\*\$/.test(l);
    if (!isSplit && !isLineCommentRe) return;

    const ctx = lines.slice(Math.max(0, i - 1), i + 7).join('\n');

    // 用 split(/\r?\n/) 的天然安全
    const usesRoptN = /\.split\(\/\\r\?\\n\/\)/.test(l);
    // 只用于算行号
    const onlyCounting = /\.split\(['"]\\n['"]\)\.length/.test(l) ||
      /slice\([^)]*\)\.split\(['"]\\n['"]\)/.test(l);
    // 后续有 trim
    const hasTrim = /\.trim\(\)|trim\b/.test(ctx);
    // 危险：行尾 $ 正则 或 拿整行与字面量严格比较
    const lineLiteralEq = /(?:===?|!==?)\s*['"][^'"]*['"]/.test(ctx) && /line|L\b|raw/i.test(ctx);
    const dollarRe = /\/[^/\n]*\$\//.test(ctx) || /\.replace\(\/\\\/\\\/\.\*\$/.test(ctx);

    let verdict, why;
    if (normalizes) { verdict = 'safe'; why = '文件已归一化 CRLF'; }
    else if (usesRoptN) { verdict = 'safe'; why = '用 /\\r?\\n/ 显式处理'; }
    else if (onlyCounting) { verdict = 'safe'; why = '仅用于算行号'; }
    else if (hasTrim) { verdict = 'safe'; why = '结果经过 trim'; }
    else if (lineLiteralEq || dollarRe) { verdict = 'DANGER'; why = '拿行内容做字面量/行尾匹配'; }
    else { verdict = 'safe'; why = '未发现危险用法'; }

    const entry = { f, line: i + 1, snippet: l.trim().slice(0, 78), why, verdict };
    (verdict === 'DANGER' ? dangerous : benign).push(entry);
  });
}

console.log('=== ⚠ 真正危险 ===');
if (!dangerous.length) console.log('  （无）');
dangerous.forEach(d => {
  console.log('  ' + d.f + ':' + d.line + '  ' + d.why);
  console.log('      ' + d.snippet);
});
console.log('');

console.log('=== 安全（附理由）===');
const byFile = {};
benign.forEach(b => { (byFile[b.f] = byFile[b.f] || []).push(b); });
for (const f of Object.keys(byFile).sort()) {
  const items = byFile[f];
  const whys = [...new Set(items.map(x => x.why))].join('；');
  console.log('  ' + f.padEnd(30) + items.length + ' 处  —— ' + whys);
}
console.log('');
console.log('合计: ' + dangerous.length + ' 处危险, ' + benign.length + ' 处安全');
process.exit(dangerous.length ? 1 : 0);
