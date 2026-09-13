// login.go —— 把本包的 OAuth 授权流程适配成 gateway.LoginFlow。
//
// # 本文件解决的核心难题：阻塞的 Wait 与轮询语义不匹配
//
// 两边的**等待模型完全不同**：
//
//	workbuddy（设备码）：Poll 每次**问一次**上游"用户授权了吗"，本身是轮询语义
//	codearts（授权码）：Session.Wait() 内部 select 到回调到达**或 TTL 超时**，
//	                    是一次最长 15 分钟的**阻塞**
//
// 所以**不能**在 HTTP handler 里直接调 Wait —— 那会占住请求 15 分钟，
// 前端超时、连接被中间层掐断，而授权其实随时可能完成。
//
// 解法：**把等待下沉到一个 goroutine，Poll 只读结果**。
//
//	首次 Poll → 起 goroutine 跑 Wait + Exchange，把结果投进 channel
//	后续 Poll → 非阻塞读 channel
//	            没结果 → 返回 gateway.ErrLoginPending（核心据此回 202）
//	            有结果 → 返回凭证
//
// 这样 Poll 永远是**瞬时**的，与 workbuddy 的调用方契约一致。
package codearts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// loginSession 一次进行中的授权，含"等待已下沉到后台"的状态。
type loginSession struct {
	oauth loginSessionHandle // 一次 OAuth 会话的句柄

	done chan loginResult // 后台 goroutine 完成后投递一次
	once sync.Once        // 保证只起一个等待 goroutine

	// startedAt 用于超时清理：用户点开授权页后**一次都没轮询**的情况下，
	// 会话也要能被回收（否则每点一次"添加账号"就泄漏一个监听端口）。
	startedAt time.Time
}

// loginSessionHandle 适配器对"一次授权会话"的最小需求。
//
// # 为什么抽这一层接口（而不是直接用 *Session）
//
// 测试**必须**能控制"用户何时完成授权"：真 `Session.Wait()` 会阻塞
// 到回调到达或 15 分钟 TTL，`deliver` 又是未导出的 —— 测试无法从外部放行。
//
// 不抽接口的话，那条最重要的断言（"Poll 立刻返回，不被阻塞"）
// 就**没法写** —— 而那正是本适配器存在的主要理由。
//
// `*Session` 天然满足它，所以产品路径零额外代码。
type loginSessionHandle interface {
	Wait() (string, error)
	// CloseCallback 关掉本地回调监听。
	//
	// ⚠ 必须在接口里 —— 每个会话占**一个真实监听端口**
	//（`net.Listen("tcp", "127.0.0.1:0")`）。会话被回收时若不关，
	// 端口会一直泄漏。源实现是命令行一次性使用，没这问题；
	// 接进常驻进程就必须关。
	CloseCallback()
}

// loginManager 适配器对"授权管理器"的最小需求（`*Manager` 满足）。
//
// ⚠ `Exchange` 的参数类型是 `*Session`（具体类型），但**适配器调用时
// 用的是 `loginSessionHandle`** —— 两者由 `sessionFor` 桥接。
// 这样测试可以注入"不是 *Session"的替身来放行等待。
type loginManager interface {
	Start() (*Session, error)
	Exchange(s *Session, code string) (json.RawMessage, error)
	HTTPClient() *http.Client
}

// sessionFor 把会话句柄还原成具体 `*Session`。
//
// 测试替身不是 `*Session` 时应返回 false，适配器据此走另一条路
// （测试路径不需要真 Exchange —— 见 login_test.go 的说明）。
func sessionFor(h loginSessionHandle) (*Session, bool) {
	s, ok := h.(*Session)
	return s, ok
}

// loginResult 后台等待的结果。
type loginResult struct {
	cred gateway.Credential
	err  error
}

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	// mgr 授权管理器。窄接口（`*Manager` 满足）—— 测试要注入替身。
	mgr loginManager
	// providerID 本上游的归属标识，由 *Provider 传入。
	providerID string
	// authDir 凭证落盘目录。同样由 Provider 传入 ——
	// `loginFlow` 是核心实际持有的对象，必须自带回答 AuthDir() 所需的信息
	//（不在这里回头读 *Provider：那会引入反向指针，且测试替身构造不出来）。
	authDir string

	mu       sync.Mutex
	sessions map[string]*loginSession
}

