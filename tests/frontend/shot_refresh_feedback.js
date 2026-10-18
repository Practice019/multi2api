// 通用面板「刷新」按钮的**可见反馈**验收（2026-10-19）
//
// 背景：用户两次说"那个刷新按钮还是没用"。实测过 —— 按钮是活的，
// 点它确实会发 `/admin/loomy/invite?page=1`。那"没用"是什么意思？
// 看面板自己的状态文字就明白了：
//   「共 44 个账号 · 已读 25 · 已绑定 25」「还有 0 项待完成」
// 数据**已经是终态**，再刷也是同一份；而按钮点击前后长得一模一样，
// 用户无从判断它干没干活。所以「没用」= **没有可见的反馈**。
//
// 这个脚本验的就是"反馈"这件事，三项：
//   ① 点击期间按钮切到「刷新中…」且 disabled（进行时措辞）
//   ② **反馈有一个最短可见时长**：服务端只要 10~14ms，若只是 await 完就复原，
//      那个措辞会在下一帧前被换掉、肉眼看不见 —— 所以下限 380ms
//   ③ 完成后 toast 给出带时刻与耗时的结果
//
// ⚠ 两条我自己踩的时序坑（都在下面标了）：
//   · `click` 是在 **press + release 之后**才派发的 —— 事件还没派发时读按钮，
//     读到的永远是原始态
//   · 所以不能靠"某个时刻读一次"来验时长，要**量端点之间的总耗时**
const { withChrome, showPanel, sleep } = require('./lib_cdp.js');

