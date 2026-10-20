// tests/frontend/verify_toggle_state_source.js —— 守住「按钮状态必须来自数据，不能来自自己改过的文案」
//
// # 这个 bug（2026-10-21，用户报「这个 apikey 启用禁用有 bug」）
//
// 原写法：
//
//	await busyRun(b, async () => {
//	  const on = b.textContent === '启用';        // ← 此时文案已经是 '…'
//	  await admin('/admin/apikeys/toggle', { body: JSON.stringify({ id, enabled: on }) });
//	}, { label: '切换', busyText: '…' });
//
// `busyRun` 一进入就把按钮文案改成 `busyText`，**然后**才调 fn。
// 所以 fn 里读到的 textContent 恒为 `'…'`，`'…' === '启用'` 永远 false
// ⇒ 永远发送 `enabled: false` ⇒ **只能禁用、永远启用不了**。
//
// 用户看到的是：状态「禁用」、按钮「启用」，点一下什么也没变。
//
// # 这个门禁守什么
//
//  ① toggle 分支**不能**用 `textContent` / `innerText` 判断业务状态
//  ② toggle 分支必须从 `_apikeysCache` 这类**数据源**派生目标状态
//  ③ 目标状态必须是 `!cur.enabled`（取反），不是字面量
//
// ⚠ 为什么不只看第 ① 条：把 `textContent` 换成 `b.dataset.enabled` 也能修好，
//   但渲染时那个属性同样可能被 busyRun 之外的代码改。用数据源是唯一稳的。
'use strict';

const fs = require('fs');
const path = require('path');

const htmlPath = path.join(__dirname, '..', '..', 'internal', 'server', 'webui.html');
const src = fs.readFileSync(htmlPath, 'utf8');

let fails = 0;
const fail = (m) => { console.log('FAIL ' + m); fails++; };
const pass = (m) => console.log('PASS ' + m);

// ── 定位 apiKeyRows 的 click 处理段 ──
const anchorStart = src.indexOf("$('apiKeyRows').addEventListener('click'");
if (anchorStart < 0) {
  fail('找不到 apiKeyRows 的 click 处理（被改名或删了？）');
  console.log('\n=== 失败 ===');
  process.exit(1);
}
// 截到下一个顶层 addEventListener 或下一个 `function ` 之前
const rest = src.slice(anchorStart);
const endCandidates = [
  rest.indexOf("\n  $('apiKeyRows')", 10),
  rest.indexOf("\n  function ", 10),
];
const end = Math.min(...endCandidates.filter((n) => n > 0).concat([8000]));
const seg = rest.slice(0, end);

// ── 切出 toggle 分支 ──
const toggleIdx = seg.indexOf("if (act === 'toggle')");
if (toggleIdx < 0) {
  fail('找不到 toggle 分支');
  console.log('\n=== 失败 ===');
  process.exit(1);
}
const nextBranch = seg.indexOf("if (act === 'edit')", toggleIdx);
const toggleSeg = seg.slice(toggleIdx, nextBranch > 0 ? nextBranch : toggleIdx + 1500);

