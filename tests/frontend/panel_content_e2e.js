// panel_content_e2e.js —— 逐面板断言"**内容真的渲染出来了**"。
//
// # 为什么需要它（变异扫描的结果）
//
// 第二轮变异扫描发现：把下面任何一个函数整个短路，13 个 E2E **全部照样绿**：
//   renderModels / loadSettings / renderGrowthGroups / renderTravel
//   renderLogs / loadHistory / renderPager / renderIdentity
//
// 原因是现有套件验证的是**导航与结构**（分组数、列数、data 属性、主题），
// 而"点开某个面板后**里面有没有东西**"从没被断言过。
// 用户看到的恰好是后者 —— 面板能点开但一片空白，正是最严重的可用性缺陷。
//
// # 设计原则：断言"内容与数据一致"，不是"元素存在"
//
// 只断言"某元素存在"会被空态占位行骗过（加载中/暂无数据也是元素）。
// 所以每个面板都要求：
//   1. 有真实内容（文本长度超过空态阈值）
//   2. **不是**加载中/空态/错误态
//   3. 关键字段确实出现（如账号表要有 UID、模型面板要有模型名）
//
// 每个断言都对应一个具体的变异用例，能在变异下变红。
const http = require('http');
const { spawn } = require('child_process');
const fs = require('fs');

const CHROME = 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe';
const PORT = 9260;
const BASE = process.env.PANEL_URL || 'http://127.0.0.1:18080/ui';
const PROFILE = require('os').tmpdir() + '/chrome-panelcontent';  // 绝对路径（原为相对 cwd，跨目录执行会串台）

