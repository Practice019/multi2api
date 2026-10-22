// browserlogin.go 页内「添加账号」的**浏览器登录**流程。
//
// # 复刻自 register-machine 的 browser-login.mjs
//
// 那条链路的判据是：
//
//	起有头浏览器（独立 profile）→ 打开站点 → 用户自己登录
//	→ 每 2 秒读一次 cookie，出现 monkeycode_ai_session 即成功
//	→ GET /users/status 校验 → 补签 key → 落盘
//
// 为什么不走"粘贴 session"：
//
//	① 用户不用懂 F12 / DevTools / cookie 是什么
//	② 密码、手机、OAuth 三条路都能走（浏览器里他自己过验证码与滑块）
//	③ 读到的 cookie 形态一定对（不存在复制错行）
//
// 粘贴式保留为**兜底**（服务器部署 / 无浏览器的环境）—— 见 login.go 的 ManualLogin。
//
// # 与 register-machine 的差异
//
// 它用 `spawnChrome` 自己挑 Chrome 路径；本仓已有 `internal/browseropen`
// （探测 Edge/Chrome + 独立 profile + 清理），所以直接复用它，不重复实现。
package monkeycode

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/browseropen"
)

// cookieName 登录态 cookie 的名字。
const cookieName = "monkeycode_ai_session"

// LoginStage 登录会话的阶段（下发给前端显示）。
type LoginStage string

const (
	StageStarting LoginStage = "starting"
	StageBrowser  LoginStage = "browser"
	StageVerify   LoginStage = "verify"
	StageSave     LoginStage = "save"
	StageDone     LoginStage = "done"
	StageError    LoginStage = "error"
	StageCancel   LoginStage = "cancelled"
)

// BrowserLoginStatus 一次浏览器登录的进度。
//
// 字段与 register-machine 的 state 逐字对应（前端按它们显示文案）。
type BrowserLoginStatus struct {
	ID          string     `json:"id"`
	Stage       LoginStage `json:"stage"`
	Message     string     `json:"message"`
	StartedAt   int64      `json:"started_at"`
	FinishedAt  int64      `json:"finished_at,omitempty"`
	Region      string     `json:"region"`
	LoginURL    string     `json:"login_url"`
	OK          bool       `json:"ok"`
	CookieFound bool       `json:"cookie_found"`
	UserID      string     `json:"user_id,omitempty"`
	Name        string     `json:"name,omitempty"`
	Plan        string     `json:"plan,omitempty"`
	ModelsOK    bool       `json:"models_ok"`
	ModelsCount int        `json:"models_count"`
	Error       string     `json:"error,omitempty"`
	Cancelled   bool       `json:"cancelled,omitempty"`
	CookiesSeen int        `json:"cookies_seen"`
}

// browserSession 一次进行中的浏览器登录。
type browserSession struct {
	status  BrowserLoginStatus
	mu      sync.Mutex
	cancel  context.CancelFunc
	closure func() // 清理（关浏览器 + 删 profile）
}

// setStatus 线程安全地改状态。
func (s *browserSession) setStatus(fn func(*BrowserLoginStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.status)
}

