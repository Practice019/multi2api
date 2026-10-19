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
//
// ============================================================================
// # 2026-10-19：交叉验证（本节）的处置 —— 判据不硬依赖 go，但有 go 时必须真跑
//
// ## 事实基线（别看错成"产品坏了"）
//
//   · 基线 commit 865ae96 上本探针 **13 PASS / 0 FAIL，exit 0** —— 它真绿过。
//   · 本文件被改写期间曾出现 25 PASS / 6 FAIL：那 6 条红**全在本节**，
//     成因是"把返回对象当字符串比"（见下面的坑），**既不是产品缺陷也不是环境缺陷**。
//   · 本会话的 PATH 里没有 go：`Get-Command go` / `where.exe go` 都是空，
//     PATH 里那条 `D:\software\go1.26.1.windows-amd64.msi\bin` 指向**不存在的目录**
//     （MSI 文件名被当成目录塞进了 PATH）。但 go **是装了的**：
//     `D:\go-path\go\bin\go.exe`（go1.22.5，Windows 上不在默认 PATH，要手动前置）。
//     别处（父代理/用户终端）能构建 —— bin\wb2api-server-probe.exe 的 mtime 就是当天。
//     ⇒ 判定必须写成**两层**：① 本会话里若 go 不可用，这条红是**没法执行**造成的；
//       ② 在能跑 go 的环境里它是否通过，只有**真跑**才知道 —— 所以下面的处置是
//       "没把 go 前置时给明确 ENV 诊断且不计数"，而不是"缺 go 就放行"。
//
// ## 处置（三者都要）
//
//   1. **能找到 go**：真跑 go test（`-count=1 -v`）。而且不止看退出码：
//      · 输出里必须有 `--- PASS: TestSignSelfConsistencyWithKnownKey`
//        （`go test -run 不存在的名字` 也是 exit 0 —— 只看退出码会假绿）；
//      · 还要把 go **真运行**打出来的两条串（`Go 实现` / `独立实现`）解析出来，
//        与本节 JS 三路重放的产物**逐字节对照**（见 [交叉验证 C]）。
//   2. **找不到 go**：打 ENV（**不计入 FAIL，也不冒充 PASS**），并把"本机 go 在哪"
//      写进诊断（本机实测 `D:\go-path\go\bin\go.exe`；也可用 `WB2API_GO` 指定）。
//   3. **判别力不依赖 go**：本节主体是纯 JS 的三路重放（crypto 内建，零三方依赖）：
//        · **A** = 从 `sign.go` 抽出的流水线（常量/分隔符/归一/字段序/模板全部抽出）
//        · **B** = 按**文档 7 条**独立重写一遍
//        · **C** = 从 `sign_selfconsistency_test.go` 的 `independentSign` 抽出的流水线
//          （它就是那条 go test 的**期望实现** ⇒ A≡C 就是"测试期望与实现一致"的静态证明）
//      另有**外部锚点**：`testdata_vector.json`（由"已实测通过服务端校验的 JS 实现"生成）
//      的 `wantAuth` 里 `Access=` / `SignedHeaders=` **两段与密钥无关**，可逐字节核对。
//
// ## ⚠⚠ 本文件踩过的坑：**返回对象被当字符串比**
//
// 症状（极好认）：`A(源码抽出的流水线)= [object Object]`、`B= [object Object]`、
// `首个分歧在第 1 字符`；或抛 `Cannot read properties of undefined (reading 'toLowerCase')`
// （头部名那步拿不到值）。
// 机制：`runSrcPipeline` / `runDocImpl` 返回的是 `{ auth, cr, sts }`，
// 而比较处写成了 `A.auth === B.auth` 却把整个对象赋给了 `a`/`b` ⇒ 两边都是对象，
// 恒不等；`while (i < A.auth.length …)` 里 `length` 是 `undefined` ⇒ 一次都不走 ⇒ `i = 0`
// ⇒ 打印"首个分歧在第 1 字符"。**"分歧在第 1 字符"就是拿错字段的指纹**。
// ⚠ 绝对不要用 `String(A.auth) === String(B.auth)` 去"修"：两边都会变成 `"[object Object]"`，
//   红立刻变**假绿**（本仓库最忌的形态）。正确做法是**先钉类型**：`authString()` 见下 ——
//   形状不对就**响亮抛错**，让"拿错字段"没法静默比较。
//
// ## 已知的覆盖边界（写下来，免得以后有人以为它证明了更多）
//
//   · **签名值本身**在无密钥时无法外部锚定（向量刻意不含 secretKey）：它只被 A/B/C 三路
//     对照与"有 go 时的真运行对照"覆盖。这是刻意的取舍，不是遗漏。
//   · **头部集合（加了哪些头）** 属实现策略，文档不管：B 沿用 A 抽出的集合；
//     策略的正确性由 wantAuth 的 SignedHeaders 段（它逐字列了要签哪几个头）锚定。
//   · **头部值**：C 的头部表是**独立写死的三项**，所以在 A/C 适用域内，
//     这三个头的值被改会红（变异 M8 实测红）；**域外**的头部（额外业务头、
//     非 body 方法下的 content-type）不参与 A/C 判据 —— 它们只在"有 go"时被真运行覆盖。
//   · A/C 的**适用域**见 [交叉验证 A2] 的说明：C 的头部表是写死的三项、且它的
//     `canonQueryOf` 对 **RawQuery** 二次转义（sign.go 是先解码再转义）——
//     域外的分歧**会打印出来**（标注"未证实谁对"），但不计数。
//   · escape 的保真度只到 sign.go 里那张替换表；`+` → `%20` 那条在 JS 侧是空操作。
//
// ## 可见性缺口（本项不能修，留给套件维护者一行改动）
//
// `run_frontend_suites.js` 对**通过**的探针只打印 `ok <file>`，会吞掉 stdout ——
// 于是 ENV 行在套件输出里看不到（直接跑本文件才看得到）。
// 建议：对 stdout 里含 `ENV ` 行的探针，把那一行也打出来。
// ============================================================================
'use strict';
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { spawnSync } = require('child_process');

const ROOT = process.env.WB2API_REPO || path.join(__dirname, '..', '..');
const SIGN_GO_PATH = path.join(ROOT, 'internal/codearts/sign.go');
const TEST_GO_PATH = path.join(ROOT, 'internal/codearts/sign_selfconsistency_test.go');
const VECTOR_PATH = path.join(ROOT, 'internal/codearts/testdata_vector.json');

// CRLF 归一：`$` 与 `[^;\n]` 这类写法在 `\r` 前会失配（本项目踩过），先归一再扫。
const SRC = fs.readFileSync(SIGN_GO_PATH, 'utf8').replace(/\r\n/g, '\n');
const TEST_SRC = fs.readFileSync(TEST_GO_PATH, 'utf8').replace(/\r\n/g, '\n');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
const envNotes = [];
// env(long, short)：long 打在发生地（要能直接排障），short 进汇总（别把排障长文重复一遍）
const env = (long, short) => { envNotes.push(short); console.log('  ENV  ' + long); };

