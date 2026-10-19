// audit_tracked_content.js —— 审计**被跟踪文件**里不该出现的内容。
//
// # 为什么需要
//
// 上一轮抓到"测试套件里带真实 api_key"。那说明我只在**自己想到的地方**
// 做检查。这类问题应该是**系统性扫描**，而不是每次碰巧发现。
//
// 本脚本按类别扫描全部被跟踪文件：
//   1. 密钥/token 形态的长字面量（复用 test_shape_scan 的判据）
//   2. 机器专属绝对路径（换机器即失效）
//   3. 内网/本机地址（不该进公开仓库）
//   4. 个人信息形态（手机号、邮箱）
//   5. 账号标识（UID / 昵称）
//
// # 判据要能区分"真问题"与"合理出现"
//
// 例如绝对路径：文档里举例说明是可以的，代码里硬编码就不行。
// 所以每类都给"允许的例外"并显式列出，而不是简单 grep 数量。
//
// ⚠⚠ 2026-10 实测：本审计一次报出 20 个文件，**里面混着两类完全不同的东西** ——
//   "规则过敏"与"真该改的内容"。混在一起的后果是没人看这份输出（20 个命中和 3 个命中
//   读起来一样急）。所以下面这一版把**规则过敏**的部分收窄、并逐条写下理由；
//   **真问题一条都不进白名单**（宁可继续红，也别把真实账号标识藏起来）：
//
//   · 机器路径：文档/报告里举例 = 合理（与既有的 README 例外同理）；
//     `tools/inject_reasoning_efforts.py` 里 `PATH = r"C:\Users\21877\.dsh\..."` =
//     **不合理**（换机器/换用户即失效，且带真实用户名）⇒ 故意留着红。
//   · 内网地址：`cmd/server/startup_addr*` 是**地址解析**功能本身（注释讲绑定语义、
//     用例要断言"显式绑内网 IP，原样"）—— 这个功能不吃内网地址就没得测 ⇒ 合理。
//     其余按 abspath 的口径：**注释里的举例不算**（注释不会让机器换不了环境）。
//   · 手机号：只有"公认演示号/补零号"算合理（`13800138000` 是中国移动的公开演示号，
//     产品里把它当 input placeholder）。`internal/workbuddy/growth.go` 注释里的
//     「实测（2026-10，账号 1700****808）」、`internal/loomy/credential.go` 的
//     「（1984***053，手机号登录的必然带）」、`internal/server/webui.html` 打码示例里的
//     `1385***740` 都是**看着像真实账号标识**的原文 ⇒ 故意留着红，等 owner 改成掩码形态。
//   · 邮箱：`a@b.c` / `e@x.com` 这种**本地部分只有一个字符**的地址是解析用例的合成样本
//     （真人联系人不会只有一个字符的本地部分）⇒ 合理。
//
// ⚠⚠ 两个**改这份审计时必踩的坑**（我一次都踩了，写下来省下一个人的时间）：
//   1. 注释里也不能写号码原文 —— 本审计会扫**自己**（上面那三个号我第一版写了全文，
//      结果 `命中 13 个文件` 里多出 `tests/frontend/audit_tracked_content.js` 自己）。
//      同一个坑在 `test_shape_scan.js` 里出现过：那里写着"我第一版这里写的是网关的真实
//      api_key —— 那让这个'密钥扫描器'自身成了泄漏源"。**改扫描器时先扫自己。**
//   2. 别在收窄规则时把原有的例外一起删掉：我第一版把 `13000000000|13800000000|
//      13900000000` 三个全零假号从清单里漏掉了，`internal/upstream/health_test.go` 立刻
//      冒出来 —— 而它本来就是被允许的。**改白名单要"加"，不是"重写"**（diff 里那条新命中
//      就是唯一的报警信号，很容易被当成"修好了"）。
const ALLOW_ABSPATH_DOC = /^(docs\/|tests\/frontend\/README)/;      // 文档/报告里的路径举例
const ALLOW_LOCALADDR = /^cmd\/server\/startup_addr/;               // 地址解析功能本体（注释+用例）
// 全零/补零假号（原有的三个**必须留着**）+ 公开演示号 13800138000
const DEMO_PHONE = /^(13000000000|13800000000|13900000000|13800138000|1[3-9]0{9})$/;
const SYNTH_EMAIL_LOCAL1 = /^[\w.+-]@/;                             // 本地部分仅 1 字符
// ⚠ 单字符本地部分**单独**不足以判"合成样本"：真实联系人里也可能有 `a@qq.com` 这种
//   （短账号）。所以这条例外必须**再加一道**：只在**测试文件**里生效 ——
//   两个真实命中（`a@b.c` ×2、`e@x.com`）全在 `*_test.go` 里，是解析用例的样本。
const TEST_FILE = /(_test\.go$|_test\.js$|(^|\/)tests?\/)/;

