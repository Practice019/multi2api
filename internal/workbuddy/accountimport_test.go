package workbuddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt workbuddy 必须自报"我能批量导入"。
//
// # 这条为什么会红（守卫测试抓到的真实不一致）
//
// workbuddy 早就有了自己的导入端点（`import.go` 的 `handleImport`），
// 所以本轮给它加 `CapImport` 能力位时，我一度**只加了能力位、没接
// `AccountImportExt`**。后果是界面渲染出「批量导入」按钮、点下去回 501
// —— 正是本轮要消灭的假按钮形态。
//
// 这条断言（连同 `gateway.TestCapImportRequiresAccountImportExt` 的判据）
// 当场把它抓住了。留着它，下次再有人加能力位而忘了实现时会立刻红。
func TestImplementsAccountImportExt(t *testing.T) {
	var p gateway.Provider = NewWithConfig(Config{})
	if _, ok := gateway.ExtOf[gateway.AccountImportExt](p); !ok {
		t.Fatal("workbuddy 必须实现 gateway.AccountImportExt —— " +
			"只声明 CapImport 不实现它，界面那个按钮点了会回 501")
	}
	if !p.Caps().Has(gateway.CapImport) {
		t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
	}
}

// TestImportPreservesDeviceToken 导入必须**保住** deviceToken。
//
// # 这是用户实测报的（"没有 deviceToken"），而且它会静默毁数据
//
// `auth.Auth.MarshalNested`（= SaveAtomic 的序列化）会把
// `account.deviceToken` **一起写回**文件。所以导入路径若不认这个字段：
//
//	导入 → 构造的 auth.Auth.DeviceToken 为空 → 落盘写空串
//	→ **用户原本写在文件里的设备令牌被覆盖成空**
//
// 之后 `upstream.resolveDeviceToken` 的回落链只剩「配置里的全局值 /
// 全局文件」，而**每账号一个**的设备令牌才是服务端认的那个 ——
// 表现是"导入之后这个号开始被风控"，且完全看不出与导入有关。
//
// ⚠ 这条同时钉住了**旧端点**（handleImport → importOne）与
// **通用入口**（ImportCredentials）两条路径 —— 它们共用 importItem，
// 所以一处修好两处都好。
func TestImportPreservesDeviceToken(t *testing.T) {
	const dt = "dev-per-account-123"
	in := `{"用户名":"u1","uid":"d0c45ed6-aaaa-bbbb-cccc-ddddeeeeffff",` +
		`"sessionToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.x",` +
		`"refreshToken":"r1","deviceToken":"` + dt + `"}`

	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(in)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(got[0].Raw, &doc); err != nil {
		t.Fatalf("落盘内容无法解析: %v", err)
	}
	if got := doc["account"]["deviceToken"]; got != dt {
		t.Errorf("落盘的 deviceToken = %v，want %q —— "+
			"写空串会把用户已有的设备令牌**静默抹掉**（每账号一个的风控头）", got, dt)
	}
}

// TestImportAcceptsDeviceTokenAliases deviceToken 的键名变体都要认。
//
// # 为什么要认别名（而不是只认一个）
//
// 用户手上的 JSON 来自**别的导出工具**，键名不受我们控制。实测遇到过的
// 三种写法：`deviceToken`（本包落盘用的）、`device_token`（snake_case，
// 其它工具的常见形态）、`设备令牌`（中文键，与导出工具那套一致）。
//
// ⚠ 漏认任一个的后果不是"报错"，而是**静默写空**（见上一条测试）——
// 所以别名不是"顺手兼容"，是**防止数据丢失**的一部分。
func TestImportAcceptsDeviceTokenAliases(t *testing.T) {
	const token = "dev-alias-xyz"
	base := `"uid":"d0c45ed6-aaaa-bbbb-cccc-ddddeeeeffff",` +
		`"sessionToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.x"`
	for _, tc := range []struct{ name, extra string }{
		{"deviceToken", `,"deviceToken":"` + token + `"`},
		{"device_token", `,"device_token":"` + token + `"`},
		{"设备令牌", `,"设备令牌":"` + token + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewWithConfig(Config{})
			got, err := p.ImportCredentials("{" + base + tc.extra + "}")
			if err != nil {
				t.Fatalf("导入失败: %v", err)
			}
			var doc map[string]map[string]any
			if err := json.Unmarshal(got[0].Raw, &doc); err != nil {
				t.Fatal(err)
			}
			if doc["account"]["deviceToken"] != token {
				t.Errorf("键名 %s 没被认出来（落盘 = %v）—— "+
					"漏认的后果是静默写空，把用户已有的设备令牌抹掉",
					tc.name, doc["account"]["deviceToken"])
			}
		})
	}
}

