// verify_t4_t5_static.js —— T4（文案统一 + 缺失显示 `—`）与 T5（Token 剩余时间）的**静态**守卫。
//
// # 为什么在浏览器 E2E 之外还要一组静态守卫
//
// 与 verify_t3_t4_static.js 同一条理由：E2E（verify_t4_t5.js）需要新鲜实例
// + Chrome，跑不动时给的是 ENV 而不是 FAIL —— 那种"跳过"积累起来等于没有守卫。
// 这一组只看**源码里必须存在的形状**，任何机器上都能跑，是 E2E 的地基。
//
// # 这一组守什么
//
// T4：
//   1. 后端 `AccountView` 仍下发 `quota`（`quotaCellHTML` 的判据依赖它）
//   2. 额度列**不再**直接渲染派生标量 `a.credits`
//   3. `quotaCellHTML` 真的读 `has_data`，且 `has_data !== true` → `—`
//   4. `credits` **字段名**没被改（API 兼容 / 决策 5 明令）
//   5. 用户可见区（剥掉注释后）不再有裸「积分」文案
//
// T5：
//   6. 后端 `token_expire_sec` 是 `*int64`（修掉 omitempty 的语义丢失）
//   7. `accountViews` 只在 `ExpiresAt > 0` 时取地址（不造"已过期"的假读数）
//   8. `fmtTokenRemain` 五档齐全（天 / 小时 / 分钟 / 已过期 / —）
//   9. Token 列**不再**是 `|| 0` + `Math.floor(d/86400) + 'd'`
//
// # 边界说明
//
// 静态检查**证明不了行为**。这里的每条断言都刻意做成"删掉/短路某一行就会红"
// 的形状，而不是"出现过某个词"。真正的行为证明在 verify_t4_t5.js（E2E）。
// 变异验证见本文件的 [M] 段（跑的是真源码副本，不是本文件的自我声明）。
'use strict';
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const REPO = (process.env.WB2API_REPO || path.resolve(__dirname, '..', '..'));
const WEBUI = path.join(REPO, 'internal/server/webui.html');
const ADMIN = path.join(REPO, 'internal/admin/admin.go');

const rawHtml = fs.readFileSync(WEBUI, 'utf8');
const adminSrc = fs.readFileSync(ADMIN, 'utf8');

