'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.resolve(__dirname, '../..');
const read = rel => fs.readFileSync(path.join(root, rel), 'utf8').replace(/\r\n/g, '\n');
const app = read('internal/server/ui/app.js');
const tokens = read('internal/server/webui-tokens.html');
function slice(src, start, end) {
  const a = src.indexOf(start), b = src.indexOf(end, a + start.length);
  assert(a >= 0 && b > a, start);
  return src.slice(a, b);
}
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function harness() {
  const elements = {}, requests = [];
  const document = { activeElement: null, querySelectorAll: () => [], addEventListener() {} };
  function element(id) {
    const classes = new Set();
    return elements[id] = {
      value: '', checked: false, disabled: false, textContent: '', style: {}, tabIndex: 0, isConnected: true,
      classList: { add: x => classes.add(x), remove: x => classes.delete(x), contains: x => classes.has(x) },
      focus() { document.activeElement = this; },
      getClientRects() { return this.hidden ? [] : [{}]; },
      addEventListener(type, fn) { this[type] = fn; }
    };
  }
  const fetch = (url, opts) => {
    const d = deferred();
    requests.push({ url, opts, ...d });
    return d.promise;
  };
  const context = vm.createContext({
    document, $: id => { assert(elements[id], 'missing DOM ' + id); return elements[id]; },
    fetch, api: (url, opts) => fetch(url, opts).then(r => r.json()),
    admin: (url, opts) => fetch(url, opts).then(r => r.json()),
    performance: { now: () => 10 }, TextDecoder, hdr: () => ({}), testUsageLine: () => '',
    toast() {}, getComputedStyle: el => ({ visibility: el.visibility || 'visible' })
  });
  return { context, elements, element, document, requests };
}
async function chat() {
  const h = harness(), { context: c, element: el, elements: e, requests: r } = h;
  ['send', 'out', 'model', 'stream', 'prompt'].forEach(el);
  vm.runInContext(slice(app, '  let sendInFlight', '  // wireTopBar'), c);
  vm.runInContext(slice(app, "    $('send').onclick = send;", "    $('models').addEventListener"), c);
  for (const event of [{ key: 'Enter', isComposing: true }, { key: 'Enter', keyCode: 229 }, { key: 'Enter', shiftKey: true }]) {
    e.prompt.keydown({ ...event, preventDefault() { assert.fail('IME/Shift Enter prevented'); } });
  }
  assert.equal(r.length, 0);
  const first = e.send.onclick();
  e.prompt.keydown({ key: 'Enter', preventDefault() {} });
  await c.send();
  assert.equal(r.length, 1);
  assert.equal(e.send.disabled, true);
  r[0].resolve({ json: async () => ({ model: 'mock', choices: [{ message: { content: 'ok' } }] }) });
  await first;
  assert.match(e.out.textContent, /ok/);
  assert.equal(e.send.disabled, false);
  const failed = c.send();
  r[1].reject(new Error('offline failure'));
  await failed;
  assert.match(e.out.textContent, /offline failure/);
  assert.equal(e.send.disabled, false);
  e.stream.checked = true;
  const streaming = c.send();
  const chunk = deferred();
  r[2].resolve({ ok: true, body: { getReader: () => ({ read: () => chunk.promise }) } });
  await Promise.resolve();
  await c.send();
  assert.equal(r.length, 3);
  chunk.reject(new Error('reader failure'));
  await streaming;
  assert.match(e.out.textContent, /reader failure/);
  assert.equal(e.send.disabled, false);
  const retry = c.send();
  r[3].resolve({ ok: false, status: 503, text: async () => 'mock' });
  await retry;
  assert.match(e.out.textContent, /503/);
  assert.equal(e.send.disabled, false);
  Object.defineProperty(e.model, 'value', { get() { throw new Error('snapshot failure'); } });
  await c.send();
  assert.match(e.out.textContent, /snapshot failure/);
  assert.equal(vm.runInContext('sendInFlight', c), false);
  console.log('PASS chat: IME, Enter/click dedupe, JSON, network, stream read/HTTP and snapshot failure release');
}
async function settings() {
  const h = harness(), { context: c, element: el, elements: e, requests: r } = h;
  const save = el('btnSettingsSave'), input = el('input'), readonly = el('readonly');
  save.textContent = '保存设置'; input.value = 'draft'; readonly.disabled = true;
  h.document.querySelectorAll = selector => {
    assert.match(selector, /section\[data-key="settings"\]/);
    return [save, input, readonly];
  };
  let fills = 0;
  c.collectSettings = () => ({ value: input.value });
  c.fillSettings = value => { fills++; input.value = value.value; };
  vm.runInContext('let settingsSaving = false;\n' + slice(app, '  async function saveSettings()', '  // ---------------------------------------------------------------- 成长计划'), c);
  const first = c.saveSettings();
  await c.saveSettings();
  assert.equal(r.length, 1);
  assert.equal(input.disabled, true);
  assert.equal(save.disabled, true);
  assert.equal(JSON.parse(r[0].opts.body).value, 'draft');
  r[0].reject(new Error('save failed'));
  await first;
  assert.equal(input.value, 'draft'); assert.equal(fills, 0);
  assert.equal(input.disabled, false); assert.equal(readonly.disabled, true);
  assert.equal(save.textContent, '保存设置');
  const retry = c.saveSettings();
  r[1].resolve({ json: async () => ({ settings: { value: 'saved' } }) });
  await retry;
  assert.equal(input.value, 'saved'); assert.equal(fills, 1);
  assert.equal(input.disabled, false); assert.equal(readonly.disabled, true);
  assert.equal(vm.runInContext('settingsSaving', c), false);
  console.log('PASS settings: in-flight lock, duplicate save, failure draft, success refill, disabled restoration');
}
async function drawer() {
  const h = harness(), { context: c, element: el, elements: e, document: d } = h;
  ['drawer', 'mask', 'btnNew', 'drawerTitle', 'w-count', 'partial', 'groupBanner', 'f-name', 'f-count', 'f-mode-auto', 'f-mode-manual', 'f-automode', 'f-exp', 'last', 'hidden', 'disabled'].forEach(el);
  const opener = el('opener'); opener.focus();
  e.hidden.hidden = true; e.disabled.disabled = true;
  e.drawer.querySelectorAll = () => [e['f-name'], e.hidden, e.disabled, e.last];
  Object.defineProperty(e.drawer, 'className', { set(v) { if (v.endsWith(' on')) this.classList.add('on'); else this.classList.remove('on'); } });
  c.state = { busy: false }; c.renderLegacyNote = () => {}; c.loadGroups = () => {}; c.syncVisibility = () => {};
  vm.runInContext(slice(tokens, 'var drawerReturnFocus', 'function renderDrag('), c);
  c.openDrawer(null);
  assert.equal(d.activeElement, e['f-name']);
  const key = (key, shiftKey = false) => { let prevented = false; c.drawerKeydown({ key, shiftKey, preventDefault() { prevented = true; } }); return prevented; };
  assert.equal(key('Tab', true), true); assert.equal(d.activeElement, e.last);
  assert.equal(key('Tab'), true); assert.equal(d.activeElement, e['f-name']);
  opener.focus(); key('Tab'); assert.equal(d.activeElement, e['f-name']);
  c.state.busy = true;
  key('Escape'); c.closeDrawer();
  assert.equal(e.drawer.classList.contains('on'), true);
  c.state.busy = false;
  key('Escape'); assert.equal(d.activeElement, opener);
  c.openDrawer(null); opener.isConnected = false; c.closeDrawer(); assert.equal(d.activeElement, e.btnNew);
  c.openDrawer(null); e.drawer.querySelectorAll = () => []; key('Tab'); assert.equal(d.activeElement, e.drawer);
  c.collectForm = () => ({ name: 'mock' });
  c.setBusy = on => { c.state.busy = on; };
  c.refresh = () => Promise.resolve();
  c.esc = value => value;
  c.api = (method, url, body) => c.fetch(url, { method, body }).then(r => r.json());
  vm.runInContext(slice(tokens, 'function save() {', '// 列表交互'), c);
  for (const outcome of ['failure', 'partial', 'success']) {
    e.btnNew.focus();
    c.openDrawer(outcome === 'partial' ? null : { id: 1 });
    c.save();
    key('Escape');
    assert.equal(e.drawer.classList.contains('on'), true);
    const request = h.requests[h.requests.length - 1];
    if (outcome === 'failure') request.reject(new Error('mock failure'));
    else {
      request.resolve({ json: async () => outcome === 'partial' ? { created: 1, requested: 2, failed: 1, failures: [] } : {} });
    }
    await new Promise(setImmediate);
    assert.equal(c.state.busy, false);
    assert.equal(e.drawer.classList.contains('on'), outcome !== 'success');
  }
  assert.equal(d.activeElement, e.btnNew);
  console.log('PASS drawer: initial/trapped/return focus, busy Escape/close, save failure/partial/success lifecycle');
}
function sync() {
  const script = read('tools/toast_sync.js');
  const src = read('internal/server/toast.js');
  function run(missing, stale, check = true) {
    const files = {
      'internal/server/toast.js': src,
      'internal/server/ui/app.js': missing ? 'const untouched = 1;' : app,
      'internal/server/webui-tokens.html': stale ? tokens.replace('function toast(msg, bad)', 'function toast(msg, wrong)') : tokens
    };
    const before = { ...files }, logs = [], process = { argv: check ? ['node', 'toast_sync.js', '--check'] : ['node', 'toast_sync.js'] };
    const fakeFS = {
      readFileSync(p) { const rel = path.relative(root, p).replace(/\\/g, '/'); assert(rel in files, rel); return files[rel]; },
      writeFileSync(p, text) { assert(!check, 'check must not write'); files[path.relative(root, p).replace(/\\/g, '/')] = text; }
    };
    vm.runInNewContext(script, { require: name => { if (name === 'fs') return fakeFS; assert.equal(name, 'path'); return path; }, __dirname: path.join(root, 'tools'), process, console: { log: x => logs.push(x), error: x => logs.push(x) } });
    return { files, before, logs, process };
  }
  assert.equal(run(false, false).process.exitCode, undefined);
  const missing = run(true, false);
  assert.equal(missing.process.exitCode, 1); assert(!missing.logs.includes('全部与 toast.js 同步。'));
  assert.equal(run(false, true).process.exitCode, 1);
  const written = run(false, true, false);
  assert.equal(written.files['internal/server/ui/app.js'], app);
  assert.equal(written.files['internal/server/webui-tokens.html'], tokens);
  assert.equal(run(true, false, false).process.exitCode, 1);
  console.log('PASS toast sync: actual targets, missing/stale failures, honest summary, generated-only repair');
}
function semantics() {
  const html = read('internal/server/webui.html');
  for (const id of ['stFrame', 'chFrame', 'grpFrame', 'mdFrame', 'tkFrame']) {
    const tag = html.match(new RegExp('<iframe\\b[^>]*id="' + id + '"[^>]*>'))[0];
    assert.match(tag, /title="[^"]+"/); assert.match(tag, /width:100%;height:calc\(100vh - 190px\);min-height:460px/);
    assert.match(tag, id === 'tkFrame' ? /border:0/ : /border:1px solid var\(--line\)/);
  }
  for (const page of [html, tokens]) assert.match(page, /id="toast"[^>]*role="log"[^>]*aria-live="polite"/);
  console.log('PASS semantics: iframe titles/sizes/border differences and toast live region');
}
(async () => { sync(); await chat(); await settings(); await drawer(); semantics(); })().catch(error => { console.error(error); process.exitCode = 1; });
