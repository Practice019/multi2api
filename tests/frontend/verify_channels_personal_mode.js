// verify_channels_personal_mode.js —— 渠道页「服务端搜索/分页 + 表单往返」的行为探针。
//
// 为什么是纯函数级探针：列表的真值在**请求 URL**（keyword / p / page_size），
// 页面里的渲染只是把返回结果铺开。把 load() 的 URL 构造与 pages() 的总数计算
// 抽出来跑，能直接抓到「只取 100 条再本地搜索」这个缺陷，而不依赖浏览器。
// 端到端（真实例 + DOM）另由 verify_channels_save.js 覆盖。
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const PAGE = path.join(__dirname, '..', '..', 'internal', 'server', 'webui-channels.html');

function extract(src, header) {
  const i = src.indexOf(header);
  if (i < 0) { throw new Error('找不到函数：' + header); }
  let depth = 0, started = false;
  for (let j = i; j < src.length; j++) {
    const c = src[j];
    if (c === '{') { depth++; started = true; }
    else if (c === '}') { depth--; if (started && depth === 0) { return src.slice(i, j + 1); } }
  }
  throw new Error('函数未闭合：' + header);
}

function loadPage(file) {
  return fs.readFileSync(file, 'utf8');
}

function makeCtx(src) {
  const ctx = {
    st: { page: 1, psize: 20, keyword: '', total: 0, rows: [], typeCounts: {}, statusCounts: {}, ability: 0, sel: {}, drag: false, dragFrom: -1, f: { status: '', type: '', group: '', sortBy: '', sortOrder: '' }, tagMode: '' },
    urls: [],
    render() {},
    exitDrag() {},
    activeFilterCount() { return 0; },
    encodeURIComponent,
    Math,
    // ⚠ loadSeq 是**页面级**的（load 之外的 var）：并发保护用「只接受最近一次 load 的响应」。
    //   把 load() 抽出来单独求值时，沙箱里必须也有它，否则第一句 ++loadSeq 就 ReferenceError。
    //   每次 makeCtx 都给一份新的 0，保证各用例互不串扰（正是页面里「一次会话一个计数器」的等价物）。
    loadSeq: 0,
  };
  ctx.api = function (method, url) { ctx.urls.push(url); return Promise.resolve({ items: [], total: 0, type_counts: {}, status_counts: {} }); };
  ctx.load = vm.runInNewContext('(' + extract(src, 'function load(') + ')', ctx);
  ctx.pages = vm.runInNewContext('(' + extract(src, 'function pages(') + ')', ctx);
  return ctx;
}

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
  const target = process.env.CHANNELS_HTML || PAGE;
  const src = loadPage(target);
  console.log('被测文件：' + target);

  // ① 搜索必须交给服务端（keyword 进 URL）
  let c = makeCtx(src);
  c.st.keyword = 'prod-key';
  c.st.page = 4;
  await c.load(false);
  const u = c.urls[0] || '';
  ok(/[?&]keyword=prod-key(&|$)/.test(u), 'load 把关键词下推到服务端（实际：' + u + '）');
  ok(/[?&]p=4(&|$)/.test(u), 'load 携带当前页码 p=4（实际：' + u + '）');
  ok(/[?&]page_size=20(&|$)/.test(u), 'load 携带当前每页条数（实际：' + u + '）');

  // ② 筛选变更回到第 1 页，翻页不重置
  c = makeCtx(src);
  c.st.page = 3;
  await c.load(true);
  ok(/[?&]p=1(&|$)/.test(c.urls[0] || ''), 'resetPage=true 时回到第 1 页（实际：' + c.urls[0] + '）');
  c = makeCtx(src);
  c.st.page = 3;
  await c.load(false);
  ok(/[?&]p=3(&|$)/.test(c.urls[0] || ''), 'resetPage=false 时保留当前页（实际：' + c.urls[0] + '）');

  // ③ 页数按服务端 total 算，而不是已取到手的行数
  c = makeCtx(src);
  c.st.total = 205;
  c.st.psize = 20;
  c.st.rows = new Array(20).fill({ id: 'x' });
  ok(c.pages() === 11, 'pages() = ceil(total/psize) = 11（实际：' + c.pages() + '）');

  // ④ 渠道表单：两个 <select> 起始标签必须闭合（缺 > 会让选项被解析成属性）
  ok(/id="f-mode">/.test(src), '创建模式 select 起始标签完整（id="f-mode">）');
  ok(/id="f-kmode">/.test(src), '多 Key 模式 select 起始标签完整（id="f-kmode">）');

  // ⑤ 可视化模型映射：真值在可视行，保存前必须写回 JSON
  const syncSrc = extract(src, 'function syncMapFromVisual(');
  ok(/syncMapFromVisual\(\)/.test(extract(src, 'function parseMap(')), 'parseMap() 先同步可视化映射再解析');
  const visRows = [{ value: 'public-a' }, { value: 'public-b' }];
  const visVals = [{ value: 'upstream-a' }, { value: 'upstream-b' }];
  const dom = {
    'f-mapvis': { hidden: false, querySelectorAll(sel) { return sel === '.mv-k' ? visRows : visVals; } },
    'f-map': { value: '{"public-a":"stale"}' },
  };
  const mctx = { dlgState: { visualMap: true }, $: (id) => dom[id], JSON, Math };
  vm.runInNewContext(syncSrc + ';syncMapFromVisual();', mctx);
  const parsed = JSON.parse(dom['f-map'].value);
  ok(parsed['public-a'] === 'upstream-a' && parsed['public-b'] === 'upstream-b',
    '可视编辑结果写回 textarea（实际：' + dom['f-map'].value + '）');

  // ⑥ 标签：新建与清空都要提交（**活代码**里不得再有跳过新建/空标签的分支）。
  //   先剥掉注释再判定 —— 注释里正记着这条旧写法，按原文匹配会误报。
  const codeOnly = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  ok(!/if \(!tag \|\| dlgState\.isNew\)/.test(codeOnly), '保存时不再跳过「新建 + 标签」');
  ok(/tag === dlgState\.tag0/.test(src), '标签未变化时才跳过 tag/batch');

  console.log(fail === 0 ? '全部通过' : (fail + ' 项失败'));
  process.exit(fail === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
