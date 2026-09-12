// Package codearts 实现 CodeArts Agent 的 OAuth 续期（DPoP + refresh_token grant）。
//
// 与 CodeBuddy 的差异：CodeArts 的 token 端点要求 **DPoP** 头
// （Demonstrating Proof-of-Possession，RFC 9449）：一个 ES256 签名的 JWT，
// 头部内嵌公钥 JWK，载荷含 htm/htu/iat/jti。
//
// 关键：DPoP 私钥**必须与授权时用的那一对相同**，否则 refresh 会被拒。
// 因此私钥要与 refresh_token 一起持久化（见 credential.go 的 DPoPPrivateKeyJWK）。
package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// DPoPKeyPair 是一对用于 DPoP 的 ES256 密钥。
type DPoPKeyPair struct {
	Private *ecdsa.PrivateKey
}

// NewDPoPKeyPair 生成新的 P-256 密钥对。
func NewDPoPKeyPair() (*DPoPKeyPair, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate P-256 key: %w", err)
	}
	return &DPoPKeyPair{Private: priv}, nil
}

// PublicJWK 返回公钥的 JWK（放进 DPoP 头的 jwk 字段）。
func (k *DPoPKeyPair) PublicJWK() map[string]string {
	x := k.Private.PublicKey.X.Bytes()
	y := k.Private.PublicKey.Y.Bytes()
	// P-256 的坐标固定 32 字节，需左侧补零，否则验证方解析失败。
	x = leftPad(x, 32)
	y = leftPad(y, 32)
	return map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   b64url(x),
		"y":   b64url(y),
	}
}

// PrivateJWK 返回私钥的 JWK（持久化用）。
func (k *DPoPKeyPair) PrivateJWK() map[string]string {
	j := k.PublicJWK()
	j["d"] = b64url(leftPad(k.Private.D.Bytes(), 32))
	return j
}

// FromPrivateJWK 从持久化的 JWK 恢复密钥对。
func FromPrivateJWK(raw json.RawMessage) (*DPoPKeyPair, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty DPoP JWK")
	}
	var j struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
		D   string `json:"d"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("parse DPoP JWK: %w", err)
	}
	if j.Kty != "EC" || j.Crv != "P-256" {
		return nil, fmt.Errorf("unsupported DPoP key: kty=%s crv=%s", j.Kty, j.Crv)
	}
	dec := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	xb, err := dec(j.X)
	if err != nil {
		return nil, fmt.Errorf("decode x: %w", err)
	}
	yb, err := dec(j.Y)
	if err != nil {
		return nil, fmt.Errorf("decode y: %w", err)
	}
	db, err := dec(j.D)
	if err != nil {
		return nil, fmt.Errorf("decode d: %w", err)
	}
	priv := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(xb),
			Y:     new(big.Int).SetBytes(yb),
		},
		D: new(big.Int).SetBytes(db),
	}
	return &DPoPKeyPair{Private: priv}, nil
}

// DPoPProof 构造 DPoP 证明（ES256 签名的 JWT）。
//
// htm = HTTP 方法，htu = 目标 URL（不含 query，RFC 9449 §4.2）。
func (k *DPoPKeyPair) DPoPProof(method, url string) (string, error) {
	header := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": k.PublicJWK(),
	}
	// htu 应去掉 query 与 fragment
	htu := url
	if i := strings.IndexAny(htu, "?#"); i >= 0 {
		htu = htu[:i]
	}
	jtiRaw := make([]byte, 32)
	if _, err := rand.Read(jtiRaw); err != nil {
		return "", err
	}
	payload := map[string]any{
		"htm": strings.ToUpper(method),
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": fmt.Sprintf("%x", jtiRaw),
	}

	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(pb)

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.Private, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign DPoP: %w", err)
	}
	// JWS 要求 r||s 定长拼接（ieee-p1363），不是 ASN.1 DER。
	sig := append(leftPad(r.Bytes(), 32), leftPad(s.Bytes(), 32)...)
	return signingInput + "." + b64url(sig), nil
}

// ---------------- 内部工具 ----------------

func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
