// verify_tokens_page.js —— 令牌页（/ui/tokens）的**端到端**验收探针（T71 补 C3 的 ⑤）。
//
// # 为什么要有它
//
// C3 把令牌页做出来了（internal/server/webui-tokens.html；入口 /ui 的 tokens 面板 +
// tkFrame + 路由 h.tokensUI + //go:embed），但它的六条门禁（①②③④⑥⑧）都是**服务端/静态**
// 判据 —— 「页面上点得开、点得动、点完对」这件事没有任何可执行判据。本脚本补这条。
//
// # 判据来源（实现即契约）
//
//	入口/表格    webui-tokens.html:1274-1300（表头五项 + #empty 文案）
//	抽屉字段     webui-tokens.html:1313-1426（编号 ①~⑯ 的字段与控件）
//	提交体       webui-tokens.html:1698-1749（collectForm：字段名即请求体字段名）
//	count 语义   internal/admin/tokens.go:20-46（count>1 ⇒ 名称上限 43 字节，第 2 条起加 6 位短码）
//	部分成功     internal/admin/tokens.go:648-723（requested/created/failed/partial/items/failures）
//	明文与缓存   internal/admin/tokens.go:434-435（no-store, no-cache, must-revalidate, private）
//
// # 用法
//
//   node tests/frontend/verify_tokens_page.js
//
// 自带隔离实例（现场探测空闲端口，**绝不碰 7863**）+ 仓库外临时目录 + -probe.exe + Chrome。
// 退出码：0 全绿 / 1 有断言红 / 2 环境前提不成立。
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

// 名称字节数（UTF-8）：与页面 collectForm 的算法一致（webui-tokens.html:1706-1710）。
function bytes(s) {
  let n = 0;
  for (const ch of s) {
    const c = ch.codePointAt(0);
    n += c < 0x80 ? 1 : (c < 0x800 ? 2 : 3);
  }
  return n;
}

// masked 判定：store.MaskTokenKey 的三档都会留下 * 或 …（48 位 Key ⇒ 前4+10*+后4）。
function isMasked(k) {
  const s = String(k === undefined || k === null ? '' : k);
  return s.indexOf('*') >= 0 || s.indexOf('…') >= 0;
}

