// verify_model_provider_filter.js —— T7 浏览器验证：对话测试的上游筛选器。
//
// # 判据用**渲染结果 + 实际交互**
//
// 静态套件（models_display_gen.js 的 [7]）已验过纯函数逻辑；
// 这里验的是"页面上真的有这个控件、点它真的会变、选择真的持久化"。
//
// ⚠ 2026 模型 id 口径变更后（本文件第 2 节的判据**翻转**了）：
//   · 后端 /v1/models 多上游模式只发 `provider/xxx`（39 条 → 23 条），
//     对话测试下拉因此也只剩这一种形态；
//   · 下拉现在按 **id 去重**（旧版是"不去重、保留 provider/xxx"，用户当时要求
//     保留显式指定上游的手段；后端只发一种写法之后，同一条 id 出现两次就是纯噪声）。
//   · 需要跑着的实例（默认 127.0.0.1:18080，可用 CHAT_URL 覆盖）+ Chrome。
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

    // ---- 2) 下拉：每个 id 只出现一次，且全部形如 provider/xxx ----
    //
    // ⚠ 这一节是**翻转**过的判据。旧版断言：
    //     「「全部上游」时下拉**不去重** —— 保留了 N 个 provider/xxx 形态（用户要求）」
    //   当时后端同时下发裸名与 `provider/裸名`，用户要的是"能显式指定上游"，
    //   所以禁止去重。现在后端多上游**只发** `provider/xxx`，同一条 id 出现两次
    //   就只剩坏处：用户选了一条却看不出是哪条，且"保持原选择"只对第一条生效
    //   （表现为选择自己跳）。所以判据反转成：**id 唯一 + 全带前缀**。
    //
    // 破了会怎样（写进失败信息里）：
    //   · 有重复 id → 下拉里两条一模一样，切换/回退选择时行为不可预期；
    //   · 有裸名 → 与 chip 区、与后端 /v1/models 的口径不一致，
    //     面板里看到的 id 复制到对话测试里会选不中（发了另一个模型）。
    const rawModels = JSON.parse(await ev(`(function(){
      return new Promise(function(resolve){
        var req = new XMLHttpRequest();
        req.open('GET', '/v1/models', true);
        req.setRequestHeader('Authorization', 'Bearer ' + (window.__WB2API_KEY__ || ''));
        req.onload = function(){
          try { resolve(JSON.stringify((JSON.parse(req.responseText).data || []).map(function(m){ return { id: m.id, owned_by: m.owned_by }; }))); }
          catch (e) { resolve('null'); }
        };
        req.onerror = function(){ resolve('null'); };
        req.send();
      });
    })()`));
    const rawIds = (rawModels === null ? [] : rawModels).map(m => m.id);
    const rawDistinct = Array.from(new Set(rawIds));
    const ownerOf = m => { const s = m.id.indexOf('/'); return s > 0 ? m.id.slice(0, s) : (m.owned_by || ''); };

    const dupIds = s0.modelIds.filter((x, i) => s0.modelIds.indexOf(x) !== i);
    const missingSlash = s0.modelIds.filter(x => x.indexOf('/') < 0);
    console.log('  下拉里的 id: ' + JSON.stringify(s0.modelIds.slice(0, 6)) + (s0.modelIds.length > 6 ? ' …' : ''));
    console.log('  /v1/models: ' + rawIds.length + ' 条 / ' + rawDistinct.length + ' 个不同 id');
    ok(s0.modelCount > 0, '模型下拉已填充（' + s0.modelCount + ' 项）');
    ok(dupIds.length === 0,
      '下拉里**每个 id 只出现一次**（下拉 ' + s0.modelCount + ' 项 / 不同 id '
      + new Set(s0.modelIds).size + ' 个）'
      + (dupIds.length ? ' —— 重复: ' + JSON.stringify(Array.from(new Set(dupIds)).slice(0, 4)) : ''));
    ok(missingSlash.length === 0,
      '下拉里的 id **全部**形如 `provider/xxx`（前缀是"显式指定上游"的唯一手段，不能丢）'
      + (missingSlash.length ? ' —— 裸名: ' + JSON.stringify(missingSlash.slice(0, 4)) : ''));
    // 条数不是写死的：跟 /v1/models 的**不同 id 数**对照。
    // 旧版的 32 已经是历史数字（后端只发一种写法后是 23 条那种量级）。
    ok(s0.modelCount === rawDistinct.length,
      '下拉项数（' + s0.modelCount + '）== /v1/models 的不同 id 数（' + rawDistinct.length + '）'
      + ' —— 既没少（筛选/去重误删）也没多（重复项）');
    // 口径前提：多上游模式下 /v1/models 自己就只发前缀名。
    // 前提不成立时上面那条"全部形如 provider/xxx"会报得莫名其妙，所以先单独说清。
    ok(rawDistinct.length > 0 && rawDistinct.every(x => x.indexOf('/') > 0),
      '前置口径：/v1/models 这一版只发 `provider/xxx`（多上游模式）—— 本次变更的前提'
      + (rawDistinct.every(x => x.indexOf('/') > 0) ? '' : '，实际有裸名: '
        + JSON.stringify(rawDistinct.filter(x => x.indexOf('/') < 0).slice(0, 4))));

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
        return { count: m.options.length, ids: ids.slice(0, 6), all: ids,
                 current: m.value,
                 ls: (function(){ try { return localStorage.getItem('wb2api.modelprovider'); } catch(e){ return 'ERR'; } })() };
      })())`));
      console.log('  选 ' + target + ' 后: ' + s1.count + ' 项；当前=' + JSON.stringify(s1.current));
      // ⚠ 「变短」只在**有多个上游**时才成立。
      //
      // 我第一版无条件断言 count < s0.modelCount，于是单上游部署下
      // 报了个假失败（选唯一的上游 == 选全部，全量条数相同是**正确**的）。
      // 判据改成按数据推导：
      //   · 多个上游 → 变短，且**精确等于** /v1/models 里属于该上游的不同 id 数
      //   · 只有一个上游 → 与"全部"相同（并明确断言这一点，而不是跳过）
      //
      // 精确条数同样从 API 数据算（不写 32/23 这类历史数字）：
      // 旧口径下"该上游的条数"含裸名与前缀名两种写法，现在是不同 id 数。
      const expectForTarget = Array.from(new Set(
        (rawModels === null ? [] : rawModels)
          .filter(m => ownerOf(m) === target)
          .map(m => m.id)));
      ok(s1.count === expectForTarget.length,
        '切到 ' + target + ' 后下拉项数（' + s1.count + '）== /v1/models 里属于它的不同 id 数（'
        + expectForTarget.length + '）—— 只列该上游、且每个 id 一次');
      ok(s1.all.every(v => ownerOf({ id: v, owned_by: '' }) === target),
        '筛选后**每一项**都属于 ' + target + '（' + s1.all.length + ' 项）');
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
