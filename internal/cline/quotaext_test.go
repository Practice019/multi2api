package cline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// newQuotaServer 起一个假的余额上游，返回指定的 balance。
func newQuotaServer(t *testing.T, balance int64, status int) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"userId": "usr-x", "balance": balance},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewWithBase(srv.URL)
}

// newQuotaProvider 造一个注入了凭证源的 Provider。
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
// # 用户明确要求：显示原始值、不做单位假设
//
// `balance: 500000` 的单位**没有确证** —— 参照项目里它只出现在 zod schema
//（`z.string()`）与 redaction 关键词表，没有任何换算点。
//
// 我们另有一个 `balanceScale`（推断为 micro-USD，÷100000 → $5.00），
// 但本扩展点**不用它**：界面上的数字若带了错误的单位，比没有数字更误导。
//
// 变异可检：把返回改成 `int64(res.Raw / balanceScale)` → 本用例红
//（500000 会变成 5）。
func TestRefreshQuotaReportsRawValue(t *testing.T) {
	c := newQuotaServer(t, 500000, 0)
	p := newQuotaProvider(t, c, "usr-x", &Auth{AccessToken: "workos:t", AccountID: "usr-x"})

	qv, ok := p.RefreshQuota("usr-x")
	if !ok {
		t.Fatal("账号存在时应返回 ok=true")
	}
	if !qv.HasData {
		t.Fatal("查到了额度就该 HasData=true")
	}
	if qv.Remaining != 500000 {
		t.Errorf("Remaining = %d，want 500000（**原始值**）—— "+
			"用户要求不做单位假设；换算成 5 会带上未经证实的单位",
			qv.Remaining)
	}
	if qv.Kind != gateway.QuotaKindCredits {
		t.Errorf("Kind = %q，want %q", qv.Kind, gateway.QuotaKindCredits)
	}
}

// TestRefreshQuotaUnknownUIDReturnsFalse 账号不在池里 → ok=false（调用方跳过/404）。
func TestRefreshQuotaUnknownUIDReturnsFalse(t *testing.T) {
	p := newQuotaProvider(t, newQuotaServer(t, 1, 0), "usr-x", &Auth{AccountID: "usr-x"})
	if _, ok := p.RefreshQuota("someone-else"); ok {
		t.Error("未知 uid 应返回 ok=false")
	}
	if _, ok := p.RefreshQuota(""); ok {
		t.Error("空 uid 应返回 ok=false")
	}
}

// TestRefreshQuotaUnwiredReturnsFalse 没注入凭证源（未接线）→ ok=false。
func TestRefreshQuotaUnwiredReturnsFalse(t *testing.T) {
	p := NewWithConfig(Config{Client: newQuotaServer(t, 1, 0)})
	// 刻意不调 SetCredentialSource
	if _, ok := p.RefreshQuota("usr-x"); ok {
		t.Error("未接线时应返回 ok=false（不是错，但取不到）")
	}
}

// TestRefreshQuotaFailureIsUnknownNotZero ★ 查不到时**必须**是"未知"，不是 0。
//
// # 这是本扩展点最要紧的语义
//
//	HasData=false              → 没查到（凭据过期/限流/网络）→ 界面显示 `—`
//	HasData=true, Remaining=0  → 查到了，确实是 0              → 界面显示 `0`
//
// 混为一谈会让"没查过"显示成看确定的 0，用户以为"这个号没额度了"而误删账号
// —— gateway.QuotaView 的注释写明这正是要修的坑。
//
// 变异可检：把失败分支改成 `return gateway.CreditsQuota(0), true` → 本用例红。
func TestRefreshQuotaFailureIsUnknownNotZero(t *testing.T) {
	cases := []struct {
		name string
		c    *Client
		auth *Auth
	}{
		{"上游 401（凭据过期）", newQuotaServer(t, 0, 401), &Auth{AccessToken: "t", AccountID: "usr-x"}},
		{"上游 500", newQuotaServer(t, 0, 500), &Auth{AccessToken: "t", AccountID: "usr-x"}},
		{"缺 account_id（查询必然失败）", newQuotaServer(t, 1, 0), &Auth{AccessToken: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newQuotaProvider(t, tc.c, "usr-x", tc.auth)
			qv, ok := p.RefreshQuota("usr-x")
			if !ok {
				t.Fatal("账号存在 → ok 应为 true（ok=false 是「账号不存在」，不是「查不到」）")
			}
			if qv.HasData {
				t.Errorf("查不到时 HasData 必须为 false（否则调用方会把 0 写回池，"+
					"用户以为额度用完）；得到 %+v", qv)
			}
			if qv.Remaining != 0 {
				t.Errorf("HasData=false 时不该带 Remaining，得到 %d", qv.Remaining)
			}
		})
	}
}

// TestRefreshQuotaZeroIsData 真的查到 0 时 HasData 必须为 true。
//
// 与上一条配对：两者是**不同**的状态，不能让"0"看起来像"没查到"。
func TestRefreshQuotaZeroIsData(t *testing.T) {
	p := newQuotaProvider(t, newQuotaServer(t, 0, 0), "usr-x",
		&Auth{AccessToken: "t", AccountID: "usr-x"})
	qv, ok := p.RefreshQuota("usr-x")
	if !ok {
		t.Fatal("ok 应为 true")
	}
	if !qv.HasData {
		t.Error("上游明确回了 balance=0 → HasData 必须是 true（「查到了，确实是 0」）")
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
		_, _ = p.RefreshQuota("usr-x")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("未接线时不该发起网络请求")
	}
}
