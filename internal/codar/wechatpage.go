package codar

// wechatpage.go —— 微信绑定扫码页（T-364）。
//
// 设计照 invite_page.go 的 RenderInvitePage / InvitePageSpec：**纯数据进、HTML 出**，
// 无 IO、无状态 ⇒ 可单测。
//
// # 三条硬约束（都是踩过坑才写下来的）
//
//  1. **相对路径**：页面里的 fetch 一律用 /admin/codar/wechat/* （同源相对路径）
//     ⇒ 部署到服务器（任意 host）都能用，不许写死 127.0.0.1 / localhost。
//  2. **轮询在浏览器侧**：每 1000ms 打一次 /wechat/status；后端**不起任何后台 job**
//    （否则一个页面被打开 N 次就会有 N 份轮询）。
//  3. **不用 Go 原始字符串里的反引号**：本文件整体是一个 Go 原始字符串字面量，
//     正文里出现反引号会直接截断字符串。所以页面 JS 里拼接一律用 + 与单引号。
//
// # 状态机（与 wechat.go 的 normalizeWechatStatus 对齐）
//
//	pending → 「请使用微信扫码绑定」；SCANNED → 「已扫码，等待确认」；
//	SUCCESS → 「微信绑定成功」（停止轮询）；FAILED → 「微信绑定失败：<message>」；
//	EXPIRED → 「二维码已过期，正在刷新」并重取（自动重取上限 3 次，之后提示手动刷新）。

import (
	"strconv"
	"strings"
)

// WechatPageSpec 渲染扫码页所需的全部数据。
type WechatPageSpec struct {
	UID string
	// Bound 已知时表示"已绑定"；Known=false 表示这次没查到（页面照常给二维码让人自己试）。
	Bound bool
	Known bool
	// Error 取状态时的错误（展示给用户，页面仍然可用）。
	Error string
}