// ⚠ authString：**先钉类型再比较**。
// 不做这一步的后果（本文件真实踩过）：把返回对象当字符串比 ⇒ 两边都是对象、
// `A.auth === B.auth` 恒不等、`while (i < A.auth.length)` 里 length=undefined ⇒ 打印
// "首个分歧在第 1 字符"（这就是拿错字段的指纹），或者抛 `… reading 'toLowerCase'`。
// ⚠ 绝不要用 `String(x) === String(y)` 去"修"：那会把两边都变成 `"[object Object]"`，
//   红变**假绿**。所以这里形状不对就响亮抛错，让"拿错字段"没有静默比较的可能。
function authString(x, what) {
  if (x === null || typeof x !== 'object' || typeof x.auth !== 'string') {
    throw new Error(what + ' 的返回值形状不对：期望 { auth: string, cr: string, sts: string }，实际 ' +
      (x === null ? 'null' : typeof x) +
      ' —— 拿错字段会让比较静默恒不等/恒相等，不许放过（也不许用 String() 掩盖）');
  }
  return x.auth;
}

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

// ---------------------------------------------------------------------------
// 基础工具：sha256 / HMAC（crypto 内建；零三方依赖，与仓库纪律一致）
// ---------------------------------------------------------------------------
const sha256Hex = s => crypto.createHash('sha256').update(String(s), 'utf8').digest('hex');
const hmacHex = (key, data) =>
  crypto.createHmac('sha256', Buffer.from(String(key), 'utf8')).update(String(data), 'utf8').digest('hex');

// must：抽不到就抛。**绝不允许**静默兜底 —— 兜底会让 A 恒等于 B，判别力归零。
function must(re, what, from) {
  const m = re.exec(from === undefined ? SRC : from);
  if (!m) {
    throw new Error('抽取失败（' + what + '）：' + re + ' 找不到 —— 守卫失效，不许静默放过\n' +
      '    可能：sign.go 改了写法/改了名。请把抽取规则与源码对齐（不要用默认值绕过）。');
  }
  return m;
}
// 按名字抠函数体（花括号配对）
function funcBody(src, marker) {
  const start = src.indexOf(marker);
  if (start < 0) throw new Error('源码里找不到 ' + marker + ' —— 抽取失败，守卫失效');
  let i = src.indexOf('{', start), d = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') d++;
    else if (src[i] === '}') { d--; if (d === 0) return src.slice(start, i + 1); }
  }
  throw new Error('括号不闭合: ' + marker);
}
// goUnquote：Go 字符串字面量 → 真值。
//
// ⚠⚠ 必须做这一步，本项目已实测踩过：从源码抽出来的 `"\n"` 在文件里是**两个字符**
//     （反斜杠 + n）。直接把它当数据用，A 会拼出字面量 `\n` 而不是换行 ——
//     canonicalRequest / stringToSign 整条都错，签名全不对，但**形状完全正常**
//     （长度对、hex 对、"看起来像个签名"），只有 A/B 逐字节对照才能看出来。
// 认不出的转义**当场抛**：宁可红，也不要"静默译错一个字符"。
function goUnquote(lit) {
  let out = '';
  for (let i = 0; i < lit.length; i++) {
    const c = lit[i];
    if (c !== '\\') { out += c; continue; }
    const n = lit[++i];
    if (n === 'n') out += '\n';
    else if (n === 't') out += '\t';
    else if (n === 'r') out += '\r';
    else if (n === '\\') out += '\\';
    else if (n === '"') out += '"';
    else if (n === "'") out += "'";
    else throw new Error('认不出的转义：\\' + n + '（在字面量 ' + JSON.stringify(lit) + ' 里）');
  }
  return out;
}

