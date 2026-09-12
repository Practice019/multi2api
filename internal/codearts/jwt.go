// jwt.go 从 refresh_token 里解出账号信息。
//
// 为什么需要：OAuth 授权响应只返回 `credentials`，**不含账号 id**，
// 所以 cmd/login 早期产出的凭证里 `account.uid` 是空串。
// 我们一度用 AK 兜底 —— 能跑，但管理台显示的是 AK 而不是账号名，
// 多账号时无法与华为云控制台对账。
//
// refresh_token 的 JWT payload 里**明文**带 `user_profile`（再一层 base64），
// 其中就有 account_id / account_name，是权威来源。
//
// 安全说明：本文件只做**解码**，不验签、不解密。
// 我们只用它取展示用账号信息，不依赖它做任何鉴权判断，
// 因此无需公钥（服务端也不下发）。JWT payload 本就是 base64 明文，
// 任何持有凭证的人都能读 —— 这里没有引入新的信息暴露面。
package codearts

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// AccountInfo 是从 refresh_token 里能解出的账号标识。
type AccountInfo struct {
	ID   string // account_id，形如 01a08fe00b8b7c21bd95e11109692b80
	Name string // account_name，形如 hw097813007
}

// AccountFromRefreshToken 解出 refresh_token 里的账号信息。
//
// 结构（实测，三层 base64）：
//
//	JWT.payload.user_profile = base64({"account_id":"...","account_name":"..."})
//
// 任一层解不开都返回 error，调用方据此回落到 account.uid / AK。
func AccountFromRefreshToken(rt string) (id, name string, err error) {
	info, err := ParseRefreshToken(rt)
	if err != nil {
		return "", "", err
	}
	return info.ID, info.Name, nil
}

// ParseRefreshToken 与 AccountFromRefreshToken 同义，返回结构体便于扩展。
func ParseRefreshToken(rt string) (*AccountInfo, error) {
	if strings.TrimSpace(rt) == "" {
		return nil, fmt.Errorf("refresh_token 为空")
	}

	// JWT 是 a.b.c 三段；签名段我们不关心，但必须存在以确认形态正确。
	parts := strings.Split(rt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("refresh_token 不是 JWT（段数 %d）", len(parts))
	}

	payloadRaw, err := b64urlDecode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("解 payload: %w", err)
	}

	var payload struct {
		UserProfile string `json:"user_profile"`
		ClientID    string `json:"client_id"`
	}
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return nil, fmt.Errorf("payload 非 JSON: %w", err)
	}
	if payload.UserProfile == "" {
		return nil, fmt.Errorf("payload 缺 user_profile")
	}

	profileRaw, err := b64urlDecode(payload.UserProfile)
	if err != nil {
		return nil, fmt.Errorf("解 user_profile: %w", err)
	}
	var profile struct {
		AccountID   string `json:"account_id"`
		AccountName string `json:"account_name"`
	}
	if err := json.Unmarshal(profileRaw, &profile); err != nil {
		return nil, fmt.Errorf("user_profile 非 JSON: %w", err)
	}
	if profile.AccountID == "" && profile.AccountName == "" {
		return nil, fmt.Errorf("user_profile 缺 account_id/account_name")
	}

	return &AccountInfo{ID: profile.AccountID, Name: profile.AccountName}, nil
}

// b64urlDecode 解 base64url，容忍缺 padding。
//
// 服务端产出的 JWT 是 RawURLEncoding（无 padding），
// 但手工构造或其它实现的 token 可能带 padding，两种都要接受。
func b64urlDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