// ⚠ 先剥掉注释再检查。
//
// 第一版直接扫原文，结果**误报**：修 commit 的注释里引用了那行坏代码
// （`//   const on = b.textContent === '启用';  // ← 此时已经不是「启用」了`）
// 作对照 —— 判据把注释里的示例当成了真代码。
//
// 这是检查"代码里有没有某模式"时最常踩的坑：**注释里写的东西也是文本**。
// 剥注释用最简单可靠的办法：逐行丢掉以 // 开头的与 /* */ 块内的行。
const stripComments = (s) => {
  const out = [];
  let inBlock = false;
  for (const line of s.split('\n')) {
    let t = line;
    if (inBlock) {
      const close = t.indexOf('*/');
      if (close < 0) continue;
      t = t.slice(close + 2);
      inBlock = false;
    }
    // 行内块注释的起止（同一行内闭合的直接摘掉）
    t = t.replace(/\/\*[\s\S]*?\*\//g, '');
    const open = t.indexOf('/*');
    if (open >= 0) {
      t = t.slice(0, open);
      inBlock = true;
    }
    // 行注释：⚠ 不能简单按 `//` 切 —— 字符串里可能有 `//`（如 URL）。
    // 本段代码里唯一可能出现的是注释，简单处理即可，但保留引号内的内容。
    let cut = -1;
    let q = null;
    for (let i = 0; i < t.length; i++) {
      const ch = t[i];
      if (q) {
        if (ch === q && t[i - 1] !== '\\') q = null;
        continue;
      }
      if (ch === "'" || ch === '"' || ch === '`') { q = ch; continue; }
      if (ch === '/' && t[i + 1] === '/') { cut = i; break; }
    }
    if (cut >= 0) t = t.slice(0, cut);
    out.push(t);
  }
  return out.join('\n');
};

const toggleCode = stripComments(toggleSeg);

// ── ① 不能用按钮文案判断状态 ──
if (/\.textContent\s*===|\.innerText\s*===|\.textContent\s*==\s*['"]|\.innerText\s*==\s*['"]/.test(toggleCode)) {
  const m = toggleCode.match(/[^\n]*(?:textContent|innerText)\s*===?[^\n]*/);
  fail('★ toggle 分支用按钮文案判断状态：' + (m ? m[0].trim() : ''));
  console.log('     ⇒ busyRun 一进入就会改文案（busyText），所以这个判断恒为 false，');
  console.log('       toggle 只会朝一个方向走（实测：永远禁用）。');
} else {
  pass('① toggle 分支不用按钮文案判断状态');
}

// ── ② 必须从数据源派生 ──
if (!/_apikeysCache/.test(toggleCode)) {
  fail('★ toggle 分支没有从 _apikeysCache 取当前状态');
} else {
  pass('② toggle 分支从 _apikeysCache 取当前状态');
}

// ── ③ 目标状态必须是对当前状态的取反 ──
if (!/const\s+on\s*=\s*!/.test(toggleCode)) {
  fail("★ 目标状态不是 `const on = !<当前状态>` 的形式");
} else {
  pass('③ 目标状态 = 当前状态的取反');
}

// ── ④ 缓存里找不到时不能"猜"（猜反了会把用户想启用的再禁一次）──
if (!/if\s*\(\s*!cur\s*\)/.test(toggleCode)) {
  fail('★ 找不到 cur 时没有早退 —— 会拿 undefined 去取反（得到 true），猜错方向');
} else {
  pass('④ 找不到 cur 时早退，不猜');
}

// ── ⑤ 回归守卫：busyRun 确实会在调 fn 之前改文案 ──
//
// 这条不是断言 toggle 的代码，而是断言**这个前提仍然成立** ——
// 若哪天 busyRun 改成"结束时才改文案"，那第 ①②③ 条的理由就变了，
// 注释里那段解释会变成误导（比没有注释更糟）。
const busyIdx = src.indexOf('async function busyRun(');
if (busyIdx < 0) {
  fail('找不到 busyRun');
} else {
  const busySeg = src.slice(busyIdx, busyIdx + 2000);
  const setTextIdx = busySeg.search(/btn\.textContent\s*=/);
  const callFnIdx = busySeg.search(/await\s+fn\s*\(|const\s+\w+\s*=\s*await\s+fn\s*\(/);
  if (setTextIdx < 0) {
    fail('busyRun 里找不到 `btn.textContent =`（前提已变，请复核 toggle 的注释）');
  } else if (callFnIdx < 0) {
    // 找不到 fn 的调用点，无法判断顺序 —— 不算失败，但要提醒
    console.log('NOTE ⑤ 无法定位 busyRun 内 fn 的调用点，跳过顺序断言');
  } else if (setTextIdx > callFnIdx) {
    fail('★ busyRun 现在**先**调 fn **后**改文案 —— 与 toggle 注释里写的前提不符，请复核');
  } else {
    pass('⑤ busyRun 确实先改文案再调 fn（toggle 不能用文案判断状态的前提仍成立）');
  }
}

if (fails > 0) {
  console.log('\n=== 失败 ' + fails + ' 项 ===');
  process.exit(1);
}
console.log('\n=== 全部通过 ===');
