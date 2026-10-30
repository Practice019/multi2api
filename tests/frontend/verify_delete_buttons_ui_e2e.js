// verify_delete_buttons_ui_e2e.js —— 在**真浏览器**里点这两个删除按钮（2026-10-19）。
//
// # 为什么还要这一层（HTTP 探针已经测过后端了）
//
// `verify_delete_flow_e2e.js` 直接打 API，验的是**后端**行为。但用户报的是
// 「点了移除没反应」—— 那是**界面上**的事，中间还隔着：
//
//	确认弹窗(confirm) → busyRun 的 busy 态 → 事件委托把 data-act 分派到 act()
//	→ 拼 URL(?purge_file=1) → 回执处理 → toast 文案 → 刷新列表 → 重画表格
//
// 任何一环断掉都表现成"点了没反应"，而 HTTP 探针看不到这些。
//
// # 只点**销毁性**的两个按钮
//
// 全应用的删除按钮只有两个（枚举见 verify_delete_flow_e2e.js 的文件头）：
//
//	① 账号池 · 移除（data-act="remove"）
//	② API 管理 · 删除（data-act="delete"）
//
// 其余按钮本文件不碰 —— 它们不改数据，且已有别的套件覆盖。
//
// # 三个坑（都固化成仪器了）
//
//	① `confirm()` 在 headless 里没人处理会卡住页面 / 被自动取消 ⇒ 请求压根不发，
//	   看起来像"按钮坏了"。所以 lib_cdp 默认**自动接受**并把文案收集起来，
//	   本文件顺带断言"弹的是哪句确认"。
//	② **toast 会被后续操作覆盖**。第一版只读一次 `#toast`，读到的是
//	   删除流程里另一条（失败）提示 —— 于是"成功提示"这条断言看的是别人的文案。
//	   ⇒ 装一个 150ms 的采样器，把整段时间轴上的 toast 全记下来（含出现时刻）。
//	③ **"行还在"未必是"没刷"**。这一屏的 `refresh()` 要等 manifest / accounts /
//	   models / stats（单次 `/admin/stats` 实测 7s，见 webui 里的注释）——
//	   所以判据得是"**多久之后**行才消失"，不能只测一个瞬时点。
//	   ⇒ 轮询到消失，并把耗时打出来（超过 SECONDS_SLOW 就显式报警）。
//
// # 数据：全合成，零真实数据
//
// 与 HTTP 探针共用 lib_instance.js（cwd=临时目录 ⇒ ./auths、./data 都在临时目录里）。
'use strict';
const fs = require('fs');
const path = require('path');
const { startInstance, httpJson } = require('./lib_instance.js');
const { withChrome, until, showPanel, sleep } = require('./lib_cdp.js');

const PORT = Number(process.env.DELUI_PORT || 7882);
const CDP = Number(process.env.DELUI_CDP || 9382);
const KEY = 'delui-probe-' + PORT;
const S = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; // 合成 session
const U1 = '990000000000000011';              // 合成 uid
const SLOW_MS = Number(process.env.DELUI_SLOW_MS || 4000); // 超过就算"慢得用户以为坏了"

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
const info = (m) => console.log('  · ' + m);

// PATCH 记录所有请求与回执（必须在**点击之前**装好）。
const PATCH_REQ = `(function(){
  window.__REQ__ = [];
  var f = window.fetch ? window.fetch.bind(window) : null;
  if (!f) return 'no-fetch';
  window.fetch = function(u, o){
    var rec = { url: String(u), method: (o && o.method) || 'GET', t: Date.now() };
    window.__REQ__.push(rec);
    return f(u, o).then(function(r){
      rec.status = r.status; rec.done = Date.now() - rec.t;
      try { r.clone().text().then(function(t){ rec.body = String(t).slice(0, 400); }, function(){}); } catch (e) {}
      return r;
    });
  };
  return 'ok';
})()`;

// PATCH_TOAST 装 150ms 采样器：toast 会被后来的提示覆盖，只读一次会看错人。
//
// ⚠ `toast()` 不是全局函数（在闭包里），所以只能采 DOM —— 正好，采 DOM 才是
// 用户真正看到的东西。样式类也一起采（bad/err 类 = 失败提示）。
const PATCH_TOAST = `(function(){
  window.__TOASTS__ = []; window.__TOAST0__ = Date.now();
  if (window.__TOASTTIMER__) clearInterval(window.__TOASTTIMER__);
  window.__TOASTTIMER__ = setInterval(function(){
    var e = document.getElementById('toast'); if (!e) return;
    var t = (e.textContent || '').trim(); if (!t) return;
    var cls = e.className || '';
    var last = window.__TOASTS__[window.__TOASTS__.length - 1];
    if (!last || last.text !== t || last.cls !== cls) {
      window.__TOASTS__.push({ ms: Date.now() - window.__TOAST0__, text: t.slice(0, 140), cls: cls });
    }
  }, 150);
  return 'ok';
})()`;

