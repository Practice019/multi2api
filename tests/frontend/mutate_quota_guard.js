// mutate_quota_guard.js —— 变异验证：把两类真实缺陷注入 webui.html，
// 确认守卫**真的会红**；负向对照确认守卫不是"永远红灯"的噪音。
//
// # 为什么守卫自己必须被验证
//
// 一条抓不住违规的守卫 = 装饰品。本项目已有先例
// （internal/workbuddy/upstream_isolation_test.go 的
//  TestGuardCatchesInjectedDefect 就是为这件事写的），
// 但那条是在 Go 里用**人造样本**验的。这里更进一步：
// 把缺陷注入**真实的 webui.html**，跑**真实的守卫**，
// 这样连"守卫读的文件路径对不对""剥离注释有没有生效"一起验了。
//
// # 两个变异对应本 bug 的两个形态
//
//   变异 1：把 all_url 还原成 /admin/credits/refresh
//           → 形态一（前端把上游私有端点当全局端点）
//           → TestWebUINoUpstreamPrivateEndpoint 必须红
//
//   变异 2：删掉 startAll 里 typeof r.updated === 'number' 那条分支
//           → 形态二（新回执落进兜底 else，toast 说"已提交"）
//           → TestStartAllHandlesQuotaReceipt 必须红
//
// # 安全：改之前先备份，跑完**无论成败**都必须还原
//
// 用 try/finally 包住，并在最后核对还原结果（字节数 + 关键行存在）。
// 一个把源码改坏又没还原的变异脚本，比不做变异验证更糟。
'use strict';
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const REPO = path.resolve(__dirname, '..', '..');
const WEBUI = path.join(REPO, 'internal', 'server', 'webui.html');

const ORIGINAL = fs.readFileSync(WEBUI);
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// runGuard 只跑本步新增的守卫（-run 过滤），返回 {code, out}。
function runGuard(pattern) {
  try {
    const out = execFileSync('go', ['test', './internal/server/', '-run', pattern, '-count=1'],
      { cwd: REPO, encoding: 'utf8', env: Object.assign({}, process.env, { CGO_ENABLED: '0' }),
        stdio: ['ignore', 'pipe', 'pipe'] });
    return { code: 0, out: out };
  } catch (e) {
    return { code: e.status == null ? -1 : e.status,
             out: String((e.stdout || '')) + String((e.stderr || '')) };
  }
}

// applyMutation 写入变异后的源码；patch 返回新字符串，或抛错说明"没找到注入点"。
function applyMutation(name, patch) {
  const src = ORIGINAL.toString('utf8');
  const next = patch(src);
  if (next === src) {
    throw new Error(name + '：注入没生效（找不到目标文本）—— 这条变异验证不成立，必须报出来而不是静默跳过');
  }
  fs.writeFileSync(WEBUI, next, 'utf8');
}

const results = {};

