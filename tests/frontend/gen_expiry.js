// [auto-fix] 已指向本仓库（可用环境变量 WB2API_REPO 覆盖）
// 生成自包含测试：成长计划任务明细的「到期」列。
//
// 用户要求：任务有一部分带到期时间，希望能显示出来。
//
// 实测依据（真实账号）：上游 18 个任务里 4 个带 valid_end，
// 例如 Expert_Philanthropy=2026-09-30T23:59:00+08:00（当天起算剩 18 天）。
//
// 关键边界：
//   - 没有期限的任务显示占位符，不是空白也不是 1970
//   - 已过期显示「已过期 N 天」而不是负数天
//   - 7 天内标黄提醒
//   - 表头列数与行内 td 数必须一致（差一列会让整张表错位）
const fs = require('fs');

// (process.env.WB2API_REPO || __dirname + '/../..')：仓库根。本文件位于 tests/frontend/，距根两级。
// 用 __dirname 而不是相对 cwd —— 从任何目录调用都能找对位置。
// WB2API_REPO 可覆盖（变异扫描用它指向一次性副本）。

const path = require('path');
const WEBUI = (process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html';
const html = fs.readFileSync(WEBUI, 'utf8');

function grab(marker) {
  const start = html.indexOf(marker);
  if (start < 0) throw new Error('找不到: ' + marker);
  let i = html.indexOf('{', start), d = 0;
  for (; i < html.length; i++) {
    if (html[i] === '{') d++;
    else if (html[i] === '}') { d--; if (d === 0) return html.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}

const src = grab('function fmtTaskExpiry(');

const header = `
const fs = require('fs');
const html = fs.readFileSync(${JSON.stringify(WEBUI)}, 'utf8');
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
`;

const tail = String.raw`
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// ---------- 1. 三档语义 ----------
console.log('\n[1] 到期状态的三档显示');
const future = fmtTaskExpiry({ expires_at: '2026-11-13T15:59:00Z', expires_in_days: 63 });
ok(future && future.text.includes('11-13') && future.text.includes('63'), '远期显示日期与天数（实际：' + (future && future.text) + '）');
ok(future && future.cls === '', '远期不加警示色');

const soon = fmtTaskExpiry({ expires_at: '2026-09-15T15:59:00Z', expires_in_days: 4, expiring_soon: true });
ok(soon && soon.text.includes('剩 4 天'), '7 天内显示「剩 N 天」（实际：' + (soon && soon.text) + '）');
ok(soon && soon.cls === 'warn', '7 天内标黄');

const expired = fmtTaskExpiry({ expires_at: '2026-09-01T15:59:00Z', expires_in_days: -10, expired: true });
ok(expired && expired.text.includes('已过期') && expired.text.includes('10'),
  '已过期显示「已过期 N 天」且 N 为正（实际：' + (expired && expired.text) + '）');
ok(expired && !expired.text.includes('-'), '不显示负数天');
ok(expired && expired.cls === 'err', '已过期标红');

// ---------- 2. 无期限的任务 ----------
console.log('\n[2] 没有期限的任务');
ok(fmtTaskExpiry({}) === null, '空对象 → null（渲染成占位符）');
ok(fmtTaskExpiry({ task_code: 'x', status: 'claimed' }) === null, '无 expires 字段 → null');
ok(fmtTaskExpiry(null) === null, 'null 输入安全');
ok(fmtTaskExpiry(undefined) === null, 'undefined 输入安全');
// 服务端对已领取任务不带这两个字段
ok(fmtTaskExpiry({ expires_at: undefined, expires_in_days: undefined }) === null,
  '已领取任务（服务端不带字段）→ null');

// ---------- 3. 边界与脏数据 ----------
console.log('\n[3] 边界与脏数据');
ok(fmtTaskExpiry({ expires_at: 'garbage', expires_in_days: 5 }) !== null,
  '日期格式怪但天数可用时仍显示（天数才是判据）');
const zero = fmtTaskExpiry({ expires_at: '2026-09-11T15:59:00Z', expires_in_days: 0 });
ok(zero && zero.text.includes('剩 0 天'), '当天到期显示「剩 0 天」而不是负数');
const oneDay = fmtTaskExpiry({ expires_at: '2026-09-12T15:59:00Z', expires_in_days: 1 });
ok(oneDay && oneDay.cls === 'warn', '剩 1 天标黄');

// ---------- 4. 列数一致性（加列最容易错的地方）----------
console.log('\n[4] 表头列数与行内 td 数一致');
//
// # 为什么不能简单地 indexOf('<th>任务</th>')（T9 踩到的坑）
//
// T9 新增的「任务调度」面板表头第一列**也叫「任务」**，于是
// indexOf 命中的是新表的表头，数出 6 列，去跟成长明细行的 7 个 td 比 ——
// 假红。
//
// 正确做法：用**成长明细表独有的列**定位它，并且用
// </tr></thead> 收尾（而不是任何 </thead>）。
//
// 另注：本文件是 String.raw 模板，注释里**不能出现反引号** ——
// 反引号会提前终止模板，generator 报错但陈旧的产物照样全绿（已踩过两次）。
const GRID_HEAD_ANCHOR = '<th class="gtask">任务</th>';
const headAnchorIdx = html.indexOf(GRID_HEAD_ANCHOR) >= 0
  ? html.indexOf(GRID_HEAD_ANCHOR)
  : html.indexOf('<th>任务</th><th>说明</th>');   // 兼容旧写法
ok(headAnchorIdx >= 0, '定位到成长明细表头');
const headEnd = html.indexOf('</tr></thead>', headAnchorIdx);
const headHtml = html.slice(headAnchorIdx, headEnd < 0 ? headAnchorIdx + 800 : headEnd);
const thead = /<th>说明<\/th>/.test(headHtml) ? headHtml : '';
ok(!!thead, '找到任务明细表头（用「说明」列确认是成长表而非任务调度表）');
ok(thead && thead.indexOf('到期') >= 0, '表头含「到期」列');
const headCount = (headHtml.match(/<th/g) || []).length;

// 只数**这一行**里的 td，而不是"从它开始的 900 个字符"。
//
// 原先固定取 rowStart 之后 900 字符再数 <td>。那个窗口是拍出来的：
// 行短了会漏数、行变长或附近多了别的内容就会把相邻表格的 td 数进来。
const rowStart = html.indexOf('<td class="gtask">');
const rowEnd = html.indexOf('</tr>', rowStart);
const rowTds = (html.slice(rowStart, rowEnd < 0 ? rowStart + 900 : rowEnd).match(/<td[ >]/g) || []).length;
ok(headCount === rowTds, '表头 ' + headCount + ' 列 = 行内 ' + rowTds + ' 个 td（不一致会让整张表错位）');

// colspan 用于空态行，也要与列数一致
const colspan = /growthRows'\)\.innerHTML = '<tr><td colspan="(\d+)"/.exec(html);
ok(!colspan || Number(colspan[1]) >= 6, 'colspan 合理（' + (colspan ? colspan[1] : '未找到') + '）');

// ---------- 5. 已有样式类存在 ----------
console.log('\n[5] 样式');
ok(/\.ggroup td\.exp\{/.test(html), '有 .exp 列样式');
ok(/\.err\{|\.warn\{/.test(html), 'warn/err 类在样式表里有定义');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
`;

fs.writeFileSync('expiry_gen.js', header + '\n' + src + '\n' + tail);
console.log('生成: expiry_gen.js');
