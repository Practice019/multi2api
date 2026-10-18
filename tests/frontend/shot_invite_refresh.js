const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9361, url: 'http://127.0.0.1:18080/ui', width: 2160, height: 1230,
    readyExpr: `document.querySelectorAll('#nav button').length`, outDir: 'shots' }, async ({ ev, errs, shot, send }) => {
    // ⚠ 邀请码 = 上游专属面板，key 是 loomy:invite。
    //   第一版我按 #nav button 里的文字找，但那一项在**动态生成的 LOOMY 分组**下
    //   （实测侧栏 i=13 text=邀请码 nav=11），且必须让面板真正显示出来再量按钮坐标 ——
    //   隐藏面板里 getBoundingClientRect() 恒为 [0,0,0,0]，点了就是在打空气。
    //   用 lib_cdp 的 showPanel（内部走 window.__wb2api__.showPanelByKey）。
    await showPanel(ev, 'loomy:invite', 2800);

    // patch fetch，之后只统计新请求
    await ev(`(function(){
      window.__reqs = [];
      if (!window.__fetchPatched) {
        window.__fetchPatched = 1;
        var of = window.fetch;
        window.fetch = function(a, b){
          try { window.__reqs.push(typeof a === 'string' ? a : (a && a.url) || '?'); } catch(e){}
          return of.apply(this, arguments);
        };
        var ox = XMLHttpRequest.prototype.open;
        XMLHttpRequest.prototype.open = function(m, u){
          try { window.__reqs.push('XHR ' + u); } catch(e){}
          return ox.apply(this, arguments);
        };
      }
      return 1;
    })()`);

    console.log('');
    console.log('=== ① 邀请码面板在不在 ===');
    const panel = JSON.parse(await ev(`(function(){
      var secs = Array.prototype.slice.call(document.querySelectorAll('#content > section'));
      var hit = secs.filter(function(s){
        return (s.dataset.key||'').indexOf('invite') >= 0 || /邀请码/.test((s.querySelector('h2')||{}).textContent||'');
      });
      if (!hit.length) return JSON.stringify({ err: '没有邀请码面板', allKeys: secs.map(function(s){ return s.dataset.key; }) });
      var s = hit[0];
      var btn = s.querySelector('button[data-plist]');
      var rows = s.querySelectorAll('tbody tr');
      var firstUid = rows.length ? (rows[0].querySelector('td')||{}).textContent : null;
      var r = btn ? btn.getBoundingClientRect() : null;
      return JSON.stringify({ key: s.dataset.key, active: s.classList.contains('active'),
        btnText: btn ? btn.textContent.trim() : null,
        btnPlist: btn ? btn.dataset.plist : null, btnPcap: btn ? btn.dataset.pcap : null,
        btnRect: r ? [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)] : null,
        rowCount: rows.length, firstUid: firstUid ? firstUid.trim().slice(0,20) : null,
        hasPager: !!s.querySelector('[data-lpager]') });
    })()`));
    if (panel.err) { console.log('  ⚠ ' + panel.err); console.log('  已有 section: ' + JSON.stringify(panel.allKeys)); process.exitCode = 2; }
    else {
      console.log('  面板 key=' + panel.key + ' active=' + panel.active);
      console.log('  刷新按钮: 「' + panel.btnText + '」 data-plist=' + panel.btnPlist + ' data-pcap=' + panel.btnPcap);
      console.log('  rect=[' + panel.btnRect.join(',') + ']');
      console.log('  表格: ' + panel.rowCount + ' 行，首行 UID=' + panel.firstUid + '  分页器=' + panel.hasPager);

      console.log('');
      console.log('=== ② 点「刷新」，看有没有真的重发请求（核心）===');
      await ev(`window.__reqs = []`);
      // **真实指针点击**（不用 .click()，避免合成事件走不到委托）
      await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: panel.btnRect[0] + Math.round(panel.btnRect[2]/2), y: panel.btnRect[1] + Math.round(panel.btnRect[3]/2), button: 'left', clickCount: 1, pointerType: 'mouse' });
      await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: panel.btnRect[0] + Math.round(panel.btnRect[2]/2), y: panel.btnRect[1] + Math.round(panel.btnRect[3]/2), button: 'left', clickCount: 1, pointerType: 'mouse' });
      await sleep(2600);

      const reqs = JSON.parse(await ev(`JSON.stringify(window.__reqs || [])`));
      const inviteReqs = reqs.filter(u => String(u).indexOf('invite') >= 0);
      console.log('  点击后发出的请求 ' + reqs.length + ' 条:');
      reqs.slice(0, 8).forEach(u => console.log('    ' + String(u).slice(0, 110)));
      console.log('  ' + (inviteReqs.length
        ? '✓✓ **确实重发了 ' + inviteReqs.length + ' 条 invite 请求** —— 按钮是活的'
        : '⚠⚠ **一条 invite 请求都没有 —— 它还是死按钮**'));
      if (!inviteReqs.length) process.exitCode = 2;

      console.log('');
      console.log('=== ③ 表格有没有重绘（重新拉到了数据）===');
      const after = JSON.parse(await ev(`(function(){
        var secs = Array.prototype.slice.call(document.querySelectorAll('#content > section'));
        var s = secs.filter(function(x){ return (x.dataset.key||'').indexOf('invite') >= 0; })[0];
        if (!s) return JSON.stringify({ err: '面板没了' });
        var rows = s.querySelectorAll('tbody tr');
        var hint = s.querySelector('.dim');
        var firstUid = rows.length ? (rows[0].querySelector('td')||{}).textContent : null;
        return JSON.stringify({ rowCount: rows.length,
          firstUid: firstUid ? firstUid.trim().slice(0,20) : null,
          skeleton: !!s.querySelector('.dim') && /加载中/.test((s.querySelector('.dim')||{}).textContent||'') });
      })()`));
      console.log('  重绘后: ' + after.rowCount + ' 行，首行 UID=' + after.firstUid + '  loading中=' + after.skeleton);
      const ok = !after.skeleton && after.rowCount > 0;
      console.log('  ' + (ok ? '✓ **表格有数据、不是加载中**' : '⚠⚠ 还停在加载中/空'));
      if (!ok) process.exitCode = 2;

      console.log('');
      console.log('=== ④ 连点两次（确认不是"只能点一次"）===');
      await ev(`window.__reqs = []`);
      for (let k = 0; k < 2; k++) {
        await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: panel.btnRect[0] + 30, y: panel.btnRect[1] + 12, button: 'left', clickCount: 1, pointerType: 'mouse' });
        await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: panel.btnRect[0] + 30, y: panel.btnRect[1] + 12, button: 'left', clickCount: 1, pointerType: 'mouse' });
        await sleep(1400);
      }
      const reqs2 = JSON.parse(await ev(`JSON.stringify(window.__reqs || [])`)).filter(u => String(u).indexOf('invite') >= 0);
      console.log('  第二次点击后 invite 请求 ' + reqs2.length + ' 条  '
        + (reqs2.length >= 2 ? '✓ 每次点击都重发（force 生效）' : '⚠⚠ 只有第一次有效'));
      if (reqs2.length < 2) process.exitCode = 2;
      await shot('invite-refresh.png');
    }

    console.log('');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();

