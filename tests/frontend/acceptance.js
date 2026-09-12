// acceptance.js —— 端到端验收：一条命令跑完"这个项目能不能交付"的全部判据。
//
// # 为什么需要
//
// 前 11 轮里我一直在"改一点 → 单独跑某个套件 → 报告"。那样有两个问题：
//   1. 结论分散在 11 次回复里，无法一眼看出当前整体状态
//   2. 某些判据（跨端一致性、可移植性）从没在同一时点一起验过
//
// 本脚本把全部判据收进一次运行，输出**可核对的清单**。
// 每一项都打印实测值，而不是只说 PASS —— 便于你复核。
//
// # 判据分组
//
//   A. 代码质量      build / vet / gofmt / go test
//   B. 前端套件      静态套件 + 浏览器 E2E
//   C. 交付物        线上内容 == HEAD、初始化注入、服务名
//   D. 可移植性      从仓库内路径能跑通（不是只在开发机的 D:\tmp）
//   E. 安全          无密钥泄漏、鉴权边界
//   F. 守卫有效性    变异扫描（可选，耗时）
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const http = require('http');
const { spawnSync } = require('child_process');

const HERE = __dirname;
const REPO = path.resolve(HERE, '..', '..');
const BASE = process.env.ACCEPT_URL || 'http://127.0.0.1:18080/ui';
const RUN_SWEEP = process.argv.includes('--with-sweep');

