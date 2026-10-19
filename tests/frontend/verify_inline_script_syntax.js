// verify_inline_script_syntax.js —— 内嵌 HTML 里每一段内联 <script> 的**语法门禁**。
//
// # 为什么需要它（这是一条真空白，不是"多测一点"）
//
// 两个页面是 `//go:embed` 的（`internal/server/webui.go:15,18`）：
//
//	//go:embed webui.html
//	//go:embed webui-login.html
//
// ⇒ **Go 编译不校验里面的 JS 语法**。一段语法错的 JS 会被安静地编进二进制，
//   什么都不会报：`go build` 绿、单测绿、`go vet` 绿，直到浏览器打开页面 ——
//   表现是"页面少了一半功能"（`renderXxx is not defined` 那种报错也要等真点一次）。
//
// 而 `webui.html` 有 **11000+ 行内联 JS**（本文件实测：`#3` 一条 script 就 11133 行）。
// 改一行注释里的反引号/括号就可能把整块打断 —— 本仓已经记过同类事故：
// generator 的 `String.raw` 模板里写了个反引号 ⇒ 模板提前终止 ⇒ **陈旧的产物照旧全绿**
// （见 `run_frontend_suites.js` 的"门禁 0"）。那次是"模板被提前终止"，
// 这次防的是同一件事的另一个容器。
//
// # 判据形状（为什么是 `new vm.Script`）
//
// 只做**语法**解析，**不执行**。用 `vm.Script` 而不是 `eval`/`new Function`：
//   · 不执行 ⇒ 没有副作用、不需要实例、不需要 Chrome，毫秒级；
//   · 报错信息带**行号偏移**（filename 里塞进 "文件#script序号"）。
//
// ⚠ 它证明的是"这段 JS 能被解析"，**证明不了行为**。行为在 `run_e2e.js` 的 25 条里。
//   两条互补，不许互替。
//
// # 判别力（自带变异验证，不动原仓库）
//
// `--self-test`：在**临时副本**上把一段合法的 script 改成语法错
// （注入 `function broken( {`），断言"检查器必须报红且**点名那段**"。
// 不做这步的话，"它是不是永远绿"没人知道 —— 本仓对静态守卫的一贯要求。
//
// 用法：
//   node verify_inline_script_syntax.js             # 查仓库里的两个页面
//   node verify_inline_script_syntax.js --self-test # 先证明它会有判别力
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const vm = require('vm');

const REPO = (process.env.WB2API_REPO || path.resolve(__dirname, '..', '..'));

// 要查的文件：**所有 `//go:embed` 进二进制的 HTML**。
// 不写死"两个"—— 从 `webui.go` 的 embed 指令里现取，将来加第三个页面自动覆盖
// （写死清单的守卫会在新增文件时**静默漏掉**，那正是它该拦的形态）。
function embeddedHtml() {
  const goFile = path.join(REPO, 'internal/server/webui.go');
  const src = fs.readFileSync(goFile, 'utf8');
  // ⚠ `//go:embed` 的路径是**相对包目录**的（`internal/server/`），不是仓库根。
  //   我第一次写成相对仓库根 ⇒ 每个文件都报 ENOENT，"2 段语法错"看着像页面坏了，
  //   其实是路径解析错了 —— 又一次"先怀疑驱动"。
  const pkgDir = path.dirname(goFile);
  const out = [];
  const re = /\/\/go:embed\s+(\S+)/g;
  let m;
  while ((m = re.exec(src))) {
    if (/\.html$/.test(m[1])) out.push(path.join(pkgDir, m[1]));
  }
  // 兜底：embed 指令被改成别的写法时，至少别静默查 0 个文件
  if (!out.length) {
    out.push(path.join(pkgDir, 'webui.html'), path.join(pkgDir, 'webui-login.html'));
  }
  return [...new Set(out)];
}

