
const fs = require('fs');
const html = fs.readFileSync("D:\\project_GIT\\workbuddy2api实验版本/internal/server/webui.html", 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '', onclick: null }; }
for (const id of ['toast','gtask','busy','notice','noticeClose']) els[id] = mkEl(id);
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
function reset() { for (const k in els) { const e = els[k]; e.hidden = true; e.className = ''; e.textContent = ''; e.innerHTML = ''; e.disabled = false; } }
// busyRun 依赖的模块级常量（从页面真实源码取，避免与实现漂移）
const SLOW_HINT_MS = Number(/const SLOW_HINT_MS = (\d+)/.exec(html)[1]);
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

function toast(msg, bad) {
    const el = $('toast');
    el.className = 'notice ' + (bad ? 'bad' : 'ok');
    el.textContent = msg;
    el.hidden = false;
    clearTimeout(toast._t);
    toast._t = setTimeout(() => { el.hidden = true; }, 5000);
  }

function notice(title, body) {
    const el = $('notice');
    el.innerHTML =
      '<div style="display:flex;align-items:baseline;gap:8px">' +
        '<b style="color:var(--warn)">' + esc(title) + '</b>' +
        '<span class="spacer" style="flex:1"></span>' +
        '<button id="noticeClose" style="font-size:11px;padding:1px 8px">知道了</button>' +
      '</div>' +
      '<div style="margin-top:6px;line-height:1.7;white-space:pre-wrap">' + esc(body) + '</div>';
    el.hidden = false;
    const btn = $('noticeClose');
    if (btn) btn.onclick = () => { el.hidden = true; };
  }

async function busyRun(btn, fn, opts) {
    const o = opts || {};
    // 连点保护：进行中就忽略。这比 disabled 属性更可靠 ——
    // 事件委托场景下按钮是重建出来的，disabled 可能还没来得及生效。
    if (btn && btn._busy) return undefined;
    if (btn) {
      btn._busy = true;
      btn.disabled = true;
    }
    const text0 = btn ? btn.textContent : '';
    // 按钮文案一定要变：只置 disabled 在视觉上几乎看不出来（尤其是深色主题下
    // 只是稍微变淡），用户仍会觉得"点了没反应"。
    // 未显式给 busyText 时按原标题推导一个 —— '领取' → '领取中…'。
    if (btn) btn.textContent = o.busyText || (text0 ? text0 + '中…' : '处理中…');

    // 用专用的 #busy，**不共用 #gtask** —— 后者是后台全量任务的进度，
    // 两者可能同时存在（点了「全部签到」之后再点任一账号的「签到」）。
    // 共用一个元素时，busy 的收尾会把全量任务的进度条一起隐藏掉。
    const g = $('busy');
    const label = o.label || text0 || '操作';
    const t0 = Date.now();
    // 阈值从 SLOW_HINT_MS 推导，不写字面量 2 —— 否则改常量时这里会悄悄漂移。
    const slowSec = Math.ceil(SLOW_HINT_MS / 1000);
    const render = () => {
      const sec = Math.floor((Date.now() - t0) / 1000);
      g.textContent = sec >= slowSec
        ? `${label}：正在刷新界面…（上游较慢，已等 ${sec}s）`
        : `${label}：正在刷新界面…`;
    };
    render();
    g.hidden = false;
    // 每 500ms 刷新一次秒数；快请求会在第一次 tick 前就被清掉，因此不会有噪音。
    const tick = setInterval(render, 500);
    const slowHint = setTimeout(render, SLOW_HINT_MS);

    try {
      return await fn();
    } catch (e) {
      toast(`${label}失败：${(e && e.message) || e}`, true);
      return false;
    } finally {
      clearInterval(tick);
      clearTimeout(slowHint);
      g.hidden = true;
      g.textContent = '';
      if (btn) {
        btn._busy = false;
        btn.disabled = false;
        btn.textContent = text0;   // 总是恢复：上面总是改过
      }
    }
  }

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
