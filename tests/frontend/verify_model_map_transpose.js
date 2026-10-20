// tests/frontend/verify_model_map_transpose.js —— 守住「模型对外名」的转置方向
//
// # 为什么需要这个文件（2026-10-21）
//
// 用户要求：「这些模型，开放的模型，对于这个 API key 开放的模型，我觉得需要
// 有一个模型映射，就是对外的模型应该叫什么」。
//
// 实现里有两处方向相反的东西：
//
//	存储（JSON / 后端）:  { 对外名: 内部名 }    ← 请求进来查一次就够
//	界面（textarea）   :  内部名 = 对外名      ← 用户视角"这个模型对外叫什么"
//
// 这两处之间要转置两次（进、出各一次）。转置写反是**静默**的：
// 保存成功、列表也显示、后端也存下了 —— 只是调用方拿到的名字对不上，
// 表现为"我照着界面写的名字发请求，网关说不认识这个模型"。
//
// 语法门禁（verify_inline_script_syntax.js）完全抓不到这类错。
// 所以单独用真跑函数的方式断言转置结果。
//
// # 做法
//
// 从 webui.html 里把 `akModelMapToText` / `akParseModelMap` 两个函数体抠出来，
// 用 new Function 在一个干净的沙箱里跑，直接断言输入输出。
'use strict';

const fs = require('fs');
const path = require('path');

const htmlPath = path.join(__dirname, '..', '..', 'internal', 'server', 'webui.html');
const src = fs.readFileSync(htmlPath, 'utf8');

let fails = 0;
const fail = (m) => { console.log('FAIL ' + m); fails++; };
const pass = (m) => console.log('PASS ' + m);

// ── 抠函数体 ──
//
// 用"从 function 名开始、到成对花括号闭合"的方式截取，不用正则硬匹配多行 ——
// 那对嵌套花括号不成立。
function extractFn(name) {
  const marker = 'function ' + name + '(';
  const start = src.indexOf(marker);
  if (start < 0) return null;
  let i = src.indexOf('{', start);
  if (i < 0) return null;
  let depth = 0;
  for (; i < src.length; i++) {
    const ch = src[i];
    if (ch === '{') depth++;
    else if (ch === '}') {
      depth--;
      if (depth === 0) return src.slice(start, i + 1);
    }
  }
  return null;
}

const fnToText = extractFn('akModelMapToText');
const fnParse = extractFn('akParseModelMap');

if (!fnToText || !fnParse) {
  fail('找不到 akModelMapToText / akParseModelMap（函数被改名或删了？）');
  console.log('\n=== 失败 ===');
  process.exit(1);
}

let toText, parse;
try {
  // eslint-disable-next-line no-new-func
  toText = new Function(fnToText + '\nreturn akModelMapToText;')();
  // eslint-disable-next-line no-new-func
  parse = new Function(fnParse + '\nreturn akParseModelMap;')();
} catch (e) {
  fail('函数体无法求值: ' + e.message);
  console.log('\n=== 失败 ===');
  process.exit(1);
}

// ── ① 存储 → 界面：{对外名: 内部名} 应变成 "内部名 = 对外名" ──
{
  const stored = { 'deepseek-v4-pro': 'csg/deepseek-v4.1-flash-1017' };
  const text = toText(stored);
  if (text !== 'csg/deepseek-v4.1-flash-1017 = deepseek-v4-pro') {
    fail('★ akModelMapToText 转置方向错了\n     期望: csg/deepseek-v4.1-flash-1017 = deepseek-v4-pro\n     实际: ' + text);
  } else {
    pass('① 存储 → 界面：内部名 = 对外名');
  }
}

// ── ② 界面 → 存储：反向转置 ──
{
  const text = 'csg/deepseek-v4.1-flash-1017 = deepseek-v4-pro';
  const { map, invalid } = parse(text);
  if (invalid.length) {
    fail('★ 合法输入被判非法: ' + JSON.stringify(invalid));
  } else if (map['deepseek-v4-pro'] !== 'csg/deepseek-v4.1-flash-1017') {
    fail('★ akParseModelMap 转置方向错了\n     期望 {\'deepseek-v4-pro\': \'csg/deepseek-v4.1-flash-1017\'}\n     实际 ' + JSON.stringify(map));
  } else {
    pass('② 界面 → 存储：{对外名: 内部名}');
  }
}

// ── ③ 往返一致（最有力的一条）──
{
  const stored = {
    'pro': 'csg/a',
    'lite': 'workbuddy/b',
  };
  const round = parse(toText(stored)).map;
  const keys = Object.keys(round).sort();
  if (keys.join(',') !== 'lite,pro' || round.pro !== 'csg/a' || round.lite !== 'workbuddy/b') {
    fail('★ 往返不一致：' + JSON.stringify(stored) + ' → ' + JSON.stringify(round));
  } else {
    pass('③ 往返一致（存储 → 文本 → 存储 不变形）');
  }
}

// ── ④ 容错：全角＝、多余空格、注释行、空行 ──
{
  const text = [
    '# 这一行是注释',
    '',
    '  csg/a   ＝   pro-1  ',
    'workbuddy/b=p2',
    '   ',
  ].join('\n');
  const { map, invalid } = parse(text);
  if (invalid.length) {
    fail('★ 容错失败（合法行被判非法）: ' + JSON.stringify(invalid));
  } else if (map['pro-1'] !== 'csg/a' || map['p2'] !== 'workbuddy/b') {
    fail('★ 容错解析结果不对: ' + JSON.stringify(map));
  } else {
    pass('④ 容错：全角＝ / 空格 / 注释 / 空行');
  }
}

// ── ⑤ 坏输入必须被**报出来**，不能静默丢 ──
//
// ⚠ 这条比"能解析"更重要：静默丢掉一行，用户会以为设上了，
//   而调用方拿到的名字对不上 —— 又是那个"保存成功但没生效"的形态。
{
  const bad = parse('没有等号的一行\n只= \n=只有右边');
  if (bad.invalid.length !== 3) {
    fail('★ 坏输入应全部报出来（期望 3 条），实际 ' + bad.invalid.length + ' 条: ' + JSON.stringify(bad.invalid));
  } else {
    pass('⑤ 坏输入全部进 invalid（不静默丢弃）');
  }
}

// ── ⑥ 键值相同 ⇒ 不产生映射（与后端 normalizeModelMap 一致）──
{
  const { map } = parse('csg/same = csg/same');
  if (Object.keys(map).length !== 0) {
    fail('★ 键值相同应被省略，实际: ' + JSON.stringify(map));
  } else {
    pass('⑥ 键值相同 ⇒ 不产生映射（与后端同一判据）');
  }
}

if (fails > 0) {
  console.log('\n=== 失败 ' + fails + ' 项 ===');
  process.exit(1);
}
console.log('\n=== 全部通过 ===');
