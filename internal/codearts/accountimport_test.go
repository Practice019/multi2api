package codearts

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt codearts 必须自报"我能批量导入"。
//
// # 为什么这条钉得住（用户要求"全部上游都可以批量导入"）
//
// 前端的「批量导入」按钮判据是 manifest 里的能力位 `import`，而后端声明它的
// 前提是**真的实现了** `gateway.AccountImportExt`。少了它，按钮要么不出现、
// 要么点了回 501（假按钮）—— 两种都是本轮要消灭的形态。
func TestImplementsAccountImportExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountImportExt](p); !ok {
		t.Fatal("codearts 必须实现 gateway.AccountImportExt —— " +
			"否则「批量导入」按钮要么不出现、要么点了回 501")
	}
	if !p.Caps().Has(gateway.CapImport) {
		t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
	}
}

// codeartsCredFixture 一份**真实形态**的凭证（键名取自 auths/codearts/ 的实测文件）。
//
// 形态是 OAuth 登录产物的**嵌套形**：{"auth":{…},"account":{…},"dpop":{…}}。
// 它同时是 `cmd/login` 与 `SaveAtomic` 的产物形态 —— 用户手上有什么就粘什么，
// 所以导入必须认的就是这一个形状（扁平形另有一份覆盖，见 badInput 的用例）。
//
// 值全是假的。`refresh_token` 用 `makeRefreshToken` 造：它是本包 jwt_test.go
// 已有的 helper，产出的 JWT 形态与线上一致（payload.user_profile 再一层 base64）。
// 用真形态而不是随手一个字符串，是因为 `ParseCredential` 的 UID **优先**取
// refresh_token 里的 account_id —— 随手写个非 JWT 会让这条最要紧的取值路径
// 在测试里从未被走到。
func codeartsCredFixture(t *testing.T) string {
	t.Helper()
	rt := makeRefreshToken(t, "01a00000000000000000000000000000", "hw000000000")
	doc := map[string]any{
		"auth": map[string]any{
			"accessKeyId":     "FAKEAK0000000000000",
			"secretAccessKey": "fake-secret-key",
			"securityToken":   "fake-sts-token",
			"expiresAt":       1790754554,
			"refresh_token":   rt,
			"clientId":        "vscode-codebot",
		},
		"account": map[string]any{
			// 刻意与 JWT 里的 account_id 一致：磁盘上这两个值同源，
			// 不一致的形态是"旧版 cmd/login 没写 uid"（那时靠 AK 兜底）。
			"uid":      "01a00000000000000000000000000000",
			"nickname": "hw000000000",
		},
		"dpop": map[string]any{
			// 只要是一段合法 JSON 对象即可（HasUsableDPoPKey 不看内容）。
			// 但**必须非空**：缺它这份凭证连发请求的资格都没有。
			"privateKeyJwk": map[string]any{
				"kty": "EC", "crv": "P-256",
				"x": "ZmFrZS14", "y": "ZmFrZS15", "d": "ZmFrZS1k",
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("构造 fixture 失败: %v", err)
	}
	return string(b)
}

// TestImportCredentialsRoundTrip 导入产出的凭证必须能**原样读回**。
//
// # 这是导入功能的核心判据
//
// 导入的实现是 `ParseCredential` + `MarshalNested` 的复合，所以真正要证的
// 是**往返一致**：导入写出的字节，`ParseCredential` 能解回同一份凭证。
// 不一致的表现是"导入成功、账号出现、但一发请求就 401"。
//
// ⚠ 逐字段比对而不是只比 uid：AK/SK/STS 三元组是**签名**的输入（少一个就
// 签不出请求），`refresh_token` 决定这份凭证能不能续期 —— 而 codearts 的
// STS 只有约 2 小时寿命，丢了它凭证两小时后就永久报废。
//
// ⚠ DPoP 私钥单独断言：它是本上游最容易漏的字段（续期凭证不是一个 token，
// 而是「refresh_token + ES256 私钥」一对），而 `json.RawMessage` 在
// "字段缺失"与"显式 null"时会留下不同字节 —— 所以这里按
// `HasUsableDPoPKey` 的语义断言，而不是判 `len(raw) > 0`。
func TestImportCredentialsRoundTrip(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(codeartsCredFixture(t))
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	// UID 来自 refresh_token 的 JWT（权威来源），不是 account.uid、也不是 AK。
	if c.UID != "01a00000000000000000000000000000" {
		t.Errorf("uid = %q —— 应取 refresh_token 里 JWT 的 account_id", c.UID)
	}
	if !strings.HasPrefix(c.FileName, "codearts-") || !strings.HasSuffix(c.FileName, ".json") {
		t.Errorf("文件名应形如 codearts-<uid>.json，实际 %q", c.FileName)
	}
	// 落盘内容必须能被自己的解析器读回（往返一致）
	back, err := ParseCredential(c.Raw)
	if err != nil {
		t.Fatalf("导入产出的内容无法被 ParseCredential 读回: %v", err)
	}
	if back.AccessKey != "FAKEAK0000000000000" {
		t.Errorf("accessKeyId 丢了：%q（它是 SDK-HMAC-SHA256 签名的一半）", back.AccessKey)
	}
	if back.SecretKey != "fake-secret-key" {
		t.Errorf("secretAccessKey 丢了：%q（缺它签不出任何请求）", back.SecretKey)
	}
	if back.SecurityToken != "fake-sts-token" {
		t.Errorf("securityToken 丢了：%q", back.SecurityToken)
	}
	if back.ExpiresAt != 1790754554 {
		t.Errorf("expiresAt = %d，want 1790754554", back.ExpiresAt)
	}
	if back.RefreshToken == "" {
		t.Error("refresh_token 丢了 —— 没有它这份 STS 凭证两小时后永久报废")
	}
	if back.ClientID != "vscode-codebot" {
		t.Errorf("clientId 丢了：%q", back.ClientID)
	}
	if !HasUsableDPoPKey(back.DPoPPrivateKeyJWK) {
		t.Errorf("dpop.privateKeyJwk 没保住（原样 %s）—— "+
			"续期凭证不是单个 token 而是「refresh_token + ES256 私钥」一对",
			c.Raw)
	}
	if back.UID != c.UID {
		t.Errorf("读回后 uid 变了：%q → %q", c.UID, back.UID)
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	p := NewWithConfig(Config{})
	f := codeartsCredFixture(t)
	got, err := p.ImportCredentials("[" + f + "," + f + "]")
	if err != nil {
		t.Fatalf("数组导入失败: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("应当产出 2 条，实际 %d", len(got))
	}
}

// TestImportCredentialsRejectsBadInput 坏输入必须**报带原因的错**。
//
// 静默返回空切片会让用户看到"导入了 0 个"而不知道哪里错了。
func TestImportCredentialsRejectsBadInput(t *testing.T) {
	p := NewWithConfig(Config{})
	for _, bad := range []struct{ name, in string }{
		{"空串", ""},
		{"非 JSON", "not json"},
		{"空数组", "[]"},
		// 嵌套形缺 AK/SK：ParseCredential 明确拒绝（`missing accessKeyId/secretAccessKey`）。
		{"缺 accessKeyId/secretAccessKey", `{"auth":{"securityToken":"x","refresh_token":"y"}}`},
		// 扁平形同样缺 AK/SK —— 两种形状的判据必须一致，否则
		// "手工抄的能导入、登录产物的不能"（本项目反复吃这种不对称的亏）。
		{"扁平形缺 AK/SK", `{"securityToken":"x","refresh_token":"y"}`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := p.ImportCredentials(bad.in); err == nil {
				t.Error("应当报错")
			}
		})
	}
}

// TestImportCredentialsFileNameHasNoSeparator 文件名不得含路径分隔符。
//
// # 为什么这条是安全断言（不是风格）
//
// 文件名会与 AuthDir 拼在一起。UID 优先取 refresh_token 里的 account_id，
// 但**回落到 AK**（`a.UID = a.AccessKey`）—— 而 AK 来自用户粘贴的请求体，
// 完全可控。若它带 `/` 进来，导入端点就成了"往任意路径写文件"的原语。
// 本包的 `sanitizeFileName` 挡这一层，这条守它。
func TestImportCredentialsFileNameHasNoSeparator(t *testing.T) {
	p := NewWithConfig(Config{})
	// 非 JWT 的 refresh_token + 带 `/` 的 AK：UID 会回落到这个 AK。
	evil := `{"auth":{"accessKeyId":"../../etc/passwd","secretAccessKey":"s"},` +
		`"account":{"uid":""}}`
	got, err := p.ImportCredentials(evil)
	if err != nil {
		return // 拒绝也算安全
	}
	for _, c := range got {
		if strings.ContainsAny(c.FileName, `/\`) {
			t.Errorf("文件名含路径分隔符：%q —— "+
				"导入端点会变成任意路径写文件的原语", c.FileName)
		}
	}
}
