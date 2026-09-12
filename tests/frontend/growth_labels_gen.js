
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const els = {};
function mkEl(id) { return { id, hidden: true, className: '', textContent: '', innerHTML: '', disabled: false, title: '' }; }
for (const id of ['growthCards','growthRows','growthGroups','growthAccount','growthViews','growthTaskMeta']) els[id] = mkEl(id);
els.growthViews.querySelectorAll = () => [];
const $ = id => els[id] || (els[id] = mkEl(id));
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
let share = [], growthSnaps = [];
function renderGrowthGroups() {}

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

function renderWith(claim, credit) {
  for (const k in els) { els[k].innerHTML = ''; els[k].textContent = ''; }
  share = [];
  growthSnaps = [{
    uid: 'u1', nickname: '账号 A', tasks_total: 18, tasks: [],
    claimable_count: 0, claimable_credit: 0,
    acceptable_count: claim, acceptable_credit: credit,
    streak_days: 1, energy: 5,
  }];
  renderGrowth(growthSnaps);
  return $('growthCards').innerHTML;
}

// ---------- 1. 卡片文案 ----------
console.log('\n[1] 卡片标签');
const cards = renderWith(17, 2150);
ok(cards.includes('待完成任务'), '含「待完成任务」');
ok(cards.includes('完成后可获得的积分'), '含「完成后可获得的积分」');
ok(!cards.includes('待接单任务'), '不再有「待接单任务」');
ok(!cards.includes('接单后可得积分'), '不再有「接单后可得积分」');
ok(!cards.includes('信用分'), '统一用「积分」，不再出现「信用分」');
// 数值仍然正确渲染
ok(cards.includes('>17<'), '待完成数量 17 正常渲染');
ok(cards.includes('>2150<'), '完成后可得 2150 正常渲染');

// ---------- 2. 静态检查：HTML 里也不该残留旧措辞 ----------
console.log('\n[2] 全局无残留旧措辞');
ok(!html.includes('待接单任务'), '页面无「待接单任务」');
ok(!html.includes('接单后可得积分'), '页面无「接单后可得积分」');
ok(!html.includes('<th>待接单</th>'), '表头不再是「待接单」');
ok(!html.includes('<th>接单后可得</th>'), '表头不再是「接单后可得」');
// 表格新表头
ok(html.includes('<th>待完成</th>') && html.includes('<th>完成后可得</th>'),
   '表头已改为「待完成 / 完成后可得」');

// ---------- 3. 「接单」作为动作词仍保留（不是要删掉它） ----------
console.log('\n[3] 动作词「接单」保留');
ok(html.includes('>接单</button>'), '行内按钮仍叫「接单」（那是实际动作）');
ok(html.includes('全部接单'), '「全部接单」按钮保留');
ok(html.includes("accept: '接单'"), 'GROWTH_LABEL 里 accept 映射仍是「接单」');

// ---------- 4. 接单按钮带 tooltip 说明不发奖励 ----------
console.log('\n[4] 接单按钮 tooltip');
const m = /data-gact="accept"[^>]*title="([^"]*)"/.exec(html);
ok(!!m, '接单按钮有 title 属性');
if (m) {
  ok(m[1].includes('不发奖励'), 'tooltip 说明「不发奖励」（实际：' + m[1] + '）');
  ok(m[1].includes('领取'), 'tooltip 指引去看「领取」');
}

// ---------- 5. 渲染出的行内按钮也带 tooltip ----------
console.log('\n[5] 渲染结果里的 tooltip');
for (const k in els) { els[k].innerHTML = ''; els[k].textContent = ''; }
share = [{ uid: 'u1', nickname: '账号 A', credits: 100 }];
growthSnaps = [{
  uid: 'u1', nickname: '账号 A', tasks_total: 18, tasks: [],
  claimable_count: 0, claimable_credit: 0,
  acceptable_count: 3, acceptable_credit: 300,
  streak_days: 1, energy: 5,
}];
renderGrowth(growthSnaps);
const rows = $('growthRows').innerHTML;
ok(rows.includes('data-gact="accept"'), '行内渲染出接单按钮');
ok(/data-gact="accept"[^>]*title="[^"]*不发奖励/.test(rows), '行内接单按钮带「不发奖励」tooltip');

// ---------- 6. 错误提示的中性/红色判定要和后端文案对齐 ----------
console.log('\n[6] 前端 toast 的良/恶性判定');
// 从页面里抽出真实的 benign 正则，避免测试自说自话
const benignRe = /const benign = (\/[^/]+\/)\.test/.exec(html);
ok(!!benignRe, '页面里能抽出 benign 判定正则');
if (benignRe) {
  const re = eval(benignRe[1]);
  // 这些是"现在不该做"，应显示为中性（不打红）
  const benignMsgs = [
    '接单 0 个（跳过 17）；被前置任务 first_buddy 挡住',
    '接单 0 个（跳过 2）',
    '没有待接单的任务',
    '任务「x」当前不可接单',
    '成长次数不足',
    '该任务已领取',
  ];
  for (const m of benignMsgs) ok(re.test(m), '中性：' + m);

  // 这些是真故障，必须打红
  const failMsgs = [
    '接单请求失败: dial tcp timeout',
    '拉取任务失败: 502 Bad Gateway',
    '接单 0 个（失败 1）；最后错误 x: internal server error',
  ];
  for (const m of failMsgs) ok(!re.test(m), '打红：' + m);
}

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
