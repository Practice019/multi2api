const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9348, url: 'http://127.0.0.1:18080/ui',
    readyExpr: `document.querySelectorAll('.acctgroup').length` }, async ({ ev, errs, shot }) => {
    await showPanel(ev, 'dashboard', 3000).catch(() => {});
    // ⚠ 等热力图真的画出来（切面板后要等一次数据）
    for (let i = 0; i < 20; i++) {
      const n = await ev(`document.querySelectorAll('.heatgrid .heatcell').length`);
      if (n > 100) break;
      await sleep(500);
    }
    const D = JSON.parse(await ev(`(function(){
      var main = document.querySelector('.heatmain');
      var grid = document.querySelector('.heatgrid');
      var days = document.querySelector('.heatdays');
      var cells = grid ? grid.querySelectorAll('.heatcell') : [];
      if (!main || !grid || !cells.length) return JSON.stringify({err:'热力图没画出来 cells='+cells.length});
      // ⚠ grid-auto-flow:column ⇒ 前 7 个是第 1 列（周一..周日）
      var c3 = cells[2];
      var d3 = days ? days.querySelectorAll('span')[1] : null;
      var sc = [];
      document.querySelectorAll('*').forEach(function(e){
        if (e.scrollWidth > e.clientWidth + 2 && e.clientWidth > 100) {
          var cs = getComputedStyle(e);
          if (/auto|scroll/.test(cs.overflowX)) sc.push({cls:(e.className||'').toString().slice(0,30), over: e.scrollWidth-e.clientWidth});
        }
      });
      var cb = cells[0].getBoundingClientRect();
      return JSON.stringify({
        main: [Math.round(main.getBoundingClientRect().width), main.clientWidth, main.scrollWidth],
        parentW: Math.round(main.parentElement.getBoundingClientRect().width),
        gridW: Math.round(grid.getBoundingClientRect().width),
        cells: cells.length, cellBox: [Math.round(cb.width), Math.round(cb.height)],
        daysH: days ? Math.round(days.getBoundingClientRect().height) : null,
        gridH: Math.round(grid.getBoundingClientRect().height),
        d3: d3 ? Math.round(d3.getBoundingClientRect().top + d3.getBoundingClientRect().height/2) : null,
        c3: c3 ? Math.round(c3.getBoundingClientRect().top + c3.getBoundingClientRect().height/2) : null,
        scrollables: sc
      });
    })()`));
    if (D.err) { console.log('  ⚠ ' + D.err); process.exitCode = 2; }
    else {
      console.log('=== 热力图（自适应版）===');
      console.log('  .heatmain 宽 ' + D.main[0] + ' · clientW ' + D.main[1] + ' · scrollW ' + D.main[2]
        + ' · 父容器 ' + D.parentW);
      console.log('  .heatgrid ' + D.gridW + ' · 单元格 ' + D.cells + ' 个 · 单格 ' + JSON.stringify(D.cellBox));
      console.log('  days 高 ' + D.daysH + ' / grid 高 ' + D.gridH);
      console.log('  "三" 中心 y=' + D.d3 + ' · 第3行中心 y=' + D.c3
        + ' ⇒ 偏差 ' + (D.d3 !== null && D.c3 !== null ? Math.abs(D.d3 - D.c3) : '?') + 'px');
      const over = D.main[2] - D.main[1];
      console.log('');
      console.log('  ① 横向滚动条: ' + (over <= 1 ? '**无** ✓' : '⚠⚠ 溢出 ' + over + 'px'));
      console.log('     全页横向可滚元素 ' + D.scrollables.length + ' 个' + (D.scrollables.length ? ' ⚠ ' + JSON.stringify(D.scrollables) : ' ✓'));
      console.log('  ② 铺满: .heatmain ' + D.main[0] + ' vs 父容器 ' + D.parentW
        + ' ⇒ ' + (D.parentW - D.main[0] < 500 ? '与右侧统计区共分 ✓' : '⚠ 仍偏窄'));
      console.log('  ③ 格宽 ' + D.cellBox[0] + 'px（参考落点 14px，≥14 = 更大 ✓）');
      if (over > 1) process.exitCode = 2;
      if (D.cellBox[1] !== 12) { console.log('  ⚠⚠ 格高不是 12px'); process.exitCode = 2; }
    }
    await shot('heat-4-adaptive.png');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