// ---------------------------------------------------------------------------
// A 的输入：从 sign.go 抽出的**签名流水线**（每条都带"认不出的表达式就抛错"的白名单）
// ---------------------------------------------------------------------------
function parsePipeline() {
  const SIGN = funcBody(SRC, 'func Sign(');
  const URI_FN = funcBody(SRC, 'func canonicalURI(');
  const ESC_FN = funcBody(SRC, 'func escape(');

  const P = {};
  P.alg = must(/algorithm\s*=\s*"([^"]+)"/, '算法名常量（algorithm）')[1];
  P.dateHeaderName = must(/dateHeader\s*=\s*"([^"]+)"/, 'dateHeader 常量')[1];
  P.upperMethod = /method\s*=\s*strings\.ToUpper\(method\)/.test(SIGN);
  P.hasBodyMethods = Array.from(SIGN.matchAll(/method == "([A-Z]+)"/g)).map(m => m[1]);
  P.dateHdrKey = must(/headers\["([^"]+)"\]\s*=\s*SDKDate\(time\.Now\(\)\)/, 'X-Sdk-Date 头名', SIGN)[1];
  P.tokenHdrKey = must(/headers\["([^"]+)"\]\s*=\s*cred\.SecurityToken/, '安全令牌头名', SIGN)[1];
  P.ctHdrKey = must(/if hasBody \{[\s\S]*?headers\["([^"]+)"\]\s*=\s*"([^"]+)"/, 'Content-Type 头名/值', SIGN)[1];
  P.ctValue = must(/if hasBody \{[\s\S]*?headers\["([^"]+)"\]\s*=\s*"([^"]+)"/, 'Content-Type 头名/值', SIGN)[2];

  // 归一：lower[<nameFn>(k)] = <trimFn>(v)
  // ⚠ Go 没有行尾分号 —— 这里只能靠换行收尾（写 `;` 会抽不到，本项目已踩过一次）
  const norm = must(/lower\[([^\]]+)\]\s*=\s*([^\n]+)/, '头部归一表达式', SIGN);
  P.lowerExpr = norm[1].trim();
  P.trimExpr = norm[2].trim();
  P.sortFn = must(/sort\.(\w+)\(signedKeys\)/, 'SignedHeaders 排序函数', SIGN)[1];
  P.signedJoin = must(/signedHeaders\s*:=\s*strings\.Join\(signedKeys,\s*"([^"]*)"\)/,
    'SignedHeaders 连接符', SIGN)[1];

  // canonicalHeaders：逐 key 写出的**顺序与字面量**（顺序变了要能红）
  const hdrLoop = must(/for _, k := range signedKeys \{([\s\S]*?)\n\t\}/, 'canonicalHeaders 循环', SIGN)[1];
  P.canonHdrSeq = Array.from(hdrLoop.matchAll(/canonicalHeaders\.WriteString\(([^)]*)\)/g)).map(m => m[1].trim());
  if (!P.canonHdrSeq.length) throw new Error('canonicalHeaders 循环里没抽到 WriteString —— 守卫失效');

  // canonicalURI 的流水线
  P.uriSplit = must(/strings\.Split\(path,\s*"([^"]*)"\)/, 'canonicalURI 的 Split 分隔符', URI_FN)[1];
  P.uriJoin = must(/strings\.Join\(segs,\s*"([^"]*)"\)/, 'canonicalURI 的 Join 分隔符', URI_FN)[1];
  P.uriSuffix = must(/strings\.HasSuffix\(uri,\s*"([^"]*)"\)/, 'canonicalURI 的尾缀检查', URI_FN)[1];
  P.uriAppend = must(/uri \+= "([^"]*)"/, 'canonicalURI 追加的尾缀', URI_FN)[1];

  // escape：QueryEscape → '+' 换 %20 → 替换表
  P.escPlus = [must(/strings\.ReplaceAll\(e,\s*"([^"]*)",\s*"([^"]*)"\)/, 'escape 的 + 替换', ESC_FN)[1],
    must(/strings\.ReplaceAll\(e,\s*"([^"]*)",\s*"([^"]*)"\)/, 'escape 的 + 替换', ESC_FN)[2]];
  const replacer = must(/strings\.NewReplacer\(([\s\S]*?)\)/, 'escape 的替换表', ESC_FN)[1];
  P.escPairs = Array.from(replacer.matchAll(/"([^"]*)",\s*"([^"]*)"/g)).map(m => [m[1], m[2]]);
  if (P.escPairs.length < 2) throw new Error('escape 替换表可疑（只抽到 ' + P.escPairs.length + ' 对）');

  // CanonicalQueryString：转义 → 排序 → k=v → '&'
  P.qEscape = Array.from(must(/qs = append\(qs, kv\{([\s\S]*?)\}\)/, 'query 的转义表达式', SIGN)[1]
    .split(',').map(s => s.trim()));
  P.qSortSrc = must(/sort\.Slice\(qs,[\s\S]*?\n\t\}\)/, 'query 的排序比较器', SIGN)[0];
  // 比较器必须"先比 k 再比 v"（顺序颠倒是静默缺陷，这里当场判）
  P.qSortKThenV = /qs\[i\]\.k != qs\[j\]\.k/.test(P.qSortSrc)
    && /qs\[i\]\.k < qs\[j\]\.k/.test(P.qSortSrc) && /qs\[i\]\.v < qs\[j\]\.v/.test(P.qSortSrc)
    && !/qs\[i\]\.k > qs\[j\]\.k/.test(P.qSortSrc);
  P.kvJoin = must(/parts\[i\] = p\.k \+ "([^"]*)" \+ p\.v/, 'query 的 k=v 连接符', SIGN)[1];
  P.pairsJoin = must(/canonicalQuery := strings\.Join\(parts, "([^"]*)"\)/, 'query 的对连接符', SIGN)[1];

  // canonicalRequest / stringToSign：**字段序**与连接符从源码抽出
  const cr = must(/canonicalRequest := strings\.Join\(\[\]string\{([\s\S]*?)\},\s*"([^"]*)"\)/,
    'canonicalRequest 的字段列表', SIGN);
  P.crFields = cr[1].split(',').map(s => s.trim()).filter(Boolean);
  P.crJoin = cr[2];
  const sts = must(/stringToSign := strings\.Join\(\[\]string\{([\s\S]*?)\},\s*"([^"]*)"\)/,
    'stringToSign 的字段列表', SIGN);
  P.stsFields = sts[1].split(',').map(s => s.trim()).filter(Boolean);
  P.stsJoin = sts[2];

  // 单次 HMAC 的 key/data
  const hm = must(/signature := hmacSHA256Hex\(([^,]+),\s*([A-Za-z0-9_]+)\)/, '签名调用', SIGN);
  P.hmacKeyExpr = hm[1].trim();
  P.hmacDataExpr = hm[2].trim();
  // hmacSHA256Hex 的实现必须是"单次"（key 原样进 hmac.New）
  const hmImpl = funcBody(SRC, 'func hmacSHA256Hex(');
  P.hmacSinglePass = /hmac\.New\(sha256\.New,\s*\[\]byte\(key\)\)/.test(hmImpl)
    && !/kDate|kRegion|kService|AWS4/.test(hmImpl);

  // Authorization 模板 + 参数序
  P.authFormat = must(/out\["Authorization"\]\s*=\s*fmt\.Sprintf\(\s*"([^"]*)"/, 'Authorization 模板', SIGN)[1];
  P.authArgs = Array.from(must(/out\["Authorization"\]\s*=\s*fmt\.Sprintf\(\s*"([^"]*)",\s*([\s\S]*?)\n\t\)/,
    'Authorization 参数', SIGN)[2].split(',').map(s => s.trim()).filter(Boolean));
  // sha256Hex 必须是 Sum256([]byte(s))（另一个 sha 变体 = 另一套哈希）
  const shImpl = funcBody(SRC, 'func sha256Hex(');
  P.shaSingle = /sha256\.Sum256\(\[\]byte\(s\)\)/.test(shImpl);
  P.shaCtor = /sha256\.Sum256/.test(shImpl);

  // ⚠ 所有"字面量数据"统一解码（见 goUnquote 的注释：不解码 = 静默译错一个字符）
  for (const k of ['alg', 'dateHeaderName', 'dateHdrKey', 'tokenHdrKey', 'ctHdrKey', 'ctValue',
    'signedJoin', 'uriSplit', 'uriJoin', 'uriSuffix', 'uriAppend', 'kvJoin', 'pairsJoin',
    'crJoin', 'stsJoin', 'authFormat']) {
    P[k] = goUnquote(P[k]);
  }
  P.escPlus = P.escPlus.map(goUnquote);
  P.escPairs = P.escPairs.map(p => p.map(goUnquote));
  return P;
}

// A 的重放：只用**抽出来的**东西执行；每个认不出的表达式当场抛错。
function srcEscape(P, s) {
  let e = encodeURIComponent(String(s));
  for (const [from, to] of P.escPairs) e = e.split(from).join(to);
  return e;
}
function applyNorm(expr, name, value) {
  if (expr === 'strings.ToLower(k)') return name.toLowerCase();
  if (expr === 'strings.TrimSpace(v)') return String(value).trim();
  throw new Error('认不出的归一表达式：' + expr + '（只认 ToLower / TrimSpace；改了就请更新白名单）');
}
function fieldValue(expr, ctx, where) {
  const map = {
    method: ctx.method,
    uri: ctx.uri,
    canonicalQuery: ctx.query,
    'canonicalHeaders.String()': ctx.canonHeaders,
    signedHeaders: ctx.signedHeaders,
    'sha256Hex(body)': sha256Hex(ctx.body),
    algorithm: ctx.alg,
    'lower[dateHeader]': ctx.date,
    'sha256Hex(canonicalRequest)': sha256Hex(ctx.canonicalRequest),
  };
  if (!(expr in map)) {
    throw new Error('认不出的字段（' + where + '）：' + expr + ' —— 抽取保真度不够，不许静默放过');
  }
  return map[expr];
}
function runSrcPipeline(P, v) {
  const method = P.upperMethod ? String(v.method).toUpperCase() : String(v.method);
  // 头部集合（实现策略）：extra → date → token → Content-Type
  const headers = {};
  for (const [k, val] of Object.entries(v.extra || {})) {
    if (k.toLowerCase() === 'authorization') continue;
    headers[k] = val;
  }
  if (!(P.dateHdrKey in headers)) headers[P.dateHdrKey] = v.date;   // 替掉 time.Now()
  if (v.st && !(P.tokenHdrKey in headers)) headers[P.tokenHdrKey] = v.st;
  if (P.hasBodyMethods.indexOf(method) >= 0 && !(P.ctHdrKey in headers)) headers[P.ctHdrKey] = P.ctValue;

  const lower = {};
  for (const [k, val] of Object.entries(headers)) {
    if (k.toLowerCase() === 'authorization') continue;
    lower[applyNorm(P.lowerExpr, k, val)] = applyNorm(P.trimExpr, k, val);
  }
  const keys = Object.keys(lower);
  if (P.sortFn !== 'Strings') throw new Error('认不出的排序函数：' + P.sortFn + '（只认 sort.Strings）');
  keys.sort();
  const signedHeaders = keys.join(P.signedJoin);
  let canonHeaders = '';
  for (const k of keys) {
    for (const e of P.canonHdrSeq) {
      if (e === 'k') canonHeaders += k;
      else if (e === 'lower[k]') canonHeaders += lower[k];
      else if (/^".*"$/.test(e)) canonHeaders += goUnquote(e.slice(1, -1));   // 字面量要解码
      else throw new Error('认不出的 canonicalHeaders 写法：' + e);
    }
  }
  const uri = (() => {
    let out = String(v.path).split(P.uriSplit).map(seg => srcEscape(P, seg)).join(P.uriJoin);
    if (!out.endsWith(P.uriSuffix)) out += P.uriAppend;
    return out;
  })();
  const query = (() => {
    if (P.qEscape[0] !== 'escape(k)' || P.qEscape[1] !== 'escape(v)') {
      throw new Error('认不出的 query 转义表达式：' + JSON.stringify(P.qEscape));
    }
    const pairs = v.queryPairs.map(([k, val]) => [srcEscape(P, k), srcEscape(P, val)]);
    if (!P.qSortKThenV) throw new Error('query 的排序比较器不是"先比 k 再比 v"（抽出的源码见文件头说明）');
    pairs.sort((a, b) => (a[0] !== b[0] ? (a[0] < b[0] ? -1 : 1) : (a[1] < b[1] ? -1 : a[1] > b[1] ? 1 : 0)));
    return pairs.map(p => p[0] + P.kvJoin + p[1]).join(P.pairsJoin);
  })();
  const ctx = { method, uri, query, canonHeaders, signedHeaders, body: v.body, alg: P.alg,
    date: lower[P.dateHeaderName] };
  const canonicalRequest = P.crFields.map(f => fieldValue(f, ctx, 'canonicalRequest')).join(P.crJoin);
  ctx.canonicalRequest = canonicalRequest;
  const stringToSign = P.stsFields.map(f => fieldValue(f, ctx, 'stringToSign')).join(P.stsJoin);
  if (P.hmacKeyExpr !== 'cred.SecretKey') {
    throw new Error('签名 key 不是直接用 cred.SecretKey（抽到：' + P.hmacKeyExpr +
      '）—— 要么是派生链，要么抽取规则过期，两者都必须红');
  }
  if (P.hmacDataExpr !== 'stringToSign') {
    throw new Error('HMAC 的 data 不是 stringToSign（抽到：' + P.hmacDataExpr + '）');
  }
  if (!P.hmacSinglePass || !P.shaSingle) throw new Error('hmac/sha 的实现不是"单次"那套');
  const signature = hmacHex(v.sk, stringToSign);
  const values = P.authArgs.map((a, i) => {
    if (a === 'algorithm') return P.alg;
    if (a === 'cred.AccessKey') return v.ak;
    if (a === 'signedHeaders') return signedHeaders;
    if (a === 'signature') return signature;
    throw new Error('认不出的 Authorization 参数#' + (i + 1) + '：' + a);
  });
  let out = P.authFormat;
  for (const val of values) out = out.replace('%s', val);
  if (out.indexOf('%s') >= 0) throw new Error('Authorization 模板里的 %s 比抽出的参数多：' + P.authFormat);
  return { auth: out, cr: canonicalRequest, sts: stringToSign };
}

