// 验证 accountGroupActions 真的为声明了 quota-probe 的上游渲染出「刷新本上游额度」。
//
// 为什么需要它：后端 manifest 里有能力位 ≠ 前端把按钮画出来了。
// 这中间隔着 hasCap / providerInfo / 模板串拼接三处，任一处错都会
// 导致"能力位有、按钮没有"（用户报的 cline 现象就是这一类）。
const fs = require('fs');

const html = fs.readFileSync('internal/server/webui.html', 'utf8');
const marker = 'function accountGroupActions(';
const i = html.indexOf(marker);
if (i < 0) {
  console.log('FAIL 找不到 accountGroupActions');
  process.exit(1);
}
let depth = 0, end = -1;
for (let k = i; k < html.length; k++) {
  if (html[k] === '{') depth++;
  else if (html[k] === '}') { depth--; if (depth === 0) { end = k; break; } }
}
const fnSrc = html.slice(i, end + 1);

const esc = (s) => String(s).replace(/[&<>"']/g, (c) => (
  { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
));

const MANIFEST = { providers: [] };
// ⚠ 必须**改同一个对象**的 providers，不能 MANIFEST = {...} 重新赋值：
// new Function 建函数时捕获的是那个对象引用，重新赋值对它不可见
//（我第一版就是这么写的，导致 gQuotaCount 恒为 undefined、探针报假红）。
const setProviders = (list) => { MANIFEST.providers = list; };
const hasCap = (pid, cap) => (
  ((MANIFEST.providers.find((x) => x.id === pid) || {}).capabilities || []).includes(cap)
);
const providerInfo = (pid) => MANIFEST.providers.find((x) => x.id === pid);

// ⚠ 保留它：`accountGroupActions` 里可能还有别的按路由后缀渲染的分支，
// 少了这个参数就是 ReferenceError（探针崩，而不是断言失败）。
const adminRouteBySuffix = () => '';

// dailyAllActionsHTML「全部签到」那组按钮的渲染。
//
// # 为什么探针要补注入它（本轮 accountGroupActions 新增了依赖）
//
// `accountGroupActions` 现在会调 `dailyAllActionsHTML(pid)`（卡片头的
// 「全部签到」）与 `hasCap(pid,'import')`（批量导入按钮）——
// 本探针是 `new Function` 单独 eval 那个函数体的，外部依赖必须**显式注入**，
// 少一个就是 `ReferenceError`（不是断言失败，整个探针崩）。
//
// 这里给一个**忠实的替身**：按 manifest 的 daily_actions 渲染，
// 与页面里的实现同形（有 batch+all_url 才出按钮）。替身只求"不崩 +
// 不含额度按钮"，本探针的判据仍然是额度按钮本身。
const dailyAllActionsHTML = (pid) => (MANIFEST.daily_actions || [])
  .filter((a) => a && a.provider === pid && a.batch && a.all_url && a.label)
  .map((a) => `<button class="gact" data-allday="${esc(a.provider)}:${esc(a.id)}">全部${esc(a.label)}</button>`)
  .join('');

const fn = new Function(
  'esc', 'MANIFEST', 'hasCap', 'providerInfo', 'adminRouteBySuffix', 'dailyAllActionsHTML',
  'return ' + fnSrc,
)(esc, MANIFEST, hasCap, providerInfo, adminRouteBySuffix, dailyAllActionsHTML);

let fails = 0;
const check = (name, cond) => {
  console.log((cond ? 'PASS ' : 'FAIL ') + name);
  if (!cond) fails++;
};

// ① 声明了 quota-probe 的上游 → 必须出现分组按钮
setProviders([{
  id: 'cline', account_count: 2,
  capabilities: ['chat', 'models', 'quota-probe'],
  login: { kind: 'device', label: '添加账号' },
}]);
let out = fn('cline');
check('有 quota-probe → 渲染「刷新本上游额度」', out.includes('刷新本上游额度'));
check('带 data-gquota="cline"', out.includes('data-gquota="cline"'));
// ⚠ 必须只看**额度那个按钮**的 title：分组行里还有「添加账号」「重载 auths」
// 等按钮，用裸 includes('2 个') 会被别的 title 命中而给出错误结论
//（我第一版就是这么写的，探针报了假红）。
// ⚠ 从 data-gquota 之后截，别在整个 out 里找 title —— 分组行还有别的按钮
//（我前两版都栽在这：第一次 includes 被「添加账号」的 title 满足，
// 第二次正则命中的仍是那一个，报的是假红）。
const gi = out.indexOf('data-gquota="');
const quotaTitle = (out.slice(gi).match(/title="([^"]*)"/) || [, ''])[1];
check('额度按钮 title 里带账号数（2 个）', quotaTitle.includes('2 个'));
check('额度按钮 title 说明不含其它上游', quotaTitle.includes('不含其它上游'));
check('按钮 class 是 gact（与其它分组按钮同款）', out.includes('class="gact" data-gquota'));

// ② 没有 quota-probe 的上游 → **不得**出现（避免假按钮）
//
// ⚠ 这里用的是**虚构的上游名**（不是真上游）—— 判据只依赖
// `capabilities` 里有没有 quota-probe，与名字无关。
// 早先借用过 `mimo`（那时它确实是"没有 quota-probe"的真实上游），
// 但它已被整体删除，继续用那个名字会让人以为代码里还依赖它。
setProviders([{
  id: 'no-quota-fake', account_count: 1,
  capabilities: ['chat', 'models'],
  login: { kind: 'device', label: '添加账号' },
}]);
out = fn('no-quota-fake');
check('无 quota-probe → 不渲染该按钮', !out.includes('刷新本上游额度'));

// ③ 没有 login 流程的上游（走另一条 return 分支）也要有该按钮
setProviders([{
  id: 'x', account_count: 3,
  capabilities: ['chat', 'quota-probe'],
  login: null,
}]);
out = fn('x');
check('无 login 分支里也渲染该按钮', out.includes('刷新本上游额度'));

// ④ 未注册的上游 → 不渲染任何动作（既有行为不变）
out = fn('unknown-provider');
check('未注册上游 → 空串（既有行为不变）', out === '');

console.log(fails === 0 ? 'ALL PASS' : ('FAILED: ' + fails));
process.exit(fails === 0 ? 0 : 1);
