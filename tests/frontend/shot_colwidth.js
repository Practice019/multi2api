const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9353, url: 'http://127.0.0.1:18080/ui', width: 1600, height: 1000,
    readyExpr: `document.querySelectorAll('.acctgroup').length`, outDir: 'shots' }, async ({ ev, errs, shot, send }) => {
    await showPanel(ev, 'accounts', 2500);
    await ev(`(function(){ try{ Object.keys(localStorage).forEach(function(k){ if(k.indexOf('wb2api.acctcol')===0) localStorage.removeItem(k); }); }catch(e){} return 1; })()`);
    await ev(`location.reload()`); await sleep(3200);
    await showPanel(ev, 'accounts', 2500); await sleep(1500);

    const snap = async (gi) => JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[${gi}];
      if (!s) return JSON.stringify({err:'组没了'});
      var grips = s.querySelectorAll('thead th .colgrip');
      var ths = s.querySelectorAll('thead th');
      var cols = s.querySelectorAll('colgroup > col');
      var w = {}, fixed = [];
      for (var i=0;i<grips.length;i++){
        var k = grips[i].dataset.colgrip;
        w[k] = Math.round(ths[i].getBoundingClientRect().width);
        // 有内联 width 的 = 固定列；弹性列的 <col> 没有 width
        if (cols[i] && cols[i].style.width) fixed.push(k);
      }
      var t = s.querySelector('table');
      return JSON.stringify({ order: Array.prototype.map.call(grips,function(g){return g.dataset.colgrip;}),
        w: w, fixed: fixed, minW: parseInt(t.style.minWidth)||0,
        tableW: Math.round(t.getBoundingClientRect().width), gripCount: grips.length, thCount: ths.length });
    })()`));

    const a = await snap(1);
    console.log('=== ① 把手就位 ===');
    console.log('  把手 ' + a.gripCount + ' / 表头 ' + a.thCount + (a.gripCount===a.thCount?' ✓':' ⚠⚠ 不等'));
    if (a.gripCount !== a.thCount) process.exitCode = 2;
    console.log('  固定列: ' + a.fixed.join(',') + '   弹性列: ' + a.order.filter(k=>a.fixed.indexOf(k)<0).join(','));
    console.log('  各列宽: ' + a.order.map(k=>k+'='+a.w[k]).join('  '));
    console.log('  表宽 ' + a.tableW + ' · min-width ' + a.minW);

    const drag = async (key, dx) => {
      const g = JSON.parse(await ev(`(function(){
        var s = document.querySelectorAll('section.acctgroup')[1];
        var grip = s.querySelector('.colgrip[data-colgrip="${key}"]');
        if (!grip) return JSON.stringify({err:'没有把手'});
        grip.scrollIntoView({block:'nearest'});
        var q = grip.getBoundingClientRect();
        return JSON.stringify({ x: Math.round(q.left+q.width/2), y: Math.round(q.top+q.height/2) });
      })()`));
      if (g.err) return g;
      await send('Input.dispatchMouseEvent', { type:'mousePressed', x:g.x, y:g.y, button:'left', clickCount:1, pointerType:'mouse' });
      for (let i=1;i<=5;i++){
        await send('Input.dispatchMouseEvent', { type:'mouseMoved', x:g.x+Math.round(dx*i/5), y:g.y, button:'left', buttons:1, pointerType:'mouse' });
        await sleep(70);
      }
      await send('Input.dispatchMouseEvent', { type:'mouseReleased', x:g.x+dx, y:g.y, button:'left', clickCount:1, pointerType:'mouse' });
      await sleep(800);
      return { ok: 1 };
    };

    console.log('');
    console.log('=== ② 拖「上游」+80px：**固定列**必须纹丝不动 ===');
    await drag('provider', 80);
    const b = await snap(1);
    console.log('  拖动后: ' + b.order.map(k=>k+'='+b.w[k]).join('  '));
    console.log('  provider: ' + a.w.provider + ' → ' + b.w.provider + ' (+' + (b.w.provider-a.w.provider) + ')');
    console.log('  min-width: ' + a.minW + ' → ' + b.minW);
    const flex = a.order.filter(k => a.fixed.indexOf(k) < 0);
    const moved = a.order.filter(k => k !== 'provider' && a.fixed.indexOf(k) >= 0 && a.w[k] !== b.w[k]);
    console.log('  ' + (moved.length === 0
      ? '✓✓ **除被拖那列外，其余 ' + (a.fixed.length - 1) + ' 个固定列宽度未变**（绝对量模型成立）'
      : '⚠⚠ 固定列被牵连: ' + moved.map(k=>k+' '+a.w[k]+'→'+b.w[k]).join(', ')));
    if (moved.length) process.exitCode = 2;
    console.log('  弹性列 ' + flex.join(',') + ' 让位 ' + (a.w[flex[0]] - b.w[flex[0]]) + 'px'
      + '  ' + (Math.abs((b.w.provider-a.w.provider) + (b.w[flex[0]]-a.w[flex[0]])) <= 2
        ? '✓（加起来的净变化 ≈ 0，正是"表宽不变、弹性列吃剩余"）' : ''));
    console.log('  ' + (b.w.provider - a.w.provider > 40 ? '✓ 被拖的列真的宽了' : '⚠⚠ 拖不动'));
    if (b.w.provider - a.w.provider <= 40) process.exitCode = 2;
    await shot('colw-1-dragged.png');

    console.log('');
    console.log('=== ③ 松手写盘 ===');
    console.log('  localStorage: ' + (await ev(`(function(){ try{ return localStorage.getItem('wb2api.acctcolw.workbuddy') || '(空)'; }catch(e){ return 'err'; } })()`)));

    console.log('');
    console.log('=== ④ 重载后保持 ===');
    await ev(`location.reload()`); await sleep(3200);
    await showPanel(ev, 'accounts', 2500); await sleep(1500);
    const c = await snap(1);
    const kept = c.w.provider === b.w.provider;
    console.log('  provider: ' + b.w.provider + ' → ' + c.w.provider + (kept?'  ✓ 记住了':'  ⚠⚠ 丢了'));
    if (!kept) process.exitCode = 2;

    console.log('');
    console.log('=== ⑤ 双击还原 ===');
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
    const d = await snap(1);
    console.log('  provider: ' + c.w.provider + ' → ' + d.w.provider + '（默认 ' + a.w.provider + '）');
    console.log('  存盘: ' + (await ev(`(function(){ try{ var v=localStorage.getItem('wb2api.acctcolw.workbuddy'); return v||'(空)'; }catch(e){ return 'err'; } })()`)));
    const ok5 = Math.abs(d.w.provider - a.w.provider) <= 2;
    console.log('  ' + (ok5 ? '✓ **回到默认宽度，没有漂移**' : '⚠⚠ 没回到默认'));
    if (!ok5) process.exitCode = 2;

    console.log('');
    console.log('=== ⑥ 拖到超过表宽 ⇒ 该出现横向滚动 ===');
    await drag('provider', 700);
    const e2 = await snap(1);
    console.log('  provider: ' + d.w.provider + ' → ' + e2.w.provider);
    console.log('  min-width ' + e2.minW + ' vs 表宽 ' + e2.tableW
      + '  ⇒ ' + (e2.minW > e2.tableW ? '✓ min-width 顶住了，表格横向溢出（该滚就滚）' : '（min-width 仍小于表宽，未到溢出点）'));
    // ⚠ 溢出要量**父容器**：表格自己是 width:auto，会跟着内容一起长，
    //   所以表自己的 scrollWidth === clientWidth 恒成立（第一版就是这么误判的）。
    const sc = JSON.parse(await ev(`(function(){
      var t = document.querySelectorAll('section.acctgroup')[1].querySelector('table');
      var p = t.parentElement;
      return JSON.stringify({ tableW: Math.round(t.getBoundingClientRect().width),
        parentSW: p.scrollWidth, parentCW: p.clientWidth,
        parentOver: getComputedStyle(p).overflowX });
    })()`));
    console.log('  表宽 ' + sc.tableW + ' · 父容器 scrollWidth ' + sc.parentSW + ' / clientWidth ' + sc.parentCW
      + ' · overflow-x=' + sc.parentOver);
    console.log('  ' + (sc.parentSW > sc.parentCW + 1
      ? '✓ **父容器确实横向溢出了 ' + (sc.parentSW - sc.parentCW) + 'px**（该滚就滚，没有压缩列宽）'
      : '⚠⚠ 父容器没溢出 —— 列宽可能被压缩了'));
    if (sc.parentSW <= sc.parentCW + 1) process.exitCode = 2;
    await shot('colw-2-wide.png');

    console.log('');
    console.log('=== ⑦ 列设置仍工作（两套机制不冲突）===');
    const colset = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var b = s.querySelector('button.colset-btn');
      if (!b) return JSON.stringify({err:'没有列设置按钮'});
      b.click();
      return JSON.stringify({ok:1});
    })()`));
    await sleep(500);
    const opened = await ev(`document.querySelectorAll('.colset:popover-open').length`);
    console.log('  列设置浮层打开 ' + opened + ' 个' + (opened ? ' ✓ 两套机制共存' : ' ⚠⚠'));
    if (!opened) process.exitCode = 2;
    if (colset.err) { console.log('  ⚠ ' + colset.err); process.exitCode = 2; }

    console.log('');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) process.exitCode = 2;
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();

