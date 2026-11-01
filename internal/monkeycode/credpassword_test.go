// credpassword_test.go 「密码随凭证一起走」这条链路的回归（T-020 第 ⑤/③ 项）。
//
// # 它守的是什么
//
// 2026-11-01 口径变更：**导入时保留 password/email**（详见 regmachine.go 文件头
// 「当初为什么丢 password、为什么现在又要」）。从前注册机的
// `password` 字段根本**没有进 regMachineDoc** —— 也就是说不是"接收了又丢弃"，
// 是连收都没收。所以这条测试钉的是"两头都对"：
//
//	① 注册机格式里带 password/email 时 → Auth.Password/Email == 原文
//	② hasPassword=true 但没有 password 字段（旧裁剪配置导出的样子）→ 留空且**不报错**
//	③ 落盘往返不丢（SaveFile → LoadFile 之后还在）
//	④ 老档只有字符串形态的续期时刻时，SessionAnchor 仍能取到（升级不退化）
//
// # 为什么"②"特别重要
//
// 那种记录在用户机器上**真实存在**（注册机侧曾把 password 从导出字段里裁掉）。
// 若这里为了"参数齐全"而编一个假密码，续期会拿着假密码去撞上游 —— 白烧一次 PoW、
// 在上游记一笔失败登录。宁可让 RenewSession 明确报"缺 email/password"。
// ⚠ 本文件的凭证一律是**假值**（.invalid 域 / 固定串）。绝不要粘真实凭证进来 —— 见 AGENTS.md 的安全条目
package monkeycode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// credPwSample 一份**带密码与邮箱**的注册机记录（字段名与真机导出一致）。
const credPwSample = `{
  "platform": "monkeycode",
  "userId": "00000000-0000-4000-8000-000000000000",
  "name": "blackwellaudrey12",
  "region": "intl",
  "session": "00000000-0000-4000-8000-000000000000",
  "email": "TestPassw0rd!FAKE",
  "hasPassword": true,
  "password": "TestPassw0rd!FAKE",
  "plan": "basic",
  "balance": 200000,
  "renewedAt": 1793545574222
}`

