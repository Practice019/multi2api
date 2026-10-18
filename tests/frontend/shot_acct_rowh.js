const { withChrome, showPanel } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9339, url: 'http://127.0.0.1:18080/ui',
    readyExpr: `document.querySelectorAll('.acctgroup').length` }, async ({ ev, errs, shot }) => {
    await showPanel(ev, 'accounts', 2500);
    const D = JSON.parse(await ev(`(function(){
      var rows = document.querySelectorAll('.acctgroup table.accttable--aligned tbody tr');
      if (!rows.length) return JSON.stringify({err:'无数据行'});
      var r = rows[0].getBoundingClientRect();
      var td = rows[0].querySelector('td');
      var cs = getComputedStyle(td);
      var b = rows[0].querySelector('button');
      var th = document.querySelector('.acctgroup table.accttable--aligned thead th');
      // 所有行高是否一致（AGENTS 第5条：**固定**高度一行）
      var hs = {}, i;
      for (i = 0; i < rows.length; i++) {
        var h = Math.round(rows[i].getBoundingClientRect().height);
        hs[h] = (hs[h] || 0) + 1;
      }
      var buttons = document.querySelectorAll('.acctgroup table.accttable--aligned tbody button');
      var bhs = {};
      for (i = 0; i < buttons.length; i++) {
        var bh = Math.round(buttons[i].getBoundingClientRect().height);
        bhs[bh] = (bhs[bh] || 0) + 1;
      }
      return JSON.stringify({
        rowH: Math.round(r.height), heightHist: hs,
        tdPad: cs.paddingTop + '/' + cs.paddingBottom,
        btnH: b ? Math.round(b.getBoundingClientRect().height) : null, btnHist: bhs,
        tdFont: cs.fontSize, thPad: getComputedStyle(th).paddingTop,
        thH: Math.round(th.getBoundingClientRect().height),
        rows: rows.length
      });
    })()`));
    console.log('=== 账号表行高（目标 40px）===');
    if (D.err) { console.log('  ⚠ ' + D.err); }
    else {
      console.log('  **行高 = ' + D.rowH + 'px** · td padding ' + D.tdPad + ' · 字号 ' + D.tdFont);
      console.log('  按钮高 ' + D.btnH + 'px（' + JSON.stringify(D.btnHist) + '）');
      console.log('  表头高 ' + D.thH + 'px · padding-top ' + D.thPad);
      console.log('  **全部 ' + D.rows + ' 行的行高分布** = ' + JSON.stringify(D.heightHist));
      const keys = Object.keys(D.heightHist);
      if (keys.length === 1) console.log('  ✓ 所有行行高一致（AGENTS 第5条"固定高度一行"）');
      else { console.log('  ⚠⚠ 行高不一致：' + keys.join('/')); process.exitCode = 2; }
      if (D.rowH === 40) console.log('  ✓ 落在参考项目的 40px');
      else console.log('  ⇒ 实测 ' + D.rowH + 'px（目标 40px）');
    }
    await shot('acct-3-rowheight40.png');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
