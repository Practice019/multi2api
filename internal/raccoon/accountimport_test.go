package raccoon

import (
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt raccoon 必须自报"我能批量导入"。
//
// # 为什么这条钉得住（用户要求"全部上游都可以批量导入"）
//
// 前端的「批量导入」按钮判据是 manifest 里的能力位 `import`，而后端声明它的
// 前提是**真的实现了** `gateway.AccountImportExt`。少了它，按钮要么不出现、
// 要么点了回 501（假按钮）。
func TestImplementsAccountImportExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountImportExt](p); !ok {
		t.Fatal("raccoon 必须实现 gateway.AccountImportExt —— " +
			"否则「批量导入」按钮要么不出现、要么点了回 501")
	}
	if !p.Caps().Has(gateway.CapImport) {
		t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
	}
}

// raccoonCredFixture 一份**真实形态**的凭证（键名取自 auths/raccoon/ 的实测文件）。
//
// 形态是嵌套形 `{"auth":{…},"account":{…}}` —— `MarshalAuthFile` 写的就是它，
// 所以这是"往返"的基准形状。
//
// 值全是假的。`expires_at` 是**字符串**（协议形态，不是数字）—— 这个类型
// 本身就是判据：`normalize` 会把它归一成毫秒字符串，而写成数字会让
// `ExpiresAtMS` 走另一条路（`strconv.ParseFloat`）。两种都能过不代表两种
// 都该测；这里钉住的是线上真实存储的那一种。
const raccoonCredFixture = `{"auth":{` +
	`"access_token":"eyJhbGciOiJIUzI1NiJ9.FAKE.SIG",` +
	`"refresh_token":"fake-refresh",` +
	`"expires_at":"1790766763000",` +
	`"office_identity":"personal",` +
	`"user_id":"7497524",` +
	`"nickname":"RaccoonFixture",` +
	`"phone":"15000000000",` +
	`"device_id":"0123456789abcdef0123456789abcdef"},` +
	`"account":{"uid":"7497524","nickname":"RaccoonFixture","phone":"15000000000"}}`

// TestImportCredentialsRoundTrip 导入产出的凭证必须能**原样读回**。
//
// # 这是导入功能的核心判据
//
// 导入的实现是 `ParseCredential` + `MarshalAuthFile` 的复合，所以真正要证的
// 是**往返一致**：导入写出的字节，`ParseCredential` 能解回同一份凭证。
// 不一致的表现是"导入成功、账号出现、但一发请求就 401"。
//
// ⚠ 逐字段比对而不是只比 uid：`office_identity`（X-Org-Code 头）、
// `device_id`（X-Client-Device-ID 头）都是**发请求必须带**的字段，
// 而它们不在 account 段里 —— "只搬 account 段"的实现会在这里红。
func TestImportCredentialsRoundTrip(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(raccoonCredFixture)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	// UID 取 user_id（服务端给的稳定 id，换 token 不会变），不是 access_token 哈希。
	if c.UID != "7497524" {
		t.Errorf("uid = %q —— 应取 user_id", c.UID)
	}
	if c.FileName != "raccoon-7497524.json" {
		t.Errorf("文件名应形如 raccoon-<uid>.json，实际 %q", c.FileName)
	}
	// 落盘内容必须能被自己的解析器读回（往返一致）
	back, err := ParseCredential(c.Raw)
	if err != nil {
		t.Fatalf("导入产出的内容无法被 ParseCredential 读回: %v", err)
	}
	if back.AccessToken != "eyJhbGciOiJIUzI1NiJ9.FAKE.SIG" {
		t.Errorf("access_token 丢了：%q（唯一鉴权材料）", back.AccessToken)
	}
	if back.RefreshToken != "fake-refresh" {
		t.Errorf("refresh_token 丢了：%q", back.RefreshToken)
	}
	if back.ExpiresAt != "1790766763000" {
		t.Errorf("expires_at = %q，want \"1790766763000\"（毫秒字符串）", back.ExpiresAt)
	}
	if back.OfficeIdentity != "personal" {
		t.Errorf("office_identity 丢了：%q（X-Org-Code 头要用它）", back.OfficeIdentity)
	}
	if back.DeviceID != "0123456789abcdef0123456789abcdef" {
		t.Errorf("device_id 丢了：%q（X-Client-Device-ID 头要用它）", back.DeviceID)
	}
	if back.Phone != "15000000000" {
		t.Errorf("phone 丢了：%q", back.Phone)
	}
	if back.UID() != c.UID {
		t.Errorf("读回后 uid 变了：%q → %q", c.UID, back.UID())
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials("[" + raccoonCredFixture + "," + raccoonCredFixture + "]")
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
		{"缺 access_token", `{"auth":{"refresh_token":"x","user_id":"7497524"}}`},
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
// 文件名会与 AuthDir 拼在一起。UID 优先取 `user_id`（上游/客户端决定的形态），
// 而请求体是用户可控的 —— 若它带 `/` 进来，导入端点就成了
// "往任意路径写文件"的原语。本包的 `sanitizeFilePart` 挡这一层，这条守它。
func TestImportCredentialsFileNameHasNoSeparator(t *testing.T) {
	p := NewWithConfig(Config{})
	evil := `{"auth":{"access_token":"t","user_id":"../../etc/passwd"}}`
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
