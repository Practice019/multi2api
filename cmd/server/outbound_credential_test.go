// outbound_credential_test.go 出站凭证装配的回归：`registryRouter.Credential`
// 必须对**两类上游**都能交出它们各自认识的凭证。
//
// # 为什么单独一个文件（这是端到端实测抓出来的真 bug）
//
// 第一版 `Credential(id, uid)` 写的是"取不到 secret 就 ok=false"。
// 逻辑看起来无懈可击 —— 出站当然需要凭证。但它漏掉了一个**部署形态上的
// 不对称**，而那个不对称是既有事实，不是缺陷：
//
//	codearts  用 SyncToDirWithSecrets 入池 → e.secret = *codearts.Auth
//	workbuddy 用 SyncToDir 入池（main.go:70）→ e.secret = **nil**
//
// 后者不需要 secret 通道：它的"私有凭证"就是 `*auth.Auth` 本身，
// 池子在 main.go 的 loadOrRestore 路径上已经把明文凭证直接装进了 e.a。
// 所以默认上游的 secret 通道**本来就是空的**。
//
// 实测后果（18099 实例）：**改造前完全能用的 workbuddy 路径被整个打挂**
//
//	POST /v1/chat/completions {"model":"glm-5.3"}
//	→ 选中 workbuddy 的号 → Credential 返回 ok=false
//	→ 出口层报 errNoProviderCredential → 换号
//	→ 三个号全换完 → 503
//	body: "all accounts unavailable ... 多上游模式下取不到该账号的凭证"
//
// 这是"修好新的、弄坏旧的"的教科书案例：三条 codearts 断言全绿，
// 而既有的主路径整条死了。
//
// # 本文件钉住的判据（形状判据，不是实现细节）
//
//	Credential(id, uid).Secret 必须是**该上游 authOf 期望的那个类型**
//
// 这条判据直接对应"上游能不能用这份凭证"，而不是"代码里走了哪个分支"。
// 用两个真实的 Provider 实现（workbuddy / codearts）各自的 authOf 去验 ——
// 它们的断言失败信息本来就会写清楚期望类型。
package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/workbuddy"
)

// setUpMixedPool 建一个"两个上游都有号"的池，装配形态与 main.go 一致：
//
//	workbuddy 走 SyncToDir（**不带 secret** —— 这就是既有形态）
//	codearts  走 SyncToDirWithSecrets（带 *codearts.Auth）
func setUpMixedPool(t *testing.T) (*gateway.Registry, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()

	// ---- codearts：落盘一份凭证再加载（与 registry_router_models_test.go 同款）----
	caPath := filepath.Join(dir, "codearts-TEST.json")
	if err := os.WriteFile(caPath, []byte(codeartsCredJSON("uid-ca-1", "AK1")), 0o600); err != nil {
		t.Fatal(err)
	}
	caList, err := codearts.LoadDir(dir)
	if err != nil {
		t.Fatalf("codearts.LoadDir: %v", err)
	}
	if len(caList) != 1 {
		t.Fatalf("codearts 凭证加载 %d 份 want 1（前置自检失败）", len(caList))
	}

	reg := gateway.NewRegistry()
	// 注册顺序：workbuddy 在前（它是默认上游，与 main.go 一致）。
	wb := workbuddy.NewWithConfig(workbuddy.Config{Provider: workbuddy.ProviderID})
	if err := reg.Register(wb); err != nil {
		t.Fatalf("注册 workbuddy: %v", err)
	}
	cb := codearts.NewWithConfig(codearts.Config{AuthDir: dir})
	cb.SetAccounts(func() []*codearts.Auth { l, _ := codearts.LoadDir(dir); return l })
	if err := reg.Register(cb); err != nil {
		t.Fatalf("注册 codearts: %v", err)
	}

	p := pool.New("")
	p.SetDefaultProvider(workbuddy.ProviderID)

	// workbuddy 入池：**SyncToDir**（不带 secret）。这是 main.go:70 的调用，
	// 也正是"默认上游的 secret 通道本来就是空的"这一事实的来源。
	wbAuth := &auth.Auth{
		UID: "uid-wb-1", AccessToken: "WB_AT", RefreshToken: "WB_RT",
		Nickname: "wb-nick", ExpiresAt: 9999999999,
	}
	p.SyncToDir([]*auth.Auth{wbAuth})

	// codearts 入池：带 secret。
	caAuths, caSecrets := codeartsAuthsFor(caList)
	p.SyncToDirWithSecrets(codearts.ProviderID, caAuths, caSecrets)

	return reg, p
}

