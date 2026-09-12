
const fs = require('fs');
const html = fs.readFileSync("D:\\project_GIT\\workbuddy2api实验版本/internal/server/webui.html", 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '', onclick: null }; }
for (const id of ['toast','notice','noticeClose','gtask','growthCards','growthRows','growthGroups','growthAccount','growthViews','growthTaskMeta']) els[id] = mkEl(id);
els.growthViews.querySelectorAll = () => [];
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let share = [], growthSnaps = [];
function renderGrowthGroups() {}

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

function growthError(e, label) {
    const msg = (e && e.message) || String(e);
    const benign = /跳过|被前置任务|需要先完成前置任务|不可领取|不可接单|没有可|没有待|不足|已领取/.test(msg);
    toast(`${label}：${msg}`, !benign);
    const blocked = /需要先完成前置任务[：:]\s*(.+)$/.exec(msg);
    if (blocked) {
      notice('接单被前置任务挡住',
        blocked[1].trim() + '\n\n这一步只能在 WorkBuddy 客户端里做，本网页无法代做。完成后回到这里再点「接单」即可。');
    }
  }

function creditsOf(uid) {
    const a = share.find(x => x.uid === uid);
    if (!a || typeof a.credits !== 'number') return null;
    return a.credits;
  }

function renderGrowth(list) {
    growthSnaps = list || [];

    const sum = growthSnaps.reduce((a, g) => {
      // 「待完成」用 pending_*（未接单 + 已接单 + 进行中），不是 acceptable_*。
      // acceptable 只数未接单的，会让已接单/进行中的任务从统计里消失 ——
      // 那样账号明明还有 5 个任务要做，卡片却显示 0。
      // 兼容：旧快照没有 pending_* 字段时回落到 acceptable_*，避免显示成 0。
      a.claim += (g.pending_count != null ? g.pending_count : (g.acceptable_count || 0));
      a.credit += (g.pending_credit != null ? g.pending_credit : (g.acceptable_credit || 0));
      a.streak = Math.max(a.streak, g.streak_days || 0);
      a.energy += g.energy || 0;
      // 可领奖：已完成但奖励还没到手的部分，这才是「现在能拿到的分」。
      a.right_now += g.claimable_credit || 0;
      a.right_now_n += g.claimable_count || 0;
      // 额度：账号当前积分余额合计，只累加拿得到的那些账号。
      const c = creditsOf(g.uid);
      if (c !== null) { a.credits += c; a.credits_n++; }
      return a;
    }, { claim: 0, credit: 0, streak: 0, energy: 0, right_now: 0, right_now_n: 0, credits: 0, credits_n: 0 });

    $('growthCards').innerHTML = [
      ['账号数', growthSnaps.length, ''],
      // 额度排在「现在可领」前面：先看家底，再看还能拿多少。
      ['额度合计', sum.credits_n ? sum.credits : '—', sum.credits_n ? '' : 'dim'],
      ['现在可领积分', sum.right_now, sum.right_now ? 'ok' : 'dim'],
      ['待领取任务', sum.right_now_n, sum.right_now_n ? 'warn' : 'dim'],
      // 「待完成 / 完成后可获得」的口径是**还没做完的任务**：
      // 未接单 + 已接单 + 进行中三种状态都算（见后端 GrowthTask.Pending）。
      // 不含 completed（那属于「待领取」，领奖是另一步）。
      // 措辞强调"完成"而不是"接单"：接单只是开始计进度，不发任何奖励。
      ['待完成任务', sum.claim, sum.claim ? 'ok' : 'dim'],
      ['完成后可获得的积分', sum.credit, sum.credit ? 'ok' : 'dim'],
      ['最长连登', sum.streak + ' 天', ''],
      ['能量合计', sum.energy, ''],
    ].map(([k, v, c]) =>
      `<div class="card"><div class="k">${esc(k)}</div><div class="v ${c}">${esc(v)}</div></div>`).join('');
    if (!growthSnaps.length) {
      $('growthRows').innerHTML = rowPlaceholder(COLS.growth, '账号池为空');
      renderGrowthGroups();
      return;
    }

    $('growthRows').innerHTML = growthSnaps.map(g => {
      // 只有「压根没数据」才整行报错。刷新失败但有上次数据时（stale=true）
      // 继续正常渲染，只在账号名旁挂一个过期标记 —— 不能让一次上游抖动抹白整张表。
      const stale = g.stale
        ? ` <span class="pill warn" title="本次刷新失败，展示的是 ${new Date(g.observed_at).toLocaleTimeString('zh-CN', { hour12: false })} 的数据：${esc(g.error || '')}">可能过期</span>`
        : '';
      const name = esc(g.nickname || (g.uid || '').slice(0, 8)) + stale;
      const U = esc(g.uid);
      if (g.error && !g.tasks_total) {
        // 「昵称」列已占 1 格，余下 COLS.growth - 1 格由错误信息占满。
        //
        // # 这是 bug B2
        //
        // 原先写死 colspan="11"，而那行只有 1 个前置 <td> + 11 = 12 格，
        // 表面上"刚好等于 12 列"。但「昵称」那一格已经占掉 1 列，
        // 所以正确值是 COLS.growth - 1 = 11 —— 数值碰巧对了，
        // **含义却是错的**：它把"整行跨列"当成了"剩余列"。
        // 一旦成长表加列，这个 11 不会跟着变，错误行就会少占一格。
        // 写成减法后，加列自动跟随。
        return `<tr><td>${name}</td><td colspan="${COLS.growth - 1}" class="err" title="${esc(g.error)}">${esc(g.error.slice(0, 70))}</td></tr>`;
      }
      // 额度取不到时显示 —（而不是 0）：账号可能已不在池里，这与「余额为零」是两回事。
      const cr = creditsOf(g.uid);
      const quota = cr === null
        ? '<span class="dim" title="该账号不在账号池视图里，取不到额度">—</span>'
        : `<span class="mono">${cr}</span>`;
      const streak = g.streak_days
        ? `${g.streak_days} 天${g.next_tier ? `<br><span class="dim" style="font-size:11px">距 ${esc(g.next_tier)} 还差 ${g.next_tier_remaining}</span>` : ''}`
        : '<span class="dim">—</span>';
      const cards = g.makeup_cards
        ? `${g.makeup_cards}${g.makeup_dates && g.makeup_dates.length ? ` <span class="pill warn">可补${g.makeup_dates.length}</span>` : ''}`
        : '<span class="dim">—</span>';
      // 待完成 / 完成后可得：口径是"还没做完"（未接单+已接单+进行中），
      // 与「接单」按钮的可用性（acceptable_count，只数未接单）是两件事。
      const pendN = g.pending_count != null ? g.pending_count : (g.acceptable_count || 0);
      const pendC = g.pending_credit != null ? g.pending_credit : (g.acceptable_credit || 0);
      const claim = pendN
        ? `<span class="pill ok">${pendN}</span>`
        : '<span class="dim">—</span>';
      const credit = pendC
        ? `<span class="pill ok">${pendC}</span>`
        : '<span class="dim">—</span>';
      // 待领取 / 现在可领：来自 completed 的任务，领了就到账。
      const pendingN = g.claimable_count
        ? `<span class="pill warn">${g.claimable_count}</span>`
        : '<span class="dim">—</span>';
      const pendingC = g.claimable_credit
        ? `<span class="pill warn">${g.claimable_credit}</span>`
        : '<span class="dim">—</span>';
      const box = g.blind_box_affordable
        ? `<span class="pill ok">${g.blind_box_affordable}</span>`
        : '<span class="dim">—</span>';
      const draw = g.lottery_chances
        ? `<span class="pill ok">${g.lottery_chances}</span>`
        : '<span class="dim">—</span>';

      const act = [
        // 领奖放最前：这是唯一真正加分的动作，有可领的就一定是当前最该点的。
        `<button data-gact="claim"   data-uid="${U}"${g.claimable_count ? '' : ' disabled'} title="领取已完成任务的奖励${g.claimable_credit ? `（共 ${g.claimable_credit} 分）` : ''}">领取</button>`,
        `<button data-gact="accept"  data-uid="${U}"${g.acceptable_count ? '' : ' disabled'} title="把未接单的任务接进列表开始计进度。接单不发奖励，奖励要走「领取」">接单</button>`,
        `<button data-gact="redeem" data-uid="${U}">兑换</button>`,
        `<button data-gact="makeup" data-uid="${U}"${g.makeup_dates && g.makeup_dates.length ? '' : ' disabled'}>补签</button>`,
        `<button data-gact="open"   data-uid="${U}"${g.blind_box_affordable ? '' : ' disabled'}>开盒</button>`,
        `<button data-gact="draw"   data-uid="${U}"${g.lottery_chances ? '' : ' disabled'}>抽奖</button>`,
      ].join(' ');

      return `<tr>
        <td>${name}</td>
        <td>${quota}</td>
        <td>${streak}</td>
        <td class="mono">${cards}</td>
        <td class="mono">${esc(g.energy)}</td>
        <td>${box}</td>
        <td>${draw}</td>
        <td>${pendingN}</td>
        <td>${pendingC}</td>
        <td>${claim}</td>
        <td>${credit}</td>
        <td style="white-space:nowrap">${act}</td>
      </tr>`;
    }).join('');

    renderGrowthGroups();
  }

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
function reset() { for (const k in els) { const e = els[k]; e.hidden = true; e.className = ''; e.textContent = ''; e.innerHTML = ''; e.onclick = null; } }

