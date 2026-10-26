// verify_wave2_pages.js —— Wave 2 三页面（状态/渠道/模型）的**可重复验收命令**。
//
// # 为什么要有它（2026-10-26 加，T15）
//
// Wave 2 的实现提交（7ea6444）里 **tests/frontend/ 一个文件都没动** —— 三页面的
// 验收在 T12/T13/T14 的任务块里写着"跑 run_frontend_suites.js / run_http_suites.js"，
// 而那两条**都不覆盖**新页面（它们验的是旧控制台与四条 HTTP 探针）。
// 于是"三页面对齐"这件事在实现侧没有可执行判据 —— 正是本仓反复吃亏的
// "实现完了，但没人守"。本脚本补的正是这条：它只验 Wave 2 新增的三页面契约。
//
// # 判据来源（PLAN-hyxapi-alignment.md §0.5 / §4 T12-T14）
//
//   状态页：字段集与阈值（75/90）、window_seconds:300、AGPL 归属（NOTICE §7(b)）
//   渠道页：page_size 上限 100、type_counts 同响应、翻页/搜索**不发请求**、状态字典
//   模型页：Model/Vendor 双 tab、合成行不可勾选、全选只勾可选行、**无定价/价格/单价/计费字样**
//           （允许「倍率」——用户 2026-10-27 裁决把倍率列搬到本页）、
//           /ui/models/data 未认证 ⇒ 401（不是 200+空数组）
//
// # 用法
//
//   node tests/frontend/verify_wave2_pages.js
//
// 自带隔离实例（**空闲端口现场探测**，绝不碰 7863）+ 临时目录 + Chrome。
// 退出码：0 全绿 / 1 有断言红 / 2 环境前提不成立。
'use strict';
// ⚠ 必须在 require(lib_instance) **之前**设：那个模块在求值时就把 EXE 定死
//（见 lib_instance.js 的 EXE_AT_LOAD 注释）。不设的话它会回落到
// `bin/wb2api-server.exe` —— **用户正在跑的那份**，本次实测就被它的
// 「构建过期」判据拦下（错误原文：构建过期，拒绝在旧页面上断言）。
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
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

// T98：模型面板的测量表达式（一次定义、三处复用：[e] 与 1920/375 两档）。
// ⚠ 刻意**全用双引号 + 不写正则**：这段会被塞进模板字符串再送到 CDP，
//   层层转义里最容易丢的就是反斜杠（本仓踩过：`\s` 变成 `s` 让守卫静默变松）。
const MEASURE_MODELS = '(function(){'
  + 'function vis(el){ if(!el) return false; var cs=getComputedStyle(el); if(cs.display==="none"||cs.visibility==="hidden") return false; var r=el.getBoundingClientRect(); return r.width>0&&r.height>0; }'
  + 'function norm(s){ return String(s==null?"":s).split(String.fromCharCode(10)).join(" ").split(String.fromCharCode(9)).join(" ").split(" ").filter(function(x){return x!=="";} ).join(" ").trim(); }'
  + 'function titlesIn(root,directOnly){ var out=[],ns=directOnly?root.querySelectorAll(":scope > h1, :scope > h2, :scope > h3"):root.querySelectorAll("h1"); for(var i=0;i<ns.length;i++){ if(vis(ns[i])) out.push(ns[i].tagName+":"+norm(ns[i].textContent).slice(0,24)); } return out; }'
  + 'function refreshIn(root){ var out=[],ns=root.querySelectorAll("button,a,[role=button]"); for(var i=0;i<ns.length;i++){ var t=norm(ns[i].textContent); var hit=t.indexOf("刷新")>=0||t.toLowerCase().indexOf("refresh")>=0; if(hit&&vis(ns[i])) out.push((ns[i].id||ns[i].tagName)+":"+t.slice(0,16)); } return out; }'
  + 'var sec=document.querySelector("section[data-key=modelcatalog]"), w=document.getElementById("mdFrame").contentWindow, d=w.document;'
  + 'var main=d.querySelector("main"), footer=d.querySelector("footer"); var bottom=0;'
  + '[main,footer].forEach(function(el){ if(el){ var r=el.getBoundingClientRect(); if(r.bottom>bottom) bottom=r.bottom; } });'
  + 'var tw=d.querySelector("#panelModels .tw"), tr=d.querySelector("#modelRows tr");'
  + 'return JSON.stringify({ shellTitles:titlesIn(sec,true), pageTitles:titlesIn(d), shellRefresh:refreshIn(sec), pageRefresh:refreshIn(d),'
  + '  innerH:w.innerHeight, contentBottom:Math.round(bottom), blank:Math.round(w.innerHeight-bottom),'
  + '  twH:tw?Math.round(tw.getBoundingClientRect().height):0, rowH:tr?Math.round(tr.getBoundingClientRect().height):0,'
  + '  docScrollW:d.documentElement.scrollWidth, docClientW:d.documentElement.clientWidth }); })()';

