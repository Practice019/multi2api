// model-picker.js —— 「按上游挑模型」的共享实现。
//
// # 这个文件是**从 webui.html 抽出来的**，不是新写的
//
// API 管理页早就有这套逻辑（modelOwnerOf + syncAKProviders + renderAKModelList，
// 注释里记着用户原话「我希望可以选择上游才能显示具体上游的模型」）。
// 我做分组页时没读那段，重写了一遍，于是：
//
//   · 形态不一致（那边 select 下拉，我这边一排按钮）
//   · 规则不一致（那边有 owned_by 兜底，我这边只切斜杠）
//   · 还引入了 [object Object]（catalog 条目是 {id,owner}，我按字符串渲染）
//
// 用户连问两次：「这个不是已经实现了么 抽共享」「直接共享层使用啊」。
//
// 所以现在：**webui.html 自己也用这里**（tools/model_picker_sync.js 注入），
// 三处（API 管理页 / 对话测试 / 分组页）跑的是同一份函数。
// 改这里 = 三处同时改，不可能再漂移。

// mpOwnerOf 从模型 id 推断上游归属。
//
// 有前缀用前缀（那是**我们的**上游 id，与 channels.json / 注册表对齐），
// 没有才回退 owned_by（那是上游自己报的厂商名，未必是我们的上游）。
function mpOwnerOf(id, ownedBy) {
  var s = String(id || '').indexOf('/');
  return s > 0 ? String(id).slice(0, s) : (ownedBy || '');
}

// mpOwners 取一组模型里出现过的上游 id（去重）。
//
// rank 可选：给定排序函数就用它（webui.html 按 manifest 顺序排），
// 否则按字典序（分组页没有 manifest 上下文）。
function mpOwners(list, rank) {
  var seen = Object.create(null);
  var out = [];
  for (var i = 0; i < list.length; i++) {
    var m = list[i];
    var owned = (m.owned_by !== undefined) ? m.owned_by : m.owner;
    var o = mpOwnerOf(m.id, owned);
    if (o && !seen[o]) { seen[o] = 1; out.push(o); }
  }
  out.sort(rank || function (a, b) { return a < b ? -1 : (a > b ? 1 : 0); });
  return out;
}

// mpFillProviderSelect 填充「上游」下拉并尽量保留当前选中值。
//
// ⚠ 恢复选中值必须写在 innerHTML **之后**：
// select 会把「不在 option 列表里的值」当场归一成空串，先设值会被清掉。
// （webui.html 的 syncAKProviders 注释里记着同一个坑。）
//
// 返回实际生效的值（不在列表里时回落空串 = 全部上游）。
function mpFillProviderSelect(sel, list, rank, allLabel) {
  if (!sel) { return ''; }
  var keep = sel.value;
  var owners = mpOwners(list, rank);
  var esc = mpEsc;
  var html = '<option value="">' + (allLabel || '全部上游') + '</option>';
  for (var i = 0; i < owners.length; i++) {
    html += '<option value="' + esc(owners[i]) + '">' + esc(owners[i]) + '</option>';
  }
  sel.innerHTML = html;
  var want = owners.indexOf(keep) >= 0 ? keep : '';
  sel.value = want;
  return want;
}

// mpFilter 按「上游 + 关键字」过滤（条目形如 {id, owned_by} 或 {id, owner}）。
//
// ⚠ 上游筛选只决定**列出什么**，绝不改已勾选的 id ——
// 否则「切一下上游」就等于「把之前选的都取消」，用户会莫名其妙丢配置。
function mpFilter(list, provider, query) {
  var q = String(query || '').trim().toLowerCase();
  return list.filter(function (m) {
    var owned = (m.owned_by !== undefined) ? m.owned_by : m.owner;
    if (provider && mpOwnerOf(m.id, owned) !== provider) { return false; }
    if (q && String(m.id).toLowerCase().indexOf(q) < 0) { return false; }
    return true;
  });
}

// mpEsc 最小 HTML 转义。
//
// 各页面都有自己的 esc()（实现略有差异），这里自带一份，
// 免得共享函数依赖宿主的同名函数 —— 那样「抽出来」就成了假共享。
function mpEsc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
