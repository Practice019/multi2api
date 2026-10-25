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
//   dcBand(svg, id, o)   悬停高亮底带（预置 display:none，与 dcCursor 配套）
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

// dcCursor 预置**悬浮参考垂线**（各趋势图同一套）。
//
// ⚠ 必须**预置**在 SVG 里（默认 display:none）：渲染函数每次重写 innerHTML，
//   在事件里现场 append 的元素重绘一次就没了。
// id 传前缀：'ct' → #ctCursor，'trend' → #trendCursor。
//
// ⚠⚠ 2026-10-26：**读数文本（#..Read）已删除**，因为已经没有调用方要它了 ——
//   原先它给的是"一行监控台风格"的读数（时间 · 累计 x% · 请求 n · Token m），
//   而两张图现在都改用共享的**悬浮卡片**（dcTipTag + dcTipHead + dcTipRows）。
//
// # 为什么不留着那个参数
//
//   留着就等于给下一张图留了一份"旧样式"的样本 —— 抄它的人会做出一张
//   与其它图**格式不同**的悬浮提示，而那正是用户这次点名的那个问题
//   （「这个鼠标悬浮 能不能复用这个的鼠标悬浮卡片」）。
//   少一个可选项，就少一次漂移的机会。
function dcCursor(svg, id, o) {
  var y1 = dcNum(o.PADT), y2 = dcNum(o.PADT) + dcNum(o.plotH);
  svg.push('<line id="' + id + 'Cursor" x1="0" y1="' + y1 + '" x2="0" y2="' + y2 +
    '" stroke="var(--fg)" stroke-width="1" stroke-dasharray="3 3" opacity="0.5" style="display:none"/>');
}

