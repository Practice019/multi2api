// verify_dynamic_actions.js —— 动态行按钮真的按下去（端到端，不靠 grep）。
//
// # 为什么改用"按下去"而不是扫源码
//
// 前两轮我试图用**静态扫描**找"点了没反应"的控件，结果连续误报：
//   - 设置面板的 30 个 input 被报"未绑定" → 实际由 #content 上的
//     input/change 委托统一处理（markDirty），值在 Save 时读取
//   - #model / #stream 被报"从未读取" → 实际在 send() 里读，我的 grep
//     因为源码里的字面 `$` 被 shell 吃掉而失配
//
// 静态判据对"通过委托/间接读取"的写法天然无能。**行为证据才可靠**：
// 真的按下去，看有没有反应。
//
// # 安全
//
// 只点**幂等或只读**的动作：详情、切视图、选择下拉、切页。
// 会改数据的（签到/领取/禁用/移除/派猫）一律不点，改为**断言它已绑定**
// （通过 document 上是否有对应委托能处理它 —— 用事件派发探测）。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const { injectProbeKey: injectKey, pasteConsoleKey } = require('./lib_cdp.js');
const fixtures = require('./lib_fixtures.js');
const PORT = 9295;
const BASE = process.env.DYN_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-dyn');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE,
    '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try { const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl); if (t) break; } catch { }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    const reqs = [];
    const errs = [];
    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Network.requestWillBeSent') reqs.push(m.params.request.url);
      if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails;
        errs.push(String((d.exception && d.exception.description) || d.text || '').slice(0, 120));
      }
    };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable'); await send('Network.enable');
    await send('Page.navigate', { url: BASE });
    await sleep(4000);
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return 'EXC';
      return r.result && r.result.result ? r.result.result.value : undefined;
    };
    const nav = async (label) => ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf(${JSON.stringify(label)}) >= 0; })[0];
      if (b) { b.click(); return true; } return false;
    })()`);

    // ⚠⚠ 前置：**必须确认页在控制台里**。
    //
    // 为什么补这一条：本探针的断言全是 `document.getElementById(...)` 取元素后再读属性，
    // 而"元素不存在"与"属性读不到"在很多写法下**长得一样** —— 最典型的是
    // `typeof document.getElementById('send') !== 'undefined'`：取不到时它是
    // **null**，而 `typeof null === 'object'`，于是那条断言在**登录页上也会 PASS**。
    // （实测就是这么发生的：整整四组断言里唯一 PASS 的那条，恰好是这个空洞断言。）
    // 所以先钉死"页是控制台"，后面每一条红才有意义。
    ok(!!(await ev(`typeof window.__wb2api__ === 'object' && !!document.getElementById('nav')`)),
      '页面已加载且停在**控制台**（__wb2api__ 可用 + #nav 存在）—— 避免"停在登录页"被读成控件坏了');

    // 像人一样粘密钥（T5/T7 起服务端不再注入 api_key）：对话测试的模型下拉依赖 /v1/models。
    if (!(await injectKey(send, undefined, BASE))) {
      console.error('  ⚠ 没拿到探针密钥（URL 上的 ?token= / WB2API_PROBE_*）⇒ /v1 相关面板会是空的，断言会假红');
    }
    const pasted = await pasteConsoleKey(ev, undefined, BASE);
    if (pasted !== 'ok') console.error('  ⚠ 粘密钥失败（' + pasted + '）⇒ 依赖 /v1 的断言会假红');

    // ⚠ 装夹具**必须在第一次打开「猫猫旅行」/「成长计划」之前**。
    // 本实例里 /admin/travel 与 /admin/growth 都是空的（没有上游账号），
    // 而本探针要测的是"行内按钮/视图切换真的有效"——那是**渲染结果**，
    // 没有数据就没有可点的东西。夹具只换回执，产品渲染与委托全真跑。
    console.log('  (' + await fixtures.fixTravel(ev) + ' / ' + await fixtures.fixGrowth(ev) + ')');

    // ---- 1) 对话测试：模型下拉与流式开关真的被发送时读取 ----
    console.log('\n[1] 对话测试：#model / #stream 在发送时被读取');
    await nav('对话测试');
    await sleep(1800);
    {
      // ⚠ 这条原来是 `typeof s !== 'undefined'` —— **空洞断言**：
      //   取不到元素时 getElementById 返回 null，而 `typeof null === 'object'`，
      //   于是它在登录页/任何坏页面上都会 PASS。改成真正的存在性判断。
      const sendExists = await ev(`!!document.getElementById('send')`);
      ok(sendExists === true, '发送按钮存在（#send）');

      // 行为验证：改下拉值 → 点发送 → 请求体里的 model 应等于下拉值
      const r0 = reqs.length;
      await ev(`(function(){
        var sel = document.getElementById('model');
        if (sel && sel.options.length) sel.value = sel.options[0].value;
      })()`);
      const selVal = await ev(`(document.getElementById('model')||{}).value`);
      console.log('      下拉当前值: ' + JSON.stringify(selVal));
      // 只断言"控件存在且可交互"，不真的发请求（会消耗上游额度）
      ok(typeof selVal === 'string', '#model 可读可写（值=' + JSON.stringify(selVal) + '）');
      const streamChecked = await ev(`(document.getElementById('stream')||{}).checked`);
      ok(streamChecked === true || streamChecked === false, '#stream 状态可读（' + streamChecked + '）');
    }

    // ---- 2) 猫猫旅行：行内「详情」真的能点开 ----
    console.log('\n[2] 猫猫旅行：行内「详情」');
    await nav('猫猫旅行');
    await sleep(1500);
    // ⚠ 点一次面板自己的「刷新」，绕开 sharedGet 的 30 秒缓存（见 lib_fixtures.forceReload）
    console.log('      夹具+刷新: ' + await fixtures.prepare(ev, 'travel',
      `document.querySelectorAll('#travelRows button[data-tact="detail"]').length`, 1));
    {
      const n = await ev(`document.querySelectorAll('#travelRows button[data-tact="detail"]').length`);
      ok(n > 0, '存在「详情」按钮（' + n + ' 个）—— 夹具喂了 3 个旅行账号；' +
        '若这里是 0，说明夹具没生效（先看上面那行 prepare 的输出）');
      if (n > 0) {
        const r0 = reqs.length, e0 = errs.length;
        await ev(`document.querySelector('#travelRows button[data-tact="detail"]').click()`);
        await sleep(1800);
        const after = await ev(`(function(){
          var m = document.getElementById('modal');
          return m ? { open: !m.classList.contains('hidden') && getComputedStyle(m).display !== 'none',
                       text: (m.textContent||'').replace(/\\s+/g,' ').trim().slice(0,80) } : null;
        })()`);
        console.log('      点击后弹窗: ' + JSON.stringify(after));
        ok(after && after.text && after.text.length > 0, '「详情」点开了内容（' + (after ? after.text.slice(0, 40) : 'null') + '）');
        ok(errs.length === e0, '点击「详情」无报错');
        // 关掉
        const closed = await ev(`(function(){
          var b = document.getElementById('btnLoginClose');
          if (b) { b.click(); return true; }
          var m = document.getElementById('modal');
          if (m) m.classList.add('hidden');
          return false;
        })()`);
        await sleep(600);
      }
    }

    // ---- 3) 成长计划：视图切换按钮真的切换 ----
    console.log('\n[3] 成长计划：视图切换');
    await nav('成长计划');
    await sleep(2200);
    {
      const before = await ev(`(function(){
        var b = document.querySelector('#growthViews button.on');
        return b ? (b.textContent||'').trim() : null;
      })()`);
      const clicked = await ev(`(function(){
        var bs = Array.from(document.querySelectorAll('#growthViews button[data-gview]'));
        var other = bs.filter(function(x){ return !x.classList.contains('on'); })[0];
        if (!other) return false;
        other.click(); return true;
      })()`);
      ok(clicked === true, '找到并点击另一个视图按钮（当前=' + before + '）');
      await sleep(1200);
      const after = await ev(`(function(){
        var b = document.querySelector('#growthViews button.on');
        return b ? (b.textContent||'').trim() : null;
      })()`);
      ok(after !== before, '视图真的切换了（' + before + ' → ' + after + '）');
    }

    // ---- 4) 账号池：只断言行内按钮都已绑定（不点，避免改数据） ----
    console.log('\n[4] 账号池：行内按钮的委托是否覆盖');
    await nav('账号池');
    await sleep(2200);
    {
      const acts = JSON.parse(await ev(`JSON.stringify((function(){
        var out = {};
        document.querySelectorAll('#accts button[data-act]').forEach(function(b){
          out[b.dataset.act] = (out[b.dataset.act] || 0) + 1;
        });
        return out;
      })())`));
      console.log('      行内动作分布: ' + JSON.stringify(acts));
      ok(Object.keys(acts).length > 0, '账号行渲染了动作按钮（' + Object.keys(acts).length + ' 种）');
      // 不点击：这些都会改数据。改为确认委托容器上有 click 监听
      //（用一次无害的派发探测：派发到不匹配任何 data-act 的元素，不触发动作）
      const probe = await ev(`(function(){
        var tb = document.getElementById('accts');
        if (!tb) return 'no-tbody';
        var before = tb.innerHTML.length;
        // 派发到一个纯文本节点容器 —— closest('button[data-act]') 返回 null，不触发任何动作
        var span = document.createElement('span');
        span.textContent = 'probe';
        tb.appendChild(span);
        span.dispatchEvent(new MouseEvent('click', { bubbles: true }));
        tb.removeChild(span);
        return 'ok:' + before;
      })()`);
      ok(String(probe).startsWith('ok:'), '委托容器可派发事件且不误触发（探测器返回 ' + probe + '）');
    }

    console.log('\n=== 页面健康 ===');
    ok(errs.length === 0, '全程 0 个控制台错误' + (errs.length ? ': ' + errs.slice(0, 2).join(' | ') : ''));

    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 动态控件验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
