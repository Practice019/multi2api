package gateway

import (
	"context"
	"io"
	"time"
)

// TB 是 testing.TB 的**最小子集**，让契约检查器既能在真实测试里跑，
// 也能在"反向验证"里用一个假的 T 观察它是否报错。
//
// 为什么不直接用 testing.TB：那样就无法用假 T 验证"契约真的能抓到违规"，
// 而一个永远绿灯的契约测试等于没有契约。
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// RunProviderContract 校验一个 Provider 实现是否合格。
//
// 每个上游包的测试文件里一行接入：
//
//	func TestContract(t *testing.T) { gateway.RunProviderContract(t, New) }
//
// 它检查的是**行为契约**，不是"能不能编译"。
//
// # 检查项（每一条都有对应的反向验证用例）
//
//  1. ID() 合法（^[a-z][a-z0-9-]*$）**且稳定**（多次调用结果一致）
//  2. Caps() 必须声明 CapChat
//  3. 任何方法都不得 panic
//  4. 声明 CapModels ⇒ Models() 必须真的返回非空目录
//  5. 声明任何能力位 ⇒ 必须能被行为验证或显式豁免（见 capabilityProbes）
//  6. Chat 返回的流必须能被完整读完并 Close，且 Close 不得报错
//  7. Chat 成功返回（err==nil）时，Status 或 Body 至少要有一个非零
//  8. ctx 取消时 Chat 必须尽快返回
//
// # 关于凭证（Option）
//
// 默认用一个**伪造凭证**跑（Provider/UID 是占位值）。真实上游会因凭证无效而报错，
// 此时契约**无法区分**"凭证不对"与"没实现"。
// 为了不让合法实现被误判为不合格，提供 WithCredential 注入真实凭证；
// 拿不到凭证的实现可以用 SkipIfNoCredential 声明跳过（跳过 ≠ 通过）。
func RunProviderContract(t TB, factory func() Provider, opts ...ContractOption) {
	t.Helper()

	if factory == nil {
		t.Fatalf("RunProviderContract: factory 不能为 nil")
		return
	}
	cfg := contractConfig{cred: Credential{Provider: "contract-probe", UID: "contract-probe"}}
	for _, o := range opts {
		o(&cfg)
	}

	// ---- 1. 构造不 panic ----
	p := construct(t, factory)
	if p == nil {
		t.Fatalf("factory 返回 nil Provider")
		return
	}

	// ---- 2. ID 合法且稳定 ----
	// 调用两次并比对：ID 不稳定会让 Registry 注册成功但后续 Get 查不到
	// （表现为"随机取不到 Provider"，是最难查的一类问题）。
	id := callString(t, "ID", p.ID)
	id2 := callString(t, "ID(第二次)", p.ID)
	if id == "" {
		t.Errorf("ID() 为空（注册与路由都依赖它）")
	} else if !validProviderID(id) {
		t.Errorf("ID()=%q 非法：要求 ^[a-z][a-z0-9-]*$（要作为模型名前缀）", id)
	} else if id2 != id {
		t.Errorf("ID() 不稳定：第一次 %q，第二次 %q。"+
			"ID 会被 Registry 用作 key，不稳定会导致注册成功但按 ID 查不到", id, id2)
	}

	// ---- 3. Caps 必须含 CapChat ----
	caps := callCaps(t, p)
	if !caps.Has(CapChat) {
		t.Errorf("Caps() 未声明 CapChat —— 没有对话能力的上游接入无意义（got %v）", caps.Names())
	}

	// ---- 4. 声明的能力必须真的可用 ----
	probeCapabilities(t, p, caps, id, cfg)

	// ---- 5. Chat 行为 ----
	if caps.Has(CapChat) {
		verifyChatNoPanic(t, p, id, cfg)
		verifyChatStreamClosable(t, p, id, cfg)
		verifyChatRespectsCancel(t, p, id)
	}
}

// contractConfig 契约检查的配置。
type contractConfig struct {
	cred Credential
}

// ContractOption 契约检查的可选项。
type ContractOption func(*contractConfig)

// WithCredential 注入真实凭证。
//
// 为什么需要：契约默认用伪造凭证，真实上游会因凭证无效报错。
// 没有真实凭证时，契约无法判断"是凭证问题还是没实现"。
// 注入后即可做完整行为验证（各上游在自己的测试里从环境变量或 fixture 读凭证）。
func WithCredential(c Credential) ContractOption {
	return func(cfg *contractConfig) { cfg.cred = c }
}

