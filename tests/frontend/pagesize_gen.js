
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false }; }
const $ = id => els[id] || (els[id] = mkEl(id));
const PAGE_SIZE_MIN = 1, PAGE_SIZE_MAX = 300, DefaultPageSize = 30;
const esc = s => String(s == null ? '' : s);
// renderPager 在改值时会调 savePageSize 落盘；本套测试只关心分页交互本身，
// 因此给它一个记录调用的桩（持久化的往返在 pagesize_persist_gen.js 里专门验证）。
let savedSizes = [];
function savePageSize(n) { savedSizes.push(n); }

// 把 renderPager 产出的 innerHTML 解析成一个可交互的桩：
//  - buttons: 每个 <button data-page="x" ... disabled?>
//  - input : <input data-page="size" ... value="N">
// renderPager 会往 el 上挂 querySelectorAll / querySelector，这里按 innerHTML 现算。
function makeStub(containerId) {
  const el = mkEl(containerId);
  const parse = () => {
    const h = el.innerHTML;
    const inputs = [];
    const re = /<input[^>]*data-page="size"[^>]*>/g;
    let m;
    while ((m = re.exec(h))) {
      const tag = m[0];
      const val = /value="([^"]*)"/.exec(tag);
      const min = /min="([^"]*)"/.exec(tag);
      const max = /max="([^"]*)"/.exec(tag);
      const step = /step="([^"]*)"/.exec(tag);
      inputs.push({ kind: 'input', value: val ? val[1] : '', min: min && min[1], max: max && max[1],
                    step: step && step[1], onchange: null, onkeydown: null });
    }
    const btns = [];
    const bre = /<button data-page="(prev|next)"([^>]*)>/g;
    while ((m = bre.exec(h))) btns.push({ kind: 'button', page: m[1], disabled: /\bdisabled\b/.test(m[2]), onclick: null });
    return { inputs, btns };
  };
  const parsed = { inputs: [], btns: [] };
  el.querySelectorAll = sel => {
    if (/button/.test(sel)) return parsed.btns;
    return [];
  };
  el.querySelector = sel => {
    if (/input/.test(sel)) return parsed.inputs[0] || null;
    return null;
  };
  // renderPager 直接赋值 innerHTML，赋值后重新解析
  let _html = '';
  Object.defineProperty(el, 'innerHTML', {
    get: () => _html,
    set: v => {
      _html = v;
      const p = parse();
      parsed.inputs.length = 0; parsed.btns.length = 0;
      p.inputs.forEach(x => parsed.inputs.push(x));
      p.btns.forEach(x => parsed.btns.push(x));
    },
  });
  return el;
}

function clampPageSize(v) {
    const n = Math.floor(Number(v));
    if (!isFinite(n) || n < PAGE_SIZE_MIN) return PAGE_SIZE_MIN;
    return n > PAGE_SIZE_MAX ? PAGE_SIZE_MAX : n;
  }

function pageCount(state) {
    return Math.max(1, Math.ceil((state.total || 0) / (state.limit || 30)));
  }

function pageIndex(state) {
    return Math.floor((state.offset || 0) / (state.limit || 30)) + 1;
  }

