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
//
// ⚠⚠ 2026-10-19 改锚点：**产品把「任务历史」标签页删了**（用户本轮要求），
//    见 `internal/server/webui.html:1573` 的注释：
//      `<!-- 任务历史标签页已删除（用户本轮要求）。原 section（data-key="workbuddy:history"）…`
//
//    这一条原来断言"存在 data-key=\"workbuddy:history\" 的 section 且带
//    data-provider=\"workbuddy\" / data-cap=\"checkin\""，于是产品**按用户要求删掉之后**
//    守卫一直在红 —— 而红的原因是产品做对了（这是"守卫没跟上产品"的典型：
//    红线指向的动作是"把面板加回来"，正好与用户的要求相反）。
//
//    改后的契约（R3 的**意图**不变：任务历史只能有一份，且归属 checkin 能力位）：
//      1. 静态 section **两份都不能有**（`history` 与 `workbuddy:history`）；
//      2. 删除这件事必须**被注释说明**（"用户本轮要求"）——
//         否则下一个人会以为是误删，把它加回来。删掉那行注释这条就红。
//
//    ⚠ 意图现在由谁承担（2026-10-19 逐行核对，别只看"基线也红"就收工）：
//      · 删除本身 + 原因：`webui.html:1573-1575` 的注释（同一段还写明 **loadHistory 渲染
//        也已一并下线**，只有 checkin 能力位保留）；
//      · checkin 能力位保留的**用处**：账号池「今日签到」列 `webui.html:3654`（列定义）
//        与 `:4687`（单元格渲染），以及调度依赖它 —— 这是 1574-1575 自己写的理由；
//      · ⚠ **需人工裁决**：`webui.html:6013` 的 `checkin: () => loadHistory()` 仍在
//        `PROVIDER_PANEL_RENDERERS` 里，而它上面 6003-6012 的注释还在说"「任务历史」是
//        已经存在的静态面板" —— 与 1573 的删除决定**自相矛盾**，看起来是一条**遗留**。
//        本文件 [4] 仍在断言"checkin 分派到 loadHistory()"（它**现在确实为真**），
//        但它的**意图**（"任务历史面板点开别是空的"）随标签页删除已经失去对象。
//        我按纪律**没有拍板**：要么由你再确认"checkin 能力位还需要专属渲染器"而保留 [4]，
//        要么把 [4] 改成断言"checkin 能力位仍被账号池「今日签到」列与调度依赖"。
//        这一条我不自行改判。
console.log('\n[1] 「任务历史」section 的归属属性（R3）');
const histSec = stripped.match(/<section[^>]*data-key="workbuddy:history"[^>]*>/);
ok(!histSec, '不存在静态 data-key="workbuddy:history" 的 section（产品已按用户要求删除该标签页，见 webui.html:1573）');
if (histSec) console.log('    意外找到: ' + histSec[0].replace(/\s+/g, ' '));
// 旧的裸写法同样必须消失
ok(!/<section[^>]*data-key="history"/.test(stripped),
  '旧的 <section data-key="history"> 已不存在（否则会有两个任务历史）');
// 删除原因必须留在源码里（注释是唯一能让"别加回来"传递下去的地方）
//
// ⚠ 判据钉**HTML 注释形态**（`<!-- 任务历史标签页已删除…`），不是"文件里出现过这句话"：
//   这句话在 `webui.html` 里出现 **2 次** —— `:1573` 的删除说明（HTML 注释，紧贴被删
//   section 的位置）与 `:12676` 的代码注释（`#histKind` 判空的理由）。
//   我第一版用 `/任务历史标签页已删除/` 判"文件里有没有"，做变异测试时发现：
//   把 `:1573` 那句改掉之后断言**仍绿**（因为 `:12676` 还在）—— 变异"没生效"被误当成
//   "判据空转"。这是同一类坑的第三次（前两次：`gen_log_order`/`gen_tokps` 的自比恒真、
//   `verify_t4_t5_static` M1 的字面锚点失效）。**做变异测试时先确认变异真的改到了目标位置。**
ok(/<!--\s*任务历史标签页已删除/.test(normalized),
  '源码里保留了"任务历史标签页已删除（用户本轮要求）"的 HTML 注释（防止被当成误删加回来）');
