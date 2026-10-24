// dash-chart.js —— 仪表盘图表的**共享绘制层**（内联 SVG、零依赖）。
//
// # 为什么有这个文件（用户原话）
//
//   「上面的表格看起来很简单」
//   「下面的实现可以抽共享一起用」
//   「我喜欢下面的表格柱状图 折线图还有等分线等等」
//
// ⇒ 顶部「性能健康」那排 chip 要**照下面抄**：同一套等分网格线、同一套柱子、
//   同一套折线、同一套配色。而下面那几张图此前是**各自内联画**的：
//   renderCacheTrendSVG 抄了一遍网格线 + 折线 + 悬浮参考线，
//   renderDailyTrendSVG 又抄了一遍网格线 + 柱子 + 折线 + 悬浮参考线。
//   健康条再抄一遍就是**第三份副本** —— 于是抽到这里，几处跑同一份实现。
//
// # 载体（照本项目既有做法）
//
// 唯一真相是本文件；由 `tools/dash_chart_sync.js` 注入 `webui.html` 顶部那段
// <script>（与 `model-picker.js` 完全同一套手法）。
// ⚠ 为什么是注入而不是 <script src>：前端语法门禁只扫**内联** <script>
//   （扫描清单取自 webui.go 的 //go:embed），外链 .js 会静默逃过门禁 ——
//   门禁全绿却根本没扫它。
//
// # 契约
//
//   dcGrid(svg, o)       横等分网格线 + 左（可右）刻度   ← cacheTrend / dailyTrend / 成功率条
//   dcGridCols(svg, o)   竖等分网格线 + 底部刻度         ← 横向条形（成功率 / 分项成功率）
//   dcRects(svg, rs, o)  柱子（竖柱与横条共用）
//   dcLine(svg, pts, o)  折线（可带数据点圆）
//   dcCursor(svg, id, o) 悬浮参考垂线 + 跟随读数（预置 display:none）
//   dcSvgTag(W, H, parts) / dcSvg(host, W, H, parts)   统一包 <svg viewBox>
//
// ⚠ 本文件**不依赖宿主的任何函数**（自带 dcEsc）：否则"抽出来"就成了假共享
//   —— `model-picker.js` 里记着同一条理由。

// DC_SEGS 网格等分段数：4 段 = 5 条线 ⇒ 0 / 25 / 50 / 75 / 100%。
// ⚠ 这个数字是**视觉语言的一部分**（用户点名要"等分线"），改它等于改全部图。
var DC_SEGS = 4;

// 配色**只有这一处**：与 cacheTrend 的命中率线同色（#58a6ff），柱子/折线走主题变量
// （写死色值会在浅色主题下糊进背景 —— 见 webui.html 顶部那段注释）。
var DC_RATE_COLOR = '#58a6ff';
var DC_BAR_COLOR = 'var(--acc)';

// dcEsc 最小 HTML 转义（自带一份，不依赖宿主）。
function dcEsc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// dcNum 把任意输入收敛成有限数（NaN / undefined / 字符串数字都兜住）。
//
// ⚠ 这不是洁癖：后端字段缺失时 undefined * 100 会得到 NaN，而 NaN 进了
//   SVG 属性就变成 x="NaN" —— 浏览器**静默丢弃**那个元素，图看起来"少画了一块"，
//   控制台一个字都不报。所有几何计算一律先过这里。
function dcNum(v) {
  var n = Number(v);
  return isFinite(n) ? n : 0;
}

