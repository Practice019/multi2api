
const fs = require('fs');
const html = fs.readFileSync((process.env.WB2API_REPO || __dirname + '/../..') + '/internal/server/webui.html', 'utf8');
const esc = s => String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
const els = {};
const $ = id => els[id] || (els[id] = { id, innerHTML: '', textContent: '' });

function normalizeDesc(items) {
    return (items || []).slice().sort((a, b) => {
      const ta = new Date(a.at).getTime(), tb = new Date(b.at).getTime();
      if (ta !== tb) return tb - ta;
      return (b.seq || 0) - (a.seq || 0);
    });
  }

function tokPerSec(tokens, totalMs) {
    if (tokens < 0) return '-';
    if (!totalMs || totalMs <= 0) return '0.0';
    return (tokens / (totalMs / 1000)).toFixed(1);
  }

function renderLogs(items) {
    const tb = $('logrows');
    const list = normalizeDesc(items).slice(0, 300);
    if (!list.length) return;
    tb.innerHTML = list.map(e => {
      const st = e.status >= 200 && e.status < 300 ? 'ok' : e.status >= 400 ? 'err' : 'warn';
      return `<tr>
        <td class="mono dim">${e.seq}</td>
        <td class="dim">${new Date(e.at).toLocaleTimeString('zh-CN', { hour12: false })}</td>
        <td class="mono">${esc(e.model)}</td>
        <td>${esc(e.mode)}</td>
        <td><span class="pill ${st}">${e.status}</span></td>
        <td class="mono dim">${esc((e.uid || '').slice(0, 8) || '-')}</td>
        <td class="mono">${e.ttfb_ms ? e.ttfb_ms + 'ms' : '-'}</td>
        <td class="mono">${e.tokens >= 0 ? e.tokens : '-'}</td>
        <td class="mono">${tokPerSec(e.tokens, e.total_ms)}</td>
        <td class="mono">${e.total_ms}ms</td>
      </tr>`;
    }).join('');
  }

let fail = 0;
const ok = (c, m) => { console.log((c ? '  PASS ' : '  FAIL ') + m); if (!c) fail++; };

function renderOne(entry) {
  $('logrows').innerHTML = '';
  renderLogs([Object.assign({ seq: 1, at: new Date().toISOString(), model: 'm', mode: 'stream', status: 200, uid: 'u', ttfb_ms: 10 }, entry)]);
  return $('logrows').innerHTML;
}

// 取出最后一行的所有单元格文本，便于按下标断言列顺序
function cells(h) {
  const tr = /<tr>([\s\S]*?)<\/tr>/.exec(h);
  if (!tr) throw new Error('没有渲染出 <tr>');
  return (tr[1].match(/<td[^>]*>[\s\S]*?<\/td>/g) || []).map(td =>
    td.replace(/<[^>]+>/g, '').trim());
}

console.log('\n[1] tok/s 基本计算');
{
  const c = cells(renderOne({ tokens: 100, total_ms: 1000 }));
  // 列顺序：# 时间 模型 模式 状态 UID TTFB tok tok/s 耗时
  ok(c.length === 10, '渲染出 10 列（新增 tok/s），实际 ' + c.length);
  ok(c[8] === '100.0', '100 tok / 1.0s → 100.0，实际 ' + c[8]);
  ok(c[7] === '100', 'tok 列仍是原始 token 数');
  ok(c[9] === '1000ms', '耗时列未被挤掉');
}

console.log('\n[2] 边界：usage 缺失（tokens = -1）不能拿去算速率');
{
  const c = cells(renderOne({ tokens: -1, total_ms: 1000 }));
  ok(c[7] === '-', 'tok 列显示 -，实际 ' + c[7]);
  ok(c[8] === '-', 'tok/s 列同样显示 -（不出现 -1.0 这种荒谬值），实际 ' + c[8]);
}