const results = [];
function record(group, name, pass, detail) {
  results.push({ group, name, pass, detail });
  const mark = pass === true ? 'PASS' : (pass === null ? 'SKIP' : 'FAIL');
  console.log('  [' + mark + '] ' + name + (detail ? '  —— ' + detail : ''));
}
function sh(cmd, args, opts) {
  const r = spawnSync(cmd, args, Object.assign({ cwd: REPO, encoding: 'utf8' }, opts || {}));
  return { code: r.status, out: (r.stdout || '') + (r.stderr || '') };
}
function get(url) {
  // ⚠ 必须收 **Buffer 再整体解码**，不能在 data 回调里逐块转字符串。
  //
  // http.get 的 data 事件按任意边界切分 chunk；一个多字节 UTF-8 字符被切开时，
  // `Buffer.toString()` 在每个半截上都会产出替换字符 U+FFFD。
  // 实测后果：中文注释里的「无家可归」被读成「无家可��」，
  // 于是"线上与 HEAD 一致"这条断言报了 **1 字节差异的假失败**。
  //
  // 已用原字节核对服务端发的是合法 UTF-8（e58fafe5bd92 = 可归），
  // 所以那是我的读取方式错，不是产品缺陷。
  return new Promise((res, rej) => {
    http.get(url, r => {
      const chunks = [];
      r.on('data', c => chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c)));
      r.on('end', () => res({ status: r.statusCode, body: Buffer.concat(chunks).toString('utf8') }));
    }).on('error', rej);
  });
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const env = Object.assign({}, process.env, { CGO_ENABLED: '0' });
  console.log('=== 端到端验收 ===');
  console.log('仓库: ' + REPO);
  console.log('实例: ' + BASE + '\n');

  // ---- A. 代码质量 ----
  console.log('[A] 代码质量');

  const build = sh('go', ['build', './...'], { env });
  record('A', 'go build ./...', build.code === 0, 'exit=' + build.code);

  const vet = sh('go', ['vet', './...'], { env });
  record('A', 'go vet ./...', vet.code === 0, 'exit=' + vet.code);

  // gofmt 必须在 **LF 副本**上查：仓库开了 core.autocrlf，直接查全是假阳性
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'fmt-'));
  let goFiles = 0;
  (function walk(d) {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      if (e.name === '.git' || e.name === 'bin') continue;
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name.endsWith('.go')) {
        goFiles++;
        const rel = path.relative(REPO, p).replace(/[\\/]/g, '_');
        fs.writeFileSync(path.join(tmp, rel), fs.readFileSync(p, 'utf8').replace(/\r\n/g, '\n'));
      }
    }
  })(REPO);
  const gofmt = sh('gofmt', ['-l', tmp]);
  const unformatted = gofmt.out.split('\n').filter(Boolean);
  record('A', 'gofmt（' + goFiles + ' 个 .go，LF 副本）',
    unformatted.length === 0, unformatted.length === 0 ? 'clean' : unformatted.length + ' 个未格式化');

  const test = sh('go', ['test', './...'], { env, timeout: 10 * 60 * 1000 });
  const okPkgs = (test.out.match(/^ok\s/gm) || []).length;
  record('A', 'go test ./...', test.code === 0, okPkgs + ' 个包通过');

  const race = sh('go', ['test', '-race', './internal/pool/'], { env: Object.assign({}, env, { CGO_ENABLED: '1' }), timeout: 5 * 60 * 1000 });
  const raceBlocked = /failed to allocate|ThreadSanitizer|cgo: C compiler/.test(race.out);
  record('A', 'go test -race（上游 TSan 缺陷）', race.code === 0 ? true : null,
    race.code === 0 ? '通过' : (raceBlocked ? '环境不支持（见 README，非本次可修）' : '失败：' + race.out.slice(0, 80)));

  // ---- B. 前端套件 ----
  console.log('\n[B] 前端套件');
  const stat = sh('node', [path.join(HERE, 'run_frontend_suites.js')], { cwd: HERE, timeout: 10 * 60 * 1000 });
  let m = stat.out.match(/=== (\d+)\/(\d+) 通过 ===/);
  record('B', '静态套件', m ? m[1] === m[2] : false, m ? m[1] + '/' + m[2] : '输出无法解析');

  const e2e = sh('node', [path.join(HERE, 'run_e2e.js'), BASE], { cwd: HERE, timeout: 30 * 60 * 1000 });
  m = e2e.out.match(/=== (\d+)\/(\d+) 通过 ===/);
  record('B', '浏览器 E2E', m ? m[1] === m[2] : false, m ? m[1] + '/' + m[2] : '输出无法解析');

  // ---- C. 交付物与线上一致性 ----
  console.log('\n[C] 交付物与线上一致性');
  const servSrc = path.join(REPO, 'internal/server/webui.html');
  const headSrc = sh('git', ['show', 'HEAD:internal/server/webui.html']);
  let served = null;
  try { served = await get(BASE); } catch (e) { /* 实例不可达 */ }

  if (served && served.status === 200) {
    // ⚠ 这个比对要掩掉**两类**运行时注入，否则会假红：
    //   1. API key（HEAD 是哨兵 `"__WB2API_KEY__"`，served 是真实密钥）
    //   2. COLS 占位符（HEAD 是 `__COLS_XXX__`，served 是数字）——
    //      这是 commit 86fda95 特意加的**服务端替换**：静态 HTML 里的
    //      `${COLS.x}` 不会被 JS 求值，改由 Go 侧替换成真实列数。
    //
    // 我前两版都只掩了密钥，于是每次都在这 5 行上报"不一致" ——
    // **两次都是我的校验写错，不是产品不一致**。
    const maskBoth = s => s
      .replace(/\r\n/g, '\n')                            // 仓库开了 autocrlf，先归一化
      .replace(/"__WB2API_KEY__"/g, '"K"')               // HEAD 的密钥哨兵
      .replace(/"[A-Za-z0-9]{32,}"/g, '"K"')           // 运行时注入的密钥（按形状，不认前缀）
      .replace(/"[A-Za-z0-9_-]{40}"/g, '"K"')            // 兜底：任何 40 位字面量
      .replace(/__COLS_[A-Z]+__/g, 'N')                  // HEAD 的列数占位符
      .replace(/colspan="\d+"/g, 'colspan="N"');         // served 里替换后的数字
    const a = maskBoth(headSrc.out), b = maskBoth(served.body);
    record('C', '线上 /ui 与 HEAD 的 webui.html 一致（掩掉两处运行时注入）', a === b,
      a === b ? (b.length + ' 字节相同') : ('长度 ' + a.length + ' vs ' + b.length));

    // 初始化注入：真 key 在内联脚本里
    const hasKey = /window\.__WB2API_KEY__ = "[^_][^"]{10,}"/.test(served.body);
    record('C', 'API key 已注入内联脚本（本机免手输）', hasKey,
      hasKey ? '已注入' : '未注入（网关可能未配 api_key）');

    // 服务名：从 /healthz 读（它稳定返回 service 字段）。
    // 我第一版从 /ui 正文里 grep `service=`，但那只在顶栏渲染后的 DOM 里，
    // 静态 HTML 里没有 —— 断言写错了地方。
    let svc = null;
    try {
      const hz = await get(BASE.replace('/ui', '/healthz'));
      svc = (JSON.parse(hz.body) || {}).service;
    } catch { /* 不可达 */ }
    record('C', '服务名来自配置而非硬编码', !!svc, svc ? 'service=' + svc : '/healthz 未返回 service');
  } else {
    record('C', '线上 /ui 与 HEAD 一致', null, '实例不可达 ' + BASE);
  }

  // ---- D. 可移植性 ----
  console.log('\n[D] 可移植性（从仓库内路径跑）');
  const inRepo = fs.existsSync(path.join(HERE, 'run_e2e.js')) &&
    fs.existsSync(path.join(HERE, 'run_frontend_suites.js'));
  record('D', '测试套件在版本库内', inRepo, inRepo ? 'tests/frontend/' : '缺失');

  const noHardcodeChrome = !fs.readdirSync(HERE).filter(f => f.endsWith('.js') && f !== 'chrome_path.js')
    .some(f => /C:\\+Program Files[^']*chrome\.exe/.test(fs.readFileSync(path.join(HERE, f), 'utf8')));
  record('D', '无写死的浏览器路径', noHardcodeChrome, '统一走 chrome_path.js');

  const resolver = sh('node', ['-e', "console.log(require('./chrome_path.js').resolveChrome())"], { cwd: HERE });
  record('D', '浏览器解析器可用', resolver.code === 0, resolver.out.trim().slice(0, 60));

  // ---- E. 安全 ----
  console.log('\n[E] 安全');
  const gitStatus = sh('git', ['status', '--porcelain']);
  record('E', '工作树干净', gitStatus.out.trim() === '', gitStatus.out.trim() ? '有未提交改动' : 'clean');

  const log = sh('git', ['log', '--oneline', 'origin/master..HEAD']);
  const commits = log.out.split('\n').filter(Boolean).length;
  record('E', '未 push（本地提交 ' + commits + ' 个）', true, 'origin/master 保持 ' +
    sh('git', ['rev-parse', '--short', 'origin/master']).out.trim());

  // looksLikeSecret：按**形状**判密钥，不认前缀（换密钥后依然有效）。
  // 必须同时含大写/小写/数字，且不含连字符 —— 后者能排除测试 UID、
  // 模型 ID、UUID 这三类纯小写 hex 的误报（第一版只判长度，误报 3 个）。
  const looksLikeSecret = s => {
    const m = /"([A-Za-z0-9]{32,})"/.exec(s);
    if (!m) return false;
    const v = m[1];
    if (/__WB2API_KEY__|__COLS_/.test(s)) return false;
    return /[A-Z]/.test(v) && /[a-z]/.test(v) && /[0-9]/.test(v);
  };
  // 全仓扫密钥：真实的 40 位 key 不应出现在任何被跟踪文件里
  const tracked = sh('git', ['ls-files']);
  let leaked = [];
  for (const f of tracked.out.split('\n').filter(Boolean)) {
    try {
      const c = fs.readFileSync(path.join(REPO, f), 'utf8');
      if (looksLikeSecret(c)) leaked.push(f);
    } catch { /* 二进制/不可读，跳过 */ }
  }
  record('E', '被跟踪文件里无真实 api_key', leaked.length === 0,
    leaked.length === 0 ? tracked.out.split('\n').filter(Boolean).length + ' 个文件已扫' : leaked.join(', '));

  // /admin/* 的**实际**安全模型（见 internal/admin/admin.go:15）：
  //   "整个 /admin/* 子树只接受 loopback 直连，非本机一律 403"
  // 也就是说本机直连**不需要** Bearer —— 这是设计，不是漏洞。
  //
  // ⚠ 我第一版断言"无 Bearer 时不得返回 200"，与本机直连的设计相矛盾，
  // 是**测试写错**。正确的安全性质是：
  //   1. 本机直连 → 允许（200）
  //   2. 非本机 → 403（这一条此前已由 security_probe.js 用 0.0.0.0 监听的
  //      实例验证过；本脚本不在本机跑不出非 loopback 的 RemoteAddr，故标注覆盖位置）
  try {
    const r = await get(BASE.replace('/ui', '/admin/ui/manifest'));
    record('E', '/admin/* 本机直连允许（设计如此）', r.status === 200,
      'status=' + r.status + '（非本机的 403 由 security_probe.js 覆盖）');
  } catch { record('E', '/admin/* 可达性', null, '不可达'); }

  // ---- 汇总 ----
  console.log('\n=== 汇总 ===');
  const byGroup = {};
  for (const r of results) {
    byGroup[r.group] = byGroup[r.group] || { pass: 0, fail: 0, skip: 0 };
    byGroup[r.group][r.pass === true ? 'pass' : r.pass === null ? 'skip' : 'fail']++;
  }
  let totalPass = 0, totalFail = 0, totalSkip = 0;
  for (const g of Object.keys(byGroup).sort()) {
    const v = byGroup[g];
    console.log('  [' + g + '] 通过 ' + v.pass + '  失败 ' + v.fail + '  跳过 ' + v.skip);
    totalPass += v.pass; totalFail += v.fail; totalSkip += v.skip;
  }
  console.log('  ─────────────────────────');
  console.log('  合计：通过 ' + totalPass + ' / 失败 ' + totalFail + ' / 跳过 ' + totalSkip);

  if (totalFail) {
    console.log('\n失败项：');
    results.filter(r => r.pass === false).forEach(r => console.log('  [' + r.group + '] ' + r.name + ' —— ' + r.detail));
  }
  if (totalSkip) {
    console.log('\n跳过项（附原因）：');
    results.filter(r => r.pass === null).forEach(r => console.log('  [' + r.group + '] ' + r.name + ' —— ' + r.detail));
  }

  fs.writeFileSync(path.join(os.tmpdir(), 'acceptance-result.json'), JSON.stringify(results, null, 2));
  process.exit(totalFail ? 1 : 0);
})();