// 抽取一段 HTML 里所有**内联** script（有 src 的是外链，不做语法检查 ——
// 它不在本仓库里，检查它等于检查别人的代码）。
function inlineScripts(html) {
  const re = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi;
  const out = [];
  let m, i = 0;
  while ((m = re.exec(html))) {
    i += 1;
    const attrs = m[1] || '';
    const body = m[2] || '';
    if (/\bsrc\s*=/.test(attrs)) continue;
    if (!body.trim()) continue;
    // 这段 script 在**整个文件**里的起始行（报错要能指回 HTML 行号）
    const line = html.slice(0, m.index).split('\n').length + (attrs.match(/\n/g) || []).length;
    out.push({ idx: i, body, line });
  }
  return out;
}

// check 返回 [{file, idx, line, msg, snippet}] —— 空数组 = 全过。
//
// ⚠ 一律经 `absOf`/`relOf` 两个助手：
//   · 内部读写用绝对路径（`embeddedHtml()` 给的就是绝对路径）；
//   · **打给人看的**一律用相对仓库根的路径 —— 本仓不许出现机器绝对路径
//     （`audit_tracked_content.js` 会因绝对路径报红，且这类路径对别人没意义）。
//   我第一次就是混用两者：`path.join(REPO, 绝对路径)` 拼出 `D:\repo\D:\repo\...`
//   ⇒ 自检说"没抓到注入" + "覆盖度 0 段"，两个 FAIL 全是**测量工具自己的错**。
function absOf(rel) { return path.isAbsolute(rel) ? rel : path.join(REPO, rel); }
function relOf(rel) {
  const a = absOf(rel);
  const r = path.relative(REPO, a);
  return r.startsWith('..') ? a : r.replace(/\\/g, '/');
}

