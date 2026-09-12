// verify_t3_t4_static.js —— T3+T4 的**静态**守卫（不需要实例、不需要浏览器）。
//
// # 为什么在浏览器 E2E 之外还要一组静态守卫
//
// 浏览器 E2E（verify_t3_t4.js）需要新鲜实例 + Chrome，属于"集成"层：
// 它证明"这一刻的 18080 页面对"，但跑不动时（实例陈旧/Chrome 抢端口）
// 会给出 ENV 而不是 FAIL —— 那种"跳过"积累起来就等于没有守卫。
//
// 这一组只看**源码里必须存在的形状**，任何机器上都能跑，是 E2E 的地基：
//   · 静态 section 的归属属性（data-provider / data-cap / data-key）
//   · panelCapsOf 的排他机制存在，且**没有**用 `!== 'checkin'` 之类
//     把能力位整个抹掉（规格 R1 的硬要求）
//   · panelCapsOf 的返回值是过滤后的（改了 filter 会让这条红）
//   · 后端 History 端点确实调用了归属过滤
//
// # 边界说明
//
// 静态检查**证明不了行为**，它只证明"这段代码还在"。
// 所以这里的每条断言都刻意做成"删掉/短路某一行就会红"的形状，
// 而不是"出现过某个词"。真正的行为证明在 verify_t3_t4.js（E2E）。
'use strict';
const fs = require('fs');
const path = require('path');

const REPO = (process.env.WB2API_REPO || path.resolve(__dirname, '..', '..'));
const WEBUI = path.join(REPO, 'internal/server/webui.html');
const html = fs.readFileSync(WEBUI, 'utf8');

