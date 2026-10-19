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
const API = TARGET.replace(/\/ui(\?.*)?$/, '');
// ⚠ api_key 的来源：**不能**回落到一个写死的字符串。
//
// 原写法 `process.env.WB2API_KEY || 'test-key-not-real'` 在套件里必然失效：
// run_e2e 只给每个套件传**一个 URL 环境变量**（这里是 T34_URL），没人设 WB2API_KEY。
// 于是 `api()` 带着 'test-key-not-real' 打 /admin/* ⇒ 拿到**登录页 HTML** ⇒
// `JSON.parse('<')` 抛 `Unexpected token '<', "<!DOCTYPE "...` ——
// 本探针长期红的那条 EXCEPTION 就是它，与产品无关（同一批端点别处逐字正常）。
//
// T7 起控制台的门是 `?token=<admin_token>`，探针实例里 api_key == admin_token，
// 所以 run_e2e 传进来的 URL 上就自带可用的 key（与 lib_cdp.probeApiKey 同源）。
function resolveKey() {
  if (process.env.WB2API_KEY) return process.env.WB2API_KEY;
  const m = TARGET.match(/[?&]token=([^&#]+)/);
  if (m) return decodeURIComponent(m[1]);
  return process.env.WB2API_PROBE_TOKEN || '';
}
const KEY = resolveKey();
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
    //
    // ⚠ 2026-10-19：「任务历史」标签页**被用户要求删除**（连 section 一起下线，
    //   见 webui.html 里 `loadHistory` 开头那段说明："「任务历史」标签页已删除
    //   （用户本轮要求）"）。所以这条由"必须有"翻转为**删除守卫**（"不许回来"）——
    //   判据的意图一律保留：T3 当初要的是"这个面板**归属**于 workbuddy，
    //   不是通用面板"；现在它既不归 workbuddy 也不归通用，因为**它不存在**。
    //   归属这件事改由下面 3b 用真实存在的生成面板来证（那才是这条契约的活证据）。
    const wbGroup = snap.navGroups.find(g => g.pid === 'workbuddy');
    ok(!!wbGroup, '有 workbuddy 分组');
    ok(wbGroup && !wbGroup.items.some(x => x.indexOf('任务历史') >= 0),
      'workbuddy 分组下**没有**「任务历史」（用户本轮要求删除该标签页）' +
      (wbGroup && wbGroup.items.some(x => x.indexOf('任务历史') >= 0) ? ' —— 它又回来了' : ''));

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
    // 5b：**任何**分组下都不许有它（把 3/4/5 收敛成一条覆盖全集的判据，
    //     顺手堵住"挪到第三个分组去"这种绕过）
    const histAnywhere = snap.navGroups.filter(g => g.items.some(x => x.indexOf('任务历史') >= 0));
    ok(histAnywhere.length === 0,
      '任何导航分组里都没有「任务历史」' +
      (histAnywhere.length ? ' —— 出现在: ' + histAnywhere.map(g => g.pid || '通用').join(',') : ''));

    // 3b：`data-provider` / `data-cap` 的归属契约 —— 用**真实存在的生成面板**证
    //
    // ⚠ 原来这条钉的是 `workbuddy:history` 那个 section（cap=checkin）。
    //   标签页删除后它不存在了，但 `data-provider` / `data-cap` 这套归属属性
    //   是**活着的契约**（导航分组、面板归属、能力位过滤都靠它）。
    //   所以改成从当前页**现取**生成面板来证 —— 判据更强：
    //   原来只证了一个上游的一个面板，现在证"每一个生成面板的 key 都恰好是
    //   provider:cap，且 cap 确实在该上游的 panelCapsOf 里"。
    const genPanels = snap.sections.filter(s => s.generated && s.provider && s.cap);
    console.log('  生成的带归属面板: ' + JSON.stringify(genPanels.map(s => s.key)));
    ok(genPanels.length > 0,
      '至少有一个生成面板带 data-provider + data-cap（否则归属契约无人可证）');
    const keyMismatch = genPanels.filter(s => s.key !== s.provider + ':' + s.cap);
    ok(keyMismatch.length === 0,
      '每个生成面板的 key == provider + ":" + cap' +
      (keyMismatch.length ? ' —— 不符: ' + JSON.stringify(keyMismatch) : ''));
    ok(!snap.sections.some(s => s.key === 'workbuddy:history'),
      '不存在 workbuddy:history section（该标签页已按用户要求下线）');

    // 6：签到/保活的**配置入口**没有丢
    //
    // ⚠ 原判据点名 5 个可编辑控件（setCheckinEnabled / setKeepaliveEnabled /
    //   setCheckinHours / setKeepaliveHours / setCheckinLogDays）全在。
    //   本轮产品把它们**合并掉了**：签到/保活统一为 30 分钟被动扫描，
    //   不再有"启用开关 + 时点"（webui.html:1632-1635 的原话："开关只是概念残留，
    //   会让人以为还能配置时点"）；保活改成**只读展示**（#setKeepaliveRO，"安全网，
    //   避免误关"）。自动派送跟着签到走，所以也不再有独立开关。
    //
    // ⇒ 那是**产品侧有意的形态变更（A）**，判据重新表达为：
    //   ① 仍然可编辑的那项还在（#setCheckinLogDays）
    //   ② 保活以只读形式可见且有内容（#setKeepaliveRO）
    //   ③ 调度说明行有内容（#schInfo）
    //   ④ **反向守卫**：已删除的那四个可编辑控件不许回来 ——
    //      留着它们就是"两处配置同一个行为"，比少一个更坏
    //   （判据的意图"配置入口没丢、用户仍然看得见这些设置"一条没减）
    const setCtrls = JSON.parse(await ev(`JSON.stringify((function(){
      var ids = ['setCheckinLogDays','setKeepaliveRO','schInfo','setReqLogDays'];
      var gone = ['setCheckinEnabled','setKeepaliveEnabled','setCheckinHours','setKeepaliveHours'];
      var o = { present: {}, removed: {} };
      ids.forEach(function(id){
        var e = document.getElementById(id);
        o.present[id] = e ? { found: true, text: (e.textContent||'').trim().length,
                              tag: e.tagName.toLowerCase(), value: (e.value||'') } : { found: false };
      });
      gone.forEach(function(id){ o.removed[id] = !!document.getElementById(id); });
      return o;
    })())`));
    console.log('    设置面板控件: ' + JSON.stringify(setCtrls));
    ok(setCtrls.present.setCheckinLogDays && setCtrls.present.setCheckinLogDays.found,
      '签到历史保留天数仍是**可编辑**控件（#setCheckinLogDays）');
    ok(setCtrls.present.setKeepaliveRO && setCtrls.present.setKeepaliveRO.found &&
       setCtrls.present.setKeepaliveRO.text > 0,
      '保活仍可见（只读 #setKeepaliveRO，内容 ' +
      (setCtrls.present.setKeepaliveRO ? setCtrls.present.setKeepaliveRO.text : '?') + ' 字）—— ' +
      '"保活被静默去掉"会在这里红');
    ok(setCtrls.present.schInfo && setCtrls.present.schInfo.found &&
       setCtrls.present.schInfo.text > 0,
      '日常调度说明行有内容（#schInfo）');
    const cameBack = Object.keys(setCtrls.removed).filter(k => setCtrls.removed[k]);
    ok(cameBack.length === 0,
      '已删除的签到/保活可编辑控件没有回来（' + cameBack.join(',') + '）—— ' +
      '两处配置同一个行为比少一处更坏');

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

    // 8b：面板"点开真的渲染出行"——改用**请求日志**面板（同一族渲染器的活载体）
    //
    // ⚠ 原来这条点「任务历史」再看 `#histrows tr`。该标签页**已被用户要求删除**
    //   （`#histrows` 不存在 ⇒ 恒 -1），所以判据的载体换成仍然活着的
    //   「请求日志」面板：`renderLogs` 与已被删掉的 history 表是同一族
    //   （都走 normalizeDesc + 分页器），"点开面板必须真有内容而不是空壳"这条
    //   意图**一个字没改**，只是换到还存在的那块面板上。
    //   同时保留**反向守卫**：`#histrows` 不许回来（那就是标签页复活了）。
    const logRows = await ev(`(async () => {
      const btns = Array.from(document.querySelectorAll('#nav button[data-nav]'));
      const b = btns.find(x => x.textContent.indexOf('请求日志') >= 0);
      if (!b) return -1;
      b.click();
      for (let i = 0; i < 20; i++) {
        await new Promise(r => setTimeout(r, 400));
        const n = document.querySelectorAll('#logrows .log-row').length;
        if (n > 0) return n;
      }
      return document.querySelectorAll('#logrows .log-row').length;
    })()`);
    ok(logRows > 0, '点开「请求日志」后真的渲染出日志行（行数=' + logRows + '）' +
      ' —— 本实例由 seed_traffic.js 灌过真实请求日志；若为 0 先跑它');
    ok((await ev(`!!document.getElementById('histrows')`)) === false,
      '#histrows 不存在（被删掉的「任务历史」标签页没有复活）');

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
