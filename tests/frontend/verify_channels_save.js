// verify_channels_save.js —— 渠道页「点了没反应」类缺陷的回归判据。
//
// # 为什么需要它
// v1.9.15 用户点「新增渠道 → 保存」，啥都没发生，控制台里是：
//   Uncaught (in promise) ReferenceError: key is not defined   at save (channels:1595)
// 那行从 29bb9c4（T6 首次落地）就写着 `!key`，而 `key` 从未声明过。
// 之所以一年没被发现：编辑已有渠道时 `!editing` 短路为 false，那行根本不执行；
// 只有**新建**才会走到 ⇒ 静态语法门禁（verify_inline_script_syntax）也查不出（语法是对的）。
//
// 判据：真的把「新建 → 保存」走一遍，并断言
//   ① 全程没有未捕获异常（ReferenceError 是这条缺陷的指纹）
//   ② 页面里多出一行、名字就是我们填的那个
//   ③ 后端 /admin/channels 真的多了一条
//
// 用法：node verify_channels_save.js（自己起隔离实例，端口 7899）
'use strict';
const path = require('path');
const { startInstance, sleep } = require('./lib_instance.js');
const { withChrome } = require('./lib_cdp.js');

const PORT = Number(process.env.CH_PORT || 7899);
const USER = 'chprobe';
const PASS = 'chprobe2026';
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  const inst = await startInstance({ port: PORT, apiKey: 'probe-key-ch' });
  try {
    const reg = await fetch(inst.base + '/admin/auth/register', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: USER, password: PASS }),
    });
    ok(reg.status === 200, '建管理员账号（bootstrap）status=' + reg.status);
    let rowsBefore = 0;
    await withChrome({ port: 9471, url: inst.base + '/ui', token: false, width: 1400, height: 900,
      outDir: path.resolve(__dirname, '..', '..', '.task', 'maximalism-v3', 'shots'),
      readyExpr: 'document.readyState === "complete"' },
      async ({ send, ev, errs, sleep: s2 }) => {
        await ev('(async () => { const r = await fetch("/admin/auth/login", {method:"POST", headers:{"Content-Type":"application/json"}, body: JSON.stringify({username:' + JSON.stringify(USER) + ', password:' + JSON.stringify(PASS) + '})}); return r.status; })()');
        await send('Page.navigate', { url: inst.base + '/ui/channels' });
        await s2(2200);
        rowsBefore = await ev('document.querySelectorAll("#pane-ch table tbody tr").length');
        ok(typeof rowsBefore === 'number', '渠道表已渲染，当前 ' + rowsBefore + ' 行');
        // 打开新增表单
        const opened = await ev('(function(){ var b = Array.from(document.querySelectorAll("button")).find(function(x){ return x.textContent.indexOf("新增渠道") >= 0; }); if (!b) return false; b.click(); return true; })()');
        ok(opened, '点到「+ 新增渠道」');
        await s2(600);
        ok(await ev('!!document.querySelector("#dlg").open'), '表单弹窗已打开');
        // 填三个必填项
        await ev('(function(){' +
          'function set(id, v){ var e = document.getElementById(id); e.value = v; e.dispatchEvent(new Event("input", {bubbles:true})); }' +
          'set("f-name", "probe-ch");' +
          'set("f-base", "https://example.invalid/v1");' +
          'set("f-key", "sk-probe-1234567890");' +
          'return document.getElementById("f-name").value; })()');
        // 点保存
        const clicked = await ev('(function(){ var b = Array.from(document.querySelectorAll("#dlg button")).find(function(x){ return x.textContent.trim() === "保存"; }); if (!b) return false; b.click(); return true; })()');
        ok(clicked, '点到「保存」');
        await s2(1800);
        const closed = await ev('!document.querySelector("#dlg").open');
        ok(closed, '保存后弹窗关闭（没关 = 请求没发成功）');
        // ⚠ 不能拿"行数 +1"当判据：空池时 tbody 里那**一行是空态行**
        //（"还没有渠道 —— 点左上「新增渠道」…"），保存成功后它被真实数据行**替换**，
        // 行数仍然是 1。这个坑我第一版就踩了（1 -> 1 假红）。
        const emptyGone = await ev('document.querySelector("#pane-ch table tbody").textContent.indexOf("还没有渠道") < 0');
        ok(emptyGone, '空态行已被真实数据行替换（"还没有渠道"消失了）');
        const rowCount = await ev('document.querySelectorAll("#pane-ch table tbody tr").length');
        ok(rowCount >= 1, '表格里至少有 1 行（实际 ' + rowCount + '）');
        const hasName = await ev('document.querySelector("#pane-ch table tbody").textContent.indexOf("probe-ch") >= 0');
        ok(hasName, '新行里能看到刚填的名字 probe-ch');
        const dlgErr = await ev('(document.getElementById("dlgErr") || {}).textContent || ""');
        ok(!dlgErr, '表单里没有残留错误提示（实际 "' + dlgErr + '"）');
        const realErrs = errs.filter(function (e) { return !/favicon|example\.invalid/i.test(e); });
        ok(realErrs.length === 0, '全程没有未捕获异常（实际 ' + JSON.stringify(realErrs.slice(0, 3)) + '）');

        // ---- 第 2 段：自动映射（2026-10-21 用户要求）----
        // 规则：real = 模型名最后一个 / 之后；pub = 前缀 + '/' + real；前缀空 ⇒ 不生成。
        const heads = await ev('Array.from(document.querySelectorAll("#pane-ch table thead th")).map(function(t){return t.textContent.trim();})');
        ok(heads.indexOf('映射') < 0, '列表里**没有**「映射」列（实际表头：' + JSON.stringify(heads) + '）');
        ok(heads.length === 6, '表头 6 列（删列后 colspan 也要跟着变，实际 ' + heads.length + '）');
        await ev('(function(){ var b = Array.from(document.querySelectorAll("button")).find(function(x){ return x.textContent.indexOf("新增渠道") >= 0; }); b.click(); })()');
        await s2(500);
        const auto = JSON.parse(await ev('(function(){' +
          'function set(id, v){ var e = document.getElementById(id); e.value = v; e.dispatchEvent(new Event("input", {bubbles:true})); }' +
          'set("f-name", "probe-auto");' +
          'set("f-base", "https://example.invalid/v1");' +
          'set("f-key", "sk-probe-auto-1234");' +
          'set("f-slug", "qiniuyun");' +
          'set("f-models", "qiniuyun/deepseek-v4.1-flash,qiniuyun/kimi-k3-free");' +
          'var rows = Array.from(document.querySelectorAll("#f-map-rows > div"));' +
          'return JSON.stringify(rows.map(function(r){ return [r.querySelector(".m-key").value, r.querySelector(".m-val").value]; }));' +
          '})()'));
        ok(auto.length === 2, '模型栏 2 个 + 前缀 ⇒ 自动生成 2 行映射（实际 ' + JSON.stringify(auto) + '）');
        ok(auto.some(r => r[0] === 'qiniuyun/deepseek-v4.1-flash' && r[1] === 'deepseek-v4.1-flash'),
          'qiniuyun/deepseek-v4.1-flash → deepseek-v4.1-flash');
        ok(auto.some(r => r[0] === 'qiniuyun/kimi-k3-free' && r[1] === 'kimi-k3-free'),
          'qiniuyun/kimi-k3-free → kimi-k3-free');
        // 前缀留空 ⇒ 不生成（对外用原名，最不容易出事的默认）
        await ev('(function(){ var e = document.getElementById("f-slug"); e.value = ""; e.dispatchEvent(new Event("input", {bubbles:true})); })()');
        const autoEmpty = JSON.parse(await ev('Array.from(document.querySelectorAll("#f-map-rows > div")).map(function(r){ return r.querySelector(".m-key").value; }).filter(Boolean).length'));
        const afterClear = JSON.parse(await ev('JSON.stringify({' +
          'rows: Array.from(document.querySelectorAll("#f-map-rows > div")).map(function(r){ return r.querySelector(".m-key").value; }),' +
          'dirty: (typeof mapDirty === "undefined" ? "n/a" : mapDirty),' +
          'slug: document.getElementById("f-slug").value' +
        '})'));
        ok(afterClear.rows.filter(Boolean).length === 0,
          '前缀清空 ⇒ 不再有任何非空映射行（实际 rows=' + JSON.stringify(afterClear.rows) +
          ' dirty=' + afterClear.dirty + ' slug="' + afterClear.slug + '"）');
        // 恢复前缀再保存，确认后端拿到的就是自动生成的那张表
        await ev('(function(){ var e = document.getElementById("f-slug"); e.value = "qiniuyun"; e.dispatchEvent(new Event("input", {bubbles:true})); })()');
        await ev('(function(){ var b = Array.from(document.querySelectorAll("#dlg button")).find(function(x){ return x.textContent.trim() === "保存"; }); b.click(); })()');
        await s2(1800);
      });
    // 后端确认
    const login = await fetch(inst.base + '/admin/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: USER, password: PASS }) });
    const ck = (login.headers.get('set-cookie') || '').split(';')[0];
    const list = await fetch(inst.base + '/admin/channels', { headers: { cookie: ck } });
    const data = await list.json();
    const arr = Array.isArray(data) ? data : (data.channels || data.data || []);
    ok(arr.some(c => c && c.name === 'probe-ch'), '后端确实多了一条 name=probe-ch 的渠道');
    const autoCh = arr.find(c => c && c.name === 'probe-auto');
    ok(!!autoCh, '后端也有 probe-auto 这条');
    if (autoCh) {
      const mm = autoCh.model_mapping || {};
      ok(mm['qiniuyun/deepseek-v4.1-flash'] === 'deepseek-v4.1-flash'
        && mm['qiniuyun/kimi-k3-free'] === 'kimi-k3-free',
        '后端存下的映射就是自动生成的那张（实际 ' + JSON.stringify(mm) + '）');
    }
  } finally { inst.stop(); }
  console.log(fail === 0 ? '\n=== 渠道页保存流程全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  // 用 exitCode 而不是 process.exit()：Windows 上强制退出会撞见 libuv 的
  // "Assertion failed: !(handle->flags & UV_HANDLE_CLOSING)" 噪音（无害但吓人）。
  process.exitCode = fail ? 1 : 0;
})().catch(e => { console.error('EX ' + (e && e.message)); process.exit(2); });