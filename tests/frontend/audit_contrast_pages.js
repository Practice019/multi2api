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
// # v3.1（2026-10-26）把它从「只扫 /ui」扩到「每个 iframe 页各扫一次」
//
// 现状缺陷：/ui/status、/ui/channels、/ui/models 各自是**独立 document**，
// 而本探针原来只遍历控制台外壳的 \`.content > section\` 面板 —— 三个 iframe 页
// **完全无人看守**（它们有自己的 <style>、自己的主题变量，与控制台不是同一份）。
// 现在按 AUDIT_PAGES 逐个文档导航、逐个量。
//
// ⚠ 主题怎么切（两类页面的机制不同，必须分开处理）：
//   · 控制台 /ui     → window.__wb2api__.setThemePref(theme)（外壳自己写 data-theme）
//   · iframe 三页    → 它们读 localStorage('wb2api.theme') 或 prefers-color-scheme。
//                      所以**导航前**先写 localStorage，导航后再兜底写
//                      documentElement.dataset.theme —— 两条都做，避免"测错主题"
//                      这种最隐蔽的假绿（量了浅色却以为在量深色）。
//
// 用法：
//   AUDIT_URL=http://127.0.0.1:7897/ui node audit_contrast_pages.js
//   可选：AUDIT_THEME=light|dark 只跑一套；AUDIT_MIN=4.5 改阈值
//         AUDIT_PAGES=/ui,/ui/status,/ui/channels,/ui/models 改扫描清单（默认就是这四个）
// 退出码：有任一条低于阈值 ⇒ 1（可直接进 CI / 当交付门禁）。
'use strict';
const path = require('path');
const fs = require('fs');
const LIB = require('./lib_contrast_audit.js');

const OUTDIR = path.resolve(__dirname, '..', '..', '.task', 'maximalism-v3', 'contrast');
const ONLY = process.env.AUDIT_THEME || '';
const MIN = Number(process.env.AUDIT_MIN || 4.5);
// 默认四个文档：控制台外壳 + 三个 iframe 页（后三个此前无人看守）
const PAGES = String(process.env.AUDIT_PAGES || '/ui,/ui/status,/ui/channels,/ui/models')
  .split(',').map((s) => s.trim()).filter(Boolean);

// setTheme 把主题写进 localStorage，供页面自己的启动脚本读取。
// ⚠ 必须在**导航之前**执行：三个 iframe 页在 <head> 里同步读它。
function setThemeExpr(theme) {
  return 'try{localStorage.setItem("wb2api.theme",' + JSON.stringify(theme) + ')}catch(e){}';
}

