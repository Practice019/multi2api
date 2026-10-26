// verify_groups_panel.js —— 分组面板（/ui/groups）T86 新增能力的端到端探针。
//
// 覆盖：列表渲染 / used_by_count 显示 / 行内展开出「模型集合 + 对外名映射」两块 /
//       「谁在用」三类（API key·渠道·令牌）/ 空引用文案 / 404 显示为错误 / 两档宽度几何。
//
// ⚠ 端点契约由 T85 实现（GET /admin/groups 的 used_by_count + GET /admin/groups/usage?id=）。
//   若后端未落地，本探针的对应断言**会红** —— 那是如实报告，不是探针坏了。
// ⚠ 必须在 require(lib_instance) **之前**设 WB2API_EXE（模块求值时就把 EXE 定死）。
if (!process.env.WB2API_EXE) process.env.WB2API_EXE = 'bin/wb2api-server-probe.exe';
'use strict';
const net = require('net');
const path = require('path');
const { startInstance } = require('./lib_instance.js');
const { withChrome } = require('./lib_cdp.js');
function freePort() { return new Promise((res, rej) => { const s = net.createServer(); s.on('error', rej); s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); }); }); }
(async () => {
  let fail = 0, skip = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
  const sk = (m) => { console.log('  SKIP ' + m); skip++; };
  const PORT = await freePort();
  const ADMIN = 'probe-key-groups-t86';   // 与 verify_tokens_page.js 同款：用它当管理凭据（Bearer）
  console.log('实例端口（现场探测）: ' + PORT + '（绝不是 7863）');
  const inst = await startInstance({ port: PORT, apiKey: ADMIN });
  const base = inst.base;
  const hdr = { Authorization: 'Bearer ' + ADMIN, 'Content-Type': 'application/json' };
  try {
    // ── 夹具：先建一个分组（打**真端点**），否则列表为空、后面所有行级断言都无从谈起。
    //   这一步本身也是断言：分组功能没接线时它会红，而不是让下面的断言全部以「0 行」的形式『红得没有信息』。
    const seed = await fetch(base + '/admin/groups', {
      method: 'POST', headers: hdr,
      body: JSON.stringify({ name: 'probe-group-t86', description: 'T86 探针夹具', models: ['probe-model-a', 'probe-model-b'] }),
    });
    ok(seed.status === 200, '夹具：POST /admin/groups 建了一个探针分组（实际 ' + seed.status + '；404 = 分组功能未接线）');
    await withChrome({ port: await freePort(), url: base + '/ui', token: inst.key, width: 1920, height: 1080,
      outDir: path.join(process.env.TEMP || '.', 't86-shots'), readyExpr: '!!window.__wb2api__' }, async ({ ev, send, sleep }) => {
      // 打开 groups 面板（按 DOM 顺序找 section，不写死导航文案）
      const keys = JSON.parse(await ev('JSON.stringify(Array.from(document.querySelectorAll(".content > section")).map(function(s){return s.getAttribute("data-key");}))'));
      ok(keys.indexOf('groups') >= 0, '控制台里有 data-key=groups 的面板（实际：' + JSON.stringify(keys) + '）');
      await ev('window.__wb2api__.showPanelByKey("groups")');
      await sleep(1800);
      const fr = await ev('!!document.getElementById("grpFrame") && document.getElementById("grpFrame").contentDocument !== null');
      ok(fr === true, 'grpFrame 存在且 contentDocument 可读（同源 + 鉴权通过）');
      const D = 'document.getElementById("grpFrame").contentDocument';
      // 列表渲染
      const rows = Number(await ev(D + '.querySelectorAll("#rows tr.grow").length')) || 0;
      ok(rows > 0, '列表渲染出 ' + rows + ' 行（> 0）');
      // used_by_count 列
      const ub = JSON.parse(await ev('JSON.stringify((function(){var d=' + D + ';var ths=Array.prototype.map.call(d.querySelectorAll("thead th"),function(t){return t.textContent.trim();});var first=d.querySelector("#rows tr.grow");return {ths:ths,cell:first?first.children[3].textContent.trim():null};})())'));
      ok(ub.ths.indexOf('引用') >= 0, '表头有「引用」列（实际 ' + JSON.stringify(ub.ths) + '）');
      ok(ub.cell !== null && ub.cell.length > 0, '首行的引用格有文案（实际 ' + JSON.stringify(ub.cell) + '；契约要求 0 时给中性文案而不是「0」）');
      // 行内展开
      await ev(D + '.querySelector("#rows tr.grow").click()');
      await sleep(1200);
      const det = JSON.parse(await ev('JSON.stringify((function(){var d=' + D + ';var g=d.querySelector("tr.gdetail:not([hidden])");if(!g)return {open:false};var t=g.textContent;return {open:true,hasModels:t.indexOf("模型集合")>=0,hasMap:t.indexOf("对外名映射")>=0,hasUsage:t.indexOf("谁在用")>=0,text:t.slice(0,160)};})())'));
      ok(det.open === true, '点行后明细行展开（hidden 被移除）');
      ok(det.hasModels === true, '明细里有「模型集合」块');
      ok(det.hasMap === true, '明细里有「对外名映射」块（一对多，不许压单值）');
      ok(det.hasUsage === true, '明细里有「谁在用」块');
      // 引用视图：三类 + 空引用文案（真打端点；T85 未落地时这里会红）
      await sleep(1200);
      const usage = JSON.parse(await ev('JSON.stringify((function(){var d=' + D + ';var h=d.querySelector("tr.gdetail:not([hidden]) [id^=gu-]");if(!h)return {err:"no usage host"};return {text:h.textContent.trim().slice(0,200),hasErrClass:!!h.querySelector(".err")};})())'));
      ok(!usage.err, '「谁在用」容器存在（实际 ' + JSON.stringify(usage) + '）');
      ok(usage.hasErrClass === false && /API key|暂无引用|无/.test(usage.text || ''), '引用视图拿到了结果（三类或「暂无引用」文案；实际 ' + JSON.stringify(usage.text) + '）');
      // 两档宽度几何（照 verify_tokens_page.js 的 [K] 段判据）
      const GEO = '(function(){try{var f=document.getElementById("grpFrame");var d=f.contentDocument;function R(el){if(!el)return null;var b=el.getBoundingClientRect();return [Math.round(b.x),Math.round(b.y),Math.round(b.width),Math.round(b.height)];}var t=d.querySelector("table");var row=d.querySelector("#rows tr.grow");return JSON.stringify({innerH:f.contentWindow.innerHeight,innerW:f.contentWindow.innerWidth,bodyScroll:d.body.scrollHeight,table:R(t),row:R(row)});}catch(e){return JSON.stringify({err:String(e)});}})()';
      for (const dim of [[1920, 1080], [375, 812]]) {
        await send('Emulation.setDeviceMetricsOverride', { width: dim[0], height: dim[1], deviceScaleFactor: 1, mobile: false });
        await sleep(800);
        const g = JSON.parse(await ev(GEO));
        console.log('    @' + dim[0] + 'x' + dim[1] + ' 几何: ' + JSON.stringify(g));
        if (g.err) { ok(false, '[' + dim[0] + '] 几何采集失败：' + g.err); continue; }
        ok(g.bodyScroll > g.innerH * 0.15, '[' + dim[0] + '] 根容器 scrollHeight(' + g.bodyScroll + ') 显著大于 0（防塌陷）');
        ok(!!g.table && g.table[3] > 0, '[' + dim[0] + '] 表格 rect 高度 > 0（实际 ' + JSON.stringify(g.table) + '）');
        ok(!!g.row && g.row[1] >= 0, '[' + dim[0] + '] 首行未被裁（top=' + (g.row ? g.row[1] : 'n/a') + ' ≥ 0）');
        ok(!!g.row && (g.row[1] + g.row[3]) <= g.innerH + 1, '[' + dim[0] + '] 首行在 iframe 可视区内（底边 ' + (g.row ? g.row[1] + g.row[3] : 'n/a') + ' ≤ innerH ' + g.innerH + '）');
      }
      await send('Emulation.clearDeviceMetricsOverride');
    });
  } catch (e) {
    console.log('EXCEPTION: ' + (e && e.message));
    fail++;
  } finally { try { await inst.stop(); } catch { } }
  console.log('\n' + (fail === 0 ? '=== 分组面板（/ui/groups）验收通过 ===' : '=== ' + fail + ' 项失败 ===') + (skip ? '（另有 ' + skip + ' 项 SKIP）' : ''));
  process.exit(fail ? 1 : 0);
})();