package codar

// smslogin.go —— 手机号 + 短信验证码登录（对标 internal/loomy/smslogin.go）。
//
// # 用户要求（逐字）
//
//	"就是手机号登录啊你对标一下我的F代其他的渠道" · "不要微信扫码"
//
// # 协议（codar2api 源码 + 实测依据）
//
//	① POST {PayBase}/v1/verification-code
//	   body: {phone, name:"user", type:"login", lot_number, captcha_output, pass_token, gen_time}
//	   ⇒ 短信下发（返回业务 code，无 msgid —— 与 loomy 不同，验码不需要回执）
//	② POST {PayBase}/v1/login   body: {username:<phone>, password:<sms code>}
//	   ⇒ 成功 {"data":{"token","username"}}；也可能 HTTP 201 + status="ip_verify_required"
//	③ POST {PayBase}/v1/user/verify_ip
//	   header: token: <②里的临时 token>   body: {username, phone, code}
//	   ⇒ 新 token（该分支需再收一次 type="ip_verify" 的短信）
//
// # captcha 从哪来（本实现的解法）
//
// 阿里云验证码 4.0（`https://static.alicaptcha.com/v4/ct4.js` + captchaId）。
// 四个字段（lot_number/captcha_output/pass_token/gen_time）必须由**页面**采集 ⇒
// 本包自己 serve 一个登录页（见 loginpage.go），页内加载官方脚本、用户过验证后
// 把四字段 POST 回来。这与 loomy 的 handleLoginPage 是同一套做法（上游自带登录 UI，
// 共享前端一行不改）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// 阿里云验证码 4.0 的公开配置（同厂商同配置；codar2api 的 defaults.json 与桌面端均用这套）。
const (
	CaptchaJSURL     = "https://static.alicaptcha.com/v4/ct4.js"
	DefaultCaptchaID = "bb7f810ecc9f6d21dcf72bcae120c8c5"
)

// 手机号 / 验证码格式（与 codar2api 的 phonePattern/codePattern 同口径）。
var (
	phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)
	codePattern  = regexp.MustCompile(`^\d{4,8}$`)
)

// errIPVerifyRequired ② 返回"需要 IP 验证"时带上的信号（附临时 token）。
type errIPVerifyRequired struct {
	TmpToken string
	Phone    string
	Message  string
}

func (e *errIPVerifyRequired) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "上游要求完成 IP 验证"
	}
	return "codar: " + msg + "（需再收一次验证码完成 IP 验证）"
}

// captchaFields 校验并归一化四个验证码字段（缺一个都不许发短信 —— 与上游同口径）。
func captchaFields(c map[string]string) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range []string{"lot_number", "captcha_output", "pass_token", "gen_time"} {
		v := strings.TrimSpace(c[k])
		if v == "" {
			return nil, errors.New("请先完成人工验证（缺少 " + k + "）")
		}
		if len(v) > 8192 {
			return nil, errors.New("人工验证参数过长（" + k + "）")
		}
		out[k] = v
	}
	return out, nil
}

// SendLoginCode 下发登录短信验证码。
//
// ⚠ 手机号格式在**本地**先校验 ⇒ 格式不对时**不会**发上游请求（不会真的发短信）。
func (p *Provider) SendLoginCode(ctx context.Context, phone string, captcha map[string]string, kind string) error {
	phone = strings.TrimSpace(phone)
	if !phonePattern.MatchString(phone) {
		return errors.New("手机号格式不正确")
	}
	if kind == "" {
		kind = "login"
	}
	if kind != "login" && kind != "ip_verify" && kind != "bind_phone" {
		return errors.New("验证码类型不正确")
	}
	body, err := captchaFields(captcha)
	if err != nil {
		return err
	}
	body["phone"] = phone
	body["name"] = "user"
	body["type"] = kind
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, status, err := p.callJSON(cctx, http.MethodPost, p.payBase+"/v1/verification-code", nil, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("codar: 发送验证码失败，上游 HTTP %d（%s）", status, truncateStr(string(raw), 200))
	}
	var out struct {
		Code    int    `json:"code"`
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("codar: 发送验证码响应无法解析: %w", err)
	}
	if out.Code != 0 && out.Code != 200 {
		return fmt.Errorf("codar: 发送验证码失败（code=%d %s）", out.Code, truncateStr(out.Message, 160))
	}
	return nil
}

