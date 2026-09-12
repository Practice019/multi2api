package gateway

import (
	"context"
	"fmt"
	"io"
	"regexp"
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
// 它检查的是**行为契约**，不是"能不能编译"。检查项：
//
//	1. ID() 合法（^[a-z][a-z0-9-]*$）
//	2. Caps() 必须声明 CapChat（没有对话能力的上游接进来没意义）
//	3. 任何方法都不得 panic
//	4. Chat 返回的流必须能被完整读完并 Close，且 Close 不得报错（否则连接泄漏）
//	5. Caps() 声明的能力必须真的可用（声明 CapModels 却 Models 报错 = 假声明）
//	6. ctx 取消时 Chat 必须尽快返回（不能无视取消把 goroutine 挂住）
//
// factory 而不是直接传实例：契约测试要多次独立取实例，避免用例间互相污染。
func RunProviderContract(t TB, factory func() Provider) {
	t.Helper()

	if factory == nil {
		t.Fatalf("RunProviderContract: factory 不能为 nil")
		return
	}

	// ---- 1. 构造不 panic ----
	p := construct(t, factory)
	if p == nil {
		t.Fatalf("factory 返回 nil Provider")
		return
	}

	// ---- 2. ID 合法 ----
	id := callString(t, "ID", p.ID)
	if id == "" {
		t.Errorf("ID() 为空（注册与路由都依赖它）")
	} else if !validProviderID(id) {
		t.Errorf("ID()=%q 非法：要求 ^[a-z][a-z0-9-]*$（要作为模型名前缀）", id)
	}

	// ---- 3. Caps 必须含 CapChat ----
	caps := callCaps(t, p)
	if !caps.Has(CapChat) {
		t.Errorf("Caps() 未声明 CapChat —— 没有对话能力的上游接入无意义（got %v）", caps.Names())
	}

	// ---- 4. 声明的能力必须真的可用 ----
	if caps.Has(CapModels) {
		verifyModels(t, p, id)
	}

	// ---- 5. Chat 行为 ----
	if caps.Has(CapChat) {
		verifyChatNoPanic(t, p, id)
		verifyChatStreamClosable(t, p, id)
		verifyChatRespectsCancel(t, p, id)
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

// verifyModels 检查声明了 CapModels 就真的能返回目录。
func verifyModels(t TB, p Provider, id string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Models() panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ms, err := p.Models(ctx, Credential{Provider: id, UID: "contract-probe"})
	if err != nil {
		// 拿不到目录可能是"凭证无效"，但**假声明**（根本没实现）也长这样。
		// 契约无法区分二者，因此只在错误信息里提示，不直接判失败 ——
		// 真正的"假声明"由各上游自己的测试覆盖。
		t.Errorf("声明了 CapModels 但 Models() 返回错误: %v"+
			"（若这是凭证问题请改用有效凭证跑契约；若是没实现就该去掉 CapModels）", err)
		return
	}
	for i, m := range ms {
		if m.ID == "" {
			t.Errorf("Models()[%d].ID 为空", i)
		}
	}
}

// verifyChatNoPanic 用**空请求体**调用 Chat，检查不 panic。
//
// 空 body 是刻意的：契约只要求"不崩"，不要求它成功 ——
// 上游会因为请求非法而报错，那是正常行为。
func verifyChatNoPanic(t TB, p Provider, id string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Chat() panic: %v（契约要求任何输入都不得 panic）", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cs, err := p.Chat(ctx, Credential{Provider: id, UID: "contract-probe"}, []byte(`{}`))
	// 无论成功还是报错都接受；这里只验证"不 panic"。
	if cs.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(cs.Body, 4096))
		_ = cs.Body.Close()
	}
	_ = err
}

// verifyChatStreamClosable 检查返回的流**能被 Close 且不报错**。
//
// 这是最容易漏的一条：Provider 若返回一个包装了 net.Conn 的流却不正确
// 转发 Close，会导致连接池耗尽 —— 上游一多就会表现成"跑一会儿全卡住"。
func verifyChatStreamClosable(t TB, p Provider, id string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Chat() panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cs, err := p.Chat(ctx, Credential{Provider: id, UID: "contract-probe"}, []byte(`{}`))
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

// modelPrefixRe 供其它包复用：模型名的前缀格式与 Provider ID 一致。
var modelPrefixRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidModelPrefix 报告一个字符串是否可作为模型名前缀（= Provider ID 格式）。
func ValidModelPrefix(s string) bool { return modelPrefixRe.MatchString(s) }

var _ = fmt.Sprintf // 保留 fmt 依赖（错误信息格式化）
