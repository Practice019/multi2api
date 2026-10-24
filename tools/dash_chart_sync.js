#!/usr/bin/env node
// dash_chart_sync.js 把 internal/server/dash-chart.js 注入到 webui.html。
//
// # 与 model_picker_sync.js / theme_sync.js 同一套手法
//
// 源文件 + START/END 标记 + 幂等替换。改源文件 → 重跑 → 页面同步。
// ⚠ 为什么注入而不是 <script src>：前端语法门禁只扫**内联** <script>
//   （扫描清单取自 webui.go 的 //go:embed），外链 .js 会静默逃过门禁。
//
// 用法：
//   node tools/dash_chart_sync.js           # 写入
//   node tools/dash_chart_sync.js --check    # 只检查是否同步（CI 用）

const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..');
const SRC = path.join(ROOT, 'internal', 'server', 'dash-chart.js');
const START = '// ==== WB2API:DASH-CHART:START（由 tools/dash_chart_sync.js 生成，勿手改）====';
const END = '// ==== WB2API:DASH-CHART:END ====';

// 只有控制台外壳要画图（登录页 / 渠道页 / 分组页都没有仪表盘图表）。
const PAGES = ['internal/server/webui.html'];

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
  // 首次注入：紧跟在 <body> 之后的一段独立 <script>（与 model-picker 同一位置约定）。
  const m = text.match(/<body[^>]*>\r?\n/);
  if (!m) { return null; }
  const at = m.index + m[0].length;
  return text.slice(0, at) + '<script>' + eol + body + eol + '</script>' + eol + text.slice(at);
}

const checkOnly = process.argv.includes('--check');
let changed = 0;
let stale = [];

for (const rel of PAGES) {
  const abs = path.join(ROOT, rel);
  const text = fs.readFileSync(abs, 'utf8');
  const next = apply(text);
  if (next === null) {
    console.error(rel + ': 找不到 <body> 锚点，无法注入');
    process.exitCode = 1;
    continue;
  }
  if (next === text) { console.log('  ok    ' + rel); continue; }
  if (checkOnly) { console.log('  STALE ' + rel); stale.push(rel); continue; }
  fs.writeFileSync(abs, next, 'utf8');
  console.log('  写入  ' + rel + '（' + (next.length - text.length >= 0 ? '+' : '') + (next.length - text.length) + ' 字节）');
  changed++;
}

if (checkOnly && stale.length) {
  console.error('\n有 ' + stale.length + ' 个页面未同步，跑 node tools/dash_chart_sync.js 修复。');
  process.exit(1);
}
if (!checkOnly) {
  console.log(changed ? '\n已更新 ' + changed + ' 个页面。' : '\n全部已是最新。');
}
