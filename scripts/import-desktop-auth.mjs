#!/usr/bin/env node
// scripts/import-desktop-auth.mjs —— 从**官方桌面客户端**的登录态导入 workbuddy 凭证。
//
// # 为什么需要它（用户实测：CLI 插件流程登录的号「试用版未激活」）
//
// 我们的登录复刻的是 **CLI 插件设备授权流程**：
//
//	POST /v2/plugin/auth/state?platform=CLI → 浏览器登录 → GET /v2/plugin/auth/token
//
// 而官方桌面端走的是**完整 OIDC 授权码流程**（`www.workbuddy.ai/auth/realms/copilot`）
// + 首启初始化（含「设置地区」一步）。实测差异：
//
//	官方桌面端登录的号：可用（478 credits，chat 通）
//	CLI 插件流程登录的号：`429 code=14017 The trial version is not yet activated`
//
// 官方登录态文件里还带着我们导出时**丢掉的字段**：
//
//	account.uin / type / areaInfoComplete / pluginEnabled / deployStatus
//	isFirstLogin / loginChannels / lastLogin / isCreator / isAdmin ...
//
// 所以：**不再自己拼登录流程，直接复用官方客户端的完整流程**，然后收割它的凭据文件。
//
// # 官方登录态文件在哪
//
//	%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\
//	    workbuddy-desktop-ai.<ISO 时间戳>.<pid>.<uuid>.info   国际版（www.workbuddy.ai）
//	    workbuddy-desktop.info                                国内版（copilot.tencent.com）
//	    workbuddy-desktop-ai.info.logged-out                  登出标记（存在即表示已登出）
//
// # 用法
//
//	node scripts/import-desktop-auth.mjs                 # 导入最新的国际版登录态
//	node scripts/import-desktop-auth.mjs --list          # 只列出可导入的文件
//	node scripts/import-desktop-auth.mjs --channel cn    # 导入国内版
//	node scripts/import-desktop-auth.mjs --file <路径>    # 指定文件
//	node scripts/import-desktop-auth.mjs --dry-run       # 只转换不落盘
//
// 落盘到 `auths/workbuddy-intl/workbuddy-<uid>.json`（与网关既有格式一致），
// 之后在管理台点「重载 auths」即可入池。

import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

const AUTH_DIR = path.join(
  process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local'),
  'CodeBuddyExtension', 'Data', 'Public', 'auth',
)

// 渠道 → 官方登录态文件名前缀 + 落盘目录 + 域
const CHANNELS = {
  intl: { prefix: 'workbuddy-desktop-ai', outDir: 'auths/workbuddy-intl', domain: 'www.workbuddy.ai', label: '国际版 WorkBuddy AI' },
  cn: { prefix: 'workbuddy-desktop', outDir: 'auths/workbuddy', domain: 'copilot.tencent.com', label: '国内版 WorkBuddy' },
}

const argv = process.argv.slice(2)
const flag = (name) => argv.includes('--' + name)
const opt = (name, dflt = '') => {
  const i = argv.indexOf('--' + name)
  return i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--') ? argv[i + 1] : dflt
}

const channel = opt('channel', 'intl')
const cfg = CHANNELS[channel]
if (!cfg) {
  console.error(`未知渠道 ${channel}（可选 ${Object.keys(CHANNELS).join(' / ')}）`)
  process.exit(1)
}

// ── 找候选登录态文件 ────────────────────────────────────────────────────────
// ⚠ 只认带时间戳的那种（`<prefix>.<ISO>.<pid>.<uuid>.info`）。
//   `<prefix>.info.logged-out` 是登出标记，不是凭据；
//   裸 `<prefix>.info` 是「当前会话」指针，可能指向已登出的号。
function candidates() {
  let names = []
  try { names = fs.readdirSync(AUTH_DIR) } catch { return [] }
  return names
    .filter((n) => n.startsWith(cfg.prefix + '.') && n.endsWith('.info') && n !== cfg.prefix + '.info')
    .map((n) => {
      const full = path.join(AUTH_DIR, n)
      let st = null
      try { st = fs.statSync(full) } catch { /* 竞态忽略 */ }
      return { name: n, full, mtime: st ? st.mtime : new Date(0), size: st ? st.size : 0 }
    })
    .sort((a, b) => b.mtime - a.mtime)
}

const list = candidates()