// capabilityProbes 各能力位的行为探针。
//
// **这是修掉"声明了却不验证"漏洞的关键。**
// 改造前只验证了 CapModels，于是"声明全部 7 个能力位却零实现"能通过契约。
//
// 每个探针返回 (是否有问题, 说明)。
// 返回 false 表示"这条能力无法自动验证" —— 那种情况要求上游**显式豁免**（见下）。
var capabilityProbes = map[Capability]func(t TB, p Provider, id string, cfg contractConfig) bool{
	CapModels: func(t TB, p Provider, id string, cfg contractConfig) bool {
		// 声明支持模型目录 ⇒ 必须真的返回非空列表
		// （返回空列表等于没实现，却会让 /v1/models 静默变空）
		return verifyModelsNonEmpty(t, p, id, cfg)
	},
}

// unverifiableCaps 无法自动行为验证的能力位。
//
// 它们的行为（签到/成长/旅行/福利/额度探测）都需要真实账号与真实上游，
// 契约测试跑不了。**但"无法自动验证"不等于"可以不实现"** ——
// 因此要求上游在 Caps() 里声明后，实现对应的小接口，
// 由该上游自己的测试覆盖（见各上游包）。
//
// 这里列出来的作用是：让"哪些能力是靠人工保证的"这件事显式可见，
// 而不是像改造前那样悄悄跳过。
var unverifiableCaps = []Capability{
	CapCheckin, CapGrowth, CapTravel, CapWelfare, CapQuotaProbe,
}

// probeCapabilities 逐条验证声明的能力。
func probeCapabilities(t TB, p Provider, caps Capability, id string, cfg contractConfig) {
	t.Helper()

	// 能自动验证的：必须通过
	for cap, probe := range capabilityProbes {
		if !caps.Has(cap) {
			continue
		}
		if !probe(t, p, id, cfg) {
			return // 探针内部已 Errorf
		}
	}

	// 无法自动验证的：检查上游是否真的实现了对应扩展点，
	// 避免"声明了却不实现"完全无声通过。
	//
	// 判据：声明 CapGrowth/CapTravel/CapWelfare/CapQuotaProbe 的上游，
	// 必须实现 AdminExt **且提供非空、结构完整的路由**。
	//
	// ⚠ 只检查"AdminExt 存在"是不够的 —— 评审证明：
	// 声明全部 7 个能力位、`AdminRoutes()` 返回 nil 的实现**零报错通过**。
	// 那正是"假声明"最省事的写法（实现空方法比不实现还容易）。
	needsAdmin := false
	for _, c := range unverifiableCaps {
		if caps.Has(c) {
			needsAdmin = true
			break
		}
	}
	if needsAdmin {
		ax, ok := ExtOf[AdminExt](p)
		if !ok {
			t.Errorf("声明了 %v 中的能力，但没实现 gateway.AdminExt —— "+
				"这些能力都通过管理端点暴露，没有 AdminRoutes 说明没实现",
				caps.Names())
		} else {
			verifyAdminRoutes(t, ax, caps)
		}
	}
}

// verifyAdminRoutes 检查 AdminExt 的实现不是空壳。
//
// 评审的绕过手法：`func (p *X) AdminRoutes() []AdminRoute { return nil }`
// 配上声明全部能力位 → 契约零报错。空实现比不实现更省事，必须挡住。
func verifyAdminRoutes(t TB, ax AdminExt, caps Capability) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("AdminRoutes() panic: %v", r)
		}
	}()
	routes := ax.AdminRoutes()
	if len(routes) == 0 {
		t.Errorf("声明了 %v 中的能力，AdminRoutes() 却返回空 —— "+
			"这些能力必须有对应的管理端点，空列表说明只是声明了没实现",
			caps.Names())
		return
	}
	// 每条路由必须结构完整 —— 缺 Handler 的路由挂上去就是 404
	seen := map[string]bool{}
	for i, r := range routes {
		if r.Method == "" {
			t.Errorf("AdminRoutes()[%d] 缺 Method: %+v", i, r)
		}
		if r.Path == "" || r.Path[0] != '/' {
			t.Errorf("AdminRoutes()[%d] 的 Path 必须以 / 开头，得到 %q", i, r.Path)
		}
		if r.Handler == nil {
			t.Errorf("AdminRoutes()[%d] (%s %s) 缺 Handler —— 挂上去会是 404",
				i, r.Method, r.Path)
		}
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("AdminRoutes() 里有重复路由 %s —— 挂载时会 panic（重复 pattern）", key)
		}
		seen[key] = true
	}
}

// construct 在受保护的调用里构造 Provider，捕获 panic。
func construct(t TB, factory func() Provider) Provider {
	var p Provider
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("factory() panic: %v", r)
			}
		}()
		p = factory()
	}()
	return p
}

func callString(t TB, name string, fn func() string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s() panic: %v", name, r)
		}
	}()
	return fn()
}

