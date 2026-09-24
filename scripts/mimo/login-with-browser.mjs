#!/usr/bin/env node
// scripts/mimo/login-with-browser.mjs — 网关自启受控浏览器，监控登录，收割小米账号 Cookie。
//
// 流程：
//   1. 找空闲端口，生成临时 user-data-dir
//   2. 启动 Edge/Chrome **独立实例**（headful 窗口自动弹出，不污染用户日常浏览器）
//   3. CDP 导航到 https://account.xiaomi.com（小米账号登录页）
//   4. 轮询 Network.getAllCookies：等到出现 --uid 对应的 passToken/cUserId/userId
//   5. 输出 {"passToken","cUserId","userId","deviceId"}，关闭浏览器，退出 0
//   6. 超时（--timeout 秒，默认 180）或用户关闭窗口 → 退出 1
//
// 零依赖：Node ≥22（内置 WebSocket/fetch）。用法：
//   node login-with-browser.mjs --uid=3260552493
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, accessSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { tmpdir } from 'node:os';
import path from 'node:path';
import net from 'node:net';

const EDGE = [
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
];
const CHROME = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
];

const args = process.argv.slice(2);
const uid = (args.find(a => a.startsWith('--uid=')) || '').split('=')[1] || '';
const timeoutSec = Number((args.find(a => a.startsWith('--timeout=')) || '--timeout=180').split('=')[1]) || 180;

function freePort() {
  return new Promise((resolve) => {
    const srv = net.createServer();
    srv.listen(0, '127.0.0.1', () => {
      const p = srv.address().port;
      srv.close(() => resolve(p));
    });
  });
}

function findBrowser() {
  for (const p of [...EDGE, ...CHROME]) {
    try { accessSync(p); return { exe: p, kind: p.includes('Edge') ? 'edge' : 'chrome' }; } catch {}
  }
  return null;
}

async function cdpCall(ws, id, method, params = {}) {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(`cdp ${method} timeout`)), 10000);
    const handler = (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id === id) {
        clearTimeout(t);
        ws.removeEventListener('message', handler);
        if (m.error) reject(new Error(JSON.stringify(m.error)));
        else resolve(m.result);
      }
    };
    ws.addEventListener('message', handler);
    ws.send(JSON.stringify({ id, method, params }));
  });
}

function connect(wsUrl) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(wsUrl);
    ws.addEventListener('open', () => resolve(ws), { once: true });
    ws.addEventListener('error', (e) => reject(new Error('ws connect failed')), { once: true });
  });
}

async function main() {
  const browser = findBrowser();
  if (!browser) { console.error('no edge/chrome found'); process.exit(1); }
  const port = await freePort();
  const profile = mkdtempSync(path.join(tmpdir(), 'mimo-login-'));
  const proc = spawn(browser.exe, [
    `--user-data-dir=${profile}`,
    `--remote-debugging-port=${port}`,
    '--no-first-run', '--no-default-browser-check',
    '--disable-features=msEdgeSidebarV2,msEdgeShoppingAssistant',
    'about:blank',
  ], { stdio: 'ignore', detached: false });

  const kill = () => {
    try { proc.kill('SIGKILL'); } catch {}
    setTimeout(() => { try { rmSync(profile, { recursive: true, force: true }); } catch {} }, 500);
  };

  // 等调试端口就绪
  let targets = null;
  const deadline = Date.now() + timeoutSec * 1000;
  for (let i = 0; i < 60; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/json/list`);
      targets = await r.json();
      break;
    } catch { await new Promise(r => setTimeout(r, 300)); }
  }
  if (!targets || !targets.length) {
    console.error('cdp target not ready'); kill(); process.exit(1);
  }
  const page = targets.find(t => t.type === 'page') || targets[0];
  const ws = await connect(page.webSocketDebuggerUrl);
  await cdpCall(ws, 1, 'Page.enable');
  await cdpCall(ws, 2, 'Network.enable');
  await cdpCall(ws, 3, 'Page.navigate', { url: 'https://account.xiaomi.com' });
  console.log('[browser] opened https://account.xiaomi.com — 请在弹出窗口登录该小米账号');

  let cookieSend = 0;
  while (Date.now() < deadline) {
    await new Promise(r => setTimeout(r, 2000));
    try {
      const { cookies } = await cdpCall(ws, 100 + (++cookieSend), 'Network.getAllCookies');
      const jar = {};
      for (const c of cookies) {
        const n = String(c.name || '').toLowerCase();
        if ((n === 'passtoken' || n === 'cuserid' || n === 'userid' || n === 'deviceid')
            && String(c.domain || '').includes('xiaomi')) {
          jar[n] = String(c.value || '');
        }
      }
      if (jar.passtoken && jar.cuserid) {
        const got = jar.userid || '';
        if (uid && got && got !== uid) {
          console.error(`[browser] 登录的是 uid=${got}，需要 ${uid} —— 请退出后用正确账号登录`);
          continue;
        }
        // route 通道（mimo-server-cn /api/sts）只认 PC 客户端设备指纹（pc_ 前缀，
        // 网页登录的 wb_ 指纹会被 serviceLogin 拒绝）——统一规范化为 pc_ + 32hex。
        let dev = jar.deviceid || '';
        if (!/^pc_/.test(dev)) {
          dev = 'pc_' + randomBytes(16).toString('hex');
        }
        console.log(JSON.stringify({
          passToken: jar.passtoken,
          cUserId: jar.cuserid,
          userId: got,
          deviceId: dev,
        }));
        ws.close();
        kill();
        process.exit(0);
      }
    } catch (e) { /* ws 可能断，重连下一轮忽略 */ }
    // 用户可能关闭窗口
    try { if (proc.exitCode !== null) { console.error('browser window closed by user'); kill(); process.exit(1); } } catch {}
  }
  console.error(`timeout after ${timeoutSec}s, no login finished`);
  ws.close();
  kill();
  process.exit(1);
}

main().catch((e) => { console.error('ERR:', e.message); process.exit(1); });