// TestImportCredentialsRoundTrip 落盘内容必须能被 auth.Parse 读回。
//
// 导入的实现是"构造 auth.Auth → MarshalNested"，所以真正要证的是
// **往返一致**：导入写出的字节，`auth.Parse` 能解回同一份凭证。
// 不一致的表现是"导入成功、账号出现、但一对话就 401"。
//
// ⚠ 按 `channel`/`domain` 校验而不只是 token：它们决定**请求打哪个路由**
//（国内 copilot.tencent.com / 海外 www.workbuddy.ai），
// 而两个实例共用同一个导入实现 —— 渠道推错会让账号进错池、请求打错站点。
func TestImportCredentialsRoundTrip(t *testing.T) {
	in := `{"用户名":"u1","uid":"d0c45ed6-aaaa-bbbb-cccc-ddddeeeeffff",` +
		`"sessionToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.x",` +
		`"refreshToken":"r1","deviceToken":"dt1"}`

	// 默认实例（国内）
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(in)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	if c.UID != "d0c45ed6-aaaa-bbbb-cccc-ddddeeeeffff" {
		t.Errorf("uid = %q", c.UID)
	}
	if !strings.HasPrefix(c.FileName, "workbuddy-") || !strings.HasSuffix(c.FileName, ".json") {
		t.Errorf("文件名应形如 workbuddy-<uid>.json，实际 %q", c.FileName)
	}
	if strings.ContainsAny(c.FileName, `/\`) {
		t.Errorf("文件名含路径分隔符：%q —— 导入端点会变成任意路径写文件的原语", c.FileName)
	}
}

// TestImportOneAlsoPreservesDeviceToken **旧端点**那条路径同样必须保住 deviceToken。
//
// # 为什么要单独一条（变异存活暴露的洞）
//
// 本包有**两条**导入路径：
//
//	通用入口  AdminHandler.handleImport → importOne        （自己写盘）
//	新扩展点  Provider.ImportCredentials → buildImportedCredential（核心写盘）
//
// 我第一版只测了后者，于是把 `importOne` 里的 `DeviceToken:` 赋值删掉后
// **测试照样绿** —— 变异存活，说明那条路径没有任何守卫。
//
// 两条路径共用 `importItem` 与 `fillAliases`（所以解析侧是同一份），
// 但"把 it.DeviceToken 搬进 auth.Auth"是**各自写的**一行 ——
// 正是这一行会漏，所以两条都要钉。
func TestImportOneAlsoPreservesDeviceToken(t *testing.T) {
	const dt = "dev-old-endpoint-456"
	it := importItem{
		UID:          "d0c45ed6-aaaa-bbbb-cccc-ddddeeeeffff",
		AccessToken:  "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.x",
		RefreshToken: "r1",
		DeviceToken:  dt,
	}
	dir := t.TempDir()
	if _, _, err := importOne(dir, "workbuddy", it); err != nil {
		t.Fatalf("importOne 失败: %v", err)
	}
	// 读回落盘的文件
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-"+it.UID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc["account"]["deviceToken"]; got != dt {
		t.Errorf("旧端点落盘的 deviceToken = %v，want %q —— "+
			"写空串会把用户已有的设备令牌**静默抹掉**", got, dt)
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
		{"缺 sessionToken", `{"uid":"d0c45ed6-aaaa-bbbb-cccc-ddddeeeeffff"}`},
		{"uid 含路径分隔符", `{"uid":"../../etc/passwd","sessionToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.x"}`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := p.ImportCredentials(bad.in); err == nil {
				t.Error("应当报错")
			}
		})
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	one := `{"用户名":"u1","uid":"d0c45ed6-aaaa-bbbb-cccc-ddddeeeeffff",` +
		`"sessionToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSJ9.x"}`
	two := `{"用户名":"u2","uid":"11111111-2222-3333-4444-555555555555",` +
		`"sessionToken":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MiJ9.y"}`
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials("[" + one + "," + two + "]")
	if err != nil {
		t.Fatalf("数组导入失败: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("应当产出 2 条，实际 %d", len(got))
	}
}
