// lib_fixtures.js —— "数据前置"类探针的**共享夹具层**。
//
// # 它解决什么问题
//
// 有一批老探针的判据是**渲染结果**（旅行详情弹窗的内容、成长计划的分组结构、
// 账号行的动作按钮…），而它们所在的那个 18080 合成实例里**没有上游数据**：
// `/admin/travel` 回 `{"accounts":[]}`、`/admin/growth` 回 `{"accounts":[]}`。
// 于是那些判据在"空数据"上红 —— 那是**数据前置缺失**，不是产品缺陷。
//
// # 为什么打桩在 fetch 层（而不是造一个 Node 桩上游、也不是替换产品函数）
//
// 三种办法的取舍：
//
//	① 造一个假上游（本地 HTTP 服务，冒充 loomy/workbuddy）——
//	   要**重新实现一遍上游协议**（登录、travel、growth 的接口形状与错误语义）。
//	   那条协议 Go 侧已有 httptest 全量覆盖（internal/workbuddy/*_test.go、
//	   internal/admin/checkin_all_test.go），在 Node 里再写一份就是同一条协议的
//	   **第二份实现**：维护成本翻倍，且一定会与真协议漂移。
//	② 替换产品函数（`window.__wb2api__.renderTravel = 假函数`）——
//	   等于把被测代码本身换掉，断言退化成"我自己的假函数长得对"。
//	③ **只替换 HTTP 回执**（本文件）—— 产品的 renderTravel / travelAct /
//	   showInfo / 事件委托 / renderGrowthGroups **全部真跑**，
//	   桩只负责"上游那边有数据"这一件事。下游每一层都是真代码。
//
// 选 ③。它不是"造假"，是**夹具**：与 Go 侧 httptest 用假上游喂真 handler 同一种手法，
// 只是夹具边界画在浏览器这一侧。
//
// # 装桩时机（很重要）
//
// `loadTravel` / `loadGrowth` 都走 `sharedGet('ep:…')`（30 秒缓存）。
// 桩必须在**面板首次加载之前**装好，否则第一次回源已经把空数据写进缓存，
// 30 秒内再刷新都不会走 fetch。所以装桩要放在 `Page.navigate` 之后、
// 第一次点导航之前。
//
// # 幂等
//
// 两个函数都只包装一次 `window.fetch`（用 `__fixturePatched` 标记），重复调用安全。
'use strict';

// ---------------------------------------------------------------- 旅行夹具
//
// 三个账号覆盖三种状态，让下面每条判据都有东西可测：
//   · traveling（在途）+ 今日已派送 → 状态列「旅行中」、派猫 disabled、领奖 disabled
//   · arrived（已到站）             → 「已到站」、领奖可点、有奖励分
//   · idle（空闲）                  → 「空闲」、派猫可点
// `has_buddy` 全为 true：没有猫的账号连「派猫」都没意义，测不到委托路径。
//
// 字段名严格照 `renderTravel` 的读取面（webui.html:10831-10876）：
// uid / nickname / has_buddy / buddy_name / buddy_rarity / state / error /
// daily_limit_reached / depart_at / arrive_at / remaining_sec / reward_credit
const TRAVEL_ACCOUNTS = [
  {
    uid: 'probe-travel-1', nickname: '旅行夹具甲', has_buddy: true,
    buddy_name: '橘座', buddy_rarity: 'SR', state: 'traveling',
    daily_limit_reached: true, depart_at: 1760000000, arrive_at: 1760014400,
    remaining_sec: 3600, reward_credit: 0,
  },
  {
    uid: 'probe-travel-2', nickname: '旅行夹具乙', has_buddy: true,
    buddy_name: '三花', buddy_rarity: 'SSR', state: 'arrived',
    daily_limit_reached: true, depart_at: 1759990000, arrive_at: 1760004400,
    remaining_sec: 0, reward_credit: 120,
  },
  {
    uid: 'probe-travel-3', nickname: '旅行夹具丙', has_buddy: true,
    buddy_name: '奶牛', buddy_rarity: 'R', state: 'idle',
    daily_limit_reached: false, depart_at: 0, arrive_at: 0,
    remaining_sec: 0, reward_credit: 0,
  },
];