// ---------------------------------------------------------------------------
// B：按**文档 7 条**独立重写（不读 sign.go；头部集合沿用 A 抽出的那份，见文件头边界说明）
// ---------------------------------------------------------------------------
const DOC_ALG = 'SDK-HMAC-SHA256';          // 文档规则 5 的字面量
function docEscape(s) {
  // 文档口径：URI 编码；与 JS encodeURIComponent 对齐（sign.go:37-39 记了这条对齐关系）
  return encodeURIComponent(String(s)).replace(/[!'()*]/g, c => '%' + c.charCodeAt(0).toString(16).toUpperCase());
}
function runDocImpl(v, headersResolved, dateHeaderName) {
  // 规则 2：lower(name) + ':' + trim(value) + '\n'，且按名字排序（规则 3）
  const lower = {};
  for (const [k, val] of Object.entries(headersResolved)) lower[k.toLowerCase()] = String(val).trim();
  const names = Object.keys(lower).sort();
  const canonHeaders = names.map(n => n + ':' + lower[n] + '\n').join('');
  // 规则 1：逐段编码，末尾补 '/'
  let uri = String(v.path).split('/').map(docEscape).join('/');
  if (!uri.endsWith('/')) uri += '/';
  // 查询串：编码 → 先 key 后 value 排序 → '&'
  const pairs = v.queryPairs.map(([k, val]) => [docEscape(k), docEscape(val)]);
  pairs.sort((a, b) => (a[0] !== b[0] ? (a[0] < b[0] ? -1 : 1) : (a[1] < b[1] ? -1 : a[1] > b[1] ? 1 : 0)));
  const query = pairs.map(p => p[0] + '=' + p[1]).join('&');
  // 规则 4：空 body 用空串哈希（'sha256Hex("")' 就是它的空串哈希）
  const canonicalRequest = [String(v.method).toUpperCase(), uri, query, canonHeaders, names.join(';'),
    sha256Hex(v.body)].join('\n');
  // 规则 5：三段。date 的取值按 A 抽出的那个头名取（头名属头部集合 = 实现策略）
  const stringToSign = [DOC_ALG, lower[dateHeaderName], sha256Hex(canonicalRequest)].join('\n');
  // 规则 6：**单次** HMAC，key = 原始 secretKey
  const signature = hmacHex(v.sk, stringToSign);
  // 规则 7：algorithm + **空格** + Access=.., SignedHeaders=.., Signature=..
  return {
    auth: DOC_ALG + ' Access=' + v.ak + ', SignedHeaders=' + names.join(';') + ', Signature=' + signature,
    cr: canonicalRequest, sts: stringToSign,
  };
}

// ---------------------------------------------------------------------------
// 向量：① 仓库向量文件（外部锚点）② go test 的自造密钥向量（解析出来，不硬编码）
//       ③ 自造硬向量（打 escape / query 排序 / 空 body / 无尾斜杠 / 头部值带空白）
// ---------------------------------------------------------------------------
function urlParts(u) {
  const parsed = new URL(u);
  // rawQuery 保留**原始**（未解码）的查询串：测试文件的 canonQueryOf 就是拿它再转义的
  return { path: parsed.pathname, queryPairs: Array.from(parsed.searchParams.entries()),
    rawQuery: parsed.search.replace(/^\?/, '') };
}
// 从 sign_selfconsistency_test.go 解析它用的向量（改了测试文件这里会跟着变/报错，不是硬编码副本）
function parseGoTestVector() {
  const body = funcBody(TEST_SRC, 'func TestSignSelfConsistencyWithKnownKey(');
  const cred = must(/cred := Credential\{([\s\S]*?)\}/, '测试里的 Credential', body)[1];
  const g = (re, what) => must(re, what, body)[1];
  const out = {
    ak: must(/AccessKey:\s*"([^"]*)"/, '测试里的 AccessKey', cred)[1],
    sk: must(/SecretKey:\s*"([^"]*)"/, '测试里的 SecretKey', cred)[1],
    st: must(/SecurityToken:\s*"([^"]*)"/, '测试里的 SecurityToken', cred)[1],
    method: g(/method\s*=\s*"([^"]+)"/, '测试里的 method'),
    url: g(/rawURL\s*=\s*"([^"]+)"/, '测试里的 rawURL'),
    // ⚠ 反引号里的 body 自带双引号（`{"model":"GLM-5.2"}`），所以不能写 [^"]*?
    body: g(/body\s*=\s*`([\s\S]*?)`/, '测试里的 body'),
    date: g(/date\s*=\s*"([^"]+)"/, '测试里的 date'),
    extraHdr: g(/map\[string\]string\{"([^"]+)":\s*date\}/, '测试里的 extra 头名'),
  };
  const up = urlParts(out.url);
  out.path = up.path;
  out.queryPairs = up.queryPairs;
  out.rawQuery = up.rawQuery;
  out.extra = { [out.extraHdr]: out.date };
  return out;
}

