
// ⚠⚠ 这个模板里（**含注释**）绝不能出现裸反引号：本次修夹具时我在下面的注释里
//    写了一个，generator 立刻 SyntaxError（Unexpected identifier）——
//    而 run_frontend_suites.js 的**门禁 0**（findRawTemplateBacktick）就是为这个坑
//    设的：generator 挂了但上一次的产物还在，直接跑产物照样全绿，
//    于是"以为产品坏了，其实是测量工具坏了"。要引用代码片段就用「」括起来。
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '' }; }
for (const id of ['growthGroups','growthAccount','growthViews','growthTaskMeta','growthCards','growthRows']) els[id] = mkEl(id);
els.growthViews.querySelectorAll = () => [];
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let growthSnaps = [], growthView = 'all', growthViewPicked = true, growthUID = '';
// autoCodes：可自动化任务码集合，产品里是 "let autoCodes = new Set();"。
// 它是**声明语句**不是函数块，grabBlock 按花括号配平，抓不了它 ⇒ 在桩里给空集合。
// ⚠ 空集合是有语义的：canAuto 因此恒为 false，行内按钮走「去客户端做」那一支 ——
//   正是本夹具要判定的分支。想测「一键完成」按钮得先把集合填上（另一个夹具的事）。
// ⚠ autoCodesLoaded 也一起给：产品里它跟着 autoCodes 成对出现，缺了会再次 ReferenceError。
let autoCodes = new Set(), autoCodesLoaded = false;
function creditsOf() { return null; }
function buildGrowthAccountSelect() {}
function syncGrowthViewButtons() {}
// renderGrowthGroups 里会调它渲染「本机登录」行；本测试不关心那部分，给个空实现。
function clientLoginHTML() { return ''; }

const GROWTH_VIEWS = {
    acceptable: { label: '待接单', test: t => t.status === 'not_accepted' && !t.locked },
    running:    { label: '进行中', test: t => t.status === 'accepted' || t.status === 'in_progress' },
    done:       { label: '已完成', test: t => t.status === 'completed' || t.status === 'claimed' },
    all:        { label: '全部',   test: () => true },
  }

const GROWTH_STATUS_TEXT = {
    // 「未接单」只在「还没领到第一只 Buddy」时出现，且官方前端没有这个状态。
    // 保留它是因为那种账号确实需要先接单，否则连进度都不显示。
    not_accepted: ['待接单', 'dim'],
    // accepted = 已接单但进度还是 0 —— 用户要做的是「去客户端完成」，
    // 所以文案直接写成「待完成」而不是「进行中」，与卡片的「待完成任务」对齐。
    accepted: ['待完成', ''],
    // in_progress = 已经做了一部分（有 current/target），才叫「进行中」。
    in_progress: ['进行中', ''],
    // completed 与 claimed 分开显示：前者奖励还没领（行内给「领取」按钮），后者已到账。
    completed: ['待领取', 'warn'],
    claimed: ['已领取', 'ok'],
  }

function growthCounts(g) {
    const c = { acceptable: 0, running: 0, done: 0, all: 0 };
    for (const t of ((g && g.tasks) || [])) {
      for (const k of Object.keys(GROWTH_VIEWS)) {
        if (GROWTH_VIEWS[k].test(t)) c[k]++;
      }
    }
    return c;
  }

function fmtTaskExpiry(t) {
    if (!t || t.expires_at === undefined || t.expires_in_days === undefined) return null;
    // 服务端对已领取的任务不带这两个字段，这里再兜一层（旧数据可能没有 expired 标记）
    const d = t.expires_in_days;
    if (t.expired || d < 0) {
      return { text: '已过期 ' + Math.abs(d) + ' 天', cls: 'err' };
    }
    if (t.expiring_soon || d <= 7) {
      return { text: '剩 ' + d + ' 天', cls: 'warn' };
    }
    // 日期只显示到「月-日」，年份对 1~2 个月的期限没有信息量
    const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(t.expires_at);
    return { text: m ? `${Number(m[2])}-${Number(m[3])}（剩 ${d} 天）` : '剩 ' + d + ' 天', cls: '' };
  }

function maskNick(n) {
    const s = String(n == null ? '' : n);
    if (!s) return '—';
    if (s.indexOf('*') >= 0) return s;
    const rs = Array.from(s);
    if (rs.length <= 4) return s;   // 太短全打码 = 不可辨认，原样
    return rs.slice(0, 3).join('') + '*'.repeat(rs.length - 5) + rs.slice(-2).join('');
  }

