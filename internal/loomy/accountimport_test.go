package loomy

import (
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt loomy 必须自报"我能批量导入"。
//
// # 为什么这条钉得住（用户要求"全部上游都可以批量导入"）
//
// 前端的「批量导入」按钮判据是 manifest 里的能力位 `import`，而后端声明它的
// 前提是**真的实现了** `gateway.AccountImportExt`。少了这个扩展点，按钮要么
// 不出现（用户看不到）、要么点了回 501（假按钮）。
//
// ⚠ loomy 此前**已经有**一份 `handleImport`（HTTP 端点，见 import.go），
// 所以这里要证的不是"有没有导入"，而是"通用入口能不能用" —— 两条路走的是
// 不同的解析器：`handleImport` 认 `userId`/`userid`/`uid` 别名，
// 通用入口走 `ParseCredential`。只有后者通了，前端那个统一按钮才有意义。
func TestImplementsAccountImportExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountImportExt](p); !ok {
		t.Fatal("loomy 必须实现 gateway.AccountImportExt —— " +
			"否则「批量导入」按钮要么不出现、要么点了回 501")
	}
	if !p.Caps().Has(gateway.CapImport) {
		t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
	}
}

// loomyCredFixture 一份**真实形态**的凭证（键名取自 auths/loomy/ 的实测文件）。
//
// 值全是假的。`session` 用 32 位小写 hex 是因为该**形态**才是判据
//（looksLikeSession 的正例）—— 32 位这个长度是"要不要打告警"的分界，
// 而具体是哪 32 个字符与任何断言无关（见 contract_hermetic_test.go 里
// 「不要把真实凭证抄进 fixture」那条教训）。
const loomyCredFixture = `{` +
	`"session":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
	`"uid":"260101010101010101",` +
	`"nickname":"NK0000000",` +
	`"phone":"139****0000",` +
	`"userId":"260101010101010101",` +
	`"loggedInAt":"2026-01-01T00:00:00.000Z"}`

// TestImportCredentialsRoundTrip 导入产出的凭证必须能**原样读回**。
//
// # 这是导入功能的核心判据
//
// 导入的实现是 `ParseCredential` + `MarshalAuthFile` 的复合，所以真正要证的
// 是**往返一致**：导入写出的字节，`ParseCredential` 能解回同一份凭证。
// 不一致的表现是"导入成功、账号出现、但一对话就 401/登录已失效"。
//
// ⚠ `session` 是 loomy **唯一**的鉴权材料，它丢了这份凭证就是废的 ——
// 所以它必须逐字保住，不是"能解出 uid 就算过"。
func TestImportCredentialsRoundTrip(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(loomyCredFixture)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	if c.UID != "260101010101010101" {
		t.Errorf("uid = %q", c.UID)
	}
	if c.FileName != "loomy-260101010101010101.json" {
		t.Errorf("文件名应形如 loomy-<uid>.json，实际 %q", c.FileName)
	}
	// 落盘内容必须能被自己的解析器读回（往返一致）
	back, err := ParseCredential(c.Raw)
	if err != nil {
		t.Fatalf("导入产出的内容无法被 ParseCredential 读回: %v", err)
	}
	if back.Session != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("session 没有保住：%q —— 它是 loomy 唯一的鉴权材料", back.Session)
	}
	if back.UID != c.UID {
		t.Errorf("读回后 uid 变了：%q → %q", c.UID, back.UID)
	}
	if back.UserID != "260101010101010101" {
		t.Errorf("userId 丢了：%q", back.UserID)
	}
	if back.Phone != "139****0000" {
		t.Errorf("phone 丢了：%q", back.Phone)
	}
	// 展示名必须与导入时一致：不一致的形态是"导入后显示 NK0000000、
	// 手动登录显示手机号"（ParseCredential 里那段"手机号优先"就是为此修的）。
	if back.Nickname != c.Nickname {
		t.Errorf("读回后 nickname 变了：%q → %q", c.Nickname, back.Nickname)
	}
	if back.Nickname != "139****0000" {
		t.Errorf("nickname = %q，want 手机号（用户要求所有上游统一用手机号显示账号）",
			back.Nickname)
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials("[" + loomyCredFixture + "," + loomyCredFixture + "]")
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
		{"缺 session", `{"uid":"260101010101010101","phone":"139****0000"}`},
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
// 文件名会与 AuthDir 拼在一起。`ParseCredential` 的 UID 优先取上游登录态里的
// `userid` —— 那个字段的形态由上游/客户端决定，而请求体是用户可控的。
// 若它带 `/` 进来，导入端点就成了"往任意路径写文件"的原语。
// 本包的 `FileName` 用 `sanitizeFilePart` 挡了一层，这条守它。
func TestImportCredentialsFileNameHasNoSeparator(t *testing.T) {
	p := NewWithConfig(Config{})
	evil := `{"session":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","userid":"../../etc/passwd"}`
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
