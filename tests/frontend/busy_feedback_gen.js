
const fs = require('fs');
const html = require('./lib_frontend_source').FRONTEND;
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
  const TOAST_MAX = 5;   // 一个框里最多列几条（更旧的折叠成「…还有 N 条」）
  const el = $('toast');
  if (!el) return;
  // 外部把框清空了（探针的 __CLEAR_TOAST__、别的代码）⇒ 队列也当清空，
  // 免得下一句提示把上一次的旧行又画回来。
  if (el.hidden && !el.innerHTML) el._lines = [];
  const lines = el._lines || (el._lines = []);
  const text = String(msg == null ? '' : msg);
  const ttl = bad ? 8000 : 5000;   // 错误留久一点：它更值得看清
  const last = lines[lines.length - 1];
  if (last && last.text === text && last.bad === !!bad) {
    last.n += 1;                   // 连续同文案 ⇒ 只加计数，不堆行
    last.deadline = Date.now() + ttl;
  } else {
    lines.push({ text: text, bad: !!bad, n: 1, deadline: Date.now() + ttl });
  }

  // renderToast 嵌在 toast **内部**：门禁只抽出 `function toast(` 这一个块去跑，
  // 独立函数在那里是 undefined、DOM API 也都没有。
  const renderToast = () => {
    const now = Date.now();
    const all = (el._lines || []).filter((l) => l.deadline > now);
    el._lines = all;
    const shown = all.slice(-TOAST_MAX);        // 队列尾部是最新的
    const dropped = all.length - shown.length;  // 被折叠掉的更旧的那些
    let html = '';
    let anyBad = false;
    for (let i = shown.length - 1; i >= 0; i--) {  // 倒序 ⇒ 最新在最上
      const l = shown[i];
      anyBad = anyBad || l.bad;
      html += '<div class="toastline ' + (l.bad ? 'bad' : 'ok') +
        '" data-i="' + (all.length - shown.length + i) + '" title="点击关闭这一条">' +
        esc(l.text) + (l.n > 1 ? '<b class="toastn">×' + l.n + '</b>' : '') + '</div>';
    }
    if (dropped > 0) html += '<div class="toastmore">…还有 ' + dropped + ' 条</div>';
    el.innerHTML = html;
    // 类名与旧实现同一口径（门禁 gen_busy_feedback.js 断言 includes('bad')）
    el.className = 'notice ' + (anyBad ? 'bad' : 'ok');
    el.hidden = shown.length === 0;
    if (!el._bound) {
      el._bound = true;   // 只绑一次：toast 会被调很多次
      el.onclick = (ev) => {
        const box = (ev && ev.target && ev.target.closest) ? ev.target.closest('.toastline') : null;
        if (!box || !el._lines) return;
        const i = Number(box.getAttribute('data-i'));
        if (isFinite(i) && i >= 0 && i < el._lines.length) el._lines.splice(i, 1);
        renderToast();
      };
    }
    clearTimeout(toast._t);
    if (shown.length) {
      const next = Math.min.apply(null, shown.map((l) => l.deadline));
      toast._t = setTimeout(renderToast, Math.max(100, next - Date.now() + 20));
    }
  };
  renderToast();
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
    // 进度文案：默认"正在刷新界面"，但**等后台任务**的调用方必须能覆盖它。
    //
    // 为什么：`growthOneClick`（一键完成）等的是"成长待办后台跑完"，
    // 单账号实测约 15s（专家链 8 次 × 6s 节流 + 事件上报间隔 1.05s），
    // 4 个账号串行约 1 分钟。而默认文案写的是"正在刷新界面" ——
    // 用户看到的是一句与事实无关的话，自然读成"卡住了 / 这功能坏了"。
    // 传 progress 后显示"正在等待上游执行"，等待性质一目了然。
    const progress = o.progress || '正在刷新界面';
    const render = () => {
      const sec = Math.floor((Date.now() - t0) / 1000);
      g.textContent = sec >= slowSec
        ? `${label}：${progress}…（上游较慢，已等 ${sec}s）`
        : `${label}：${progress}…`;
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
