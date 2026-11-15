'use strict';
// models_plaza_ui_test.js 模型广场前端的判据。
//
// # 这次 bug 的精确形状（判据 ① 就是为它写的）
//
// `#unifiedRows` 在标记里存在、在**脚本区一次都没出现** ——
// 表格因此恒空。用户看到的是一片空白表头，而代码"看起来"实现了广场。
//
// 只断言"页面里有 unifiedRows"会让这个 bug 永远绿：
// 标记里本来就有。必须断言它**在脚本区**出现（被真正引用）。
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const vm = require('node:vm');

const root = path.resolve(__dirname, '../../internal/server');
const page = fs.readFileSync(path.join(root, 'webui-models.html'), 'utf8');

let failures = 0;
const pending = [];
function test(name, fn) {
  pending.push(Promise.resolve().then(fn).then(
    () => console.log('PASS ' + name),
    e => { failures++; console.error('FAIL ' + name + ': ' + e.message); }));
}

// scripts 切出所有内联脚本块（排除带 src 的）。
function scripts() {
  const re = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi;
  const out = [];
  let m;
  while ((m = re.exec(page))) { out.push(m[1]); }
  return out;
}

// scriptBody 所有内联脚本拼起来（判据在"脚本区"里找东西）。
function scriptBody() { return scripts().join('\n'); }

// ★ ① unifiedRows 必须在**脚本区**被引用（那条 bug 的正反面）。
test('plaza table is actually rendered from script', () => {
  const js = scriptBody();
  assert.ok(js.includes('unifiedRows'),
    '★ #unifiedRows 只在标记里出现、脚本区没有 —— 表格会恒空（这正是本次要修的 bug）');
  assert.ok(js.includes('renderPlaza'), '必须有渲染函数');
  assert.ok(js.includes('loadPlaza'), '必须有取数函数');
});

// ★ ② 数据源必须是**分组广场**端点，不是渠道目录。
test('plaza uses the group-based endpoint', () => {
  const js = scriptBody();
  assert.ok(js.includes('/admin/channels/plaza'),
    '★ 广场要读分组口径的 /admin/channels/plaza —— 渠道目录回答的是另一个问题');
});

// ★ ③ 接入片段**不含密钥**（复制是给人贴给同事的动作）。
test('plaza snippet carries no secret', () => {
  const js = scriptBody();
  const at = js.indexOf('function plazaSnippet(');
  assert.ok(at >= 0, 'plazaSnippet 存在');
  // 取到函数结束（下一个 `\n  }`）。
  const end = js.indexOf('\n  }', at);
  const body = js.slice(at, end > at ? end : at + 400);
  assert.ok(body.includes('base_url'), '应带 base_url');
  assert.ok(body.includes('model'), '应带 model');
  for (const bad of ['api_key', 'apiKey', 'admin_token', 'Authorization', 'Bearer', 'sk-', 'token']) {
    assert.ok(!body.includes(bad),
      '★ plazaSnippet 里出现了凭据相关字样「' + bad + '」—— 复制模型名不该带出密钥');
  }
});

// ★ ④ 孤儿模型必须**看得见**（in_catalog=false 是最该报的东西）。
test('plaza surfaces models without channel supply', () => {
  const js = scriptBody();
  assert.ok(js.includes('in_catalog'), '要读 in_catalog');
  assert.ok(js.includes('orphan'), '要有孤儿标记');
  assert.ok(js.includes('没有渠道能服务') || js.includes('无渠道供给'),
    '要把"没渠道能服务"写给人看，而不是只改个颜色');
});

// ⑤ 空态要区分"没配"与"筛掉了"（混成一句会让人建重复的东西）。
test('plaza distinguishes empty from filtered', () => {
  const js = scriptBody();
  assert.ok(js.includes('还没有任何分组配置对外模型'), '要有"没配"的空态文案');
  assert.ok(js.includes('没有匹配'), '要有"被筛掉"的空态文案');
});

// ⑥ 筛选与刷新必须接线。
test('plaza wires search and refresh', () => {
  const js = scriptBody();
  assert.ok(js.includes("getElementById('unifiedSearch')"), '搜索框要接线');
  assert.ok(js.includes("getElementById('unifiedRefresh')"), '刷新按钮要接线');
});

// ⑦ 页面内联脚本必须语法正确（改的是 132 KB 大文件）。
test('models page inline scripts parse', () => {
  const list = scripts();
  assert.ok(list.length >= 1, '至少一个内联脚本块');
  list.forEach((src, i) => {
    try { new vm.Script(src); }
    catch (e) { throw new Error('第 ' + (i + 1) + ' 个内联脚本语法错误: ' + e.message); }
  });
});

Promise.all(pending).then(() => {
  if (failures) { console.error(failures + ' FAILED'); process.exit(1); }
  console.log('all plaza ui tests passed');
});
