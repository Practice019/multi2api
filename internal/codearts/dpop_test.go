package codearts

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

// 测试里频繁用到的两个小常量，避免反复写 time.Duration 字面量。
const (
	timeMinute = time.Minute
)

func nowUnix() int64 { return time.Now().Unix() }

// bigFromBytes 把 32 字节定长大端整数转成 *big.Int。
func bigFromBytes(b []byte) *big.Int { return new(big.Int).SetBytes(b) }

// decodeB64urlTest 容忍有无 padding。
func decodeB64urlTest(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatalf("base64url 解码失败 %q: %v", s, err)
	}
	return b
}

// TestDPoPProofStructure 校验 DPoP 证明的 JOSE 头与载荷符合 RFC 9449。
func TestDPoPProofStructure(t *testing.T) {
	kp, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	const target = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	proof, err := kp.DPoPProof("POST", target+"?ignored=1")
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 应为 3 段，实际 %d", len(parts))
	}

	var hdr struct {
		Alg string            `json:"alg"`
		Typ string            `json:"typ"`
		JWK map[string]string `json:"jwk"`
	}
	if err := json.Unmarshal(decodeB64urlTest(t, parts[0]), &hdr); err != nil {
		t.Fatalf("解析头失败: %v", err)
	}
	if hdr.Alg != "ES256" {
		t.Errorf("alg = %q, 期望 ES256", hdr.Alg)
	}
	if hdr.Typ != "dpop+jwt" {
		t.Errorf("typ = %q, 期望 dpop+jwt", hdr.Typ)
	}
	if hdr.JWK["kty"] != "EC" || hdr.JWK["crv"] != "P-256" {
		t.Errorf("jwk 类型错误: %v", hdr.JWK)
	}
	if hdr.JWK["d"] != "" {
		t.Error("公钥 JWK 不能包含私钥分量 d")
	}
	// P-256 坐标必须定长 32 字节，否则验证方会解析失败
	if l := len(decodeB64urlTest(t, hdr.JWK["x"])); l != 32 {
		t.Errorf("jwk.x 长度 = %d, 期望 32", l)
	}
	if l := len(decodeB64urlTest(t, hdr.JWK["y"])); l != 32 {
		t.Errorf("jwk.y 长度 = %d, 期望 32", l)
	}

	var pl struct {
		Htm string `json:"htm"`
		Htu string `json:"htu"`
		Iat int64  `json:"iat"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(decodeB64urlTest(t, parts[1]), &pl); err != nil {
		t.Fatalf("解析载荷失败: %v", err)
	}
	if pl.Htm != "POST" {
		t.Errorf("htm = %q, 期望 POST", pl.Htm)
	}
	// RFC 9449: htu 不含 query
	if pl.Htu != target {
		t.Errorf("htu = %q, 期望 %q（应去掉 query）", pl.Htu, target)
	}
	if pl.Iat == 0 || pl.Jti == "" {
		t.Error("iat/jti 缺失")
	}
}

// TestDPoPSignatureVerifiable 用公钥验证签名，确保 ieee-p1363 编码正确。
//
// 这是最容易出错的地方：Go 默认输出 ASN.1 DER，而 JWS 要求 r||s 定长拼接。
// 若编码错，服务端会直接拒绝（STS5.1806 之类）。
func TestDPoPSignatureVerifiable(t *testing.T) {
	kp, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := kp.DPoPProof("POST", "https://example.com/token")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	sig := decodeB64urlTest(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("ES256 签名应为 64 字节 (r||s)，实际 %d", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&kp.Private.PublicKey, digest[:], bigFromBytes(sig[:32]), bigFromBytes(sig[32:])) {
		t.Fatal("DPoP 签名验证失败")
	}
}

// TestDPoPPrivateJWKRoundTrip 确保持久化后能恢复同一把密钥。
// 这对续期至关重要：私钥变了，refresh 会被服务端拒绝。
func TestDPoPPrivateJWKRoundTrip(t *testing.T) {
	kp, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(kp.PrivateJWK())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := FromPrivateJWK(raw)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 用恢复的密钥签一份，再用原公钥验
	proof, err := restored.DPoPProof("POST", "https://example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	sig := decodeB64urlTest(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&kp.Private.PublicKey, digest[:], bigFromBytes(sig[:32]), bigFromBytes(sig[32:])) {
		t.Fatal("恢复的密钥与原密钥不一致")
	}
}

// TestParseCredential 覆盖两种磁盘形态。
func TestParseCredential(t *testing.T) {
	nested := `{
	  "auth": {"accessKeyId":"AK1","secretAccessKey":"SK1","securityToken":"ST1","expiresAt":1800000000,"refresh_token":"RT1"},
	  "account": {"uid":"U1","nickname":"n1"},
	  "dpop": {"privateKeyJwk":{"kty":"EC","crv":"P-256","x":"a","y":"b","d":"c"}},
	  "clientId": "vscode-codebot"
	}`
	a, err := ParseCredential([]byte(nested))
	if err != nil {
		t.Fatalf("嵌套形解析失败: %v", err)
	}
	if a.AccessKey != "AK1" || a.SecretKey != "SK1" || a.SecurityToken != "ST1" {
		t.Errorf("凭证字段错误: %+v", a)
	}
	if a.UID != "U1" || a.Nickname != "n1" {
		t.Errorf("账号字段错误: uid=%q nick=%q", a.UID, a.Nickname)
	}
	if len(a.DPoPPrivateKeyJWK) == 0 {
		t.Error("DPoP 私钥未解析出来")
	}
	if a.ClientID != "vscode-codebot" {
		t.Errorf("clientId = %q", a.ClientID)
	}

	flat := `{"accessKeyId":"AK2","secretAccessKey":"SK2","securityToken":"ST2","expiresAt":1900000000}`
	b, err := ParseCredential([]byte(flat))
	if err != nil {
		t.Fatalf("扁平形解析失败: %v", err)
	}
	if b.AccessKey != "AK2" {
		t.Errorf("扁平形 AK = %q", b.AccessKey)
	}

	// 缺 AK/SK 应报错（避免空凭证混入账号池）
	if _, err := ParseCredential([]byte(`{"securityToken":"only"}`)); err == nil {
		t.Error("缺少 AK/SK 时应当报错")
	}
}

// TestNeedsRefresh 校验 30 分钟短有效期下的续期判定。
func TestNeedsRefresh(t *testing.T) {
	// 已过期
	expired := &Auth{ExpiresAt: 1}
	if !expired.NeedsRefresh(5 * timeMinute) {
		t.Error("已过期凭证应判定需刷新")
	}
	// 无过期时间（视为需刷新，安全侧）
	if !(&Auth{}).NeedsRefresh(timeMinute) {
		t.Error("无 expiresAt 应判定需刷新")
	}
	// 还很久（45 分钟后），窗口 5 分钟 -> 不需刷新
	fresh := &Auth{ExpiresAt: nowUnix() + 45*60}
	if fresh.NeedsRefresh(5 * timeMinute) {
		t.Error("45 分钟后才过期，5 分钟窗口内不应刷新")
	}
	// 只剩 3 分钟，窗口 5 分钟 -> 需刷新
	soon := &Auth{ExpiresAt: nowUnix() + 3*60}
	if !soon.NeedsRefresh(5 * timeMinute) {
		t.Error("仅剩 3 分钟，5 分钟窗口内应刷新")
	}
}
