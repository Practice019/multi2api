// toast.js —— 通知框的**唯一实现**（外壳控制台 + 令牌页共用）。
//
// # 为什么抽成共享层（2026-11-01）
//
// 用户报「右上角如果出现多个消息 多交替显示 这也是 bug」之后，外壳改成了**堆叠式**
// （逐条列出、各自计时、重复文案合并成 ×N、超过上限折叠成「…还有 N 条」）。
// 但令牌页还留着旧实现：
//
//     el.textContent = msg;  el.className = 'toast on' + (bad ? ' bad' : '');
//     setTimeout(() => el.className = 'toast', 2000);
//
// ⇒ 同一个产品里两种通知语义：令牌页批量轮换一次发十几条结果时，用户仍然只读得到最后一条
//（与外壳那个 bug 一模一样的症状，只是发生在另一个页面）。
//
// # 契约（宿主页面必须提供两样东西）
//
//   $('toast')   —— 通知容器（id="toast"），样式在主题块里（#toast .toastline 等，
//                   由 theme_sync 同步到所有页面，所以视觉本来就是统一的）
//   esc(s)       —— HTML 转义（各页自带一份，实现略有差异，故不在这里重复定义）
//
// # 两条实现约束（照抄外壳那份的踩坑记录，别改）
//
//   ① 只用 el.innerHTML 拼字符串，**不用** createElement/appendChild ——
//      tests/frontend/gen_busy_feedback.js 会把这个函数抽出来跑在**假元素**上
//      （只有 textContent/className/innerHTML/hidden），DOM API 会直接炸。
//   ② 只引用**函数体内**的东西：队列挂在元素上（el._lines），上限写成函数内常量。
//      模块级变量在抽出来的那份里不存在 ⇒ ReferenceError（第一版踩过）。
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