// Package loginui 是「手机号 + 短信验证码」登录页的**共享模板**（T-328）。
//
// # 为什么抽出来（用户要求："参考一下 loomy 的登录账号怎么实现的 看看能不能抽共享"）
//
// loomy 与 codar 的手机号登录页结构完全一样：
//
//	标题 + 副标题 + 手机号输入 + [发送验证码] + 验证码输入 + [登录] + 结果提示 + 底部说明
//	深色卡片 CSS 逐字相同（本包直接用 loomy 那份，codar 从此不再自带一套浅色样式）
//
// 差异只在**可参数化的那几处**：
//
//	· 端点路径（/admin/<上游>/login/sms|verify[/ip-verify]）
//	· 是否带 state（loomy 的会话 state 走 query 并回传；codar 不需要）
//	· 是否需要额外验证组件（codar 要阿里云 captcha 的四个字段）
//	· 是否有第二步（codar 的 IP 验证）
//	· 文案与底部说明
//
// # 刻意**不**统一的（协议本质不同，抽了反而更难懂）
//
//	· 认证方式：loomy 是 HMAC-SHA1 签名；codar 是阿里云 captcha + Bearer
//	· 请求体结构：loomy {base,param{...}} vs codar {phone,name,type,lot_number,...}
//	· 登录后的凭证：loomy session+userid vs codar JWT+username(+IP 二次验证)
//	这些留在各自上游里（见本包调用方）。
//
// # 为什么不 import internal/gateway
//
// 它只是 HTML 模板，没有任何上游身份 ⇒ 不依赖 gateway 就不会被架构测试当成上游
// （上游由"是否 import gateway"自动发现）。但仍要登记进 cmd/server 的 notUpstream 清单
// （那条门禁枚举 internal/ 下每个目录）。
package loginui

import "strings"

// PageSpec 一份登录页的**全部差异**。零值不可用：Title 与两个 URL 必填。
type PageSpec struct {
	Title    string // 页面标题（<title> 与 <h1> 都用它）
	Subtitle string // 标题下的一行小字

	PhoneLabel       string // 默认「手机号（11 位，中国大陆）」
	PhonePlaceholder string // 默认「例如 13800138000」
	CodeLabel        string // 默认「短信验证码」
	CodePlaceholder  string // 默认「6 位数字」
	SendLabel        string // 默认「发送验证码」
	LoginLabel       string // 默认「登录并添加账号」

	SMSURL      string // 发验证码端点（POST）
	VerifyURL   string // 验码登录端点（POST）
	ExtraURL    string // 可选：第二步端点（例如 codar 的 IP 验证）
	ExtraPrompt string // 可选：第二步的提示（点登录后弹出输入框时显示）

	UseState bool // true ⇒ 从 ?state= 读取并在每次 POST 里回传（loomy 的会话 state）

	NeedCaptcha bool     // true ⇒ 渲染"人工验证"容器并收集四个字段
	CaptchaID   string   // 阿里云 captchaId
	HeadScripts []string // 额外 <script src>（例如阿里云 ct4.js）

	HintHTML    string // 底部说明（允许少量 HTML）
	SuccessText string // 成功提示前缀，默认「登录成功」
}

