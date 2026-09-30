// 验证分组行按钮**真的**被推到右侧（`.gacts{margin-left:auto}` 生效）。
//
// # 为什么需要这条探针（用户报的「按钮不靠右」）
//
// CSS 选择器写错**不会报错**，只是规则静默不匹配 —— 表现是"样式没生效"，
// 而没有任何日志或异常。我第一版写的是
//
//	.acctgroup > .gacts { margin-left: auto }
//
// 而实际 DOM 是 `.acctgroup > .ghead > .gacts` —— `.gacts` 不是
// `.acctgroup` 的直接子节点，所以那条规则**从来没有匹配过**。
//
// 这类 bug 靠"读代码"很难发现（选择器看起来完全合理），
// 必须**按真实 DOM 逐段验证**。本探针做两件事：
//
//	① 从 webui.html 里把渲染分组行的 HTML 抠出来，解析出 DOM 层级
//	② 对层级里每个 .gacts 元素的**祖先链**，检查是否存在一条
//	   margin-left:auto 的规则能命中它
//
// 不引入 jsdom（本仓前端探针都用 Node 原生）——这里只需要判断
// "选择器的层级链是否与 DOM 的祖先链一致"，用字符串比对即可。
const fs = require('fs');

const html = fs.readFileSync('internal/server/webui.html', 'utf8');

let fails = 0;
const check = (name, cond, detail) => {
  console.log((cond ? 'PASS ' : 'FAIL ') + name);
  if (!cond) { fails++; if (detail) console.log('      ' + detail); }
};

// ---- ① 从 real DOM 提取 .gacts 的祖先链 ----
//
// 分组行的 HTML 由一个模板串拼出来（accountGroupHTML）。这里不执行它，
// 而是直接读模板串里的字面结构 —— 探针要验证的正是"模板写的层级"
// 与"CSS 选择器写的层级"是否一致。
const tplStart = html.indexOf("+ `<div class=\"ghead\">`");
if (tplStart < 0) {
  console.log('FAIL 找不到分组行模板（accountGroupHTML 的 `<div class="ghead">`）');
  process.exit(1);
}
// 往后取一段，够覆盖到 </div>
const tpl = html.slice(tplStart, tplStart + 2000);

check('模板里有 .ghead', tpl.includes('class="ghead"'));
check('模板里有 .gacts', tpl.includes('class="gacts"'));
check('模板里有 .ghead-btn（标题按钮）', tpl.includes('class="ghead-btn"'));
// .gacts 必须出现在 .ghead 之内（即 .ghead 开标签在 .gacts 之前，
// 而 </div> 在其后）—— 这就是"三层"而不是"两层"的证据。
const iGhead = tpl.indexOf('class="ghead"');
const iGacts = tpl.indexOf('class="gacts"');
const iClose = tpl.indexOf('</div>', iGhead);
check('.gacts 在 .ghead 内部（不是 .acctgroup 的直接子节点）',
  iGhead >= 0 && iGacts > iGhead && iClose > iGacts,
  `ghead@${iGhead} gacts@${iGacts} </div>@${iClose}`);

// ---- ② CSS 选择器必须与那个层级一致 ----
//
// 取出所有含 margin-left:auto 且提到 .gacts 的规则，检查它是否写了三层。
const cssRules = html.split('\n').filter((l) => l.includes('.gacts') && l.includes('margin-left:auto'));
check('存在 .gacts 的 margin-left:auto 规则', cssRules.length > 0,
  '没找到 → 按钮不会被推到右侧');
const threeLevel = html.split('\n').some((l) =>
  l.includes('.acctgroup > .ghead > .gacts') && l.includes('margin-left:auto'));
check('该规则写的是三层选择器 .acctgroup > .ghead > .gacts', threeLevel,
  '写两层（.acctgroup > .gacts）不匹配实际 DOM → 规则静默失效（我第一版的 bug）');

// ---- ③ 不得同时残留那条错误的两层规则 ----
//
// 留着它无害（不匹配），但会误导后来者以为"已经有右对齐了"。
// 且若将来 DOM 变成两层，两条规则会同时生效 —— 语义重复。
const staleTwoLevel = html.split('\n').some((l) =>
  /^\s*\.acctgroup > \.gacts\{/.test(l));
check('没有残留的两层规则 .acctgroup > .gacts{', !staleTwoLevel,
  '两层规则不匹配实际 DOM，留着会让人以为已经右对齐了');

// ---- ④ 按钮都在 .gacts 里（用户要求"这些按钮全部右对齐"）----
//
// ⚠ 「添加账号」的文案**不带 `＋`**（用户明确要求"添加账号不需要那个加号"）。
// 这里跟着改 —— 否则这条探针会因为一个已经不存在的加号而永远红。
//
// ⚠ 顺序也由用户指定（第三版）：
//
//	全部签到 → 添加账号 → 批量导入 → 刷新本上游额度 → 重载 auths
//
// 顺序由下面 ⑤ 单独钉住；这里只查"每一项都还在"。
const wanted = ['添加账号', '批量导入', '刷新本上游额度', '重载 auths'];
for (const label of wanted) {
  check(`按钮「${label}」由 accountGroupActions 产出`, html.includes(label),
    `没找到「${label}」`);
}
check('accountGroupActions 的输出挂在 .gacts 里',
  /class="gacts">\$\{accountGroupActions\(/.test(html),
  '按钮若不在 .gacts 容器里，margin-left:auto 推不到它们');

// ---- ⑤ 五项的出现次序必须与用户指定的完全一致 ----
//
// # 为什么这条值得单独立
//
// 用户连续给了三版顺序，前两版我理解错了。把次序钉在探针里，
// 下次有人"顺手重排"时会当场红，而不是等用户再报一次。
//
// 判据取**最后一次 return** 里各变量的出现位置（accountGroupActions 只有一个
// return，中间那些 var 定义不算）。
const retAt = html.lastIndexOf('return gDailyAll');
if (retAt < 0) {
  check('找到 accountGroupActions 的 return', false, '找不到 return gDailyAll');
} else {
  const ret = html.slice(retAt, retAt + 400);
  const order = ['gDailyAll', 'addBtn', 'importBtn', 'gQuotaBtn', 'reloadBtn'];
  let last = -1;
  let ok = true;
  const seen = [];
  for (const v of order) {
    const i = ret.indexOf(v);
    seen.push(v + '@' + i);
    if (i < 0 || i < last) { ok = false; }
    last = i;
  }
  check('按钮顺序 == 全部签到 → 添加账号 → 批量导入 → 刷新额度 → 重载 auths',
    ok, '实际位置: ' + seen.join(' '));
}

console.log(fails === 0 ? 'ALL PASS' : ('FAILED: ' + fails));
process.exit(fails === 0 ? 0 : 1);
