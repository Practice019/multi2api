#!/usr/bin/env node
// toast_sync.js 把 internal/server/toast.js 注入到**需要通知框**的页面。
//
// # 与 model_picker_sync.js / dash_chart_sync.js / theme_sync.js 同一套手法
//
// 源文件 + START/END 标记 + 幂等替换。改源文件 → 重跑 → 页面同步。
// ⚠ 为什么注入而不是 <script src>：前端语法门禁只扫**内联** <script>
//   （清单取自 webui.go 的 //go:embed），外链 .js 会静默逃过门禁。
//
// # 注入哪些页面（判据是「谁有通知框」，不是「谁是外壳」）
//
//   webui.html        外壳控制台（批量动作会一次发几十条结果 ⇒ 必须有堆叠）
//   webui-tokens.html 令牌页（批量轮换同理）
//
// 其余 4 页（状态/分组/渠道/登录）**不用**通知框：它们没有 #toast 元素、也没有
// 一处 toast() 调用。等哪一页真的要用，再把它加进 PAGES —— 而不是先注进去留着当死代码。

// ⚠ 2026-11-14 反例（真的发生过）：模型页本来没有通知框，我在给「重拉」做反馈时
// 自造了一套 .toasts 浮动层 —— 那是重复轮子（共享层有多行堆叠 / 连续同文案合并成
// ×N / 超上限折叠，我造的那套一条都没有，批量操作会退化成「只读得到最后一条」）。
// 正确做法就是：加 #toast 容器 + 补 （宿主契约）+ 把页面加进 PAGES + 跑本工具注入。
//   判据：internal/server/frontend_notify_layer_test.go 的
//         TestFrontendHasSingleNotificationLayer（扫全部 webui*.html）
//
// 用法：
//   node tools/toast_sync.js           # 写入
//   node tools/toast_sync.js --check    # 只检查是否同步（CI 用）

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const SRC = path.join(ROOT, 'internal', 'server', 'toast.js');
const START = '// ==== WB2API:TOAST:START（由 tools/toast_sync.js 生成，勿手改）====';
const END = '// ==== WB2API:TOAST:END ====';

const PAGES = [
  'internal/server/ui/app.js',
  'internal/server/webui-tokens.html',
  'internal/server/webui-models.html',
];

function eolOf(text) {
  return text.indexOf('\r\n') >= 0 ? '\r\n' : '\n';
}

function block() {
  const js = fs.readFileSync(SRC, 'utf8').replace(/\r\n/g, '\n').replace(/\s+$/, '');
  return START + '\n' + js + '\n' + END;
}

function apply(text) {
  const eol = eolOf(text);
  const body = block().replace(/\n/g, eol);
  const s = text.indexOf(START);
  const e = text.indexOf(END);
  if (s >= 0 && e > s) {
    return text.slice(0, s) + body + text.slice(e + END.length);
  }
  // 首次注入：替换掉页面里**已有的** toast 实现（否则会重复声明 ⇒ SyntaxError）。
  //
  // ⚠⚠ 必须用**大括号配对**取函数体，不能用 `/function toast…\{[\s\S]*?\n\}/`：
  //   非贪婪会在函数体里**第一个** `\n}` 处停下（toast 内部有 renderToast，
  //   它的闭括号正好是那个位置），于是替换只吃掉前半段、把后半段留在原地 ——
  //   更糟的是它会把**后面的函数**一起卷进来（实测：quotaScopeToast 整块被删，
  //   而内联语法门禁照样绿 —— 因为删完之后剩下的文本仍然括号平衡）。
  //   那次是被既有的 TestQuotaToastReportsSkipped 抓到的（它找不到 quotaScopeToast）。
  const at = text.indexOf('function toast(msg, bad) {');
  if (at < 0) {
    return null;
  }
  let i = text.indexOf('{', at), depth = 0, end = -1;
  for (; i < text.length; i++) {
    const c = text[i];
    if (c === '{') depth++;
    else if (c === '}') { depth--; if (depth === 0) { end = i + 1; break; } }
  }
  if (end < 0) {
    return null;
  }
  // ⚠ `body` 在上面已经按该页 EOL 转过一次了 —— 这里**不能再转**：
  //   再 replace 一次会把 \r\n 变成 \r\r\n（第一版就是这样，于是每次跑都"有变化"，
  //   同步工具看起来永远不同步，--check 在 CI 里恒红）。
  return text.slice(0, at) + body + text.slice(end);
}

const checkOnly = process.argv.includes('--check');
let changed = 0;
const stale = [];

for (const rel of PAGES) {
  const abs = path.join(ROOT, rel);
  const text = fs.readFileSync(abs, 'utf8');
  const next = apply(text);
  if (next === null) {
    console.error(rel + ': 找不到可替换的 toast 实现（首次注入需要页面里已有 function toast(msg, bad)）');
    process.exitCode = 1;
    continue;
  }
  if (next === text) {
    console.log('  ok    ' + rel);
    continue;
  }
  if (checkOnly) {
    console.log('  STALE ' + rel + '（与 toast.js 不同步，跑 node tools/toast_sync.js 修复）');
    stale.push(rel);
    continue;
  }
  fs.writeFileSync(abs, next);
  changed++;
  console.log('  写入  ' + rel);
}

if (checkOnly && stale.length) {
  console.error('发现 ' + stale.length + ' 个页面与共享层不同步');
  process.exitCode = 1;
} else if (checkOnly) {
  console.log('全部与 toast.js 同步。');
} else {
  console.log('已更新 ' + changed + ' 个页面。');
}
