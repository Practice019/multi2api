// verify_delete_flow_e2e.js —— 「删除」这条链的真实例 e2e（2026-10-19）。
//
// # 为什么要这个探针
//
// 用户报：**导入之后删除不掉**（截图里两行"已禁用"的 loomy 账号，点「移除」没反应），
// 并要求「检查一下所有的删除按钮」，且**不许用真实数据测试**。
//
// 全应用里只有**两个**销毁性按钮（grep 全量枚举，判据是"发出去的请求会删东西"）：
//
//   ① 账号池 · 移除  → DELETE /admin/accounts/{uid}?purge_file=1
//   ② API 管理 · 删除 → POST   /admin/apikeys/delete  {id}
//
// （另外几个看着像删除的都不是：`enable/disable` 只改状态、`cooldown/clear` 只清冷却、
//   `apikeys/reset` 是换 key 不删条目、`akSelNone` 那个"清空"是清**选择**。）
//
// # 数据来源：全合成，零真实数据
//
// lib_instance 起隔离实例（cwd=临时目录 ⇒ ./auths、./data 都落在临时目录里），
// 凭证当场编（session 全 a、uid 9900000000000000NN）。**不读**仓库的 auths/、
// 也不读 auths-test/。
//
// # 步骤**逐字照前端**的顺序（这是关键）
//
// 前端 openImportModal（webui.html:5575）做的是两件事：
//
//	① POST /admin/accounts/import  {provider, data}
//	② POST /admin/accounts/reload  {provider}      ← 漏掉它就会把"池子没刷"
//	                                                误判成"删除接口坏了"
//
// 所以探针必须发这两条，再删。每一类都查四件事：
//
//	HTTP 回执 / 池里还在不在 / **凭证文件是否真的没了** / 重载后**会不会复活**
//
// ⚠ 最后两件才是用户真正的判据。"删除不掉"有两种形态，只测第一种会漏第二种：
//   ① 点了没反应或报错（回执非 2xx）
//   ② 看起来删了，文件还在 ⇒ 重载/重启又装回池子（本仓历史上踩过的"复活"坑）
'use strict';
const fs = require('fs');
const path = require('path');
const { startInstance, httpJson } = require('./lib_instance.js');

const PORT = Number(process.env.DEL_PORT || 7881);
const KEY = 'del-probe-' + PORT;
const S = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; // 合成 session（32 hex）
const U_DISK = '990000000000000001';          // A：直接落盘
const U_IMP = '990000000000000002';           // B：走导入端点
const U_OFF = '990000000000000003';           // C：先禁用再删

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };
const info = (m) => console.log('  · ' + m);
const cred = (uid) => JSON.stringify({ session: S, userid: uid });
const files = (tmp) => { try { return fs.readdirSync(path.join(tmp, 'auths', 'loomy')).sort(); } catch { return []; } };