// LoginByCode 用短信验证码登录。
//
// 成功 ⇒ 返回 *Auth（token 已从响应里取出、exp 从 JWT 解出）。
// 需要 IP 验证 ⇒ 返回 *errIPVerifyRequired（带临时 token，前端应引导走 VerifyIP）。
func (p *Provider) LoginByCode(ctx context.Context, phone, code string) (*Auth, error) {
	phone, code = strings.TrimSpace(phone), strings.TrimSpace(code)
	if !phonePattern.MatchString(phone) {
		return nil, errors.New("手机号格式不正确")
	}
	if !codePattern.MatchString(code) {
		return nil, errors.New("短信验证码格式不正确")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, status, err := p.callJSON(cctx, http.MethodPost, p.payBase+"/v1/login", nil, map[string]any{"username": phone, "password": code})
	if err != nil {
		return nil, err
	}
	// 上游的"需要 IP 验证"是 HTTP 201（codar2api 也是这么判的）
	var out struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Data    struct {
			Token    string `json:"token"`
			Username string `json:"username"`
			Status   string `json:"status"`
			Message  string `json:"message"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	st := strings.ToLower(firstNonEmpty(out.Data.Status, out.Status))
	if st == "ip_verify_required" {
		// 临时 token 可能在外层或 data 里
		tmp := out.Data.Token
		p.rememberIPFlow(phone, tmp)
		return nil, &errIPVerifyRequired{TmpToken: tmp, Phone: phone, Message: firstNonEmpty(out.Data.Message, out.Message)}
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("codar: 登录失败，上游 HTTP %d（%s）", status, truncateStr(string(raw), 200))
	}
	if out.Code != 0 && out.Code != 200 {
		return nil, fmt.Errorf("codar: 登录失败（code=%d %s）", out.Code, truncateStr(firstNonEmpty(out.Data.Message, out.Message), 160))
	}
	tok := strings.TrimSpace(out.Data.Token)
	if tok == "" {
		return nil, errors.New("codar: 登录响应里没有 token")
	}
	a := &Auth{Token: tok, Username: firstNonEmpty(out.Data.Username, phone), ExpiresAt: jwtExpiry(tok)}
	a.fillUID()
	if a.Nickname == "" {
		a.Nickname = a.UID
	}
	p.forgetIPFlow(phone)
	return a, nil
}

// VerifyIP 完成 IP 验证并用新令牌收尾。
func (p *Provider) VerifyIP(ctx context.Context, phone, code string) (*Auth, error) {
	phone, code = strings.TrimSpace(phone), strings.TrimSpace(code)
	if !phonePattern.MatchString(phone) || !codePattern.MatchString(code) {
		return nil, errors.New("手机号或验证码格式不正确")
	}
	tmp, ok := p.ipFlowToken(phone)
	if !ok || tmp == "" {
		return nil, errors.New("codar: 没有待完成的 IP 验证会话，请重新登录")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimRight(p.payBase, "/")+"/v1/user/verify_ip", strings.NewReader(
		`{"username":`+jsonString(phone)+`,"phone":`+jsonString(phone)+`,"code":`+jsonString(code)+`}`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}
	req.Header.Set("token", tmp) // ★ 该端点用 token 头，不是 Authorization
	resp, err := p.client.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			raw = append(raw, buf[:n]...)
			if len(raw) > 1<<20 {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	var out struct {
		Code int `json:"code"`
		Data struct {
			Token    string `json:"token"`
			Username string `json:"username"`
		} `json:"data"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &out)
	tok := firstNonEmpty(out.Data.Token, out.Token)
	if resp.StatusCode != http.StatusOK || tok == "" {
		return nil, fmt.Errorf("codar: IP 验证失败，上游 HTTP %d（%s）", resp.StatusCode, truncateStr(string(raw), 200))
	}
	a := &Auth{Token: tok, Username: firstNonEmpty(out.Data.Username, phone), ExpiresAt: jwtExpiry(tok)}
	a.fillUID()
	if a.Nickname == "" {
		a.Nickname = a.UID
	}
	p.forgetIPFlow(phone)
	return a, nil
}

// ── IP 验证会话表（phone → 临时 token）───────────────────────────────

const ipFlowTTL = 15 * time.Minute

func (p *Provider) rememberIPFlow(phone, tmp string) {
	p.ipMu.Lock()
	defer p.ipMu.Unlock()
	if p.ipFlows == nil {
		p.ipFlows = map[string]ipFlow{}
	}
	now := time.Now()
	for k, v := range p.ipFlows {
		if now.Sub(v.At) > ipFlowTTL {
			delete(p.ipFlows, k)
		}
	}
	p.ipFlows[phone] = ipFlow{Tmp: tmp, At: now}
}

func (p *Provider) ipFlowToken(phone string) (string, bool) {
	p.ipMu.Lock()
	defer p.ipMu.Unlock()
	v, ok := p.ipFlows[phone]
	if !ok || time.Since(v.At) > ipFlowTTL {
		return "", false
	}
	return v.Tmp, true
}

func (p *Provider) forgetIPFlow(phone string) {
	p.ipMu.Lock()
	defer p.ipMu.Unlock()
	delete(p.ipFlows, phone)
}

type ipFlow struct {
	Tmp string
	At  time.Time
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// jsonString 把一个字符串编成 JSON 字面量（避免手拼转义）。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
