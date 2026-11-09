// verify_cred_dom_leak.js —— 门禁：**掩码态下，完整凭据不得出现在 DOM 里** + **凭据块只保留管理令牌一行**
//
// # 这条判据在防什么（不变量）
//
// 令牌页（/ui/tokens）的「系统凭据」卡片默认显示掩码、点「显示」才给明文。
// 如果掩码态就把完整值写进 DOM（例如塞进 `data-full="…"` 属性），那么：
//   · 打开 DevTools 元素面板 —— 一眼看到完整 token，**不需要点任何按钮**；
//   · 「显示」按钮退化成**纯视觉效果，没有任何安全含义**（它挡不住任何人）。
// ⇒ 正确的形态：完整值留在 JS 变量里，只有用户**主动点开**那一刻才写进 DOM 的文本节点。
//
// 另按用户 2026-11-09 的要求：「只要一个管理令牌」⇒ `api_key` 那一行**不再渲染**，
// 完整 api_key 因此**根本不该出现在这一页的 DOM 里**（后端字段照旧返回，见 [A] 段）。
//
// # 为什么必须是一条可重复运行的探针（而不是一次性 Playwright 脚本）
//
// 这是本页唯一能证明"显示/隐藏真的有意义"的判据。写成一次性脚本，
// 下次有人为省事把值放回属性里，没有任何东西会红。
//
// # 判据来源（实现即契约）
//
//   webui-tokens.html 的「系统凭据」IIFE（`#credRows`）：
//     · 掩码 `<code data-cred-slot="admin">` 的文本 = 掩码（未点开时）
//     · 完整值来自 `/admin/apikeys` 的 `admin_token_full`
//       （internal/admin/apikeys.go:111/116；只有管理员会话才有值）
//     · 「显示/隐藏」[data-rev]、「复制」[data-cp]、「轮换」[data-rot]
//
// # 用法
//
//   node tests/frontend/verify_cred_dom_leak.js
//
// 自带隔离实例（现场探测空闲端口，**绝不碰 7863**）+ 仓库外临时目录 + Chrome。
// 退出码：0 全绿 / 1 有断言红 / 2 环境前提不成立。
//
// ⚠ 改了 internal/server/*.html 必须**重建探针二进制**（那些页面走 //go:embed，
//   否则浏览器拿到的是内嵌的旧页面，最容易误判成前端 bug）。
//
// ⚠ 必须在 require(lib_instance) **之前**设 WB2API_EXE：那个模块在求值时就把 EXE 定死。
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
'use strict';
const net = require('net');
const os = require('os');
const path = require('path');
const { startInstance } = require('./lib_instance.js');
const { withChrome } = require('./lib_cdp.js');

function freePort() {
  return new Promise((res, rej) => {
    const s = net.createServer();
    s.on('error', rej);
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); });
  });
}

// ⚠ 两把**故意不同**：只有不同才能分辨"漏的是哪一把"
//   （探针实例默认把 api_key 与 admin_token 设成同一个值，见 lib_instance.js:323-336）
const ADMIN_TOKEN = 'cred-dom-probe-admin-token-0001';
const API_KEY = 'cred-dom-probe-api-key-0002';

// 在 iframe 里取样。
// ⚠ 只在这里做**一次** JSON.stringify（外层）；里面一律返回**裸对象**。
//   早先我两层都 stringify ⇒ JSON.parse 得到字符串 ⇒ 各字段 undefined ⇒
//   断言 `!undefined` 恒真 ⇒ 全部假绿。这类"假绿"比红更危险，故留此注记。
function sampleExpr() {
  return 'JSON.stringify((function(){'
    + 'var f=document.getElementById("tkFrame"); var d=f&&f.contentDocument;'
    + 'if(!d||!d.body) return {err:"no-iframe"};'
    + 'var box=d.getElementById("credRows"); if(!box) return {err:"no-credRows"};'
    + 'var html=d.body.outerHTML;'
    + 'var codes=Array.prototype.map.call(box.querySelectorAll("code[data-cred-slot]"),function(c){'
    + '  return {slot:c.getAttribute("data-cred-slot"),text:c.textContent}; });'
    + 'var attrLeak=0;'
    + 'Array.prototype.forEach.call(box.querySelectorAll("code"),function(c){'
    + '  Array.prototype.forEach.call(c.attributes,function(a){ if(a.value.indexOf("cred-dom-probe-")>=0) attrLeak++; }); });'
    + 'var h=box.querySelector(".credhd");'
    + 'return {'
    + '  boxHtml: box.outerHTML,'
    + '  hasAdminToken: html.indexOf(' + JSON.stringify(ADMIN_TOKEN) + ')>=0,'
    + '  hasApiKey: html.indexOf(' + JSON.stringify(API_KEY) + ')>=0,'
    + '  valueInAttribute: attrLeak>0,'
    + '  codes: codes,'
    + '  rev: box.querySelectorAll("[data-rev]").length,'
    + '  cp: box.querySelectorAll("[data-cp]").length,'
    + '  rot: box.querySelectorAll("[data-rot]").length,'
    + '  hasDialogKeyRow: box.textContent.indexOf("对话密钥")>=0,'
    + '  hasApiKeyWord: box.textContent.indexOf("api_key")>=0,'
    + '  revealsUnconfigured: box.textContent.indexOf("未配置")>=0,'
    + '  boxText: box.textContent'
    + '};})())';
}

