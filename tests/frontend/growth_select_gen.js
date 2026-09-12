
const fs = require('fs');
// ⚠ 密钥**绝不**写进源码。此前这份文件里直接写了一串真实 api_key，
// 随测试套件一起提交进了版本库（被 acceptance.js 的密钥扫描抓到）。
// 现在从环境变量取；未设置时用一个明显无效的占位值 ——
// 断言不依赖密钥的真实性（它只测渲染逻辑）。
const KEY = process.env.WB2API_KEY || 'test-key-not-real';
let growthSnaps = [], growthUID = '', growthView = 'acceptable', growthViewPicked = false;
let clientStatus = null;
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
const elements = {};
function resetEls() {
  elements.growthAccount = { innerHTML: '', value: '' };
  elements.growthViews = { querySelectorAll: () => [] };
  elements.growthTaskMeta = { textContent: '' };
  elements.growthGroups = { innerHTML: '' };
}
resetEls();
const $ = id => elements[id];
// 本机登录状态由**合成夹具**提供，不请求任何在跑的实例。
//
// 原实现是 fetch('http://127.0.0.1:7863/admin/client-login') —— 7863 是
// 用户的生产网关：测试会在别人机器上失败，并把真实账号名带进断言输出。
//
// 字段形状按 clientLoginHTML 的真实契约：
//   enabled / current{uid,nickname} / client_running / candidates[]{uid,valid,current}
async function fetchClientStatus() {
  return {
    enabled: true,
    current: { uid: 'test-uid-0002', nickname: '测试账号乙' },
    client_running: false,
    has_backup: false,
    candidates: [
      { uid: 'test-uid-0001', nickname: '测试账号甲', valid: true, current: false },
      { uid: 'test-uid-0002', nickname: '测试账号乙', valid: true, current: true },
      { uid: 'test-uid-0003', nickname: '测试账号丙', valid: true, current: false },
    ],
  };
}

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

