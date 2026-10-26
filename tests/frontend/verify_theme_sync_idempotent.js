// verify_theme_sync_idempotent.js —— theme_sync 的「幂等 + 分隔符卫生」守卫。
//
// # 为什么需要它（2026-10-26 的真缺陷）
// tools/theme_sync.js 的 block() 曾经这么写：
//     [START, css, END].join(eol).split('\n').join(eol)
// eol 是 CRLF 时，join 先插入 \r\n，紧接的 split('\n') 又从中间切开它
// （元素尾巴留一个 \r），join 再补 \r\n ⇒ 生成 **\r\r\n（双 CR）**。
// 后果具有极强的误导性：
//   · 脚本自己刚写完的页面，--check 是 ok 的（写完即自洽）；
//   · 但**任何人手同步过一次的 CRLF 页，永远过不了 --check** —— 视觉完全一致、
//     只差那两个多出来的 \r。门禁于是从「守卫」变成了「误导源」：
//     它报「你落后了」，而真正该改的是它自己。
// 这类缺陷用眼睛看不出来（浏览器渲染 \r\r\n 与 \r\n 一样），只能靠断言。
//
// # 本守卫断言三件事
//   ① **幂等**：连跑两次 sync，第二次必须**一个字节都不改**；
//   ② **分隔符卫生**：块内不得出现 \r\r，也不得出现裸 \r（CRLF 页块内应只有 \r\n，
//      LF 页块内应完全没有 \r）；
//   ③ **能收敛**：人为把块内的分隔符改成「单 CR」或「单 LF」后，再跑 sync
//      应到达不动点且 --check exit=0。
//
//
// # ③ 的边界（写清楚，免得下一个人以为这里漏测了）
// 把块内分隔符换成 **LF** 时，整页的「行尾多数票」可能被翻过去（CRLF 页里块占的
// 行数不小），此时 eolOf() 会判定这页是 LF 页、于是按 LF 生成块 —— 文件成为混合行尾，
// 但 --check 仍然 exit=0、且再跑一次不再变（是个不动点）。这是 eolOf「按文件自身行尾
// 风格生成」的**既定语义**，不属于 block() 的分隔符缺陷。所以本守卫对这种情况只断言
// 「收敛 + 块内自洽」，只有方向明确的情形（CRLF 页 → 单 CR；单 CR 永不影响多数票）
// 才额外断言「回到规范的 CRLF 字节」。
//
// # 它会临时改写 6 个页面，但**必定原样还原**
// 先用内存快照，finally 里恢复并逐页按字节校验恢复成功；恢复失败也要 exit 1
// （宁可红着报，也不留一个被测试改坏的仓库）。
'use strict';
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const ROOT = path.resolve(__dirname, '..', '..');
const TOOL = path.join(ROOT, 'tools', 'theme_sync.js');
const START = '/* ==== WB2API:MAXIMALISM:START（由 tools/theme_sync.js 生成，勿手改）==== */';
const END = '/* ==== WB2API:MAXIMALISM:END ==== */';
const PAGES = [
  'internal/server/webui.html',
  'internal/server/webui-login.html',
  'internal/server/webui-channels.html',
  'internal/server/webui-groups.html',
  'internal/server/webui-status.html',
  'internal/server/webui-models.html',
  'internal/server/webui-tokens.html',
];

let fail = 0;
const ok = (cond, msg) => { console.log((cond ? '  PASS ' : '  FAIL ') + msg); if (!cond) fail++; };
const read = (rel) => fs.readFileSync(path.join(ROOT, rel), 'utf8');
const write = (rel, text) => fs.writeFileSync(path.join(ROOT, rel), text);
function sync(args) {
  try {
    const out = execFileSync(process.execPath, [TOOL].concat(args || []), { cwd: ROOT, encoding: 'utf8' });
    return { code: 0, out: out };
  } catch (e) {
    return { code: (e.status === undefined || e.status === null) ? 1 : e.status, out: String(e.stdout || '') + String(e.stderr || '') };
  }
}
function eolOf(text) { const crlf = (text.match(/\r\n/g) || []).length; const lf = (text.match(/\n/g) || []).length; return crlf > lf / 2 ? '\r\n' : '\n'; }
function blockOf(text) { const s = text.indexOf(START); if (s < 0) return null; const e = text.indexOf(END, s); if (e < 0) return null; return text.slice(s, e + END.length); }
// 把块内的行尾统一改成 want（用于人为制造"非规范分隔符"）
function rewriteBlock(text, want) {
  const b = blockOf(text);
  if (b === null) return null;
  const fixed = b.split(/\r\n|\r|\n/).join(want);
  return text.slice(0, text.indexOf(START)) + fixed + text.slice(text.indexOf(START) + b.length);
}