try {
  // ---------------------------------------------------------------- 基线
  console.log('=== 基线：未经变异的源码，守卫必须绿 ===');
  let b = runGuard('TestWebUI|TestStartAllHandlesQuotaReceipt|TestQuotaToastReportsSkipped');
  console.log('  go test exit=' + b.code);
  ok(b.code === 0, '基线守卫通过（否则后面的"红"说明不了任何事）');

  // ---------------------------------------------------------------- 变异 1
  console.log('\n=== 变异 1：all_url 还原成 /admin/credits/refresh（形态一）===');
  //
  // ⚠ webui.html 是 CRLF（实测 5968 个 CRLF / 0 个裸 LF），
  // 所以替换也要按行做，不能让 `\n` 字面量去碰 `\r\n`。
  applyMutation('变异1', s => s.replace(
    "all_url: '/admin/accounts/quota/refresh'",
    "all_url: '/admin/credits/refresh'"));
  let r1 = runGuard('TestWebUINoUpstreamPrivateEndpoint');
  const tail1 = r1.out.split('\n').filter(l => /webui\.html 里出现了|FAIL|--- FAIL/.test(l)).slice(0, 4);
  console.log('  go test exit=' + r1.code);
  tail1.forEach(l => console.log('  | ' + l.trim()));
  ok(r1.code !== 0, '【变异 1】守卫变红（缺陷被抓住）');
  ok(/admin\/credits\/refresh/.test(r1.out), '【变异 1b】失败信息指出了具体路径字面量');

  // ---------------------------------------------------------------- 变异 2
  console.log('\n=== 变异 2：删掉 startAll 的 updated 分支（形态二）===');
  //
  // ⚠ 两次踩坑都记在这里，它们是同一个形态（**改动的失败模式是静默不生效**）：
  //
  // 坑 1：第一版用正则 `/[\s\S]*?\n      \}\n/`，把上一行的行首缩进算漏了 →
  //       replace 静默不匹配 → 变异"没注入" → 脚本接着报"守卫没红"的假结论。
  // 坑 2：改成字面量 join('\n') 之后仍然不匹配 —— 因为 **webui.html 是 CRLF**
  //       （实测 5968 个 CRLF、0 个裸 LF）。源码里的 `\n` 在文件里是 `\r\n`。
  //
  // 所以这里：按行取**真实**片段（保留原文件的换行风格），再删。
  // 并且 applyMutation 在 next === src 时**抛错**（宁可报"验证不成立"，
  // 也不静默跳过 —— 一条没注入成功的变异验证毫无价值）。
  applyMutation('变异2', s => {
    const lines = s.split('\n');
    // ⚠ 必须在 **startAll 的函数体内**找这条分支。
    // 第三版踩的坑：直接 findIndex("else if (r && typeof r.updated === 'number')")
    // 命中的是 **accountAct** 里那条（第 2526 行）—— 同一个判据在文件里
    // 出现了两次（行内按钮与顶部按钮各一处，这是**有意**的：
    // 两条路径各自认新回执）。删错那一处会验到另一个函数上，
    // 而脚本还会因为"守卫变红了"报 PASS —— 结论对但理由错。
    // 先定位 startAll 的起点，再从那里往后找。
    const fnAt = lines.findIndex(l => l.indexOf('async function startAll(') >= 0);
    if (fnAt < 0) throw new Error('变异2：找不到 async function startAll( —— 无法定位');
    const at = lines.findIndex((l, i) =>
      i > fnAt && l.indexOf("else if (r && typeof r.updated === 'number')") >= 0);
    if (at < 0) return s;   // 不匹配 → applyMutation 抛错
    const end = at + 4;     // else-if 行 + 3 条语句 + 收尾的 `}`
    if (lines[end] === undefined || lines[end].trim() !== '}') {
      throw new Error('变异2：分支结构与预期不符（第 ' + (end + 1) + ' 行是 ' +
        JSON.stringify(lines[end]) + '，期望 "}"）—— 拒绝盲删，避免改坏源码');
    }
    lines.splice(at, end - at + 1);
    return lines.join('\n');
  });
  let r2 = runGuard('TestStartAllHandlesQuotaReceipt');
  const tail2 = r2.out.split('\n').filter(l => /startAll 里没有|--- FAIL/.test(l)).slice(0, 4);
  console.log('  go test exit=' + r2.code);
  tail2.forEach(l => console.log('  | ' + l.trim()));
  ok(r2.code !== 0, '【变异 2】守卫变红（形态二被抓住）');
  ok(/updated/.test(r2.out), '【变异 2b】失败信息指出了缺的是哪条分支判据');
} catch (e) {
  console.log('  FAIL 变异脚本异常: ' + (e && e.message));
  fail++;
} finally {
  // ---------------------------------------------------------------- 还原
  fs.writeFileSync(WEBUI, ORIGINAL);
  const back = fs.readFileSync(WEBUI);
  console.log('\n=== 还原 ===');
  const sameBytes = back.length === ORIGINAL.length && Buffer.compare(back, ORIGINAL) === 0;
  ok(sameBytes, 'webui.html 已逐字节还原（' + back.length + ' 字节）');
  const s = back.toString('utf8');
  ok(s.includes("all_url: '/admin/accounts/quota/refresh'"), '还原后顶部按钮的 all_url 是 core 通用端点');
  ok(s.includes("typeof r.updated === 'number'"), '还原后 startAll 的 updated 分支还在');
  // 还原之后守卫必须重新变绿 —— 否则"变异导致红"与"代码本来就红"分不开。
  const b2 = runGuard('TestWebUI|TestStartAllHandlesQuotaReceipt|TestQuotaToastReportsSkipped');
  console.log('  还原后 go test exit=' + b2.code);
  ok(b2.code === 0, '还原后守卫重新变绿（证明前面的红**确实**由变异引起）');
}

console.log(fail === 0 ? '\n=== 变异验证全部通过 ===' : '\n=== 失败 ' + fail + ' 条 ===');
process.exit(fail === 0 ? 0 : 1);