// waitGone 轮询"某选择器命中数变 0"，返回 {ms, ok}。
//
// 判据是**多久之后**消失：这一屏的刷新链很长（manifest → accounts → models → stats），
// 瞬时值会骗人。超过 SLOW_MS 就标记成"慢"（用户会以为按钮没反应）。
async function waitGone(ev, sel, timeoutMs = 20000, stepMs = 250) {
  const t0 = Date.now();
  for (;;) {
    const n = await ev(`document.querySelectorAll(${JSON.stringify(sel)}).length`);
    if (n === 0) return { ok: true, ms: Date.now() - t0 };
    if (Date.now() - t0 > timeoutMs) return { ok: false, ms: Date.now() - t0, n };
    await sleep(stepMs);
  }
}

(async () => {
  let inst = null;
  try {
    inst = await startInstance({
      port: PORT, apiKey: KEY,
      auths: { 'loomy/loomy-ui1.json': JSON.stringify({ session: S, userid: U1 }) },
      config: { loomy: { enabled: true, auth_dir: './auths/loomy' } },
    });
    const credFile = path.join(inst.tmp, 'auths', 'loomy', 'loomy-ui1.json');
    const mk = await httpJson(inst.base, '/admin/apikeys', { method: 'POST', token: KEY, body: { name: 'ui-probe-key' } });
    info('实例 ' + inst.base + '（pid=' + inst.pid + '）· 合成凭证 ' + path.basename(credFile) + ' · 建 key → ' + mk.status);

    await withChrome({
      port: CDP, url: inst.base + '/ui', width: 1440, height: 1100,
      readyExpr: 'document.getElementById("accts") ? 1 : 0',
    }, async (ctx) => {
      const { ev, dialogs, errs, shot } = ctx;

      // ---------------- ① 账号池 · 移除 ----------------
      console.log('\n=== ① 账号池 · 移除（真点按钮）===');
      await showPanel(ev, 'accounts', 1200);
      const rowN = await until(ev, 'document.querySelectorAll("#accts tr.acctrow").length', (n) => n > 0, 20000);
      info('账号池行数 = ' + rowN);
      const hit = await ev(`document.querySelectorAll('button[data-act="remove"][data-uid="${U1}"]').length`);
      ok(hit === 1, '找得到这一行的「移除」按钮（命中 ' + hit + ' 个）');
      if (hit !== 1) { await shot('delui-no-button.png'); throw new Error('找不到按钮，后续断言无意义'); }

      info('装仪器: ' + await ev(PATCH_REQ) + ' / ' + await ev(PATCH_TOAST));
      dialogs.length = 0;

      const t0 = Date.now();
      await ev(`document.querySelector('button[data-act="remove"][data-uid="${U1}"]').click()`);
      await sleep(700);
      info('确认弹窗 ' + dialogs.length + ' 个: ' + JSON.stringify(dialogs.map((d) => d.message.split('\n')[0])));
      ok(dialogs.length >= 1, '点「移除」会弹一次确认（不弹 = 事件没接上）');
      ok(dialogs.some((d) => /彻底删除/.test(d.message)), '确认文案说的是「彻底删除」');

      const rawDel = await until(ev, 'JSON.stringify(window.__REQ__.filter(function(r){return r.method==="DELETE"}))',
        (s) => { try { return JSON.parse(s).some((r) => r.status); } catch { return false; } }, 15000);
      const del = JSON.parse(rawDel || '[]');
      info('DELETE ' + JSON.stringify(del.map((r) => ({ url: r.url, status: r.status, done: r.done + 'ms' }))));
      ok(del.length === 1, '恰好发了一次 DELETE');
      ok(!!(del[0] && del[0].url.indexOf('purge_file=1') >= 0), 'URL 带 purge_file=1（不带就只出池、文件留着会复活）');
      ok(!!(del[0] && del[0].status === 200), '回执 200');
      info('回执体: ' + (del[0] && del[0].body));

      // 行**多久**才消失（不是"是否立刻消失"）
      const gone = await waitGone(ev, `button[data-act="remove"][data-uid="${U1}"]`);
      ok(gone.ok, '这一行最终从表里消失了（耗时 ' + gone.ms + 'ms' + (gone.ok ? '' : '，超时仍剩 ' + gone.n + ' 个') + '）');
      ok(gone.ok && gone.ms <= SLOW_MS, '消失耗时 ' + gone.ms + 'ms ≤ ' + SLOW_MS + 'ms（超了用户就会觉得"点了没反应"）');
      ok(!fs.existsSync(credFile), '★ 凭证文件真的被删掉了（' + credFile + '）');

      // toast 全时间轴（谁覆盖了谁）
      const toasts = JSON.parse(await ev('JSON.stringify(window.__TOASTS__)'));
      info('toast 时间轴:');
      toasts.forEach((t) => info('   +' + t.ms + 'ms  [' + t.cls + '] ' + t.text));
      const good = toasts.find((t) => /已彻底删除/.test(t.text));
      const badG = toasts.filter((t) => /失败|没删掉|错误/.test(t.text));
      ok(!!good, '时间轴里有「已彻底删除」这条成功提示');
      ok(badG.length === 0, '删除成功时**不该**再出现失败类提示（' + badG.length + ' 条' +
        (badG.length ? '：' + badG.map((t) => t.text).join(' / ') : '') + '）');
      if (badG.length) {
        ok(!good || badG[0].ms > good.ms, '失败提示出现在成功提示**之前**（否则就是它把成功提示盖掉了 = 用户看到"删不掉"）');
      }

      const bad = errs.filter((e) => /is not defined|ReferenceError|TypeError|Cannot read/.test(e));
      ok(bad.length === 0, '页面无"代码坏了"级异常（' + bad.length + ' 条；共收集 ' + errs.length + ' 条 console 噪音）');
      if (bad.length) info('异常: ' + bad.join(' | '));
      await shot('delui-accounts-after.png');
      info('点击 → 行消失总耗时 ' + (Date.now() - t0) + 'ms');

      // ---------------- ② API 管理 · 删除 ----------------
      console.log('\n=== ② API 管理 · 删除（真点按钮）===');
      await ev('window.__REQ__ = []; window.__TOASTS__ = []; window.__TOAST0__ = Date.now();');
      // ⚠ 2026-10-30：用户报「api管理和令牌功能重叠」⇒ 令牌页成为唯一入口，旧「API 管理」
//   面板改为 hidden（DOM 与代码都保留）。本探针测的是它那张表上的删除按钮，
//   所以切之前自己把面板解锁（只动本隔离实例的 DOM，不碰产品代码）。
await ev('(() => { const s = document.querySelector(\'section[data-key="apikeys"]\'); if (s) { s.hidden = false; } })()');
await showPanel(ev, 'apikeys', 1200);
      const krows = await until(ev, 'document.querySelectorAll("#apiKeyRows button[data-act=delete]").length', (n) => n > 0, 15000);
      info('Key 行数 = ' + krows);
      ok(krows >= 1, '列表里有一把合成 key');
      dialogs.length = 0;
      const t1 = Date.now();
      await ev('document.querySelector("#apiKeyRows button[data-act=delete]").click()');
      await sleep(900);

      const raw2 = await until(ev, 'JSON.stringify(window.__REQ__)',
        (s) => { try { return JSON.parse(s).some((r) => r.url.indexOf('apikeys/delete') >= 0 && r.status); } catch { return false; } }, 15000);
      const del2 = JSON.parse(raw2 || '[]').filter((r) => r.url.indexOf('apikeys/delete') >= 0);
      info('delete ' + JSON.stringify(del2.map((r) => ({ url: r.url, status: r.status, body: r.body }))));
      ok(del2.length === 1 && del2[0].status === 200, 'apikey 删除发出且回执 200');
      ok(dialogs.length >= 1, 'apikey 删除也弹一次确认（' + (dialogs[0] ? dialogs[0].message.slice(0, 36) : '') + '）');

      const kgone = await waitGone(ev, '#apiKeyRows button[data-act=delete]');
      ok(kgone.ok, '这把 key 最终从列表里消失（耗时 ' + kgone.ms + 'ms）');
      const html = await ev('document.getElementById("apiKeyRows") ? document.getElementById("apiKeyRows").innerText.replace(/\\s+/g," ").slice(0,200) : "(无表体)"');
      info('表体内容: ' + JSON.stringify(html));
      const tk = JSON.parse(await ev('JSON.stringify(window.__TOASTS__)'));
      info('toast 时间轴:');
      tk.forEach((t) => info('   +' + t.ms + 'ms  [' + t.cls + '] ' + t.text));
      ok(!!tk.find((t) => /已删除/.test(t.text)), '时间轴里有「已删除」成功提示');
      ok(tk.filter((t) => /失败/.test(t.text)).length === 0, 'apikey 删除不该出现失败提示');
      await shot('delui-apikeys-after.png');
      info('点击 → 行消失总耗时 ' + (Date.now() - t1) + 'ms');
    });

    // 后端侧最终核对
    const lst = await httpJson(inst.base, '/admin/apikeys', { token: KEY });
    const keys = (lst.json && (lst.json.keys || lst.json.apikeys || lst.json.items)) || [];
    ok(keys.length === 0, '后端确认 apikeys 已空（剩 ' + keys.length + ' 条）');
  } catch (e) {
    console.log('  探针异常: ' + (e && e.stack ? e.stack : e));
    fail++;
  } finally {
    if (inst) inst.stop();
  }
  console.log('\n' + (fail ? '结果: ' + fail + ' 条 FAIL' : '结果: 全绿'));
  process.exit(fail ? 1 : 0);
})();