// TestCredentialWorkbuddyFallsBackToAuth 是**主闸门**：默认上游（secret 通道为空）
// 必须拿到 `*auth.Auth`，而不是 ok=false。
//
// 变异验证（必须红）：把 registryRouter.Credential 里的
// `if secret, ok := ...; ok && secret != nil { ... } else { cred.Secret = a }`
// 改回 `if secret == nil { return ok=false }` → 本用例红，
// 且真机上表现为 workbuddy 全路径 503（端到端实测见文件头注释）。
func TestCredentialWorkbuddyFallsBackToAuth(t *testing.T) {
	reg, p := setUpMixedPool(t)
	r := registryRouter{reg: reg, p: p}

	cred, ok := r.Credential(workbuddy.ProviderID, "uid-wb-1")
	if !ok {
		t.Fatalf("★ 默认上游取不到凭证 —— workbuddy 路径会整体 503。\n" +
			"  默认上游用 SyncToDir 入池（不带 secret），secret 为空是**正常形态**，" +
			"应当回落到 *auth.Auth 本体。")
	}
	if cred.Secret == nil {
		t.Fatal("Secret 为 nil：上游的 authOf 会直接报『凭证为空』")
	}
	// 形状判据：workbuddy 的 authOf 只接受 *auth.Auth。
	a, isAuth := cred.Secret.(*auth.Auth)
	if !isAuth {
		t.Fatalf("Secret 类型 = %T，workbuddy 期望 *auth.Auth", cred.Secret)
	}
	if a.AccessToken != "WB_AT" {
		t.Errorf("拿到的是别的东西：AccessToken=%q want WB_AT", a.AccessToken)
	}
	if cred.UID != "uid-wb-1" {
		t.Errorf("UID=%q want uid-wb-1", cred.UID)
	}
	if cred.Provider != workbuddy.ProviderID {
		t.Errorf("Provider=%q want %q", cred.Provider, workbuddy.ProviderID)
	}
}

// TestCredentialCodeartsPrefersSecret codearts 必须拿到它自己的
// `*codearts.Auth`（**不能**被回落逻辑覆盖成通用凭证 —— 那样签名材料就没了）。
func TestCredentialCodeartsPrefersSecret(t *testing.T) {
	reg, p := setUpMixedPool(t)
	r := registryRouter{reg: reg, p: p}

	uid := p.AvailableUIDsFor(codearts.ProviderID)[0]
	cred, ok := r.Credential(codearts.ProviderID, uid)
	if !ok {
		t.Fatalf("codearts 取不到凭证")
	}
	ca, isCodeArts := cred.Secret.(*codearts.Auth)
	if !isCodeArts {
		t.Fatalf("Secret 类型 = %T，codearts 期望 *codearts.Auth\n"+
			"  ★ 说明回落逻辑把 secret 通道覆盖掉了 —— 签名材料会丢失。", cred.Secret)
	}
	if ca.AccessKey == "" {
		t.Error("codearts 凭证的 AccessKey 为空（签名会失败）")
	}
}

