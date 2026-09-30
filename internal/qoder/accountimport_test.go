package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestImplementsAccountImportExt qoder / qodercn 必须自报"我能批量导入"。
//
// # 为什么这条钉得住（用户要求"全部上游都可以批量导入"）
//
// 前端的「批量导入」按钮判据是 manifest 里的能力位 `import`，而后端声明它的
// 前提是**真的实现了** `gateway.AccountImportExt`。少了它，按钮要么不出现、
// 要么点了回 501（假按钮）。
//
// ⚠ 两个实例都要查：本包的 `ID()` 返回 `productID`（`qoder` / `qodercn`），
// 前端是**按实例**渲染卡片的。只查国际版会漏掉"CN 卡片上没有那个按钮"。
func TestImplementsAccountImportExt(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    gateway.Provider
	}{
		{"国际版", NewWithConfig(Config{})},
		{"中国版", NewWithConfig(Config{Product: QoderCN})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := gateway.ExtOf[gateway.AccountImportExt](tc.p); !ok {
				t.Fatal("必须实现 gateway.AccountImportExt —— " +
					"否则「批量导入」按钮要么不出现、要么点了回 501")
			}
			if !tc.p.Caps().Has(gateway.CapImport) {
				t.Error("Caps() 里必须声明 CapImport —— 前端按能力位渲染那个按钮")
			}
		})
	}
}

// qoderCredFixture 一份**真实形态**的凭证（键名取自 auths/qoder/ 的实测文件）。
//
// 形态是嵌套形 `{"auth":{…},"account":{…}}` —— `MarshalAuthFile` 写的就是它。
//
// 值全是假的。`access_token` 用 `dt-` 前缀的不透明串**不是**装饰：
// qoder 的 token 不是 JWT（解不出 exp），所以 `expires_at` 那个绝对毫秒时刻
// 是「Token」列与"要不要续期"的唯一权威 —— 写成 JWT 会让这条最要紧的
// 取值路径（ExpiresAtMS 的主路径）在测试里从未被走到。
const qoderCredFixture = `{"auth":{` +
	`"access_token":"dt-FAKE0000000000000000000",` +
	`"refresh_token":"drt-FAKE00000000000000000",` +
	`"expires_at":1793346517000,` +
	`"refresh_token_expires_at":1821858517000,` +
	`"uid":"01a00000-0000-7000-8000-000000000000",` +
	`"nickname":"nick000000",` +
	`"machine_id":"00000000000000000000000000000000",` +
	`"device_id":"",` +
	`"product_id":"qoder"},` +
	`"account":{"uid":"01a00000-0000-7000-8000-000000000000","nickname":"nick000000"}}`

// TestImportCredentialsRoundTrip 导入产出的凭证必须能**原样读回**。
//
// # 这是导入功能的核心判据
//
// 导入的实现是 `ParseCredential` + `MarshalAuthFile` 的复合，所以真正要证的
// 是**往返一致**：导入写出的字节，`ParseCredential` 能解回同一份凭证。
// 不一致的表现是"导入成功、账号出现、但一发请求就 401"。
//
// ⚠ 逐字段比对而不是只比 uid：`machine_id`（**续期请求体必须带它**）
// 与两个绝对毫秒时刻（Token 列 + 续期判据）都不在 account 段里 ——
// "只搬 account 段"的实现会在这里红。
func TestImportCredentialsRoundTrip(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials(qoderCredFixture)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	c := got[0]
	if c.UID != "01a00000-0000-7000-8000-000000000000" {
		t.Errorf("uid = %q —— 应取 auth.uid", c.UID)
	}
	// 国际版文件名不带后缀；CN 版带 `-cn`（两版共用同一目录，靠它防互相覆盖）。
	if c.FileName != "qoder-01a00000-0000-7000-8000-000000000000.json" {
		t.Errorf("文件名 = %q，want qoder-<uid>.json", c.FileName)
	}
	// 落盘内容必须能被自己的解析器读回（往返一致）
	back, err := ParseCredential(c.Raw)
	if err != nil {
		t.Fatalf("导入产出的内容无法被 ParseCredential 读回: %v", err)
	}
	if back.AccessToken != "dt-FAKE0000000000000000000" {
		t.Errorf("access_token 没有保住 `dt-` 前缀：%q", back.AccessToken)
	}
	if back.RefreshToken != "drt-FAKE00000000000000000" {
		t.Errorf("refresh_token 丢了：%q", back.RefreshToken)
	}
	if back.ExpiresAt != 1793346517000 {
		t.Errorf("expires_at = %d，want 1793346517000 —— "+
			"它是「Token」列与「要不要续期」的唯一权威（dt- token 解不出 exp）", back.ExpiresAt)
	}
	if back.RefreshTokenExpiresAt != 1821858517000 {
		t.Errorf("refresh_token_expires_at = %d，want 1821858517000 —— "+
			"缺它就分不清「access 过期可续」与「refresh 也过期只能重登」",
			back.RefreshTokenExpiresAt)
	}
	if back.MachineID != "00000000000000000000000000000000" {
		t.Errorf("machine_id 丢了：%q —— 续期请求体必须带它，丢了续期会被拒",
			back.MachineID)
	}
	if back.UIDValue() != c.UID {
		t.Errorf("读回后 uid 变了：%q → %q", c.UID, back.UIDValue())
	}
}

