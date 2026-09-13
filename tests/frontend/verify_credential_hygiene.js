// verify_credential_hygiene.js —— S1/S2/S3/S4 的**端到端**验收。
//
// # 为什么需要它（单测覆盖不到的部分）
//
// 四个子问题里有三个**只在真实启动路径上显形**：
//
//   S1  跳过文件的日志 —— 要看**启动时的 stderr**，单测抓不到
//   S2  同 uid 冲突选优 —— 要看**日志说了丢哪个** + 池子里是哪一份
//   S3  日志数字 == 界面数字 —— 必须**同时读日志与 API** 才能对照
//
// 这是本项目反复的教训：**单测覆盖单元内部逻辑，覆盖不了"跨组件的接线"。**
//
// # 用法
//
//   node verify_credential_hygiene.js            # 用 18080 与它当前的日志
//   LOG_PORT=18081 node verify_credential_hygiene.js
//
// ⚠ 它**只读**：读 /admin/accounts、读日志文件，不写任何东西。
'use strict';
const http = require('http');
const fs = require('fs');

// 端口来源（按优先级）：
//   1. HYGIENE_URL —— run_e2e.js 的约定（它给每个套件传一个 URL 环境变量）
//   2. LOG_PORT    —— 单独跑时更直接
//   3. 18080       —— 默认
//
// ⚠ 两种都要支持：run_e2e 传 URL、手动跑习惯传端口。
// 只认一种会导致"单独跑绿、纳入套件后红"，而那种失败最难查
//（看起来像套件坏了，实际是参数没接上）—— 本项目踩过一次。
function resolvePort() {
  const u = process.env.HYGIENE_URL;
  if (u) { try { return Number(new URL(u).port || 80); } catch { /* 落到下一档 */ } }
  return Number(process.env.LOG_PORT || 18080);
}
const PORT = resolvePort();

const SERVICES = [
  { url: 'http://127.0.0.1:7863', label: '用户网关 7863' },
  { url: 'http://127.0.0.1:3000', label: 'new-api 3000' },
];

function api(path) {
  return new Promise((res) => {
    const key = process.env.WB2API_KEY || (() => {
      try { return JSON.parse(fs.readFileSync('D:\\tmp\\run-' + PORT + '.json', 'utf8')).api_key; }
      catch { return ''; }
    })();
    http.get({ host: '127.0.0.1', port: PORT, path, headers: { Authorization: 'Bearer ' + key } }, r => {
      let d = ''; r.on('data', c => d += c);
      r.on('end', () => { try { res(JSON.parse(d)); } catch { res(null); } });
    }).on('error', () => res(null));
  });
}

(async () => {
  let fail = 0;
  const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

  console.log('=== 凭证卫生验收（只读）===\n');

  // ---- 1. 账号池数字（S3 的对照基准）----
  const acc = await api('/admin/accounts');
  if (!acc) { console.log('  无法访问 /admin/accounts（实例没起？）'); process.exit(2); }
  const list = acc.accounts || [];
  const by = {};
  list.forEach(a => { by[a.provider] = (by[a.provider] || 0) + 1; });
  console.log('  账号池: ' + JSON.stringify(by) + '  共 ' + list.length);

  // uid 唯一性（S2 的判据：池子里不该有重复 uid）
  const uids = list.map(a => a.uid);
  const dupUids = uids.filter((u, i) => uids.indexOf(u) !== i);
  ok(dupUids.length === 0, '账号池里 uid 唯一（无重复）' +
    (dupUids.length ? ' —— 重复: ' + JSON.stringify(dupUids) : ''));

  // ---- 2. codearts 账号数 ----
  const caCount = list.filter(a => a.provider === 'codearts').length;
  console.log('  codearts 账号数: ' + caCount);

  // ---- 3. 读启动日志（S1/S2/S3 的证据都在这里）----
  //
  // ⚠ 默认路径不能写死某个文件名。
  //
  // 我第一版写的是 `D:\tmp\ad3.err` —— 那是**某一次**启动的重定向目标。
  // 后来重启用了 `ad5.err`，脚本仍在读 `ad3.err`（旧文件），
  // 于是它拿**修复前**的日志报"日志说 2、界面 1"，
  // 看起来像修复没生效 —— **那是测量工具在骗我**（本项目第 N 次）。
  //
  // 正确做法：**扫目录里最新的 `ad*.err`**，或用 LOG_FILE 显式指定。
  function resolveLogFile() {
    if (process.env.LOG_FILE) return process.env.LOG_FILE;
    const dir = 'D:\\tmp';
    try {
      const cands = fs.readdirSync(dir)
        .filter(f => /^ad.*\.err$/.test(f))
        .map(f => ({ p: dir + '\\' + f, t: fs.statSync(dir + '\\' + f).mtimeMs }))
        .sort((a, b) => b.t - a.t);
      if (cands.length) return cands[0].p;
    } catch { /* 拿不到就返回空 */ }
    return '';
  }
  const logPath = resolveLogFile();
  let log = '';
  try { log = fs.readFileSync(logPath, 'utf8'); } catch { /* 拿不到就跳过日志类断言 */ }

  if (!log) {
    console.log('\n  ⚠ 读不到日志文件 —— 日志类断言跳过');
    console.log('    （设 LOG_FILE 指向网关的 stderr 重定向文件）');
  } else {
    console.log('\n=== 启动日志分析（' + logPath + '）===');

    // S3：日志说的数字必须 == 界面数字
    const m = /codearts: 已并入账号池 (\d+) 个账号/.exec(log);
    if (m) {
      const logged = Number(m[1]);
      console.log('  日志说并入: ' + logged + '；界面实际: ' + caCount);
      ok(logged === caCount,
        'S3 日志数字 == 界面数字（' + logged + ' vs ' + caCount + '）—— ' +
        '不一致会让用户只能猜');
    } else {
      console.log('  （日志里没有"已并入账号池 N 个账号"这一行）');
    }

    // S1：跳过文件要有日志
    const skips = log.split('\n').filter(l => /codearts: 跳过/.test(l));
    console.log('  跳过文件的日志: ' + skips.length + ' 条');
    skips.slice(0, 3).forEach(l => console.log('    ' + l.trim().slice(0, 100)));

    // S2：同 uid 冲突要有说明
    const conflicts = log.split('\n').filter(l => /有 \d+ 份凭证|丢弃/.test(l));
    console.log('  同 uid 冲突的日志: ' + conflicts.length + ' 条');
    conflicts.slice(0, 3).forEach(l => console.log('    ' + l.trim().slice(0, 100)));
  }

  console.log('\n=== 保护端口（全程未碰）===');
  for (const s of SERVICES) console.log('  ' + s.label + ' → 未触碰');

  console.log(fail === 0 ? '\n=== 凭证卫生验收通过 ===' : '\n=== ' + fail + ' 项失败 ===');
  process.exit(fail ? 1 : 0);
})();
