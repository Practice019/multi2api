package admin

// reload_secrets_test.go —— 「重载 auths」必须把上游的**不透明 secret** 一起装进池子。
//
// # 这条守的是评审 R2
//
// 只用 `gateway.CredentialLoader` 时，核心拿到的是**投影后的 uid/nickname**
//（`Credential` 里没有 secret 字段），于是同步池子只能调
// `SyncToDirFor(provider, auths)` —— 那些条目 `e.secret == nil`。
//
// 对 codearts 这种"真凭证不在 *auth.Auth 里、走池的不透明 secret 通道"的上游：
//
//	启动后新拷入 / 新登录的凭证 → 用户点「重载 auths」
//	→ 池里多出该 uid，但没有 secret
//	→ 该号被选中时取不到可用的 *codearts.Auth（类型不对）→ 必失败
//
// 而按 store 同步账号的路径**只在启动时跑一次** → 得重启网关才恢复。
// 这恰好推翻了 codeartscreds.go 里那句"三条路径必须同源"的声明：
// 对启动后新增的号，不是"造出第二个对象"，而是"一个对象都没有"。
//
// 修法：新增可选扩展点 `gateway.CredentialSecretLoader`，实现它的上游
// 由核心改调 `Pool.SyncToDirWithSecrets`。核心全程不认识上游类型
//（secret 是不透明的 `any`），"secret 从哪来"由上游自己回答。