const clickExpr = (sel) => '(function(){var d=document.getElementById("tkFrame").contentDocument;'
  + 'var b=d.querySelector("#credRows ' + sel + '"); if(!b) return "no-btn"; b.click(); return "ok";})()';

(async () => {
  let fail = 0, skip = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
  const sk = (m) => { console.log('  SKIP ' + m); skip++; };

  console.log('【判据】掩码态 DOM 不含完整凭据；凭据块只保留「管理令牌」一行');
  const PORT = await freePort();
  console.log('实例端口（现场探测）: ' + PORT + '（绝不是 7863）');
  console.log('两把凭据故意不同：admin_token=' + ADMIN_TOKEN.slice(0, 14) + '… api_key=' + API_KEY.slice(0, 14) + '…');

  let inst;
  try {
    inst = await startInstance({ port: PORT, apiKey: API_KEY, config: { admin_token: ADMIN_TOKEN } });
  } catch (e) {
    console.error('\n环境前提不成立：' + (e && e.message));
    console.error('（先构建探针：go build -o bin/wb2api-server-probe.exe ./cmd/server）');
    process.exit(2);
  }
  const base = inst.base;

  // ── [A] 前置：后端仍然给完整值（证明"只删展示、不删能力"）
  console.log('\n[A] 前置：/admin/apikeys 仍然返回完整值（证明"只删前端展示，不删后端能力"）');
  const j = await (await fetch(base + '/admin/apikeys', { headers: { Authorization: 'Bearer ' + ADMIN_TOKEN } })).json();
  ok(!!j.admin_token_full && j.admin_token_full === ADMIN_TOKEN,
    '回执里有 admin_token_full 且值正确（' + String(j.admin_token_full || '(空)').slice(0, 12) + '…）');
  ok(!!j.admin_key && !!j.admin_key_full,
    '回执里**仍然**有 admin_key 掩码与 admin_key_full（掩码=' + String(j.admin_key || '(空)') + '）—— 后端字段一个没删');
  ok(j.admin_key_full === API_KEY && ADMIN_TOKEN !== API_KEY,
    'admin_key_full 与 admin_token 是**两把不同的**钥匙（能分辨哪一把漏了）');

  const CH_PORT = await freePort();
  let exited = false;
  await withChrome({
    port: CH_PORT, url: base + '/ui', token: ADMIN_TOKEN, width: 1500, height: 1000,
    outDir: path.join(os.tmpdir(), 'cred-dom-shots'),
    readyExpr: 'document.readyState === "complete"',
  }, async ({ ev, sleep, errs }) => {
    await sleep(2500);

    console.log('\n[B] 进入令牌面板');
    const switched = await ev('(function(){ if(!window.__wb2api__||!window.__wb2api__.showPanelByKey) return "no-hook";'
      + 'return window.__wb2api__.showPanelByKey("tokens")?"ok":"no-section";})()');
    ok(switched === 'ok', '面板切到 data-key="tokens"（实际 ' + switched + '）');
    await sleep(1200);

    const s1 = JSON.parse(await ev(sampleExpr()));
    if (s1.err) { sk('取不到凭据区（' + s1.err + '）⇒ 本条测不到'); return; }

    console.log('\n[C] 凭据块只剩一行「管理令牌」，且 api_key 不出现');
    ok(!s1.hasDialogKeyRow, '★ 凭据块里**不含**「对话密钥」字样');
    ok(!s1.hasApiKeyWord, '★ 凭据块里**不含**「api_key」字样');
    ok(s1.rev === 1 && s1.cp === 1 && s1.rot === 1,
      '★ 恰好一个「显示」/一个「复制」/一个「轮换」（实际 ' + s1.rev + '/' + s1.cp + '/' + s1.rot + '）');
    ok(s1.codes.length === 1 && s1.codes[0].slot === 'admin',
      '恰好一个凭据槽且 slot=admin（实际 ' + JSON.stringify(s1.codes.map(c => c.slot)) + '）');
    ok(!s1.revealsUnconfigured, '不出现「未配置」（管理令牌已配置）');

    console.log('\n[D] ★ 核心不变量：掩码态 DOM 不含完整值');
    ok(!s1.hasAdminToken, '★ 掩码态：iframe DOM 里**不含**完整 admin_token');
    ok(!s1.hasApiKey, '★ 掩码态：iframe DOM 里**不含**完整 api_key（那一行已删，更不该有）');
    ok(!s1.valueInAttribute, '掩码态：凭据 code 的属性值里**没有**完整值（不是塞在 data-* 里）');
    // ⚠ 必须守卫：红的时候槽可能一个都没有（旧实现用的是 data-mask/data-full，
    //   没有 data-cred-slot）⇒ 直接读 codes[0].text 会抛异常、把后面的断言全吞掉。
    const c0 = s1.codes[0] || { text: '(没有槽)' };
    ok(s1.codes.length === 1 && c0.text.indexOf('cred-dom-probe') < 0,
      '槽的**文本**是掩码（' + c0.text + '）');

    console.log('\n[E] 点「显示」→ 明文才进 DOM；点「隐藏」→ 又退出 DOM');
    ok((await ev(clickExpr('[data-rev]'))) === 'ok', '点得到「显示」按钮');
    await sleep(350);
    const s2 = JSON.parse(await ev(sampleExpr()));
    ok(s2.hasAdminToken, '★ 点开后 DOM 里**出现**完整 admin_token（按钮真的在起作用）');
    ok((s2.codes[0] || {}).text === ADMIN_TOKEN,
      '槽的 textContent == 完整值（长度 ' + String((s2.codes[0] || {}).text || '').length + '）');

    ok((await ev(clickExpr('[data-rev]'))) === 'ok', '点得到「隐藏」按钮');
    await sleep(350);
    const s3 = JSON.parse(await ev(sampleExpr()));
    ok(!s3.hasAdminToken, '★ 点「隐藏」后 DOM 里**又不含**完整 admin_token');
    ok((s3.codes[0] || {}).text !== ADMIN_TOKEN, '槽回到掩码（' + String((s3.codes[0] || {}).text || '').slice(0, 14) + '…）');

    console.log('\n[F] 点「复制」→ 送进剪贴板的是完整值（值来自内存，与 DOM 无关）');
    // ⚠ 读剪贴板在 CDP 下常被挡住（实测 readText 抛
    //   "Failed to execute \'readText\' on \'Clipboard\': Document is not focused"）。
    //   所以主判据换成**截获 writeText 的入参** —— 它直接回答"送进剪贴板的是什么"，
    //   比读回来更贴近要断言的东西，也不受焦点/权限影响。
    //   ⚠ 补丁必须打在**iframe 的 window** 上：`ev()` 在**顶层文档**求值，
    //     而 `navigator.clipboard` 是**每个 frame 各自一个对象** ——
    //     给顶层的打补丁，iframe 里的按钮照样调它自己那个（实测因此假红过一次）。
    const patched = await ev('(function(){ try { var w = document.getElementById("tkFrame").contentWindow;'
      + 'w.__copied = null;'
      + 'var c = w.navigator.clipboard; if(!c || !c.writeText) return "no-clip";'
      + 'c.writeText = function(t){ w.__copied = String(t); return Promise.resolve(); };'
      + 'return "patched"; } catch(e){ return "ERR:" + e.message; } })()');
    ok((await ev(clickExpr('[data-cp]'))) === 'ok', '点得到「复制」按钮');
    await sleep(450);
    const got = await ev('(function(){ try { var w = document.getElementById("tkFrame").contentWindow;'
      + 'return w.__copied === undefined ? null : w.__copied; } catch(e){ return null; } })()');
    if (patched === 'patched') {
      ok(got === ADMIN_TOKEN, '★ 送进剪贴板的字符串 == 完整 admin_token（长度 '
        + String(got === null ? 0 : got.length) + '）');
    } else {
      sk('截获 writeText 失败（' + String(patched).slice(0, 40) + '）⇒ 复制路径未验');
    }

    console.log('\n[G] 卫生：无未捕获异常 / console.error');
    const errList = errs || [];
    ok(errList.length === 0, '全程无未捕获异常 / console.error（实际 ' + errList.length + ' 条'
      + (errList.length ? '：' + JSON.stringify(errList.slice(0, 3)) : '') + '）');
  });

  try { await inst.stop(); } catch (e) { /* 已停 */ }

  console.log('\n' + (fail ? '=== 凭据 DOM 门禁：红（' + fail + ' 条失败）===' : '=== 凭据 DOM 门禁：绿 ===')
    + (skip ? '（另有 ' + skip + ' 项 SKIP）' : ''));
  if (!exited) { exited = true; process.exit(fail ? 1 : 0); }
})().catch((e) => { console.error('探针异常：' + (e && e.stack || e)); process.exit(1); });
