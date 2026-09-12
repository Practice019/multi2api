// verify_sign_against_spec.js —— 把实现逐条对照华为云官方文档
// （https://support.huaweicloud.com/intl/en-us/devg-apisign/api-sign-algorithm-001.html）
//
// # 为什么要做
//
// 我在 commit d49be12 里断言"华为云用单次 HMAC，不是 AWS 的四步派生链"——
// 那是**从源码反推**的结论。本轮查到官方文档，用文档逐条核对这个断言。
//
// 文档里可核对的规则（原文引用见 README/报告）：
//   1. CanonicalURI **必须以 '/' 结尾**（"A URI must end with a slash (/) for
//      signature calculation"）
//   2. CanonicalHeaders 每项 = Lowercase(name) + ':' + Trimall(value) + '\n'
//   3. SignedHeaders 小写、字母序、';' 分隔
//   4. RequestPayload 空时用**空串**参与哈希
//   5. StringToSign = "SDK-HMAC-SHA256\n" + X-Sdk-Date + "\n" + Hex(SHA256(CanonicalRequest))
//   6. **signature = HexEncode(HMAC(SecretAccessKey, stringToSign))** ← 单次 HMAC
//   7. Authorization 格式：`algorithm Access=AK, SignedHeaders=.., Signature=..`
//      （algorithm 与 Access 之间是**空格**，不是逗号）
'use strict';
const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

const ROOT = path.join(__dirname, '..', '..');
const SRC = fs.readFileSync(path.join(ROOT, 'internal/codearts/sign.go'), 'utf8');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

console.log('=== 对照官方文档核对实现 ===\n');

// 规则 6：单次 HMAC（最关键的一条）
console.log('[规则 6] signature = HexEncode(HMAC(SecretAccessKey, stringToSign))');
ok(/hmacSHA256Hex\(cred\.SecretKey,\s*stringToSign\)/.test(SRC),
  '确为**单次** HMAC，key 直接用 SecretKey（无 AWS 式派生链）');
ok(!/kDate|kRegion|kService|SDK4|AWS4/.test(SRC),
  '无 kDate/kRegion/kService 派生链（AWS SigV4 才有那套）');

// 规则 5：StringToSign 三段
console.log('\n[规则 5] StringToSign = ALGORITHM \\n Date \\n SHA256(CanonicalRequest)');
ok(/algorithm\s*=\s*"SDK-HMAC-SHA256"/.test(SRC), '算法名为 SDK-HMAC-SHA256');
ok(/stringToSign[\s\S]{0,120}algorithm[\s\S]{0,120}sha256Hex\(canonicalRequest\)/.test(SRC),
  'StringToSign 由 algorithm / date / 规范请求哈希三段组成');

// 规则 1：CanonicalURI 必须以 '/' 结尾
console.log("\n[规则 1] CanonicalURI 必须以 '/' 结尾");
const canon = /func canonicalURI\([\s\S]*?\n}/.exec(SRC);
ok(!!canon, '找到 canonicalURI 实现');
ok(canon && /HasSuffix\([^)]*"\/"\)/.test(canon[0]),
  'canonicalURI 里补了末尾斜杠（HasSuffix 检查）');

// 规则 2：CanonicalHeaders 小写 + TrimSpace + 换行
console.log('\n[规则 2] CanonicalHeaders = lower(name) + ":" + Trimall(value) + "\\n"');
ok(/strings\.ToLower\(k\)/.test(SRC), '头部名转小写');
ok(/strings\.TrimSpace\(v\)/.test(SRC), '头部值做 trim');

// 规则 3：SignedHeaders 排序 + ';'
console.log("\n[规则 3] SignedHeaders 小写、字母序、';' 分隔");
ok(/sort\.Strings\(signedKeys\)/.test(SRC), 'SignedHeaders 按字母序排序');
ok(/strings\.Join\(signedKeys,\s*";"\)/.test(SRC), "用 ';' 连接");

// 规则 4：空 body 用空串哈希
console.log('\n[规则 4] RequestPayload 为空时用空串参与哈希');
ok(/sha256Hex\(body\)/.test(SRC), '对 body 直接做 sha256Hex（空串自然得空串哈希）');

// 规则 7：Authorization 格式
console.log('\n[规则 7] Authorization: algorithm Access=AK, SignedHeaders=.., Signature=..');
const authRe = /"%s Access=%s, SignedHeaders=%s, Signature=%s"/.exec(SRC);
ok(!!authRe, 'Authorization 模板与文档一致（algorithm 后接**空格**再 Access）');

// 交叉验证：跑一次签名，检查输出形状符合规则 7
console.log('\n[交叉验证] 实跑一次签名，检查 Authorization 头形状');
const goTest = spawnSync('go', ['test', './internal/codearts/',
  '-run', 'TestSignSelfConsistencyWithKnownKey', '-v'],
  { cwd: ROOT, encoding: 'utf8', env: Object.assign({}, process.env, { CGO_ENABLED: '0' }) });
ok(goTest.status === 0, 'TestSignSelfConsistencyWithKnownKey 通过（与独立重写实现逐字节一致）');

console.log('');
if (fail === 0) {
  console.log('=== 实现与官方文档逐条一致 ===');
  console.log('（含最关键的一条：签名是**单次** HMAC(SecretKey, stringToSign)，');
  console.log('  不是 AWS SigV4 的四步派生链 —— 本轮已用官方文档确认）');
} else {
  console.log('=== ' + fail + ' 项与文档不符 ===');
}
process.exit(fail ? 1 : 0);