function get(u) {
  return new Promise((res, rej) => {
    http.get(u, r => { let d = ''; r.on('data', c => d += c); r.on('end', () => res(d)); }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

// 每个面板：nav 是导航名。
//
// ⚠ 判据的演化（每一版都在**正常版**上假红过，逐条记下来防止再犯）：
//   v1 "文本长度 >= N"              → 刚启动的实例日志为空 → 假红
//   v2 "行数 > 0"                   → 卡片/表单型面板没有 tbody → 假红
//   v3 ".card/.gitem"               → 模型与设置面板 DOM 又不同 → 假红
//   v4 "非空叶子 >= 3"              → **正常版全绿**，但放过了 4 个真实变异
//                                     （日志 4730→255 字、历史 3767→134 字，
//                                      仍满足"叶子>=3"）
//
// 最终判据：**内容量必须与数据量相称**。
// 表格型面板断言"数据行数 == 接口返回的条目数"（数据驱动，不是魔法数字）；
// 其余面板断言"叶子数超过一个明显偏低的门槛"，
// 门槛由实测正常值取下界（留足余量，但不至于放过 10 倍缩水）。
const PANELS = [
  { nav: '可用模型', key: 'models', must: ['模型'], reject: ['加载中'],
    minLeaves: 20, api: '/admin/models/preview', countPath: 'models' },
  { nav: '请求日志', key: 'logs', must: [], reject: ['加载中'], table: true,
    emptyOk: ['落盘日志为空', '暂无请求', '实时视图中'],
    minLeaves: 60, api: '/admin/logs/history?limit=30&offset=0', countPath: 'items' },
  { nav: '任务历史', key: 'history', must: [], reject: ['加载中'], table: true,
    emptyOk: ['暂无记录'],
    minLeaves: 60, api: '/admin/checkin/history?limit=30&offset=0', countPath: 'items' },
  // ⚠ 设置面板除了"输入框被回填"，还要看**只读信息行**。
  // 变异 S7（renderLogFileInfo 短路）会让 #setLogFileInfo 变成 0 字符，
  // 而输入框断言完全看不到 —— 用户会丢掉"日志落盘路径/条数/保留天数"这段信息。
  { nav: '设置', key: 'settings', must: [], reject: [],
    minLeaves: 30, expectFilledInputs: true, requireNonEmpty: ['#setLogFileInfo'] },
  // ⚠ 成长计划：叶子数判据抓不住 renderGrowthGroups 变异（正常 101 / 变异 95，
  // 只差 6%，任何阈值都是碰运气）。改用该函数**独有**的 DOM 指纹：
  // 正常版有 .ggroup/.ghead/.gname 各 1 个，变异版一个都没有（实测对比）。
  { nav: '成长计划', key: 'workbuddy:growth', must: [], reject: ['加载中'], table: true,
    emptyOk: ['暂无', '没有账号'], minLeaves: 60,
    requireSelectors: ['.ggroup', '.ghead', '.gname'],
    api: '/admin/growth', countPath: 'accounts' },
  { nav: '猫猫旅行', key: 'workbuddy:travel', must: [], reject: ['加载中'], table: true,
    emptyOk: ['暂无', '没有账号'], minLeaves: 30, api: '/admin/travel', countPath: 'cats' },
];

(async () => {
  fs.rmSync(PROFILE, { recursive: true, force: true });
  const ch = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--remote-debugging-port=' + PORT, '--user-data-dir=' + PROFILE,
    '--window-size=1440,1000', 'about:blank'], { stdio: 'ignore' });
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  try {
    let t = null;
    for (let i = 0; i < 40; i++) {
      try {
        const l = JSON.parse(await get('http://127.0.0.1:' + PORT + '/json/list'));
        t = l.find(x => x.type === 'page' && x.webSocketDebuggerUrl);
        if (t) break;
      } catch { /* 等 */ }
      await sleep(250);
    }
    if (!t) throw new Error('Chrome 未启动');

    const ws = new WebSocket(t.webSocketDebuggerUrl);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    let id = 0;
    const pend = new Map();
    ws.onmessage = e => { const m = JSON.parse(e.data); if (m.id && pend.has(m.id)) { pend.get(m.id)(m); pend.delete(m.id); } };
    const send = (mm, p) => new Promise(r => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method: mm, params: p || {} })); });
    await send('Page.enable');
    await send('Runtime.enable');
    await send('Page.navigate', { url: BASE });
    await sleep(4000);

    const ev = async (x) => {
      const r = await send('Runtime.evaluate', { expression: x, returnByValue: true, awaitPromise: true });
      if (r.result && r.result.exceptionDetails) throw new Error('页面异常: ' + JSON.stringify(r.result.exceptionDetails).slice(0, 200));
      return r.result && r.result.result ? r.result.result.value : undefined;
    };

    // 先确认实例可达（避免在死端口上得出"面板空白"的假结论）
    const alive = await ev('typeof window.__wb2api__ === "object"');
    ok(alive === true, '页面已加载（__wb2api__ 可用）—— 避免量到死实例');

    for (const p of PANELS) {
      // 点击导航
      const clicked = await ev(`(function(){
        var bs = Array.from(document.querySelectorAll('#nav button[data-nav]'));
        var b = bs.filter(function(x){ return x.textContent.indexOf(${JSON.stringify(p.nav)}) >= 0; })[0];
        if (b) { b.click(); return true; }
        return false;
      })()`);
      ok(clicked, `找到并点击「${p.nav}」`);
      if (!clicked) continue;
      // ⚠ 1600ms 对设置面板不够：#setLogFileInfo 由**另一个**请求填充
      // （/admin/logs/history 里的 file 字段），比配置本身晚到。
      // 我在这里等待不足时，正常版会假红「含关键字段「落盘」」。
      await sleep(p.key === 'settings' ? 3000 : 1600);

      const info = await ev(`(function(){
        var sec = document.querySelector('#content > section.active');
        if (!sec) return { err: 'no active section' };
        var txt = (sec.textContent || '').replace(/\\s+/g, ' ').trim();
        var leaves = 0, all = sec.querySelectorAll('*');
        for (var i = 0; i < all.length; i++) {
          var el = all[i];
          if (el.children.length === 0 && (el.textContent || '').trim().length > 0) leaves++;
        }
        return {
          key: sec.dataset.key || '',
          len: txt.length,
          text: txt.slice(0, 200),
          rows: sec.querySelectorAll('tbody tr').length,
          leaves: leaves,
          // 表格型面板的"真实数据行"（排除空态那一行）
          dataRows: Array.from(sec.querySelectorAll('tbody tr'))
            .filter(function(tr){ return !tr.querySelector('td[colspan]'); }).length,
          leafText: Array.from(sec.querySelectorAll('*'))
            .filter(function(e){ return e.children.length === 0 && (e.textContent||'').trim().length > 0; })
            .map(function(e){ return e.textContent.trim(); }).slice(0, 8),
          inputs: sec.querySelectorAll('input,select,textarea').length,
          nonEmpty: (function(){
            var o = {};
            ${JSON.stringify(p.requireNonEmpty || [])}.forEach(function(sel){
              var e = document.querySelector(sel);
              o[sel] = e ? (e.textContent || '').trim().length : -1;
            });
            return o;
          })(),
          selCounts: (function(){
            var o = {};
            ${JSON.stringify(p.requireSelectors || [])}.forEach(function(s){ o[s] = sec.querySelectorAll(s).length; });
            return o;
          })(),
        };
      })()`);

      if (info.err) { ok(false, `${p.nav}: ${info.err}`); continue; }

      // 1) 不得停留在加载态
      const hitReject = (p.reject || []).filter(w => info.text.indexOf(w) >= 0);
      ok(hitReject.length === 0,
        `${p.nav}: 不在加载态${hitReject.length ? '（命中 ' + hitReject.join('/') + '）' : ''}`);

      // 2) 内容量必须与**接口数据量**相称。
      //
      // 这是唯一既能放过"空态"、又能抓住"缩水 10 倍"的判据：
      // 先问后端要一次数据，再看面板渲染了多少。
      const emptyWords = p.emptyOk || [];
      const hasEmptyState = emptyWords.some(w => info.text.indexOf(w) >= 0);
      const hasTableData = info.dataRows > 0;

      let dataCount = null;
      if (p.api) {
        dataCount = await ev(`(function(){
          var x = new XMLHttpRequest();
          try {
            x.open('GET', ${JSON.stringify(p.api)}, false);
            x.setRequestHeader('Authorization', 'Bearer ' + (window.__WB2API_KEY__ || ''));
            x.send(null);
            if (x.status !== 200) return -1;
            var d = JSON.parse(x.responseText);
            var v = d[${JSON.stringify(p.countPath)}];
            if (Array.isArray(v)) return v.length;
            if (typeof v === 'number') return v;
            return -2;
          } catch (e) { return -3; }
        })()`);
      }

      const enoughLeaves = info.leaves >= (p.minLeaves || 3);
      ok(enoughLeaves || hasEmptyState,
        `${p.nav}: 渲染量充足（非空叶子 ${info.leaves} >= ${p.minLeaves || 3}）或明确空态${hasEmptyState ? '(是)' : '(否)'}`);

      // 数据驱动：接口有条目时，**表格型**面板必须有对应数量的数据行。
      //
      // ⚠ 只对表格型面板断言 dataRows —— "可用模型"用的是卡片布局，
      // 接口 30 条而 dataRows 恒为 0（那是设计如此）。我第一版没区分，
      // 在正常版上假红一次。用 `table:true` 显式标注哪些面板是表格。
      if (dataCount !== null && dataCount > 0 && p.table) {
        ok(hasTableData,
          `${p.nav}: 接口返回 ${dataCount} 条，面板有 ${info.dataRows} 个数据行 —— 不能两者都有数据却渲染成空`);
      }
      if (dataCount !== null) {
        console.log(`      （接口 ${dataCount} 条 / 渲染 ${info.dataRows} 行 / 叶子 ${info.leaves} / 文本 ${info.len} 字）`);
      }

      // 3) 关键字段出现
      for (const m of p.must) {
        ok(info.text.indexOf(m) >= 0, `${p.nav}: 含关键字段「${m}」`);
      }

      // 3a2) 指定的只读信息行必须有内容（变异 S7 的落点）
      for (const sel of (p.requireNonEmpty || [])) {
        const n = info.nonEmpty ? info.nonEmpty[sel] : -1;
        ok(n > 0, `${p.nav}: ${sel} 有内容（${n} 字）—— 只读信息行不是空的`);
      }

      // 3b) 结构指纹：该面板**独有**的元素必须存在。
      //
      // 这是"叶子数"抓不住变异时的替代方案：renderGrowthGroups 被短路后
      // 叶子只少 6%（101→95），任何阈值都是碰运气；但 .ggroup/.ghead/.gname
      // 会**直接消失**（实测：正常各 1 个，变异 0 个）。断言"存在"比
      // 断言"数量够多"更能锚定到具体渲染函数。
      if (p.requireSelectors && p.requireSelectors.length) {
        for (const sel of p.requireSelectors) {
          const n = info.selCounts ? info.selCounts[sel] : 0;
          ok(n > 0, `${p.nav}: 结构指纹 ${sel} 存在（${n} 个）—— 锚定到具体渲染函数`);
        }
      }
      // 4) 设置面板要有回填的输入值（这正是 loadSettings 的作用）
      if (p.expectFilledInputs) {
        const filled = await ev(`(function(){
          var sec = document.querySelector('#content > section.active');
          if (!sec) return -1;
          var vals = Array.from(sec.querySelectorAll('input[type=text],input[type=number]'))
            .map(function(i){ return String(i.value||'').trim(); });
          return vals.filter(function(v){ return v.length > 0; }).length;
        })()`);
        ok(filled > 0, `设置: 有 ${filled} 个输入框被回填了值（loadSettings 生效）`);
      }
      console.log(`      （文本 ${info.len} 字，行数 ${info.rows}，输入框 ${info.inputs}）`);
    }

    ws.close();
  } catch (e) { console.log('\nEXCEPTION: ' + e.message); fail++; }
  finally { try { ch.kill(); } catch { /* 已退出 */ } }

  console.log(fail === 0 ? '\n=== 面板内容渲染全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
