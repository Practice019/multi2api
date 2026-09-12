// test_shape_scan.js —— 验证"按形状扫描密钥"既不漏报也不误报。
//
// 上一版扫描器写死了密钥前 8 位：换密钥即失效，且仓库里留了片段。
// 改成按形状（引号内 >=32 位 base62）后必须验证：
//   1. 不会把哨兵/占位符当成密钥（误报）
//   2. 真的能抓到 40 位密钥（不漏报）
'use strict';
const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const REPO = path.resolve(__dirname, '..', '..');

// 密钥的形状判据（不认前缀，换密钥后依然有效）：
//   - 引号内的字面量
//   - 长度 >= 32
//   - **必须同时含大写、小写、数字**（base62）
//
// ⚠ 我第一版只判"长度 >= 32"，产生 3 个误报：
//     "01a08fe00b8b7c21bd95e11109692b80"   ← 测试 UID，34 位纯小写 hex
//     "a8bcb36232554267a5142361cc25a393"   ← 模型 ID，34 位纯小写 hex
//     "u-00000000-0000-4000-8000-..."      ← UUID，含连字符
//   真实密钥是**混合大小写**的 base62（实测 5fkV...FqQ 含大小写与数字），
//   加"三类字符都要有"这一条即可把它们全部排除。
const RE = /"[A-Za-z0-9_-]{32,}"/;
const IS_BASE62 = s => {
  const m = /"([A-Za-z0-9_-]{32,})"/.exec(s);
  if (!m) return false;
  const v = m[1];
  // 连字符通常意味着 UUID / 复合标识，不是纯密钥
  if (v.includes('-')) return false;
  return /[A-Z]/.test(v) && /[a-z]/.test(v) && /[0-9]/.test(v);
};
const NOT_SECRET = /__WB2API_KEY__|__COLS_|COLS_/;
const looksLikeSecret = s => RE.test(s) && IS_BASE62(s) && !NOT_SECRET.test(s);

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

console.log('=== 形状扫描器自测 ===\n');

// 1) 误报检查：仓库里的"合法的长字面量"不应被当成密钥
const SAMPLES = [
  ['window.__WB2API_KEY__ = "__WB2API_KEY__";', false, '密钥哨兵'],
  ['colspan="__COLS_LOGS__"', false, 'COLS 占位符'],
  ['const ROOT = process.env.WB2API_REPO || __dirname', false, '普通代码'],
  ['"a".repeat(40)', false, '不含引号长字面量'],
  ['access_token = "abcdefghijklmnopqrstuvwxyz0123456789ABCD"', true, '40 位混合大小写 token'],
  ['const KEY = "__REDACTED_LEAKED_KEY__";', true, '真实形态的密钥'],
  // 以下三条是我第一版（只判长度）的误报，现在必须被排除
  ['"01a08fe00b8b7c21bd95e11109692b80"', false, '测试 UID（34 位纯小写 hex）'],
  ['"a8bcb36232554267a5142361cc25a393"', false, '模型 ID（34 位纯小写 hex）'],
  ['"u-00000000-0000-4000-8000-000000000000"', false, 'UUID（含连字符）'],
];
for (const [text, shouldHit, name] of SAMPLES) {
  const hit = looksLikeSecret(text);
  ok(hit === shouldHit, name + ' → ' + (hit ? '命中' : '未命中') + '（期望' + (shouldHit ? '命中' : '未命中') + '）');
}

// 2) 实跑：仓库里应 0 命中
console.log('\n实际扫描被跟踪文件：');
const tracked = execFileSync('git', ['ls-files'], { cwd: REPO, encoding: 'utf8' })
  .split('\n').filter(Boolean);
const hits = [];
for (const f of tracked) {
  try {
    const c = fs.readFileSync(path.join(REPO, f), 'utf8');
    if (looksLikeSecret(c)) hits.push(f);
  } catch { /* 二进制/不可读 */ }
}
ok(hits.length === 0, tracked.length + ' 个被跟踪文件，0 命中' + (hits.length ? ': ' + hits.slice(0, 5).join(', ') : ''));

// 3) 反向验证：把密钥塞进一个临时文件，扫描器必须抓到
console.log('\n反向验证（扫描器真的能抓）：');
const tmpf = path.join(require('os').tmpdir(), 'shape-scan-probe.txt');
fs.writeFileSync(tmpf, 'const KEY = "__REDACTED_LEAKED_KEY__";');
const c = fs.readFileSync(tmpf, 'utf8');
ok(looksLikeSecret(c), '故意写入的密钥被命中');
fs.unlinkSync(tmpf);

console.log('');
console.log(fail === 0 ? '=== 扫描器行为正确（不漏报、不误报）===' : '=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