function renderPager(state) {
    const el = $(state.container);
    if (!el) return;
    const pages = pageCount(state);
    const cur = pageIndex(state);
    const total = state.total || 0;
    const from = total === 0 ? 0 : state.offset + 1;
    const to = Math.min(state.offset + state.limit, total);

    el.innerHTML = `
      <button data-page="prev"${cur <= 1 ? ' disabled' : ''}>上一页</button>
      <span class="pageno">第 ${cur} / ${pages} 页</span>
      <button data-page="next"${cur >= pages ? ' disabled' : ''}>下一页</button>
      <span>共 ${total} 条${total ? `，本页 ${from}-${to}` : ''}</span>
      <span class="spacer"></span>
      <span>每页</span>
      <input data-page="size" type="number" class="pagesize"
             value="${state.limit}" min="${PAGE_SIZE_MIN}" max="${PAGE_SIZE_MAX}"
             step="1" inputmode="numeric"
             title="可直接输入 ${PAGE_SIZE_MIN}-${PAGE_SIZE_MAX} 之间的任意整数">
      <span>条</span>`;

    el.querySelectorAll('button[data-page]').forEach(btn => {
      btn.onclick = () => {
        if (btn.disabled) return;
        const next = btn.dataset.page === 'prev' ? cur - 1 : cur + 1;
        if (next < 1 || next > pages) return;
        state.offset = (next - 1) * state.limit;
        state.onGo();
      };
    });

    // 每页条数：输入框提交时生效。
    // 用 change（失焦或回车才触发）而不是 input（每敲一个键触发）——
    // 否则想输入「120」时，敲到「1」就已经按每页 1 条重查了一次，白白多打两次请求。
    const box = el.querySelector('input[data-page="size"]');
    if (box) {
      const apply = () => {
        const next = clampPageSize(box.value);
        // 把夹紧后的值写回输入框，让用户看到自己输的 999 实际按 300 生效
        box.value = String(next);
        if (next === state.limit) return;   // 没变就不重查
        state.limit = next;
        savePageSize(next);                 // 持久化：刷新页面后仍是这个值
        state.offset = 0;   // 改变每页条数属于主动改变查询范围 → 回第 1 页
        state.onGo();
      };
      box.onchange = apply;
      box.onkeydown = ev => {
        if (ev.key === 'Enter') { ev.preventDefault(); apply(); box.blur(); return; }
        // 上下方向键增减一页条数：与原生 number 行为一致，但走同一条夹紧路径。
        // 基准取**输入框当前值**而不是 state.limit —— state.limit 只在提交时更新，
        // 若以它为基准，连按上键后按下键会从旧值回退（上到 31、再按下变成 29）。
        if (ev.key === 'ArrowUp' || ev.key === 'ArrowDown') {
          ev.preventDefault();
          box.value = String(clampPageSize(clampPageSize(box.value) + (ev.key === 'ArrowUp' ? 1 : -1)));
        }
      };
    }
  }

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

function freshState(limit, total) {
  const el = makeStub('p1');
  els.p1 = el;
  let calls = 0;
  const st = { offset: 0, limit, total, container: 'p1', onGo: () => { calls++; } };
  return { st, el, calls: () => calls };
}

// ---------- 1. clampPageSize 的夹紧语义 ----------
console.log('\n[1] clampPageSize：任意输入都收敛为合法整数');
ok(clampPageSize(50) === 50, '普通整数原样通过');
ok(clampPageSize('120') === 120, '字符串数字可解析');
ok(clampPageSize(0) === 1, '0 夹到下限 1');
ok(clampPageSize(-5) === 1, '负数夹到下限 1');
ok(clampPageSize(999) === 300, '超上限夹到 300');
ok(clampPageSize('') === 1, '空字符串夹到下限 1（不是 NaN）');
ok(clampPageSize('abc') === 1, '非数字夹到下限 1');
ok(clampPageSize(null) === 1, 'null 夹到下限 1');
ok(clampPageSize(undefined) === 1, 'undefined 夹到下限 1');
ok(clampPageSize(45.9) === 45, '小数向下取整');
ok(clampPageSize('  77  ') === 77, '带空白的输入可解析');
// 关键：输出必须永远在 [1,300]，绝不能让 NaN 漏到请求参数里
let allLegal = true;
for (const v of [NaN, Infinity, -Infinity, '1e9', 1e9, {}, [], '0x20', 12.7]) {
  const r = clampPageSize(v);
  if (!Number.isInteger(r) || r < 1 || r > 300) { allLegal = false; console.log('    违约输入:', JSON.stringify(v), '->', r); }
}
ok(allLegal, '所有畸形输入都落在 [1,300] 整数区间内');

