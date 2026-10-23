// model-picker.js —— 模型候选选择器（分组页与渠道页共用）。
//
// # 为什么要有这个文件（抽共享层）
//
// 两个页面都要「从几百个模型里挑一部分」：
//   · 分组页：挑出这个分组包含哪些模型
//   · 渠道页：挑出这条渠道声明能服务哪些模型
// 而候选项有 699 个、分属 15 个上游（实测），**不按上游筛就是没法用** ——
// cline 一家就占 497 个，其余全被压在滚动区深处。
//
// 这段逻辑（切上游前缀 / 按上游过滤 / 面板渲染 / 勾选状态）两边各写一遍必然漂移：
// 会长成「分组页能按上游筛、渠道页不能」这种无从解释的差异。所以抽到这里。
//
// # 为什么不做成 <script src> 外部文件
//
// 前端语法门禁（tests/frontend/verify_inline_script_syntax.js）只扫**内联** <script>，
// 且扫描清单取自 webui.go 的 //go:embed 行。做成外部 .js 会让这段代码
// **静默逃过门禁** —— 门禁全绿，但根本没扫它。
//
// 所以本文件是**源文件**，由 tools/model_picker_sync.js 注入到各页面的
// <script> 里（与 theme-max.css 由 theme_sync.js 注入同一套手法）。
// 改这里 → 跑同步脚本 → 各页面同时更新。

// 从模型名取上游：'cline/openai/gpt-5' → 'cline'。
//
// ⚠ 只切**第一个**斜杠。模型名本身可能还有斜杠（cline 的一堆都是
// 'cline/anthropic/claude-sonnet-5.5' 这种），按最后一个切会得到 'anthropic'
// —— 那是个模型厂牌不是我们的上游，筛出来对不上渠道配置。
function mpProviderOf(model) {
  var i = model.indexOf('/');
  return i < 0 ? '' : model.slice(0, i);
}

// 把模型列表按上游归组，按数量降序（常用的上游排在前面）。
//
// 返回 [{provider, models}]，provider 为 '' 的那组是「没有上游前缀」的模型
// （单上游部署、或上游 id 为空时会这样）—— 它**必须**单独成组，
// 否则那些模型会从筛选面板里彻底消失。
function mpGroupByProvider(models) {
  var byProv = Object.create(null);
  for (var i = 0; i < models.length; i++) {
    var m = models[i];
    var p = mpProviderOf(m);
    (byProv[p] || (byProv[p] = [])).push(m);
  }
  var out = [];
  for (var k in byProv) { out.push({ provider: k, models: byProv[k] }); }
  out.sort(function (a, b) {
    // 无前缀的那组排最后：它不是「一个上游」，视觉上单独放更好认。
    if (a.provider === '') { return 1; }
    if (b.provider === '') { return -1; }
    return b.models.length - a.models.length;
  });
  return out;
}

// 按上游 + 关键字过滤。
//
// provider 为空串表示「全部上游」；关键字只比模型名（含上游前缀，
// 这样输 'cline/' 也能筛）。
function mpFilter(models, provider, query) {
  var q = (query || '').trim().toLowerCase();
  return models.filter(function (m) {
    if (provider && mpProviderOf(m) !== provider) { return false; }
    if (q && m.toLowerCase().indexOf(q) < 0) { return false; }
    return true;
  });
}

// 渲染上游筛选条：每个上游一个按钮 + 数量。
//
// onPick(provider) 由调用方处理 —— 本函数只管画，不管状态。
// 空 provider 表示「全部」。
function mpRenderProviderBar(host, groups, active, totalAll, onPick) {
  if (!host) { return; }
  var items = [{ provider: '', label: '全部', count: totalAll }];
  for (var i = 0; i < groups.length; i++) {
    var g = groups[i];
    items.push({
      provider: g.provider,
      label: g.provider === '' ? '（无前缀）' : g.provider,
      count: g.models.length
    });
  }
  host.innerHTML = items.map(function (it) {
    var on = (it.provider === active) ? ' mp-on' : '';
    return '<button type="button" class="mp-chip' + on + '" data-p="' + it.provider + '">'
      + it.label + ' <span class="mp-n">' + it.count + '</span></button>';
  }).join('');
  // 事件委托：筛选条会整块重渲染，逐个绑监听会在重渲染后失效。
  host.onclick = function (ev) {
    var el = ev.target;
    while (el && el !== host && !el.getAttribute('data-p')) { el = el.parentNode; }
    if (!el || el === host) { return; }
    onPick(el.getAttribute('data-p') || '');
  };
}
