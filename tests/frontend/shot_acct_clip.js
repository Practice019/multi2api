const { withChrome, showPanel } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9340, url: 'http://127.0.0.1:18080/ui',
    readyExpr: `document.querySelectorAll('.acctgroup').length` }, async ({ ev, errs, shot }) => {
    await showPanel(ev, 'accounts', 2500);
    const D = JSON.parse(await ev(`(function(){
      // 按**表头文字**定位列，再量该列所有单元格是否溢出（scrollWidth > clientWidth）
      var heads = [].slice.call(document.querySelectorAll('.acctgroup table.accttable--aligned thead th'));
      var idx = {};
      heads.forEach(function(h, i){ if (!(h.textContent.trim() in idx)) idx[h.textContent.trim()] = i; });
      var res = [];
      Object.keys(idx).forEach(function(name){
        var i = idx[name];
        var clip = 0, total = 0, worst = 0, sample = '';
        var rows = document.querySelectorAll('.acctgroup table.accttable--aligned tbody tr');
        for (var r = 0; r < rows.length; r++) {
          var tds = rows[r].querySelectorAll('td');
          if (tds.length <= i) continue;
          var td = tds[i];
          total++;
          if (td.scrollWidth > td.clientWidth + 1) {
            clip++;
            var over = td.scrollWidth - td.clientWidth;
            if (over > worst) { worst = over; sample = td.textContent.trim().slice(0, 40); }
          }
        }
        res.push({ name: name, total: total, clipped: clip, worstOver: worst, sample: sample });
      });
      var allTd = document.querySelectorAll('.acctgroup table.accttable--aligned tbody td');
      var withTitle = 0, totalTd = 0, longest = 0, longestTxt = '';
      // ⚠⚠ 真正该断言的**不是**"所有单元格都有 title"，而是
      //   「**被截断的**单元格必须有 title」。
      //   第一版报「323/654 没有 title」看着像大问题，其实那 323 个是
      //   「成功 / 熔断 / 在途 / 额度」这类**短数字格，根本不会溢出** ——
      //   对它们要求 title 是给一个永远用不到的提示。
      //   而 title 的意义只在"截断之后还能看到全文"（知识库那条经验的 NEVER 项）。
      //   ⇒ 又一次印证：判据本身会错，而且错得看着很合理。
      var clippedNoTitle = 0, clippedTotal = 0, clippedNoTitleSample = '';
      for (var k = 0; k < allTd.length; k++) {
        totalTd++;
        var td = allTd[k];
        if (td.scrollWidth > td.clientWidth + 1) {
          clippedTotal++;
          if (!td.hasAttribute('title') || td.getAttribute('title') === '') {
            clippedNoTitle++;
            if (!clippedNoTitleSample) clippedNoTitleSample = td.textContent.trim().slice(0, 36);
          }
        }
        if (td.hasAttribute('title')) withTitle++;
        var L = td.textContent.trim().length;
        if (L > longest) { longest = L; longestTxt = td.textContent.trim().slice(0, 30); }
      }
      // nowrap 是否生效
      var anyTd = allTd[0];
      return JSON.stringify({
        cols: res, tdTotal: totalTd, tdWithTitle: withTitle,
        clippedTotal: clippedTotal, clippedNoTitle: clippedNoTitle,
        clippedNoTitleSample: clippedNoTitleSample,
        whiteSpace: anyTd ? getComputedStyle(anyTd).whiteSpace : '?',
        textOverflow: anyTd ? getComputedStyle(anyTd).textOverflow : '?',
        overflow: anyTd ? getComputedStyle(anyTd).overflow : '?',
        longest: longest, longestTxt: longestTxt
      });
    })()`));
    console.log('=== 账号表截断检测（fixed 布局引入的新风险）===');
    console.log('  单元格 ' + D.tdTotal + ' 个 · 带 title ' + D.tdWithTitle
      + ' · **被截断 ' + D.clippedTotal + ' 个**');
    console.log('');
    console.log('  列名'.padEnd(14) + '单元格  ' + '被截断  ' + '最大溢出');
    const clippedCols = [];
    D.cols.forEach((c) => {
      console.log('  ' + c.name.padEnd(12) + String(c.total).padStart(6) + String(c.clipped).padStart(8)
        + (c.clipped ? String(c.worstOver + 'px').padStart(10) + '  样本:' + c.sample : '      —'));
      if (c.clipped) clippedCols.push(c.name + '(' + c.clipped + '/' + c.total + ')');
    });
    console.log('');
    if (clippedCols.length) console.log('  ⇒ 有内容被截断的列（截断本身可以接受，见下）: ' + clippedCols.join(' '));
    else console.log('  ✓ 没有单元格内容被截断');
    // ⚠ 这条才是硬断言：截断了就必须能看到全文
    if (D.clippedNoTitle > 0) {
      console.log('  ⚠⚠ **被截断且没有 title 的单元格 ' + D.clippedNoTitle + ' 个** —— 等于信息丢失'
        + '（样本:' + D.clippedNoTitleSample + '）');
      process.exitCode = 2;
    } else if (D.clippedTotal > 0) {
      console.log('  ✓ 被截断的 ' + D.clippedTotal + ' 个单元格**全都带 title** ⇒ 悬停可见全文');
    }
    await shot('acct-4-clip.png');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(0);
})();