// Render 生成整页 HTML。
//
// ⚠ 用**占位符替换**而不是 fmt.Sprintf：页面里有大量 CSS 百分号（width:100%），
// Sprintf 会把它们当成格式动词吃掉（这是会静默产出坏页面的坑）。
func Render(spec PageSpec) string {
	r := strings.NewReplacer(
		"{{TITLE}}", def(spec.Title, "手机号登录"),
		"{{SUBTITLE}}", esc(def(spec.Subtitle, "登录成功后本页可以关闭，控制台会自动完成添加。")),
		"{{PHONE_LABEL}}", esc(def(spec.PhoneLabel, "手机号（11 位，中国大陆）")),
		"{{PHONE_PH}}", esc(def(spec.PhonePlaceholder, "例如 13800138000")),
		"{{CODE_LABEL}}", esc(def(spec.CodeLabel, "短信验证码")),
		"{{CODE_PH}}", esc(def(spec.CodePlaceholder, "6 位数字")),
		"{{SEND_LABEL}}", esc(def(spec.SendLabel, "发送验证码")),
		"{{LOGIN_LABEL}}", esc(def(spec.LoginLabel, "登录并添加账号")),
		"{{SMS_URL}}", esc(spec.SMSURL),
		"{{VERIFY_URL}}", esc(spec.VerifyURL),
		"{{EXTRA_URL}}", esc(spec.ExtraURL),
		"{{EXTRA_PROMPT}}", esc(def(spec.ExtraPrompt, "请填第二步收到的验证码")),
		"{{USE_STATE}}", boolJS(spec.UseState),
		"{{NEED_CAPTCHA}}", boolJS(spec.NeedCaptcha),
		"{{CAPTCHA_ID}}", esc(spec.CaptchaID),
		"{{HEAD_SCRIPTS}}", headScripts(spec.HeadScripts),
		"{{CAPTCHA_BLOCK}}", captchaBlock(spec),
		"{{HINT}}", def(spec.HintHTML, ""),
		"{{SUCCESS}}", esc(def(spec.SuccessText, "登录成功")),
	)
	return r.Replace(pageTemplate)
}

func def(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func boolJS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func headScripts(list []string) string {
	var b strings.Builder
	for _, u := range list {
		if strings.TrimSpace(u) == "" {
			continue
		}
		b.WriteString(`<script src="` + esc(u) + `"></script>` + "\n")
	}
	return b.String()
}

// esc 极简转义：模板里拼进属性的都是我们自己的常量，但仍然过一道，
// 免得将来有人把用户输入接进来时无声出问题。
func esc(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;",
	).Replace(s)
}

// pageTemplate 与 loomy 原有的页面 CSS 一致（两个上游从此共用这一份样式）。
const pageTemplate = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{TITLE}}</title>
{{HEAD_SCRIPTS}}<style>
:root{
  /* ── 字面色（照抄 internal/server/webui.html 的 :root 字面色）── */
  --ink:#1A1A1A; --cream:#FFF5E6; --pink:#FF3CAC; --yellow:#FFE600;
  --blue:#2B00FF; --acid:#00FF85; --orange:#FF5C00;
  /* 字体：零外部请求（本仓铁律），只用系统字族 + 逐级降级链 */
  --font-display:'Arial Black','Archivo Black','Anton','Impact','Haettenschweiler','PingFang SC','Microsoft YaHei',sans-serif;
  --font-ui:system-ui,-apple-system,'Segoe UI',Roboto,'PingFang SC','Microsoft YaHei',sans-serif;
  --font-mono:ui-monospace,SFMono-Regular,Consolas,'Courier New',monospace;
  /* 语义角色 —— 浅色（奶油纸），与 webui.html 同值 */
  --bg:#FFF5E6; --panel:#FFEFD8; --panel2:#FFE3BE; --line:#1A1A1A;
  --fg:#1A1A1A; --dim:#5C4636; --acc:#2B00FF; --on-acc:#FFF5E6;
  --ok:#0A6B2E; --warn:#A83600; --err:#C6004B; --code-bg:#FFF0CF;
  --band-bg:#FFE600; --band-fg:#120F0D;
  --err-bg:#FFDDE6; --err-fg:#9E0033;
  /* 海报三件套：粗黑边 / 硬阴影 / 零圆角（与控制台同款） */
  --hard:#1A1A1A; --edge:2px solid var(--ink); --radius:0;
  --shadow-hard:6px 6px 0 var(--hard); --shadow-hard-sm:3px 3px 0 var(--hard);
}
/* 暗色（跟随系统）—— 与 webui.html 的 "无 data-theme 时" 那一段同值。
   本页在控制台里是 iframe，永远没有 data-theme，所以这一段就是它的暗色主题。 */
@media (prefers-color-scheme: dark){
  :root{
    --bg:#120F0D; --panel:#1C1714; --panel2:#271F1A; --line:#FFE600;
    --fg:#FFF5E6; --dim:#C7B4A0; --acc:#FF3CAC; --on-acc:#120F0D;
    --ok:#00FF85; --warn:#FFD400; --err:#FF5470; --code-bg:#0B0908;
    --band-bg:#FFE600; --band-fg:#120F0D; --hard:#FF3CAC;
    --err-bg:#2A0C14; --err-fg:#FF9AA5;
  }
}
body{margin:0;padding:32px;font:14px/1.6 var(--font-ui);background:var(--bg);color:var(--fg);
  display:flex;justify-content:center;align-items:flex-start}
