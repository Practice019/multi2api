// 列宽拖拽 —— 滚动容器那部分（2026-10-19）
//
// 验的是三件在"拖动"之外、但同一轮必须成立的事：
//   ① 加 `.acctwrap` 之后，`> table` 那两条子选择器**没被打断**（折叠功能还活着）
//   ② 把某列拖到超出卡片之后，**右边的内容是"要滚"而不是"被裁掉"**
//   ③ 拖拽本身回归正常
//
// ⚠ 两条踩过的坑，都留在脚本里当注释（别再犯）：
//   · 溢出要量**父容器**，不是表格自己 —— 表格 width:auto 会跟着内容长，
//     它的 scrollWidth === clientWidth 恒成立（第一版就这么误判过）
//   · `scrollLeft` 是异步生效的，设完必须**等一拍**再读坐标
//     （与 popover 的 toggle 同一个坑）
const { withChrome, showPanel, sleep } = require('./lib_cdp.js');

const Q = String.fromCharCode(39);   // 单引号，避免在模板串里叠引号

(async () => {
  await withChrome({ port: 9355, url: 'http://127.0.0.1:18080/ui', width: 1600, height: 1000,
    readyExpr: `document.querySelectorAll('.acctgroup').length`, outDir: 'shots' },
    async ({ ev, errs, shot, send }) => {
    await showPanel(ev, 'accounts', 2500);
    // 干净起点
    await ev(`(function(){ try{ Object.keys(localStorage).forEach(function(k){ if(k.indexOf('wb2api.acctcol')===0) localStorage.removeItem(k); }); }catch(e){} return 1; })()`);
    await ev(`location.reload()`);
    await sleep(3200);
    await showPanel(ev, 'accounts', 2500);
    await sleep(1500);

    // ---------- ① 折叠没被打断 ----------
    console.log('=== ① 折叠功能还在吗（`> table` 那条选择器没被打断）===');
    // 先确保是展开态（上一次运行可能把它留成折叠了），再点一次看是否收起
    await ev(`(function(){ var s=document.querySelectorAll('section.acctgroup')[1];
      if (s.classList.contains('collapsed')) s.querySelector('button[data-atoggle]').click(); return 1; })()`);
    await sleep(400);
    const before = await ev(`getComputedStyle(document.querySelectorAll('section.acctgroup')[1].querySelector('table')).display`);
    await ev(`(function(){ document.querySelectorAll('section.acctgroup')[1].querySelector('button[data-atoggle]').click(); return 1; })()`);
    await sleep(400);
    const afterF = await ev(`getComputedStyle(document.querySelectorAll('section.acctgroup')[1].querySelector('table')).display`);
    console.log('  table display: ' + before + ' → ' + afterF
      + (afterF === 'none' ? '  ✓ 折叠生效，选择器没被打断' : '  ⚠⚠ 折叠坏了'));
    if (afterF !== 'none') process.exitCode = 2;
    await ev(`(function(){ document.querySelectorAll('section.acctgroup')[1].querySelector('button[data-atoggle]').click(); return 1; })()`);
    await sleep(400);
    const restored = await ev(`getComputedStyle(document.querySelectorAll('section.acctgroup')[1].querySelector('table')).display`);
    console.log('  再点一次展开: ' + restored + (restored !== 'none' ? '  ✓' : '  ⚠⚠'));
    if (restored === 'none') process.exitCode = 2;

    // ---------- ② 拖宽之后右边"要滚"而不是"被裁" ----------
    console.log('');
    console.log('=== ② 把「上游」拖到 828px，右侧内容还能滚到吗 ===');
    const g = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var grip = s.querySelector('.colgrip[data-colgrip="provider"]');
      grip.scrollIntoView({ block: 'nearest' });
      var q = grip.getBoundingClientRect();
      return JSON.stringify({ x: Math.round(q.left + q.width/2), y: Math.round(q.top + q.height/2) });
    })()`));
    await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: g.x, y: g.y, button: 'left', clickCount: 1, pointerType: 'mouse' });
    for (let i = 1; i <= 5; i++) {
      await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: g.x + Math.round(700*i/5), y: g.y, button: 'left', buttons: 1, pointerType: 'mouse' });
      await sleep(70);
    }
    await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: g.x + 700, y: g.y, button: 'left', clickCount: 1, pointerType: 'mouse' });
    await sleep(1000);

    const w0 = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var w = s.querySelector('.acctwrap');
      var t = w.querySelector('table');
      return JSON.stringify({ overflowX: getComputedStyle(w).overflowX,
        sw: w.scrollWidth, cw: w.clientWidth, over: w.scrollWidth - w.clientWidth,
        tableW: Math.round(t.getBoundingClientRect().width) });
    })()`));
    console.log('  .acctwrap overflow-x=' + w0.overflowX);
    console.log('  表宽 ' + w0.tableW + ' · 容器 scrollWidth ' + w0.sw + ' / clientWidth ' + w0.cw);
    console.log('  ⇒ 可滚 ' + w0.over + 'px' + (w0.over > 100 ? '  ✓ 真的可以横向滚（不是被裁掉）' : '  ⚠⚠ 溢出了但滚不动'));
    if (w0.over <= 100) process.exitCode = 2;

    // ⚠ scrollLeft 是异步生效的：设完必须等一拍再读坐标
    await ev(`(function(){ document.querySelectorAll('section.acctgroup')[1].querySelector('.acctwrap').scrollLeft = 999999; return 1; })()`);
    await sleep(300);
    const w1 = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var w = s.querySelector('.acctwrap');
      // ⚠ 量**那一列的 th**，不是把手：把手（.colgrip）是**故意探出**
      //   右缘 3px 的装饰（为了好点中），滚到底时内容右缘恰好贴容器右缘，
      //   所以把手必然超 3px —— 拿它当判据永远差那 3px（第一版就这么误判的）。
      //   用户真正关心的是"那一列的内容看不看得见"。
      var ths = s.querySelectorAll('thead th');
      var thOps = ths[ths.length - 1];
      var cols = s.querySelectorAll('colgroup > col');
      var lastCol = cols[cols.length - 1];
      var wr = w.getBoundingClientRect(), tr = thOps.getBoundingClientRect();
      return JSON.stringify({ scrollLeft: w.scrollLeft,
        thLeft: Math.round(tr.left), thRight: Math.round(tr.right),
        lastColW: lastCol ? lastCol.style.width : '?',
        wrapLeft: Math.round(wr.left), wrapRight: Math.round(wr.right) });
    })()`));
    console.log('  滚到底：scrollLeft=' + w1.scrollLeft);
    console.log('    最后一列（操作，宽 ' + w1.lastColW + '）的 th ' + w1.thLeft + '..' + w1.thRight
      + ' · 容器 ' + w1.wrapLeft + '..' + w1.wrapRight);
    const vis = w1.thLeft >= w1.wrapLeft - 2 && w1.thRight <= w1.wrapRight + 2;
    console.log('  ' + (vis ? '✓ **滚到最右后「操作」列完整可见**（内容在，只是要滚）'
      : '⚠⚠ 滚到底了还看不到'));
    if (!vis) process.exitCode = 2;
    await shot('colw-3-scrollable.png');

    // ---------- ③ 回归 ----------
    console.log('');
    console.log('=== ③ 拖拽本身仍然正常 ===');
    const r = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      return JSON.stringify({ grips: s.querySelectorAll('.colgrip').length,
        ths: s.querySelectorAll('thead th').length,
        rows: s.querySelectorAll('tbody tr').length,
        saved: localStorage.getItem('wb2api.acctcolw.workbuddy') });
    })()`));
    console.log('  把手 ' + r.grips + ' / 表头 ' + r.ths + ' / 数据行 ' + r.rows + ' · 存盘 ' + r.saved);
    if (r.grips !== r.ths) process.exitCode = 2;
    if (!r.rows) process.exitCode = 2;

    console.log('');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) process.exitCode = 2;
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
