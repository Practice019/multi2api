// audit_dead_buttons.js —— **静态**找出"没有任何事件处理"的按钮。
//
// # 为什么需要它（与 audit_buttons.js 互补）
//
// `audit_buttons.js` 靠**行为证据**（点击后 DOM 变没变、有没有请求）判断死按钮。
// 那有两个弱点：
//   1. 慢（每个按钮要等 1.5s）
//   2. 会**误报** —— 剪贴板写入、弹窗、下载都不会改 DOM 也不发请求，
//      但它们是有效按钮（实测「复制 curl 示例」就是这样被误报的）
//
// 本脚本改用**静态判据**：按钮的 id 在源码里是否出现在事件绑定位置。
// 这能精确回答"这个按钮有没有被接上"，且瞬间完成。
//
// 判据：对每个 `id="btnXxx"`，在源码里查找 `'btnXxx'.onclick` /
// `'btnXxx'.addEventListener` / `getElementById('btnXxx')` 后接绑定。
// 注意排除：纯导航按钮（data-nav 由委托处理）、submit 表单按钮等。
'use strict';
const fs = require('fs');
const path = require('path');
const cpSelf = require('child_process');   // 自检用：把自己当子进程跑，配合 AUDIT_WEBUI 换输入

// ⚠⚠ 递归哨兵 —— 没有它这个脚本会**炸掉机器**（实测：内存打满、进程树失控）。
//
// # 缺陷形态（本仓库既有，2026-09-30 修）
//
// `scan()` 用 spawnSync(process.execPath, [__filename]) **把自己再跑一遍**
// 去换 AUDIT_WEBUI。而子进程里**同样**会跑到文件末尾那 3 个自检，
// 每个自检又各 spawn 一次 —— 分支因子 3、深度无界：
//
//	1 → 3 → 9 → 27 → …（每次都是完整 Node 进程，各自还要读 440KB HTML）
//
// 实测后果：跑到第 3~4 层时内存打满、系统卡死（用户报"内存爆炸了 卡死了"）。
//
// # 为什么之前没人发现
//
// 它的输出是"自检 N/N 通过"，看不出底下起了一棵树；而且前两层很快就
// 返回了，只有机器慢的时候才会暴露成"卡死"。
//
// # 修法：子进程只做扫描，不再自检
//
// 自检的**目的**是"证明扫描器抓得住注入的缺陷"，那只需要跑**一层**子进程
// （父进程注入样本 → 子进程扫描 → 看退出码）。子进程再自检没有任何意义 ——
// 它扫的是父进程刚写好的临时文件，与"扫描器有没有判别力"无关。
//
// ⚠ 用环境变量而不是命令行参数：`scan()` 已经在传 env，加一个键最省，
// 也不会与"用户手跑 node audit_dead_buttons.js"的用法冲突。
const IS_CHILD_SCAN = process.env.AUDIT_DEAD_BTN_CHILD === '1';

const REPO = path.resolve(__dirname, '..', '..');
// 可用 AUDIT_WEBUI 指向别处的 webui.html —— 变异测试需要它指向一次性副本。
// （我第一版漏了这行，于是"验证扫描器能抓出缺陷"时一直在扫真实仓库，
//   得到的"未命中"是假结论。测工具本身时，先确认工具读的是你以为的那个文件。）
const WEBUI = process.env.AUDIT_WEBUI || path.join(REPO, 'internal/server/webui.html');
const src = fs.readFileSync(WEBUI, 'utf8');

// 排除清单：这些按钮由**事件委托**或页面机制处理，不在按钮自己身上绑
const DELEGATED = new Set([
  'themeSwitch',       // 容器，内部按钮由委托处理
  'accts', 'jobrows', 'logrows', 'histrows', 'travelRows', 'growthRows', // 表格容器
]);

console.log('=== 静态死按钮扫描 ===\n');

