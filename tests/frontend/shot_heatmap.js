// shot_heatmap.js —— 给「活跃热力图」拍图 + 取运行时诊断（2026-10-19）。
//
// # 为什么要单独一个
//
// 这次改的是**三件只有肉眼能判对错**的事：
//   1. 图**默认滚到最右（最新）** —— 断言只能查"scrollLeft 等于多少"，
//      而"该不该是这样"要看图：左边半年是空白、右边才是有数据的日子。
//   2. **悬停浮层** —— 它的正确性一半在"内容对不对"、一半在"位置压没压到手"。
//   3. **右侧统计卡** —— 三个数字必须能一句话解释（活跃天数 / 峰值单日 / 最近提交）。
//
// 所以本脚本既截三张图，也把 DOM 里的关键事实打出来 ——
// 图给"好不好看"，数字给"对不对"（参考项目 `shot_accounts.js` 的同一套理由）。
//
// # 复用
//
// Chrome 定位、CDP 握手、截图那段与 `shot_accounts.js` 同源（都走 `chrome_path.js`）；
// ⚠ 调试端口用 9331 而不是它的 9329，为的是**两个脚本可以同时跑**而不互踢。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9331;
const TARGET = process.env.HEAT_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-heat-' + Date.now());
const OUT = path.resolve(__dirname, '..', '..', 'shots');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function get(u, h) {
  return new Promise((res, rej) => {
    http.get(u, { headers: h || {} }, (r) => {
      const c = [];
      r.on('data', (x) => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res(Buffer.concat(c).toString('utf8')));
    }).on('error', rej);
  });
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { }
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run',
    '--no-default-browser-check', '--remote-debugging-port=' + PORT,
    '--user-data-dir=' + PROFILE, '--window-size=1600,1200', 'about:blank'],
    { stdio: 'ignore' });

  let ws = null;
  let exitCode = 0;
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');
    ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0;
    const pend = new Map();
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); }
    };
    const send = (mm, p) => new Promise((r) => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    // ⚠⚠ **轮询等就绪，不要死等**（2026-10-19 踩到）：
    //   `loadHeatmap()` 要等 `/admin/stats/series?range=365d`（366 个桶）回来。
    //   实例**冷启动**时这个请求慢得多，我第一次固定等 9 秒，截出来是一块空白 ——
    //   而同一份代码在服务热了之后再跑就是 371 个格子。
    //   ⇒ 判据用"格阵里有子节点"，而不是"过了多少毫秒"。
    let waitedMs = 0, ready = 0;
    for (let i = 0; i < 60; i++) {
      ready = await ev(`(function(){var h=document.getElementById('statsHeat'); return h?h.children.length:0;})()`) || 0;
      if (ready > 100) break;
      await sleep(500);
      waitedMs += 500;
    }
    console.log('  热力图就绪判据: 格阵子节点=' + ready + '（等待 ' + waitedMs + 'ms）');
    if (ready <= 100) { console.log('  ⚠⚠ 20 秒内没画出格子 —— 后面所有结论都不可信'); exitCode = 3; }
    await sleep(600);                            // 再给 rAF 的滚动定位一点时间

    async function shot(name) {
      const r = await send('Page.captureScreenshot', { format: 'png', captureBeyondViewport: true });
      const b = Buffer.from(r.result.data, 'base64');
      fs.writeFileSync(path.join(OUT, name), b);
      console.log('  已保存 ' + path.join(OUT, name) + '（' + b.length + ' 字节）');
    }

    console.log('=== 诊断：热力图 ===');
    const diag = await ev(`(function(){
      var host = document.getElementById('statsHeat');
      if (!host) return JSON.stringify({err:'#statsHeat 不存在'});
      var sc = host.parentElement;
      var cells = host.querySelectorAll('.heatcell[data-day]');
      var blanks = host.querySelectorAll('.heatcell.blank');
      var cols = Math.ceil((cells.length + blanks.length) / 7);
      var lvs = {};
      for (var i=0;i<cells.length;i++){
        var m = (cells[i].className.match(/lv([0-4])/) || [])[1] || '?';
        lvs[m] = (lvs[m]||0)+1;
      }
      var st = document.getElementById('heatStats');
      var stats = [];
      if (st) { var ks = st.querySelectorAll('.hs'); for (var j=0;j<ks.length;j++) stats.push(ks[j].querySelector('.k').textContent + '=' + ks[j].querySelector('.v').textContent); }
      return JSON.stringify({
        cols: cols,
        withData: cells.length,
        blanks: blanks.length,
        levelHistogram: lvs,
        scroll: { left: Math.round(sc.scrollLeft), max: Math.round(sc.scrollWidth - sc.clientWidth), sw: sc.scrollWidth, cw: sc.clientWidth,
                  ovx: getComputedStyle(sc).overflowX, gw: getComputedStyle(host).width, gsw: host.scrollWidth },
        months: (document.getElementById('statsHeatMonths')||{}).children ? document.getElementById('statsHeatMonths').children.length : 0,
        stats: stats,
        firstDay: cells.length ? cells[0].getAttribute('data-day') : null,
        lastDay: cells.length ? cells[cells.length-1].getAttribute('data-day') : null,
        align: (function(){
          var d = document.querySelector('.heatdays');
          return d ? { daysH: Math.round(d.getBoundingClientRect().height), gridH: Math.round(host.getBoundingClientRect().height), daysMT: d.style.marginTop } : {err:'no .heatdays'};
        })(),
        legend: (function(){
          var c = document.querySelector('.heatfoot .heatcell');
          if (!c) return 'no legend cell';
          var r = c.getBoundingClientRect();
          return Math.round(r.width) + 'x' + Math.round(r.height);
        })(),
        colors: (function(){
          var out = [];
          ['lv0','lv1','lv2','lv3','lv4'].forEach(function(k){
            var e = document.querySelector('#statsHeat .heatcell.' + k);
            if (e) out.push(k + '=' + getComputedStyle(e).backgroundColor);
          });
          return out.join(' ');
        })()
      });
    })()`);
    const D = JSON.parse(diag);
    console.log('  列数=' + D.cols + ' · 有 data-day 的格=' + D.withData + ' · blank=' + D.blanks);
    console.log('  首格=' + D.firstDay + ' · 末格=' + D.lastDay + ' · 月份标签数=' + D.months);
    console.log('  **档位直方图 = ' + JSON.stringify(D.levelHistogram) + '**');
    console.log('  **滚动 = left ' + D.scroll.left + ' / max ' + D.scroll.max + '**（两者相等才叫"默认停在最新"）');
    console.log('     scrollWidth=' + D.scroll.sw + ' · clientWidth=' + D.scroll.cw
      + ' · overflowX=' + D.scroll.ovx + ' · gridWidth=' + D.scroll.gw + ' · gridScrollW=' + D.scroll.gsw);
    console.log('  右侧统计卡 = ' + JSON.stringify(D.stats));
    // ⚠ 周标签对齐：判据是"标签列的总高 == 格阵总高"（两边 gap 都是 2px，
    //   总高一致 ⇒ 逐行对齐）。用户反馈过"位置不对"，所以这条要显式打出来。
    console.log('  **周标签对齐 = 标签高 ' + D.align.daysH + ' / 格阵高 ' + D.align.gridH
      + ' · marginTop=' + D.align.daysMT + '（两者相等才叫对齐）**');
    console.log('  图例色块实测宽高 = ' + D.legend + '（0 宽 = 用户说的"没有示例图标"）');

    // ① 整页
    await shot('heat-1-full.png');

    // ② 热力图特写：滚到它并让视口对齐
    await ev(`(function(){var h=document.querySelector('.heatwrap'); if(h) h.scrollIntoView({block:'center'});})()`);
    await sleep(700);
    await shot('heat-2-closeup.png');

    // ③ 悬停浮层：把一个格子的中心坐标喂给 Input.dispatchMouseEvent
    console.log('=== 诊断：悬停浮层 ===');
    const box = JSON.parse(await ev(`(function(){
      var c = document.querySelectorAll('#statsHeat .heatcell[data-day]');
      if (!c.length) return JSON.stringify({err:'没有格子'});
      var t = c[c.length-3];                       // 倒数第 3 个：必然在视口内且是最近有数据的日子附近
      var r = t.getBoundingClientRect();
      return JSON.stringify({ x: Math.round(r.left + r.width/2), y: Math.round(r.top + r.height/2), day: t.getAttribute('data-day') });
    })()`));
    if (!box.err) {
      await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: box.x, y: box.y, buttons: 0 });
      await sleep(500);
      const tip = await ev(`(function(){
        var el = document.getElementById('hoverTip');
        if (!el) return JSON.stringify({err:'#hoverTip 不存在'});
        return JSON.stringify({ display: getComputedStyle(el).display, text: el.textContent, x: el.style.left, y: el.style.top,
          rect: (function(){var r=el.getBoundingClientRect(); return {l:Math.round(r.left),t:Math.round(r.top),w:Math.round(r.width),h:Math.round(r.height)};})() });
      })()`);
      console.log('  悬停到 ' + box.day + ' @(' + box.x + ',' + box.y + ')');
      console.log('  **浮层 = ' + tip + '**');
      await shot('heat-3-hover.png');
      // ⚠ 位置守卫：浮层不能被视口裁掉（参考项目那边专门做了 clamp）
      const T = JSON.parse(tip);
      if (T.rect && (T.rect.l < 0 || T.rect.t < 0 || T.rect.l + T.rect.w > 1600 || T.rect.t + T.rect.h > 1200)) {
        console.log('  ⚠⚠ 浮层超出视口 —— clamp 有问题');
        exitCode = 2;
      }
    } else {
      console.log('  ⚠ ' + box.err);
      exitCode = 2;
    }

    // ④ 点击常显读数（这条是原有的，回归确认没被浮层改动带坏）
    if (!box.err) {
      await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: box.x, y: box.y, button: 'left', clickCount: 1 });
      await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: box.x, y: box.y, button: 'left', clickCount: 1 });
      await sleep(400);
      const pick = await ev(`(function(){var o=document.getElementById('heatPick'); return o?o.textContent:'';})()`);
      console.log('=== 诊断：点击常显读数 ===');
      console.log('  ' + pick);
      await shot('heat-4-picked.png');
    }

    console.log('=== 结论：' + (exitCode === 0 ? '通过' : '有问题（见上面的 ⚠⚠）') + ' ===');
  } catch (e) {
    console.error('失败: ' + (e && e.message ? e.message : e));
    exitCode = 1;
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { ch.kill(); } catch { }
    await sleep(300);
  }
  process.exit(exitCode);
})();