async function main() {
  const base = process.env.AUDIT_URL || '';
  if (!base) { console.error('需要 AUDIT_URL（例如 http://127.0.0.1:7897/ui）'); process.exit(2); }
  const user = process.env.AUDIT_USER || '';
  const pass = process.env.AUDIT_PASS || '';
  const origin = new URL(base).origin;
  const { withChrome, sleep } = require('./lib_cdp.js');
  fs.mkdirSync(OUTDIR, { recursive: true });
  const report = [];
  const perPage = new Map();
  await withChrome({ port: Number(process.env.AUDIT_CDP_PORT || 9431), url: base, token: process.env.WB2API_PROBE_TOKEN || false,
    width: 1600, height: 1000, outDir: OUTDIR,
    readyExpr: 'document.readyState === "complete"' }, async ({ ev }) => {
    if (user) {
      const st = await ev('(async () => { const r = await fetch("/admin/auth/login", {method:"POST", headers:{"Content-Type":"application/json"}, body: JSON.stringify({username:' + JSON.stringify(user) + ', password:' + JSON.stringify(pass) + '})}); return r.status; })()');
      if (st !== 200) { console.error('登录失败 status=' + st); process.exit(3); }
      await ev('location.replace("/ui")');
      await sleep(3200);
    }
    const themes = ONLY ? [ONLY] : ['light', 'dark'];
    const bump = (page, theme, sel, n) => {
      const k = page + '|' + theme;
      const cur = perPage.get(k) || { total: 0, bySel: {} };
      cur.total += n;
      if (sel) cur.bySel[sel] = (cur.bySel[sel] || 0) + n;
      perPage.set(k, cur);
    };

    for (const page of PAGES) {
      const url = origin + page;
      // ── 控制台外壳：沿用原来的"逐面板"逻辑（面板是 CSS 隐藏的 section，必须逐个显示）
      if (page === '/ui' || page === '/ui/') {
        for (const theme of themes) {
          await ev(setThemeExpr(theme));
          await ev('window.__wb2api__ && window.__wb2api__.setThemePref(' + JSON.stringify(theme) + ')');
          await sleep(500);
          const keys = JSON.parse(await ev('JSON.stringify(Array.from(document.querySelectorAll(".content > section")).map(s => s.getAttribute("data-key")))'));
          await ev('window.__audit = ' + LIB.source + ';');
          for (const key of keys) {
            await ev('window.__wb2api__.showPanelByKey(' + JSON.stringify(key) + ')');
            await sleep(750);
            const rows = JSON.parse(await ev('JSON.stringify(window.__audit({min: ' + MIN + '}))'));
            for (const r of rows) report.push(Object.assign({ page: page, theme: theme, panel: key }, r));
            bump(page, theme, null, rows.length);
            console.log('  [' + theme + '] ' + page + ' ' + String(key).padEnd(10) + ' 低于阈值 ' + rows.length + ' 处');
          }
        }
        await ev('window.__wb2api__.setThemePref("system")');
        continue;
      }
      // ── iframe 页：独立 document，直接导航过去整页量（没有面板概念）
      for (const theme of themes) {
        await ev(setThemeExpr(theme));
        await ev('location.href = ' + JSON.stringify(url));
        // 等新文档就绪（导航后 window.__audit 会丢，必须重新注入）
        for (let i = 0; i < 40; i++) {
          await sleep(200);
          const ready = await ev('document.readyState === "complete" && !!document.body');
          if (ready) break;
        }
        await ev(setThemeExpr(theme));
        await ev('document.documentElement.setAttribute("data-theme", ' + JSON.stringify(theme) + ')');
        await sleep(900);
        // 自证"量的是对的主题"：把实际生效的 data-theme 打出来（防"测错主题"的假绿）
        const applied = await ev('document.documentElement.getAttribute("data-theme")');
        await ev('window.__audit = ' + LIB.source + ';');
        const rows = JSON.parse(await ev('JSON.stringify(window.__audit({min: ' + MIN + '}))'));
        for (const r of rows) report.push(Object.assign({ page: page, theme: theme, panel: '(whole document)' }, r));
        bump(page, theme, null, rows.length);
        console.log('  [' + theme + '] ' + page + ' ' + String('(whole)').padEnd(10) + ' 低于阈值 ' + rows.length + ' 处（生效 data-theme=' + applied + '）');
      }
    }
  });
  report.sort((a, b) => a.cr - b.cr);
  fs.writeFileSync(path.join(OUTDIR, 'audit.json'), JSON.stringify(report, null, 2));
  console.log('');
  console.log('=== 逐页汇总（每页每主题的不达标数）===');
  for (const page of PAGES) {
    for (const theme of (ONLY ? [ONLY] : ['light', 'dark'])) {
      const cur = perPage.get(page + '|' + theme) || { total: 0 };
      console.log('  ' + String(cur.total).padStart(4) + '  ' + page.padEnd(16) + theme);
    }
  }
  console.log('');
  console.log('=== 汇总：' + report.length + ' 处低于阈值（按严重度排序，最多 40 条）===');
  for (const r of report.slice(0, 40)) {
    console.log('  ' + String(r.cr).padStart(5) + ':1 (需 ' + r.need + ') [' + r.theme + r.page + '/' + r.panel + '] ' + r.sel + '  "' + r.text + '"  fg=' + r.fg + ' bg=' + r.bg);
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
