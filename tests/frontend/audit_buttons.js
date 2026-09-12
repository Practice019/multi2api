// audit_buttons.js —— 逐个**按下**界面上的每个按钮，检测"点了没反应"。
//
// # 为什么做这个
//
// 上一轮「清屏」按钮被发现在**完全无效**（点了内容不变）。
// 那不是我特意找的 —— 是我"真的按了一下"才发现的。
// 说明：现有 16 个套件覆盖了**读**，但没有系统性覆盖**按**。
//
// 本脚本枚举所有可见按钮，逐个点击，记录：
//   - 是否产生 DOM 变化（before/after 快照对比）
//   - 是否发出网络请求
//   - 是否报错
// 三者**都没有** = 疑似死按钮。
//
// # 安全约束
//
// 只点**只读或幂等**的按钮。会改数据的一律跳过（签到、领奖、派猫、
// 禁用、移除、保存设置、切换客户端登录等），并把它们列出来由人工判断。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9294;
const BASE = process.env.AUDIT_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-btn-audit');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

// 会改动数据/状态的按钮 —— 一律不点（按文字匹配）
const DESTRUCTIVE = [
  '签到', '全部签到', '领取', '领奖', '全部领奖', '派猫', '全部派猫',
  '禁用', '启用', '移除', '删除', '保存设置', '保存', '放弃修改',
  '重载 auths', '刷新全部积分', '全部保活', '保活', '积分',
  '连接', '回滚', '切换', '强制刷新目录', '清屏', '恢复显示',
  '添加账号', '立即刷新',
];

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE,
    '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });

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
    const reqs = [];
    const errs = [];
    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Network.requestWillBeSent') reqs.push(m.params.request.method + ' ' + m.params.request.url.replace('http://127.0.0.1:18080', '').replace(/^http:\/\/127\.0\.0\.1:\d+/, ''));
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

    const PANELS = ['仪表盘', '账号池', '任务调度', '请求日志', '任务历史', '可用模型', '对话测试', '设置', '猫猫旅行', '成长计划', '签到与保活', '福利中心'];
    const report = [];

    for (const panel of PANELS) {
      if (!await nav(panel)) continue;
      await sleep(1800);

      // 列出该面板里所有可见按钮（排除导航与主题切换）
      const btns = JSON.parse(await ev(`JSON.stringify((function(){
        var sec = document.querySelector('#content > section.active');
        if (!sec) return [];
        var out = [];
        sec.querySelectorAll('button').forEach(function(b, i){
          var txt = (b.textContent || '').replace(/\\s+/g, ' ').trim();
          var r = b.getBoundingClientRect();
          out.push({ i: i, text: txt, id: b.id || '', dis: b.disabled,
                     visible: r.width > 0 && r.height > 0 });
        });
        return out;
      })())`));

      for (const b of btns) {
        if (!b.visible || b.dis || !b.text) continue;
        const isDestructive = DESTRUCTIVE.some(d => b.text.includes(d));
        if (isDestructive) {
          report.push({ panel, text: b.text, id: b.id, skipped: '会改数据，未点击' });
          continue;
        }

        const beforeSig = await ev(`(function(){
          var sec = document.querySelector('#content > section.active');
          return sec ? (sec.textContent || '').replace(/\\s+/g,' ').trim().slice(0, 3000) : '';
        })()`);
        const r0 = reqs.length, e0 = errs.length;

        // 按下去
        await ev(`(function(){
          var sec = document.querySelector('#content > section.active');
          var bs = Array.from(sec.querySelectorAll('button'));
          var b = bs[${b.i}];
          if (b) b.click();
        })()`);
        await sleep(1500);

        const afterSig = await ev(`(function(){
          var sec = document.querySelector('#content > section.active');
          return sec ? (sec.textContent || '').replace(/\\s+/g,' ').trim().slice(0, 3000) : '';
        })()`);
        const newReqs = reqs.slice(r0);
        const newErrs = errs.slice(e0);
        const changed = beforeSig !== afterSig;

        report.push({
          panel, text: b.text, id: b.id,
          changed, requests: newReqs.length, errors: newErrs.length,
          sample: newReqs.slice(0, 2),
          verdict: (changed || newReqs.length > 0) ? 'ok' : 'NO-OP',
        });
        console.log(`  [${(changed || newReqs.length > 0) ? 'ok  ' : 'NO-OP'}] ${panel} / "${b.text}"  变化=${changed} 请求=${newReqs.length} 错误=${newErrs.length}`);
        if (newErrs.length) console.log('         错误: ' + newErrs[0]);
      }
    }

    console.log('\n=== 汇总 ===');
    const noop = report.filter(r => r.verdict === 'NO-OP');
    const ok = report.filter(r => r.verdict === 'ok');
    const skipped = report.filter(r => r.skipped);
    console.log('  点击并有效果: ' + ok.length);
    console.log('  **点了没反应**: ' + noop.length);
    noop.forEach(r => console.log('    ' + r.panel + ' / "' + r.text + '"' + (r.id ? ' (#' + r.id + ')' : '')));
    console.log('  未点击（会改数据）: ' + skipped.length);
    const byPanel = {};
    skipped.forEach(r => { byPanel[r.panel] = (byPanel[r.panel] || 0) + 1; });
    Object.keys(byPanel).forEach(p => console.log('    ' + p + ': ' + byPanel[p] + ' 个'));

    fs.writeFileSync(path.join(os.tmpdir(), 'button-audit.json'), JSON.stringify(report, null, 2));
    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); }
  finally { try { ch.kill(); } catch { } }
  process.exit(0);
})();
