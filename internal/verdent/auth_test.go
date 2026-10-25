// auth_test.go 凭据解析的判据 —— 重点是 JWT 里 user_id 的**类型与精度**。
//
// # 为什么这条测试必须存在（2026-10-26 实测踩到）
//
// 上游的 JWT payload 是：
//
//	{"user_id": 749951105572520000, "token_type": "access", "exp": 1795509874, ...}
//
// **user_id 是 JSON 数字**。第一版把它声明成 string，于是 json.Unmarshal 报
// "cannot unmarshal number into Go struct field"，DecodeJWT 返回 error，
// Usable() 恒 false —— 表现为"账号文件明明在 auths/verdent 里，LoadDir 一条都读不出来"，
// 而日志只说"里没有可用的 token"（那句日志是真的，只是没说是类型不对）。
//
// 更隐蔽的是**精度**：这个数有 18 位，超过 float64 的 53 位整数精度。
// 它的量级在 2^59~2^60，ULP = 2^7 = 128，而 749951105572520000 不是 128 的倍数
// —— 用 float64 兜会把 uid 悄悄改掉，而 uid 是账号池主键：
// 改掉 = 每次重启多一条幽灵账号（且删不掉，因为 uid 对不上）。
//
// 所以下面同时钉住两件事：**类型能认** 与 **精度不丢**。
package verdent

import (
	"encoding/base64"
	"testing"
)

// syntheticJWT 用给定的 payload JSON 片段拼一个三段式 token（签名段是假的）。
//
// ⚠ 只用于测试：不提交任何真实凭证。签名不参与解析（我们刻意不验签，见 DecodeJWT）。
func syntheticJWT(payloadJSON string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc("{\"alg\":\"HS256\",\"typ\":\"JWT\"}") + "." + enc(payloadJSON) + ".fakesignature"
}

// TestDecodeJWTAcceptsNumericUserID 数字形态的 user_id 必须被认出来。
func TestDecodeJWTAcceptsNumericUserID(t *testing.T) {
	tok := syntheticJWT("{\"user_id\":749951105572520000,\"version\":0,\"token_type\":\"access\",\"exp\":1795509874}")
	p, err := DecodeJWT(tok)
	if err != nil {
		t.Fatalf("数字形态的 user_id 解析失败（回归：第一版就是死在这里）: %v", err)
	}
	if got := p.UserIDString(); got != "749951105572520000" {
		t.Errorf("user_id = %q，want 749951105572520000", got)
	}
}

// TestDecodeJWTKeepsLargeUserIDPrecision 18 位 uid 必须**逐位精确**。
//
// 这条是防"有人把 UseNumber 去掉"的：去掉之后数字会走 float64，
// 而 749951105572520000 在 float64 里会变成 749951105572520064 或 749951105572519936。
func TestDecodeJWTKeepsLargeUserIDPrecision(t *testing.T) {
	const want = "749951105572520000"
	tok := syntheticJWT("{\"user_id\":749951105572520000,\"token_type\":\"access\"}")
	p, err := DecodeJWT(tok)
	if err != nil {
		t.Fatal(err)
	}
	got := p.UserIDString()
	if got != want {
		t.Errorf("uid 精度丢失：实得 %s，期望 %s —— "+
			"这是 float64 舍入的典型症状，检查 DecodeJWT 里的 UseNumber 是否还在", got, want)
	}
}

// TestDecodeJWTAcceptsStringUserID 字符串形态也要认（上游可能两种都发过）。
func TestDecodeJWTAcceptsStringUserID(t *testing.T) {
	p, err := DecodeJWT(syntheticJWT("{\"user_id\":\"749951105572520000\",\"token_type\":\"access\"}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.UserIDString(); got != "749951105572520000" {
		t.Errorf("字符串形态的 user_id = %q", got)
	}
}

// TestAuthUIDFallsBackToJWT 文件里没写 userId 时，从 JWT 里取。
func TestAuthUIDFallsBackToJWT(t *testing.T) {
	a := &Auth{Token: syntheticJWT("{\"user_id\":\"749951105572520000\",\"token_type\":\"access\"}")}
	if got := a.UID(); got != "749951105572520000" {
		t.Errorf("UID() = %q，want 从 JWT 解出的那个", got)
	}
	if !a.Usable() {
		t.Error("Usable() = false —— 有合法 JWT 就应当可用")
	}
}

// TestAuthUIDPrefersField 文件里的 userId 优先于 JWT（它是显式声明的事实）。
func TestAuthUIDPrefersField(t *testing.T) {
	a := &Auth{
		UserID: "from-file",
		Token:  syntheticJWT("{\"user_id\":\"749951105572520000\",\"token_type\":\"access\"}"),
	}
	if got := a.UID(); got != "from-file" {
		t.Errorf("UID() = %q，want from-file（文件里的显式字段优先）", got)
	}
}

// TestParseAuthFileAcceptsBothShapes 两种磁盘形态都要能读。
func TestParseAuthFileAcceptsBothShapes(t *testing.T) {
	flat := []byte("{\"version\":3,\"platform\":\"verdent\",\"userId\":\"u1\",\"token\":\"t1\",\"email\":\"a@b.c\"}")
	a, err := parseAuthFile(flat)
	if err != nil {
		t.Fatalf("扁平形态解析失败: %v", err)
	}
	if a.UserID != "u1" || a.Token != "t1" || a.Email != "a@b.c" {
		t.Errorf("扁平形态字段没对上: %+v", a)
	}
	wrapped := []byte("{\"auth\":{\"userId\":\"u2\",\"token\":\"t2\"},\"remark\":\"手抄的\"}")
	b, err := parseAuthFile(wrapped)
	if err != nil {
		t.Fatalf("包装形态解析失败: %v", err)
	}
	if b.UserID != "u2" || b.Token != "t2" {
		t.Errorf("包装形态字段没对上: %+v", b)
	}
}

// TestTokenExpiredAtUsesJWTExp 到期时刻应当来自 JWT 的 exp（秒 → 时间）。
func TestTokenExpiredAtUsesJWTExp(t *testing.T) {
	a := &Auth{Token: syntheticJWT("{\"user_id\":1,\"exp\":1795509874}")}
	got := a.TokenExpiredAt()
	if got.IsZero() {
		t.Fatal("TokenExpiredAt 返回零值 —— JWT 里的 exp 没被用上")
	}
	if got.Unix() != 1795509874 {
		t.Errorf("到期时刻 = %d，want 1795509874", got.Unix())
	}
}
