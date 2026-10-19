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
//
// ⚠⚠ 第二版加了混合大小写判据之后**又冒出一个误报**（2026-10，本文件实测）：
//     internal/cline/cline.go:58  const workOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"
//   它是 WorkOS 的 **client_id** —— 公开标识（同文件注释写明：与生产环境
//   CLINE_ENVIRONMENTS.production、以及凭据 JWT 的 client_id claim 两处一致），
//   不是密钥。误报的机制值得记住：**下划线前缀本身带小写**，
//   `client_` 把"含小写"这一条凑齐了，而尾段 `01K3A541FN8TA3EPPHTD2325AR`
//   是 ULID（只有大写+数字）—— 真密钥不会有这种"前缀凑齐大小写"的形状。
//
//   所以判据改成：**大小写混合要在"某一段"（`_` 之间）成立**。
//   下划线只是命名空间分隔（client_ / pk_ / sk_live_），真密钥即使带下划线，
//   总有一段是混合大小写的随机串 ⇒ 照样命中（见下方自测样本）。
//
//   ⚠ 为什么是"任一段"而不是"最后一段"：我第一版判最后一段，结果有个洞 ——
//   密钥体内部再带下划线时会被漏掉。反例：`sk_test_51AbCdEfGhIjKlMnOpQrStUvWxYz_1234`
//   （最后一段 `1234` 只有数字 ⇒ 最后一段判据漏报）。改成"任一段"之后：
//     · `client_01K3A541FN8TA3EPPHTD2325AR`：`client` 无数字、`01K3…` 无小写 ⇒ **两段都不成立** ⇒ 仍被排除 ✓
//     · 上面那个收费密钥：`51AbCd…` 段混合 ⇒ 命中 ✓
//   两种形状都有自测样本钉住。
//
// ⚠⚠⚠ 第三轮修：**两个"按文件"过滤造成的假绿洞**（2026-10，本文件实测）
//
//   这一版之前，`looksLikeSecret(整篇文件内容)` 里有两处是**按文件**判的，
//   而扫描器要判的是"文件里的**某个字面量**像不像密钥" —— 量词放错位置就会漏报：
//     ① `exec` 只取**第一个**长字面量：一个文件先出现无害字面量、后面才是真密钥 ⇒ **整篇漏报**。
//        量出来的影响面：`git ls-files` 887 个文件里，改成"任一匹配"后**新增命中 0 个**（当时仓库是干净的）；
//        但这个洞是"将来会漏"的那种 —— 真出现密钥时它恰好可能被排在一个无害字面量之后。
//     ② `NOT_SECRET`（`__WB2API_KEY__|__COLS_`）是**按文件**豁免的：文件里只要**提到**过哨兵名，
//        整篇文件就再也不被判了 —— 而 `webui.html` 里就有 `__WB2API_KEY__`（服务端注入点），
//        **等于把最该盯的那个文件整篇豁免掉了**。这条是实测发现的（翻 `grep -n` 时才注意到）。
//   修法：改成**按字面量**判（`scanHits` 遍历每个长字面量，每个字面量各自过形状与哨兵），
//   并逐条自测这两个洞（见下方样本"首匹配盲区"与"整篇哨兵豁免"）。
//   代价：**扫描时排除本文件**（自测样本里必须出现"像密钥"的字面量，否则自测无意义）
//   —— 排除意味着本文件的字面量不再被扫，所以样本必须是**合成值**
//   （第一版这里写过真实 api_key ⇒ 扫描器自身成了泄漏源，见下方样本注释）。
const LIT = /"([A-Za-z0-9_-]{32,})"/g;
const segLooksRandom = (v) =>
  !v.includes('-') && v.split('_').some(seg => /[A-Z]/.test(seg) && /[a-z]/.test(seg) && /[0-9]/.test(seg));
const NOT_SECRET = /__WB2API_KEY__|__COLS_|COLS_/;

