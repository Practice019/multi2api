package codearts

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRefreshTokenLive 端到端验证 refresh_token grant。
//
// 这是整个"自动续期"能力的核心证明：用真实凭证调 /v1/oauth2/tokens
// 的 refresh_token 分支，确认能换回新的 AK/SK/securityToken。
//
// 需要环境变量 CORARTS_TEST_CRED 指向 auths/codearts-*.json（cmd/login 的产物）。
// 未提供时跳过 —— 不让缺凭证的机器上跑红。
//
// 为什么单独写这个测试：之前所有验证都只覆盖了签名与 chat，
// 续期路径从未被真实验证过。而 CodeArts 的 STS 只有约 2 小时寿命，
// 续期一旦不通，网关跑上两小时就会全线 503。
func TestRefreshTokenLive(t *testing.T) {
	credPath := os.Getenv("CORARTS_TEST_CRED")
	if credPath == "" {
		t.Skip("未设置 CORARTS_TEST_CRED，跳过联网续期测试")
	}
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Skipf("读不到凭证 %s: %v", credPath, err)
	}
	a, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("解析凭证: %v", err)
	}
	if a.RefreshToken == "" {
		t.Fatal("凭证无 refresh_token —— 这是从 vscdb 导出的过渡凭证，无法验证续期")
	}
	if len(a.DPoPPrivateKeyJWK) == 0 {
		t.Fatal("凭证无 DPoP 私钥 —— 续期无法签名")
	}

	// 拷到临时目录，让 SaveAtomic 写到副本而不是真凭证。
	//
	// 这里刻意**不**备份/还原 Auth 原值：Auth 内含 sync.Mutex，
	// 按值复制会触发 go vet 的 copylocks 告警（且可能复制出已加锁的互斥量）。
	// 测试只读原始文件、只写临时副本，因此不需要还原。
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "codearts-test.json")
	if err := os.WriteFile(tmpFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a.FilePath = tmpFile

	c := New()
	beforeAK := a.AccessKey
	beforeExp := a.ExpiresAt

	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("续期失败（自动续期不可用）: %v", err)
	}

	// 断言：拿到了新凭证
	if a.AccessKey == "" || a.SecretKey == "" || a.SecurityToken == "" {
		t.Fatalf("续期后凭证不完整: ak=%q sk_len=%d st_len=%d",
			a.AccessKey, len(a.SecretKey), len(a.SecurityToken))
	}
	if a.ExpiresAt <= 0 {
		t.Error("续期后 expiresAt 未更新")
	}
	t.Logf("✅ 续期成功")
	t.Logf("   旧 AK: %s  新 AK: %s", beforeAK, a.AccessKey)
	t.Logf("   旧过期: %d  新过期: %d", beforeExp, a.ExpiresAt)
	if a.AccessKey != beforeAK {
		t.Log("   （AK 已轮换，说明确实换发了一套新 STS 凭证）")
	}

	// 断言：新凭证真的能用
	if err := c.Verify(a); err != nil {
		t.Fatalf("续期后的凭证无法通过 caller-identity 校验: %v", err)
	}
	t.Log("✅ 续期后的凭证通过 caller-identity 校验")

	// 断言：refresh_token 仍在（否则下次续期就没得用了）
	if a.RefreshToken == "" {
		t.Error("续期后 refresh_token 丢失，下次将无法续期")
	}
	// 断言：DPoP 私钥未被破坏（续期的前提）
	if len(a.DPoPPrivateKeyJWK) == 0 {
		t.Error("续期后 DPoP 私钥丢失")
	}

}

// TestRefreshRequiresPrerequisites 确认缺 refresh_token / DPoP 私钥时
// 给出的是**明确错误**而不是静默失败或空指针。
func TestRefreshRequiresPrerequisites(t *testing.T) {
	c := New()

	t.Run("无 refresh_token", func(t *testing.T) {
		a := &Auth{AccessKey: "AK", SecretKey: "SK", DPoPPrivateKeyJWK: []byte(`{"kty":"EC"}`)}
		err := c.RefreshToken(a)
		if err == nil {
			t.Fatal("应报错")
		}
		if !contains(err.Error(), "refresh_token") {
			t.Errorf("错误信息未点明缺 refresh_token: %v", err)
		}
	})

	t.Run("无 DPoP 私钥", func(t *testing.T) {
		a := &Auth{AccessKey: "AK", SecretKey: "SK", RefreshToken: "RT"}
		err := c.RefreshToken(a)
		if err == nil {
			t.Fatal("应报错")
		}
		if !contains(err.Error(), "DPoP") {
			t.Errorf("错误信息未点明缺 DPoP 私钥: %v", err)
		}
	})
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
