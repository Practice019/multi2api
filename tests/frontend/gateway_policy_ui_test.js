'use strict';
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const root = path.resolve(__dirname, '../../internal/server');
const read = name => fs.readFileSync(path.join(root, name), 'utf8');
let failures = 0;
const pending = [];
function test(name, fn) {
  pending.push(Promise.resolve().then(fn).then(() => console.log('PASS ' + name), e => {
    failures++;
    console.error('FAIL ' + name + ': ' + e.message);
  }));
}
function snippet(source, start, end) {
  const from = source.indexOf(start);
  assert.ok(from >= 0, start);
  const to = source.indexOf(end, from + start.length);
  assert.ok(to > from, end);
  return source.slice(from, to);
}
const channels = read('webui-channels.html');
const groups = read('webui-groups.html');
const tokens = read('webui-tokens.html');
const models = read('webui-models.html');
const app = read('ui/app.js');
test('channel kinds and providers', () => {
  for (const text of ['id="f-kind"', '/admin/channels/providers', 'provider_id:', 'function syncChannelKind(', 'gateway:navigate']) assert.ok(channels.includes(text), text);
  assert.ok(!channels.includes('id="f-wt"'));
  assert.ok(!channels.includes("['', 'random', 'round_robin']"));
});
test('versioned channel group editor', () => {
  for (const text of ['channel_ids', 'policy_version', 'function loadGroupChannels(', 'groupPolicyVersion === 2']) assert.ok(groups.includes(text), text);
});
test('token policy preview and model intersection', () => {
  for (const text of ['id="f-model-limits-enabled"', 'id="f-model-limits"', '/admin/tokens/preview', 'excluded_models', 'unresolved_groups', 'policy_version']) assert.ok(tokens.includes(text), text);
  assert.ok(!tokens.includes("fetch('/admin/apikeys',"));
});
test('model resource labels and complete refresh', () => {
  assert.ok(!models.includes("'可调用'"));
  assert.ok(models.includes('return load().then('));
  assert.ok(models.includes('state.registry'));
  for (const gone of ["setAttribute('data-act', 'probe')", "el('button', null, '只看它')"]) assert.ok(!models.includes(gone));
});
test('parent theme and safe navigation bridge', () => {
  assert.ok(app.includes("'gateway:navigate'"));
  assert.ok(app.includes('function syncFrameThemes('));
});
for (const name of ['webui-channels.html', 'webui-groups.html', 'webui-tokens.html', 'webui-models.html', 'webui.html']) {
  test('syntax ' + name, () => {
    for (const match of read(name).matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/gi)) {
      if (!/\bsrc\s*=/.test(match[1])) new vm.Script(match[2], { filename: name });
    }
  });
}
test('syntax app.js', () => new vm.Script(app));
test('preview failures never render effective permission success', async () => {
  const elements = {
    'f-model-limits-enabled': { checked: true },
    'f-model-limits': { value: '' },
    policyPreview: { textContent: '' }
  };
  const context = vm.createContext({
    state: { groups: ['g-one'] },
    policyPreviewSequence: 0,
    $: id => elements[id],
    api: async () => { throw new Error('HTTP 503'); }
  });
  vm.runInContext(snippet(tokens, 'function previewTokenPolicy()', "$('btnPolicyPreview')"), context);
  await context.previewTokenPolicy();
  assert.match(elements.policyPreview.textContent, /^权限预览失败/);
  context.api = async () => ({ ready: false, models: [], excluded_models: [], unresolved_groups: [] });
  await context.previewTokenPolicy();
  assert.match(elements.policyPreview.textContent, /^目录未就绪/);
  context.api = async () => ({ error: 'bad', ready: true, models: [], excluded_models: [], unresolved_groups: [] });
  await context.previewTokenPolicy();
  assert.match(elements.policyPreview.textContent, /^权限预览失败/);
});
test('token v2 payload keeps deny-empty and legacy edits omit advanced fields', () => {
  const elements = {
    'f-mode-auto': { checked: false }, 'f-count': { value: '1' },
    'f-name': { value: 'fixture' }, 'f-exp': { value: '' },
    'f-model-limits-enabled': { checked: true }, 'f-model-limits': { value: '' }
  };
  const context = vm.createContext({ state: { editing: null, groups: ['g-one'] }, tokenGroupsReady: true, $: id => elements[id] });
  vm.runInContext(snippet(tokens, 'function tokenPolicyVersion()', 'var policyPreviewSequence'), context);
  vm.runInContext(snippet(tokens, 'function collectForm()', 'function save()'), context);
  const body = context.collectForm();
  assert.equal(body.policy_version, 2);
  assert.equal(body.model_limits_enabled, true);
  assert.equal(body.model_limits.length, 0);
  assert.ok(!('automatic' in body));
  assert.ok(!('remain_quota' in body));
  context.state.editing = { id: 7, policy_version: 0, remain_quota: 0 };
  const legacy = context.collectForm();
  assert.equal(legacy.policy_version, 0);
  for (const field of ['remain_quota', 'unlimited_quota', 'model_limits', 'model_limits_enabled', 'cross_group_retry']) assert.ok(!(field in legacy), field);
  context.state.editing = null;
  context.tokenGroupsReady = false;
  assert.throws(() => context.collectForm(), /清单未就绪/);
});
test('token list masks even unexpectedly plaintext responses', () => {
  const context = vm.createContext({});
  vm.runInContext(snippet(tokens, 'function maskTokenKey(', 'function tokenPolicyVersion('), context);
  assert.notEqual(context.maskTokenKey('sk-fixture-secret-value'), 'sk-fixture-secret-value');
  assert.equal(context.maskTokenKey('short'), '••••');
});
Promise.all(pending).then(() => { process.exitCode = failures ? 1 : 0; });
