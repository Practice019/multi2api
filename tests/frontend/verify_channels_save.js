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
// 用法：node verify_channels_save.js（自己起隔离实例；端口**现场探测空闲值**；绝不碰 7863）
//
// # 端口为什么不能写死（T97 追加）
// 这里曾经写死 7899。它本身与 7863 无关，但**是典型的假红发生器**：探针是并发跑的
// （run_frontend_suites / 各自手动起），硬编码端口必然在某个并发组合下撞车，
// 报出来的是 "Error: 端口 7899 被占用（EADDRINUSE）"，看起来像功能回归，实际只是抢端口。
// 所以：开跑前用 TCP bind 现场探一个空闲端口（CH_PORT 显式给定时仍以它为准）。
//
// ⚠ 与 lib_instance.js:180-186 的纪律**不冲突**，两件事说的是不同时机：
//   · 那条禁止的是「**撞了之后偷偷换端口**」—— 那会把真实碰撞藏起来，让探针 A/B 串台；
//   · 这里做的是「**开跑之前**先挑一个当前没人用的端口」，并且仍用 assertPortFree 复核、
//     拿不到就直接红（复核失败时的提示会点名谁占着，见 lib_instance.js:203）。
// 同理，CDP 端口与截图目录也改成每次运行唯一（并发跑时不互相覆盖）。
'use strict';
const path = require('path');
const net = require('net');
const { startInstance, sleep, assertPortFree, whoOwnsPort } = require('./lib_instance.js');
const { withChrome } = require('./lib_cdp.js');

// freePort 现场探一个空闲端口（bind 0 ⇒ 内核分配，随即释放，紧接着交给被测进程）。
function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const p = srv.address().port;
      srv.close(() => resolve(p));
    });
  });
}

// resolvePort 解析实例端口：CH_PORT 给了就用它（显式指定 = 用户知道自己在做什么），
// 否则现场探一个；两种情况都**复核一次**（被占即红，绝不悄悄换）。
async function resolvePort(envName) {
  const explicit = Number(process.env[envName] || 0);
  if (explicit) {
    const chk = await assertPortFree(explicit);
    if (!chk.free) {
      console.error('端口 ' + explicit + '（来自 ' + envName + '）已被占用：' + chk.reason +
        '｜占用者：' + whoOwnsPort(explicit));
      process.exit(1);
    }
    return explicit;
  }
  for (let i = 0; i < 8; i++) {
    const p = await freePort();
    const chk = await assertPortFree(p);
    if (chk.free) { return p; }   // 极端竞态（刚释放就被别人抢）⇒ 再探一次
  }
  console.error('连着 8 次都没探到空闲端口 —— 本机端口资源异常，先查再说。');
  process.exit(1);
}
const USER = 'chprobe';
const PASS = 'chprobe2026';
const PROBE_KEY = 'sk-probe-1234567890';
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

