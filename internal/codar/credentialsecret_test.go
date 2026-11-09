// credentialsecret_test.go 编译期钉住「必须实现 secret 版扫描器」。
//
// # 这是一次真实报障的回归钉（2026-11-09）
//
// 用户批量导入两个 codar 号后：账号列表能看到、但 `has_token=false`、
// 额度空、请求必失败；同一时刻 `/admin/codar/quota` 却正常（凭证文件是好的）。
//
// 根因：`LoadCredentialsWithSecrets` 的返回类型写成了 `[]gateway.Credential`
// （应为 `[]gateway.CredentialSecret`）。Go 的接口是**结构化**的，方法签名
// 必须**全等** —— 返回值类型不同 ⇒ `ExtOf[CredentialSecretLoader]` 断言失败 ⇒
// 装配层退回裸投影 ⇒ 池里没有 secret ⇒ 该号被选中时取不到可用凭证。
//
// 为什么之前的测试没红：`RunProviderContract` 只验 4 个核心方法，不验
// "可选扩展点是否真的被认出来"。本文件的静态断言把这条缺口补上：
// 谁把返回类型改回 `[]gateway.Credential`，**编译就失败**。
package codar

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/gateway"
)

// 四个"装配层要靠类型断言才认得出"的扩展点，全部静态钉住。
//
// ⚠ 少了任何一条，症状都是"运行时静默不生效"而不是编译失败 —— 这类缺陷
// 本仓踩过多次（例：扩展点写成访问器方法 `func (p *Provider) XxxExt() (Xxx, bool)`
// 不会被 ExtOf 发现，实测有 4 个上游因此长期没有「添加账号」按钮）。
var (
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialTokenExt     = (*Provider)(nil)
)

// TestLoadCredentialsWithSecretsCarriesSecret 除了编译期断言，再验一遍**内容**：
// 每条都必须带上非 nil 的 Secret，否则并池后池子里拿不到凭证。
func TestLoadCredentialsWithSecretsCarriesSecret(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"codar-u1.json": `{"auth":{"token":"t1","username":"user_111_1","expiresAt":1999999999},"account":{"uid":"user_111_1"}}`,
		"codar-u2.json": `{"auth":{"token":"t2","username":"user_222_2","expiresAt":1999999999},"account":{"uid":"user_222_2"}}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := NewWithConfig(Config{AuthDir: dir})
	items, err := p.LoadCredentialsWithSecrets(dir)
	if err != nil {
		t.Fatalf("LoadCredentialsWithSecrets: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应扫到 2 条，实际 %d", len(items))
	}
	for _, it := range items {
		if it.Credential.UID == "" {
			t.Error("投影缺 UID")
		}
		if it.Secret == nil {
			t.Errorf("uid=%s 的 Secret 为 nil —— 并池后池子拿不到凭证（正是本次报障的形态）", it.Credential.UID)
			continue
		}
		a, ok := it.Secret.(*Auth)
		if !ok {
			t.Errorf("uid=%s 的 Secret 类型是 %T，应为 *codar.Auth", it.Credential.UID, it.Secret)
			continue
		}
		if a.Token == "" {
			t.Errorf("uid=%s 的 Secret.Token 为空", it.Credential.UID)
		}
	}
}

// TestLoadCredentialsAndSecretsAreSameSource 两个扫描器必须**同源**（本仓铁律）。
//
// 不同源的后果：池里那份与磁盘那份是两个对象，续期后一个变一个不变 ——
// codearts 曾因这个形态出现「一次性 refresh_token 被消费两次 → 503」。
func TestLoadCredentialsAndSecretsAreSameSource(t *testing.T) {
	dir := t.TempDir()
	body := `{"auth":{"token":"tX","username":"user_999_9","expiresAt":1999999999},"account":{"uid":"user_999_9"}}`
	if err := os.WriteFile(filepath.Join(dir, "codar-x.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewWithConfig(Config{AuthDir: dir})
	plain, err := p.LoadCredentials(dir)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	withSecret, err := p.LoadCredentialsWithSecrets(dir)
	if err != nil {
		t.Fatalf("LoadCredentialsWithSecrets: %v", err)
	}
	if len(plain) != len(withSecret) {
		t.Fatalf("两个扫描器条数不一致：%d vs %d", len(plain), len(withSecret))
	}
	if plain[0].UID != withSecret[0].Credential.UID {
		t.Errorf("UID 不一致：%q vs %q", plain[0].UID, withSecret[0].Credential.UID)
	}
}