// snapshot 线程安全地取状态副本。
func (s *browserSession) snapshot() BrowserLoginStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// StartBrowserLogin 起一次浏览器登录，立即返回会话 ID。
//
// 进度用 BrowserLoginStatusOf(id) 轮询 —— 与 register-machine 同款
//（那边是 startMonkeyCodeLogin + getMonkeyCodeLoginStatus）。
func (p *Provider) StartBrowserLogin(region string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(region) == "" {
		region = RegionCN
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	id := fmt.Sprintf("mklogin-%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	sess := &browserSession{}
	sess.status = BrowserLoginStatus{
		ID:        id,
		Stage:     StageStarting,
		Message:   "正在启动浏览器…",
		StartedAt: time.Now().UnixMilli(),
		Region:    region,
		LoginURL:  hostOf(region) + "/",
	}
	sess.cancel = cancel

	p.mu.Lock()
	if p.browserLogins == nil {
		p.browserLogins = map[string]*browserSession{}
	}
	// 清理已完成超过 30 分钟的会话。
	now := time.Now().UnixMilli()
	for k, v := range p.browserLogins {
		st := v.snapshot()
		if st.FinishedAt > 0 && now-st.FinishedAt > 30*60*1000 {
			delete(p.browserLogins, k)
		}
	}
	p.browserLogins[id] = sess
	p.mu.Unlock()

	go p.runBrowserLogin(ctx, sess, region, timeout)
	return id, nil
}

// BrowserLoginStatusOf 取某个会话的进度（不存在返回 nil）。
func (p *Provider) BrowserLoginStatusOf(id string) *BrowserLoginStatus {
	p.mu.RLock()
	sess := p.browserLogins[strings.TrimSpace(id)]
	p.mu.RUnlock()
	if sess == nil {
		return nil
	}
	st := sess.snapshot()
	return &st
}

// CancelBrowserLogin 取消一次登录。
func (p *Provider) CancelBrowserLogin(id string) bool {
	p.mu.RLock()
	sess := p.browserLogins[strings.TrimSpace(id)]
	p.mu.RUnlock()
	if sess == nil {
		return false
	}
	sess.setStatus(func(s *BrowserLoginStatus) { s.Cancelled = true })
	if sess.cancel != nil {
		sess.cancel()
	}
	return true
}

// hostOf 区域对应的网页基址。
func hostOf(region string) string {
	if region == RegionIntl {
		return hostIntl
	}
	return hostCN
}

// runBrowserLogin 执行一次浏览器登录的完整链路。
//
// 步骤与 register-machine 的 run() 一一对应：
//
//	① 起浏览器（独立 profile + CDP 端口）
//	② 轮询 cookie，直到出现 monkeycode_ai_session
//	③ 校验 session（fetchStatus）
//	④ 补签 key + 落盘
func (p *Provider) runBrowserLogin(ctx context.Context, sess *browserSession, region string, timeout time.Duration) {
	defer func() {
		if sess.closure != nil {
			sess.closure()
		}
	}()

	finish := func(stage LoginStage, msg string, ok bool) {
		sess.setStatus(func(s *BrowserLoginStatus) {
			s.Stage = stage
			s.Message = msg
			s.OK = ok
			s.FinishedAt = time.Now().UnixMilli()
			if !ok && stage == StageError && s.Error == "" {
				s.Error = msg
			}
		})
	}

	// ① 规划并启动浏览器。
	cmd, err := browseropen.Plan(runtime.GOOS, hostOf(region)+"/", browseropen.Opts{
		Isolated: true,
		TempDir:  p.profileRoot(),
	})
	if err != nil {
		finish(StageError, "启动浏览器失败："+err.Error(), false)
		return
	}
	port, err := freePort()
	if err != nil {
		finish(StageError, "分配 CDP 端口失败："+err.Error(), false)
		return
	}

	args := append([]string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
	}, cmd.Args...)
	args = append(args, hostOf(region)+"/")

	proc := exec.Command(cmd.Path, args...)
	proc.Stdout = nil
	proc.Stderr = nil
	if err := proc.Start(); err != nil {
		finish(StageError, "启动 "+cmd.Browser+" 失败："+err.Error(), false)
		return
	}
	profileDir := cmd.ProfileDir
	sess.closure = func() {
		if proc.Process != nil {
			_ = proc.Process.Kill()
		}
		if profileDir != "" {
			go func() {
				time.Sleep(1500 * time.Millisecond)
				_ = os.RemoveAll(profileDir)
			}()
		}
	}

	sess.setStatus(func(s *BrowserLoginStatus) {
		s.Stage = StageBrowser
		s.Message = "浏览器已打开（" + cmd.Browser + "），请在窗口里登录 MonkeyCode"
	})

	// 等 CDP 就绪。
	if _, err := cdpWaitReady(ctx, port, 25*time.Second); err != nil {
		finish(StageError, "浏览器调试端口未就绪："+err.Error(), false)
		return
	}

	// ② 轮询 cookie。
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var raw string
	for {
		select {
		case <-ctx.Done():
			finish(StageCancel, "已取消", false)
			return
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			finish(StageError, fmt.Sprintf("等待登录超时（%d 分钟）—— 没读到 %s", int(timeout.Minutes()), cookieName), false)
			return
		}

		cookies, cerr := p.readSessionCookie(ctx, port, hostOf(region))
		if cerr != nil {
			continue // 页面还在跳转，下一轮再试
		}
		sess.setStatus(func(s *BrowserLoginStatus) { s.CookiesSeen = cookies.count })
		if cookies.session != "" {
			raw = cookies.session
			break
		}
		elapsed := int(time.Since(time.UnixMilli(sess.snapshot().StartedAt)).Minutes())
		sess.setStatus(func(s *BrowserLoginStatus) {
			s.Message = fmt.Sprintf("等待登录中…（已 %d 分钟，当前 cookie %d 个）", elapsed, cookies.count)
		})
	}

	sess.setStatus(func(s *BrowserLoginStatus) {
		s.CookieFound = true
		s.Stage = StageVerify
		s.Message = "已读到 session，正在校验…"
	})

	// ③ 校验。
	a := &Auth{Session: raw, Region: region}
	st, err := p.probeStatus(ctx, a)
	if err != nil {
		finish(StageError, "读到 session 但校验失败："+shortErr(err), false)
		return
	}
	a.UserID = strings.TrimSpace(st.User.ID)
	a.Nickname = strings.TrimSpace(st.User.Name)
	sess.setStatus(func(s *BrowserLoginStatus) {
		s.UserID = a.UserID
		s.Name = a.Nickname
	})

	if list, merr := p.client.FetchModels(ctx, a); merr == nil {
		learnInterfaces(list)
		sess.setStatus(func(s *BrowserLoginStatus) {
			s.ModelsOK = true
			s.ModelsCount = len(list)
		})
	}

	// ④ 补签 key + 落盘。
	sess.setStatus(func(s *BrowserLoginStatus) {
		s.Stage = StageSave
		s.Message = "正在保存账号…"
	})
	if strings.TrimSpace(a.APIKey) == "" || strings.TrimSpace(a.SigningSecret) == "" {
		pk, kerr := p.client.CreateAPIKey(ctx, a, "multi2api")
		if kerr != nil {
			finish(StageError, "已登录但签发模型 key 失败："+shortErr(kerr), false)
			return
		}
		a.APIKey = pk.APIKey
		a.SigningSecret = pk.SigningSecret
	}
	a.AddedAt = time.Now().Format(time.RFC3339)
	if _, err := SaveFile(p.authDir, a); err != nil {
		finish(StageError, "落盘失败："+err.Error(), false)
		return
	}
	p.mu.Lock()
	if p.creds == nil {
		p.creds = map[string]*Auth{}
	}
	p.creds[a.UID()] = a
	p.mu.Unlock()
	log.Printf("monkeycode: 浏览器登录成功 %s（region=%s）", a.DisplayName(), a.RegionOrDefault())
	finish(StageDone, "登录成功："+a.DisplayName(), true)
}

// cookieScan 一次 cookie 读取的结果。
type cookieScan struct {
	count   int
	session string
}

// readSessionCookie 读一次 cookie，挑出 monkeycode_ai_session。
func (p *Provider) readSessionCookie(ctx context.Context, port int, host string) (cookieScan, error) {
	list, err := cdpListTargets(ctx, port)
	if err != nil {
		return cookieScan{}, err
	}
	t := cdpPickTarget(list, host)
	if t == nil {
		return cookieScan{}, fmt.Errorf("没有可用页面 target")
	}
	cookies, err := cdpReadCookies(ctx, t.WebSocketDebugger)
	if err != nil {
		return cookieScan{}, err
	}
	var out cookieScan
	for _, c := range cookies {
		if c.Name == cookieName && strings.TrimSpace(c.Value) != "" {
			out.session = strings.TrimSpace(c.Value)
		}
		if strings.Contains(c.Domain, "monkeycode") {
			out.count++
		}
	}
	return out, nil
}

// freePort 找一个空闲的本地端口。
//
// 手法：监听 :0 让内核分配，读回端口号后立刻关掉。
// ⚠ 关掉到浏览器真正监听之间有一个极小的竞态窗口，但 127.0.0.1 上的
// 随机高位端口被抢的概率可忽略；真撞上了浏览器会启动失败并明确报错。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("意外的监听地址类型 %T", l.Addr())
	}
	return addr.Port, nil
}

// profileRoot 浏览器 profile 的父目录（跟凭证目录放一起，便于清理）。
func (p *Provider) profileRoot() string {
	if p.authDir == "" {
		return ""
	}
	return p.authDir + string(os.PathSeparator) + ".browser-profiles"
}

// 编译期小工具：确认 http 包仍被使用（handler 在别处）。
var _ = http.MethodGet

