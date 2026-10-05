// persist_test.go 登录后落盘链路的守卫。
//
// # 这个文件守的是**用户实际撞到的三次连续失败**
//
// 用户点「添加账号」后依次撞到：
//
//	① 501「该上游的凭证结构尚未接入落盘」  ← Secret 没实现 MarshalAuthFile
//	② 501「没有实现 gateway.CredentialLoader」 ← 重扫凭证没实现
//	③ （潜在）账号并池但投影错了
//
// 三段都是**类型断言**判据，且都在**授权成功之后**才触发 ——
// 所以单测如果只测"Start 给了 URL、Poll 给了凭证"会全绿，
// 而用户看到的是"登录不了"。
//
// 判据：**把核心的判据原样搬进测试**，而不是自己瞎猜什么算完整。
package zcode

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/gateway"
)

// authFileWriter 是核心 pollViaFlow 的落盘判据（内部窄接口，此处复刻）。
//
// ⚠ 刻意**复刻**而不是引用：admin 里那个是包私有类型。
// 而复刻的风险是"两边分叉" —— 所以字段与签名逐字抄自
// internal/admin/admin.go 的 authFileWriter。
type authFileWriter interface {
	MarshalAuthFile() (name string, raw []byte, err error)
}

// 实测发现的第 ① 个必需点：Secret 必须能序列化成 auth 文件。
//
// 这条断言为什么必须存在：我最初的测试断言的是 `Secret.(*Auth)`，
// 而生产的判据是 `Secret.(authFileWriter)` ——
// **两者不一致**，于是测试全绿、登录却卡在 501。
// 断言写错判据 = 守了一个生产中不存在的契约。
func TestPollSecretIsAuthFileWriter(t *testing.T) {
	f := newFakeOAuth(t, "ready")
	p := newLoginProvider(t, f)

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	cred, err := p.Poll(state)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}

	mw, ok := cred.Secret.(authFileWriter)
	if !ok {
		t.Fatalf("Secret 必须实现 MarshalAuthFile（核心落盘判据），实际 %T ——"+
			"否则登录会以 501「该上游的凭证结构尚未接入落盘」失败，"+
			"而用户看到的是「登录不了」", cred.Secret)
	}
	name, raw, err := mw.MarshalAuthFile()
	if err != nil {
		t.Fatalf("MarshalAuthFile 失败: %v", err)
	}
	if name == "" {
		t.Error("文件名不能为空")
	}
	if len(raw) == 0 {
		t.Error("内容不能为空")
	}
	// 落盘形态必须是 {"auth":{...}}（本仓约定，admin 的导入按键名解析）。
	if back, err := parseAuth(raw); err != nil || !back.Usable() {
		t.Errorf("落盘内容读不回来（err=%v）:\n%s", err, raw)
	}
}

// 实测发现的第 ② 个必需点：必须实现 CredentialLoader。
//
// 不实现的症状极具迷惑性：**凭证文件正确落盘了**，
// 但账号池里没有 —— 用户看到"登录成功但账号没出现"。
func TestCredentialLoaderIsDiscoverable(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	if _, ok := gateway.ExtOf[gateway.CredentialLoader](p); !ok {
		t.Error("必须实现 gateway.CredentialLoader —— 否则登录后核心重扫凭证会 501" +
			"「凭证已写入…但上游没有实现 gateway.CredentialLoader」")
	}
	// 带 secret 的版本也是必需的（否则并池拿不到 *Auth）。
	if _, ok := gateway.ExtOf[gateway.CredentialSecretLoader](p); !ok {
		t.Error("必须实现 gateway.CredentialSecretLoader —— 否则账号进了池但没有凭证")
	}
}