console.log('\n[3] 边界：total_ms = 0 不能除零');
{
  const c = cells(renderOne({ tokens: 50, total_ms: 0 }));
  ok(c[8] === '0.0', '耗时 0 时显示 0.0（不是 Inf/NaN），实际 ' + c[8]);
}

console.log('\n[4] 小数位与四舍五入');
{
  ok(cells(renderOne({ tokens: 68, total_ms: 2600 }))[8] === '26.2', '68 tok / 2.6s → 26.2');
  ok(cells(renderOne({ tokens: 365, total_ms: 4200 }))[8] === '86.9', '365 tok / 4.2s → 86.9');
  ok(cells(renderOne({ tokens: 1, total_ms: 3000 }))[8] === '0.3', '1 tok / 3.0s → 0.3');
}

console.log('\n[5] tokens = 0 是合法值（与 -1 语义不同）');
{
  const c = cells(renderOne({ tokens: 0, total_ms: 2000 }));
  ok(c[7] === '0', 'tok 列显示 0');
  ok(c[8] === '0.0', 'tok/s 显示 0.0（不是 -）');
}

console.log('\n[6] 静态校验：表头与 colspan 同步更新');
{
  ok(html.includes('<th>tok/s</th>'), '表头含 tok/s 列');
  const bad = (html.match(/colspan="9"[^>]*>\s*(?:暂无请求|实时视图中|正在读取落盘|未启用落盘|落盘日志为空|已清屏)/g) || []);
  ok(bad.length === 0, '日志表的占位行 colspan 不应再是 9（实际残留 ' + bad.length + ' 处）');

  // # 断言的是"不变式"，不是某个字面量（T5 改造后修正）
  //
  // 上一版断言 html 里存在字面量 'colspan="10"'。T5 把日志表的 colspan
  // 改成由 COLS.logs 推导（因为写死数字正是 B1/B2 那类漂移的根因），
  // 字面量随之消失 —— 那条断言就红了一个"其实是改进"的改动。
  //
  // 现在断言真正的性质：日志表列数由常量定义、且占位行走统一的
  // rowPlaceholder（不再有散落的写死数字）。
  ok(/const\s+COLS\s*=\s*\{[^}]*logs\s*:\s*10/.test(html), '日志表列数由 COLS.logs = 10 定义');
  ok(/function\s+logPlaceholder\s*\(/.test(html), '日志占位行走 logPlaceholder');
  ok(/rowPlaceholder\(COLS\.logs/.test(html), 'logPlaceholder 由 rowPlaceholder + COLS.logs 推导 colspan');

  // 反向验证：日志表里不应再出现写死的 colspan 数字，
  // 而应**走占位符**（服务端在 webui.go 替换为数字）。
  //
  // 注意：这里的 html 变量是**源文件**（含占位符），不是服务端替换后的输出。
  // 所以本套件只能断言"源码里是占位符"；"替换后是数字"由
  // verify_hist3.js 的 HTTP 检查负责（它读 /ui 的真实响应）。
  // logrows 在 jobrows 之后（DOM 顺序），所以 slice 起点用 logrows、终点用下一个 section 的开头。
  const logStart = html.indexOf('id="logrows"');
  const logEnd = html.indexOf('id="jobrows"');
  const logSection = logEnd > logStart ? html.slice(logStart, logEnd) : html.slice(logStart);
  ok(logSection.includes('__COLS_LOGS__'),
    '日志表静态 colspan 用 __COLS_LOGS__ 占位符（由 webui.go 在服务端替换为数字）');
  const literalNum = (logSection.match(/colspan="d+"/g) || []);
  ok(literalNum.length === 0,
    '日志表源码里没有硬编码的数字 colspan（实际 ' + literalNum.length + ' 处）');
}

console.log(fail === 0 ? '\n=== 全部通过 ===' : '\n=== ' + fail + ' 项失败 ===');
process.exit(fail ? 1 : 0);