function renderGrowthGroups() {
    const host = $('growthGroups');
    if (!host) return;
    if (!growthSnaps.length) {
      host.innerHTML = '<div class="dim" style="padding:14px 16px;font-size:12px">账号池为空。</div>';
      $('growthTaskMeta').textContent = '';
      return;
    }

    buildGrowthAccountSelect();
    // 「全部账号」选中时：任务表按账号铺开太长，显示提示 + 各账号待办摘要，
    // 让"点一键完成 = 所有账号都执行"这个作用域一眼可见。
    if (growthUID === '') {
      const lines = growthSnaps.map(g => {
        const c = growthCounts(g);
        const name = esc(maskNick(g.nickname));
        const left = c.acceptable + c.running;
        const done = c.done;
        return `<div style="padding:5px 0;font-size:12px">${name}
          <span class="pill ${left ? 'warn' : 'dim'}">待完成 ${left}</span>
          <span class="pill ${done ? 'ok' : 'dim'}">已完成 ${done}</span></div>`;
      }).join('');
      host.innerHTML = `<div class="dim" style="padding:12px 16px;font-size:12px">
        已选择「全部账号」：点上方「一键完成」将对所有账号依次执行（成长待办+领奖 → 开学季）。
        要查看/操作单个账号请在下拉框选择它。</div><div style="padding:0 16px">${lines}</div>`;
      $('growthTaskMeta').textContent = '';
      return;
    }
    const g = growthSnaps.find(x => x.uid === growthUID) || growthSnaps[0];
    const c = growthCounts(g);

    // 首次加载且用户没手动选过视图时，落到第一个非空视图。
    // 只做一次：之后用户点哪个就是哪个，刷新数据也不会把视图抢回去。
    // 必须在下面读 test 之前完成，否则这次渲染用的还是旧视图。
    if (!growthViewPicked) {
      for (const k of ['acceptable', 'running', 'done', 'all']) {
        if (c[k] > 0) { growthView = k; break; }
      }
      syncGrowthViewButtons();
    }

    const view = GROWTH_VIEWS[growthView] ? growthView : 'acceptable';
    const test = GROWTH_VIEWS[view].test;

    $('growthTaskMeta').textContent =
      `待接单 ${c.acceptable} · 进行中 ${c.running} · 已完成 ${c.done} · 共 ${c.all}`;

    const name = esc(maskNick(g.nickname));
    if (g.error && !g.tasks_total) {
      host.innerHTML = `<div class="ggroup">
        <div class="ghead"><span class="gname">${name}</span>
          <span class="err" style="font-size:12px">${esc(g.error.slice(0, 120))}</span></div>
      </div>`;
      return;
    }

    const tasks = (g.tasks || []).filter(test);
    const rows = tasks.map(t => {
      const [label, cls] = GROWTH_STATUS_TEXT[t.status] || [t.status, 'dim'];
      // 进度做成「数值 + 细进度条」：光看 4/5 不如一眼看出还剩多少。
      // target=0 或缺失时（如 black_cat 这类活动任务）不画条，避免 0/0 的假进度。
      let prog = '<span class="dim">—</span>';
      if (t.target > 0) {
        const cur = Math.max(0, Math.min(t.current || 0, t.target));
        const pct = Math.round((cur / t.target) * 100);
        prog = `<span class="mono" title="${cur}/${t.target}">${cur}/${t.target}</span>` +
               `<div class="pbar" title="${pct}%"><i style="width:${pct}%"></i></div>`;
      }
      const reward = [
        t.reward_credit ? `${t.reward_credit} 分` : '',
        t.reward_energy ? `+${t.reward_energy} 能量` : '',
      ].filter(Boolean).join(' ') || '<span class="dim">—</span>';
      const locked = t.locked ? ' <span class="pill warn">未解锁</span>' : '';
      // 领取优先于接单：completed 的任务已经做完了，唯一可做的就是领奖。
      //
      // ⚠ 这一格原来对 accepted / in_progress 一律显示「去客户端做」，
      // 注释写着"这两类任务**本网页无法代做**"。移植伪造遥测层之后该结论
      // **不再成立**：autoActions 里那 17 条就是网关能直接做完的。
      //
      // 所以现在的判据是「该任务码在不在后端能力表里」：
      //   在 → 「一键完成」（后端自己会处理"先接单"这一步）
      //   不在 → 仍然退回说明文字 —— 对**确实**只能去客户端做的任务保留诚实提示
      //
      // ⚠ 必须排除**已做完**的任务（claimed）。claimed 的 claimable 为 false，
      // 于是它会一路落到这个分支上 —— 只看能力表的话，「已领取」的任务行也会
      // 长出一个「一键完成」按钮，点了只会回一句"已完成（1/1）"。
      // 这里复用 GROWTH_VIEWS.done.test 而**不另写一遍** status 判断：
      // "什么算做完"只有一处定义，否则两边必然会漂移（视图把它算已完成、
      // 按钮却认为还能再做）。
      const canAuto = autoCodes.has(t.task_code) && !GROWTH_VIEWS.done.test(t);
      const btn = t.claimable
        ? `<button data-gclaimreward="${esc(t.task_code)}" data-uid="${esc(g.uid)}">领取 ${t.reward_credit || 0} 分</button>`
        : (canAuto && !t.locked)
          ? `<button data-gauto="${esc(t.task_code)}" data-uid="${esc(g.uid)}" class="ghost" title="网关直接把该任务做完并领奖（上报伪造的客户端行为遥测）；只做这一个任务">一键完成</button>`
          : (t.status === 'not_accepted' && !t.locked)
            ? `<button data-gaccept="${esc(t.task_code)}" data-uid="${esc(g.uid)}" class="ghost">接单</button>`
            : (t.status === 'accepted' || t.status === 'in_progress')
              ? '<span class="dim" style="font-size:11px">去客户端做</span>'
              : '<span class="dim">—</span>';
      const tag = t.tag ? ` <span class="pill">${esc(t.tag)}</span>` : '';
      // 到期列：只有部分任务有期限（上游 18 个里 4 个），其余显示占位。
      // 已过期时即使有「领取」按钮也要同时给出提示 —— 那种情况下按钮点了会失败。
      const exp = fmtTaskExpiry(t);
      const expiry = exp
        ? `<span class="${exp.cls}" title="${esc(t.expires_at)}">${esc(exp.text)}</span>`
        : '<span class="dim">—</span>';

      return `<tr>
        <td class="gtask">${esc(t.title || t.task_code)}${tag}<br>
          <span class="dim mono" style="font-size:11px">${esc(t.task_code)}</span></td>
        <td class="gdesc">
          <div title="${esc(t.description || '')}">${t.description ? esc(t.description) : '<span class="dim">—</span>'}</div>
          ${t.how_to ? `<div class="howto" title="${esc(t.how_to)}">怎么做：${esc(t.how_to)}</div>` : ''}
        </td>
        <td><span class="pill ${cls}">${esc(label)}</span>${locked}</td>
        <td class="mono">${prog}</td>
        <td class="mono">${reward}</td>
        <td class="mono exp">${expiry}</td>
        <td style="white-space:nowrap">${btn}</td>
      </tr>`;
    }).join('');

    host.innerHTML = `<div class="ggroup">
      <div class="ghead">
        <span class="gname">${name}</span>
        <span class="dim mono" style="font-size:11px">${esc(g.uid)}</span>
        <span class="dim" style="font-size:12px">待接单 ${c.acceptable} · 进行中 ${c.running} · 已完成 ${c.done}</span>
        ${g.claimable_count
          ? `<span class="pill warn">${g.claimable_count} 个待领取 · ${g.claimable_credit} 分</span>
             <button data-gclaimall="${esc(g.uid)}">全部领取</button>`
          : '<span class="pill ok">奖励已领完</span>'}
        <span class="spacer"></span>
        <span class="tog">${clientLoginHTML(g.uid)}</span>
      </div>
      ${tasks.length ? `<table><thead><tr>
        <th>任务</th><th>说明</th><th>状态</th><th>进度</th><th>奖励</th><th title="仅部分任务有期限（上游 18 个里 4 个带），已领取的不显示">到期</th><th>操作</th>
      </tr></thead><tbody>${rows}</tbody></table>`
      : `<div class="dim" style="padding:10px 16px;font-size:12px">该账号在「${esc(GROWTH_VIEWS[view].label)}」下没有任务。换个视图或换个账号看看。</div>`}
    </div>`;
  }

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

