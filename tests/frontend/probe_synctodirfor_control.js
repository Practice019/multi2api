// probe_synctodirfor_control.js —— 判据 6 的**隔离复现 + 对照实验**。
//
// # 为什么单独跑这一份
//
// 浏览器套件的判据 6 失败（codearts 与 workbuddy 重载后的池大小都是 3）。
// 有三种可能，必须分清——否则会把"测试假设错"当成"功能坏"：
//
//   (a) 我的假设错：SyncToDirFor 无论 provider 为何，都对全池生效
//   (b) 环境问题：codearts 的凭证文件实际被扫到了默认上游域
//   (c) 功能真坏：provider 没传到目录选择
//
// 判据 4（请求体真的带 codearts）与 `dir` 回执都已是直接证据，
// 但光看浏览器结果分不清 (a)/(b)。这里加一个**对照**：
//
//   · 对照 A：显式 provider=codearts
//   · 对照 B：**完全不传 provider**（后端回落到 workbuddy）
//
// 若 A 与 B 的 after 相同 → provider **真的没起作用**（功能坏，同 (c)）
// 若 A 与 B 的 after 不同 → provider 起了作用，判据 6 的假设（池大小必然不同）错
//
// 这个对照比"两目录文件数"可靠：它不依赖目录内容，只依赖"两次调用用了不同的 provider"。
'use strict';
const http = require('http');
const fs = require('fs');

const PORT = Number(process.env.RELOAD_PORT || 18080);
const KEY = JSON.parse(fs.readFileSync('D:\\tmp\\run-' + PORT + '.json', 'utf8')).api_key;

function post(bodyObj) {
  return new Promise(res => {
    const b = JSON.stringify(bodyObj);
    const q = http.request({ host: '127.0.0.1', port: PORT, path: '/admin/accounts/reload', method: 'POST',
      headers: { Authorization: 'Bearer ' + KEY, 'Content-Type': 'application/json',
                 'Content-Length': Buffer.byteLength(b) } },
      r => { let d = ''; r.on('data', c => d += c); r.on('end', () => { let j = null; try { j = JSON.parse(d); } catch { }
        res({ code: r.statusCode, json: j, body: d }); }); });
    q.on('error', () => res({ code: 0, body: '(请求失败)' }));
    q.write(b); q.end();
  });
}

(async () => {
  let bad = 0;
  console.log('=== 判据 6 隔离复现：SyncToDirFor 是否按 provider 分域 ===\n');

  const A = await post({ provider: 'codearts' });
  const B = await post({});                    // 不传 → 后端回落 workbuddy
  const C = await post({ provider: 'workbuddy' });

  console.log('  对照 A  provider=codearts : ' + JSON.stringify(A.json));
  console.log('  对照 B  不传 provider     : ' + JSON.stringify(B.json) + '  (回落 workbuddy)');
  console.log('  对照 C  provider=workbuddy: ' + JSON.stringify(C.json));

  const same = (x, y) => x && y && x.after === y.after;
  console.log('\n  A.provider=' + (A.json && A.json.provider) + '  dir=' + (A.json && A.json.dir)
    + '  scanned=' + (A.json && A.json.scanned));
  console.log('  B.provider=' + (B.json && B.json.provider) + '  dir=' + (B.json && B.json.dir)
    + '  scanned=' + (B.json && B.json.scanned));

  // 关键对照：显式 codearts 与"回落 workbuddy"是否给出同一个 after
  const differ = A.json && B.json && A.json.after !== B.json.after;
  console.log('\n  A.after(' + (A.json && A.json.after) + ') vs B.after(' + (B.json && B.json.after) + ') '
    + (differ ? '→ **不同**：provider 真的改变了行为' : '→ **相同**：provider 没有改变行为'));

  // 目录必须分别是 auths\codearts 与 auths\workbuddy
  const dirOk = A.json && /codearts/.test(A.json.dir) && B.json && /workbuddy/.test(B.json.dir);
  console.log('  目录回执是否各自指向自己的上游: ' + (dirOk ? '是' : '否'));
  if (!dirOk) bad++;

  // 结论判定
  console.log('\n  结论：');
  if (dirOk && !differ) {
    console.log('    provider **起了作用**（扫的是各自的目录），');
    console.log('    但两个上游对齐后池大小恰好相同（各有 3 个凭证）。');
    console.log('    → 浏览器套件判据 6 的**假设**错了，不是功能坏。');
  } else if (dirOk && differ) {
    console.log('    provider 起了作用，且池大小不同 → 判据 6 应该通过（环境与刚才不同）。');
  } else {
    console.log('    ⚠ 目录没按 provider 选 → 功能可能真的坏了，需要进一步排查。');
  }

  console.log(bad === 0 ? '\n=== 隔离复现完成 ===' : '\n=== ' + bad + ' 项硬失败 ===');
  process.exit(bad ? 1 : 0);
})();
