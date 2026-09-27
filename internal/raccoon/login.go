// login.go Raccoon 的登录流程（微信扫码轮询）。
//
// # 关键事实：code 由客户端本地随机生成，服务端接受任意自造 code
//
// 实测（AGENTS.md:3539-3541）：POST /login_with_qrcode_code 带一个本地生成的
// 32 位 hex 就返回 200 + {"status":"pending"}。扫码后服务端把该 code 置为 success。
//
// 正因如此，我们**完全不需要**官方 `office-raccoon://auth/callback` 那条链路
// （那条也不可用：回调地址写死在 Web bundle 里，改不成 localhost）。
//
// # 与参照项目的一处差异（刻意）
//
// 参照项目把二维码渲染在一个**本机 HTTP 登录页**里（127.0.0.1 随机端口，
// 页面内联 SVG 二维码 + 阿里云滑块脚本）。本网关是纯后端，没有那个页面 ——
// 改为把**二维码承载 URL** 作为登录链接返回，由前端自行渲染成二维码。
//
// 这样做的好处：不绑端口、不注入 HTML、不需要 QR 编码实现；
// 且二维码内容（URL）本来就是扫码要的东西，页面只是它的可视化 ——
// 参照项目自己也把"SVG 转义引号"列为踩过的坑（AGENTS.md:3654-3666）。
//
// 短信路径**不实现**：它要求 `captcha_param`，而那只能由 AliyunCaptcha.js
// 在浏览器里执行滑块后产出（raccoon-login-page.ts:417,506-524）。
// 纯 Go 无法程序化完成，源码侧也把短信列为次选。
package raccoon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// loginEntry 一次在途登录的会话。
type loginEntry struct {
	state       string
	qrCode      string
	qrURL       string
	issuedAt    time.Time
	expiredAt   string
	rotations   int // canceled 时换过几次 code
	ready       bool
	cred        gateway.Credential
	failRsn     string
	mu          sync.Mutex
}

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	p   *Provider
	mu  sync.Mutex
	ses map[string]*loginEntry
}

// LoginFlow 返回（并缓存）登录流程实例。
//
// ⚠ 必须缓存：每次新建会让 sessions map 各是一份空的，于是 start 存下的会话
// 在 poll 里找不到（其它上游端到端实测抓过这个 bug）。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil {
		return nil, false
	}
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, ses: map[string]*loginEntry{}}
	})
	return p.loginCached, true
}

// Configured 报告这份部署真的能走登录流程吗。
//
// Raccoon 的登录只看 API 基址可达，没有"是否配了 OAuth 客户端"这一层。
func (f *loginFlow) Configured() bool { return f != nil && f.p != nil }

// ── 让 *Provider 直接满足 gateway.LoginFlow ────────────────────────────────
//
// ⚠ 这三个转发方法是**必需的**：核心用 `ExtOf[LoginFlow](p)`（类型断言）
// 发现扩展点，而访问器 `LoginFlow() (LoginFlow, bool)` 的返回值
// **不参与方法集匹配** —— 断言看不见它。
//
// 少了它们，本包测试全绿（测试走访问器）但 `/admin/providers` 下发
// `login:null`，界面上没有「＋ 添加账号」按钮。实测事故就是这个形态，
// 现由 gateway.RunProviderContract 的 verifyExtensionsDiscoverable 守住。

// Start 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Start() (string, string, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("raccoon: 登录流程未初始化")
	}
	return lf.Start()
}

// Poll 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("raccoon: 登录流程未初始化")
	}
	return lf.Poll(state)
}

// Configured 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Configured() bool {
	lf, ok := p.LoginFlow()
	return ok && lf.Configured()
}

// Start 发起一次登录：生成 code + 承载 URL，并启动后台轮询。
//
// 返回 (state, loginURL, error)。state 供 Poll 使用。
func (f *loginFlow) Start() (string, string, error) {
	if f == nil || f.p == nil {
		return "", "", errors.New("raccoon: 登录流程未配置")
	}
	code, err := GenerateQRCode()
	if err != nil {
		return "", "", fmt.Errorf("raccoon: 生成扫码 code 失败: %w", err)
	}
	e := &loginEntry{
		state:    code, // state 即 qrcode_code（它天然唯一且随机）
		qrCode:   code,
		qrURL:    QRLoginURL(code),
		issuedAt: time.Now(),
	}

	f.mu.Lock()
	f.ses[e.state] = e
	f.mu.Unlock()

	go f.pollLoop(e)
	return e.state, e.qrURL, nil
}

