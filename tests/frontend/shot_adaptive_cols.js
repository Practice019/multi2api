const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9357, url: 'http://127.0.0.1:18080/ui', width: 2160, height: 1230,
    readyExpr: `document.querySelectorAll('.acctgroup').length`, outDir: 'shots' }, async ({ ev, errs, shot, send }) => {
    await showPanel(ev, 'accounts', 2500);
    await ev(`(function(){ try{ Object.keys(localStorage).forEach(function(k){ if(k.indexOf('wb2api.acctcol')===0) localStorage.removeItem(k); }); }catch(e){} return 1; })()`);
    await ev(`location.reload()`); await sleep(3400);
    await showPanel(ev, 'accounts', 2500); await sleep(1800);

    console.log('=== ① 顶栏那个方块没了没 ===');
    const imgs = JSON.parse(await ev(`(function(){
      var h = document.querySelector('header');
      var out = [];
      for (var i=0;i<h.children.length;i++){
        var c = h.children[i];
        var r = c.getBoundingClientRect();
        out.push(c.tagName + (c.id?('#'+c.id):'') + ' [' + Math.round(r.left) + ',' + Math.round(r.width) + ']');
      }
      return JSON.stringify({ imgs: h.querySelectorAll('img').length, kids: out,
        firstLeft: h.firstElementChild ? Math.round(h.firstElementChild.getBoundingClientRect().left) : null });
    })()`));
    console.log('  header 里 img 数 = ' + imgs.imgs + (imgs.imgs === 0 ? '  ✓ 已去掉' : '  ⚠⚠ 还在'));
    if (imgs.imgs !== 0) process.exitCode = 2;
    console.log('  header 子元素: ' + imgs.kids.join('  '));
    console.log('  第一个子元素 left = ' + imgs.firstLeft + '（原来 img 在 left=16，现在 h1 顶上去）');
    // 再确认坐标 (55,22) 上已经不是 img
    const at = await ev(`(function(){ var e = document.elementFromPoint(55, 22); return e ? e.tagName : 'null'; })()`);
    console.log('  坐标 (55,22) 上的元素: ' + at + (at !== 'IMG' ? '  ✓ 不是 img 了' : '  ⚠⚠ 还是 img'));
    if (at === 'IMG') process.exitCode = 2;

    console.log('');
    console.log('=== ② 表格自适应列宽了吗（不同分组同一列宽度应当不同）===');
    const per = JSON.parse(await ev(`(function(){
      var out = [];
      var secs = document.querySelectorAll('section.acctgroup');
      for (var i = 0; i < secs.length; i++) {
        var s = secs[i];
        var grips = s.querySelectorAll('thead th .colgrip');
        var ths = s.querySelectorAll('thead th');
        var cols = s.querySelectorAll('colgroup > col');
        var w = {}, overridden = [];
        for (var j = 0; j < grips.length; j++) {
          var k = grips[j].dataset.colgrip;
          w[k] = Math.round(ths[j].getBoundingClientRect().width);
          if (cols[j] && cols[j].style.width) overridden.push(k);
        }
        var t = s.querySelector('table');
        out.push({ pid: s.dataset.acctgroup, n: s.querySelectorAll('tbody tr').length,
          w: w, overridden: overridden, minW: parseInt(t.style.minWidth) || 0,
          tableW: Math.round(t.getBoundingClientRect().width),
          layout: getComputedStyle(t).tableLayout });
      }
      return JSON.stringify(out);
    })()`));
    console.log('  table-layout: ' + per[0].layout + (per[0].layout === 'auto' ? '  ✓ 自适应' : '  ⚠⚠ 还是 fixed'));
    if (per[0].layout !== 'auto') process.exitCode = 2;
    console.log('  写死宽度的列（应为空，因为没人拖过）: '
      + per.map(p => p.pid + '={' + p.overridden.join(',') + '}').join(' '));
    const anyFixed = per.some(p => p.overridden.length > 0);
    if (anyFixed) { console.log('  ⚠⚠ 还有固定宽'); process.exitCode = 2; }
    console.log('  min-width: ' + per.map(p => p.pid + '=' + p.minW).join('  '));

    console.log('');
    console.log('  ── 同一列在各分组里的宽度（自适应 ⇒ 应当各不相同）──');
    const colSets = [['provider','昵称'],['uid','UID'],['quota','额度'],['status','状态'],['ops','操作']];
    let diffCount = 0;
    colSets.forEach(([key, label]) => {
      const vals = per.filter(p => p.w[key] !== undefined).map(p => ({ pid: p.pid, w: p.w[key] }));
      const uniq = Array.from(new Set(vals.map(v => v.w)));
      const isDiff = uniq.length > 1;
      if (isDiff) diffCount++;
      console.log('    ' + label + '(' + key + '): ' + vals.map(v => v.pid + '=' + v.w).join('  ')
        + '   ⇒ 不同值 ' + uniq.length + ' 个' + (isDiff ? '  ✓ 按各自内容算' : '  （恰好相同）'));
    });
    console.log('  ' + (diffCount >= 3 ? '✓ **多数列在各分组里宽度不同 ⇒ 自适应成立**'
      : '⚠⚠ 只有 ' + diffCount + ' 列不同，看起来还在用统一宽度'));
    if (diffCount < 3) process.exitCode = 2;

    // 对比一下旧行为：以前 UID 列被钉在同一个宽度
    const uidVals = per.filter(p => p.w.uid !== undefined).map(p => p.w.uid);
    console.log('  UID 列宽度 ' + Math.min.apply(null, uidVals) + '..' + Math.max.apply(null, uidVals)
      + '  （改前所有分组都是同一个固定值）');

    console.log('');
    console.log('=== ③ 列宽拖拽仍然有效 ===');
    const g = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var grip = s.querySelector('.colgrip[data-colgrip="provider"]');
      grip.scrollIntoView({ block: 'nearest' });
      var q = grip.getBoundingClientRect();
      return JSON.stringify({ x: Math.round(q.left+q.width/2), y: Math.round(q.top+q.height/2),
        before: Math.round(s.querySelector('thead th').getBoundingClientRect().width) });
    })()`));
    await send('Input.dispatchMouseEvent', { type:'mousePressed', x:g.x, y:g.y, button:'left', clickCount:1, pointerType:'mouse' });
    for (let i=1;i<=4;i++){
      await send('Input.dispatchMouseEvent', { type:'mouseMoved', x:g.x+Math.round(90*i/4), y:g.y, button:'left', buttons:1, pointerType:'mouse' });
      await sleep(70);
    }
    await send('Input.dispatchMouseEvent', { type:'mouseReleased', x:g.x+90, y:g.y, button:'left', clickCount:1, pointerType:'mouse' });
    await sleep(900);
    const drag = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var t = s.querySelector('table');
      var cols = s.querySelectorAll('colgroup > col');
      return JSON.stringify({ provider: Math.round(s.querySelector('thead th').getBoundingClientRect().width),
        colW: cols[0] ? (cols[0].style.width || '(auto)') : '?',
        saved: localStorage.getItem('wb2api.acctcolw.workbuddy') });
    })()`));
    console.log('  provider: ' + g.before + ' → ' + drag.provider + '   col width=' + drag.colW);
    console.log('  存盘: ' + drag.saved);
    const okDrag = drag.provider - g.before > 40 && drag.colW !== '(auto)';
    console.log('  ' + (okDrag ? '✓ 拖拽在 auto 布局下同样生效' : '⚠⚠ 拖不动了'));
    if (!okDrag) process.exitCode = 2;

    console.log('');
    console.log('=== ④ 双击还原 ⇒ 该列回到自适应 ===');
    await ev(`(function(){ var g=document.querySelectorAll('section.acctgroup')[1].querySelector('.colgrip[data-colgrip="provider"]'); g.scrollIntoView({block:'nearest'}); return 1; })()`);
    const g2 = JSON.parse(await ev(`(function(){
      var g = document.querySelectorAll('section.acctgroup')[1].querySelector('.colgrip[data-colgrip="provider"]');
      var q = g.getBoundingClientRect();
      return JSON.stringify({x:Math.round(q.left+q.width/2), y:Math.round(q.top+q.height/2)});
    })()`));
    for (let i=0;i<2;i++){
      await send('Input.dispatchMouseEvent', { type:'mousePressed', x:g2.x, y:g2.y, button:'left', clickCount:2, pointerType:'mouse' });
      await send('Input.dispatchMouseEvent', { type:'mouseReleased', x:g2.x, y:g2.y, button:'left', clickCount:2, pointerType:'mouse' });
      await sleep(80);
    }
    await sleep(900);
    const undo = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var cols = s.querySelectorAll('colgroup > col');
      return JSON.stringify({ colW: cols[0] ? (cols[0].style.width || '(auto)') : '?',
        provider: Math.round(s.querySelector('thead th').getBoundingClientRect().width),
        saved: localStorage.getItem('wb2api.acctcolw.workbuddy') });
    })()`));
    console.log('  provider = ' + undo.provider + '   col width = ' + undo.colW + '   存盘 ' + undo.saved);
    const okUndo = undo.colW === '(auto)';
    console.log('  ' + (okUndo ? '✓ 回到自适应（col 不再写宽度）' : '⚠⚠ 还钉着'));
    if (!okUndo) process.exitCode = 2;

    console.log('');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) process.exitCode = 2;
    await shot('fit-1-adaptive.png');
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
