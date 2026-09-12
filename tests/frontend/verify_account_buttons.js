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

    ws.close();
  } catch (e) { console.log('EXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { } }
  console.log(fail === 0 ? '\n=== 账号池按钮清单验证通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
