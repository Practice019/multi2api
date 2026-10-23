#!/usr/bin/env node
// model_picker_sync.js 把 internal/server/model-picker.js 注入到各页面。
//
// # 为什么要注入而不是 <script src>
//
// 前端语法门禁只扫**内联** <script>，扫描清单取自 webui.go 的 //go:embed。
// 做成外部 .js 会让这段代码静默逃过门禁（门禁全绿但没扫它）。
// 内联之后它就是普通的内联脚本，跟着既有 7 段一起被扫。
//
// # 与 theme_sync.js 同一套手法
//
// 源文件 + START/END 标记 + 幂等替换。改源文件 → 重跑 → 各页面同步。
//
// 用法：
//   node tools/model_picker_sync.js           # 写入
//   node tools/model_picker_sync.js --check    # 只检查是否同步（CI 用）

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const SRC = path.join(ROOT, 'internal', 'server', 'model-picker.js');
const START = '// ==== WB2API:MODEL-PICKER:START（由 tools/model_picker_sync.js 生成，勿手改）====';
const END = '// ==== WB2API:MODEL-PICKER:END ====';

// 只有**需要跨上游筛选**的页面。
//
// ⚠ 渠道页刻意不在列（实测后撤掉的）：
//
// 渠道页的候选来自 /admin/channels/discover，是**单条渠道**的上游模型，
// 天然同源（实测 csg 返回 22 个）。而且其中 6 个名字里含 "/" ——
// 那是模型自己的组织名（Qwen/...、OpenCSG/...），不是我们的上游。
// 套 mpProviderOf 会把它们切成 Qwen / OpenCSG 这种**假上游**，
// 筛出来对不上任何渠道配置，比不筛更乱。
//
// 分组页不同：它的候选是**全站目录**（699 个、15 个真上游），
// 按 "/" 切第一个正好得到上游 id（与 channels.json 的 name 对齐）。
const PAGES = [
  'internal/server/webui-groups.html',
];

function eolOf(text) {
  return text.indexOf('\r\n') >= 0 ? '\r\n' : '\n';
}

function block(eol) {
  const js = fs.readFileSync(SRC, 'utf8').replace(/\r\n/g, '\n').replace(/\s+$/, '');
  // 包进一对标记里，便于下次定位替换。
  return START + '\n' + js + '\n' + END;
}

function apply(text) {
  const eol = eolOf(text);
  const body = block(eol).replace(/\n/g, eol);
  const s = text.indexOf(START);
  const e = text.indexOf(END);
  if (s >= 0 && e > s) {
    return text.slice(0, s) + body + text.slice(e + END.length);
  }
  // 首次注入：放在最后一个 </script> 之前？不行 —— 那可能在别人的函数里。
  // 约定：在 <body> 之后紧跟一段独立 <script>。各页面都在 body 开头有元素，
  // 所以插在 '<body' 标签所在行的下一行是安全的。
  const m = text.match(/<body[^>]*>\r?\n/);
  if (!m) { return null; }
  const at = m.index + m[0].length;
  const tagOpen = '<script>' + eol;
  const tagClose = eol + '</script>' + eol;
  return text.slice(0, at) + tagOpen + body + tagClose + text.slice(at);
}

const checkOnly = process.argv.includes('--check');
let changed = 0;
let stale = [];

for (const rel of PAGES) {
  const abs = path.join(ROOT, rel);
  let text;
  try { text = fs.readFileSync(abs, 'utf8'); } catch (e) {
    console.error('读不到 ' + rel + ': ' + e.message);
    process.exitCode = 1;
    continue;
  }
  const next = apply(text);
  if (next === null) {
    console.error(rel + ': 找不到 <body> 锚点，无法注入');
    process.exitCode = 1;
    continue;
  }
  if (next === text) {
    console.log('  ok    ' + rel);
    continue;
  }
  if (checkOnly) {
    console.log('  STALE ' + rel);
    stale.push(rel);
    continue;
  }
  fs.writeFileSync(abs, next, 'utf8');
  console.log('  写入  ' + rel);
  changed++;
}

if (checkOnly && stale.length) {
  console.error('\n有 ' + stale.length + ' 个页面未同步，跑 node tools/model_picker_sync.js 修复。');
  process.exit(1);
}
if (!checkOnly) {
  console.log(changed ? '\n已更新 ' + changed + ' 个页面。' : '\n全部已是最新。');
}
