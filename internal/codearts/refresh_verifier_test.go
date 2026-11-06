package codearts

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// ── T-229：续期请求要不要带 PKCE verifier ────────────────────────────────
//
// # 背景（两实现之间的硬冲突）
//
// 官方实现（Agent2Providers `codearts/oauth.rs:13-16`）把这条写成硬要求：
//
//	"上游把 `refresh_token` 绑在**当初那次授权的 PKCE 上下文**上
//	（官方扩展同样把 verifier 与 DPoP 私钥一起落盘）。只带 refresh token 会被拒。"
//
// 而本仓历史上**不带**它也能通过（`refresh_test.go` 的 live 测试跑通过 ⇒
// 上游似乎并不校验）。两件事都可能是真的 —— 所以本轮取"**加了不坏**"的策略：
//
//	新凭证（登录产生）带上 verifier → 对齐官方；
//	老凭证（没有该字段）**一个字都不发** → 行为与改动前逐字节一致。
//
// 这两条都必须被测试钉住，否则"向后兼容"只是口头承诺。

// newSTSCapture 起一个假 STS 端点，把每次收到的表单按顺序捕获。
//
// 只捕获**请求**，不解析响应 —— 本测试要验的是"发出去的字段"，
// 而响应解析另有 `refresh_test.go` 覆盖。响应给 200 + 空 JSON 即可。
func newSTSCapture(t *testing.T, forms *[]url.Values) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("假 STS 解析表单失败: %v", err)
		}
		*forms = append(*forms, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
}

// TestRefreshRequestCarriesCodeVerifier ★ 有 verifier ⇒ refresh grant 必须带上它。
func TestRefreshRequestCarriesCodeVerifier(t *testing.T) {
	var forms []url.Values
	srv := newSTSCapture(t, &forms)
	defer srv.Close()

	c := New()
	c.STSBase = srv.URL

	a := newTestAuth(t, t.TempDir())
	a.CodeVerifier = "VERIFIER-FOR-TEST"

	kp, err := FromPrivateJWK(a.DPoPPrivateKeyJWK)
	if err != nil {
		t.Fatalf("从落盘 JWK 恢复 DPoP 私钥失败: %v（这条链断了就根本到不了 HTTP 层）", err)
	}
	if _, _, err := c.refreshAttempt(a, kp); err != nil {
		t.Fatalf("refreshAttempt: %v", err)
	}

	if len(forms) != 1 {
		t.Fatalf("假 STS 收到 %d 次请求，期望恰好 1 次", len(forms))
	}
	f := forms[0]
	if got := f.Get("code_verifier"); got != "VERIFIER-FOR-TEST" {
		t.Errorf("code_verifier = %q；官方要求 refresh grant 同带它（oauth.rs:13-16）", got)
	}
	if got := f.Get("grant_type"); got != "refresh_token" {
		t.Errorf("grant_type = %q，期望 refresh_token", got)
	}
	if f.Get("client_id") == "" {
		t.Error("client_id 为空")
	}
	if f.Get("refresh_token") == "" {
		t.Error("refresh_token 为空")
	}
}

// TestRefreshOmitsEmptyCodeVerifier ★ 向后兼容：老凭证（无 verifier）⇒ **不发该字段**。
//
// 断言"键根本不存在"而不是"值为空串"：空串会让上游看到一个"
// `code_verifier=`"的空参数，那是**新行为**而不是旧行为。
func TestRefreshOmitsEmptyCodeVerifier(t *testing.T) {
	var forms []url.Values
	srv := newSTSCapture(t, &forms)
	defer srv.Close()

	c := New()
	c.STSBase = srv.URL

	a := newTestAuth(t, t.TempDir()) // newTestAuth 不设 CodeVerifier ⇒ 相当于老凭证
	if a.CodeVerifier != "" {
		t.Fatalf("前置条件被破坏：newTestAuth 竟然带了 verifier（%q）", a.CodeVerifier)
	}

	kp, err := FromPrivateJWK(a.DPoPPrivateKeyJWK)
	if err != nil {
		t.Fatalf("恢复 DPoP 私钥失败: %v", err)
	}
	if _, _, err := c.refreshAttempt(a, kp); err != nil {
		t.Fatalf("refreshAttempt: %v", err)
	}
	if len(forms) != 1 {
		t.Fatalf("假 STS 收到 %d 次请求，期望 1 次", len(forms))
	}
	if _, exists := forms[0]["code_verifier"]; exists {
		t.Errorf("老凭证不该发 code_verifier，实际发了 %q（破坏了向后兼容）", forms[0].Get("code_verifier"))
	}
	// 其余字段一个都不能少（防止"为了不发 verifier 把别的也动了"）
	for _, k := range []string{"client_id", "grant_type", "refresh_token"} {
		if forms[0].Get(k) == "" {
			t.Errorf("老凭证路径丢了字段 %s", k)
		}
	}
}
