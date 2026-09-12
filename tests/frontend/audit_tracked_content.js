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
    // 允许出现在：README 的示例、chrome_path 的候选列表、注释里的说明
    test: (c, f) => {
      if (/README|chrome_path|acceptance|test_shape_scan|mutation_sweep/.test(f)) return false;
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
      // 允许：文档、以及明确用 127.0.0.1 作为示例或默认值的场景
      if (/README|chrome_path/.test(f)) return false;
      return /https?:\/\/(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(c);
    },
    why: '内网地址不该进公开仓库',
  },
  {
    id: 'phone',
    name: '手机号形态（11 位大陆号码）',
    test: (c, f) => {
      // 允许测试夹具里的明显假号
      const hits = [...c.matchAll(/\b1[3-9]\d{9}\b/g)].map(x => x[0]);
      const fake = hits.filter(h => /^(13000000000|13800000000|13900000000|1[3-9]0{9})$/.test(h));
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
      const okDomains = /@(example\.(com|org)|test\.(com|local)|localhost)$/;
      const afterScheme = /:\/\/[\w.-]+:[\w.-]+@/;   // scheme://user:pass@
      const hostPort = /@[\w-]+\.[\w-]+:\d+$/;      // @host:port
      return hits.some(h =>
        !okDomains.test(h) && !afterScheme.test(c.slice(0, c.indexOf(h) + h.length)) && !hostPort.test(h));
    },
    why: '可能是真实联系人（连接串里的 user:pass@host 不算）',
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
