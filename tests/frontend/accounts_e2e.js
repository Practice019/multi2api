// accounts_e2e.js —— 真实浏览器验证账号池分组渲染与 B1/B5 修复。
//
// # 验证什么
//
//  T5 验收：账号表按上游分组，组标题显示「上游名 · N 个账号」
//  B1   ：错误态整行提示与真实列数一致（11 列），不再露底色
//  B5   ：/status 回落路径下账号仍能正确归属上游，且有"推断"标记
//
// # 为什么必须在真浏览器里做
//
// 这些都是**渲染结果**：colspan 是否等于表头列数、分组行是否真的插进去了、
// 单元格总数是否等于 cols × rows —— 抠函数文本在 Node 里跑证明不了这些。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = require('./chrome_path.js').resolveChrome();
const PORT = 9224;
const URL_UNDER_TEST = process.env.ACC_TEST_URL || 'http://127.0.0.1:18099/ui';
const PROFILE = require('os').tmpdir() + '/chrome-accprofile';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

// WORKBUDDY_HEADERS —— workbuddy 那 11 个表头，**硬编码**。
//
// # 为什么不从 `window.__wb2api__.DEFAULT_ACCT_COLUMNS` 取（那是循环论证）
//
// workbuddy 不实现 `AccountColumnsExt`，所以它渲染的列集来自**页面自己的**
// 回落常量。若期望值也从这个常量派生，断言就退化成恒真：
//
//	把 'token' 改成 'token_expiry'、或干脆删掉一列
//	→ want 跟着变 → 与本条及 td 数、unknown 三条一起全绿
//
// 实测确认过：这样改坏之后，当年那条 `ths === 11` 反而**会红** ——
// 也就是说 T5 的改写在这一处比它替换掉的旧断言更弱。
//
// 所以期望值必须来自**用户的要求**（「workbuddy 直接复用现在的标题」），
// 而不是来自被测页面。改文案要同时改这里和
// `internal/server/webui_account_columns_test.go` 里那份 —— 两处都硬编码是
// **故意的**：那正是"逐字不变"该有的摩擦。
const WORKBUDDY_HEADERS = [
  '上游', '昵称', 'UID', '额度', '状态', 'Token',
  '今日签到', '成功', '熔断', '在途', '操作',
];

