const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9351, url: 'http://127.0.0.1:18080/ui', width: 1600, height: 1000,
    readyExpr: `document.querySelectorAll('.acctgroup').length`, outDir: 'shots' }, async ({ ev, errs, shot, send }) => {
    await showPanel(ev, 'accounts', 2500);
    // 清干净起点
    await ev(`(function(){ try{ Object.keys(localStorage).forEach(function(k){ if(k.indexOf('wb2api.acctcols.')===0) localStorage.removeItem(k); }); }catch(e){} return 1; })()`);
    await ev(`(function(){ try{ return 1; }catch(e){} })()`);
    await ev(`location.reload()`); await sleep(3200);
    await showPanel(ev, 'accounts', 2500);
    await sleep(1500);

    const head = async () => JSON.parse(await ev(`(function(){
      var secs = document.querySelectorAll('section.acctgroup');
      var s = secs[1];
      var ths = s.querySelectorAll('table.accttable thead tr th');
      return JSON.stringify(Array.prototype.map.call(ths, function(h){ return h.textContent.trim().slice(0,5); }));
    })()`));

    const h0 = await head();
    console.log('=== ⑥ 拖动重排 ===');
    console.log('  拖动前表头: ' + JSON.stringify(h0));

    // 打开浮层
    await ev(`(function(){ document.querySelectorAll('section.acctgroup')[1].querySelector('button.colset-btn').click(); return 1; })()`);
    await sleep(500);

    // 量第 1 行的手柄与第 3 行的位置
    const g = JSON.parse(await ev(`(function(){
      var p = document.querySelector('.colset:popover-open');
      if (!p) return JSON.stringify({err:'浮层没开'});
      var rows = p.querySelectorAll('.colset-row');
      var g1 = rows[0].querySelector('.colset-grip');
      var r3 = rows[2];
      g1.scrollIntoView({block:'nearest'});
      var a = g1.getBoundingClientRect();
      var b = r3.getBoundingClientRect();
      return JSON.stringify({
        fromKey: rows[0].dataset.colsrow, toKey: r3.dataset.colsrow,
        from: [Math.round(a.left+a.width/2), Math.round(a.top+a.height/2)],
        to:   [Math.round(b.left+b.width/2), Math.round(b.top+b.height/2)],
        all: Array.prototype.map.call(rows, function(r){ return r.dataset.colsrow; })
      });
    })()`));
    if (g.err) { console.log('  ⚠ ' + g.err); process.exitCode = 2; }
    else {
      console.log('  把第 1 行（' + g.fromKey + '）拖到第 3 行（' + g.toKey + '）');
      await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: g.from[0], y: g.from[1], button: 'left', clickCount: 1, pointerType: 'mouse' });
      for (const f of [0.25, 0.5, 0.75, 1]) {
        await send('Input.dispatchMouseEvent', { type: 'mouseMoved',
          x: Math.round(g.from[0] + (g.to[0]-g.from[0])*f), y: Math.round(g.from[1] + (g.to[1]-g.from[1])*f),
          button: 'left', buttons: 1, pointerType: 'mouse' });
        await sleep(90);
      }
      await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: g.to[0], y: g.to[1], button: 'left', clickCount: 1, pointerType: 'mouse' });
      await sleep(2500);

      const stored = await ev(`(function(){ try{ var v=null; Object.keys(localStorage).forEach(function(k){ if(k.indexOf('wb2api.acctcols.')===0) v=localStorage.getItem(k); }); if(!v) return '(空)'; return JSON.parse(v).map(function(x){return x.k;}).join(','); }catch(e){ return 'err'; } })()`);
      console.log('  存盘顺序: ' + stored);
      const h1 = await head();
      console.log('  拖动后表头: ' + JSON.stringify(h1));
      const ok6 = (h1[2] === h0[0]) && (h1[0] === h0[1]);
      console.log('  ' + (ok6 ? '✓ **顺序真的变了，且与存盘一致**' : '⚠⚠ 顺序没变'));
      if (!ok6) process.exitCode = 2;
      await shot('colset-2-dragged.png');
    }

    // ---- ⑦ 恢复默认 ----
    console.log('');
    console.log('=== ⑦ 恢复默认 ===');
    const rb = JSON.parse(await ev(`(function(){
      var p = document.querySelector('.colset:popover-open');
      if (!p) { var s = document.querySelectorAll('section.acctgroup')[1]; s.querySelector('button.colset-btn').click(); }
      p = document.querySelector('.colset:popover-open');
      var b = p.querySelector('button[data-colsreset]');
      b.scrollIntoView({block:'nearest'});
      var q = b.getBoundingClientRect();
      return JSON.stringify({ pid: b.dataset.colsreset, x: Math.round(q.left+q.width/2), y: Math.round(q.top+q.height/2) });
    })()`));
    await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: rb.x, y: rb.y, button: 'left', clickCount: 1, pointerType: 'mouse' });
    await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: rb.x, y: rb.y, button: 'left', clickCount: 1, pointerType: 'mouse' });
    await sleep(2500);
    const after = await ev(`(function(){ try{ return localStorage.getItem('wb2api.acctcols.' + ${JSON.stringify('workbuddy')}) === null ? '(已清空)' : '(还在)'; }catch(e){ return 'err'; } })()`);
    const h2 = await head();
    console.log('  存盘: ' + after);
    console.log('  恢复后表头: ' + JSON.stringify(h2));
    const ok7 = (after === '(已清空)') && (h2.length === h0.length) && (h2.join() === h0.join());
    console.log('  ' + (ok7 ? '✓ **完全回到默认**' : '⚠⚠ 没回到默认'));
    if (!ok7) process.exitCode = 2;

    console.log('');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
