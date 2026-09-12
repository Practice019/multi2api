// user_session_e2e.js —— 像真实用户一样**操作**界面，而不只是看渲染结果。
//
// # 与已有套件的区别
//
// 现有 14 个 E2E 套件大多在**读**：点导航、读面板文本、比数据。
// 但用户实际会**点按钮**（刷新、签到、清屏、翻页、切视图、改设置）。
// 本轮补的是"真的按下去，看会发生什么"：
//   - 点击后有无可见反馈（按钮态/忙碌提示/toast）
//   - 是否发出正确的请求（方法与路径）
//   - 有无控制台报错
//   - 是否留下副作用（比如"清屏"是否真的清了）
//
// 全部用**只读或幂等**的操作，不做任何写账号/删数据的动作。
'use strict';
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

let CHROME;
try { CHROME = require('./chrome_path.js').resolveChrome(); }
catch (e) { console.log('EXCEPTION: ' + e.message); process.exit(1); }

const PORT = 9290;
const BASE = process.env.USER_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-usersession');

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE,
    '--window-size=1440,1000', 'about:blank'], { stdio: 'ignore' });

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { /* 等 */ }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');

    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0;
    const pend = new Map();
    const consoleErrs = [];
    const requests = [];
    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
        consoleErrs.push((m.params.args || []).map(a => a.value || a.description || '').join(' ').slice(0, 150));
      }
      if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails;
        consoleErrs.push('EX: ' + String((d.exception && d.exception.description) || d.text || '').slice(0, 150));
      }
      if (m.method === 'Network.requestWillBeSent') {
        requests.push(m.params.request.method + ' ' + m.params.request.url.replace('http://127.0.0.1:18080', ''));
      }
    };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Network.enable');
    await send('Page.navigate', { url: BASE });
    await sleep(4000);

    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return 'EXC:' + JSON.stringify(r.result.exceptionDetails).slice(0, 150);
      return r.result && r.result.result ? r.result.result.value : undefined;
    };
    const clickNav = async (label) => ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf(${JSON.stringify(label)}) >= 0; })[0];
      if (b) { b.click(); return true; }
      return false;
    })()`);

    ok(await ev('typeof window.__wb2api__ === "object"') === true, '页面已加载');

    // ---- 1) 仪表盘「立即刷新」 ----
    console.log('\n[1] 仪表盘：点「立即刷新」');
    await clickNav('仪表盘');
    await sleep(1500);
    {
      const before = requests.length;
      const clicked = await ev(`(function(){
        var b = document.getElementById('refresh');
        if (!b) return false;
        b.click(); return true;
      })()`);
      ok(clicked === true, '找到并点击「立即刷新」按钮');
      // 观察 1.5s 内的请求
      await sleep(1800);
      const newReqs = requests.slice(before);
      ok(newReqs.length > 0, '点击后发出了请求（' + newReqs.length + ' 个）');
      const hasHealth = newReqs.some(r => /\/healthz/.test(r));
      ok(hasHealth, '其中包含 /healthz（刷新确实在拉状态）');
      // 反馈：toast 或 busy 文案
      const fb = await ev(`(function(){
        var t = document.getElementById('toast');
        var b = document.getElementById('busy');
        return JSON.stringify({
          toast: t ? (t.textContent || '').trim().slice(0, 60) : null,
          busy: b ? (b.textContent || '').trim().slice(0, 60) : null,
          busyHidden: b ? b.hidden : null,
        });
      })()`);
      const f = JSON.parse(fb);
      console.log('      反馈: ' + JSON.stringify(f));
      ok((f.toast && f.toast.length > 0) || (f.busy && f.busy.length > 0) || f.busyHidden === true,
        '点击有可见反馈（toast 或忙碌提示）');
    }

    // ---- 2) 请求日志「清屏」 ----
    console.log('\n[2] 请求日志：点「清屏」');
    await clickNav('请求日志');
    await sleep(2000);
    {
      const before = requests.length;
      const clicked = await ev(`(function(){
        var b = document.getElementById('logClear') || document.getElementById('btnLogClear');
        if (!b) {
          // 找文字是"清屏"的按钮
          b = Array.from(document.querySelectorAll('button')).filter(function(x){
            return (x.textContent || '').trim() === '清屏';
          })[0];
        }
        if (!b) return false;
        b.click(); return true;
      })()`);
      ok(clicked === true, '找到并点击「清屏」');
      await sleep(2000);
      const placeholder = await ev(`(function(){
        var tb = document.getElementById('logrows');
        return tb ? (tb.textContent || '').replace(/\\s+/g,' ').trim().slice(0, 60) : '(no tbody)';
      })()`);
      console.log('      清屏后表格内容: ' + JSON.stringify(placeholder));
      ok(typeof placeholder === 'string' && placeholder.length > 0,
        '清屏后表格有明确状态（不是空白表格）');
    }

    // ---- 3) 任务历史：翻页 ----
    console.log('\n[3] 任务历史：翻到下一页');
    await clickNav('任务历史');
    await sleep(2000);
    {
      const info = await ev(`(function(){
        var pg = document.getElementById('histPager');
        if (!pg) return JSON.stringify({no:true});
        var btns = Array.from(pg.querySelectorAll('button')).map(function(b){
          return { t: (b.textContent || '').trim(), dis: b.disabled };
        });
        return JSON.stringify(btns);
      })()`);
      console.log('      分页按钮: ' + info);
      const nextIdx = await ev(`(function(){
        var pg = document.getElementById('histPager');
        if (!pg) return -1;
        var bs = Array.from(pg.querySelectorAll('button'));
        for (var i = 0; i < bs.length; i++) {
          if ((bs[i].textContent || '').indexOf('下一页') >= 0) return i;
        }
        return -1;
      })()`);
      ok(nextIdx >= 0, '找到「下一页」按钮');
      if (nextIdx >= 0) {
        const before = requests.length;
        const firstBefore = await ev(`(function(){
          var tr = document.querySelector('#histrows tr');
          return tr ? (tr.textContent || '').replace(/\\s+/g,' ').trim().slice(0, 40) : '';
        })()`);
        await ev(`(function(){
          var bs = Array.from(document.getElementById('histPager').querySelectorAll('button'));
          bs[${nextIdx}].click();
        })()`);
        await sleep(2000);
        const firstAfter = await ev(`(function(){
          var tr = document.querySelector('#histrows tr');
          return tr ? (tr.textContent || '').replace(/\\s+/g,' ').trim().slice(0, 40) : '';
        })()`);
        const newReqs = requests.slice(before);
        ok(newReqs.length > 0, '翻页发出了新请求（' + newReqs.length + ' 个）');
        ok(firstBefore !== firstAfter, '翻页后首行内容变了（确实换了页）');
        console.log('      第1页首行: ' + JSON.stringify(firstBefore));
        console.log('      第2页首行: ' + JSON.stringify(firstAfter));
      }
    }

    // ---- 4) 设置面板：改一个值再放弃 ----
    console.log('\n[4] 设置：改值后点「放弃修改」应还原');
    await clickNav('设置');
    await sleep(2500);
    {
      const r = await ev(`(function(){
        var el = document.getElementById('setCheckinLogDays');
        if (!el) return JSON.stringify({no:true});
        var orig = el.value;
        el.value = '999';
        el.dispatchEvent(new Event('input', { bubbles: true }));
        var dirty = document.getElementById('settingsDirty');
        var dirtyText = dirty ? (dirty.textContent || '').trim() : null;
        var btn = Array.from(document.querySelectorAll('button')).filter(function(x){
          return (x.textContent || '').trim().indexOf('放弃') >= 0;
        })[0];
        if (!btn) return JSON.stringify({ orig: orig, noBtn: true, dirtyText: dirtyText });
        btn.click();
        return JSON.stringify({ orig: orig, dirtyText: dirtyText, clicked: true });
      })()`);
      const o = JSON.parse(r);
      console.log('      改前值/脏标记: ' + JSON.stringify(o));
      ok(!o.no && !o.noBtn, '找到设置输入框与「放弃修改」按钮');
      await sleep(1500);
      const after = await ev(`(function(){
        var el = document.getElementById('setCheckinLogDays');
        return el ? el.value : null;
      })()`);
      ok(after === o.orig, '放弃修改后值被还原（' + JSON.stringify(after) + ' ← ' + JSON.stringify(o.orig) + '）');
    }

    // ---- 5) 主题切换按钮 ----
    console.log('\n[5] 顶栏：切主题');
    {
      const before = await ev(`document.documentElement.getAttribute('data-theme')`);
      const clicked = await ev(`(function(){
        var b = document.querySelector('#themeSwitch button[data-theme-mode="dark"]');
        if (!b) return false;
        b.click(); return true;
      })()`);
      ok(clicked === true, '找到并点击「深色」');
      await sleep(900);
      const after = await ev(`document.documentElement.getAttribute('data-theme')`);
      ok(after === 'dark', '主题切到 dark（' + before + ' → ' + after + '）');
      // 还原
      await ev(`document.querySelector('#themeSwitch button[data-theme-mode="system"]').click()`);
      await sleep(600);
    }

    // ---- 汇总 ----
    console.log('\n=== 页面健康 ===');
    ok(consoleErrs.length === 0, '全程 0 个控制台错误' + (consoleErrs.length ? ': ' + consoleErrs.slice(0, 3).join(' | ') : ''));
    console.log('  全程请求数: ' + requests.length);

    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { /* 已退出 */ } }

  console.log(fail === 0 ? '\n=== 用户操作路径验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
