package codearts

import "testing"

// TestParseCredentialFillsUIDFromAccessKey 锁定一个真实踩过的严重缺陷：
//
// OAuth 授权响应只返回 `credentials`，**不含账号 id**。因此 cmd/login 早期版本
// 写出的凭证里 `account.uid` 是空串。
//
// 而 uid 是账号池的主键：空串会让所有账号塌成**同一个键**，
// 在途租约（Acquire/Release）互相干扰 —— 表现为
// 「连续 N 次成功后 in_flight 卡住不降，之后恒 503 no_healthy_account」。
// 症状指向"账号不健康"，完全看不出是 uid 为空。
//
// 修复是在解析层用 AK 兜底（AK 一定存在且同账号内唯一）。
func TestParseCredentialFillsUIDFromAccessKey(t *testing.T) {
	// 模拟 cmd/login 早期产出的凭证：有 auth、无 account
	raw := []byte(`{
	  "auth": {
	    "accessKeyId": "AK_TEST_123",
	    "secretAccessKey": "SK",
	    "securityToken": "ST",
	    "expiresAt": 1800000000,
	    "refresh_token": "RT"
	  },
	  "dpop": {"privateKeyJwk": {"kty":"EC","crv":"P-256","x":"a","y":"b","d":"c"}},
	  "clientId": "vscode-codebot"
	}`)

	a, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if a.UID == "" {
		t.Fatal("UID 为空 —— 账号池会因空主键而计数错乱（in_flight 卡住、恒 503）")
	}
	if a.UID != a.AccessKey {
		t.Errorf("UID = %q, 期望回落到 AK %q", a.UID, a.AccessKey)
	}
}

// TestParseCredentialKeepsExplicitUID 确认有显式 uid 时不被覆盖。
func TestParseCredentialKeepsExplicitUID(t *testing.T) {
	raw := []byte(`{
	  "auth": {"accessKeyId":"AK1","secretAccessKey":"SK","securityToken":"ST","expiresAt":1},
	  "account": {"uid": "REAL_UID", "nickname": "n"}
	}`)
	a, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if a.UID != "REAL_UID" {
		t.Errorf("显式 uid 被覆盖为 %q", a.UID)
	}
	if a.Nickname != "n" {
		t.Errorf("nickname = %q", a.Nickname)
	}
}

// TestParseCredentialFlatAlsoGetsUID 确认扁平形也走同样的兜底。
func TestParseCredentialFlatAlsoGetsUID(t *testing.T) {
	raw := []byte(`{"accessKeyId":"AK_FLAT","secretAccessKey":"SK","securityToken":"ST"}`)
	a, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if a.UID != "AK_FLAT" {
		t.Errorf("扁平形 UID = %q, 期望 AK_FLAT", a.UID)
	}
}

// TestAccountsAllHaveDistinctUID 确认多账号下 uid 互不相同。
//
// 这是上面那个缺陷的根源场景：若所有账号 uid 都是空串，
// 账号池会把它们当成同一个号。
func TestAccountsAllHaveDistinctUID(t *testing.T) {
	mk := func(ak string) []byte {
		return []byte(`{"auth":{"accessKeyId":"` + ak + `","secretAccessKey":"SK","securityToken":"ST","expiresAt":1}}`)
	}
	a1, err := ParseCredential(mk("AK_A"))
	if err != nil {
		t.Fatal(err)
	}
	a2, err := ParseCredential(mk("AK_B"))
	if err != nil {
		t.Fatal(err)
	}
	if a1.UID == a2.UID {
		t.Fatalf("两个不同账号的 UID 相同（%q）—— 会导致账号池计数错乱", a1.UID)
	}
	if a1.UID == "" || a2.UID == "" {
		t.Fatal("UID 不应为空")
	}
}
