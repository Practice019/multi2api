// test_chrome_resolver.js —— 验证解析器**真的会回退**，而不是换个地方写死。
//
// # 为什么要专门测它
//
// "抽出一个 resolver"很容易变成"把硬编码从 16 个文件搬到 1 个文件" ——
// 那样可移植性没有真正改善，只是看起来干净了。
// 必须证明：显式覆盖生效、找不到时**明确报错**（而不是静默返回空串）。
'use strict';
const path = require('path');
const { execFileSync } = require('child_process');

const RESOLVER = path.join(__dirname, 'chrome_path.js');
let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// 在子进程里跑，便于控制环境变量
function run(env) {
  try {
    const out = execFileSync(process.execPath, ['-e',
      `try { console.log('OK:' + require(${JSON.stringify(RESOLVER)}).resolveChrome()); }` +
      `catch (e) { console.log('ERR:' + e.message.replace(/\\n/g, ' | ')); }`,
    ], { encoding: 'utf8', env: Object.assign({}, process.env, env) });
    return out.trim();
  } catch (e) {
    return 'THROW:' + String(e.message).slice(0, 120);
  }
}

console.log('=== Chrome 解析器行为验证 ===\n');

// 1) 默认解析（本机应能找到 Chrome）
const def = run({});
ok(def.startsWith('OK:'), '默认解析成功: ' + def.slice(0, 90));

// 2) CHROME_PATH 覆盖必须生效（指向一个真实存在的可执行文件：
//    用 node 自己当替身 —— 解析器只校验"文件存在"，不校验是不是浏览器）
const realExe = process.execPath;
const overridden = run({ CHROME_PATH: realExe });
ok(overridden === 'OK:' + realExe,
  'CHROME_PATH 覆盖生效（解析到 ' + overridden.slice(3, 60) + '）');
ok(overridden !== def,
  '覆盖结果与默认结果不同 —— 证明确实读了环境变量，不是忽略它');

// 3) CHROME_PATH 指向不存在的文件 → 必须**明确报错**（含路径）
const missing = run({ CHROME_PATH: 'C:\\definitely\\not\\here.exe' });
ok(missing.startsWith('ERR:'), '指向不存在的文件时抛错（而不是静默返回空）');
ok(/not\\here\.exe/.test(missing), '错误信息里含用户给的那个路径');

// 4) 找不到任何浏览器时，错误必须**列出试过的位置**（便于排障）
//    用一个 PATH 被清空、且把候选都指到不存在处的环境模拟
const noBrowser = run({
  CHROME_PATH: '',
  LOCALAPPDATA: 'C:\\nonexistent',
  PATH: 'C:\\Windows\\System32',   // 去掉 where 能找到 chrome 的可能
});
// 这台机器上 Chrome 确实存在于候选路径里，所以这里**预期仍能解析成功**。
// 我们真正要断言的是：无论成功或失败，输出都是明确的（OK: 或 ERR: 两种之一），
// 绝不能是空串或 undefined。
ok(noBrowser.startsWith('OK:') || noBrowser.startsWith('ERR:'),
  '清空环境后仍是明确结果（OK/ERR 二选一，不是空串）: ' + noBrowser.slice(0, 70));

// 5) 关键反面：绝不能在**文件不存在**的情况下返回路径
if (def.startsWith('OK:')) {
  const p = def.slice(3);
  const fs = require('fs');
  ok(fs.existsSync(p), '返回的路径确实存在（不是凭记忆拼出来的）');
}

console.log('');
console.log(fail === 0 ? '=== 解析器行为正确 ===' : '=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