if (flag('list')) {
  console.log(`登录态目录: ${AUTH_DIR}`)
  console.log(`渠道: ${cfg.label}（前缀 ${cfg.prefix}）`)
  if (!list.length) { console.log('  （没有可导入的登录态文件）'); process.exit(0) }
  for (const c of list) {
    let uid = '?', nick = '?'
    try {
      const j = JSON.parse(fs.readFileSync(c.full, 'utf-8'))
      uid = j?.account?.uid || '?'
      nick = j?.account?.nickname || '?'
    } catch { /* 忽略 */ }
    console.log(`  ${c.mtime.toISOString()}  ${String(c.size).padStart(6)}B  ${nick}  ${uid}`)
    console.log(`      ${c.name}`)
  }
  // 登出标记
  const out = path.join(AUTH_DIR, cfg.prefix + '.info.logged-out')
  if (fs.existsSync(out)) console.log(`\n  ⚠ 存在登出标记: ${path.basename(out)}（该渠道当前处于已登出状态）`)
  process.exit(0)
}

const target = opt('file') || (list[0] && list[0].full)
if (!target) {
  console.error(`没有找到 ${cfg.label} 的登录态文件。`)
  console.error(`  目录: ${AUTH_DIR}`)
  console.error(`  请先用官方客户端登录: D:\\software\\WorkBuddy-win32-x64-user-5.5.2.37849279-910352f0\\WorkBuddyAI\\WorkBuddyAI.exe`)
  process.exit(1)
}

// ── 转换 ────────────────────────────────────────────────────────────────────
const raw = JSON.parse(fs.readFileSync(target, 'utf-8'))
const acc = raw.account || {}
const a = raw.auth || {}

if (!a.accessToken && !a.refreshToken) {
  console.error(`✗ ${path.basename(target)} 里没有 accessToken/refreshToken`)
  process.exit(1)
}
if (!acc.uid) {
  console.error(`✗ ${path.basename(target)} 里没有 account.uid（无法决定落盘文件名）`)
  process.exit(1)
}

// 官方 expiresAt 是**毫秒**；网关要的是**秒**。
const toSec = (v) => {
  const n = Number(v || 0)
  if (!n) return 0
  return n > 1e12 ? Math.floor(n / 1000) : Math.floor(n)
}

const out = {
  auth: {
    accessToken: a.accessToken || '',
    refreshToken: a.refreshToken || '',
    expiresAt: toSec(a.expiresAt),
    domain: a.domain || cfg.domain,
    channel: channel,
  },
  account: {
    uid: acc.uid,
    enterpriseId: acc.enterpriseId || '',
    nickname: acc.nickname || '',
    // ★ 官方登录态里这些字段是我们 CLI 流程**拿不到**的，全部保留。
    //   `areaInfoComplete` 是用户实测「官方登录有个设置地区步骤」的对应字段。
    uin: acc.uin || '',
    type: acc.type || '',
    areaInfoComplete: acc.areaInfoComplete === true,
    pluginEnabled: acc.pluginEnabled === true,
    isFirstLogin: acc.isFirstLogin === true,
    loginChannels: acc.loginChannels || undefined,
    phoneNumber: acc.phoneNumber || undefined,
    // 设备令牌：官方会给，CLI 流程没有；网关的 injectDeviceToken 会用
    deviceToken: acc.deviceToken || undefined,
    // 登录态文件里的会话态（排障用，不参与鉴权）
    sessionState: a.sessionState || undefined,
    scope: a.scope || undefined,
    source: 'desktop-client',
    importedFrom: path.basename(target),
    importedAt: new Date().toISOString(),
  },
}
// 去掉 undefined，保持文件干净
for (const k of Object.keys(out.account)) if (out.account[k] === undefined) delete out.account[k]

const outFile = path.join(cfg.outDir, `workbuddy-${acc.uid}.json`)

console.log(`源文件   : ${target}`)
console.log(`渠道     : ${cfg.label}`)
console.log(`账号     : ${acc.nickname}  uid=${acc.uid}  uin=${acc.uin || '(无)'}`)
console.log(`地区信息 : areaInfoComplete=${out.account.areaInfoComplete}  type=${acc.type || '(无)'}`)
console.log(`accessToken : len=${out.auth.accessToken.length}  expiresAt=${out.auth.expiresAt} (${out.auth.expiresAt ? new Date(out.auth.expiresAt * 1000).toISOString() : '-'})`)
console.log(`refreshToken: len=${out.auth.refreshToken.length}`)
console.log(`落盘     : ${outFile}`)

if (flag('dry-run')) {
  console.log('\n--dry-run：未落盘。转换结果：')
  console.log(JSON.stringify(out, null, 2))
  process.exit(0)
}

fs.mkdirSync(cfg.outDir, { recursive: true })
const tmp = outFile + '.tmp'
fs.writeFileSync(tmp, JSON.stringify(out, null, 2), { encoding: 'utf-8', mode: 0o600 })
fs.renameSync(tmp, outFile)
console.log(`\n✅ 已导入 → ${outFile}`)
console.log('   下一步：管理台点「重载 auths」（选对应上游）即可入池。')