// 编译期断言：本类型必须满足核心认的形状。
var _ gateway.LoginFlow = (*loginFlow)(nil)

// ⚠ LoginFlow() 必须**缓存**实例 —— 这是端到端实测抓到的 bug。
//
// 第一版写成每次调用都 `return &loginFlow{...}` 新建，于是
// `sessions` map 每次都是空的：
//
//	/admin/login/start → 实例 A 存下会话
//	/admin/login/poll  → 实例 B（sessions 空）→ "授权会话不存在或已结束"
//
// **后果：codearts 的页内添加账号永远不可能成功。**
//
// # 为什么单测没抓到
//
// `newTestFlow` 是**直接构造一个 loginFlow 再连续调 Poll** ——
// 全程同一个实例，所以"每次调用都换实例"这件事**根本不会发生**。
// 单元测试测的是"一个 flow 内部的逻辑"，而 bug 在"flow 的获取方式"上。
//
// 抓到它的是**端到端**：`start` 之后我直打回调地址，
// 回调服务器**真能打通**（说明流程活着），但 `poll` 说会话不存在 ——
// 两个观察互相矛盾，唯一解释就是 poll 拿到的不是同一个 flow。
//
// 教训：**单测覆盖单元内部逻辑，覆盖不了"对象的生命周期"。**
// 这类 bug 只能靠端到端或"跨调用"的测试暴露。
// LoginFlow 返回本上游的登录流程；未配置时返回 false。
//
// ⚠ 这里同时是**两个用途的入口**：
//
//  1. `LoginFlow()` —— 给知道自己在问谁的调用方（返回 bool，语义清晰）
//  2. `Start`/`Poll`/`Configured` 挂在 *Provider 上 —— 让
//     `gateway.ExtOf[gateway.LoginFlow](p)` 认出来
//
// 方法集匹配是 Go 类型断言的硬要求："返回接口的访问器"
// 对断言**完全不可见**（这是 T10-d 实测踩到的坑）。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil || p.login == nil {
		return nil, false
	}
	// ⚠ 缓存，不每次新建 —— 见上面那段注释（端到端实测抓到的 bug）：
	// start 与 poll 必须拿到**同一个**实例，否则 sessions 各是一份空 map。
	p.loginFlowOnce.Do(func() {
		p.loginFlowCached = &loginFlow{
			mgr:        p.login,
			providerID: p.ID(),
			authDir:    p.authDir,
			sessions:   make(map[string]*loginSession),
		}
		// 后台定期回收超时会话。
		//
		// ⚠ 必须在这里起（`sync.Once` 内），不是每个请求里 ——
		// 否则每次 Start 都会多一个常驻 goroutine。
		//
		// 生命周期：这个 goroutine **永不退出**，随进程结束而结束。
		// 这是刻意的 —— `loginFlow` 与 Provider 同生命周期，
		// 而 Provider 是进程级的（装配一次，用到退出）。
		// 加一个 Stop() 只会制造"谁来调它"的问题，而没有实际收益。
		go p.loginFlowCached.gcTick(gcInterval)
	})
	return p.loginFlowCached, true
}

// Start 让 *Provider 满足 gateway.LoginFlow。
func (p *Provider) Start() (string, string, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("codearts: 未配置登录流程")
	}
	return f.Start()
}

// Poll 让 *Provider 满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("codearts: 未配置登录流程")
	}
	return f.Poll(state)
}

// Configured 报告这份部署真的能走登录流程。
//
// 判据与 LoginFlow() 一致：`cfg.Login` 非 nil。
// **这是"不给假按钮"的关键** —— 上游为了让 ExtOf 认出来必须挂上
// Start/Poll，那是编译期事实；没配 Portal 的部署同样"看起来实现了"。
// 见 gateway.LoginFlow.Configured 的注释。
func (p *Provider) Configured() bool { return p != nil && p.login != nil }

// AuthDir 本上游凭证的落盘目录（`gateway.LoginFlow` 要求）。
//
// # ⚠ 这是实测踩出来的
//
// 我第一版让核心的 `pollViaFlow` 用 `h.cfg.AuthDir` 落盘 ——
// 那是**默认上游（workbuddy）的目录**。于是 codearts 授权成功后，
// 凭证被往 workbuddy 的 `./auths` 写（用户实测报的
// `rename auths.tmp auths: Access is denied.` 就是这条路径上的第一步失败）。
//
// 现在由上游自报：Provider 知道自己从哪**读**凭证（`p.authDir`），
// 那就是它该往哪**写**。核心只管"拿到目录 + 文件名就写"，不猜。
func (p *Provider) AuthDir() string {
	if p == nil {
		return ""
	}
	return p.authDir
}