function get(url) {
  return new Promise((res, rej) => {
    http.get(url, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const chrome = spawn(CHROME, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    `--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`,
    '--window-size=1400,1000', 'about:blank',
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
      } catch { /* 还没起来 */ }
      await sleep(250);
    }
    if (!target) throw new Error('Chrome 未启动');

    const ws = new WebSocket(target.webSocketDebuggerUrl);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    let id = 0; const pending = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
    const send = (method, params) => new Promise(r => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params: params || {} })); });

    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: URL_UNDER_TEST });
    await sleep(3000);

    const evalJs = async (expr) => {
      const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) {
        throw new Error('页面内异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 300));
      }
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // ---------------------------------------------------------------- 列数与 colspan 一致性
    //
    // # ⚠ 这里原来是「账号表 11 列」—— 拆多表之后它会**静默变绿**
    //
    // 原断言是 `document.querySelector('#accts').closest('table')` 再数 `thead th`。
    // 那**只看得到第一张表**。拆成"每上游一张表"之后：
    //   · 容器变成 <div>，`closest('table')` 返回 null（这一条会炸，还好）
    //   · 但若只把它改成"取第一张表"，`ths === 11` **照样通过** ——
    //     而 codearts 那张表可能已经涨到 20 列，没人知道
    //
    // 所以判据改成**逐组比对列集快照**：把每个上游的表头文字数组打出来，
    // 与「该上游自报的列集」逐一对照。列数只是它的推论，不再是判据本身。
    console.log('\n[B1] 每个上游各自一张表，各自列数与 colspan 自洽');
    const meta = JSON.parse(await evalJs(`(() => {
      const secs = Array.from(document.querySelectorAll('#accts > section.acctgroup'));
      return JSON.stringify(secs.map(s => {
        const tb = s.querySelector('table');
        const ths = Array.from(tb.querySelectorAll('thead th')).map(th => th.textContent.trim());
        const colspans = Array.from(tb.querySelectorAll('tbody td[colspan]'))
          .map(td => Number(td.getAttribute('colspan')));
        return { provider: s.dataset.acctgroup, cols: ths.length, ths, colspans };
      }));
    })()`));
    ok(meta.length >= 1, '至少渲染出一张上游表（实际 ' + meta.length + '）');
    meta.forEach(g => {
      console.log('    ' + g.provider + ' 列数=' + g.cols + ' ' + JSON.stringify(g.ths));
    });
    // 每张表自己的 colspan 都必须 >= 它自己的列数（B1 的形态是"小于"）
    let b1bad = [];
    meta.forEach(g => {
      g.colspans.filter(n => n < g.cols).forEach(n => b1bad.push(g.provider + ':' + n + '<' + g.cols));
    });
    ok(b1bad.length === 0, '没有小于**本表**列数的 colspan（B1 形态）' +
      (b1bad.length ? ' —— 发现 ' + JSON.stringify(b1bad) : ''));
    // 每张表至少有一列 —— 0 列说明列集没取到（回落失败），比"列数不对"更根本
    ok(meta.every(g => g.cols > 0), '每张表都有表头（0 列说明列集回落失败）');

    // 逐组比对列集**与后端自报的一致**（这才是真正的判据）
    //
    // ⚠ 这里**不能**把"未知列 id"也从期望里滤掉 —— 那等于装置抄了被测对象的规则
    //（被测代码就是"未知就跳过"，期望也"未知就跳过"，两边一起错 → 永远绿）。
    // 所以同时取**未经处理的自报列集长度**做交叉校验：
    // 只有当"自报的每一列前端都认识"时，渲染列数才允许等于自报列数。
    console.log('\n[B1b] 每个上游的表头 == 它在 manifest 里自报的列集');
    const colCheck = JSON.parse(await evalJs(`(() => {
      const W = window.__wb2api__;
      const m = W.manifest();
      const titleOf = id => (W.ACCT_COLUMN_DEFS[id] || {}).title || null;
      // workbuddy 那 11 个表头由 Node 侧**硬编码**注入（不从 W.DEFAULT_ACCT_COLUMNS 取）。
      const HARDCODED = ${JSON.stringify(WORKBUDDY_HEADERS)};
      const out = [];
      Array.from(document.querySelectorAll('#accts > section.acctgroup')).forEach(s => {
        const pid = s.dataset.acctgroup;
        const info = m.providers.find(p => p.id === pid);
        // 自报了列集（codearts）→ 用 manifest 当期望（"manifest ↔ 渲染一致"才是真契约）
        // 没自报（workbuddy）→ 用硬编码的 11 列当期望
        const declared = (info && Array.isArray(info.accounts_columns) && info.accounts_columns.length)
          ? info.accounts_columns : null;
        // 期望：把**认识的**列翻成标题；同时记下有无未知列
        const unknown = declared ? declared.filter(id => !W.ACCT_COLUMN_DEFS[id]) : [];
        const want = declared ? declared.map(titleOf).filter(Boolean) : HARDCODED;
        const got = Array.from(s.querySelectorAll('table > thead th')).map(th => th.textContent.trim());
        // 每个数据行的 td 数（用来交叉校验"表头跳了、单元格没跳"）
        const rowTds = Array.from(s.querySelectorAll('table > tbody > tr.acctrow'))
          .map(r => r.children.length);
        out.push({ pid, want, got, rawLen: declared ? declared.length : null,
                   selfReported: !!declared, unknown, rowTds });
      });
      return JSON.stringify(out);
    })()`));
    let colBad = [];
    colCheck.forEach(c => {
      if (JSON.stringify(c.want) !== JSON.stringify(c.got)) {
        colBad.push(c.pid + ': want ' + JSON.stringify(c.want) + ' got ' + JSON.stringify(c.got));
      }
    });
    ok(colBad.length === 0, '每个上游的表头都等于它自报的列集（顺序也算）' +
      (colBad.length ? ' —— ' + JSON.stringify(colBad) : ''));

    // ---- 上游**没自报**列集时（workbuddy）：表头必须逐字等于硬编码的 11 列 ----
    //
    // 上面那条对"自报列集"的上游是 manifest ↔ 渲染一致性；对没自报的上游，
    // 期望值必须来自本文件的 WORKBUDDY_HEADERS（用户要求），不能来自页面常量。
    const notReported = colCheck.filter(c => !c.selfReported);
    ok(notReported.length >= 1,
      '存在"未自报列集"的上游（workbuddy 走回落）—— 否则下面这条空转（实际 ' +
      notReported.length + ' 个）');
    const hardBad = notReported.filter(c =>
      JSON.stringify(c.got) !== JSON.stringify(WORKBUDDY_HEADERS));
    ok(hardBad.length === 0,
      '未自报列集的上游渲染出**逐字**等于硬编码的 11 列表头' +
      (hardBad.length ? ' —— ' + JSON.stringify(hardBad.map(c => ({ p: c.pid, got: c.got }))) : ''));

    // ---- rawLen 的交叉校验：自报几列就必须渲染几列 ----
    //
    // `unknown` 那条只覆盖"前端不认识这个 id"；它覆盖不了"认识但被
    // acctColumnsFor 静默漏掉"。长度比对是独立的一面。
    const rawLenBad = colCheck.filter(c => c.selfReported && c.got.length !== c.rawLen);
    ok(rawLenBad.length === 0,
      '自报列集的长度 == 渲染出的列数（有列被静默丢掉会在这里红）' +
      (rawLenBad.length ? ' —— ' + JSON.stringify(rawLenBad.map(c => ({ p: c.pid, rawLen: c.rawLen, got: c.got.length }))) : ''));

    // ---- 交叉校验：表头跳过的列，数据行也必须跳（评审发现的洞）----
    //
    // # 为什么必须有这两条
    //
    // 上面那条的 `want` 是"把**认识的**列翻成标题" —— 它和被测代码
    //（acctColumnsFor 滤掉未知 id）用的是**同一套规则**。
    // 两边一起错的时候，它会照样绿。
    //
    // 实测过的洞：`acctThHTML`（表头）与 `accountRow`（单元格）各自跳过未知列、
    // 互不知情。它们恰好规则相同，所以当时 th/td 能对上 —— 但那是**巧合**：
    // 给表头补一个占位分支就会 th 多一格、**整行错位**。
    //
    // 所以这里加两条**独立于实现**的判据：
    //   ① 自报里每一列前端都要认识（有未知 id 就是契约漂移，必须显式失败）
    //   ② 每个数据行的 td 数 == 表头列数（结构上不许错位）
    const unknownCols = colCheck.filter(c => c.unknown.length);
    ok(unknownCols.length === 0,
      'manifest 自报的列 id 前端全部认识（有未知 id = 前后端契约漂移）' +
      (unknownCols.length ? ' —— ' + JSON.stringify(unknownCols.map(c => ({ p: c.pid, unknown: c.unknown }))) : ''));

    const colMisalign = [];
    colCheck.forEach(c => {
      c.rowTds.forEach((n, i) => {
        if (n !== c.got.length) colMisalign.push(c.pid + ' 第' + i + '行: td=' + n + ' th=' + c.got.length);
      });
    });
    ok(colMisalign.length === 0,
      '每个数据行的 td 数 == 它表的表头列数（表头与单元格跳列不一致会在这里红）' +
      (colMisalign.length ? ' —— ' + JSON.stringify(colMisalign) : ''));

    // ---------------------------------------------------------------- 分组渲染
    console.log('\n[T5] 账号池按上游分区（每个上游一个 <section>）');
    const grouped = JSON.parse(await evalJs(`(() => {
      const secs = Array.from(document.querySelectorAll('#accts > section.acctgroup'));
      return JSON.stringify({
        sectionCount: secs.length,
        heads: secs.map(s => (s.querySelector('.ghead') || {}).textContent || '').map(t => t.replace(/\\s+/g,' ').trim()),
        accountRows: document.querySelectorAll('#accts > section.acctgroup > table > tbody > tr.acctrow').length,
      });
    })()`));
    console.log('    分区数=' + grouped.sectionCount + '  账号行=' + grouped.accountRows);
    grouped.heads.forEach(h => console.log('      · ' + h));
    ok(grouped.sectionCount >= 1, '至少有一个上游分区（实际 ' + grouped.sectionCount + '）');
    ok(grouped.heads.every(h => /个账号/.test(h)), '每个分区标题都带账号数');
    ok(grouped.accountRows > 0, '分区里还有真实账号行');

    // ---------------------------------------------------------------- 分组的**正确性**（自审补）
    //
    // # 为什么 ">= 1 个分组" 不够
    //
    // 自审注入实验证明：把 groupAccountsByProvider 换成"所有账号塞进一个默认上游组"，
    // 上面那三条**全部照样通过** —— 因为那确实还是"有 1 个分组、带账号数、
    // 有真实行"。分组功能整块失效却被判绿。
    //
    // 所以必须断言**分组数量与归属由数据决定**：
    //   分组数 == 账号实际覆盖到的上游数（从 manifest 推导，不硬编码）
    //   每个分组标题里的上游名，必须真的出现在 manifest.providers 里
    console.log('\n[T5] 分组数量与归属由数据决定（不是"有分组就行"）');
    const grpInfo = JSON.parse(await evalJs(`(() => {
      const W = window.__wb2api__;
      const m = W.manifest();
      const known = new Set(m.providers.map(p => p.id));
      // 分区标题里的上游名 = section 的 data-acctgroup 经 providerTitle 渲染出来的
      const secs = Array.from(document.querySelectorAll('#accts > section.acctgroup'));
      const titles = secs.map(s => {
        const n = s.querySelector('.gname');
        return n ? n.textContent.trim() : '';
      });
      // 每个账号行**自己**的归属：data-acctof（渲染时写死的事实），
      // 不再靠"第 1 格文本"—— 那是列序的函数，而列序现在按上游不同了。
      const rowProviders = Array.from(new Set(
        Array.from(document.querySelectorAll('#accts > section.acctgroup > table > tbody > tr.acctrow'))
          .map(r => r.dataset.acctof || '')
          .filter(Boolean)));
      return JSON.stringify({
        titles,
        known: Array.from(known),
        rowProviders,
        // 每个分区的 data-acctgroup，用来核对"账号行确实在自己那个分区里"
        secProviders: secs.map(s => s.dataset.acctgroup),
      });
    })()`));
    console.log('    分区标题=' + JSON.stringify(grpInfo.titles));
    console.log('    账号行归属=' + JSON.stringify(grpInfo.rowProviders));

    // 分区标题必须都是已知上游（不是空串、不是"（未标注）"以外的瞎写）
    ok(grpInfo.titles.every(t => t !== ''), '每个分区标题都有上游名（空标题说明分区没渲染出名字）');
    ok(grpInfo.titles.every(t => grpInfo.known.indexOf(t) >= 0 || t === '（未标注）'),
      '分区标题都是 manifest 里真实存在的上游（或兜底组）');

    // 关键：**每个账号行必须落在它自己那个上游的分区里**。
    // 把所有账号塞进一个分区时，这条会红（而"有分组就行"那三条照样绿）。
    const misplaced = JSON.parse(await evalJs(`(() => {
      const bad = [];
      Array.from(document.querySelectorAll('#accts > section.acctgroup')).forEach(s => {
        const pid = s.dataset.acctgroup;
        Array.from(s.querySelectorAll('table > tbody > tr.acctrow')).forEach(r => {
          if ((r.dataset.acctof || '') !== pid) bad.push(pid + ' 里混进了 ' + (r.dataset.acctof || '空'));
        });
      });
      return JSON.stringify(bad);
    })()`));
    ok(misplaced.length === 0, '每个账号行都在它自己上游的分区里' +
      (misplaced.length ? ' —— ' + JSON.stringify(misplaced) : ''));

    // 分区数必须覆盖账号实际涉及的所有上游
    const covered = grpInfo.rowProviders.filter(p => p !== '');
    if (covered.length > 1) {
      ok(grpInfo.titles.length >= covered.length,
        '分区数(' + grpInfo.titles.length + ') 覆盖了账号涉及的所有上游(' + covered.length + ' 个: ' + covered.join(',') + ')');
    } else {
      console.log('    （账号只涉及 ' + covered.length + ' 个上游，跳过分区数对照）');
    }

    // ---------------------------------------------------------------- 行的单元格配平
    console.log('\n[B1] 每个账号行的单元格数 == **它自己那张表**的列数');
    const balanced = JSON.parse(await evalJs(`(() => {
      const bad = [];
      Array.from(document.querySelectorAll('#accts > section.acctgroup')).forEach(s => {
        const tb = s.querySelector('table');
        const cols = tb.querySelectorAll('thead th').length;
        Array.from(tb.querySelectorAll('tbody tr')).forEach((r, i) => {
          let span = 0;
          Array.from(r.children).forEach(td => { span += Number(td.getAttribute('colspan') || 1); });
          if (span !== cols) bad.push({ provider: s.dataset.acctgroup, row: i, span, cols, text: r.textContent.slice(0, 40) });
        });
      });
      return JSON.stringify(bad);
    })()`));
    ok(balanced.length === 0, '每个上游表里的行都恰好占满**它自己**的列数' +
      (balanced.length ? ' —— 异常行 ' + JSON.stringify(balanced) : ''));

    // ---------------------------------------------------------------- 福利列：不许说"未领取"
    //
    // # 语义红线（本次的硬要求）
    //
    // 「福利」列的答案来自我们自己的领取记录。上游只回 claimable 布尔，
    // 分不清"今天已领完"与"资格不符" —— 所以**拿不准时必须显示 `—`**，
    // 绝不允许渲染「未领取」/「已领完」/「无可领」这类**断言**。
    //
    // 这条断言是**全等**的：把那几个词钉成黑名单，
    // 将来有人"顺手"把一个模糊状态渲染成确定结论时会立刻变红。
    console.log('\n[语义] 「福利」列只说事实，不猜上游状态');
    const welfareTxt = await evalJs(`(() => {
      const secs = Array.from(document.querySelectorAll('#accts > section.acctgroup'));
      const cellByCol = (s, wanted) => {
        const ths = Array.from(s.querySelectorAll('table > thead th')).map(th => th.textContent.trim());
        const idx = ths.indexOf(wanted);
        if (idx < 0) return null;
        return Array.from(s.querySelectorAll('table > tbody > tr.acctrow')).map(r => {
          const td = r.children[idx];
          return td ? td.textContent.trim() : '';
        });
      };
      return JSON.stringify({ codearts: (() => {
        const s = secs.find(x => cellByCol(x, '福利'));
        return s ? cellByCol(s, '福利') : null;
      })() });
    })()`);
    const wv = JSON.parse(welfareTxt);
    console.log('    福利列内容=' + JSON.stringify(wv.codearts));
    if (wv.codearts && wv.codearts.length) {
      const banned = ['未领取', '已领完', '无可领', '没得领'];
      const hit = [];
      wv.codearts.forEach(t => banned.forEach(b => { if (t.indexOf(b) >= 0) hit.push(t); }));
      ok(hit.length === 0, '福利列没有出现断言性的词（' + banned.join('/') + '）' +
        (hit.length ? ' —— 实际 ' + JSON.stringify(hit) : ''));
      // 允许的取值是一个**闭集**：已领取 / 未领到 / 失败 / —
      const allowed = new Set(['已领取', '未领到', '失败', '—', '']);
      const off = wv.codearts.filter(t => !allowed.has(t));
      ok(off.length === 0, '福利列的值都在允许集合内（已领取/未领到/失败/—）' +
        (off.length ? ' —— 越界 ' + JSON.stringify(off) : ''));
    } else {
      console.log('    （页面上没有「福利」列 —— 当前实例的 codearts 可能没自报该列，跳过）');
    }

    // ---------------------------------------------------------------- 页面无脚本错误
    console.log('\n[健康] 页面脚本无异常');
    const errs = await evalJs('JSON.stringify(window.__errs || [])');
    ok(errs === '[]' || errs === undefined, '无未捕获的脚本错误');

    ws.close();
  } catch (e) {
    console.log('\nEXCEPTION: ' + e.message);
    fail++;
  } finally {
    try { chrome.kill(); } catch { /* 已退出 */ }
  }

  console.log(fail === 0 ? '\n=== 账号池端到端全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