// 归一化 + 剥注释 —— 与 verify_t3_t4_static.js 完全同一套。
//
// ⚠ CRLF 坑（本项目踩过）：`/\/\/.*$/` 里的 `$` 在 `\r` 前匹配不上，
// 于是 CRLF 文件的行注释**整段剥不掉**，扫描器静默失效。
// 所以先把 \r\n 归一成 \n 再剥。
//
// ⚠ 但**不能**用剥过的文本去断言"界面上没有『积分』"这类**跨行**事实：
// 本文件按"注释之外是否还有『积分』"来判，正是要利用"注释会被剥掉"这一点。
const normalized = rawHtml.replace(/\r\n/g, '\n');
const stripped = normalized
  .replace(/<!--[\s\S]*?-->/g, '')
  .replace(/\/\*[\s\S]*?\*\//g, '')
  .replace(/(^|[^:])\/\/.*$/gm, '$1');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// ================================================================ T4
console.log('\n[T4-1] 额度列的判据来自 `quota.has_data`，不是派生标量');
ok(/function\s+quotaCellHTML\s*\(/.test(stripped), '存在 quotaCellHTML()（额度列的唯一渲染点）');
// 额度列必须走 quotaCellHTML —— 写回 `${esc(a.credits)}` 就是改造前的缺陷形态
ok(/<td>\$\{quotaCellHTML\(a\)\}<\/td>/.test(stripped),
  '账号行的额度单元格调用 quotaCellHTML(a)（而不是直接渲染 a.credits）');
// 反向守卫：旧的裸渲染必须消失
ok(!/<td class="mono">\$\{esc\(a\.credits\)\}<\/td>/.test(stripped),
  '旧的 `<td class="mono">${esc(a.credits)}</td>` 已不存在（它就是"没查过也显示 0"的来源）');

console.log('\n[T4-2] quotaCellHTML 真的区分「没查过」与「查到 0」');
const qStart = stripped.indexOf('function quotaCellHTML(');
ok(qStart >= 0, '找到 quotaCellHTML');
if (qStart >= 0) {
  const qBody = stripped.slice(qStart, qStart + 1400);
  ok(/\.has_data\s*!==\s*true/.test(qBody),
    '空数据判据是 `q.has_data !== true`（不是 `!q.has_data`：老后端没有这个字段时要回落，不是判缺）');
  ok(/has_data\s*!==\s*true[^\n]*\n[^\n]*—/.test(qBody) || /—/.test(qBody),
    '`—` 确实出现在这个函数里（缺失时的显示值）');
  // ⚠ 关键反向守卫：不能写成 `credits || '—'` —— 那会把合法的 0 也吞成缺数据，
  // 是同一个 bug 的镜像版本（把确定的零伪装成不知道）。
  ok(!/credits\s*\|\|/.test(qBody),
    '没有 `credits || ...` 写法（那会把合法的 0 吞成「缺数据」——方向反了的同一个 bug）');
  // 回落分支必须基于"quota 是不是对象"，而不是"has_data 是不是真"
  ok(/typeof\s+q\s*!==\s*'object'/.test(qBody),
    '回落判据是 `typeof q !== "object"`（老后端没有 quota 字段时退回 credits）');
}

console.log('\n[T4-3] `credits` 字段名没被改动（API 兼容 / 决策 5）');
const poolStatus = fs.readFileSync(path.join(REPO, 'internal/pool/pool.go'), 'utf8');
ok(/Credits\s+int64\s+`json:"credits"/.test(poolStatus),
  'pool.Status 仍是 `Credits int64 `json:"credits"``（字段名不动，只改界面文案）');

console.log('\n[T4-4] 用户可见区不再有裸「积分」文案');
// 逐行找"注释之外"的「积分」。
//
// 判据不是"全文 0 次" —— 注释里保留历史措辞是**对的**（它们解释"改造前叫什么"）。
// 所以剥注释之后再找，剩下的每一条都是**会渲染给用户**的。
const liveHits = [];
stripped.split('\n').forEach((line, i) => {
  if (line.includes('积分')) liveHits.push((i + 1) + ': ' + line.trim());
});
// KV_LABELS 是**上游字段名的映射表**，不是界面文案 —— 唯一的豁免。
// 判据：这一行是 `Xxx: '...'` 形式的**字段名 → 中文**条目，且左边是上游字段标识符。
const isFieldMap = (l) => /^\s*[A-Za-z_][A-Za-z0-9_]*\s*:\s*'[^']*积分[^']*',?\s*$/.test(l);
const offenders = liveHits.filter(h => !isFieldMap(h.slice(h.indexOf(': ') + 2)));
if (offenders.length) {
  console.log('    非注释区仍含「积分」的行:');
  offenders.forEach(h => console.log('      ' + h));
}
ok(offenders.length === 0,
  '剥掉注释后，源码里没有裸「积分」文案（' + offenders.length + ' 处；KV_LABELS 字段名映射已豁免）');
// 豁免不是"放过" —— 单独确认豁免掉的那几行确实是字段映射表里的
const mapHits = liveHits.filter(h => isFieldMap(h.slice(h.indexOf(': ') + 2)));
if (mapHits.length) {
  console.log('    （已豁免 ' + mapHits.length + ' 条 KV_LABELS 字段名映射 —— 它们对着上游字段，不是界面文案）');
}
ok(/const\s+KV_LABELS\s*=/.test(stripped), 'KV_LABELS 仍然存在（豁免有据可依，不是凭空放行）');

// ================================================================ T5
console.log('\n[T5-1] 后端 token_expire_sec 用指针表达「未知」');
ok(/TokenExpireSec\s+\*int64\s+`json:"token_expire_sec,omitempty"`/.test(adminSrc),
  'AccountView.TokenExpireSec 是 `*int64`（nil = 未知；omitempty 保留但语义已修）');
ok(/TokenExpireAt\s+\*int64\s+`json:"token_expire_at,omitempty"`/.test(adminSrc),
  'AccountView.TokenExpireAt 也是 `*int64`（同一份判据，不能只改一半）');
// 反向守卫：不能再是裸 int64
ok(!/TokenExpireSec\s+int64\s/.test(adminSrc),
  'TokenExpireSec 不再是裸 `int64`（那正是 omitempty 吞掉"未知"的根因）');

console.log('\n[T5-2] accountViews 只在真的有过期时间时才填指针');
// 取 accountViews 函数体
const avStart = adminSrc.indexOf('func (h *Handler) accountViews()');
ok(avStart >= 0, '找到 accountViews');
if (avStart >= 0) {
  const rest = adminSrc.slice(avStart);
  const nextFn = rest.indexOf('\nfunc ', 10);
  const avBody = nextFn > 0 ? rest.slice(0, nextFn) : rest;
  ok(/if\s+a\.ExpiresAt\s*>\s*0\s*\{/.test(avBody),
    '取址被 `if a.ExpiresAt > 0` 保护（ExpiresAt=0 是"没有可读的过期时间"，不是"刚过期"）');
  // 反向守卫：不能无条件取址 —— ExpiresAt=0 时会造出约 -17.9 亿的负值，
  // 前端会渲染成「已过期」，比原来的 `0d` 更糟。
  const beforeGuard = avBody.split('if a.ExpiresAt > 0')[0];
  ok(!/TokenExpireSec\s*=/.test(beforeGuard),
    '保护块**之外**没有对 TokenExpireSec 的赋值（否则 ExpiresAt=0 会变成"已过期"假读数）');
}

console.log('\n[T5-3] 前端有五档齐全的统一渲染器');
ok(/function\s+fmtTokenRemain\s*\(/.test(stripped), '存在 fmtTokenRemain()（所有上游共用的唯一渲染器）');
const fStart = stripped.indexOf('function fmtTokenRemain(');
if (fStart >= 0) {
  const fBody = stripped.slice(fStart, fStart + 1600);
  ok(/sec\s*===\s*undefined\s*\|\|\s*sec\s*===\s*null/.test(fBody),
    '未知判据是 `=== undefined || === null`（不是 falsy —— 合法的 0 不能被吞）');
  ok(/return\s*'—'/.test(fBody), '未知 → `—`');
  ok(/return\s*'已过期'/.test(fBody), '负值 → `已过期`');
  ok(/' 天'/.test(fBody), '有「天」这一档');
  ok(/' 小时'/.test(fBody), '有「小时」这一档（一天以内不再一律 0d）');
  ok(/' 分钟'/.test(fBody), '有「分钟」这一档（codearts 的 STS 只有约 2 小时寿命，必须看得出）');
  // 向上取整会把"还剩 30 秒"显示成"1 分钟" —— 倒计时里这是**多报**，不能有。
  ok(!/Math\.round/.test(fBody), '不用 Math.round（倒计时宁可少报不可多报，一律向下取整）');
}

console.log('\n[T5-4] Token 列走统一渲染器，旧写法已消失');
const aStart = stripped.indexOf('function accountRow(');
if (aStart >= 0) {
  const aBody = stripped.slice(aStart, aStart + 4000);
  ok(/fmtTokenRemain\(d\)/.test(aBody), 'Token 列调用 fmtTokenRemain(d)');
  // 反向守卫：旧的 `|| 0` + `'d'` 写法必须消失
  ok(!/a\.token_expire_sec\s*\|\|\s*0/.test(aBody),
    '旧的 `a.token_expire_sec || 0` 已消失（它把"未知"吞成 0，于是渲染成 0d）');
  ok(!/Math\.floor\(d\s*\/\s*86400\)\s*\+\s*'d'/.test(aBody),
    "旧的 `Math.floor(d / 86400) + 'd'` 已消失（只按天，一天以内全是 0d）");
} else {
  ok(false, '找到 accountRow');
}

// ================================================================ [M] 变异验证
//
// # 为什么把变异验证放在**本文件内**（而不是靠人手动改一行再跑）
//
// 本项目已有的做法是"手动改一个字面量 → 看断言红不红 → 改回来"。
// 那个做法的问题是：**改回来**这一步会失败（忘记 / 改错行 / 编辑器自动格式化），
// 于是仓库被留在一个变异状态里，而没人知道。
//
// 这里改成：把源码**复制到临时文件**做变异，用 WB2API_REPO 指向副本跑本文件，
// 断言"副本上必须红"。原仓库**一个字节都不动**。
//
// 变异 1（T4 的核心）：把额度列改回 `${esc(a.credits)}` —— 必须红。
console.log('\n[M] 变异验证（在临时副本上跑，不动原仓库）');
function runStaticOn(variantHtml, label) {
  const tmp = fs.mkdtempSync(path.join(require('os').tmpdir(), 'wb2api-mut-'));
  try {
    fs.mkdirSync(path.join(tmp, 'internal/server'), { recursive: true });
    fs.mkdirSync(path.join(tmp, 'internal/admin'), { recursive: true });
    fs.mkdirSync(path.join(tmp, 'internal/pool'), { recursive: true });
    fs.writeFileSync(path.join(tmp, 'internal/server/webui.html'), variantHtml, 'utf8');
    fs.writeFileSync(path.join(tmp, 'internal/admin/admin.go'), adminSrc, 'utf8');
    fs.writeFileSync(path.join(tmp, 'internal/pool/pool.go'), poolStatus, 'utf8');
    let out = '';
    try {
      out = execFileSync(process.execPath, [__filename], {
        env: Object.assign({}, process.env, { WB2API_REPO: tmp, WB2API_MUTANT: '1' }),
        encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
      });
    } catch (e) {
      out = (e.stdout || '') + (e.stderr || '');
    }
    return out;
  } finally {
    try { fs.rmSync(tmp, { recursive: true, force: true }); } catch { }
  }
}

// replaceInFunction 只在**指定函数的函数体内**做一次字符串替换。
//
// # 为什么必须限定范围
//
// `const d = a.token_expire_sec;` 在 webui.html 里有**两处**：
//
//	tokenExpiryCellHTML()  —— codearts 的「Token 到期」列（本次新增）
//	accountRow()           —— workbuddy 的「Token」列（原有）
//
// 而 T5-4 的守卫是**按 accountRow 的函数体**断言的。用全局 .replace 只会命中
// 前面那处，变异就打在了守卫根本不看的地方 —— 表现是"断言没变红"，极易被误读成
// "守卫是假的"，实际是**变异无效**。（这条 M3 就这么假报了很久。）
//
// 找不到函数时原样返回：下面的"变异没生效"检查会把它报出来，
// 而不是让它静悄悄地变成一条恒绿的假变异。
function replaceInFunction(src, fnAnchor, from, to) {
  const i = src.indexOf(fnAnchor);
  if (i < 0) return src;
  // 函数体结尾：本文件用两空格缩进，函数自己的收尾是行首恰好 `  }`。
  const j = src.indexOf('\n  }\n', i);
  const end = j < 0 ? src.length : j;
  return src.slice(0, i) + src.slice(i, end).replace(from, to) + src.slice(end);
}

// 变异体本身是**可编译/可解析**的 —— 它只是一次等价的字符串替换，
// 产物仍是合法 HTML/JS。这一点很重要：一个语法坏掉的变异体让断言变红
// 只能证明"文件坏了"，证明不了"守卫真的盯着这个行为"。
if (!process.env.WB2API_MUTANT) {
  const mutants = [
    {
      name: 'M1：额度列改回 `${esc(a.credits)}`（T4 核心）',
      html: normalized.replace('<td>${quotaCellHTML(a)}</td>', '<td class="mono">${esc(a.credits)}</td>'),
      expect: '旧写法',
    },
    {
      name: 'M2：quotaCellHTML 改成 `credits ||` 兜底（把合法的 0 吞成缺数据）',
      html: normalized.replace('if (q.has_data !== true) return', 'if (!a.credits) return'),
      expect: 'has_data',
    },
    {
      name: 'M3：Token 列改回 `|| 0` + 只按天',
      // 定点改 accountRow 里那一处（守卫看的正是它），不是全局第一处。
      html: replaceInFunction(normalized, 'function accountRow(',
        'const d = a.token_expire_sec;', 'const d = a.token_expire_sec || 0;'),
      expect: 'token_expire_sec',
    },
  ];
  for (const m of mutants) {
    // 变异必须真的改变了源码 —— 否则"红了"可能只是因为别的原因
    if (m.html === normalized) {
      ok(false, m.name + ' —— 变异**没生效**（锚点串没匹配上，这条变异是无效的）');
      continue;
    }
    const out = runStaticOn(m.html, m.name);
    const red = /FAIL/.test(out);
    ok(red, m.name + ' → 断言变红（证明守卫真的盯着它，不是空转）');
    if (!red) console.log('      变异体输出:\n' + out.split('\n').filter(l => /FAIL|PASS/.test(l)).map(l => '      ' + l).join('\n'));
  }
  // 基线：未变异的副本必须**全绿** —— 否则上面的"红"说明不了任何事
  //（一个恒红的守卫在变异时也是红的）。
  const baseline = runStaticOn(normalized, 'baseline');
  ok(!/FAIL/.test(baseline), '基线（未变异副本）全绿 —— 上面的"红"才有意义');
}

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
