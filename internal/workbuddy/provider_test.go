package workbuddy

import (
	"context"
	"testing"

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

// ⚠ 契约测试已移到 `contract_hermetic_test.go`。
//
// 这里原有两个测试：
//
//	TestContractNoCredential   手写了一个"契约子集"，**不调用** RunProviderContract
//	TestContractWithCredential 调了它，但无 token 时 t.Skip
//
// 阶段 0 评审的 F4 指出这构成判据 2 最实质的漏洞：
// CI（`go test ./...`，无 token）里 **RunProviderContract 从未被执行**，
// "必须通过契约"实际由手写的重复断言保证 —— 而手写断言会与契约各自漂移。
//
// 现在统一到 `contract_hermetic_test.go` 的 `TestContract`：用 httptest 假上游，
// 无需真实凭证即可让契约**完整执行**，因此 CI 里不会跳过。

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
