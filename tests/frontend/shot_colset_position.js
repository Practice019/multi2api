const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9360, url: 'http://127.0.0.1:18080/ui', width: 2160, height: 1230,
    readyExpr: `document.querySelectorAll('.acctgroup').length`, outDir: 'shots' }, async ({ ev, errs, shot, send }) => {
    await showPanel(ev, 'accounts', 2500);
    await sleep(1200);

    console.log('=== ① 所有 .colset 在关闭态的位置（原来它们都在 0,0）===');
    const closed = JSON.parse(await ev(`(function(){
      var out = [];
      document.querySelectorAll('.colset').forEach(function(p, i){
        var r = p.getBoundingClientRect();
        out.push({ i: i, id: p.id, display: getComputedStyle(p).display,
          rect: [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)] });
      });
      return JSON.stringify(out);
    })()`));
    console.log('  浮层 ' + closed.length + ' 个；示例 ' + JSON.stringify(closed[0]));
    console.log('  （关闭态的 rect 全是 0 是正常的 —— display:none）');

    console.log('');
    console.log('=== ② 打开列设置，看它落在哪（核心）===');
    const info = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var btn = s.querySelector('button.colset-btn');
      var r = btn.getBoundingClientRect();
      return JSON.stringify({ btnLeft: Math.round(r.left), btnRight: Math.round(r.right),
        btnTop: Math.round(r.top), btnBottom: Math.round(r.bottom),
        winW: window.innerWidth, winH: window.innerHeight, pid: s.dataset.acctgroup });
    })()`));
    console.log('  触发按钮 [' + info.pid + ']: left=' + info.btnLeft + ' right=' + info.btnRight
      + ' top=' + info.btnTop + ' bottom=' + info.btnBottom);
    await ev(`(function(){ document.querySelectorAll('section.acctgroup')[1].querySelector('button.colset-btn').click(); return 1; })()`);
    await sleep(600);

    const open = JSON.parse(await ev(`(function(){
      var p = document.querySelector('.colset:popover-open');
      if (!p) return JSON.stringify({ err: '没有打开的浮层' });
      var r = p.getBoundingClientRect();
      return JSON.stringify({ id: p.id,
        rect: [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)],
        left: p.style.left, top: p.style.top,
        inTopLeftCorner: (r.left < 130 && r.top < 130) });
    })()`));
    if (open.err) { console.log('  ⚠ ' + open.err); process.exitCode = 2; }
    else {
      console.log('  浮层 rect=[' + open.rect.join(',') + ']  style.left=' + open.left + ' top=' + open.top);
      // ⚠ 判据不能写成"左缘严格等于按钮左缘"：面板 336px 宽，靠右的按钮
      //   会让它出屏，此时**被夹住才是对的**（实测 left=1798=2142-336-8）。
      //   同理"垂直必须在按钮下方"也不对 —— 下方放不下就翻到上方（实测 top=523）。
      //   真正要断言的是：① 不再在左上角 ② 完整在视口内 ③ 与按钮的垂直距离
      //   不超过「下方」或「上方」这两种合理布局。
      const gapBelow = open.rect[1] - info.btnBottom;
      const gapAbove = info.btnTop - (open.rect[1] + open.rect[3]);
      console.log('  与按钮的垂直间隙: 下方 ' + gapBelow + ' / 上方 ' + gapAbove);
      const vertOK = (gapBelow >= -2 && gapBelow <= 16) || (gapAbove >= -2 && gapAbove <= 16);
      console.log('  ' + (vertOK ? '✓ 贴按钮（下方或翻到上方）' : '⚠⚠ 垂直位置既不在上方也不在下方'));
      if (!vertOK) process.exitCode = 2;
      const leftOK = open.rect[0] <= info.btnLeft + 3 && open.rect[0] >= info.btnLeft - 400;
      console.log('  ' + (leftOK ? '✓ 横向在按钮左侧附近（左对齐，必要时被防出屏夹住）'
        : '⚠⚠ 横向位置离谱'));
      if (!leftOK) process.exitCode = 2;
      console.log('  ' + (!open.inTopLeftCorner ? '✓✓ **不再落在左上角 (0,0)**' : '⚠⚠ 还在左上角'));
      if (open.inTopLeftCorner) process.exitCode = 2;
      const inView = open.rect[0] >= 0 && open.rect[0] + open.rect[2] <= info.winW + 1;
      console.log('  ' + (inView ? '✓ 完整在视口内（没有左右溢出）' : '⚠⚠ 有部分出屏'));
      if (!inView) process.exitCode = 2;
    }
    await shot('colset-positioned.png');

    console.log('');
    console.log('=== ③ 左上角区域现在干净了吗 ===');
    const corner = JSON.parse(await ev(`(function(){
      var hits = [];
      document.querySelectorAll('.colset, .ops-menu').forEach(function(p){
        var r = p.getBoundingClientRect();
        var st = getComputedStyle(p);
        if (st.display === 'none') return;
        if (r.left < 200 && r.top < 200) hits.push(p.id + ' rect=' + JSON.stringify([Math.round(r.left),Math.round(r.top),Math.round(r.width),Math.round(r.height)]));
      });
      return JSON.stringify(hits);
    })()`));
    console.log('  打开着的浮层里，落在左上角 (0..200,0..200) 的: '
      + (corner.length ? corner.join(' | ') : '**无** ✓✓'));
    if (corner.length) process.exitCode = 2;

    console.log('');
    console.log('=== ④ 关掉它，确认监听被摘掉 ===');
    await ev(`(function(){ document.querySelectorAll('section.acctgroup')[1].querySelector('button.colset-btn').click(); return 1; })()`);
    await sleep(500);
    const after = JSON.parse(await ev(`(function(){
      var p = document.querySelector('.colset:popover-open');
      var anyOpen = document.querySelectorAll('.colset:popover-open').length;
      var leftover = document.querySelectorAll('.colset').length;
      return JSON.stringify({ anyOpen: anyOpen, leftover: leftover });
    })()`));
    console.log('  打开的浮层 ' + after.anyOpen + ' 个，DOM 里还剩 ' + after.leftover + ' 个');
    if (after.anyOpen !== 0) process.exitCode = 2;

    console.log('');
    console.log('=== ⑤ 回归：⋯ 菜单的定位没被改坏 ===');
    const om = JSON.parse(await ev(`(function(){
      var s = document.querySelectorAll('section.acctgroup')[1];
      var b = s.querySelector('button.ops-more');
      if (!b) return JSON.stringify({ err: '这一组没有 ⋯ 按钮' });
      b.click();
      return JSON.stringify({ ok: 1 });
    })()`));
    await sleep(600);
    if (om.err) console.log('  （跳过：' + om.err + '）');
    else {
      const omo = JSON.parse(await ev(`(function(){
        var p = document.querySelector('.ops-menu:popover-open');
        if (!p) return JSON.stringify({ err: '没打开' });
        var r = p.getBoundingClientRect();
        return JSON.stringify({ rect: [Math.round(r.left),Math.round(r.top),Math.round(r.width),Math.round(r.height)],
          left: p.style.left, top: p.style.top });
      })()`));
      if (omo.err) console.log('  ⚠ ' + omo.err);
      else {
        console.log('  ⋯ 菜单 rect=[' + omo.rect.join(',') + '] style.left=' + omo.left);
        const okOps = omo.rect[0] > 200 && omo.rect[1] > 100;
        console.log('  ' + (okOps ? '✓ 仍在按钮附近（没退化到左上角）' : '⚠⚠ 位置不对'));
        if (!okOps) process.exitCode = 2;
      }
    }

    console.log('');
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) process.exitCode = 2;
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();

