package codar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"workbuddy2api/internal/gateway"
)

// 邀请码绑定（T-353）。
//
// # 端点来源：桌面端 asar 的**前端 JS**（不是二进制、也不是 codar2api）
//
// 逐字来自 `resources/app.asar → renderer/assets/index-Cr4tsyUc.js`：
//
//	async function addUserInvite(session, inviteCode, captcha) {
//	  const response = await requestAuth("/v1/user/add-invite", {
//	    username: session.username,
//	    invite_code: inviteCode,
//	    lot_number: captcha.lotNumber,
//	    captcha_output: captcha.captchaOutput,
//	    pass_token: captcha.passToken,
//	    gen_time: captcha.genTime,
//	  }, authHeaders(session));
//	  assertSuccessCode(response, t("邀请码绑定失败"));
//	}
//	async function checkUserInvited(session) {
//	  const response = await requestAuth("/v1/user/check-invited", { username: session.username }, authHeaders(session));
//	  return response.data === true;
//	}
//
// ⚠ 教训（T-351/T-352 踩过）：逆上游能力时，**前端 JS 比 Go 二进制权威**。
// 二进制里只有路由字符串常量（甚至被拆分/拼接而搜不到），前端才有完整调用形状。
//
// # 实测行为（2026-11-08，用新账号）
//
//	check-invited 已被邀请 → {"code":200,"message":"用户已被邀请","data":true}   （老账号）
//	check-invited 未被邀请 → {"code":200,"message":"用户未被邀请","data":false}  （新账号 ⇒ 还能绑）
//	add-invite 缺 captcha  → 400「请完成人机验证，刷新页面后重试」
//	add-invite 假 captcha  → 500「添加邀请关系失败: 'result'」（说明 4 字段被接受，卡在码本身）
//
// # 微信绑定
//
// 官方文案：「老用户首次绑定邀请码**并完成微信绑定**后同样计入」⇒ 微信绑定影响的是
// "这笔邀请算不算邀请人的进度"，**不是**绑定动作本身的前置条件 ⇒ 我们不做微信（那是桌面端
// 本地 connector 的能力，`/api/connectors/wechat/*` 是它自带 sidecar 的端点）。
//
// # 为什么本地先拦 captcha
//
// 缺字段时上游只回一句模糊的「请完成人机验证」；本地说清**缺哪几个**，用户/界面才好办。

// 邀请码绑定的 captcha 字段名（与前端 JS 的 payload 一字不差）。
var captchaFieldNames = []string{"lot_number", "captcha_output", "pass_token", "gen_time"}