const snap = new Map(PAGES.map((rel) => [rel, read(rel)]));
console.log('=== theme_sync 幂等性 / 分隔符卫生守卫 ===');
try {
  // ── ① 幂等
  const r1 = sync([]);
  ok(r1.code === 0, '第 1 次 node tools/theme_sync.js exit=0');
  const canonical = new Map(PAGES.map((rel) => [rel, read(rel)]));
  const r2 = sync([]);
  ok(r2.code === 0, '第 2 次 node tools/theme_sync.js exit=0');
  for (const rel of PAGES) {
    ok(read(rel) === canonical.get(rel), rel + '：第 2 次 sync 未改任何字节（幂等）');
  }
  const chk = sync(['--check']);
  ok(chk.code === 0, 'node tools/theme_sync.js --check exit=0');

  // ── ② 分隔符卫生
  for (const rel of PAGES) {
    const text = read(rel);
    const b = blockOf(text);
    ok(b !== null, rel + '：找得到 START/END 标记');
    if (!b) continue;
    const eol = eolOf(text);
    ok(!/\r\r/.test(b), rel + '：块内没有 \r\r（双 CR）');
    if (eol === '\r\n') ok(!/\r(?!\n)/.test(b), rel + '：CRLF 页块内没有裸 \r');
    else ok(!/\r/.test(b), rel + '：LF 页块内完全没有 \r');
  }

  // ── ③ 人为破坏分隔符后仍能收敛
  for (const rel of PAGES) {
    const eol = eolOf(canonical.get(rel));
    const wants = (eol === '\r\n') ? ['\r', '\n'] : ['\r', '\r\n'];
    for (const want of wants) {
      const broken = rewriteBlock(read(rel), want);
      if (broken === null) { ok(false, rel + '：改写块失败'); continue; }
      write(rel, broken);
      // 收敛判据刻意是「跑到不动点」而不是「一次就好」：eolOf 是**多数票**启发式，
      // 人为破坏后第一次 sync 可能先按旧票数写一版、第二次才稳定下来。
      // 断言「一次就好」会变成一条随文件行数比例漂移的脆弱断言（本守卫初版就栽在这）。
      const rs = sync([]);
      sync([]);
      const chk2 = sync(['--check']);
      const before = read(rel);
      sync([]);
      const stable = read(rel) === before;
      const cc = (read(rel).match(/\r\n/g) || []).length, ll = (read(rel).match(/\n/g) || []).length;
      const label = rel + '：块内分隔符人为改成 ' + JSON.stringify(want) + ' 后';
      ok(rs.code === 0 && chk2.code === 0 && stable, label + ' 收敛到不动点且 --check exit=0（行尾票数 CRLF=' + cc + ' / LF=' + ll + '）');
      const nb = blockOf(read(rel));
      ok(nb !== null && !/\r\r/.test(nb) && !/\r(?!\n)/.test(nb), label + ' 块内自洽（无 \\r\\r、无裸 \\r）');
      if (eol === '\r\n' && want === '\r') {
        // 方向明确：块内换成单 CR 不会改变整页的多数票 ⇒ 必须回到规范 CRLF 字节
        ok(read(rel) === canonical.get(rel), label + ' 回到规范的 CRLF 字节');
      }
    }
  }
} finally {
  for (const rel of PAGES) write(rel, snap.get(rel));
  let restored = true;
  for (const rel of PAGES) if (read(rel) !== snap.get(rel)) { restored = false; console.log('  FAIL ' + rel + '：未能原样还原！'); }
  ok(restored, '测试结束后 ' + PAGES.length + ' 个页面已按字节原样还原');
}
console.log(fail === 0 ? '\n=== 守卫通过（' + (PAGES.length) + ' 页）===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
