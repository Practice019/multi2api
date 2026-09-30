package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// quotaScopeStub 一个实现了 QuotaExt 的桩上游，记录**被问了哪些 uid**。
//
// 这正是本组用例要断言的：作用域对不对，等价于"问了哪些 uid"。
type quotaScopeStub struct {
	id string
	// asked 记录 RefreshQuota 收到的 uid（按调用顺序）。
	asked *[]string
}

func (s quotaScopeStub) ID() string               { return s.id }
func (s quotaScopeStub) Caps() gateway.Capability { return gateway.CapChat | gateway.CapQuotaProbe }
func (s quotaScopeStub) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (s quotaScopeStub) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (s quotaScopeStub) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	*s.asked = append(*s.asked, uid)
	return gateway.CreditsQuota(123), true
}

// newScopeFixture 造一个含两个上游、三个账号的池 + registry。
//
//	alpha: a1, a2      beta: b1
func newScopeFixture(t *testing.T) (*Handler, *[]string) {
	t.Helper()
	reg := gateway.NewRegistry()
	asked := &[]string{}
	if err := reg.Register(quotaScopeStub{id: "alpha", asked: asked}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(quotaScopeStub{id: "beta", asked: asked}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	for _, c := range []struct{ uid, provider string }{
		{"a1", "alpha"}, {"a2", "alpha"}, {"b1", "beta"},
	} {
		p.AddFor(c.provider, &auth.Auth{UID: c.uid, Nickname: c.uid}, nil)
	}
	h := New(Config{Registry: reg, Pool: p, DefaultProvider: "alpha"})
	return h, asked
}

// doScope 按 body 调一次额度刷新端点，返回回执。
func doScope(t *testing.T, h *Handler, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/quota/refresh", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345" // 绕开本机校验（httptest 默认 192.0.2.1 会被 403）
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是 JSON（HTTP %d）：%s", rec.Code, rec.Body.String())
	}
	return out
}

// TestQuotaRefreshScopeAccount 单账号作用域：只刷那一个 uid。
//
// # 这是用户报的核心问题
//
// 「在账号池里点某一行的『额度』按钮，结果**所有账号的额度都被刷新了**」。
// 根因是本端点以前不收参数、恒为全池刷新 —— 行内按钮的 `{uid}` 被忽略。
//
// 判据：被问到的 uid **集合恰好是那一个**（多一个少一个都算错）。
//
// 变异可检：后端改回"忽略 body.uid 恒刷全池" → 本用例红
// （asked 会变成 3 个而不是 1 个）。
func TestQuotaRefreshScopeAccount(t *testing.T) {
	h, asked := newScopeFixture(t)
	out := doScope(t, h, `{"uid":"a1"}`)

	if out["scope"] != "account" {
		t.Errorf("scope = %v，want account", out["scope"])
	}
	if got := *asked; len(got) != 1 || got[0] != "a1" {
		t.Errorf("被刷的 uid = %v，want 只有 [a1] —— "+
			"行内按钮必须只刷该账号；刷到别人就是用户报的那个 bug", got)
	}
	if out["count"] != float64(1) {
		t.Errorf("count = %v，want 1", out["count"])
	}
}

// TestQuotaRefreshScopeProvider 上游作用域：只刷该上游的全部账号。
//
// 「刷新本上游额度」按钮 —— 三层里的中间一层（用户要求新增）。
func TestQuotaRefreshScopeProvider(t *testing.T) {
	h, asked := newScopeFixture(t)
	out := doScope(t, h, `{"provider":"alpha"}`)

	if out["scope"] != "provider" {
		t.Errorf("scope = %v，want provider", out["scope"])
	}
	got := *asked
	if len(got) != 2 {
		t.Fatalf("被刷的 uid = %v，want alpha 的 2 个（a1 a2）—— "+
			"按上游刷新不该动别的上游（b1）", got)
	}
	for _, u := range got {
		if u == "b1" {
			t.Errorf("beta 的账号 b1 被刷了 —— 按上游刷新只该刷自己那批：%v", got)
		}
	}
	if out["count"] != float64(2) {
		t.Errorf("count = %v，want 2", out["count"])
	}
}

// TestQuotaRefreshScopeAll 全部作用域：不带参数刷全池（**改造前的行为**）。
//
// # 向后兼容的落点
//
// 既有调用方（顶部按钮、脚本、curl）都不带参数，必须逐字段保持原行为。
func TestQuotaRefreshScopeAll(t *testing.T) {
	h, asked := newScopeFixture(t)
	out := doScope(t, h, ``)

	if out["scope"] != "all" {
		t.Errorf("scope = %v，want all", out["scope"])
	}
	if got := *asked; len(got) != 3 {
		t.Errorf("被刷的 uid = %v，want 全部 3 个 —— "+
			"不带参数必须保持改造前的全池语义", got)
	}
	if out["count"] != float64(3) {
		t.Errorf("count = %v，want 3", out["count"])
	}
}

// TestQuotaRefreshScopeQueryParams query 参数也认（便于 curl/脚本）。
//
// 与 body 等价 —— 两套入口不能只有一个能用（那会让人以为参数没生效）。
func TestQuotaRefreshScopeQueryParams(t *testing.T) {
	h, asked := newScopeFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/quota/refresh?uid=a2", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := *asked; len(got) != 1 || got[0] != "a2" {
		t.Errorf("query 形式被刷的 uid = %v，want [a2] —— "+
			"query 与 body 必须等价（否则脚本调用会静默刷全池）", got)
	}
}