(async () => {
  await withChrome({ port: 9366, url: 'http://127.0.0.1:18080/ui', width: 1600, height: 1000,
    readyExpr: `document.querySelectorAll('#nav button').length`, outDir: 'shots' },
    async ({ ev, errs, shot, send }) => {
    await sleep(2400);

    // 记账请求
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
      await showPanel(ev, KEY, 2800);

      const info = JSON.parse(await ev(`(function(){
        var s = document.querySelector('section[data-key=' + JSON.stringify('${KEY}') + ']');
        var b = s.querySelector('button[data-plist]');
        var r = b.getBoundingClientRect();
        return JSON.stringify({ label: b.textContent.trim(), disabled: b.disabled,
          rect: [Math.round(r.left), Math.round(r.top), Math.round(r.width), Math.round(r.height)],
          rows: s.querySelectorAll('tbody tr').length,
          updated: (function(){
            var m = (s.textContent||'').match(/更新于[^\\d]*(\\d{1,2}:\\d{2}:\\d{2})/);
            return m ? m[1] : null;
          })() });
      })()`));
      console.log('  点前: 「' + info.label + '」 disabled=' + info.disabled
        + ' 表格 ' + info.rows + ' 行 · 面板写着「更新于 ' + info.updated + '」');

      await ev(`window.__reqs = []`);
      const cx = info.rect[0] + Math.round(info.rect[2] / 2);
      const cy = info.rect[1] + Math.round(info.rect[3] / 2);

      // ⚠ 关键：把「读按钮状态」用页面内的 rAF 循环挂上，而不是从外面 sleep 后读一次 ——
      //   因为 click 的派发时机不好卡。这里让页面自己记录"按钮出现过哪些文案"。
      await ev(`(function(){
        window.__btnLog = [];
        var s = document.querySelector('section[data-key=' + JSON.stringify('${KEY}') + ']');
        var b = s.querySelector('button[data-plist]');
        var t0 = performance.now();
        window.__btnT0 = t0;
        var stop = false;
        window.__btnStop = function(){ stop = true; };
        (function tick(){
          if (stop) return;
          try { window.__btnLog.push([Math.round(performance.now() - t0), b.textContent.trim(), b.disabled]); } catch(e){}
          requestAnimationFrame(tick);
        })();
        return 1;
      })()`);

      await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: cx, y: cy, pointerType: 'mouse' });
      await sleep(100);
      const t0 = Date.now();
      await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: cx, y: cy, button: 'left', clickCount: 1, pointerType: 'mouse' });
      await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: cx, y: cy, button: 'left', clickCount: 1, pointerType: 'mouse' });

      // 等按钮复原（轮询，别死等）
      let restoredAtMs = null;
      for (let i = 0; i < 60; i++) {
        await sleep(50);
        const st = JSON.parse(await ev(`(function(){
          var s = document.querySelector('section[data-key=' + JSON.stringify('${KEY}') + ']');
          var b = s.querySelector('button[data-plist]');
          return JSON.stringify({ label: b.textContent.trim(), disabled: b.disabled });
        })()`));
        if (!st.disabled && st.label === '刷新') { restoredAtMs = Date.now() - t0; break; }
      }
      await sleep(300);

      // ⚠⚠ 提取要抓**全部帧里真正变化的那几次**，不能 slice 前 N 帧。
      //   我第一版写了 slice(0,8)，而 rAF 循环是在点击**之前**就启动的
      //   （点击要等 sleep(100) + 事件往返），于是那 8 帧全在点击之前，
      //   一条「刷新中…」都看不到 —— 白白怀疑了两轮"节点被重建"。
      //   去重也不能写成 last/last2 各自引用（第一个分支里 last2 还没定义）。
      const log = JSON.parse(await ev(`(function(){
        window.__btnStop();
        var out = [], prev = null;
        (window.__btnLog || []).forEach(function(e){
          var k = e[1] + '|' + e[2];
          if (k === prev) return;
          prev = k;
          out.push(e);
        });
        return JSON.stringify({ frames: (window.__btnLog || []).length, changes: out });
      })()`));
      await ev(`window.__btnStop && window.__btnStop()`);

      const after = JSON.parse(await ev(`(function(){
        var s = document.querySelector('section[data-key=' + JSON.stringify('${KEY}') + ']');
        var b = s.querySelector('button[data-plist]');
        var t = document.getElementById('toast');
        var m = (s.textContent||'').match(/更新于[^\\d]*(\\d{1,2}:\\d{2}:\\d{2})/);
        return JSON.stringify({ label: b.textContent.trim(), disabled: b.disabled,
          updated: m ? m[1] : null,
          toastHidden: t ? t.hidden : null, toastText: t ? t.textContent : null });
      })()`));
      const reqs = JSON.parse(await ev(`JSON.stringify(window.__reqs || [])`)).filter(u => String(u).indexOf(cap) >= 0);

      // ── 断言 ──
      console.log('  rAF 共 ' + log.frames + ' 帧，文案变化: ' + JSON.stringify(log.changes));
      const sawBusy = (log.changes || []).some(e => String(e[1]).indexOf('刷新中') >= 0 && e[2] === true);
      console.log('  ' + (sawBusy ? '✓ **点击期间出现过「刷新中…」+ disabled**'
        : '⚠⚠ 从没出现过进行时措辞'));

      console.log('  从点击到按钮复原: ' + (restoredAtMs === null ? '超时未复原' : restoredAtMs + 'ms'));
      const minOK = restoredAtMs !== null && restoredAtMs >= 380;
      console.log('  ' + (minOK ? '✓ **≥380ms 的最短反馈生效**（服务端只要 10~14ms，否则根本看不见）'
        : '⚠⚠ 少于 380ms —— 反馈会一闪而过'));

      console.log('  请求: ' + (reqs.length ? reqs.map(u => String(u).slice(0, 50)).join(' | ') : '(无)'));
      console.log('  「更新于」: ' + info.updated + ' → ' + after.updated
        + (after.updated && after.updated !== info.updated ? '  ✓ 变了' : '  （相同 —— 这个上游数据是终态）'));
      console.log('  toast: hidden=' + after.toastHidden + ' ' + JSON.stringify(after.toastText));
      const toastOK = !after.toastHidden && after.toastText && /已刷新/.test(after.toastText);
      console.log('  ' + (toastOK ? '✓ **完成后给出带时刻与耗时的结果**' : '⚠⚠ 没有 toast'));

      if (!sawBusy || !minOK || !reqs.length || !toastOK) process.exitCode = 2;
      await shot('feedback-' + cap + '.png');
      console.log('');
    }

    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) process.exitCode = 2;
  }).catch(e => { console.error('失败: ' + e.message); process.exit(1); });
  process.exit(process.exitCode || 0);
})();

