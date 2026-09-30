package lobsterai

import (
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt lobsterai 必须自报"我能批量导入"。
//
// # 为什么这条钉得住（用户要求"全部上游都可以批量导入"）
//
// 前端的「批量导入」按钮判据是 manifest 里的能力位 `import`，而后端声明它的
// 前提是**真的实现了** `gateway.AccountImportExt`。少了它，按钮要么不出现、
// 要么点了回 501（假按钮）。
func TestImplementsAccountImportExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountImportExt](p); !ok {
		t.Fatal("lobsterai 必须实现 gateway.AccountImportExt —— " +
			"否则「批量导入」按钮要么不出现、要么点了回 501")
	}
	if !p.Caps().Has(gateway.CapImport) {
		t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
	}
}

// lobsteraiCredFixture 一份**真实形态**的凭证（键名取自 auths/lobsterai/ 的实测文件）。
//
// 形态是嵌套形 `{"auth":{…},"account":{…}}` —— `MarshalAuthFile` 写的就是它。
//
// ⚠ 这份 fixture 刻意**带上** `uuid` / `first_keyfrom` / `latest_keyfrom`：
// 它们是本上游最容易漏的三个字段（不在服务端响应里，是客户端侧生成的状态），
// 而每次续期都要原样回传 —— 丢了它们续期会被服务端拒绝。
// 只测"access_token 能读回"会完全漏掉这三条，而它们的失效形态是
// "导入成功、能用两小时、之后再也续不上"（最难归因的那一种）。
//
// 值全是假的。`expires_at` 是字符串形态（协议形态）。
const lobsteraiCredFixture = `{"auth":{` +
	`"access_token":"eyJhbGciOiJIUzUxMiJ9.FAKE.SIG",` +
	`"refresh_token":"fake-refresh",` +
	`"expires_at":"1793347966000",` +
	`"uid":"103668",` +
	`"user_id":"103668",` +
	`"nickname":"150****0000",` +
	`"uuid":"00000000-0000-4000-8000-000000000000",` +
	`"first_keyfrom":"1790624813202",` +
	`"latest_keyfrom":"1790624813202"},` +
	`"account":{"uid":"103668","nickname":"150****0000"}}`

// TestImportCredentialsRoundTrip 导入产出的凭证必须能**原样读回**。
//
// # 这是导入功能的核心判据
//
// 导入的实现是 `ParseCredential` + `MarshalAuthFile` 的复合，所以真正要证的
// 是**往返一致**：导入写出的字节，`ParseCredential` 能解回同一份凭证。
// 不一致的表现是"导入成功、账号出现、但一发请求就 401 或两小时后续期被拒"。
func TestImportCredentialsRoundTrip(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(lobsteraiCredFixture)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	if c.UID != "103668" {
		t.Errorf("uid = %q —— 应取 auth.uid", c.UID)
	}
	if c.FileName != "lobsterai-103668.json" {
		t.Errorf("文件名应形如 lobsterai-<uid>.json，实际 %q", c.FileName)
	}
	// 落盘内容必须能被自己的解析器读回（往返一致）
	back, err := ParseCredential(c.Raw)
	if err != nil {
		t.Fatalf("导入产出的内容无法被 ParseCredential 读回: %v", err)
	}
	if back.AccessToken != "eyJhbGciOiJIUzUxMiJ9.FAKE.SIG" {
		t.Errorf("access_token 丢了：%q（唯一鉴权材料）", back.AccessToken)
	}
	if back.RefreshToken != "fake-refresh" {
		t.Errorf("refresh_token 丢了：%q", back.RefreshToken)
	}
	if back.ExpiresAt != "1793347966000" {
		t.Errorf("expires_at = %q，want \"1793347966000\"（毫秒字符串）", back.ExpiresAt)
	}
	if back.UserID != "103668" {
		t.Errorf("user_id 丢了：%q（续期请求体要用它）", back.UserID)
	}
	// ⚠ 下面三个是"导入成功但之后续不上"的三个必要条件。
	if back.UUID != "00000000-0000-4000-8000-000000000000" {
		t.Errorf("uuid 丢了：%q —— 装上它才叫往返一致；丢了每次续期都会被拒",
			back.UUID)
	}
	if back.FirstKeyfrom != "1790624813202" {
		t.Errorf("first_keyfrom 丢了：%q（续期请求体要原样回传，不取当前时刻）",
			back.FirstKeyfrom)
	}
	if back.LatestKeyfrom != "1790624813202" {
		t.Errorf("latest_keyfrom 丢了：%q（与 first_keyfrom 同理）", back.LatestKeyfrom)
	}
	if back.UIDValue() != c.UID {
		t.Errorf("读回后 uid 变了：%q → %q", c.UID, back.UIDValue())
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials("[" + lobsteraiCredFixture + "," + lobsteraiCredFixture + "]")
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
		{"缺 access_token", `{"auth":{"refresh_token":"x","uid":"103668"}}`},
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
// 文件名会与 AuthDir 拼在一起。`UIDValue()` 优先取 `uid` / `user_id`，
// 最后回落到 access_token 哈希 —— 前两者来自用户粘贴的请求体，完全可控。
// 若它带 `/` 进来，导入端点就成了"往任意路径写文件"的原语。
// 本包的 `sanitizeFilePart` 挡这一层，这条守它。
func TestImportCredentialsFileNameHasNoSeparator(t *testing.T) {
	p := NewWithConfig(Config{})
	evil := `{"auth":{"access_token":"t","uid":"../../etc/passwd"}}`
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