// RenderWechatPage 渲染扫码页 HTML（纯函数）。
func RenderWechatPage(spec WechatPageSpec) string {
	var b strings.Builder
	uid := spec.UID
	if uid == "" {
		uid = "(未指定账号)"
	}
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>绑定微信 · `)
	b.WriteString(esc(uid))
	b.WriteString(`</title>
<style>
:root{--cream:#FFF8F0;--ink:#120F0D;--line:#E8DCCB;--acc:#07C160;--dim:#6B5A4A}
*{box-sizing:border-box}
body{margin:0;padding:18px;background:var(--cream);color:var(--ink);
     font:14px/1.6 system-ui,-apple-system,'Segoe UI',Roboto,'PingFang SC','Microsoft YaHei',sans-serif}
h2{margin:0 0 6px;font-size:16px}
p.note{margin:0 0 14px;color:var(--dim);font-size:12px}
#qrbox{width:240px;height:240px;margin:0 auto 14px;background:#fff;border:1px solid var(--line);
       border-radius:12px;display:flex;align-items:center;justify-content:center;overflow:hidden}
#qrimg{width:100%;height:100%;object-fit:contain;display:none}
#qrloading{color:var(--dim);font-size:12px}
#status{text-align:center;font-weight:600;margin:0 0 14px}
#status.ok{color:var(--acc)}
#status.bad{color:#C0392B}
#err{color:#C0392B;font-size:12px;text-align:center;margin:0 0 10px;white-space:pre-wrap}
.row{display:flex;justify-content:center}
button{padding:9px 16px;border:1px solid var(--line);border-radius:8px;background:#fff;
       color:var(--ink);font-weight:600;cursor:pointer}
button:disabled{opacity:.5;cursor:default}
</style></head><body>
<h2>绑定微信 · `)
	b.WriteString(esc(uid))
	b.WriteString(`</h2>
<p class="note">通过邀请码注册的用户，绑定微信后可领取 2,000 点数邀请奖励；被邀请者需绑定微信，手机号与微信绑定后邀请点数会自动补齐。</p>
`)
	if spec.Bound {
		// 已绑定：不渲染二维码，也不轮询
		b.WriteString(`<p id="status" role="status" class="ok">本账号已绑定微信 ✓</p>
`)
	} else {
		if spec.Error != "" {
			b.WriteString(`<p id="err">` + esc(spec.Error) + `</p>
`)
		}
		b.WriteString(`<div id="qrbox"><span id="qrloading">正在加载二维码</span><img id="qrimg" alt="微信绑定二维码"></div>
<p id="status" role="status">请使用微信扫码绑定</p>
<div class="row"><button id="refresh" type="button">刷新二维码</button></div>
`)
	}
	// 已绑定：**整段脚本都不发**（不轮询、不取码）—— 契约原文「已绑定的账号…不轮询」。
	// 顺带好处：已绑定页面完全静态，被任何缓存都不影响正确性。
	if !spec.Bound {
		b.WriteString(`<script>
(function(){
  var WECHAT_QR = '/admin/codar/wechat/qr';
  var WECHAT_STATUS = '/admin/codar/wechat/status';
  var POLL_MS = 1000;
  var QR_TTL_MS = `)
		b.WriteString(strconv.Itoa(wechatQRTTLSeconds * 1000))
		b.WriteString(`;
  var MAX_AUTO_REFRESH = 3;
  var uid = `)
		b.WriteString(jsStr(spec.UID))
		b.WriteString(`;
  var bound = `)
		if spec.Bound {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		b.WriteString(`;

  var elStatus = document.getElementById('status');
  var elImg = document.getElementById('qrimg');
  var elLoading = document.getElementById('qrloading');
  var elRefresh = document.getElementById('refresh');
  var scene = '';
  var timer = null;
  var ttlTimer = null;
  var autoRefreshes = 0;

  function say(text, cls){
    if(!elStatus) return;
    elStatus.textContent = text;
    elStatus.className = cls || '';
  }
  function stopPolling(){
    if(timer){ clearInterval(timer); timer = null; }
    if(ttlTimer){ clearTimeout(ttlTimer); ttlTimer = null; }
  }
  function loadQR(){
    if(!elImg) return;
    say('请使用微信扫码绑定');
    fetch(WECHAT_QR + '?uid=' + encodeURIComponent(uid), { headers: { 'Accept': 'application/json' } })
      .then(function(r){ return r.json().then(function(j){ return { s: r.status, j: j }; }); })
      .then(function(res){
        var j = res.j || {};
        if(!j.ok){ say('二维码加载失败：' + (j.error || ('HTTP ' + res.s)), 'bad'); return; }
        if(j.bound){ say('本账号已绑定微信 ✓', 'ok'); stopPolling(); return; }
        scene = j.scene || '';
        if(!scene){ say('二维码数据不完整', 'bad'); return; }
        elImg.src = j.qr_url;
        elImg.style.display = 'block';
        if(elLoading) elLoading.style.display = 'none';
        startPolling();
        if(ttlTimer) clearTimeout(ttlTimer);
        ttlTimer = setTimeout(function(){ autoRefresh('二维码到期'); }, QR_TTL_MS);
      })
      .catch(function(e){ say('二维码加载失败：' + e, 'bad'); });
  }
  function autoRefresh(why){
    if(autoRefreshes >= MAX_AUTO_REFRESH){
      say('二维码已过期，请点「刷新二维码」', 'bad');
      stopPolling();
      return;
    }
    autoRefreshes++;
    say('二维码已过期，正在刷新…');
    loadQR();
  }
  function startPolling(){
    if(!scene) return;
    stopPolling();
    timer = setInterval(function(){
      fetch(WECHAT_STATUS + '?scene=' + encodeURIComponent(scene), { headers: { 'Accept': 'application/json' } })
        .then(function(r){ return r.json().then(function(j){ return { s: r.status, j: j }; }); })
        .then(function(res){
          var j = res.j || {};
          if(!j.ok){ return; }
          var st = j.status;
          if(st === 'SUCCESS'){ say('微信绑定成功', 'ok'); stopPolling(); return; }
          if(st === 'SCANNED'){ say('已扫码，等待确认'); return; }
          if(st === 'FAILED'){ say('微信绑定失败：' + (j.message || '未知原因'), 'bad'); stopPolling(); return; }
          if(st === 'EXPIRED' || j.expired){ autoRefresh('场景失效'); }
        })
        .catch(function(){ /* 网络抖动：下一次轮询继续 */ });
    }, POLL_MS);
  }
  if(elRefresh){
    elRefresh.addEventListener('click', function(){ autoRefreshes = 0; loadQR(); });
  }
  if(!bound && elImg){ loadQR(); }
})();
</script></body></html>`)
	}
	return b.String()
}