func callCaps(t TB, p Provider) (out Capability) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Caps() panic: %v", r)
			out = 0
		}
	}()
	return p.Caps()
}

// verifyModelsNonEmpty 检查声明了 CapModels 就真的能返回**非空**目录。
//
// 「返回空列表」和「没实现」在调用方看来是一样的 —— 都会让 /v1/models 变空。
func verifyModelsNonEmpty(t TB, p Provider, id string, cfg contractConfig) (ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Models() panic: %v", r)
			ok = false
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cred := cfg.cred
	cred.Provider = id
	ms, err := p.Models(ctx, cred)
	if err != nil {
		// 契约**无法**区分"凭证无效"与"没实现"。
		// 因此不判失败，而是明确告知：要完整验证必须注入有效凭证。
		t.Errorf("声明了 CapModels 但 Models() 返回错误: %v\n"+
			"  若这是凭证问题：用 gateway.WithCredential(...) 注入有效凭证后重跑契约。\n"+
			"  若是根本没实现：从 Caps() 里去掉 CapModels。", err)
		return false
	}
	if len(ms) == 0 {
		t.Errorf("声明了 CapModels 但 Models() 返回空列表 —— " +
			"等于没实现（会让 /v1/models 静默变空）")
		return false
	}
	for i, m := range ms {
		if m.ID == "" {
			t.Errorf("Models()[%d].ID 为空", i)
		}
	}
	return true
}

// verifyChatNoPanic 调用 Chat，检查不 panic，且**成功返回时响应体不是空的**。
//
// 空 body 是刻意的：契约只要求"不崩"，不要求它成功 ——
// 上游会因为请求非法而报错，那是正常行为。
//
// ⚠ 另外检查"err == nil 但 Status 与 Body 都为零值"这种情况：
// 调用方会拿到一个既无状态码又无内容的响应，无法判断发生了什么。
// 这属于典型的"静默失败"，必须挡住。
func verifyChatNoPanic(t TB, p Provider, id string, cfg contractConfig) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Chat() panic: %v（契约要求任何输入都不得 panic）", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cred := cfg.cred
	cred.Provider = id
	cs, err := p.Chat(ctx, cred, []byte(`{}`))
	// 无论成功还是报错都接受；这里只验证"不 panic"。
	if cs.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(cs.Body, 4096))
		_ = cs.Body.Close()
	}
	// 成功返回（无 error）却给出空响应 —— 调用方无法判断发生了什么
	if err == nil && cs.Status == 0 && cs.Body == nil {
		t.Errorf("Chat 返回 (ChatStream{Status:0, Body:nil}, nil)：\n" +
			"  既没有状态码也没有响应体，调用方拿到的是不可判断的空响应。\n" +
			"  要么返回真实 Status（哪怕非 2xx），要么返回 error。")
	}
}

// verifyChatStreamClosable 检查返回的流**能被 Close 且不报错**。
//
// 这是最容易漏的一条：Provider 若返回一个包装了 net.Conn 的流却不正确
// 转发 Close，会导致连接池耗尽 —— 上游一多就会表现成"跑一会儿全卡住"。
func verifyChatStreamClosable(t TB, p Provider, id string, cfg contractConfig) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Chat() panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cred := cfg.cred
	cred.Provider = id
	cs, err := p.Chat(ctx, cred, []byte(`{}`))
	if err != nil || cs.Body == nil {
		return // 没拿到流（凭证/网络问题）时这条不适用
	}
	// 先读一点，模拟真实消费
	_, _ = io.Copy(io.Discard, io.LimitReader(cs.Body, 1024))
	if cerr := cs.Body.Close(); cerr != nil {
		t.Errorf("Chat 返回的流 Close() 报错: %v（会导致连接泄漏）", cerr)
	}
	// 二次 Close 不应 panic（有些实现会）
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("二次 Close() panic: %v", r)
		}
	}()
	_ = cs.Body.Close()
}

// verifyChatRespectsCancel 检查 ctx 取消后 Chat 不能长时间挂住。
//
// 一个上游不尊重 ctx 会让"取消请求"失效：前端断开后上游调用仍在跑，
// 上游一多就把并发额度占满。
func verifyChatRespectsCancel(t TB, p Provider, id string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Chat() 在已取消的 ctx 上 panic: %v", r)
			}
			close(done)
		}()
		cs, err := p.Chat(ctx, Credential{Provider: id, UID: "contract-probe"}, []byte(`{}`))
		if cs.Body != nil {
			_ = cs.Body.Close()
		}
		_ = err
	}()

	select {
	case <-done:
		// 正常：立刻返回
	case <-time.After(15 * time.Second):
		t.Errorf("Chat() 在 ctx 已取消时 15 秒内未返回（不尊重取消会导致并发额度被占满）")
	}
}
