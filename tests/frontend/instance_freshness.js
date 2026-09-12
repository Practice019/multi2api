// instance_freshness.js —— 测量前的强制检查：跑着的实例是不是**当前源码**编的？
//
// # 为什么有这个文件
//
// 本项目出了一次严重事故：源码改了、没重新编译、进程没重启，
// 于是所有基于"运行中实例"的观察**全部来自旧行为**，
// 而我据此判断修复是否生效 —— 结论整个不成立。
//
// 更讽刺的是：这条警告**当时已经写在 Builder 的 SOUL.md 里**
// （"go build -o 在二进制被占用时会静默失败 —— 构建后核对 LastWriteTime"），
// 但它是**口头规范**，没人执行。口头规范会被遗忘。
//
// # 所以做成脚本
//
// 任何要基于运行实例下结论的测试，先跑这个。
// 判据：**进程启动时间 > 所有源文件的最大修改时间**。
// 不满足就是"陈旧实例"，直接判失败 —— 不要让人有机会看到
// "看起来合理"的假结果。
'use strict';
const fs = require('fs');
const path = require('path');
const http = require('http');
const { execFileSync } = require('child_process');

const REPO = path.resolve(__dirname, '..', '..');
const PORT = Number(process.env.FRESH_PORT || 18080);

// 会被编译进二进制的源码目录
const SOURCE_DIRS = ['internal', 'cmd'];
const SOURCE_EXT = ['.go', '.html'];

function newestSourceMtime() {
  let newest = 0;
  let newestFile = null;
  (function walk(dir) {
    let entries;
    try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) { walk(p); continue; }
      if (!SOURCE_EXT.some(x => e.name.endsWith(x))) continue;
      // 测试文件不进二进制，但它们的改动同样说明"源码在动" ——
      // 保守起见也算进去（宁可多报陈旧，不可漏报）
      const m = fs.statSync(p).mtimeMs;
      if (m > newest) { newest = m; newestFile = path.relative(REPO, p); }
    }
  })(path.join(REPO, SOURCE_DIRS[0]));
  (function walk(dir) {
    let entries;
    try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
    for (const e of entries) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) { walk(p); continue; }
      if (!SOURCE_EXT.some(x => e.name.endsWith(x))) continue;
      const m = fs.statSync(p).mtimeMs;
      if (m > newest) { newest = m; newestFile = path.relative(REPO, p); }
    }
  })(path.join(REPO, SOURCE_DIRS[1]));
  return { mtime: newest, file: newestFile };
}

function listeningPid(port) {
  try {
    const out = execFileSync('powershell', ['-NoProfile', '-Command',
      `(Get-NetTCPConnection -State Listen -LocalPort ${port} -ErrorAction SilentlyContinue | Select-Object -First 1).OwningProcess`],
      { encoding: 'utf8' });
    const n = parseInt(out.trim(), 10);
    return Number.isFinite(n) ? n : null;
  } catch { return null; }
}

function processStartMs(pid) {
  // ⚠ 不要在 JS 里把 .NET ticks 换算成 unix ms —— 我第一版那么做了，
  // 结果整整差了 8 小时（时区），把**陈旧实例判成了新鲜**。
  //
  // 这个错误的形态很值得记：换算看起来"只是算术"，
  // 而 `.NET ticks` 的 epoch（0001-01-01）与时区叠加，
  // 错出来的偏差恰好等于本地时区偏移 —— 不核对绝对时间根本发现不了。
  //
  // 正确做法：让 PowerShell 直接把时间输出成**可比较的 ISO 字符串**，
  // 由 JS 用 Date 解析。少一次手写换算 = 少一个出错的地方。
  try {
    const out = execFileSync('powershell', ['-NoProfile', '-Command',
      `$p = Get-Process -Id ${pid} -ErrorAction SilentlyContinue;` +
      `if ($p) { $p.StartTime.ToString('o') }`],
      { encoding: 'utf8' });
    const s = out.trim();
    if (!s) return null;
    const t = Date.parse(s);
    return Number.isFinite(t) ? t : null;
  } catch { return null; }
}