// TestImportCredentialsAcceptsArray 数组（多条）也要认。
func TestImportCredentialsAcceptsArray(t *testing.T) {
	p := NewWithConfig(Config{})
	got, err := p.ImportCredentials("[" + qoderCredFixture + "," + qoderCredFixture + "]")
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
		{"缺 access_token", `{"auth":{"refresh_token":"drt-FAKE","uid":"u1"}}`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if _, err := p.ImportCredentials(bad.in); err == nil {
				t.Error("应当报错")
			}
		})
	}
}

// TestImportCredentialsRejectsOtherProduct 另一个产品的凭证必须**当场拒绝**。
//
// # 为什么这条是必需判据（两个实例共用同一份凭证目录）
//
// qoder 与 qodercn 靠文件里的 `product_id` 区分（见 credential.go 的
// `LoadDirFor`）。若导入时不校验它，CN 实例会把一份国际版凭证写进同一个
// 目录、并在池子里标成 CN —— 续期请求就会打 CN 的端点、带国际版的
// refresh_token → 401，而界面显示的是"凭证已失效，请重新登录"：
// 把"导错产品"说成了"凭证坏了"，归因被彻底带偏。
//
// ⚠ 两个方向都要查：只查 intl 拒 CN 会漏掉"CN 卡片收下了国际版凭证"
// 这种恰好更常见的误操作（用户手上有的是国际版的号）。
func TestImportCredentialsRejectsOtherProduct(t *testing.T) {
	other := `{"auth":{"access_token":"dt-FAKE","refresh_token":"drt-FAKE",` +
		`"uid":"u1","product_id":"%s"}}`

	t.Run("国际版实例拒中国版凭证", func(t *testing.T) {
		p := NewWithConfig(Config{})
		in := strings.Replace(other, "%s", ProviderIDCN, 1)
		got, err := p.ImportCredentials(in)
		if err == nil {
			t.Fatalf("应当拒绝 product_id=%s 的凭证，实际产出 %d 条 —— "+
				"收下它会让续期打错端点、并伪装成「凭证已失效」",
				ProviderIDCN, len(got))
		}
		if !strings.Contains(err.Error(), ProviderIDCN) {
			t.Errorf("错误信息应说清这份属于 %s（用户要知道该去哪张卡片导入）：%v",
				ProviderIDCN, err)
		}
	})

	t.Run("中国版实例拒国际版凭证", func(t *testing.T) {
		p := NewWithConfig(Config{Product: QoderCN})
		in := strings.Replace(other, "%s", providerID, 1)
		got, err := p.ImportCredentials(in)
		if err == nil {
			t.Fatalf("应当拒绝 product_id=%s 的凭证，实际产出 %d 条",
				providerID, len(got))
		}
		if !strings.Contains(err.Error(), providerID) {
			t.Errorf("错误信息应说清这份属于 %s：%v", providerID, err)
		}
	})
}

