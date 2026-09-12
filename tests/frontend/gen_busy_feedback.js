// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：操作后前端自动刷新的可见反馈（Task 1 + Task 2）。
//
// 现场问题：点操作按钮后 6.9 秒内页面完全静止，用户判定"没刷新"。
// 根因：growthAct/travelAct/act 末尾裸调 loadGrowth(true) 等，既不 await、
// 也不给按钮 busy 态、也没有全局进度提示。
//
// 本测试断言：
//   A. 统一的「操作 → 刷新」编排函数存在且被所有写操作复用（单一实现）
//   B. 刷新期间按钮 disabled + 文案变化；结束后恢复
//   C. 刷新期间出现全局进度提示；结束后消失
//   D. 异常路径也要恢复（不能永久禁用）
//   E. 超过 2s 才补「上游较慢」文案（不给快请求加噪音）
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(WEBUI, 'utf8');

function grabBlock(startMarker) {
  // 从标记处起找 '{' 再配平到对应 '}'。
  // 注意：grabBlock('function foo(') 会**丢掉前面的 async 关键字**，
  // 抽出的是 `function foo() {...}`，用到 await 就会 SyntaxError。
  // 所以调用方在需要 async 时要显式把标记写成 'async function foo(' 并向左扩展。
  const idx = html.indexOf(startMarker);
  if (idx < 0) throw new Error('找不到: ' + startMarker);
  // 若标记前紧邻 "async " 且标记本身以 "function" 开头，把 async 一起带上。
  let start = idx;
  if (/^function\s/.test(startMarker)) {
    const pre = html.slice(Math.max(0, idx - 6), idx);
    if (pre === 'async ') start = idx - 6;
  }
  let i = html.indexOf('{', idx), depth = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}') { depth--; if (depth === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + startMarker);
}

const pieces = [
  grabBlock('function toast('),
  grabBlock('function notice('),
  grabBlock('function busyRun('),
];