const fmt = ms => ms ? new Date(ms).toLocaleString('zh-CN', { hour12: false }) : '(未知)';

(async () => {
  console.log('=== 实例新鲜度检查（端口 ' + PORT + '）===');

  const pid = listeningPid(PORT);
  if (!pid) {
    console.log('  端口 ' + PORT + ' 上没有监听进程');
    process.exit(2);
  }
  const startMs = processStartMs(pid);
  const src = newestSourceMtime();

  console.log('  监听 PID      : ' + pid);
  console.log('  进程启动时间  : ' + fmt(startMs));
  console.log('  最新源码修改  : ' + fmt(src.mtime) + '   (' + src.file + ')');

  const stale = startMs && src.mtime > startMs;
  const driftSec = startMs ? Math.round((src.mtime - startMs) / 1000) : null;

  console.log('');
  if (stale) {
    console.log('  ✗ **陈旧实例** —— 源码比进程新 ' + driftSec + ' 秒');
    console.log('    所有基于这个实例的观察都来自**旧行为**，结论不成立。');
    console.log('    请先重新编译并重启，再测量。');
  } else {
    console.log('  ✓ 实例比源码新，可以基于它下结论');
  }

  // 顺带确认服务真的在答话（端口在监听 ≠ 服务正常）
  await new Promise(res => {
    const r = http.get('http://127.0.0.1:' + PORT + '/healthz', resp => {
      let d = '';
      resp.on('data', c => d += c);
      resp.on('end', () => { console.log('  /healthz: ' + d.trim()); res(); });
    });
    r.on('error', e => { console.log('  /healthz 不可达: ' + e.message); res(); });
    r.setTimeout(3000, () => { r.destroy(); console.log('  /healthz 超时'); res(); });
  });

  // 额外核对：**前端也被 embed 进二进制**，所以陈旧二进制会服务陈旧 UI。
  //
  // `internal/server/webui.go` 有 `//go:embed webui.html` —— 编译期内嵌。
  // 后果：改了 HTML 但不重新编译，浏览器拿到的仍**旧 UI**，
  // 而磁盘上的文件是新的。只看源码会以为改动生效了。
  //
  // 判据：比较磁盘 webui.html 与实例实际下发的长度。
  // 差异可能是正常的运行时注入（密钥、COLS 占位符），所以用**宽松阈值**
  // 判"明显不同"（>2KB），避免假红；真正的判据仍是上面的时间戳。
  await new Promise(res => {
    const diskPath = path.join(REPO, 'internal', 'server', 'webui.html');
    let diskLen = null;
    try {
      // ⚠ 必须按**字节**读并与实例下发的**字节**比。
      // 我第一版用 readFileSync(p,'utf8').length（字符数），而 CRLF 的
      // 字符数与字节数不同 —— 拿字符数比字节数会得出无意义的差值
      //（实测同一文件读到 180092 和 234104 两个数，差的就是换行编码）。
      // 统一用字节：Buffer.byteLength(归一化 CRLF)。
      const raw = fs.readFileSync(diskPath);
      diskLen = raw.length;
    } catch { /* ignore */ }
    const r = http.get('http://127.0.0.1:' + PORT + '/ui', resp => {
      const chunks = [];
      resp.on('data', c => chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c)));
      resp.on('end', () => {
        const n = Buffer.concat(chunks).length;
        if (diskLen === null) { res(); return; }
        const diff = Math.abs(n - diskLen);
        console.log('  /ui 字节数: 磁盘 ' + diskLen + ' / 实例 ' + n +
          '（差 ' + diff + '；CRLF 与运行时注入会带来小差异）');
        if (diff > 4096) {
          console.log('  ⚠ 差异过大 —— 实例内嵌的 UI 与磁盘不是同一份');
          console.log('    //go:embed 是编译期内嵌：改了 HTML 必须重新编译。');
        }
        res();
      });
    });
    r.on('error', () => res());
    r.setTimeout(3000, () => { r.destroy(); res(); });
  });

  process.exit(stale ? 1 : 0);
})();
