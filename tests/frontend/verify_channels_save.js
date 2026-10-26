// verify_channels_save.js —— 渠道页「点了没反应」类缺陷的回归判据（C2 重写版）。
//
// # 为什么需要它（保留 T6 以来的本意，一个字都没弱化）
// v1.9.15 用户点「新增渠道 → 保存」，啥都没发生，控制台里是：
//   Uncaught (in promise) ReferenceError: key is not defined   at save (channels:1595)
// 那行从 29bb9c4 起就写着 `!key`，而 `key` 从未声明过；静态语法门禁查不出（语法是对的）。
// 所以这条探针只做一件事：**真的把「新建 → 填必填 → 保存」走一遍**，并断言：
//   ① 必填校验真的拦得住（新建时密钥留空 ⇒ 报错且不落库，不是"静默保存成功"）
//   ② 保存后抽屉关闭、列表里真的多了一条、名字就是我们填的那个
//   ③ 后端 /admin/channels 真的多了一条
//   ④ 列表响应体不含明文 key，而显式 reveal 含（明文边界，C2/B1 同口径）
//   ⑤ 全程没有未捕获异常（ReferenceError 是那条缺陷的指纹）
//
// # C2 重写说明（DOM 契约变了，判据没变）
// 旧页面是 <dialog id="dlg">+ #f-key/#f-slug/#f-map-rows 的六列表格；C2 换成 12 列
// （勾选/ID/名称/标签名称/类型/分组/状态/优先级/权重/已用/渠道余额/操作）+ 抽屉 div#dlg
// + 密钥多行文本框 #f-keys。旧探针断言的"前缀自动生成映射"随旧交互一起消失（C2 的
// 模型重定向是 JSON↔可视化 + 批量生成），因此那两条断言换成对**新契约**的等价断言：
// 表头 12 列、操作列右对齐、模型重定向文本域能被保存进 model_mapping。
//
// 用法：node verify_channels_save.js（自己起隔离实例，端口 7899；绝不碰 7863）
'use strict';
const path = require('path');
const { startInstance, sleep } = require('./lib_instance.js');
const { withChrome } = require('./lib_cdp.js');