// Configured 报告这份部署真的能走登录流程（*loginFlow 视角）。
//
// ⚠ `*loginFlow` **也要**实现它：核心拿到的接口值是 `*loginFlow`
// （`LoginFlow()` 返回的是它），而 `Start`/`Poll` 挂在 *Provider 上只是
// 为了让 `ExtOf` 的类型断言认出来。
//
// 两处判据必须一致，否则会出现"manifest 说有按钮、点了却报未配置"。
func (f *loginFlow) Configured() bool {
	return f != nil && f.mgr != nil
}

// AuthDir 转发给 Provider（`gateway.LoginFlow` 要求 *loginFlow 也实现它）。
//
// 核心拿到的是 `*loginFlow`，所以落盘目录必须在这一层拿得到 ——
// 只在 `*Provider` 上实现是不够的（那正是 T10-d 踩过的"方法集不匹配"）。
func (f *loginFlow) AuthDir() string {
	if f == nil {
		return ""
	}
	return f.authDir
}

// ---------------------------------------------------------------------------
// loginFlow 的实现
// ---------------------------------------------------------------------------

// Start 发起一次授权：生成 PKCE/DPoP、起本地回调服务器、返回授权 URL。
func (f *loginFlow) Start() (string, string, error) {
	s, err := f.mgr.Start()
	if err != nil {
		return "", "", fmt.Errorf("发起 CodeArts 授权: %w", err)
	}
	f.mu.Lock()
	f.gcLocked()
	f.sessions[s.State] = &loginSession{oauth: s, done: make(chan loginResult, 1), startedAt: time.Now()}
	f.mu.Unlock()
	return s.State, s.AuthURL, nil
}

// Poll 查询授权结果。**非阻塞** —— 见文件头的说明。
//
// 第一次调用时把 `Wait() + Exchange()` 下沉到 goroutine，
// 之后每次只读 channel。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	f.mu.Lock()
	sess, ok := f.sessions[state]
	if !ok {
		f.mu.Unlock()
		// state 未知或已被换走（重复轮询同一次授权）。
		// 用 ErrLoginPending 而不是错误：核心对未知 state 的
		// 处理是 401/410，而"已换过"更接近"这次授权结束了"。
		// ⚠ 这里刻意不返回凭证 —— 凭证只在 done 里取，
		// 保证"兑换"这个动作**恰好发生一次**。
		return gateway.Credential{}, errStateGone
	}
	f.mu.Unlock()

	// 把阻塞的等待下沉到后台，只做一次。
	//
	// # 为什么要下沉
	//
	// `Session.Wait()` 会 select 到回调到达**或 15 分钟 TTL**。
	// 直接在 handler 里跑 = 一个 HTTP 请求被占住最长 15 分钟，
	// 而授权其实随时可能完成。所以起 goroutine 去等，
	// Poll 只从 channel 非阻塞读。
	sess.once.Do(func() {
		go func() {
			code, werr := sess.oauth.Wait() // 阻塞直到回调到达或 15 分钟 TTL
			if werr != nil {
				sess.done <- loginResult{err: werr}
				return
			}
			// 兑换凭证。
			//
			// ⚠ 这里要 `sessionFor` 还原具体类型：测试替身（fakeManager）
			// 的会话不是 `*Session`，而真实现的 Exchange 需要它。
			// 替身路径下 Exchange 用 nil 也能工作 —— 它不碰真会话。
			var handle *Session
			if real, ok := sessionFor(sess.oauth); ok {
				handle = real
			}
			raw, xerr := f.mgr.Exchange(handle, code)
			if xerr != nil {
				sess.done <- loginResult{err: xerr}
				return
			}
			cred, cerr := f.credentialFrom(raw)
			sess.done <- loginResult{cred: cred, err: cerr}
		}()
	})

	select {
	case r := <-sess.done:
		// 拿到结果后把会话摘掉 —— 同一个 state 不该能换两次。
		f.mu.Lock()
		delete(f.sessions, state)
		f.mu.Unlock()
		if r.err != nil {
			return gateway.Credential{}, r.err
		}
		return r.cred, nil
	default:
		// 用户还没在浏览器完成授权 —— 这正是 pending 的含义。
		return gateway.Credential{}, gateway.ErrLoginPending
	}
}

