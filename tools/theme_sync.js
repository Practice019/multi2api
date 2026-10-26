// theme_sync.js —— 把 theme-max.css（唯一真相）内联进六个页面的 <style> 末尾。
//
// # 为什么"内联"而不是"外链 CSS"
// 六个页面都是 //go:embed 的单文件 HTML，本仓的既有约束是
// 「单文件、零外部依赖、零构建步骤」（internal/server/webui.go:2）。外链 CSS 要加
// 路由 + 处理缓存/版本，等于为了少写 600 行 CSS 引入一个新的部署面。
//
// # 为什么还要有唯一真相
// 六个页面各有自己的变量名（控制台 --acc / 登录页 --accent / 渠道页 --acc+--bd / 状态页与模型页各有自己的 --mono 一族），
// 各写一份必然漂移。所以真身只有 internal/server/theme-max.css 一份，
// 本脚本把它原样灌进每个页面的标记之间；改样式只改那一份，然后跑：
//
//   node tools/theme_sync.js            # 同步（幂等）
//   node tools/theme_sync.js --check    # 只校验：不同步就 exit 1（可进 CI）
//
// ⚠⚠ 顺序敏感（2026-10-26 实测）：**任何页面的整页重写之后，必须再跑一次本脚本**。
//   块是**生成物**：重写页面（改版、换外壳、从别处拷一份）很容易把 START..END 整段丢掉，
//   而丢掉是**静默的** —— 页面照样渲染，只是退回了 v2 配色，没有任何报错。
//   实测：C2 重写 webui-channels.html 后块为空，我这次跑 sync 才补回 **+35818 字节**。
//   ⇒ 改完页面的验收清单里请固定包含：node tools/theme_sync.js && node tools/theme_sync.js --check。
//   （--check 会以 STALE 点名缺块的页面，所以只要它进了验收就不会漏。）
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
  // v3.1（2026-10-26）：状态页与模型页此前**不在名单里** —— 它们是独立 document
  // （iframe 内嵌与直开都算），于是长期停在 v2 GitHub 蓝，与其余四页视觉分叉；
  // 更糟的是它们**同时逃过了两份守卫**：主题一致性（本脚本）与对比度巡检
  // （tests/frontend/audit_contrast_pages.js 原先只扫控制台外壳）都不看它们。
  // 首次注入由本脚本自己完成：没有标记时插在**最后一个 </style> 之前**（见 apply）。
  'internal/server/webui-status.html',
  'internal/server/webui-models.html',
  // T68 / Wave C3：令牌（API Key）管理页。与状态页/模型页同一条理由 ——
  // 独立 document 若不进名单，会同时逃过主题一致性与对比度两份守卫。
  'internal/server/webui-tokens.html',
];

function eolOf(text) {
  const crlf = (text.match(/\r\n/g) || []).length;
  const lf = (text.match(/\n/g) || []).length;
  return crlf > lf / 2 ? '\r\n' : '\n';
}

function block(eol) {
  // ⚠⚠ 这里必须**只用 \n 组装、最后一次性换 eol**（2026-10-26 修的真缺陷）。
  //
  // # 原来错在哪
  // 旧写法是 [START, css, END].join(eol).split('\n').join(eol)：
  //   join(eol) 先插入了 \r\n，紧接的 split('\n') 又把每个 \r\n 从中间切开
  //   （元素尾巴留下一个 \r），join(eol) 再补一个 \r\n ⇒ **\r\r\n（双 CR）**。
  // 于是：脚本自己刚写完的页面 --check 是 ok 的（写完即自洽），
  // 但**任何人手同步过、或由别的路径写过一次的 CRLF 页，永远过不了 --check** ——
  // 哪怕视觉完全一致、只差那两个多出来的 \r。这是最坏的一类缺陷：
  // 门禁看着是绿的，报的却是「你落后了」，把人往错误方向支。
  //
  // # 现在的写法（两步，各自单一职责）
  //   ① 把 CSS 正文规范化成 \n（\r\n 与**裸 \r** 都归一 —— 裸 CR 是旧 Mac 行尾，
  //      留着会让下面的 split 判断失准）；
  //   ② 用 \n 组装整块，最后 split('\n').join(eol) **只做一次** eol 转换。
  // ⇒ 块内不可能出现 \r\r，也不可能出现裸 \r；对 CRLF / LF 页面都幂等。
  //
  // # 为什么不干脆把 CRLF 页面整体转成 LF
  // 本仓有 .gitattributes 与 tests/frontend/audit_crlf_sensitivity.js 的纪律：
  // 整文件换行改写会在 git 里制造「全文件都变了」的巨大假 diff，把真改动埋掉。
  // 所以这里**只规范块内的分隔符**，页面其余部分的既有行尾原样保留。
  const css = fs.readFileSync(SRC, 'utf8').replace(/\r\n?/g, '\n').replace(/\s+$/, '');
  return [START, css, END].join('\n').split('\n').join(eol);
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