// 时序数字不是手机号：`{"ts":17600006004}` 这种 11 位 epoch 毫秒会被手机号正则命中
//（实测 `internal/wire/softrate_test.go:73`）。
//
// ⚠ 判据落在**键名**上（ts/timestamp/time/ms/at/nonce…），**不能**落在"以 176/17x 开头"上：
//   同一个仓库里 `1700****808` 是看着像真实账号的那种命中、`17600006004` 是时间戳，
//   两者前三位撞车 —— 用号段排除等于把真命中一起放过。
//   键名判据不会伤到任何一条真命中：它们全在注释与 `"phone":"….…"` 里。
//
// ⚠⚠ 自证：我写下上面那条"注释里也不能写号码原文"的警告之后，**在说明这句话时又写了两次**
//    （`1700****808` 的全文 —— 这里刻意用掩码写法），于是本审计连续两次扫到自己
//    （`tests/frontend/audit_tracked_content.js` 出现在命中清单里）。这不是笔误，
//    是这种"扫描器扫自己"的坑**天然容易复发**：连"描述这个坑"的句子本身都会触发它。
//
// ⚠⚠ 还有一个更阴的坑：**不能靠 `grep 自己的文件名` 来自检**。
//    本审计打印命中清单时**只显示前 8 条**（后面是 `…还有 N 个`），
//    我第一次自检时自己就藏在被省略的那几条里 ⇒ 自检报"干净"，实际在命中。
//    可靠的自检只有两条：①看每栏的**计数**；②自己按 `git ls-files` + 同一套正则数一遍。
//    改这个文件时把这两条当收尾动作，别靠记性和 grep。
const TIME_KEY = /"(ts|timestamp|time|ms|at|nonce|started_at|created_at)"\s*:\s*$/;
const isTimeLiteral = (content, hit) => {
  const at = content.indexOf(hit);
  if (at < 0) return false;
  return TIME_KEY.test(content.slice(Math.max(0, at - 40), at));
};

// 连接串里的凭据（`scheme://user:pass@host` 命中的其实是密码）不是邮箱。
// 判据落在**命中位置之前**的文本上：那里必须出现过 `scheme://user:pass@`。
// 抽成函数是因为原来的写法在 test() 里两次 indexOf/slice，读起来像"随手糊的例外"，
// 而它其实是一条有名字的规则（连接串凭据）。
function afterHostPortLike(hit, content) {
  const at = content.indexOf(hit);
  const head = at < 0 ? content : content.slice(0, at + hit.length);
  return /:\/\/[\w.-]+:[\w.-]+@/.test(head) || /@[\w-]+\.[\w-]+:\d+$/.test(hit);
}
'use strict';
const fs = require('fs');
const path = require('path');
const os = require('os');
const { execFileSync } = require('child_process');

const REPO = path.resolve(__dirname, '..', '..');

function tracked() {
  return execFileSync('git', ['ls-files'], { cwd: REPO, encoding: 'utf8' })
    .split('\n').filter(Boolean);
}
function read(f) {
  try { return fs.readFileSync(path.join(REPO, f), 'utf8'); } catch { return null; }
}

// ---- 判据 ----
const looksLikeSecret = s => {
  const m = /"([A-Za-z0-9]{32,})"/.exec(s);
  if (!m) return false;
  if (/__WB2API_KEY__|__COLS_/.test(s)) return false;
  const v = m[1];
  return /[A-Z]/.test(v) && /[a-z]/.test(v) && /[0-9]/.test(v);
};

