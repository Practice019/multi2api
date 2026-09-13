package codearts

// login_test.go —— R1-b 的守卫：适配器把**阻塞的** Wait 正确异步化了。
//
// # 本文件要钉住的核心风险
//
// 两边的等待模型不同：
//
//	workbuddy（设备码）：Poll 每次**问一次**上游，本身是轮询语义
//	codearts（授权码）：Session.Wait() 是**阻塞**的（select 到回调或 15 分钟 TTL）
//
// 若把 Wait 直接在 handler 里跑，一次请求会被占住最长 15 分钟。
// 适配器把它下沉到 goroutine，Poll 只非阻塞读 channel。
//
// **这个"下沉"是看不见的** —— 代码能编译、能跑、也能通过"功能测试"，
// 但真实使用时表现为"点添加账号后界面卡住"。
// 所以必须有断言钉住：**Poll 必须立刻返回**。

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fakeSession 一个不碰真实回调服务器的会话替身。
//
// `release` 关掉它 `Wait()` 才返回 —— 测试据此精确控制"用户何时完成授权"。
type fakeSession struct {
	release   chan struct{}
	code      string
	waitErr   error
	closed    bool
	closeOnce sync.Once
}

func newFakeSession() *fakeSession {
	return &fakeSession{release: make(chan struct{}), code: "CODE-1"}
}

func (f *fakeSession) Wait() (string, error) {
	<-f.release
	if f.waitErr != nil {
		return "", f.waitErr
	}
	return f.code, nil
}

func (f *fakeSession) CloseCallback() {
	f.closeOnce.Do(func() { f.closed = true })
}

// fakeManager 满足 loginManager。
type fakeManager struct {
	startErr    error
	exchangeRaw json.RawMessage
	exchangeErr error
	// lastSession 记录 Exchange 收到的是不是真 *Session（应为 nil —— 替身路径）
	exchanges int
}

func (f *fakeManager) Start() (*Session, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	// 只用于让 Start 有个返回值；本测试的会话句柄是 fakeSession，不是它。
	return &Session{State: "ST-fake", AuthURL: "https://portal.example/authorize?x=1"}, nil
}

func (f *fakeManager) Exchange(s *Session, code string) (json.RawMessage, error) {
	f.exchanges++
	return f.exchangeRaw, f.exchangeErr
}

func (f *fakeManager) HTTPClient() *http.Client { return &http.Client{} }

// newTestFlow 造一个 handler 已换成交替会话的 loginFlow。
//
// 为什么手工塞 sessions 而不是走 Start：`Start()` 内部会调
// `mgr.Start()` 并把返回的 `*Session` 存起来，而测试要的是 `fakeSession`。
// 直接塞是**刻意的** —— 它让"等待被下沉"这条断言可以精确控制。
func newTestFlow(mgr loginManager, sess loginSessionHandle, state string) *loginFlow {
	return &loginFlow{
		mgr:        mgr,
		providerID: "codearts",
		sessions: map[string]*loginSession{
			state: {oauth: sess, done: make(chan loginResult, 1), startedAt: time.Now()},
		},
	}
}

// ---------------------------------------------------------------------------
// 1. Poll 必须**立即**返回，不能被 Wait 阻塞（本文件最重要的一条）
// ---------------------------------------------------------------------------

