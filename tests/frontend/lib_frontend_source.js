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
// ⚠ CSS 也要算进来：有些判据找的是**样式规则**（例如「折叠是否真的 display:none」），
//   而那些规则住在 app.css 里（T-409 拆包时与 JS 一起外置的）。
const APP_CSS = path.join(REPO, 'internal/server/ui/app.css');

// readOr：读文件；**只在 WB2API_REPO 被显式设置时**容忍缺文件。
//
// # 为什么要容忍（这不是「掩盖事故」）
//
// 变异自检的工作方式是：把**要改的那一个文件**写进一次性副本（例如只写
// `internal/server/webui.html`），再 `WB2API_REPO=副本` 跑同一个工具，
// 断言「副本上必须红」。副本里没有 ui/app.js 是**正常的**（不是事故）。
//
// ⚠ 但真实仓库里缺文件**必须继续炸**：否则 app.js 被改名/搬走这类事故
//   会被静默吞成空串，而所有守卫会一起「变绿」。所以这里按 WB2API_REPO 分档 ——
//   与本目录其它工具「用 WB2API_REPO 指向副本做变异」的约定同源。
const readOr = (p) => {
  try {
    return fs.readFileSync(p, 'utf8');
  } catch (e) {
    if (process.env.WB2API_REPO) return '';
    throw e;
  }
};

const html = readOr(PAGE);
const js = readOr(APP_JS);
const css = readOr(APP_CSS);
// FRONTEND = 拆包前的扫描面（JS/CSS 当初就在 html 里，位置不同而已）。
const FRONTEND = html + '\n' + js + '\n' + css;

module.exports = { REPO, PAGE, APP_JS, APP_CSS, html, js, css, FRONTEND };
