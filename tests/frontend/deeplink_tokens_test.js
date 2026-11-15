'use strict';
// deeplink_tokens_test.js —— 令牌页 URL 深链（U-11「URL 深链与筛选恢复」）的判据。
//
// # 为什么这些断言值得写
//
// 深链看着"加两行就行"，但它有三处会静默出错：
//
//   ① 用 pushState 而不是 replaceState
//      搜索框每输一个字都变 ⇒ 每个击键进历史栈 ⇒ 用户按一次返回只退一个字符。
//      那是**比不实现更糟**的体验，而且在开发机上点两下看不出来（要点十下）。
//   ② 不设白名单
//      URL 是外部输入（可手工拼、可被别的页面跳过来）。把任意键读进页面状态
//      等于开了一个不受控的入口。
//   ③ 先取列表再恢复筛选
//      顺序反了会先拉一遍全量、再拉一遍筛选后的 —— 用户看到列表闪一下。
//
// 这三处都能在没有浏览器的情况下钉住：①② 是纯函数与字符串，③ 是文本顺序。
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const vm = require('node:vm');

const root = path.resolve(__dirname, '../../internal/server');
const tokens = fs.readFileSync(path.join(root, 'webui-tokens.html'), 'utf8');

let failures = 0;
const pending = [];
function test(name, fn) {
  pending.push(Promise.resolve().then(fn).then(
    () => console.log('PASS ' + name),
    e => { failures++; console.error('FAIL ' + name + ': ' + e.message); }));
}

// dlSource 切出深链助手的源码（从 DL_KEYS 定义到 dlRestore 结束）。
function dlSource() {
  const from = tokens.indexOf('var DL_KEYS =');
  assert.ok(from >= 0, 'DL_KEYS 定义');
  const to = tokens.indexOf('window.addEventListener(\'popstate\'', from);
  assert.ok(to > from, 'popstate 之前');
  return tokens.slice(from, to);
}

// dlSandbox 在隔离环境里跑助手，返回它的纯函数。
function dlSandbox() {
  const src = dlSource() +
    '\n;globalThis.__dl = { dlEncode: dlEncode, dlDecode: dlDecode, DL_KEYS: DL_KEYS };';
  const box = { window: { location: { pathname: '/ui/tokens', search: '', hash: '' }, history: {} },
                globalThis: {}, $: () => null };
  box.globalThis = box;
  vm.createContext(box);
  new vm.Script(src).runInContext(box);
  return box.__dl;
}

// ★ ① 必须 replaceState，不许 pushState。
test('deeplink uses replaceState not pushState', () => {
  const src = dlSource();
  assert.ok(src.includes('replaceState'), '应调用 replaceState');
  assert.ok(!src.includes('pushState'),
    '★ 不许用 pushState：搜索框每输一个字都会变，pushState 会让每个击键进历史栈，' +
    '用户按一次返回只退一个字符');
});

// ★ ② 白名单：只认 q，未知键一律丢弃。
test('deeplink decode drops unknown keys', () => {
  const dl = dlSandbox();
  assert.deepEqual([...dl.DL_KEYS], ['q'], '白名单只有 q');

  const got = dl.dlDecode('?q=abc&evil=1&token=sk-oops');
  assert.equal(got.q, 'abc');
  assert.equal(got.evil, undefined, '★ 未知键不许进页面状态（URL 是外部输入）');
  assert.equal(got.token, undefined, '★ 凭据形态的键更不许进');
  assert.deepEqual(Object.keys(got), ['q'], '只应有 q');
});

// ★ ③ 编码只输出白名单键，空值不出现。
test('deeplink encode only emits non-empty whitelisted keys', () => {
  const dl = dlSandbox();
  assert.equal(dl.dlEncode({ q: 'name / Key / owner' }),
    'q=name%20%2F%20Key%20%2F%20owner');
  assert.equal(dl.dlEncode({ q: '  ' }), '', '纯空白视为空');
  assert.equal(dl.dlEncode({ q: '' }), '', '空串不出现');
  assert.equal(dl.dlEncode({}), '', '没值就不带查询串');
  // 非白名单键不许被编码出去（否则"复制地址"会带出无关状态）。
  assert.equal(dl.dlEncode({ q: 'a', key: 'sk-secret' }), 'q=a');
});

// 往返：因为编码/解码都是标准 percent-encoding，任意普通串应能还原。
test('deeplink round-trips ordinary values', () => {
  const dl = dlSandbox();
  for (const v of ['abc', '中文关键字', 'a b', 'x=y&z', '100%', '日本語']) {
    const enc = dl.dlEncode({ q: v });
    const back = dl.dlDecode('?' + enc);
    assert.equal(back.q, v, v);
  }
});

// ★ ④ 恢复必须在取列表之前（否则列表会闪一下，且多打一次请求）。
test('deeplink restores before first refresh', () => {
  const restore = tokens.indexOf('dlRestore();');
  const firstRefresh = tokens.indexOf('\nrefresh();');
  assert.ok(restore > 0, 'dlRestore 存在');
  assert.ok(firstRefresh > 0, 'refresh() 存在');
  assert.ok(restore < firstRefresh,
    '★ dlRestore() 必须在 refresh() 之前 —— 反过来会先拉一遍全量、再拉一遍筛选后的');
});

// ★ ⑤ 输入时就写回地址栏（不能等到防抖结束）。
test('deeplink syncs on input immediately', () => {
  const inputAt = tokens.indexOf("$('kw').addEventListener('input'");
  assert.ok(inputAt > 0, 'kw 的 input 监听存在');
  const block = tokens.slice(inputAt, inputAt + 700);
  const syncAt = block.indexOf('dlSync(');
  const timerAt = block.indexOf('window.setTimeout');
  assert.ok(syncAt > 0, '★ 输入时必须调用 dlSync');
  assert.ok(timerAt < 0 || syncAt < timerAt,
    '★ dlSync 必须在 setTimeout 之前 —— 否则"刚输完就复制地址"拿到旧值');
});

// ⑥ 无凭据进 URL 的路径（判据是"编码函数不碰除 q 之外的任何键"，已在 ③ 覆盖；
//    这里再钉一次：整页不许把 token/key 写进 URL）。
test('deeplink never puts credentials in the URL', () => {
  const src = dlSource();
  for (const bad of ['token', 'key', 'api_key', 'admin_token', 'sk-']) {
    assert.ok(!src.includes("'" + bad + "'"),
      '★ 深链助手里出现了凭据相关的键名 ' + bad + ' —— URL 会进历史/Referer/聊天记录');
  }
});

// ★ ⑦ 页面内联脚本必须语法正确（改的是 121 KB 的大文件，这条守卫最值钱）。
test('tokens page inline scripts parse', () => {
  const re = /<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi;
  let n = 0;
  let m;
  while ((m = re.exec(tokens))) {
    n++;
    try { new vm.Script(m[1]); }
    catch (e) { throw new Error('第 ' + n + ' 个内联脚本语法错误: ' + e.message); }
  }
  assert.ok(n >= 1, '至少应有一个内联脚本块');
});
Promise.all(pending).then(() => {
  if (failures) { console.error(failures + ' FAILED'); process.exit(1); }
  console.log('all deeplink tests passed');
});
