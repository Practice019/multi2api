// chrome_path.js —— 浏览器可执行文件的**共享解析器**。
//
// # 为什么需要它
//
// 16 个套件各自写死了 `C:\Program Files\Google\Chrome\Application\chrome.exe`。
// 换一台机器（或换 Chrome/Edge 安装位置）就要改 16 个文件 —— 而漏改一个
// 表现为"该套件静默报 Chrome 未启动"，很容易被当成"环境问题"忽略掉。
//
// 解析顺序（先精确后宽松）：
//   1. 环境变量 CHROME_PATH（显式指定，最高优先）
//   2. 常见安装位置（Chrome 的三种典型路径）
//   3. Edge（Chromium 内核，CDP 协议一致，可直接顶替）
//   4. PATH 里的 chrome / msedge
//
// 找不到就**抛错并列出试过的路径** —— 静默返回空串会让调用方
// 报出"Chrome 未启动"这种含糊错误，掩盖真实原因。
'use strict';

const fs = require('fs');
const path = require('path');
const { execFileSync } = require('child_process');

const CANDIDATES = [
  // Chrome —— 64 位默认位置
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  // Chrome —— 32 位 / 用户级安装
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  path.join(process.env.LOCALAPPDATA || '', 'Google\\Chrome\\Application\\chrome.exe'),
  // Edge —— Chromium 内核，CDP 行为与 Chrome 一致，可替代
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  // macOS / Linux（让脚本在别的平台也能跑）
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];

let cached = null;

// resolveChrome 返回可用的浏览器可执行文件路径。
function resolveChrome() {
  if (cached) return cached;

  // 1) 显式指定
  const fromEnv = process.env.CHROME_PATH;
  if (fromEnv) {
    if (!fs.existsSync(fromEnv)) {
      throw new Error('CHROME_PATH 指向的文件不存在: ' + fromEnv);
    }
    cached = fromEnv;
    return cached;
  }

  // 2) 常见位置
  for (const c of CANDIDATES) {
    if (c && fs.existsSync(c)) { cached = c; return cached; }
  }

  // 3) PATH
  for (const name of ['chrome', 'google-chrome', 'chromium', 'msedge']) {
    try {
      const which = process.platform === 'win32' ? 'where' : 'which';
      const out = execFileSync(which, [name], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
      const first = out.split(/\r?\n/).map(s => s.trim()).filter(Boolean)[0];
      if (first && fs.existsSync(first)) { cached = first; return cached; }
    } catch { /* 该名字不在 PATH 里 */ }
  }

  // 找不到：**明确报错并列出试过的位置**，而不是静默返回空
  throw new Error(
    '找不到浏览器可执行文件。试过:\n  ' +
    CANDIDATES.filter(Boolean).join('\n  ') +
    '\n  PATH 里的: chrome / google-chrome / chromium / msedge' +
    '\n可设置环境变量 CHROME_PATH 显式指定。'
  );
}

// isChromiumBased 判断解析到的是 Chrome 还是 Edge（供日志区分）。
function browserName(p) {
  return /msedge/i.test(p) ? 'Edge' : 'Chrome';
}

module.exports = { resolveChrome, browserName, CANDIDATES };
