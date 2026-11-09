package codar

// loginpage.go —— 手机号登录页 + 发码/验码端点（对标 loomy 的 handleLoginPage）。
//
// # 为什么由上游自己 serve 一个页面
//
// 阿里云验证码 4.0 的四个字段（lot_number/captcha_output/pass_token/gen_time）**只能由页面采集**，
// 而本项目的硬规矩是"加一个上游时共享前端一行都不改"。loomy 的解法就是上游自带登录 UI
// （`GET /admin/loomy/login`），本实现照办：页面与端点都住在 internal/codar 里。
//
// 页面是同源加载的（由 admin 服务器下发）⇒ 浏览器自带管理员会话 cookie ⇒
// 页面里的 fetch 无需手工塞 token。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/loginui"
)

const (
	loginPagePath = "/admin/codar/login"
	loginSMSPath  = loginPagePath + "/sms"
	loginVerPath  = loginPagePath + "/verify"
	loginIPPath   = loginPagePath + "/ip-verify"
)

// loginPageHTML 登录页（自包含：只外链官方 captcha 脚本）。

// handleLoginPage 下发登录页（GET）。
func (p *Provider) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// ★ T-328/T-329：登录页 HTML 抽到 internal/loginui（与 loomy 共用一套样式与 JS 骨架），
	// 本上游只声明自己的差异：要多一步人工验证、多一个 IP 验证端点、文案不同。
	html := loginui.Render(loginui.PageSpec{
		Title:       "科搭 Codar · 手机号登录",
		Subtitle:    "流程：填手机号 → 过人工验证 → 收短信 → 填验证码。凭据登录成功后直接写入 auths/codar/。",
		SMSURL:      loginSMSPath,
		VerifyURL:   loginVerPath,
		ExtraURL:    loginIPPath,
		ExtraPrompt: "上游要求完成 IP 验证：请填新收到的验证码",
		NeedCaptcha: true,
		CaptchaID:   DefaultCaptchaID,
		HeadScripts: []string{CaptchaJSURL},
		LoginLabel:  "登录并保存凭据",
		HintHTML: "人工验证由阿里云验证码组件提供（无感通过或拖一下滑块）。<br>" +
			"登录成功后凭据写入 <code>auths/codar/</code>，到「账号池」点一次「重载 auths」即可看到该账号。",
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}

// saveLoginAuth 把登录得到的凭证落盘到本上游的目录（0600），返回文件名。
func (p *Provider) saveLoginAuth(a *Auth) (string, error) {
	dir := p.authDir
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join("auths", providerID)
	}
	path, err := SaveOne(dir, a)
	if err != nil {
		return "", err
	}
	return filepath.Base(path), nil
}

// handleLoginSMS POST /admin/codar/login/sms  {phone, captcha:{lot_number,captcha_output,pass_token,gen_time}}
func (p *Provider) handleLoginSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string            `json:"phone"`
		Kind    string            `json:"kind"`
		Captcha map[string]string `json:"captcha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	if err := p.SendLoginCode(r.Context(), body.Phone, body.Captcha, body.Kind); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "验证码已发送"})
}

// handleLoginVerify POST /admin/codar/login/verify  {phone, code}
func (p *Provider) handleLoginVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	a, err := p.LoginByCode(r.Context(), body.Phone, body.Code)
	if err != nil {
		var ipErr *errIPVerifyRequired
		if errorsAs(err, &ipErr) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "need": "ip_verify", "error": err.Error(),
				"hint": "请再收一次验证码（发码时 type=ip_verify），然后填到下一步",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	name, serr := p.saveLoginAuth(a)
	if serr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "凭据落盘失败：" + serr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": a.UID, "nickname": a.Nickname, "file": name,
		"expires_at": a.ExpiresAt, "saved_at": time.Now().Format(time.RFC3339),
		"hint": "已写入 auths/" + providerID + "/；到「账号池」点一次「重载 auths」即可见到该账号",
	})
}

// handleLoginIPVerify POST /admin/codar/login/ip-verify  {phone, code}
func (p *Provider) handleLoginIPVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	a, err := p.VerifyIP(r.Context(), body.Phone, body.Code)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	name, serr := p.saveLoginAuth(a)
	if serr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "凭据落盘失败：" + serr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": a.UID, "file": name, "hint": "已保存，请点「重载 auths」"})
}

// errorsAs 小包装（避免在文件头 import errors 只为一处）。
func errorsAs(err error, target **errIPVerifyRequired) bool {
	e, ok := err.(*errIPVerifyRequired)
	if ok {
		*target = e
	}
	return ok
}

// ── T-325：「添加账号」按钮（core 的判据是两个扩展点都要）──────────────────

var (
	_ gateway.ManualLoginExt     = (*Provider)(nil)
	_ gateway.LoginFlow          = (*Provider)(nil)
	_ gateway.ManualLoginPageExt = (*Provider)(nil)
)

// ManualLoginIsPage 告诉 core：ManualLogin().path 是一个**页面**（内嵌 iframe），
// 不是"粘贴回跳 URL"的 POST 端点。前端据此分流（它只看这个布尔，不认上游名）。
func (p *Provider) ManualLoginIsPage() bool { return true }

// ManualLogin 让分组头的「添加账号」里出现"打开本上游登录页"的入口。
//
// # 为什么需要它（T-325 实测的差距）
//
//	没有 ManualLoginExt 时：manifest 的 login.manual 缺失 ⇒ 用户只能从左侧导航找页面。
//	有了它：与 codearts / monkeycode 完全同形（path + instructions）。
//
// path 指向本包自带的登录页（T-324 实现）。
func (p *Provider) ManualLogin() (string, string) {
	return loginPagePath,
		"点开此页：填手机号 → 过人工验证 → 收短信 → 输入验证码。登录成功后凭据直接写入 auths/codar/，" +
			"再到「账号池」点一次「重载 auths」即可看到该账号。"
}

// Start/Poll/Configured 实现 gateway.LoginFlow。
//
// # 为什么这三点是"占位但诚实"
//
// core 的 manifest 判据是 `ExtOf[LoginFlow] && Configured()` —— **只实现 ManualLoginExt 不给按钮**。
// 而本上游的登录交互（人工验证）只能发生在**页面里**（core 的对话框没有 captcha 组件）
// ⇒ 所以：
//
//	Start  ：发一个会话号、返回**空 authURL**（与 monkeycode 同形：前端据此走"等结果"分支，
//	         不会去开一个不存在的链接）；真正的交互由 ManualLogin 指的页面承担。
//	Poll   ：不假装在轮询 —— 明确告诉调用方"请到页面里完成"，而不是永远 pending（那会让前端空转）。
//	Configured：恒 true（页面不需要任何配置）。
func (p *Provider) Start() (state, authURL string, err error) {
	return fmt.Sprintf("codar-page-%d", time.Now().UnixNano()), "", nil
}

// Poll 见 Start 的说明：本上游的登录不经由 core 的轮询链路。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	return gateway.Credential{}, fmt.Errorf("codar: 请到「手机号登录」页面完成登录（%s）", loginPagePath)
}

// Configured 恒 true：登录页不依赖任何配置。
func (p *Provider) Configured() bool { return p != nil }
