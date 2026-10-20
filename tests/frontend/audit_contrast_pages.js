// audit_contrast_pages.js —— 逐页逐元素的对比度巡检（视觉可读性的机器判据）。
//
// # 为什么它比 contrast_e2e 更进一步
// contrast_e2e 只量 5 个代表元素（thead th / tbody td / pre / .pill / .pill.dim）——
// 那是**抽样**：它保证这 5 个位置没问题，保证不了整页没有看不清的地方。
// 本轮 maximalism 把分组头、分区标题带、统计卡都铺上了亮色底，这些位置不在抽样
// 清单里，于是「分组头上的次要文字铺在粉底上糊成一片」可以一路绿着交付。
//
// 本探针换成**全量**：遍历每个面板的每个可见文本元素，
//   · 从 body 到它自己把所有祖先背景按 alpha 合成出真实底色（半透明 color-mix 也算对）
//   · 用 WCAG 公式算实际对比度，正文阈值 4.5:1、大字号 3:1
//   · 不达标就点名：CSS 路径 + 文本片段 + 前景/底色 + 实测比值 + 字号字重
//
// 用法：
//   AUDIT_URL=http://127.0.0.1:7897/ui AUDIT_USER=xx AUDIT_PASS=yy node audit_contrast_pages.js
//   可选：AUDIT_THEME=light|dark 只跑一套；AUDIT_MIN=4.5 改阈值
// 退出码：有任一条低于阈值 ⇒ 1（可直接进 CI / 当交付门禁）。
'use strict';
const path = require('path');
const fs = require('fs');
const LIB = require('./lib_contrast_audit.js');

const OUTDIR = path.resolve(__dirname, '..', '..', '.task', 'maximalism-v3', 'contrast');
const ONLY = process.env.AUDIT_THEME || '';
const MIN = Number(process.env.AUDIT_MIN || 4.5);

async function main() {
  const base = process.env.AUDIT_URL || '';
  if (!base) { console.error('需要 AUDIT_URL（例如 http://127.0.0.1:7897/ui）'); process.exit(2); }
  const user = process.env.AUDIT_USER || '';
  const pass = process.env.AUDIT_PASS || '';
  const { withChrome, sleep } = require('./lib_cdp.js');
  fs.mkdirSync(OUTDIR, { recursive: true });
  const report = [];
  await withChrome({ port: 9431, url: base, token: process.env.WB2API_PROBE_TOKEN || false, width: 1600, height: 1000, outDir: OUTDIR,
    readyExpr: 'document.readyState === "complete"' }, async ({ ev }) => {
    if (user) {
      const st = await ev('(async () => { const r = await fetch("/admin/auth/login", {method:"POST", headers:{"Content-Type":"application/json"}, body: JSON.stringify({username:' + JSON.stringify(user) + ', password:' + JSON.stringify(pass) + '})}); return r.status; })()');
      if (st !== 200) { console.error('登录失败 status=' + st); process.exit(3); }
      await ev('location.replace("/ui")');
      await sleep(3200);
    }
    const keys = JSON.parse(await ev('JSON.stringify(Array.from(document.querySelectorAll(".content > section")).map(s => s.getAttribute("data-key")))'));
    await ev('window.__audit = ' + LIB.source + ';');
    const themes = ONLY ? [ONLY] : ['light', 'dark'];
    for (const theme of themes) {
      await ev('window.__wb2api__ && window.__wb2api__.setThemePref(' + JSON.stringify(theme) + ')');
      await sleep(500);
      for (const key of keys) {
        await ev('window.__wb2api__.showPanelByKey(' + JSON.stringify(key) + ')');
        await sleep(750);
        const rows = JSON.parse(await ev('JSON.stringify(window.__audit({min: ' + MIN + '}))'));
        for (const r of rows) report.push(Object.assign({ theme: theme, panel: key }, r));
        console.log('  [' + theme + '] ' + String(key).padEnd(10) + ' 低于阈值 ' + rows.length + ' 处');
      }
    }
    await ev('window.__wb2api__.setThemePref("system")');
  });
  report.sort((a, b) => a.cr - b.cr);
  fs.writeFileSync(path.join(OUTDIR, 'audit.json'), JSON.stringify(report, null, 2));
  console.log('');
  console.log('=== 汇总：' + report.length + ' 处低于阈值（按严重度排序，最多 40 条）===');
  for (const r of report.slice(0, 40)) {
    console.log('  ' + String(r.cr).padStart(5) + ':1 (需 ' + r.need + ') [' + r.theme + '/' + r.panel + '] ' + r.sel + '  "' + r.text + '"  fg=' + r.fg + ' bg=' + r.bg);
  }
  const bySel = {};
  for (const r of report) bySel[r.sel] = (bySel[r.sel] || 0) + 1;
  if (report.length) {
    console.log('');
    console.log('=== 按选择器去重（前 20）===');
    Object.entries(bySel).sort((a, b) => b[1] - a[1]).slice(0, 20).forEach(function (kv) { console.log('  ' + kv[1] + '  ' + kv[0]); });
  }
  console.log('');
  console.log('清单：' + path.join(OUTDIR, 'audit.json'));
  process.exit(report.length ? 1 : 0);
}
main().catch(function (e) { console.error('EX ' + (e && e.message)); process.exit(2); });