(async () => {
  let fail = 0, skip = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
  const sk = (m) => { console.log('  SKIP ' + m); skip++; };

  const PORT = await freePort();
  const TOKEN = 'probe-key-tokens';
  console.log('实例端口（现场探测）: ' + PORT + '（绝不是 7863）');
  const inst = await startInstance({ port: PORT, apiKey: TOKEN });
  const base = inst.base;
  const hdr = { Authorization: 'Bearer ' + TOKEN, 'Content-Type': 'application/json' };
  let plaintextKeys = [];           // 从 /key 端点取回的明文（后面拿它做"列表里不许出现"的反证）
  let createdIds = [];              // 页面/接口造出来的令牌 id，供 DOM 阶段复用
  try {
    // ── A) 路由与鉴权 ────────────────────────────────────────────────────
    console.log('\n[A] 入口挂载 + 未认证行为');
    const rUi = await fetch(base + '/ui/tokens', { redirect: 'manual' });
    const locUi = String(rUi.headers.get('location') || '');
    ok(rUi.status === 302 && locUi.indexOf('/ui') >= 0, '未认证 GET /ui/tokens ⇒ 302 回 /ui（实际 ' + rUi.status + ' ' + locUi + '）');
    const rListNoAuth = await fetch(base + '/admin/tokens', { redirect: 'manual' });
    ok(rListNoAuth.status === 401, '未认证 GET /admin/tokens ⇒ 401（实际 ' + rListNoAuth.status + '）');
    const rKeyNoAuth = await fetch(base + '/admin/tokens/1/key', { method: 'POST' });
    ok(rKeyNoAuth.status === 401 || rKeyNoAuth.status === 404, '未认证 POST /admin/tokens/1/key ⇒ 401/404（实际 ' + rKeyNoAuth.status + '；200 就是漏鉴权）');

    // ── B) 空库 + 建 1 条 ────────────────────────────────────────────────
    console.log('\n[B] 列表起始为空 + 建 1 条');
    const rEmpty = await fetch(base + '/admin/tokens?p=1&page_size=50', { headers: hdr });
    const jEmpty = await rEmpty.json();
    ok(rEmpty.status === 200 && Array.isArray(jEmpty.items) && jEmpty.total === 0, '隔离实例起始 0 条（实际 total=' + jEmpty.total + '）');

    const oneName = '探针单条';
    const rOne = await fetch(base + '/admin/tokens', { method: 'POST', headers: hdr, body: JSON.stringify({ name: oneName, unlimited_quota: true, automatic: true, auto_group_mode: 'inherit', auto_groups: [], groups: [] }) });
    const jOne = await rOne.json();
    ok(rOne.status === 200 && jOne.created === 1 && jOne.requested === 1 && jOne.failed === 0, 'POST /admin/tokens（count 缺省）⇒ created=1/requested=1/failed=0（实际 ' + JSON.stringify({ code: rOne.status, created: jOne.created, requested: jOne.requested, failed: jOne.failed }) + '）');
    const one = (jOne.items || [])[0] || {};
    createdIds.push(one.id);
    ok(!!one.id && one.name === oneName, '响应 items[0] 带 id 与名称（实际 ' + JSON.stringify(one) + '）');
    ok(isMasked(one.key), '创建响应里的 key 是**掩码**（实际 ' + one.key + '）');

    // ── C) count=3 与 43 字节上限 ────────────────────────────────────────
    console.log('\n[C] count=3 的名称后缀 + 43/50 字节边界');
    const batchBase = '探针批量';
    const rThree = await fetch(base + '/admin/tokens', { method: 'POST', headers: hdr, body: JSON.stringify({ name: batchBase, count: 3, unlimited_quota: true, automatic: true, auto_group_mode: 'inherit', auto_groups: [], groups: [] }) });
    const jThree = await rThree.json();
    ok(rThree.status === 200 && jThree.created === 3 && jThree.requested === 3, 'count=3 ⇒ created=3/requested=3（实际 ' + JSON.stringify({ code: rThree.status, created: jThree.created, requested: jThree.requested }) + '）');
    const names = (jThree.items || []).map((x) => x.name);
    for (const it of (jThree.items || [])) createdIds.push(it.id);
    ok(names.length === 3 && names[0] === batchBase, '第 1 条名称 = 原名（实际 ' + JSON.stringify(names) + '）');
    ok(names.slice(1).every((n) => n.indexOf(batchBase + '-') === 0 && n.length === batchBase.length + 7), '第 2 条起是「原名-6位短码」（实际 ' + JSON.stringify(names.slice(1)) + '）');
    const threeKeys = (jThree.items || []).map((x) => x.key);
    ok(threeKeys.length === 3 && new Set(threeKeys).size === 3 && threeKeys.every(isMasked), '三条各自有唯一的掩码 key（样本 ' + JSON.stringify(threeKeys) + '）');

    const name43 = 'x'.repeat(43);
    const r43 = await fetch(base + '/admin/tokens', { method: 'POST', headers: hdr, body: JSON.stringify({ name: name43, count: 2, automatic: true, auto_group_mode: 'inherit', auto_groups: [], groups: [] }) });
    ok(r43.status === 200, '43 字节 + count=2 ⇒ 200（上限之内；实际 ' + r43.status + '）');
    if (r43.status === 200) { const j43 = await r43.json(); for (const it of (j43.items || [])) createdIds.push(it.id); }
    const r50b = await fetch(base + '/admin/tokens', { method: 'POST', headers: hdr, body: JSON.stringify({ name: 'y'.repeat(50), count: 2, automatic: true, auto_group_mode: 'inherit', auto_groups: [], groups: [] }) });
    const b50 = await r50b.text();
    ok(r50b.status === 400, '50 字节 + count=2 ⇒ 400（上限降到 43；实际 ' + r50b.status + '，体 ' + b50.slice(0, 80) + '）');
    const r50one = await fetch(base + '/admin/tokens', { method: 'POST', headers: hdr, body: JSON.stringify({ name: 'z'.repeat(50), count: 1, unlimited_quota: true, automatic: true, auto_group_mode: 'inherit', auto_groups: [], groups: [] }) });
    const j50one = r50one.status === 200 ? await r50one.json() : null;
    if (j50one) { for (const it of (j50one.items || [])) createdIds.push(it.id); }
    ok(r50one.status === 200, '同样 50 字节在 count=1 时放行（证明 43 只对批量生效；实际 ' + r50one.status + '）');

    // ── D) 明文只在显式端点出现 + no-store + 掩码 ────────────────────────
    console.log('\n[D] 取明文：no-store 与"列表/详情不许含明文"');
    const rDet = await fetch(base + '/admin/tokens/' + one.id, { headers: hdr });
    const jDet = await rDet.json();
    ok(rDet.status === 200 && isMasked(jDet.key), '详情 GET /admin/tokens/{id} 的 key 是掩码（实际 ' + jDet.key + '）');
    const rKey = await fetch(base + '/admin/tokens/' + one.id + '/key', { method: 'POST', headers: hdr });
    const jKey = await rKey.json();
    const cc = String(rKey.headers.get('cache-control') || '');
    ok(rKey.status === 200, 'POST /admin/tokens/{id}/key ⇒ 200（实际 ' + rKey.status + '）');
    ok(cc.indexOf('no-store') >= 0, '取明文响应带 Cache-Control 且含 no-store（实际 ' + JSON.stringify(cc) + '）');
    const plain = String(jKey.key || '');
    plaintextKeys.push(plain);
    ok(plain.length === 48 && !/^sk-/.test(plain), '明文是 48 位裸 Key（无 sk- 前缀；实际长度 ' + plain.length + '，前缀 ' + plain.slice(0, 3) + '）');
    const rList2 = await fetch(base + '/admin/tokens?p=1&page_size=100', { headers: hdr });
    const listBody = await rList2.text();
    ok(listBody.indexOf(plain) < 0, '★ 列表响应体里**不含**任何明文 key（正反两面：上一条取到明文，这一条证它不外泄）');
    const detBody = await (await fetch(base + '/admin/tokens/' + one.id, { headers: hdr })).text();
    ok(detBody.indexOf(plain) < 0, '★ 详情响应体里**不含**明文 key');
    ok(listBody.indexOf(String(one.key)) >= 0, '列表里出现的是掩码（' + one.key + '）');

    // ── E) 编辑与启停（HTTP 面）──────────────────────────────────────────
    console.log('\n[E] 编辑名称 + 启停（HTTP 面）');
    const renamed = oneName + '-已改名';
    const rUpd = await fetch(base + '/admin/tokens/update', { method: 'POST', headers: hdr, body: JSON.stringify({ id: one.id, name: renamed, unlimited_quota: true, expired_time: -1 }) });
    ok(rUpd.status === 200, 'POST /admin/tokens/update（改名）⇒ 200（实际 ' + rUpd.status + '）');
    const jAfterUpd = await (await fetch(base + '/admin/tokens/' + one.id, { headers: hdr })).json();
    ok(jAfterUpd.name === renamed, '详情里名称已更新为 ' + renamed + '（实际 ' + jAfterUpd.name + '）');
    const rTog = await fetch(base + '/admin/tokens/update', { method: 'POST', headers: hdr, body: JSON.stringify({ id: one.id, status_only: true, status: 2 }) });
    ok(rTog.status === 200, 'status_only 切禁用 ⇒ 200（实际 ' + rTog.status + '）');
    const jDisabled = await (await fetch(base + '/admin/tokens/' + one.id, { headers: hdr })).json();
    ok(jDisabled.status === 2, '详情里 status=2（禁用；实际 ' + jDisabled.status + '）');
    await fetch(base + '/admin/tokens/update', { method: 'POST', headers: hdr, body: JSON.stringify({ id: one.id, status_only: true, status: 1 }) });

    // ── F) DOM：入口 / 表格 / 空态 / 抽屉 17 字段 / 建·改·停·删 ───────────
    console.log('\n[F] 浏览器（Chrome/CDP）：入口与页面本体');
    const CH_PORT = await freePort();
    await withChrome({
      port: CH_PORT, url: base + '/ui', token: TOKEN, width: 1500, height: 1000,
      outDir: path.join(os.tmpdir(), 'tokens-page-shots'),
      readyExpr: 'document.readyState === "complete"',
    }, async ({ ev, errs, dialogs, sleep }) => {
      await sleep(2500);

      // F1) 入口可点开：用页面自暴露的 showPanelByKey（按 data-key 切，不靠文字）
      const switched = await ev(`(function(){
        if (!window.__wb2api__ || !window.__wb2api__.showPanelByKey) return 'no-hook';
        return window.__wb2api__.showPanelByKey('tokens') ? 'ok' : 'no-section';
      })()`);
      ok(switched === 'ok', '面板切到 data-key="tokens"（实际 ' + switched + '；no-section = 入口 section 漏了）');
      await sleep(800);
      const fr = JSON.parse(await ev(`JSON.stringify((function(){
        var sec = document.querySelector('section[data-key="tokens"]');
        var f = document.getElementById('tkFrame');
        var doc = null; try { doc = f && f.contentDocument; } catch (e) { doc = null; }
        return {
          active: !!(sec && sec.classList.contains('active')),
          exists: !!f, src: f ? f.getAttribute('src') : null,
          readable: !!(doc && doc.body), url: doc ? String(doc.location && doc.location.pathname) : null
        };
      })())`));
      ok(fr.active, 'tokens section 进入 .active（实际 ' + fr.active + '）');
      ok(fr.exists && String(fr.src).indexOf('/ui/tokens') === 0, 'tkFrame 存在且 src=/ui/tokens（src=' + fr.src + '）');
      ok(fr.readable && fr.url === '/ui/tokens', 'tkFrame 的 contentDocument 可读且落在 /ui/tokens（实际 ' + fr.url + '）');

      // F2) 表格列名 + 空态文案（先把库清空，再看空态）
      const th = JSON.parse(await ev(`JSON.stringify((function(){
        var d = document.getElementById('tkFrame').contentDocument;
        return Array.prototype.map.call(d.querySelectorAll('table thead th'), function(x){ return x.textContent.trim(); });
      })())`));
      const needCols = ['名称', 'Key', '创建日期', '最近使用', '操作'];
      const missingCols = needCols.filter(function(c){ return !th.some(function(x){ return x.indexOf(c) >= 0; }); });
      ok(missingCols.length === 0, '表头含 名称/Key/创建日期/最近使用/操作（实际 ' + JSON.stringify(th) + '；缺 ' + JSON.stringify(missingCols) + '）');

      // 清空：把 HTTP 阶段建的都删掉，再点页面刷新 → 空态
      const delAll = await fetch(base + '/admin/tokens/delete', { method: 'POST', headers: hdr, body: JSON.stringify({ ids: createdIds.filter((x) => !!x) }) });
      const jDelAll = await delAll.json().catch(function(){ return {}; });
      console.log('      清空 ' + createdIds.length + ' 条 ⇒ ' + delAll.status + ' ' + JSON.stringify(jDelAll));
      await ev(`(function(){ document.getElementById('tkFrame').contentDocument.getElementById('btnRefresh').click(); })()`);
      await sleep(900);
      const empty = JSON.parse(await ev(`JSON.stringify((function(){
        var d = document.getElementById('tkFrame').contentDocument;
        var e = d.getElementById('empty');
        var rows = d.querySelectorAll('#rows tr').length;
        return { rows: rows, cls: e ? e.className : null, text: e ? e.textContent.replace(/\\s+/g,' ').trim() : null };
      })())`));
      ok(empty.rows === 0, '清空后表格 0 行（实际 ' + empty.rows + '）');
      ok(empty.cls === 'empty' && /还没有令牌/.test(String(empty.text)), '空态有明确文案（实际 ' + JSON.stringify(empty.text) + '）');

      // F3) 抽屉 17 个字段：逐字段存在 + 可交互
      await ev(`(function(){ document.getElementById('tkFrame').contentDocument.getElementById('btnNew').click(); })()`);
      await sleep(500);
      const fields = JSON.parse(await ev(`JSON.stringify((function(){
        var d = document.getElementById('tkFrame').contentDocument;
        var ids = ['f-name','f-count','f-mode-auto','f-mode-manual','f-automode','f-autogroup-pick','btnAutoAdd','f-autogroups',
                   'f-group-pick','btnGroupAdd','f-groups','f-cross','f-codes','f-retry','f-unlimited','f-amount',
                   'f-allowq','f-exp','btnNever','f-mllimit','f-models','f-ips','btnSave','btnCancel'];
        return ids.map(function(id){
          var el = d.getElementById(id);
          if (!el) return { id: id, exists: false };
          var dis = !!el.disabled;
          // interactable：可见（offsetParent 非 null）且未被 disabled；checkbox/radio 还要能切
          var vis = el.offsetParent !== null || el.type === 'checkbox' || el.type === 'radio';
          return { id: id, exists: true, disabled: dis, visible: vis, tag: el.tagName.toLowerCase() };
        });
      })())`));
      const missingF = fields.filter(function(f){ return !f.exists; });
      const disabledF = fields.filter(function(f){ return f.exists && f.disabled; });
      ok(missingF.length === 0, '创建抽屉字段全部存在（检查 ' + fields.length + ' 个控件；缺 ' + JSON.stringify(missingF.map(function(f){return f.id;})) + '）');
      ok(disabledF.length === 0, '没有任何字段处于 disabled（异常 ' + JSON.stringify(disabledF.map(function(f){return f.id;})) + '）');
      // 条件字段（自动/有序分组、额度、模型限制）**默认是隐藏的** —— 所以「可见」必须在各自
      // 触发条件成立之后再判。第一版一次性断言「全都可见」，把设计当成了缺陷（22 项失败里有 1 项是它）。
      const vis = JSON.parse(await ev(`JSON.stringify((function(){
        var d = document.getElementById('tkFrame').contentDocument;
        var seen = {};
        function rec(ids){ ids.forEach(function(id){ var el = d.getElementById(id); if (el && el.offsetParent !== null) { seen[id] = true; } }); }
        function fire(id){ var el = d.getElementById(id); el.dispatchEvent(new Event('change', { bubbles: true })); }
        rec(['f-name','f-count','f-mode-auto','f-mode-manual','f-automode','f-unlimited','f-allowq','f-exp','btnNever','f-mllimit','f-ips','btnSave','btnCancel']);
        d.getElementById('f-mode-manual').checked = true; fire('f-mode-manual');
        rec(['f-group-pick','btnGroupAdd','f-groups']);
        d.getElementById('f-mode-auto').checked = true; fire('f-mode-auto');
        var am = d.getElementById('f-automode'); am.value = 'custom'; fire('f-automode');
        rec(['f-autogroup-pick','btnAutoAdd','f-autogroups']);
        var u = d.getElementById('f-unlimited'); u.checked = false; fire('f-unlimited');
        rec(['f-amount']);
        var ml = d.getElementById('f-mllimit'); ml.checked = true; fire('f-mllimit');
        rec(['f-models']);
        return { seen: seen, groupPickOptions: d.getElementById('f-group-pick').options.length };
      })())`));
      const mustSee = ['f-name','f-count','f-mode-auto','f-mode-manual','f-automode','f-group-pick','btnGroupAdd','f-groups',
                       'f-autogroup-pick','btnAutoAdd','f-autogroups','f-unlimited','f-amount','f-allowq','f-exp','btnNever',
                       'f-mllimit','f-models','f-ips','btnSave','btnCancel'];
      const notSeen = mustSee.filter(function(id){ return !vis.seen[id]; });
      ok(notSeen.length === 0, '每个字段在**自己的触发条件下**可见且可交互（未取到样本：' + JSON.stringify(notSeen) + '）');
      // f-codes / f-retry 要多一个条件：**已选 ≥2 个分组**（webui-tokens.html 的 syncVisibility）。
      // 候选够就真驱动一次（选两个分组 → 加入），不够就明确 SKIP —— 不假装覆盖。
      if (vis.groupPickOptions >= 2) {
        const codesVis = JSON.parse(await ev(`JSON.stringify((function(){
          var d = document.getElementById('tkFrame').contentDocument;
          var pick = d.getElementById('f-group-pick');
          var add = d.getElementById('btnGroupAdd');
          pick.value = pick.options[0].value; add.click();
          pick.value = pick.options[1].value; add.click();
          return {
            codes: d.getElementById('f-codes').offsetParent !== null,
            retry: d.getElementById('f-retry').offsetParent !== null,
            selected: d.querySelectorAll('#f-groups .dragitem').length
          };
        })())`));
        ok(codesVis.selected >= 2 && codesVis.codes && codesVis.retry, '选满 2 个分组后 f-codes/f-retry 可见（已选 ' + codesVis.selected + ' 个）');
      } else {
        sk('f-codes / f-retry 的可见性未取到样本：它们要「已选 ≥2 个分组」才显示，而本隔离实例只有 ' + vis.groupPickOptions + ' 个可选分组；两个控件本身存在且未 disabled，已由上一组断言覆盖');
      }

      // F4) 抽屉建 1 条：成功 + 列表出现 + Key 是掩码
      console.log('\n[G] 抽屉建 1 条 / count=3 / 43 字节前端守卫');
      const drawerCreate = async (name, count) => {
        await ev(`(function(){ document.getElementById('tkFrame').contentDocument.getElementById('btnNew').click(); })()`);
        await sleep(400);
        await ev(`(function(){
          var d = document.getElementById('tkFrame').contentDocument;
          d.getElementById('f-name').value = ${JSON.stringify(name)};
          var c = d.getElementById('f-count');
          c.value = ${JSON.stringify(String(count))};
          c.dispatchEvent(new Event('input', { bubbles: true }));
          return true;
        })()`);
        await sleep(200);
        await ev(`(function(){ document.getElementById('tkFrame').contentDocument.getElementById('btnSave').click(); })()`);
        await sleep(1200);
        return JSON.parse(await ev(`JSON.stringify((function(){
          var d = document.getElementById('tkFrame').contentDocument;
          var t = d.getElementById('toast');
          var rows = Array.prototype.map.call(d.querySelectorAll('#rows tr'), function(tr){
            var tds = tr.querySelectorAll('td');
            return { name: tds[0] ? tds[0].textContent.trim() : '', key: tds[1] ? tds[1].textContent.trim() : '' };
          });
          return { toast: t ? t.textContent.trim() : '', toastCls: t ? t.className : '', rows: rows };
        })())`));
      };

      const one = await drawerCreate('页面建一条', 1);
      ok(one.rows.length === 1 && one.rows[0].name === '页面建一条', '抽屉建 1 条后列表出现该条（实际 ' + JSON.stringify(one.rows) + '）');
      ok(isMasked(one.rows[0].key), '列表里的 Key 是掩码（实际 ' + one.rows[0].key + '）');
      ok(/已创建 1 条/.test(one.toast), '成功提示是「已创建 1 条」（实际 ' + JSON.stringify(one.toast) + '）');

      const three = await drawerCreate('页面批量', 3);
      const batchRows = three.rows.filter((r) => r.name.indexOf('页面批量') === 0);
      ok(three.rows.length === 4, 'count=3 后列表 4 行（实际 ' + three.rows.length + '）');
      // ⚠ 列表按 id 倒序（新的在前）⇒ 创建顺序里的「第 1 条」在页面上是**最后**一行。
      //   所以只能按「恰好 1 条原名 + 恰好 2 条带 6 位后缀」断言，不能按下标。
      const exactBase = batchRows.filter((r) => r.name === '页面批量');
      const suffixed = batchRows.filter((r) => /^页面批量-.{6}$/.test(r.name));
      ok(exactBase.length === 1 && suffixed.length === 2, '三条批量 = 1 条原名 + 2 条带 6 位后缀（实际 ' + JSON.stringify(batchRows.map((r) => r.name)) + '）');

      const tooLong = await drawerCreate('q'.repeat(50), 2);
      ok(tooLong.rows.length === 4, '50 字节 + count=2 被前端拦住，没有新增行（实际 ' + tooLong.rows.length + '）');
      // 只断言**文案**：toast 的 on/bad 类名会被页面自己的定时器复位（读到的时刻可能已复位），
      // 用它判红绿会变成时序敏感的假红 —— 文案才是判据。
      ok(/43 字节/.test(tooLong.toast), '前端给的是 43 字节上限的提示（实际 ' + JSON.stringify(tooLong.toast) + '）');
      await ev(`(function(){ document.getElementById('tkFrame').contentDocument.getElementById('btnCancel').click(); })()`);
      await sleep(300);

      // F5) 行内操作：编辑 / 启停 / 取明文 / 删除
      console.log('\n[H] 编辑 / 启停 / 取明文 / 删除（DOM）');
      const clickRowAct = async (act, idx) => {
        return JSON.parse(await ev(`JSON.stringify((function(){
          var d = document.getElementById('tkFrame').contentDocument;
          var btns = d.querySelectorAll('#rows button[data-act="${act}"]');
          var b = btns[${idx}];
          if (!b) return { clicked: false, found: btns.length };
          b.click();
          return { clicked: true, found: btns.length, id: b.getAttribute('data-id') };
        })())`));
      };
      const editClick = await clickRowAct('edit', 0);
      ok(editClick.clicked, '点得到第 1 行的「编辑」（实际 ' + JSON.stringify(editClick) + '）');
      await sleep(600);
      const editedName = '页面改过名';
      await ev(`(function(){ document.getElementById('tkFrame').contentDocument.getElementById('f-name').value = ${JSON.stringify(editedName)}; })()`);
      await ev(`(function(){ document.getElementById('tkFrame').contentDocument.getElementById('btnSave').click(); })()`);
      await sleep(1200);
      const afterEdit = JSON.parse(await ev(`JSON.stringify(Array.prototype.map.call(document.getElementById('tkFrame').contentDocument.querySelectorAll('#rows tr td:first-child'), function(td){ return td.textContent.trim(); }))`));
      ok(afterEdit.indexOf(editedName) >= 0, '改名后列表出现「' + editedName + '」（实际 ' + JSON.stringify(afterEdit) + '）');

      const beforeToggle = JSON.parse(await ev(`JSON.stringify(Array.prototype.map.call(document.getElementById('tkFrame').contentDocument.querySelectorAll('#rows tr'), function(tr){ var b = tr.querySelector('button[data-act="toggle"]'); return b ? b.textContent.trim() : null; }))`));
      const togClick = await clickRowAct('toggle', 0);
      ok(togClick.clicked, '点得到第 1 行的「禁用/启用」（实际 ' + JSON.stringify(togClick) + '）');
      await sleep(1200);
      const afterToggle = JSON.parse(await ev(`JSON.stringify(Array.prototype.map.call(document.getElementById('tkFrame').contentDocument.querySelectorAll('#rows tr'), function(tr){ var b = tr.querySelector('button[data-act="toggle"]'); return b ? b.textContent.trim() : null; }))`));
      ok(beforeToggle[0] !== afterToggle[0], '启停后按钮文案翻转（' + beforeToggle[0] + ' → ' + afterToggle[0] + '）');
      const firstToggleId = Number(togClick.id);
      const jStatus = await (await fetch(base + '/admin/tokens/' + firstToggleId, { headers: hdr })).json();
      ok(jStatus.status === 1 || jStatus.status === 2, '服务端也能查到该条状态（id=' + firstToggleId + ' status=' + jStatus.status + '）');

      await ev(`(function(){
        var w = document.getElementById('tkFrame').contentWindow;
        w.__keyReq = 0;
        try { w.navigator.clipboard.writeText = function(){ return Promise.resolve(); }; } catch (e) { }
        var of = w.fetch;
        w.fetch = function(u, i){ if (String(u).indexOf('/key') >= 0) { w.__keyReq++; } return of.apply(this, arguments); };
        return true;
      })()`);
      const copyClick = await clickRowAct('copy', 0);
      ok(copyClick.clicked, '点得到「复制」（实际 ' + JSON.stringify(copyClick) + '）');
      await sleep(900);
      const keyReq = await ev(`document.getElementById('tkFrame').contentWindow.__keyReq`);
      const copyToast = await ev(`document.getElementById('tkFrame').contentDocument.getElementById('toast').textContent.trim()`);
      ok(Number(keyReq) === 1, '点复制触发了 POST /admin/tokens/{id}/key（实际请求数 ' + keyReq + '）');
      ok(String(copyToast).length > 0, '复制后给了用户反馈（实际 ' + JSON.stringify(copyToast) + '）');

      const rowsBefore = Number(await ev(`document.getElementById('tkFrame').contentDocument.querySelectorAll('#rows tr').length`));
      const delClick = await clickRowAct('del', 0);
      const delId = Number(delClick.id);
      await sleep(1400);
      const rowsAfter = Number(await ev(`document.getElementById('tkFrame').contentDocument.querySelectorAll('#rows tr').length`));
      ok(rowsAfter === rowsBefore - 1, '删除后列表少一行（' + rowsBefore + ' → ' + rowsAfter + '）');
      const confirmDlg = dialogs.filter((d) => d.type === 'confirm');
      ok(confirmDlg.length >= 1 && /删除令牌/.test(confirmDlg[confirmDlg.length - 1].message), '删除前弹了确认框且文案点明删的是令牌（实际 ' + JSON.stringify(confirmDlg) + '）');
      const rGone = await fetch(base + '/admin/tokens/' + delId, { headers: hdr });
      ok(rGone.status === 404, '被删的 id 详情 ⇒ 404（实际 ' + rGone.status + '；id=' + delId + '）');

      // F9) 部分成功：存在性断言 + 明确声明行为未覆盖
      console.log('\n[I] 部分成功展示（能覆盖到哪一步，如实说）');
      const partial = JSON.parse(await ev(`JSON.stringify((function(){
        var d = document.getElementById('tkFrame').contentDocument;
        var el = d.getElementById('partial');
        return { exists: !!el, cls: el ? el.className : null };
      })())`));
      ok(partial.exists && String(partial.cls).indexOf('hidden') >= 0, '#partial 容器存在且默认隐藏（只证明展示位存在）');
      sk('「部分成功」的**行为**未覆盖：createTokens 只在单条 CreateToken 失败时产生 failures（tokens.go:708-711），而该分支要么靠 48 位 Key 唯一索引碰撞、要么靠 SetAutoGroups 编码失败 —— 公共 API 无法确定性触发；handler 级用例（internal/admin/tokens_test.go）同样没有构造它。要覆盖得注入故障（改实现或加测试钩子），超出"只新增探针文件"的范围。');

      // F10) 控制台卫生：全程无未捕获异常 / console.error
      console.log('\n[J] 控制台卫生');
      ok(errs.length === 0, '全程无未捕获异常 / console.error（实际 ' + errs.length + ' 条' + (errs.length ? '：' + JSON.stringify(errs.slice(0, 3)) : '') + '）');
      console.log('      原生对话框（自动接受）共 ' + dialogs.length + ' 个：' + JSON.stringify(dialogs.map((d) => d.type)));
    });
  } catch (e) {
    console.log('EXCEPTION: ' + (e && e.message));
    fail++;
  } finally {
    try { await inst.stop(); } catch { }
  }
  console.log('\n' + (fail === 0 ? '=== 令牌页（/ui/tokens）验收通过 ===' : '=== ' + fail + ' 项失败 ===')
    + (skip ? '（另有 ' + skip + ' 项 SKIP，原因见上）' : ''));
  process.exit(fail ? 1 : 0);
})();