import (
	"net/http"
	"path/filepath"
	"reflect"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// secretProvider 同时实现 CredentialLoader 与 CredentialSecretLoader。
type secretProvider struct {
	stubProvider
	dir   string
	items []gateway.CredentialSecret
}

func (p *secretProvider) AuthDir() string { return p.dir }

func (p *secretProvider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	out := make([]gateway.Credential, 0, len(p.items))
	for _, it := range p.items {
		c := it.Credential
		c.Provider = p.ID()
		out = append(out, c)
	}
	return out, nil
}

func (p *secretProvider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	out := make([]gateway.CredentialSecret, 0, len(p.items))
	for _, it := range p.items {
		c := it.Credential
		c.Provider = p.ID()
		out = append(out, gateway.CredentialSecret{Credential: c, Secret: it.Secret})
	}
	return out, nil
}

// 编译期断言：桩的三个扩展点都要被认出来。
// 少了 ExtOf 断言，失配会**静默**退化 → 用例红得莫名其妙。
var (
	_ gateway.AuthDirExt             = (*secretProvider)(nil)
	_ gateway.CredentialLoader       = (*secretProvider)(nil)
	_ gateway.CredentialSecretLoader = (*secretProvider)(nil)
)

// plainLoaderProvider **只**实现 CredentialLoader（对照组的回落路径）。
type plainLoaderProvider struct {
	stubProvider
	dir   string
	creds []gateway.Credential
}

func (p *plainLoaderProvider) AuthDir() string { return p.dir }

func (p *plainLoaderProvider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	out := make([]gateway.Credential, 0, len(p.creds))
	for _, c := range p.creds {
		c.Provider = p.ID()
		out = append(out, c)
	}
	return out, nil
}

var (
	_ gateway.AuthDirExt       = (*plainLoaderProvider)(nil)
	_ gateway.CredentialLoader = (*plainLoaderProvider)(nil)
)

// TestReloadInstallsOpaqueSecretIntoPool 钉住"带 secret 的扫描器"这条分支。
//
// 变异：把 accountsReload 里 `if secrets != nil { SyncToDirWithSecrets } else { SyncToDirFor }`
// 换回无条件 `SyncToDirFor` → 本用例的 SecretOf 必须变红。
func TestReloadInstallsOpaqueSecretIntoPool(t *testing.T) {
	base := t.TempDir()
	caDir := filepath.Join(base, "codearts")

	// 不透明的 secret：核心**不认识**它的类型，只搬运。
	// 用 map 而不是某个上游结构体，正是为了证明这一点（本包不得 import 上游包）。
	secret := map[string]string{"kind": "opaque-upstream-cred", "token": "T-1"}

	reg := gateway.NewRegistry()
	prov := &secretProvider{
		stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
		dir:          caDir,
		items: []gateway.CredentialSecret{
			{Credential: gateway.Credential{UID: "ca-new", Nickname: "新号"}, Secret: secret},
		},
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("codearts")

	h := New(Config{
		Pool:            p,
		Registry:        reg,
		AuthDir:         caDir,
		AuthsBase:       base,
		DefaultProvider: "codearts",
	})

	rec, resp := postReload(t, h, `{"provider":"codearts"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload 失败: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if resp.Scanned != 1 {
		t.Fatalf("scanned 应为 1，实际 %d", resp.Scanned)
	}

	got, ok := p.SecretOf("ca-new")
	if !ok {
		t.Fatal("池里 ca-new 没有 secret —— 重载路径没把上游的不透明凭证装进去。\n" +
			"对 codearts 这类上游，该号被选中时必然取不到可用的 *codearts.Auth（类型不对）→ 必失败，\n" +
			"而按 store 同步的路径只在启动时跑一次，用户只能重启网关。")
	}
	if !reflect.DeepEqual(got, secret) {
		t.Fatalf("池里存的不透明 secret 与上游给出的不是同一份：\n  got  = %#v\n  want = %#v", got, secret)
	}
}

// TestReloadWithoutSecretLoaderLeavesNoSecret 钉住**回落路径**的现状。
//
// 这条不是在测"期望行为"，而是把"没实现 CredentialSecretLoader 会发生什么"
// 固定下来 —— 它正是 codearts 必须实现那个接口的理由。
//
// 若哪天这里变成"有 secret 了"，说明回落路径改了语义（核心凭空造出了 secret，
// 或者 SyncToDirFor 开始自己发明凭证）—— 那是必须显式知道的变化，不该静默发生。
func TestReloadWithoutSecretLoaderLeavesNoSecret(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "someup")
	reg := gateway.NewRegistry()
	prov := &plainLoaderProvider{
		stubProvider: stubProvider{id: "someup", caps: gateway.CapChat},
		dir:          dir,
		creds:        []gateway.Credential{{UID: "u-1", Nickname: "n"}},
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("someup")
	h := New(Config{Pool: p, Registry: reg, AuthDir: dir, AuthsBase: base, DefaultProvider: "someup"})

	rec, resp := postReload(t, h, `{"provider":"someup"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload 失败: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if resp.Scanned != 1 {
		t.Fatalf("scanned 应为 1，实际 %d", resp.Scanned)
	}
	if got, ok := p.SecretOf("u-1"); ok && got != nil {
		t.Fatalf("没实现 CredentialSecretLoader 的上游不该凭空得到 secret，实际 %#v", got)
	}
}

// TestReloadCarriesCredsWhenSecretIsAuth 钉住"secret 就是带凭证的 *auth.Auth"这条路径。
//
// # 守的是用户实测报的 bug
//
// 现象：管理台「批量导入」一个 workbuddy 账号后，该号在账号池里可见、
// 刷新额度与签到都正常，但**猫猫旅行 / 成长计划一律报「无可用凭证」**。
//
// 根因：accountsReload 把扫描结果**投影成 uid/nickname** 再交给池
//（`auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})`），
// 而 workbuddy 的凭证就是 `*auth.Auth` 本身、走 secret 通道。
// 于是池里 `e.a` 成了空投影，而 workbuddy 的取号点
//（creds() / authOf() / 各处 AuthByUID）**只读 e.a、不读 secret**
// → RefreshToken 为空 → 判定"无可用凭证"。
//
// pool 侧 `!authHasCreds(a) && authHasCreds(e.a)` 那条保护覆盖不到本场景：
// 首次导入后池里那份本来就是空投影，新旧两边都无凭证 → 保护不生效，
// `e.a = a` 照常把 token 清掉（且重启也修不好，因为 reload 同样传投影）。
//
// 修法：secret 是带凭证的 *auth.Auth 时，**直接用它当池条目**，不投影。
//
// 变异：去掉 accountsReload 里那个 `if sa, ok := secrets[c.UID].(*auth.Auth); ok ...`
// 分支（退回无条件投影）→ 本用例必须变红。
func TestReloadCarriesCredsWhenSecretIsAuth(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "workbuddy")

	// workbuddy 的真实形态：Credential 是投影，Secret 是同源的 *auth.Auth（带 token）。
	realAuth := &auth.Auth{
		UID:          "wb-1",
		Nickname:     "妖精七七",
		AccessToken:  "at-real",
		RefreshToken: "rt-real",
		ExpiresAt:    1794994297,
	}
	reg := gateway.NewRegistry()
	prov := &secretProvider{
		stubProvider: stubProvider{id: "workbuddy", caps: gateway.CapChat},
		dir:          dir,
		items: []gateway.CredentialSecret{
			{Credential: gateway.Credential{UID: "wb-1", Nickname: "妖精七七"}, Secret: realAuth},
		},
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("workbuddy")
	h := New(Config{Pool: p, Registry: reg, AuthDir: dir, AuthsBase: base, DefaultProvider: "workbuddy"})

	rec, resp := postReload(t, h, `{"provider":"workbuddy"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload 失败: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if resp.Scanned != 1 {
		t.Fatalf("scanned 应为 1，实际 %d", resp.Scanned)
	}

	// ★ 核心断言：取号路径读的就是 AuthByUID 返回的 e.a，它必须带凭证。
	a := p.AuthByUID("wb-1")
	if a == nil {
		t.Fatal("账号应已进池")
	}
	if a.RefreshToken == "" {
		t.Fatalf("❌ e.a.RefreshToken 为空 —— workbuddy 的 creds()/authOf() 会判定"+
			"「无可用凭证」，猫猫旅行与成长计划会整片报错。实际: %+v", a)
	}
	if a.AccessToken != "at-real" {
		t.Errorf("AccessToken 应为真实凭证，实际 %q", a.AccessToken)
	}

	// secret 通道照常。
	if got, ok := p.SecretOf("wb-1"); !ok || got != realAuth {
		t.Errorf("secret 通道应保存同一份 *auth.Auth: %v, %v", got, ok)
	}
}

// TestReloadStillProjectsWhenSecretIsOpaque 保证上面那条修复**不影响别的上游**。
//
// codearts 这类上游的凭证不在 *auth.Auth 上（是不透明的 STS 结构），
// secret 不是 *auth.Auth → 仍走原投影路径 → e.a 里不该凭空出现凭证。
// 这是"改一个上游不能顺手改掉另一个上游语义"的守卫。
func TestReloadStillProjectsWhenSecretIsOpaque(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "codearts")
	secret := map[string]string{"kind": "opaque-upstream-cred", "token": "T-1"}

	reg := gateway.NewRegistry()
	prov := &secretProvider{
		stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
		dir:          dir,
		items: []gateway.CredentialSecret{
			{Credential: gateway.Credential{UID: "ca-1", Nickname: "codearts号"}, Secret: secret},
		},
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("codearts")
	h := New(Config{Pool: p, Registry: reg, AuthDir: dir, AuthsBase: base, DefaultProvider: "codearts"})

	rec, _ := postReload(t, h, `{"provider":"codearts"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reload 失败: HTTP %d %s", rec.Code, rec.Body.String())
	}

	a := p.AuthByUID("ca-1")
	if a == nil {
		t.Fatal("账号应已进池")
	}
	if a.AccessToken != "" || a.RefreshToken != "" {
		t.Errorf("非 *auth.Auth 的 secret 不该被塞进 e.a: %+v", a)
	}
	if got, ok := p.SecretOf("ca-1"); !ok || !reflect.DeepEqual(got, secret) {
		t.Errorf("不透明 secret 应照常保存: %v, %v", got, ok)
	}
}