const header = `
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '', onclick: null }; }
for (const id of ['toast','gtask','busy','notice','noticeClose']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
function reset() { for (const k in els) { const e = els[k]; e.hidden = true; e.className = ''; e.textContent = ''; e.innerHTML = ''; e.disabled = false; } }
// busyRun 依赖的模块级常量（从页面真实源码取，避免与实现漂移）
const SLOW_HINT_MS = Number(/const SLOW_HINT_MS = (\\d+)/.exec(html)[1]);
// 生成物里也要能按标记抽取源码块（用于断言「busyRun 不引用 #gtask」这类结构约束）
function grabBlock(marker) {
  const start = html.indexOf(marker);
  if (start < 0) throw new Error('找不到: ' + marker);
  let i = html.indexOf('{', start), depth = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}') { depth--; if (depth === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// ---------- A. 单一实现 ----------
console.log('\n[A] 统一的「操作 → 刷新」编排');
ok(typeof busyRun === 'function', 'busyRun 存在（统一编排函数）');
const callSites = (html.match(/busyRun\(/g) || []).length;
ok(callSites >= 3, 'busyRun 至少 1 处定义 + 2 处调用（实际 ' + callSites + ' 处）');
// 反证：旧的裸调用模式不应再出现在写操作路径
const barePattern = /\n\s{4}loadGrowth\(true\);\n\s{4}refresh\(\);\n\s{4}loadHistory\(\);/;
ok(!barePattern.test(html), '旧的裸 loadGrowth(true)+refresh()+loadHistory() 组合已移除');

// ---------- B. 按钮 busy 态 ----------
console.log('\n[B] 按钮 busy 态');
(async () => {
  reset();
  const btn = mkEl('b1'); btn.textContent = '领取';
  let sawDisabled = null, sawText = null;
  await busyRun(btn, async () => {
    sawDisabled = btn.disabled;
    sawText = btn.textContent;
    return true;
  });
  ok(sawDisabled === true, '执行期间按钮被禁用');
  ok(sawText && sawText !== '领取', '执行期间按钮文案变化（实际：' + sawText + '）');
  ok(btn.disabled === false, '完成后按钮恢复可用');
  ok(btn.textContent === '领取', '完成后按钮文案恢复');

  // 连点保护
  reset();
  const btn2 = mkEl('b2'); btn2.textContent = '领取';
  let runs = 0;
  const slow = () => new Promise(r => setTimeout(() => { runs++; r(true); }, 60));
  const p1 = busyRun(btn2, slow);
  await busyRun(btn2, slow);   // 第二次应被忽略
  await p1;
  ok(runs === 1, '进行中再次点击被忽略（实际执行 ' + runs + ' 次）');

  // ---------- C. 全局进度提示 ----------
  console.log('\n[C] 全局进度提示');
  reset();
  const btn3 = mkEl('b3'); btn3.textContent = '刷新';
  let duringHidden = null, duringText = '';
  await busyRun(btn3, async () => {
    duringHidden = $('busy').hidden;
    duringText = $('busy').textContent;
    return true;
  });
  ok(duringHidden === false, '刷新期间全局进度可见');
  ok(/刷新|处理|进行/.test(duringText), '进度文案说明在做什么（实际：' + duringText + '）');
  ok($('busy').hidden === true, '刷新结束后进度提示消失');

  // ---------- C2. 不与后台任务进度共用元素（回归） ----------
  //
  // 曾经的实现把 busy 进度写进 #gtask，而 #gtask 同时被 watchTask() 用来显示
  // 后台全量任务的进度。两者可能同时存在（点了「全部签到」后再点任一账号的
  // 「签到」），于是 busy 的收尾会把还在跑的任务进度条一并隐藏。
  console.log('\n[C2] 与后台任务进度互不干扰');
  ok(html.includes('id="busy"'), '存在专用的 #busy 元素');
  // 直接查「busyRun 函数体里有没有引用 #gtask」——比匹配赋值语句更稳，
  // 也更能表达意图：busy 的实现**不得**碰 gtask。
  const busySrc = grabBlock('function busyRun(');
  ok(busySrc.includes("$('busy')"), 'busyRun 引用 #busy');
  ok(!busySrc.includes("$('gtask')"), 'busyRun **不**引用 #gtask（否则会擦掉后台任务进度）');
  // 场景：后台任务正在跑 → 点一个按钮 → 按钮流程结束后，后台任务进度必须还在
  reset();
  $('gtask').hidden = false;
  $('gtask').textContent = 'checkin 执行中… 已用 5s';   // 模拟 watchTask 的输出
  const btnB = mkEl('bB'); btnB.textContent = '签到';
  await busyRun(btnB, async () => true);
  ok($('gtask').hidden === false && /执行中/.test($('gtask').textContent),
    '按钮流程结束后，后台任务进度未被擦除（实际：' + $('gtask').textContent + '）');

  // ---------- D. 异常路径 ----------
  console.log('\n[D] 异常路径');
  reset();
  const btn4 = mkEl('b4'); btn4.textContent = '领取';
  let threw = false;
  try { await busyRun(btn4, async () => { throw new Error('boom'); }); } catch { threw = true; }
  ok(threw === false, '异常被吞掉不外抛（调用方不需要 try）');
  ok(btn4.disabled === false, '异常后按钮恢复可用（不会永久禁用）');
  ok(btn4.textContent === '领取', '异常后文案恢复');
  ok($('busy').hidden === true, '异常后进度提示也消失（不残留）');
  ok($('toast').hidden === false && $('toast').className.includes('bad'), '异常给出红色提示');

  // fn 返回 false 也应恢复
  reset();
  const btn5 = mkEl('b5'); btn5.textContent = '领取';
  await busyRun(btn5, async () => false);
  ok(btn5.disabled === false && btn5.textContent === '领取', '返回 false 时按钮同样恢复');

  // ---------- E. 慢请求才补文案 ----------
  console.log('\n[E] 「上游较慢」阈值');
  ok(/2000|2\s*\*\s*1000/.test(html), '存在 2 秒阈值常量');
  const slowRe = /上游较慢|仍在刷新|较慢/;
  ok(slowRe.test(html), '存在「较慢」提示文案');
  // 快路径不应出现该文案
  reset();
  const btn6 = mkEl('b6'); btn6.textContent = '刷新';
  await busyRun(btn6, async () => { return true; });   // 立即完成
  ok(!slowRe.test($('busy').textContent || ''), '快请求不出现「较慢」文案（无噪音）');

  console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
`;

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('busy_feedback_gen.js', out);
console.log('生成: busy_feedback_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
