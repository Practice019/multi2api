// contract_hermetic_test.go 行为契约（**判据 2 的正式入口**）。
//
// ⚠ 本文件是**冻结形态**（用户指定的写法）：契约测试必须能在
// "没有任何真实凭证、也不碰网络"的情况下跑完 —— 因为它在 CI 里跑。
//
//	func TestContract(t *testing.T) {
//	    gateway.RunProviderContract(t, func() gateway.Provider { return NewProvider(Config{}) })
//	}
//
// # 契约会检查什么（与本包相关的四条）
//
//	ID() 合法且稳定（^[a-z][a-z0-9-]*$，因为要当模型名前缀）
//	Caps() 必须含 CapChat
//	声明了 CapModels ⇒ Models() 必须**真的返回非空**（伪造凭证也要非空）
//	Chat 不得 panic、返回的流必须能 Close、ctx 取消必须尽快返回
//
// # 为什么"伪造凭证"是本包最关键的一条约束
//
// 契约默认用一个伪造凭证（Provider/UID = "contract-probe"）跑，那个
// Credential.Secret 是 nil。所以：
//
//	Models() 必须 `a, _ := authOf(cred)` —— 断言失败也要能回答
//	Chat()   必须**不 panic**、且在 ctx 已取消时立刻返回（连一次网络都不发）
//
// 这两条都在实现里有对应的注释（见 provider.go 的 Models / Chat）。
package phanthywork

import (
	"context"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestContract 行为契约。**不碰网络**：用伪造凭证跑，网络失败被容忍。
func TestContract(t *testing.T) {
	gateway.RunProviderContract(t, func() gateway.Provider { return NewProvider(Config{}) })
}

// TestProviderSatisfiesDeclaredExtensions 把"扩展点必须能被 ExtOf 发现"钉住。
//
// ⚠ 这条是**独立于契约测试**的：契约只检查"声明了能力位 ⇒ 必须实现"，
// 而"实现了扩展点却写成访问器"是另一类静默失效（本仓实测过四个上游
// 因此一直没有「＋ 添加账号」按钮）。
func TestProviderSatisfiesDeclaredExtensions(t *testing.T) {
	p := NewProvider(Config{})
	if _, ok := gateway.ExtOf[gateway.LoginFlow](p); !ok {
		t.Error("Provider 必须能被 ExtOf[LoginFlow] 发现（访问器形态发现不了）")
	}
	if _, ok := gateway.ExtOf[gateway.ManualLoginExt](p); !ok {
		t.Error("Provider 必须能被 ExtOf[ManualLoginExt] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.AuthDirExt](p); !ok {
		t.Error("Provider 必须能被 ExtOf[AuthDirExt] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.CredentialLoader](p); !ok {
		t.Error("Provider 必须能被 ExtOf[CredentialLoader] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.CredentialSecretLoader](p); !ok {
		t.Error("Provider 必须能被 ExtOf[CredentialSecretLoader] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.CredentialExpiryExt](p); !ok {
		t.Error("Provider 必须能被 ExtOf[CredentialExpiryExt] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.CredentialTokenExt](p); !ok {
		t.Error("Provider 必须能被 ExtOf[CredentialTokenExt] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.ErrorClassifier](p); !ok {
		t.Error("Provider 必须能被 ExtOf[ErrorClassifier] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.CredentialRefresher](p); !ok {
		t.Error("Provider 必须能被 ExtOf[CredentialRefresher] 发现")
	}
	if _, ok := gateway.ExtOf[gateway.DisplayNameExt](p); !ok {
		t.Error("Provider 必须能被 ExtOf[DisplayNameExt] 发现")
	}
}

// TestModelsNeverEmptyWithFakeCredential 无凭证也要返回非空目录。
//
// ⚠ 报告 §9 的原话：「契约测试用伪造凭证跑，一上来 return nil, err 会被判
// 「声明了 CapModels 却没实现」」。这条把那个行为单独钉一次（即使契约
// 因为别的原因为红，这条也能独立给出结论）。
func TestModelsNeverEmptyWithFakeCredential(t *testing.T) {
	p := NewProvider(Config{})
	cases := []gateway.Credential{
		{Provider: providerID, UID: "contract-probe"},          // Secret = nil
		{Provider: providerID, UID: "x", Secret: "wrong-type"}, // 错型
		{Provider: providerID, UID: "y", Secret: (*Auth)(nil)}, // nil 指针（断言会成功）
		{Provider: providerID, UID: "z", Secret: &Auth{}},      // 空 Auth
	}
	for i, c := range cases {
		ms, err := p.Models(context.Background(), c)
		if err != nil {
			t.Errorf("用例 %d：Models 不该报错（伪造凭证也必须能回兜底目录）: %v", i, err)
			continue
		}
		if len(ms) == 0 {
			t.Errorf("用例 %d：Models 返回空列表 —— 会被判「声明了 CapModels 却没实现」", i)
		}
	}
	// 兜底目录必须包含免费模型（它是唯一无条件免费的那条）
	ms, _ := p.Models(context.Background(), gateway.Credential{Provider: providerID})
	found := false
	for _, m := range ms {
		if m.ID == DefaultModelID {
			found = true
			if m.ContextWindow != 262_144 {
				t.Errorf("%s 的上下文窗口 = %d, want 262144（实测 /api/models/current）", m.ID, m.ContextWindow)
			}
		}
		if strings.HasPrefix(m.ID, providerID+"/") {
			t.Errorf("目录里的 id 不该带 phanthywork/ 前缀（坑③：核心负责拼前缀）: %q", m.ID)
		}
	}
	if !found {
		t.Errorf("兜底目录里缺少 %s", DefaultModelID)
	}
	// 目录条数应与报告的 14 条一致（远端不可用时就是这份静态表）
	if len(ms) != len(fallbackCatalog) {
		t.Errorf("无凭证时的目录条数 = %d, want %d（报告 §5 实测 14 个）", len(ms), len(fallbackCatalog))
	}
}

// TestChatOnCanceledContextReturnsFast ctx 已取消 ⇒ 立刻返回，不发请求。
//
// 契约要求"不尊重取消会让前端断开后上游调用仍在跑，占满并发额度"。
// 这条独立复现一次，是因为契约里那条有 15 秒容忍 —— 这里要求**立刻**。
func TestChatOnCanceledContextReturnsFast(t *testing.T) {
	p := NewProvider(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := p.Chat(ctx, gateway.Credential{}, []byte("{}")); err == nil {
			t.Error("已取消的 ctx 上 Chat 应返回错误")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Chat 在已取消的 ctx 上没有立刻返回")
	}
}