function render(tasks) {
  growthSnaps = [{
    uid: 'u1', nickname: '账号 A', tasks_total: tasks.length, tasks,
    claimable_count: 0, claimable_credit: 0, acceptable_count: 0, acceptable_credit: 0,
    streak_days: 1, energy: 5,
  }];
  growthView = 'all'; growthUID = 'u1';
  renderGrowthGroups();
  return $('growthGroups').innerHTML;
}

// ---------- 1. accepted 的文案 ----------
console.log('\n[1] accepted = 待完成（不是进行中）');
let h = render([
  { task_code: 'a', status: 'accepted', title: 'A', reward_credit: 100, target: 5, current: 0 },
]);
ok(h.includes('待完成'), 'accepted 显示「待完成」');
ok(!/>进行中</.test(h), 'accepted 不显示「进行中」');
h = render([
  { task_code: 'b', status: 'in_progress', title: 'B', reward_credit: 100, target: 5, current: 3 },
]);
ok(h.includes('进行中'), 'in_progress 显示「进行中」');

// ---------- 2. 进度条 ----------
console.log('\n[2] 进度条');
h = render([
  { task_code: 'c', status: 'in_progress', title: 'C', reward_credit: 100, target: 5, current: 3 },
]);
ok(h.includes('3/5'), '显示 3/5');
ok(h.includes('class="pbar"'), '有进度条容器');
ok(/class="pbar"[^>]*>\s*<i style="width:60%">/.test(h.replace(/\s+/g, ' ')), '宽度 60%（3/5）');
// target=0 不画条
h = render([
  { task_code: 'd', status: 'accepted', title: 'D', reward_credit: 0, target: 0, current: 0 },
]);
ok(!h.includes('class="pbar"'), 'target=0 时不画进度条（避免 0/0 假进度）');
// 边界：current 超过 target 时封顶 100%
h = render([
  { task_code: 'e', status: 'in_progress', title: 'E', reward_credit: 100, target: 3, current: 99 },
]);
ok(/width:100%/.test(h), 'current>target 时封顶 100%（不画出界条）');