// TestCredentialBothProvidersUsableByTheirAuthOf 用两个真实 Provider 各自的
// authOf 期望来兜底验证（它们不接受错误类型，失败信息本来就会写清期望）。
//
// ⚠ 不能用 `Provider.Models(...)` 直接探：workbuddy 的 Models() 会**真的发
// 一次网络请求**（FetchModels），拿一份假 token 去会被上游回 401 ——
// 那是「凭证无效」，不是「类型不对」。契约层面这两者本来就不可区分
// （见 gateway.RunProviderContract 里关于 WithCredential 的注释：
// 「契约无法区分凭证不对与没实现」）。
//
// 所以这里用**类型断言 + 字段真值**验，那才是本文件要钉的东西。
func TestCredentialBothProvidersUsableByTheirAuthOf(t *testing.T) {
	reg, p := setUpMixedPool(t)
	r := registryRouter{reg: reg, p: p}

	// workbuddy：唯一可接受的 Secret 形态是 *auth.Auth。
	wbCred, ok := r.Credential(workbuddy.ProviderID, "uid-wb-1")
	if !ok {
		t.Fatal("workbuddy 取不到凭证")
	}
	if _, isAuth := wbCred.Secret.(*auth.Auth); !isAuth {
		t.Errorf("workbuddy 的 Secret 是 %T，它的 authOf 只接受 *auth.Auth", wbCred.Secret)
	}

	// codearts：唯一可接受的 Secret 形态是 *codearts.Auth（带 AK/SK/DPoP 私钥）。
	uid := p.AvailableUIDsFor(codearts.ProviderID)[0]
	caCred, ok := r.Credential(codearts.ProviderID, uid)
	if !ok {
		t.Fatal("codearts 取不到凭证")
	}
	ca, isCodeArts := caCred.Secret.(*codearts.Auth)
	if !isCodeArts {
		t.Fatalf("codearts 的 Secret 是 %T，它的 authOf 只接受 *codearts.Auth", caCred.Secret)
	}
	// 续期需要的材料必须都在（缺 DPoP 私钥就发不出 refresh）。
	if ca.AccessKey == "" || ca.SecretKey == "" {
		t.Error("codearts 凭证缺 AK/SK（签名会失败）")
	}
	if ca.RefreshToken == "" {
		t.Error("codearts 凭证缺 refresh_token（无法续期）")
	}
}

// TestCredentialUnknownProvider / UnknownUID：接线问题的两种形态都必须 ok=false
// （出口层据此换号，而不是拿着空凭证去发请求）。
func TestCredentialRejectsUnknownProviderAndUID(t *testing.T) {
	reg, p := setUpMixedPool(t)
	r := registryRouter{reg: reg, p: p}

	if _, ok := r.Credential("nope", "uid-wb-1"); ok {
		t.Error("未注册的上游应当 ok=false")
	}
	if _, ok := r.Credential(workbuddy.ProviderID, "no-such-uid"); ok {
		t.Error("池子里不存在的账号应当 ok=false")
	}
}

// ---------------------------------------------------------------------------
// 跨上游串号（P1 真漏网）：Credential 的**归属校验**
// ---------------------------------------------------------------------------
//
// # 被钉住的 bug
//
// registryRouter.Credential(id, uid) 原来只做两件事：
//
//	if _, ok := r.reg.Get(id); !ok { return ok=false }   // 上游已注册
//	a := r.p.AuthByUID(uid); if a == nil { return ok=false } // uid 在池里
//
// **它从不校验 uid 是否属于 id 那个上游。**
//
// 于是这条链路是活的（每一步都已存在，只有最后一步缺校验）：
//
//	1. 客户端先发 {"model":"codearts/GLM-5.2","metadata":{"conversation_id":"K"}}
//	   → Session.Bind("K", <codearts uid>) 并镜像到 Redis
//	2. 同一 conversation_id=K 下次改发 {"model":"glm-5.2"}（裸名 → 默认上游 workbuddy）
//	3. 粘性命中那个 codearts uid（handler 侧已由 PickByUIDFor 拦住 —— 见
//	   sticky_provider_test.go），但**任何其它调用方**（旧代码路径、运维接口、
//	   未来的新选号逻辑）只要拿着这个 uid 问 workbuddy 要凭证，就会走到这里
//	4. Credential("workbuddy", <codearts uid>) 返回
//	   Secret = *codearts.Auth 的 Credential
//	5. workbuddy 的 authOf 断言 *auth.Auth 失败 → 报错
//	   → **惩罚一个无辜的账号**（记错/冷却/熔断），而真正的错在路由
//
// # 为什么这条必须单独修（不能只靠粘性域对齐）
//
// 粘性侧的对齐（PickByUIDFor）只堵住了**一条**调用路径。Credential 是
// 出口层与上游之间**唯一的凭证装配点**，它自己不带归属校验 =
// 两道防线只剩一道：任何人再写一条调用路径就重新漏。
// 加校验后，跨上游串号被降级成**一次换号**（ok=false → 出口层
// errNoProviderCredential → 换号），而不是静默的类型错误 + 无辜号被罚。

