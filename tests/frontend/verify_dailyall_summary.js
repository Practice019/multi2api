#!/usr/bin/env node
// verify_dailyall_summary.js —— 「全量每日动作」回执汇总的**行为**门禁。
//
// # 它守的是什么（用户 2026-11-01 报障）
//
//	「monkeycode 全部签到有问题 检查 同时检查其他的上游的同时签到」
//
// 查证结论：**后端没错，错在前端的汇总**。上游卡片头那个「全部签到」的旧实现是
//
//	const ok = r.results.filter(x => x.status === 'ok').length;
//	toast('全部' + label + '完成：成功 ' + ok + '/' + r.results.length)
//
// 而 monkeycode 26 个号当天**都已签到**（状态 already）⇒ 界面显示
// 「全部签到完成：成功 0/26」—— 用户当场判定"全部签到坏了"。
// 同一条形状还被 codearts / lobsterai / qoder / trae / minimax 五个上游共用。
//
// # 为什么判据落在 dailyAllSummary 上
//
// 它是纯函数（不碰 DOM、不碰网络），所以能直接把**真实回执**喂进去验；
// 而"界面上显示了什么"由此被钉住 —— 比在浏览器里点一次按钮稳得多
// （点击探针受卡片展开态、实例状态影响，实测两次跑出不同结果）。
//
// # 自带判别力验证（最后一段）
//
// 把"只数 ok"的旧实现喂给同一个用例，断言它**必须红** —— 否则这条门禁
// 只是"断言恒真"的摆设（本仓有条同类教训：恒真的断言比没有断言更糟）。
'use strict';
const fs = require('fs');
const path = require('path');

const REPO = process.env.WB2API_REPO || path.resolve(__dirname, '..', '..');
const PAGE = path.join(REPO, 'internal', 'server', 'webui.html');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// grabFunc 按标记抠出一个函数体（大括号配对；与其余探针同一做法）。
function grabFunc(src, marker) {
  const start = src.indexOf(marker);
  if (start < 0) throw new Error('找不到: ' + marker);
  let i = src.indexOf('{', start), depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) return src.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}

const html = fs.readFileSync(PAGE, 'utf8');

// ── ① 结构守卫（先看源码里还在不在"只数 ok"那条路上）────────────────────
const startAllBody = grabFunc(html, 'async function startAll(');
ok(startAllBody.includes('dailyAllSummary('),
  'startAll 走 dailyAllSummary 汇总（而不是自己数 ok）');
// ⚠ 判据前先剥掉整行注释：修法的注释里**故意**引用了那条错误写法（它要解释为什么），
//   不剥注释会把「解释」当成「代码」——本仓踩过同类（见 server 的 softrate 守卫）。
const startAllCode = startAllBody.split('\n')
  .filter((line) => !line.trim().startsWith('//'))
  .join('\n');
ok(!/status\s*===\s*['"]ok['"]/.test(startAllCode),
  'startAll 的**代码**里不再出现 status === "ok" 的计数（那正是把 already 数成失败的写法）');
ok(!/成功\s*'?\s*\+\s*ok\s*\+/.test(startAllBody) && !startAllBody.includes('成功 ${ok}'),
  'startAll 里不再拼「成功 N/M」的文案');

// ── ② 取出纯函数，喂真实回执 ─────────────────────────────────────────────
const fnSrc = grabFunc(html, 'function dailyAllSummary(');
const dailyAllSummary = new Function(fnSrc + '; return dailyAllSummary;')();

const mk = (n, status) => Array.from({ length: n }, () => ({ status: status, detail: '' }));

// 用例 1：**真实现场**（用户报障的那一次）：25 个已签 + 1 个凭证失效
const real = dailyAllSummary('签到', { ok: true, claimed: 0, results: mk(25, 'already').concat(mk(1, 'fail')) });
console.log('  真实现场 → ' + JSON.stringify(real));
ok(real.msg.includes('已签 25'), '把「今天已签到」如实报成 已签 25（实际：' + real.msg + '）');
ok(real.msg.includes('失败 1'), '真失败只数真失败（实际：' + real.msg + '）');
ok(real.bad === true, '有真失败时标红');
ok(!real.msg.includes('成功 0'), '★ 不再出现「成功 0」那种把 already 当失败的文案');

// 用例 2：本次真的签上了
const fresh = dailyAllSummary('签到', { ok: true, results: mk(3, 'ok').concat(mk(2, 'already')) });
console.log('  新签+已签 → ' + JSON.stringify(fresh));
ok(fresh.msg.includes('新签 3') && fresh.msg.includes('已签 2'), '新签与已签分开报（实际：' + fresh.msg + '）');
ok(fresh.bad === false, '全是成功语义时不标红');

// 用例 3：上游今天没开放（skip 不是失败）
const skipped = dailyAllSummary('签到', { ok: true, results: mk(2, 'skip') });
ok(skipped.msg.includes('跳过 2') && skipped.bad === false, 'skip 报成跳过、且不算失败（实际：' + skipped.msg + '）');

// 用例 4：一个账号都没有
const none = dailyAllSummary('签到', { ok: true, claimed: 0, results: [] });
ok(none.msg.includes('没有账号') && none.bad === false, '空结果说「没有账号」而不是「成功 0/0」（实际：' + none.msg + '）');

// 用例 5：领取类（codearts 福利领取）
const claim = dailyAllSummary('领取', { ok: true, claimed: 5, results: mk(5, 'ok') });
ok(claim.msg.includes('领取 5'), '领取数出现在文案里（实际：' + claim.msg + '）');

// 用例 6：未来新增的未知状态 —— 不许静默吞掉
const weird = dailyAllSummary('签到', { ok: true, results: [{ status: 'newthing' }] });
ok(weird.bad === true && weird.msg.includes('失败 1'),
  '未知状态保守计入失败（宁可多报，也不让"没签上"看起来没事）（实际：' + weird.msg + '）');

// ── ③ 判别力自带验证：把"只数 ok"的旧实现喂给用例 1，它必须红 ──────────
const oldImpl = function (label, r) {
  const okN = r.results.filter((x) => x.status === 'ok').length;
  return { msg: '全部' + label + '完成：成功 ' + okN + '/' + r.results.length, bad: false };
};
const oldOut = oldImpl('签到', { results: mk(25, 'already').concat(mk(1, 'fail')) });
ok(oldOut.msg.includes('成功 0/26'),
  '判别力验证：旧实现确实会输出「成功 0/26」（说明本门禁的用例 1 能抓住它）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
