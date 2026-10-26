// verify_models_grouped.js —— T5+T6 验证：模型面板去重、按上游分组、可折叠、倍率。
//
// # 判据全部用**渲染结果**，不读源码/常量
//
// 本项目第 10 次事故就是"读代码推断 ≠ 实际渲染"（我差点因为查了全角 ×
// 而把正常的倍率功能当坏的）。所以这里一律用 DOM。
//
// ⚠ 2026 模型 id 口径变更后的现状（本文件第 1/1b 节的判据跟着翻转过）：
//   · 后端 /v1/models 多上游模式**只发** `provider/xxx`（39 条 → 23 条）；
//   · 面板 chip 现在显示**完整 id**（`workbuddy/glm-5.2`），组内仍按去前缀的
//     裸名去重 —— 所以"不重复"与"前缀在"要**同时**成立，缺一条就是回归。
//   · 需要跑着的实例（默认 127.0.0.1:18080，可用 MODELS_URL 覆盖）+ Chrome。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
// 探针自带夹具密钥：T5/T7 起产品不再把凭据注入页面（那是安全改造的目标之一），
// 页内 fetch('/v1/models') 必须由探针自己带 key —— 见 lib_cdp.injectProbeKey。
const { injectProbeKey: injectKey, pasteConsoleKey, until } = require('./lib_cdp.js');
const PORT = 9306;
const TARGET = process.env.MODELS_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-models');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

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
    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable'); await send('Runtime.enable');
    await send('Page.navigate', { url: TARGET });
    await sleep(5500);
    // ⚠ 注入必须在新文档建立**之后**（紧跟 navigate 的赋值会被随后的导航冲掉，
    // 表现为 __PROBE_API_KEY__ 仍 undefined ⇒ 空 Bearer ⇒ 假红）。
    // 页内 fetch 用的密钥是**探针自己的夹具**：T5/T7 起产品不再把凭据注入页面
    // （那是本次安全改造的目标），探针得自带 key —— 见 lib_cdp.injectProbeKey。
    if (!(await injectKey(send, undefined, TARGET))) {
      console.error('  ⚠ 环境没给探针密钥（WB2API_PROBE_KEY / WB2API_PROBE_TOKEN）⇒ 页内 /v1/models 是空目录，断言会假红');
    }
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

    // ── ⓪ 身份自证：先确认「我进的是控制台」，再谈断言（2026-10-26 加）──────
    //
    // T7 起 `/ui` 需要身份（登录会话或 `?token=`）。本探针若没带身份，就会停在
    // **登录页**上：那时 `#models`/`#nav` 根本不存在，下面每条断言都会红 ——
    // 而红的原因是**验错了页面**，不是产品缺陷（本仓把这类红叫「假红」，
    // 已有专门的门禁 verify_probe_console_loaded.js 在静态层面拦它）。
    //
    // 所以在做任何业务断言之前先自证；进不去就**明确报错退出**（exit 2 = 环境/前提
    // 不成立，不是产品红），并给出可直接复制的命令。
    const identity = JSON.parse(await ev(`JSON.stringify((function(){
      return {
        hasNav: !!document.querySelector('#nav'),
        hasModels: !!document.querySelector('#models'),
        hasPassword: !!document.querySelector('input[type=password]'),
        title: document.title || '',
        href: location.href,
      };
    })())`));
    if (!identity.hasNav || !identity.hasModels) {
      console.error('  ✗✗ 没进控制台 —— 停在登录页/错误页，后面的断言全是假红。');
      console.error('     实测现场: ' + JSON.stringify(identity));
      console.error('     原因: T7 起 /ui 需要身份；本探针不带 ?token= 时到不了模型面板。');
      console.error('     跑法（把 <port>/<admin_token> 换成你的实例）:');
      console.error('       MODELS_URL="http://127.0.0.1:<port>/ui?token=<admin_token>" node tests/frontend/verify_models_grouped.js');
      console.error('     （探针自起的隔离实例：token 就是 startInstance 返回的 key）');
      console.error('     ⚠ 退出码 2 = 前提不成立，不算产品红。');
      process.exit(2);
    }

    // 像人一样把密钥粘进顶栏并点「应用」。
    // ⚠ 必须做：T5/T7 起控制台**不再**由服务端注入 api_key（安全改造的目标），
    // 于是「可用模型」这些依赖 /v1 的面板默认是空的 —— 不粘就断"有分组"会假红，
    // 而那个红是**契约变了**，不是产品坏了。见 lib_cdp.pasteConsoleKey。
    const pasted = await pasteConsoleKey(ev, undefined, TARGET);
    if (pasted !== 'ok') console.error('  ⚠ 粘密钥失败（' + pasted + '）⇒ 下面的模型断言会假红');

    // 切到可用模型
    await ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf('可用模型') >= 0; })[0];
      if (b) b.click();
    })()`);
    await sleep(2500);

    // ⚠⚠ 有界等**真实就绪信号**，别靠睡固定时间（2026-10-19 补）。
    //
    // # 为什么必须有（实测抖动，非产品缺陷）
    //
    // 同一份代码、同一条命令连跑：一次 `exit 0`，下一次 **10 项失败**
    // （`分组: []`、`分组标题: []`、`默认展开（—）`）。差的只是
    // "睡醒时 `/v1/models` 回来了没有"。
    //
    // 这正是主脑那条口径的活例子：**判据必须靠真信号，不能靠时间**
    //  —— `verify_model_provider_filter.js` 里那个 `waitModels(1)` 是**恒真**的
    //     假等待（`auto` 项永远在），所以我把它换成了 `waitProviderOptions`。
    //     这里同理：要等的是"**至少一个 chip 画出来了**"，
    //     那才是"模型目录回来了 + renderModels 跑过了"的唯一外部信号。
    //
    // 15 秒内没出现，后面的断言该红还是红（而且会如实说明"就绪信号没出现"）。
    if (typeof until === 'function') {
      // ⚠ `until` **不抛**（超时就把最后一次的值返回）—— 所以判据是"它返回的值够不够"，
      //   不能写成 `.then(()=>true)`（那样里面超时了外面也是 true，等于没等）。
      const readyN = await until(ev, `document.querySelectorAll('#models .chip').length`,
        (v) => Number(v) >= 1, 15000);
      const n = Number(readyN) || 0;
      console.log('  就绪信号: #models .chip = ' + n + (n >= 1 ? '' : '（15s 内没等到）'));
      if (n < 1) {
        console.error('  ⚠ 15s 内模型面板没画出任何 chip ⇒ /v1/models 没回来' +
          '（先看上面有没有"粘密钥失败"的告警），下面的分组/去重断言会假红');
      }
    }

    // ---- 1) 去重 ----
    //
    // 「原始条数」要**真的去问 API**，不能用 `window.__wb2api__.models()`
    // —— 那个函数没有暴露（我第一版就是这么写的，于是 raw=null，
    // 断言退化成 `16 < null` 恒 false，报了个假失败）。
    // 判据必须是可对照的真数据：原始目录 vs 渲染出的 chip 数。
    //
    // ⚠ 新口径：**连 owned_by 一起取**。去重的键是 (owner, 去前缀后的裸名)，
    // 而 owner 对裸名条目来自 owned_by —— 只取 id 算不出期望值，
    // 期望值就只能退回"全局裸名集合"，那会漏掉"两个上游各有一个 glm-5.2"
    // 这种正当的两条（它们该各留一条，不该被算成重复）。
    const raw = JSON.parse(await ev(`(function(){
      return new Promise(function(resolve){
        var req = new XMLHttpRequest();
        req.open('GET', '/v1/models', true);
        req.setRequestHeader('Authorization', 'Bearer ' + (window.__PROBE_API_KEY__ || ''));
        req.onload = function(){
          try { resolve(JSON.stringify((JSON.parse(req.responseText).data || []).map(function(m){ return { id: m.id, owned_by: m.owned_by }; }))); }
          catch (e) { resolve('null'); }
        };
        req.onerror = function(){ resolve('null'); };
        req.send();
      });
    })()`));

    const snap = JSON.parse(await ev(`JSON.stringify((function(){
      var chips = Array.from(document.querySelectorAll('#models .chip'));
      var ids = chips.map(function(c){ return c.dataset.id; });
      var names = chips.map(function(c){ return (c.textContent||'').trim(); });
      var groups = Array.from(document.querySelectorAll('#models .mgroup')).map(function(g){
        return {
          owner: g.dataset.owner,
          collapsed: g.classList.contains('collapsed'),
          headText: (g.querySelector('.mgrouphead')||{}).textContent ? g.querySelector('.mgrouphead').textContent.replace(/\\s+/g,' ').trim() : '',
          chips: g.querySelectorAll('.chip').length,
          chipIds: Array.from(g.querySelectorAll('.chip')).map(function(c){ return c.dataset.id; }),
          visible: (g.querySelector('.chips')||{}).offsetParent !== null,
        };
      });
      return { chipCount: chips.length, ids: ids, names: names, groups: groups };
    })())`));

    const rawModels = raw === null ? [] : raw;
    const rawIds = rawModels.map(m => m.id);
    // 与前端 groupModelsByOwner 同一条规则（前缀优先，否则 owned_by）+ 只去前缀。
    const ownerOf = m => { const s = m.id.indexOf('/'); return s > 0 ? m.id.slice(0, s) : (m.owned_by || ''); };
    const bareOf = m => { const s = m.id.indexOf('/'); return s > 0 ? m.id.slice(s + 1) : m.id; };
    // 期望值 = 不同的 (owner, 裸名) 对数 —— 这正是界面 chip 的去重键。
    const rawDedup = new Set(rawModels.map(m => ownerOf(m) + '|' + bareOf(m)));

    console.log('  分组: ' + JSON.stringify(snap.groups.map(g => g.owner + '(' + g.chips + ')')));
    console.log('  chip 总数: ' + snap.chipCount + '；原始目录 ' + rawIds.length + ' 条；去重后应为 ' + rawDedup.size);
    console.log('  前 6 个: ' + JSON.stringify(snap.names.slice(0, 6)));

    ok(snap.groups.length >= 1, '至少有一个上游分组（' + snap.groups.length + '）');
    ok(rawIds.length > 0, '拿到了原始目录条数（' + rawIds.length + '）');
    ok(snap.chipCount === rawDedup.size,
      'chip 数（' + snap.chipCount + '）== (上游,裸名) 去重后的模型数（' + rawDedup.size + '），'
      + '原始目录 ' + rawIds.length + ' 条 → 说明按前缀去重生效');
    // 后端新口径：多上游只发 `provider/xxx`，同一 id 不该重复出现。
    // （这条是"重复"最直接的来源：重复 id 会让上面那条恰好在 rawDedup 里被抹平，
    //   所以必须单独盯住 —— 否则"后端又双发"时上面那条仍然绿。）
    ok(new Set(rawIds).size === rawIds.length,
      '/v1/models 自己就不重复：不同 id 数（' + new Set(rawIds).size + '）== 条数（' + rawIds.length + '）'
      + (new Set(rawIds).size === rawIds.length ? '' : ' —— 后端又发重复了'));

    // 每个分组内不得有重名，且**组内 id 唯一**（新口径：显示完整 id 之后，
    // 两条不同的模型不会长得一样，所以"字面重复"与"id 重复"是同一件事）。
    const dup = await ev(`(function(){
      var bad = [];
      document.querySelectorAll('#models .mgroup').forEach(function(g){
        var seen = {}, owner = g.dataset.owner;
        g.querySelectorAll('.chip').forEach(function(c){
          var n = (c.textContent||'').trim();
          if (seen[n]) bad.push(owner + ':' + n);
          seen[n] = 1;
        });
      });
      return JSON.stringify(bad);
    })()`);
    ok(JSON.parse(dup).length === 0, '组内无重名 chip' + (JSON.parse(dup).length ? ' —— ' + dup : ''));

    const dupId = [];
    for (const g of snap.groups) {
      const s = new Set();
      for (const id of (g.chipIds || [])) { if (s.has(id)) dupId.push(g.owner + ':' + id); s.add(id); }
    }
    ok(dupId.length === 0, '组内 chip 的 data-id 唯一' + (dupId.length ? ' —— 重复: ' + JSON.stringify(dupId) : ''));
    ok(new Set(snap.ids).size === snap.ids.length,
      '整个 chips 区没有重复的 data-id（' + snap.ids.length + ' 条）'
      + (new Set(snap.ids).size === snap.ids.length ? '' : ' —— 重复: '
        + JSON.stringify(snap.ids.filter((x, i) => snap.ids.indexOf(x) !== i).slice(0, 4))));

    // ---- 1b) chip 名**就是**完整的 `provider/xxx`（新口径）----
    //
    // ⚠ 这条判据是从旧版**翻转**过来的：旧版断言"chip 名里没有 provider/xxx 形态
    // （前缀已去掉）"。翻转的原因不是"前缀变好了"，而是**避免重复的手段换了一处**：
    //   · 旧口径：后端同时发裸名与 `provider/裸名` → 前端抹掉前缀 + 按裸名去重，
    //     否则同一模型显示两次（用户报的"重复了，很多"）。
    //   · 新口径：后端多上游**只发** `provider/xxx`（39 条 → 23 条），前端保留
    //     完整 id 并按裸名在组内去重 —— 前缀在这里是**归属信息**，不是重复源。
    //
    // 破了会怎样（这是失败信息要说清的部分）：chip 名退回裸名 →
    //   1. 可用模型面板上，"这个模型属于哪个上游"消失，而对话测试下拉里仍是
    //      `workbuddy/glm-5.2` —— 两处对不上，用户没法把面板里看到的 id 拿去测；
    //   2. 两个上游若有同名模型（比如都叫 glm-5.2），面板上就是两条字面完全相同的
    //      chip，用户看到的"重复"原样回来；
    //   3. `data-id` 与可见文本从此**不是同一个东西**（点击/复制拿到带前缀、
    //      眼睛看到裸名），测试里复制的 id 直接 404。
    const missingSlash = snap.ids.filter(id => String(id).indexOf('/') < 0);
    ok(missingSlash.length === 0,
      'chip 名**就是**完整的 `provider/xxx`（前缀必须保留）—— 实际裸名 '
      + missingSlash.length + ' 个' + (missingSlash.length ? ': ' + JSON.stringify(missingSlash.slice(0, 4)) : ''));
    // 可见文本 = id + 倍率标记，所以文本必须以 id 开头（三处口径一致）。
    const textMismatch = snap.names.filter((n, i) => n.indexOf(snap.ids[i]) !== 0);
    ok(textMismatch.length === 0,
      'chip 可见文本以完整 id 开头（data-id / title / 文本三处同一口径）'
      + (textMismatch.length ? ' —— 不一致: ' + JSON.stringify(textMismatch.slice(0, 4)) : ''));
    const slashIds = snap.ids.filter(id => String(id).indexOf('/') > 0);
    ok(slashIds.length === snap.ids.length && snap.ids.length > 0,
      '全部 ' + snap.ids.length + ' 条 chip 的 data-id 都带 `provider/` 前缀');

    // ---- 2) 分组标题带计数 ----
    const withCount = snap.groups.filter(g => /\d+\s*个模型/.test(g.headText));
    console.log('  分组标题: ' + JSON.stringify(snap.groups.map(g => g.headText)));
    ok(withCount.length === snap.groups.length,
      '每个分组标题都带模型数（' + withCount.length + '/' + snap.groups.length + '）');

    // ---- 3) 可折叠 ----
    const first = snap.groups[0];
    ok(first && !first.collapsed && first.visible, '默认展开（' + (first ? first.owner : '—') + '）');

    await ev(`(function(){
      var hd = document.querySelector('#models button[data-mtoggle]');
      if (hd) hd.click();
    })()`);
    await sleep(700);
    const afterCollapse = JSON.parse(await ev(`JSON.stringify((function(){
      var g = document.querySelector('#models .mgroup');
      var chips = g.querySelector('.chips');
      return {
        collapsed: g.classList.contains('collapsed'),
        chipsHidden: chips ? getComputedStyle(chips).display === 'none' : null,
        aria: (g.querySelector('.mgrouphead')||{}).getAttribute ? g.querySelector('.mgrouphead').getAttribute('aria-expanded') : null,
        ls: (function(){ try { return localStorage.getItem('wb2api.modelgroup.' + g.dataset.owner); } catch(e){ return 'ERR'; } })(),
      };
    })())`));
    console.log('  折叠后: ' + JSON.stringify(afterCollapse));
    ok(afterCollapse.collapsed, '点击分组头后加上了 collapsed 类');
    ok(afterCollapse.chipsHidden === true, 'chips 区域真的隐藏了（display:none）');
    ok(afterCollapse.aria === 'false', 'aria-expanded 同步为 false（可访问性）');
    ok(afterCollapse.ls === '0', '折叠状态已写入 localStorage（刷新后保持）');

    // 展开回来
    await ev(`document.querySelector('#models button[data-mtoggle]').click()`);
    await sleep(700);
    const afterExpand = JSON.parse(await ev(`JSON.stringify((function(){
      var g = document.querySelector('#models .mgroup');
      return { collapsed: g.classList.contains('collapsed'),
               ls: (function(){ try { return localStorage.getItem('wb2api.modelgroup.' + g.dataset.owner); } catch(e){ return 'ERR'; } })() };
    })())`));
    ok(!afterExpand.collapsed && afterExpand.ls === '1', '再点一次展开，状态也持久化');

    // ---- 4) 倍率 ----
    const mult = JSON.parse(await ev(`JSON.stringify((function(){
      var withMult = 0, unknown = 0, total = 0, samples = [];
      document.querySelectorAll('#models .chip').forEach(function(c){
        total++;
        var m = c.querySelector('.mult');
        if (m) { withMult++; if (m.classList.contains('unknown')) unknown++; }
        if (samples.length < 5) samples.push((c.textContent||'').trim());
      });
      return { total: total, withMult: withMult, unknown: unknown, samples: samples };
    })())`));
    console.log('  倍率: ' + JSON.stringify(mult));
    ok(mult.withMult === mult.total, '每个 chip 都有倍率标记（' + mult.withMult + '/' + mult.total + '）');
    ok(mult.unknown > 0, '有 ' + mult.unknown + ' 个显示 `x无`（倍率表里没有的 —— 用户要求）');

    // 不出现 undefined / NaN
    const junk = snap.names.filter(n => /undefined|NaN|null/.test(n));
    ok(junk.length === 0, 'chip 名里没有 undefined/NaN/null' + (junk.length ? ' —— ' + JSON.stringify(junk) : ''));

    // ---- 5) 空态分组：声明了 models 能力但没有模型的上游必须出现 ----
    //
    // ⚠ 这条是**评审抓到的漏实现**：规格里画了 `codearts (0)` +
    // "没有可用账号，无法获取模型目录"，但我没实现 —— 而当时
    // 25 静态 + 22 E2E + 16 验收**全绿也没发现**。
    //
    // 原因是所有断言都在验"有的东西对不对"（去重、分组、折叠、倍率），
    // **没有一条验"该有的东西在不在"**。空白不会让任何断言变红 ——
    // 这正是"断言只测存在、不测内容"的镜像：**断言只测内容、不测存在**。
    //
    // 判据：拿 manifest 里声明了 models 能力的上游集合，
    // 与界面上出现的分组集合对照 —— 不该有"声明了却没出现"的。
    // ── 显式前提（2026-10-26 加，读这一节之前必须先读）──────────────────
    //
    // 本节判据是「manifest 里声明了 models 能力的上游**都**要出现分组」，
    // 而**产品**有一条相反的裁定（internal/server/webui.html:13824）：
    //
    //     const groups = allGroups.filter(g => !(g.accountCount === 0));
    //     ⇒ 明确知道 0 个账号的上游，**整组不显示**
    //
    // 两条同时成立的唯一条件是：**每个上游都至少有一个可用账号**。所以：
    //   · 在「上游账号齐全」的真实部署上跑 ⇒ 本节应全绿（这是它的目标场景）；
    //   · 在半配置的隔离实例上跑 ⇒ 缺账号的上游既不显示分组、也不算空态分组
    //     ⇒ 本节会红，而那个红是**前提不成立**，不是产品缺陷。
    // 先把这个前提打印出来，否则这一节的红无法归因。
    console.log('\n[前提] 本节要求「每个声明 models 的上游都至少有 1 个可用账号」');
    console.log('       （与 webui.html:13824「0 账号整组不显示」是一对相反的判据）');
    const acctCounts = JSON.parse(await ev(`JSON.stringify(
      (window.__wb2api__.manifest().providers || []).map(function(p){ return { id: p.id, accounts: p.account_count }; })
    )`));
    console.log('       当前各上游账号数: ' + JSON.stringify(acctCounts));
    const zeroAcct = acctCounts.filter(p => p.accounts === 0).map(p => p.id);
    if (zeroAcct.length) {
      console.log('       ⚠ 这些上游账号数为 0，按产品裁定**整组不显示** ⇒ 下面的「都出现」' +
        '断言会红，且红的原因是这个前提，不是产品缺陷: ' + JSON.stringify(zeroAcct));
    }

    console.log('\n[5] 声明了 models 能力的上游都要有分组（含空态）');
    const declared = JSON.parse(await ev(`JSON.stringify(
      (window.__wb2api__.manifest().providers || [])
        .filter(function(p){ return p && (p.capabilities || []).indexOf('models') >= 0; })
        .map(function(p){ return p.id; })
    )`));
    const shownOwners = snap.groups.map(g => g.owner);
    console.log('  manifest 声明 models 的上游: ' + JSON.stringify(declared));
    console.log('  界面上出现的分组: ' + JSON.stringify(shownOwners));

    const missing = declared.filter(d => shownOwners.indexOf(d) < 0);
    ok(missing.length === 0,
      '声明了 models 能力的上游**都**出现在模型面板里' +
      (missing.length ? ' —— 缺失: ' + JSON.stringify(missing) : ''));

    // 空态组必须有可自我解释的说明（不是一片空白）
    const emptyState = JSON.parse(await ev(`JSON.stringify((function(){
      var out = [];
      document.querySelectorAll('#models .mgroup').forEach(function(g){
        if (g.querySelectorAll('.chip').length) return;
        out.push({ owner: g.dataset.owner,
                   text: (g.querySelector('.chips')||{}).textContent ? g.querySelector('.chips').textContent.replace(/\\s+/g,' ').trim() : '' });
      });
      return out;
    })())`));
    console.log('  空态分组: ' + JSON.stringify(emptyState.map(e => e.owner + ' → ' + e.text.slice(0, 30))));
    for (const e of emptyState) {
      ok(e.text.length > 0,
        '空态分组「' + e.owner + '」有说明文字（不是一片空白）');
      ok(/账号|目录/.test(e.text),
        '空态分组「' + e.owner + '」的说明讲清了原因（含"账号"或"目录"）');
    }
    if (emptyState.length === 0) {
      console.log('  （当前没有空态分组 —— 所有上游都有模型）');
    }

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== T5+T6 验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
