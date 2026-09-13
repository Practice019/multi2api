// verify_model_provider_filter.js —— T7 浏览器验证：对话测试的上游筛选器。
//
// # 判据用**渲染结果 + 实际交互**
//
// 静态套件（models_display_gen.js 的 [7]）已验过纯函数逻辑；
// 这里验的是"页面上真的有这个控件、点它真的会变、选择真的持久化"。
//
// 关键区别（与 chip 区**故意不同**）：
//   · chip 区（可用模型面板）：去重
//   · 本处（对话测试下拉）：**不去重**，保留 provider/xxx 形态
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9307;
const TARGET = process.env.CHAT_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-t7');
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

    // 切到对话测试
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('对话测试') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2000);

    // ---- 1) 控件存在 ----
    const s0 = JSON.parse(await ev(`JSON.stringify((function(){
      var p = document.getElementById('modelProvider');
      var m = document.getElementById('model');
      return {
        hasProvider: !!p,
        providerOptions: p ? Array.from(p.options).map(function(o){ return o.value; }) : null,
        providerLabel: p ? Array.from(p.options).map(function(o){ return o.textContent; }) : null,
        currentModel: m ? m.value : null,
        modelCount: m ? m.options.length : 0,
        modelIds: m ? Array.from(m.options).map(function(o){ return o.value; }) : [],
        ls: (function(){ try { return localStorage.getItem('wb2api.modelprovider'); } catch(e){ return 'ERR'; } })(),
      };
    })())`));
    console.log('  上游选项: ' + JSON.stringify(s0.providerLabel));
    console.log('  模型下拉: ' + s0.modelCount + ' 项；当前=' + JSON.stringify(s0.currentModel));
    ok(s0.hasProvider, '对话测试里有 #modelProvider 下拉');

    // ---- 2) 「全部」时不去重：应包含 provider/xxx 形态 ----
    const prefixed = s0.modelIds.filter(x => x.indexOf('/') >= 0);
    console.log('  含前缀的模型 id: ' + prefixed.length + ' 个（前 3: ' + JSON.stringify(prefixed.slice(0, 3)) + '）');
    ok(s0.modelCount > 0, '模型下拉已填充（' + s0.modelCount + ' 项）');
    ok(prefixed.length > 0,
      '「全部上游」时下拉**不去重** —— 保留了 ' + prefixed.length + ' 个 provider/xxx 形态（用户要求）');

    // ---- 3) 切上游 → 列表变短且只含该上游 ----
    const owners = s0.providerOptions.filter(v => v);
    console.log('  可选上游: ' + JSON.stringify(owners));
    ok(owners.length > 0, '上游下拉列出了实际出现的上游（' + owners.length + ' 个）');

    if (owners.length > 0) {
      const target = owners[0];
      await ev(`(function(){
        var p = document.getElementById('modelProvider');
        p.value = ${JSON.stringify(target)};
        p.dispatchEvent(new Event('change', { bubbles: true }));
      })()`);
      await sleep(800);
      const s1 = JSON.parse(await ev(`JSON.stringify((function(){
        var m = document.getElementById('model');
        var ids = Array.from(m.options).map(function(o){ return o.value; });
        return { count: m.options.length, ids: ids.slice(0, 6),
                 current: m.value,
                 ls: (function(){ try { return localStorage.getItem('wb2api.modelprovider'); } catch(e){ return 'ERR'; } })() };
      })())`));
      console.log('  选 ' + target + ' 后: ' + s1.count + ' 项；当前=' + JSON.stringify(s1.current));
      // ⚠ 「变短」只在**有多个上游**时才成立。
      //
      // 我第一版无条件断言 count < s0.modelCount，于是单上游部署下
      // 报了个假失败（选唯一的上游 == 选全部，32→32 是**正确**的）。
      // 判据改成按数据推导：
      //   · 多个上游 → 变短
      //   · 只有一个上游 → 与"全部"相同（并明确断言这一点，而不是跳过）
      const expectShrink = owners.length > 1;
      if (expectShrink) {
        ok(s1.count > 0 && s1.count < s0.modelCount,
          '切到 ' + target + ' 后列表变短（' + s0.modelCount + ' → ' + s1.count + '）');
      } else {
        ok(s1.count === s0.modelCount,
          '只有 1 个上游时，选它 == 选全部（' + s1.count + ' == ' + s0.modelCount
          + '）—— 这是正确行为，不是筛选失效');
      }

      const wrong = JSON.parse(await ev(`JSON.stringify((function(){
        var m = document.getElementById('model');
        return Array.from(m.options).map(function(o){ return o.value; })
          .filter(function(v){
            var s = v.indexOf('/');
            var owner = s > 0 ? v.slice(0, s) : '';
            // 裸名无从判断归属，不算越界：只有"前缀是别家"才算
            return s > 0 && owner !== ${JSON.stringify(target)};
          });
      })())`));
      ok(wrong.length === 0, '筛选后不含别家上游的模型' + (wrong.length ? ' —— 越界: ' + JSON.stringify(wrong) : ''));

      // 当前值必须在选项里（否则界面显示空但内层 value 还是旧的 → 发的和看的不一致）
      ok(s1.ids.indexOf(s1.current) >= 0 || s1.current === '',
        '切换后当前值指向一个真实存在的选项（actual=' + JSON.stringify(s1.current) + '）');

      ok(s1.ls === target, '选择已写入 localStorage（' + JSON.stringify(s1.ls) + '）');

      // ---- 4) 刷新后保持 ----
      await send('Page.navigate', { url: TARGET });
      await sleep(5500);
      await ev(`(function(){
        var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
          .filter(function(x){ return x.textContent.indexOf('对话测试') >= 0; })[0];
        if (b) b.click();
      })()`);
      await sleep(2000);
      const s2 = JSON.parse(await ev(`JSON.stringify((function(){
        var p = document.getElementById('modelProvider');
        return { value: p ? p.value : null,
                 inOptions: p ? Array.from(p.options).map(function(o){ return o.value; }).indexOf(p.value) >= 0 : false };
      })())`));
      console.log('  刷新后: ' + JSON.stringify(s2));
      ok(s2.value === target && s2.inOptions,
        '刷新后仍选中 ' + target + '（且它在选项里 —— 不是"值还在但选项没了"）');
    }

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== T7 验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
