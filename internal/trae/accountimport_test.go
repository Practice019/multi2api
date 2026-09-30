package trae

import (
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt trae 必须自报"我能批量导入"。
//
// # 为什么这条钉得住（用户要求"全部上游都可以批量导入"）
//
// 前端的「批量导入」按钮判据是 manifest 里的能力位 `import`，而后端声明它的
// 前提是**真的实现了** `gateway.AccountImportExt`。少了它，按钮要么不出现、
// 要么点了回 501（假按钮）。
func TestImplementsAccountImportExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountImportExt](p); !ok {
		t.Fatal("trae 必须实现 gateway.AccountImportExt —— " +
			"否则「批量导入」按钮要么不出现、要么点了回 501")
	}
	if !p.Caps().Has(gateway.CapImport) {
		t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
	}
}

// traeCredFixture 一份**真实形态**的凭证（键名取自 auths/trae/ 的实测文件）。
//
// 形态是嵌套形 `{"auth":{…},"account":{…}}`，且 `auth` 段用**驼峰**
//（`accessToken` / `machineId`）而 `account` 段用驼峰（`enterpriseId`）——
// 与其它上游的小写下划线完全不同。这是本包最容易"看起来对、实际全丢"的地方。
//
// ⚠ 这份 fixture 刻意带上 `clientId`：它是 trae 最容易漏的一个字段
//（ExchangeToken 的 ClientID 必须与**签发**这份 refreshToken 的那个一致）。
// 硬编码一个值去刷所有账号，对"从桌面客户端导入"的账号会刷新失败 →
// 到期 → 账号表现为"突然过期"。只测 accessToken 能读回会完全漏掉它。
//
// 值全是假的。`expiresAt` / `refreshExpiresAt` 是 Unix **秒**（不是毫秒）。
const traeCredFixture = `{"auth":{` +
	`"accessToken":"eyJhbGciOiJSUzI1NiJ9.FAKE.SIG",` +
	`"refreshToken":"fake-refresh",` +
	`"expiresAt":1791964158,` +
	`"refreshExpiresAt":1806306558,` +
	`"clientId":"fakeclientid00",` +
	`"domain":"trae.cn",` +
	`"apiHost":"https://api.trae.com.cn",` +
	`"machineId":"00000000000000000000000000000000",` +
	`"deviceId":"11111111111111111111111111111111"},` +
	`"account":{` +
	`"uid":"3929003642848586",` +
	`"enterpriseId":"fakeent00000000",` +
	`"nickname":"用户00000000000"}}`

// TestImportCredentialsRoundTrip 导入产出的凭证必须能**原样读回**。
//
// # 这是导入功能的核心判据
//
// 导入的实现是 `Parse` + `MarshalAuthFile` 的复合（⚠ 本包的解析器叫 `Parse`，
// 不是别的上游的 `ParseCredential`），所以真正要证的是**往返一致**：
// 导入写出的字节，`Parse` 能解回同一份凭证。
// 不一致的表现是"导入成功、账号出现、但一发请求就 401 或到期后突然过期"。
//
// ⚠ 逐字段比对而不是只比 uid：`refreshToken` 是**消费型**的（ExchangeToken
// 用一次即作废），`clientId` 决定续期能不能成功；两个过期时刻一个决定
// 什么时候该续、另一个决定这份凭证还能不能续。
func TestImportCredentialsRoundTrip(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(traeCredFixture)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	if c.UID != "3929003642848586" {
		t.Errorf("uid = %q —— 应取 account.uid", c.UID)
	}
	if c.FileName != "trae-3929003642848586.json" {
		t.Errorf("文件名应形如 trae-<uid>.json，实际 %q", c.FileName)
	}
	// 落盘内容必须能被自己的解析器读回（往返一致）
	back, err := Parse(c.Raw)
	if err != nil {
		t.Fatalf("导入产出的内容无法被 Parse 读回: %v", err)
	}
	if back.AccessToken != "eyJhbGciOiJSUzI1NiJ9.FAKE.SIG" {
		t.Errorf("accessToken 丢了：%q（Cloud-IDE-JWT 头要用它）", back.AccessToken)
	}
	if back.RefreshToken != "fake-refresh" {
		t.Errorf("refreshToken 丢了：%q（它是消费型的，丢了就只能重登）", back.RefreshToken)
	}
	if back.ExpiresAt != 1791964158 {
		t.Errorf("expiresAt = %d，want 1791964158（Unix 秒）", back.ExpiresAt)
	}
	if back.RefreshExpiresAt != 1806306558 {
		t.Errorf("refreshExpiresAt = %d，want 1806306558 —— "+
			"不跟踪它，到期后 ExchangeToken 必然失败且**没有预警**，"+
			"账号表现为「突然过期」", back.RefreshExpiresAt)
	}
	// ⚠ 最容易漏的一个：硬编码 clientId 去刷所有账号，对从桌面客户端导入的
	// 账号会刷新失败 → 到期 → 突然过期。
	if back.ClientID != "fakeclientid00" {
		t.Errorf("clientId 丢了：%q —— "+
			"ExchangeToken 的 ClientID 必须与签发这份 refreshToken 的那个一致", back.ClientID)
	}
	if back.MachineID != "00000000000000000000000000000000" {
		t.Errorf("machineId 丢了：%q（x-machine-id 头要用它）", back.MachineID)
	}
	if back.DeviceID != "11111111111111111111111111111111" {
		t.Errorf("deviceId 丢了：%q（x-device-id 头要用它）", back.DeviceID)
	}
	if back.Domain != "trae.cn" {
		t.Errorf("domain 丢了：%q", back.Domain)
	}
	if back.ApiHost != "https://api.trae.com.cn" {
		t.Errorf("apiHost 丢了：%q（ExchangeToken 的 host）", back.ApiHost)
	}
	if back.EnterpriseID != "fakeent00000000" {
		t.Errorf("enterpriseId 丢了：%q", back.EnterpriseID)
	}
	if back.Nickname != "用户00000000000" {
		t.Errorf("nickname 丢了：%q", back.Nickname)
	}
	// UID 是 `Auth` 上的**字段**（不是方法）—— 与 cline / raccoon / qoder 的
	// `UID()` / `UIDValue()` 不同，这里按字段读。
	if back.UID != c.UID {
		t.Errorf("读回后 uid 变了：%q → %q", c.UID, back.UID)
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials("[" + traeCredFixture + "," + traeCredFixture + "]")
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
		// 嵌套形缺 accessToken：Parse 明确拒绝（"唯一鉴权材料"）。
		{"缺 accessToken", `{"auth":{"refreshToken":"fake-refresh"},"account":{"uid":"u1"}}`},
		// 扁平形同样缺 —— 两种形状的判据必须一致，否则会出现
		// "手写的能导入、登录产物不能"这种别扭的不对称。
		{"扁平形缺 accessToken", `{"refreshToken":"fake-refresh","uid":"u1"}`},
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
// 文件名会与 AuthDir 拼在一起。UID 来自 `account.uid`（上游/客户端决定的
// 形态），而请求体是用户可控的 —— 若它带 `/` 进来，导入端点就成了
// "往任意路径写文件"的原语。本包的 `sanitizeFilePart` 挡这一层，这条守它。
func TestImportCredentialsFileNameHasNoSeparator(t *testing.T) {
	p := NewWithConfig(Config{})
	evil := `{"auth":{"accessToken":"t"},"account":{"uid":"../../etc/passwd"}}`
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