// 返回"这篇内容里像密钥的字面量"（按字面量过滤，不是按文件）
function scanHits(content) {
  const out = [];
  for (const m of content.matchAll(LIT)) {
    const v = m[1];
    if (segLooksRandom(v) && !NOT_SECRET.test(v)) out.push(v);
  }
  return out;
}
// 自测样本用的是"一段文本" ⇒ 同一套函数即可
const looksLikeSecret = (s) => scanHits(s).length > 0;

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
  // ⚠ 样本里**不能放真实密钥**。
  //
  // 我第一版这里写的是网关的真实 api_key —— 那让这个"密钥扫描器"自身
  // 成了泄漏源（它随 5b03b4e 提交进了版本库）。
  // 讽刺的是那一轮我正在修同类泄漏。
  //
  // 判据是**形状**（引号内 ≥32 位、含大小写与数字），不依赖具体值，
  // 所以换成合成样本即可，扫描能力不受影响。
  ['const KEY = "Aa1Bb2Cc3Dd4Ee5Ff6Gg7Hh8Ii9Jj0Kk1Ll2Mm3";', true, '真实形态的密钥（合成样本）'],
  // 以下三条是我第一版（只判长度）的误报，现在必须被排除
  ['"01a08fe00b8b7c21bd95e11109692b80"', false, '测试 UID（34 位纯小写 hex）'],
  ['"a8bcb36232554267a5142361cc25a393"', false, '模型 ID（34 位纯小写 hex）'],
  ['"u-00000000-0000-4000-8000-000000000000"', false, 'UUID（含连字符）'],
  // 第二条误报（2026-10 实测）：公开标识 —— 前缀凑齐了"含小写"，尾段是 ULID
  ['const workOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"', false, '公开标识（client_id，尾段 ULID：只有大写+数字）'],
  // 但带下划线的**真密钥**不许被这条新规则放过：尾段仍是混合大小写 ⇒ 命中
  ['const KEY = "sk_live_FAKE_EXAMPLE_DoNotUse00000";', true, '带命名空间前缀的真密钥（尾段混合大小写）'],
  // 反向：密钥体**内部**再带下划线时不能漏（这就是"只看最后一段"的洞）
  ['const KEY = "sk_test_51AbCdEfGhIjKlMnOpQrStUvWxYz_1234";', true, '内部还有下划线的真密钥（任一段混合 ⇒ 命中）'],
  // 第三轮两个洞的回归样本（旧语义在这两条上都是 false ⇒ 假绿）
  ['const A = "01a08fe00b8b7c21bd95e11109692b80"; const B = "Aa1Bb2Cc3Dd4Ee5Ff6Gg7Hh8Ii9Jj0Kk1";',
    true, '首匹配盲区：无害字面量在前、真密钥在后（旧 `exec` 只看第一个 ⇒ 漏报）'],
  ['window.__WB2API_KEY__ = "__WB2API_KEY__"; const K = "Aa1Bb2Cc3Dd4Ee5Ff6Gg7Hh8Ii9Jj0Kk1";',
    true, '整篇哨兵豁免：文件提到过 `__WB2API_KEY__` 但另有真密钥（旧按文件豁免 ⇒ 漏报）'],
];
for (const [text, shouldHit, name] of SAMPLES) {
  const hit = looksLikeSecret(text);
  ok(hit === shouldHit, name + ' → ' + (hit ? '命中' : '未命中') + '（期望' + (shouldHit ? '命中' : '未命中') + '）');
}

// 2) 实跑：仓库里应 0 命中
console.log('\n实际扫描被跟踪文件：');
const tracked = execFileSync('git', ['ls-files'], { cwd: REPO, encoding: 'utf8' })
  .split('\n').filter(Boolean);
// 扫描时排除本文件：自测样本里必须出现"像密钥"的字面量，否则自测没有意义。
// 代价与纪律：**本文件的字面量不再被扫** ⇒ 样本必须是合成值，绝不能写真实密钥
//（第一版犯过：这里写过网关的真实 api_key，随提交进了版本库，见上方样本注释）。
const SELF = path.relative(REPO, __filename).replace(/\\/g, '/');
const hits = [];
for (const f of tracked) {
  if (f === SELF) continue;
  try {
    const c = fs.readFileSync(path.join(REPO, f), 'utf8');
    const h = scanHits(c);
    if (h.length) hits.push(f + '(' + h.length + ')');
  } catch { /* 二进制/不可读 */ }
}
ok(hits.length === 0, (tracked.length - 1) + ' 个被跟踪文件（排除本文件自身），0 命中'
  + (hits.length ? ': ' + hits.slice(0, 5).join(', ') : ''));
console.log('    已扫描 ' + (tracked.length - 1) + ' 个文件；本文件自身排除（理由见代码注释）');

// 3) 反向验证：把密钥塞进一个临时文件，扫描器必须抓到
//
// 这条是**反向验证** —— 没有它，"0 命中"无法区分
// "仓库真干净"与"扫描器坏了"。用合成样本，不用真实密钥。
console.log('\n反向验证（扫描器真的能抓）：');
const tmpf = path.join(require('os').tmpdir(), 'shape-scan-probe.txt');
fs.writeFileSync(tmpf, 'const KEY = "Aa1Bb2Cc3Dd4Ee5Ff6Gg7Hh8Ii9Jj0Kk1Ll2Mm3";');
const c = fs.readFileSync(tmpf, 'utf8');
ok(looksLikeSecret(c), '故意写入的密钥被命中');
fs.unlinkSync(tmpf);

console.log('');
console.log(fail === 0 ? '=== 扫描器行为正确（不漏报、不误报）===' : '=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