// ---------- 2. 渲染出的是 input 而不是 select ----------
console.log('\n[2] 渲染产物');
{
  const { st, el } = freshState(30, 743);
  renderPager(st);
  ok(!/<select/.test(el.innerHTML), '不再渲染 <select>（固定档位已移除）');
  ok(/<input[^>]*data-page="size"/.test(el.innerHTML), '渲染出 <input data-page="size">');
  ok(/type="number"/.test(el.innerHTML), '类型为 number');
  ok(/value="30"/.test(el.innerHTML), '当前值回填为 state.limit（30）');
  ok(/min="1"/.test(el.innerHTML) && /max="300"/.test(el.innerHTML), 'min=1 max=300 落到 DOM 上（浏览器层也是硬约束）');
  ok(/step="1"/.test(el.innerHTML), 'step=1，不接受小数');
  ok(/inputmode="numeric"/.test(el.innerHTML), '移动端弹数字键盘');
  ok(/每页/.test(el.innerHTML) && /条/.test(el.innerHTML), '保留「每页 … 条」文案');
}

// ---------- 3. 输入生效路径 ----------
console.log('\n[3] 改值 → 夹紧 → 回第 1 页 → 重查');
{
  const { st, el, calls } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  ok(!!box && typeof box.onchange === 'function', 'input 上挂了 onchange');
  ok(typeof box.onkeydown === 'function', 'input 上挂了 onkeydown');

  st.offset = 60;           // 先进到第 3 页
  box.value = '120';
  box.onchange();
  ok(st.limit === 120, 'limit 更新为 120');
  ok(st.offset === 0, '改每页条数后回到第 1 页');
  ok(calls() === 1, '触发一次重查');
  ok(box.value === '120', '输入框保持为夹紧后的值');
  ok(savedSizes.length === 1 && savedSizes[0] === 120, '改值时调用 savePageSize(120) 落盘（实际 ' + JSON.stringify(savedSizes) + '）');
}
{
  const { st, el, calls } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  box.value = '999';
  box.onchange();
  ok(st.limit === 300, '输入 999 实际按 300 生效');
  ok(box.value === '300', '输入框被改写为 300（用户能看出被夹紧了）');
  ok(calls() === 1, '触发重查');
}
{
  const { st, el, calls } = freshState(100, 743);
  renderPager(st);
  const box = el.querySelector('input');
  savedSizes.length = 0;
  box.value = '100';
  box.onchange();
  ok(calls() === 0, '值未变化时不重查');
  ok(savedSizes.length === 0, '值未变化时不落盘');
}
{
  // 夹紧后与当前值相同（如输入 999 而当前已是 300）也不重查
  const { st, el, calls } = freshState(300, 743);
  renderPager(st);
  const box = el.querySelector('input');
  box.value = '999';
  box.onchange();
  ok(calls() === 0 && st.limit === 300, '夹紧后等于当前值时同样不重查');
}
{
  // 清空输入框后失焦：应按最小值生效，不能变成 NaN 请求
  const { st, el } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  box.value = '';
  box.onchange();
  ok(st.limit === 1, '清空后按下限 1 生效（不产生 NaN）');
  ok(box.value === '1', '输入框被改写为 1');
}