(async () => {
  let fail = 0, skip = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
  const sk = (m) => { console.log('  SKIP ' + m); skip++; };

  const PORT = await freePort();
  const TOKEN = 'probe-key-wave2';
  console.log('实例端口（现场探测）: ' + PORT + '（绝不是 7863）');
  const inst = await startInstance({ port: PORT, apiKey: TOKEN });
  const base = inst.base, hdr = { Authorization: 'Bearer ' + TOKEN };
  try {
    // ── a) 未认证的路由行为 ──────────────────────────────────────────────
    console.log('\n[a] 三入口挂载 + 未认证行为');
    for (const p of ['/ui/status', '/ui/channels', '/ui/models']) {
      const r = await fetch(base + p, { redirect: 'manual' });
      const loc = String(r.headers.get('location') || '');
      ok(r.status === 302 && loc.includes('/ui'), '未认证 GET ' + p + ' ⇒ 302 回 /ui（实际 ' + r.status + ' ' + loc + '）');
    }
    const rData = await fetch(base + '/ui/models/data', { redirect: 'manual' });
    ok(rData.status === 401, '未认证 GET /ui/models/data ⇒ 401（实际 ' + rData.status + '；200+空数组就是漏鉴权）');

    // ── b) 状态页取数端点 ────────────────────────────────────────────────
    console.log('\n[b] 状态页：/admin/system-status 契约');
    const rs = await fetch(base + '/admin/system-status', { headers: hdr });
    ok(rs.status === 200, '认证 GET /admin/system-status ⇒ 200（实际 ' + rs.status + '）');
    let j = null; try { j = await rs.json(); } catch { }
    // ⚠ 形状以**实现**为准（internal/admin/system_status.go:70-77 直接 writeJSON(payload)，
    //   payload 是 internal/sysmon.Status：{service, health, resources, …}）。
    //   计划 §6.4 写的是 hyxapi 的字段名（version/request_health/…）—— 实现换了一套命名，
    //   语义对得上但**名称不对应**；本层只验语义，命名差异由验收报告点名（见回报）。
    const d = (j && j.data) ? j.data : (j || {});
    const H = d.health || {}, S = d.service || {}, R = d.resource || d.resources || {};
    ok(H.window_seconds === 300, 'health.window_seconds = 300（实际 ' + H.window_seconds + '）');
    ok(['ok', 'warning', 'critical'].indexOf(String(S.status)) >= 0, 'service.status ∈ {ok,warning,critical}（实际 ' + S.status + '）');
    ok(Array.isArray(S.warnings), 'service.warnings 是数组');
    ok(typeof R.cpu_pct === 'number' && typeof R.mem_pct === 'number', 'resources.cpu_pct / mem_pct 是数字（实际 ' + R.cpu_pct + '/' + R.mem_pct + '）');
    ok(typeof H.rpm_5m === 'number' && typeof H.success_rate === 'number', 'health.rpm_5m / success_rate 存在（实际 ' + H.rpm_5m + '/' + H.success_rate + '）');

    // ── 播种一条**合成行**（M3 变异测试需要样本）──────────────────────────
    // 合成行 = 渠道声明在用、但上游目录里没有的名字（webui-models.html:26-31）。
    // 隔离实例的上游目录是空的，所以这里造渠道、声明假模型名，
    // 模型页就会出现 synthetic 行 —— 否则「合成行不可勾选」这条断言无样本（只能 SKIP）。
    //
    // ⚠ 必须播种 **≥ 21 条**（SEED_CHANNELS）：渠道页 CH_PAGE_SIZE=20 ⇒ 只有超过一页，
    //   「下一页」才是有效控件；否则翻页断言（以及针对它的变异测试）都是空过 ——
    //   2026-10-26 的 M2 变异就因此未能证伪过一次。
    const SEED_CHANNELS = 21;
    let seedOK = 0, seedStatus = [];
    for (let i = 1; i <= SEED_CHANNELS; i++) {
      const seed = await fetch(base + '/admin/channels', {
        method: 'POST',
        headers: Object.assign({ 'Content-Type': 'application/json' }, hdr),
        body: JSON.stringify({
          name: 'probe-synth-' + String(i).padStart(2, '0'),
          base_url: 'https://example.invalid/v1',
          keys: ['sk-probe-0000000000'],
          models: ['probe-synthetic-' + i],
        }),
      });
      seedStatus.push(seed.status);
      if (seed.ok) seedOK++;
    }
    console.log('\n[seed] 播种 ' + SEED_CHANNELS + ' 条渠道（≥2 页，让翻页控件真的有效）⇒ 成功 ' + seedOK + '/' + SEED_CHANNELS + '，状态 ' + JSON.stringify(seedStatus.slice(0, 4)) + '…');

    // ── c) 渠道页服务端契约 ──────────────────────────────────────────────
    console.log('\n[c] 渠道页：page_size 上限 / type_counts / 状态字典');
    const rc = await fetch(base + '/admin/channels?p=1&page_size=1000', { headers: hdr });
    ok(rc.status === 200, 'GET /admin/channels ⇒ 200（实际 ' + rc.status + '）');
    let jc = null; try { jc = await rc.json(); } catch { }
    const cd = jc && (jc.data || jc) || {};
    ok(cd.page_size === 100, 'page_size=1000 被夹到 100（实际 ' + cd.page_size + '）');
    ok(cd.type_counts && typeof cd.type_counts === 'object', 'type_counts 与列表在同一响应里');
    ok(typeof cd.total === 'number' && typeof cd.page === 'number', 'total/page 字段存在');

    // ── DOM（Chrome + CDP）──────────────────────────────────────────────
    const CH_PORT = await freePort();
    await withChrome({
      port: CH_PORT, url: base + '/ui', token: TOKEN, width: 1500, height: 1000,
      outDir: path.join(os.tmpdir(), 'wave2-pages-shots'),
      readyExpr: 'document.readyState === "complete"',
    }, async ({ ev, sleep }) => {
      await sleep(2500);
      console.log('\n[a2] 三入口 iframe（DOM）');
      const fr = JSON.parse(await ev(`JSON.stringify(['stFrame','chFrame','mdFrame'].map(function(id){
        var f = document.getElementById(id);
        var doc = null; try { doc = f && f.contentDocument; } catch (e) { doc = null; }
        return { id: id, exists: !!f, src: f ? f.getAttribute('src') : null,
                 readable: !!(doc && doc.body), url: doc ? String(doc.location && doc.location.pathname) : null };
      }))`));
      for (const f of fr) {
        ok(f.exists && f.src && f.src.indexOf('/ui/') === 0, f.id + ' 存在且 src 指向 /ui/*（src=' + f.src + '）');
        ok(f.readable, f.id + ' 的 contentDocument 可读（同源 + 鉴权通过）');
      }

      console.log('\n[b2] 状态页（iframe 内）');
      const st = JSON.parse(await ev(`(function(){
        var w = document.getElementById('stFrame').contentWindow, d = w.document;
        var credit = d.getElementById('credit');
        var a = credit ? credit.querySelector('a') : null;
        return JSON.stringify({
          sections: d.querySelectorAll('section').length,
          creditText: credit ? credit.textContent.replace(/\\s+/g,' ').trim() : null,
          link: a ? a.getAttribute('href') : null,
          // ⚠ 阈值/窗口是页面脚本里的**闭包内变量**（webui-status.html:310-312 在一个 IIFE 里），
          //   挂在 window 上取不到（上一版就是这么误报的）。判据改成**服务端下发的页面原文**：
          //   它同时是变异敏感的（改 75→50 会让下面的正则失配）。
          src: d.documentElement.outerHTML,
          appText: (d.getElementById('app')||{}).textContent ? d.getElementById('app').textContent.slice(0,60) : ''
        });
      })()`));
      const src = String(st.src || '');
      const mWarn = src.match(/WARN_PCT\s*=\s*(\d+)/), mCrit = src.match(/CRIT_PCT\s*=\s*(\d+)/), mWin = src.match(/WINDOW_SECONDS\s*=\s*(\d+)/);
      console.log('       sections=' + st.sections + ' WARN_PCT=' + (mWarn && mWarn[1]) + ' CRIT_PCT=' + (mCrit && mCrit[1])
        + ' WINDOW_SECONDS=' + (mWin && mWin[1]) + ' link=' + st.link);
      ok(st.sections >= 1, '状态页至少 1 个 <section>（实际 ' + st.sections + '）');
      ok(/New API contributors/.test(String(st.creditText)), 'AGPL 归属文案可见（NOTICE §7(b)）');
      ok(String(st.link).indexOf('github.com/QuantumNous/new-api') >= 0, '归属链接指向上游仓库（' + st.link + '）');
      ok(!!mWarn && mWarn[1] === '75' && !!mCrit && mCrit[1] === '90', '阈值三档 = 75/90（PLAN §0.5；实际 ' + (mWarn && mWarn[1]) + '/' + (mCrit && mCrit[1]) + '）');
      ok(!!mWin && mWin[1] === '300', 'WINDOW_SECONDS = 300（实际 ' + (mWin && mWin[1]) + '）');
      // ── T92：状态面板的**唯一性**与**宽度**断言（防「两个标题/两个刷新」「内容比 iframe 窄」回归）
      // 判据只算**可见**元素（display:none / visibility:hidden / 零尺寸都排除）——
      // 否则把按钮藏起来就能骗过门禁。
      // 标题与刷新都统计**两处**：控制台外壳的 section 内 + iframe 页面内（用户看到的是两者之和）。
      // ⚠ 必须先把这个面板切成**活动**面板：前面的 [b1] 逐帧走过 stFrame/chFrame/mdFrame，
      //   结束时停在 models ⇒ 状态页的 section 是 display:none，其 iframe 内所有 rect 都是 0，
      //   「可见标题数」会恒为 0（我第一版就是这么误报的）。可见性判据必须建立在真的可见之上。
      await ev('window.__wb2api__.showPanelByKey("status")');
      await sleep(900);
      const uniq = JSON.parse(await ev(`(function(){
        function vis(el){ if(!el) return false; var cs=getComputedStyle(el); if(cs.display==='none'||cs.visibility==='hidden') return false; var r=el.getBoundingClientRect(); return r.width>0&&r.height>0; }
        // 只数**页面级标题**：iframe 内的 h1 + 外壳 section 的**直接子** h2/h3。
        // ⚠ 不数页面内容里的分区标题（服务状态/资源与入口/请求健康/…）：那是内容结构，不是「标题重复」。
        function titlesIn(root, directOnly){ var out=[]; var ns=directOnly?root.querySelectorAll(':scope > h1, :scope > h2, :scope > h3'):root.querySelectorAll('h1'); for(var i=0;i<ns.length;i++){ if(vis(ns[i])) out.push(ns[i].tagName+':'+ns[i].textContent.replace(/\\s+/g,' ').trim().slice(0,24)); } return out; }
        function refreshIn(root){ var out=[]; var ns=root.querySelectorAll('button,a,[role=button]'); for(var i=0;i<ns.length;i++){ var t=(ns[i].textContent||'').replace(/\\s+/g,'').trim(); if(/刷新|Refresh/i.test(t)&&vis(ns[i])) out.push(t.slice(0,16)); } return out; }
        var sec=document.querySelector('section[data-key=status]');
        var w=document.getElementById('stFrame').contentWindow, d=w.document;
        var main=d.querySelector('main');
        return JSON.stringify({ shellTitles:titlesIn(sec,true), pageTitles:titlesIn(d),
          shellRefresh:refreshIn(sec), pageRefresh:refreshIn(d),
          mainW: main?Math.round(main.getBoundingClientRect().width):0, innerW:w.innerWidth,
          docScrollW:d.documentElement.scrollWidth, docClientW:d.documentElement.clientWidth,
          cd:(d.getElementById('cd')||{}).textContent||'' });
      })()`));
      const allTitles = (uniq.shellTitles || []).concat(uniq.pageTitles || []);
      const allRefresh = (uniq.shellRefresh || []).concat(uniq.pageRefresh || []);
      console.log('        可见标题=' + JSON.stringify(allTitles) + ' 可见刷新控件=' + JSON.stringify(allRefresh));
      console.log('        内容列=' + uniq.mainW + ' / iframe 内宽=' + uniq.innerW + ' = ' + (uniq.innerW ? (uniq.mainW / uniq.innerW).toFixed(3) : 'n/a') + '；#cd=' + JSON.stringify(uniq.cd));
      ok(allTitles.length === 1, '状态面板内**可见标题恰好 1 个**（实际 ' + allTitles.length + '：' + JSON.stringify(allTitles) + '）');
      ok(allRefresh.length === 1, '状态面板内**可见刷新控件恰好 1 个**（实际 ' + allRefresh.length + '：' + JSON.stringify(allRefresh) + '）');
      ok(String(uniq.cd).length > 0, '倒计时信息没丢（#cd = ' + JSON.stringify(uniq.cd) + '）');
      if (uniq.innerW >= 1000) {   // 本探针视口下 iframe 内宽约 1262 ⇒ 让 90% 断言真的跑到
        ok((uniq.mainW / uniq.innerW) >= 0.9, '内容列占 iframe 宽 ≥ 90%（实际 ' + uniq.mainW + '/' + uniq.innerW + ' = ' + (uniq.mainW / uniq.innerW).toFixed(3) + '；修前 960/1262 = 0.761）');
      } else {
        console.log('        （窄屏 ' + uniq.innerW + 'px：跳过 90% 宽度断言，改判无横向溢出）');
      }
      ok(uniq.docScrollW <= uniq.docClientW + 1, '状态页无横向溢出（scrollWidth ' + uniq.docScrollW + ' ≤ clientWidth ' + uniq.docClientW + '）');

      console.log('\n[c2] 渠道页：翻页与搜索**不发请求**（内存分页）');
      await ev(`(function(){
        var w = document.getElementById('chFrame').contentWindow;
        w.__probeReq = 0;
        var of = w.fetch, ox = w.XMLHttpRequest.prototype.open;
        w.fetch = function(){ w.__probeReq++; return of.apply(this, arguments); };
        w.XMLHttpRequest.prototype.open = function(){ w.__probeReq++; return ox.apply(this, arguments); };
        return true;
      })()`);
      await sleep(300);
      await ev(`(function(){ var b = document.getElementById('chFrame').contentDocument.getElementById('ch-next'); if (b) b.click(); })()`);
      await sleep(400);
      await ev(`(function(){ var d = document.getElementById('chFrame').contentDocument; var i = d.getElementById('flt-q'); if (i) { i.value='probe'; i.dispatchEvent(new d.defaultView.Event('input',{bubbles:true})); } })()`);
      await sleep(500);
      const afterLocal = await ev(`document.getElementById('chFrame').contentWindow.__probeReq`);
      ok(Number(afterLocal) === 0, '翻页 + 搜索后新增请求数 = 0（实际 ' + afterLocal + '）');
      // 对照组：状态筛选是**服务端**过滤，必须发请求（证明计数器真的在工作）
      await ev(`(function(){ var d = document.getElementById('chFrame').contentDocument; var s = d.getElementById('flt-status'); if (s) { s.value='1'; s.dispatchEvent(new d.defaultView.Event('change',{bubbles:true})); } })()`);
      await sleep(900);
      const afterServer = await ev(`document.getElementById('chFrame').contentWindow.__probeReq`);
      ok(Number(afterServer) > 0, '对照组：状态筛选触发了请求（实际 ' + afterServer + '）⇒ 计数器有效');

      console.log('\n[d] 模型页：双 tab / 合成行不可勾选 / 无定价字样');
      const md = JSON.parse(await ev(`(function(){
        var d = document.getElementById('mdFrame').contentDocument;
        var rows = Array.from(d.querySelectorAll('#modelsTable tbody tr'));
        var synth = rows.filter(function(r){ return r.getAttribute('data-synthetic') === '1'; });
        return JSON.stringify({
          tabs: d.querySelectorAll('#tabs button[role=tab]').length,
          modelsPanel: !!d.getElementById('panelModels'), vendorsPanel: !!d.getElementById('panelVendors'),
          rows: rows.length, synth: synth.length,
          synthDisabled: synth.filter(function(r){ var c=r.querySelector('input[type=checkbox]'); return c && c.disabled; }).length,
          bodyText: (d.body ? d.body.textContent : '').replace(/\\s+/g,' ')
        });
      })()`));
      console.log('       tabs=' + md.tabs + ' rows=' + md.rows + ' synth=' + md.synth + ' synthDisabled=' + md.synthDisabled);
      ok(md.tabs === 2 && md.modelsPanel && md.vendorsPanel, 'Model / Vendor 双 tab 存在（tabs=' + md.tabs + '）');
      if (md.synth > 0) {
        ok(md.synthDisabled === md.synth, '全部合成行不可勾选（' + md.synthDisabled + '/' + md.synth + '）');
      } else {
        sk('本次隔离实例没有合成行（需要"渠道声明在用但目录没有"的模型）—— 该断言未取到样本');
      }
      // ⚠ 2026-10-27 用户裁决：把「倍率」搬到模型目录页显示（原话要点：「例如倍率检测
      //   表格需要再添加一列信息」）。所以这里**允许**倍率出现，但仍然**禁止**价格/单价/计费。
      //   注意性质：这是**撤销一条前提**（用户改了产品决策），不是放宽判据 ——
      //   改动只是把「倍率」从禁用词表里移出，其余四个词一个没动。
      const pricing = ['定价', '价格', '单价', '计费'].filter(w => md.bodyText.indexOf(w) >= 0);
      ok(pricing.length === 0, '页面没有定价/价格/单价/计费字样（允许「倍率」，见上方用户裁决；命中: ' + JSON.stringify(pricing) + '）');
      const all = JSON.parse(await ev(`(function(){
        var d = document.getElementById('mdFrame').contentDocument, c = d.getElementById('checkAll');
        if (!c) return JSON.stringify({err:'no checkAll'});
        c.click();
        var rows = Array.from(d.querySelectorAll('#modelsTable tbody tr'));
        var checked = rows.filter(function(r){ var b=r.querySelector('input[type=checkbox]'); return b && b.checked; });
        var checkedSynth = checked.filter(function(r){ return r.getAttribute('data-synthetic') === '1'; });
        c.click();
        return JSON.stringify({ checked: checked.length, checkedSynth: checkedSynth.length });
      })()`));
      if (all.err) { sk('找不到 #checkAll'); }
      else { ok(all.checkedSynth === 0, '全选后没有任何合成行被勾中（勾中 ' + all.checked + ' 行，其中合成 ' + all.checkedSynth + '）'); }

      // ── T98：模型目录面板的**唯一性 / 撑满 / 空态**（防「两个标题/两个刷新」「下方一片空白」回归）
      // 判据口径逐条照 [b2]（状态页那一版）：
      //   · 只数**页面级标题** = iframe 内 h1 + 外壳 section 的直接子 h2/h3；
      //   · 刷新控件 = 面板范围内文本含「刷新/Refresh」的 button/a/[role=button]；
      //   · 可见性三判据（display / visibility / 零尺寸）—— 把按钮藏起来骗不过门禁；
      //   · ⚠ 必须先 showPanelByKey 切到该面板：display:none 下 iframe 内 rect 全 0，会量出 0 个。
      console.log('\n[e] 模型面板：标题与刷新唯一 + 内容撑满 + 空态可见（T98）');
      await ev('window.__wb2api__.showPanelByKey("modelcatalog")');
      await sleep(900);
      const mo = JSON.parse(await ev(MEASURE_MODELS));
      const moTitles = (mo.shellTitles || []).concat(mo.pageTitles || []);
      const moRefresh = (mo.shellRefresh || []).concat(mo.pageRefresh || []);
      console.log('        可见标题=' + JSON.stringify(moTitles) + ' 可见刷新控件=' + JSON.stringify(moRefresh));
      console.log('        iframe 内高=' + mo.innerH + ' 内容底=' + mo.contentBottom + ' 空白=' + mo.blank
        + ' .tw 高=' + mo.twH + ' 行高=' + mo.rowH + ' 溢出=' + mo.docScrollW + '/' + mo.docClientW);
      ok(moTitles.length === 1, '模型面板内**可见标题恰好 1 个**（实际 ' + moTitles.length + '：' + JSON.stringify(moTitles) + '）');
      ok(moRefresh.length === 1, '模型面板内**可见刷新控件恰好 1 个**（实际 ' + moRefresh.length + '：' + JSON.stringify(moRefresh) + '）');
      // 撑满：T98 前实测 contentBottom=288 / innerH=814 ⇒ 下方 527px 无内容且无边框。
      // 判据留 60px 余量（底部内边距 + 未来加一行提示都还在余量内），但不允许半屏空白。
      ok(mo.blank <= 60, '内容撑满 iframe（空白 ' + mo.blank + 'px ≤ 60；修前 527px）');
      ok(mo.twH > 200 && mo.twH > mo.rowH, '表格容器（有边框与底）跟着长高（.tw=' + mo.twH + 'px，单行 ' + mo.rowH + 'px）');
      ok(mo.rowH > 0 && mo.rowH <= 40, '行高没被拉伸变形（单行 ' + mo.rowH + 'px，应在 26 上下）');
      ok(mo.docScrollW <= mo.docClientW + 1, '模型页无横向溢出（scrollWidth ' + mo.docScrollW + ' ≤ clientWidth ' + mo.docClientW + '）');
      // 空态：过滤到无匹配 ⇒ 必须**看到文案**（而不是一片空白）
      const emp = JSON.parse(await ev(`(function(){
        function vis(el){ if(!el) return false; var cs=getComputedStyle(el); if(cs.display==='none'||cs.visibility==='hidden') return false; var r=el.getBoundingClientRect(); return r.width>0&&r.height>0; }
        function norm(s){ return String(s==null?'':s).split(String.fromCharCode(10)).join(' ').split(String.fromCharCode(9)).join(' ').split(' ').filter(function(x){return x!=='';}).join(' ').trim(); }
        var w=document.getElementById('mdFrame').contentWindow, d=w.document;
        var i=d.getElementById('search'); if(!i) return JSON.stringify({err:'no #search'});
        i.value='zzz-no-such-model-t98'; i.dispatchEvent(new w.Event('input',{bubbles:true}));
        var e=d.getElementById('modelsEmpty');
        return JSON.stringify({ rows:d.querySelectorAll('#modelRows tr').length, visible:e?vis(e):false, text:e?norm(e.textContent).slice(0,70):null });
      })()`));
      ok(emp.rows === 0, '无匹配过滤后表格 0 行（实际 ' + emp.rows + '）');
      ok(emp.visible && String(emp.text).length > 0, '空态可见且有文案（' + JSON.stringify(emp.text) + '）');
      await ev(`(function(){ var w=document.getElementById('mdFrame').contentWindow, d=w.document; var i=d.getElementById('search'); if(i){ i.value=''; i.dispatchEvent(new w.Event('input',{bubbles:true})); } return true; })()`);
      await sleep(400);
      const back = JSON.parse(await ev(`JSON.stringify({rows: document.getElementById('mdFrame').contentDocument.querySelectorAll('#modelRows tr').length})`));
      ok(back.rows > 0, '清空过滤后行数恢复（' + back.rows + ' 行）');
    });

      // ── T98：同一组唯一性判据在 **1920 与 375** 两档都要过（口径同 [e]）
      for (const vp of [[1920, 1080], [375, 900]]) {
        const tag = '[f' + (vp[0] === 375 ? '-narrow' : '') + '] 模型面板 @' + vp[0] + 'x' + vp[1];
        console.log('');
        console.log(tag);
        const m = await withChrome({
          port: CH_PORT, url: base + '/ui', token: TOKEN, width: vp[0], height: vp[1],
          outDir: path.join(os.tmpdir(), 'wave2-pages-shots'), readyExpr: 'document.readyState === "complete"',
        }, async ({ ev, sleep }) => {
          await sleep(2200);
          // ⚠ 必须先切到这个面板（display:none 下 iframe 内 rect 全 0 ⇒ 会量出 0 个标题）
          await ev('window.__wb2api__.showPanelByKey("modelcatalog")');
          await sleep(1200);
          return JSON.parse(await ev(MEASURE_MODELS));
        });
        const T = (m.shellTitles || []).concat(m.pageTitles || []);
        const R = (m.shellRefresh || []).concat(m.pageRefresh || []);
        console.log('        可见标题=' + JSON.stringify(T) + ' 可见刷新控件=' + JSON.stringify(R)
          + ' iframe 内宽=' + m.docClientW + ' 溢出=' + m.docScrollW + ' 空白=' + m.blank);
        ok(T.length === 1, tag + ' 可见标题恰好 1 个（实际 ' + T.length + '：' + JSON.stringify(T) + '）');
        ok(R.length === 1, tag + ' 可见刷新控件恰好 1 个（实际 ' + R.length + '：' + JSON.stringify(R) + '）');
        ok(m.docScrollW <= m.docClientW + 1, tag + ' 页面内无横向溢出（' + m.docScrollW + ' ≤ ' + m.docClientW + '）');
        if (vp[0] >= 1000) {
          ok(m.blank <= 60, tag + ' 内容撑满 iframe（空白 ' + m.blank + 'px ≤ 60）');
        } else {
          console.log('        （窄档只判唯一性与无横向溢出：窄屏内容比 iframe 高是正常的，允许纵向滚动）');
        }
      }
  } catch (e) {
    console.log('EXCEPTION: ' + (e && e.message));
    fail++;
  } finally {
    try { inst.stop(); } catch { }
  }
  console.log('\n' + (fail === 0 ? '=== Wave 2 三页面验收通过 ===' : '=== ' + fail + ' 项失败 ===')
    + (skip ? '（另有 ' + skip + ' 项 SKIP）' : ''));
  process.exit(fail ? 1 : 0);
})();
