// foster_parenting_probe.js —— 独立验证 Builder 的论断：
// "账号表里不能用 <div> 容器包 <tr>，也不能靠兄弟选择器隐藏行"
//
// # 为什么要单独验
//
// Builder 据此**改变了实现方式**（从"复用 .mgroup"改成"行上的持久类"）。
// 如果它的论断错了，那它做的是一个不必要的复杂方案；
// 如果对了，那我的计划书（"照抄同一套"）就是错的。
//
// 这条判据**与产品代码无关** —— 纯浏览器规范的实测，所以不依赖任何实例。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9311;
const PROFILE = path.join(os.tmpdir(), 'chrome-foster');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, 'about:blank'], { stdio: 'ignore' });
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
    await send('Page.navigate', { url: 'data:text/html,<p>x</p>' });
    await sleep(1200);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

    // ---- 试验 1：把 <div> 放进 <tbody> 会怎样 ----
    const t1 = JSON.parse(await ev(`JSON.stringify((function(){
      document.body.innerHTML = '<table><tbody id="tb">'
        + '<div id="wrapper"><tr id="r1"><td>a</td></tr></div>'
        + '<tr id="r2"><td>b</td></tr>'
        + '</tbody></table>';
      var tb = document.getElementById('tb');
      return {
        tbodyHTML: tb.innerHTML.replace(/\\s+/g,' ').trim().slice(0, 120),
        // wrapper 还在 tbody 里吗？
        wrapperParent: (document.getElementById('wrapper') || {}).parentNode
                        ? document.getElementById('wrapper').parentNode.tagName : 'GONE',
        // r1 的父节点是谁？
        r1Parent: (document.getElementById('r1') || {}).parentNode
                        ? document.getElementById('r1').parentNode.tagName : 'GONE',
      };
    })())`));
    console.log('  试验1 结果: ' + JSON.stringify(t1));
    ok(t1.wrapperParent !== 'TBODY',
      '【foster parenting】<div> 放进 <tbody> 后**不再**是 tbody 的子节点（实际父节点 ' + t1.wrapperParent + '）');
    ok(t1.r1Parent === 'TBODY',
      '【foster parenting】里面的 <tr> 被**搬出来**直接挂到 tbody（实际 ' + t1.r1Parent + '）—— 所以"用 div 包住一组 tr"这种结构活不到运行期');

    // ---- 试验 2：兄弟选择器能不能按分组隐藏 ----
    const t2 = JSON.parse(await ev(`JSON.stringify((function(){
      var st = document.createElement('style');
      st.textContent = 'tr.grouprow.collapsed ~ tr.acctrow{display:none}';
      document.head.appendChild(st);
      document.body.innerHTML = '<table><tbody id="tb2">'
        + '<tr class="grouprow collapsed" id="g1"><td>组1</td></tr>'
        + '<tr class="acctrow" id="a1"><td>账号1</td></tr>'
        + '<tr class="acctrow" id="a2"><td>账号2</td></tr>'
        + '<tr class="grouprow" id="g2"><td>组2</td></tr>'
        + '<tr class="acctrow" id="a3"><td>账号3</td></tr>'
        + '</tbody></table>';
      var vis = function(id){
        var el = document.getElementById(id);
        return el ? getComputedStyle(el).display !== 'none' : null;
      };
      return { a1_visible: vis('a1'), a2_visible: vis('a2'), a3_visible: vis('a3') };
    })())`));
    console.log('  试验2 结果: ' + JSON.stringify(t2));
    ok(t2.a1_visible === false && t2.a2_visible === false,
      '【兄弟选择器"有效"】grouprow.collapsed 之后的 acctrow 确实被隐藏了');

    // ⚠ 这里是我第一版断言写错的地方。
    //
    // 我原先断言 a3 应该**可见**（"后一个分组的账号没被误隐藏"）—— 实测 a3 也隐藏了。
    // 那不是产品 bug，是 `~` 的语义本来如此：
    //
    //   `tr.grouprow.collapsed ~ tr.acctrow`
    //   = 同一父节点下，位于 .grouprow.collapsed **之后的所有** .acctrow
    //
    // 它**不区分**这些 acctrow 属于哪个分组 —— 表里没有嵌套结构可依据。
    //
    // 所以 Builder 说"不能靠兄弟选择器"是对的，只是**原因不是选择器失效、
    // 而是选择器过宽**（会连后面分组的账号一起隐藏）。
    // 我第一版的理由写错了，但结论（不能用）是对的。
    ok(t2.a3_visible === false,
      '【关键】折叠组1 会**连组2 的账号一起隐藏**（a3 实际 ' + t2.a3_visible + '）—— ' +
      '这正是"兄弟选择器不能用于分组折叠"的真正原因：它无法界定分组边界');

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 探针完成 ===' : '\n=== ' + fail + ' 项与预期不符 ===');
  process.exit(fail ? 1 : 0);
})();