const P = parsePipeline();
const vecFile = JSON.parse(fs.readFileSync(VECTOR_PATH, 'utf8'));
const PLACEHOLDER_SK = 'SECRET-NOT-IN-REPO';   // 向量刻意不含 secretKey（见文件头边界说明）
const VECS = [];
for (const c of vecFile.cases) {
  const up = urlParts(c.url);
  VECS.push({
    name: 'vector:' + c.name, anchor: c.wantAuth,
    method: c.method, url: c.url, path: up.path, queryPairs: up.queryPairs, rawQuery: up.rawQuery,
    body: c.body, date: c.date, ak: vecFile.accessKey, sk: PLACEHOLDER_SK,
    st: 'TOKENEXAMPLE', extra: { 'X-Sdk-Date': c.date },
    note: 'testdata_vector.json（由已实测通过服务端校验的 JS 实现生成）',
  });
}
const gt = parseGoTestVector();
VECS.push({
  name: 'go-test 自造密钥向量', method: gt.method, url: gt.url, path: gt.path,
  queryPairs: gt.queryPairs, rawQuery: gt.rawQuery,
  body: gt.body, date: gt.date, ak: gt.ak, sk: gt.sk, st: gt.st, extra: gt.extra,
  note: 'sign_selfconsistency_test.go:46 的向量（自造密钥）',
});
const D = '20260101T000000Z';
VECS.push({
  name: '硬向量：query 需转义 + 重复 key + 路径无尾斜杠',
  method: 'GET', url: 'https://h.example.com/v5/caller-identity?kw=a%20b&kw=a%21b&z=1',
  path: '/v5/caller-identity', queryPairs: [['kw', 'a b'], ['kw', 'a!b'], ['z', '1']],
  rawQuery: 'kw=a%20b&kw=a%21b&z=1',
  body: '', date: D, ak: 'AKIDEXAMPLE', sk: 'SECRETEXAMPLE', st: 'TOKENEXAMPLE',
  extra: { 'X-Sdk-Date': D }, note: '自造：打 escape（空格/!）、重复 key 排序、尾斜杠',
});
VECS.push({
  name: '硬向量：POST 空 body + 头部值带首尾空白',
  method: 'POST', url: 'https://h.example.com/v1/oauth2/tokens',
  path: '/v1/oauth2/tokens', queryPairs: [], rawQuery: '', body: '', date: D,
  ak: 'AKIDEXAMPLE', sk: 'SECRETEXAMPLE', st: 'TOKENEXAMPLE',
  extra: { 'X-Sdk-Date': D, 'X-Custom': '  padded  ' },
  note: '自造：打规则 4（空 body 空串哈希）与规则 2 的 Trimall',
});

// ---------------------------------------------------------------------------
// [交叉验证 A] 源码抽出的流水线 vs 文档独立重写 —— 逐字节
// ---------------------------------------------------------------------------
console.log('\n[交叉验证 A] 源码抽出的流水线（A） vs 文档独立重写（B）—— 逐字节');
console.log('  （A 的常量/分隔符/归一/字段序/模板全部来自 sign.go：' +
  P.alg + ' / SignedHeaders 连接符 ' + JSON.stringify(P.signedJoin) + ' / 字段序 ' +
  P.crFields.length + ' 项 / ' + P.hmacKeyExpr + '）');
const results = [];
for (const v of VECS) {
  let A = null, B = null, err = null;
  try {
    A = runSrcPipeline(P, v);
    // B 沿用 A 抽出的头部集合（头部集合是实现策略，文档不管；见文件头边界说明）
    B = runDocImpl(v, runSrcHeaders(P, v), P.dateHeaderName);
    // ⚠ 比较前**先钉类型**：拿错字段（把对象当串比）是本文件踩过的坑，见 authString 注释
    authString(A, 'runSrcPipeline'); authString(B, 'runDocImpl');
  } catch (e) { err = e; }
  results.push({ v, a: A && A.auth, b: B && B.auth, A, B, err });
  if (err) { ok(false, 'A/B 对照 ' + v.name + ' —— 抽取/重放失败：' + err.message); continue; }
  if (A.auth === B.auth) {
    ok(true, 'A == B：' + v.name + '（' + v.note + '）');
  } else {
    let i = 0; while (i < A.auth.length && A.auth[i] === B.auth[i]) i++;
    ok(false, 'A != B：' + v.name + '（' + v.note + '）\n' +
      '      首个分歧在第 ' + (i + 1) + ' 字符\n' +
      '      A(源码抽出的流水线)= ' + A.auth + '\n      B(文档独立重写)     = ' + B.auth + '\n' +
      // 中间量必须打出来：只给两个签名的话，"差在哪一段"要重新手工算一遍
      '      A 的 canonicalRequest = ' + JSON.stringify(A.cr) + '\n' +
      '      B 的 canonicalRequest = ' + JSON.stringify(B.cr) + '\n' +
      '      A 的 stringToSign     = ' + JSON.stringify(A.sts) + '\n' +
      '      B 的 stringToSign     = ' + JSON.stringify(B.sts));
  }
}
// 头部集合要能被 B 复用，所以单独抽出来（与 runSrcPipeline 内同一套策略，避免两份实现）
function runSrcHeaders(Pp, v) {
  const headers = {};
  for (const [k, val] of Object.entries(v.extra || {})) {
    if (k.toLowerCase() === 'authorization') continue;
    headers[k] = val;
  }
  if (!(Pp.dateHdrKey in headers)) headers[Pp.dateHdrKey] = v.date;
  if (v.st && !(Pp.tokenHdrKey in headers)) headers[Pp.tokenHdrKey] = v.st;
  const m = Pp.upperMethod ? String(v.method).toUpperCase() : String(v.method);
  if (Pp.hasBodyMethods.indexOf(m) >= 0 && !(Pp.ctHdrKey in headers)) headers[Pp.ctHdrKey] = Pp.ctValue;
  return headers;
}

