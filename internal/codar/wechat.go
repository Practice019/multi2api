package codar

// wechat.go —— 微信绑定（T-364）：codar 邀请奖励的**第二段**。
//
// # 为什么需要它（官方原文，取自桌面端 asar 的翻译表）
//
//	「通过邀请码注册的用户，绑定微信后可领取 2,000 点数邀请奖励。」
//	「被邀请者需绑定微信。手机号与微信绑定后，邀请点数会自动补齐。」
//
// ⇒ 邀请奖励是两段式：①被邀请人绑邀请码（已有）②被邀请人**再绑微信** ⇒ 邀请人的点数才入账。
//   所以面板要能显示"微信：已绑定/未绑定"，并提供扫码绑定。
//
// # ★ 上游的一个大坑（与 T-357 同源）：业务码在 body 里、HTTP 恒为 200
//
// 实测（2026-11-08）：
//
//	POST /v1/user/wechat-bound   {username} → 200 {"code":200,…,"data":true|false}
//	POST /v1/wechat_agent/bind   {username} → 200 {"code":200,…,"data":{"scene":…,"qr_url":…}}
//	                              已绑账号 → 200 {"code":400,"message":"用户已绑定微信",…}   ★ HTTP 仍 200
//	POST /v1/wechat/login/status {scene}    → 200 {"code":200,…,"data":{"status":"pending","mode":"bind"}}
//	                              scene 失效 → 200 {"code":404,"message":"场景已失效",…}       ★ HTTP 仍 200
//
// ⇒ 判失败一律看 `code`，**不能**看 HTTP 状态；否则"已绑定"会被读成"生成了二维码"。
//   （T-357 的 inviteViewFor 踩过同一个坑，这里沿用同一判据。）

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

const (
	wechatBoundUpstream  = "/v1/user/wechat-bound"   // 查"本账号是否已绑微信"
	wechatBindUpstream   = "/v1/wechat_agent/bind"   // 取绑定二维码（scene + qr_url）
	wechatStatusUpstream = "/v1/wechat/login/status" // 轮询扫码结果（**不需要** Authorization）

	// 管理端点路径（与 invite 同族，前端按此写）
	wechatQRPath     = "/admin/codar/wechat/qr"
	wechatStatusPath = "/admin/codar/wechat/status"
	wechatPagePath   = "/admin/codar/wechat-page"

	// wechatQRTTLSeconds 二维码有效期（秒）。前端据此每 5 分钟自动刷新。
	// 依据：桌面端 POLL_DELAY_MS=1000、QR_REFRESH_MS=5*60000（5 分钟）。
	wechatQRTTLSeconds = 300
)

// WechatBound 查某账号是否已绑定微信。data 是**布尔**（不是对象）。
func (p *Provider) WechatBound(uid string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return p.wechatBoundCtx(ctx, uid)
}

