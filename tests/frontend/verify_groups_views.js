// verify_groups_views.js —— 分组页**列表视图**的行为探针（本轮新增）。
//
// # 它验什么（以及为什么不是"读源码猜"）
//
// 用例把 webui-groups.html 的 <script> 真跑起来，喂一个最小 DOM 桩 + 假的
// GET /admin/groups 响应，然后调用页面自己的函数（onFilterChange / renderList /
// toggleDetail），断言**渲染出来的表格内容**。所以它抓的是行为：
//   · 超过一页时真的只渲染一页（默认 20）；
//   · 改搜索/筛选真的回第 1 页（而不是停在"第 3 页 / 共 1 页"的空屏）；
//   · 空态真的区分"没有分组"与"搜索无匹配"；
//   · 展开按钮真的维护 aria-expanded。
//
// # 它抓不到什么
//
// 真实浏览器的焦点顺序、样式、以及真后端的引用聚合 —— 那些归 verify_groups_panel.js
// （起隔离实例 + Chrome）。本文件是**静态/VM 级**守卫，不启动任何进程。
//
// 用法：node verify_groups_views.js   （exit 0 = 全绿）
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const HTML = path.join(__dirname, '..', '..', 'internal', 'server', 'webui-groups.html');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// 最小 DOM 桩：getElementById 按 id 记忆同一个元素；innerHTML 只当字符串存。
// ⚠ 故意**不**实现 HTML 解析 —— 断言看的是"写进 innerHTML 的字符串"，
//   那正是页面拼出来的渲染结果；引入真解析器反而会把断言变成对解析器的测试。
function makeDom() {
  const els = new Map();
  function el(id) {
    if (!els.has(id)) {
      els.set(id, {
        id, innerHTML: '', textContent: '', value: '', disabled: false,
        style: {}, classList: { add() {}, remove() {}, contains() { return false; } },
        listeners: {},
        addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
        setAttribute(k, v) { this['attr_' + k] = String(v); },
        getAttribute(k) { return this['attr_' + k]; },
        removeAttribute(k) { delete this['attr_' + k]; },
        hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this, 'attr_' + k); },
        appendChild() {}, firstElementChild: null, close() {}, showModal() {},
      });
    }
    return els.get(id);
  }
  const rows = el('rows');
  // 明细行的「填充」判定读 det.firstElementChild；给一个稳定对象即可。
  const detailCell = { innerHTML: '', getAttribute() { return null; }, setAttribute() {} };
  return {
    els, el, rows, detailCell,
    document: {
      getElementById: el,
      querySelector: () => null,
      querySelectorAll: () => [],
      addEventListener() {},
    },
  };
}

function groupsFixture(n) {
  const out = [];
  for (let i = 1; i <= n; i++) {
    out.push({
      id: 'g' + i,
      name: i === 7 ? 'vip-probe' : ('group-' + i),
      description: i === 7 ? 'probe description' : '',
      models: ['m1'],
      model_count: 1,
      // 偶数有引用、奇数无引用 —— 让"引用筛选"有可判定的分布。
      used_by_count: i % 2 === 0 ? 2 : 0,
      created_at: '2026-01-01T00:00:00Z',
    });
  }
  return out;
}

// 计数：渲染出的主行数（明细行也带 class="gdetail"，必须分开数）。
const countRows = (html) => (html.match(/class="grow"/g) || []).length;

