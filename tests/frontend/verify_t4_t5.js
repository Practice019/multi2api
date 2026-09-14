// verify_t4_t5.js —— T4（文案统一 + 缺失显示 `—`）与 T5（Token 统一剩余时间）的浏览器实测。
//
// # 判据全部取自**渲染结果**，不读源码推断
//
// 本项目的教训（第 10 次）：因为一个字符的判据错误去"修"一个正常功能。
// 所以这里全部用真 Chrome + 真 manifest + 真 /admin/accounts 做判据。
//
// # 验 4 组
//
//   T4-A 额度列：两个上游都渲染出**真值**（codearts 7474 / workbuddy 有数）
//   T4-B 缺失显示：has_data=false → `—`；has_data=true + credits=0 → `0`
//        （两个方向都要验 —— 只验一个的话，"永远显示 —"的实现也能通过）
//   T4-C 文案：可见文本里不再有裸「积分」
//   T5   Token 列：两上游**格式一致**；未知渲染成 `—` 而不是 `0d`
//
// # 为什么"没查过"这条要用注入而不是改后端数据
//
// 改 data/state.json 会**真的**改动产品数据（且要重启实例）—— 一次测量污染了被测对象。
// 这里改成喂给 renderAccounts 一份**真实回执 + 一个账号 has_data=false** 的输入。
// 走的仍是完全真实的渲染管线（renderAccounts → accountRow → quotaCellHTML），
// 只是输入换了一份。而"输入换掉后输出对不对"正是 T4 要证明的东西。
//
// ⚠ 注入的 JS 一律用**字符串拼接**，不用模板字面量：
// 页面脚本里出现 ${...} 时会被**外层** JS 先插值掉，送到浏览器的是残缺代码，
// 报出来是 `SyntaxError: missing ) after argument list` —— 看起来像页面坏了，
// 其实是**测量工具**坏了（与 run_frontend_suites.js 的反引号门禁同一类问题）。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9317;
const TARGET = process.env.T45_URL || 'http://127.0.0.1:18080/ui';
const API = TARGET.replace(/\/ui$/, '');
// ⚠ 同 shot_accounts.js：不得硬编码真实密钥（详见该文件的注释）。
const KEY = process.env.WB2API_KEY;
if (!KEY) {
  console.error('缺少 WB2API_KEY 环境变量。本文件不得硬编码真实密钥。');
  process.exit(1);
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

function get(u, h) {
  return new Promise((res, rej) => {
    http.get(u, { headers: h || {} }, r => {
      const c = [];
      r.on('data', x => c.push(Buffer.isBuffer(x) ? x : Buffer.from(x)));
      r.on('end', () => res(Buffer.concat(c).toString('utf8')));
    }).on('error', rej);
  });
}
const api = async p => JSON.parse(await get(API + p, { Authorization: 'Bearer ' + KEY }));

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  // ⚠ 用**带时间戳的**独立 profile 目录，并且清理失败时**不抛错**。
  //
  // 实测踩到：固定路径 `chrome-t45` 在上一轮 Chrome 未完全退干净时
  // 会 rmSync EPERM —— 整个套件在第 57 行就崩了，一条断言都没跑到。
  // 那不是产品缺陷，是**测量工具的脆弱**：用固定路径就等于让两次运行互相干扰。
  // 带时间戳 = 每次运行都是干净的目录，天然免疫。
  const PROFILE = path.join(os.tmpdir(), 'chrome-t45-' + Date.now());
  try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch { /* 不阻塞 */ }
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
  let ws = null;
  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');
    ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); }
    };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(6000);
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) {
        const d = r.result.exceptionDetails;
        throw new Error('页面异常: ' + (d.exception && d.exception.description ? d.exception.description : JSON.stringify(d).slice(0, 300)));
      }
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    const real = await api('/admin/accounts');
    const REAL_JSON = JSON.stringify(real.accounts);

    // ================================================================
    console.log('=== [0] 页面基础事实 ===');
    const meta = JSON.parse(await ev(
      'JSON.stringify({'
      + ' hasW: !!window.__wb2api__,'
      + ' providers: (window.__wb2api__?window.__wb2api__.manifest().providers:[]).map(function(p){return p.id;}),'
      + ' rows: document.querySelectorAll("#accts tr.acctrow").length,'
      + ' groups: document.querySelectorAll("#accts tr.grouprow").length'
      + '})'));
    console.log('  manifest providers = ' + JSON.stringify(meta.providers));
    console.log('  账号行 = ' + meta.rows + '；分组行 = ' + meta.groups);
    ok(meta.hasW, 'window.__wb2api__ 契约层存在');
    ok(meta.rows > 0, '账号表渲染出账号行（' + meta.rows + ' 行）');

    // ================================================================
    console.log('\n=== [T4-A] 额度列读到 quota.has_data（真值渲染） ===');
    const quotaCells = JSON.parse(await ev(
      'JSON.stringify((function(){'
      + ' var out=[];'
      + ' document.querySelectorAll("#accts tr.acctrow").forEach(function(tr){'
      + '   var tds=tr.querySelectorAll("td");'
      + '   out.push({provider: tr.dataset.acctof,'
      + '     quotaCell: (tds[3]?tds[3].textContent:"").trim()});'
      + ' });'
      + ' return out;'
      + '})())'));
    quotaCells.forEach(c => console.log('    ' + String(c.provider).padEnd(11) + ' 额度列 = ' + JSON.stringify(c.quotaCell)));

    let weird = 0;
    for (const c of quotaCells) {
      if (c.quotaCell === 'undefined' || c.quotaCell === '' || /NaN/.test(c.quotaCell)) weird++;
    }
    ok(weird === 0, '没有单元格渲染成 undefined / 空 / NaN（' + weird + ' 处）');

    const caApi = (real.accounts || []).find(a => a.provider === 'codearts');
    const caRender = quotaCells.find(c => c.provider === 'codearts');
    if (caRender && caApi) {
      console.log('    codearts: 界面 = ' + JSON.stringify(caRender.quotaCell)
        + ' / 后端 credits = ' + JSON.stringify(caApi.credits)
        + ' / has_data = ' + caApi.quota.has_data);
      ok(caRender.quotaCell === String(caApi.credits),
        'codearts 额度列 == 后端 credits（' + caApi.credits + '）—— 不再是旧的"空/0"');
    } else {
      console.log('    SKIP 本实例没有 codearts 账号行');
    }
    const wbApis = (real.accounts || []).filter(a => a.provider === 'workbuddy');
    const wbRenders = quotaCells.filter(c => c.provider === 'workbuddy');
    if (wbRenders.length && wbApis.length) {
      let allOk = true;
      for (const a of wbApis) if (!wbRenders.find(x => x.quotaCell === String(a.credits))) allOk = false;
      console.log('    workbuddy 后端额度: ' + JSON.stringify(wbApis.map(a => a.credits)));
      console.log('    workbuddy 界面渲染: ' + JSON.stringify(wbRenders.map(x => x.quotaCell)));
      ok(allOk, '每个 workbuddy 账号的额度都在表里有对应的渲染值（回归）');
    }

    // ================================================================
    console.log('\n=== [T4-B] 「没查过」与「查到 0」必须能区分 ===');

    // ⚠ 必须**按 uid 定位那一行**，不能取 `tr.acctrow` 的第一个。
    //
    // 实测踩到的坑：账号表是**按上游分组**渲染的（groupAccountsByProvider），
    // 组的顺序由 manifest 决定，不是"我改的那个账号排第一"。
    // 第一版拿了 `document.querySelector('#accts tr.acctrow')`，
    // 结果读到的是**另一个账号**（workbuddy 的 1116），
    // 于是断言报"has_data=false → 显示 1116" —— 一个**测量错误**，
    // 看起来却像产品缺陷。这与本仓 verify_account_buttons.js 里那条
    // "按钮顺序是设计决策"的教训同源：**测量必须钉到被测对象上**。
    //
    // 定位方式：账号行没有 data-uid 属性，但 UID 列显示了前 8 位
    //（`${esc(U.slice(0,8))}…`），用它匹配即可，且不依赖行序。
    const locator = (uid) => '(function(){'
      + ' var want=' + JSON.stringify(String(uid).slice(0, 8)) + ';'
      + ' var rows=document.querySelectorAll("#accts tr.acctrow");'
      + ' for (var i=0;i<rows.length;i++){'
      + '   var tds=rows[i].querySelectorAll("td");'
      + '   var uidCell=(tds[2]?tds[2].textContent:"").trim();'
      + '   if (uidCell.indexOf(want)===0) return rows[i];'
      + ' }'
      + ' return null;'
      + '})()';

    // B1：has_data=false（没查过）→ 必须 `—`
    //
    // credits 一并置 0：这正是后端"没查过"时的真实形态
    //（QuotaView.Effective() 对 HasData=false 返回 0，omitempty 让 remaining 消失）。
    // 只改 has_data 而留着 credits=7474 就测不到"0 与 — 的区分"—— 那才是 T4 的核心。
    const TARGET_UID = (real.accounts || [])[0] && real.accounts[0].uid;
    const inj = JSON.parse(await ev(
      '(function(){'
      + ' var W=window.__wb2api__;'
      + ' var real=' + REAL_JSON + ';'
      + ' if(!real.length) return JSON.stringify({target:null});'
      + ' var t=JSON.parse(JSON.stringify(real[0]));'
      + ' t.quota=Object.assign({}, t.quota||{}, {has_data:false});'
      + ' delete t.quota.remaining;'
      + ' t.credits=0;'
      + ' var modified=[t].concat(JSON.parse(JSON.stringify(real.slice(1))));'
      + ' W.renderAccounts(modified);'
      + ' var tr=' + locator(TARGET_UID) + ';'
      + ' return JSON.stringify({target:t.uid, found:!!tr,'
      + '   text: tr?tr.querySelectorAll("td")[3].textContent.trim():null,'
      + '   html: tr?tr.querySelectorAll("td")[3].innerHTML:null});'
      + '})()'));
    console.log('    注入账号 = ' + String(inj.target).slice(0, 8) + '…（' + inj.target + '）');
    console.log('    按 UID 定位到行 = ' + inj.found);
    console.log('    额度格实际文本 = ' + JSON.stringify(inj.text));
    console.log('    额度格实际 HTML = ' + JSON.stringify(inj.html));
    ok(inj.found, '能按 UID 定位到被注入的那一行（否则下面的断言测的不是它）');
    ok(inj.text === '—', 'has_data=false → 显示 `—`（实际 ' + JSON.stringify(inj.text) + '）');
    ok(inj.text !== '0', 'has_data=false **不**显示 `0`（T4 要修的正是这个）');

    // B1b：同一张表里 has_data=true 的行**仍有数字**
    // —— 证明判据是**逐行**的，不是"整列一刀切变成 —"
    const colAfter = JSON.parse(await ev(
      'JSON.stringify(Array.prototype.map.call('
      + ' document.querySelectorAll("#accts tr.acctrow"),'
      + ' function(tr){ return tr.querySelectorAll("td")[3].textContent.trim(); }))'));
    console.log('    注入后整列 = ' + JSON.stringify(colAfter));
    ok(colAfter.filter(x => /^[0-9]+$/.test(x)).length > 0,
      '同一张表里 has_data=true 的行**仍有数字**（判据逐行生效，不是整列一刀切）');

    // B2：has_data=true + credits=0（**查到 0**）→ 必须 `0`，不是 `—`
    //
    // 这是"能区分"的另一半。少了它，一个"永远显示 —"的实现也能让 B1 通过。
    const zero = JSON.parse(await ev(
      '(function(){'
      + ' var W=window.__wb2api__;'
      + ' var real=' + REAL_JSON + ';'
      + ' var t=JSON.parse(JSON.stringify(real[0]));'
      + ' t.quota={kind:"credits", has_data:true};'
      + ' t.credits=0;'
      + ' var modified=[t].concat(JSON.parse(JSON.stringify(real.slice(1))));'
      + ' W.renderAccounts(modified);'
      + ' var tr=' + locator(TARGET_UID) + ';'
      + ' return JSON.stringify({found:!!tr,'
      + '   text: tr?tr.querySelectorAll("td")[3].textContent.trim():null});'
      + '})()'));
    console.log('    has_data=true + credits=0 → 实际 = ' + JSON.stringify(zero.text));
    ok(zero.found && zero.text === '0', 'has_data=true 且 credits=0 → 显示 `0`（「查到 0」不能被当成「没查过」）');

    // B3：缺 quota 字段（老后端 / 回落路径）→ 退回 credits，不误判成缺数据
    const noQuota = JSON.parse(await ev(
      '(function(){'
      + ' return JSON.stringify({'
      + '   missing: window.__wb2api__.quotaCellHTML({credits:42}),'
      + '   realZero: window.__wb2api__.quotaCellHTML({credits:0, quota:{has_data:true}}),'
      + '   unknown: window.__wb2api__.quotaCellHTML({credits:0, quota:{has_data:false}})'
      + ' });'
      + '})()'));
    console.log('    quotaCellHTML 三种输入: ' + JSON.stringify(noQuota));
    ok(/42/.test(noQuota.missing), '没有 quota 字段时回落到 credits（显示 42，不误判成缺数据）');
    ok(/0/.test(noQuota.realZero) && !/—/.test(noQuota.realZero), 'has_data=true + 0 → `0`');
    ok(/—/.test(noQuota.unknown), 'has_data=false → `—`');

    // ================================================================
    console.log('\n=== [T4-C] 可见文本里不再有裸「积分」 ===');
    await ev('window.__wb2api__.renderAccounts(' + REAL_JSON + ')');
    await sleep(400);
    const scan = JSON.parse(await ev(
      'JSON.stringify((function(){'
      + ' var out={hits:[],attrs:[]};'
      + ' var re=/积分/;'
      + ' var w=document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT, null);'
      + ' var n;'
      + ' while((n=w.nextNode())){'
      + '   var v=(n.nodeValue||"");'
      + '   if(re.test(v)){'
      + '     var el=n.parentElement;'
      + '     out.hits.push({text:v.replace(/\\s+/g," ").trim().slice(0,60),'
      + '       tag:el?el.tagName:"?", id:el?(el.id||""):"",'
      + '       visible:!!(el&&el.offsetParent!==null)});'
      + '   }'
      + ' }'
      + ' document.querySelectorAll("[title],[placeholder]").forEach(function(el){'
      + '   var s=(el.getAttribute("title")||"")+" "+(el.getAttribute("placeholder")||"");'
      + '   if(re.test(s)) out.attrs.push({tag:el.tagName, id:el.id||"", v:s.trim().slice(0,60)});'
      + ' });'
      + ' return out;'
      + '})())'));
    console.log('    含「积分」的文本节点: ' + scan.hits.length);
    scan.hits.forEach(h => console.log('      [' + (h.visible ? '可见' : '隐藏') + '] <' + h.tag + (h.id ? '#' + h.id : '') + '> "' + h.text + '"'));
    console.log('    title/placeholder 里含「积分」的: ' + scan.attrs.length);
    scan.attrs.forEach(h => console.log('      <' + h.tag + (h.id ? '#' + h.id : '') + '> "' + h.v + '"'));
    ok(scan.hits.filter(h => h.visible).length === 0,
      '当前可见文本里没有「积分」（' + scan.hits.filter(h => h.visible).length + ' 处可见）');
    ok(scan.attrs.length === 0, '没有 title/placeholder 里还写着「积分」');

    // ================================================================
    console.log('\n=== [T5] Token 列：两上游格式统一 ===');
    const tokCols = JSON.parse(await ev(
      'JSON.stringify((function(){'
      + ' var out=[];'
      + ' document.querySelectorAll("#accts tr.acctrow").forEach(function(tr){'
      + '   var tds=tr.querySelectorAll("td");'
      + '   var pill=tds[5]?tds[5].querySelector(".pill"):null;'
      + '   out.push({provider:tr.dataset.acctof,'
      + '     token:(tds[5]?tds[5].textContent:"").trim(),'
      + '     cls: pill?pill.className:"(无 pill)"});'
      + ' });'
      + ' return out;'
      + '})())'));
    tokCols.forEach(c => console.log('    ' + String(c.provider).padEnd(11) + ' Token列 = ' + JSON.stringify(c.token) + '  ' + c.cls));

    const FMT = /^(\d+ 天|\d+ 小时|\d+ 分钟|已过期|—)$/;
    const badFmt = tokCols.filter(c => !FMT.test(c.token));
    ok(badFmt.length === 0,
      '每个 Token 格都符合统一格式（N 天 / N 小时 / N 分钟 / 已过期 / —）'
      + (badFmt.length ? ' —— 越界: ' + JSON.stringify(badFmt) : ''));
    const oldFmt = tokCols.filter(c => /^\d+d$/.test(c.token));
    ok(oldFmt.length === 0, '没有旧的 `Nd` 格式残留（' + JSON.stringify(oldFmt.map(x => x.token)) + '）');

    if (caRender && caApi) {
      console.log('    codearts has_token = ' + caApi.has_token
        + ' / token_expire_sec = ' + (caApi.token_expire_sec === undefined ? '(字段不存在=未知)' : caApi.token_expire_sec));
      const caTok = tokCols.find(c => c.provider === 'codearts');
      ok(caTok && caTok.token === '—', 'codearts 的 Token 列 = `—`（未知），实际 ' + JSON.stringify(caTok && caTok.token));
    }
    const wbTok = tokCols.filter(c => c.provider === 'workbuddy');
    if (wbTok.length) {
      console.log('    workbuddy 后端 token_expire_sec = ' + JSON.stringify(wbApis.map(a => a.token_expire_sec)));
      console.log('    workbuddy Token 列 = ' + JSON.stringify(wbTok.map(x => x.token)));
      ok(wbTok.every(x => /^(\d+ 天|\d+ 小时|\d+ 分钟|已过期)$/.test(x.token)),
        'workbuddy 的 Token 列是 N 天/N 小时/N 分钟 之一（实测约 59 天 → 应为「59 天」）');
    }

    // ---- fmtTokenRemain 五档逐一验证（调页面里的真函数）----
    console.log('\n  --- fmtTokenRemain 五档（调页面里的真函数） ---');
    const cases = [
      [3 * 86400, '3 天'], [86400, '1 天'], [86400 - 1, '23 小时'],
      [5 * 3600, '5 小时'], [3600, '1 小时'], [3600 - 1, '59 分钟'],
      [28 * 60, '28 分钟'], [60, '1 分钟'], [0, '0 分钟'],
      [-1, '已过期'], [undefined, '—'], [null, '—'],
    ];
    const got = JSON.parse(await ev(
      'JSON.stringify(' + JSON.stringify(cases.map(c => c[0])) + '.map(function(v){'
      + ' return window.__wb2api__.fmtTokenRemain(v); }))'));
    let fmtFail = 0;
    cases.forEach((c, i) => {
      const pass = got[i] === c[1];
      if (!pass) fmtFail++;
      console.log('      ' + (pass ? 'PASS' : 'FAIL') + ' fmtTokenRemain(' + JSON.stringify(c[0]) + ') = '
        + JSON.stringify(got[i]) + '（期望 ' + JSON.stringify(c[1]) + '）');
    });
    ok(fmtFail === 0, 'fmtTokenRemain 五档全部符合规格（' + (cases.length - fmtFail) + '/' + cases.length + '）');

    // ---- 未知 vs 0 秒：必须给出不同结果（T5 的核心）----
    const undef = await ev('window.__wb2api__.fmtTokenRemain(undefined)');
    const zeroSec = await ev('window.__wb2api__.fmtTokenRemain(0)');
    console.log('      fmtTokenRemain(undefined) = ' + JSON.stringify(undef) + '；fmtTokenRemain(0) = ' + JSON.stringify(zeroSec));
    ok(undef !== zeroSec,
      '「未知」与「0 秒」渲染成**不同**的东西（' + JSON.stringify(undef) + ' vs ' + JSON.stringify(zeroSec)
      + '）—— 改造前 `|| 0` 让它们都是 0d');

    // ---- 变异验证（前端侧）：把 fmtTokenRemain 换成旧实现，断言必须红 ----
    console.log('\n=== [M] 变异验证（页面内替换函数，不动源码） ===');
    const mutResult = JSON.parse(await ev(
      '(function(){'
      + ' var W=window.__wb2api__;'
      + ' var orig=W.fmtTokenRemain;'
      + ' var old=function(sec){ var d=sec||0;'
      + '   return (d<0)?"已过期":(Math.floor(d/86400)+"d"); };'
      + ' return JSON.stringify({'
      + '   mutant_undefined: old(undefined),'
      + '   mutant_zero: old(0),'
      + '   mutant_5h: old(5*3600),'
      + '   real_undefined: orig(undefined),'
      + '   real_zero: orig(0),'
      + '   real_5h: orig(5*3600)'
      + ' });'
      + '})()'));
    console.log('    旧实现: undefined=' + JSON.stringify(mutResult.mutant_undefined)
      + ' 0=' + JSON.stringify(mutResult.mutant_zero)
      + ' 5h=' + JSON.stringify(mutResult.mutant_5h));
    console.log('    现实现: undefined=' + JSON.stringify(mutResult.real_undefined)
      + ' 0=' + JSON.stringify(mutResult.real_zero)
      + ' 5h=' + JSON.stringify(mutResult.real_5h));
    ok(mutResult.mutant_undefined === '0d',
      '变异体确实复现了旧缺陷（undefined → `0d`）—— 说明这个变异是有效的');
    ok(mutResult.mutant_undefined === mutResult.mutant_zero,
      '旧实现下「未知」与「0 秒」**不可区分**（都是 0d）—— 这正是 T5 要修的东西');
    ok(mutResult.real_undefined !== mutResult.real_zero,
      '现实现下两者**可区分**（' + JSON.stringify(mutResult.real_undefined) + ' vs ' + JSON.stringify(mutResult.real_zero) + '）');

    // ---- 变异 2：把 quotaCellHTML 换成旧的裸渲染 ----
    const mutQ = JSON.parse(await ev(
      '(function(){'
      + ' var W=window.__wb2api__;'
      + ' var old=function(a){ return String(a.credits == null ? "" : a.credits); };'
      + ' return JSON.stringify({'
      + '   mutant_unknown: old({credits:0, quota:{has_data:false}}),'
      + '   mutant_realzero: old({credits:0, quota:{has_data:true}}),'
      + '   real_unknown: W.quotaCellHTML({credits:0, quota:{has_data:false}}),'
      + '   real_realzero: W.quotaCellHTML({credits:0, quota:{has_data:true}})'
      + ' });'
      + '})()'));
    console.log('    旧实现: has_data=false→' + JSON.stringify(mutQ.mutant_unknown)
      + ' has_data=true,0→' + JSON.stringify(mutQ.mutant_realzero));
    console.log('    现实现: has_data=false→' + JSON.stringify(mutQ.real_unknown)
      + ' has_data=true,0→' + JSON.stringify(mutQ.real_realzero));
    ok(mutQ.mutant_unknown === '0',
      '变异体确实复现了旧缺陷（has_data=false → `0`）—— 这就是"没查过显示 0"的根因');
    ok(/—/.test(mutQ.real_unknown) && /0/.test(mutQ.real_realzero),
      '现实现把两者分开了：没查过 → `—`，查到 0 → `0`');

    // ================================================================
    console.log('\n=== [健康] 无脚本错误 ===');
    const errs = await ev('JSON.stringify(window.__errs || [])');
    console.log('    window.__errs = ' + errs);
    ok(errs === '[]' || errs === undefined || errs === 'undefined', '无未捕获脚本错误');

  } catch (e) {
    console.log('EXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { if (ws) ws.close(); } catch { }
    try { ch.kill(); } catch { }
  }
  console.log(fail === 0 ? '\n=== T4+T5 浏览器实测通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
