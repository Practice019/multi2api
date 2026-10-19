// verify_accts_delegation.js —— R3 的委托是否会随刷新累积（本项目改过的那个 bug）。
//
// # 为什么必须单独测这一条
//
// `renderAccounts` 每次刷新都重建整个 #accts。如果折叠的事件是**逐行**绑的，
// 监听器会随刷新次数累积 —— 这正是本项目 F2 的形态。
// 实现是委托绑在 #accts 上（一次），但"我这么写了"不等于"它没泄漏"，
// 所以这里用 CDP 的 DOMDebugger 实际数一遍。
//
// ═══════════════════════════════════════════════════════════════════════════
// 2026-10-19 重写（判定 = (a) 探针前提过期，产品无缺陷）
//
// 症状：套件里 2 条 FAIL。实测（`node verify_accts_delegation.js`）复现为：
//
//	#accts 初始监听器数: -1        ← 拿不到 DOM
//	FAIL #accts 上有委托监听器
//	PASS #accts 上的监听器数是常数级（-1 个…）   ← **假 PASS**
//	PASS #accts 监听器数不随刷新增长（-1 → -1）  ← **假 PASS**（两个哨兵自比）
//	no-group → JSON.parse('no-group') 抛错      ← 第 2 条 FAIL
//
// 根因是**探针自己的三个前提过期**，不是产品：
//  ① 依赖外部实例（默认 18080）且 URL **不带身份**。18080 上当时是一个
//     **别人遗留的探针实例**（pid 5792，wb2api-server-probe.exe，12:36:16 起），
//     它的 admin_token 未知 ⇒ 浏览器拿到的是登录页 ⇒ `#accts` 根本不存在
//     ⇒ objectId undefined ⇒ DOMDebugger 失败 ⇒ count()=-1。
//  ② 哨兵值 -1 会**假绿**两条断言（`-1 <= 4`、`-1 === -1`）。本版把
//     "不增长"改成 `n1 === n0 && n0 > 0`，哨兵再也过不去（见 A4）。
//  ③ 折叠判据的锚点 `#accts tr.grouprow` 已随账号池改版失效：现在是
//     `section.acctgroup[data-acctgroup]` + `button.ghead-btn[data-atoggle]`，
//     折叠靠 `.acctgroup.collapsed > table{display:none}`（webui.html:252）。
//     `tr.grouprow` 只在注释里还留着（webui.html:3443），DOM 里已 0 个。
//
// 在**自起隔离实例**上实测（空池）：n0 = 2、8 轮重绘后 n1 = 2（不增长），
// 浏览器错误 0 —— 核心判据（委托不累积）**在产品上是通过的**。
//
// 本版做的事：
//  · 自己起隔离实例（lib_instance）+ 自己选空闲端口，**不再依赖 18080**；
//    仍可用 ACC_URL 指向外部实例（向后兼容），那时请自带 `?token=`。
//  · 合成 1 个 loomy 账号：空池下折叠判据没有行可隐藏，
//    会退化成恒真（"隐藏 0 行"也算通过）—— 这是**弱化**，所以必须给素材。
//  · 折叠判据换到当前锚点，并且比旧版**更强**：不只"点了有反应"，
//    还要求真的隐藏了表、不误伤别的组、再点能恢复、aria-expanded 跟着变。
//  · profile 目录**唯一化**并显式删除（旧版用固定名 D:\temp\chrome-acctlistener：
//    两次运行/并发运行会在启动时 EPERM 崩掉，实测踩到；被 kill 的路径还会漏）。
//  · 收尾日志：chrome 已停 / CDP 端口已释放 / profile 已删，逐条打出来。
//
// ⚠ 为什么不复用 lib_cdp.js（虽然它是共享层）：它**从不删除**自己的 profile
//   （`chrome-cdp-<port>-<ts>`，见它的 finally 只 kill 不 rm）。
//   实测 D:\temp 下已堆了 **124** 个 chrome-* 残留目录 —— 这是共享层的债，
//   不该在这条探针里再欠一笔。建议单独修 lib_cdp（影响 60+ 个探针），
//   本探针先自带一份"带清理"的最小 CDP 客户端。
// ═══════════════════════════════════════════════════════════════════════════
'use strict';
const http = require('http');
const fs = require('fs');
const net = require('net');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