// Poll 查询授权结果。未完成时返回 gateway.ErrLoginPending。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	if f == nil {
		return gateway.Credential{}, errors.New("raccoon: 登录流程未配置")
	}
	f.mu.Lock()
	e, ok := f.ses[state]
	f.mu.Unlock()
	if !ok {
		return gateway.Credential{}, errors.New("raccoon: 登录会话不存在或已结束，请重新发起")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failRsn != "" {
		return gateway.Credential{}, errors.New(e.failRsn)
	}
	if !e.ready {
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	cred := e.cred
	f.mu.Lock()
	delete(f.ses, state)
	f.mu.Unlock()
	return cred, nil
}

// QRCodeURL 返回本次登录的二维码承载 URL（供前端渲染二维码）。
//
// 未完成的会话才会返回；已完成/不存在时返回空串。
func (f *loginFlow) QRCodeURL(state string) string {
	f.mu.Lock()
	e := f.ses[state]
	f.mu.Unlock()
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.qrURL
}

// pollLoop 后台轮询直到成功/取消/超时。
//
// # 状态处理（照搬参照 raccoon-oauth.ts:140-180）
//
//	pending  → 继续
//	logging  → 继续，记录 expired_at
//	canceled → **换一个新 code** 并更新 URL（不换的话用户卡死）
//	success  → 取凭据
//	其它      → 已在 NormalizeQRStatus 里降级为 pending
func (f *loginFlow) pollLoop(e *loginEntry) {
	deadline := time.Now().Add(time.Duration(loginTimeoutMS) * time.Millisecond)
	interval := time.Duration(qrPollIntervalMS) * time.Millisecond

	for {
		if time.Now().After(deadline) {
			f.markFailed(e, "raccoon: 登录超时（300 秒内未完成）")
			return
		}
		time.Sleep(interval)

		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(requestTimeoutMS)*time.Millisecond)
		res, err := f.p.client.PollQRCode(ctx, e.qrCode)
		cancel()

		// ⚠ 任何异常都降级为 pending 继续轮询（不终止、不换 code）：
		// 网络抖动不该让用户正在扫的二维码作废。
		if err != nil {
			continue
		}

		switch res.Status {
		case QRStatusLogging:
			if res.ExpiredAt != "" {
				e.mu.Lock()
				e.expiredAt = res.ExpiredAt
				e.mu.Unlock()
			}
		case QRStatusCanceled:
			// ⚠ 换一个新 code（否则用户卡死），并更新 URL
			if code, gerr := GenerateQRCode(); gerr == nil {
				e.mu.Lock()
				e.qrCode = code
				e.qrURL = QRLoginURL(code)
				e.rotations++
				e.mu.Unlock()
			}
		case QRStatusSuccess:
			f.finishLogin(e, res.AccessToken, res.RefreshToken)
			return
		}
		// pending 与其它情况：继续
	}
}

// finishLogin 用拿到的 token 构造凭据并标记就绪。
func (f *loginFlow) finishLogin(e *loginEntry, accessToken, refreshToken string) {
	a := &Auth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
	if ms := decodeJWTExpMS(accessToken); ms > 0 {
		a.ExpiresAt = fmt.Sprintf("%d", ms)
	}
	// 尽力补全展示字段（失败不影响登录，见 EnrichCredential）
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(requestTimeoutMS)*time.Millisecond)
	f.p.client.EnrichCredential(ctx, a)
	cancel()
	if a.Nickname == "" {
		a.Nickname = a.DisplayUID()
	}

	cred := gateway.Credential{
		Provider: providerID,
		UID:      a.UID(),
		Nickname: a.DisplayUID(),
		Secret:   a,
	}
	if ms := a.ExpiresAtMS(); ms > 0 {
		cred.ExpiresAt = time.UnixMilli(ms)
	}

	// 登录成功后领一次性登录奖励（失败只记日志，不影响登录）
	if res, err := f.p.client.ClaimLoginGrant(context.Background(), a); err == nil && res.Claimed {
		// 有意不在这里打日志噪声：由装配层的管理端点展示
		_ = res
	}

	e.mu.Lock()
	e.ready = true
	e.cred = cred
	e.mu.Unlock()
}

// markFailed 标记会话失败。
func (f *loginFlow) markFailed(e *loginEntry, reason string) {
	e.mu.Lock()
	e.failRsn = reason
	e.mu.Unlock()
}

// 编译期断言。
var _ gateway.LoginFlow = (*loginFlow)(nil)