// ---------- 3. 按钮策略 ----------
console.log('\n[3] 操作列');
h = render([
  { task_code: 'f', status: 'completed', title: 'F', reward_credit: 300, target: 1, current: 1, claimable: true },
]);
ok(h.includes('data-gclaimreward="f"'), 'completed 给「领取」按钮');
ok(h.includes('领取 300 分'), '按钮带金额');

h = render([
  { task_code: 'g', status: 'accepted', title: 'G', reward_credit: 100, target: 5, current: 0 },
]);
ok(!h.includes('data-gaccept="g"'), 'accepted 不给接单按钮（已经接过了）');
ok(h.includes('去客户端做'), 'accepted 提示「去客户端做」');

h = render([
  { task_code: 'h', status: 'in_progress', title: 'H', reward_credit: 100, target: 5, current: 2 },
]);
ok(!h.includes('data-gaccept="h"'), 'in_progress 不给接单按钮');
ok(h.includes('去客户端做'), 'in_progress 也提示「去客户端做」');

h = render([
  { task_code: 'i', status: 'not_accepted', title: 'I', reward_credit: 100, target: 5, current: 0 },
]);
ok(h.includes('data-gaccept="i"'), 'not_accepted 给「接单」按钮');
ok(/data-gaccept="i"[^>]*class="ghost"/.test(h), '接单按钮是次要样式 ghost');

h = render([
  { task_code: 'j', status: 'claimed', title: 'J', reward_credit: 100, target: 1, current: 1 },
]);
ok(!h.includes('data-gaccept="j"') && !h.includes('data-gclaimreward="j"'), 'claimed 无按钮');

// L 锁定任务不给接单
h = render([
  { task_code: 'k', status: 'not_accepted', title: 'K', reward_credit: 100, locked: true, target: 5 },
]);
ok(!h.includes('data-gaccept="k"'), '未解锁的任务不给接单按钮');
ok(h.includes('未解锁'), '显示「未解锁」标记');

// ---------- 4. 静态：设置项说明与 CSS ----------
console.log('\n[4] 设置项与样式');
// ⚠ 这条断言原来是 /setGrowthAccept[\s\S]{0,200}官方前端没有接单按钮/ ——
//   那个设置项**已经不在了**：commit e95b814「设置页再隐藏 3 个纯收益开关」
//   把「自动领取奖励」「自动接单」从设置页拿掉了（用户决定；理由写在
//   webui.html:1631-1632 的注释里，后端 key growth_auto_accept_tasks 仍在
//   internal/workbuddy/settings.go:37，只是不再有 UI）。
//   ⇒ 断言的**作用域**错了（守着一个被有意移除的 UI 元素），所以改成守**现在的决定**：
//     纯收益开关不展示，而「接单」这个**动作**必须还在 —— 那才是本面板的能力。
ok(!/id="setGrowthAccept"/.test(html), '设置页不再有「自动接单」开关（纯收益项不展示）');
ok(!/id="setGrowthClaim"/.test(html), '设置页不再有「自动领取奖励」开关（同一批隐藏）');
ok(/id="setGrowthMakeup"/.test(html) && /id="setGrowthRedeem"/.test(html)
   && /id="setGrowthOpen"/.test(html) && /id="setGrowthDraw"/.test(html),
   '四个「消耗资源」开关仍在（补签 / 连登兑换 / 开盲盒 / 抽奖）');
ok(html.includes('全部接单'), '「全部接单」按钮仍在（隐藏的是开关，不是动作）');
// 接单按钮为什么必须有：官方前端里根本没有这个状态（webui.html:8562）。
// 这句注释是"当初为什么做它"的唯一记录，钉住它 —— 否则下次有人会把整个接单掰掉。
ok(html.includes('官方前端没有这个状态'), '页面里保留了「官方前端没有接单状态」的理由');
ok(/\.pbar\{/.test(html) && /\.pbar i\{/.test(html), '.pbar 样式已定义');
ok(/\.ghost\{/.test(html), '.ghost 样式已定义');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