// dcGrid 横等分网格线 + 刻度（**下方两张趋势图的网格线唯一出处**）。
//
// o = {
//   W, PADL, PADR, PADT, plotH,   // 版面
//   max,                          // 左轴上限（顶部那条线 = max，底部 = 0）
//   fmt(v) -> string,             // 左刻度文本（不给就只画线、不标数）
//   fill,                         // 左刻度颜色（双轴时必须与所属线同色）
//   rightMax, rightFmt(v),        // 右轴（可选；不给就不画右刻度）
//   segs,                         // 段数（默认 DC_SEGS = 4）
//   labelEvery                    // 每几段标一个数（默认 1；矮图用 2 防糊）
// }
function dcGrid(svg, o) {
  var segs = o.segs || DC_SEGS;
  var padr = o.PADR == null ? 0 : o.PADR;
  var max = dcNum(o.max);
  var every = o.labelEvery || 1;
  for (var i = 0; i <= segs; i++) {
    var y = o.PADT + (o.plotH * i) / segs;
    svg.push('<line x1="' + o.PADL + '" y1="' + y.toFixed(1) + '" x2="' + (o.W - padr) +
      '" y2="' + y.toFixed(1) + '" stroke="var(--line)" stroke-width="1"/>');
    if (o.fmt && i % every === 0) {
      svg.push('<text class="ax" x="' + (o.PADL - 6) + '" y="' + (y + 3).toFixed(1) +
        '" text-anchor="end"' + (o.fill ? ' fill="' + o.fill + '"' : '') + '>' +
        dcEsc(o.fmt(max * (1 - i / segs))) + '</text>');
    }
    if (o.rightFmt && i % every === 0) {
      svg.push('<text class="ax" x="' + (o.W - padr + 6) + '" y="' + (y + 3).toFixed(1) +
        '" fill="var(--warn)">' + dcEsc(o.rightFmt(dcNum(o.rightMax) * (1 - i / segs))) + '</text>');
    }
  }
}

// dcGridCols 竖等分网格线 + 底部百分比刻度（**横向条形图的网格线唯一出处**）。
//
// 为什么横向条要用**竖**网格线：条越长 = 百分比越大，等分线只能垂直于条。
// 段数与颜色沿用 dcGrid（同一套视觉语言），刻度固定按 0/25/50/75/100% 标。
//
// o = { W, PADL, PADR, PADT, plotH, fmt, fill, segs, labelY }
function dcGridCols(svg, o) {
  var segs = o.segs || DC_SEGS;
  var padr = o.PADR == null ? 0 : o.PADR;
  var plotW = o.W - o.PADL - padr;
  for (var i = 0; i <= segs; i++) {
    var x = o.PADL + (plotW * i) / segs;
    svg.push('<line x1="' + x.toFixed(1) + '" y1="' + o.PADT + '" x2="' + x.toFixed(1) +
      '" y2="' + (o.PADT + o.plotH) + '" stroke="var(--line)" stroke-width="1"/>');
    if (o.fmt) {
      svg.push('<text class="ax" x="' + x.toFixed(1) + '" y="' + (o.labelY || (o.PADT + o.plotH + 9)) +
        '" text-anchor="' + (i === 0 ? 'start' : i === segs ? 'end' : 'middle') + '"' +
        (o.fill ? ' fill="' + o.fill + '"' : '') + '>' + dcEsc(o.fmt((100 * i) / segs)) + '</text>');
    }
  }
}

// dcRects 画柱子（竖柱 / 横条共用）。
//
// rs = [{x, y, w, h}, …]；o = {fill, rx, opacity}
// ⚠ 宽或高 <= 0 的**跳过**：0% 的条不该留下一条 1px 的"有值"痕迹。
function dcRects(svg, rs, o) {
  var opt = o || {};
  for (var i = 0; i < (rs || []).length; i++) {
    var r = rs[i];
    var w = dcNum(r.w), h = dcNum(r.h);
    if (w <= 0 || h <= 0) continue;
    svg.push('<rect x="' + dcNum(r.x).toFixed(1) + '" y="' + dcNum(r.y).toFixed(1) +
      '" width="' + w.toFixed(1) + '" height="' + h.toFixed(1) +
      '" fill="' + (r.fill || opt.fill || DC_BAR_COLOR) + '"' +
      (opt.rx ? ' rx="' + dcNum(opt.rx).toFixed(1) + '"' : '') +
      (opt.opacity == null ? '' : ' opacity="' + opt.opacity + '"') + '/>');
  }
}