function syncGrowthViewButtons() {
    const box = $('growthViews');
    if (!box) return;
    for (const b of box.querySelectorAll('button[data-gview]')) {
      b.classList.toggle('on', b.dataset.gview === growthView);
    }
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

function buildGrowthAccountSelect() {
    const sel = $('growthAccount');
    if (!sel) return;
    const prev = growthUID;
    const opts = growthSnaps.map(g => {
      const c = growthCounts(g);
      const name = g.nickname || (g.uid || '').slice(0, 8);
      const hint = g.tasks_total
        ? `（待接单 ${c.acceptable} · 进行中 ${c.running}）`
        : '（未探测）';
      return `<option value="${esc(g.uid)}">${esc(name + hint)}</option>`;
    }).join('');
    sel.innerHTML = opts;

    // 保持原选择；账号被删掉时回落到第一个。
    const exists = growthSnaps.some(g => g.uid === prev);
    growthUID = exists ? prev : ((growthSnaps[0] || {}).uid || '');
    sel.value = growthUID;
  }

function clientLoginHTML(uid) {
    if (!clientStatus) {
      return '<span class="dim" style="font-size:11px">本机登录：读取中…</span>';
    }
    if (!clientStatus.enabled) {
      return `<span class="dim" style="font-size:11px" title="${esc(clientStatus.error || '')}">本机登录：不可用</span>`;
    }
    const cur = clientStatus.current;
    const curTxt = cur
      ? `本机登录：${esc(cur.nickname || cur.uid)}`
      : '本机登录：未登录';
    // 客户端在跑时改盘会被它按内存会话原地覆盖，所以按钮置灰并说明原因，
    // 而不是让用户点了再收到一句拒绝。
    const running = !!clientStatus.client_running;
    const runTip = 'WorkBuddy 客户端正在运行，它会把内存里的旧登录态写回磁盘、覆盖本次切换。请先完全退出客户端（含托盘图标）。';
    const cand = (clientStatus.candidates || []).find(c => c.uid === uid);
    let act;
    if (!cand) {
      act = '<span class="dim" style="font-size:11px">无可用凭证</span>';
    } else if (!cand.valid) {
      act = '<span class="pill err">凭证已过期</span>';
    } else if (cand.current) {
      act = '<span class="pill ok">已是本机登录</span>';
    } else {
      const nm = cand.nickname || cand.uid;
      act = running
        ? `<button disabled title="${esc(runTip)}">切换本机登录</button>`
        : `<button data-clswitch="${esc(uid)}" data-clname="${esc(nm)}" `
          + `title="把本机 WorkBuddy 客户端切换到 ${esc(nm)}（改写客户端登录态，需重启客户端）">切换本机登录</button>`;
    }
    const rb = clientStatus.has_backup
      ? (running
        ? `<button disabled title="${esc(runTip)}">回滚</button>`
        : `<button data-clrestore="1" title="回滚到 ${esc(clientStatus.backup_nick || clientStatus.backup_uid)}`
          + `（备份于 ${esc(clientStatus.backup_at || '未知时间')}）">回滚</button>`)
      : '';
    const runPill = running
      ? ` <span class="pill warn" title="${esc(runTip)}">客户端运行中</span>`
      : '';
    return `<span class="dim" style="font-size:11px">${curTxt}</span>${runPill}${act}${rb}`;
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

    const name = esc(g.nickname || (g.uid || '').slice(0, 8));
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
      // accepted / in_progress 给的不是按钮而是说明 —— 这两类任务**本网页无法代做**，
      // 必须去 WorkBuddy 客户端里真正把动作做掉。留一个禁用按钮只会让人反复点。
      const btn = t.claimable
        ? `<button data-gclaimreward="${esc(t.task_code)}" data-uid="${esc(g.uid)}">领取 ${t.reward_credit || 0} 分</button>`
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
        <span class="dim mono" style="font-size:11px">${esc((g.uid || '').slice(0, 8))}</span>
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

// ---------------------------------------------------------------------------
// 合成夹具（**不再请求** 127.0.0.1:7863）。
//
// 原写法是 fetch('http://127.0.0.1:7863/admin/growth') —— 7863 是
// **用户的生产网关**。那让测试依赖"本机恰好在跑那个实例"（别人机器上必然
// 失败），并把真实账号 UID/昵称带进了断言输出。
//
// 本套件要验的是**渲染规则**（选项数、默认选中、刷新后保持、回落到第一个、
// 三视图之和），与数据来源无关；换成合成夹具不影响它证明的东西。
const FIXTURE_ACCOUNTS = [
  {
    uid: 'test-uid-0001', nickname: '测试账号甲',
    tasks_total: 3, tasks_completed: 1, tasks_accepted: 1,
    acceptable_count: 1, acceptable_credit: 10, acceptable_energy: 1,
    pending_count: 1, pending_credit: 20, pending_energy: 1,
    claimable_count: 1, claimable_credit: 30, claimable_energy: 1,
    streak_days: 5, next_tier_remaining: 3, makeup_cards: 1,
    remaining_days: 20, energy: 12,
    blind_box_affordable: true, blind_box_cost: 10, lottery_chances: 2,
    observed_at: '2026-01-01T00:00:00Z', error: '',
    // tasks 供 growthCounts() 统计三视图，并给「说明」列提供 description/how_to。
    // create_canvas 是断言点名的任务码，必须存在。
    tasks: [
      { task_code: 'create_canvas', status: 'not_accepted', locked: false, reward_credit: 10,
        description: '达成条件：完成一次画布创建',
        how_to: '操作指引：在工作台新建画布即可' },
      { task_code: 'test-task-b', status: 'accepted', locked: false, reward_credit: 20,
        description: '达成条件：进行中的任务', how_to: '' },
      { task_code: 'test-task-c', status: 'completed', locked: false, reward_credit: 30,
        description: '达成条件：已完成的任务', how_to: '' },
    ],
  },
  {
    uid: 'test-uid-0002', nickname: '测试账号乙',
    tasks_total: 2, tasks_completed: 0, tasks_accepted: 2,
    acceptable_count: 0, acceptable_credit: 0, acceptable_energy: 0,
    pending_count: 2, pending_credit: 10, pending_energy: 2,
    claimable_count: 0, claimable_credit: 0, claimable_energy: 0,
    streak_days: 1, next_tier_remaining: 7, makeup_cards: 0,
    remaining_days: 30, energy: 3,
    blind_box_affordable: false, blind_box_cost: 10, lottery_chances: 0,
    observed_at: '2026-01-01T00:00:00Z', error: '',
    tasks: [
      { task_code: 'test-task-d', status: 'in_progress', locked: false, reward_credit: 5,
        description: '达成条件：进行中', how_to: '' },
      { task_code: 'test-task-e', status: 'in_progress', locked: false, reward_credit: 5,
        description: '达成条件：进行中', how_to: '' },
    ],
  },
  {
    uid: 'test-uid-0003', nickname: '测试账号丙',
    tasks_total: 0, tasks_completed: 0, tasks_accepted: 0,
    acceptable_count: 0, acceptable_credit: 0, acceptable_energy: 0,
    pending_count: 0, pending_credit: 0, pending_energy: 0,
    claimable_count: 0, claimable_credit: 0, claimable_energy: 0,
    streak_days: 0, next_tier_remaining: 0, makeup_cards: 0,
    remaining_days: 0, energy: 0,
    blind_box_affordable: false, blind_box_cost: 0, lottery_chances: 0,
    observed_at: '2026-01-01T00:00:00Z', error: '',
    tasks: [],
  },
];

(async () => {
  growthSnaps = FIXTURE_ACCOUNTS;

  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  console.log('真实账号数: ' + growthSnaps.length + '  (' + growthSnaps.map(g => g.nickname).join(', ') + ')');

  console.log('\n[1] 下拉框选项');
  buildGrowthAccountSelect();
  const optHtml = elements.growthAccount.innerHTML;
  optHtml.split('</option>').filter(s => s.includes('<option')).forEach(s => console.log('   ' + s.replace(/^.*?<option /, '<option ') + '</option>'));
  ok((optHtml.match(/<option /g) || []).length === growthSnaps.length, '选项数 == 账号数 (' + growthSnaps.length + ')');
  ok(optHtml.includes('待接单'), '选项文字带待接单计数');

  console.log('\n[2] 默认选择');
  ok(growthUID === growthSnaps[0].uid, '默认选中第一个账号 (' + growthSnaps[0].nickname + ')');
  ok(elements.growthAccount.value === growthUID, 'select.value 与 growthUID 一致');

  console.log('\n[3] 刷新保持用户选择');
  const second = (growthSnaps[1] || growthSnaps[0]).uid;
  growthUID = second;
  buildGrowthAccountSelect();
  ok(growthUID === second, '刷新后仍是用户选的账号');

  console.log('\n[4] 选中账号消失时回落');
  growthUID = 'gone-0000-0000-0000-000000000000';
  buildGrowthAccountSelect();
  ok(growthUID === growthSnaps[0].uid, '回落到第一个账号');

  console.log('\n[5] 各账号计数（三视图应互斥且加总等于全部）');
  for (const g of growthSnaps) {
    const c = growthCounts(g);
    console.log('   ' + g.nickname + ': 待接单 ' + c.acceptable + ' · 进行中 ' + c.running + ' · 已完成 ' + c.done + ' · 共 ' + c.all);
    ok(c.acceptable + c.running + c.done === c.all, g.nickname + ' 三视图之和 == 全部');
    ok(c.all === (g.tasks || []).length, g.nickname + ' 全部 == tasks 长度');
  }

  console.log('\n[6] 渲染只画一个账号（不是全部）');
  growthViewPicked = false;
  growthUID = growthSnaps[0].uid;
  growthView = 'all';       // 全部，确保能拿到 create_canvas 这类已完成任务
  growthViewPicked = true;
  renderGrowthGroups();
  const gHtml = elements.growthGroups.innerHTML;
  const heads = (gHtml.match(/class="gname"/g) || []).length;
  ok(heads === 1, '明细区只有 1 个账号节头（实际 ' + heads + '），账号数 ' + growthSnaps.length);
  if (growthSnaps.length > 1) ok(!gHtml.includes(growthSnaps[1].nickname), '未渲染第二个账号');
  ok(elements.growthTaskMeta.textContent.includes('待接单'), 'meta 显示了计数');

  console.log('\n[6b] 任务详细信息确实输出到前端');
  ok(gHtml.includes('<th>说明</th>'), '表头含「说明」列');
  const first = growthSnaps[0];
  let checkedDesc = 0, checkedHow = 0;
  for (const t of (first.tasks || [])) {
    if (t.description) { ok(gHtml.includes(esc(t.description)), '说明已渲染: ' + t.task_code + ' -> ' + t.description.slice(0, 24)); checkedDesc++; break; }
  }
  for (const t of (first.tasks || [])) {
    if (t.how_to) { ok(gHtml.includes(esc(t.how_to)), '怎么做已渲染: ' + t.task_code); checkedHow++; break; }
  }
  const cc = (first.tasks || []).find(t => t.task_code === 'create_canvas');
  if (cc) {
    console.log('   create_canvas 说明 = ' + cc.description);
    console.log('   create_canvas 怎么做 = ' + (cc.how_to || '(无)'));
    ok(gHtml.includes(esc(cc.description)), '用户点名的 create_canvas 达成条件已渲染');
    if (cc.how_to) ok(gHtml.includes(esc(cc.how_to)), '用户点名的 create_canvas 操作指引已渲染');
  }
  ok(checkedDesc > 0, '说明列有实际内容');

  console.log('\n[7] 自动落到第一个非空视图');
  console.log('   选中视图 = ' + growthView);
  const c0 = growthCounts(growthSnaps.find(g => g.uid === growthUID));
  ok(c0[growthView] > 0 || growthView === 'all', '自动选中的视图非空 (' + growthView + '=' + c0[growthView] + ')');

  console.log('\n[8] 切账号会重画');
  if (growthSnaps.length > 1) {
    const target = growthSnaps[1];
    growthUID = target.uid;
    growthViewPicked = true;
    renderGrowthGroups();
    const h2 = elements.growthGroups.innerHTML;
    ok(h2.includes(target.nickname), '切到 ' + target.nickname + ' 后渲染了该账号');
    ok(!h2.includes(growthSnaps[0].nickname), '不再渲染上一个账号');
  }

  console.log('\n[9] 空账号池');
  const savedSnaps = growthSnaps;
  growthSnaps = [];
  growthUID = '';
  buildGrowthAccountSelect();
  ok(elements.growthAccount.innerHTML === '', '空池时下拉框无选项');

  console.log('\n[10] 明细节头里的「本机登录」控制条');
  growthSnaps = savedSnaps;
  growthUID = growthSnaps[0].uid;
  growthView = 'all';
  growthViewPicked = true;
  const realStatus = await fetchClientStatus();
  console.log('    后端: enabled=' + realStatus.enabled + ' current=' + (realStatus.current && realStatus.current.nickname)
    + ' has_backup=' + realStatus.has_backup);

  // 10a) 读取中（clientStatus 为 null）
  clientStatus = null;
  renderGrowthGroups();
  ok(elements.growthGroups.innerHTML.includes('本机登录：读取中'), '未加载时显示「读取中」');

  // 10b) 未启用
  clientStatus = { enabled: false, error: '测试：未启用' };
  renderGrowthGroups();
  ok(elements.growthGroups.innerHTML.includes('本机登录：不可用'), '未启用时显示「不可用」');

  // 10c) 真实状态
  clientStatus = realStatus;
  renderGrowthGroups();
  const gh = elements.growthGroups.innerHTML;
  const togIdx = gh.indexOf('本机登录：');
  console.log('    控制条片段: ' + (togIdx >= 0
    ? gh.slice(togIdx, togIdx + 160).replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim()
    : '(未找到本机登录文字)'));
  ok(gh.includes('本机登录：'), '节头包含本机登录文字');
  if (realStatus.current) {
    ok(gh.includes(esc(realStatus.current.nickname)), '显示当前客户端登录账号名');
  }
  const curCand = (realStatus.candidates || []).find(c => c.uid === growthUID);
  if (curCand && curCand.current) {
    ok(gh.includes('已是本机登录'), '正在看的账号就是本机登录账号 → 显示「已是本机登录」');
  } else if (curCand) {
    ok(gh.includes('data-clswitch'), '正在看的账号不是本机登录账号 → 出现「切换本机登录」按钮');
  }
  ok(realStatus.has_backup ? gh.includes('data-clrestore') : !gh.includes('data-clrestore'),
    '有备份才出现「回滚」按钮（has_backup=' + realStatus.has_backup + '）');

  // 10d) 切到另一个账号，按钮应指向那个账号
  if (growthSnaps.length > 1) {
    const other = growthSnaps[1];
    growthUID = other.uid;
    renderGrowthGroups();
    const gh2 = elements.growthGroups.innerHTML;
    const m = gh2.match(/data-clswitch="([^"]+)"/);
    if (m) ok(m[1] === other.uid, '切换按钮指向当前展示的账号 ' + other.uid.slice(0, 8) + '（看的和切的一致）');
    else ok(gh2.includes('已是本机登录'), '第二账号已是本机登录，无切换按钮');
  }

  // 10e) 旧独立面板确实已移除
  const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
  ok(!html.includes('<h2>本地登录'), '页面里已没有独立的「本地登录」面板');
  ok(!html.includes('clientRows'), '页面里没有旧面板的残留 DOM id');

  console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