// CheckInvited 查本账号是否**已被**别人邀请（true = 不能/不必再绑）。
func (p *Provider) CheckInvited(ctx context.Context, uid string) (bool, string, error) {
	a, err := p.pickAuth(uid)
	if err != nil {
		return false, "", err
	}
	// ⚠ 容忍 nil ctx：公开签名允许调用方传 nil（既有测试就是这么调的），
	// 而 context.WithTimeout(nil, …) 会 panic ⇒ 这里统一兜底成 Background。
	// （改实现而不是改老测试：nil-ctx 是所有调用方的坑，不该让调用方各自小心。）
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, status, cerr := p.callJSON(cctx, http.MethodPost, p.payBase+"/v1/user/check-invited", a,
		map[string]any{"username": a.UID})
	if cerr != nil {
		return false, "", cerr
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    bool   `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		return false, "", fmt.Errorf("codar: check-invited 响应解析失败（HTTP %d）：%s", status, truncateStr(string(raw), 200))
	}
	return out.Data, out.Message, nil
}

// MissingCaptchaFields 返回 captcha 里缺哪些字段（空 = 齐了）。
func MissingCaptchaFields(captcha map[string]string) []string {
	var missing []string
	for _, k := range captchaFieldNames {
		if captcha == nil || captcha[k] == "" {
			missing = append(missing, k)
		}
	}
	return missing
}

// BindInvite 把自己的账号绑到某个邀请码上（**写操作**，且要 captcha）。
//
// captcha 是阿里云 4.0 的四个字段（与发短信验证码同款），键名必须是
// lot_number / captcha_output / pass_token / gen_time —— 少一个上游就回「请完成人机验证」。
func (p *Provider) BindInvite(ctx context.Context, uid, inviteCode string, captcha map[string]string) (string, error) {
	if inviteCode == "" {
		return "", fmt.Errorf("codar: 邀请码为空")
	}
	if missing := MissingCaptchaFields(captcha); len(missing) > 0 {
		return "", fmt.Errorf("codar: 人机验证未完成（缺少 %v）—— 请先在页面上完成验证再提交", missing)
	}
	a, err := p.pickAuth(uid)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body := map[string]any{
		"username":       a.UID,
		"invite_code":    inviteCode,
		"lot_number":     captcha["lot_number"],
		"captcha_output": captcha["captcha_output"],
		"pass_token":     captcha["pass_token"],
		"gen_time":       captcha["gen_time"],
	}
	raw, status, cerr := p.callJSON(cctx, http.MethodPost, p.payBase+"/v1/user/add-invite", a, body)
	if cerr != nil {
		return "", cerr
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Success bool   `json:"success"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Code != 200 || !out.Success {
		return out.Message, fmt.Errorf("codar: 绑定邀请码失败（HTTP %d，code=%d）：%s", status, out.Code, truncateStr(out.Message, 120))
	}
	return out.Message, nil
}

// inviteRecords 查"我邀请来的人"的记录（前端 JS：GET /v1/promotion/invites → data[]）。
func (p *Provider) inviteRecords(ctx context.Context, a *Auth) ([]map[string]any, error) {
	// ⚠ 容忍 nil ctx：公开签名允许调用方传 nil（既有测试就是这么调的），
	// 而 context.WithTimeout(nil, …) 会 panic ⇒ 这里统一兜底成 Background。
	// （改实现而不是改老测试：nil-ctx 是所有调用方的坑，不该让调用方各自小心。）
	if ctx == nil {
		ctx = context.Background()
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, _, err := p.callJSON(rctx, http.MethodPost, p.payBase+"/v1/promotion/invites", a,
		map[string]any{"username": a.UID, "token": 0})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil, nil
	}
	return out.Data, nil
}

// handleInviteBind POST /admin/codar/invite/bind
//
// body: {"uid": "<账号>", "invite_code": "<别人的码>",
//
//	"lot_number","captcha_output","pass_token","gen_time"}
//
// ★ 没有 captcha 四字段时**本地**返回 400（不发请求）—— 见 BindInvite 的注释。
func (p *Provider) handleInviteBind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID        string `json:"uid"`
		InviteCode string `json:"invite_code"`
		LotNumber  string `json:"lot_number"`
		CaptchaOut string `json:"captcha_output"`
		PassToken  string `json:"pass_token"`
		GenTime    string `json:"gen_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是 JSON：" + err.Error()})
		return
	}
	captcha := map[string]string{
		"lot_number": req.LotNumber, "captcha_output": req.CaptchaOut,
		"pass_token": req.PassToken, "gen_time": req.GenTime,
	}
	if missing := MissingCaptchaFields(captcha); len(missing) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "missing_captcha": missing,
			"error": fmt.Sprintf("人机验证未完成（缺少 %v）：先在页面上完成验证，再把四个字段一起提交", missing),
		})
		return
	}
	msg, err := p.BindInvite(r.Context(), req.UID, req.InviteCode, captcha)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "message": msg})
		return
	}
	// 绑成功后顺手回一个"已被邀请"状态，界面可立刻刷新
	invited, _, _ := p.CheckInvited(r.Context(), req.UID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "invited": invited})
}

// 让契约测试看到 handleInviteBind 用的是 gateway（与其它上游一致的做法）。
var _ = gateway.CreditsQuota
