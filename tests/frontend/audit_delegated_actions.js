// audit_delegated_actions.js —— 动态行按钮的**契约一致性**检查。
//
// # 为什么需要
//
// 前两轮各抓到一个"点了没反应"的静态按钮（清屏、任务调度刷新）。
// 但界面上大量按钮是**模板动态生成**的（账号行的签到/禁用它、成长行的
// 领取/接单、旅行行的派猫/领奖…），它们靠**事件委托**处理。
//
// 静态按钮漏绑 = 按钮没反应；
// 委托按钮漏配 = 点了没反应**或**报错。
//
// # 判据：模板发出的 data-* 动作 vs 委托里实际处理的动作
//
//   1. 收集源码里所有 `data-xxx="..."` 的发出点（模板字符串里写的）
//   2. 收集所有委托 handler 里读的 `dataset.xxx`
//   3. 差集 = 发出了但没人处理的动作（点了没反应）
//
// 反向也查：handler 读的动作没有任何地方发出 = 死代码。
'use strict';
const fs = require('fs');
const path = require('path');

const REPO = path.resolve(__dirname, '..', '..');
const WEBUI = process.env.AUDIT_WEBUI || path.join(REPO, 'internal/server/webui.html');
const src = fs.readFileSync(WEBUI, 'utf8');

// 我们关心的动作类 data 属性（data-provider / data-cap 是布局元数据，不算动作）
const ACTION_PREFIX = ['data-act', 'data-gact', 'data-gclaim', 'data-gclaimreward',
  'data-gclaimall', 'data-tact', 'data-tclaim', 'data-plist', 'data-uid'];

console.log('=== 事件委托动作一致性 ===\n');
console.log('文件: ' + WEBUI + '\n');

// 1) 收集"发出"的动作：data-xxx="值"
const emitted = new Map();   // attr -> Set(values)
for (const attr of ACTION_PREFIX) {
  const re = new RegExp(attr + '="([^"$]*)"', 'g');   // 排除 ${} 动态值
  const vals = new Set();
  for (const m of src.matchAll(re)) {
    if (m[1] && !m[1].includes('$')) vals.add(m[1]);
  }
  if (vals.size) emitted.set(attr, vals);
}

// 2) 收集"处理"的动作：dataset.xxx 或 ev.target.dataset.xxx
const handled = new Map();
for (const attr of ACTION_PREFIX) {
  const camel = attr.replace(/^data-/, '').replace(/-([a-z])/g, (_, c) => c.toUpperCase());
  const re = new RegExp('dataset\\.' + camel + '\\b', 'g');
  const n = (src.match(re) || []).length;
  if (n) handled.set(attr, n);
}

console.log('=== 模板发出的动作（值） ===');
for (const [attr, vals] of [...emitted].sort()) {
  console.log('  ' + attr.padEnd(20) + ' → ' + [...vals].sort().join(', '));
}
console.log('');

console.log('=== 委托里读取的动作（出现次数） ===');
for (const [attr, n] of [...handled].sort()) {
  console.log('  ' + attr.padEnd(20) + ' 被读 ' + n + ' 次');
}
console.log('');

// 3) 差集：发出了但没被读
let problems = 0;
console.log('=== 检查 ===');
for (const [attr, vals] of emitted) {
  if (!handled.has(attr)) {
    console.log('  ✗ ' + attr + ' 被模板发出（' + vals.size + ' 种值），但**没有任何 handler 读取它**');
    console.log('      值: ' + [...vals].sort().join(', '));
    problems++;
  }
}
// 反向：读了但没发出
for (const [attr] of handled) {
  if (!emitted.has(attr)) {
    console.log('  ⚠ ' + attr + ' 被 handler 读取，但模板里没找到对应的发出点（可能是动态值或死代码）');
  }
}
if (problems === 0) console.log('  所有发出的动作都有对应 handler');

console.log('');
console.log('合计: ' + problems + ' 个动作缺少 handler');
process.exit(problems ? 1 : 0);