.card{width:100%;max-width:460px;background:var(--panel);border:var(--edge);
  border-radius:var(--radius);box-shadow:var(--shadow-hard);padding:22px 22px 20px}
/* 标题做成"黄底黑框"的色块（控制台的 band 观感），不需要额外包一层 div */
h1{display:inline-block;margin:0;padding:5px 10px;font-size:15px;line-height:1.35;
  font-family:var(--font-display);font-weight:900;letter-spacing:-.3px;
  background:var(--band-bg);color:var(--band-fg);border:var(--edge);box-shadow:var(--shadow-hard-sm)}
p.sub{margin:12px 0 2px;color:var(--dim);font-size:12px;line-height:1.75}
label{display:block;margin:14px 0 6px;color:var(--dim);font-size:12px;font-weight:600;letter-spacing:.2px}
input{width:100%;box-sizing:border-box;padding:9px 11px;border-radius:var(--radius);
  border:1px solid var(--line);background:var(--bg);color:var(--fg);font-size:14px;font-family:var(--font-ui)}
input:focus{outline:none;border-color:var(--acc);box-shadow:var(--shadow-hard-sm)}
.row{display:flex;gap:8px;margin-top:2px}
.row input{flex:1;min-width:0}
button{padding:9px 13px;border-radius:var(--radius);border:1px solid var(--line);background:var(--panel2);
  color:var(--fg);font-size:13px;font-weight:600;cursor:pointer;white-space:nowrap;font-family:var(--font-ui)}
button:hover{border-color:var(--acc)}
button.primary{background:var(--acc);border-color:var(--acc);color:var(--on-acc);
  width:100%;margin-top:20px;font-family:var(--font-display);font-weight:900;letter-spacing:.2px}
button:disabled{opacity:.45;cursor:not-allowed}
.msg{margin-top:14px;padding:9px 11px;border:1px solid var(--line);border-radius:var(--radius);
  font-size:12px;display:none;white-space:pre-wrap;background:var(--panel2)}
.msg.ok{display:block;background:var(--panel2);border-color:var(--ok);color:var(--ok);font-weight:600}
.msg.bad{display:block;background:var(--err-bg);border-color:var(--err);color:var(--err-fg)}
.hint{margin-top:18px;padding-top:14px;border-top:1px solid var(--line);color:var(--dim);font-size:11.5px;line-height:1.75}
code{background:var(--code-bg);border:1px solid var(--line);padding:0 4px;border-radius:0;font-family:var(--font-mono);font-size:11.5px}
#cap{margin:14px 0}
a{color:var(--acc)}</style></head><body>
<div class="card">
  <h1>{{TITLE}}</h1>
  <p class="sub">{{SUBTITLE}}</p>

  <label>{{PHONE_LABEL}}</label>
  <input id="phone" inputmode="numeric" maxlength="11" placeholder="{{PHONE_PH}}" autocomplete="tel">

  {{CAPTCHA_BLOCK}}

  <label>{{CODE_LABEL}}</label>
  <div class="row">
    <input id="code" inputmode="numeric" maxlength="8" placeholder="{{CODE_PH}}">
    <button id="send">{{SEND_LABEL}}</button>
  </div>

  <button class="primary" id="login">{{LOGIN_LABEL}}</button>
  <div class="msg" id="msg"></div>

  <div class="hint">{{HINT}}</div>
