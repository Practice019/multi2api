// lib_frontend_source.js —— 前端**全部源码**的读取入口（webui.html + ui/app.js）。
//
// # 为什么需要它（与 Go 侧的 readFrontendSource 是**同一件事故**）
//
// T-409 拆包把主 script 从 `webui.html` **外置**到了 `internal/server/ui/app.js`
//（页面用 `<script src="/ui/app.js" defer>` 加载）。Go 侧的守卫当时一并改成
// `readFrontendSource()` —— 它的注释记着代价：只读 html 的守卫**一次红了 85 条**。
//
// 但 **JS 侧（本目录）的工具没跟着改**：它们仍然 `readFileSync(.../webui.html)`
// 再 grep JS 标记 ⇒ 报「找不到 X」「定义 0 次」这类**假红**。
// 一处真因、多个症状：审计/生成器/探针各红一条，看起来像一堆互不相关的坏东西。
//
// ⇒ 本文件是 JS 侧的唯一入口。判据与 Go 侧**逐字一致**：
//   守卫问的是「**前端源码**里有没有这段结构」，不是「某个文件名里有没有」。
//
// ⚠ 只想读 HTML 本体的检查（例如「页面里不该再有 <style> 块」）请直接用 `PAGE`，
//   不要用 `FRONTEND` —— 与 Go 侧同一条规矩。
//
// ⚠ 顺序：html 在前、js 在后（少数检查看首行，JS 拼在后面不影响它们），与 Go 侧一致。
const fs = require('fs');
const path = require('path');

// REPO：仓库根。本文件在 tests/frontend/，距根两级。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）—— 与本目录其它工具同一约定。
const REPO = process.env.WB2API_REPO || path.join(__dirname, '..', '..');
const PAGE = path.join(REPO, 'internal/server/webui.html');
const APP_JS = path.join(REPO, 'internal/server/ui/app.js');

const html = fs.readFileSync(PAGE, 'utf8');
const js = fs.readFileSync(APP_JS, 'utf8');
// FRONTEND = 拆包前的扫描面（JS 当初就在 html 里，位置不同而已）。
const FRONTEND = html + '\n' + js;

module.exports = { REPO, PAGE, APP_JS, html, js, FRONTEND };
