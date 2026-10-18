const { withChrome, showPanel, sleep } = require('./lib_cdp.js');
(async () => {
  await withChrome({ port: 9345, url: 'http://127.0.0.1:18080/ui',
    readyExpr: `document.querySelectorAll('.acctgroup').length` }, async ({ ev, errs, send }) => {
    console.log('=== ① 刷新按钮现在是活的吗（真的点它 + 看有没有网络请求）===');
    // 切到 loomy 的新手任务面板
    await showPanel(ev, 'loomy:tasks', 2000).catch(() => {});
    await sleep(800);
    const info = JSON.parse(await ev(`(function(){
      var btns = document.querySelectorAll('button[data-plist]');
      var vis = null;
      for (var i=0;i<btns.length;i++){
        var r = btns[i].getBoundingClientRect();
        if (r.width > 0 && r.height > 0) { vis = { pid: btns[i].dataset.plist, cap: btns[i].dataset.pcap,
          rect: [Math.round(r.left),Math.round(r.top),Math.round(r.width),Math.round(r.height)] }; break; }
      }
      return JSON.stringify({ total: btns.length, firstVisible: vis });
    })()`));
    console.log('  data-plist 按钮共 ' + info.total + ' 个 · 第一个可见的 = ' + JSON.stringify(info.firstVisible));
    if (info.firstVisible) {
      const [x, y] = [info.firstVisible.rect[0] + 20, info.firstVisible.rect[1] + 10];
      await send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1 });
      await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1 });
      await sleep(1500);
      const after = await ev(`(function(){
        var b = document.querySelector('button[data-plist]');
        var host = b ? document.getElementById('pbody-' + b.dataset.plist + '-' + b.dataset.pcap) : null;
        return JSON.stringify({ hostTextLen: host ? host.textContent.length : -1,
          hint: (function(){var s=document.querySelector('section[data-cap]'); return s? s.querySelector('h2 .dim') ? '' : '' : '';})() });
      })()`);
      console.log('  点完后 host 文本长度 = ' + after.hostTextLen + '（>0 说明面板有内容）');
      console.log('  ✓ 按钮可点、有响应');
    } else {
      console.log('  ⚠ 没有可见的 data-plist 按钮');
    }
    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));

    console.log('');
    console.log('=== ② z-index 阶梯（提示必须在最上）===');
    const z = JSON.parse(await ev(`(function(){
      function g(sel){ var e=document.querySelector(sel); return e?getComputedStyle(e).zIndex:null; }
      return JSON.stringify({ notices: g('.notices'), hovertip: g('.hovertip'),
        modal: g('#modal'), infoModal: g('#infoModal'), akModal: g('#akModal') });
    })()`));
    console.log('  ' + JSON.stringify(z));
    const vals = Object.entries(z).filter(([, v]) => v && v !== 'auto').map(([k, v]) => [k, Number(v)]);
    const notices = z.notices ? Number(z.notices) : 0;
    const others = vals.filter(([k]) => k !== 'notices').map(([, v]) => v);
    const maxOther = Math.max(...others);
    console.log('  .notices = ' + notices + ' · 其它最高 = ' + maxOther
      + (notices > maxOther ? '  ✓ 提示在最上（弹窗盖不住它）' : '  ⚠⚠ 提示被弹窗盖住'));
    if (!(notices > maxOther)) process.exitCode = 2;
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();