console.log('    依据: webui.html:' + (normalized.slice(0, normalized.search(/任务历史标签页已删除/)).split('\n').length) + ' 的注释');

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

// ---------------------------------------------------------------- 4. 能力位的承载者
console.log('\n[4] checkin 能力位的承载者（标签页删了，能力位还在被谁用）');
//
// ⚠⚠ 2026-10-19 按父代理裁决改成**选项 B**。原来这条断言的是
//   "`PROVIDER_PANEL_RENDERERS.checkin` 必须有专属渲染器，否则「任务历史」面板点开是空的"。
//   但产品**已按用户要求删掉「任务历史」标签页**（`webui.html:1573-1575`），
//   那个意图（"标签页点开别是空的"）**已经失去对象**。父代理核实：
//     · `loadHistory` **仍在**（定义 `webui.html:9369`，全文件 17 处引用，**与基线一致**，自带 histrows 守卫）；
//     · 但 `PROVIDER_PANEL_RENDERERS.checkin`（`webui.html:6014`）**实际不可达**
//       ⇒ 不再把它当作"面板有内容"的保证。
//   于是判据改成断言 checkin 能力位**真正的承载者**（每条都能指到行，删掉/改掉就红）。
const CHECKIN_CARRIERS = [
  { re: /checkin:\s*\{\s*title:\s*'今日签到'/, why: '账号池「今日签到」列定义（webui.html:3654）' },
  { re: /checkin:\s*`<td>\$\{ck\}<\/td>`/, why: '该列的单元格渲染器（webui.html:4687）' },
  { re: /const\s+QUOTA_ANCHOR\s*=\s*'checkin'/, why: '每日动作按位置切分的锚点（webui.html:4564）' },
  { re: /SPECIAL_PANEL_CAPS\s*=\s*\[[^\]]*'checkin'/, why: '排他表含 checkin ⇒ 不生成通用面板（webui.html:2910）' },
];
for (const c of CHECKIN_CARRIERS) ok(c.re.test(stripped), 'checkin 能力位承载者仍在：' + c.why);
// loadHistory 本身不许被顺手删掉（父代理核实它仍被 17 处引用）
ok(/function\s+loadHistory\s*\(/.test(stripped), 'loadHistory 仍然存在（标签页删了，函数与 histrows 守卫还在）');
// 分派表里那条**遗留项**：存在就必须仍指向 loadHistory（防顺手改坏）；已不在也不再判红（父代理裁定其不可达）
const rpStart = stripped.indexOf('const PROVIDER_PANEL_RENDERERS = {');
ok(rpStart >= 0, '找到 PROVIDER_PANEL_RENDERERS');
if (rpStart >= 0) {
  const rpBody = stripped.slice(rpStart, rpStart + 1400);
  if (/checkin:\s*\(/.test(rpBody)) {
    ok(/checkin:\s*\(\)\s*=>\s*loadHistory\(\)/.test(rpBody),
      '（遗留项在位）checkin 分派仍指向 loadHistory()');
    console.log('      ⚠ 该遗留项父代理已裁定**不可达**（1573 删掉了标签页）；保留断言只为"别顺手改坏"');
  } else {
    console.log('      （分派表里已无 checkin 项 —— 按父代理裁定不判红）');
  }
}
// 跨语言守卫：Go 侧在钉"能力位 ↔ 账号列"一致性（本文件只读它，不改）
const COLS_TEST = path.join(REPO, 'internal/cline/accountcolumns_test.go');
if (fs.existsSync(COLS_TEST)) {
  const ct = fs.readFileSync(COLS_TEST, 'utf8');
  ok(/CapCheckin/.test(ct) && /列集|accountcolumns/.test(ct),
    'Go 侧一致性守卫仍在：internal/cline/accountcolumns_test.go（CapCheckin ↔ 列集含 checkin）');
} else {
  console.log('      （未找到 internal/cline/accountcolumns_test.go —— 跨语言守卫不在本副本，跳过）');
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
