// verify_tokens_personal_mode.js —— 令牌页「个人自用」语义的静态门禁。
//
// 为什么需要它：本仓没有通用 DOM 测试框架；令牌页的关键行为（到期判定、
// 不提交额度、旧高级配置只读交代）都能被抽成**纯函数**或**提交体**，
// 用 Node 直接读源码跑，毫秒级，不需要实例/浏览器。
//
// 它守三件事：
//   ① 界面不再提供额度配置（无限额度勾选 / 剩余额度输入）
//   ② 到期令牌被判为 expired，且不能从列表「启用」
//   ③ 本页不再编辑的旧字段（额度 / IP 白名单 / 模型限制 / 跨组重试）**一个都不提交**，
//      由后端按「未提交即保持原值」处理；同时用 #legacyNote 只读交代存量值，
//      不能让用户把「界面看不到」理解成「配置没有」。
'use strict';
const fs = require('fs');
const path = require('path');
const REPO = process.env.WB2API_REPO || path.resolve(__dirname, '..', '..');
const FILE = path.join(REPO, 'internal', 'server', 'webui-tokens.html');
const html = fs.readFileSync(FILE, 'utf8');

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

// ── ① 额度配置必须已从界面移除 ─────────────────────────────────────────
for (const id of ['f-unlimited', 'f-amount', 'f-ips', 'f-cross']) {
  ok(html.indexOf('id="' + id + '"') < 0, '界面不再有 #' + id + '（个人自用：额度/未兑现的高级项不提供编辑）');
}

// ── ② 到期判定是纯函数，直接跑 ────────────────────────────────────────
function grab(name) {
  const start = html.indexOf('function ' + name + '(');
  if (start < 0) return null;
  let depth = 0, seen = false;
  for (let i = start; i < html.length; i++) {
    const ch = html[i];
    if (ch === '{') { depth++; seen = true; }
    else if (ch === '}') { depth--; if (seen && depth === 0) return html.slice(start, i + 1).replace(/^function [a-zA-Z0-9_]+/, 'return function'); }
  }
  return null;
}
const body = grab('tokenStateOf');
ok(body !== null, '页面里存在纯函数 tokenStateOf（到期/禁用/启用三态的唯一判据）');
if (body) {
  const tokenStateOf = new Function(body)();
  const now = 1700000000;
  ok(tokenStateOf({ status: 1, expired_time: -1 }, now) === 'enabled', '永不 + 启用 ⇒ enabled');
  ok(tokenStateOf({ status: 1, expired_time: now + 60 }, now) === 'enabled', '未到期 ⇒ enabled');
  ok(tokenStateOf({ status: 1, expired_time: now }, now) === 'expired', '恰好到期（<= now）⇒ expired（边界按已失效处理）');
  ok(tokenStateOf({ status: 1, expired_time: now - 1 }, now) === 'expired', '已过期 ⇒ expired');
  ok(tokenStateOf({ status: 2, expired_time: now - 1 }, now) === 'expired', '过期且已禁用 ⇒ 仍报 expired（原因优先，不能让用户以为还能改回来）');
  ok(tokenStateOf({ status: 2, expired_time: -1 }, now) === 'disabled', '手动禁用 ⇒ disabled');
}

// ── ③ 旧策略字段：**一个都不提交**，且页面上有只读交代 ─────────────
//
// # 契约（2026-11-10 定；后端同源实现见 internal/admin/tokens.go 的 Present 判定）
// 本页不再编辑额度 / IP 白名单 / 模型限制 / 跨组重试。保真的方式是**一个都不提交**：
// 后端按「未提交即保持原值」处理 ⇒ 既不丢配置，也不会拿陈旧快照覆盖刚被改过的值。
//
// ⚠ 这一段原先守的是另一个契约（「页面必须把旧值回传」）。改成现在的形态是**加强**而非放宽：
//   原来只要求「值被送回去」，现在要求「页面根本没有能力改它」，并要求界面把这件事讲给用户听
//   （#legacyNote）—— 否则用户会把「界面看不到」当成「没配」。
const POLICY_KEYS = ['unlimited_quota', 'remain_quota', 'model_limits_enabled', 'model_limits', 'allow_ips', 'cross_group_retry', 'group_switch_retry_count', 'group_switch_status_codes'];

// collectForm 决定**提交体里有什么**，所以它是这条契约的真正落点。
function grabBody(name) {
  const start = html.indexOf('function ' + name + '(');
  if (start < 0) return null;
  let depth = 0, seen = false;
  for (let i = start; i < html.length; i++) {
    const ch = html[i];
    if (ch === '{') { depth++; seen = true; }
    else if (ch === '}') { depth--; if (seen && depth === 0) return html.slice(start, i + 1); }
  }
  return null;
}
const collect = grabBody('collectForm');
ok(collect !== null, '页面里存在 collectForm（提交体的唯一构造点）');
if (collect) {
  const leaked = POLICY_KEYS.filter((k) => new RegExp('(^|[\\s,{])' + k + '\\s*:').test(collect));
  ok(leaked.length === 0, '提交体不含额度 / IP / 模型限制 / 跨组策略字段（实际泄漏：' + JSON.stringify(leaked) + '）');
  ok(/body\.unlimited_quota\s*=\s*true/.test(collect), '新建时显式 unlimited_quota = true（自用：默认不限额度）');
  ok(/state\.editing/.test(collect), '新建与编辑在提交体上被区分（否则编辑会顺带重置额度）');
  ok(!/remain_quota/.test(collect), '提交体不出现 remain_quota（额度不参与本页）');
}

// state.legacy 必须是**完整**的一份旧值快照，否则只读提示说不出到底带着什么。
const legacyAt = html.indexOf('state.legacy = row ?');
const legacyEnd = legacyAt < 0 ? -1 : html.indexOf('renderLegacyNote()', legacyAt);
const legacyBlock = (legacyAt < 0 || legacyEnd < 0) ? '' : html.slice(legacyAt, legacyEnd);
ok(legacyBlock.length > 0, 'openDrawer 里记录了 state.legacy（旧值快照）');
for (const k of POLICY_KEYS) {
  ok(legacyBlock.indexOf(k) >= 0, 'state.legacy 覆盖 ' + k);
}
ok(html.indexOf('function renderLegacyNote()') >= 0, '存在 renderLegacyNote（只读交代的渲染点）');
ok(html.indexOf('id="legacyNote"') > 0, '提示容器 #legacyNote 在页面里（只展示，不参与提交）');
ok(/保持原样/.test(html), '提示文案说明保存时保持原样（不让用户以为配置丢了）');

console.log(fail === 0 ? '\n全部通过' : '\n失败 ' + fail + ' 项');
process.exit(fail === 0 ? 0 : 1);
