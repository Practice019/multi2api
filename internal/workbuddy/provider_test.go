package workbuddy

import (
	"context"
	"os"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// Task 3a：把 workbuddy 上游适配成 gateway.Provider。
//
// # 为什么先做这个（而不是先搬业务）
//
// 这一步**只做薄适配**，不搬任何业务逻辑。目的是验证一件事：
// **gateway 的 4 方法接口能不能装下现有的 workbuddy**。
//
// 如果装不下（比如 Chat 需要额外参数、Models 需要凭证结构、Caps 无法表达），
// 现在发现比搬完 350 处业务后再发现便宜得多。
// 这就是 plan 里说的"抽的过程本身就是对接口的第一次检验"。

// TestContract 接入契约测试 —— 判据 2 的落地方式。
//
// # 分成两段，而不是"无凭证就整体跳过"
//
// 契约里有一部分检查**不需要凭证**（ID 合法且稳定、Caps 含 CapChat、
// 不 panic、流能 Close、ctx 取消能返回）。无条件跳过会把这些也丢掉，
// 让 CI 在无凭证环境下"看起来很绿"却什么都没验。
//
// 所以：
//   - 无凭证 → 跑 TestContractNoCredential（能验的那部分 + 明确记录未验证项）
//   - 有凭证 → 跑 TestContractWithCredential（完整行为验证）
func TestContractNoCredential(t *testing.T) {
	p := New()

	// ID 合法且稳定
	id1, id2 := p.ID(), p.ID()
	if id1 != providerID || id2 != id1 {
		t.Errorf("ID 应稳定为 %q，得到 %q / %q", providerID, id1, id2)
	}
	// Caps 含 CapChat
	if !p.Caps().Has(gateway.CapChat) {
		t.Error("必须声明 CapChat")
	}
	// Secret 类型不对时返回错误而非 panic（不需要凭证）
	if _, err := p.Models(context.Background(), gateway.Credential{Secret: 42}); err == nil {
		t.Error("凭证类型不对应返回错误")
	}
	// Chat 传无效凭证不得 panic
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Chat 不该 panic: %v", r)
			}
		}()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 已取消 → 应立刻返回
		_, _ = p.Chat(ctx, gateway.Credential{}, []byte(`{}`))
	}()

	// 明确记录：哪些契约项**没有**被验证
	t.Log("未验证（需凭证）：Models 非空、Chat 流可 Close、" +
		"Chat 成功时响应非空。设 WORKBUDDY_TEST_TOKEN 可完整验证。")
}

// TestContractWithCredential 完整契约（需要真实凭证）。
func TestContractWithCredential(t *testing.T) {
	tok := os.Getenv("WORKBUDDY_TEST_TOKEN")
	if tok == "" {
		t.Skip("无 WORKBUDDY_TEST_TOKEN —— 跳过需要凭证的完整契约。" +
			"注意：跳过 ≠ 通过。TestContractNoCredential 已覆盖不需要凭证的部分。")
	}
	gateway.RunProviderContract(t, New, gateway.WithCredential(gateway.Credential{
		Provider: providerID,
		UID:      "contract-test",
		Secret:   &auth.Auth{AccessToken: tok},
	}))
}

// TestProviderIdentity 身份与能力声明。
func TestProviderIdentity(t *testing.T) {
	p := New()

	if got := p.ID(); got != "workbuddy" {
		t.Errorf("ID()=%q want %q（会作为模型名前缀出现）", got, "workbuddy")
	}

	caps := p.Caps()
	if !caps.Has(gateway.CapChat) {
		t.Error("必须声明 CapChat")
	}
	if !caps.Has(gateway.CapModels) {
		t.Error("workbuddy 支持动态模型目录，应声明 CapModels")
	}
	// workbuddy 的专属能力
	for _, c := range []gateway.Capability{gateway.CapCheckin, gateway.CapGrowth, gateway.CapTravel} {
		if !caps.Has(c) {
			t.Errorf("workbuddy 具备该能力但未声明: %v", gateway.String(c))
		}
	}
	// CapQuotaProbe：Task 3c 修正 —— workbuddy 有**主动**额度探测
	// （RefreshCredits 直接调上游 get-user-resource，不依赖对话响应推断），
	// 而 /admin/credits/refresh 端点在 3a 的清单里就已标成 CapQuotaProbe。
	// 两边必须一致，否则前端按能力位渲染时那个入口会静默消失。
	if !caps.Has(gateway.CapQuotaProbe) {
		t.Error("workbuddy 有主动额度探测（RefreshCredits），且 /admin/credits/refresh " +
			"已声明 CapQuotaProbe，应一并声明该能力位")
	}
	// 不该声明没有的
	for _, c := range []gateway.Capability{gateway.CapWelfare} {
		if caps.Has(c) {
			t.Errorf("workbuddy 不具备该能力，不该声明: %v", gateway.String(c))
		}
	}
}

// TestImplementsProvider 编译期断言（放运行时是不让它被优化掉）。
func TestImplementsProvider(t *testing.T) {
	var _ gateway.Provider = New()
}

// TestCapsMatchAdminExt 声明了签到/成长/旅行 ⇒ 必须实现 AdminExt。
//
// 这条是契约里"假声明检查"的具体体现：
// 声明了这些能力（都通过管理端点暴露），却没有 AdminRoutes 就是没实现。
func TestCapsMatchAdminExt(t *testing.T) {
	p := New()
	ax, ok := gateway.ExtOf[gateway.AdminExt](p)
	if !ok {
		t.Fatal("声明了 CapCheckin/CapGrowth/CapTravel，必须实现 gateway.AdminExt")
	}
	routes := ax.AdminRoutes()
	if len(routes) == 0 {
		t.Fatal("AdminRoutes() 返回空 —— 声明了能力却没有端点")
	}
	// 每个端点必须有方法、路径、处理器
	for i, r := range routes {
		if r.Method == "" || r.Path == "" || r.Handler == nil {
			t.Errorf("AdminRoutes()[%d] 不完整: %+v", i, r)
		}
		if r.Path[0] != '/' {
			t.Errorf("AdminRoutes()[%d].Path 应以 / 开头: %q", i, r.Path)
		}
	}
}

// TestSecretTypeAssertionIsSafe 凭证类型不符时返回明确错误，不 panic。
//
// gateway.Credential.Secret 是 any（各上游凭证结构不同）。
// 传错类型必须有可读错误 —— 否则表现为 nil 解引用崩溃。
func TestSecretTypeAssertionIsSafe(t *testing.T) {
	p := New()
	// 传一个明显不对的 Secret
	cred := gateway.Credential{
		Provider: "workbuddy",
		UID:      "u1",
		Secret:   "这不是 auth.Auth",
	}
	// 不应 panic；应返回错误
	_, err := p.Models(context.Background(), cred)
	if err == nil {
		t.Error("Secret 类型不对时应返回错误，实际 nil")
	}
}
