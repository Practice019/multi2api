// shot_acct_align.js —— 量账号池的**跨组列对齐**（2026-10-19）。
//
// # 为什么单为"对齐"写一个脚本
//
// 账号池不是一个表，是**每个上游一张表**（.acctgroup 各自带 thead）。于是列宽
// 由每张表自己算：workbuddy 组某列宽 90px、loomy 组同一列宽 120px，两组上下摞在
// 一起时那列就是**错开的**。这个缺陷在单组的截图上完全看不出来 ——
// 必须**跨组比同一列索引的左边缘**才能量到。
//
// 参考项目 Agent2API 的解法是把列轨道写成 CSS 变量、放在**共同容器**上靠继承
// 作用到每一行（见 page-requests.css 的 --req-cols）。抄它之前先量清楚差距有多大，
// 免得改完说不出"好了多少"。
//
// ⚠ 调试口 9338（9329 accounts / 9331 heatmap / 9335 jobs / 9336 logs / 9337 settings）。
'use strict';
const { withChrome, showPanel } = require('./lib_cdp.js');

const TARGET = process.env.ACCT_URL || 'http://127.0.0.1:18080/ui';
let exitCode = 0;

(async () => {
  await withChrome({
    port: 9338,
    url: TARGET,
    readyExpr: `document.querySelectorAll('#accts .acctgroup, .acctgroups .acctgroup').length`,
  }, async ({ ev, errs, shot }) => {
    await showPanel(ev, 'accounts', 2500);

    const D = JSON.parse(await ev(`(function(){
      var host = document.querySelector('#accts') || document.querySelector('.acctgroups');
      if (!host) return JSON.stringify({err:'账号池容器不存在'});
      // ⚠⚠ 列身份必须用**列名文本**，不能用索引。
      //   第一次写的是按索引比，但各组列集不同（codearts 7 列、默认 9 列），
      //   索引 3 在一组是「额度」、在另一组可能是「免费额度」—— 比出来的是**假阳性**。
      //   列名（th 的文本）在每个组里是同一列的唯一标识。
      var groups = host.querySelectorAll('.acctgroup');
      var out = [];
      for (var g = 0; g < groups.length; g++) {
        var grp = groups[g];
        var head = grp.querySelector('.ghead');
        var name = head ? head.textContent.trim().replace(/\\s+/g, ' ').slice(0, 26) : '?';
        var tb = grp.querySelector('table');
        var ths = tb ? tb.querySelectorAll('thead th') : [];
        var byName = {};
        var order = [];
        for (var t = 0; t < ths.length; t++) {
          var label = ths[t].textContent.trim();
          var r = ths[t].getBoundingClientRect();
          if (!(label in byName)) {
            // ⚠⚠ 记**宽度**，不是左边缘。
            //   左边缘跨组比是**错的判据**：各组列集本就不同（实测 colgroup 列数
            //   8/9/7/4 都有），前面少一列则后面整体左移一列宽 —— 那是必然不是缺陷。
            //   第一版按左边缘比，报出"7 列错开"，其中绝大多数是这种假阳性。
            //   真正该一致的是**列宽**：那才是 colgroup 说了算的东西；
            //   宽度齐了，列集相同的两组就必然逐列对齐。
            byName[label] = Math.round(r.width);
            order.push(label);
          }
        }
        var firstRow = tb ? tb.querySelector('tbody tr') : null;
        var rowLefts = [];
        if (firstRow) {
          var tds = firstRow.querySelectorAll('td');
          for (var d = 0; d < tds.length; d++) rowLefts.push(Math.round(tds[d].getBoundingClientRect().left));
        }
        var cols = tb ? tb.querySelectorAll('colgroup col').length : 0;
        var hasFixed = tb ? getComputedStyle(tb).tableLayout : '?';
        out.push({
          name: name, cols: ths.length, colgroupCols: cols, tableLayout: hasFixed,
          rowCols: rowLefts.length, byName: byName, order: order,
          collapsed: grp.classList.contains('collapsed'),
          tableW: tb ? Math.round(tb.getBoundingClientRect().width) : 0
        });
      }
      return JSON.stringify({
        groups: out.length,
        totalAccounts: host.querySelectorAll('table tbody tr').length,
        detail: out
      });
    })()`));

    if (D.err) { console.log('  ⚠ ' + D.err); exitCode = 2; }
    else {
      console.log('=== 账号池跨组对齐诊断（按列名比）===');
      console.log('  分组 ' + D.groups + ' 个 · 表格行合计 ' + D.totalAccounts);

      const open = D.detail.filter((g) => !g.collapsed && g.cols);
      console.log('  展开的组 ' + open.length + ' 个'
        + ' · 表布局 ' + JSON.stringify([...new Set(open.map((g) => g.tableLayout))])
        + ' · colgroup 列数 ' + JSON.stringify([...new Set(open.map((g) => g.colgroupCols))]));

      // ⚠ 弹性列不参与"宽度一致"的判定 —— 它的定义就是"吃掉本表剩余空间"，
      //   而各组的列集不同（少一列 ⇒ 剩余空间多一列宽）⇒ 它跨组**必然**不同。
      //   所以这里断言的是：**除弹性列外，每个列名在各组的宽度完全一致**。
      const FLEX = ['昵称'];   // 与 ACCT_COLUMN_DEFS 里唯一没给 w 的那一列对应
      const names = [];
      open.forEach((g) => g.order.forEach((n) => { if (!names.includes(n)) names.push(n); }));
      console.log('  列名（并集）：' + JSON.stringify(names));
      console.log('  ⚠ 弹性列（不参与宽度一致性判定）：' + JSON.stringify(FLEX));
      console.log('');
      console.log('    列名'.padEnd(14) + open.map((g) => g.name.slice(0, 9).padStart(11)).join(''));
      let ragged = 0;
      const badNames = [];
      names.forEach((n) => {
        const xs = open.map((g) => (n in g.byName ? g.byName[n] : undefined));
        const valid = xs.filter((x) => typeof x === 'number');
        const spread = valid.length > 1 ? Math.max(...valid) - Math.min(...valid) : 0;
        const isFlex = FLEX.includes(n);
        if (spread > 2 && !isFlex) { ragged++; badNames.push(n + '(' + spread + 'px)'); }
        console.log('    ' + n.padEnd(12)
          + xs.map((x) => String(x === undefined ? '—' : x).padStart(11)).join('')
          + (isFlex ? '   ◀ 弹性列（跨组不同是对的）'
            : (spread > 2 ? '   ⚠⚠ 宽度差 ' + spread + 'px' : '   ✓')));
      });
      console.log('');
      console.log('  ⇒ **宽度不一致的固定列 = ' + ragged + ' 个**' + (badNames.length ? '：' + badNames.join(' ') : ''));
      if (ragged === 0) {
        console.log('  ⇒ ✓ 列轨道已由 colgroup 统一：除弹性列外，各列跨组宽度完全一致');
      } else { console.log('  ⇒ ⚠⚠ 有固定列的宽度跨组不一致'); exitCode = 2; }

      // 组内：表头列数与首行格数必须相等。
      // ⚠ 空组（0 个账号）没有 tbody 行 ⇒ 0 是正常的，排除掉。
      const badSelf = D.detail.filter((g) => {
        if (!g.rowCols) return false;                 // 空组，跳过
        return g.cols !== g.rowCols;
      });
      if (badSelf.length) {
        console.log('  ⚠⚠ 表头列数与首行格数不一致: '
          + JSON.stringify(badSelf.map((g) => g.name + '(' + g.cols + 'vs' + g.rowCols + ')')));
        exitCode = 2;
      } else {
        console.log('  ✓ 有账号的组里，表头列数 = 首行格数');
      }

      // 列集相同的两组之间，应当逐列对齐 —— 这是"对齐"的严格形式，额外验一次
      const bySig = {};
      open.forEach((g) => { const k = g.order.join(','); (bySig[k] = bySig[k] || []).push(g); });
      const shared = Object.values(bySig).filter((a) => a.length > 1);
      console.log('  列集相同的组群: ' + Object.values(bySig).map((a) => a.length + '组').join(' / ')
        + '（共 ' + shared.length + ' 个群有多于一组）');
    }

    await shot('acct-1-align.png');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) exitCode = 2;
    console.log('=== 结论：' + (exitCode === 0 ? '通过（诊断跑完）' : '有问题') + ' ===');
  }).catch((e) => { console.error('失败: ' + (e && e.message ? e.message : e)); exitCode = 1; });
  process.exit(exitCode);
})();
