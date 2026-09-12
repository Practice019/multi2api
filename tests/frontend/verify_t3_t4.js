// verify_t3_t4.js —— 验证 T3+T4 的用户可见结果（浏览器实测）。
//
// 不靠读源码推断 —— 本项目的教训（第 10 次）：
// 我差点因为一个字符的判据错误去"修"一个正常功能。
// 这里全部用**渲染结果**做判据。
//
// # 为什么用真浏览器而不是"抠函数在 Node 里跑"
//
// 面板"生成"（buildProviderPanels → panelCapsOf）与"隐藏"（hideOrphanPanels
// → capNamesOf）用的是**两条不同的判据**：
//   生成 = 该能力位有没有 GET 入口（减去已由专属面板承担的）
//   隐藏 = 上游**声明**了这个能力位（providers[].capabilities）
// 两条判据的组合结果只有真 DOM + 真 manifest 才能证明 ——
// 只看函数逻辑看不出"生成了又被立刻隐藏"这种恒真流程（R2 就是这个形状）。
//
// 验 8 条：
//   1. 导航里**没有**「签到与保活」
//   2. #content 里**没有** workbuddy:core section（R2：干脆不生成，而不是生成后隐藏）
//   3. 导航 workbuddy 分组下有「任务历史」
//   4. 通用分组下**没有**任务历史
//   5. codearts 分组下**没有**任务历史
//   6. 设置面板的签到/保活控件**完好**（没被误删）
//   7. checkin 能力位**仍然存在**（R1 禁止"在 panelCapsOf 里把能力位整个抹掉"）
//   8. 任务历史的内容只含**本上游**账号（T4 的出口过滤）
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9302;
const TARGET = process.env.T34_URL || 'http://127.0.0.1:18080/ui';
const API = TARGET.replace(/\/ui$/, '');
const KEY = process.env.WB2API_KEY || (process.env.WB2API_KEY_DEV || 'test-key-not-real');
const PROFILE = path.join(os.tmpdir(), 'chrome-t34');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u, h) { return new Promise((res, rej) => { http.get(u, { headers: h || {} }, r => { const c = []; r.on('data', x => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x))); r.on('end', () => res(Buffer.concat(c).toString('utf8'))); }).on('error', rej); }); }
const api = async p => JSON.parse(await get(API + p, { Authorization: 'Bearer ' + KEY }));

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
    if (!t) throw new Error('Chrome 未启动');
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(5500);
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 300));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // 收集全部事实
    const snap = JSON.parse(await ev(`JSON.stringify((function(){
      var out = { navGroups: [], navLabels: [], sections: [], settingsCtrls: [], panelCaps: null, caps: [] };
      document.querySelectorAll('#nav .navgroup').forEach(function(g){
        var hd = g.querySelector('.navgrouphd');
        out.navGroups.push({
          pid: g.dataset.pid || '',
          items: Array.from(g.querySelectorAll('button[data-nav]')).map(function(b){
            return (b.textContent||'').replace(/\\s+/g,' ').trim();
          })
        });
      });
      out.navLabels = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .map(function(b){ return (b.textContent||'').replace(/\\s+/g,' ').trim(); });
      Array.from(document.querySelectorAll('#content > section')).forEach(function(s){
        out.sections.push({ key: s.dataset.key || '', provider: s.dataset.provider || '',
          cap: s.dataset.cap || '', hidden: s.hidden, generated: !!s.dataset.generated });
      });
      if (window.__wb2api__) {
        out.panelCaps = window.__wb2api__.panelCapsOf('workbuddy');
        var m = window.__wb2api__.manifest();
        var wb = (m.providers||[]).filter(function(p){ return p && p.id === 'workbuddy'; })[0];
        out.caps = wb ? wb.capabilities : [];
      }
      ['setCheckinEnabled','setKeepaliveEnabled','setCheckinHours','setKeepaliveHours','setCheckinLogDays']
        .forEach(function(id){ out.settingsCtrls.push({ id: id, found: !!document.getElementById(id) }); });
      return out;
    })())`));

    console.log('=== 导航分组 ===');
    snap.navGroups.forEach(g => {
      console.log('  [' + (g.pid || '通用') + '] ' + g.items.join(' | '));
    });
    console.log('\n=== #content sections ===');
    snap.sections.forEach(s => console.log('  key=' + String(s.key).padEnd(24) +
      ' provider=' + String(s.provider).padEnd(11) + ' cap=' + String(s.cap).padEnd(12) +
      (s.hidden ? '[隐藏]' : '[显示]') + (s.generated ? ' (生成)' : '')));
    console.log('\n  panelCapsOf(workbuddy) = ' + JSON.stringify(snap.panelCaps));
    console.log('  workbuddy.capabilities  = ' + JSON.stringify(snap.caps));

    console.log('\n=== 断言 ===');
    // 1
    ok(!snap.navLabels.some(l => l.indexOf('签到与保活') >= 0),
      '导航里没有「签到与保活」' +
      (snap.navLabels.some(l => l.indexOf('签到与保活') >= 0) ? ' —— 仍在' : ''));
    // 2
    const coreSec = snap.sections.filter(s => s.key === 'workbuddy:core' || (s.cap === 'core' && s.provider === 'workbuddy'));
    ok(coreSec.length === 0, '没有 workbuddy:core section（干脆不生成）' +
      (coreSec.length ? ' —— 仍有 ' + coreSec.length + ' 个' : ''));
    // 2b
    const checkinGen = snap.sections.filter(s => s.cap === 'checkin' && s.generated);
    ok(checkinGen.length === 0, '没有**生成**的 checkin 通用面板（R1）' +
      (checkinGen.length ? ' —— 仍有 ' + checkinGen.length + ' 个' : ''));

    // 7（先断言能力位还在，再断言面板归属）
    ok(snap.caps.indexOf('checkin') >= 0,
      "checkin 能力位仍然存在于 manifest（没有被整个抹掉）");

    // 3
    const wbGroup = snap.navGroups.find(g => g.pid === 'workbuddy');
    ok(!!wbGroup, '有 workbuddy 分组');
    ok(wbGroup && wbGroup.items.some(x => x.indexOf('任务历史') >= 0),
      'workbuddy 分组下有「任务历史」（实际: ' + (wbGroup ? wbGroup.items.join('/') : '—') + '）');

    // 4
    const genericGroup = snap.navGroups.find(g => g.pid === '');
    ok(genericGroup && !genericGroup.items.some(x => x.indexOf('任务历史') >= 0),
      '通用分组下**没有**任务历史（实际: ' + (genericGroup ? genericGroup.items.join('/') : '—') + '）');

    // 5
    const caGroup = snap.navGroups.find(g => g.pid === 'codearts');
    if (caGroup) {
      ok(!caGroup.items.some(x => x.indexOf('任务历史') >= 0),
        'codearts 分组下没有任务历史（实际: ' + caGroup.items.join('/') + '）');
    } else {
      console.log('  SKIP codearts 分组不存在（可能未启用）');
    }

    // 3b：section 的归属属性
    const histSec = snap.sections.find(s => s.cap === 'checkin' && s.provider === 'workbuddy');
    ok(!!histSec && histSec.key === 'workbuddy:history',
      '任务历史 section 带 data-provider="workbuddy" / data-cap="checkin"' +
      (histSec ? '（实际 key=' + histSec.key + '）' : '（找不到）'));
    ok(!!histSec && histSec.hidden === false, '任务历史 section 未被隐藏');

    // 6
    const missing = snap.settingsCtrls.filter(c => !c.found);
    ok(snap.settingsCtrls.length === 5 && missing.length === 0,
      '设置面板的签到/保活控件完好（5 个都在）' +
      (missing.length ? ' —— 缺: ' + missing.map(x => x.id).join(',') : ''));

    // 8：出口过滤（真端点，真数据）
    const acc = await api('/admin/accounts');
    const wbUids = new Set((acc.accounts || []).filter(a => a.provider === 'workbuddy').map(a => a.uid));
    const raw = await api('/admin/checkin/history?limit=300&offset=0');
    const uids = [...new Set((raw.items || []).map(x => x.uid))];
    const bad = uids.filter(u => !wbUids.has(u));
    console.log('    端点 total=' + raw.total + '，本页 uid 种类=' + uids.length +
      '（' + uids.map(u => u.slice(0, 8)).join(',') + '）');
    ok(bad.length === 0, '历史端点只返回本上游账号的记录' +
      (bad.length ? ' —— 越界: ' + bad.map(u => u.slice(0, 8)).join(',') : ''));
    ok(raw.total > 0 && raw.total < 5000,
      'total 是**过滤后**的数（' + raw.total + ' < 全量 5000）');

    // 点开面板，确认内容真的渲染
    const rows = await ev(`(async () => {
      const btns = Array.from(document.querySelectorAll('#nav button[data-nav]'));
      const b = btns.find(x => x.textContent.indexOf('任务历史') >= 0);
      if (!b) return -1;
      b.click();
      await new Promise(r => setTimeout(r, 2500));
      return document.querySelectorAll('#histrows tr').length;
    })()`);
    ok(rows > 0, '点开「任务历史」后表格真的渲染出行（行数=' + rows + '）');

    // 页面健康
    ok(await ev('String(window.__navError||"")') === '', '导航无运行时错误');
    const errs = await ev('JSON.stringify(window.__errs || [])');
    ok(errs === '[]' || errs === undefined, '无未捕获脚本错误');

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== T3+T4 验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