(async () => {
  const src = fs.readFileSync(HTML, 'utf8');
  const scripts = [...src.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)].map((m) => m[1]);
  ok(scripts.length >= 1, '页面里有可执行的 <script> 块（实际 ' + scripts.length + ' 块）');
  const code = scripts[scripts.length - 1];

  const dom = makeDom();
  const fixture = groupsFixture(25);
  const sandbox = {
    console,
    document: dom.document,
    setTimeout, clearTimeout, setInterval, clearInterval,
    // 页面靠同源 fetch 拿数据；这里只回放固定响应，**不碰网络**。
    fetch: async (url) => {
      if (String(url).indexOf('/admin/groups/usage') >= 0) {
        return { ok: true, status: 200, text: async () => JSON.stringify({ total: 0, keys: [], channels: [], tokens: [] }) };
      }
      return { ok: true, status: 200, text: async () => JSON.stringify({ groups: fixture, enabled: true }) };
    },
    location: { origin: 'http://probe.invalid' },
    alert: () => {},
    confirm: () => true,
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);

  try {
    vm.runInContext(code, sandbox, { filename: 'webui-groups.inline.js' });
  } catch (e) {
    ok(false, '页面脚本求值不抛异常（实际抛了：' + e.message + '）');
    console.log('\n=== ' + fail + ' 项失败 ===');
    process.exit(1);
  }

  ok(true, '页面脚本在当前桩上求值成功（无语法/顶层状态错误）');

  // ⚠ 页面里的 listPage / groups 是 **let**（脚本级词法绑定），不会挂到 sandbox 上。
  //   直接写 sandbox.listPage = 2 改的是另一个对象，测出来的仍是旧状态 ——
  //   所以一切状态驱动都走「在同一 context 里再跑一段脚本」。
  const run = (js) => vm.runInContext(js, sandbox);

  // load() 是顶层异步调用，等它落定。
  await new Promise((r) => setTimeout(r, 30));

  ok(countRows(dom.rows.innerHTML) === 20,
    '默认每页 20 行（实际渲染 ' + countRows(dom.rows.innerHTML) + ' 行，共 25 个分组）');
  ok(/第 1 \/ 2 页/.test(dom.el('pager').innerHTML),
    '分页条显示「第 1 / 2 页」（实际 ' + JSON.stringify(dom.el('pager').innerHTML) + '）');
  ok(/共 25 个分组/.test(dom.el('stat').textContent),
    '统计行给出总数（实际 ' + JSON.stringify(dom.el('stat').textContent) + '）');

  // 翻到第 2 页再搜索：必须回到第 1 页，否则用户会停在空屏。
  run('listPage = 2; renderList();');
  ok(countRows(dom.rows.innerHTML) === 5, '第 2 页渲染 5 行（实际 ' + countRows(dom.rows.innerHTML) + '）');

  dom.el('gFilter').value = 'vip';
  run('onFilterChange();');
  ok(run('listPage') === 1, '搜索后回到第 1 页（实际 listPage=' + run('listPage') + '）');
  ok(countRows(dom.rows.innerHTML) === 1, '搜索 vip 命中 1 行（实际 ' + countRows(dom.rows.innerHTML) + '）');
  ok(dom.rows.innerHTML.indexOf('vip-probe') >= 0, '命中的是名称匹配的那一个分组');

  // 描述也参与搜索（需求：名称/描述搜索）。
  dom.el('gFilter').value = 'probe description';
  run('onFilterChange();');
  ok(countRows(dom.rows.innerHTML) === 1, '按描述也能搜到（实际 ' + countRows(dom.rows.innerHTML) + ' 行）');

  // 搜索无匹配 ≠ 没有分组。
  dom.el('gFilter').value = 'zzz-no-such-group';
  run('onFilterChange();');
  ok(countRows(dom.rows.innerHTML) === 0, '无匹配时不渲染数据行');
  ok(/没有符合当前搜索/.test(dom.rows.innerHTML) && /清空搜索/.test(dom.rows.innerHTML),
    '无匹配时空态说的是「没有符合条件」并给出下一步（实际 ' + JSON.stringify(dom.rows.innerHTML.slice(0, 120)) + '）');
  ok(!/还没有分组/.test(dom.rows.innerHTML), '无匹配时**不**误报成「还没有分组」');

  // 引用筛选：有引用 = 偶数 used_by_count>0，25 个里 12 个。
  dom.el('gFilter').value = '';
  dom.el('gRefFilter').value = 'used';
  run('onFilterChange();');
  ok(/匹配 12 个/.test(dom.el('stat').textContent),
    '「有引用」筛出 12 个（实际 ' + JSON.stringify(dom.el('stat').textContent) + '）');
  dom.el('gRefFilter').value = 'unused';
  run('onFilterChange();');
  ok(/匹配 13 个/.test(dom.el('stat').textContent),
    '「无引用」筛出 13 个（实际 ' + JSON.stringify(dom.el('stat').textContent) + '）');

  // 空列表：真的一句"还没有分组"，且引导到**令牌**页（旧入口「API 管理」已隐藏）。
  dom.el('gRefFilter').value = 'all';
  dom.el('gFilter').value = '';
  run('groups = []; renderList();');
  ok(/还没有分组/.test(dom.rows.innerHTML), '空列表时给「还没有分组」空态');
  ok(/「令牌」页/.test(dom.rows.innerHTML) && !/「API 管理」/.test(dom.rows.innerHTML),
    '空态引导到「令牌」页，不再指向已隐藏的「API 管理」');

  // 展开按钮：aria-expanded 必须跟着真实状态走（读屏依赖它）。
  run('groups = ' + JSON.stringify(groupsFixture(3)) + '; renderList();');
  ok(/aria-expanded="false"/.test(dom.rows.innerHTML), '初始渲染时展开按钮 aria-expanded=false');
  ok(/aria-controls="gd-g1"/.test(dom.rows.innerHTML), '展开按钮通过 aria-controls 指向明细行');
  ok(/aria-label="展开分组 group-1 的详情"/.test(dom.rows.innerHTML), '展开按钮有可读名称');

  // 桩一个明细行 + 按钮，直接驱动 toggleDetail。
  const det = dom.el('gd-g2');
  det.setAttribute('hidden', '');
  det.firstElementChild = dom.detailCell;
  const btn = { attrs: { 'aria-expanded': 'false' }, textContent: '▸', setAttribute(k, v) { this.attrs[k] = v; } };
  dom.document.querySelector = (sel) => (String(sel).indexOf('button.gx') >= 0 ? btn : null);
  run('toggleDetail("g2")');
  ok(!det.hasAttribute('hidden'), 'toggleDetail 展开时移除 hidden');
  ok(btn.attrs['aria-expanded'] === 'true', '展开后 aria-expanded=true（实际 ' + btn.attrs['aria-expanded'] + '）');
  run('toggleDetail("g2")');
  ok(det.hasAttribute('hidden') && btn.attrs['aria-expanded'] === 'false',
    '再点一次收起，aria-expanded 回到 false');

  // ── 保存防重：连点两次只能发一个请求（需求 4）──────────────────────────
  //
  // 判据是**请求次数**，不是"按钮 disabled"：脚本层才是真闸门（用户可能用键盘、
  // 或者第一次请求还没回来就再点一次）。这里把 fetch 换成计数器，直接调两遍 save()。
  {
    let posts = 0;
    const baseFetch = sandbox.fetch;
    sandbox.fetch = async (url, opt) => {
      if (String(url).indexOf('/admin/groups') >= 0 && opt && opt.method === 'POST') {
        posts++;
        await new Promise((r) => setTimeout(r, 20));   // 模拟在途，给第二次点击可乘之机
        return { ok: true, status: 200, text: async () => JSON.stringify({ ok: true }) };
      }
      return baseFetch(url, opt);
    };
    run('groups = []; renderList();');
    // 打开"新增"表单并填一个名字（openForm 内部会 ensureCatalog，桩里会走 fetch，但不打分组写口）。
    run('editing = null; $("f-name").value = "probe-new"; $("f-desc").value = ""; picked = new Set();');
    const p1 = run('save()');
    const p2 = run('save()');
    await Promise.all([p1, p2]);
    ok(posts === 1, '连点两次保存只发出 1 个写请求（实际 ' + posts + ' 个）');
    ok(run('saving') === false, '保存结束后 saving 状态复位（否则表单会永久卡在保存中）');
    sandbox.fetch = baseFetch;
  }

  console.log('\n' + (fail === 0 ? '=== 分组列表视图验收通过 ===' : '=== ' + fail + ' 项失败 ==='));
  process.exit(fail ? 1 : 0);
})();
