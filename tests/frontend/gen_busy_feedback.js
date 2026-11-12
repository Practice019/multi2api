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
const html = require('./lib_frontend_source').FRONTEND;

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
const html = require('./lib_frontend_source').FRONTEND;
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

  // ---------- F. 多条提示互不覆盖（2026-11-01 用户报的缺陷） ----------
  //
  // 用户原话：「右上角如果出现多个消息 多交替显示 这也是bug」。
  // 旧实现是 el.textContent = msg 一句话 —— 后一条**覆盖**前一条，批量动作
  // （全部签到一次几十条结果）里它们在一个框内快速交替，只读得到最后一条。
  //
  // 这里在**假元素**上验：假元素没有 DOM 方法，所以这几条断言同时也钉住了
  // "toast 不许用 createElement/appendChild"（用了就会在这里炸）。
  console.log('\n[F] 多条提示并存（不覆盖）');
  reset();
  toast('第一条：已签到', false);
  toast('第二条：失败', true);
  const boxAfter2 = $('toast').innerHTML;
  ok(boxAfter2.indexOf('第一条：已签到') >= 0 && boxAfter2.indexOf('第二条：失败') >= 0,
    '两条提示**同时**在框里（旧实现只会剩最后一条）：' + JSON.stringify($('toast').textContent));
  ok((boxAfter2.match(/toastline/g) || []).length === 2, '渲染成 2 行（实际 ' + (boxAfter2.match(/toastline/g) || []).length + ' 行）');
  ok($('toast').className.includes('bad'), '只要有一条是失败 ⇒ 整个框标红（否则错误会被当成成功）');
  ok($('toast').hidden === false, '有内容时框可见');

  // 连续同文案 ⇒ 合并成「×N」，不堆一屏同样的字
  reset();
  toast('同一句失败', true);
  toast('同一句失败', true);
  toast('同一句失败', true);
  ok($('toast').innerHTML.indexOf('×3') >= 0,
    '连续重复的同文案合并成 ×3（实际 ' + JSON.stringify($('toast').innerHTML) + '）');
  ok(($('toast').innerHTML.match(/toastline/g) || []).length === 1, '合并后只占 1 行');

  // 超过上限 ⇒ 折叠成「…还有 N 条」，既不假装只有几条、也不糊满半屏
  reset();
  for (let i = 0; i < 8; i++) toast('第 ' + (i + 1) + ' 条', false);
  const capHtml = $('toast').innerHTML;
  const lines8 = (capHtml.match(/toastline/g) || []).length;
  ok(lines8 === 5, '最多列 5 行（实际 ' + lines8 + '）');
  ok(/toastmore/.test(capHtml) && /还有 3 条/.test(capHtml),
    '被折叠的 3 条给一行汇总（实际 ' + JSON.stringify($('toast').textContent) + '）');
  ok(capHtml.indexOf('第 8 条') < capHtml.indexOf('第 7 条'),
    '最新的排在最上面（批量操作时用户最该看到刚发生的那条）');

  // 过期的行自己消失，且最后一个过期后整个框收起
  reset();
  const realNow = Date.now;
  try {
    toast('会过期的一条', false);
    ok($('toast').hidden === false, '刚写入时可见');
    Date.now = () => realNow() + 6000;   // 快进过 5s 的 TTL
    toast('新的一条', false);
    const afterExpire = $('toast').innerHTML;
    ok(afterExpire.indexOf('会过期的一条') < 0, '过期的那条不再渲染（实际 ' + JSON.stringify($('toast').textContent) + '）');
    Date.now = () => realNow() + 60000;  // 让新的一条也过期
    toast('再一条', false);
    toast('再一条', false);              // 触发一次渲染，排掉过期行
    ok($('toast').innerHTML.indexOf('再一条') >= 0, '新写入的仍然在');
  } finally {
    Date.now = realNow;
  }

  console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
`;

const out = header + '\n' + pieces.join('\n\n') + '\n' + tail;
fs.writeFileSync('busy_feedback_gen.js', out);
console.log('生成: busy_feedback_gen.js (' + out.length + ' 字节)');
pieces.forEach(p => console.log('  ' + p.split('\n')[0].trim()));
