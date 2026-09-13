// diag_multiplier_mismatch.js —— 倍率 id 错位的诊断（**已修复后的对照口径**）。
//
// ============================================================================
// # 它原来诊断的是什么（历史线索，不再是缺陷）
//
// 实测线索：32 个模型里只有 15 个能在 /admin/models/preview 里找到倍率。
// 规律是：**倍率表的键是上游目录里的裸名**（`glm-5.2`），而模型面板显示
// **`provider/model`**（`workbuddy/glm-5.2`）→ 直接查表全部落空，
// 界面上表现为满屏 `x无`（把"上游没配倍率"这个**假事实**报给用户）。
//
// # 它现在诊断什么
//
// 前端已经加了**唯一**归一函数 `multKeyOf(id)`（去掉 `provider/` 前缀后查表。
// 源码在 webui.html，判据在 internal/server/webui_models_unified_test.go）。
// 所以这条错位**应当已经消失**，本脚本随之从"找缺陷"变成"修复后的对照"：
//
//   1. 直接命中率    mult[id]              —— 旧口径（带前缀的 id 必然落空）
//   2. 归一后命中率  mult[multKeyOf(id)]   —— **界面真正用的口径**（必须命中）
//   3. 逐条列出"归一后仍找不到倍率"的模型 —— 那才是**当前**的真缺陷
//
// 判据与退出码：
//   · 有模型，且归一后命中率 == 100%               → 打印"归一生效" → exit 0
//   · 有模型，但归一后仍有落空的                   → 列出落空项 → exit 1
//     （真缺陷：后端没下发这个模型的倍率，或前端归一被绕过）
//   · 一个模型都没取到（实例没起/KEY 不对）        → exit 2，**不给结论**
// ⚠ 本脚本**不会伪造结果**：连不上实例时只报"需要实例"，不打印任何"通过"。
//
// # 前置条件（这是诊断脚本，不是自动化套件）
//
//   · 需要**跑着的实例**：默认 127.0.0.1:18080（FRESH_PORT 覆盖），
//     Bearer KEY 取 WB2API_KEY（默认 test-key-not-real）；
//   · 它只看 **API 数据**（/v1/models + /admin/models/preview），不看 DOM。
//     面板上真正渲染成什么样由 verify_models_grouped.js 的第 [4] 节与
//     diag_multiplier_ui.js（真 Chrome + CDP，需同一个实例）负责。
// ============================================================================
'use strict';
const http = require('http');

const KEY = process.env.WB2API_KEY || 'test-key-not-real';
const PORT = Number(process.env.FRESH_PORT || 18080);

// 与前端 multKeyOf 同一条规则：去掉第一个 `provider/` 前的前缀；没有前缀则原样。
//
// ⚠ 这里**故意重写一份**（不 require 前端源码、不 eval webui.html）：
// 诊断脚本要独立复算。抄被测对象就等于用被测对象证明被测对象。
// 正本与它的守卫在 webui.html / webui_models_unified_test.go 里。
function multKeyOf(id) {
  const s = String(id == null ? '' : id).indexOf('/');
  return s > 0 ? String(id).slice(s + 1) : String(id == null ? '' : id);
}

