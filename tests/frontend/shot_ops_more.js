const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9342, url: 'http://127.0.0.1:18080/ui',
    readyExpr: `document.querySelectorAll('.acctgroup').length` }, async ({ ev, errs, shot }) => {
    await showPanel(ev, 'accounts', 2500);
    console.log('=== 操作列：3 直显 + ⋯ 收纳（真的点开）===');

    const pre = JSON.parse(await ev(`(function(){
      var tds = document.querySelectorAll('.acctgroup table.accttable--aligned tbody td');
      var opTd = null;
      var rows = document.querySelectorAll('.acctgroup table.accttable--aligned tbody tr');
      // 找到"操作"列（按 row 自己那组的表头索引）
      var over = 0, total = 0, badges = {};
      for (var r = 0; r < rows.length; r++) {
        var tb = rows[r].closest('table');
        var ths = [].slice.call(tb.querySelectorAll('thead th'));
        var ci = -1;
        for (var i = 0; i < ths.length; i++) if (ths[i].textContent.trim() === '操作') { ci = i; break; }
        if (ci < 0) continue;
        var t = rows[r].querySelectorAll('td')[ci];
        if (!t) continue;
        total++;
        if (t.scrollWidth > t.clientWidth + 1) over++;
        var n = t.querySelectorAll(':scope > button').length;
        badges[n] = (badges[n] || 0) + 1;
      }
      var more = document.querySelectorAll('.ops-more').length;
      var menus = document.querySelectorAll('.ops-menu').length;
      var firstMenu = document.querySelector('.ops-menu');
      return JSON.stringify({ total: total, over: over, directBtns: badges,
        moreBtns: more, menus: menus,
        menuClosed: firstMenu ? !firstMenu.matches(':popover-open') : null,
        popoverSupported: typeof HTMLElement.prototype.showPopover === 'function' });
    })()`));
    console.log('  数据行 ' + pre.total + ' · 溢出 ' + pre.over + ' 行'
      + ' · 直显按钮数分布 ' + JSON.stringify(pre.directBtns));
    console.log('  ⋯ 按钮 ' + pre.moreBtns + ' 个 · popover 面板 ' + pre.menus + ' 个'
      + ' · 浏览器支持 popover = ' + pre.popoverSupported);
    if (pre.menuClosed !== null) console.log('  点之前：面板是关的 = ' + pre.menuClosed);

    // ⚠⚠ 真的点开它（经验："交互类改动要真的触发交互后读回状态断言"）
    //
    // ⚠ 而且必须**点完等一下再读**：`toggle` 事件是**异步派发**的（进任务队列），
    //   在同一次 evaluate 里"点完立刻 getBoundingClientRect"读到的还是 UA 默认位置
    //   —— 第一版就是这么写的，得到 [0,0,102,39] 并差点误判"定位代码没生效"。
    const clicked = JSON.parse(await ev(`(function(){
      var b = document.querySelector('.ops-more');
      if (!b) return JSON.stringify({err:'没有 ⋯ 按钮'});
      b.click();
      return JSON.stringify({ id: b.getAttribute('popovertarget') });
    })()`));
    await sleep(400);   // ← 让 toggle 事件跑完
    const opened = JSON.parse(await ev(`(function(){
      var m = document.getElementById(${JSON.stringify(clicked.id)});
      if (!m) return JSON.stringify({err:'面板不存在'});
      var r = m.getBoundingClientRect();
      var b = document.querySelector('[popovertarget="' + CSS.escape(${JSON.stringify(clicked.id)}) + '"]');
      var br = b.getBoundingClientRect();
      return JSON.stringify({
        id: m.id, exists: true,
        open: m.matches(':popover-open'),
        btnCount: m.querySelectorAll('button').length,
        labels: [].slice.call(m.querySelectorAll('button')).map(function(x){return x.textContent.trim();}),
        rect: [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)],
        btnRect: [Math.round(br.left), Math.round(br.top), Math.round(br.right), Math.round(br.bottom)],
        inlineLeft: m.style.left || '(未设)', inlineTop: m.style.top || '(未设)'
      });
    })()`));
    console.log('');
    console.log('  ⚡ 点了第一个 ⋯（target=' + opened.id + '）：');
    console.log('    面板存在 = ' + opened.exists + ' · **是打开状态 = ' + opened.open + '**');
    console.log('    面板内按钮 ' + opened.btnCount + ' 个: ' + JSON.stringify(opened.labels));
    console.log('    面板位置/尺寸 = ' + JSON.stringify(opened.rect) + ' · inline left/top = '
      + opened.inlineLeft + ' / ' + opened.inlineTop);
    console.log('    触发按钮位置 = ' + JSON.stringify(opened.btnRect));
    // 定位是否真的生效：面板应当在按钮下方（top ≈ 按钮 bottom）且水平与按钮右缘对齐
    const [pl, pt, pw, ph] = opened.rect;
    const [, , brRight, brBottom] = opened.btnRect;
    const nearBtn = Math.abs((pl + pw) - brRight) <= 4 && (Math.abs(pt - (brBottom + 4)) <= 4);
    console.log('    ⇒ 贴着触发按钮 = ' + (nearBtn ? '是 ✓' : '否 ⚠⚠')
      + '（面板右缘 ' + (pl + pw) + ' vs 按钮右缘 ' + brRight
      + ' · 面板顶 ' + pt + ' vs 按钮底+4 ' + (brBottom + 4) + '）');
    if (!nearBtn) process.exitCode = 2;
    await sleep(300);
    await shot('acct-5-popover.png');
    // 再点一次应当关掉
    const closed = await ev(`(function(){var b=document.querySelector('.ops-more'); b.click();
      var m=document.getElementById(b.getAttribute('popovertarget'));
      return m.matches(':popover-open');})()`);
    console.log('    再点一次后 = ' + (closed === false ? '已关闭 ✓' : '仍开着 ⚠'));
    if (opened.open !== true) { console.log('  ⚠⚠ popover 没打开'); process.exitCode = 2; }
    if (opened.btnCount < 1) { console.log('  ⚠⚠ 面板里没有按钮'); process.exitCode = 2; }
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
