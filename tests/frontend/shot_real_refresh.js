const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9369, url: 'http://127.0.0.1:18080/ui', width: 1600, height: 1000,
    readyExpr: `document.querySelectorAll('#nav button').length`, outDir: 'shots' }, async ({ ev, errs, shot, send }) => {
    await sleep(2400);
    await ev(`(function(){
      window.__reqs = [];
      if (!window.__fetchPatched) {
        window.__fetchPatched = 1;
        var of = window.fetch;
        window.fetch = function(a, b){
          try { window.__reqs.push(typeof a === 'string' ? a : (a && a.url) || '?'); } catch(e){}
          return of.apply(this, arguments);
        };
      }
      return 1;
    })()`);

    for (const KEY of ['loomy:invite', 'loomy:tasks']) {
      const cap = KEY.split(':')[1];
      console.log('=== ' + KEY + ' ===');
      await showPanel(ev, KEY, 4000);
      const info = JSON.parse(await ev(`(function(){
        var s = document.querySelector('section[data-key=' + JSON.stringify('${KEY}') + ']');
        var b = s.querySelector('button[data-plist]');
        var r = b.getBoundingClientRect();
        var blank = 0, rows = s.querySelectorAll('tbody tr');
        rows.forEach(function(tr){ if (/^\\s*—\\s*$/.test((tr.querySelector('td:nth-child(5)')||{}).textContent||'')) blank++; });
        return JSON.stringify({ rect: [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)],
          rows: rows.length, blankBalance: blank });
      })()`));
      console.log('  刷新前: ' + info.rows + ' 行，余额显示「—」的行数 = ' + info.blankBalance);

      await ev(`window.__reqs = []`);
      const cx = info.rect[0] + Math.round(info.rect[2] / 2), cy = info.rect[1] + Math.round(info.rect[3] / 2);
      await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: cx, y: cy, pointerType: 'mouse' });
      await sleep(100);
      const t0 = Date.now();
      await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: cx, y: cy, button: 'left', clickCount: 1, pointerType: 'mouse' });
      await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: cx, y: cy, button: 'left', clickCount: 1, pointerType: 'mouse' });

      // 等按钮复原（真查要十几秒）
      let restored = null;
      for (let i = 0; i < 200; i++) {
        await sleep(300);
        const st = JSON.parse(await ev(`(function(){
          var s = document.querySelector('section[data-key=' + JSON.stringify('${KEY}') + ']');
          var b = s.querySelector('button[data-plist]');
          return JSON.stringify({ label: b.textContent.trim(), disabled: b.disabled });
        })()`));
        if (!st.disabled && st.label === '刷新') { restored = Date.now() - t0; break; }
      }
      await sleep(600);

      const after = JSON.parse(await ev(`(function(){
        var s = document.querySelector('section[data-key=' + JSON.stringify('${KEY}') + ']');
        var rows = s.querySelectorAll('tbody tr');
        var blank = 0;
        rows.forEach(function(tr){ if (/^\\s*—\\s*$/.test((tr.querySelector('td:nth-child(5)')||{}).textContent||'')) blank++; });
        var t = document.getElementById('toast');
        var m = (s.textContent||'').match(/更新于[^\\d]*(\\d{1,2}:\\d{2}:\\d{2})/);
        return JSON.stringify({ rows: rows.length, blankBalance: blank,
          updated: m ? m[1] : null,
          toast: t ? t.textContent : null, toastHidden: t ? t.hidden : null });
      })()`));
      const reqs = JSON.parse(await ev(`JSON.stringify(window.__reqs || [])`));

      console.log('  耗时: ' + (restored === null ? '超时' : restored + 'ms') + '   （真查上游，慢是正常的）');
      console.log('  请求: ' + (reqs.length ? reqs.map(u => String(u).slice(0, 62)).join(' | ') : '(无)'));
      const hasRefresh = reqs.some(u => String(u).indexOf('refresh=1') >= 0);
      console.log('  ' + (hasRefresh ? '✓✓ **URL 上带了 refresh=1**' : '⚠⚠ URL 上没有 refresh=1 —— 拿到的还是缓存'));
      console.log('  刷新后: ' + after.rows + ' 行，余额「—」的行数 = ' + after.blankBalance
        + '   ' + (after.blankBalance < info.blankBalance ? '✓✓ **减少了**' : after.blankBalance === info.blankBalance ? '（没变）' : '⚠'));
      console.log('  「更新于」: ' + info.updated + ' → ' + after.updated);
      console.log('  toast: ' + JSON.stringify(after.toast));

      if (!hasRefresh) process.exitCode = 2;
      await shot('realrefresh-' + cap + '.png');
      console.log('');
    }

    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) process.exitCode = 2;
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
