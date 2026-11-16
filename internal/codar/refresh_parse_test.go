// refresh_parse_test.go codar 续期响应的**宽容解析**与**键名诊断**判据。
//
// # 为什么这组判据重要
//
// 本仓已经出过一次「续期响应的字段名全错」的事故（qoder，见 docs/progress-log.md）：
// 旧实现读 access_token + expires_in，而真实响应里两个都不存在
// ⇒ 续期**永远**报"缺少 access_token"，即使服务端回 200，功能静默失效。
//
// codar 这次报的是「续期响应缺少新令牌」—— 同一个形状的病。
// 所以修法分两半：
//
//	① 宽容解析：常见字段名都认一遍（互斥，多认无代价）
//	② 诊断：再次失败时把**响应里的键名**带进错误（键名足以定位，值可能含令牌）
package codar

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// readP1Source 读同包的 p1.go 源码（供"某段代码不该存在"这类静态断言）。
//
// 用 runtime.Caller 定位而不是相对路径：`go test` 的工作目录是包目录，
// 但换个 runner 就可能不同 —— 那会让这条判据**静默跳过**（t.Skip），
// 而"跳过"和"通过"在结果里长得一样。
func readP1Source() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", os.ErrNotExist
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "p1.go"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ① 顶层 token。
func TestPickTokenTopLevel(t *testing.T) {
	got := pickToken(map[string]any{"token": "abc"})
	if got != "abc" {
		t.Fatalf("顶层 token = %q，want abc", got)
	}
}

// ② data.token。
func TestPickTokenNested(t *testing.T) {
	got := pickToken(map[string]any{"data": map[string]any{"token": "nested"}})
	if got != "nested" {
		t.Fatalf("data.token = %q，want nested", got)
	}
}

// ★ ③ codar 风格：续期返回的是下一轮的 refresh_token。
func TestPickTokenRefreshTokenShape(t *testing.T) {
	got := pickToken(map[string]any{
		"data": map[string]any{
			"refresh_token": "rt-123",
			"expires_at":    "2027-01-01T00:00:00Z",
		},
	})
	if got != "rt-123" {
		t.Fatalf("★ data.refresh_token = %q，want rt-123 —— 这正是「字段名全错」那类事故的形状", got)
	}
}

// ④ 非字符串值一律跳过（expires_in 之类）。
func TestPickTokenSkipsNonString(t *testing.T) {
	got := pickToken(map[string]any{
		"token":      float64(3600),
		"expires_in": float64(3600),
		"data":       map[string]any{"token": map[string]any{"x": 1}},
	})
	if got != "" {
		t.Fatalf("非字符串值不该被当成令牌，实际 %q", got)
	}
}

// ⑤ 空/空白 ⇒ 空。
func TestPickTokenEmpty(t *testing.T) {
	for _, m := range []map[string]any{
		nil, {}, {"token": ""}, {"token": "   "}, {"data": nil},
	} {
		if got := pickToken(m); got != "" {
			t.Fatalf("%v ⇒ %q，want 空", m, got)
		}
	}
}

// ★ ⑥ 键名诊断：列出键，**绝不带值**。
func TestJSONKeyNamesListsKeysNotValues(t *testing.T) {
	raw := []byte(`{"code":400,"message":"bad","token":"SECRET-VALUE-123"}`)
	got := jsonKeyNames(raw)

	for _, want := range []string{"code", "message", "token"} {
		if !strings.Contains(got, want) {
			t.Fatalf("★ 应列出键 %q，实际 %q", want, got)
		}
	}
	if strings.Contains(got, "SECRET-VALUE-123") {
		t.Fatalf("★★ 键名诊断**泄漏了值**：%q —— 值里可能就有令牌", got)
	}
	if strings.Contains(got, "400") && strings.Contains(got, "bad") {
		t.Fatalf("不该出现值：%q", got)
	}
}

// ⑦ data 嵌套也要列出来（那才是 codar 的形状）。
func TestJSONKeyNamesIncludesDataKeys(t *testing.T) {
	raw := []byte(`{"data":{"refresh_token":"x","expires_at":"y"}}`)
	got := jsonKeyNames(raw)
	if !strings.Contains(got, "data:") {
		t.Fatalf("应标出 data 下的键，实际 %q", got)
	}
	for _, want := range []string{"refresh_token", "expires_at"} {
		if !strings.Contains(got, want) {
			t.Fatalf("data 下应有 %q，实际 %q", want, got)
		}
	}
	if strings.Contains(got, "refresh_token\":") || strings.Contains(got, `:"x"`) {
		t.Fatalf("不该出现值：%q", got)
	}
}