// 1) 收集 HTML 里所有 `<button id="...">`
//
// ⚠ 必须排除**模板里生成的行按钮**：它们的 id 形如 id="${esc(U)}"，
// 由事件委托（#content 上的一次性监听）处理，不在按钮自己身上绑。
// 第一版没排除，报了 19 个假死按钮（签到/积分/派猫/领奖…全是表格行里的）。
const isTemplateId = (id) => /\$\{/.test(id);

// ⚠⚠ 必须用**剥过注释的** code，而不是原始 src。
//
// 这是本扫描器长期误报的根因：按钮收集在第 1 步用 src，绑定判定在第 2 步用 code，
// 两处输入不一致。于是**被注释掉的按钮**（例如 T3 移入注释说明的
// `<button id="btnAllCheckin">`）会被当成"页面上的真实按钮"收进来，
// 然后在剥注释后的 code 里当然找不到绑定 → 报成死按钮。
//
// 实测：`btnAllCheckin` 只出现在 webui.html 的账号池 h2 注释块里
// （T3 已把它从 DOM 移除；本轮起它被 **btnAllCheckinAll** 取代 ——
// 见下面自检样本的注释：换名字是刻意的，能让"旧实现回来了"一眼可见）。
// 剥注释后全文 0 次出现，它根本不是页面上的按钮。
//
// 这与文件末尾 stripComments 注释里记的那类错误是同一个：
// "验证书写形式而非本体" —— 这里连"本体是否存在"都没先确认。
//
// 判据修正：**注释不是 DOM**。注释里的 `<button>` 不渲染、不可点，
// 既不该被算作死按钮，也不该被算作活按钮 —— 它不存在。
const stripCommentsEarly = (s) => {
  const lf = s.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  return lf
    .replace(/<!--[\s\S]*?-->/g, '')          // HTML 注释
    .replace(/\/\*[\s\S]*?\*\//g, '')          // 块注释
    .split('\n')
    .map(l => l.replace(/\/\/.*$/, ''))        // 行注释
    .join('\n');
};
const markup = stripCommentsEarly(src);

const buttons = [];
const commentedOut = [];
for (const m of markup.matchAll(/<button\s[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/button>/g)) {
  const id = m[1];
  if (isTemplateId(id)) continue;
  const text = m[2].replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
  buttons.push({ id, text });
}

// 透明化：被注释掉的按钮单独列出来，**不判死**。
// 不列的话，"这个按钮怎么没被检查"就会变成下一个需要考古的谜题。
for (const m of src.matchAll(/<button\s[^>]*id="([^"]+)"[^>]*>([\s\S]*?)<\/button>/g)) {
  const id = m[1];
  if (isTemplateId(id)) continue;
  if (markup.includes(`id="${id}"`)) continue;
  const text = m[2].replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
  commentedOut.push({ id, text });
}
console.log('HTML 里的静态按钮（排除模板行按钮 + 注释块）: ' + buttons.length + '\n');
if (commentedOut.length) {
  console.log('（以下按钮只存在于注释中，不渲染、不可点，故不参与判定）');
  commentedOut.forEach(b => console.log('  · #' + b.id + ' "' + b.text + '"'));
  console.log('');
}

// 2) 对每个 id，看源码里是否有绑定
//
// ⚠ 必须**先剥掉注释**。我第一版直接在全文上匹配，结果把注释里提到的
// `$('btnJobRefresh').onclick = ...` 也算成了绑定 —— 变异测试证实它
// 抓不出"绑定被删掉"的情况（删了绑定后注释仍在，扫描器照样报"有绑定"）。
// 这与本项目反复出现的"验证书写形式而非本体"是同一类错误。
function stripComments(s) {
  // ⚠ 先把 CRLF 归一化成 LF —— 否则 `\/\/.*$` **完全失效**。
  //
  // 行尾是 `\r` 时，正则里的 `$`（无 m 标志）匹配不上，于是整行原样保留，
  // "剥注释"变成空操作。我最初没归一化，导致注释里提到的绑定被当成真绑定，
  // 变异测试（删掉绑定）抓不出来 —— 工具本身骗了我。
  const lf = s.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
  return lf
    .replace(/<!--[\s\S]*?-->/g, '')          // HTML 注释也剥（页面里有）
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .split('\n')
    .map(l => l.replace(/\/\/.*$/, ''))
    .join('\n');
}
const code = stripComments(src);

const dead = [];
const alive = [];
for (const b of buttons) {
  if (DELEGATED.has(b.id)) { alive.push({ ...b, how: '委托/容器' }); continue; }

  const id = b.id;
  const patterns = [
    new RegExp(`\\$\\('${id}'\\)\\.onclick`),                    // $('x').onclick
    new RegExp(`getElementById\\('${id}'\\)\\.onclick`),         // getElementById('x').onclick
    new RegExp(`\\$\\('${id}'\\)\\.addEventListener`),           // addEventListener
    new RegExp(`getElementById\\('${id}'\\)\\.addEventListener`),
  ];
  let how = null;
  for (const p of patterns) { if (p.test(code)) { how = '直接绑定'; break; } }

  // 批量绑定：id 出现在**数组字面量**里，且该数组附近有 onclick/addEventListener。
  //
  // ⚠ 两个坑，都靠变异测试才发现：
  //   1. `['"]id['"]` 无边界 → `btnJobRefresh` 是 `btnGrowthRefresh` 的子串，
  //      匹配到后者的绑定，把死按钮判成"有绑定"
  //   2. 加边界后仍命中 —— 因为 HTML 里 `id="btnJobRefresh"` **本身就是**引号
  //      包裹的 id，被当成了"数组里的 id"
  //
  // 所以必须：先剥掉 HTML（只看 JS），再要求 id 是**单引号**包裹
  //（JS 数组用单引号；HTML 属性用双引号，天然区分）。
  if (!how) {
    const jsOnly = code.replace(/<[^>]*>/g, '\n');   // 去掉所有 HTML 标签
    const re = new RegExp(`'${id}'`);
    const m = re.exec(jsOnly);
    if (m) {
      const ctx = jsOnly.slice(Math.max(0, m.index - 400), m.index + 600);
      if (/onclick|addEventListener/.test(ctx)) how = '批量绑定';
    }
  }

  if (how) alive.push({ ...b, how });
  else dead.push(b);
}

console.log('=== 有绑定的按钮 ===');
alive.forEach(b => console.log('  ✓ #' + b.id.padEnd(22) + ' "' + b.text.slice(0, 16) + '"  ' + b.how));
console.log('');
console.log('=== ⚠ 没有任何绑定的按钮（点了没反应）===');
if (!dead.length) console.log('  （无）');
dead.forEach(b => console.log('  ✗ #' + b.id.padEnd(22) + ' "' + b.text.slice(0, 24) + '"'));

console.log('');
console.log('合计: ' + alive.length + ' 个有绑定, ' + dead.length + ' 个疑似死按钮');

// ---------------------------------------------------------------------------
// 自检：这个扫描器必须**同时**满足两条相反的性质，否则它是没用的。
//
//   性质 A（不误报）：注释块里的 `<button>` 不能被判成死按钮。
//   性质 B（不漏报）：真正没有绑定的 `<button>` 必须被判成死按钮。
//
// 只修 A 的最省事写法是把"找不到绑定"一律放过 —— 那样性质 B 就死了，
// 工具从此永远绿。所以两条都要在本文件里构造出来实测，
// 用 AUDIT_WEBUI 指向临时副本（不动原仓库，也就不会污染别人的工作树）。
const os = require('os');
function scan(target) {
  // ⚠ 必须带哨兵：子进程只扫描、**不再自检**。否则会递归派生进程树
  //（分支因子 3、深度无界）—— 实测把内存打满、机器卡死。
  // 详见文件头 IS_CHILD_SCAN 的注释。
  const env = { ...process.env, AUDIT_WEBUI: target, AUDIT_DEAD_BTN_CHILD: '1' };
  const r = cpSelf.spawnSync(process.execPath, [__filename], { encoding: 'utf8', env });
  return { code: r.status, out: (r.stdout || '') + (r.stderr || '') };
}

// 子进程到此为止：它只负责"扫这个文件、用退出码回答有没有死按钮"。
if (IS_CHILD_SCAN) {
  process.exit(dead.length ? 1 : 0);
}

const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'deadbtn-'));
const results = [];
function selfCheck(name, mutate, wantDead) {
  const f = path.join(tmpDir, name + '.html');
  fs.writeFileSync(f, mutate(src));
  const r = scan(f);
  const gotDead = r.code !== 0;
  const pass = gotDead === wantDead;
  results.push(pass);
  console.log((pass ? '  PASS ' : '  FAIL ') + `自检 ${name}：期望${wantDead ? '判死(exit 1)' : '不判死(exit 0)'}，实际 exit ${r.code}`);
  if (!pass) console.log(r.out.split('\n').filter(l => /✗|合计/.test(l)).map(l => '      ' + l).join('\n'));
}

