// webui.html 内联 JS 的语法门禁。
//
// ⚠ 为什么需要它：webui.html 是**单文件**（HTML + CSS + 三块内联 <script>），
// 语法错不会让 `go build` 失败（embed 只把它当字节），但页面会整块白屏 ——
// 而"白屏"在服务端日志里**什么都不留**。所以每次改它都必须先过这一关。
//
// 用法：node chk_js.js <path/to/webui.html>
// 退出码：0 = 全部内联脚本语法正确；1 = 有语法错（**此时绝不替换二进制**）。
const fs = require('fs');
const vm = require('vm');
const os = require('os');
const path = require('path');

const file = process.argv[2];
if (!file) {
  console.error('用法: node chk_js.js <webui.html>');
  process.exit(2);
}
const html = fs.readFileSync(file, 'utf8');

// 只挑**没有 src 属性**的 <script>（外链脚本不在这里检查）
const re = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi;
let m;
let n = 0;
let bad = 0;
while ((m = re.exec(html))) {
  n++;
  const code = m[1];
  if (!code.trim()) continue;
  const tmp = path.join(os.tmpdir(), 'wb2api_inline_' + n + '.js');
  fs.writeFileSync(tmp, code);
  try {
    // ⚠ vm.Script 只**编译**不执行 —— 正是"语法检查"要的语义
    new vm.Script(code, { filename: tmp });
  } catch (e) {
    bad++;
    // 把出错行号换算回 html 里的绝对行号，省得人工找
    const upto = html.slice(0, m.index).split('\n').length;
    console.error('  ⚠ 第 ' + n + ' 块内联脚本语法错（html 起始行约 ' + upto + '）: ' + e.message);
  }
}
if (bad) {
  console.error('  内联脚本 ' + n + ' 块，语法错 ' + bad + ' 块 ⇒ 门禁红，不要替换二进制');
  process.exit(1);
}
console.log('  ✓ 内联脚本 ' + n + ' 块，语法全部正确');