// errStateGone 该 state 不存在（已兑换、已过期、或从未发起）。
var errStateGone = errors.New("codearts: 授权会话不存在或已结束，请重新发起")

// credentialFrom 把落盘用的凭证 JSON 转成通用凭证。
//
// # 为什么 Secret 放原始 JSON 而不是解析后的结构
//
// 核心落盘时要求 Secret 实现 `MarshalAuthFile() (name, raw, err)`
// （见 admin.pollViaFlow 的注释）。本包产出的 raw **本身就是**那个格式
// （`{"auth":{...},"account":{...},"dpop":{...}}`），且已在源仓库
// 与网关的 `internal/codearts/credential.go` 逐字段对齐（移植时实测）。
//
// 所以这里只需要**把原名与内容原样包一层**，不做任何字段映射 ——
// 字段映射是"看起来成功、实际字段错位"的高发区。
func (f *loginFlow) credentialFrom(raw json.RawMessage) (gateway.Credential, error) {
	if len(raw) == 0 {
		return gateway.Credential{}, errors.New("codearts: 授权返回了空凭证")
	}
	doc := &codeartsAuthFile{raw: raw}

	// uid 从响应里取（授权响应不含账号 id 时源实现已用 AK 兜底，
	// 见它 Exchange 里的注释）。这里读回来只用于展示与去重。
	var probe struct {
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"account"`
		Auth struct {
			AccessKeyID string `json:"accessKeyId"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return gateway.Credential{}, fmt.Errorf("解析授权结果: %w", err)
	}
	uid := probe.Account.UID
	if uid == "" {
		uid = probe.Auth.AccessKeyID // 与源实现同一个兜底
	}
	if uid == "" {
		return gateway.Credential{}, errors.New("codearts: 授权结果里没有账号标识（uid 与 AK 都为空）")
	}

	// ⚠ 文件名必须在这里定下来（`codearts-<uid>.json`，与源仓库命令行路径一致）。
	//
	// 我第一版**没有设它** —— `MarshalAuthFile` 返回空串，于是核心：
	//
	//	filepath.Join("auths", "") = "auths"    ← 目录本身被当成文件
	//	os.Rename("auths.tmp", "auths")         ← Access is denied
	//
	// 用户看到的报错是 `凭证落盘失败: rename auths.tmp auths: Access is denied.`
	// —— 一个**完全看不出与文件名有关**的失败。
	//
	// 现在 `MarshalAuthFile` 在名字为空时**明确报错**，这类错误不会再
	// 以"重命名目录"的形态出现。
	doc.name = "codearts-" + uid + ".json"

	var expiresAt time.Time
	if probe.Auth.ExpiresAt > 0 {
		expiresAt = time.Unix(probe.Auth.ExpiresAt, 0)
	}
	return gateway.Credential{
		Provider:  f.providerID,
		UID:       uid,
		Nickname:  probe.Account.Nickname,
		ExpiresAt: expiresAt,
		Secret:    doc,
	}, nil
}

// gcLocked 回收超时会话（调用方持锁）。
//
// # 为什么必须清理
//
// 每个会话**占着一个本地监听端口**（`net.Listen("tcp", "127.0.0.1:0")`）。
// 用户在界面上点几次"添加账号"又不去授权，就会留下几个永不关闭的监听。
// 源实现是**命令行一次性使用**，没有这个问题；接进**常驻进程**就必须清。
//
// TTL 用比上游略长的值：上游 15 分钟超时后 `Wait` 会自己返回并关掉
// 回调服务器，这里再宽限 1 分钟让那条返回路径先走完。
func (f *loginFlow) gcLocked() {
	ttl := TTL + time.Minute
	for k, s := range f.sessions {
		if time.Since(s.startedAt) <= ttl {
			continue
		}
		// 关掉回调服务器：这是唯一会泄漏的资源（一个真实监听端口）。
		if s.oauth != nil {
			s.oauth.CloseCallback()
		}
		delete(f.sessions, k)
	}
}

