const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
const P = 9350, URL = 'http://127.0.0.1:18080/ui';
(async () => {
  await withChrome({ port: P, url: URL, width: 1600, height: 1000,
    readyExpr: `document.querySelectorAll('.acctgroup').length`, outDir: 'shots' }, async ({ ev, errs, shot, send }) => {
    await showPanel(ev, 'accounts', 2500);

    // 先摸一个干净的起点（清掉本机可能残留的偏好）
    await ev(`(function(){ try{ Object.keys(localStorage).forEach(function(k){ if(k.indexOf('wb2api.acctcols.')===0) localStorage.removeItem(k); }); }catch(e){} return 1; })()`);
    await ev(`loadAccounts && loadAccounts()`);
    await sleep(2500);

    const base = JSON.parse(await ev(`(function(){
      var btns = document.querySelectorAll('button[data-colstoggle]');
      var panels = document.querySelectorAll('.colset');
      var gs = document.querySelectorAll('.acctgroup');
      var colsetBtns = document.querySelectorAll('button.colset-btn');
      return JSON.stringify({ groups: gs.length, colsetBtns: colsetBtns.length,
        panels: panels.length, toggles: btns.length });
    })()`));
    console.log('=== ① 控件就位 ===');
    console.log('  分组 ' + base.groups + ' 个 · 「列设置」按钮 ' + base.colsetBtns + ' 个 · 浮层 ' + base.panels + ' 个 · 开关 ' + base.toggles + ' 个');
    if (base.colsetBtns !== base.groups) { console.log('  ⚠⚠ 按钮数与分组数不等'); process.exitCode = 2; }

    // 选 workbuddy 那组（列最多）
    const pick = JSON.parse(await ev(`(function(){
      var secs = document.querySelectorAll('section.acctgroup');
      var best = null;
      for (var i=0;i<secs.length;i++){
        var t = secs[i].querySelector('table.accttable thead tr');
        var n = t ? t.querySelectorAll('th').length : 0;
        if (n && (!best || n > best.n)) best = { i: i, n: n, pid: (secs[i].querySelector('button[data-greload]')||{}).dataset ? secs[i].querySelector('button[data-greload]').dataset.greload : '?' };
      }
      return JSON.stringify(best);
    })()`));
    console.log('');
    console.log('=== ② 挑列最多的那一组 ===');
    console.log('  第 ' + pick.i + ' 组（' + pick.pid + '），表头 ' + pick.n + ' 列');

    const measure = async (label) => JSON.parse(await ev(`(function(){
      var secs = document.querySelectorAll('section.acctgroup');
      var s = secs[${pick.i}];
      if (!s) return JSON.stringify({err:'组没了'});
      var ths = s.querySelectorAll('table.accttable thead tr th');
      var cols = s.querySelectorAll('table.accttable colgroup col');
      var firstRow = s.querySelector('table.accttable tbody tr');
      var tds = firstRow ? firstRow.querySelectorAll('td') : [];
      return JSON.stringify({
        ths: ths.length, cols: cols.length, tds: tds.length,
        thText: Array.prototype.map.call(ths, function(h){ return h.textContent.trim().slice(0,6); }),
        colW: Array.prototype.map.call(cols, function(c){ return c.style.width || c.getAttribute('width') || ''; })
      });
    })()`));

    const m0 = await measure('基线');
    console.log('  基线：th=' + m0.ths + ' col=' + m0.cols + ' td=' + m0.tds);
    console.log('    表头内容: ' + JSON.stringify(m0.thText));

    // ---- ③ 关掉中间某一列 ----
    console.log('');
    console.log('=== ③ 关掉一列（表头/单元格/colgroup 必须同时 -1）===');
    const off = JSON.parse(await ev(`(function(){
      var secs = document.querySelectorAll('section.acctgroup');
      var s = secs[${pick.i}];
      var b = s.querySelector('button.colset-btn');
      if (!b) return JSON.stringify({err:'没有列设置按钮'});
      b.click();
      return JSON.stringify({ ok: 1 });
    })()`));
    if (off.err) { console.log('  ⚠ ' + off.err); process.exitCode = 2; }
    await sleep(500);
    const opened = JSON.parse(await ev(`(function(){
      var p = document.querySelectorAll('.colset:popover-open');
      if (!p.length) return JSON.stringify({open:0});
      var rows = p[0].querySelectorAll('.colset-row');
      var names = Array.prototype.map.call(rows, function(r){ return r.querySelector('.colset-name').textContent.trim(); });
      return JSON.stringify({ open: p.length, rows: rows.length, names: names });
    })()`));
    console.log('  浮层打开 ' + opened.open + ' 个 · ' + opened.rows + ' 行');
    console.log('    列名: ' + JSON.stringify(opened.names));
    if (!opened.open) { console.log('  ⚠⚠ 浮层没打开'); process.exitCode = 2; }

    // 关掉"第 3 行"那一列（真点它的 checkbox）
    const tgt = JSON.parse(await ev(`(function(){
      var p = document.querySelector('.colset:popover-open');
      var rows = p.querySelectorAll('.colset-row');
      var r = rows[2];
      var cb = r.querySelector('input[data-colstoggle]');
      cb.scrollIntoView({block:'nearest'});
      var q = cb.getBoundingClientRect();
      return JSON.stringify({ key: r.dataset.colsrow, name: r.querySelector('.colset-name').textContent.trim(),
        x: Math.round(q.left+q.width/2), y: Math.round(q.top+q.height/2) });
    })()`));
    console.log('  要关掉: ' + tgt.name + '（key=' + tgt.key + '）');
    await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: tgt.x, y: tgt.y, button: 'left', clickCount: 1 });
    await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: tgt.x, y: tgt.y, button: 'left', clickCount: 1 });
    await sleep(2500);
    const m1 = await measure('关一列后');
    console.log('  关掉后：th=' + m1.ths + ' col=' + m1.cols + ' td=' + m1.tds);
    console.log('    表头内容: ' + JSON.stringify(m1.thText));
    const ok3 = (m1.ths === m0.ths - 1) && (m1.cols === m0.cols - 1) && (m1.tds === m0.tds - 1);
    console.log('  ' + (ok3 ? '✓ **三者同时 -1**' : '⚠⚠ 三者没有同时 -1（th ' + m0.ths + '→' + m1.ths + ', col ' + m0.cols + '→' + m1.cols + ', td ' + m0.tds + '→' + m1.tds + '）'));
    if (!ok3) process.exitCode = 2;
    if (m1.thText.indexOf(tgt.name) >= 0) { console.log('  ⚠⚠ 表头里还有那一列'); process.exitCode = 2; }

    // ---- ④ 刷新后保持 ----
    console.log('');
    console.log('=== ④ 刷页面之后还在吗（localStorage 持久化）===');
    const stored = await ev(`(function(){ var k=null; try{ Object.keys(localStorage).forEach(function(x){ if(x.indexOf('wb2api.acctcols.')===0) k=x+' = '+localStorage.getItem(x); }); }catch(e){} return k||'(空)'; })()`);
    console.log('  localStorage: ' + stored);
    await ev(`location.reload()`);
    await sleep(3000);
    await showPanel(ev, 'accounts', 2500).catch(()=>{});
    await sleep(1500);
    const m2 = await measure('重载后');
    console.log('  重载后：th=' + m2.ths + ' col=' + m2.cols + ' td=' + m2.tds);
    const ok4 = (m2.ths === m1.ths) && (m2.cols === m1.cols) && (m2.tds === m1.tds);
    console.log('  ' + (ok4 ? '✓ 偏好被记住了（th/col/td 都还是 ' + m1.ths + '）' : '⚠⚠ 重载后列数变了（偏好没存住）'));
    if (!ok4) process.exitCode = 2;

    // ---- ⑤ 最后一列关不掉 ----
    console.log('');
    console.log('=== ⑤ 关到只剩一列时，最后一列的开关应被禁用 ===');
    const last = JSON.parse(await ev(`(function(){
      var secs = document.querySelectorAll('section.acctgroup');
      var s = secs[${pick.i}];
      s.querySelector('button.colset-btn').click();
      return JSON.stringify({ok:1});
    })()`));
    await sleep(400);
    const dis = JSON.parse(await ev(`(function(){
      var p = document.querySelector('.colset:popover-open');
      if(!p) return JSON.stringify({err:'没打开'});
      var cbs = p.querySelectorAll('input[data-colstoggle]');
      var on = 0, dis = 0;
      Array.prototype.forEach.call(cbs, function(c){ if(c.checked) on++; if(c.disabled) dis++; });
      return JSON.stringify({ total: cbs.length, checked: on, disabled: dis });
    })()`));
    console.log('  ' + JSON.stringify(dis));
    const ok5 = dis.checked > 1 ? dis.disabled === 0 : (dis.disabled === 1);
    console.log('  ' + (ok5 ? '✓ 禁用规则正确（只剩一列时那一列 disabled）' : '⚠ 禁用规则不对'));
    if (!ok5) process.exitCode = 2;

    await shot('colset-1-open.png');
    console.log('');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();