function check(files) {
  const bad = [];
  for (const rel of files) {
    const abs = absOf(rel);
    let html;
    try { html = fs.readFileSync(abs, 'utf8'); }
    catch (e) { bad.push({ file: relOf(rel), idx: 0, line: 0, msg: '读不到文件: ' + e.message, snippet: '' }); continue; }
    for (const s of inlineScripts(html)) {
      try {
        // ⚠ filename 必须是**带路径的字符串**：这样 stack 里的
        //   `<path>#3:1234` 能被直接读出行号，报错才可定位。
        new vm.Script(s.body, { filename: relOf(rel) + '#' + s.idx });
      } catch (e) {
        const ln = Number((String(e.stack || '').match(/#\d+:(\d+)/) || [])[1] || 0);
        const lines = s.body.split('\n');
        const snippet = ln
          ? lines.slice(Math.max(0, ln - 3), Math.min(lines.length, ln + 2))
            .map((t, k) => (Math.max(0, ln - 3) + k + 1) + ': ' + t).join('\n')
          : '';
        bad.push({
          file: relOf(rel), idx: s.idx, line: s.line + ln - 1,
          msg: e.message, snippet,
        });
      }
    }
  }
  return bad;
}

// ---- 自检：证明"注入语法错 ⇒ 必红" ----
function selfTest() {
  const files = embeddedHtml();
  console.log('=== 内联 script 语法门禁 · 自检（判别力）===');
  let bad = 0;

  // 用例 1：原仓库必须全过（否则门禁一接进来就红，说明是它自己的问题）
  const clean = check(files);
  const ok1 = clean.length === 0;
  console.log((ok1 ? 'PASS ' : 'FAIL ') + '好样本：仓库里的 ' + files.map(relOf).join(', ')
    + ' 应全过　［实际 ' + (ok1 ? '全过' : clean.length + ' 段报错') + '］');
  if (!ok1) { clean.forEach((b) => console.log('      ' + b.file + ' 第 ' + b.line + ' 行: ' + b.msg)); bad += 1; }

  // 用例 2：注入语法错，必须在**临时副本**上被抓到（不动原仓库）
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'wb2api-inline-syntax-'));
  try {
    for (const rel of files) {
      const src = absOf(rel);
      if (!fs.existsSync(src)) continue;
      const dst = path.join(tmp, path.basename(rel));
      // 注入点必须落在**某段 script 的体内**：找一个已知的非空 script 块，
      // 在其开头插一行语法错。用"最后一处 </script> 之前"最稳。
      const html = fs.readFileSync(src, 'utf8');
      const cut = html.lastIndexOf('</script>');
      const injected = cut > 0
        ? html.slice(0, cut) + '\nfunction broken( {\n' + html.slice(cut)
        : html + '\n<script>function broken( {</script>\n';
      fs.writeFileSync(dst, injected);
    }
    const mutated = fs.readdirSync(tmp).map((f) => path.join(tmp, f));
    const got = check(mutated);
    const ok2 = got.length > 0 && got.every((b) => /Unexpected|Invalid|missing/i.test(b.msg));
    console.log((ok2 ? 'PASS ' : 'FAIL ') + '坏样本：注入 `function broken( {` ⇒ 必须报红且给行号　'
      + '［实际 ' + (got.length ? got.map((b) => path.basename(b.file) + ':' + b.line).join(',') : '没抓到') + '］');
    if (!ok2) bad += 1;
    got.forEach((b) => console.log('      ' + path.basename(b.file) + ' ⇒ 第 ' + b.line + ' 行: ' + b.msg));
  } finally {
    try { fs.rmSync(tmp, { recursive: true, force: true }); } catch { /* 临时目录，删不掉也不影响判据 */ }
  }

  // 用例 3：抽到的 script 数必须非零 —— 否则"全绿"是因为**什么都没查**
  const total = files.reduce((n, rel) => {
    try { return n + inlineScripts(fs.readFileSync(absOf(rel), 'utf8')).length; }
    catch { return n; }
  }, 0);
  const ok3 = total > 0;
  console.log((ok3 ? 'PASS ' : 'FAIL ') + '覆盖度：真的抽到了内联 script　［实际 ' + total + ' 段］');
  if (!ok3) bad += 1;

  console.log('\n自检结论: ' + (bad ? bad + ' 例没达到预期 ⇒ 门禁自身有问题' : '好样本绿、坏样本红、覆盖度非零 ⇒ 有判别力'));
  return bad ? 1 : 0;
}

if (process.argv.slice(2).includes('--self-test')) process.exit(selfTest());

const files = embeddedHtml();
console.log('=== 内嵌 HTML 的内联 script 语法门禁 ===');
console.log('目标: ' + files.map(relOf).join(', ') + '（清单取自 internal/server/webui.go 的 //go:embed）');
let segs = 0;
for (const rel of files) {
  try { segs += inlineScripts(fs.readFileSync(absOf(rel), 'utf8')).length; } catch { /* 下面的 check 会报 */ }
}
const bad = check(files);
for (const rel of files) {
  let n = 0;
  try { n = inlineScripts(fs.readFileSync(absOf(rel), 'utf8')).length; } catch { /* 已报 */ }
  const mine = bad.filter((b) => b.file === relOf(rel));
  console.log((mine.length ? '  FAIL ' : '  ok   ') + relOf(rel) + '（' + n + ' 段内联 script）');
}
if (bad.length) {
  console.log('\n失败明细：');
  bad.forEach((b) => {
    console.log('  ' + b.file + ' 内联 script #' + b.idx + '（约第 ' + b.line + ' 行）: ' + b.msg);
    if (b.snippet) console.log('    ' + b.snippet.split('\n').join('\n    '));
  });
}
console.log('\n=== ' + (bad.length ? bad.length + ' 段语法错' : '全部通过（' + segs + ' 段）') + ' ===');
console.log('⚠ 本门禁只证明"能被解析"，证明不了行为；行为在 run_e2e.js 的 25 条里。两条互补。');
process.exit(bad.length ? 1 : 0);