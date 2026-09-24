#!/usr/bin/env node
// scripts/mimo/read-desktop-cookies.mjs — 读 MiMo 桌面端 Cookie 库（value 列明文）输出 route 通道弹药。
// 输出: {"passToken":"…","cUserId":"…","userId":"…","deviceId":"…"}
// 读不到（未登录/库不存在/被锁）→ 退出码 1 + stderr 原因（网关探测失败=跳过，不影响登录）。
import { DatabaseSync } from 'node:sqlite';
import os from 'node:os';
import path from 'node:path';

const CANDIDATES = [
  path.join(process.env.APPDATA || '', 'Xiaomi MiMo', 'Partitions', 'xiaomi-account', 'Network', 'Cookies'),
  path.join(process.env.APPDATA || '', 'Xiaomi MiMo', 'Network', 'Cookies'),
];

function read(dbPath) {
  const db = new DatabaseSync(dbPath, { readOnly: true });
  try {
    const rows = db.prepare(
      "SELECT name, value FROM cookies WHERE host_key LIKE '%xiaomi%' ORDER BY name",
    ).all();
    const jar = {};
    for (const r of rows) {
      const n = String(r.name).toLowerCase();
      const v = String(r.value ?? '');
      if (n === 'passtoken' || n === 'cuserid' || n === 'userid' || n === 'deviceid') {
        jar[n] = v;
      }
    }
    return jar;
  } finally {
    db.close();
  }
}

let lastErr = 'no cookie db found';
for (const p of CANDIDATES) {
  try {
    const jar = read(p);
    if (jar.passtoken && jar.cuserid) {
      console.log(JSON.stringify({
        passToken: jar.passtoken,
        cUserId: jar.cuserid,
        userId: jar.userid || '',
        deviceId: jar.deviceid || '',
      }));
      process.exit(0);
    }
    lastErr = `db exists but missing passToken/cUserId (${p})`;
  } catch (e) {
    lastErr = `${e.message} (${p})`;
  }
}
console.error(lastErr);
process.exit(1);
