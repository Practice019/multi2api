// login.go Cline 的登录流程（WorkOS 设备码轮询）。
//
// # 与其它上游的差异（这是本包最"干净"的地方）
//
//	codearts / lobsterai → 本地回调服务器收 code（要绑端口）
//	qoder                → PKCE 设备码轮询（404 = 尚未授权）
//	mimo                 → 加密回跳 URL 粘贴
//	cline                → WorkOS 设备码轮询（authorization_pending = 尚未授权）
//
// Cline **不起本地端口**，也就没有"服务器部署下回调打不进本机"这类问题
// （那正是别的上游需要 manual 模式的原因）。
//
// # 两步式返回（不是风格偏好，是浏览器硬约束）
//
// startLogin 必须**立即**返回授权 URL，然后后台轮询。原因是 window.open
// 只在用户点击后的短暂窗口（transient activation，约 5 秒）内有效；
// 若把"拿设备码 → 打开页面 → 等授权"做成一次阻塞调用，调用方拿到 URL 时
// 手势已过期，弹窗被拦截。
//
// 这里与 Qoder 略有不同：设备码授权请求本身是**网络调用**，必须先 await 它
// 拿到 URL —— 但那只是一次快速的 POST（实测数百毫秒），不涉及用户等待，
// 仍能满足"立即返回 URL"的要求。
package cline

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
	state    string
	device   DeviceAuthorization
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
	// cancel 用于 close()：外部请求取消时中止后台轮询。
	cancel context.CancelFunc
}

// LoginFlow 返回（并缓存）本 Provider 的登录流程实例。
//
// ⚠ 必须缓存：每次新建会让 sessions map 各是一份空的，于是 start 存下的会话
// 在 poll 里找不到。其它上游端到端实测抓过这个 bug。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil {
		return nil, false
	}
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, ses: map[string]*loginEntry{}}
	})
	return p.loginCached, true
}

// Configured 报告这份部署真的能走登录流程吗（见 gateway.LoginFlow 的注释）。
//
// Cline 的登录只需要能访问 WorkOS，没有"是否配了 OAuth 客户端"这一层，
// 所以恒为 true（除非 Provider 自身为空）。
func (f *loginFlow) Configured() bool { return f != nil && f.p != nil }

// ── 让 *Provider 直接满足 gateway.LoginFlow ────────────────────────────────
//
// ⚠ 这三个转发方法是**必需的**，不是风格问题。
//
// 核心用 `gateway.ExtOf[LoginFlow](p)` 发现扩展点，那是**类型断言**
// `p.(LoginFlow)`；而访问器 `LoginFlow() (LoginFlow, bool)` 的返回值
// 不参与方法集匹配 —— 断言对它**不可见**。
//
// 少了这三个方法，本包自己的测试全绿（测试走访问器），
// 但 `/admin/providers` 会下发 `login:null`，界面上没有「＋ 添加账号」按钮。
// 实测事故就是这个形态；现在由 gateway.RunProviderContract 的
// verifyExtensionsDiscoverable 守住（见那里的注释）。
//
// 与 trae / workbuddy / loomy / mimo 的写法一致。

// Start 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Start() (string, string, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("cline: 登录流程未初始化")
	}
	return lf.Start()
}

// Poll 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	lf, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("cline: 登录流程未初始化")
	}
	return lf.Poll(state)
}

// Configured 让 *Provider 直接满足 gateway.LoginFlow。
//
// 转发到流程实例的 Configured，而不是直接 `return p != nil`：
// 保持"配置判据只有一处"（将来 Cline 若引入需要配置的登录方式，
// 只改 loginFlow.Configured 一处即可，不会出现两份判据漂移）。
func (p *Provider) Configured() bool {
	lf, ok := p.LoginFlow()
	return ok && lf.Configured()
}

// Start 发起一次登录：拿设备码 + 授权 URL，并启动后台轮询。
//
// 返回 (state, authURL, error)。state 供 Poll 使用。
func (f *loginFlow) Start() (string, string, error) {
	if f == nil || f.p == nil {
		return "", "", errors.New("cline: 登录流程未配置")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(httpTimeoutMS)*time.Millisecond)
	defer cancel()

	dev, err := f.p.client.StartDeviceAuth(ctx)
	if err != nil {
		return "", "", err
	}
	loginURL := dev.LoginURL()
	if loginURL == "" {
		return "", "", errors.New("cline: 登录服务未返回授权地址")
	}

	state := dev.DeviceCode
	e := &loginEntry{state: state, device: dev, issuedAt: time.Now()}

	f.mu.Lock()
	f.ses[state] = e
	f.mu.Unlock()

	go f.pollLoop(e)
	return state, loginURL, nil
}

// Poll 查询授权结果。未完成时返回 gateway.ErrLoginPending。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	if f == nil {
		return gateway.Credential{}, errors.New("cline: 登录流程未配置")
	}
	f.mu.Lock()
	e, ok := f.ses[state]
	f.mu.Unlock()
	if !ok {
		return gateway.Credential{}, fmt.Errorf("cline: 登录会话不存在或已结束，请重新发起")
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
	// 取走后删除：凭证只能被取一次（与其它上游一致）
	f.mu.Lock()
	delete(f.ses, state)
	f.mu.Unlock()
	return cred, nil
}