// LoadCredentials 要能把**真实落盘的文件**读回来。
//
// 这条把落盘与重扫两端接起来：任何一端单独测都可能漏掉
// "文件名与扫描规则不匹配"这类问题。
func TestCredentialLoaderReadsPersistedFile(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{AuthDir: dir})
	p.probeOrigin = false

	// 先按登录产出的形态落盘一份（模拟核心写盘那一步）。
	a := &Auth{
		Kind: CredKindJWT, JWT: "jwt-token-value", RefreshToken: "rt-1",
		UID: "persist-user", Nickname: "落盘测试号", CaptchaRegion: "cn",
	}
	path, err := SaveFile(dir, a)
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	// 核心重扫：只投影 uid/nickname/file_path。
	list, err := p.LoadCredentials(dir)
	if err != nil {
		t.Fatalf("LoadCredentials 失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应读到 1 份凭证，实际 %d", len(list))
	}
	got := list[0]
	if got.UID != "persist-user" {
		t.Errorf("uid = %q，期望 persist-user", got.UID)
	}
	if got.Nickname == "" {
		t.Error("nickname 不能为空（账号池显示它）")
	}
	if got.Provider != providerID {
		t.Errorf("provider = %q，期望 %q", got.Provider, providerID)
	}
	// ⚠ FilePath 必须回填 —— 它是"删除账号时连文件一起删"的依据。
	// 缺了会出现本仓实测过的"删除后重启复活"。
	if got.FilePath == "" {
		t.Error("FilePath 未回填 —— 删账号时文件不会被移除，重启后账号会复活")
	}
	if filepath.Base(got.FilePath) != filepath.Base(path) {
		t.Errorf("FilePath 指向的文件不对: %q，期望 %q", got.FilePath, path)
	}

	// 带 secret 的版本要给出**可断言的** *Auth。
	withSecret, err := p.LoadCredentialsWithSecrets(dir)
	if err != nil {
		t.Fatalf("LoadCredentialsWithSecrets 失败: %v", err)
	}
	if len(withSecret) != 1 {
		t.Fatalf("应读到 1 份，实际 %d", len(withSecret))
	}
	inner, ok := withSecret[0].Secret.(*Auth)
	if !ok || inner == nil {
		t.Fatalf("Secret 应是 *Auth，实际 %T", withSecret[0].Secret)
	}
	if inner.JWT != "jwt-token-value" {
		t.Errorf("Secret 里的 JWT 丢了: %q", inner.JWT)
	}
	if inner.KindOf() != CredKindJWT {
		t.Errorf("Secret 的通道丢了: %q", inner.KindOf())
	}
}

// LoadCredentials 的边界：目录不存在 / 空目录都不是错误。
//
// 判据来自接口注释：「读不到任何凭证**不是错误**（目录为空/还没添加过账号）」。
// 把它当错误会让"刚启用还没加账号"的部署在每次重载时刷红日志。
func TestCredentialLoaderToleratesEmptyAndMissingDir(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()})
	p.probeOrigin = false

	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "nope")} {
		list, err := p.LoadCredentials(dir)
		if err != nil {
			t.Errorf("目录 %q 不应报错: %v", dir, err)
		}
		if len(list) != 0 {
			t.Errorf("目录 %q 应返回空切片，实际 %d 项", dir, len(list))
		}
	}
}

// 空 dir 要回落到实例自己的 authDir（单上游旧形态会传空串）。
func TestCredentialLoaderFallsBackToOwnAuthDir(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{AuthDir: dir})
	p.probeOrigin = false

	a := &Auth{Kind: CredKindAPIKey, APIKey: "fallback-key", UID: "fb-user"}
	if _, err := SaveFile(dir, a); err != nil {
		t.Fatal(err)
	}
	// 传空 dir —— 应该仍能读到（回落到 p.authDir）。
	list, err := p.LoadCredentials("")
	if err != nil {
		t.Fatalf("空 dir 应回落实例配置: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("空 dir 时也应读到实例目录里的凭证，实际 %d 份", len(list))
	}
}

// 落盘 → 重扫 → 再落盘 的**幂等性**（同一 uid 不产生第二份文件）。
//
// 为什么值得测：登录两次（比如第一次超时后重来）若产出两个文件，
// 账号池会出现两份同 uid 的凭证 —— 而池子按 uid 去重，
// 于是"其中一个永远不被用到，而它可能持有已失效的令牌"。
func TestPersistIsIdempotentPerUID(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{AuthDir: dir})
	p.probeOrigin = false

	for i := 0; i < 3; i++ {
		a := &Auth{Kind: CredKindJWT, JWT: "tok", UID: "same-user"}
		if _, err := SaveFile(dir, a); err != nil {
			t.Fatalf("第 %d 次落盘失败: %v", i+1, err)
		}
	}
	list, err := p.LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		names, _ := os.ReadDir(dir)
		var ns []string
		for _, n := range names {
			ns = append(ns, n.Name())
		}
		t.Fatalf("同一 uid 反复落盘应只有 1 份凭证，实际 %d 份: %v", len(list), ns)
	}
}