function snap(o) {
  return Object.assign({
    uid: 'u1', nickname: '账号 A', tasks_total: 18, tasks: [],
    claimable_count: 0, claimable_credit: 0,
    acceptable_count: 0, acceptable_credit: 0,
    streak_days: 1, energy: 5,
  }, o);
}

// ---------- 1. 「待完成」用 pending_* ----------
console.log('\n[1] 待完成 / 完成后可得 用 pending_*');
reset(); share = [];
growthSnaps = [snap({ pending_count: 5, pending_credit: 600, acceptable_count: 17, acceptable_credit: 2150 })];
renderGrowth(growthSnaps);
let cards = $('growthCards').innerHTML;
ok(cards.includes('>5<'), '待完成用 pending_count=5（不是 acceptable 的 17）');
ok(cards.includes('>600<'), '完成后可得用 pending_credit=600（不是 2150）');
ok(!cards.includes('>17<'), '不显示 acceptable_count=17');
ok(!cards.includes('>2150<'), '不显示 acceptable_credit=2150');
ok(cards.includes('待完成任务') && cards.includes('完成后可获得的积分'), '标签文案正确');

// 兼容旧快照：没有 pending_* 时回落到 acceptable_*
console.log('\n[1b] 旧快照回落');
reset();
growthSnaps = [snap({ acceptable_count: 7, acceptable_credit: 700 })];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('>7<') && cards.includes('>700<'), '缺 pending_* 时回落到 acceptable_*（不显示 0）');

