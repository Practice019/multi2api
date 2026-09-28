// login.go Qoder 的登录流程（PKCE 设备码轮询，**不起本地端口**）。
//
// # 与其它上游的差异
//
//	codearts / lobsterai / trae → 本地回调服务器（要绑端口）
//	cline                        → WorkOS 设备码（看响应体 error 字段）
//	qoder                        → PKCE 设备码（**看 HTTP 404**）
//	raccoon                      → 微信扫码
//
// Qoder 也不起本地端口 —— 所以没有"服务器部署下回调打不进本机"的问题。
package qoder

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// loginEntry 一次在途登录。
type loginEntry struct {
	state    string
	issuedAt time.Time
	ready    bool
	cred     gateway.Credential
	failRsn  string
	mu       sync.Mutex
}

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	p   *Provider
	mu  sync.Mutex
	ses map[string]*loginEntry
}

// Configured 是否可走登录流程。
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
//
// ⚠ 两个 Qoder 实例（国际版 / 中国版）都靠这三个方法获得登录能力 ——
// 它们是同类型的两个值，方法集相同。

// Start 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Start() (string, string, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("qoder: 登录流程未初始化")
	}
	return lf.Start()
}

// Poll 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("qoder: 登录流程未初始化")
	}
	return lf.Poll(state)
}

// Configured 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Configured() bool {
	lf, ok := p.LoginFlow()
	return ok && lf.Configured()
}

// Start 发起一次登录：生成 state + 授权 URL，后台轮询。
//
// 返回 (state, authURL, error)。
func (f *loginFlow) Start() (string, string, error) {
	if f == nil || f.p == nil {
		return "", "", errors.New("qoder: 登录流程未配置")
	}
	state, err := randomState()
	if err != nil {
		return "", "", fmt.Errorf("qoder: 生成 state 失败: %w", err)
	}
	e := &loginEntry{state: state, issuedAt: time.Now()}
	f.mu.Lock()
	f.ses[state] = e
	f.mu.Unlock()

	go f.pollLoop(e)
	return state, f.p.client.DeviceSelectURL(state), nil
}

// randomState 生成随机 state（32 位 hex）。
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// pollLoop 后台轮询直到成功/终态/超时。
//
// # ⚠ 判据：HTTP 404 = 用户尚未授权（**继续轮询，不是错误**）
//
// 实测依据：该端点返回 404 而任意不存在的路径返回 401 ——
// 说明它被网关豁免认证、由业务层报「会话未就绪」。
//
// 网络失败连续 pollMaxFailures 次才放弃；成功拿到响应即归零。
func (f *loginFlow) pollLoop(e *loginEntry) {
	deadline := time.Now().Add(time.Duration(loginTimeoutMS) * time.Millisecond)
	interval := time.Duration(pollIntervalMS) * time.Millisecond
	failures := 0

	for {
		if time.Now().After(deadline) {
			f.markFailed(e, "qoder: 登录超时（300 秒内未完成）")
			return
		}
		time.Sleep(interval)

		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(requestTimeoutMS)*time.Millisecond)
		res, err := f.p.client.PollDeviceToken(ctx, e.state)
		cancel()

		if err != nil {
			failures++
			if failures >= pollMaxFailures {
				f.markFailed(e, fmt.Sprintf("qoder: 无法连接登录服务（连续 %d 次失败）：%v",
					pollMaxFailures, err))
				return
			}
			continue
		}
		failures = 0

		if res.Pending {
			continue
		}
		f.finishLogin(e, res.AccessToken, res.RefreshToken)
		return
	}
}

// finishLogin 构造凭据并标记就绪。
func (f *loginFlow) finishLogin(e *loginEntry, accessToken, refreshToken string) {
	a := &Auth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ProductID:    f.p.productID,
	}
	// machine_id：续期请求体要用它，登录时生成一次
	if mid, err := randomState(); err == nil {
		a.MachineID = mid
	}
	// 尽力补全展示信息（失败不影响登录）
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(requestTimeoutMS)*time.Millisecond)
	ui := f.p.client.FetchUserInfo(ctx, a)
	cancel()
	a.UID = ui.UID
	a.Nickname = ui.Nickname
	if a.UID == "" {
		a.UID = DerivedUID(accessToken)
	}
	if a.Nickname == "" {
		a.Nickname = shortUID(a.UID)
	}

	cred := gateway.Credential{
		Provider: f.p.productID,
		UID:      a.UIDValue(),
		Nickname: a.Nickname,
		// ⚠ 必须是 authFile 包装，不能直接放 `a` —— 核心落盘要求
		// Secret 实现 MarshalAuthFile（只有上游知道自己凭证的文件名与形状）。
		Secret: &authFile{a: a},
	}
	if ms := a.ExpiresAtMS(); ms > 0 {
		cred.ExpiresAt = time.UnixMilli(ms)
	}

	e.mu.Lock()
	e.ready = true
	e.cred = cred
	e.mu.Unlock()
}

// Poll 查询授权结果。未完成时返回 gateway.ErrLoginPending。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	if f == nil {
		return gateway.Credential{}, errors.New("qoder: 登录流程未配置")
	}
	f.mu.Lock()
	e, ok := f.ses[state]
	f.mu.Unlock()
	if !ok {
		return gateway.Credential{}, errors.New("qoder: 登录会话不存在或已结束，请重新发起")
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

func (f *loginFlow) markFailed(e *loginEntry, reason string) {
	e.mu.Lock()
	if e.failRsn == "" {
		e.failRsn = reason
	}
	e.mu.Unlock()
}

var _ gateway.LoginFlow = (*loginFlow)(nil)

// ── 凭证落盘包装 ────────────────────────────────────────────────────────

// authFile 让一份 *Auth 满足核心的落盘窄接口（MarshalAuthFile）。
//
// # 为什么用包装类型而不是给 *Auth 直接加方法
//
// `*Auth` 有两个身份，且**形状不同**：
//
//	池内 secret   运行期用的结构
//	落盘文件      嵌套形 {"auth":{...},"account":{...}}（见 MarshalAuthFile）
//
// 让同一个类型承担两者，意味着"池里那份"和"盘上那份"共用一个序列化路径，
// 而它们的字段集并不相同。包装类型把"落盘形态"这件事显式化，
// 也与 trae / mimo / loomy / codearts 的既有做法一致。
type authFile struct{ a *Auth }

// MarshalAuthFile 返回 (文件名, 内容) —— 核心 pollViaFlow 的 authFileWriter 契约。
//
// ⚠ 不实现它就等于"登录不了"：浏览器授权会成功，但核心落盘那一步
// 会以 501 拒绝（「该上游的凭证结构尚未接入落盘」）。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("qoder: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, err
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("qoder: 凭证文件名为空")
	}
	return name, raw, nil
}