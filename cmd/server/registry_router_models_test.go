// registry_router_models_test.go T1 的核心回归：registryRouter.Models
// 必须能真的拿到各上游的模型目录。
//
// # 这个文件守的是什么（用户实测的 bug）
//
// 用户点界面「刷新模型」，**codearts 的模型扫不出来**。
//
// 根因（本文件用真实 codearts.Provider 复现）：
//
//	registryRouter.Models 用 gateway.Credential{Provider: id} 调上游 ——
//	**Secret 是 nil**。而 codearts.Provider.Models() 第一件事就是 authOf(cred)，
//	它对 nil Secret 返回错误：
//
//	    codearts: 凭证为空（Credential.Secret 未设置）
//
//	于是 registryRouter 拿到的 err != nil → 返回 ok=false →
//	出口层的 modelList 走 `continue` → codearts 被静默跳过。
//
// **codearts 其实完全能报模型**（Models() 是静态表、不发网络请求）。
// 只是没人给它一份凭证。
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/workbuddy"
)

// codeartsCredJSON 造一份 codearts 凭证（扁平形，与 multiprovider_test.go 同款）。
func codeartsCredJSON(uid, ak string) string {
	return `{"accessKeyId":"` + ak + `",` +
		`"secretAccessKey":"` + ak + `_SECRET",` +
		`"securityToken":"` + ak + `_ST",` +
		`"expiresAt":9999999999,` +
		`"refresh_token":"rt_` + ak + `",` +
		`"clientId":"vscode-codebot",` +
		`"uid":"` + uid + `",` +
		`"nickname":"` + ak + `_nick"}`
}

// setUpCodeartsRegistry 建一个真实的 codearts Provider + 账号池，
// 装配形态与 cmd/server/main.go 一致（SetAccounts + SyncToDirWithSecrets）。
func setUpCodeartsRegistry(t *testing.T, withAccount bool) (*gateway.Registry, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()

	var list []*codearts.Auth
	if withAccount {
		path := filepath.Join(dir, "codearts-TEST.json")
		if err := os.WriteFile(path, []byte(codeartsCredJSON("uid-ca-1", "AK1")), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		list, err = codearts.LoadDir(dir)
		if err != nil {
			t.Fatalf("LoadDir: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("凭证加载 %d 份 want 1（前置自检失败）", len(list))
		}
	}

	reg := gateway.NewRegistry()
	cb := codearts.NewWithConfig(codearts.Config{AuthDir: dir})
	cb.SetAccounts(func() []*codearts.Auth {
		l, _ := codearts.LoadDir(dir)
		return l
	})
	if err := reg.Register(cb); err != nil {
		t.Fatalf("注册 codearts: %v", err)
	}
	// 再加一个 workbuddy 作为默认上游，保证 router.Default() 有值。
	wb := workbuddy.NewWithConfig(workbuddy.Config{Provider: workbuddy.ProviderID})
	if err := reg.Register(wb); err != nil {
		t.Fatalf("注册 workbuddy: %v", err)
	}

	p := pool.New("")
	if withAccount {
		auths, secrets := codeartsAuthsFor(list)
		p.SyncToDirWithSecrets(codearts.ProviderID, auths, secrets)
	}
	return reg, p
}

// TestRegistryRouterModelsCodearts 是 T1 的**失败复现**：
// 账号在池里、Provider 声明了 CapModels、Models() 能给出模型 ——
// router.Models 必须返回 ok=true。
//
// ⚠ 注意本用例的断言重点不是"几条"，而是 **ok=true**：
// 若 router 不给上游传 secret，codearts.Provider.Models() 会在 authOf(nil)
// 上直接失败 → ok=false → 模型在 /v1/models 里被静默跳过（用户报的"扫不出来"）。
//
// 暴露的**内容**：静态目录里 7 个存在的模型都必须出现。
// 注意这里**不**按额度过滤 —— codearts 的 benefit 通道是**每日**免费额度，
// 当天用完不算"模型不存在"（曾经的错误结论，见 channel_test.go 的纠错注释）。
// 过滤只发生在 Provider.Models 且是可逆的（日切自动失效），
// 而本用例跑在未标记额度的干净状态下，因此应当看到全部 7 个。
func TestRegistryRouterModelsCodearts(t *testing.T) {
	reg, p := setUpCodeartsRegistry(t, true)
	r := registryRouter{reg: reg, p: p}

	ms, ok := r.Models(context.Background(), "codearts")
	if !ok {
		t.Fatalf("★ registryRouter.Models(codearts) 返回 ok=false —— " +
			"codearts 的模型在 /v1/models 里被静默跳过（这就是用户报的『扫不出来』）")
	}
	if len(ms) == 0 {
		t.Fatal("ok=true 却一个模型都没有")
	}

	got := map[string]bool{}
	for _, m := range ms {
		got[m.ID] = true
	}

	// 目录必须包含全部 7 个**存在**的模型（大小写逐字符）。
	//
	// ⚠ 早先这里断言过"3 个 benefit 模型不得出现"—— 那是错的：
	// 它们只是 **benefit 通道的每日免费额度**当天用完，次日重置，
	// 不是模型不存在。是否"当前不可用"由 quotaCache 表达，
	// 而且**默认（未标记）状态下必须全部列出** ——
	// 这正是本用例要钉的不变式：目录层不得把"临时额度"当成"不存在"。
	for _, id := range []string{
		"GLM-5.2", "glm-5.2-sft-harmony",
		"openpangu-2.0-pro", "openpangu-2.0-flash",
		"deepseek-v4-flash-0731", "deepseek-v4-pro-0813",
		"glm-5.3-flash",
	} {
		if !got[id] {
			t.Errorf("缺少模型 %q（它在本账号上存在，只是额度可能每日耗尽）", id)
		}
	}
}

// TestRegistryRouterModelsNoAccountSkips 没账号的上游仍返回 ok=false ——
// 这是**刻意保留**的行为：没号必然拉不到，省掉一次注定失败的往返。
//
// ⚠ 注意区分两类 ok=false：
//
//	没账号        → 跳过是对的
//	有账号但没给凭证 → **bug**（本文件另一个用例钉住）
func TestRegistryRouterModelsNoAccountSkips(t *testing.T) {
	reg, p := setUpCodeartsRegistry(t, false)
	r := registryRouter{reg: reg, p: p}
	if _, ok := r.Models(context.Background(), "codearts"); ok {
		t.Fatal("没账号时应当返回 ok=false")
	}
}

// TestRegistryRouterModelsUnknownProvider 未注册的上游返回 ok=false。
func TestRegistryRouterModelsUnknownProvider(t *testing.T) {
	reg, p := setUpCodeartsRegistry(t, true)
	r := registryRouter{reg: reg, p: p}
	if _, ok := r.Models(context.Background(), "nope"); ok {
		t.Fatal("未注册上游应当返回 ok=false")
	}
}

// codeartsAuthsFor 把 codearts 凭证投影成核心形态（与 main.go 的
// dedupeCodeartsByUID 返回的两件套同形状：auth.Auth 投影 + 不透明 secret）。
func codeartsAuthsFor(list []*codearts.Auth) ([]*auth.Auth, map[string]any) {
	secrets := make(map[string]any, len(list))
	auths := make([]*auth.Auth, 0, len(list))
	for _, ca := range list {
		secrets[ca.UID] = ca
		auths = append(auths, &auth.Auth{UID: ca.UID, Nickname: ca.Nickname})
	}
	return auths, secrets
}