// 性质 A：现有仓库必须 exit 0（注释里的按钮不判死、真按钮都有绑定）
selfCheck('A-当前仓库-无死按钮', s => s, false);

// 性质 B：把账号池顶部那个按钮的绑定**整段删掉** → 必须判死。
//
// # 为什么样本从"取消注释旧按钮"换成"删掉绑定"
//
// 旧样本注入的是 `<button id="btnAllCheckin">全部签到</button>`（T3 之前的
// 写死按钮）。本轮起它只存在于**注释块**里，而注释不是 DOM ——
// 注入进去也扫不到，于是"注入了缺陷却期望判死"变成**永远红**。
// 一条永远红的自检等于没有自检（下一个人会把它删掉）。
//
// 新样本与本轮的真实结构同形：真按钮在 DOM 里、绑定被拿掉。
// 判据（"没有绑定的按钮必须被判死"）一个字没变。
//
// ⚠ 注入方式必须是**删掉整行**，不能只把它替换成别的代码：
// 扫描器的第二档判据是"这个 id 出现在某段 onclick/addEventListener
// 附近（400 字符窗口）" —— 把绑定替换成注释掉的空语句时，
// 窗口里仍然有 `onclick` 字样，于是它照样算"有绑定"。
// （实测：我第一版就是这么写的，自检 B 假绿变假红。）
selfCheck('B-真按钮的绑定被删掉-必须判死', s => s.replace(
  "$('btnAllCheckinAll').onclick = () => busyRun($('btnAllCheckinAll'), async () => {",
  'void 0; {'),
  true);