// wechatBoundCtx 与 WechatBound 同逻辑，但**用调用方的 ctx**。
//
// 为什么需要它（T-366）：inviteViewFor 给每个账号派生了一个更短的子超时，
// 若这一步自建 20s ctx，那个子超时对它就完全无效 —— 卡住的账号仍会把整行拖满 20s。
func (p *Provider) wechatBoundCtx(ctx context.Context, uid string) (bool, error) {
	a, err := p.pickAuth(uid)
	if err != nil {
		return false, err
	}
	raw, _, cerr := p.callJSON(ctx, http.MethodPost, p.payBase+wechatBoundUpstream, a,
		map[string]any{"username": a.UID})
	if cerr != nil {
		return false, cerr
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    bool   `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		return false, errUpstreamShape("wechat-bound", raw, jerr)
	}
	if out.Code != 200 {
		return false, errUpstreamBiz("wechat-bound", out.Code, out.Message)
	}
	return out.Data, nil
}

// WechatBindQR 取绑定二维码。返回 (scene, qr_url, 上游 message, err)。
//
// ⚠ qr_url 原样透传（不下载、不转存）：它是微信的 showqrcode 链接，后端去取只会多一次往返与一份缓存。
func (p *Provider) WechatBindQR(uid string) (string, string, string, error) {
	a, err := p.pickAuth(uid)
	if err != nil {
		return "", "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, _, cerr := p.callJSON(ctx, http.MethodPost, p.payBase+wechatBindUpstream, a,
		map[string]any{"username": a.UID})
	if cerr != nil {
		return "", "", "", cerr
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Scene  string `json:"scene"`
			QRURL  string `json:"qr_url"`
			QRURL2 string `json:"qrUrl"` // 兼容另一种写法（桌面端 JS 里就是二者取一）
		} `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		return "", "", "", errUpstreamShape("wechat_agent/bind", raw, jerr)
	}
	if out.Code != 200 {
		// 典型：{"code":400,"message":"用户已绑定微信"} ⇒ 把上游 message 带出去给界面显示
		return "", "", out.Message, errUpstreamBiz("wechat_agent/bind", out.Code, out.Message)
	}
	qr := out.Data.QRURL
	if qr == "" {
		qr = out.Data.QRURL2
	}
	if strings.TrimSpace(out.Data.Scene) == "" || strings.TrimSpace(qr) == "" {
		return "", "", out.Message, errUpstreamShape("wechat_agent/bind", raw, nil)
	}
	return out.Data.Scene, qr, out.Message, nil
}

// WechatScanStatus 查一次扫码状态（**不需要**凭据 —— 实测带与不带 Authorization 结果完全一致）。
//
// 返回：raw（上游原样 status）、norm（归一化后的五档）、mode、message、expired（场景失效）、
// username（上游回带的账号名，SUCCESS 后用它回查 bound）。
func (p *Provider) WechatScanStatus(scene string) (raw, norm, mode, message, username string, expired bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rawBytes, _, cerr := p.callJSON(ctx, http.MethodPost, p.payBase+wechatStatusUpstream, nil,
		map[string]any{"scene": scene})
	if cerr != nil {
		return "", "", "", "", "", false, cerr
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Status   string `json:"status"`
			Mode     string `json:"mode"`
			Message  string `json:"message"`
			Username string `json:"username"`
		} `json:"data"`
	}
	var rawOut struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rawBytes, &rawOut)
	if jerr := json.Unmarshal(rawBytes, &out); jerr != nil {
		return "", "", "", "", "", false, errUpstreamShape("wechat/login/status", rawBytes, jerr)
	}
	// ★ 404 = 场景失效：**不是错误**，是"这个二维码过期了，请重取"（HTTP 仍是 200）
	if out.Code == 404 {
		return "", "EXPIRED", out.Data.Mode, rawOut.Message, out.Data.Username, true, nil
	}
	if out.Code != 200 {
		return "", "", "", "", "", false, errUpstreamBiz("wechat/login/status", out.Code, out.Message)
	}
	raw = out.Data.Status
	norm = normalizeWechatStatus(raw)
	if norm == "EXPIRED" {
		expired = true
	}
	if out.Data.Message != "" {
		message = out.Data.Message
	}
	return raw, norm, out.Data.Mode, message, out.Data.Username, expired, nil
}

// normalizeWechatStatus 把上游 status 归一化成前端只需认的五档。
//
// SUCCESS|COMPLETED → SUCCESS；FAILED → FAILED；EXPIRED → EXPIRED；SCANNED → SCANNED；
// 其余（含空、pending、未知）→ pending。大小写与首尾空白都不敏感。
func normalizeWechatStatus(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SUCCESS", "COMPLETED":
		return "SUCCESS"
	case "FAILED":
		return "FAILED"
	case "EXPIRED":
		return "EXPIRED"
	case "SCANNED":
		return "SCANNED"
	default:
		return "pending"
	}
}

