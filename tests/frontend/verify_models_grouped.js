// verify_models_grouped.js —— T5+T6 验证：模型面板去重、按上游分组、可折叠、倍率。
//
// # 判据全部用**渲染结果**，不读源码/常量
//
// 本项目第 10 次事故就是"读代码推断 ≠ 实际渲染"（我差点因为查了全角 ×
// 而把正常的倍率功能当坏的）。所以这里一律用 DOM。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9306;
const TARGET = process.env.MODELS_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-models');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(5500);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

    // 切到可用模型
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('可用模型') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2500);

    // ---- 1) 去重 ----
    //
    // 「原始条数」要**真的去问 API**，不能用 `window.__wb2api__.models()`
    // —— 那个函数没有暴露（我第一版就是这么写的，于是 raw=null，
    // 断言退化成 `16 < null` 恒 false，报了个假失败）。
    // 判据必须是可对照的真数据：原始目录 vs 渲染出的 chip 数。
    const raw = JSON.parse(await ev(`(function(){
      return new Promise(function(resolve){
        var req = new XMLHttpRequest();
        req.open('GET', '/v1/models', true);
        req.setRequestHeader('Authorization', 'Bearer ' + (window.__WB2API_KEY__ || ''));
        req.onload = function(){
          try { resolve(JSON.stringify((JSON.parse(req.responseText).data || []).map(function(m){ return m.id; }))); }
          catch (e) { resolve('null'); }
        };
        req.onerror = function(){ resolve('null'); };
        req.send();
      });
    })()`));

    const snap = JSON.parse(await ev(`JSON.stringify((function(){
      var chips = Array.from(document.querySelectorAll('#models .chip'));
      var ids = chips.map(function(c){ return c.dataset.id; });
      var names = chips.map(function(c){ return (c.textContent||'').trim(); });
      var groups = Array.from(document.querySelectorAll('#models .mgroup')).map(function(g){
        return {
          owner: g.dataset.owner,
          collapsed: g.classList.contains('collapsed'),
          headText: (g.querySelector('.mgrouphead')||{}).textContent ? g.querySelector('.mgrouphead').textContent.replace(/\\s+/g,' ').trim() : '',
          chips: g.querySelectorAll('.chip').length,
          visible: (g.querySelector('.chips')||{}).offsetParent !== null,
        };
      });
      return { chipCount: chips.length, ids: ids, names: names, groups: groups };
    })())`));

    const rawIds = raw === null ? [] : raw;
    const rawDedup = new Set(rawIds.map(x => {
      const s = x.indexOf('/');
      return s > 0 ? x.slice(s + 1) : x;
    }));

    console.log('  分组: ' + JSON.stringify(snap.groups.map(g => g.owner + '(' + g.chips + ')')));
    console.log('  chip 总数: ' + snap.chipCount + '；原始目录 ' + rawIds.length + ' 条；去重后应为 ' + rawDedup.size);
    console.log('  前 6 个: ' + JSON.stringify(snap.names.slice(0, 6)));

    ok(snap.groups.length >= 1, '至少有一个上游分组（' + snap.groups.length + '）');
    ok(rawIds.length > 0, '拿到了原始目录条数（' + rawIds.length + '）');
    ok(snap.chipCount === rawDedup.size,
      'chip 数（' + snap.chipCount + '）== 去重后的模型数（' + rawDedup.size + '），'
      + '原始目录 ' + rawIds.length + ' 条 → 说明按前缀去重生效');

    // 每个分组内不得有重名
    const dup = await ev(`(function(){
      var bad = [];
      document.querySelectorAll('#models .mgroup').forEach(function(g){
        var seen = {}, owner = g.dataset.owner;
        g.querySelectorAll('.chip').forEach(function(c){
          var n = (c.textContent||'').trim();
          if (seen[n]) bad.push(owner + ':' + n);
          seen[n] = 1;
        });
      });
      return JSON.stringify(bad);
    })()`);
    ok(JSON.parse(dup).length === 0, '组内无重名 chip' + (JSON.parse(dup).length ? ' —— ' + dup : ''));

    // 不应再有 `provider/xxx` 形态的 chip 名（前缀已去掉）
    const prefixed = snap.names.filter(n => n.indexOf('/') >= 0 && n.indexOf('x') !== 0);
    ok(prefixed.length === 0, 'chip 名里没有 `provider/xxx` 形态（前缀已去掉）' +
      (prefixed.length ? ' —— ' + JSON.stringify(prefixed.slice(0, 4)) : ''));

    // ---- 2) 分组标题带计数 ----
    const withCount = snap.groups.filter(g => /\d+\s*个模型/.test(g.headText));
    console.log('  分组标题: ' + JSON.stringify(snap.groups.map(g => g.headText)));
    ok(withCount.length === snap.groups.length,
      '每个分组标题都带模型数（' + withCount.length + '/' + snap.groups.length + '）');

    // ---- 3) 可折叠 ----
    const first = snap.groups[0];
    ok(first && !first.collapsed && first.visible, '默认展开（' + (first ? first.owner : '—') + '）');

    await ev(`(function(){
      var hd = document.querySelector('#models button[data-mtoggle]');
      if (hd) hd.click();
    })()`);
    await sleep(700);
    const afterCollapse = JSON.parse(await ev(`JSON.stringify((function(){
      var g = document.querySelector('#models .mgroup');
      var chips = g.querySelector('.chips');
      return {
        collapsed: g.classList.contains('collapsed'),
        chipsHidden: chips ? getComputedStyle(chips).display === 'none' : null,
        aria: (g.querySelector('.mgrouphead')||{}).getAttribute ? g.querySelector('.mgrouphead').getAttribute('aria-expanded') : null,
        ls: (function(){ try { return localStorage.getItem('wb2api.modelgroup.' + g.dataset.owner); } catch(e){ return 'ERR'; } })(),
      };
    })())`));
    console.log('  折叠后: ' + JSON.stringify(afterCollapse));
    ok(afterCollapse.collapsed, '点击分组头后加上了 collapsed 类');
    ok(afterCollapse.chipsHidden === true, 'chips 区域真的隐藏了（display:none）');
    ok(afterCollapse.aria === 'false', 'aria-expanded 同步为 false（可访问性）');
    ok(afterCollapse.ls === '0', '折叠状态已写入 localStorage（刷新后保持）');

    // 展开回来
    await ev(`document.querySelector('#models button[data-mtoggle]').click()`);
    await sleep(700);
    const afterExpand = JSON.parse(await ev(`JSON.stringify((function(){
      var g = document.querySelector('#models .mgroup');
      return { collapsed: g.classList.contains('collapsed'),
               ls: (function(){ try { return localStorage.getItem('wb2api.modelgroup.' + g.dataset.owner); } catch(e){ return 'ERR'; } })() };
    })())`));
    ok(!afterExpand.collapsed && afterExpand.ls === '1', '再点一次展开，状态也持久化');

    // ---- 4) 倍率 ----
    const mult = JSON.parse(await ev(`JSON.stringify((function(){
      var withMult = 0, unknown = 0, total = 0, samples = [];
      document.querySelectorAll('#models .chip').forEach(function(c){
        total++;
        var m = c.querySelector('.mult');
        if (m) { withMult++; if (m.classList.contains('unknown')) unknown++; }
        if (samples.length < 5) samples.push((c.textContent||'').trim());
      });
      return { total: total, withMult: withMult, unknown: unknown, samples: samples };
    })())`));
    console.log('  倍率: ' + JSON.stringify(mult));
    ok(mult.withMult === mult.total, '每个 chip 都有倍率标记（' + mult.withMult + '/' + mult.total + '）');
    ok(mult.unknown > 0, '有 ' + mult.unknown + ' 个显示 `x无`（倍率表里没有的 —— 用户要求）');

    // 不出现 undefined / NaN
    const junk = snap.names.filter(n => /undefined|NaN|null/.test(n));
    ok(junk.length === 0, 'chip 名里没有 undefined/NaN/null' + (junk.length ? ' —— ' + JSON.stringify(junk) : ''));

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== T5+T6 验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