// TestRegMachineKeepsPasswordAndEmail ① 注册机格式里的 password/email 必须落到 Auth。
//
// 断言用**原文相等**而不是"非空"：后者在字段名写错（比如 `passwd`）时也会绿，
// 而那种错的表现是"续期永远说缺密码"，用户根本猜不到是导入途径丢了字段。
func TestRegMachineKeepsPasswordAndEmail(t *testing.T) {
	a, err := parseAuth([]byte(credPwSample))
	if err != nil {
		t.Fatalf("解析注册机记录失败: %v", err)
	}
	if a.Password != "TestPassw0rd!FAKE" {
		t.Errorf("password 没保留：got %q want %q（2026-11-01 起必须保留）", a.Password, "TestPassw0rd!FAKE")
	}
	if a.Email != "TestPassw0rd!FAKE" {
		t.Errorf("email 没保留：got %q", a.Email)
	}
	if !a.HasRenewCreds() {
		t.Error("email/password 都在时 HasRenewCreds 应为 true（否则续期会被前置检查挡掉）")
	}
	// renewedAt 是**毫秒时间戳数字**：必须容错解析，不能把整条 JSON 弄坏。
	if a.SessionRenewedAt.IsZero() {
		t.Error("注册机的 renewedAt（毫秒数字）没解析出来 —— SessionAnchor 会退回 AddedAt 估算")
	} else if want := time.UnixMilli(1793545574222); !a.SessionRenewedAt.Equal(want) {
		t.Errorf("renewedAt 解析成 %s，want %s", a.SessionRenewedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestRegMachineHasPasswordWithoutPlaintext ② 有 hasPassword 但没明文 → 留空、不报错。
//
// 这是**旧导出配置**的真实形态（注册机曾把 password 从 exportFields 里裁掉）。
func TestRegMachineHasPasswordWithoutPlaintext(t *testing.T) {
	const noPlain = `{"platform":"monkeycode","userId":"u1","name":"n1","region":"cn","session":"sess-1","hasPassword":true,"email":"a@b.c"}`
	a, err := parseAuth([]byte(noPlain))
	if err != nil {
		t.Fatalf("没有明文密码不该让导入失败: %v", err)
	}
	if a.Password != "" {
		t.Errorf("没有明文时 Password 必须留空（不许造假），实际 %q", a.Password)
	}
	if a.Email != "a@b.c" {
		t.Errorf("email 该保留：got %q", a.Email)
	}
	if a.HasRenewCreds() {
		t.Error("只有 email 没有密码时 HasRenewCreds 必须为 false（否则会拿空密码去撞上游）")
	}
	if !a.Usable() {
		t.Error("缺密码不影响 Usable（session 在就usable）——别把凭证整条丢掉")
	}
}

// TestPasswordRoundTripsDisk ③ 落盘往返不丢（SaveFile → LoadFile）。
//
// # 为什么单钉这一条
//
// 导入路径的终点是 credOf → MarshalAuthFile → 核心写盘。若某个环节做了
// 字段白名单裁剪（本仓踩过："导进本仓毫无用处"的字段被主动剔掉），
// 那么内存里明明有密码、重启后就没了 —— 现象是"昨天能续期，今天说缺密码"。
func TestPasswordRoundTripsDisk(t *testing.T) {
	a, err := parseAuth([]byte(credPwSample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 补上本仓独有的字段（真机上也由签发流程写），确保往返覆盖全部关键字段。
	a.APIKey = "oma_test_key"
	a.SigningSecret = "omas_test_secret"

	dir := t.TempDir()
	path, err := SaveFile(dir, a)
	if err != nil {
		t.Fatalf("写盘失败: %v", err)
	}
	back, err := LoadFile(path)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if back.Password != a.Password {
		t.Errorf("★ password 落盘后丢了：%q → %q（续期材料没了）", a.Password, back.Password)
	}
	if back.Email != a.Email {
		t.Errorf("email 落盘后丢了：%q → %q", a.Email, back.Email)
	}
	if back.APIKey != a.APIKey || back.SigningSecret != a.SigningSecret {
		t.Errorf("api_key/signing_secret 落盘后丢了：%q/%q", back.APIKey, back.SigningSecret)
	}
	if back.SessionRenewedAt.IsZero() || !back.SessionRenewedAt.Equal(a.SessionRenewedAt) {
		t.Errorf("session_renewed_at 落盘后不一致：%s vs %s",
			back.SessionRenewedAt.Format(time.RFC3339), a.SessionRenewedAt.Format(time.RFC3339))
	}
	if back.Region != a.Region || back.UserID != a.UserID || back.Nickname != a.Nickname {
		t.Errorf("身份字段被改：region=%q userid=%q nick=%q", back.Region, back.UserID, back.Nickname)
	}
}

// TestPasswordNotInDefaultFlatJSON ④ 空的 password/email/续期章**不进** JSON。
//
// # 为什么这条必须有（实测原因）
//
// `time.Time` 是 struct，`encoding/json` 的 **omitempty 对它无效** ——
// 零值时间照样被写成 `"session_renewed_at":"0001-01-01T00:00:00Z"`。
// 后果不只是难看：那个值**会被解析成一个合法时刻（公元 1 年）**，
// 于是 SessionAnchor 认为"这号续期于公元 1 年" ⇒ 到期列显示"早就过期了"。
// 所以 Auth 自己实现 MarshalJSON（零值续期章用 nil 指针 ⇒ omitempty 生效）。
func TestPasswordNotInDefaultFlatJSON(t *testing.T) {
	raw, err := json.Marshal(&Auth{Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"password", "email", "session_renewed_at", "session_renewed_at_raw"} {
		if _, ok := m[k]; ok {
			t.Errorf("空值字段 %q 不该出现在 JSON 里（omitempty 失效；零值续期章会被当成公元 1 年）", k)
		}
	}
	// 反向断言：有值时必须**在**（别把 omitempty 写成"永远不输出"）。
	raw2, _ := json.Marshal(&Auth{Session: "s", Password: "p", Email: "e@x.y", SessionRenewedAt: time.Unix(1793545574, 0)})
	var m2 map[string]any
	if err := json.Unmarshal(raw2, &m2); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"password", "email", "session_renewed_at"} {
		if _, ok := m2[k]; !ok {
			t.Errorf("有值时字段 %q 却没写进去（序列化把真数据吞了）", k)
		}
	}
}

// TestLegacyRenewedAtRawStillAnchors ⑤ 老档的字符串形态续期时刻仍能当锚点。
//
// 兼容路径见 Auth.SessionRenewedAtRaw 的说明：删掉它会让老档一升级就
// "续期时刻失效" → 到期列退回 AddedAt 估算 → 刚续过的号被误报"即将过期"。
func TestLegacyRenewedAtRawStillAnchors(t *testing.T) {
	renewed := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	past := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a := &Auth{
		Session:             "s",
		AddedAt:             past.Format(time.RFC3339),
		SessionRenewedAtRaw: renewed.Format(time.RFC3339),
	}
	// 走一遍真实解析路径（JSON 里的那个键名）。
	raw, _ := json.Marshal(map[string]string{
		"session":                a.Session,
		"added_at":               a.AddedAt,
		"session_renewed_at_raw": a.SessionRenewedAtRaw,
	})
	var parsed Auth
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if parsed.SessionRenewedAtRaw != a.SessionRenewedAtRaw {
		t.Fatalf("session_renewed_at_raw 没解出来：%q", parsed.SessionRenewedAtRaw)
	}
	at, ok := SessionAnchor(&parsed)
	if !ok || !at.Equal(renewed) {
		t.Errorf("老档续期时刻应优先于 AddedAt=%s，实际 ok=%v at=%s",
			past.Format(time.RFC3339), ok, at.Format(time.RFC3339))
	}
}

// TestAuthTimeTolerantForms ParseAuthTime 的四种输入形态。
//
// 少认一种的表现都很隐蔽：导入不报错、但续期时刻静默为零 ⇒ 到期列退回估算。
func TestAuthTimeTolerantForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
		ok   bool
	}{
		{"毫秒时间戳（注册机形态）", "1793545574222", time.UnixMilli(1793545574222), true},
		{"秒时间戳", "1793545574", time.Unix(1793545574, 0), true},
		{"RFC3339", "2026-11-01T23:00:00Z", time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC), true},
		{"手写空格形态", "2026-11-01 23:00:00", time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC), true},
		{"纯日期", "2026-11-01", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), true},
		{"空", "", time.Time{}, false},
		{"null", "null", time.Time{}, false},
		{"看不懂", "昨天下午", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := ParseAuthTime(c.in)
		if ok != c.ok {
			t.Errorf("%s：ok=%v want %v（输入 %q）", c.name, ok, c.ok, c.in)
			continue
		}
		if ok && !got.Equal(c.want) {
			t.Errorf("%s：got %s want %s", c.name, got.UTC().Format(time.RFC3339), c.want.UTC().Format(time.RFC3339))
		}
	}
}

// TestAuthFileMode0600 ⑥ 含明文密码的档必须是 0600（Unix 语义；Windows 上不校验）。
//
// 这条是"明文密码落盘"这个决定的**唯一补偿措施**：既然要落，就不能是 0644。
func TestAuthFileMode0600(t *testing.T) {
	a := &Auth{Session: "s", Password: "p", Email: "e@x.y"}
	path, err := SaveFile(t.TempDir(), a)
	if err != nil {
		t.Fatalf("写盘失败: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 上 os.Stat 的 Perm 恒为 0666/0444，这个断言没有意义（也不该为了它 skip 整条）。
	// Unix 语义下必须验证：含明文密码的档绝不能是 0644。
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("凭据档权限过宽：%v（含明文密码，必须 0600）", fi.Mode().Perm())
	}
	_ = filepath.Base(path)
}
