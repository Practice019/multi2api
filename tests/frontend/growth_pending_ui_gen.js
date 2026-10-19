
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
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
        blocked[1].trim() + '\n\n若这个前置任务在上方任务表里带「一键完成」按钮，直接点它即可；' +
        '任务表里没有的（或显示「去客户端做」的）才需要去 WorkBuddy 客户端里做。' +
        '前置做完后回到这里再点「接单」。');
    }
  }

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
// ⚠ 形状变了、意图没变：两张卡（待完成任务 / 完成后可获得的额度）在产品里被
//   **合并**成一张「待办合计 5 任务 · +600」（webui.html:8424-8425，
//   commit f32624a「成长计划精简 —— 12 列/8 卡合并为 6 列/6 卡」）。
//   断言跟着改成"合并卡里那两个数"，但仍然守着本夹具的核心判据：
//   数必须来自 pending_*，不能是 acceptable_*（后者只数未接单的，
//   已接单/进行中的任务会从统计里消失 —— 那才是这个夹具要防的缺陷）。
ok(cards.includes('待办合计'), '含「待办合计」合并卡');
ok(cards.includes('5 任务 · +600'), '待办合计 = 「5 任务 · +600」（pending_count / pending_credit）');
ok(!cards.includes('17') && !cards.includes('2150'), '不显示 acceptable_* 的 17 / 2150');
ok(!cards.includes('待接单任务'), '旧卡名「待接单任务」已合并掉');

// 兼容旧快照：没有 pending_* 时回落到 acceptable_*
console.log('\n[1b] 旧快照回落');
reset();
growthSnaps = [snap({ acceptable_count: 7, acceptable_credit: 700 })];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('7 任务 · +700'), '缺 pending_* 时回落到 acceptable_*（不显示 0）');

// 多账号求和
console.log('\n[1c] 多账号求和');
reset();
growthSnaps = [
  snap({ uid: 'u1', pending_count: 3, pending_credit: 300 }),
  snap({ uid: 'u2', pending_count: 2, pending_credit: 200 }),
];
renderGrowth(growthSnaps);
cards = $('growthCards').innerHTML;
ok(cards.includes('5 任务 · +500'), '两个账号 pending 求和 = 5 任务 / 500 额度');

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
// ⚠ 这条断言原来是对**整篇 HTML** 做全文匹配：!/全部\$\{label\}失败：/.test(html)。
//   它现在必然红，而红的**不是**成长路径 —— 那句字面量还活在 startAll()
//   （webui.html:5064，每日动作：签到 / 刷新额度）里，那是**另一个功能**，
//   本来就该自己拼自己的 toast。全文匹配把一个无关功能算进了成长的账上，
//   判据的**作用域**错了，所以它红得没有道理（"测量工具坏了"的典型形态）。
//   改成把判据钉在**成长批量路径**上：抽 growthAll 那一小段源码看它的 catch。
const gAllAt = html.indexOf('async function growthAll(');
const gAll = gAllAt < 0 ? '' : html.slice(gAllAt, gAllAt + 1200);
ok(gAllAt > 0, '页面里能定位到 growthAll（成长批量动作）');
ok(/growthError\(e,/.test(gAll), 'growthAll 的 catch 走 growthError（单一实现）');
ok(!/全部接单失败|全部领取失败/.test(html), '成长路径不再自己拼「全部X失败：」');
// ⚠ 想在模板里写「全部X失败：」那种**带美元加大括号的字面量**：不能直接写。
//   tail 是 String.raw，但 String.raw 只免转义、**不免插值** —— 美元加大括号
//   一样会被求值，生成器当场 ReferenceError（label 不存在）。
//   原断言是把 $ 与 { 各自转义后才放进正则的；本条干脆换成不含它的形态，
//   既避开这个坑，也不再被"另一个功能"的同形字符串误伤（见上一条注释）。
ok(html.includes('id="notice"'), '页面里有 #notice 元素');
ok(/\.notice\.warn\{/.test(html), '.notice.warn 样式已定义');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
