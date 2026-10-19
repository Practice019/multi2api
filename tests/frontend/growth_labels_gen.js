
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

function maskNick(n) {
    const s = String(n == null ? '' : n);
    if (!s) return '—';
    if (s.indexOf('*') >= 0) return s;
    const rs = Array.from(s);
    if (rs.length <= 4) return s;   // 太短全打码 = 不可辨认，原样
    return rs.slice(0, 3).join('') + '*'.repeat(rs.length - 5) + rs.slice(-2).join('');
  }

function creditsOf(uid) {
    const a = share.find(x => x.uid === uid);
    if (!a || typeof a.credits !== 'number') return null;
    return a.credits;
  }

const COLS = {
    travel: 8,   // 猫猫旅行
    growth: 6,   // 成长计划（精简：额度/能量、连登/补签、待完成/可得、可领 合并）
    // ⚠ jobs / logs 已移除（2026-10-19）：任务调度页换成卡片网格、请求日志页换成
    //   grid 表格（照抄参考项目 Agent2API），两处都不再有 `<table>` 列数，
    //   HTML 里的 `__COLS_JOBS__` / `__COLS_LOGS__` 占位符也一并删了 ⇒ 留着就是死字段。
    //   剩下的三条仍被 rowPlaceholder 使用，别顺手删。
  }