// `/admin/travel/status?uid=…` 的回执，形状照 `travelAct('detail')` 的读取面
//（webui.html:10900-10917）：buddy{name,rarity,personality} / travel{state,location.name,
//  duration_hours,depart_at,arrive_at,reward_credit,daily_limit_reached,record_id} /
//  remaining_sec / clock_skew_sec / travel_error
function travelStatusFor(uid) {
  const a = TRAVEL_ACCOUNTS.find(x => x.uid === uid) || TRAVEL_ACCOUNTS[0];
  return {
    buddy: { name: a.buddy_name, rarity: a.buddy_rarity, personality: '探针夹具性格' },
    travel: {
      state: a.state,
      location: { name: '探针夹具目的地' },
      duration_hours: 4,
      depart_at: a.depart_at,
      arrive_at: a.arrive_at,
      reward_credit: a.reward_credit,
      daily_limit_reached: a.daily_limit_reached,
      record_id: 'rec-probe-' + a.uid,
    },
    remaining_sec: a.remaining_sec,
    clock_skew_sec: 0,
    travel_error: '',
  };
}

// ---------------------------------------------------------------- 成长夹具
//
// 字段名照 `renderGrowth(list)` / `growthCounts(g)` 的读取面（webui.html:8416-8482）：
// uid / nickname / pending_count / pending_credit / acceptable_count / acceptable_credit /
// streak_days / energy / claimable_credit / claimable_count / tasks_total
//
// ⚠ 两个账号而不是一个：只有一个的话"按账号分组/切换"那些判据无从下手。
const GROWTH_ACCOUNTS = [
  {
    uid: 'probe-growth-1', nickname: '成长夹具甲',
    acceptable_count: 2, acceptable_credit: 100,
    pending_count: 3, pending_credit: 180,
    claimable_count: 1, claimable_credit: 60,
    streak_days: 7, energy: 50, tasks_total: 5, tasks_done: 2,
    observed_at: '2026-10-19T08:00:00Z',
    tasks: [
      { task_code: 'daily-login', title: '每日登录', state: 'acceptable', credit: 50, description: '登录一次' },
      { task_code: 'invite-friend', title: '邀请好友', state: 'acceptable', credit: 50, description: '邀请一位好友' },
      { task_code: 'run-once', title: '跑一次对话', state: 'running', credit: 80, description: '发起一次对话' },
      { task_code: 'claim-gift', title: '领取礼包', state: 'done', credit: 60, description: '领取成长礼包' },
    ],
  },
  {
    uid: 'probe-growth-2', nickname: '成长夹具乙',
    acceptable_count: 1, acceptable_credit: 30,
    pending_count: 1, pending_credit: 30,
    claimable_count: 0, claimable_credit: 0,
    streak_days: 3, energy: 20, tasks_total: 2, tasks_done: 1,
    observed_at: '2026-10-19T08:00:00Z',
    tasks: [
      { task_code: 'daily-login', title: '每日登录', state: 'acceptable', credit: 30, description: '登录一次' },
    ],
  },
];

// ---------------------------------------------------------------- 通用装桩器
//
// 在页内包装 window.fetch。两道纪律：
//   · 只拦**精确端点**（含 query 前缀匹配），别的一律走原 fetch：
//     拦宽了会把 /admin/accounts 这类真实数据也顶掉，那是另一种假。
//   · 返回真 `Response` 对象（不是裸对象）—— 产品的 admin() 直接调
//     `r.ok` / `r.json()`，裸对象会在别处炸出与夹具无关的错。
const PATCH_FN = `
window.__fixturePatched = window.__fixturePatched || { travel: false, growth: false };
function __fixtureInstall(kind, table) {
  if (window.__fixturePatched[kind]) return 'already';
  var real = window.fetch.bind(window);
  window.fetch = function (input, init) {
    var url = (typeof input === 'string') ? input : ((input && input.url) || '');
    for (var i = 0; i < table.length; i++) {
      if (url.indexOf(table[i].path) >= 0) {
        var body = (typeof table[i].body === 'function')
          ? table[i].body(url) : table[i].body;
        return Promise.resolve(new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }));
      }
    }
    return real(input, init);
  };
  window.__fixturePatched[kind] = true;
  return 'installed';
}
`;

/**
 * 装旅行夹具。返回 'installed' / 'already' / 'err:…'。
 *
 * ⚠ 必须早于第一次打开「猫猫旅行」面板。
 */
