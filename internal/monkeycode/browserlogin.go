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
	"path/filepath"
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
	// browserPID 本次启动的浏览器主进程 pid（0 = 没起过）。
	//
	// ⚠ 存在的理由（T-031）：defer 里的兜底清理**不能依赖 closure 是否已赋值** ——
	// closure 在 Start 成功之后才赋值，而"卡在中间被取消/超时/进程被杀"这些路径
	// 恰恰是最需要清理的。有了 pid，无论 closure 在不在，都能按 pid 再补一刀。
	pid int
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
// （那边是 startMonkeyCodeLogin + getMonkeyCodeLoginStatus）。
func (p *Provider) StartBrowserLogin(region string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(region) == "" {
		region = RegionCN
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	id := fmt.Sprintf("mklogin-%d", time.Now().UnixNano())
	// ★ 陈旧 profile 巡检（T-031，**每进程一次**）。
	//
	// 为什么放在这里而不是"服务启动时"：那要改 cmd/server 的启动顺序（本轮不动它），
	// 而"第一次有人点浏览器登录"正是最需要止血的时刻 —— 用户机器上 22 小时攒了
	// 111 个 wb2api-login-*，这条巡检会把超过 1 小时的清掉（**正在用的 mtime 是新的，
	// 绝不会被误删**）。
	//
	// ⚠ 它只是**止血**，不是根因修复：真正保证删除的时机是进程自己活着走到 closure。
	//   进程被杀（测试二进制结束/服务重启/Ctrl-C）时那条路永远走不到。
	sweepStaleProfilesOnce.Do(func() {
		SweepStaleLoginProfiles(p.profileRoot(), time.Hour)
	})
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
			return
		}
		// ⚠ 兜底（T-031）：closure 还没赋值（或已被并发路径取走）时，按 pid 再补一刀。
		//
		// 为什么需要它：closure 在 `proc.Start()` **成功之后**才赋值 ——
		// 于是"起好了进程、但还没走到赋值那行就返回"在原理上会漏（虽然当前代码里
		// 每个 Start 之后的 return 都在 defer 覆盖范围内，但那是**时序上的巧合**，
		// 不是结构性保证：将来在中间插一个早返回就会破）。
		// 有了这条，清理**不再依赖 closure 是否被赋值**。
		if pid := sess.pidOf(); pid > 0 {
			killPidTreeBestEffort(pid)
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
	// ⚠ 必须在 Start **之前**：Unix 要在这里设 Setpgid（进程组只能出生时成型，
	//   事后改不了）；Windows 是空操作 —— 但共用同一调用点，两平台写法不会分叉。
	prepareProcessForTreeKill(proc)
	if err := proc.Start(); err != nil {
		finish(StageError, "启动 "+cmd.Browser+" 失败："+err.Error(), false)
		return
	}
	// 把浏览器挂进"能整体终止"的容器（Windows: Job Object；Unix: 进程组已在 Start 时成型）。
	// ⚠ 失败**不阻断登录** —— 清理手段降级（Windows 退化成 taskkill）不该让用户登不上。
	killTree, jerr := attachProcessTree(proc.Process)
	if jerr != nil {
		logf("monkeycode: 浏览器未能挂入可级联终止的容器（清理降级，可能留残留）：%v", jerr)
	}
	profileDir := cmd.ProfileDir
	sess.setPID(proc.Process.Pid)
	sess.closure = func() {
		// ★★ 原来这里只有 `proc.Process.Kill()` —— 它**不级联子进程**（用户实测 18 个
		//   孤儿 msedge、111 个残留 profile）。整棵树清理见 killtree_*.go。
		if killTree != nil {
			if err := killTree(); err != nil {
				logf("monkeycode: 清理浏览器进程树出错（可能有残留）：%v", err)
			}
		}
		// ★★ profile 清理与进程清理**解耦**（T-031）：
		//
		// 老写法是"睡 1.5 秒再 RemoveAll" —— 两个毛病：
		//   ① 1.5 秒是**拍的**：Chromium 写盘/持有句柄可能更久 ⇒ RemoveAll 静默失败；
		//   ② RemoveAll 的失败**没人看** ⇒ 111 个残留就是这么攒出来的（22 小时）。
		// 现在：等**进程真退出**（最多 10 秒）再删，删不掉就**记一行 warn**。
		if profileDir != "" {
			go removeProfileWhenGone(profileDir, proc.Process.Pid)
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
//
// ★ T-456：**不再返回空串**。
//
// # 为什么（用户实测报障）
//
// 原来没配 authDir 时返回 ""，看起来"什么都没做"，实际是**把决定权交给了下游** ——
// makeProfileDir 会回落到 `os.TempDir()`（用户的 %TEMP% 就是 D:\temp）。
// 实测后果：`go test ./...` 里那条会真起浏览器的用例，每次都往 D:\temp 扔一个
// **525 MB** 的 Edge profile，攒了 **24 个 ≈ 12 GB**，1 小时吃掉 8 GB 磁盘。
//
// ⇒ 宁可建在**我们自己的地盘**（工作目录下的 .browser-profiles），
//
//	也不要往系统临时目录里堆 —— 那是用户不知道谁建的地方。
func (p *Provider) profileRoot() string {
	if p.authDir == "" {
		return ".browser-profiles"
	}
	return p.authDir + string(os.PathSeparator) + ".browser-profiles"
}

// 编译期小工具：确认 http 包仍被使用（handler 在别处）。
var _ = http.MethodGet

// ── T-031：兜底清理与 profile 清理（与进程清理解耦）─────────────────────

// setPID / pidOf 记本次启动的浏览器主进程 pid（兜底清理用，见 browserSession.pid 的注释）。
func (s *browserSession) setPID(pid int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pid = pid
	s.mu.Unlock()
}

func (s *browserSession) pidOf() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

// killPidTreeBestEffort 按 pid 尽力杀整棵树（**幂等**：进程没了就是成功）。
//
// ⚠ 与 killProcessTree 的关系：那个吃 `*os.Process`（能走 Job Object，最可靠）；
//
//	这个是"只有 pid 可用"时的兜底（closure 没赋值那条路径）。两者都不该 panic。
func killPidTreeBestEffort(pid int) {
	if pid <= 0 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil || p == nil {
		return
	}
	if err := killProcessTree(p); err != nil {
		logf("monkeycode: 兜底清理浏览器进程树（pid=%d）出错：%v", pid, err)
	}
}

// removeProfileWhenGone 等浏览器进程真的退出，再删 profile 目录。
//
// # 为什么不能"睡固定时长再删"
//
// 老写法 `sleep 1.5s; RemoveAll` 是**拍的**：Chromium 可能还在写盘/持有句柄，
// 于是 RemoveAll 静默失败 —— 用户机器上就这样攒了 **111 个** `wb2api-login-*`
// （22 小时跨度）。两条修正：
//
//	① 判据从"等了多久"换成"**进程还在不在**"（轮询，最多 10 秒）
//	② 删不掉**必须留痕**（warn 日志）—— 静默失败正是它攒到 111 个的原因
func removeProfileWhenGone(dir string, pid int) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := os.RemoveAll(dir); err != nil {
		logf("monkeycode: ⚠ 临时 profile 未能删除（残留，需人工清理）：%s —— %v", dir, err)
		return
	}
	logf("monkeycode: 临时 profile 已清理：%s", dir)
}

// processAlive 报告 pid 还活着没（Windows/Unix 各一套，见 process_alive_*.go）。

// SweepStaleLoginProfiles 清掉陈旧的临时 profile（**止血**用，不解决根因）。
//
// # 为什么需要它
//
// 真正保证"profile 一定被删"的时机是**进程自己活着走到 closure**。而当整个进程
// 被杀（测试二进制结束、服务重启、Ctrl-C）时，那条路永远不会走到 —— 谁也没机会删。
// 所以还需要一个"下次启动时把过期的扫掉"的兜底：它不解决根因，但能防止无限累积。
//
// 判据用**目录 mtime 超过 `olderThan`**（默认 1 小时，见调用点）—— 正在使用的
// profile 的 mtime 是新的，绝不会被误删。
func SweepStaleLoginProfiles(root string, olderThan time.Duration) (removed, failed int) {
	if strings.TrimSpace(root) == "" {
		return 0, 0
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return 0, 0 // 目录不存在/读不动都不算错误（首次运行就是这种状态）
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), loginProfilePrefix) {
			continue
		}
		// ★★ T-448：**先杀掉"启动太久"的孤儿进程** —— 必须在 mtime 闸门**之前**。
		//
		// # 为什么必须在闸门之前（实测死锁）
		//
		// 孤儿进程会持续刷新 profile 的 mtime ⇒ 它永远是"新的" ⇒ 被下面的闸门 continue 掉
		// ⇒ **永远清不掉**。实测：造场景（mtime 老 + msedge 指向它）后 Sweep 报告 removed=1，
		// 但那个目录和 8 个孤儿都还在 —— 因为 mtime 被刷成了"现在"。
		// 而"正在登录"与"孤儿在刷新"在 mtime 上**完全同形** ⇒ mtime 原理上判不了这件事。
		//
		// # 判据：进程启动时长（与 mtime 无关 ⇒ 不会被孤儿自己刷新掉）
		//
		// 登录窗口 10 分钟（LoginFlow 的 timeout）⇒ 启动 > orphanProcAge 的进程
		// **不可能是当前这次登录** ⇒ 一定是历史遗留的孤儿。
		//
		// ⚠ full 必须在这里就定义（下面 mtime 流程里也要用）—— 它在本次改动前
		//   定义在闸门之后，所以"闸门之前要用它"的代码编译不过（T-448 实测红过一次）。
		full := filepath.Join(root, e.Name())
		const orphanProcAge = 30 * time.Minute
		if infos, perr := listProcsFn("msedge.exe", loginProfilePrefix, e.Name()); perr == nil {
			killed := 0
			for _, pi := range infos {
				if pi.Age < orphanProcAge {
					continue // 可能是当前登录 ✓ 不碰
				}
				logf("monkeycode: 发现孤儿浏览器 pid=%d（已运行 %s）占用 %s，杀掉",
					pi.PID, pi.Age.Round(time.Minute), e.Name())
				if kerr := killPIDFn(pi.PID); kerr != nil {
					logf("monkeycode: 杀孤儿失败 pid=%d: %v", pi.PID, kerr)
					continue
				}
				killed++
			}
			if killed > 0 {
				// 等句柄释放（实测 200ms 够用；进程刚死时文件句柄可能还没完全释放）
				time.Sleep(200 * time.Millisecond)
				// 进程都杀光了 ⇒ 这个 profile 一定是垃圾 ⇒ **直接删，不再看 mtime**。
				// 理由：已证明"有进程引用它"且"那些进程都是孤儿" ⇒ 不需要 mtime 佐证。
				if rerr := os.RemoveAll(full); rerr != nil {
					failed++
					logf("monkeycode: ⚠ 删孤儿 profile 失败：%s —— %v", full, rerr)
				} else {
					removed++
				}
				continue // 已处理完，不要再走下面的 mtime 流程（否则可能重复删）
			}
		}

		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue // 新的（可能正在用）⇒ 不碰
		}
		// ★ T-446：删之前**先杀掉引用该 profile 的孤儿浏览器**。
		//
		// # 为什么必须这一步（实测因果，2026-11-09）
		//
		// 只删目录时 RemoveAll 会因文件被占用而失败 —— 现场 165 个**全失败**；
		// 先杀进程再删 ⇒ 同 165 个**全成功**（磁盘 97.94% → 83.8%）。
		// 孤儿来自**异常路径**（用户中途放弃 / 轮询超时 / 服务重启使 Job Object 消失），
		// 正常路径已有树杀（killtree_windows.go）+ 等退出再删（removeProfileWhenGone）。
		//
		// # 安全（误杀防线）
		//
		// 只杀命令行**同时**含 loginProfilePrefix 与该目录**全名**的进程
		//（AND 语义 ⇒ 不会碰到用户自己的浏览器，也不会杀掉正在登录中的那个别的 profile）。
		// ⚠ 走可注入的 listProcsFn/killPIDFn，以便测试验证这条 AND 判据（见 procsweep.go）。
		// ★ T-448/T-449：这里**也要按年龄过滤** —— 否则"mtime 老"的目录会把
		// "只跑了 5 分钟的进程"也杀掉，而那可能正是**当前这次登录**
		//（登录慢/卡住时 profile 的 mtime 不更新 ⇒ 老的 mtime ≠ 没有活跃登录）。
		// 实测：TestSweepKeepsRecentProcEvenIfMtimeOld 抓到过这个越界 ✓
		if infos, perr := listProcsFn("msedge.exe", loginProfilePrefix, e.Name()); perr == nil && len(infos) > 0 {
			for _, pi := range infos {
				if pi.Age < orphanProcAge {
					continue // 可能是当前登录 ✓ 不碰
				}
				logf("monkeycode: 巡检发现孤儿 msedge pid=%d（已运行 %s）占用 %s，先杀掉再删",
					pi.PID, pi.Age.Round(time.Minute), e.Name())
				if kerr := killPIDFn(pi.PID); kerr != nil {
					logf("monkeycode: 杀孤儿失败 pid=%d: %v", pi.PID, kerr)
				}
			}
			// 等句柄释放：进程刚被杀时文件句柄可能还没完全释放，
			// 立刻 RemoveAll 仍会失败。200ms 是现场验证够用的值。
			time.Sleep(200 * time.Millisecond)
		}

		if rerr := os.RemoveAll(full); rerr != nil {
			failed++
			logf("monkeycode: ⚠ 陈旧 profile 清理失败：%s —— %v", full, rerr)
			continue
		}
		removed++
	}
	if removed > 0 || failed > 0 {
		logf("monkeycode: 陈旧临时 profile 巡检：已清 %d 个，失败 %d 个（早于 %v 的）",
			removed, failed, olderThan)
	}
	return removed, failed
}

// sweepStaleProfilesOnce 让陈旧 profile 巡检每进程只跑一次（见 StartBrowserLogin 的调用点）。
const loginProfilePrefix = "wb2api-login-"

var sweepStaleProfilesOnce sync.Once
