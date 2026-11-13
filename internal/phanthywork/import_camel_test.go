// import_camel_test.go 第四种形态（顶层驼峰）的判据（2026-11-14 T-914）。
//
// # 用户报障
//
//	他把一份**顶层驼峰**的 JSON 粘进「批量导入」，导不进去：
//
//	{"platform":"phanthy","userid":"cmwc64nj21ejmsw3ilrzs2iq2","phone":"15083583411",
//	 "account":"+86 150****3411","accessToken":"eyJ…","refreshToken":"ZnGR…",
//	 "expiresAt":1794545067558}
//
// # 这一份到底缺什么（本轮实际查到的，不是猜）
//
//	`parseOfficialShape` 的兜底路径（没有 claudeAiOauth 根键时用**顶层**自己当 inner）
//	其实**已经能读到** accessToken/refreshToken/expiresAt ⇒ 真正没被读的是 **`userid`**：
//	uid 只能回落到 JWT 的 sub ⇒ sub 取不到时整份导入以「无法确定账号标识」失败。
//	⇒ 本轮把 `userid`（及 phone/account 作展示线索）纳入判据，**且只在顶层兜底那一档生效**
//	  （带 claudeAiOauth 根键的旧输入行为一字不变）。
//
// ⚠ 判定仍然只在 ParseCredential 一处（import.go 的文件头纪律：手写第二份映射 = 字段错位）。
package phanthywork

import (
	"encoding/json"
	"strings"
	"testing"
)

// camelDoc 用户那份原样 JSON（⚠ 令牌用假值，但**形状与字段名逐字照抄**）。
func camelDoc(access, refresh string, expMS int64) string {
	return `{"platform":"phanthy","userid":"cmwc64nj21ejmsw3ilrzs2iq2","phone":"15083583411",` +
		`"account":"+86 150****3411","accessToken":"` + access + `",` +
		`"refreshToken":"` + refresh + `","expiresAt":` + itoa64(expMS) + `}`
}

func itoa64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// ① 用户那份原样 JSON ⇒ 解析成功，uid/到期/token 全对。
func TestParseCredentialTopLevelCamelShape(t *testing.T) {
	const access = "eyJhbGciOiJSUzI1NiJ9.payload.sig-top"
	const refresh = "ZnGR-refresh-token"
	a, err := ParseCredential([]byte(camelDoc(access, refresh, 1794545067558)))
	if err != nil {
		t.Fatalf("用户这份顶层驼峰 JSON 必须能解析，实际: %v", err)
	}
	if got := a.UID(); got != "cmwc64nj21ejmsw3ilrzs2iq2" {
		t.Errorf("UID 应取 userid 字段，实际 %q", got)
	}
	exp, ok := a.ExpiresAt()
	if !ok || exp != 1794545067 {
		t.Errorf("ExpiresAt 应为 1794545067（毫秒 ÷1000），实际 %d ok=%v", exp, ok)
	}
	if a.OAuth.AccessToken != access {
		t.Errorf("accessToken 必须与输入**逐字节相等**：got %q", a.OAuth.AccessToken)
	}
	if a.OAuth.RefreshToken != refresh {
		t.Errorf("refreshToken 必须与输入**逐字节相等**：got %q", a.OAuth.RefreshToken)
	}
	// phone/account 只能作**展示线索**（uid 的唯一来源是 userid ⇒ 已在上一条钉住）。
	if a.NicknameValue == "" {
		t.Error("account/phone 应作为展示名兜底填进 NicknameValue（只做兜底，不当 uid）")
	}
	if a.Provider != providerID {
		t.Errorf("Provider 应是 %s，实际 %q", providerID, a.Provider)
	}
}

// ② 落盘形态与其它来源**同形**：marshal ⇒ 再按①（网关嵌套形）解析回来，逐项相等。
func TestTopLevelCamelShapeMarshalsLikeOtherSources(t *testing.T) {
	a, err := ParseCredential([]byte(camelDoc("tok-a", "rt-a", 1794545067558)))
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := a.MarshalAuthFile()
	if err != nil {
		t.Fatalf("MarshalAuthFile: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("落盘必须是合法 JSON: %v", err)
	}
	for _, k := range []string{"auth", "account"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("落盘形态必须与其它来源同形（缺根键 %q）: %s", k, raw)
		}
	}
	// 回读（走①的解析器）⇒ uid/token/到期必须一致。
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("回读自己落盘的文件失败: %v", err)
	}
	if back.UID() != a.UID() || back.OAuth.AccessToken != a.OAuth.AccessToken ||
		back.OAuth.RefreshToken != a.OAuth.RefreshToken {
		t.Errorf("回读后不一致：uid=%q/%q token=%q/%q", back.UID(), a.UID(),
			back.OAuth.AccessToken, a.OAuth.AccessToken)
	}
}

// ④ 冲突输入：既有 claudeAiOauth 根键（旧形态②）又有顶层驼峰字段 ⇒ **按旧形态解释**。
func TestWrappedShapeWinsOverTopLevelCamel(t *testing.T) {
	doc := `{"claudeAiOauth":{"accessToken":"WRAPPED","refreshToken":"WRAPPED-RT","expiresAt":1794545000000},` +
		`"accessToken":"TOPLEVEL","refreshToken":"TOPLEVEL-RT","userid":"top-level-uid","expiresAt":1794549999999}`
	a, err := ParseCredential([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if a.OAuth.AccessToken != "WRAPPED" {
		t.Errorf("★ 冲突时必须按**旧形态**解释（claudeAiOauth 里的那份），实际取了 %q", a.OAuth.AccessToken)
	}
	if a.OAuth.ExpiresAtMS != 1794545000000 {
		t.Errorf("到期时刻也该取旧形态那份，实际 %d", a.OAuth.ExpiresAtMS)
	}
	// ⚠ 顶层 userid **不参与**：带根键的输入按旧口径解释（其 uid 仍来自 token 的 sub / 别处）。
	if a.UIDValue == "top-level-uid" {
		t.Error("带 claudeAiOauth 根键时不该读顶层 userid（那会改变旧输入的解释）")
	}
}

// 顺带钉住：第三种（本包扁平形）仍不受影响 —— 回归锁的另一半（完整回归在既有测试里）。
func TestFlatShapeStillPreferredWhenNoCamelKeys(t *testing.T) {
	doc := `{"oauth":{"access_token":"FLAT","refresh_token":"FLAT-RT"},"uid":"flat-uid"}`
	a, err := ParseCredential([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if a.OAuth.AccessToken != "FLAT" || strings.TrimSpace(a.UID()) != "flat-uid" {
		t.Errorf("第三种形态的行为不许变：token=%q uid=%q", a.OAuth.AccessToken, a.UID())
	}
}