// ⚠⚠ WB2API_EXE 必须在 require('./lib_instance.js') **之前**设。
//
// lib_instance 在**模块求值那一刻**就把 EXE 定死了，之后再改环境变量它只警告不生效
// —— 它会去起 bin/wb2api-server.exe，而那通常是**用户正在跑的旧构建**，
// 症状是"新代码被当成旧行为来查"，极具误导性。
process.env.WB2API_EXE = process.env.WB2API_EXE || 'bin/wb2api-server-probe.exe';
const instLib = require('./lib_instance.js');
const { startInstance, httpJson, sleep } = instLib;

const CHROME = require('./chrome_path.js').resolveChrome();

// 端口纪律：实例 7880–7899、CDP 92xx–93xx（7863/3080/3000 是别人的，绝不占用）。
// 逐个探测**空闲**端口，而不是写死一个 —— 实测 18080 上就蹲着别人遗留的实例，
// 写死端口会把"端口被占"伪装成"产品坏了"。
const INST_LO = 7882, INST_HI = 7899;
const CDP_LO = 9252, CDP_HI = 9299;

// 仍可指向外部实例（向后兼容）。那时探针**不会**替你带身份，
// 请在 URL 里自带 `?token=`，否则拿到的会是登录页（本文件开头记录的那个坑）。
const TARGET = process.env.ACC_URL || '';

// 折叠判据的素材：1 个合成 loomy 账号。与 verify_delete_buttons_ui_e2e.js 同一套
// 合成凭证形状（session 32 个 a + 合成 uid），已知能进池并被渲染成账号行。
const S_SESSION = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const U1 = '990000000000000011';

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
const info = (m) => console.log('  · ' + m);