const CHECKS = [
  {
    id: 'secret',
    name: '密钥形态的长字面量',
    test: (c) => looksLikeSecret(c),
    why: '真实凭证不得进版本库',
  },
  {
    id: 'abspath',
    name: '机器专属绝对路径',
    // 允许出现在：README 的示例、chrome_path 的候选列表、注释里的说明、文档/报告
    test: (c, f) => {
      if (/README|chrome_path|acceptance|test_shape_scan|mutation_sweep/.test(f)) return false;
      if (ALLOW_ABSPATH_DOC.test(f)) return false;
      // 只看"代码里的字符串字面量"，注释里的举例不算
      const code = c.split('\n').filter(l => !/^\s*(\/\/|#|\*)/.test(l)).join('\n');
      return /['"][A-Za-z]:[\\\/](Users|project_GIT|tmp|software)/.test(code);
    },
    why: '换机器/换用户即失效；应改为相对路径或环境变量',
  },
  {
    id: 'localaddr',
    name: '内网 / 本机地址',
    test: (c, f) => {
      // 允许：文档、明确用 127.0.0.1 作为示例或默认值的场景、以及地址解析功能本体
      if (/README|chrome_path/.test(f)) return false;
      if (ALLOW_LOCALADDR.test(f)) return false;
      // 与 abspath 同一口径：注释里的举例不算（注释不会让这个仓库换不了环境）
      const code = c.split('\n').filter(l => !/^\s*(\/\/|#|\*)/.test(l)).join('\n');
      return /https?:\/\/(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(code);
    },
    why: '内网地址不该进公开仓库',
  },
  {
    id: 'phone',
    name: '手机号形态（11 位大陆号码）',
    test: (c, f) => {
      // 允许：公开演示号与补零号（`13800138000` 被产品当 input placeholder 用），
      // 以及 `{"ts":17600006004}` 这种时序数字（键名判据，见 TIME_KEY 的注释）。
      // ⚠ 其余一律照报 —— 注释里"实测账号 1700****808"那种就是这条判据要抓的东西。
      const hits = [...c.matchAll(/\b1[3-9]\d{9}\b/g)].map(x => x[0]);
      const fake = hits.filter(h => DEMO_PHONE.test(h) || isTimeLiteral(c, h));
      return hits.length > fake.length;
    },
    why: '可能是真实账号标识',
  },
  {
    id: 'email',
    name: '邮箱地址',
    test: (c, f) => {
      if (/README|\.md$/.test(f)) return false;
      const hits = [...c.matchAll(/[\w.+-]+@[\w-]+\.[\w.]+/g)].map(x => x[0]);
      // 允许的形态：
      //   - 示例域（example.com / test.com / localhost）
      //   - **连接串里的凭据**：`scheme://user:pass@host` 或 `user:pass@host:port`
      //     被匹配到的其实是密码，不是邮箱。
      //     例：`rediss://default:tok@foo.upstash.io:6379` → 命中 `tok@foo.upstash.io`
      //     第一版没排除它，误报了一个文件。
      //   - **本地部分只有 1 个字符**（`a@b.c` / `e@x.com`）：解析用例的合成样本
      const okDomains = /@(example\.(com|org)|test\.(com|local)|localhost)$/;
      return hits.some(h =>
        !okDomains.test(h) && !(TEST_FILE.test(f) && SYNTH_EMAIL_LOCAL1.test(h)) && !afterHostPortLike(h, c));
    },
    why: '可能是真实联系人（连接串里的 user:pass@host 不算，单字符本地部分算合成样本）',
  },
];

(async () => {
  const files = tracked();
  console.log('=== 被跟踪内容审计 ===');
  console.log('文件数: ' + files.length + '\n');

  const findings = [];
  for (const chk of CHECKS) {
    const hits = [];
    for (const f of files) {
      const c = read(f);
      if (c === null) continue;
      try {
        if (chk.test(c, f)) hits.push(f);
      } catch (e) { /* 该文件不适用 */ }
    }
    console.log('[' + chk.id + '] ' + chk.name);
    console.log('     命中 ' + hits.length + ' 个文件' + (hits.length ? ':' : ''));
    hits.slice(0, 8).forEach(h => console.log('       ' + h));
    if (hits.length > 8) console.log('       …还有 ' + (hits.length - 8) + ' 个');
    console.log('     判据理由: ' + chk.why);
    console.log('');
    if (hits.length) findings.push({ check: chk.id, files: hits });
  }

  console.log('=== 汇总 ===');
  if (findings.length === 0) {
    console.log('  全部 ' + CHECKS.length + ' 类检查通过，0 命中');
  } else {
    findings.forEach(f => console.log('  ⚠ [' + f.check + '] ' + f.files.length + ' 个文件'));
  }

  // 额外：统计仓库里"机器专属路径"的整体分布，便于人工判断
  console.log('\n=== 参考：绝对路径在各文件的出现次数（前 10）===');
  const counts = [];
  for (const f of files) {
    const c = read(f);
    if (!c) continue;
    const n = (c.match(/[A-Za-z]:[\\\/](Users|project_GIT|tmp|software)/g) || []).length;
    if (n) counts.push({ f, n });
  }
  counts.sort((a, b) => b.n - a.n).slice(0, 10).forEach(x => console.log('  ' + String(x.n).padStart(4) + '  ' + x.f));
  if (!counts.length) console.log('  （无）');

  process.exit(findings.length ? 1 : 0);
})();
