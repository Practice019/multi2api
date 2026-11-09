package codar

import (
	"encoding/json"
	"net/http"
	"strings"
)

// 邀请码页面（T-354）。
//
// # 为什么必须是一个「页面」
//
// BindInvite 要阿里云 4.0 的四个 captcha 字段（lot_number / captcha_output / pass_token / gen_time），
// 那是**浏览器里官方组件**才能产出的东西 —— 后端造不出来 ⇒ 绑定表单只能落在页面上
// （与 T-324/T-342 的登录页同一个理由）。
//
// # captcha 组件的来源
//
// 与 internal/loginui 的登录页**同一套**：同一个 ct4.js、同一个 captchaId、
// 同一种初始化（initAlicom4 两参数把实例存进 window.obj）、同一个 pickCaptcha() 取四字段。
// ⚠ 这里是**复制**而不是 import：loginui 的那段 JS 嵌在它 365 行的页面模板里，
//
//	抽取会动到已经验证过的登录路径（T-342/T-343 的红绿都在那条链上）。
//	复制的那几行已在下方注明来源；将来若要做共享抽取，请连同 loginui 的测试一起改。
//
// # 数据驱动
//
// 页面只认「自己的 uid + 上游给的码/状态」，**不认上游名** —— 换一个上游只要能提供
// 同样的 key（invite_code / invited），这套页面照样能用（与 webui 的判据一致）。
const invitePagePath = "/admin/codar/invite-page"

// InvitePageSpec 渲染邀请页所需的全部数据（纯数据 ⇒ 渲染函数可单测）。
type InvitePageSpec struct {
	UID string
	// Code 预填的邀请码（?code= 传入；用户在列表页点「绑定」时带过来）。
	Code           string
	InviteCode     string
	InviteLink     string
	Invited        bool
	InvitedMessage string
	Records        []map[string]any
	CaptchaID      string
	CaptchaJSURL   string
}

// attrEsc HTML **属性值**转义：esc 只挡 & < >，而属性值还必须挡引号，
// 否则 ?code= 里塞一个引号就能跳出 value="" 改写标签（本页面是**同源**的，注入风险真实）。
func attrEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;").Replace(s)
}