(async () => {
  let inst = null;
  try {
    inst = await startInstance({
      port: PORT, apiKey: KEY,
      auths: { 'loomy/loomy-probe-disk1.json': cred(U_DISK), 'loomy/loomy-probe-disk2.json': cred(U_OFF) },
      config: { loomy: { enabled: true, auth_dir: './auths/loomy' } },
    });
    const base = inst.base, tok = { token: KEY };
    // 统一的"看池子里有谁"
    const pool = async () => {
      const r = await httpJson(base, '/admin/accounts', tok);
      return ((r.json && r.json.accounts) || []).map((x) => x.uid);
    };
    const reload = (pid) => httpJson(base, '/admin/accounts/reload', { method: 'POST', token: KEY, body: pid ? { provider: pid } : {} });

    console.log('实例已起: ' + base + '（pid=' + inst.pid + '，cwd=' + inst.tmp + '）');
    info('初始 auths/loomy: [' + files(inst.tmp).join(', ') + ']');
    info('初始池子: [' + (await pool()).join(', ') + ']\n');

    // ---------------------------------------------------------------- B 导入
    console.log('=== B 逐字照前端：import → reload{provider} → 看池子 ===');
    const imp = await httpJson(base, '/admin/accounts/import', { method: 'POST', token: KEY, body: { provider: 'loomy', data: cred(U_IMP) } });
    info('import → ' + imp.status + '  ' + imp.text.slice(0, 260));
    info('导入后 auths/loomy: [' + files(inst.tmp).join(', ') + ']');
    const rl1 = await reload('loomy');
    info('reload{provider:loomy} → ' + rl1.status + '  ' + rl1.text.slice(0, 260));
    const p1 = await pool();
    info('reload 后池子: [' + p1.join(', ') + ']');
    ok(p1.indexOf(U_IMP) >= 0, '导入的账号进了池子（否则前端点「移除」必然 404）');
    if (p1.indexOf(U_IMP) < 0) {
      console.log('  ⚠⚠ 这一条就是"删除不掉"的根因：账号在**界面上看得到**、却不在池子里。');
      console.log('      reload 的回执已打在上面 —— 看 provider/dir/scanned 三个字段。');
      const noPid = await reload();
      info('（对照）reload 不带 provider → ' + noPid.status + '  ' + noPid.text.slice(0, 200));
    }

    // ---------------------------------------------------------------- 删导入的那个
    console.log('\n=== ① 删除 B（导入进来的）uid=' + U_IMP + ' ===');
    const b1 = files(inst.tmp);
    const d1 = await httpJson(base, '/admin/accounts/' + U_IMP + '?purge_file=1', { method: 'DELETE', token: KEY });
    info('DELETE → ' + d1.status + '  ' + d1.text.slice(0, 260));
    ok(d1.status === 200, '删除回执 200');
    ok(files(inst.tmp).length === b1.length - 1, '凭证文件真的少了 1 个');
    ok(!!(d1.json && d1.json.purged_file), 'purged_file 非空（空 ⇒ 文件还在 ⇒ 会复活）');
    info('删除后池子: [' + (await pool()).join(', ') + ']');

    // ---------------------------------------------------------------- 复活检查
    console.log('\n=== ② 重载 / 重启后会不会复活（"删除不掉"的第②种形态）===');
    const rl2 = await reload('loomy');
    info('reload{loomy} → ' + rl2.status + '  ' + rl2.text.slice(0, 200));
    const p2 = await pool();
    ok(p2.indexOf(U_IMP) < 0, '重载后已删的号没有复活');

    // ---------------------------------------------------------------- 删直接落盘的
    console.log('\n=== ③ 删除 A（直接落盘、未禁用，用来对照 B）uid=' + U_DISK + ' ===');
    const b2 = files(inst.tmp);
    const rowA = (await httpJson(base, '/admin/accounts', tok)).json;
    info('删前这一行的字段: ' + JSON.stringify(((rowA && rowA.accounts) || []).find((x) => x.uid === U_DISK) || null));
    const d2 = await httpJson(base, '/admin/accounts/' + U_DISK + '?purge_file=1', { method: 'DELETE', token: KEY });
    info('DELETE → ' + d2.status + '  ' + d2.text.slice(0, 260));
    ok(d2.status === 200, '删除回执 200');
    ok(files(inst.tmp).length === b2.length - 1, '凭证文件真的少了 1 个');
    ok(!!(d2.json && d2.json.purged_file), 'purged_file 非空（空 ⇒ FilePath 丢了 ⇒ 重启复活）');
    const p3 = await pool();
    ok(p3.indexOf(U_DISK) < 0, '删完这个号立刻从池里消失');
    const rl3 = await reload('loomy');
    info('reload{loomy} 后 → ' + rl3.status + '  ' + rl3.text.slice(0, 160));
    const p4 = await pool();
    ok(p4.indexOf(U_DISK) < 0, '★ 重载后**没有复活**（文件真被删掉了才算过）');
    if (p4.indexOf(U_DISK) >= 0) info('⚠ 复活了 —— 凭证文件还在盘上，重启就会把它装回来');

    // ---------------------------------------------------------------- 已禁用形态
    console.log('\n=== ④ 已禁用形态（截图里那两行都是"已禁用"）uid=' + U_OFF + ' ===');
    const dis = await httpJson(base, '/admin/accounts/' + U_OFF + '/disable', { method: 'POST', token: KEY, body: {} });
    info('disable → ' + dis.status + '  ' + dis.text.slice(0, 160));
    const b3 = files(inst.tmp);
    const d3 = await httpJson(base, '/admin/accounts/' + U_OFF + '?purge_file=1', { method: 'DELETE', token: KEY });
    info('DELETE → ' + d3.status + '  ' + d3.text.slice(0, 260));
    ok(d3.status === 200, '已禁用的账号也能删（截图里就是这种）');
    ok(files(inst.tmp).length === b3.length - 1, '凭证文件真的少了 1 个');
    ok(!!(d3.json && d3.json.purged_file), 'purged_file 非空');

    // ---------------------------------------------------------------- 另一个删除按钮
    console.log('\n=== ⑤ API 管理 · 删除（POST /admin/apikeys/delete）===');
    const c1 = await httpJson(base, '/admin/apikeys', { method: 'POST', token: KEY, body: { name: 'probe-key-1' } });
    info('建 key → ' + c1.status);
    let lst = await httpJson(base, '/admin/apikeys', tok);
    let keys = (lst.json && (lst.json.keys || lst.json.apikeys || lst.json.items)) || [];
    const kid = keys.length ? keys[0].id : '';
    ok(!!kid, '拿到刚建的 key id');
    if (kid) {
      const del = await httpJson(base, '/admin/apikeys/delete', { method: 'POST', token: KEY, body: { id: kid } });
      info('delete → ' + del.status + '  ' + del.text.slice(0, 200));
      ok(del.status === 200, 'apikey 删除回执 200');
      lst = await httpJson(base, '/admin/apikeys', tok);
      keys = (lst.json && (lst.json.keys || lst.json.apikeys || lst.json.items)) || [];
      ok(keys.length === 0, '列表里已经没有这条 key');
    }

    console.log('\n=== 收尾：server.log 里与删除相关的行 ===');
    console.log(inst.readLog().split(/\r?\n/).filter((l) => /delete|remove|purge|reload|import/i.test(l)).slice(-16).map((l) => '    ' + l).join('\n'));
  } catch (e) {
    console.log('  探针异常: ' + (e && e.stack ? e.stack : e));
    fail++;
  } finally {
    if (inst) inst.stop();
  }
  console.log('\n' + (fail ? '结果: ' + fail + ' 条 FAIL' : '结果: 全绿'));
  process.exit(fail ? 1 : 0);
})();
