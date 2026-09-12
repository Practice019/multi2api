package codearts

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// makeRefreshToken 造一个结构真实的 refresh_token JWT。
//
// 真实形态（从线上凭证解出）：
//
//	header  : {"typ":"JWT","alg":"RS256","kid":"cn-north-4:4"}
//	payload : {exp, iat, iss, type, user_profile, client_id, cnf, jti, federation}
//	         其中 user_profile 是**再一层 base64** 的 JSON：
//	         {"account_id":"...","account_name":"...","features_switches":{...}}
func makeRefreshToken(t *testing.T, accountID, accountName string) string {
	t.Helper()
	profile := map[string]any{
		"account_id":   accountID,
		"account_name": accountName,
	}
	pb, _ := json.Marshal(profile)
	payload := map[string]any{
		"exp":          1791723869,
		"iat":          1789131869,
		"iss":          "cn-north-4",
		"type":         "refreshToken",
		"user_profile": base64.RawURLEncoding.EncodeToString(pb),
		"client_id":    "vscode-codebot",
	}
	plb, _ := json.Marshal(payload)
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"RS256","kid":"cn-north-4:4"}`))
	return hdr + "." + base64.RawURLEncoding.EncodeToString(plb) + ".fakesig"
}

// TestAccountFromRefreshToken 锁定从 refresh_token 解出真实账号信息。
//
// 为什么需要它：OAuth 授权响应只返回 credentials，**不含账号 id**，
// 所以 cmd/login 写出的凭证里 account.uid 一直是空串，
// 我之前用 AK 兜底 —— 能跑，但管理台显示的是 AK 而非账号名，
// 多账号时无法与华为云控制台对账。
//
// refresh_token 的 payload 里**明文**带 user_profile，是权威来源。
func TestAccountFromRefreshToken(t *testing.T) {
	rt := makeRefreshToken(t, "01a08fe00b8b7c21bd95e11109692b80", "hw097813007")

	id, name, err := AccountFromRefreshToken(rt)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if id != "01a08fe00b8b7c21bd95e11109692b80" {
		t.Errorf("account_id = %q", id)
	}
	if name != "hw097813007" {
		t.Errorf("account_name = %q", name)
	}
}

// TestAccountFromRefreshTokenRejectsBadInput 确认畸形输入返回 error 而非 panic。
func TestAccountFromRefreshTokenRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"空串", ""},
		{"不是 JWT", "not-a-jwt"},
		{"只有两段", "aaa.bbb"},
		{"payload 不是 base64", "aaa.!!!.ccc"},
		{"payload 不是 JSON", "aaa." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".ccc"},
		{"无 user_profile", "aaa." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".ccc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := AccountFromRefreshToken(c.in); err == nil {
				t.Error("应返回 error")
			}
		})
	}
}

// TestParseCredentialPrefersJWTAccount 锁定三级回落顺序：
// JWT 解析成功 → 用 JWT 的 id；否则 → account.uid；再否则 → AK。
func TestParseCredentialPrefersJWTAccount(t *testing.T) {
	rt := makeRefreshToken(t, "REAL_ACCOUNT_ID", "real-user")

	t.Run("JWT 优先于 account.uid", func(t *testing.T) {
		doc := credFile{
			Auth:    credBody{AccessKey: "AK1", SecretKey: "SK", SecurityToken: "ST", RefreshToken: rt},
			Account: accountBlock{UID: "STALE_UID", Nickname: "stale"},
		}
		raw, _ := json.Marshal(doc)
		a, err := ParseCredential(raw)
		if err != nil {
			t.Fatal(err)
		}
		if a.UID != "REAL_ACCOUNT_ID" {
			t.Errorf("UID = %q, 期望 REAL_ACCOUNT_ID（JWT 应优先）", a.UID)
		}
		if a.Nickname != "real-user" {
			t.Errorf("Nickname = %q, 期望 real-user", a.Nickname)
		}
	})

	t.Run("无 JWT 时回落 account.uid", func(t *testing.T) {
		doc := credFile{
			Auth:    credBody{AccessKey: "AK2", SecretKey: "SK", SecurityToken: "ST"},
			Account: accountBlock{UID: "EXPLICIT_UID", Nickname: "n"},
		}
		raw, _ := json.Marshal(doc)
		a, err := ParseCredential(raw)
		if err != nil {
			t.Fatal(err)
		}
		if a.UID != "EXPLICIT_UID" {
			t.Errorf("UID = %q, 期望 EXPLICIT_UID", a.UID)
		}
	})

	t.Run("JWT 与 uid 都无时回落 AK", func(t *testing.T) {
		doc := credFile{
			Auth: credBody{AccessKey: "AK3", SecretKey: "SK", SecurityToken: "ST"},
		}
		raw, _ := json.Marshal(doc)
		a, err := ParseCredential(raw)
		if err != nil {
			t.Fatal(err)
		}
		if a.UID != "AK3" {
			t.Errorf("UID = %q, 期望 AK3", a.UID)
		}
	})

	t.Run("JWT 畸形时回落 AK（不报错）", func(t *testing.T) {
		doc := credFile{
			Auth: credBody{AccessKey: "AK4", SecretKey: "SK", SecurityToken: "ST", RefreshToken: "garbage"},
		}
		raw, _ := json.Marshal(doc)
		a, err := ParseCredential(raw)
		if err != nil {
			t.Fatalf("畸形 refresh_token 不应导致解析失败: %v", err)
		}
		if a.UID != "AK4" {
			t.Errorf("UID = %q, 期望 AK4", a.UID)
		}
	})
}

// TestAccountFromRefreshTokenDoesNotVerifySignature 说明解析是**纯解码**。
//
// 我们只用它取账号展示信息，不依赖它做鉴权，
// 因此不验签（验签需要公钥，而服务端并未下发）。
// 这个测试固定该语义：篡改签名不影响解析结果。
func TestAccountFromRefreshTokenDoesNotVerifySignature(t *testing.T) {
	rt := makeRefreshToken(t, "ID_X", "user-x")
	parts := splitDots(rt)
	tampered := parts[0] + "." + parts[1] + ".TAMPERED_SIGNATURE"

	id, name, err := AccountFromRefreshToken(tampered)
	if err != nil {
		t.Fatalf("篡改签名不应影响解析: %v", err)
	}
	if id != "ID_X" || name != "user-x" {
		t.Errorf("解析结果 = %q/%q", id, name)
	}
}

func splitDots(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	out = append(out, cur)
	return out
}