// TestCredentialRejectsUIDFromOtherProvider 是本案的**主断言**：
// 拿 codearts 的 uid 去问 workbuddy 要凭证，必须 ok=false。
//
// 变异验证（必须红）：把 multiprovider.go 的 Credential 里那行归属校验
//
//	if got, ok := r.p.ProviderOf(uid); !ok || got != id { return ok=false }
//
// 删掉 → 本用例红：ok=true，且 Secret 是 *codearts.Auth（workbuddy 的
// authOf 拿到它会直接报错，并让这个无辜的 codearts 号被记账惩罚）。
func TestCredentialRejectsUIDFromOtherProvider(t *testing.T) {
	reg, p := setUpMixedPool(t)
	r := registryRouter{reg: reg, p: p}

	caUID := p.AvailableUIDsFor(codearts.ProviderID)
	if len(caUID) == 0 {
		t.Fatal("前置自检失败：codearts 池里没有可用的号")
	}
	otherUID := caUID[0]

	// 前置自检：这个 uid 在池里**确实存在**（否则本用例会因"uid 不存在"
	// 这条无关理由而假绿 —— 那正是修复前代码唯一的拦截点）。
	if p.AuthByUID(otherUID) == nil {
		t.Fatalf("前置自检失败：%q 不在池里，本用例测不到归属校验", otherUID)
	}

	cred, ok := r.Credential(workbuddy.ProviderID, otherUID)
	if ok {
		t.Fatalf("★ Credential(%q, %q) 返回了 ok=true —— 跨上游串号没被拦住。\n"+
			"  拿到 Secret 类型 = %T（workbuddy 的 authOf 只接受 *auth.Auth）。\n"+
			"  后果：workbuddy 用 codearts 的凭证发请求 → 报错 → **惩罚这个无辜的 codearts 账号**。",
			workbuddy.ProviderID, otherUID, cred.Secret)
	}

	// 反方向：拿 workbuddy 的 uid 去问 codearts，同样必须 ok=false。
	wbUIDs := p.AvailableUIDsFor(workbuddy.ProviderID)
	if len(wbUIDs) == 0 {
		t.Fatal("前置自检失败：workbuddy 池里没有可用的号")
	}
	if cred, ok := r.Credential(codearts.ProviderID, wbUIDs[0]); ok {
		t.Fatalf("★ Credential(%q, %q) 返回了 ok=true（Secret=%T）—— 反向串号没被拦住。",
			codearts.ProviderID, wbUIDs[0], cred.Secret)
	}
}

// TestCredentialSameProviderStillWorks 正面回归：归属校验**不得误伤正路**。
//
// 最省事的"修法"是把 Credential 改成恒 ok=false，或把归属判据写反 ——
// 那样跨上游测试全绿而两条真实路径全死（workbuddy 全 503、codearts 发不出签名）。
// 本用例守住：同一个上游的 (id, uid) 必须照常拿到**该上游自己的凭证类型**。
func TestCredentialSameProviderStillWorks(t *testing.T) {
	reg, p := setUpMixedPool(t)
	r := registryRouter{reg: reg, p: p}

	// workbuddy：uid 属于 workbuddy → ok=true，Secret 是 *auth.Auth。
	cred, ok := r.Credential(workbuddy.ProviderID, "uid-wb-1")
	if !ok {
		t.Fatalf("★ 同上游取凭证被归属校验误伤：Credential(%q, uid-wb-1) 返回 ok=false",
			workbuddy.ProviderID)
	}
	if _, isAuth := cred.Secret.(*auth.Auth); !isAuth {
		t.Errorf("workbuddy 的 Secret 是 %T，want *auth.Auth", cred.Secret)
	}

	// codearts：uid 属于 codearts → ok=true，Secret 是 *codearts.Auth。
	caUID := p.AvailableUIDsFor(codearts.ProviderID)[0]
	cred, ok = r.Credential(codearts.ProviderID, caUID)
	if !ok {
		t.Fatalf("★ 同上游取凭证被归属校验误伤：Credential(%q, %q) 返回 ok=false",
			codearts.ProviderID, caUID)
	}
	if _, isCodeArts := cred.Secret.(*codearts.Auth); !isCodeArts {
		t.Errorf("codearts 的 Secret 是 %T，want *codearts.Auth", cred.Secret)
	}
	if cred.UID != caUID {
		t.Errorf("UID=%q want %q（必须用调用方指定的那个号）", cred.UID, caUID)
	}
}