function portFree(p) {
  return new Promise((r) => {
    const s = net.createServer();
    s.once('error', () => r(false));
    s.once('listening', () => s.close(() => r(true)));
    s.listen(p, '127.0.0.1');
  });
}
async function pickFreePort(lo, hi) {
  for (let p = lo; p <= hi; p++) if (await portFree(p)) return p;
  return 0;
}
// 等端口真的释放：kill 之后内核回收监听槽有一小段延迟，
// 立刻回读会拿到"仍被占用"的假红（收尾日志里的这一项要可信，否则它是装饰）。
// rmRetry：Windows 上 chrome 刚 kill 时仍握着 profile 里的文件（AV/索引也会），
// 一次 rm 失败不代表删不掉 —— 退避重试 3 次。
async function waitPortFree(p, timeoutMs) {
  const dl = Date.now() + (timeoutMs || 3000);
  for (;;) {
    if (await portFree(p)) return true;
    if (Date.now() > dl) return false;
    await sleep(250);
  }
}
function rmRetry(p, tries) {
  for (let i = 1; i <= (tries || 3); i++) {
    try { fs.rmSync(p, { recursive: true, force: true }); } catch { /* 下一轮再试 */ }
    if (!fs.existsSync(p)) return true;
    const end = Date.now() + 400;
    while (Date.now() < end) { /* 退避等待 */ }
  }
  return !fs.existsSync(p);
}
function get(u) {
  return new Promise((res, rej) => {
    http.get(u, (r) => { let d = ''; r.on('data', (c) => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
// 轮询到条件成立；超时返回最后一次读数（不用固定 sleep，冷启动耗时不可预测）。
async function until(ev, expr, want, timeoutMs) {
  const dl = Date.now() + (timeoutMs || 25000);
  let last;
  while (Date.now() < dl) {
    last = await ev(expr);
    if (typeof want === 'function' ? want(last) : last === want) return last;
    await sleep(400);
  }
  return last;
}

(async () => {
  let inst = null, ch = null, profile = null, cdpPort = 0, exitCode = 0;

  // 收尾：跑完 / 抛错 / 被 Ctrl-C 都要走到这里。
  // ⚠ 只有"能进 finally"的路径才收得干净；SIGKILL 拦不住任何东西 ——
  //   所以 profile 名里带 pid 与时间戳（唯一化），漏一个也不会撞下一次运行
  //   （旧版的固定名 + 启动时 rmSync 正是"漏一次 ⇒ 下一次直接 EPERM 崩"）。
  const cleanup = async () => {
    console.log('\n=== 收尾 ===');
    if (ch) { try { ch.kill(); } catch { /* 已退出 */ } }
    await sleep(400);
    console.log('  chrome: 已停（pid ' + (ch && ch.pid ? ch.pid : '—') + '）');
    if (cdpPort) {
      const free = await waitPortFree(cdpPort, 3000);
      console.log('  CDP 端口 ' + cdpPort + ': ' + (free ? '已释放' : '⚠ 仍被占用'));
    }
    if (profile) {
      const gone = rmRetry(profile, 3);
      console.log('  profile ' + profile + ': ' + (gone ? '已删' : '⚠ 删除失败')
        + '（' + os.tmpdir() + ' 下现存 chrome-* 目录 ' + leftOver() + ' 个 —— 其中历史残留来自 lib_cdp，见文件开头注释）');
    }
    if (inst) {
      inst.stop();
      await sleep(400);
      const free = await waitPortFree(inst.port || 0, 3000);
      console.log('  隔离实例: 已停（pid ' + inst.pid + '）· 监听端口 ' + (inst.port || '—') + ': ' + (free ? '已释放' : '⚠ 仍被占用'));
      // 实例的临时目录（auths/data/config.json）同样不该留 —— 它和 profile 是同一类泄漏。
      const tmpGone = rmRetry(inst.tmp, 3);
      console.log('  实例临时目录 ' + inst.tmp + ': ' + (tmpGone ? '已删' : '⚠ 删除失败'));
    } else {
      console.log('  隔离实例: 未起（用 ACC_URL 指向外部实例）');
    }
  };
  function leftOver() {
    try { return fs.readdirSync(os.tmpdir()).filter((n) => n.startsWith('chrome-')).length; } catch { return '?'; }
  }
  process.on('SIGINT', async () => { await cleanup(); process.exit(130); });
  process.on('SIGTERM', async () => { await cleanup(); process.exit(143); });

  try {
    // ---------- ① 目标：外部实例 或 自起隔离实例 ----------
    let target = TARGET, base = '', key = '';
    if (!target) {
      const port = await pickFreePort(INST_LO, INST_HI);
      if (!port) {
        console.log('  ⚠ ' + INST_LO + '–' + INST_HI + ' 全被占用，起不了隔离实例');
        info('诊断：先腾一个端口，或用 ACC_URL=<带 ?token= 的控制台 URL> 指外部实例');
        process.exit(2);
      }
      if (!fs.existsSync(instLib.EXE)) {
        console.log('  ⚠ 探针二进制不存在: ' + instLib.EXE);
        info('诊断：先构建 `go build -o bin/wb2api-server-probe.exe ./cmd/server`，');
        info('      或设 WB2API_EXE=<相对仓库根的路径>（⚠ 必须在 require(lib_instance) 之前设，' +
          '本文件已把它放在 require 前，所以你只需导出环境变量）');
        process.exit(2);
      }
      try {
        inst = await startInstance({
          port: port, apiKey: 'accts-deleg-' + port,
          auths: { 'loomy/loomy-deleg1.json': JSON.stringify({ session: S_SESSION, userid: U1 }) },
          config: { loomy: { enabled: true, auth_dir: './auths/loomy' } },
        });
      } catch (e) {
        // ⚠ 实例起不来必须**指名道姓**地失败，不能伪装成监听器断言的红/绿。
        console.log('  ⚠ 隔离实例未起来: ' + (e && e.message ? e.message : e));
        info('诊断三连：① WB2API_EXE=' + (process.env.WB2API_EXE || '(未设)') + ' 实际用 ' + instLib.EXE);
        info('          ② html 走 //go:embed —— 改了 internal/server/*.html 必须**重建**二进制，');
        info('             否则浏览器拿到的是内嵌的旧页面（最容易被误判成前端 bug）');
        info('          ③ 端口段 ' + INST_LO + '–' + INST_HI + '，被占时先腾');
        process.exit(2);
      }
      inst.port = port;
      base = inst.base; key = inst.key;
      target = base + '/ui?token=' + encodeURIComponent(key);
      info('自起隔离实例 ' + base + '（pid=' + inst.pid + '）· 合成 1 个 loomy 账号');
    } else {
      info('用 ACC_URL 指向外部实例: ' + target);
    }

    // ---------- ② 起浏览器（唯一 profile；跑完必删） ----------
    cdpPort = await pickFreePort(CDP_LO, CDP_HI);
    if (!cdpPort) {
      // 这条 abort 发生在**实例已经起来之后** ⇒ 必须自己收掉，否则 process.exit
      // 会绕过 finally 把实例进程与它的临时目录留在机器上（同类泄漏，实测遇到过）。
      console.log('  ⚠ ' + CDP_LO + '–' + CDP_HI + ' 全被占用，起不了 CDP');
      if (inst) { inst.stop(); await sleep(300); rmRetry(inst.tmp, 3); }
      process.exit(2);
    }
    profile = path.join(os.tmpdir(), 'chrome-accts-deleg-' + cdpPort + '-' + process.pid + '-' + Date.now());
    ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
      '--remote-debugging-port=' + cdpPort, '--user-data-dir=' + profile,
      '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
    info('CDP 端口 ' + cdpPort + ' · profile ' + profile);

    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + cdpPort + '/json/list'));
        t = l.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { /* 还没起来 */ }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 没起来（调试口 ' + cdpPort + ' 连不上）');

    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0; const pend = new Map();
    const pageErrs = [];
    ws.onmessage = (e) => {
      const m = JSON.parse(e.data);
      if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); return; }
      if (m.method === 'Runtime.exceptionThrown') {
        const d = m.params.exceptionDetails || {};
        pageErrs.push('[' + (d.exception && d.exception.description ? d.exception.description : d.text) + ']');
      }
    };
    const send = (mm, p) => new Promise((r) => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) return 'EXC';
      return r.result && r.result.result ? r.result.result.value : undefined;
    };
    await send('Page.enable'); await send('Runtime.enable'); await send('DOM.enable');
    await send('Page.navigate', { url: target });

    // 等控制台骨架（而不是固定 sleep 7s —— 冷启动耗时不可预测）
    const ready = await until(ev, `(document.querySelectorAll('#accts').length === 1 && typeof window.__wb2api__ === 'object') ? 1 : 0`, 1, 30000);
    if (ready !== 1) {
      ok(false, '控制台未就绪（30s 内没拿到 #accts + window.__wb2api__）—— ' +
        '这是**实例/身份/二进制**层面的前提问题，不是监听器纪律问题；' +
        '诊断：URL 必须带 ?token=（自起实例本文件已带）、二进制必须是当前构建（html 走 go:embed）');
      await ev(`window.__wb2api__ && window.__wb2api__.showPanelByKey('accounts')`);
      if (ws) ws.close();
      throw new Error('控制台未就绪');
    }
    await ev(`window.__wb2api__.showPanelByKey('accounts')`);
    await sleep(1200);

    // ---------- ③ 前置自检：把"没实例/没身份"从"监听器泄漏"里分出来 ----------
    const pre = JSON.parse(await ev(`JSON.stringify({
      accts: document.querySelectorAll('#accts').length,
      groups: document.querySelectorAll('#accts .acctgroup').length,
      acctrows: document.querySelectorAll('#accts tr.acctrow').length,
      legacyGroupRows: document.querySelectorAll('#accts tr.grouprow').length
    })`));
    info('前置: ' + JSON.stringify(pre));
    ok(pre.accts === 1, '#accts 存在（' + pre.accts + '）—— 拿不到时下面所有计数都是哨兵值，必须先卡这一条');
    ok(pre.groups > 0, '账号池渲染出 ' + pre.groups + ' 个上游分组');
    ok(pre.acctrows > 0, '池里有 ' + pre.acctrows + ' 个账号行（折叠判据的素材）');
    // 旧锚点已死的**实测**记录：这条不是"断言必须为 0"，而是把"锚点换过"钉成可见事实，
    // 避免下次有人拿 tr.grouprow 去写断言（产品里只剩注释，webui.html:3443）。
    info('遗留锚点 tr.grouprow 命中 ' + pre.legacyGroupRows + ' 个（产品已改版为 section.acctgroup，0 是正常的）');

    // ---------- ④ 核心判据：委托监听器不随重绘累积 ----------
    const root = await send('Runtime.evaluate', { expression: `document.getElementById('accts')` });
    const objId = root.result && root.result.result ? root.result.result.objectId : undefined;
    ok(!!objId, '#accts 的 objectId 拿到了（DOMDebugger 只能挂在它上面）');
    const count = async () => {
      if (!objId) return -1;
      const r = await send('DOMDebugger.getEventListeners', { objectId: objId });
      return (r.result && r.result.listeners ? r.result.listeners.length : -1);
    };

    const n0 = await count();
    console.log('  #accts 初始监听器数: ' + n0);
    ok(n0 > 0, '#accts 上有委托监听器（折叠与账号动作靠它）');
    ok(n0 <= 4, '#accts 上的监听器数是常数级（' + n0 + ' 个：账号动作 + 分组动作 + 折叠 + 可能的其它），不是每行一个');

    // 取一份**真实**池，让每轮重绘都走"有账号行"的路径。
    //
    // 为什么不用空池：那条路虽然合法（renderAccounts 就是为灌空池暴露的），
    // 但"逐行绑"这类缺陷只在**有行**时才绑得出来 —— 空池重绘 8 次，
    // 一个逐行绑的泄漏实现同样会全绿。所以素材必须是有账号的池。
    let pool = [];
    // 取池的顺序：Node 侧直连（有令牌时最可靠）→ 页内 fetch 兜底。
    //
    // ⚠ 页内 fetch 依赖 `window.__PROBE_API_KEY__`，而那个变量是 lib_cdp 注入的；
    //   本探针自带的 CDP 客户端不注入它 ⇒ 外部实例模式下**必须**从环境变量拿令牌，
    //   否则池是空的、重绘 8 次全是空池（"逐行绑"这类缺陷就绑不出来了）。
    const tok = key || process.env.WB2API_PROBE_TOKEN || process.env.WB2API_PROBE_KEY || '';
    const poolBase = inst ? base : (() => { try { return new URL(target).origin; } catch { return ''; } })();
    if (tok && poolBase) {
      const r = await httpJson(poolBase, '/admin/accounts', { token: tok });
      pool = (r.json && r.json.accounts) || [];
      if (!pool.length) info('Node 侧取池返回 ' + r.status + '（' + String(r.text).slice(0, 120) + '）');
    } else {
      const raw = await ev(`(async function(){
        try {
          var r = await fetch('/admin/accounts', { headers: { Authorization: 'Bearer ' + (window.__PROBE_API_KEY__ || '') } });
          var j = await r.json(); return JSON.stringify(j.accounts || []);
        } catch (e) { return '[]'; }
      })()`);
      try { pool = JSON.parse(raw) || []; } catch { pool = []; }
    }
    ok(pool.length > 0, '取到真实账号池用于重绘（' + pool.length + ' 个；外部实例模式需 WB2API_PROBE_TOKEN 或页内 __PROBE_API_KEY__，否则这里会红）');
    const poolJSON = JSON.stringify(pool);
    const render = () => ev(`window.__wb2api__.renderAccounts(${poolJSON})`);

    // 反复触发重绘（renderAccounts 每次都重建整个 #accts）
    for (let i = 0; i < 8; i++) {
      await render();
      await ev(`(function(){ var b=document.getElementById('refresh'); if(b) b.click(); return 1 })()`);
      await sleep(700);
    }
    await sleep(1500);
    const n1 = await count();
    console.log('  8 轮刷新重绘后监听器数: ' + n1);
    // ⚠ 判据里带 n0 > 0：只看 `n1 === n0` 时，两次 DOMDebugger 失败（-1 === -1）
    //   也会通过 —— 那正是旧版 2 条红里混着的那条假绿。哨兵不许过这条线。
    ok(n1 === n0 && n0 > 0, '#accts 监听器数**不随刷新增长**（' + n0 + ' → ' + n1 + '）—— 逐行绑会在这里红');
    const alive = JSON.parse(await ev(`JSON.stringify({ accts: document.querySelectorAll('#accts').length,
      rows: document.querySelectorAll('#accts tr.acctrow').length })`));
    ok(alive.accts === 1, '8 轮重绘后 #accts 仍在（' + alive.accts + '）—— DOM 被换过而不是页面被换走');
    ok(alive.rows > 0, '8 轮重绘后账号行仍在（' + alive.rows + ' 行）—— 证明重绘真的重建了行');

    // ---------- ⑤ 折叠仍工作，且委托没被重绘破坏 ----------
    // 锚点已换成当前结构：section.acctgroup[data-acctgroup] + button.ghead-btn[data-atoggle]。
    // 判据比旧版强：不只是"点了有反应"，还要真的隐藏了那张表、不误伤别的组、再点能恢复。
    const w = JSON.parse(await ev(`(function(){
      var groups = Array.prototype.slice.call(document.querySelectorAll('#accts .acctgroup'));
      var target = null;
      for (var i = 0; i < groups.length; i++) {
        if (groups[i].querySelector('table') && groups[i].querySelectorAll('tbody tr').length > 0) { target = groups[i]; break; }
      }
      if (!target) return JSON.stringify({ err: 'no-group-with-rows' });
      var btn = target.querySelector('button[data-atoggle]');
      if (!btn) return JSON.stringify({ err: 'no-atoggle-btn' });
      var tb = target.querySelector('table');
      var others = groups.filter(function(g){ return g !== target && g.querySelector('table'); });
      var before = { collapsed: target.classList.contains('collapsed'),
        display: getComputedStyle(tb).display, rows: tb.querySelectorAll('tbody tr').length, aria: btn.getAttribute('aria-expanded') };
      btn.click();
      var after = { collapsed: target.classList.contains('collapsed'),
        display: getComputedStyle(tb).display, aria: btn.getAttribute('aria-expanded'),
        othersTotal: others.length,
        othersVisible: others.filter(function(g){ return getComputedStyle(g.querySelector('table')).display !== 'none'; }).length };
      btn.click();
      var restored = { collapsed: target.classList.contains('collapsed'), display: getComputedStyle(tb).display };
      return JSON.stringify({ provider: target.getAttribute('data-acctgroup'), before: before, after: after, restored: restored });
    })()`));
    console.log('  第 8 轮重绘后点折叠: ' + JSON.stringify(w));
    ok(!w.err, '折叠判据找到可折叠的组（' + (w.err || 'provider=' + w.provider) + '）');
    if (!w.err) {
      ok(w.before.collapsed === false && w.after.collapsed === true,
        '点标题真的折叠了（collapsed ' + w.before.collapsed + ' → ' + w.after.collapsed + '）—— 委托活在 #accts 上，不在被替换的行上');
      ok(w.after.display === 'none' && w.before.rows > 0,
        '折叠真的隐藏了内容（该组表 display ' + w.before.display + ' → ' + w.after.display + '，隐藏前有 ' + w.before.rows + ' 行）');
      ok(w.after.aria === 'false' && w.before.aria === 'true',
        'aria-expanded 跟着状态走（' + w.before.aria + ' → ' + w.after.aria + '）');
      ok(w.after.othersVisible === w.after.othersTotal,
        '折叠只影响自己那组（其它 ' + w.after.othersTotal + ' 组全部仍可见）');
      ok(w.restored.collapsed === false && w.restored.display !== 'none',
        '再点一次能展开回来（collapsed=' + w.restored.collapsed + ' display=' + w.restored.display + '）');
    }
    ok(pageErrs.length === 0, '页面无 JS 异常（' + (pageErrs.length ? pageErrs.join(' | ') : '0 条') + '）');

    try { ws.close(); } catch { /* 已关 */ }
  } catch (e) {
    console.log('EXCEPTION: ' + (e && e.message ? e.message : e));
    exitCode = 2;
  } finally {
    await cleanup();
  }
  console.log(fail === 0 && exitCode === 0
    ? '\n=== 账号池委托监听器纪律通过 ==='
    : '\n=== ' + fail + ' 项失败' + (exitCode ? ' + 异常退出' : '') + ' ===');
  process.exit(fail ? 1 : exitCode);
})();
