// tests/frontend/verify_model_map_editor.js —— 守住「模型对外名」编辑器是行式、且转置方向对
//
// # 历史
//
// 2026-10-21 第一版是 textarea（每行 `内部名 = 对外名`），配套门禁
// verify_model_map_transpose.js 用「抠出纯函数真跑」的方式断言转置方向。
//
// 当天用户改要求：「我希望是左边一个名字，右边一个名字，自己填。然后可以新增行，
// 一开始是没有的。然后你可以点新增一个行，然后填左边的，填右边的。」
//
// ⇒ 改成行式（每行两个 input + datalist）。行式依赖 DOM，没有可以抠出来裸跑的
//   纯函数了，所以这门禁改成**结构 + 方向**双查：
//
//	结构：必须有 akMapRows 容器 / akMapAdd 按钮 / datalist / 三个函数
//	方向：akCollectMap 里必须是 `map[right] = left`（对外名 → 内部名）
//
// ⚠ 方向那条仍然是这门禁最值钱的部分：写反了是**静默**的 ——
//   保存成功、行也显示，只是调用方拿到的名字对不上。
'use strict';

const fs = require('fs');
const path = require('path');

const htmlPath = path.join(__dirname, '..', '..', 'internal', 'server', 'webui.html');
const src = fs.readFileSync(htmlPath, 'utf8');

let fails = 0;
const fail = (m) => { console.log('FAIL ' + m); fails++; };
const pass = (m) => console.log('PASS ' + m);

// ── ① 结构：容器 / 按钮 / 候选列表 ──
for (const [id, why] of [
  ['akMapRows', '行容器（每行两个 input）'],
  ['akMapAdd', '「+ 新增一行」按钮'],
  ['akMapModelOpts', '左侧候选 datalist'],
]) {
  if (!src.includes(`id="${id}"`)) fail(`★ 缺少 #${id} —— ${why}`);
  else pass(`① #${id} 存在（${why}）`);
}

// ── ② 旧 textarea 必须彻底删掉 ──
//
// 留着它不会报错（没人引用），但会让人以为还有两条编辑路径，
// 而实际生效的只有行式那一条 —— 这种"看起来有两个入口"最难排查。
if (/id="akModelMap"/.test(src) || /akModelMapToText|akParseModelMap/.test(src)) {
  fail('★ 旧的 textarea 编辑器（#akModelMap / akModelMapToText / akParseModelMap）还有残留');
} else {
  pass('② 旧 textarea 编辑器已彻底移除');
}

// ── ③ 三个函数都在 ──
for (const fn of ['akAddMapRow', 'akRenderMap', 'akCollectMap']) {
  if (!new RegExp(`function\\s+${fn}\\s*\\(`).test(src)) fail(`★ 缺少函数 ${fn}`);
  else pass(`③ 函数 ${fn} 存在`);
}

// ── ④ ★ 转置方向：akCollectMap 里必须是 map[对外名] = 内部名 ──
const ci = src.indexOf('function akCollectMap(');
if (ci < 0) {
  fail('★ 找不到 akCollectMap');
} else {
  // 取到函数结束（成对花括号）
  let i = src.indexOf('{', ci), depth = 0, end = -1;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) { end = i + 1; break; } }
  }
  const body = end > 0 ? src.slice(ci, end) : '';
  // 期望至少有一行形如 `map[right] = left`
  const rightIsKey = /map\[\s*right\s*\]\s*=\s*left/.test(body);
  const wrongWay = /map\[\s*left\s*\]\s*=\s*right/.test(body);
  if (wrongWay) {
    fail('★ akCollectMap 转置方向反了：写成 map[left] = right');
    console.log('     ⇒ 存储约定是 {对外名: 内部名}，反了会让请求侧查不到映射，');
    console.log('       表现为「界面上设了别名，但调用方用别名仍然 404」。');
  } else if (!rightIsKey) {
    fail('★ akCollectMap 里找不到 `map[right] = left`（转置语句被改了？）');
  } else {
    pass('④ akCollectMap 转置方向正确：map[对外名] = 内部名');
  }
}

// ── ⑤ 左侧候选必须来自「已勾选的模型」，不是全量目录 ──
{
  const si = src.indexOf('function syncAKMapDatalist(');
  if (si < 0) {
    fail('★ 找不到 syncAKMapDatalist');
  } else {
    let i = src.indexOf('{', si), depth = 0, end = -1;
    for (; i < src.length; i++) {
      if (src[i] === '{') depth++;
      else if (src[i] === '}') { depth--; if (depth === 0) { end = i + 1; break; } }
    }
    const body = end > 0 ? src.slice(si, end) : '';
    if (!/akSelected\s*\(\s*\)/.test(body)) {
      fail('★ 左侧候选没用 akSelected() —— 候选应是"已勾选的模型"（用户原话）');
    } else if (/akCatalog/.test(body)) {
      fail('★ 左侧候选用了全量 akCatalog —— 会给没勾的模型也列出来，选它等于配一条永不生效的映射');
    } else {
      pass('⑤ 左侧候选来自 akSelected()（已勾选的模型）');
    }
  }
}

// ── ⑥ 「新增一行」必须真的绑上（未绑 = 点了没反应）──
if (!/\$\('akMapAdd'\)\.onclick\s*=/.test(src)) {
  fail("★ #akMapAdd 没有绑 onclick —— 按钮点了不会有任何反应");
} else {
  pass('⑥ 「+ 新增一行」已绑定');
}

if (fails > 0) {
  console.log('\n=== 失败 ' + fails + ' 项 ===');
  process.exit(1);
}
console.log('\n=== 全部通过 ===');
