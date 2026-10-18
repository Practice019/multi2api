// shot_settings.js —— 「设置」页拍图 + 诊断（2026-10-19）。
//
// # 这一轮要先回答的问题是"该不该做分类导航"
//
// 参考项目 Agent2API 的设置页是两栏（左栏 140px 常驻分类导航 + 右栏独占滚动），
// 它给的理由是「设置项按分类拆开后，导航必须常驻可点，不能跟着内容一起滚走」。
// 但**那条理由成立的前提是页面足够长** —— 如果一屏就看得完，
// 加导航只是多一个要点两次的东西。
//
// 所以我们先量：**设置页总高 / 视口高**。这个比值决定抄不抄那条。
// 这一份脚本的价值就在这 —— 先量再决定，而不是照着参考硬搬。
//
// ⚠ 调试口 9337（9329 accounts / 9331 heatmap / 9335 jobs / 9336 logs）。
'use strict';
const { withChrome, until, showPanel } = require('./lib_cdp.js');

const TARGET = process.env.SETTINGS_URL || 'http://127.0.0.1:18080/ui';
let exitCode = 0;

(async () => {
  await withChrome({
    port: 9337,
    url: TARGET,
    readyExpr: `document.querySelectorAll('[data-key="settings"]').length`,
  }, async ({ ev, errs, shot, sleep }) => {
    await showPanel(ev, 'settings', 1200);

    const D = JSON.parse(await ev(`(function(){
      var sec = document.querySelector('[data-key="settings"]');
      if (!sec) return JSON.stringify({err:'设置面板不存在'});
      var r = sec.getBoundingClientRect();
      var sets = sec.querySelectorAll('fieldset');
      var rows = [];
      for (var i = 0; i < sets.length; i++) {
        var f = sets[i];
        var rect = f.getBoundingClientRect();
        var lg = f.querySelector('legend');
        rows.push({
          legend: lg ? lg.textContent.trim().replace(/\\s+/g, ' ') : '(无)',
          h: Math.round(rect.height),
          readonly: f.classList.contains('readonly'),
          badges: Array.prototype.map.call(f.querySelectorAll('.badge'), function(b){
            return b.textContent.trim(); })
        });
      }
      var grids = sec.querySelectorAll('.formgrid');
      var gridCols = [];
      for (var g = 0; g < grids.length; g++) {
        gridCols.push(getComputedStyle(grids[g]).gridTemplateColumns);
      }
      var nums = sec.querySelectorAll('input[type=number]');
      var numW = [];
      for (var n = 0; n < nums.length; n++) {
        var nr = nums[n].getBoundingClientRect();
        numW.push({ id: nums[n].id, w: Math.round(nr.width),
                    align: getComputedStyle(nums[n]).textAlign });
      }
      // subgroup 的上边框：紧跟 legend 的那些应当是 0（否则分组头顶会多一条虚线）
      var subs = sec.querySelectorAll('.subgroup');
      var subTop = [];
      for (var s = 0; s < subs.length; s++) {
        var prev = subs[s].previousElementSibling;
        subTop.push({
          text: subs[s].textContent.trim().slice(0, 14),
          afterLegend: !!(prev && prev.tagName === 'LEGEND'),
          borderTop: getComputedStyle(subs[s]).borderTopWidth,
          marginTop: getComputedStyle(subs[s]).marginTop
        });
      }
      return JSON.stringify({
        sectionH: Math.round(r.height),
        viewportH: window.innerHeight,
        scrollable: sec.scrollHeight - sec.clientHeight,
        fieldsets: rows,
        formgrids: gridCols,
        numbers: numW,
        subgroups: subTop,
        checkboxes: sec.querySelectorAll('input[type=checkbox]').length,
        readOnlyBlocks: sec.querySelectorAll('fieldset.readonly').length,
        legendBadges: Array.prototype.map.call(sec.querySelectorAll('fieldset > legend .badge'),
          function(b){ return b.textContent.trim(); })
      });
    })()`));

    if (D.err) { console.log('  ⚠ ' + D.err); exitCode = 2; }
    else {
      console.log('=== 设置页诊断 ===');
      console.log('  **面板高 ' + D.sectionH + 'px / 视口 ' + D.viewportH + 'px（比值 '
        + (D.sectionH / D.viewportH).toFixed(2) + '）** · 内部可滚 ' + D.scrollable + 'px');
      console.log('  fieldset ' + D.fieldsets.length + ' 个（其中只读 ' + D.readOnlyBlocks + ' 个）· 复选框 ' + D.checkboxes + ' 个');
      D.fieldsets.forEach((f, i) => {
        console.log('    ' + (i + 1) + '. ' + f.legend.padEnd(30)
          + ' 高 ' + String(f.h).padStart(4) + 'px'
          + (f.readonly ? ' [只读]' : '      ')
          + ' 徽章 ' + JSON.stringify(f.badges));
      });
      console.log('  formgrid 轨道: ' + JSON.stringify(D.formgrids));
      console.log('  数字输入框: ' + JSON.stringify(D.numbers));
      console.log('  subgroup 上边框（紧跟 legend 的应当是 0px）:');
      D.subgroups.forEach((s) => {
        const ok = s.afterLegend ? (s.borderTop === '0px' && s.marginTop === '0px') : true;
        console.log('    ' + (s.afterLegend ? '[紧接 legend]' : '[组内分隔]  ')
          + ' "' + s.text + '" border-top=' + s.borderTop + ' margin-top=' + s.marginTop
          + (ok ? '' : '  ⚠⚠ 不该有上边框'));
        if (!ok) exitCode = 2;
      });
      // 数字框：88px + 居中（照参考项目抄的那条）
      const badNum = D.numbers.filter((n) => n.w !== 88 || n.align !== 'center');
      if (badNum.length) {
        console.log('  ⚠⚠ 有数字框不是 88px/居中: ' + JSON.stringify(badNum));
        exitCode = 2;
      } else {
        console.log('  ✓ 三个数字框都是 88px + 居中');
      }
      console.log('  徽章汇总: ' + JSON.stringify(D.legendBadges));

      // 这个比值就是"要不要上分类导航"的判据，直接给结论而不是留给人看
      const ratio = D.sectionH / D.viewportH;
      if (ratio <= 1.0) {
        console.log('  ⇒ 比值 ' + ratio.toFixed(2) + ' ≤ 1 ⇒ **一屏看得完，不上分类导航**'
          + '（加了只是多两个要点的地方）');
      } else {
        console.log('  ⇒ 比值 ' + ratio.toFixed(2) + ' > 1 ⇒ 多屏 ⇒ 分类导航有价值，'
          + '可照参考项目的 settings-layout');
      }
    }

    await shot('settings-1-full.png');
    // 只读块给一张特写：fieldset.readonly 与普通 fieldset 的差异要肉眼可分
    await ev(`(function(){var f=document.querySelectorAll('fieldset.readonly')[0];
      if(f) f.scrollIntoView({block:'center'});})()`);
    await sleep(400);
    await shot('settings-2-readonly.png');

    console.log('  浏览器错误: ' + (errs.length ? errs.join(' | ') : '无'));
    if (errs.length) exitCode = 2;
    console.log('=== 结论：' + (exitCode === 0 ? '通过' : '有问题') + ' ===');
  }).catch((e) => { console.error('失败: ' + (e && e.message ? e.message : e)); exitCode = 1; });
  process.exit(exitCode);
})();
