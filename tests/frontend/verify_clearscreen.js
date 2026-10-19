// verify_clearscreen.js —— 验证清屏修复的**完整行为**，而不只是"行数变少了"。
//
// 需要证明 4 件事：
//   1. 点「清屏」后内容真的清空（不是闪一下又回来）
//   2. 清屏后**10s 轮询不会把它填回来**（这是修复的关键 —— 只改按钮不够）
//   3. 按钮变成「恢复显示」，点了能恢复
//   4. 清屏期间切走再切回来，仍然是清屏状态（挂起是全局的，不只挡定时器）
//
// 只测 1 的话，把 setInterval 停掉就能骗过；2、4 才是真正的行为契约。
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9293;
const PROFILE = path.join(os.tmpdir(), 'chrome-verifyclear');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1000', 'about:blank'], { stdio: 'ignore' });
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
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
    await send('Page.navigate', { url: process.env.CLEAR_URL || 'http://127.0.0.1:18080/ui' });
    await sleep(4000);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };
    // ⚠ 2026-10-19：请求日志从 `<table><tbody><tr>` 换成了 **grid 行**
    //   （webui.html:9337-9377 的 renderLogs：`<div class="log-row">`，
    //    理由见那段注释 —— 表头粘不住 / 数值列不右对齐 / 空值写 `-` / 空列表直接 return）。
    //
    // 所以 `#logrows tr` **恒定 0**，而这条探针的每一条断言都以它为基数 ——
    // 原来那句"前置：清屏前有多行数据（0）"是**判据本身过期**，不是数据缺失
    //（实测：本实例落盘日志有 80 条，`/admin/logs/history` 回 total=80）。
    // 判据的意图一个字没改：数**真实日志条目**的行数。
    const rows = () => ev(`document.querySelectorAll('#logrows .log-row').length`);
    // 清屏后不是"恰好 1 行"，而是 **0 条真实条目 + 一句说清状态的占位**。
    // 判据同样没放宽：仍然要求"内容真的清空"，只是把它表达成
    // "没有任何真实条目"+"必须有明确状态文案"两条，覆盖原来那 1 行的含义。
    const clearedState = () => ev(`JSON.stringify((function(){
      var host = document.getElementById('logrows');
      return {
        rows: host.querySelectorAll('.log-row').length,
        text: (host.textContent || '').replace(/\\s+/g, ' ').trim().slice(0, 60),
        isCleared: /已清屏/.test(host.textContent || ''),
      };
    })())`);
    const btnText = () => ev(`(function(){var b=document.getElementById('btnLogClear');return b?(b.textContent||'').trim():null;})()`);
    const nav = async (label) => ev(`(function(){
      var b = Array.from(document.querySelectorAll('#nav button[data-nav]'))
        .filter(function(x){ return x.textContent.indexOf(${JSON.stringify(label)}) >= 0; })[0];
      if (b) { b.click(); return true; } return false;
    })()`);

    await nav('请求日志');
    await sleep(2500);
    const before = await rows();
    console.log('  点击前: ' + before + ' 行，按钮="' + await btnText() + '"');
    ok(before > 1, '前置：清屏前有多行数据（' + before + '）—— ' +
      '本实例由 D:\\temp\\wb2api-t12\\seed_traffic.js 灌过真实请求日志；' +
      '若这里是 0，先跑那个脚本再测');

    // 1) 清屏
    console.log('\n[1] 点清屏');
    await ev(`document.getElementById('btnLogClear').click()`);
    await sleep(1200);
    const cs = JSON.parse(await clearedState());
    console.log('  清屏后: ' + JSON.stringify(cs));
    ok(cs.rows === 0, '内容真的清空（' + before + ' → ' + cs.rows + ' 条真实条目）');
    ok(cs.isCleared, '清屏后给出了明确状态（装配占位文案含「已清屏」）：' + JSON.stringify(cs.text));
    ok(await btnText() === '恢复显示', '按钮变为「恢复显示」（给出回头路）');

    // 2) 关键：等过一个轮询周期（10s），内容不得被填回
    console.log('\n[2] 等 12s 越过一个轮询周期（10s）');
    await sleep(12000);
    const afterPoll = await rows();
    ok(afterPoll === 0, '轮询没有把它填回来（' + afterPoll + ' 条）—— 挂起对定时器生效');

    // 3) 切走再切回来，仍是清屏状态
    //
    // ⚠ 「任务历史」标签页已被用户要求删除（见 webui.html 那段说明）——
    //   原来这里切到它。改成切「任务调度」：判据的意图是"切到**别的**面板再回来"，
    //   与切的是哪一个无关（任何一个真实存在的面板都能达成这个意图）。
    console.log('\n[3] 切到别的面板再切回');
    ok(await nav('任务调度'), '切到了另一个面板（任务调度）');
    await sleep(1500);
    await nav('请求日志');
    await sleep(2000);
    const afterSwitch = JSON.parse(await clearedState());
    ok(afterSwitch.rows === 0 && afterSwitch.isCleared,
      '切回后仍是清屏状态（' + afterSwitch.rows + ' 条）—— 挂起不只挡定时器');

    // 4) 恢复显示
    console.log('\n[4] 点「恢复显示」');
    await ev(`document.getElementById('btnLogClear').click()`);
    await sleep(2500);
    const afterResume = await rows();
    ok(afterResume > 1, '恢复后数据回来（' + afterResume + ' 条）');
    ok(await btnText() === '清屏', '按钮变回「清屏」');

    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 清屏行为契约全部满足 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