func TestPollReturnsImmediatelyWhileWaiting(t *testing.T) {
	mgr := &fakeManager{}
	sess := newFakeSession() // release 未关 = 用户还没授权
	lf := newTestFlow(mgr, sess, "ST-1")

	done := make(chan error, 1)
	go func() {
		_, perr := lf.Poll("ST-1")
		done <- perr
	}()

	select {
	case perr := <-done:
		if !errors.Is(perr, gateway.ErrLoginPending) {
			t.Errorf("未完成时应返回 ErrLoginPending，实际 %v", perr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Poll 被阻塞了 —— 等待没有下沉到后台。" +
			"真实后果：点「添加账号」后 HTTP 请求会挂住最长 15 分钟")
	}
}

// ---------------------------------------------------------------------------
// 2. 放行后拿到凭证
// ---------------------------------------------------------------------------

func TestPollReturnsCredentialAfterRelease(t *testing.T) {
	mgr := &fakeManager{exchangeRaw: json.RawMessage(`{
		"auth":{"accessKeyId":"AK123","secretAccessKey":"SK","securityToken":"ST",
		        "expiresAt":1893456000,"refresh_token":"RT","clientId":"vscode-codebot"},
		"account":{"uid":"AK123","nickname":"tester"},
		"dpop":{"privateKeyJwk":{"kty":"EC"}}
	}`)}
	sess := newFakeSession()
	lf := newTestFlow(mgr, sess, "ST-1")

	// 首次 Poll 起后台等待
	if _, err := lf.Poll("ST-1"); !errors.Is(err, gateway.ErrLoginPending) {
		t.Fatalf("首次 Poll 应 pending，实际 %v", err)
	}

	close(sess.release) // 用户完成授权

	var cred gateway.Credential
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, perr := lf.Poll("ST-1")
		if perr == nil {
			cred = c
			break
		}
		if !errors.Is(perr, gateway.ErrLoginPending) {
			t.Fatalf("轮询出错: %v", perr)
		}
		if time.Now().After(deadline) {
			t.Fatal("3 秒内没拿到凭证 —— 后台等待没有把结果投递出来")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if cred.UID != "AK123" {
		t.Errorf("UID=%q，应从 account.uid 取（AK123）", cred.UID)
	}
	if cred.Provider != "codearts" {
		t.Errorf("Provider=%q，应为 codearts（由本包填 —— 授权响应不知道自己是哪个上游）",
			cred.Provider)
	}
	if cred.Secret == nil {
		t.Fatal("Secret 为空 —— 核心落盘时要求它实现 MarshalAuthFile")
	}
	mw, ok := cred.Secret.(interface {
		MarshalAuthFile() (string, []byte, error)
	})
	if !ok {
		t.Fatalf("Secret 类型 %T 没实现 MarshalAuthFile —— 核心会返回 501", cred.Secret)
	}
	_, raw, merr := mw.MarshalAuthFile()
	if merr != nil {
		t.Errorf("MarshalAuthFile 报错: %v", merr)
	}
	if len(raw) == 0 {
		t.Error("MarshalAuthFile 返回空内容 —— 落盘会得到空文件")
	}
}

// ---------------------------------------------------------------------------
// 3. 同一 state 只能兑换一次
// ---------------------------------------------------------------------------

func TestPollConsumesStateOnce(t *testing.T) {
	mgr := &fakeManager{exchangeRaw: json.RawMessage(`{
		"auth":{"accessKeyId":"AK1","expiresAt":1893456000},
		"account":{"uid":"AK1"}}`)}
	sess := newFakeSession()
	lf := newTestFlow(mgr, sess, "ST-1")

	if _, err := lf.Poll("ST-1"); !errors.Is(err, gateway.ErrLoginPending) {
		t.Fatalf("首次 Poll: %v", err)
	}
	close(sess.release)

	deadline := time.Now().Add(3 * time.Second)
	for {
		_, perr := lf.Poll("ST-1")
		if perr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("超时：第一次兑换没成功")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if mgr.exchanges != 1 {
		t.Errorf("Exchange 被调用 %d 次，应为 1 —— 一次授权只能兑换一次", mgr.exchanges)
	}

	// ⚠ 这条断言我第一版写错了，值得记下来。
	//
	// 第一版写的是 `if _, err := lf.Poll(state); err == nil { 报错 }` ——
	// **它抓不到"不摘除会话"这个缺陷**（实测：变异后测试仍绿）。
	//
	// 原因：Poll 有三种返回，其中**两种都是非 nil error**：
	//
	//	正确行为（会话已摘除）→ errStateGone        （非 nil）
	//	缺陷行为（会话没摘）  → gateway.ErrLoginPending  （也是非 nil！）
	//
	// 所以 `err == nil` 这个判据**区分不了两者** —— 它只排除了
	// "返回凭证"这一种，而缺陷根本不走那条路。
	//
	// 正确判据：**必须断言 err 是 errStateGone**，而不是"某个 error"。
	// 教训：当"失败"有多个形态时，"是非 nil 就算对"是个假判据。
	if _, err := lf.Poll("ST-1"); !errors.Is(err, errStateGone) {
		t.Errorf("第二次 Poll 应返回 errStateGone（会话已被消费），实际 %v。\n"+
			"若这里是 ErrLoginPending，说明会话没被摘除 —— "+
			"同一份凭证可以被反复取走，而每次取走都意味着一次真实的 token 兑换",
			err)
	}
}

// ---------------------------------------------------------------------------
// 4. 未配置 → Configured 为 false（不给假按钮）
// ---------------------------------------------------------------------------

func TestUnconfiguredProviderHasNoLoginFlow(t *testing.T) {
	p := &Provider{} // login 为 nil
	if _, ok := p.LoginFlow(); ok {
		t.Error("未配置 login 时 LoginFlow() 应返回 false")
	}
	if p.Configured() {
		t.Error("未配置 login 时 Configured() 应为 false —— " +
			"否则 manifest 会下发 login，前端渲染出点了报错的假按钮")
	}

	p2 := &Provider{login: NewManager("", "", "")}
	if !p2.Configured() {
		t.Error("配了 login 后 Configured() 应为 true")
	}
	if _, ok := p2.LoginFlow(); !ok {
		t.Error("配了 login 后 LoginFlow() 应返回 true")
	}
	// 关键：必须能被核心的**类型断言**认出来
	if _, ok := gateway.ExtOf[gateway.LoginFlow](p2); !ok {
		t.Error("gateway.ExtOf[LoginFlow] 认不出配好登录的 Provider —— " +
			"方法集不匹配（「返回接口的访问器」对类型断言不可见，这是 T10-d 踩过的坑）")
	}
}

// ---------------------------------------------------------------------------
// 5. Start 的错误要透传
// ---------------------------------------------------------------------------

func TestStartPropagatesError(t *testing.T) {
	mgr := &fakeManager{startErr: errors.New("端口占满")}
	lf := &loginFlow{mgr: mgr, providerID: "codearts", sessions: map[string]*loginSession{}}

	// 直接调 Start（它会用 mgr.Start）—— fake 的 Start 返回错
	if _, _, err := lf.Start(); err == nil {
		t.Error("Start 失败时应把错误透出去 —— 吞掉错误会让界面显示成功但什么都没有")
	}
}

// ---------------------------------------------------------------------------
// 6. gcLocked 不能因 nil 而 panic（源实现没测这条路径）
// ---------------------------------------------------------------------------

func TestGCHandlesSessionWithoutCallback(t *testing.T) {
	lf := &loginFlow{sessions: map[string]*loginSession{
		"old": {
			oauth:     &fakeSession{}, // CloseCallback 是幂等空操作
			startedAt: time.Now().Add(-2 * (TTL + time.Minute)),
		},
		"fresh": {
			oauth:     &fakeSession{},
			startedAt: time.Now(),
		},
	}}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("gcLocked panic 了: %v —— 它会被定期调用，panic 即进程退出", r)
		}
	}()
	lf.mu.Lock()
	lf.gcLocked()
	lf.mu.Unlock()

	if _, ok := lf.sessions["old"]; ok {
		t.Error("超时会话没被回收 —— 每个会话占一个监听端口，不回收就是端口泄漏")
	}
	if _, ok := lf.sessions["fresh"]; !ok {
		t.Error("未超时的会话被误删 —— 用户授权还没做完就丢了会话")
	}
}

// TestGCRecordsSessionClosed 回收时必须真的关掉回调监听。
//
// 为什么单独测：`CloseCallback` 漏调不会让任何功能失败，
// 只会在运行几天后表现为"监听本地回调端口失败：address already in use"。
// 那种 bug 在开发期**看不见**。
func TestGCRecordsSessionClosed(t *testing.T) {
	sess := &fakeSession{}
	lf := &loginFlow{sessions: map[string]*loginSession{
		"old": {oauth: sess, startedAt: time.Now().Add(-2 * (TTL + time.Minute))},
	}}
	lf.mu.Lock()
	lf.gcLocked()
	lf.mu.Unlock()

	if !sess.closed {
		t.Error("回收超时会话时没有调用 CloseCallback —— 回调监听端口会泄漏")
	}
}

// ---------------------------------------------------------------------------
// 7. LoginFlow() 必须返回**同一个**实例（跨调用的状态）
// ---------------------------------------------------------------------------

// TestLoginFlowIsStableAcrossCalls 钉住"start 与 poll 拿到同一个 flow"。
//
// # 这条守卫是被一个真 bug 逼出来的
//
// 第一版 `LoginFlow()` 每次调用都新建实例，于是 `sessions` map 各是一份空表：
//
//	/admin/login/start → 实例 A 存下会话
//	/admin/login/poll  → 实例 B（空）→ "授权会话不存在或已结束"
//
// 后果：**页内添加账号永远不可能成功**，且没有任何报错提示原因。
//
// # 为什么这个 bug 之前没被抓住
//
// 本文件其余测试都用 `newTestFlow` —— 它**直接构造一个 loginFlow**
// 然后连续调方法，全程同一个实例。所以"每次调用都换实例"这件事
// 在那些测试里**根本不会发生**。
//
// 我实际是靠**端到端**发现的：start 之后直打回调地址，回调服务器
// **真能打通**（说明流程活着），但 poll 说会话不存在。
// 两个观察互相矛盾 → 唯一解释是 poll 拿到的不是同一个 flow。
//
// 所以这条断言的写法必须是"**跨调用比较身份**" ——
// 只有这样才能在不跑端到端的情况下钉住它。
func TestLoginFlowIsStableAcrossCalls(t *testing.T) {
	p := &Provider{login: NewManager("", "", "")}

	a, ok := p.LoginFlow()
	if !ok {
		t.Fatal("配了 login 后 LoginFlow() 应返回 true")
	}
	b, ok := p.LoginFlow()
	if !ok {
		t.Fatal("第二次调用也应返回 true")
	}

	if a != b {
		t.Error("LoginFlow() 两次返回了**不同实例** —— " +
			"start 与 poll 会各自拿到一份空的 sessions map，" +
			"表现是 poll 永远报「授权会话不存在」，而**页内添加账号永远不可能成功**。" +
			"必须缓存实例（Provider.loginFlowCached）")
	}

	// 更强的判据：往里塞一个会话，再从**第二次调用**的返回值里读回来。
	// 这直接模拟了 start → poll 的跨调用路径。
	lf := b.(*loginFlow)
	lf.mu.Lock()
	lf.sessions["ST-cross"] = &loginSession{oauth: &fakeSession{}, done: make(chan loginResult, 1)}
	lf.mu.Unlock()

	again, _ := p.LoginFlow()
	lf2 := again.(*loginFlow)
	lf2.mu.Lock()
	_, present := lf2.sessions["ST-cross"]
	lf2.mu.Unlock()

	if !present {
		t.Error("start 存下的会话在下次 LoginFlow() 的实例里读不到 —— " +
			"这就是那个 bug 的直接形态（跨调用状态丢失）")
	}
}

// TestLoginFlowConfiguredConsistency 两处 Configured 判据必须一致。
//
// `Configured` 同时挂在 `*Provider` 与 `*loginFlow` 上：
// 前者供 `ExtOf` 的类型断言，后者是核心实际拿到的接口值。
// 两者若不一致，会出现"manifest 说有按钮、点了却报未配置"。
func TestLoginFlowConfiguredConsistency(t *testing.T) {
	// 未配置
	p0 := &Provider{}
	if p0.Configured() {
		t.Error("未配置时 *Provider.Configured() 应为 false")
	}

	// 已配置 —— 两处都应为 true
	p1 := &Provider{login: NewManager("", "", "")}
	if !p1.Configured() {
		t.Error("已配置时 *Provider.Configured() 应为 true")
	}
	lf, ok := p1.LoginFlow()
	if !ok {
		t.Fatal("已配置时 LoginFlow() 应为 true")
	}
	if !lf.Configured() {
		t.Error("*loginFlow.Configured() 应为 true —— " +
			"核心拿到的接口值是 *loginFlow，它若返回 false 会直接 501")
	}
}