async function fixTravel(ev) {
  // 三条 status 回执按 uid 索引，页内按 query 取。
  const statusTable = {};
  for (const a of TRAVEL_ACCOUNTS) statusTable[a.uid] = travelStatusFor(a.uid);
  const listBody = JSON.stringify({
    accounts: TRAVEL_ACCOUNTS, auto_claim: true, auto_depart: true, checkin_hours: [9, 21],
  });
  return ev(`(function(){
    try {
      ${PATCH_FN}
      var STATUS = ${JSON.stringify(statusTable)};
      return __fixtureInstall('travel', [
        // ⚠ 顺序：更长的路径必须排在前面。前缀匹配是 indexOf，
        //   若 '/admin/travel' 排在前，'/admin/travel/status?uid=…' 会被它先吃掉。
        { path: '/admin/travel/status', body: function (u) {
            var m = /[?&]uid=([^&]*)/.exec(u);
            var uid = m ? decodeURIComponent(m[1]) : '';
            return STATUS[uid] || STATUS[${JSON.stringify(TRAVEL_ACCOUNTS[0].uid)}];
          } },
        { path: '/admin/travel', body: ${listBody} },
      ]);
    } catch (e) { return 'err:' + e.message; }
  })()`);
}

/**
 * 装成长夹具。返回 'installed' / 'already' / 'err:…'。
 *
 * ⚠ 必须早于第一次打开「成长计划」面板。
 */
async function fixGrowth(ev) {
  return ev(`(function(){
    try {
      ${PATCH_FN}
      return __fixtureInstall('growth', [
        { path: '/admin/growth/auto/actions',
          body: { actions: [{ task_code: 'daily-login' }, { task_code: 'run-once' },
                            { task_code: 'claim-gift' }, { task_code: 'invite-friend' }] } },
        { path: '/admin/growth',
          body: ${JSON.stringify({ accounts: GROWTH_ACCOUNTS })} },
      ]);
    } catch (e) { return 'err:' + e.message; }
  })()`);
}

/**
 * 点开面板后**强制绕开页内共享缓存**。
 *
 * # 为什么需要这一步
 *
 * `loadTravel` / `loadGrowth` 的非刷新路径走 `sharedGet('ep:/admin/…')`，
 * 那是**30 秒 TTL 的共享缓存**。夹具是在页面加载**之后**才装的，而首次回源
 * 可能已经把"空数据"写进缓存 ⇒ 出现"夹具明明装上了，面板却还是空的"。
 *
 * 面板自己的「刷新」按钮走 `?refresh=1` 分支（`admin()` 直连、绕开缓存），
 * 所以点它就是产品里现成的、正当的绕开路径 —— 也是用户看到空面板时会做的事。
 * 这不是测试作弊：它不改变产品行为，只是把"用户会点刷新"这一步补上。
 *
 * @param {string} which 'travel' | 'growth'
 */
async function forceReload(ev, which) {
  const ids = { travel: 'btnTravelRefresh', growth: 'btnGrowthRefresh' };
  const id = ids[which];
  if (!id) return 'err:unknown-panel';
  return ev(`(function(){
    var b = document.getElementById(${JSON.stringify(id)});
    if (!b) return 'no-btn';
    if (b.disabled) return 'busy';
    b.click();
    return 'clicked';
  })()`);
}

/**
 * 装夹具 + 点刷新 + 有界等待就绪特征，一步到位。
 *
 * @param {string} which  'travel' | 'growth'
 * @param {string} readyJs 页内表达式，返回"已渲染的条目数"
 * @param {number} readyMin 认为就绪的最小值
 * @returns {Promise<string>} 形如 'installed/clicked/ready:3'
 */
async function prepare(ev, which, readyJs, readyMin) {
  const inst = which === 'travel' ? await fixTravel(ev) : await fixGrowth(ev);
  const clicked = await forceReload(ev, which);
  let n = 0;
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    n = Number(await ev(readyJs)) || 0;
    if (n >= (readyMin || 1)) break;
    await new Promise(r => setTimeout(r, 400));
  }
  return inst + '/' + clicked + '/ready:' + n;
}

module.exports = {
  fixTravel,
  fixGrowth,
  forceReload,
  prepare,
  TRAVEL_ACCOUNTS,
  GROWTH_ACCOUNTS,
};