// gcInterval 后台回收的间隔。
//
// ⚠ 抽成**变量**而不是直接写 `TTL / 4` —— 测试要把它调短，
// 才能验"`LoginFlow()` 真的起了 ticker"这条**接线**。
//
// 用 `var` 而非 `const`：常量在测试里改不了，那样测试就只能自己起
// ticker，于是它测的是"`gcTick` 函数能工作"而不是"`LoginFlow()` 会起它"
// —— 我第一次就是这么写的，**变异验证时去掉 ticker 它仍然绿**。
//
// > 断言测了实现的一部分，漏了接线。
var gcInterval = TTL / 4

// gcTick 定期回收超时会话（后台 goroutine）。
//
// # 为什么需要它（Reviewer 抓到的缺口）
//
// 我第一版只在 `Start()` 里调 `gcLocked()`。Reviewer 做了 17 分钟实验定格：
//
//	创建会话 → 过 TTL 后**监听仍 ALIVE**（没有被回收）
//	→ 只有触发下一次 Start 时才 DEAD
//
// 后果：一个**只狂点「添加账号」、从不完成授权**的用户，
// 能在 16 分钟内堆到任意多个监听端口（Reviewer 实测堆到 24 个）。
//
// **不是真泄漏**（TTL 后会被下次 Start 清掉），所以判轻微 ——
// 但"资源回收依赖下一次用户操作"本身就是坏形状：
// 它是**无界的**（用户点得越多堆得越多），而回收时机**不在系统控制内**。
//
// 所以补一个定时回收，让过期会话**不依赖下一次 Start**。
//
// # 间隔
//
// 取 TTL/4（约 4 分钟）：回收延迟最多 4 分钟，既能及时释放，
// 也不会让 ticker 太频繁。用 `TTL` 推导而不是写死分钟数 ——
// 将来调 TTL 时这里自动跟随，不会漏改。
func (f *loginFlow) gcTick(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		f.mu.Lock()
		f.gcLocked()
		f.mu.Unlock()
	}
}

// codeartsAuthFile 实现核心要的 authFileWriter 窄接口。
//
// 方法签名与 `*oauth.Credential` 的 `MarshalAuthFile` **同名同签名**
// （见 admin.pollViaFlow 的注释）—— 所以两边能共用同一条落盘路径。
type codeartsAuthFile struct {
	raw json.RawMessage
	// name 落盘文件名。空则用 uid 兜底（核心会用 UID 拼）。
	name string
}

// MarshalAuthFile 返回 (文件名, 内容)。
//
// # ⚠ 这里我第一版返回了**空名字**，导致落盘直接失败
//
// 第一版写的是"不自己拼名，由调用方用 UID 决定"，注释还写着
// "核心的 pollViaFlow 会用 gateway.Credential.UID 决定文件名"。
//
// **但核心根本没实现那个逻辑** —— 它把空名直接传给了 writeAuthFile：
//
//	filepath.Join("auths", "") = "auths"       ← 目录本身被当成文件
//	tmp := "auths.tmp"
//	os.Rename("auths.tmp", "auths")            ← Access is denied
//
// 用户实际看到的报错：
//
//	凭证落盘失败: rename auths.tmp auths: Access is denied.
//
// 教训：**我假设了调用方会做某件事，但没对着它的代码核实。**
// 跨层契约里"由对方决定"这种写法，必须先去对方那里确认它真的做了 ——
// 否则失败会以完全无关的形态出现（这里是"重命名目录"）。
func (c *codeartsAuthFile) MarshalAuthFile() (string, []byte, error) {
	if c == nil || len(c.raw) == 0 {
		return "", nil, errors.New("codearts: 凭证内容为空")
	}
	if c.name == "" {
		// 走到这里说明 credentialFrom 没填上名字 —— 那是编程错误。
		// **明确报错**而不是回落成空串：空串会变成"重命名目录"那种
		// 看起来与文件名无关的诡异失败。
		return "", nil, errors.New("codearts: 凭证文件名未设置（uid 解析失败？）")
	}
	return c.name, []byte(c.raw), nil
}

// httpClientOf 暴露给测试用（本包只需要它不为 nil）。
func (f *loginFlow) httpClientOf() *http.Client {
	return f.mgr.HTTPClient()
}
