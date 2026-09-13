// verify_account_buttons.js —— 账号池顶部按钮的**清单守卫**。
//
// # 为什么需要它
//
// 用户要求删掉「全部保活」。删的时候我查了一遍：**没有任何测试断言
// 账号池顶部有哪几个按钮** —— 意味着"多一个按钮"或"少一个按钮"
// 都不会被发现。
//
// 本项目的既有教训（评审 CP2 F4）：把组头账号数徽章整块删掉，
// 18 套件 + 3 个 E2E **全部照样全绿** —— 因为断言只检查"有某某组"，
// 从不检查它到底有什么。
//
// 所以这里断言**完整清单**（不是"包含某些"）：
//   · 应当存在的必须都在
//   · 应当**不存在**的必须真的不在（防回归：删掉的又回来了）
//
// # 判据来自哪里
//
// 硬编码在这份测试里（而不是从源码抓）—— 因为"按钮清单"本身就是要被
// 钉住的产品决策。清单变了就应该有人来改这条断言，而不是自动跟随。
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9304;
const TARGET = process.env.ACC_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = path.join(os.tmpdir(), 'chrome-accbtn');
const sleep = ms => new Promise(r => setTimeout(r, ms));
function get(u) { return new Promise((res, rej) => { http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej); }); }

// 应当存在的（顺序也断言 —— 按钮顺序是设计决策）
//
// T10 之后：h2 上只剩"对整池生效"的两个。
// 「＋ 添加账号」与「重载 auths」已移入**各上游分组行**（见下方分组断言）。
const EXPECTED = ['全部签到', '刷新全部积分'];
// 应当**不存在**的（已删除 / 已移走，防回归）
const FORBIDDEN = ['全部保活', '＋ 添加账号', '重载 auths'];

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE, '--window-size=1440,1100', 'about:blank'], { stdio: 'ignore' });
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
    await send('Page.navigate', { url: TARGET });
    await sleep(5000);
    const ev = async (x) => { const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true }); return r.result && r.result.result ? r.result.result.value : undefined; };

    const btns = JSON.parse(await ev(`JSON.stringify((function(){
      var sec = document.querySelector('#content > section[data-key="accounts"]');
      if (!sec) return null;
      var h2 = sec.querySelector('h2');
      if (!h2) return null;
      return Array.from(h2.querySelectorAll('button')).map(function(b){
        return (b.textContent || '').replace(/\\s+/g, ' ').trim();
      });
    })())`));

    console.log('  账号池顶部按钮: ' + JSON.stringify(btns));
    ok(Array.isArray(btns), '找到账号池面板与它的 h2 按钮区');

    if (Array.isArray(btns)) {
      // 全等断言（顺序也管）—— 不是"包含"
      ok(JSON.stringify(btns) === JSON.stringify(EXPECTED),
        '按钮清单与顺序完全等于期望\n      期望: ' + JSON.stringify(EXPECTED) +
        '\n      实际: ' + JSON.stringify(btns));

      for (const f of FORBIDDEN) {
        ok(!btns.some(b => b.indexOf(f) >= 0),
          '已删除的「' + f + '」不在界面上（防回归）');
      }
    }

    // 行内「保活」必须仍在（那是排障动作，与删掉的"全部保活"不同）
    const inlineKeepalive = await ev(`(function(){
      return document.querySelectorAll('#accts button[data-act="keepalive"]').length;
    })()`);
    console.log('  账号行内「保活」按钮数: ' + inlineKeepalive);
    ok(inlineKeepalive > 0, '账号**行内**的「保活」仍在（' + inlineKeepalive + ' 个）—— 删的是顶部那个，不是这个');

    // ---------------------------------------------------------------- T10：分组行动作
    //
    // 每个上游分组行都应带自己的动作按钮。判据：
    //   · 有 data-greload 的按钮数 == 分组数（每个上游都能重载它的目录）
    //   · 有 data-gadd 的上游 == manifest 里 login 非空的那些
    //     （没有页内登录流程的上游**不该**有添加按钮 —— 那是个假按钮）
    const grp = JSON.parse(await ev(`JSON.stringify((function(){
      var out = { groups: 0, reload: 0, add: [], addProviders: [], groupsWithAdd: [] };
      document.querySelectorAll('#accts tr.grouprow').forEach(function(tr){
        out.groups++;
        if (tr.querySelector('button[data-greload]')) out.reload++;
        var a = tr.querySelector('button[data-gadd]');
        if (a) { out.groupsWithAdd.push(a.dataset.gadd); out.addProviders.push(a.dataset.gadd); }
      });
      out.add = Array.from(document.querySelectorAll('#accts button[data-gadd]')).map(function(b){ return b.dataset.gadd; });
      return out;
    })())`));
    console.log('  分组行: ' + grp.groups + ' 个；带重载按钮: ' + grp.reload + '；带添加按钮: ' + JSON.stringify(grp.add));

    ok(grp.groups > 0, '账号池里有上游分组行（' + grp.groups + ' 个）');
    ok(grp.reload === grp.groups,
      '每个分组行都有「重载 auths」（' + grp.reload + '/' + grp.groups + '）');

    // 有页内登录流程的上游才该有「＋ 添加账号」
    const withLogin = JSON.parse(await ev(`JSON.stringify(
      (window.__wb2api__.manifest().providers || [])
        .filter(function(p){ return p && p.login && p.login.kind; })
        .map(function(p){ return p.id; })
    )`));
    console.log('  manifest 里支持页内登录的上游: ' + JSON.stringify(withLogin));
    const sameSet = JSON.stringify(grp.add.slice().sort()) === JSON.stringify(withLogin.slice().sort());
    ok(sameSet,
      '「＋ 添加账号」只出现在支持页内登录的上游上\n      界面: ' + JSON.stringify(grp.add) +
      '\n      manifest: ' + JSON.stringify(withLogin));

    // ================================================================
    // R2：账号池必须列出**所有**上游（含 0 账号）
    // ================================================================
    //
    // # 判据（与模型面板补空态那次**同一条**）
    //
    // > 存在于 manifest.providers 的上游，界面上都必须出现。
    //
    // 期望值**从 manifest 现算**，不硬编码 —— 写死 ['workbuddy','codearts']
    // 就等于把这个部署的上游当成产品事实，加第三个上游时测试会假红，
    // 而"漏掉新上游"这个真缺陷反而不会被抓。
    console.log('\n[R2] 上游全集：manifest 里有的，账号池里都要有');
    const r2 = JSON.parse(await ev(`JSON.stringify((function(){
      var W = window.__wb2api__;
      var want = W.providerRegistryIds();          // manifest 的上游全集
      var seen = [];                               // 账号池实际渲染出来的分组
      document.querySelectorAll('#accts tr.grouprow').forEach(function(tr){
        seen.push(tr.dataset.acctgroup);
      });
      // 每个分组自己的账号行数（数到下一个分组行为止）
      var counts = {};
      document.querySelectorAll('#accts tr.grouprow').forEach(function(tr){
        var n = 0;
        for (var e = tr.nextElementSibling; e; e = e.nextElementSibling) {
          if (e.classList.contains('grouprow')) break;
          if (e.classList.contains('acctrow')) n++;
        }
        counts[tr.dataset.acctgroup] = n;
      });
      // 空组（0 账号）的自我解释文案
      var notes = {};
      document.querySelectorAll('#accts tr.grouprow').forEach(function(tr){
        var f = tr.querySelector('.gfolded');
        notes[tr.dataset.acctgroup] = f ? f.textContent.trim() : '';
      });
      return { want: want, seen: seen, counts: counts, notes: notes };
    })())`));
    console.log('  manifest 上游全集: ' + JSON.stringify(r2.want));
    console.log('  账号池实际分组  : ' + JSON.stringify(r2.seen));
    console.log('  各组账号数      : ' + JSON.stringify(r2.counts));

    // 断言 1：集合相等（不是"包含" —— 多一个没登记的分组同样是缺陷）
    const wantSorted = r2.want.slice().sort();
    const seenSorted = r2.seen.slice().filter(p => r2.want.indexOf(p) >= 0).sort();
    ok(JSON.stringify(wantSorted) === JSON.stringify(seenSorted),
      '账号池列出的上游**集合**等于 manifest 的上游集合\n      manifest: ' + JSON.stringify(wantSorted) +
      '\n      账号池  : ' + JSON.stringify(seenSorted));
    // 逐个点名：漏掉任何一个都要报出是哪个
    for (const p of r2.want) {
      ok(r2.seen.indexOf(p) >= 0, '上游 ' + p + ' 出现在账号池里（哪怕 0 个账号）');
    }

    // 断言 2：0 账号的组必须有一句**能自我解释**的话，不能是空白
    const zeroGroups = r2.want.filter(p => (r2.counts[p] || 0) === 0);
    console.log('  0 账号的上游: ' + JSON.stringify(zeroGroups) +
      (zeroGroups.length ? '' : '（本实例没有空组 —— 见 README 用 18081 实例覆盖）'));
    for (const p of zeroGroups) {
      ok((r2.notes[p] || '').length > 0,
        '0 账号的上游 ' + p + ' 有自我解释的说明（不是空白）: ' + JSON.stringify(r2.notes[p]));
      // 说明要讲"为什么没有"，不只是复述数字 —— 复述数字等于没说
      ok(/暂无账号|未在 manifest/.test(r2.notes[p] || ''),
        '上游 ' + p + ' 的说明讲清了"为什么没有"（含"暂无账号"）：' + JSON.stringify(r2.notes[p]));
    }

    // 断言 3：空组排在有账号的**之后**（用户确认的排序）
    const firstEmpty = r2.seen.findIndex(p => (r2.counts[p] || 0) === 0);
    if (firstEmpty >= 0) {
      const after = r2.seen.slice(firstEmpty).every(p => (r2.counts[p] || 0) === 0);
      ok(after, '空组（' + r2.seen.slice(firstEmpty).join(',') + '）全部排在非空组之后，没有交错');
    } else {
      console.log('    （没有空组，跳过排序断言）');
    }

    // 断言 4：提前返回已经删掉 —— 池子 0 个号时**仍然**要渲染出所有上游
    //
    // 这是 R2 的第二处缺陷：`if (!share.length) return '账号池为空'`。
    // 直接调 renderAccounts([]) 打进去，看它是不是还吐那句提前返回的文案。
    const emptyRender = await ev(`(function(){
      try {
        window.__wb2api__.renderAccounts([]);
        var html = document.getElementById('accts').innerHTML;
        return JSON.stringify({
          hasPlaceholder: /账号池为空/.test(html),
          groupCount: document.querySelectorAll('#accts tr.grouprow').length,
          providers: Array.prototype.slice.call(document.querySelectorAll('#accts tr.grouprow'))
                       .map(function(tr){ return tr.dataset.acctgroup; }),
        });
      } catch (e) { return JSON.stringify({ error: e.message }); }
    })()`);
    const er = JSON.parse(emptyRender);
    console.log('  空池渲染: ' + emptyRender);
    ok(!er.error, 'renderAccounts([]) 不抛异常' + (er.error ? '（' + er.error + '）' : ''));
    ok(er.hasPlaceholder === false,
      '空池**不再**显示「账号池为空」提前返回（那是 R2 删掉的出口）');
    ok(er.groupCount === r2.want.length,
      '空池时仍然渲染出全部 ' + r2.want.length + ' 个上游分组（实际 ' + er.groupCount + '）');

    // 恢复真实数据：刚才把表清空了，点一次刷新把它拉回来。
    // 必须 await 足够久 —— refresh() 是全量回源，本实例实测约 4 秒。
    await ev(`(function(){ var b=document.getElementById('refresh'); if(b) b.click(); return 1 })()`);
    await sleep(6000);

    // ================================================================
    // R3：分组可收起展开
    // ================================================================
    //
    // # 为什么必须量**真实几何**（offsetHeight / getComputedStyle.display）
    //
    // 只查 class 是可以作假的：给分组行挂上 collapsed 类但没有任何
    // display 规则命中它，"折叠"就是个空动作 —— 这正是变异验证 1 的形态。
    // 所以断言必须落在"行**真的**不占高度了"这个可观测量上。
    //
    // ⚠ 必须先切到账号池面板：其它面板 active 时这一块是 display:none，
    // 所有 offsetHeight **恒为 0** —— 断言会"看起来全对"其实什么都没测。
    // （这是我实测踩到的：第一版探针没切面板，拿到的全是 0。）
    console.log('\n[R3] 分组可收起展开（真实几何，不是只看 class）');
    const shown = await ev(`String(window.__wb2api__.showPanelByKey('accounts'))`);
    await sleep(1200);
    ok(shown === 'true', '能切到账号池面板（showPanelByKey("accounts")）');

    // 找一个**有账号行**的分组来测（空组没有行可隐藏，测不出折叠）
    const target = await ev(`(function(){
      var out = null;
      document.querySelectorAll('#accts tr.grouprow').forEach(function(tr){
        if (out) return;
        for (var e = tr.nextElementSibling; e; e = e.nextElementSibling) {
          if (e.classList.contains('grouprow')) break;
          if (e.classList.contains('acctrow')) { out = tr.dataset.acctgroup; return; }
        }
      });
      return out || '';
    })()`);
    console.log('  被测分组（有账号行的）: ' + JSON.stringify(target));
    ok(!!target, '账号池里存在"分组行 + 至少一个账号行"的分组');

    if (target) {
      // 重置成展开态（上一次运行可能留下了偏好）
      await ev(`localStorage.removeItem(window.__wb2api__.LS_ACCTGROUP + '.' + ${JSON.stringify(target)})`);

      const measure = async (pid) => JSON.parse(await ev(`JSON.stringify((function(){
        var T = ${JSON.stringify(target)};
        var tr = document.querySelector('#accts tr.grouprow[data-acctgroup="' + T + '"]');
        if (!tr) return { error: '找不到分组行 ' + T };
        var rows = [];
        for (var e = tr.nextElementSibling; e; e = e.nextElementSibling) {
          if (e.classList.contains('grouprow')) break;
          if (e.classList.contains('acctrow')) rows.push({ h: e.offsetHeight, disp: getComputedStyle(e).display });
        }
        var btn = tr.querySelector('button[data-atoggle]');
        return {
          collapsed: tr.classList.contains('collapsed'),
          aria: btn ? btn.getAttribute('aria-expanded') : null,
          totalH: rows.reduce(function(a, r){ return a + r.h; }, 0),
          hidden: rows.filter(function(r){ return r.disp === 'none' && r.h === 0; }).length,
          visible: rows.filter(function(r){ return r.disp !== 'none' && r.h > 0; }).length,
          n: rows.length,
        };
      })())`));

      const before = await measure(target);
      console.log('  折叠前: ' + JSON.stringify(before));
      ok(!before.error, '找得到被测分组行' + (before.error ? '（' + before.error + '）' : ''));
      ok(before.n > 0, '该分组有 ' + before.n + ' 个账号行可折叠');
      ok(before.visible === before.n && before.totalH > 0,
        '展开态：全部 ' + before.n + ' 行都可见且有高度（合计 ' + before.totalH + 'px）');

      // 点标题 → 折叠
      await ev(`document.querySelector('#accts tr.grouprow[data-acctgroup="' + ${JSON.stringify(target)} + '"] button[data-atoggle]').click()`);
      await sleep(400);
      const after = await measure(target);
      console.log('  折叠后: ' + JSON.stringify(after));

      // ---- 关键断言：真的塌陷了（不是只改 class）----
      ok(after.collapsed === true, '点标题后分组行带上了 collapsed');
      ok(after.hidden === after.n,
        '折叠后**全部** ' + after.n + ' 个账号行的 display 都变成 none（实际 ' + after.hidden + ' 个）');
      ok(after.totalH === 0,
        '折叠后这些行的**真实高度合计为 0**（实际 ' + after.totalH + 'px）—— ' +
        '只改 class 而不隐藏的"假折叠"会在这里红');
      ok(after.aria === 'false', 'aria-expanded 同步为 false（实际 ' + JSON.stringify(after.aria) + '）');

      // 再点一次 → 展开
      await ev(`document.querySelector('#accts tr.grouprow[data-acctgroup="' + ${JSON.stringify(target)} + '"] button[data-atoggle]').click()`);
      await sleep(400);
      const back = await measure(target);
      console.log('  再展开: ' + JSON.stringify(back));
      ok(back.collapsed === false && back.hidden === 0 && back.totalH > 0,
        '再点一次恢复展开（高度回到 ' + back.totalH + 'px）');
      ok(back.aria === 'true', 'aria-expanded 同步回 true');

      // ---- 折叠状态持久化（localStorage，独立前缀）----
      const lsKey = await ev(`window.__wb2api__.LS_ACCTGROUP + '.' + ${JSON.stringify(target)}`);
      await ev(`document.querySelector('#accts tr.grouprow[data-acctgroup="' + ${JSON.stringify(target)} + '"] button[data-atoggle]').click()`);
      await sleep(400);
      const lsVal = await ev(`localStorage.getItem(window.__wb2api__.LS_ACCTGROUP + '.' + ${JSON.stringify(target)})`);
      console.log('  localStorage: ' + lsKey + ' = ' + JSON.stringify(lsVal));
      ok(lsVal === '0', '折叠状态写进 ' + lsKey + '（值为 "0"）');

      // 前缀必须独立：绝不能与模型面板共用（那会互相覆盖且不报错）
      const prefix = await ev(`window.__wb2api__.LS_ACCTGROUP`);
      console.log('  账号池前缀: ' + JSON.stringify(prefix));
      ok(prefix !== 'wb2api.modelgroup',
        '账号池用的键前缀与模型面板**不同**（' + JSON.stringify(prefix) + ' ≠ "wb2api.modelgroup"）');
      ok(/^wb2api\.acctgroup$/.test(prefix || ''),
        '前缀是约定的 wb2api.acctgroup（实际 ' + JSON.stringify(prefix) + '）');

      // 重绘后仍保持（renderAccounts 每次重建表格，状态必须经 localStorage 恢复）
      await ev(`(function(){ var b=document.getElementById('refresh'); if(b) b.click(); return 1 })()`);
      await sleep(4500);
      await ev(`window.__wb2api__.showPanelByKey('accounts')`);
      await sleep(800);
      const afterRedraw = await measure(target);
      console.log('  刷新重绘后: ' + JSON.stringify(afterRedraw));
      ok(afterRedraw.collapsed === true && afterRedraw.totalH === 0,
        '刷新（表格整块重建）后折叠仍然保持 —— 状态来自 localStorage，不是渲染期的残留 class');

      // ---- 折叠**不跨组**：兄弟选择器做不到这件事，所以这条必须在 ----
      //
      // `tr.grouprow.collapsed ~ tr.acctrow` 会选中"其后**所有** acctrow"，
      // 跨越分组边界。实测证据见 foster_parenting_probe.js。
      const others = JSON.parse(await ev(`JSON.stringify((function(){
        var out = [];
        document.querySelectorAll('#accts tr.grouprow').forEach(function(tr){
          if (tr.dataset.acctgroup === ${JSON.stringify(target)}) return;
          var vis = 0, tot = 0;
          for (var e = tr.nextElementSibling; e; e = e.nextElementSibling) {
            if (e.classList.contains('grouprow')) break;
            if (e.classList.contains('acctrow')) { tot++; if (getComputedStyle(e).display !== 'none') vis++; }
          }
          out.push({ p: tr.dataset.acctgroup, tot: tot, vis: vis, collapsed: tr.classList.contains('collapsed') });
        });
        return out;
      })())`));
      console.log('  其它分组: ' + JSON.stringify(others));
      for (const o of others) {
        ok(o.collapsed === false && o.vis === o.tot,
          '折叠 ' + target + ' **不影响** ' + o.p + '（它 ' + o.vis + '/' + o.tot + ' 行仍可见）—— ' +
          '兄弟选择器会在这里跨组隐藏');
      }

      // 收尾：把偏好清掉，不给下一次运行留状态
      await ev(`localStorage.removeItem(window.__wb2api__.LS_ACCTGROUP + '.' + ${JSON.stringify(target)})`);
    }

    // ================================================================
    // R4：x无 与其它倍率气泡**同款**（计算值比对，不是字符串比对）
    // ================================================================
    console.log('\n[R4] x无 的气泡与其它倍率计算值相同');
    const bubbles = JSON.parse(await ev(`JSON.stringify((function(){
      function grab(sel){
        var el = document.querySelector('#models ' + sel);
        if (!el) return null;
        var cs = getComputedStyle(el);
        return { text: el.textContent.trim(), bg: cs.backgroundColor, color: cs.color, border: cs.borderStyle };
      }
      return {
        unknown: grab('.chip .mult.unknown'),
        plain: grab('.chip .mult:not(.unknown):not(.free):not(.hi)'),
        free: grab('.chip .mult.free'),
      };
    })())`));
    console.log('  ' + JSON.stringify(bubbles, null, 1).replace(/\n/g, '\n  '));

    if (bubbles.unknown && bubbles.plain) {
      ok(bubbles.unknown.bg === bubbles.plain.bg,
        'x无 与普通倍率的 background **计算值**相同\n      x无 : ' + bubbles.unknown.bg +
        '\n      普通: ' + bubbles.plain.bg);
      ok(bubbles.unknown.color === bubbles.plain.color,
        'x无 与普通倍率的 color 计算值相同\n      x无 : ' + bubbles.unknown.color +
        '\n      普通: ' + bubbles.plain.color);
      ok(bubbles.unknown.border === bubbles.plain.border,
        'x无 不再有虚线边框特例（border-style 相同：' + bubbles.unknown.border + '）');
      ok(bubbles.unknown.text === 'x无', '未知倍率的文字仍是 x无（与免费 x0 靠文字区分）');
    } else {
      ok(false, '模型面板里同时存在 x无 与普通倍率气泡（否则这条断言没测到东西）');
    }
    // 免费仍必须与"缺失"可区分 —— 用户接受的方案是**只靠文字**，
    // 所以这里断言文字不同、而**不**断言颜色不同。
    if (bubbles.unknown && bubbles.free) {
      ok(bubbles.unknown.text !== bubbles.free.text,
        '「缺失」(x无) 与「免费」(' + bubbles.free.text + ') 的文字不同 —— 区分由文字承担');
    }

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 账号池按钮清单验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