</div>
<script>
(function(){
  var $ = function(id){ return document.getElementById(id); };
  var STATE = new URLSearchParams(location.search).get('state') || '';
  var USE_STATE = {{USE_STATE}};
  var NEED_CAPTCHA = {{NEED_CAPTCHA}};
  var CAPTCHA_ID = '{{CAPTCHA_ID}}';
  var SMS_URL = '{{SMS_URL}}', VERIFY_URL = '{{VERIFY_URL}}', EXTRA_URL = '{{EXTRA_URL}}';
  var EXTRA_PROMPT = '{{EXTRA_PROMPT}}';
  var SUCCESS_TEXT = '{{SUCCESS}}';
  var msgid = '', cap = {};
  var SEND_LABEL = '';

  function show(text, ok){
    var m = $('msg'); m.textContent = text; m.className = 'msg ' + (ok ? 'ok' : 'bad');
  }
  function post(path, body){
    var payload = Object.assign({}, body || {});
    if (USE_STATE) payload.state = STATE;
    return fetch(path, { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(payload) })
      .then(function(r){ return r.text().then(function(t){
        var j = {}; try { j = JSON.parse(t); } catch(e) { j = { error: t }; }
        if (!r.ok || j.ok === false) throw new Error((j && j.error) || ('HTTP ' + r.status));
        return j;
      }); });
  }
  function pickCaptcha(){
    try { if (window.obj && typeof window.obj.getValidate === 'function') return window.obj.getValidate() || cap; } catch(e) {}
    return cap;
  }
  SEND_LABEL = ($('send') || {}).textContent || '发送验证码';
  if (USE_STATE && !STATE) show('缺少 state —— 请回到控制台重新点「添加账号」', false);

  // 人工验证：回调必须写进选项（initAlicom4 **不返回**可链式调用的实例 —— T-327 实测）。
  if (NEED_CAPTCHA) {
    try {
      if (typeof window.initAlicom4 !== 'function') {
        show('人工验证脚本未加载（离线环境？）—— 发验证码会被上游拒绝', false);
      } else {
        var capOpts = {
          captchaId: CAPTCHA_ID, product: 'bind', language: 'zh',
          onSuccess: function(){ cap = pickCaptcha() || {}; show('人工验证通过，可以发送验证码了', true); },
          onError: function(e){ show('人工验证失败：' + (e && e.msg ? e.msg : '请重试'), false); }
        };
        // ⚠ 4.0 的 initAlicom4 有"两个参数"的用法：第二个回调拿到组件实例。
        //   拿到它就存进 window.obj —— pickCaptcha() 正是从那里取四个字段。
        try { window.initAlicom4(capOpts, function(cfg){ window.obj = cfg || window.obj; }); }
        catch (e2) { try { window.initAlicom4(capOpts); } catch (e3) { show('初始化人工验证失败：' + e3, false); } }
        // ★ T-342：触发入口 —— 点按钮才把组件弹出来（各家展示接口名不一，做特性探测）。
        var trig = $('captcha-trigger');
        if (trig) trig.addEventListener('click', function(){
          var o = window.obj;
          try {
            if (o && typeof o.showBox === 'function') { o.showBox(); return; }
            if (o && typeof o.popup === 'function') { o.popup(); return; }
            if (o && typeof o.show === 'function') { o.show(); return; }
            show('验证组件未就绪：请刷新本页重试（若仍不行，请在新标签打开本页）', false);
          } catch (e4) { show('组件调用失败：' + e4, false); }
        });
      }
    } catch(e) { show('初始化人工验证失败：' + e, false); }
  }

  $('send').addEventListener('click', function(){
    var b = this, phone = $('phone').value.trim();
    if (!phone) { show('请先填手机号', false); return; }
    b.disabled = true; b.textContent = '发送中…';
    var extra = {};
    if (NEED_CAPTCHA) {
      // ★ T-342：captcha 没集齐就**本地**拦下 —— 不要让上游回一句"缺少 lot_number"
      //   （用户实测看到的正是那句，而它完全没告诉他该做什么）。
      var c0 = pickCaptcha() || {};
      if (!c0.lot_number || !c0.captcha_output || !c0.pass_token || !c0.gen_time) {
        show('请先完成上方的人工验证（点「开始人工验证」）', false);
        b.disabled = false; b.textContent = SEND_LABEL;
        return;
      }
    }
    if (NEED_CAPTCHA) extra.captcha = pickCaptcha();
    post(SMS_URL, Object.assign({ phone: phone }, extra)).then(function(j){
      msgid = j.msgid || '';
      show(j.message || '验证码已发送，请查收短信。', true);
      var n = 60;
      var t = setInterval(function(){
        n--; b.textContent = n > 0 ? ('重发 ' + n + 's') : '重新发送';
        if (n <= 0) { clearInterval(t); b.disabled = false; b.textContent = '重新发送'; }
      }, 1000);
    }).catch(function(e){
      b.disabled = false; b.textContent = '发送验证码';
      show('发送失败：' + e.message, false);
    });
  });

  $('login').addEventListener('click', function(){
    var b = this, phone = $('phone').value.trim(), code = $('code').value.trim();
    b.disabled = true; b.textContent = '登录中…';
    var body = { phone: phone, code: code };
    if (msgid) body.msgid = msgid;
    post(VERIFY_URL, body).then(function(j){
      b.textContent = '登录成功';
      show(SUCCESS_TEXT + '：' + (j.nickname || j.uid) + '\n可以关闭本页了，控制台会自动完成添加。', true);
      // 嵌在控制台对话框里时，通知父窗口刷新账号列表（数据驱动的消息 type）。
          // ★ T-343：登录成功后**自动并池** —— 否则凭证只在磁盘上、池里没有它的 secret，
          //   界面会一直「额度完成：已更新 0 · 失败 1」且 TOKEN 列是 —（用户实测现象）。
          //   这里调的正是「账号池 → 重载 auths」那个既有路由，等价于用户手点一次。
          try { fetch('/admin/accounts/reload', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).catch(function(){}); } catch (eReload) {}
      try { if (window.parent && window.parent !== window) window.parent.postMessage({ type: 'manual-page-login-done', uid: j.uid || '' }, location.origin); } catch(e) {}
    }).catch(function(e){
      if (EXTRA_URL && /ip_verify|IP 验证/.test(e.message)) {
        var c2 = window.prompt(EXTRA_PROMPT);
        if (!c2) { b.disabled = false; b.textContent = '登录并添加账号'; return; }
        post(EXTRA_URL, { phone: phone, code: c2 }).then(function(j2){
          b.textContent = '登录成功';
          show(SUCCESS_TEXT + '：' + (j2.nickname || j2.uid), true);
          try {
            fetch('/admin/accounts/reload', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).catch(function(){});
          } catch (e6) {}
          // ★ T-343：登录成功后**自动并池** —— 否则凭证只在磁盘上、池里没有它的 secret，
          //   界面会一直「额度完成：已更新 0 · 失败 1」且 TOKEN 列是 —（用户实测现象）。
          //   这里调的正是「账号池 → 重载 auths」那个既有路由，等价于用户手点一次。
          try { if (window.parent && window.parent !== window) window.parent.postMessage({ type: 'manual-page-login-done', uid: j2.uid || '' }, location.origin); } catch(e2) {}
        }).catch(function(e2){
          b.disabled = false; b.textContent = '登录并添加账号';
          show('第二步失败：' + e2.message, false);
        });
        return;
      }
      b.disabled = false; b.textContent = '登录并添加账号';
      show('登录失败：' + e.message, false);
    });
  });
})();
</script>
</body></html>`

// captchaBlock 渲染人工验证区（T-342）。
//
// # 为什么不是空的 <div id="cap">
//
// 此前这里是个空 div：initAlicom4 只给了回调、**没有任何可点的入口** ⇒
// 用户根本无从完成验证 ⇒ CAPTCHA 恒为空 ⇒ 点「发送验证码」只会拿到上游一句
// "缺少 lot_number"（用户实测报的正是这个）。所以这里给出一个明确的触发按钮：
// 点它才去把组件弹出来（各家组件的展示接口名不一致，JS 侧做了特性探测）。
//
// NeedCaptcha=false 的上游（如 loomy）**不渲染任何东西** —— 页面上不该出现用不上的按钮。
func captchaBlock(spec PageSpec) string {
	if !spec.NeedCaptcha {
		return ""
	}
	return `<div class="capbox" id="capwrap">
    <button id="captcha-trigger" class="captcha-trigger" type="button">开始人工验证</button>
    <div class="dim" style="font-size:11.5px;margin-top:6px">点上面的按钮完成验证；通过后再点「` + spec.sendLabelOrDefault() + `」。</div>
  </div>`
}

// sendLabelOrDefault 默认按钮文案（与模板里 {{SEND_LABEL}} 的默认值保持一致）。
func (spec PageSpec) sendLabelOrDefault() string {
	return def(spec.SendLabel, "发送验证码")
}
