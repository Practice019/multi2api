// theme_e2e.js —— T10 验证：深色/浅色三态主题。
//
// # 验证什么
//
//  1. 三态切换生效并持久化（刷新后保持）
//  2. 默认跟随系统
//  3. 浅色主题下正文对比度 >= 4.5:1（**实算**，不是看代码说"挑过色"）
//  4. 首屏防闪白：data-theme 在 <style> 之前就被设好
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9232;
const URL_UNDER_TEST = process.env.T10_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-t10profile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const chrome = spawn(CHROME, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`,
    '--window-size=1440,1000', 'about:blank',
  ], { stdio: 'ignore' });

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let target = null;
    for (let i = 0; i < 40; i++) {
      try {
        const list = JSON.parse(await get(`http://127.0.0.1:${PORT}/json/list`));
        target = list.find(t => t.type === 'page' && t.webSocketDebuggerUrl);
        if (target) break;
      } catch { /* 等 */ }
      await sleep(250);
    }
    if (!target) throw new Error('Chrome 未启动');

    const ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let id = 0; const pending = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
    const send = (m, p) => new Promise(r => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method: m, params: p || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: URL_UNDER_TEST });
    await sleep(3000);

    const evalJs = async (expr) => {
      const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 300));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    console.log('\n[T10-1] 初始状态与三态按钮');
    const init = JSON.parse(await evalJs(`(() => {
      const btns = Array.from(document.querySelectorAll('#themeSwitch button[data-theme-mode]'));
      return JSON.stringify({
        theme: document.documentElement.getAttribute('data-theme'),
        buttons: btns.map(b => b.dataset.themeMode),
        active: btns.filter(b => b.classList.contains('on')).map(b => b.dataset.themeMode),
      });
    })()`));
    console.log('    当前主题=' + init.theme + '  按钮=' + JSON.stringify(init.buttons) + '  选中=' + JSON.stringify(init.active));
    ok(['dark', 'light'].indexOf(init.theme) >= 0, 'data-theme 是 dark/light 之一（实际 ' + init.theme + '）');
    ok(JSON.stringify(init.buttons) === JSON.stringify(['system', 'light', 'dark']), '三个按钮齐全');
    ok(init.active.length === 1, '恰好一个按钮处于选中态');

    console.log('\n[T10-2] 切换生效 + 持久化');
    const sw = JSON.parse(await evalJs(`(() => {
      const W = window.__wb2api__;
      // 先把存储清成哨兵值，再切换 —— 这样"读到的值"就能证明
      // **确实发生了一次写入**，而不是碰巧读到上一轮留下的旧值。
      //
      // # 为什么必须清（自审发现的测试隔离缺陷）
      //
      // 原断言是 localStorage.getItem('wb2api.theme') === 'light'。
      // 当"持久化"这个功能被删掉时，存储里仍留着**上一轮运行写的 'light'**，
      // 于是断言照样通过 —— 测出来的绿取决于上一个变体留下什么，
      // 表现为同一个注入在不同轮次上时红时绿。
      localStorage.setItem('wb2api.theme', '__SENTINEL__');
      W.setThemePref('light');
      const savedAfterLight = localStorage.getItem('wb2api.theme');
      const light = document.documentElement.getAttribute('data-theme');
      const bgLight = getComputedStyle(document.body).backgroundColor;
      W.setThemePref('dark');
      const savedAfterDark = localStorage.getItem('wb2api.theme');
      const dark = document.documentElement.getAttribute('data-theme');
      const bgDark = getComputedStyle(document.body).backgroundColor;
      W.setThemePref('light');
      return JSON.stringify({ light, dark, bgLight, bgDark, savedAfterLight, savedAfterDark });
    })()`));
    console.log('    light=' + sw.light + ' bg=' + sw.bgLight + ' | dark=' + sw.dark + ' bg=' + sw.bgDark);
    console.log('    存储: light 后=' + sw.savedAfterLight + '  dark 后=' + sw.savedAfterDark);
    ok(sw.light === 'light', '切浅色 → data-theme=light');
    ok(sw.dark === 'dark', '切深色 → data-theme=dark');
    ok(sw.bgLight !== sw.bgDark, '两套主题的 body 背景**实际不同**（不只是属性变了）');
    // 关键：必须从哨兵被改写，才证明"写"发生了
    ok(sw.savedAfterLight !== '__SENTINEL__' && sw.savedAfterLight === 'light',
      '切换真的**写入**了 localStorage（从哨兵改为 light，实际=' + sw.savedAfterLight + '）');
    ok(sw.savedAfterDark === 'dark', '第二次切换也写入（实际=' + sw.savedAfterDark + '）');

    console.log('\n[T10-3] 刷新后保持');
    await send('Page.navigate', { url: URL_UNDER_TEST });
    await sleep(2500);
    const after = await evalJs(`document.documentElement.getAttribute('data-theme')`);
    console.log('    刷新后 data-theme=' + after);
    ok(after === 'light', '刷新后仍是浅色（持久化生效）');

    console.log('\n[T10-4] 浅色主题下对比度实算（WCAG AA >= 4.5:1）');
    await evalJs(`window.__wb2api__.setThemePref('light')`);
    const contrast = JSON.parse(await evalJs(`(() => {
      const lum = (c) => {
        const m = c.match(/\\d+/g).map(Number).slice(0,3).map(v => {
          v = v/255;
          return v <= 0.03928 ? v/12.92 : Math.pow((v+0.055)/1.055, 2.4);
        });
        return 0.2126*m[0] + 0.7152*m[1] + 0.0722*m[2];
      };
      const ratio = (a,b) => { const l1=lum(a), l2=lum(b); const hi=Math.max(l1,l2), lo=Math.min(l1,l2); return (hi+0.05)/(lo+0.05); };
      const bodyBg = getComputedStyle(document.body).backgroundColor;
      const bodyFg = getComputedStyle(document.body).color;
      const dimEl = document.querySelector('.dim');
      const dimFg = dimEl ? getComputedStyle(dimEl).color : bodyFg;
      return JSON.stringify({
        body: Number(ratio(bodyFg, bodyBg).toFixed(2)),
        dim: Number(ratio(dimFg, bodyBg).toFixed(2)),
        bg: bodyBg, fg: bodyFg, dimFg,
      });
    })()`));
    console.log('    正文对比度=' + contrast.body + ':1  .dim=' + contrast.dim + ':1  (' + contrast.fg + ' on ' + contrast.bg + ')');
    ok(contrast.body >= 4.5, '正文对比度 >= 4.5:1（实际 ' + contrast.body + '）');
    ok(contrast.dim >= 4.5, '次要文字(.dim)对比度 >= 4.5:1（实际 ' + contrast.dim + '）');

    console.log('\n[T10-5] 深色主题对比度同样达标');
    await evalJs(`window.__wb2api__.setThemePref('dark')`);
    const c2 = JSON.parse(await evalJs(`(() => {
      const lum = (c) => {
        const m = c.match(/\\d+/g).map(Number).slice(0,3).map(v => {
          v = v/255;
          return v <= 0.03928 ? v/12.92 : Math.pow((v+0.055)/1.055, 2.4);
        });
        return 0.2126*m[0] + 0.7152*m[1] + 0.0722*m[2];
      };
      const ratio = (a,b) => { const l1=lum(a), l2=lum(b); const hi=Math.max(l1,l2), lo=Math.min(l1,l2); return (hi+0.05)/(lo+0.05); };
      const bg = getComputedStyle(document.body).backgroundColor;
      const fg = getComputedStyle(document.body).color;
      return JSON.stringify({ r: Number(ratio(fg,bg).toFixed(2)) });
    })()`));
    console.log('    深色正文对比度=' + c2.r + ':1');
    ok(c2.r >= 4.5, '深色正文对比度 >= 4.5:1（实际 ' + c2.r + '）');

    console.log('\n[T10-6] 防首屏闪白：data-theme 在 <style> 之前设好');
    //
    // # 为什么读**源码**而不是 document.documentElement.outerHTML
    //
    // 浏览器会把解析后的 DOM 重新序列化，<head> 内的顺序不一定保留原文顺序，
    // 所以用 outerHTML 里的位置比较不可靠（第一版就是这么假失败的）。
    // 防闪白关心的是**原始响应里**脚本是否排在 <style> 与 <body> 之前 ——
    // 那才是浏览器实际解析的顺序。
    const raw = await evalJs(`(async () => {
      const r = await fetch('/ui');
      return await r.text();
    })()`);
    const iSet = raw.indexOf("setAttribute('data-theme'");
    // 用正则找**真正的** <style> 开标签（行首或紧跟在 </script> 之后），
    // 而不是 indexOf —— 注释里提到 "<style>" 这个词会先被匹配到，
    // 那正是第一版假失败的原因。
    const mStyle = /(^|\n)\s*<style>/.exec(raw);
    const mBody = /(^|\n)\s*<body/.exec(raw);
    const iStyle = mStyle ? mStyle.index : -1;
    const iBody = mBody ? mBody.index : -1;
    console.log('    源码位置: setAttribute=' + iSet + '  <style>=' + iStyle + '  <body>=' + iBody);
    ok(iSet >= 0, '找到设 data-theme 的脚本');
    ok(iStyle > 0 && iSet < iStyle, '该脚本在 <style> 之前');
    ok(iBody > 0 && iSet < iBody, '该脚本在 <body> 之前（首帧渲染前就已设好主题）');

    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== T10 端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