// TestImportCredentialsEmptyProductAssignedToInstance 没写 product_id 的凭证
// 要**归给本实例**（用户在哪张卡片上点的导入，意图就是那个产品）。
//
// 手抄的凭证常常没有 `product_id`（auths 里那些是网关自己写的才带），
// 而"缺省即拒绝"会让用户面对一个他无从理解的错误。
func TestImportCredentialsEmptyProductAssignedToInstance(t *testing.T) {
	p := NewWithConfig(Config{})
	in := `{"auth":{"access_token":"dt-FAKE","refresh_token":"drt-FAKE",` +
		`"uid":"u1","machine_id":"m1"}}`
	got, err := p.ImportCredentials(in)
	if err != nil {
		t.Fatalf("没写 product_id 的凭证应当被接受（归给本实例 %s）: %v",
			providerID, err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	// ⚠ 断言落盘**字节**里的 product_id，而不是内存里的字段：
	// 池子与 `LoadDirFor` 都是按文件内容过滤的，只在内存里改对是无效的。
	var doc struct {
		Auth struct {
			ProductID string `json:"product_id"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(got[0].Raw, &doc); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if doc.Auth.ProductID != providerID {
		t.Errorf("落盘 product_id = %q，want %q —— "+
			"空 product_id 必须被赋成本实例的产品，否则 LoadDirFor 会把它当别的产品",
			doc.Auth.ProductID, providerID)
	}
}

// TestImportCredentialsCNAcceptsEmptyProduct 中国版实例必须收下**没写
// product_id** 的凭证，并把它赋成 qodercn。
//
// # 这条替换掉了一份 characterization 测试（缺陷已修）
//
// 子代理最初发现并**如实报告**（没擅自改生产文件）了一个真实缺陷：
//
//	ParseCredential → normalize()：if a.ProductID == "" { a.ProductID = providerID }
//
// `providerID` 是**包级常量 `"qoder"`**（国际版），拿不到实例的产品。
// 于是"没写 product_id"被解析器**提前填成国际版**，导入里那句
// "空则归本实例"的分支永远走不到：
//
//	国际版实例：空 → 填 "qoder" → 恰好等于本实例 → 放行（侥幸对）
//	中国版实例：空 → 填 "qoder" → ≠ "qodercn" → **被拒**
//
// 用户看到的是「该凭证属于 qoder，不能在 qodercn 上导入」—— 他根本没写
// qoder、也没有国际版的号。那条错误把归因指向了错误的方向。
//
// 现在 `ImportCredentials` 改为读**用户粘贴的原文**（`explicitProductID`）
// 来判"有没有显式指定产品"，解析器的兜底值不再参与这个判断。
//
// ⚠ 这条测试钉的是**修好之后**的行为 —— 与原来那份 characterization
// 测试是相反的两面。它自己的注释当时就写了"一旦修好，删掉它"
//（因为留着它会红）。删除 + 换成正面断言，正是那条指示的落实。
func TestImportCredentialsCNAcceptsEmptyProduct(t *testing.T) {
	p := NewWithConfig(Config{Product: QoderCN})
	// 没写 product_id（手工抄的凭证常常就是这样）
	in := `{"auth":{"access_token":"dt-FAKE","refresh_token":"drt-FAKE","uid":"u1"}}`
	got, err := p.ImportCredentials(in)
	if err != nil {
		t.Fatalf("CN 实例应当收下没写 product_id 的凭证（用户既然在 qodercn 的"+
			"卡片上导入，意图就是 qodercn）—— 实际报错: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当产出 1 条，实际 %d", len(got))
	}
	var doc struct {
		Auth struct {
			ProductID string `json:"product_id"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(got[0].Raw, &doc); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if doc.Auth.ProductID != ProviderIDCN {
		t.Errorf("落盘 product_id = %q，want %q —— "+
			"赋错产品会让 LoadDirFor 认不出它（账号进不了池），"+
			"且续期会打错端点", doc.Auth.ProductID, ProviderIDCN)
	}
}

// TestImportCredentialsExplicitWrongProductStillRejected **显式写了**别的产品
// 仍然必须拒 —— 修上面那条缺陷时不能把这条判据一起放松。
//
// # 为什么要相邻地放在这里
//
// 修"空值被误拒"最容易的做法是把整个产品校验删掉。那会把真正该拒的
//（用户拿国际版的凭证往 CN 卡片上贴）也放行 —— 而那种凭证的续期
// **必然失败**（两个产品的 authBase / clientId 完全不同），
// 界面上表现为"凭证已失效，请重新登录"，把"贴错产品"说成了"凭证坏了"。
func TestImportCredentialsExplicitWrongProductStillRejected(t *testing.T) {
	p := NewWithConfig(Config{Product: QoderCN})
	in := `{"auth":{"access_token":"dt-FAKE","refresh_token":"drt-FAKE",` +
		`"uid":"u1","product_id":"qoder"}}`
	_, err := p.ImportCredentials(in)
	if err == nil {
		t.Fatal("显式写了 product_id=qoder 的凭证不该被 CN 实例收下 —— " +
			"它的续期会打错端点、并伪装成『凭证已失效』")
	}
	if !strings.Contains(err.Error(), providerID) {
		t.Errorf("错误信息应说清这份属于 %s（用户要知道该去哪张卡片导入）: %v",
			providerID, err)
	}
}

// TestImportCredentialsExplicitProductInTopLevel 顶层写 `product_id` 也要认。
//
// 本包落盘用嵌套形（`auth.product_id`），但**手工抄的**凭证更可能写成顶层。
// 只认嵌套形的话，"顶层写了别的产品"会被当成"没写"而放行 ——
// 那正是上面那条缺陷的另一种形态。
func TestImportCredentialsExplicitProductInTopLevel(t *testing.T) {
	p := NewWithConfig(Config{Product: QoderCN})
	in := `{"product_id":"qoder",` +
		`"auth":{"access_token":"dt-FAKE","refresh_token":"drt-FAKE","uid":"u1"}}`
	if _, err := p.ImportCredentials(in); err == nil {
		t.Error("顶层写了 product_id=qoder 时也该拒 —— " +
			"只认嵌套形会让这种输入被当成『没写』而放行")
	}
}