// TestQuotaRefreshUnknownUIDIs404 传了池里没有的 uid → 404，**不是"刷了 0 个"**。
//
// 与 reload 同一条原则：显式传了却查不到要**刺眼地失败**，
// 不能伪装成"成功但什么也没做"（用户会以为额度刷过了）。
func TestQuotaRefreshUnknownUIDIs404(t *testing.T) {
	h, asked := newScopeFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/quota/refresh",
		strings.NewReader(`{"uid":"nope"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("HTTP %d，want 404（账号不存在）", rec.Code)
	}
	if len(*asked) != 0 {
		t.Errorf("404 时不该刷任何账号，实际刷了 %v", *asked)
	}
}

// TestQuotaRefreshUnknownProviderIs404 上游在池里没账号 → 404。
func TestQuotaRefreshUnknownProviderIs404(t *testing.T) {
	h, _ := newScopeFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/quota/refresh",
		strings.NewReader(`{"provider":"gamma"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("HTTP %d，want 404（该上游在池里没有账号）", rec.Code)
	}
}

// TestQuotaRefreshUIDWinsOverProvider uid 优先于 provider（两者都传时）。
//
// 明确优先级，避免"两个都给"时行为靠实现顺序决定 ——
// 那类不确定是最难查的（同一次请求可能刷 1 个也可能刷 2 个）。
func TestQuotaRefreshUIDWinsOverProvider(t *testing.T) {
	h, asked := newScopeFixture(t)
	out := doScope(t, h, `{"uid":"a1","provider":"alpha"}`)

	if out["scope"] != "account" {
		t.Errorf("scope = %v，want account —— uid 更具体，应当优先", out["scope"])
	}
	if got := *asked; len(got) != 1 || got[0] != "a1" {
		t.Errorf("被刷的 uid = %v，want 只有 [a1]", got)
	}
}

// TestQuotaRefreshResponseKeepsAccounts 回执仍带 accounts（前端一次调用重绘）。
//
// 三层作用域都要带 —— 漏了会让"点行内按钮后整表不刷新"。
func TestQuotaRefreshResponseKeepsAccounts(t *testing.T) {
	h, _ := newScopeFixture(t)
	out := doScope(t, h, `{"uid":"a1"}`)
	if _, ok := out["accounts"]; !ok {
		t.Error("回执缺少 accounts —— 前端点完按钮无法重绘表格")
	}
}
