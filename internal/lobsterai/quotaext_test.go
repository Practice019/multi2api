package lobsterai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// newQuotaServer 假余额上游（lobsterai：/api/user/profile-summary 的 totalCreditsRemaining）。
func newQuotaServer(t *testing.T, points float64, code int) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/profile-summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if code != 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": "boom"})
			return
		}
		// lobsterai 用**信封**形：{code:0, data:{...}}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"totalCreditsRemaining": points},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewWithBase(srv.URL)
}

func newQuotaProvider(t *testing.T, c *Client, uid string, a *Auth) *Provider {
	t.Helper()
	p := NewWithConfig(Config{Client: c})
	p.SetCredentialSource(func(got string) (gateway.Credential, bool) {
		if got != uid {
			return gateway.Credential{}, false
		}
		return gateway.Credential{Provider: ProviderID, UID: uid, Secret: a}, true
	})
	return p
}

// TestRefreshQuotaReportsRawValue 额度**原样**报出，不做单位换算。
//
// 用户明确要求：显示原始值、不做单位假设（积分是上游自己的记账单位，
// 我们没有可靠依据换算）。
//
// 变异可检：把返回改成乘/除任何系数 → 本用例红。
func TestRefreshQuotaReportsRawValue(t *testing.T) {
	p := newQuotaProvider(t, newQuotaServer(t, 3400, 0), "u1", &Auth{AccessToken: "t"})
	qv, ok := p.RefreshQuota("u1")
	if !ok || !qv.HasData {
		t.Fatalf("应查到额度：ok=%v HasData=%v", ok, qv.HasData)
	}
	if qv.Remaining != 3400 {
		t.Errorf("Remaining = %d，want 3400（**原始值**，不做单位换算）", qv.Remaining)
	}
	if qv.Kind != gateway.QuotaKindCredits {
		t.Errorf("Kind = %q，want %q", qv.Kind, gateway.QuotaKindCredits)
	}
}

// TestRefreshQuotaUnknownUIDReturnsFalse 账号不在池里 → ok=false。
func TestRefreshQuotaUnknownUIDReturnsFalse(t *testing.T) {
	p := newQuotaProvider(t, newQuotaServer(t, 1, 0), "u1", &Auth{AccessToken: "t"})
	if _, ok := p.RefreshQuota("nope"); ok {
		t.Error("未知 uid 应返回 ok=false")
	}
	if _, ok := p.RefreshQuota(""); ok {
		t.Error("空 uid 应返回 ok=false")
	}
}

// TestRefreshQuotaUnwiredReturnsFalse 没注入凭证源 → ok=false（不是错，但取不到）。
func TestRefreshQuotaUnwiredReturnsFalse(t *testing.T) {
	p := NewWithConfig(Config{Client: newQuotaServer(t, 1, 0)})
	if _, ok := p.RefreshQuota("u1"); ok {
		t.Error("未接线时应 ok=false")
	}
}

// TestRefreshQuotaFailureIsUnknownNotZero ★ 查不到时是"未知"，**不是 0**。
//
//	HasData=false              → 没查到 → 界面 `—`
//	HasData=true, Remaining=0  → 查到了确实是 0 → 界面 `0`
//
// 混同会让"没查过"显示成确定的 0，用户以为号废了而误删。
func TestRefreshQuotaFailureIsUnknownNotZero(t *testing.T) {
	p := newQuotaProvider(t, newQuotaServer(t, 0, 500), "u1", &Auth{AccessToken: "t"})
	qv, ok := p.RefreshQuota("u1")
	if !ok {
		t.Fatal("账号存在 → ok 应为 true")
	}
	if qv.HasData {
		t.Errorf("上游失败时 HasData 必须 false，得到 %+v", qv)
	}
}

// TestRefreshQuotaZeroIsData 真查到 0 时 HasData 必须 true（与上条配对）。
func TestRefreshQuotaZeroIsData(t *testing.T) {
	p := newQuotaProvider(t, newQuotaServer(t, 0, 0), "u1", &Auth{AccessToken: "t"})
	qv, ok := p.RefreshQuota("u1")
	if !ok || !qv.HasData {
		t.Fatalf("上游回 0 也算查到：ok=%v HasData=%v", ok, qv.HasData)
	}
	if qv.Remaining != 0 {
		t.Errorf("Remaining = %d，want 0", qv.Remaining)
	}
}

// TestRefreshQuotaNoNetworkWhenUnwired 不接线时**不发网络请求**。
func TestRefreshQuotaNoNetworkWhenUnwired(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.RefreshQuota("u1")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("未接线时不该发网络请求")
	}
}