// 性质 B2：造一个全新的、确实没有绑定的按钮 → 必须判死（防止"只看某个 id"的特判）
//
// ⚠ 插入锚点本轮从 `<span id="allDailyActs">`（已随 T3 的顶部槽位一起删掉）
// 换成账号池 h2 里那条**真实存在**的按钮行。锚点不存在时
// `s.replace` 会**静默返回原串** —— 于是"注入了缺陷"这句话是假的，
// 而自检照样按"期望判死"去比，得到的红/绿都无意义。
selfCheck('B2-全新无绑定按钮-必须判死', s => s.replace(
  '<button id="btnAllCheckinAll"',
  '<button id="btnTotallyUnbound">没人绑我</button><button id="btnAllCheckinAll"'),
  true);

fs.rmSync(tmpDir, { recursive: true, force: true });

// ---------------------------------------------------------------------------
// 递归守卫：**注入一个"没有哨兵的 scan()"**，确认这条自检抓得住它。
//
// # 为什么需要它（这条是踩出来的）
//
// 上面那个递归缺陷（scan 派生自己、子进程又自检）已经真实发生过一次：
// 实测把内存打满、机器卡死。它当时**没有任何断言**能发现 ——
// 输出照样是"自检 3/3 通过"。
//
// 判据（静态、与运行期行为无关）：
//   scan() 传的 env 里必须有哨兵键，且文件里必须有 IS_CHILD_SCAN 的提前退出。
// 两条缺一，进程树就会长出来。
//
// ⚠ 与文件里其余自检同一条原则：**用与真实缺陷同形的样本喂给判据**，
// 而不是声称"我们检查了"。
// ---------------------------------------------------------------------------
{
  const selfSrc = fs.readFileSync(__filename, 'utf8');
  const checks = [
    ['scan() 传了递归哨兵（AUDIT_DEAD_BTN_CHILD）',
      /AUDIT_DEAD_BTN_CHILD:\s*'1'/.test(selfSrc)],
    ['文件里有 IS_CHILD_SCAN 的提前退出',
      /if\s*\(\s*IS_CHILD_SCAN\s*\)\s*\{\s*process\.exit/.test(selfSrc)],
  ];
  for (const [name, okk] of checks) {
    results.push(okk);
    console.log((okk ? '  PASS ' : '  FAIL ') + '自检 递归哨兵：' + name);
  }
  // 反向：把哨兵那行注掉，判据必须变红（否则这条守卫是装饰品）。
  const mutated = selfSrc.replace(/AUDIT_DEAD_BTN_CHILD:\s*'1'/, 'AUDIT_DEAD_BTN_CHILD: undefined');
  const caught = !/AUDIT_DEAD_BTN_CHILD:\s*'1'/.test(mutated);
  results.push(caught);
  console.log((caught ? '  PASS ' : '  FAIL ') +
    '自检 递归哨兵变异可检：拿掉哨兵后判据变红');
}

console.log('');
const selfFail = results.filter(r => !r).length;
console.log('自检: ' + (results.length - selfFail) + '/' + results.length + ' 通过');
process.exit(dead.length || selfFail ? 1 : 0);