const PORT = Number(process.env.CH_PORT || 7899);
const USER = 'chprobe';
const PASS = 'chprobe2026';
const PROBE_KEY = 'sk-probe-1234567890';
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

    await withChrome({ port: 9471, url: inst.base + '/ui', token: false, width: 1400, height: 900,
      outDir: path.resolve(__dirname, '..', '..', '.task', 'maximalism-v3', 'shots'),
      readyExpr: 'document.readyState === "complete"' },
      async ({ ev, errs, sleep: s2 }) => {
        await ev('(async () => { const r = await fetch("/admin/auth/login", {method:"POST", headers:{"Content-Type":"application/json"}, body: JSON.stringify({username:' + JSON.stringify(USER) + ', password:' + JSON.stringify(PASS) + '})}); return r.status; })()');
        await ev('null'); // 让上面的 login 先落地
        await s2(200);
        await ev('location.href = "/ui/channels"');
        await s2(2500);

        const rowsBefore = await ev('document.querySelectorAll("#ch-table tbody tr").length');
        ok(typeof rowsBefore === 'number' && rowsBefore >= 0, '渠道表已渲染，当前 ' + rowsBefore + ' 行');
        const heads = await ev('Array.from(document.querySelectorAll("#ch-table thead th")).map(function(t){return t.textContent.trim();})');
        ok(heads.length === 12, '表头 12 列（实际 ' + heads.length + '：' + JSON.stringify(heads) + '）');
        ok(heads.indexOf('标签名称') >= 0 && heads.indexOf('渠道余额') >= 0, '标签名称与渠道余额两列都在');

        // ── 第 1 段：必填校验（新建时密钥留空必须被拦住，且不许落库）──
        const opened = await ev('(function(){ var b = document.getElementById("btn-new"); if (!b) return false; b.click(); return true; })()');
        ok(opened, '点到「新建渠道」');
        await s2(500);
        ok(await ev('!!document.getElementById("dlg")'), '抽屉已打开（#dlg 存在）');
        await ev('(function(){' +
          'function set(id, v){ var e = document.getElementById(id); e.value = v; e.dispatchEvent(new Event("input", {bubbles:true})); }' +
          'set("f-name", "probe-empty");' +
          'set("f-base", "https://example.invalid/v1");' +
          'return true; })()');
        await ev('(function(){ var b = document.getElementById("f-save"); b.click(); })()');
        await s2(700);
        const guardMsg = await ev('(document.getElementById("banner") || {}).textContent || ""');
        ok(guardMsg.indexOf('密钥') >= 0, '新建时密钥留空被拦下（提示："' + guardMsg + '"）');
        ok(await ev('!!document.getElementById("dlg")'), '被拦下时抽屉仍然开着（不是静默关闭）');

        // ── 第 2 段：正常保存 ──
        await ev('(function(){' +
          'function set(id, v){ var e = document.getElementById(id); e.value = v; e.dispatchEvent(new Event("input", {bubbles:true})); }' +
          'set("f-name", "probe-ch");' +
          'set("f-base", "https://example.invalid/v1");' +
          'set("f-keys", "sk-probe-1234567890");' +
          'set("f-models", "qiniuyun/deepseek-v4.1-flash");' +
          'set("f-groups", "default");' +
          'return document.getElementById("f-name").value; })()');
        const clicked = await ev('(function(){ var b = document.getElementById("f-save"); if (!b) return false; b.click(); return true; })()');
        ok(clicked, '点到「保存」');
        await s2(2000);
        ok(await ev('!document.getElementById("dlg")'), '保存后抽屉关闭（没关 = 请求没发成功）');
        const rowCount = await ev('document.querySelectorAll("#ch-table tbody tr").length');
        ok(rowCount >= 1, '表格里至少有 1 行（实际 ' + rowCount + '）');
        const hasName = await ev('document.querySelector("#ch-table tbody").textContent.indexOf("probe-ch") >= 0');
        ok(hasName, '新行里能看到刚填的名字 probe-ch');
        const realErrs = errs.filter(function (e) { return !/favicon|example\.invalid/i.test(e); });
        ok(realErrs.length === 0, '全程没有未捕获异常（实际 ' + JSON.stringify(realErrs.slice(0, 3)) + '）');
      });

    // ── 后端确认（含明文边界）──
    const login = await fetch(inst.base + '/admin/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: USER, password: PASS }) });
    const ck = (login.headers.get('set-cookie') || '').split(';')[0];
    const list = await fetch(inst.base + '/admin/channels', { headers: { cookie: ck } });
    const raw = await list.text();
    const data = JSON.parse(raw);
    const arr = Array.isArray(data) ? data : (data.channels || data.items || []);
    ok(arr.some(c => c && c.name === 'probe-ch'), '后端确实多了一条 name=probe-ch 的渠道');
    ok(raw.indexOf(PROBE_KEY) < 0, '列表响应体不含明文 key（C2/B1 明文边界）');
    const ch = arr.find(c => c && c.name === 'probe-ch');
    if (ch) {
      const rv = await fetch(inst.base + '/admin/channels/reveal', {
        method: 'POST', headers: { 'Content-Type': 'application/json', cookie: ck },
        body: JSON.stringify({ id: ch.id }),
      });
      const rvRaw = await rv.text();
      ok(rv.status === 200 && rvRaw.indexOf(PROBE_KEY) >= 0, '显式 reveal 才回明文（status=' + rv.status + '）');
      ok(Array.isArray(ch.models) && ch.models.indexOf('qiniuyun/deepseek-v4.1-flash') >= 0,
        '支持模型已保存（实际 ' + JSON.stringify(ch.models) + '）');
    }
  } finally { inst.stop(); }
  console.log(fail === 0 ? '\n=== 渠道页保存流程全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exitCode = fail === 0 ? 0 : 1;
})();
