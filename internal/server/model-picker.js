// model-picker.js —— 模型候选选择器（分组页用；与 API 管理页同一套规则）。
//
// # 这个文件解决什么
//
// 分组页要从**全站目录**（实测 699 个模型、15 个上游，cline 一家占 497）里
// 挑一部分。平分铺开等于大海捞针，所以必须「先选上游，再看它的模型」。
//
// # ⚠ 这是一次「本该复用却重造」的教训，写在这里防复发
//
// API 管理页**早就实现了**同样的东西（webui.html 的 #akProvider + modelOwnerOf
// + renderAKModelList），而且注释里明确记着用户的原话：
//
//   「我希望可以选择上游才能显示具体上游的模型，不要全部显示出来」
//
// 我第一版在分组页用「一排 chip 按钮」重写了一遍 —— 形态与 API 页不一致
// （那边是 <select> 下拉），而且**漏了 owned_by 兜底**。用户一眼看出不对：
//
//   「这个实现和api管理里面的实现不一样啊 直接复刻啊抽共享」
//
// 所以现在这份共享层是**照 API 页的规则复刻**的：
//   · 上游取自 modelOwnerOf（切第一个 / ，回退 owned_by）
//   · 控件形态是 <select> 下拉（与 API 页视觉一致）
// 以后新增页面要选模型，直接用这里，不要再写第三遍。

// mpOwnerOf 推断一个模型属于哪个上游。
//
// 与 webui.html 的 modelOwnerOf **逐字同构**（含 owned_by 兜底）。
//
// 为什么优先切 id 而不是只用 owned_by：id 的前缀是**我们的**上游 id
//（与 channels.json 的 name / 注册表 id 对齐），而 owned_by 是上游自己
// 报的厂商名（可能是 'openai'、'system' 这种），未必是我们的上游。
// 只有 id 里没有前缀时（单上游部署会这样）才退回 owned_by。
function mpOwnerOf(id, ownedBy) {
  var s = String(id || '').indexOf('/');
  return s > 0 ? String(id).slice(0, s) : (ownedBy || '');
}

// mpOwners 从目录里取出所有上游 id（去重、字典序）。
//
// 字典序而不是按数量：下拉框的顺序要稳定 —— 按数量排会让「用了几天之后
// 某个上游涨上去了，位置突然变了」，用户按记忆点第二项会点错。
function mpOwners(catalog) {
  var seen = Object.create(null);
  var out = [];
  for (var i = 0; i < catalog.length; i++) {
    var o = mpOwnerOf(catalog[i].id, catalog[i].owner);
    if (o && !seen[o]) { seen[o] = 1; out.push(o); }
  }
  out.sort();
  return out;
}

// mpFilter 按「上游 + 关键字」过滤目录条目（条目形如 {id, owner}）。
//
// provider 为空串 = 全部上游。
//
// ⚠ 上游筛选只决定**列出什么**，绝不改已勾选的 id ——
// 否则「切一下上游」就等于「把之前选的都取消」，用户会莫名其妙丢配置。
function mpFilter(catalog, provider, query) {
  var q = (query || '').trim().toLowerCase();
  return catalog.filter(function (m) {
    if (provider && mpOwnerOf(m.id, m.owner) !== provider) { return false; }
    if (q && m.id.toLowerCase().indexOf(q) < 0) { return false; }
    return true;
  });
}

// mpFillProviderSelect 用当前目录填充上游下拉。
//
// ⚠ 恢复选中值必须写在 innerHTML **之后**：
// <select> 会把「不在 option 列表里的值」当场归一成空串，先设值会被清掉。
// （webui.html 的 syncAKProviders 里记着同一个坑。）
function mpFillProviderSelect(sel, catalog, allLabel) {
  if (!sel) { return; }
  var keep = sel.value;
  var owners = mpOwners(catalog);
  var html = '<option value="">' + (allLabel || '全部上游') + '</option>';
  for (var i = 0; i < owners.length; i++) {
    html += '<option value="' + mpHtmlEsc(owners[i]) + '">' + mpHtmlEsc(owners[i]) + '</option>';
  }
  sel.innerHTML = html;
  sel.value = owners.indexOf(keep) >= 0 ? keep : '';
}

// mpHtmlEsc 最小 HTML 转义（上游 id 理论上是 slugs，但不该赌它）。
function mpHtmlEsc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
