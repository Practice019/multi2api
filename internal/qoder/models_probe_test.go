package qoder

// models_probe_test.go —— qoder 模型目录**必须是静态表**这件事的守卫（T-237）。
//
// # 为什么要有这个文件
//
// 用户的要求是"所有模型目录都不要硬编码，而是探测上游"。qoder 实测**做不到**，
// 原因是可复核的技术约束（不是"没人做"）：
//
//	远端 `GET /algo/api/v2/model/list`（常量见 qoder.go:198 modelListPath）
//	需要 WASM 签名，而我们的 WASM 接缝**只注册了一个请求构造入口**：
//
//	  qodercontext_prepareInferRequest   （internal/qoderwasm/wasm.go:212-218 的 import 表）
//
//	并且这个入口的产物被硬断言必须是加密**推理**端点：
//
//	  signer.go:409-422  → if !strings.Contains(url, encryptedInferMarker) { 报错 }
//	  encryptedInferMarker = "/algo/api/v2/service/pro/sse/agent_chat_generation"
//
//	即：**没有任何入口能构造一个"对任意 GET 请求签名"的调用**。
//	所以 qoder 的 Models() 与其余 9 个上游不同，它**恒不发网络请求** ——
//	静态表不是偷懒，是当前签名能力的边界。
//
// # 什么时候该删掉这个文件
//
// 当 WASM 多出一个通用请求构造入口（或证明 `generate_runtime_auth_fields`
// 产出的账号级字段足以让 `/algo/api/v2/model/list` 通过鉴权）时，
// 应该：① 先做一次受控真实请求确认**返回的 ID 形态**；② 注意公开端点认"通用名"
// （qwen-flash 这类），而我们的加密端点只认**目录 key**（models.go:8-11）——
// 若上游只给通用名，探测结果**不能直接替换静态表**，只能用来交叉核对。
// ③ 改 Models() 时把本文件改成"探测 + 回退"的双路径测试。

import (
	"context"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestModelsSkipsNetwork Models() **不该发任何网络请求**。
//
// 写法与本包既有的三个同类守卫一致（TestRefreshSkewSkipsNetwork /
// TestTokenExpirySkipsNetwork / TestHasTokenSkipsNetwork）：把 base 指到一个
// 必然连不上的地址，再用看门狗判"它有没有在等网络"。
func TestModelsSkipsNetwork(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	cred := gateway.Credential{Secret: testAuth()}

	done := make(chan struct{})
	var (
		got []gateway.ModelInfo
		err error
	)
	go func() {
		defer close(done)
		got, err = p.Models(context.Background(), cred)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Models() 超过 2 秒 —— 它不该发网络请求（静态表是唯一的目录来源，见本文件头）")
	}
	if err != nil {
		t.Fatalf("Models() 报错: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Models() 返回空目录 —— 静态表是唯一来源，空表会让前端把该上游的模型全判为不存在")
	}
}

// TestModelsReturnsStaticTableForProduct 目录内容**逐条等于** ModelsFor(productID)。
//
// 这条钉住"静态表是权威"：若有人改成"探测优先"，在探测失败/为空时仍必须回到
// 这张表；而若他把表本身改坏（漏项、串了产品线），这里会红。
func TestModelsReturnsStaticTableForProduct(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string // productID
	}{
		{"国际版", Config{}, Qoder.ID},
		{"中国版", Config{Product: QoderCN}, QoderCN.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewWithConfig(tc.cfg)
			got, err := p.Models(context.Background(), gateway.Credential{Secret: testAuth()})
			if err != nil {
				t.Fatalf("Models() 报错: %v", err)
			}
			want := ModelsFor(tc.want)
			if len(got) != len(want) {
				t.Fatalf("目录条数 = %d，want %d（必须与 ModelsFor(%q) 一致）", len(got), len(want), tc.want)
			}
			for i := range want {
				if got[i].ID != want[i].ID {
					t.Errorf("第 %d 条 ID = %q，want %q", i, got[i].ID, want[i].ID)
				}
				if got[i].ContextWindow != want[i].ContextWindow {
					t.Errorf("%s 的 ContextWindow = %d，want %d", want[i].ID, got[i].ContextWindow, want[i].ContextWindow)
				}
			}
		})
	}
}

// TestModelsProductsDifferModels 实例的两条产品线**必须给出不同的目录**。
//
// 这是 models.go 顶部那条实测教训的守卫（CN 沿用国际版表 ⇒ 菜单里出现 5 个
// CN 端点根本不认的模型，点了就报错）。
func TestModelsProductsDifferModels(t *testing.T) {
	intl, err := NewWithConfig(Config{}).Models(context.Background(), gateway.Credential{Secret: testAuth()})
	if err != nil {
		t.Fatalf("国际版 Models() 报错: %v", err)
	}
	cn, err := NewWithConfig(Config{Product: QoderCN}).Models(context.Background(), gateway.Credential{Secret: testAuth()})
	if err != nil {
		t.Fatalf("中国版 Models() 报错: %v", err)
	}
	if len(intl) == 0 || len(cn) == 0 {
		t.Fatal("两条产品线的目录都不该为空")
	}
	same := len(intl) == len(cn)
	if same {
		for i := range intl {
			if intl[i].ID != cn[i].ID {
				same = false
				break
			}
		}
	}
	if same {
		t.Error("国际版与中国版给出了完全相同的目录 —— 沿用会让 CN 菜单出现端点不认的模型（见 models.go 顶部实测）")
	}
}

// TestModelsHonorsCanceledContext 已取消的 ctx 必须立刻报错（provider.go:298-300 的契约）。
func TestModelsHonorsCanceledContext(t *testing.T) {
	p := NewWithConfig(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Models(ctx, gateway.Credential{Secret: testAuth()}); err == nil {
		t.Error("ctx 已取消却返回 nil error —— 契约要求先返回 ctx.Err()")
	}
}