// RenderInvitePage 渲染邀请页 HTML（纯函数：无 IO、无状态）。
func RenderInvitePage(spec InvitePageSpec) string {
	if spec.CaptchaJSURL == "" {
		spec.CaptchaJSURL = CaptchaJSURL
	}
	if spec.CaptchaID == "" {
		spec.CaptchaID = DefaultCaptchaID
	}
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<title>邀请码</title>
<script src="`)
	b.WriteString(esc(spec.CaptchaJSURL))
	b.WriteString(`"></script>
<style>
:root{--cream:#FFF8F0;--pink:#FF4D9D;--ink:#120F0D;--line:#E8DCCB}
*{box-sizing:border-box}
body{margin:0;padding:18px;background:var(--cream);color:var(--ink);
     font-family:"Arial Black",Arial,sans-serif;font-size:13px}
.card{background:#fff;border:1px solid var(--line);border-radius:8px;padding:14px;margin-bottom:12px}
h3{margin:0 0 10px;font-size:14px;letter-spacing:.5px}
code{font-family:ui-monospace,Consolas,monospace;background:#FFF1F7;padding:2px 6px;border-radius:4px}
a{color:var(--pink)}
input{font-family:ui-monospace,Consolas,monospace;padding:7px 9px;border:1px solid var(--line);
      border-radius:6px;background:#fff;color:var(--ink);min-width:200px}
button{font-family:"Arial Black",Arial,sans-serif;padding:7px 14px;border:1px solid var(--ink);
       border-radius:6px;background:var(--pink);color:#fff;cursor:pointer}
button.ghost{background:#fff;color:var(--ink)}
button:disabled{opacity:.5;cursor:not-allowed}
#msg{margin-top:9px;min-height:18px;font-family:ui-monospace,Consolas,monospace}
#msg.bad{color:#B00020}#msg.good{color:#0B7A33}
table{border-collapse:collapse;width:100%}td,th{border-bottom:1px solid var(--line);padding:5px 6px;text-align:left}
</style></head><body>
`)
	// ① 我的邀请码
	b.WriteString(`<div class="card"><h3>我的邀请码</h3><div>邀请码：<code id="mycode">`)
	b.WriteString(esc(spec.InviteCode))
	b.WriteString(`</code></div><div style="margin-top:6px">邀请链接：<a id="mylink" href="`)
	b.WriteString(esc(spec.InviteLink))
	b.WriteString(`" target="_blank" rel="noreferrer">`)
	b.WriteString(esc(spec.InviteLink))
	b.WriteString(`</a></div></div>`)
	// ② 邀请记录
	b.WriteString(`<div class="card"><h3>我邀请的人</h3>`)
	if len(spec.Records) == 0 {
		b.WriteString(`<div>还没有邀请记录。把上面的链接给朋友，对方注册后你就会得到奖励。</div>`)
	} else {
		b.WriteString(`<table><tr><th>用户</th><th>注册时间</th></tr>`)
		for _, r := range spec.Records {
			b.WriteString(`<tr><td>` + esc(str(r["username"])) + `</td><td>` + esc(str(r["registration_date"])) + `</td></tr>`)
		}
		b.WriteString(`</table>`)
	}
	b.WriteString(`</div>`)
	// ③ 绑定（已被邀请则隐藏 —— 每个号只能被邀请一次）
	if spec.Invited {
		b.WriteString(`<div class="card"><h3>绑定别人的邀请码</h3><div>你<b>已被邀请</b>（`)
		b.WriteString(esc(spec.InvitedMessage))
		b.WriteString(`）—— 每个账号只能被邀请一次，这里不再提供绑定。</div></div>`)
	} else {
		b.WriteString(`<div class="card"><h3>绑定别人的邀请码</h3>
<div>填对方的邀请码，先完成人工验证，再点绑定。官方说明：老用户首次绑定邀请码并完成微信绑定后同样计入邀请人进度。</div>
<div style="margin-top:9px;display:flex;gap:8px;align-items:center;flex-wrap:wrap">
  <input id="invite-code" value="` + attrEsc(spec.Code) + `" placeholder="对方的邀请码，如 BB5VYS" autocomplete="off">
  <div id="capbox" class="capbox"><button id="captcha-trigger" class="captcha-trigger" type="button">开始人工验证</button></div>
  <button id="bind">绑定</button>
</div>
<div id="msg"></div></div>`)
	}
	// ④ 脚本
	b.WriteString(`<script>
var UID_TEXT = `)
	b.WriteString(jsStr(spec.UID))
	b.WriteString(`, CAPTCHA_ID = `)
	b.WriteString(jsStr(spec.CaptchaID))
	b.WriteString(`, INVITED = `)
	if spec.Invited {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString(`;
function $(id){ return document.getElementById(id); }
function show(t, ok){ var m = $('msg'); if(!m) return; m.textContent = t; m.className = ok ? 'good' : 'bad'; }
// ⚠ 与 internal/loginui 的登录页同一套取字段逻辑（那边是组件实测过的写法，见 loginui.go 的 captcha 段）。
function pickCaptcha(){
  var o = window.obj || {};
  try {
    var v = (o.getValidate && o.getValidate()) || {};
    return { lot_number: v.lotNumber || v.lot_number || '', captcha_output: v.captchaOutput || v.captcha_output || '',
             pass_token: v.passToken || v.pass_token || '', gen_time: v.genTime || v.gen_time || '' };
  } catch (e) { return {}; }
}
function missingCap(c){
  var need = ['lot_number','captcha_output','pass_token','gen_time'], miss = [];
  for (var i=0;i<need.length;i++){ if(!c || !c[need[i]]) miss.push(need[i]); }
  return miss;
}
if (typeof window.initAlicom4 === 'function') {
  var capOpts = { captchaId: CAPTCHA_ID, product: 'bind', width: '300px',
    onSuccess: function(){ show('人工验证通过', true); },
    onError: function(){ show('人工验证未通过，请重试', false); } };
  try { window.initAlicom4(capOpts, function(cfg){ window.obj = cfg || window.obj; }); }
  catch (e2) { try { window.initAlicom4(capOpts); } catch (e3) { show('初始化人工验证失败：' + e3, false); } }
}
if ($('captcha-trigger')) {
  $('captcha-trigger').addEventListener('click', function(){
    var o = window.obj || {};
    var opened = false;
    try { if (typeof o.showBox === 'function') { o.showBox(); opened = true; } } catch (e) {}
    if (!opened) { try { if (typeof o.popup === 'function') { o.popup(); opened = true; } } catch (e) {} }
    if (!opened) { try { if (typeof o.show === 'function') { o.show(); opened = true; } } catch (e) {} }
    if (!opened) show('请刷新本页再试；若仍不行，请在新标签打开本页完成验证。', false);
  });
}
if ($('bind')) {
  $('bind').addEventListener('click', function(){
    var btn = $('bind');
    var code = ($('invite-code').value || '').trim();
    if (!code) { show('请先填邀请码', false); return; }
    var c = pickCaptcha() || {};
    var miss = missingCap(c);
    if (miss.length) { show('请先完成上方的人工验证（缺少 ' + miss.join('、') + '）', false); return; }
    btn.disabled = true;
    fetch('` + invitePagePath + `/bind', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ uid: UID_TEXT, invite_code: code,
        lot_number: c.lot_number, captcha_output: c.captcha_output,
        pass_token: c.pass_token, gen_time: c.gen_time }) })
      .then(function(r){ return r.json().then(function(j){ return { s: r.status, j: j }; }); })
      .then(function(o){
        btn.disabled = false;
        if (o.j && o.j.ok) { show('绑定成功：' + (o.j.message || '') + '，本页即将刷新', true); setTimeout(function(){ location.reload(); }, 1200); }
        else { show((o.j && (o.j.error || o.j.message)) || ('绑定失败（HTTP ' + o.s + '）'), false); }
      })
      .catch(function(e){ btn.disabled = false; show('绑定请求失败：' + e, false); });
  });
}
</script></body></html>`)
	return b.String()
}

// handleInvitePage GET /admin/codar/invite-page?uid=
//
// 渲染邀请页（自己的码 + 记录 + 绑定表单）。页面自己再调一次 /admin/codar/invite
// 拿不到就用查询参数里的 uid 直查，避免"页面打开时数据是旧的"。
func (p *Provider) handleInvitePage(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	spec := InvitePageSpec{
		UID: uid, Code: strings.TrimSpace(r.URL.Query().Get("code")),
		CaptchaJSURL: CaptchaJSURL, CaptchaID: DefaultCaptchaID,
	}
	if a, err := p.pickAuth(uid); err == nil {
		ctx := r.Context()
		var info struct {
			Data struct {
				InviteCode string `json:"invite_code"`
				InviteLink string `json:"invite_link"`
			} `json:"data"`
		}
		if raw, _, cerr := p.callJSON(ctx, http.MethodPost, p.payBase+"/v1/promotion/info", a,
			map[string]any{"username": a.UID, "token": 0}); cerr == nil {
			_ = json.Unmarshal(raw, &info)
			spec.InviteCode, spec.InviteLink = info.Data.InviteCode, info.Data.InviteLink
		}
		if invited, msg, cerr := p.CheckInvited(ctx, a.UID); cerr == nil {
			spec.Invited, spec.InvitedMessage = invited, msg
		}
		spec.Records, _ = p.inviteRecords(ctx, a)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(RenderInvitePage(spec)))
}

// handleInvitePageBind POST /admin/codar/invite-page/bind —— 页面专用（转发到同一套 BindInvite）。
func (p *Provider) handleInvitePageBind(w http.ResponseWriter, r *http.Request) {
	p.handleInviteBind(w, r)
}

// ── 页面渲染用的小工具（本包此前没有；HTML 页面在 T-354 才出现） ──────────────

// esc 做 HTML 转义（邀请码/链接/上游文案都可能含特殊字符）。
func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// jsStr 把一个字符串安全地嵌进 <script>（用 JSON 引号，顺带挡掉 </script> 注入）。
func jsStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return strings.ReplaceAll(string(b), "</", "<\\/")
}

// str 把上游 JSON 里的任意值渲染成字符串（nil/数字/字符串都吃得下）。
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return strings.Trim(string(b), `"`)
	}
}