// pollLoop 后台轮询直到成功/失败/超时。
//
// # 重试语义（照搬参照 cline-oauth.ts:230-307）
//
//	authorization_pending → 按当前间隔继续（**不是错误**）
//	slow_down             → 间隔**累积** +1s 后继续
//	网络失败              → 计数，连续 5 次才放弃；成功拿到响应即归零
//	其它错误              → 终态，直接结束
func (f *loginFlow) pollLoop(e *loginEntry) {
	deadline := time.Now().Add(time.Duration(e.device.ExpiresInMS) * time.Millisecond)
	interval := time.Duration(e.device.IntervalMS) * time.Millisecond
	if interval < pollMinIntervalMS*time.Millisecond {
		interval = pollMinIntervalMS * time.Millisecond
	}
	failures := 0

	for {
		if time.Now().After(deadline) {
			f.markFailed(e, "登录等待已超时，请重新发起登录")
			return
		}
		time.Sleep(interval)

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(httpTimeoutMS)*time.Millisecond)
		res, err := f.p.client.PollDeviceAuth(ctx, e.device.DeviceCode)
		cancel()

		if err != nil {
			// 本模块自己抛的确定性错误直接终止，不计入网络失败。
			if isServerError(err) {
				f.markFailed(e, err.Error())
				return
			}
			failures++
			if failures >= pollMaxFailures {
				f.markFailed(e, fmt.Sprintf("无法连接 Cline 登录服务（连续 %d 次失败）：%v",
					pollMaxFailures, err))
				return
			}
			continue
		}
		failures = 0 // 成功拿到响应即归零

		if res.SlowDown {
			// ⚠ 累积而不是重置：源码 intervalSeconds += 1。
			// 用固定间隔会在服务端要求降速后持续被限流。
			interval += slowDownStepMS * time.Millisecond
			continue
		}
		if res.Pending {
			continue
		}
		if res.AccessToken == "" {
			f.markFailed(e, "登录服务返回异常：token 响应缺少必要字段")
			return
		}

		// 第 3 步：用 WorkOS token 换 Cline 自己的 token
		rctx, rcancel := context.WithTimeout(context.Background(), time.Duration(httpTimeoutMS)*time.Millisecond)
		a, rerr := f.p.client.RegisterTokens(rctx, res.AccessToken, res.RefreshToken)
		rcancel()
		if rerr != nil {
			f.markFailed(e, rerr.Error())
			return
		}
		f.markReady(e, a)
		return
	}
}

// isServerError 判定错误是否为本模块自抛的确定性错误（而非网络抖动）。
//
// 参照项目的判据是 message 前缀 `登录服务返回异常`（SERVER_ERROR_PREFIX）。
func isServerError(err error) bool {
	return err != nil && len(err.Error()) >= len("登录服务返回异常") &&
		err.Error()[:len("登录服务返回异常")] == "登录服务返回异常"
}

// markReady 标记会话就绪。
func (f *loginFlow) markReady(e *loginEntry, a *Auth) {
	cred := gateway.Credential{
		Provider: providerID,
		UID:      a.UID(),
		Nickname: a.Nickname,
		// ⚠ 必须是 authFile 包装，不能直接放 `a`。
		//
		// 核心落盘（admin.go 的 pollViaFlow）要求 Secret 实现
		//
		//	MarshalAuthFile() (name string, raw []byte, err error)
		//
		// 因为**只有上游自己知道**凭证该写成什么文件名、什么字段形状。
		// 直接放 `a` 会让浏览器授权成功后停在
		//「该上游的凭证结构尚未接入落盘（LoginFlow 的 Secret 需要实现
		// MarshalAuthFile）」—— 用户看到的是"登录不了"。
		Secret: &authFile{a: a},
	}
	if a.ExpireTime > 0 {
		cred.ExpiresAt = time.UnixMilli(a.ExpireTime)
	}
	e.mu.Lock()
	e.ready = true
	e.cred = cred
	e.mu.Unlock()
}

// ── 凭证落盘包装 ────────────────────────────────────────────────────────

// authFile 让一份 *Auth 满足核心的落盘窄接口（MarshalAuthFile）。
//
// # 为什么用包装类型而不是给 *Auth 直接加方法
//
// `*Auth` 有两个身份，且**形状不同**：
//
//	池内 secret   运行期用的结构（含 FilePath 等不落盘字段）
//	落盘文件      嵌套形 {"auth":{…},"account":{…}}（见 MarshalAuthFile）
//
// 让同一个类型同时承担两者，就意味着"池里那份"和"盘上那份"共用一个
// 序列化路径 —— 而它们的字段集并不相同。包装类型把"落盘形态"这件事
// 显式化，也与 trae / mimo / loomy / codearts 的既有做法一致。
type authFile struct{ a *Auth }

// MarshalAuthFile 返回 (文件名, 内容) —— 核心 pollViaFlow 的 authFileWriter 契约。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("cline: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, err
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("cline: 凭证文件名为空")
	}
	return name, raw, nil
}

// markFailed 标记会话失败。
func (f *loginFlow) markFailed(e *loginEntry, reason string) {
	e.mu.Lock()
	e.failRsn = reason
	e.mu.Unlock()
}

// ManualLogin 实现 gateway.ManualLoginExt 吗？—— **不实现**。
//
// 见本文件顶部：设备码轮询不需要回调地址，所以没有"粘贴回跳 URL"这条逃生路径。
// 上游若需要它，核心会通过 ExtOf 探测，未实现即降级（前端不渲染那个输入框）。
var _ = (func() {
	// 编译期确认 *Provider 满足 LoginFlow（扩展点断言见 provider.go）
	var _ gateway.LoginFlow = (*loginFlow)(nil)
})