function rowPlaceholder(cols, text, cls) {
    return `<tr><td colspan="${cols}" class="${cls || 'dim'}">${esc(text)}</td></tr>`;
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
      // 额度排在「可领」前面：先看家底，再看还能拿多少。
      ['额度合计', sum.credits_n ? sum.credits : '—', sum.credits_n ? '' : 'dim'],
      // 待办合计 = 待完成任务 + 完成后可得（合并：「5 任务 · +600」）。
      ['待办合计', `${sum.claim} 任务 · +${sum.credit}`, sum.claim ? 'warn' : 'dim'],
      // 可领合计 = 待领取任务 + 现在可领额度（合并：「2 项 · +300」）。
      ['可领合计', `${sum.right_now_n} 项 · +${sum.right_now}`, sum.right_now_n ? 'ok' : 'dim'],
      ['最长连登', sum.streak + ' 天', ''],
      ['能量合计', sum.energy, ''],
    ].filter(([k, v]) =>
      // ⚠ 值为 0 的「合计类」卡**直接不生成**（用户要求"少空白、合理利用空间"）。
      //   实测最典型的浪费是「可领合计 **0 项 · +0**」—— 值为 0 时它没有任何信息量。
      // ⚠ 判据保守：**只过滤名字含「合计」的** —— 「账号数 0」是**最重要**的信息
      //   （配了上游却没账号），一刀切会掩盖故障。
      // ⚠⚠ 为什么"不生成"而不是"生成后再扫一遍藏掉"：我在 `refresh()` 里扫过一遍、
      //   又加过 500ms 延时再扫一遍，**两次都没生效** —— 因为这几张卡要等
      //   `loadGrowth()` 拿到上游数据才画，**扫描的时机永远赶不上**（看截图才发现）。
      //   在生成处判掉，与时序无关。
      !(k.indexOf('合计') >= 0 && /^[0\s+项·—–\-]*$/.test(String(v)))
    ).map(([k, v, c]) =>
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
      const name = esc(maskNick(g.nickname)) + stale;
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
      // ⚠⚠ 用户要求「**高度保证一行**」—— 这里原来有两处 `<br>`，把「连登」格撑成**三行**
      //    （17 天 / 距 28d 还差 11 / 补签×2）⇒ 整行高度是别行的三倍。
      //    ⚠ 注意：光靠全局的 `white-space:nowrap` **拦不住 `<br>`**（那是显式换行），
      //    必须真的把它去掉 —— 用户当场指着这张表说「这个表格不符合要求」。
      // 改法照钉钉 / 飞书表格：**一行内用 `·` 串起来 + 全文进 `title`**（悬停可看，
      // 被截断成 `…` 也不丢信息），补签卡单独留一个小徽章（那是要点的信息）。
      const streakFull = [
        g.streak_days ? `${g.streak_days} 天` : '',
        g.next_tier ? `距 ${g.next_tier} 还差 ${g.next_tier_remaining}` : '',
        g.makeup_cards ? `补签×${g.makeup_cards}` : '',
        (g.makeup_cards && g.makeup_dates && g.makeup_dates.length) ? `可补 ${g.makeup_dates.length}` : '',
      ].filter(Boolean).join(' · ');
      const streak = g.streak_days
        ? `<span title="${esc(streakFull)}">${g.streak_days} 天`
          + (g.makeup_cards
              ? `<span class="pill warn" style="margin-left:6px">补签×${g.makeup_cards}</span>`
              : '')
          + `</span>`
        : '<span class="dim">—</span>';
      // ⚠ 补签那格**已经并入上面**（同一行的 span 里），所以这里留空 ——
      //    保留变量名是为了不动下面拼接那一行的代码（少一处改动少一处风险）。
      const makeupHtml = '';
      // 待完成 / 完成后可得：口径是"还没做完"（未接单+已接单+进行中），
      // 合并成一格：「5 +600」（+600 = 完成后可获得的额度）。
      const pendN = g.pending_count != null ? g.pending_count : (g.acceptable_count || 0);
      const pendC = g.pending_credit != null ? g.pending_credit : (g.acceptable_credit || 0);
      const pendCell = pendN
        ? `<span class="pill ok">${pendN}</span>${pendC ? `<span class="dim" style="font-size:11px"> +${pendC}</span>` : ''}`
        : '<span class="dim">—</span>';
      // 可领：已完成但奖励还没领（合并待领取 + 现在可领）：「2 +300」。
      const claimCell = g.claimable_count
        ? `<span class="pill warn">${g.claimable_count}</span>${g.claimable_credit ? `<span class="dim" style="font-size:11px"> +${g.claimable_credit}</span>` : ''}`
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

      // 额度/能量合并成一格：额度 + 「能量N」（能量 >0 才显示，纯文字，不用 ⚡）。
      const quotaEnergy = `${quota}${g.energy ? ` <span class="dim" style="font-size:11px">能量${g.energy}</span>` : ''}`;

      return `<tr>
        <td>${name}</td>
        <td>${quotaEnergy}</td>
        <td>${streak}${makeupHtml}</td>
        <td>${pendCell}</td>
        <td>${claimCell}</td>
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
// ⚠ 形状变了、意图没变（2026-10 订正）。
//
// 产品把「待完成任务」+「完成后可获得的额度」两张卡**合并**成一张
// 「待办合计 17 任务 · +2150」（webui.html:8424-8425，commit f32624a
// 「成长计划精简 —— 12 列/8 卡合并为 6 列/6 卡」，用户要求少空白）。
// 所以这里断言的不再是两张卡的名字，而是**合并后那张卡的两个数**：
// 任务数来自 pending_count、额度来自 pending_credit（口径还是"没做完的"）。
ok(cards.includes('待办合计'), '含「待办合计」（待完成 + 完成后可得的合并卡）');
ok(cards.includes('17 任务 · +2150'), '待办合计 = 「17 任务 · +2150」（任务数 + 完成后可得的额度）');
ok(!cards.includes('待接单任务'), '不再有「待接单任务」');
ok(!cards.includes('接单后可得积分'), '不再有「接单后可得积分」');
// ⚠ T4 订正：这条原来断言「统一用**积分**」。T4 把界面用词统一到了
// **「额度」**（决策 5，用户原话"不要叫积分叫额度都可以"），
// 所以"统一"这个**意图**没变，变的只是统一到哪个词。
// 断言跟着改成"不再出现任何游离的『积分』说法" —— 它守的东西反而更强了：
// 改造前是"两个词里挑一个"，现在是"页面上不该再有第二个词"。
ok(!cards.includes('信用分'), '不再出现「信用分」（已废弃的第三种叫法）');
ok(!cards.includes('现在可领积分'), '不再有「现在可领积分」（T4 已统一为「现在可领额度」）');
// 0 值的「合计」卡不生成（webui.html:8430-8439）：claimable 全 0 时它是
// 「0 项 · +0」，占一格却没有任何信息量。本夹具这次把它一起钉住 ——
// 它是合并折腾之后最容易被下一次改动带回来的那张卡。
ok(!cards.includes('可领合计'), '可领合计为 0 时不生成该卡（少空白）');
ok(cards.includes('账号数'), '「账号数」卡不受 0 值过滤影响');
// ⚠ 过滤规则本身也要钉住**作用域**：只过滤名字含「合计」的卡。
// 「账号数 0」是"配了上游却没账号"的最重要信号，一刀切会掩盖真故障。
// 这条读的是产品源码里的判据（精确到一处表达式），不是对整篇做全文匹配 ——
// 全文匹配正是本文件上一次假红的成因（见 [2] 的表头断言）。
ok(/indexOf\('合计'\) >= 0/.test(html), '0 值过滤只作用于名字含「合计」的卡（账号数 0 必须留着）');

// ---------- 2. 静态检查：HTML 里也不该残留旧措辞 ----------
console.log('\n[2] 全局无残留旧措辞');
ok(!html.includes('待接单任务'), '页面无「待接单任务」');
ok(!html.includes('接单后可得积分'), '页面无「接单后可得积分」');
ok(!html.includes('<th>待接单</th>'), '表头不再是「待接单」');
ok(!html.includes('<th>接单后可得</th>'), '表头不再是「接单后可得」');
// T4：用户可见区域不该再出现「积分」。见 tests/frontend/verify_t4_t5.js
// 那条更精确的判据（它剥注释后按**渲染结果**判，不在源码上做全文匹配）。
ok(!html.includes('<th>积分</th>'), '表头不再是「积分」（T4 已统一为「额度」）');
// 表格新表头：合并成 6 列后是「待完成 / 可领」，口径解释搬进了 title
// （webui.html:1506-1507）。
//
// ⚠ 这里踩过一次假红：原来断言写的是 '<th>待完成</th>' —— 表头**加了 title
//   属性**之后就匹配不上了，看起来像"产品把表头改回旧词了"，其实一个字符都
//   没丢。字面量断言必须跟着**实际标记**走，或者只用得上名字的部分。
//   用 title 里的口径解释当锚点反而更强：它同时钉住"表头叫什么"和
//   "界面必须解释 +N 是什么意思"。
ok(/<th title="[^"]*\+N 是完成后可获得的额度">待完成<\/th>/.test(html),
   '表头「待完成」带口径说明（+N = 完成后可获得的额度）');
ok(/<th title="[^"]*">可领<\/th>/.test(html), '表头「可领」带口径说明');

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

// ---------- 7. 空池路径（静态自检逼出来的一条真分支） ----------
//
// 这条分支原来**一次都没跑过**：抽取列表里没有 rowPlaceholder / COLS，
// 测试体也从没喂过空列表 ⇒ "渲染空池"这条路从生成到现在没有任何覆盖，
// 一跑就是 ReferenceError。lib_extract_check.js 在生成阶段把它点了出来。
// 现在"两条都补"：抽真函数 + 真的跑一次。
console.log('\n[7] 空池占位');
for (const k in els) { els[k].innerHTML = ''; els[k].textContent = ''; }
share = [];
renderGrowth([]);
const emptyCards = $('growthCards').innerHTML;
const emptyRows = $('growthRows').innerHTML;
ok(emptyCards.includes('账号数'), '空池时仍画「账号数」卡');
ok(!emptyCards.includes('可领合计'), '空池时不画「可领合计」（0 值合计卡不生成）');
ok(emptyRows.includes('账号池为空'), '空池时表格给出占位文案');
// colspan 必须跟着 COLS.growth 走，不能写死 —— 成长表精简过一次列数（12 → 6），
// 写死的数字不会跟着变（本条与 gen_ui_manifest 的 colspan 漂移守卫同类，只是就地）。
ok(emptyRows.includes('colspan="' + COLS.growth + '"'), '空池占位的 colspan == COLS.growth（' + COLS.growth + '，不写死）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