// ★ ⑧ 非 JSON ⇒ 只报长度，**不落原文**（原文可能就是令牌）。
func TestJSONKeyNamesNonJSONCarriesNoContent(t *testing.T) {
	raw := []byte("SECRET-RAW-BODY")
	got := jsonKeyNames(raw)
	if strings.Contains(got, "SECRET-RAW-BODY") {
		t.Fatalf("★★ 非 JSON 时把原文写进了诊断：%q", got)
	}
	if !strings.Contains(got, "非 JSON") {
		t.Fatalf("应说明不是 JSON，实际 %q", got)
	}
}

// ★ ⑨ 「未临期」的判据只看**语义**，不看状态码。
//
// 这条钉的是一个**不能从函数签名看出来**的约定：上游若把 400 改成 200 携带
// 同样的语义，锁状态码会让护栏失效 ⇒ 每 6 小时刷一条假错误。
func TestNotCloseToExpirationIsStatusAgnostic(t *testing.T) {
	src, err := readP1Source()
	if err != nil {
		t.Skipf("读不到 p1.go：%v", err)
	}
	// 守卫那一段不能再出现 `status == http.StatusBadRequest &&` 的组合。
	if strings.Contains(src, "status == http.StatusBadRequest && strings.Contains(string(raw), \"not close to expiration\")") {
		t.Fatal("★ 「未临期」的判定仍然锁着 400 —— 上游改状态码就会每 6 小时刷假错误")
	}
	if !strings.Contains(src, `strings.Contains(strings.ToLower(string(raw)), "not close to expiration")`) {
		t.Fatal("★ 找不到语义化的「未临期」判定（大小写不敏感的那种）")
	}
}

// ★ ⑩ 上游错误信封的 code/message 必须能被读出来（它们是排查的全部信息）。
//
// 用户实测的现场：日志只有「响应键：code,message」—— 知道了形状，
// 仍然不知道上游说了什么，等于半个诊断。
func TestJSONEnvelopeExtractsCodeAndMessage(t *testing.T) {
	raw := []byte(`{"code":100003,"message":"账号已被停用"}`)
	got := jsonEnvelope(raw)
	if !strings.Contains(got, "code=100003") {
		t.Fatalf("应带出 code，实际 %q", got)
	}
	if !strings.Contains(got, "账号已被停用") {
		t.Fatalf("应带出 message，实际 %q", got)
	}
}

// Anthropic 风格的 {"error":{...}} 也要认。
func TestJSONEnvelopeHandlesNestedError(t *testing.T) {
	raw := []byte(`{"error":{"code":"invalid_grant","message":"Refresh token is invalid or expired."}}`)
	got := jsonEnvelope(raw)
	if !strings.Contains(got, "invalid_grant") || !strings.Contains(got, "invalid or expired") {
		t.Fatalf("嵌套 error 应被展开，实际 %q", got)
	}
}

// ★ ⑪ **绝不**带出令牌字段（诊断不能变成泄漏）。
func TestJSONEnvelopeNeverLeaksToken(t *testing.T) {
	raw := []byte(`{"code":1,"message":"x","token":"SECRET-T","refresh_token":"SECRET-RT"}`)
	got := jsonEnvelope(raw)
	if strings.Contains(got, "SECRET-T") || strings.Contains(got, "SECRET-RT") {
		t.Fatalf("★★ 诊断泄漏了令牌：%q", got)
	}
}

// message 限长且按 rune 截（中文不能断字）。
func TestJSONEnvelopeTruncatesLongMessage(t *testing.T) {
	long := strings.Repeat("错", 300)
	got := jsonEnvelope([]byte(`{"code":1,"message":"` + long + `"}`))
	if len([]rune(got)) > 200 {
		t.Fatalf("message 应限长，实际 %d 字符", len([]rune(got)))
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatal("★ 出现了替换字符（按字节截断了多字节汉字）")
	}
}

// 没有 code/message 时明确说，而不是给一个空串。
func TestJSONEnvelopeSaysWhenAbsent(t *testing.T) {
	if got := jsonEnvelope([]byte(`{"data":{"x":1}}`)); !strings.Contains(got, "无 code/message") {
		t.Fatalf("应明确说没有，实际 %q", got)
	}
}