function get(path) {
  return new Promise((res, rej) => {
    http.get({ host: '127.0.0.1', port: PORT, path, headers: { Authorization: 'Bearer ' + KEY } }, r => {
      const chunks = [];
      r.on('data', c => chunks.push(c));
      r.on('end', () => { try { res(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch { res(null); } });
    }).on('error', rej);
  });
}

(async () => {
  let modelsResp, prevResp;
  try {
    modelsResp = await get('/v1/models');
    prevResp = await get('/admin/models/preview');
  } catch (e) {
    console.log('=== 需要跑着的实例，本次**没有**取到数据 —— 不给结论 ===');
    console.log('  目标: http://127.0.0.1:' + PORT + '（FRESH_PORT 可覆盖），KEY=' + JSON.stringify(KEY));
    console.log('  连不上: ' + e.message);
    process.exit(2);
  }
  if (!modelsResp || !Array.isArray(modelsResp.data) || !prevResp || !Array.isArray(prevResp.models)) {
    console.log('=== 实例可达，但 /v1/models 或 /admin/models/preview 的响应不是预期结构 —— 不给结论 ===');
    console.log('  /v1/models.data 是数组: ' + !!(modelsResp && Array.isArray(modelsResp.data))
      + '；/admin/models/preview.models 是数组: ' + !!(prevResp && Array.isArray(prevResp.models)));
    process.exit(2);
  }

  const models = modelsResp.data;
  const prev = prevResp.models;
  const mult = {};
  prev.forEach(x => { if (x && x.model) mult[x.model] = x.multiplier; });

  console.log('=== 逐个模型看倍率命中情况（含归一后）===\n');
  const records = models.map(m => {
    const id = m.id;
    const key = multKeyOf(id);
    const hasKey = Object.prototype.hasOwnProperty.call(mult, key);
    return {
      id,
      owned: m.owned_by,
      key,
      directHas: Object.prototype.hasOwnProperty.call(mult, id),   // 旧口径：完整 id 直查
      directValue: mult[id],
      normHas: hasKey,                                            // 新口径：归一后查（界面用的）
      normValue: hasKey ? mult[key] : undefined,
      prefixed: id.indexOf('/') > 0,
    };
  });

  console.log('--- 带前缀（' + records.filter(r => r.prefixed).length + ' 个）---');
  records.filter(r => r.prefixed).forEach(r => console.log('  ' + r.id.padEnd(30)
    + ' 直查=' + r.directHas
    + '  归一(' + r.key + ')=' + r.normHas + ' 倍率=' + JSON.stringify(r.normValue)));
  console.log('\n--- 裸名（' + records.filter(r => !r.prefixed).length + ' 个）---');
  records.filter(r => !r.prefixed).forEach(r => console.log('  ' + r.id.padEnd(30)
    + ' 直查=' + r.directHas + '  倍率=' + JSON.stringify(r.directValue)));

  const directHit = records.filter(r => r.directHas).length;
  const normHit = records.filter(r => r.normHas).length;
  const misses = records.filter(r => !r.normHas);

  console.log('\n=== 汇总（口径：面板显示完整 id，倍率表的键是裸名）===');
  console.log('  直接命中   mult[id]            : ' + directHit + '/' + records.length
    + '   ← 旧口径；带前缀的必然落空，这是**预期**的');
  console.log('  归一后命中 mult[multKeyOf(id)] : ' + normHit + '/' + records.length
    + '   ← 界面真正用的口径；应当 == 总数');

  // 只在倍率表里、不在 /v1/models 里的名字。
  // 新口径下这是**正常**的：表用裸名、目录用前缀名，前端靠 multKeyOf 对上。
  console.log('\n=== 只在倍率表里出现、/v1/models 里没有的名字（前 12）===');
  const ids = new Set(records.map(r => r.id));
  const onlyInTable = prev.filter(x => x && x.model && !ids.has(x.model));
  onlyInTable.slice(0, 12).forEach(x => console.log('  ' + String(x.model).padEnd(26) + ' ×' + x.multiplier));
  console.log('  （共 ' + onlyInTable.length + ' 个）');
  console.log('  新口径下这是正常的：表键=裸名、目录 id=provider/裸名；'
    + '归一后如果仍对不上，下面的"落空清单"才是缺陷。');

  let bad = 0;
  if (misses.length) {
    bad = misses.length;
    console.log('\n=== ⚠ 归一后仍找不到倍率的模型（' + misses.length + ' 个）—— **当前的真缺陷** ===');
    misses.slice(0, 20).forEach(r => console.log('  ' + r.id.padEnd(30)
      + ' 归一键=' + JSON.stringify(r.key)
      + '  表里有这个键=' + Object.prototype.hasOwnProperty.call(mult, r.key)));
    console.log('  两种可能：① 后端 /admin/models/preview 没有下发这个模型（上游目录与倍率目录不一致）；'
      + '\n              ② 前端归一被绕过（有人又写回 modelMultipliers[id] —— 见 webui_models_unified_test.go 的守卫）。');
  }

  console.log('\n=== 结论（对照口径）===');
  if (records.length === 0) {
    console.log('  /v1/models 是空的 —— 无从判断（没有可用账号？）。不给"通过"结论。');
    process.exit(2);
  }
  if (directHit === 0 && normHit > 0) {
    console.log('  确认错位存在且**已被前端归一修好**：倍率表的键是裸名，'
      + '面板 id 是 `provider/model`，直查 ' + directHit + '/' + records.length
      + ' 落空；经 multKeyOf 归一后 ' + normHit + '/' + records.length + ' 命中。');
    console.log('  → 面板不会再因为这条错位显示满屏 `x无`。');
  } else if (normHit === records.length) {
    console.log('  归一后 100% 命中（' + normHit + '/' + records.length + '）：'
      + '倍率表的键与目录 id 本来就对得上（单上游裸名口径），归一是**幂等**的，没有副作用。');
  }
  if (bad) {
    console.log('  ⚠ 但仍有 ' + bad + ' 个模型归一后查不到倍率 —— 见上面的"落空清单"，那是真缺陷。');
  }

  // 第二个命名不一致：倍率表里叫 default，而目录里叫 auto。
  const autoRec = records.find(r => r.key === 'auto');
  if (autoRec && !autoRec.normHas && Object.prototype.hasOwnProperty.call(mult, 'default')) {
    console.log('  另外 "auto" 在倍率表里叫 "default" —— 是**第二个**命名不一致'
      + '（multKeyOf 只去前缀，修不了这一条；界面上它就是 `x无`）。');
  }

  console.log(bad ? '\n=== 有落空项（见上）===' : '\n=== 归一后无落空 ===');
  process.exit(bad ? 1 : 0);
})().catch(e => { console.log('EXCEPTION: ' + e.message); process.exit(1); });
