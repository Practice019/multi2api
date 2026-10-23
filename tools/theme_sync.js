// theme_sync.js —— 把 theme-max.css（唯一真相）内联进三个页面的 <style> 末尾。
//
// # 为什么"内联"而不是"外链 CSS"
// 三个页面都是 //go:embed 的单文件 HTML，本仓的既有约束是
// 「单文件、零外部依赖、零构建步骤」（internal/server/webui.go:2）。外链 CSS 要加
// 路由 + 处理缓存/版本，等于为了少写 600 行 CSS 引入一个新的部署面。
//
// # 为什么还要有唯一真相
// 三个页面各有自己的变量名（控制台 --acc / 登录页 --accent / 渠道页 --acc+--bd），
// 各写一份必然漂移。所以真身只有 internal/server/theme-max.css 一份，
// 本脚本把它原样灌进每个页面的标记之间；改样式只改那一份，然后跑：
//
//   node tools/theme_sync.js            # 同步（幂等）
//   node tools/theme_sync.js --check    # 只校验：不同步就 exit 1（可进 CI）
//
// ⚠ 行尾按**文件自身**的行尾风格生成：webui.html 在工作树里是 CRLF
//   （见 .gitattributes / tests/frontend/audit_crlf_sensitivity.js），
//   混进 LF 会让那一套审计报出真问题。
'use strict';
const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const SRC = path.join(ROOT, 'internal', 'server', 'theme-max.css');
const START = '/* ==== WB2API:MAXIMALISM:START（由 tools/theme_sync.js 生成，勿手改）==== */';
const END = '/* ==== WB2API:MAXIMALISM:END ==== */';
const PAGES = [
  'internal/server/webui.html',
  'internal/server/webui-login.html',
  'internal/server/webui-channels.html',
  'internal/server/webui-groups.html',
];

function eolOf(text) {
  const crlf = (text.match(/\r\n/g) || []).length;
  const lf = (text.match(/\n/g) || []).length;
  return crlf > lf / 2 ? '\r\n' : '\n';
}

function block(eol) {
  const css = fs.readFileSync(SRC, 'utf8').replace(/\r\n/g, '\n').replace(/\s+$/, '');
  return [START, css, END].join(eol).split('\n').join(eol);
}

function apply(text, eol) {
  const blk = block(eol);
  const s = text.indexOf(START);
  if (s >= 0) {
    const e = text.indexOf(END, s);
    if (e < 0) throw new Error('找到了 START 但找不到 END —— 标记被改坏了');
    return text.slice(0, s) + blk + text.slice(e + END.length);
  }
  // 首次注入：插在**最后一个** </style> 之前（页面只有一块样式表，但别假设）
  const at = text.lastIndexOf('</style>');
  if (at < 0) throw new Error('页面里没有 </style> —— 不是预期的单文件页面？');
  return text.slice(0, at) + blk + eol + text.slice(at);
}

let stale = 0;
for (const rel of PAGES) {
  const p = path.join(ROOT, rel);
  const text = fs.readFileSync(p, 'utf8');
  const eol = eolOf(text);
  const next = apply(text, eol);
  if (next === text) {
    console.log('  ok    ' + rel + '（已是最新）');
    continue;
  }
  if (process.argv.includes('--check')) {
    console.log('  STALE ' + rel + '（theme-max.css 变了但页面没同步：跑 node tools/theme_sync.js）');
    stale++;
    continue;
  }
  fs.writeFileSync(p, next);
  console.log('  write ' + rel + '（' + (next.length - text.length >= 0 ? '+' : '') + (next.length - text.length) + ' 字节）');
}
if (stale) { console.log('\n' + stale + ' 个页面未同步'); process.exit(1); }
console.log('\n完成。');