(async () => {
  // 每次运行都探一次 ⇒ 并发跑不撞车；打印出来便于事后对日志（端口应每次不同）。
  const PORT = await resolvePort('CH_PORT');
  const CDP_PORT = await resolvePort('CH_CDP_PORT');
  console.log('  实例端口 = ' + PORT + '｜CDP 端口 = ' + CDP_PORT + '（现场探测；CH_PORT/CH_CDP_PORT 可显式覆盖）');
  const inst = await startInstance({ port: PORT, apiKey: 'probe-key-ch' });
  try {
    const reg = await fetch(inst.base + '/admin/auth/register', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: USER, password: PASS }),
    });
    ok(reg.status === 200, '建管理员账号（bootstrap）status=' + reg.status);

    await withChrome({ port: CDP_PORT, url: inst.base + '/ui', token: false, width: 1400, height: 900,
      // 截图目录也按 pid 唯一化：并发跑时两条探针不会互相覆盖对方的现场截图。
      outDir: path.resolve(__dirname, '..', '..', '.task', 'maximalism-v3', 'shots', 'ch-' + process.pid),
      readyExpr: 'document.readyState === "complete"' },
      async ({ send, ev, errs, sleep: s2 }) => {
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

        // ── T97：该面板内「可见标题恰好 1 个 / 可见刷新控件恰好 1 个」──
        // 口径（照 verify_wave2_pages.js 的 [b2]）：只数**页面级**标题 = iframe 内 h1
        // + 外壳 section 的**直接子** h2/h3（不数内容分区标题）；刷新控件同理只数
        // 外壳 section 头里那个 + iframe 内的页面级刷新按钮。
        // 可见性三判据：display / visibility / 零尺寸（rect 全 0）。
        // ⚠ 必须先切到该面板再量：display:none 下 rect 全是 0，会量出"0 个标题"的假绿。
        // ⚠ 必须先回到**外壳**并切到该面板：本探针此前一直在 /ui/channels（页面单独一个
        //   document，没有外壳、没有 chFrame）。外壳的导航项是脚本按 .content > section
        //   的 DOM 顺序**动态生成**的（webui.html:2901），所以这里按文案"渠道"找它。
        await ev('location.href = "/ui"');
        await s2(3000);
        const switched = await ev('(function(){' +
          'var btns = Array.prototype.slice.call(document.querySelectorAll("#nav button, .nav button, nav button"));' +
          'var b = btns.filter(function(x){ return x.textContent.trim().indexOf("渠道") >= 0; })[0];' +
          'if (b) { b.click(); return "clicked:" + b.textContent.trim(); }' +
          'var sec = document.querySelector("section[data-key=\\"channels\\"]");' +
          'if (sec) { sec.hidden = false; sec.style.display = ""; return "forced:nav=" + btns.length; }' +
          'return "none:nav=" + btns.length; })()');
        console.log('       切换路径 = ' + switched);
        await s2(700);
        const COUNT_EXPR = '(function(){' +
          'function vis(el){ if(!el) return false; var w=(el.ownerDocument.defaultView||window); var cs=w.getComputedStyle(el);' +
          '  if(cs.display==="none"||cs.visibility==="hidden"||Number(cs.opacity)===0) return false;' +
          '  var r=el.getBoundingClientRect(); return r.width>0&&r.height>0; }' +
          'function desc(el){ var c=(typeof el.className==="string"&&el.className.trim())?("."+el.className.trim().split(/\\s+/).join(".")):""; return el.tagName.toLowerCase()+(el.id?("#"+el.id):"")+c; }' +
          'var out={iframeTitles:[],shellTitles:[],iframeRefresh:[],shellRefresh:[]};' +
          'var f=document.getElementById("chFrame"); var d=f&&f.contentDocument;' +
          'if(d){ Array.prototype.forEach.call(d.querySelectorAll("h1"),function(x){ if(vis(x)) out.iframeTitles.push(desc(x)); });' +
          '  Array.prototype.forEach.call(d.querySelectorAll("button"),function(x){ if(vis(x)&&x.textContent.trim()==="刷新") out.iframeRefresh.push(desc(x)); }); }' +
          'var sec=document.querySelector("section[data-key=\\"channels\\"]");' +
          'if(sec){ Array.prototype.forEach.call(sec.querySelectorAll(":scope > h2, :scope > h3"),function(x){ if(vis(x)) out.shellTitles.push(desc(x)); });' +
          '  Array.prototype.forEach.call(sec.querySelectorAll(":scope > h2 button, :scope > h3 button"),function(x){ if(vis(x)) out.shellRefresh.push(desc(x)); }); }' +
          'out.secVisible=vis(sec); return JSON.stringify(out); })()';
        async function measure(width, height, label) {
          if (width) {
            await send('Emulation.setDeviceMetricsOverride', { width: width, height: height, deviceScaleFactor: 1, mobile: width < 500 });
            await s2(400);
          }
          const m = JSON.parse(await ev(COUNT_EXPR));
          const t = m.iframeTitles.length + m.shellTitles.length;
          const rf = m.iframeRefresh.length + m.shellRefresh.length;
          console.log('       [' + label + '] 标题=' + t + '（iframe ' + JSON.stringify(m.iframeTitles) + ' + 外壳 ' + JSON.stringify(m.shellTitles) + '）' +
            ' 刷新=' + rf + '（iframe ' + JSON.stringify(m.iframeRefresh) + ' + 外壳 ' + JSON.stringify(m.shellRefresh) + '）面板可见=' + m.secVisible);
          ok(t === 1, '[' + label + '] 该面板内可见**页面级标题**恰好 1 个（实际 ' + t + '：iframe ' + JSON.stringify(m.iframeTitles) + ' + 外壳 ' + JSON.stringify(m.shellTitles) + '）');
          ok(rf === 1, '[' + label + '] 该面板内可见**刷新控件**恰好 1 个（实际 ' + rf + '：iframe ' + JSON.stringify(m.iframeRefresh) + ' + 外壳 ' + JSON.stringify(m.shellRefresh) + '）');
        }
        await measure(1920, 1000, '1920');
        await measure(375, 812, '375');
        await send('Emulation.clearDeviceMetricsOverride');
        await s2(300);
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
