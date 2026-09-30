package cline

import (
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt cline 必须自报"我能批量导入"。
//
// # 为什么这条钉得住（用户要求"全部上游都可以批量导入"）
//
// 前端的「批量导入」按钮判据是 manifest 里的能力位 `import`，而后端
// 声明它的前提是**真的实现了** `gateway.AccountImportExt`。
// 少了这个扩展点，按钮要么不出现（用户看不到）、要么点了回 501
// （假按钮）—— 两种都是本轮要消灭的形态。
func TestImplementsAccountImportExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountImportExt](p); !ok {
		t.Fatal("cline 必须实现 gateway.AccountImportExt —— " +
			"否则「批量导入」按钮要么不出现、要么点了回 501")
	}
	if !p.Caps().Has(gateway.CapImport) {
		t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
	}
}

// clineCredFixture 一份**真实形态**的凭证（键名取自 auths/cline/ 的实测文件）。
//
// 值都是假的：测试只验证"字段有没有被搬对"，不需要真 token。
const clineCredFixture = `{"auth":{` +
	`"access_token":"workos:eyJhbGciOiJSUzI1NiJ9.FAKE.SIG",` +
	`"refresh_token":"fake-refresh",` +
	`"expire_time":1790754554000,` +
	`"account_id":"usr-01M3MJDCGSD05W4Y83JQC71XCT",` +
	`"email":"a@b.c"},` +
	`"account":{"uid":"usr-01M3MJDCGSD05W4Y83JQC71XCT","nickname":"甲","email":"a@b.c"}}`

// TestImportCredentialsRoundTrip 导入产出的凭证必须能**原样读回**。
//
// # 这是导入功能的核心判据
//
// 导入的实现是 `ParseCredential` + `MarshalAuthFile` 的复合，所以
// 真正要证的是**往返一致**：导入写出的字节，`ParseCredential` 能解回
// 同一份凭证。不一致的话表现是"导入成功、账号出现、但一对话就 401"。
//
// ⚠ 逐字段比对而不是只比 uid：`access_token` 的前缀（`workos:`）、
// `account_id`（余额端点必须用它，不是 JWT 的 sub）都是**必须保住**的字段。
func TestImportCredentialsRoundTrip(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(clineCredFixture)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	if c.UID != "usr-01M3MJDCGSD05W4Y83JQC71XCT" {
		t.Errorf("uid = %q", c.UID)
	}
	if !strings.HasPrefix(c.FileName, "cline-") || !strings.HasSuffix(c.FileName, ".json") {
		t.Errorf("文件名应形如 cline-<uid>.json，实际 %q", c.FileName)
	}
	// 落盘内容必须能被自己的解析器读回（往返一致）
	back, err := ParseCredential(c.Raw)
	if err != nil {
		t.Fatalf("导入产出的内容无法被 ParseCredential 读回: %v", err)
	}
	if back.AccessToken != "workos:eyJhbGciOiJSUzI1NiJ9.FAKE.SIG" {
		t.Errorf("access_token 没有保住 `workos:` 前缀：%q", back.AccessToken)
	}
	if back.AccountID != "usr-01M3MJDCGSD05W4Y83JQC71XCT" {
		t.Errorf("account_id 丢了：%q（余额端点必须用它，不是 JWT 的 sub）", back.AccountID)
	}
	if back.ExpireTime != 1790754554000 {
		t.Errorf("expire_time = %d，want 1790754554000", back.ExpireTime)
	}
	if back.RefreshToken != "fake-refresh" {
		t.Errorf("refresh_token 丢了：%q", back.RefreshToken)
	}
	if back.UID() != c.UID {
		t.Errorf("读回后 uid 变了：%q → %q", c.UID, back.UID())
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials("[" + clineCredFixture + "," + clineCredFixture + "]")
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
		{"缺 access_token", `{"auth":{"refresh_token":"x"}}`},
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
// 文件名会与 AuthDir 拼在一起。若 uid 能带 `/` 进来，导入端点就成了
// "往任意路径写文件"的原语 —— 而请求体是用户可控的。
// 本包的 `FileName` 用 `sanitizeFilePart` 挡了一层，这条守它。
func TestImportCredentialsFileNameHasNoSeparator(t *testing.T) {
	p := NewWithConfig(Config{})
	// 用一个带 `/` 的 account_id 试探
	evil := `{"auth":{"access_token":"workos:t","account_id":"../../etc/passwd"}}`
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