// ---------------------------------------------------------------------------
// [交叉验证 A2] C = 从**测试文件**的 independentSign 抽出的流水线
//
// 这一路是那条 `go test -run TestSignSelfConsistencyWithKnownKey` 的**静态等价物**：
// 该测试做的事就是"Sign() 的产出 vs independentSign() 的产出，逐字节比"，
// 而这里把 independentSign 也抽出来在 JS 里重放，于是**不需要 go 也能证明
// 测试的期望实现与实现本身一致**（在 C 适用的向量域内，见下面的 cApplicable）。
//
// ⚠ 域限制是**诚实标注**，不是取巧：independentSign 的头部表是写死的三项
//   （content-type / x-sdk-date / x-security-token），且它的 canonQueryOf 对
//   **原始 RawQuery** 直接再转义（sign.go 是 u.Query() 先解码再转义）——
//   两者只在这种输入上同域：
//     · extra 恰好只有 X-Sdk-Date（没有额外业务头）
//     · raw query 里没有 %XX 或 '+'（否则 sign.go 会解码再编码，测试实现会二次转义）
//   域外的分歧点单独打印（未证实谁对 —— 见文件头"已知边界"），不混进 A2 的判据。
// ---------------------------------------------------------------------------
function parseTestImpl() {
  const IND = funcBody(TEST_SRC, 'func independentSign(');
  const TESTFN = funcBody(TEST_SRC, 'func TestSignSelfConsistencyWithKnownKey(');
  const IURI = funcBody(TEST_SRC, 'func canonURIOf(');
  const IQRY = funcBody(TEST_SRC, 'func canonQueryOf(');
  const C = {};
  const hdrBlock = must(/hdrs := map\[string\]string\{([\s\S]*?)\n\t\}/, 'independentSign 的头部表', IND)[1];
  C.hdrs = Array.from(hdrBlock.matchAll(/"([^"]+)":\s*([^,\n]+),/g)).map(m => [m[1], m[2].trim()]);
  if (!C.hdrs.length) throw new Error('抽不到 independentSign 的头部表 —— 守卫失效');
  C.sortNames = /sort\.Strings\(names\)/.test(IND);
  const loop = must(/for _, n := range names \{([\s\S]*?)\n\t\}/, 'independentSign 的 canonical headers 循环', IND)[1];
  C.hdrSeq = Array.from(loop.matchAll(/hb\.WriteString\(([^\n]*)\)\s*$/gm)).map(m => m[1].trim());
  if (!C.hdrSeq.length) throw new Error('independentSign 的 canonical headers 循环里没抽到 WriteString');
  C.signedJoin = goUnquote(must(/signedHeaders := strings\.Join\(names,\s*"([^"]*)"\)/, 'independentSign 的 SignedHeaders 连接符', IND)[1]);
  const can = must(/canonical := strings\.Join\(\[\]string\{([\s\S]*?)\},\s*"([^"]*)"\)/, 'independentSign 的 canonical 字段表', IND);
  C.crFields = can[1].split(',').map(s => s.trim()).filter(Boolean);
  C.crJoin = goUnquote(can[2]);
  const sts = must(/stringToSign := strings\.Join\(\[\]string\{([\s\S]*?)\},\s*"([^"]*)"\)/, 'independentSign 的 stringToSign 字段表', IND);
  C.stsFields = sts[1].split(',').map(s => s.trim()).filter(Boolean);
  C.stsJoin = goUnquote(sts[2]);
  must(/sig := hex\.EncodeToString\(hmacRaw\(\[\]byte\(cred\.SecretKey\),\s*stringToSign\)\)/, 'independentSign 的签名调用', IND);
  C.hmacSinglePass = /\[\]byte\(cred\.SecretKey\)/.test(IND) && !/kDate|kRegion|kService|AWS4/.test(IND);
  C.retExpr = must(/return ([\s\S]*?)\n\}/, 'independentSign 的 return 表达式', IND)[1];
  C.authParts = C.retExpr.split('+').map(s => s.trim()).filter(Boolean);
  // canonicalURI / canonicalQuery 的流水线（测试文件里各自独立实现）
  C.uriSplit = goUnquote(must(/strings\.Split\(p,\s*"([^"]*)"\)/, 'canonURIOf 的 Split', IURI)[1]);
  C.uriJoin = goUnquote(must(/strings\.Join\(segs,\s*"([^"]*)"\)/, 'canonURIOf 的 Join', IURI)[1]);
  C.uriSuffix = goUnquote(must(/strings\.HasSuffix\(out,\s*"([^"]*)"\)/, 'canonURIOf 的尾缀检查', IURI)[1]);
  C.uriAppend = goUnquote(must(/out \+= "([^"]*)"/, 'canonURIOf 追加的尾缀', IURI)[1]);
  C.qSplit = goUnquote(must(/strings\.Split\(raw,\s*"([^"]*)"\)/, 'canonQueryOf 的 Split', IQRY)[1]);
  C.qSplitN = goUnquote(must(/strings\.SplitN\(p,\s*"([^"]*)",\s*2\)/, 'canonQueryOf 的 SplitN', IQRY)[1]);
  C.qKvJoin = goUnquote(must(/pairs\[i\] = k \+ "([^"]*)" \+ v/, 'canonQueryOf 的 k=v 连接符', IQRY)[1]);
  C.qJoin = goUnquote(must(/return strings\.Join\(pairs,\s*"([^"]*)"\)/, 'canonQueryOf 的对连接符', IQRY)[1]);
  C.qSort = /sort\.Strings\(pairs\)/.test(IQRY);
  C.qEscapes = /escape\(kv\[0\]\)/.test(IQRY) && /escape\(kv\[1\]\)/.test(IQRY);
  C.logf = Array.from(TESTFN.matchAll(/t\.Logf\("([\s\S]*?)%s",\s*([A-Za-z0-9_]+)\)/g))
    .map(m => [m[1].replace(/\s+$/, ''), m[2]]);
  if (C.logf.length < 2) throw new Error('抽不到测试里那两条 t.Logf 标签 —— 守卫失效（见文件头说明）');
  C.shaSingle = /sha256\.Sum256\(\[\]byte\(s\)\)/.test(funcBody(TEST_SRC, 'func hexSHA256('));
  return C;
}
function testImplFieldValue(Cc, expr, ctx) {
  const map = {
    'strings.ToUpper(method)': ctx.method,
    'canonURI': ctx.uri,
    'canonQuery': ctx.query,
    'hb.String()': ctx.canonHeaders,
    'signedHeaders': ctx.signedHeaders,
    'hexSHA256(body)': sha256Hex(ctx.body),
    '"SDK-HMAC-SHA256"': DOC_ALG,
    'date': ctx.date,
    'hexSHA256(canonical)': sha256Hex(ctx.canonicalRequest),
  };
  if (!(expr in map)) throw new Error('认不出的字段（independentSign 的 ' + ctx.where + '）：' + expr);
  return map[expr];
}
function runTestImpl(Cc, v, P) {
  if (!Cc.sortNames) throw new Error('independentSign 没有对头部名排序（sort.Strings(names)）');
  if (!Cc.qSort) throw new Error('canonQueryOf 没有排序（sort.Strings(pairs)）');
  if (!Cc.qEscapes) throw new Error('canonQueryOf 没有转义 key/value');
  if (!Cc.hmacSinglePass || !Cc.shaSingle) throw new Error('independentSign 的哈希/HMAC 不是"单次"那套');
  // 头部：测试里写死的三项，值按表达式解析
  const raw = {};
  for (const [name, expr] of Cc.hdrs) {
    if (/^".*"$/.test(expr)) raw[name] = goUnquote(expr.slice(1, -1));
    else if (expr === 'date') raw[name] = v.date;
    else if (expr === 'cred.SecurityToken') raw[name] = v.st;
    else throw new Error('认不出的头值表达式：' + expr);
  }
  const names = Object.keys(raw);
  names.sort();
  let hb = '';
  for (const n of names) {
    for (const e of Cc.hdrSeq) {
      if (e === 'n') hb += n;
      else if (e === 'strings.TrimSpace(hdrs[n])') hb += String(raw[n]).trim();
      else if (/^".*"$/.test(e)) hb += goUnquote(e.slice(1, -1));
      else throw new Error('认不出的 canonical headers 写法（independentSign）：' + e);
    }
  }
  const signedHeaders = names.join(Cc.signedJoin);
  let uri = String(v.path).split(Cc.uriSplit).map(s => srcEscape(P, s)).join(Cc.uriJoin);
  if (!uri.endsWith(Cc.uriSuffix)) uri += Cc.uriAppend;
  const query = (() => {
    if (v.rawQuery === '') return '';
    const pairs = v.rawQuery.split(Cc.qSplit).map(p => {
      const i = p.indexOf(Cc.qSplitN);
      const k = i < 0 ? p : p.slice(0, i);
      const val = i < 0 ? '' : p.slice(i + Cc.qSplitN.length);
      return srcEscape(P, k) + Cc.qKvJoin + srcEscape(P, val);
    });
    pairs.sort();
    return pairs.join(Cc.qJoin);
  })();
  const ctx = { method: String(v.method).toUpperCase(), uri, query, canonHeaders: hb, signedHeaders,
    body: v.body, date: v.date, where: 'canonical' };
  const canonicalRequest = Cc.crFields.map(f => testImplFieldValue(Cc, f, ctx)).join(Cc.crJoin);
  ctx.canonicalRequest = canonicalRequest;
  ctx.where = 'stringToSign';
  const stringToSign = Cc.stsFields.map(f => testImplFieldValue(Cc, f, ctx)).join(Cc.stsJoin);
  const signature = hmacHex(v.sk, stringToSign);
  const auth = Cc.authParts.map(part => {
    if (/^"[\s\S]*"$/.test(part)) return goUnquote(part.slice(1, -1));
    if (part === 'cred.AccessKey') return v.ak;
    if (part === 'signedHeaders') return signedHeaders;
    if (part === 'sig') return signature;
    throw new Error('认不出的 Authorization 拼接段（independentSign）：' + part);
  }).join('');
  return { auth, cr: canonicalRequest, sts: stringToSign };
}

console.log('\n[交叉验证 A2] A（sign.go） vs C（测试文件里的独立重写）—— 即那条 go test 的静态等价物');
const C = parseTestImpl();
console.log('  （C 的常量/分隔符/字段序/头部表全部来自 sign_selfconsistency_test.go：' +
  '头部表 ' + C.hdrs.map(h => h[0]).join('/') + ' / 字段序 ' + C.crFields.length + ' 项）');
let cCompared = 0, cSkipped = [];
// A/C 的适用域（判据见上面 A2 的说明）
const cApplicable = v => {
  const keys = Object.keys(v.extra || {});
  return keys.length === 1 && keys[0] === 'X-Sdk-Date'      // 没有额外业务头
    && !/[%+]/.test(v.rawQuery)                             // query 里没有 %XX / '+'
    && P.hasBodyMethods.indexOf(String(v.method).toUpperCase()) >= 0;  // 方法在测试那条向量的形状内
};
for (const v of VECS) {
  if (!cApplicable(v)) { cSkipped.push(v.name); continue; }
  cCompared++;
  let A = null, Cc = null, err = null;
  try {
    A = runSrcPipeline(P, v); Cc = runTestImpl(C, v, P);
    authString(A, 'runSrcPipeline'); authString(Cc, 'runTestImpl');   // 先钉类型
  } catch (e) { err = e; }
  if (err) { ok(false, 'A/C 对照 ' + v.name + ' —— 抽取/重放失败：' + err.message); continue; }
  if (Cc.auth === A.auth) {
    ok(true, 'A == C：' + v.name + '（测试的期望实现与实现逐字节一致）');
  } else {
    let i = 0; while (i < A.auth.length && Cc.auth[i] === A.auth[i]) i++;
    ok(false, 'A != C：' + v.name + '（第 ' + (i + 1) + ' 字符起分歧）\n' +
      '      A(sign.go)        = ' + A.auth + '\n      C(independentSign)= ' + Cc.auth + '\n' +
      '      A 的 canonical = ' + JSON.stringify(A.cr) + '\n      C 的 canonical = ' + JSON.stringify(Cc.cr));
  }
}
ok(cCompared >= 3, 'A/C 在适用域内至少比了 3 个向量（实际 ' + cCompared + ' 个）—— 域外的不算：' +
  cSkipped.join('、'));
// 域外向量必须**被说出来**（未证实谁对，所以不计数，但绝不能静默跳过）
for (const v of VECS) {
  if (cApplicable(v)) continue;
  const A = runSrcPipeline(P, v), Cc = runTestImpl(C, v, P);
  const la = A.cr.split('\n'), lb = Cc.cr.split('\n');
  const diff = [];
  for (let i = 0; i < Math.max(la.length, lb.length); i++) if (la[i] !== lb[i]) diff.push(i);
  if (!diff.length) {
    console.log('  ENV  ' + v.name + '：A/C 适用域外（' +
      (Object.keys(v.extra || {}).length !== 1 ? '带额外业务头' :
        /[%+]/.test(v.rawQuery) ? 'query 含 %XX/+' : '方法不是 body 方法') +
      '），但本向量下 A 与 C 的 canonical **逐字节相同**');
  } else {
    const i = diff[0];
    console.log('  ENV  ' + v.name + '：A/C 适用域外，canonical 第 ' + diff.map(x => x + 1).join('/') +
      ' 行不同：\n        A = ' + JSON.stringify(la[i]) + '\n        C = ' + JSON.stringify(lb[i]) +
      '\n        已知的两种域外成因：① canonQueryOf 对 **RawQuery** 二次转义' +
      '（sign.go 是 u.Query() 先解码再转义）② C 的头部表是**写死的三项**，' +
      '对非 body 方法 / 带额外业务头时不适用。谁对**未证实**' +
      '（仓库向量与那条 go test 都不覆盖这种输入），需要真实凭证或补一条带转义的向量才能定');
  }
}

// ---------------------------------------------------------------------------
// [交叉验证 B] 外部锚点：wantAuth 的 Access= / SignedHeaders= 两段与密钥无关
// ---------------------------------------------------------------------------
console.log('\n[交叉验证 B] 外部锚点：testdata_vector.json 的 wantAuth 段（两段与密钥无关）');
let anchored = 0;
for (const r of results) {
  if (!r.v.anchor) continue;
  const m = /^SDK-HMAC-SHA256 Access=([^,]+), SignedHeaders=([^,]+), Signature=([0-9a-f]+)$/.exec(r.v.anchor);
  if (!m) { ok(false, 'wantAuth 形状不认识：' + r.v.anchor); continue; }
  const mine = /^SDK-HMAC-SHA256 Access=([^,]+), SignedHeaders=([^,]+), Signature=([0-9a-f]+)$/.exec(r.a || '');
  ok(!!mine, '本地产出的 Authorization 形状与 wantAuth 同构：' + r.v.name);
  if (!mine) continue;
  ok(mine[1] === m[1] && mine[2] === m[2],
    'Access / SignedHeaders 段逐字节相同：' + r.v.name +
    '\n      本地 ' + mine[1] + ' | ' + mine[2] + '\n      向量 ' + m[1] + ' | ' + m[2]);
  ok(mine[3].length === 64 && mine[3] === mine[3].toLowerCase() && m[3].length === 64,
    'Signature 段形状一致（64 位小写 hex；**值未核对** —— 向量刻意不含 secretKey）：' + r.v.name);
  anchored++;
}
ok(anchored === 3, '仓库向量文件的 3 个 case 都做了锚点核对（实际 ' + anchored + ' 个）');

// ---------------------------------------------------------------------------
// [交叉验证 C] go test：有 go 才真跑；没有则 ENV（不计数 FAIL，但**大声**说明）
// ---------------------------------------------------------------------------
console.log('\n[交叉验证 C] go test 端到端（TestSignSelfConsistencyWithKnownKey）');
// 先静态确认那个测试还存在 —— 否则 go test -run 一个不存在的名字也是 exit 0（假绿）
ok(/func TestSignSelfConsistencyWithKnownKey\(/.test(TEST_SRC),
  '测试名仍存在于 sign_selfconsistency_test.go（否则 -run 一个不存在的名字也会 exit 0）');

function findGo() {
  const tried = [];
  if (process.env.WB2API_GO) {
    if (fs.existsSync(process.env.WB2API_GO)) return { exe: process.env.WB2API_GO, why: 'WB2API_GO 环境变量' };
    tried.push('WB2API_GO=' + process.env.WB2API_GO + '（该文件不存在）');
  }
  const p = spawnSync('go', ['version'], { encoding: 'utf8' });
  if (!p.error && p.status === 0) return { exe: 'go', why: 'PATH' };
  tried.push('PATH 上的 go（spawnSync 失败：' + (p.error ? p.error.code : 'exit ' + p.status) + '）');
  const cands = [
    // 本机实测位置：go1.22.5 装在这里，**不在默认 PATH**（Windows 上要手动前置）
    'D:\\go-path\\go\\bin\\go.exe',
    'C:\\Program Files\\Go\\bin\\go.exe', 'C:\\Go\\bin\\go.exe',
    path.join(process.env.LOCALAPPDATA || '', 'Programs', 'Go', 'bin', 'go.exe'),
    'D:\\software\\go\\bin\\go.exe', 'D:\\Go\\bin\\go.exe',
  ];
  for (const c of cands) {
    if (c && fs.existsSync(c)) return { exe: c, why: '常见安装位置 ' + c + '（不在 PATH，需要时手动前置）' };
    tried.push(c);
  }
  // 本机特有的坏 PATH 项：PATH 里有、但目录根本不存在（点名它，省下一次排障）
  // ⚠ 过滤要精确到"看起来是 Go 工具链的目录"：`/go/i` 会把 `.cargo\bin` 也卷进来（c-a-r-**go**），
  //   实测踩过一次，于是报告里出现了三条莫名其妙的 ".cargo\bin 不存在"。
  const broken = (process.env.PATH || '').split(path.delimiter)
    .filter(d => d && !fs.existsSync(d))
    .filter(d => /(^|[\\\/])(go|go1[^\\\/]*|golang|gobin)([\\\/]|$)/i.test(d));
  return { exe: null, why: '没找到 go；试过：' + tried.join('、') +
    (broken.length ? '；PATH 里这些目录**不存在**：' + broken.join('、') : '') +
    '；本机实测 go 在 D:\\go-path\\go\\bin\\go.exe（不在默认 PATH）' };
}
const goInfo = findGo();
if (!goInfo.exe) {
  env('本条判据**未被执行**：没找到 go 工具链 —— ' + goInfo.why,
    'go test 未执行（没找到 go；原因长文见上面 ENV 行）');
  env('判别力由 [交叉验证 A/A2/B] 保留（纯 JS，不经 go）：A=sign.go 抽出的流水线、' +
    'B=按文档独立重写、C=测试文件的 independentSign 抽出的流水线，三者互相逐字节一致，' +
    '且 Access/SignedHeaders 段与仓库向量 wantAuth 锚定',
    '判别力由 A/A2/B 纯 JS 对照保留（不经 go）');
  env('把 go 前置进 PATH（本机：D:\\go-path\\go\\bin）或设 WB2API_GO=<go.exe 路径>，' +
    '本项自动变成真跑，无需改代码',
    '前置 go 或设 WB2API_GO 即自动真跑（不需改代码）');
} else {
  const r = spawnSync(goInfo.exe, ['test', './internal/codearts/', '-run',
    'TestSignSelfConsistencyWithKnownKey', '-count=1', '-v'],
    { cwd: ROOT, encoding: 'utf8', env: Object.assign({}, process.env, { CGO_ENABLED: '0' }) });
  const out = String(r.stdout || '') + String(r.stderr || '');
  ok(r.status === 0, 'go test 通过（go 来自 ' + goInfo.why + '，exit=' + r.status + '）' +
    (r.status === 0 ? '' : '\n      ' + out.split('\n').filter(l => /FAIL|Error|错误/.test(l)).slice(0, 3).join('\n      ')));
  ok(/--- PASS: TestSignSelfConsistencyWithKnownKey/.test(out),
    'go test 输出里确有 --- PASS: TestSignSelfConsistencyWithKnownKey（防"没跑也 exit 0"的假绿）');

  // ---- 桥：JS 三路重放 vs **Go 真运行**打出来的串（逐字节）----
  //
  // 这是最强的一环：A/B/C 都是"读源码重放"，这里是"真执行"的观测值。
  // 日志标签从测试文件里抽（`t.Logf("Go 实现  : %s", auth)`），不把文案硬编码在这里；
  // 变量名 → 哪一路：`auth` 是 Sign() 的产出（对 A：sign.go 抽出的流水线），
  // `want` 是 independentSign() 的产出（对 C）。B（文档重写）在这条上应当**也**等于 auth。
  const gv = VECS.find(x => x.name === 'go-test 自造密钥向量');
  const jsA = runSrcPipeline(P, gv);
  const jsB = runDocImpl(gv, runSrcHeaders(P, gv), P.dateHeaderName);
  const jsC = runTestImpl(C, gv, P);
  const pick = label => {
    const line = out.split('\n').find(l => l.indexOf(label) >= 0);
    if (!line) return null;
    const m = /(SDK-HMAC-SHA256 .*)$/.exec(line.trim());
    return m ? m[1].trim() : null;
  };
  const seen = {};
  for (const [label, varName] of C.logf) {
    const got = pick(label);
    ok(got !== null, 'go -v 输出里找到日志行「' + label + '」（变量 ' + varName + '）');
    if (got === null) continue;
    seen[varName] = got;
    const mine = varName === 'auth' ? jsA : (varName === 'want' ? jsC : null);
    if (mine === null) { ok(false, '认不出的日志变量 ' + varName + '（只认 auth=实现 / want=独立实现）'); continue; }
    ok(authString(mine, varName) === got,
      'JS 重放的 ' + (varName === 'auth' ? 'sign.go（A）' : 'independentSign（C）') +
      ' 与 Go **真运行**逐字节一致（' + varName + ' = ' + got.slice(0, 60) + '…）');
  }
  if (seen.auth) {
    ok(jsB.auth === seen.auth,
      '纯 JS 的**文档独立重写**（B）复现了 Go 真运行的签名 —— 三条独立路径同一串');
  }
}

console.log('');
if (fail === 0) {
  console.log('=== 实现与官方文档逐条一致 ===');
  console.log('（含最关键的一条：签名是**单次** HMAC(SecretKey, stringToSign)，');
  console.log('  不是 AWS SigV4 的四步派生链 —— 本轮已用官方文档确认）');
  if (envNotes.length) {
    console.log('');
    console.log('⚠ ' + envNotes.length + ' 条 ENV（**不计入 FAIL，但也不冒充 PASS**：它说明有一条判据没被执行）：');
    envNotes.forEach(n => console.log('  · ' + n));
    console.log('  ⚠ 可见性：run_frontend_suites.js 对通过的探针只打印 `ok <file>`，会吞掉这里的 ENV 行 ——');
    console.log('    要看 ENV 请直接跑本文件（见文件头"可见性缺口"）。');
  }
} else {
  console.log('=== ' + fail + ' 项与文档不符 ===');
  if (envNotes.length) envNotes.forEach(n => console.log('  （另有 ENV：' + n + '）'));
}
process.exit(fail ? 1 : 0);