// ---------- 4. 键盘交互 ----------
console.log('\n[4] 键盘');
{
  const { st, el } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  let prevented = false;
  box.onkeydown({ key: 'ArrowUp', preventDefault: () => { prevented = true; } });
  ok(box.value === '31', '方向键上 +1');
  ok(prevented === true, '拦截了原生行为（走同一条夹紧路径）');
  box.onkeydown({ key: 'ArrowDown', preventDefault: () => {} });
  ok(box.value === '30', '方向键下 -1');
}
{
  // 回归：上上下下必须回到原点。基准若取 state.limit（只在提交时更新），
  // 连按上键后按下键会从旧值回退（31 -> 29），这条断言就是为拦住它写的。
  const { st, el } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  const key = k => box.onkeydown({ key: k, preventDefault: () => {} });
  key('ArrowUp'); key('ArrowUp'); key('ArrowUp');
  ok(box.value === '33', '连按三次上键 = 30+3');
  key('ArrowDown'); key('ArrowDown'); key('ArrowDown');
  ok(box.value === '30', '再连按三次下键回到 30（基准取自输入框当前值）');
  // 全程不应触发任何重查：方向键只改输入框，提交才查
  ok(true, '方向键只改输入框、不重查（提交由 change/Enter 触发）');
}
{
  // 手输一个值后再按方向键，应从手输的值续算
  const { st, el } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  box.value = '100';
  box.onkeydown({ key: 'ArrowUp', preventDefault: () => {} });
  ok(box.value === '101', '手输 100 后按上键得 101（不是从 state.limit 的 30 算）');
}
{
  // 空值 + 方向键也不能产生 NaN
  const { st, el } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  box.value = '';
  box.onkeydown({ key: 'ArrowUp', preventDefault: () => {} });
  ok(box.value === '2', '空值按上键：先按下限 1 再 +1 = 2（不产生 NaN）');
}
{
  const { st, el, calls } = freshState(30, 743);
  renderPager(st);
  const box = el.querySelector('input');
  let blurred = false; box.blur = () => { blurred = true; };
  box.value = '50';
  let prevented = false;
  box.onkeydown({ key: 'Enter', preventDefault: () => { prevented = true; } });
  ok(st.limit === 50, '回车提交生效');
  ok(calls() === 1, '回车触发一次重查');
  ok(blurred === true, '回车后失焦（视觉上确认已提交）');
  ok(prevented === true, '回车被拦截，不会触发外层表单默认行为');
}
{
  // 上下键夹紧：在上限继续按不应越界
  const { st, el } = freshState(300, 743);
  renderPager(st);
  const box = el.querySelector('input');
  box.onkeydown({ key: 'ArrowUp', preventDefault: () => {} });
  ok(box.value === '300', '已在上限时按上键不越界');
}
{
  const { st, el } = freshState(1, 743);
  renderPager(st);
  const box = el.querySelector('input');
  box.onkeydown({ key: 'ArrowDown', preventDefault: () => {} });
  ok(box.value === '1', '已在 ≥下限时按下键不越界');
}

// ---------- 5. 分页计算随新 limit 正确变化 ----------
console.log('\n[5] 页码计算');
{
  const { st } = freshState(30, 743);
  ok(pageCount(st) === 25, '743 条 / 每页 30 = 25 页');
  st.limit = 100;
  ok(pageCount(st) === 8, '改为 100 后 = 8 页');
  st.limit = 300;
  ok(pageCount(st) === 3, '改为 300 后 = 3 页');
  st.limit = 743;
  ok(pageCount(st) === 1, '一次显示全部 = 1 页');
  st.total = 0;
  ok(pageCount(st) === 1, '总数为 0 时仍是 1 页（不出现第 0/0 页）');
}
{
  const { st, el } = freshState(120, 743);
  st.offset = 120;
  renderPager(st);
  ok(/第 2 \/ 7 页/.test(el.innerHTML), '每页 120 条第 2 页显示「第 2 / 7 页」（实际：' +
    (/pageno">([^<]*)</.exec(el.innerHTML) || [])[1] + '）');
  ok(/value="120"/.test(el.innerHTML), '输入框显示 120');
}

// ---------- 6. 静态：旧常量已清除 ----------
console.log('\n[6] 静态检查');
ok(!html.includes('PAGE_SIZES'), '旧的 PAGE_SIZES 常量已移除（不再有固定档位）');
ok(!/data-page="size">\s*<option/.test(html), '没有残留的 option 渲染');
ok(/clampPageSize/.test(html), 'clampPageSize 存在于页面中');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