// dcBand 预置"悬停高亮底带"（默认 display:none），与 dcCursor 配套。
//
// # 为什么也放进共享层
//   加上它之后，**仪表盘里所有 SVG 元素的构造都在本文件里**：
//   网格线 / 刻度 / 柱子 / 折线 / 参考垂线 / 高亮底带 / 外壳。
//   之前 #statsTrend 那张图自己 push 了一段 <rect>，于是"拼 SVG 元素"
//   这件事在共享层之外还剩一份 —— 用户的要求是"不要不同的地方不同的实现"，
//   留一处就等于下次还有人照抄它。
//
// id 传前缀：'trend' → #trendHoverBand（沿用 webui.html 里既有的 id）。
// ⚠ 底带必须**最先 push**（= 最底层），否则会盖在色块上把颜色洗淡 ——
//   调用方负责顺序，本函数只负责"长什么样"。
function dcBand(svg, id, o) {
  svg.push('<rect id="' + id + 'HoverBand" x="0" y="' + dcNum(o.PADT) + '" width="0" height="' +
    dcNum(o.plotH) + '" fill="var(--fg)" opacity="0.07" style="display:none"/>');
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

// ── 悬浮卡片（三块：外壳 / 明细行 / 定位）────────────────────────────────
//
// # 为什么要抽（用户原话）
//
//   「这个鼠标悬浮 能不能复用这个的鼠标悬浮卡片」
//
//   当时 #statsTrend 有一张**富卡片**（时间头 + 合计 + 分模型明细 + 色点），
//   而 #cacheTrend 只有 dcCursor 生成的**单行 text 读数**。两张图并排看，
//   一个是卡片、一个是行字 —— 又是「两处慢慢漂移」的同一形态。
//
// # 抽的是哪一层
//
//   抽**外形**（外壳样式、行的排法、定位算法），**不抽内容**：
//   各图要显示的字段本来就不同（趋势图是分模型、缓存图是双轴口径），
//   强行统一字段只会逼出一堆 if。调用方自己拼 innerHTML，用下面三块拼。
//
// ⚠ 三块必须**一起**用，否则外形会重新漂移：
//   只用 dcTipTag 不用 dcTipPlace ⇒ 定位算法又是各写一套。

// dcTipTag 悬浮卡片的**外壳**（一个默认隐藏的绝对定位 div）。
//
// ⚠ 必须是**预置在 HTML 里**的（不是事件里现场 append）：渲染函数每次重写
//   innerHTML，现场 append 的元素重绘一次就没了 —— 与 dcCursor 同一理由。
function dcTipTag(id) {
  return '<div id="' + id + '" style="display:none;position:absolute;pointer-events:none;' +
    'background:var(--panel);border:1px solid var(--line);border-radius:6px;padding:4px 8px;' +
    'font-size:11px;white-space:nowrap;z-index:5;box-shadow:0 4px 14px var(--shadow);' +
    'min-width:130px"></div>';
}

// dcTipHead 卡片的**首行**（时间 + 可选副标题）。
//
// ⚠ 抽它的理由：两张图的首行都是「粗体时间」，字号/间距必须一致 ——
//   那正是「看起来像两个组件」最容易发生的地方。
function dcTipHead(title, sub) {
  return '<div style="font-weight:600;margin-bottom:2px">' + dcEsc(title) + '</div>' +
    (sub ? '<div style="color:var(--dim);margin-bottom:2px">' + sub + '</div>' : '');
}

// dcTipRows 卡片的**明细行**：色点 + 名字 + 右对齐数值。
//
// items = [{color, name, value, dim}]
//   · color 传 CSS 色值 —— 惯例是**与该行对应的那条线同色**，
//     这样「卡片里的这一行」与「图上那条线」一眼对得上。
//   · value 已经是**格式化好的字符串**（各图量纲不同，本函数不管格式）。
//   · dim=true ⇒ 整行压暗，用于次要口径（如「本小时」对「累计」）。
//
// ⚠ 名字要 dcEsc（模型名来自上游，可能带尖括号）。value 由调用方保证安全。
function dcTipRows(items) {
  return (items || []).map(function (o) {
    var muted = o.dim ? 'opacity:.7' : '';
    var vcolor = o.dim ? 'var(--dim)' : 'var(--fg)';
    return '<div style="display:flex;align-items:center;gap:5px;' + muted + '">' +
      '<i style="width:7px;height:7px;border-radius:2px;background:' + (o.color || 'var(--dim)') + ';flex:none"></i>' +
      '<span style="overflow:hidden;text-overflow:ellipsis;max-width:160px;color:var(--dim)">' + dcEsc(o.name) + '</span>' +
      '<b style="margin-left:auto;color:' + vcolor + ';padding-left:8px">' + o.value + '</b>' +
      '</div>';
  }).join('');
}

// dcTipPlace 把卡片摆到光标附近（横向跟手 + 纵向优先上方）。
//
// 三条约束（都是实拍调出来的，别随手改）：
//   · 横向 mx+12 跟手，但按**实测** offsetWidth clamp ⇒ 贴右边缘时不翻到屏幕外；
//   · 纵向优先放光标**上方**（不挡正在看的数据），贴顶时落到下方；
//   · 最后再 clamp 一次到 rect 内 —— 小图里「上方也放不下、下方也放不下」时
//     不至于把卡片丢到面板外面。
//
// ⚠ offsetWidth/Height 在 display:none 时是 0 ⇒ **先 display:block 再量**，
//   量不到（首帧）才退回 fallback。顺序反了的话卡片每次按 fallback 定位、会跳一下。
function dcTipPlace(tip, ev, rect, o) {
  var opt = o || {};
  tip.style.display = 'block';
  var mx = ev.clientX - rect.left, my = ev.clientY - rect.top;
  var tw = tip.offsetWidth || opt.fallbackW || 150, th = tip.offsetHeight || opt.fallbackH || 90;
  tip.style.left = Math.min(Math.max(mx + 12, 2), Math.max(2, rect.width - tw - 2)) + 'px';
  var top = my - th - 10, maxTop = Math.max(2, rect.height - th - 2);
  tip.style.top = (top >= 2 ? Math.min(top, maxTop) : Math.min(my + 14, maxTop)) + 'px';
}

// dcTipHide 收起若干卡片/游标（鼠标离开时调）。ids 传 id 或 id 数组。
function dcTipHide(host, ids) {
  var list = typeof ids === 'string' ? [ids] : (ids || []);
  for (var i = 0; i < list.length; i++) {
    var e = host.querySelector('#' + list[i]);
    if (e) e.style.display = 'none';
  }
}

// dcTier 按百分比分档取色（**与下方图表同一套语义色**）。
//
// 阈值沿用页面既有口径（renderHealthBar / renderModelRank 里那两处同值）：
//   >= 95 绿（--ok） · >= 80 琥珀（--warn） · 其余 红（--err） · 无数 灰（--dim）
function dcTier(pct) {
  if (pct == null) return 'var(--dim)';
  return pct >= 95 ? 'var(--ok)' : pct >= 80 ? 'var(--warn)' : 'var(--err)';
}