// dcLine 画折线（可带数据点圆）。
//
// pts = [[x, y], …]；o = {stroke, width, opacity, dots, dotR, dotFill}
// ⚠ 少于 2 点**不画线**（一个点连不成线），但仍可画圆点 —— 所以 dots 单独判。
function dcLine(svg, pts, o) {
  var opt = o || {};
  var list = pts || [];
  var stroke = opt.stroke || DC_BAR_COLOR;
  if (list.length >= 2) {
    var d = [];
    for (var i = 0; i < list.length; i++) {
      d.push((i ? 'L' : 'M') + dcNum(list[i][0]).toFixed(1) + ',' + dcNum(list[i][1]).toFixed(1));
    }
    svg.push('<path d="' + d.join(' ') + '" fill="none" stroke="' + stroke +
      '" stroke-width="' + (opt.width || 1.6) + '"' +
      (opt.opacity == null ? '' : ' opacity="' + opt.opacity + '"') +
      ' stroke-linejoin="round" stroke-linecap="round"/>');
  }
  if (opt.dots) {
    for (var k = 0; k < list.length; k++) {
      svg.push('<circle cx="' + dcNum(list[k][0]).toFixed(1) + '" cy="' + dcNum(list[k][1]).toFixed(1) +
        '" r="' + (opt.dotR || 2.2) + '" fill="' + (opt.dotFill || stroke) + '" opacity="0.9"/>');
    }
  }
}

// dcCursor 预置"悬浮参考垂线 + 跟随读数"（cacheTrend / dailyTrend 同一套）。
//
// ⚠ 必须**预置**在 SVG 里（默认 display:none）：渲染函数每次重写 innerHTML，
//   在事件里现场 append 的元素重绘一次就没了。
// id 传前缀：'ct' → #ctCursor / #ctRead，'dt' → #dtCursor / #dtRead。
function dcCursor(svg, id, o) {
  var y1 = dcNum(o.PADT), y2 = dcNum(o.PADT) + dcNum(o.plotH);
  svg.push('<line id="' + id + 'Cursor" x1="0" y1="' + y1 + '" x2="0" y2="' + y2 +
    '" stroke="var(--fg)" stroke-width="1" stroke-dasharray="3 3" opacity="0.5" style="display:none"/>');
  svg.push('<text id="' + id + 'Read" class="ax" x="0" y="' + (o.readY || 12) +
    '" text-anchor="middle" fill="var(--fg)" style="display:none"></text>');
}

// dcSvgTag 拼出 <svg> 外壳（返回字符串，供 innerHTML 里内联嵌图表用）。
//
// o 可选：{style, label} —— 顶部健康条要在**同一段字符串**里同时给出"一行字 +
// 一张迷你图"，所以允许覆盖 style（默认 100% 宽高 = 填满下方趋势图的容器）
// 并允许带一段 label 文本（SVG 里的 <text> 也能显示，但字号/换行不如 HTML 可控）。
function dcSvgTag(W, H, parts, o) {
  var opt = o || {};
  var style = opt.style || 'width:100%;height:100%';
  return '<svg viewBox="0 0 ' + dcNum(W) + ' ' + dcNum(H) +
    '" preserveAspectRatio="none" style="' + style + '">' + (parts || []).join('') + '</svg>';
}

// dcSvg 把拼好的元素装进 host，并返回同一段 HTML。
//
// ⚠ host 为假值时不写 DOM、只返回字符串（顶部健康条要的是字符串，
//   下方趋势图要的是写 DOM —— 两种用法共用这一个外壳）。
function dcSvg(host, W, H, parts, o) {
  var html = dcSvgTag(W, H, parts, o);
  if (host) host.innerHTML = html;
  return html;
}

// dcPct 百分比条的**唯一换算入口**：value/den → 0..100 的百分比。
//
// ⚠ 三处必须一致（顶部成功率条 / 分项成功率条组 / 下方命中率趋势）：
//   · 分母 <= 0 ⇒ 返回 null（**不是 0**）—— "没有请求"与"成功率 0%"是两件事，
//     渲染成 0% 会让人以为网关全挂了；
//   · 不夹到 100（上游分开上报时命中率合法可超 100%，见 cacheTrend 的注释），
//     夹取值等于谎报数据；
//   · 负数（后端异常）按 0 处理，避免条子反向画。
function dcPct(v, den) {
  var d = dcNum(den);
  if (d <= 0) return null;
  var r = (dcNum(v) / d) * 100;
  return r < 0 ? 0 : r;
}

// dcTier 按百分比分档取色（**与下方图表同一套语义色**）。
//
// 阈值沿用页面既有口径（renderHealthBar / renderModelRank 里那两处同值）：
//   >= 95 绿（--ok） · >= 80 琥珀（--warn） · 其余 红（--err） · 无数 灰（--dim）
function dcTier(pct) {
  if (pct == null) return 'var(--dim)';
  return pct >= 95 ? 'var(--ok)' : pct >= 80 ? 'var(--warn)' : 'var(--err)';
}
