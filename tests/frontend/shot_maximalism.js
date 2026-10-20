// shot_maximalism.js —— 视觉语言 v3（maximalism）的目视证据：一拍四屏两主题。
//
// # 为什么单独一个脚本
// 断言能证明"某格等于什么"，证明不了"整页看起来是不是同一套语言"。
// 改配色/字体/描边这类改动，唯一的验收方式是**看图**：
// 颗粒有没有压住文字、亮色块上的字是不是墨色、表格有没有被"吵"淹没。
//
// 用法：node tests/frontend/shot_maximalism.js
// 产物：.task/maximalism-v3/shots/*.png（不进版本库的产物目录）
'use strict';
const path = require('path');
const fs = require('fs');
const { startInstance } = require('./lib_instance.js');
const { withChrome, showPanel, sleep } = require('./lib_cdp.js');

const PORT = Number(process.env.SHOT_PORT || 7899);
const OUT = path.resolve(__dirname, '..', '..', '.task', 'maximalism-v3', 'shots');

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const inst = await startInstance({ port: PORT, apiKey: 'probe-key-shot-max' });
  console.log('实例：' + inst.base);
  try {
    await withChrome({ port: 9401, url: inst.base + '/ui', width: 1600, height: 1100, outDir: OUT,
      readyExpr: '!!(window.__wb2api__ && document.querySelector("#nav button"))' }, async ({ ev, shot }) => {
      for (const theme of ['light', 'dark']) {
        await ev('window.__wb2api__.setThemePref(' + JSON.stringify(theme) + ')');
        await sleep(600);
        await showPanel(ev, 'dashboard', 1200);
        await shot('dashboard-' + theme + '.png');
        await showPanel(ev, 'accounts', 1400);
        await shot('accounts-' + theme + '.png');
        await showPanel(ev, 'logs', 1200);
        await shot('logs-' + theme + '.png');
      }
      await ev('window.__wb2api__.setThemePref("system")');
    });
    // 登录页：token:false ⇒ 不带 ?token=，服务端给的就是登录页
    await withChrome({ port: 9402, url: inst.base + '/ui', token: false, width: 1200, height: 900, outDir: OUT,
      readyExpr: 'document.readyState === "complete"' }, async ({ ev, shot }) => {
      await sleep(1200);
      await shot('login.png');
      await ev('document.documentElement.setAttribute("data-theme","dark")');
      await sleep(400);
      await shot('login-dark.png');
    });
  } finally {
    inst.stop();
  }
})().catch((e) => { console.error('EXCEPTION: ' + (e && e.message)); process.exit(1); });