// 剥注释，避免"注释里写了这段代码"让断言假绿。
//
// ⚠ CRLF 坑（本项目踩过）：`/\/\/.*$/` 里的 `$` 在 `\r` 前匹配不上，
// 于是 CRLF 文件的行注释**整段剥不掉**，扫描器静默失效。
// 所以先把 \r\n 归一成 \n 再剥。
const normalized = html.replace(/\r\n/g, '\n');
const stripped = normalized
  .replace(/<!--[\s\S]*?-->/g, '')
  .replace(/\/\*[\s\S]*?\*\//g, '')
  .replace(/(^|[^:])\/\/.*$/gm, '$1');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// ---------------------------------------------------------------- 1. section 归属
console.log('\n[1] 「任务历史」section 的归属属性（R3）');
const histSec = stripped.match(/<section[^>]*data-key="workbuddy:history"[^>]*>/);
ok(!!histSec, '存在 data-key="workbuddy:history" 的 section');
if (histSec) {
  console.log('    实际标签: ' + histSec[0].replace(/\s+/g, ' '));
  ok(/data-provider="workbuddy"/.test(histSec[0]), '带 data-provider="workbuddy"');
  // data-cap 必须是 checkin —— 它的数据来自 /admin/checkin/history，
  // manifest 里那条路由的归属就是这个能力位。
  // 若有人改成别的（比如 history），hideOrphanPanels 会因"上游没声明该能力位"
  // 把整个面板隐藏 —— 表现是"任务历史凭空消失"。
  ok(/data-cap="checkin"/.test(histSec[0]), '带 data-cap="checkin"（与端点归属一致）');
}
// 旧的裸写法必须消失：留着它就等于"通用区还有一个任务历史"
ok(!/<section[^>]*data-key="history"/.test(stripped),
  '旧的 <section data-key="history"> 已不存在（否则会有两个任务历史）');

// ---------------------------------------------------------------- 2. R1：能力位没被抹掉
console.log('\n[2] R1：checkin 能力位没有被整个抹掉');
// 规格明令禁止的写法：在 panelCapsOf 里用 `!== 'checkin'` 之类直接排除。
// 它能让「签到与保活」消失，但也会让 checkin 能力位从 panelCapsOf 的
// 输出语义里消失 —— 而「任务历史」正挂在这个能力位上。
const forbidden = [
  { re: /cap\s*!==\s*'checkin'/, why: "panelCapsOf 里写死 cap !== 'checkin'" },
  { re: /cap\s*!==\s*"checkin"/, why: 'panelCapsOf 里写死 cap !== "checkin"' },
  { re: /c\s*!==\s*'checkin'/, why: "循环里写死 c !== 'checkin'" },
];
for (const f of forbidden) {
  ok(!f.re.test(stripped), '没有' + f.why + '（那会让能力位整个消失）');
}
// 排他表必须存在，并且真的被用来过滤。
//
// # 判据为什么不写成 /SPECIAL_PANEL_CAPS.indexOf\('checkin'\)/
//
// 那是**抄实现**：断言的形状必须等于代码此刻的写法，于是任何等价改写
//（`SPECIAL_PANEL_CAPS.includes('checkin')`、把表拆成两次 push）
// 都会让守卫假红，而假红最终会被当成噪声忽略 —— 那时真缺陷也一起被忽略了。
//
// 真正要守的是**语义**：checkin 真的在表里。所以按"表字面量里取元素"解析。
ok(/const\s+SPECIAL_PANEL_CAPS\s*=\s*\[/.test(stripped), '存在 SPECIAL_PANEL_CAPS（专属面板已承担的能力位）');
const capTable = stripped.match(/const\s+SPECIAL_PANEL_CAPS\s*=\s*\[([^\]]*)\]/);
if (capTable) {
  const entries = capTable[1].split(',').map(s => s.trim()).filter(Boolean);
  console.log('    排他表内容: [' + entries.join(', ') + ']');
  ok(entries.some(e => e === "'checkin'" || e === '"checkin"'),
    "排他表里真的有 'checkin'");
  ok(entries.some(e => /CORE_CAP/.test(e)), '排他表里有 CORE_CAP（R2）');
} else {
  ok(false, '无法解析 SPECIAL_PANEL_CAPS 的字面量');
}
ok(/function\s+hasSPECIALPanelExcluded\s*\(/.test(stripped), '有 hasSPECIALPanelExcluded 判定函数');
// panelCapsOf 必须以 filter 收口 —— 这是"两条判据只写一遍"的形态。
// 若有人改成在两个 push 点各判一次，这里会红（那正是"判据存两份"的复现）。
const pcoStart = stripped.indexOf('function panelCapsOf(');
ok(pcoStart >= 0, '找到 panelCapsOf');
if (pcoStart >= 0) {
  const pcoBody = stripped.slice(pcoStart, pcoStart + 1600);
  ok(/return\s+out\.filter\(/.test(pcoBody),
    'panelCapsOf 在出口用 filter 统一排除（判据只写一处）');
  ok(/CORE_CAP/.test(pcoBody), 'core 走同一条出口（R2）');
}

// ---------------------------------------------------------------- 3. core 面板
console.log('\n[3] R2：workbuddy:core 不再被生成');
ok(/SPECIAL_PANEL_CAPS\s*=\s*\['checkin',\s*CORE_CAP\]/.test(stripped),
  "排他表包含 CORE_CAP（core 干脆不生成，而不是生成后隐藏）");

// ---------------------------------------------------------------- 4. 渲染分派
console.log('\n[4] checkin 能力位在渲染分派表里有专属渲染器');
// 没有这一条，点开「任务历史」会走 loadGenericList 并被塞进
// #pbody-workbuddy-checkin —— 而那个容器不存在（R1 已让 checkin 不生成通用面板），
// 于是面板点开是空的（"存在、可点、无报错、只是没内容"）。
const rpStart = stripped.indexOf('const PROVIDER_PANEL_RENDERERS = {');
ok(rpStart >= 0, '找到 PROVIDER_PANEL_RENDERERS');
if (rpStart >= 0) {
  const rpBody = stripped.slice(rpStart, rpStart + 900);
  ok(/checkin:\s*\(\)\s*=>\s*loadHistory\(\)/.test(rpBody),
    'checkin 分派到 loadHistory()（否则面板点开是空的）');
}

// ---------------------------------------------------------------- 5. 后端出口过滤
console.log('\n[5] 后端 History 端点真的做了归属过滤（R4）');
const ENDPOINTS = path.join(REPO, 'internal/workbuddy/adminendpoints.go');
const epsrc = fs.readFileSync(ENDPOINTS, 'utf8').replace(/\r\n/g, '\n');
const histStart = epsrc.indexOf('func (h *AdminHandler) History(');
ok(histStart >= 0, '找到 History 端点');
if (histStart >= 0) {
  // 取到函数结束（下一个顶层 func 之前）
  const rest = epsrc.slice(histStart);
  const nextFn = rest.indexOf('\nfunc ', 10);
  const body = nextFn > 0 ? rest.slice(0, nextFn) : rest;
  ok(/h\.ownUIDs\(\)/.test(body), 'Summary: 调用 h.ownUIDs() 取本上游账号集合');
  ok(/own\s*!=\s*nil\s*&&\s*!own\[/.test(body) || /!own\[/.test(body),
    '真的用它做了过滤（存在 !own[...] 的跳过分支）');
  ok(/PageAll\(\)/.test(body), '先取全量再过滤（过滤发生在分页之前）');
  // 反例守卫：不许退回 Page() —— 那会让 total 变成过滤前的数
  ok(!/lg\.Page\(/.test(body),
    'History 不再用 lg.Page()（它的 total 是过滤前的，前端页数会多一页空的）');
}

// ---------------------------------------------------------------- 6. 存储层不认识上游
console.log('\n[6] 判据只有一份：checkinlog 不认识"上游"');
const LOGSRC = path.join(REPO, 'internal/checkinlog/checkinlog.go');
const logsrc = fs.readFileSync(LOGSRC, 'utf8').replace(/\r\n/g, '\n');
ok(/func \(l \*Log\) PageAll\(\)/.test(logsrc), 'checkinlog 提供 PageAll()（全量快照）');
// 若有人把归属判据搬进存储层，这个包就会出现 provider 概念 —— 那是判据分裂的开始。
ok(!/\bprovider\b/i.test(logsrc.replace(/\/\/.*$/gm, '')),
  'checkinlog 的**代码**里不出现 provider（判据不搬进存储层）');

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