// 多账号求和
console.log('\n[1c] 多账号求和');
reset();
growthSnaps = [
  snap({ uid: 'u1', pending_count: 3, pending_credit: 300 }),
  snap({ uid: 'u2', pending_count: 2, pending_credit: 200 }),
];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('>5<'), '两个账号 pending 求和 = 5');
ok(cards.includes('>500<'), '两个账号 credit 求和 = 500');

// ---------- 2. 持久通知面板 ----------
console.log('\n[2] notice() 持久面板');
reset();
notice('标题', '正文内容');
const n = $('notice');
ok(n.hidden === false, '调用后可见');
ok(n.innerHTML.includes('标题'), '含标题');
ok(n.innerHTML.includes('正文内容'), '含正文');
ok(n.innerHTML.includes('知道了'), '含关闭按钮');
ok(typeof $('noticeClose').onclick === 'function', '关闭按钮绑定了 handler');
$('noticeClose').onclick();
ok(n.hidden === true, '点「知道了」后隐藏');
// 与 toast 的区别：不应挂自动消失定时器
ok(typeof notice._t === 'undefined', 'notice 不设自动消失定时器（与 toast 相反）');
// 转义：正文里的 HTML 不能注入
reset();
notice('x', '<img src=x onerror=alert(1)>');
ok(!$('notice').innerHTML.includes('<img'), '正文被 HTML 转义（无注入）');

// ---------- 3. growthError 的分层处理 ----------
console.log('\n[3] growthError 分层');
// 3a 被前置挡住：toast 中性 + 弹面板
reset();
growthError(new Error('接单 0 个（跳过 17）；需要先完成前置任务：first_buddy（「领取一只 Buddy」—— 需在 WorkBuddy 客户端内完成）'), '接单');
ok($('toast').hidden === false, '仍给一条 toast');
ok(!$('toast').className.includes('bad'), 'toast 不打红（预期内）');
ok($('notice').hidden === false, '额外弹出持久面板');
ok($('notice').innerHTML.includes('领取一只 Buddy'), '面板里带出前置任务的中文指引');
ok($('notice').innerHTML.includes('WorkBuddy 客户端'), '面板说明要去哪里做');

// 3b 普通跳过：只 toast，不弹面板
reset();
growthError(new Error('接单 0 个（跳过 3）'), '接单');
ok($('toast').hidden === false && !$('toast').className.includes('bad'), '跳过：中性 toast');
ok($('notice').hidden === true, '跳过：不弹面板');

// 3c 真故障：红色 toast，不弹面板
reset();
growthError(new Error('接单请求失败: dial tcp timeout'), '接单');
ok($('toast').className.includes('bad'), '真故障：红色 toast');
ok($('notice').hidden === true, '真故障：不弹面板（面板只用于"去做什么"）');

// 3d 无前置信息时不弹面板
reset();
growthError(new Error('需要先完成前置任务但格式不同'), '接单');
ok($('notice').hidden === true, '缺少可解析的前置内容时不弹面板');

// ---------- 4. 静态：两条路径共用 growthError ----------
console.log('\n[4] 单一实现（避免两套提示逻辑）');
const callSites = (html.match(/growthError\(/g) || []).length;
ok(callSites >= 3, 'growthError 至少有 1 处定义 + 2 处调用（实际 ' + callSites + ' 处）');
ok(!/全部\$\{label\}失败：/.test(html), '「全部X失败：」旧写法已移除（改走 growthError）');
ok(html.includes('id="notice"'), '页面里有 #notice 元素');
ok(/\.notice\.warn\{/.test(html), '.notice.warn 样式已定义');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