// errUpstreamBiz 上游**业务码**失败（HTTP 200 但 code != 200）。
//
// 为什么单独一个构造函数：上游把错误放在 body 的 code/message 里，HTTP 恒 200
// ⇒ "HTTP 成功" 与 "业务成功"是两件事，错误信息里必须把 code 与 message 都带上，
// 否则界面只能显示一句"失败了"，排查不了。
func errUpstreamBiz(ep string, code int, message string) error {
	return errShape{"codar: " + ep + " 上游业务错误 code=" + strconv.Itoa(code) + " message=" + message}
}

// errUpstreamShape 响应体形状不符（解析失败或缺字段）。
func errUpstreamShape(ep string, raw []byte, cause error) error {
	msg := "codar: " + ep + " 响应结构不符：" + truncateStr(string(raw), 200)
	if cause != nil {
		msg += "（" + cause.Error() + "）"
	}
	return errShape{msg}
}

type errShape struct{ s string }

func (e errShape) Error() string { return e.s }

// ─────────────────────────────────────────────────────────────────────────────
// 管理端点
// ─────────────────────────────────────────────────────────────────────────────

// handleWechatQR GET /admin/codar/wechat/qr?uid=
//
//	已绑定 → {"ok":true,"uid":…,"bound":true}（不生成二维码）
//	未绑定 → {"ok":true,"uid":…,"bound":false,"scene":…,"qr_url":…,"expires_in":300}
//	取码失败（含"用户已绑定微信"）→ 502 {"ok":false,"error":…,"upstream_message":…}
//	uid 不存在 → 404
func (p *Provider) handleWechatQR(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	bound, err := p.WechatBound(uid)
	if err != nil {
		status := http.StatusBadGateway
		if isNoCredentialErr(err) {
			// 凭证找不到 —— 这不是"上游失败了"，是"你问的账号不存在"⇒ 404 更准确
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if bound {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "bound": true})
		return
	}
	scene, qrURL, upMsg, berr := p.WechatBindQR(uid)
	if berr != nil {
		out := map[string]any{"ok": false, "error": berr.Error()}
		if upMsg != "" {
			out["upstream_message"] = upMsg
		}
		writeJSON(w, http.StatusBadGateway, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "bound": false,
		"scene": scene, "qr_url": qrURL, "expires_in": wechatQRTTLSeconds,
	})
}

// handleWechatStatus GET /admin/codar/wechat/status?scene=
//
// 场景失效（上游 code=404）⇒ **200** + status=EXPIRED + expired=true（不是错误）。
// 传输/业务失败 ⇒ 502。status 变 SUCCESS 时顺手回查一次 bound。
func (p *Provider) handleWechatStatus(w http.ResponseWriter, r *http.Request) {
	scene := strings.TrimSpace(r.URL.Query().Get("scene"))
	raw, norm, mode, message, username, expired, err := p.WechatScanStatus(scene)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	bound := false
	if norm == "SUCCESS" && username != "" {
		if b, berr := p.WechatBound(username); berr == nil {
			bound = b
		}
	}
	out := map[string]any{
		"ok": true, "status": norm, "raw_status": raw,
		"mode": mode, "message": message, "expired": expired, "bound": bound,
	}
	if message == "" {
		out["message"] = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWechatPage GET /admin/codar/wechat-page?uid=
//
// 自包含扫码页：二维码 + 状态行 + 浏览器侧轮询（**后端不起任何后台 job**）。
func (p *Provider) handleWechatPage(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	spec := WechatPageSpec{UID: uid}
	if uid != "" {
		if bound, err := p.WechatBound(uid); err == nil {
			spec.Bound = bound
			spec.Known = true
		} else {
			spec.Error = err.Error()
		}
	} else {
		spec.Error = "缺少 uid 参数"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(RenderWechatPage(spec)))
}

// isNoCredentialErr 判断错误是不是"找不到该 uid 的凭证"（pickAuth 的措辞）。
func isNoCredentialErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "找不到 uid=") || strings.Contains(s, "没有任何可用凭证")
}

var _ = gateway.CapInvite
